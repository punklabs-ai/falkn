package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/punklabs-ai/falkn/internal/agent"
	"github.com/punklabs-ai/falkn/internal/protocol"
)

func TestManagerRunsIndependentReconnectablePTYs(t *testing.T) {
	tools := t.TempDir()
	writeFakeAgent(t, tools, "falkn-test-agent")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	registry := agent.NewRegistry([]agent.Spec{{ID: "test", DisplayName: "Test", Executable: "falkn-test-agent"}})
	manager := NewManager(registry)
	first, err := manager.Create(protocol.CreateParams{Title: "First", Directory: t.TempDir(), AgentID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create(protocol.CreateParams{Title: "Second", Directory: t.TempDir(), AgentID: "test"})
	if err != nil {
		t.Fatal(err)
	}

	waitForTranscript(t, manager, first.ID, "ready")
	waitForTranscript(t, manager, second.ID, "ready")
	if err := manager.Send(first.ID, "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Send(second.ID, "beta"); err != nil {
		t.Fatal(err)
	}
	firstOutput := waitForTranscript(t, manager, first.ID, "echo:alpha")
	secondOutput := waitForTranscript(t, manager, second.ID, "echo:beta")
	if strings.Contains(firstOutput, "echo:beta") || strings.Contains(secondOutput, "echo:alpha") {
		t.Fatal("session output leaked between PTYs")
	}

	if err := manager.Send(first.ID, "quit"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Send(second.ID, "quit"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerUsesAndResizesMobileTerminalViewport(t *testing.T) {
	tools := t.TempDir()
	writeFakeAgent(t, tools, "falkn-test-agent")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	registry := agent.NewRegistry([]agent.Spec{{ID: "test", DisplayName: "Test", Executable: "falkn-test-agent"}})
	manager := NewManager(registry)
	record, err := manager.Create(protocol.CreateParams{
		Title: "Sized", Directory: t.TempDir(), AgentID: "test", TerminalColumns: 52, TerminalRows: 36,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.find(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertPTYSize(t, session.pty, 52, 36)

	if err := manager.Resize(record.ID, 60, 44); err != nil {
		t.Fatal(err)
	}
	assertPTYSize(t, session.pty, 60, 44)

	if err := manager.Send(record.ID, "quit"); err != nil {
		t.Fatal(err)
	}
}

func TestManagerStopsSessionAndRetainsTranscript(t *testing.T) {
	tools := t.TempDir()
	writeFakeAgent(t, tools, "falkn-test-agent")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	registry := agent.NewRegistry([]agent.Spec{{ID: "test", DisplayName: "Test", Executable: "falkn-test-agent"}})
	manager := NewManager(registry)
	record, err := manager.Create(protocol.CreateParams{
		Title: "Disposable", Directory: t.TempDir(), AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTranscript(t, manager, record.ID, "ready")

	if err := manager.Stop(record.ID); err != nil {
		t.Fatal(err)
	}
	listed := manager.List()
	if len(listed) != 1 || listed[0].Status != "stopped" {
		t.Fatalf("stopped session list was %#v", listed)
	}
	transcript, stoppedRecord, err := manager.Transcript(record.ID, 600)
	if err != nil {
		t.Fatal(err)
	}
	if stoppedRecord.Status != "stopped" || !strings.Contains(transcript, "ready") {
		t.Fatalf("stopped transcript=%q status=%q", transcript, stoppedRecord.Status)
	}
}

func TestManagerDeletesSession(t *testing.T) {
	tools := t.TempDir()
	writeFakeAgent(t, tools, "falkn-test-agent")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	registry := agent.NewRegistry([]agent.Spec{{ID: "test", DisplayName: "Test", Executable: "falkn-test-agent"}})
	manager := NewManager(registry)
	record, err := manager.Create(protocol.CreateParams{
		Title: "Disposable", Directory: t.TempDir(), AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTranscript(t, manager, record.ID, "ready")

	if err := manager.Delete(record.ID); err != nil {
		t.Fatal(err)
	}
	if len(manager.List()) != 0 {
		t.Fatal("deleted session remained in the session list")
	}
	if _, _, err := manager.Transcript(record.ID, 600); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("transcript after deletion returned %v, want ErrSessionNotFound", err)
	}
}

func TestArchivedSessionPersistsAndCanBeRestored(t *testing.T) {
	tools := t.TempDir()
	writeFakeAgent(t, tools, "falkn-test-agent")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	registry := agent.NewRegistry([]agent.Spec{{ID: "test", DisplayName: "Test", Executable: "falkn-test-agent"}})
	statePath := filepath.Join(t.TempDir(), "sessions.json")

	manager := NewManager(registry)
	if err := manager.EnablePersistence(statePath); err != nil {
		t.Fatal(err)
	}
	record, err := manager.Create(protocol.CreateParams{
		Title: "Archived", Directory: t.TempDir(), AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTranscript(t, manager, record.ID, "ready")
	if err := manager.Stop(record.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Archive(record.ID); err != nil {
		t.Fatal(err)
	}

	reloaded := NewManager(registry)
	if err := reloaded.EnablePersistence(statePath); err != nil {
		t.Fatal(err)
	}
	listed := reloaded.List()
	if len(listed) != 1 || !listed[0].Archived || listed[0].Status != "stopped" {
		t.Fatalf("persisted archive was %#v", listed)
	}
	transcript, _, err := reloaded.Transcript(record.ID, 600)
	if err != nil || !strings.Contains(transcript, "ready") {
		t.Fatalf("persisted transcript=%q error=%v", transcript, err)
	}
	if err := reloaded.Restore(record.ID); err != nil {
		t.Fatal(err)
	}
	if reloaded.List()[0].Archived {
		t.Fatal("restored session remained archived")
	}
}

func TestSessionRenameIsValidatedAndPersists(t *testing.T) {
	tools := t.TempDir()
	writeFakeAgent(t, tools, "falkn-test-agent")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	registry := agent.NewRegistry([]agent.Spec{{ID: "test", DisplayName: "Test", Executable: "falkn-test-agent"}})
	statePath := filepath.Join(t.TempDir(), "sessions.json")

	manager := NewManager(registry)
	if err := manager.EnablePersistence(statePath); err != nil {
		t.Fatal(err)
	}
	record, err := manager.Create(protocol.CreateParams{
		Title: "Untitled shell", Directory: t.TempDir(), AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Delete(record.ID)

	if err := manager.Rename(record.ID, "  Production incident  "); err != nil {
		t.Fatal(err)
	}
	if got := manager.List()[0].Title; got != "Production incident" {
		t.Fatalf("renamed title = %q", got)
	}
	if err := manager.Rename(record.ID, "   "); err == nil {
		t.Fatal("empty rename unexpectedly succeeded")
	}
	if got := manager.List()[0].Title; got != "Production incident" {
		t.Fatalf("invalid rename changed title to %q", got)
	}

	reloaded := NewManager(registry)
	if err := reloaded.EnablePersistence(statePath); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.List()[0].Title; got != "Production incident" {
		t.Fatalf("persisted title = %q", got)
	}
}

func TestRunningSessionMustBeStoppedBeforeArchive(t *testing.T) {
	tools := t.TempDir()
	writeFakeAgent(t, tools, "falkn-test-agent")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	registry := agent.NewRegistry([]agent.Spec{{ID: "test", DisplayName: "Test", Executable: "falkn-test-agent"}})
	manager := NewManager(registry)
	record, err := manager.Create(protocol.CreateParams{
		Title: "Running", Directory: t.TempDir(), AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Delete(record.ID)

	if err := manager.Archive(record.ID); err == nil {
		t.Fatal("archiving a running session unexpectedly succeeded")
	}
}

func TestNotificationMutePersistsWithSession(t *testing.T) {
	tools := t.TempDir()
	writeFakeAgent(t, tools, "falkn-test-agent")
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	registry := agent.NewRegistry([]agent.Spec{{ID: "test", DisplayName: "Test", Executable: "falkn-test-agent"}})
	statePath := filepath.Join(t.TempDir(), "sessions.json")

	manager := NewManager(registry)
	if err := manager.EnablePersistence(statePath); err != nil {
		t.Fatal(err)
	}
	record, err := manager.Create(protocol.CreateParams{
		Title: "Muted", Directory: t.TempDir(), AgentID: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, detach, _, err := manager.Attach(record.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetNotificationsMuted(record.ID, true); err != nil {
		t.Fatal(err)
	}
	if listed := manager.List(); len(listed) != 1 || !listed[0].NotificationsMuted || listed[0].AttachedClients != 1 {
		t.Fatalf("muted session list = %#v", listed)
	}

	reloaded := NewManager(registry)
	if err := reloaded.EnablePersistence(statePath); err != nil {
		t.Fatal(err)
	}
	if listed := reloaded.List(); len(listed) != 1 || !listed[0].NotificationsMuted || listed[0].AttachedClients != 0 || listed[0].FocusedClients != 1 {
		t.Fatalf("persisted muted session list = %#v", listed)
	}
	detach()
	if err := manager.Stop(record.ID); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.SetNotificationsMuted(record.ID, false); err != nil {
		t.Fatal(err)
	}
	if unmuted := reloaded.List()[0]; unmuted.NotificationsMuted || unmuted.FocusedClients != 0 {
		t.Fatalf("session remained suppressed after unmute: %#v", unmuted)
	}
}

func TestNormalizedTerminalSizeDefaultsAndBounds(t *testing.T) {
	columns, rows := normalizedTerminalSize(0, 0)
	if columns != defaultTerminalCols || rows != defaultTerminalRows {
		t.Fatalf("defaults were %dx%d", columns, rows)
	}
	columns, rows = normalizedTerminalSize(1, 1_000)
	if columns != minimumTerminalCols || rows != maximumTerminalRows {
		t.Fatalf("bounded size was %dx%d", columns, rows)
	}
}

func TestManagerCreatesAttachableShellAndTracksAttachment(t *testing.T) {
	tools := t.TempDir()
	shell := filepath.Join(tools, "test-shell")
	script := "#!/bin/sh\nif [ \"$1\" = -ic ]; then printf '\\n__FALKND_PATH__%s\\n' \"$PATH\"; exit 0; fi\nprintf 'shell-ready\\n'\nwhile IFS= read -r line; do printf 'shell:%s\\n' \"$line\"; done\n"
	if err := os.WriteFile(shell, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)

	manager := NewManager(agent.NewRegistry(nil))
	record, err := manager.CreateShell(protocol.ShellCreateParams{Title: "Local", Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Delete(record.ID)
	waitForTranscript(t, manager, record.ID, "shell-ready")

	_, _, detach, attached, err := manager.Attach(record.ID, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if attached.AttachedClients != 1 || attached.FocusedClients != 1 || attached.Kind != "shell" {
		t.Fatalf("attached record = %#v", attached)
	}
	listed := manager.List()[0]
	if listed.AttachedClients != 1 || listed.FocusedClients != 1 {
		t.Fatalf("attached record after list = %#v", listed)
	}
	detach()
	listed = manager.List()[0]
	if listed.AttachedClients != 0 || listed.FocusedClients != 0 {
		t.Fatalf("detached record = %#v", listed)
	}
}

func TestSupportedAgentNameRecognizesNodeLaunchers(t *testing.T) {
	if got := supportedAgentName("node", "/opt/homebrew/lib/node_modules/codex/codex.js --model test"); got != "codex" {
		t.Fatalf("detected %q, want codex", got)
	}
	if got := supportedAgentName("claude", "--permission-mode plan"); got != "claude" {
		t.Fatalf("detected %q, want claude", got)
	}
}

func assertPTYSize(t *testing.T, terminal *os.File, expectedColumns, expectedRows int) {
	t.Helper()
	rows, columns, err := pty.Getsize(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if columns != expectedColumns || rows != expectedRows {
		t.Fatalf("PTY size was %dx%d, want %dx%d", columns, rows, expectedColumns, expectedRows)
	}
}

func writeFakeAgent(t *testing.T, directory, name string) {
	t.Helper()
	path := filepath.Join(directory, name)
	script := "#!/bin/sh\nprintf 'ready\\n'\nwhile IFS= read -r line; do\n  printf 'echo:%s\\n' \"$line\"\n  [ \"$line\" = quit ] && exit 0\ndone\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func waitForTranscript(t *testing.T, manager *Manager, id, expected string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, _, err := manager.Transcript(id, 600)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output, expected) {
			return output
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s did not produce %q", id, expected)
	return ""
}

func TestAttentionIsRecomputedOnlyAfterNewOutput(t *testing.T) {
	session := &liveSession{
		subscribers: map[uint64]*outputSubscriber{},
		transcript:  newByteRing(1024),
		record: protocol.SessionRecord{
			ID: "falkn-attention", AgentID: "claude", Status: "running",
		},
	}

	session.refreshAttention()
	if !session.attentionValid {
		t.Fatal("attention was not evaluated")
	}
	firstSequence := session.attentionSequence

	// A second poll with no new output must reuse the cached answer. Every
	// attached client polls the list every two seconds, so recomputing here
	// would render a transcript per session per poll.
	session.attentionState = agent.AttentionNeedsInput
	session.refreshAttention()
	if session.attentionState != agent.AttentionNeedsInput {
		t.Fatalf("cached attention was recomputed: %q", session.attentionState)
	}
	if session.attentionSequence != firstSequence {
		t.Fatalf("sequence moved without output: %d", session.attentionSequence)
	}

	// New output has to invalidate it.
	session.outputMu.Lock()
	session.outputSequence++
	session.outputMu.Unlock()
	session.refreshAttention()
	if session.attentionState == agent.AttentionNeedsInput {
		t.Fatal("attention was not recomputed after new output")
	}
	if session.record.Attention != string(session.attentionState) {
		t.Fatalf("record attention = %q, want %q", session.record.Attention, session.attentionState)
	}
}

func TestStoppedSessionReportsThatItIsNoLongerRunning(t *testing.T) {
	manager := NewManager(agent.NewRegistry(nil))
	created, err := manager.Create(protocol.CreateParams{
		Title: "probe", Directory: t.TempDir(), AgentID: "shell",
	})
	if err != nil {
		created, err = manager.CreateShell(protocol.ShellCreateParams{
			Title: "probe", Directory: t.TempDir(),
		})
		if err != nil {
			t.Skipf("no session could be created in this environment: %v", err)
		}
	}
	if err := manager.Stop(created.ID); err != nil {
		t.Fatal(err)
	}
	// Anything needing a live session has to say so distinctly, so a caller can
	// open a new session instead of reporting a failure.
	err = manager.Send(created.ID, "hello")
	if !errors.Is(err, ErrSessionNotRunning) {
		t.Fatalf("sending to a stopped session = %v, want ErrSessionNotRunning", err)
	}
	if errors.Is(err, ErrSessionNotFound) {
		t.Fatal("a stopped session was reported as missing")
	}
}
