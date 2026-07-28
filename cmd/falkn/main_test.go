package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/punklabs-ai/falkn/internal/protocol"
	"github.com/punklabs-ai/falkn/internal/server"
)

func TestShouldRequestAttachHandoffForNestedAttachedSession(t *testing.T) {
	sessions := []protocol.SessionRecord{{
		ID: "falkn-shell", Status: "running", AttachedClients: 1,
	}}
	if !shouldRequestAttachHandoff(sessions, "falkn-shell", "falkn-codex") {
		t.Fatal("nested attached session did not request a handoff")
	}
	for _, test := range []struct {
		name    string
		current string
		target  string
		session protocol.SessionRecord
	}{
		{name: "outside Falkn", target: "falkn-codex", session: sessions[0]},
		{name: "same session", current: "falkn-shell", target: "falkn-shell", session: sessions[0]},
		{
			name: "no local attachment", current: "falkn-shell", target: "falkn-codex",
			session: protocol.SessionRecord{ID: "falkn-shell", Status: "running"},
		},
		{
			name: "stopped shell", current: "falkn-shell", target: "falkn-codex",
			session: protocol.SessionRecord{ID: "falkn-shell", Status: "stopped", AttachedClients: 1},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if shouldRequestAttachHandoff(
				[]protocol.SessionRecord{test.session},
				test.current,
				test.target,
			) {
				t.Fatal("unexpected attach handoff")
			}
		})
	}
}

func TestSupportsFeature(t *testing.T) {
	preflight := protocol.PreflightResult{Features: []string{"shell", "attach"}}
	if !supportsFeature(preflight, "attach") {
		t.Fatal("advertised feature was not found")
	}
	if supportsFeature(preflight, "daemon_shutdown") {
		t.Fatal("missing feature was reported as available")
	}
}

func TestRunningSessionsFiltersStoppedRecords(t *testing.T) {
	sessions := []protocol.SessionRecord{
		{ID: "running", Status: "running"},
		{ID: "stopped", Status: "stopped"},
	}
	running := runningSessions(sessions)
	if len(running) != 1 || running[0].ID != "running" {
		t.Fatalf("running sessions = %#v", running)
	}
}

func TestReusableShellSessionAttachesExistingShellInCurrentDirectory(t *testing.T) {
	sessions := []protocol.SessionRecord{
		{ID: "elsewhere", Directory: "/tmp/elsewhere", Status: "running", Kind: "shell"},
		{ID: "current", Directory: "/tmp/project", Status: "running", Kind: "shell"},
	}
	matched := reusableShellSession(sessions, "/tmp/project", "")
	if matched == nil || matched.ID != "current" {
		t.Fatalf("reusable session = %#v", matched)
	}
}

func TestReusableShellSessionDoesNotAttachLegacyAgentSession(t *testing.T) {
	sessions := []protocol.SessionRecord{
		{ID: "agent", Directory: "/tmp/project", Status: "running"},
	}
	if matched := reusableShellSession(sessions, "/tmp/project", ""); matched != nil {
		t.Fatalf("reusable session = %#v, want nil", matched)
	}
}

func TestIsShellSessionAllowsPreFeatureShellAttachment(t *testing.T) {
	sessions := []protocol.SessionRecord{{ID: "shell", Kind: "shell"}, {ID: "agent", Kind: "agent"}}
	if !isShellSession(sessions, "shell") {
		t.Fatal("shell session was not recognised")
	}
	if isShellSession(sessions, "agent") {
		t.Fatal("agent session was reported as a shell")
	}
}

func TestIncompatibleDaemonErrorNamesPreservedSessions(t *testing.T) {
	err := incompatibleShellDaemonError(
		protocol.PreflightResult{DaemonVersion: "0.2.8"},
		[]protocol.SessionRecord{{ID: "falkn-123", Title: "Existing", Status: "running"}},
	)
	for _, expected := range []string{"0.2.8", "Existing", "falkn-123", "mobile app", "upgrade automatically"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("error %q does not contain %q", err, expected)
		}
	}
}

func TestReusableShellSessionSkipsTheSessionTheCallerIsInside(t *testing.T) {
	sessions := []protocol.SessionRecord{
		{ID: "current", Directory: "/tmp/project", Status: "running", Kind: "shell"},
	}
	// Running falkn inside a session matches that session's own directory.
	// Attaching it to itself feeds its output back into its own terminal.
	if matched := reusableShellSession(sessions, "/tmp/project", "current"); matched != nil {
		t.Fatalf("reusable session = %#v, want nil so a new session is created", matched)
	}
	// Another shell on the same directory is still reusable.
	sessions = append(sessions, protocol.SessionRecord{
		ID: "sibling", Directory: "/tmp/project", Status: "running", Kind: "shell",
	})
	matched := reusableShellSession(sessions, "/tmp/project", "current")
	if matched == nil || matched.ID != "sibling" {
		t.Fatalf("reusable session = %#v, want the sibling", matched)
	}
}

func TestNewSessionOpensInPlaceOfTheSessionItWasRunFrom(t *testing.T) {
	sessions := []protocol.SessionRecord{
		{ID: "current", Status: "running", Kind: "shell", AttachedClients: 1},
	}
	// A session started from inside another replaces it in the terminal rather
	// than nesting a second client in that session's own display.
	if !shouldRequestAttachHandoff(sessions, "current", "created") {
		t.Fatal("a session created from inside another did not hand off")
	}
	// With no terminal attached there is nothing to hand off to.
	sessions[0].AttachedClients = 0
	if shouldRequestAttachHandoff(sessions, "current", "created") {
		t.Fatal("handoff was requested with no attached client to receive it")
	}
}

func TestSessionGoneRecognisesEndedAndMissingSessions(t *testing.T) {
	for code, want := range map[string]bool{
		"session_not_running": true,
		"session_not_found":   true,
		"session_error":       false,
		"":                    false,
	} {
		err := error(&server.AttachRejectedError{Code: code, Message: "rejected"})
		if got := server.SessionGone(err); got != want {
			t.Fatalf("code %q reported gone=%v, want %v", code, got, want)
		}
	}
	// An ordinary failure must not be mistaken for a vanished session, or a real
	// problem would be answered by silently opening another shell.
	if server.SessionGone(errors.New("connection refused")) {
		t.Fatal("an unrelated error was treated as a vanished session")
	}
	if server.SessionGone(nil) {
		t.Fatal("nil was treated as a vanished session")
	}
}
