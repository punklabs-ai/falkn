package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/punklabs-ai/falkn/internal/protocol"
)

func TestNotificationMuteUsesDedicatedAttachFrame(t *testing.T) {
	serverConnection, clientConnection := net.Pipe()
	defer serverConnection.Close()
	defer clientConnection.Close()
	writer := &attachFrameWriter{connection: clientConnection}

	done := make(chan error, 1)
	go func() {
		done <- writer.notificationsMuted(true)
	}()
	frame := make([]byte, 6)
	if _, err := io.ReadFull(serverConnection, frame); err != nil {
		t.Fatal(err)
	}
	if frame[0] != attachNotifyFrame || binary.BigEndian.Uint32(frame[1:5]) != 1 || frame[5] != 1 {
		t.Fatalf("notification frame = %v", frame)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNotificationHotkeyIsConsumed(t *testing.T) {
	input, keyboard, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer keyboard.Close()
	serverConnection, clientConnection := net.Pipe()
	defer serverConnection.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		readAttachInput(input, &attachFrameWriter{connection: clientConnection}, attachInputHandlers{prefix: DefaultAttachPrefix, supportsNotifications: true})
	}()

	if _, err := keyboard.Write([]byte{DefaultAttachPrefix, 'm'}); err != nil {
		t.Fatal(err)
	}
	kind, payload := readTestAttachFrame(t, serverConnection)
	if kind != attachNotifyFrame || !bytes.Equal(payload, []byte{1}) {
		t.Fatalf("hotkey frame kind=%q payload=%v", kind, payload)
	}

	if _, err := keyboard.Write([]byte{DefaultAttachPrefix, 'q'}); err != nil {
		t.Fatal(err)
	}
	kind, payload = readTestAttachFrame(t, serverConnection)
	if kind != attachDetachFrame || len(payload) != 0 {
		t.Fatalf("detach frame kind=%q payload=%v", kind, payload)
	}
	<-done
}

func TestAttachInputPreservesTerminalBytes(t *testing.T) {
	input, keyboard, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer keyboard.Close()
	serverConnection, clientConnection := net.Pipe()
	defer serverConnection.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		readAttachInput(input, &attachFrameWriter{connection: clientConnection}, attachInputHandlers{prefix: DefaultAttachPrefix, supportsNotifications: true})
	}()

	expected := []byte("\x1b[31mraw\x1b[0m")
	if _, err := keyboard.Write(expected); err != nil {
		t.Fatal(err)
	}
	kind, payload := readTestAttachFrame(t, serverConnection)
	if kind != attachInputFrame || !bytes.Equal(payload, expected) {
		t.Fatalf("input frame kind=%q payload=%q, want %q", kind, payload, expected)
	}

	if _, err := keyboard.Write([]byte{DefaultAttachPrefix, 'q'}); err != nil {
		t.Fatal(err)
	}
	_, _ = readTestAttachFrame(t, serverConnection)
	<-done
}

func TestDoubledPrefixSendsOneLiteralPrefixByte(t *testing.T) {
	input, keyboard, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer keyboard.Close()
	serverConnection, clientConnection := net.Pipe()
	defer serverConnection.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		readAttachInput(input, &attachFrameWriter{connection: clientConnection}, attachInputHandlers{prefix: DefaultAttachPrefix, supportsNotifications: true})
	}()

	if _, err := keyboard.Write([]byte{DefaultAttachPrefix, DefaultAttachPrefix}); err != nil {
		t.Fatal(err)
	}
	kind, payload := readTestAttachFrame(t, serverConnection)
	if kind != attachInputFrame || !bytes.Equal(payload, []byte{DefaultAttachPrefix}) {
		t.Fatalf("doubled prefix frame kind=%q payload=%v", kind, payload)
	}

	if _, err := keyboard.Write([]byte{DefaultAttachPrefix, 'q'}); err != nil {
		t.Fatal(err)
	}
	_, _ = readTestAttachFrame(t, serverConnection)
	<-done
}

func TestConfiguredPrefixReplacesTheDefault(t *testing.T) {
	input, keyboard, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer keyboard.Close()
	serverConnection, clientConnection := net.Pipe()
	defer serverConnection.Close()

	const prefix = byte(0x01) // Ctrl-A
	done := make(chan struct{})
	go func() {
		defer close(done)
		readAttachInput(input, &attachFrameWriter{connection: clientConnection}, attachInputHandlers{prefix: prefix, supportsNotifications: true})
	}()

	// The former default must reach the session untouched once it is not the
	// configured prefix.
	if _, err := keyboard.Write([]byte{0x02, 'n'}); err != nil {
		t.Fatal(err)
	}
	kind, payload := readTestAttachFrame(t, serverConnection)
	if kind != attachInputFrame || !bytes.Equal(payload, []byte{0x02, 'n'}) {
		t.Fatalf("released prefix frame kind=%q payload=%v", kind, payload)
	}

	if _, err := keyboard.Write([]byte{prefix, 'q'}); err != nil {
		t.Fatal(err)
	}
	kind, payload = readTestAttachFrame(t, serverConnection)
	if kind != attachDetachFrame || len(payload) != 0 {
		t.Fatalf("detach frame kind=%q payload=%v", kind, payload)
	}
	<-done
}

func TestAttachPresenceWatcherImmediatelyMarksLegacyPresence(t *testing.T) {
	directory := t.TempDir()
	sessionID := "falkn-0123456789abcdef"
	done := make(chan struct{})
	bar := newAttachStatusBar(io.Discard, 80, 24, "Infra", "codex", false, true)
	go watchAttachStatus(Paths{
		Socket:               filepath.Join(directory, "missing.sock"),
		NotificationConfig:   filepath.Join(directory, "missing-notifications.json"),
		NotificationPresence: filepath.Join(directory, "presence"),
	}, sessionID, bar, done)
	defer close(done)

	presence := filepath.Join(directory, "presence", sessionID)
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(presence); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attached-client presence heartbeat was not written")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInitialAttachFooterIncludesTheHotkeyAndSessionNumber(t *testing.T) {
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 90, 24, "two", "shell", false, true)
	records := []protocol.SessionRecord{
		{ID: "falkn-one", Title: "one", Status: "running", CreatedAt: 1},
		{ID: "falkn-two", Title: "two", Status: "running", CreatedAt: 2},
	}
	if err := initializeAttachStatusBar(bar, "falkn-two", `Ctrl-\`, records); err != nil {
		t.Fatal(err)
	}

	rendered := output.String()
	if !strings.Contains(rendered, `Ctrl-\ [2] ↑ sessions`) {
		t.Fatalf("first footer omitted the session indicator: %q", rendered)
	}
	if strings.Count(rendered, "Falkn") != 1 {
		t.Fatalf("initialization drew an incomplete footer first: %q", rendered)
	}
}

func TestCreateAttachedSessionUsesCurrentDirectoryAndHandsOff(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "falkn-create-test.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "falknd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	requests := make(chan protocol.Request, 2)
	daemonDone := make(chan error, 1)
	go func() {
		for count := 0; count < 2; count++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				daemonDone <- acceptErr
				return
			}
			var request protocol.Request
			if decodeErr := json.NewDecoder(connection).Decode(&request); decodeErr != nil {
				_ = connection.Close()
				daemonDone <- decodeErr
				return
			}
			requests <- request

			var response protocol.Response
			switch request.Method {
			case "list":
				response = protocol.Success(request.RequestID, protocol.ListResult{
					Sessions: []protocol.SessionRecord{{
						ID: "falkn-current", Directory: "/tmp/falkn-project", Status: "running",
					}},
				})
			case "shell.create":
				response = protocol.Success(request.RequestID, protocol.CreateResult{
					Session: protocol.SessionRecord{ID: "falkn-created", Status: "running"},
				})
			default:
				response = protocol.Failure(request.RequestID, "unexpected_method", request.Method)
			}
			encodeErr := json.NewEncoder(connection).Encode(response)
			_ = connection.Close()
			if encodeErr != nil {
				daemonDone <- encodeErr
				return
			}
		}
		daemonDone <- nil
	}()

	serverConnection, clientConnection := net.Pipe()
	defer serverConnection.Close()
	bar := newAttachStatusBar(io.Discard, 100, 30, "project", "shell", false, true)
	createDone := make(chan error, 1)
	go func() {
		createDone <- createAttachedSession(
			socket,
			"falkn-current",
			bar,
			&attachFrameWriter{connection: clientConnection},
		)
	}()

	kind, payload := readTestAttachFrame(t, serverConnection)
	if kind != attachDetachFrame || len(payload) != 0 {
		t.Fatalf("new-session frame kind=%q payload=%v", kind, payload)
	}
	if err := <-createDone; err != nil {
		t.Fatal(err)
	}
	if err := <-daemonDone; err != nil {
		t.Fatal(err)
	}
	if target := bar.requestedHandoffSessionID(); target != "falkn-created" {
		t.Fatalf("handoff target = %q, want falkn-created", target)
	}

	listRequest := <-requests
	createRequest := <-requests
	if listRequest.Method != "list" || createRequest.Method != "shell.create" {
		t.Fatalf("daemon methods = %q, %q", listRequest.Method, createRequest.Method)
	}
	var params protocol.ShellCreateParams
	if err := json.Unmarshal(createRequest.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Directory != "/tmp/falkn-project" || params.Title != "falkn-project" {
		t.Fatalf("create params directory/title = %q/%q", params.Directory, params.Title)
	}
	if params.TerminalColumns != 100 || params.TerminalRows != 29 {
		t.Fatalf(
			"create terminal size = %dx%d, want 100x29",
			params.TerminalColumns,
			params.TerminalRows,
		)
	}
}

func readTestAttachFrame(t *testing.T, connection net.Conn) (byte, []byte) {
	t.Helper()
	header := make([]byte, 5)
	if _, err := io.ReadFull(connection, header); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[1:]))
	if _, err := io.ReadFull(connection, payload); err != nil {
		t.Fatal(err)
	}
	return header[0], payload
}
