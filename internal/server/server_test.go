package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/punklabs-ai/falkn/internal/protocol"
	"github.com/punklabs-ai/falkn/internal/session"
)

func TestPreflightAdvertisesCLIAndSafeShutdownFeatures(t *testing.T) {
	server := New("test-version")
	response := server.dispatch(protocol.Request{
		Version: protocol.Version, RequestID: "preflight", Method: "preflight",
	})
	if !response.OK {
		t.Fatalf("preflight failed: %#v", response.Error)
	}
	encoded, err := json.Marshal(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	var result protocol.PreflightResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"shell", "attach", "daemon_shutdown", "uploads", "attention"} {
		found := false
		for _, feature := range result.Features {
			found = found || feature == expected
		}
		if !found {
			t.Fatalf("preflight features %v do not contain %q", result.Features, expected)
		}
	}
}

func TestDaemonShutdownRefusesWhileASessionIsRunning(t *testing.T) {
	server := New("test-version")
	record, err := server.sessions.CreateShell(protocol.ShellCreateParams{
		Title: "Running", Directory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.sessions.Stop(record.ID)

	response := server.dispatch(protocol.Request{
		Version: protocol.Version, RequestID: "shutdown", Method: "daemon.shutdown",
		Params: json.RawMessage("{}"),
	})
	if response.OK || response.Error == nil || response.Error.Code != "daemon_busy" {
		t.Fatalf("shutdown response = %#v, want daemon_busy", response)
	}
	select {
	case <-server.shutdown:
		t.Fatal("busy daemon began shutting down")
	default:
	}
}

func TestDaemonShutdownIsAcceptedWhenIdle(t *testing.T) {
	server := New("test-version")
	daemonConnection, clientConnection := net.Pipe()
	defer clientConnection.Close()
	go server.handle(daemonConnection)

	request := protocol.Request{
		Version: protocol.Version, RequestID: "shutdown", Method: "daemon.shutdown",
		Params: json.RawMessage("{}"),
	}
	if err := json.NewEncoder(clientConnection).Encode(request); err != nil {
		t.Fatal(err)
	}
	var response protocol.Response
	if err := json.NewDecoder(clientConnection).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("shutdown failed: %#v", response.Error)
	}
	select {
	case <-server.shutdown:
	case <-time.After(time.Second):
		t.Fatal("idle daemon did not begin shutting down")
	}
}

func TestAFinishedSessionIsReportedDistinctlyFromAMissingOne(t *testing.T) {
	// A session can end between being listed and being attached. The caller has
	// to be able to tell that apart from a genuine failure, so it can open a new
	// session rather than report one it chose itself as gone.
	finished := sessionFailure("req", fmt.Errorf(
		"session %q is no longer running: %w", "falkn-1", session.ErrSessionNotRunning))
	if finished.Error == nil || finished.Error.Code != "session_not_running" {
		t.Fatalf("finished session error = %+v", finished.Error)
	}
	missing := sessionFailure("req", session.ErrSessionNotFound)
	if missing.Error == nil || missing.Error.Code != "session_not_found" {
		t.Fatalf("missing session error = %+v", missing.Error)
	}
	other := sessionFailure("req", errors.New("disk full"))
	if other.Error == nil || other.Error.Code != "session_error" {
		t.Fatalf("unrelated error = %+v", other.Error)
	}
}
