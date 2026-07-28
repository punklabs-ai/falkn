package server

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/punklabs-ai/falkn/internal/notifications"
	"github.com/punklabs-ai/falkn/internal/protocol"
	"golang.org/x/term"
)

func AttachClient(sessionID string, input, output *os.File) error {
	if !term.IsTerminal(int(input.Fd())) || !term.IsTerminal(int(output.Fd())) {
		return errors.New("attach requires an interactive terminal")
	}
	prefix, err := AttachPrefix()
	if err != nil {
		return err
	}
	paths, err := RuntimePaths()
	if err != nil {
		return err
	}
	connection, err := connect(paths.Socket)
	if err != nil {
		if err := startDaemon(paths); err != nil {
			return err
		}
		connection, err = waitForDaemon(paths.Socket, 3*time.Second)
		if err != nil {
			return err
		}
	}
	defer connection.Close()

	columns, rows, err := term.GetSize(int(output.Fd()))
	if err != nil || columns <= 0 || rows <= 0 {
		columns, rows = 80, 24
	}
	contentRows := terminalContentRows(columns, rows)
	request := protocol.Request{
		Version: protocol.Version, RequestID: fmt.Sprintf("cli-%d-%d", os.Getpid(), time.Now().UnixNano()), Method: "attach",
	}
	request.Params, _ = json.Marshal(protocol.AttachParams{
		SessionID: sessionID, Columns: columns, Rows: contentRows,
	})
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := connection.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("send attach request: %w", err)
	}
	reader := bufio.NewReader(connection)
	responseLine, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read attach response: %w", err)
	}
	var response struct {
		OK     bool                  `json:"ok"`
		Result protocol.AttachResult `json:"result"`
		Error  *protocol.RPCError    `json:"error,omitempty"`
	}
	if err := json.Unmarshal(responseLine, &response); err != nil {
		return errors.New("falknd returned a malformed attach response")
	}
	if !response.OK {
		if response.Error != nil {
			return &AttachRejectedError{Code: response.Error.Code, Message: response.Error.Message}
		}
		return errors.New("falknd rejected the attach request")
	}
	_ = connection.SetDeadline(time.Time{})

	// Read the session list before taking over the screen. The first footer frame
	// can then include both the prefix and this session's stable number, instead
	// of briefly drawing an incomplete footer on every switch.
	records, recordsErr := fetchAttachSessions(paths.Socket)
	state, err := term.MakeRaw(int(input.Fd()))
	if err != nil {
		return fmt.Errorf("enter raw terminal mode: %w", err)
	}
	defer term.Restore(int(input.Fd()), state)
	// Attaching replays the session's own output. Clear the screen first so the
	// session takes the terminal over rather than being appended to whatever was
	// there, which is what makes switching read as a switch instead of as more
	// output from the session being left. The scrollback is untouched, so what
	// was on screen is still there to scroll back to.
	_, _ = fmt.Fprint(output, "\x1b[H\x1b[2J")
	supportsNotifications := containsFeature(response.Result.Features, "notification_mute")
	statusBar := newAttachStatusBar(
		output,
		columns,
		rows,
		response.Result.Session.Title,
		response.Result.Session.AgentID,
		response.Result.Session.NotificationsMuted,
		supportsNotifications,
	)
	if recordsErr != nil {
		records = nil
	}
	_ = initializeAttachStatusBar(statusBar, sessionID, AttachPrefixName(prefix), records)
	defer statusBar.close()
	statusUpdatesDone := make(chan struct{})
	defer close(statusUpdatesDone)
	go watchAttachStatus(paths, sessionID, statusBar, statusUpdatesDone)

	writer := &attachFrameWriter{connection: connection}
	menu := newAttachMenu(statusBar, writer, paths.Socket, sessionID)
	resizes := make(chan os.Signal, 1)
	signal.Notify(resizes, syscall.SIGWINCH)
	defer signal.Stop(resizes)
	go func() {
		for range resizes {
			newColumns, newRows, sizeErr := term.GetSize(int(output.Fd()))
			if sizeErr == nil && newColumns > 0 && newRows > 0 {
				_ = statusBar.resize(newColumns, newRows)
				// The session's height depends on whether the switcher is open,
				// so it comes from the bar rather than the terminal size.
				_ = writer.resize(statusBar.columnsAndContentRows())
			}
		}
	}()

	detachRequested := make(chan struct{}, 1)
	go func() {
		readAttachInput(input, writer, attachInputHandlers{
			prefix:                prefix,
			notificationsMuted:    response.Result.Session.NotificationsMuted,
			supportsNotifications: supportsNotifications,
			onNotificationsMuted:  func(muted bool) { _ = statusBar.setNotificationsMuted(muted) },
			onNewSession: func() {
				_ = createAttachedSession(paths.Socket, sessionID, statusBar, writer)
			},
			onDetach: func() { detachRequested <- struct{}{} },
			onSwitch: func(request sessionSwitch) {
				switchAttachedSession(paths.Socket, sessionID, request, statusBar, writer)
			},
			onPrefixArmed: func(armed bool) { _ = statusBar.setPrefixArmed(armed) },
			menu:          menu,
		})
	}()
	_, copyErr := io.Copy(statusBar, reader)
	if handoffSessionID := statusBar.requestedHandoffSessionID(); handoffSessionID != "" {
		_ = statusBar.close()
		return &AttachHandoffError{SessionID: handoffSessionID}
	}
	select {
	case <-detachRequested:
		_ = statusBar.close()
		_, _ = fmt.Fprint(output, "\r\n[falkn: detached; session is still running]\r\n")
		return nil
	default:
	}
	if copyErr != nil && !errors.Is(copyErr, net.ErrClosed) {
		return fmt.Errorf("read session output: %w", copyErr)
	}
	// A shell exits by closing its PTY, which closes this output stream. If
	// another session is still running, keep Falkn in control of the terminal
	// and hand it to the nearest predecessor instead of dropping the user back
	// to the host shell. An explicit prefix-Q detach returned above and must
	// never take this path.
	if records, err := fetchAttachSessions(paths.Socket); err == nil {
		if target := selectEndedSessionFallback(records, sessionID); target != "" {
			_ = statusBar.close()
			return &AttachHandoffError{SessionID: target}
		}
	}
	return nil
}

// initializeAttachStatusBar draws the first footer only after all immediately
// available state is installed. In particular, the hotkey and session number
// must not disappear between the old attachment and the first status poll for
// the new one.
func initializeAttachStatusBar(
	bar *attachStatusBar,
	sessionID, prefixName string,
	records []protocol.SessionRecord,
) error {
	bar.setPrefixName(prefixName)
	if records != nil {
		return bar.updateSessions(sessionID, records)
	}
	return bar.render()
}

type attachFrameWriter struct {
	connection net.Conn
	mu         sync.Mutex
}

func (w *attachFrameWriter) frame(kind byte, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	header := make([]byte, 5)
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := w.connection.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.connection.Write(payload)
		return err
	}
	return nil
}

func (w *attachFrameWriter) resize(columns, rows int) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint16(payload[0:2], uint16(columns))
	binary.BigEndian.PutUint16(payload[2:4], uint16(rows))
	return w.frame(attachResizeFrame, payload)
}

func (w *attachFrameWriter) focus(focused bool) error {
	value := byte(0)
	if focused {
		value = 1
	}
	return w.frame(attachFocusFrame, []byte{value})
}

func (w *attachFrameWriter) notificationsMuted(muted bool) error {
	value := byte(0)
	if muted {
		value = 1
	}
	return w.frame(attachNotifyFrame, []byte{value})
}

func containsFeature(features []string, expected string) bool {
	for _, feature := range features {
		if feature == expected {
			return true
		}
	}
	return false
}

func watchAttachStatus(paths Paths, sessionID string, statusBar *attachStatusBar, done <-chan struct{}) {
	// The presence heartbeat also protects clients connected to a notification
	// watcher from before attached_clients was part of the protocol.
	_ = notifications.MarkActive(paths.NotificationPresence, sessionID)
	// Fill the footer before the first tick. Waiting two seconds for it leaves
	// the session unnumbered and its siblings uncounted for long enough to see,
	// and switching sessions goes through a fresh client every time, so the gap
	// would show on every switch.
	if records, err := fetchAttachSessions(paths.Socket); err == nil {
		_ = statusBar.updateSessions(sessionID, records)
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			_ = notifications.MarkActive(paths.NotificationPresence, sessionID)
			_ = ensureNotificationWatcher(paths)
			records, err := fetchAttachSessions(paths.Socket)
			if err == nil {
				_ = statusBar.updateSessions(sessionID, records)
			}
		}
	}
}

// fetchAttachSessions returns the local daemon's whole session list. The footer
// and the switcher both describe the other sessions, so they share one poll
// rather than each asking for the same list.
func fetchAttachSessions(socket string) ([]protocol.SessionRecord, error) {
	var result protocol.ListResult
	if err := callAttachDaemon(socket, "list", protocol.EmptyParams{}, &result); err != nil {
		return nil, err
	}
	return result.Sessions, nil
}

// callAttachDaemon issues a short control request alongside the long-lived
// attach connection. Session polling and creating a new shell use this path so
// neither operation can interfere with the attached session's frame stream.
func callAttachDaemon(socket, method string, params, destination any) error {
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return err
	}
	connection, err := connect(socket)
	if err != nil {
		return err
	}
	defer connection.Close()
	timeout := time.Second
	if method == "shell.create" {
		timeout = 10 * time.Second
	}
	_ = connection.SetDeadline(time.Now().Add(timeout))

	request := protocol.Request{
		Version: protocol.Version, RequestID: fmt.Sprintf("attach-control-%d", time.Now().UnixNano()), Method: method,
	}
	request.Params = encodedParams
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return err
	}
	if unixConnection, ok := connection.(*net.UnixConn); ok {
		_ = unixConnection.CloseWrite()
	}
	var response struct {
		OK     bool               `json:"ok"`
		Result json.RawMessage    `json:"result"`
		Error  *protocol.RPCError `json:"error,omitempty"`
	}
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		return err
	}
	if !response.OK {
		if response.Error != nil {
			return errors.New(response.Error.Message)
		}
		return fmt.Errorf("falknd rejected the %s request", method)
	}
	if destination == nil {
		return nil
	}
	if err := json.Unmarshal(response.Result, destination); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

// switchableSessions lists the running sessions the switcher can move between,
// oldest first so the order a user cycles through stays stable as sessions
// come and go.
func switchableSessions(records []protocol.SessionRecord) []protocol.SessionRecord {
	running := make([]protocol.SessionRecord, 0, len(records))
	for _, record := range records {
		if record.Status == "running" && !record.Archived {
			running = append(running, record)
		}
	}
	sort.Slice(running, func(left, right int) bool {
		return sessionComesBefore(running[left], running[right])
	})
	return running
}

// sessionComesBefore is the switcher's stable session order. Timestamps have
// second precision, so IDs keep positions and footer numbers deterministic when
// two sessions are opened together.
func sessionComesBefore(left, right protocol.SessionRecord) bool {
	if left.CreatedAt == right.CreatedAt {
		return left.ID < right.ID
	}
	return left.CreatedAt < right.CreatedAt
}

// selectEndedSessionFallback picks where the terminal goes when its foreground
// session ends. Prefer the nearest older session, which makes exiting session 2
// feel like returning to session 1. If the oldest session ended, use the next
// newer one. A clean stream close while the current session is still reported
// running is not an exit and must not cause an unexplained switch.
func selectEndedSessionFallback(records []protocol.SessionRecord, currentID string) string {
	var current *protocol.SessionRecord
	for index := range records {
		if records[index].ID == currentID {
			current = &records[index]
			break
		}
	}
	running := switchableSessions(records)
	if len(running) == 0 {
		return ""
	}
	if current == nil {
		// A session deleted as it ended no longer has a position to compare.
		// The newest remaining one is the least surprising place to return.
		return running[len(running)-1].ID
	}
	if current.Status == "running" {
		return ""
	}

	previous := ""
	for _, candidate := range running {
		if sessionComesBefore(candidate, *current) {
			previous = candidate.ID
			continue
		}
		if previous != "" {
			return previous
		}
		return candidate.ID
	}
	return previous
}

// switchAttachedSession hands this client to another local session. The list is
// fetched fresh rather than reused from the status poll, so a session that
// ended in the last two seconds is not a switch target. Closing the connection
// unblocks the output copy, which then reports the handoff and lets the CLI
// replace the process.
func switchAttachedSession(
	socket, sessionID string,
	request sessionSwitch,
	statusBar *attachStatusBar,
	writer *attachFrameWriter,
) {
	records, err := fetchAttachSessions(socket)
	if err != nil {
		return
	}
	target := selectSwitchTarget(switchableSessions(records), sessionID, request)
	if target == "" || target == sessionID {
		return
	}
	if !statusBar.requestHandoff(target) {
		return
	}
	_ = writer.frame(attachDetachFrame, nil)
	_ = writer.connection.Close()
}

// createAttachedSession opens another persistent shell in the attached
// session's recorded directory and hands this terminal to it. The original
// session is detached, not stopped, so it remains available in the switcher.
func createAttachedSession(
	socket, sessionID string,
	statusBar *attachStatusBar,
	writer *attachFrameWriter,
) error {
	records, err := fetchAttachSessions(socket)
	if err != nil {
		return err
	}
	var current *protocol.SessionRecord
	for index := range records {
		if records[index].ID == sessionID {
			current = &records[index]
			break
		}
	}
	if current == nil {
		return fmt.Errorf("attached session %s is no longer listed", sessionID)
	}

	directory := current.Directory
	title := filepath.Base(filepath.Clean(directory))
	if title == "." || title == string(filepath.Separator) || title == "" {
		title = "Falkn shell"
	}
	columns, rows := statusBar.columnsAndContentRows()
	var created protocol.CreateResult
	if err := callAttachDaemon(socket, "shell.create", protocol.ShellCreateParams{
		Title: title, Directory: directory, TerminalColumns: columns, TerminalRows: rows,
	}, &created); err != nil {
		return err
	}
	target := created.Session.ID
	if !validAttachHandoffSessionID(target) {
		return errors.New("falknd returned an invalid session ID")
	}
	if !statusBar.requestHandoff(target) {
		return errors.New("another session handoff is already in progress")
	}
	frameErr := writer.frame(attachDetachFrame, nil)
	closeErr := writer.connection.Close()
	if frameErr != nil {
		return frameErr
	}
	return closeErr
}

// AttachRejectedError carries the daemon's reason for refusing an attachment,
// so a caller that chose the session itself can tell a session that has since
// ended from a genuine failure.
type AttachRejectedError struct {
	Code    string
	Message string
}

func (e *AttachRejectedError) Error() string { return e.Message }

// SessionGone reports whether a session named in a request has ended or been
// removed. A session listed a moment ago can be gone by the time it is
// attached, which is a reason to open a new one rather than to give up.
func SessionGone(err error) bool {
	var rejected *AttachRejectedError
	if !errors.As(err, &rejected) {
		return false
	}
	return rejected.Code == "session_not_running" || rejected.Code == "session_not_found"
}
