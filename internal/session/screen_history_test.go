package session

import (
	"strings"
	"testing"

	xterm "github.com/gitpod-io/xterm-go"
)

func TestAlternateScreenBuildsStableTranscriptScrollback(t *testing.T) {
	terminal := xterm.New(xterm.WithCols(20), xterm.WithRows(3), xterm.WithScrollback(10))
	defer terminal.Dispose()
	session := &liveSession{terminal: terminal}

	writeTerminalScreen(t, session, "\x1b[?1049hone\r\ntwo\r\nthree")
	writeTerminalScreen(t, session, "\r\nfour")
	writeTerminalScreen(t, session, "\r\nfive")

	got := strings.Join(session.renderedTerminalLinesLocked(), "\n")
	for _, expected := range []string{"one", "two", "three", "four", "five"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("transcript %q does not contain %q", got, expected)
		}
	}
}

func TestAlternateScreenNavigationDoesNotReplaceTranscriptTail(t *testing.T) {
	terminal := xterm.New(xterm.WithCols(20), xterm.WithRows(3), xterm.WithScrollback(10))
	defer terminal.Dispose()
	session := &liveSession{terminal: terminal}

	writeTerminalScreen(t, session, "\x1b[?1049hone\r\ntwo\r\nthree")
	writeTerminalScreen(t, session, "\r\nfour")
	writeTerminalScreen(t, session, "\r\nfive")
	before := strings.Join(session.renderedTerminalLinesLocked(), "\n")

	// This is the shape of an alternate-screen application repainting an
	// earlier viewport after a different attached client scrolls upward.
	writeTerminalScreen(t, session, "\x1b[2J\x1b[Hone\r\ntwo\r\nthree")
	after := strings.Join(session.renderedTerminalLinesLocked(), "\n")
	if after != before {
		t.Fatalf("navigation changed transcript\nbefore: %q\nafter:  %q", before, after)
	}
}

func TestNormalTerminalKeepsNativeScrollback(t *testing.T) {
	terminal := xterm.New(xterm.WithCols(20), xterm.WithRows(2), xterm.WithScrollback(10))
	defer terminal.Dispose()
	session := &liveSession{terminal: terminal}

	writeTerminalScreen(t, session, "one\r\ntwo\r\nthree")
	got := strings.Join(session.renderedTerminalLinesLocked(), "\n")
	for _, expected := range []string{"one", "two", "three"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("normal transcript %q does not contain %q", got, expected)
		}
	}
}

func writeTerminalScreen(t *testing.T, session *liveSession, output string) {
	t.Helper()
	if _, err := session.terminal.Write([]byte(output)); err != nil {
		t.Fatal(err)
	}
	session.captureTerminalHistoryLocked()
}
