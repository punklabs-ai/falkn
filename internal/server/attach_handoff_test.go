package server

import (
	"bytes"
	"errors"
	"testing"
)

func TestAttachStatusBarRequestsNestedAttachHandoffAcrossWrites(t *testing.T) {
	var control bytes.Buffer
	if err := WriteAttachHandoff(&control, "falkn-target-123"); err != nil {
		t.Fatal(err)
	}
	sequence := control.Bytes()
	split := len(sequence) / 2

	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 80, 24, "Shell", "", false, true)
	if _, err := bar.Write(sequence[:split]); err != nil {
		t.Fatalf("first handoff fragment: %v", err)
	}
	if _, err := bar.Write(sequence[split:]); !errors.Is(err, errAttachHandoff) {
		t.Fatalf("second handoff fragment error = %v, want handoff", err)
	}
	if got := bar.requestedHandoffSessionID(); got != "falkn-target-123" {
		t.Fatalf("handoff session = %q", got)
	}
	if !bytes.Contains(output.Bytes(), sequence) {
		t.Fatal("handoff control sequence was not preserved in the child stream")
	}
}

func TestAttachHandoffRejectsUnsafeSessionID(t *testing.T) {
	for _, sessionID := range []string{"", "other-session", "falkn-bad\x07value"} {
		var output bytes.Buffer
		if err := WriteAttachHandoff(&output, sessionID); err == nil {
			t.Fatalf("WriteAttachHandoff(%q) succeeded", sessionID)
		}
	}
}

func TestOrdinaryOSCDoesNotRequestAttachHandoff(t *testing.T) {
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 80, 24, "Shell", "", false, true)
	if _, err := bar.Write([]byte("\x1b]0;ordinary title\x07")); err != nil {
		t.Fatal(err)
	}
	if got := bar.requestedHandoffSessionID(); got != "" {
		t.Fatalf("unexpected handoff session %q", got)
	}
}

func TestAttachHistoryDoesNotReplayAHandoff(t *testing.T) {
	var control bytes.Buffer
	if err := WriteAttachHandoff(&control, "falkn-session-two"); err != nil {
		t.Fatal(err)
	}
	history := append([]byte("before"), control.Bytes()...)
	history = append(history, []byte("after")...)

	replay := stripAttachHandoffs(history)
	if got, want := string(replay), "beforeafter"; got != want {
		t.Fatalf("stripped history = %q, want %q", got, want)
	}

	// The bytes returned by Attach are written through the same status bar as
	// live output. Once the historical event is removed, returning to the source
	// session cannot immediately request the old target again.
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 80, 24, "Shell", "", false, true)
	if _, err := bar.Write(replay); err != nil {
		t.Fatal(err)
	}
	if got := bar.requestedHandoffSessionID(); got != "" {
		t.Fatalf("replayed history requested handoff to %q", got)
	}
}

func TestAttachHistoryPreservesUnknownControlStrings(t *testing.T) {
	history := []byte("title\x1b]0;ordinary title\x07 malformed " +
		attachHandoffPrefix + "not-base64!\x07 tail")
	if got := stripAttachHandoffs(history); !bytes.Equal(got, history) {
		t.Fatalf("unknown control strings changed:\n got %q\nwant %q", got, history)
	}
}
