package server

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/punklabs-ai/falkn/internal/protocol"
)

func TestReadRequestAcceptsANewlineDelimitedJSONFrame(t *testing.T) {
	request, err := readRequest(strings.NewReader("{\"method\":\"list\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(request), `{"method":"list"}`; got != want {
		t.Fatalf("request = %q, want %q", got, want)
	}
}

func TestReadRequestRejectsLegacyBase64Arguments(t *testing.T) {
	err := serve(strings.NewReader("eyJtZXRob2QiOiJsaXN0In0=\n"), io.Discard, remoteOrigin)
	if err == nil || err.Error() != "request is not valid JSON" {
		t.Fatalf("serve error = %v, want invalid JSON", err)
	}
}

func TestReadRequestRejectsOversizedFrames(t *testing.T) {
	frame := bytes.Repeat([]byte{'x'}, maxRequestBytes+1)
	frame = append(frame, '\n')
	if _, err := readRequest(bytes.NewReader(frame)); err == nil {
		t.Fatal("oversized request was accepted")
	}
}

func TestSuccessfulTranscriptRequestMarksSessionVisible(t *testing.T) {
	directory := t.TempDir()
	sessionID := "falkn-bb75e4695dd332be"
	params, err := json.Marshal(protocol.TranscriptParams{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(protocol.Success("test", protocol.TranscriptResult{
		Transcript: "Working", Status: "running",
	}))
	if err != nil {
		t.Fatal(err)
	}
	paths := Paths{NotificationPresence: directory}
	markNotificationPresence(protocol.Request{
		Version: protocol.Version, RequestID: "test", Method: "transcript", Params: params,
	}, response, paths)

	if _, err := os.Stat(filepath.Join(directory, sessionID)); err != nil {
		t.Fatalf("presence marker was not created: %v", err)
	}
}

func TestAppInteractionsMakeSessionsWorthNotifyingAbout(t *testing.T) {
	sessionID := "falkn-bb75e4695dd332be"
	sessionParams, err := json.Marshal(protocol.SessionParams{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	createResponse, err := json.Marshal(protocol.Success("test", protocol.CreateResult{
		Session: protocol.SessionRecord{ID: sessionID, Title: "From the phone"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	acknowledgement, err := json.Marshal(protocol.Success("test", protocol.AckResult{Accepted: true}))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		method   string
		params   []byte
		response []byte
		want     bool
	}{
		{name: "app starts a session", method: "shell.create", response: createResponse, want: true},
		{name: "app opens a session", method: "transcript", params: sessionParams, response: acknowledgement, want: true},
		{name: "app answers a session", method: "key", params: sessionParams, response: acknowledgement, want: true},
		{name: "app lists sessions", method: "list", response: acknowledgement, want: false},
		{name: "app renames a session", method: "rename", params: sessionParams, response: acknowledgement, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			markNotificationFollow(protocol.Request{
				Version: protocol.Version, RequestID: "test", Method: test.method, Params: test.params,
			}, test.response, Paths{NotificationFollow: directory})

			_, err := os.Stat(filepath.Join(directory, sessionID))
			if followed := err == nil; followed != test.want {
				t.Fatalf("session followed = %t, want %t", followed, test.want)
			}
		})
	}
}

func TestRejectedRequestsDoNotMakeASessionWorthNotifyingAbout(t *testing.T) {
	directory := t.TempDir()
	sessionID := "falkn-bb75e4695dd332be"
	params, err := json.Marshal(protocol.SessionParams{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(protocol.Failure("test", "session_not_found", "no such session"))
	if err != nil {
		t.Fatal(err)
	}
	markNotificationFollow(protocol.Request{
		Version: protocol.Version, RequestID: "test", Method: "transcript", Params: params,
	}, response, Paths{NotificationFollow: directory})

	if _, err := os.Stat(filepath.Join(directory, sessionID)); !os.IsNotExist(err) {
		t.Fatalf("a rejected request left a follow marker: %v", err)
	}
}
