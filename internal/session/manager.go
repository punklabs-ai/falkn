package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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

type TranscriptPage struct {
	Transcript string
	StartLine  int
	EndLine    int
	TotalLines int
	HasEarlier bool
	HistoryID  string
}

const (
	maxTranscriptBytes  = 2 * 1024 * 1024
	maxInputBytes       = 256 * 1024
	defaultTerminalCols = 60
	defaultTerminalRows = 44
	minimumTerminalCols = 40
	maximumTerminalCols = 160
	minimumTerminalRows = 20
	maximumTerminalRows = 100
)

var (
	ErrSessionNotFound = errors.New("session not found")
	// ErrSessionNotRunning distinguishes a session that has finished from one
	// that never existed. A session can end between being listed and being
	// attached, and a caller that picked it from a list can recover by opening
	// a new one instead of failing.
	ErrSessionNotRunning = errors.New("session is not running")
)

type liveSession struct {
	record             protocol.SessionRecord
	pty                *os.File
	command            *exec.Cmd
	transcript         *byteRing
	terminal           *xterm.Terminal
	terminalMu         sync.Mutex
	terminalHistory    []string
	lastTerminalScreen []string
	stateMu            sync.RWMutex
	agentStartMu       sync.Mutex
	agentStartingUntil time.Time
	writeMu            sync.Mutex
	outputMu           sync.Mutex
	subscribers        map[uint64]*outputSubscriber
	nextSubscriber     uint64
	storedText         string
	outputSequence     uint64
	attentionSequence  uint64
	attentionState     agent.AttentionState
	attentionValid     bool
}

// refreshAttention recomputes the agent's attention state, but only when the
// session has produced output since the last answer. Every attached client
// polls the session list every two seconds, and rendering a transcript to
// evaluate it is far too costly to repeat while a session sits idle.
func (s *liveSession) refreshAttention() {
	s.outputMu.Lock()
	sequence := s.outputSequence
	s.outputMu.Unlock()

	s.stateMu.RLock()
	fresh := s.attentionValid && s.attentionSequence == sequence
	cached := s.attentionState
	status, agentID, kind := s.record.Status, s.record.AgentID, s.record.Kind
	s.stateMu.RUnlock()

	attention := cached
	if !fresh {
		attention = agent.AttentionStateFor(kind, agentID, s.renderedTranscript(600), status)
	}

	s.stateMu.Lock()
	s.attentionState = attention
	s.attentionSequence = sequence
	s.attentionValid = true
	s.record.Attention = string(attention)
	s.stateMu.Unlock()
}

func (s *liveSession) snapshot() protocol.SessionRecord {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.record
}

func (s *liveSession) setStoppedTranscript(status, transcript string) {
	s.stateMu.Lock()
	s.record.Status = status
	s.storedText = transcript
	s.stateMu.Unlock()
}

func (s *liveSession) setArchived(archived bool) {
	s.stateMu.Lock()
	s.record.Archived = archived
	s.stateMu.Unlock()
}

func (s *liveSession) setTitle(title string) string {
	s.stateMu.Lock()
	previous := s.record.Title
	s.record.Title = title
	s.stateMu.Unlock()
	return previous
}

func (s *liveSession) persistedSnapshot() persistedSession {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return persistedSession{Record: s.record, Transcript: s.storedText}
}

type Manager struct {
	mu          sync.RWMutex
	sessions    map[string]*liveSession
	agents      *agent.Registry
	storagePath string
}

type persistedState struct {
	Version  int                `json:"version"`
	Sessions []persistedSession `json:"sessions"`
}

type persistedSession struct {
	Record     protocol.SessionRecord `json:"record"`
	Transcript string                 `json:"transcript"`
}

func NewManager(registry *agent.Registry) *Manager {
	return &Manager{sessions: make(map[string]*liveSession), agents: registry}
}

func (m *Manager) EnablePersistence(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read session state: %w", err)
	}

	state := persistedState{Version: 1}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("decode session state: %w", err)
		}
		if state.Version != 1 {
			return fmt.Errorf("unsupported session state version %d", state.Version)
		}
	}

	m.mu.Lock()
	m.storagePath = path
	for _, stored := range state.Sessions {
		if stored.Record.ID == "" {
			continue
		}
		if stored.Record.Status == "running" {
			stored.Record.Status = "stopped"
		}
		// Client attachments are process-local and cannot survive a daemon
		// restart. Keep only the compatibility suppression marker used for a
		// persistently muted session.
		stored.Record.AttachedClients = 0
		stored.Record.FocusedClients = 0
		if stored.Record.NotificationsMuted {
			stored.Record.FocusedClients = 1
		}
		m.sessions[stored.Record.ID] = &liveSession{
			record:     stored.Record,
			transcript: newByteRing(maxTranscriptBytes),
			storedText: stored.Transcript,
		}
	}
	m.mu.Unlock()
	return m.persist()
}

func (m *Manager) List() []protocol.SessionRecord {
	m.mu.RLock()
	sessions := make([]*liveSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.RUnlock()

	records := make([]protocol.SessionRecord, 0, len(sessions))
	metadataChanged := false
	for _, session := range sessions {
		if session.refreshForegroundAgent(m.agents) {
			metadataChanged = true
		}
		session.refreshAttention()
		records = append(records, session.snapshot())
	}
	if metadataChanged {
		_ = m.persist()
	}

	sort.Slice(records, func(left, right int) bool {
		return records[left].CreatedAt > records[right].CreatedAt
	})
	return records
}

func (m *Manager) RunningSessions() []protocol.SessionRecord {
	records := m.List()
	running := make([]protocol.SessionRecord, 0, len(records))
	for _, record := range records {
		if record.Status == "running" {
			running = append(running, record)
		}
	}
	return running
}

func (m *Manager) Create(params protocol.CreateParams) (protocol.SessionRecord, error) {
	title := strings.TrimSpace(params.Title)
	if title == "" {
		return protocol.SessionRecord{}, errors.New("a session title is required")
	}
	if utf8.RuneCountInString(title) > 200 {
		return protocol.SessionRecord{}, errors.New("the session title must be 200 characters or fewer")
	}

	spec, ok := m.agents.Find(params.AgentID)
	if !ok {
		return protocol.SessionRecord{}, fmt.Errorf("unsupported agent %q", params.AgentID)
	}
	resolution, ok := agent.ResolveExecutable(spec)
	if !ok {
		return protocol.SessionRecord{}, fmt.Errorf("%s is not available in falknd's PATH", spec.DisplayName)
	}
	launchArguments, err := agent.LaunchArguments(spec, params.PermissionMode, params.AdditionalArguments)
	if err != nil {
		return protocol.SessionRecord{}, err
	}

	directory, err := workspace.Resolve(params.Directory)
	if err != nil {
		return protocol.SessionRecord{}, err
	}

	id, err := newSessionID()
	if err != nil {
		return protocol.SessionRecord{}, fmt.Errorf("create session ID: %w", err)
	}
	columns, rows := normalizedTerminalSize(params.TerminalColumns, params.TerminalRows)

	command := exec.Command(resolution.Executable, launchArguments...)
	command.Dir = directory
	command.Env = terminalEnvironment(resolution.Environment,
		"FALKN_SESSION_ID="+id,
		"FALKN_AGENT_ID="+spec.ID,
	)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: uint16(rows), Cols: uint16(columns)})
	if err != nil {
		return protocol.SessionRecord{}, fmt.Errorf("start %s: %w", spec.DisplayName, err)
	}

	session := &liveSession{
		record: protocol.SessionRecord{
			ID:               id,
			Title:            title,
			Directory:        directory,
			AgentID:          spec.ID,
			PreferredAgentID: spec.ID,
			AgentState:       "active",
			CreatedAt:        time.Now().Unix(),
			Status:           "running",
			Process:          spec.Executable,
			AttachedClients:  0,
			PermissionMode:   normalizedPermissionMode(params.PermissionMode),
			Kind:             "agent",
		},
		pty:        terminal,
		command:    command,
		transcript: newByteRing(maxTranscriptBytes),
		terminal: xterm.New(
			xterm.WithCols(columns),
			xterm.WithRows(rows),
			xterm.WithScrollback(2_000),
		),
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

func normalizedPermissionMode(value string) string {
	if value == agent.PermissionFullAccess {
		return agent.PermissionFullAccess
	}
	return agent.PermissionStandard
}

func (m *Manager) Transcript(sessionID string, historyLines int) (string, protocol.SessionRecord, error) {
	page, record, err := m.TranscriptPage(sessionID, historyLines, nil)
	return page.Transcript, record, err
}

func (m *Manager) TranscriptPage(
	sessionID string,
	historyLines int,
	beforeLine *int,
) (TranscriptPage, protocol.SessionRecord, error) {
	session, err := m.find(sessionID)
	if err != nil {
		return TranscriptPage{}, protocol.SessionRecord{}, err
	}
	session.refreshForegroundAgent(m.agents)
	record := session.snapshot()
	return session.transcriptPage(historyLines, beforeLine), record, nil
}

func (m *Manager) Resize(sessionID string, columns, rows int) error {
	session, err := m.running(sessionID)
	if err != nil {
		return err
	}
	columns, rows = normalizedTerminalSize(columns, rows)

	// Hold terminal output while the PTY notifies the agent of SIGWINCH. This
	// ensures any redraw is interpreted using the same dimensions as the PTY.
	session.terminalMu.Lock()
	defer session.terminalMu.Unlock()
	if err := pty.Setsize(session.pty, &pty.Winsize{Rows: uint16(rows), Cols: uint16(columns)}); err != nil {
		return fmt.Errorf("resize session terminal: %w", err)
	}
	session.terminal.Resize(columns, rows)
	return nil
}

func (m *Manager) Send(sessionID, text string) error {
	if text == "" {
		return errors.New("input cannot be empty")
	}
	if len(text) > maxInputBytes {
		return fmt.Errorf("input exceeds the %d-byte limit", maxInputBytes)
	}
	session, err := m.running(sessionID)
	if err != nil {
		return err
	}

	data := []byte(text)
	if strings.ContainsAny(text, "\r\n") {
		data = append([]byte("\x1b[200~"), data...)
		data = append(data, []byte("\x1b[201~")...)
	}
	if err := session.write(data); err != nil {
		return err
	}
	// Interactive agents distinguish a paste from the following submit key.
	// Keeping these as separate terminal events matches a physical keyboard.
	time.Sleep(15 * time.Millisecond)
	return session.write([]byte{'\r'})
}

func (m *Manager) SendKey(sessionID, key string) error {
	keys := map[string][]byte{
		"Enter":  {'\r'},
		"Escape": {0x1b},
		"Up":     {0x1b, '[', 'A'},
		"Down":   {0x1b, '[', 'B'},
		"C-c":    {0x03},
	}
	sequence, ok := keys[key]
	if !ok {
		return fmt.Errorf("unsupported key %q", key)
	}
	session, err := m.running(sessionID)
	if err != nil {
		return err
	}
	return session.write(sequence)
}

func (m *Manager) Stop(sessionID string) error {
	session, err := m.find(sessionID)
	if err != nil {
		return err
	}

	if session.snapshot().Status != "running" {
		return nil
	}
	transcript := session.renderedTranscript(2_000)
	if session.command != nil && session.command.Process != nil {
		if err := session.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("stop session process: %w", err)
		}
	}
	session.setStoppedTranscript("stopped", transcript)
	if session.pty != nil {
		_ = session.pty.Close()
	}
	return m.persist()
}

func (m *Manager) Delete(sessionID string) error {
	session, err := m.find(sessionID)
	if err != nil {
		return err
	}
	if session.snapshot().Status == "running" {
		if err := m.Stop(sessionID); err != nil {
			return err
		}
	}

	m.mu.Lock()
	if m.sessions[sessionID] == session {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	return m.persist()
}

func (m *Manager) Archive(sessionID string) error {
	session, err := m.find(sessionID)
	if err != nil {
		return err
	}
	if session.snapshot().Status == "running" {
		return errors.New("stop the session before archiving it")
	}
	session.setArchived(true)
	return m.persist()
}

func (m *Manager) Restore(sessionID string) error {
	session, err := m.find(sessionID)
	if err != nil {
		return err
	}
	session.setArchived(false)
	return m.persist()
}

func (m *Manager) Rename(sessionID, requestedTitle string) error {
	title := strings.TrimSpace(requestedTitle)
	if title == "" {
		return errors.New("a session title is required")
	}
	if utf8.RuneCountInString(title) > 200 {
		return errors.New("the session title must be 200 characters or fewer")
	}
	session, err := m.find(sessionID)
	if err != nil {
		return err
	}
	previous := session.setTitle(title)
	if previous == title {
		return nil
	}
	if err := m.persist(); err != nil {
		session.setTitle(previous)
		return err
	}
	return nil
}

func (m *Manager) SetNotificationsMuted(sessionID string, muted bool) error {
	session, err := m.find(sessionID)
	if err != nil {
		return err
	}
	session.outputMu.Lock()
	session.stateMu.Lock()
	unchanged := session.record.NotificationsMuted == muted
	session.record.NotificationsMuted = muted
	session.updateNotificationSuppressionLocked()
	session.stateMu.Unlock()
	session.outputMu.Unlock()
	if unchanged {
		return nil
	}
	return m.persist()
}

func (m *Manager) find(sessionID string) (*liveSession, error) {
	m.mu.RLock()
	session := m.sessions[sessionID]
	m.mu.RUnlock()
	if session == nil {
		return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}
	return session, nil
}

func (m *Manager) running(sessionID string) (*liveSession, error) {
	session, err := m.find(sessionID)
	if err != nil {
		return nil, err
	}
	if session.snapshot().Status != "running" {
		return nil, fmt.Errorf("session %q is no longer running: %w", sessionID, ErrSessionNotRunning)
	}
	return session, nil
}

func (s *liveSession) write(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.pty.Write(data)
	if err != nil {
		return fmt.Errorf("write to session: %w", err)
	}
	return nil
}

func (s *liveSession) readOutput() {
	defer s.pty.Close()
	buffer := make([]byte, 32*1024)
	for {
		count, err := s.pty.Read(buffer)
		if count > 0 {
			chunk := append([]byte(nil), buffer[:count]...)
			s.outputMu.Lock()
			s.transcript.Write(chunk)
			s.outputSequence++
			attachmentsChanged := false
			for id, subscriber := range s.subscribers {
				if subscriber.enqueue(chunk) {
					continue
				}
				delete(s.subscribers, id)
				subscriber.close()
				attachmentsChanged = true
			}
			if attachmentsChanged {
				s.stateMu.Lock()
				s.updateNotificationSuppressionLocked()
				s.stateMu.Unlock()
			}
			s.outputMu.Unlock()
			s.terminalMu.Lock()
			_, _ = s.terminal.Write(chunk)
			s.captureTerminalHistoryLocked()
			s.terminalMu.Unlock()
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.transcript.Write([]byte("\n[falknd stopped reading terminal output]\n"))
			}
			return
		}
	}
}

func (s *liveSession) renderedTranscript(historyLines int) string {
	return s.transcriptPage(historyLines, nil).Transcript
}

func (s *liveSession) transcriptPage(historyLines int, beforeLine *int) TranscriptPage {
	lines := s.renderedTranscriptLines()
	totalLines := len(lines)
	endLine := totalLines
	if beforeLine != nil {
		endLine = min(max(*beforeLine, 0), totalLines)
	}
	if historyLines <= 0 {
		historyLines = 600
	}
	historyLines = min(historyLines, maximumTerminalHistoryLines)
	startLine := max(0, endLine-historyLines)
	pageLines := lines[startLine:endLine]
	return TranscriptPage{
		Transcript: strings.Join(pageLines, "\n"),
		StartLine:  startLine,
		EndLine:    endLine,
		TotalLines: totalLines,
		HasEarlier: startLine > 0,
		HistoryID:  transcriptHistoryID(lines),
	}
}

func (s *liveSession) renderedTranscriptLines() []string {
	if s.terminal == nil {
		s.stateMu.RLock()
		defer s.stateMu.RUnlock()
		return cleanedTranscriptLines([]byte(s.storedText))
	}

	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	lines := s.renderedTerminalLinesLocked()
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	lines = compactBlankLines(lines, 2)
	if len(lines) == 0 {
		return cleanedTranscriptLines(s.transcript.Bytes())
	}
	if len(lines) > maximumTerminalHistoryLines {
		lines = lines[len(lines)-maximumTerminalHistoryLines:]
	}
	return lines
}

func transcriptHistoryID(lines []string) string {
	const fingerprintLines = 4
	end := min(len(lines), fingerprintLines)
	fingerprint := sha256.Sum256([]byte(strings.Join(lines[:end], "\n")))
	return hex.EncodeToString(fingerprint[:8])
}

func normalizedTerminalSize(columns, rows int) (int, int) {
	if columns == 0 {
		columns = defaultTerminalCols
	}
	if rows == 0 {
		rows = defaultTerminalRows
	}
	columns = min(max(columns, minimumTerminalCols), maximumTerminalCols)
	rows = min(max(rows, minimumTerminalRows), maximumTerminalRows)
	return columns, rows
}

func compactBlankLines(lines []string, maximumRun int) []string {
	compacted := make([]string, 0, len(lines))
	emptyRun := 0
	for _, line := range lines {
		if line == "" {
			emptyRun++
			if emptyRun > maximumRun {
				continue
			}
		} else {
			emptyRun = 0
		}
		compacted = append(compacted, line)
	}
	return compacted
}

func (m *Manager) waitForExit(s *liveSession) {
	err := s.command.Wait()
	if err != nil {
		s.transcript.Write([]byte(fmt.Sprintf("\n[falknd: process exited: %v]\n", err)))
	}
	if s.snapshot().Status == "running" {
		s.setStoppedTranscript("ended", s.renderedTranscript(2_000))
		_ = m.persist()
	}
	s.closeSubscribers()
}

func (m *Manager) persist() error {
	m.mu.RLock()
	path := m.storagePath
	sessions := make([]*liveSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.RUnlock()
	if path == "" {
		return nil
	}

	state := persistedState{Version: 1, Sessions: make([]persistedSession, 0, len(sessions))}
	for _, session := range sessions {
		stored := session.persistedSnapshot()
		if stored.Record.Status != "running" && stored.Transcript == "" {
			stored.Transcript = session.renderedTranscript(2_000)
		}
		state.Sessions = append(state.Sessions, stored)
	}
	sort.Slice(state.Sessions, func(left, right int) bool {
		return state.Sessions[left].Record.CreatedAt > state.Sessions[right].Record.CreatedAt
	})
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create session state directory: %w", err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return fmt.Errorf("write session state: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace session state: %w", err)
	}
	return nil
}

func newSessionID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "falkn-" + hex.EncodeToString(bytes), nil
}
