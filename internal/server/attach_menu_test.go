package server

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	xterm "github.com/gitpod-io/xterm-go"
)

// fakeMenu records what the input reader asks of the switcher.
type fakeMenu struct {
	open    bool
	moves   []int
	indexes []int
	chosen  int
	closed  int
	toggles int
}

func (m *fakeMenu) isOpen() bool          { return m.open }
func (m *fakeMenu) toggle()               { m.toggles++; m.open = !m.open }
func (m *fakeMenu) move(delta int)        { m.moves = append(m.moves, delta) }
func (m *fakeMenu) selectIndex(index int) { m.indexes = append(m.indexes, index) }
func (m *fakeMenu) choose()               { m.chosen++; m.open = false }
func (m *fakeMenu) close()                { m.closed++; m.open = false }

func TestPrefixedUpOpensTheSwitcher(t *testing.T) {
	menu := &fakeMenu{}
	reader, _ := readerFor(t, attachInputHandlers{menu: menu})
	reader.consume([]byte{DefaultAttachPrefix, 0x1b, '[', 'A'})
	if menu.toggles != 1 || !menu.open {
		t.Fatalf("prefixed up did not open the switcher: %+v", menu)
	}
}

func TestOpenSwitcherKeepsKeysFromTheSession(t *testing.T) {
	menu := &fakeMenu{open: true}
	reader, serverConnection := readerFor(t, attachInputHandlers{menu: menu})

	// Arrows move the selection instead of reaching the agent.
	reader.consume([]byte{0x1b, '[', 'B'})
	reader.consume([]byte{0x1b, '[', 'A'})
	reader.consume([]byte("j"))
	reader.consume([]byte("k"))
	want := []int{1, -1, 1, -1}
	if len(menu.moves) != len(want) {
		t.Fatalf("moves = %v, want %v", menu.moves, want)
	}
	for index := range want {
		if menu.moves[index] != want[index] {
			t.Fatalf("move %d = %d, want %d", index, menu.moves[index], want[index])
		}
	}

	// Nothing may have been forwarded to the session.
	serverConnection.SetReadDeadline(deadlineSoon())
	if _, err := serverConnection.Read(make([]byte, 1)); err == nil {
		t.Fatal("the open switcher forwarded a key to the session")
	}
}

func TestSwitcherKeysSelectAndClose(t *testing.T) {
	menu := &fakeMenu{open: true}
	reader, _ := readerFor(t, attachInputHandlers{menu: menu})
	reader.consume([]byte{'\r'})
	if menu.chosen != 1 {
		t.Fatalf("enter did not switch: %+v", menu)
	}

	menu.open = true
	reader.consume([]byte{'4'})
	if len(menu.indexes) != 1 || menu.indexes[0] != 4 {
		t.Fatalf("digit did not select: %+v", menu.indexes)
	}

	// An escape is held until something settles it, so an arrow key delivered in
	// pieces cannot be mistaken for Esc.
	menu.open = true
	reader.consume([]byte{0x1b})
	if menu.closed != 0 {
		t.Fatalf("escape closed the switcher before it was settled: %+v", menu)
	}
	reader.resolveMenuEscape()
	if menu.closed != 1 {
		t.Fatalf("escape did not close the switcher: %+v", menu)
	}
	menu.open = true
	reader.consume([]byte{0x1b, '[', 'B'})
	if menu.closed != 1 {
		t.Fatalf("an arrow key closed the switcher: %+v", menu)
	}
}

func TestSwitcherRowsShrinkTheSessionAndAreGivenBack(t *testing.T) {
	const columns, rows = 80, 24
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, columns, rows, "Infra", "codex", false, true)
	if err := bar.render(); err != nil {
		t.Fatal(err)
	}
	if _, got := bar.columnsAndContentRows(); got != rows-1 {
		t.Fatalf("content rows with only a footer = %d, want %d", got, rows-1)
	}

	lines := []string{"one", "two", "three"}
	if err := bar.setMenuLines(lines); err != nil {
		t.Fatal(err)
	}
	if _, got := bar.columnsAndContentRows(); got != rows-1-len(lines) {
		t.Fatalf("content rows with the switcher open = %d, want %d", got, rows-1-len(lines))
	}
	// The session's terminal shrinking is what makes an agent repaint itself, so
	// the scrolling region has to shrink with it.
	if !strings.Contains(output.String(), "\x1b[1;20r") {
		t.Fatalf("switcher did not shrink the scrolling region: %q", output.String())
	}

	if err := bar.setMenuLines(nil); err != nil {
		t.Fatal(err)
	}
	if _, got := bar.columnsAndContentRows(); got != rows-1 {
		t.Fatalf("content rows after closing = %d, want %d", got, rows-1)
	}
	if !strings.Contains(output.String(), "\x1b[1;23r") {
		t.Fatalf("closing did not restore the scrolling region: %q", output.String())
	}
}

func TestSwitcherDoesNotCoverTheSessionsOutput(t *testing.T) {
	const columns, rows = 80, 24

	terminal := xterm.New(xterm.WithCols(columns), xterm.WithRows(rows))
	defer terminal.Dispose()
	var output bytes.Buffer
	bar := newAttachStatusBar(&output, columns, rows, "Infra", "codex", false, true)
	if err := bar.render(); err != nil {
		t.Fatal(err)
	}
	// Fill the session's area with numbered rows so any loss is visible.
	for row := 1; row <= rows-1; row++ {
		if _, err := bar.Write([]byte(padTestRow(row))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := terminal.Write(output.Bytes()); err != nil {
		t.Fatal(err)
	}
	output.Reset()

	if err := bar.setMenuLines([]string{"menu one", "menu two", "menu three"}); err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.Write(output.Bytes()); err != nil {
		t.Fatal(err)
	}

	buffer := terminal.Buffer()
	screen := make([]string, 0, rows)
	for row := 0; row < rows; row++ {
		screen = append(screen, buffer.TranslateBufferLineToString(buffer.YBase+row, true, 0, columns))
	}
	// The last rows the session had written must have scrolled up rather than
	// been painted over by the switcher.
	if !strings.Contains(strings.Join(screen, "\n"), "row 20") {
		t.Fatalf("opening the switcher lost the session's output:\n%s", strings.Join(screen, "\n"))
	}
	if !strings.Contains(screen[rows-2], "menu three") {
		t.Fatalf("switcher did not occupy the row above the footer: %q", screen[rows-2])
	}
	if !strings.Contains(screen[rows-1], "Falkn") {
		t.Fatalf("footer did not stay on the last row: %q", screen[rows-1])
	}
}

func TestFooterAdvertisesTheSwitcherKey(t *testing.T) {
	content := footerContent{title: "Infra", agent: "codex", prefixName: `Ctrl-\`}
	line := visibleFooter(statusBarLine(100, content, false))
	if !strings.Contains(line, `Ctrl-\ ↑ sessions`) {
		t.Fatalf("footer %q does not advertise the switcher", line)
	}
	if got := runeCount(line); got != 99 {
		t.Fatalf("footer width with the hint = %d, want 99", got)
	}
	// While the switcher is open it explains its own keys.
	open := visibleFooter(statusBarLine(100, content, true))
	if strings.Contains(open, "sessions") {
		t.Fatalf("footer %q still advertises the open switcher", open)
	}
}

func TestNarrowFooterShortensTheReminderRatherThanLosingIt(t *testing.T) {
	content := footerContent{title: "Infra", agent: "codex", prefixName: `Ctrl-\`}
	// A binding nothing else reveals is worth a few columns, so the reminder is
	// abbreviated as the terminal narrows instead of disappearing.
	for _, columns := range []int{100, 72, 60, 55, 50, 48, 44, 42} {
		line := visibleFooter(statusBarLine(columns, content, false))
		if !strings.Contains(line, `Ctrl-\`) {
			t.Fatalf("footer at %d columns lost the prefix reminder: %q", columns, line)
		}
		if got := runeCount(line); got != columns-1 {
			t.Fatalf("footer at %d columns is %d runes", columns, got)
		}
	}
}

func TestHeldPrefixIsShownInTheFooter(t *testing.T) {
	content := footerContent{title: "Infra", agent: "codex", prefixName: `Ctrl-\`}
	idle := statusBarLine(100, content, false)
	if strings.Contains(idle, prefixArmedStyle) {
		t.Fatalf("idle footer highlights the prefix: %q", idle)
	}

	content.prefixArmed = true
	held := statusBarLine(100, content, false)
	// Only the prefix itself changes, so the reminder reads the same either way.
	if !strings.Contains(held, prefixArmedStyle+`Ctrl-\`+statusBarStyleSequence) {
		t.Fatalf("held prefix was not highlighted: %q", held)
	}
	if visibleFooter(held) != visibleFooter(idle) {
		t.Fatalf("holding the prefix changed the footer text:\n%q\n%q", visibleFooter(held), visibleFooter(idle))
	}
}

func padTestRow(row int) string {
	return fmt.Sprintf("row %d\r\n", row)
}

func deadlineSoon() time.Time {
	return time.Now().Add(50 * time.Millisecond)
}

func TestTheSessionNumberSurvivesOpeningTheSwitcher(t *testing.T) {
	content := footerContent{
		title: "web", agent: "codex", prefixName: `Ctrl-\`,
		sessionNumber: 2, sessionCount: 3,
	}
	closed := visibleFooter(statusBarLine(100, content, false))
	if !strings.Contains(closed, `Ctrl-\ [2] ↑ sessions`) {
		t.Fatalf("closed footer = %q", closed)
	}
	// The switcher explains its own keys, so the invitation to open it goes, but
	// the number stays: blinking it every time the switcher is used reads as the
	// indicator being lost.
	open := visibleFooter(statusBarLine(100, content, true))
	if !strings.Contains(open, `Ctrl-\ [2]`) {
		t.Fatalf("open footer dropped the session number: %q", open)
	}
	if strings.Contains(open, "sessions") || strings.Contains(open, "↑") {
		t.Fatalf("open footer still invites opening the switcher: %q", open)
	}
}

func TestASplitArrowStillMovesTheSwitcher(t *testing.T) {
	menu := &fakeMenu{open: true}
	reader, serverConnection := readerFor(t, attachInputHandlers{menu: menu})

	// A terminal is free to deliver an arrow key one byte at a time. Treating the
	// lone escape as Esc closed the switcher and spilled "[A" into the session,
	// so the Enter that followed went to the shell instead of switching.
	reader.consume([]byte{0x1b})
	if menu.closed != 0 {
		t.Fatalf("a split arrow closed the switcher: %+v", menu)
	}
	if !reader.menuEscapePending() {
		t.Fatal("the escape was not held for the rest of the key")
	}
	reader.consume([]byte{'['})
	reader.consume([]byte{'A'})
	if len(menu.moves) != 1 || menu.moves[0] != -1 {
		t.Fatalf("split arrow produced %v", menu.moves)
	}
	if menu.closed != 0 {
		t.Fatalf("the switcher closed during a split arrow: %+v", menu)
	}

	// Nothing may have leaked to the session.
	serverConnection.SetReadDeadline(deadlineSoon())
	if _, err := serverConnection.Read(make([]byte, 1)); err == nil {
		t.Fatal("a split arrow leaked bytes into the session")
	}
}

func TestALoneEscapeStillClosesTheSwitcher(t *testing.T) {
	menu := &fakeMenu{open: true}
	reader, _ := readerFor(t, attachInputHandlers{menu: menu})

	// Held until something settles it, then resolved by the grace period.
	reader.consume([]byte{0x1b})
	if menu.closed != 0 {
		t.Fatalf("the escape was not held: %+v", menu)
	}
	reader.resolveMenuEscape()
	if menu.closed != 1 {
		t.Fatalf("the grace period did not close the switcher: %+v", menu)
	}
	if reader.menuEscapePending() {
		t.Fatal("the escape stayed pending after being resolved")
	}
}

func TestEscapeFollowedByAnotherKeyClosesAndPassesItOn(t *testing.T) {
	menu := &fakeMenu{open: true}
	reader, serverConnection := readerFor(t, attachInputHandlers{menu: menu})

	done := make(chan struct{})
	go func() { defer close(done); reader.consume([]byte{0x1b, 'x'}) }()
	kind, payload := readTestAttachFrame(t, serverConnection)
	<-done
	if menu.closed != 1 {
		t.Fatalf("escape did not close the switcher: %+v", menu)
	}
	// The key after a lone escape belongs to the session now in front.
	if kind != attachInputFrame || !bytes.Equal(payload, []byte{'x'}) {
		t.Fatalf("frame kind=%q payload=%v, want the trailing key", kind, payload)
	}
}
