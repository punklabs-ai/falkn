package session

import (
	"strings"
	"testing"

	xterm "github.com/gitpod-io/xterm-go"
)

func TestByteRingKeepsNewestBytes(t *testing.T) {
	ring := newByteRing(8)
	ring.Write([]byte("12345"))
	ring.Write([]byte("67890"))
	if got := string(ring.Bytes()); got != "34567890" {
		t.Fatalf("got %q, want %q", got, "34567890")
	}
}

func TestTerminalModelResolvesCursorUpdates(t *testing.T) {
	terminal := xterm.New(xterm.WithCols(20), xterm.WithRows(3), xterm.WithScrollback(10))
	defer terminal.Dispose()
	_, _ = terminal.Write([]byte("progress 10%\rprogress 90%\r\nready\x1b[31m!\x1b[0m"))

	first := terminal.Buffer().Lines.Get(0).TranslateToString(true, 0, -1)
	second := terminal.Buffer().Lines.Get(1).TranslateToString(true, 0, -1)
	if first != "progress 90%" || second != "ready!" {
		t.Fatalf("resolved screen was %q / %q", first, second)
	}
}

func TestTranscriptRemovesTerminalControlsAndBoundsLines(t *testing.T) {
	raw := []byte("old\n\x1b[31mred\x1b[0m\nnew\r\n")
	if got := cleanTranscript(raw, 2); got != "red\nnew" {
		t.Fatalf("got %q, want %q", got, "red\\nnew")
	}
}

func TestTranscriptHandlesSplitLookingEscapeData(t *testing.T) {
	raw := []byte("before\x1b]0;title\x07after")
	if got := cleanTranscript(raw, 10); got != "beforeafter" {
		t.Fatalf("got %q", got)
	}
	if strings.ContainsRune(cleanTranscript(raw, 10), '\x1b') {
		t.Fatal("transcript retained an escape byte")
	}
}

func TestCompactBlankLinesKeepsConversationSpacing(t *testing.T) {
	lines := []string{"first", "", "", "", "", "second"}
	got := strings.Join(compactBlankLines(lines, 2), "|")
	if got != "first|||second" {
		t.Fatalf("got %q", got)
	}
}
