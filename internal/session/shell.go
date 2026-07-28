package session

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/creack/pty"
	xterm "github.com/gitpod-io/xterm-go"
	"github.com/punklabs-ai/falkn/internal/agent"
	"github.com/punklabs-ai/falkn/internal/protocol"
	"github.com/punklabs-ai/falkn/internal/workspace"
)

func (m *Manager) CreateShell(params protocol.ShellCreateParams) (protocol.SessionRecord, error) {
	title := strings.TrimSpace(params.Title)
	if title == "" {
		return protocol.SessionRecord{}, errors.New("a session title is required")
	}
	if utf8.RuneCountInString(title) > 200 {
		return protocol.SessionRecord{}, errors.New("the session title must be 200 characters or fewer")
	}
	directory, err := workspace.Resolve(params.Directory)
	if err != nil {
		return protocol.SessionRecord{}, err
	}
	shell, err := agent.ResolveUserShell()
	if err != nil {
		return protocol.SessionRecord{}, err
	}
	id, err := newSessionID()
	if err != nil {
		return protocol.SessionRecord{}, fmt.Errorf("create session ID: %w", err)
	}
	columns, rows := normalizedTerminalSize(params.TerminalColumns, params.TerminalRows)

	command := exec.Command(shell.Executable)
	command.Args[0] = "-" + filepath.Base(shell.Executable)
	command.Dir = directory
	command.Env = terminalEnvironment(shell.Environment,
		"FALKN_SESSION_ID="+id,
		"FALKN_SESSION_KIND=shell",
	)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: uint16(rows), Cols: uint16(columns)})
	if err != nil {
		return protocol.SessionRecord{}, fmt.Errorf("start shell: %w", err)
	}

	session := &liveSession{
		record: protocol.SessionRecord{
			ID:              id,
			Title:           title,
			Directory:       directory,
			AgentID:         "shell",
			CreatedAt:       time.Now().Unix(),
			Status:          "running",
			Process:         filepath.Base(shell.Executable),
			AttachedClients: 0,
			PermissionMode:  agent.PermissionStandard,
			Kind:            "shell",
		},
		pty:         terminal,
		command:     command,
		transcript:  newByteRing(maxTranscriptBytes),
		terminal:    xterm.New(xterm.WithCols(columns), xterm.WithRows(rows), xterm.WithScrollback(2_000)),
		subscribers: make(map[uint64]*outputSubscriber),
	}
	session.terminal.OnData(func(data string) {
		_ = session.write([]byte(data))
	})

	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()
	if err := m.persist(); err != nil {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		_ = command.Process.Kill()
		_ = terminal.Close()
		return protocol.SessionRecord{}, fmt.Errorf("persist session: %w", err)
	}

	go session.readOutput()
	go m.waitForExit(session)
	return session.snapshot(), nil
}

func (m *Manager) Attach(sessionID string, columns, rows int) ([]byte, <-chan []byte, func(), protocol.SessionRecord, error) {
	session, err := m.running(sessionID)
	if err != nil {
		return nil, nil, nil, protocol.SessionRecord{}, err
	}
	if columns > 0 || rows > 0 {
		if err := m.Resize(sessionID, columns, rows); err != nil {
			return nil, nil, nil, protocol.SessionRecord{}, err
		}
	}

	session.outputMu.Lock()
	history := session.transcript.Bytes()
	id := session.nextSubscriber
	session.nextSubscriber++
	subscriber := newOutputSubscriber()
	session.subscribers[id] = subscriber
	session.stateMu.Lock()
	session.updateNotificationSuppressionLocked()
	record := session.record
	session.stateMu.Unlock()
	session.outputMu.Unlock()

	var once sync.Once
	detach := func() {
		once.Do(func() {
			session.outputMu.Lock()
			if subscriber, ok := session.subscribers[id]; ok {
				delete(session.subscribers, id)
				subscriber.close()
			}
			session.stateMu.Lock()
			session.updateNotificationSuppressionLocked()
			session.stateMu.Unlock()
			session.outputMu.Unlock()
		})
	}
	return history, subscriber.updates, detach, record, nil
}

func (m *Manager) WriteRaw(sessionID string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if len(data) > maxInputBytes {
		return fmt.Errorf("input exceeds the %d-byte limit", maxInputBytes)
	}
	session, err := m.running(sessionID)
	if err != nil {
		return err
	}
	return session.write(data)
}

func (s *liveSession) closeSubscribers() {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	for id, subscriber := range s.subscribers {
		delete(s.subscribers, id)
		subscriber.close()
	}
	s.stateMu.Lock()
	s.updateNotificationSuppressionLocked()
	s.stateMu.Unlock()
}

// FocusedClients remains populated as a compatibility suppression signal for
// notification watchers from releases that predate AttachedClients handling.
// outputMu and stateMu must both be held by the caller.
func (s *liveSession) updateNotificationSuppressionLocked() {
	s.record.AttachedClients = len(s.subscribers)
	s.record.FocusedClients = s.record.AttachedClients
	if s.record.NotificationsMuted && s.record.FocusedClients == 0 {
		s.record.FocusedClients = 1
	}
}

type processRecord struct {
	pid     int
	parent  int
	command string
	args    string
}

func (s *liveSession) refreshForegroundAgent() {
	record := s.snapshot()
	if record.Kind != "shell" || record.Status != "running" || s.command == nil || s.command.Process == nil {
		return
	}
	agentID, process := activeAgentProcess(s.command.Process.Pid)
	if agentID == "" {
		agentID = "shell"
		process = filepath.Base(s.command.Path)
	}
	s.stateMu.Lock()
	s.record.AgentID = agentID
	s.record.Process = process
	s.stateMu.Unlock()
}

func activeAgentProcess(rootPID int) (string, string) {
	output, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=,args=").Output()
	if err != nil {
		return "", ""
	}
	records := make([]processRecord, 0)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parent, parentErr := strconv.Atoi(fields[1])
		if pidErr != nil || parentErr != nil {
			continue
		}
		records = append(records, processRecord{
			pid: pid, parent: parent, command: fields[2], args: strings.Join(fields[3:], " "),
		})
	}

	descendants := map[int]bool{rootPID: true}
	for changed := true; changed; {
		changed = false
		for _, record := range records {
			if !descendants[record.pid] && descendants[record.parent] {
				descendants[record.pid] = true
				changed = true
			}
		}
	}
	for index := len(records) - 1; index >= 0; index-- {
		record := records[index]
		if record.pid == rootPID || !descendants[record.pid] {
			continue
		}
		if id := supportedAgentName(record.command, record.args); id != "" {
			return id, filepath.Base(record.command)
		}
	}
	return "", ""
}

func supportedAgentName(command, arguments string) string {
	candidates := append([]string{command}, strings.Fields(arguments)...)
	for _, candidate := range candidates {
		base := strings.ToLower(filepath.Base(strings.Trim(candidate, "'\"")))
		switch base {
		case "codex", "codex.js", "codex.mjs":
			return "codex"
		case "claude", "claude.js", "claude.mjs":
			return "claude"
		}
	}
	return ""
}
