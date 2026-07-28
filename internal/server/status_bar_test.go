package server

import (
	"bytes"
	"strings"
	"testing"

	xterm "github.com/gitpod-io/xterm-go"
	"github.com/punklabs-ai/falkn/internal/protocol"
)

func TestStatusBarReservesOneClientLocalRow(t *testing.T) {
	if got := terminalContentRows(100, 40); got != 39 {
		t.Fatalf("content rows = %d, want 39", got)
	}
	if got := terminalContentRows(30, 20); got != 20 {
		t.Fatalf("narrow terminal content rows = %d, want 20", got)
	}
}

func TestStatusBarRendersOutsideAgentOutput(t *testing.T) {
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 80, 24, "Infra", "claude", false, true)
	if _, err := bar.Write([]byte("agent output")); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.HasPrefix(got, "agent output") {
		t.Fatalf("agent output prefix = %q", got)
	}
	if !strings.Contains(got, "\x1b[1;23r") || !strings.Contains(got, "\x1b[24;1H") || !strings.Contains(got, "Falkn") {
		t.Fatalf("status bar render = %q", got)
	}
	if !strings.Contains(got, statusBarStyleSequence) {
		t.Fatalf("status bar does not use its blue style: %q", got)
	}
}

func TestStatusBarDoesNotRedrawForOrdinaryAgentOutput(t *testing.T) {
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 80, 24, "Infra", "claude", false, true)
	if err := bar.render(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if _, err := bar.Write([]byte("agent output")); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "agent output" {
		t.Fatalf("ordinary output triggered footer repaint: %q", got)
	}
}

func TestStatusBarRedrawsAfterEraseDisplay(t *testing.T) {
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 80, 24, "Infra", "claude", false, true)
	if err := bar.render(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if _, err := bar.Write([]byte("\x1b[J")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Falkn") {
		t.Fatalf("footer did not return after erase-display: %q", output.String())
	}
}

func TestStatusBarDoesNotInterruptSplitCSISequence(t *testing.T) {
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 80, 24, "Infra", "codex", false, true)
	if _, err := bar.Write([]byte("\x1b[")); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "\x1b[" {
		t.Fatalf("footer interrupted partial CSI: %q", got)
	}
	if _, err := bar.Write([]byte("31mred")); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.HasPrefix(got, "\x1b[31mred") {
		t.Fatalf("raw CSI bytes were not contiguous: %q", got)
	}
}

func TestStatusBarDoesNotInterruptSplitUTF8Character(t *testing.T) {
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 80, 24, "Infra", "codex", false, true)
	encoded := []byte("🎨")
	if _, err := bar.Write(encoded[:2]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), encoded[:2]) {
		t.Fatalf("footer interrupted partial UTF-8: %q", output.Bytes())
	}
	if _, err := bar.Write(encoded[2:]); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(output.Bytes(), encoded) {
		t.Fatalf("UTF-8 bytes were not contiguous: %q", output.Bytes())
	}
}

func TestStatusBarRestoresAgentColourState(t *testing.T) {
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 80, 24, "Infra", "codex", false, true)
	if _, err := bar.Write([]byte("\x1b[31mR")); err != nil {
		t.Fatal(err)
	}
	if _, err := bar.Write([]byte("R")); err != nil {
		t.Fatal(err)
	}

	terminal := xterm.New(xterm.WithCols(80), xterm.WithRows(24))
	defer terminal.Dispose()
	if _, err := terminal.Write(output.Bytes()); err != nil {
		t.Fatal(err)
	}
	line := terminal.NormalBuffer().Lines.Get(0)
	if line == nil {
		t.Fatal("terminal did not render agent output")
	}
	first := &xterm.CellData{}
	second := &xterm.CellData{}
	line.LoadCell(0, first)
	line.LoadCell(1, second)
	if first.IsFgDefault() || first.GetFgColor() != second.GetFgColor() {
		t.Fatalf("footer changed agent colours: first=%#v second=%#v", first, second)
	}
}

func TestStatusBarRestoresReservedRowAfterSplitAlternateScreenSequence(t *testing.T) {
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 80, 24, "Infra", "codex", false, true)
	if err := bar.render(); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if _, err := bar.Write([]byte("\x1b[?10")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "Falkn") {
		t.Fatalf("footer interrupted alternate-screen sequence: %q", output.String())
	}
	if _, err := bar.Write([]byte("49h")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "\x1b[1;23r") {
		t.Fatalf("alternate-screen render did not restore content region: %q", output.String())
	}
}

func TestStatusBarUpdatesWhenShellStartsAnAgent(t *testing.T) {
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 90, 24, "Infra", "shell", false, true)
	if err := bar.updateSessions("falkn-1", []protocol.SessionRecord{
		{ID: "falkn-1", Title: "Infra", AgentID: "codex", Status: "running", NotificationsMuted: true},
	}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "codex") || !strings.Contains(got, "muted") {
		t.Fatalf("updated status output = %q", got)
	}
}

func TestStatusBarDoesNotRepaintForAnUnchangedSessionList(t *testing.T) {
	records := []protocol.SessionRecord{
		{ID: "falkn-1", Title: "Infra", AgentID: "codex", Status: "running"},
		{ID: "falkn-2", Title: "Web", AgentID: "claude", Status: "running"},
	}
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, 90, 24, "Infra", "codex", false, true)
	if err := bar.updateSessions("falkn-1", records); err != nil {
		t.Fatal(err)
	}
	output.Reset()

	// Every attached client polls the list every two seconds. An unchanged list
	// must not write anything into the stream the agent is drawing on.
	if err := bar.updateSessions("falkn-1", records); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "" {
		t.Fatalf("unchanged session list repainted the footer: %q", got)
	}
}

func TestFooterDoesNotClaimTheRowTheCursorIsOn(t *testing.T) {
	const columns, rows = 80, 24
	const prompt = "alice@workstation ~ % "

	terminal := xterm.New(xterm.WithCols(columns), xterm.WithRows(rows))
	defer terminal.Dispose()
	// Fill the screen so the prompt sits on the last row, which is what running
	// falkn in a terminal that is already full looks like.
	if _, err := terminal.Write([]byte(strings.Repeat("filler\r\n", rows-1) + prompt)); err != nil {
		t.Fatal(err)
	}
	if got := terminal.CursorY(); got != rows-1 {
		t.Fatalf("prompt row = %d, want the last row %d", got, rows-1)
	}
	column := terminal.CursorX()

	var output bytes.Buffer
	bar := newAttachStatusBar(&output, columns, rows, "project", "shell", false, true)
	if err := bar.render(); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write(output.Bytes()); err != nil {
		t.Fatal(err)
	}

	if got := terminal.CursorY(); got == rows-1 {
		t.Fatalf("cursor was left on the footer row %d", got)
	}
	if got := terminal.CursorY(); got != rows-2 {
		t.Fatalf("cursor row = %d, want %d, one row above the footer", got, rows-2)
	}
	if got := terminal.CursorX(); got != column {
		t.Fatalf("cursor column = %d, want %d unchanged", got, column)
	}
	// The prompt has to survive the scroll, not be overwritten by the footer.
	buffer := terminal.Buffer()
	promptLine := buffer.TranslateBufferLineToString(buffer.YBase+buffer.Y, true, 0, columns)
	if !strings.Contains(promptLine, prompt) {
		t.Fatalf("prompt row = %q, want it to still contain %q", promptLine, prompt)
	}
	footer := buffer.TranslateBufferLineToString(buffer.YBase+buffer.Y+1, true, 0, columns)
	if !strings.Contains(footer, "Falkn") {
		t.Fatalf("footer row = %q", footer)
	}
}

func TestFooterDoesNotScrollATerminalWithRoomBelowTheCursor(t *testing.T) {
	const columns, rows = 80, 24
	const prompt = "alice@workstation ~ % "

	terminal := xterm.New(xterm.WithCols(columns), xterm.WithRows(rows))
	defer terminal.Dispose()
	if _, err := terminal.Write([]byte(prompt)); err != nil {
		t.Fatal(err)
	}
	row, column := terminal.CursorY(), terminal.CursorX()

	var output bytes.Buffer
	bar := newAttachStatusBar(&output, columns, rows, "project", "shell", false, true)
	if err := bar.render(); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write(output.Bytes()); err != nil {
		t.Fatal(err)
	}

	// Reserving the footer row must be free when the cursor is nowhere near it.
	if terminal.CursorY() != row || terminal.CursorX() != column {
		t.Fatalf("cursor moved to %d,%d, want %d,%d", terminal.CursorY(), terminal.CursorX(), row, column)
	}
	buffer := terminal.Buffer()
	promptLine := buffer.TranslateBufferLineToString(buffer.YBase+row, true, 0, columns)
	if !strings.Contains(promptLine, prompt) {
		t.Fatalf("prompt row = %q, want it to still contain %q", promptLine, prompt)
	}
}
