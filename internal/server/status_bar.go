package server

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/punklabs-ai/falkn/internal/protocol"
)

const (
	minimumStatusBarColumns  = 40
	minimumStatusBarRows     = 4
	layoutSequenceTailBytes  = 32
	handoffSequenceTailBytes = 512
	statusBarStyleSequence   = "\x1b[0;38;5;75m"
)

type attachStatusBar struct {
	mu                    sync.Mutex
	output                io.Writer
	columns               int
	rows                  int
	content               footerContent
	supportsNotifications bool
	menuLines             []string
	scrollRegionDirty     bool
	reservedRowsClaimed   int
	renderPending         bool
	pendingClearRows      []int
	layoutSequenceTail    []byte
	handoffSequenceTail   []byte
	handoffSessionID      string
	streamState           terminalStreamState
	closed                bool
}

func newAttachStatusBar(
	output io.Writer,
	columns, rows int,
	title, agent string,
	notificationsMuted, supportsNotifications bool,
) *attachStatusBar {
	return &attachStatusBar{
		output: output, columns: columns, rows: rows,
		content: footerContent{
			title: title, agent: agent,
			notificationsMuted:    notificationsMuted,
			supportsNotifications: supportsNotifications,
		},
		supportsNotifications: supportsNotifications,
		scrollRegionDirty:     true, renderPending: true,
	}
}

func terminalContentRows(columns, rows int) int {
	if statusBarVisible(columns, rows) {
		return rows - 1
	}
	return rows
}

// reservedRowsLocked counts the rows Falkn is drawing on: the footer, plus the
// switcher when it is open.
func (bar *attachStatusBar) reservedRowsLocked() int {
	if !statusBarVisible(bar.columns, bar.rows) {
		return 0
	}
	return 1 + len(bar.menuLines)
}

// setMenuLines replaces the switcher's rows. Passing none closes it.
func (bar *attachStatusBar) setMenuLines(lines []string) error {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	if len(lines) == 0 && len(bar.menuLines) == 0 {
		return nil
	}
	if len(lines) < len(bar.menuLines) {
		// Rows the switcher is giving back have to be wiped, or its last frame
		// stays on screen beneath a shorter one.
		bottom := bar.rows - 1
		for row := bottom; row > bottom-len(bar.menuLines)+len(lines); row-- {
			bar.pendingClearRows = appendUniqueRow(bar.pendingClearRows, row)
		}
		bar.reservedRowsClaimed = 1 + len(lines)
	}
	bar.menuLines = append(bar.menuLines[:0], lines...)
	bar.scrollRegionDirty = true
	bar.renderPending = true
	return bar.renderLocked()
}

func statusBarVisible(columns, rows int) bool {
	return columns >= minimumStatusBarColumns && rows >= minimumStatusBarRows
}

// Write keeps the child PTY stream byte-for-byte intact. The footer is emitted
// only when the child stream is at a complete ANSI/UTF-8 boundary, so a local
// escape sequence or multibyte character can never be inserted into the middle
// of output produced by Codex, Claude, or the shell.
func (bar *attachStatusBar) Write(data []byte) (int, error) {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	count, err := bar.output.Write(data)
	if count > 0 {
		written := data[:count]
		bar.observeLayoutLocked(written)
		bar.streamState.consume(written)
		bar.observeHandoffLocked(written)
	}
	if err != nil {
		return count, err
	}
	if bar.handoffSessionID != "" {
		return count, errAttachHandoff
	}
	if err := bar.renderLocked(); err != nil {
		return count, err
	}
	return count, nil
}

func (bar *attachStatusBar) requestedHandoffSessionID() string {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	return bar.handoffSessionID
}

func (bar *attachStatusBar) render() error {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	bar.renderPending = true
	return bar.renderLocked()
}

func (bar *attachStatusBar) resize(columns, rows int) error {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	if statusBarVisible(bar.columns, bar.rows) {
		bar.pendingClearRows = appendUniqueRow(bar.pendingClearRows, bar.rows)
	}
	bar.columns = columns
	bar.rows = rows
	if !statusBarVisible(columns, rows) {
		// Shrinking below the footer's minimum releases every row and restores the
		// full scrolling region, so growing back has to claim them again.
		bar.reservedRowsClaimed = 0
		bar.menuLines = nil
	}
	bar.scrollRegionDirty = true
	bar.renderPending = true
	return bar.renderLocked()
}

func (bar *attachStatusBar) setNotificationsMuted(muted bool) error {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	if bar.content.notificationsMuted == muted {
		return nil
	}
	bar.content.notificationsMuted = muted
	bar.renderPending = true
	return bar.renderLocked()
}

// updateSessions refreshes the footer from a whole session list, because what
// it says about the other sessions matters as much as what it says about this
// one. The comparison keeps an unchanged list from repainting: every attached
// client polls every two seconds, and the footer writes into the same stream
// the agent is drawing on.
func (bar *attachStatusBar) updateSessions(sessionID string, records []protocol.SessionRecord) error {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	content := footerContentFor(sessionID, records)
	content.supportsNotifications = bar.supportsNotifications
	content.prefixName = bar.content.prefixName
	content.prefixArmed = bar.content.prefixArmed
	if content == bar.content {
		return nil
	}
	bar.content = content
	bar.renderPending = true
	return bar.renderLocked()
}

func (bar *attachStatusBar) close() error {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	if bar.closed {
		return nil
	}
	bar.closed = true
	if !bar.streamState.safeBoundary() {
		// The child disconnected in the middle of a terminal sequence. Do not
		// corrupt that sequence merely to clear local presentation chrome.
		return nil
	}
	for _, row := range bar.pendingClearRows {
		if err := bar.clearLocked(row); err != nil {
			return err
		}
	}
	if statusBarVisible(bar.columns, bar.rows) {
		for row := bar.rows; row > bar.rows-bar.reservedRowsLocked(); row-- {
			if err := bar.clearLocked(row); err != nil {
				return err
			}
		}
	}
	return nil
}

func (bar *attachStatusBar) renderLocked() error {
	if bar.closed || !bar.streamState.safeBoundary() ||
		(!bar.renderPending && len(bar.pendingClearRows) == 0) {
		return nil
	}
	for _, row := range bar.pendingClearRows {
		if err := bar.clearLocked(row); err != nil {
			return err
		}
	}
	bar.pendingClearRows = nil
	if !statusBarVisible(bar.columns, bar.rows) {
		bar.renderPending = false
		return nil
	}
	reserved := bar.reservedRowsLocked()
	contentBottom := bar.rows - reserved

	var frame strings.Builder
	frame.WriteString(bar.claimRowsLocked(reserved))
	frame.WriteString("\x1b7")
	if bar.scrollRegionDirty {
		fmt.Fprintf(&frame, "\x1b[1;%dr", contentBottom)
	}
	frame.WriteString(statusBarStyleSequence)
	for index, line := range bar.menuLines {
		fmt.Fprintf(&frame, "\x1b[%d;1H\x1b[2K%s", contentBottom+1+index, line)
		frame.WriteString(statusBarStyleSequence)
	}
	fmt.Fprintf(&frame, "\x1b[%d;1H\x1b[2K%s\x1b[0m\x1b8",
		bar.rows, statusBarLine(bar.columns, bar.content, bar.menuOpenLocked()))

	_, err := io.WriteString(bar.output, frame.String())
	if err == nil {
		bar.scrollRegionDirty = false
		bar.reservedRowsClaimed = reserved
		bar.renderPending = false
	}
	return err
}

func (bar *attachStatusBar) menuOpenLocked() bool {
	return len(bar.menuLines) > 0
}

// claimRowsLocked frees the rows Falkn is about to draw on, so opening the
// switcher pushes the session's output up rather than covering it.
//
// The first row is claimed with an index, which scrolls only when the cursor is
// already on the last row and is undone by the cursor-up otherwise. Attaching
// from a full terminal is exactly that case. Further rows are claimed with an
// explicit scroll-up, because by then the cursor is inside the region and an
// index would not move anything.
func (bar *attachStatusBar) claimRowsLocked(reserved int) string {
	claimed := bar.reservedRowsClaimed
	if reserved <= claimed {
		return ""
	}
	sequence := ""
	if claimed == 0 {
		sequence = "\x1bD\x1b[A"
		claimed = 1
	}
	if grown := reserved - claimed; grown > 0 {
		sequence += fmt.Sprintf("\x1b[%dS\x1b[%dA", grown, grown)
	}
	return sequence
}

func (bar *attachStatusBar) clearLocked(row int) error {
	_, err := fmt.Fprintf(bar.output, "\x1b7\x1b[r\x1b[0m\x1b[%d;1H\x1b[2K\x1b8", row)
	return err
}

func (bar *attachStatusBar) observeLayoutLocked(data []byte) {
	combined := make([]byte, 0, len(bar.layoutSequenceTail)+len(data))
	combined = append(combined, bar.layoutSequenceTail...)
	combined = append(combined, data...)
	if terminalOutputResetsLayout(combined) {
		bar.scrollRegionDirty = true
		bar.renderPending = true
	}
	if len(combined) > layoutSequenceTailBytes {
		combined = combined[len(combined)-layoutSequenceTailBytes:]
	}
	bar.layoutSequenceTail = append(bar.layoutSequenceTail[:0], combined...)
}

func (bar *attachStatusBar) observeHandoffLocked(data []byte) {
	if bar.handoffSessionID != "" {
		return
	}
	combined := make([]byte, 0, len(bar.handoffSequenceTail)+len(data))
	combined = append(combined, bar.handoffSequenceTail...)
	combined = append(combined, data...)
	if sessionID := attachHandoffSessionID(combined); sessionID != "" {
		bar.handoffSessionID = sessionID
		return
	}
	if len(combined) > handoffSequenceTailBytes {
		combined = combined[len(combined)-handoffSequenceTailBytes:]
	}
	bar.handoffSequenceTail = append(bar.handoffSequenceTail[:0], combined...)
}

func terminalOutputResetsLayout(data []byte) bool {
	for _, sequence := range [][]byte{
		[]byte("\x1bc"),
		[]byte("\x1b[J"), []byte("\x1b[0J"), []byte("\x1b[1J"),
		[]byte("\x1b[2J"), []byte("\x1b[3J"),
		[]byte("\x1b[r"), []byte("\x1b[;r"),
		[]byte("\x1b[?47h"), []byte("\x1b[?47l"),
		[]byte("\x1b[?1047h"), []byte("\x1b[?1047l"),
		[]byte("\x1b[?1049h"), []byte("\x1b[?1049l"),
	} {
		if strings.Contains(string(data), string(sequence)) {
			return true
		}
	}
	return false
}

type terminalStreamMode uint8

const (
	terminalStreamGround terminalStreamMode = iota
	terminalStreamEscape
	terminalStreamCSI
	terminalStreamOSC
	terminalStreamOSCEscape
	terminalStreamControlString
	terminalStreamControlStringEscape
)

type terminalStreamState struct {
	mode          terminalStreamMode
	utf8Remaining int
}

func (state *terminalStreamState) safeBoundary() bool {
	return state.mode == terminalStreamGround && state.utf8Remaining == 0
}

func (state *terminalStreamState) consume(data []byte) {
	for _, value := range data {
		if state.utf8Remaining > 0 {
			if value&0xc0 == 0x80 {
				state.utf8Remaining--
				continue
			}
			// Invalid UTF-8 ends the partial character. Process this byte as a
			// new terminal token so an ESC still enters escape mode.
			state.utf8Remaining = 0
		}

		switch state.mode {
		case terminalStreamGround:
			state.consumeGround(value)
		case terminalStreamEscape:
			switch value {
			case '[':
				state.mode = terminalStreamCSI
			case ']':
				state.mode = terminalStreamOSC
			case 'P', 'X', '^', '_':
				state.mode = terminalStreamControlString
			default:
				if value < 0x20 || value > 0x2f {
					state.mode = terminalStreamGround
				}
			}
		case terminalStreamCSI:
			if value == 0x1b {
				state.mode = terminalStreamEscape
			} else if value >= 0x40 && value <= 0x7e {
				state.mode = terminalStreamGround
			}
		case terminalStreamOSC:
			if value == 0x07 || value == 0x9c {
				state.mode = terminalStreamGround
			} else if value == 0x1b {
				state.mode = terminalStreamOSCEscape
			}
		case terminalStreamOSCEscape:
			if value == '\\' || value == 0x07 || value == 0x9c {
				state.mode = terminalStreamGround
			} else if value != 0x1b {
				state.mode = terminalStreamOSC
			}
		case terminalStreamControlString:
			if value == 0x9c {
				state.mode = terminalStreamGround
			} else if value == 0x1b {
				state.mode = terminalStreamControlStringEscape
			}
		case terminalStreamControlStringEscape:
			if value == '\\' || value == 0x9c {
				state.mode = terminalStreamGround
			} else if value != 0x1b {
				state.mode = terminalStreamControlString
			}
		}
	}
}

func (state *terminalStreamState) consumeGround(value byte) {
	switch value {
	case 0x1b:
		state.mode = terminalStreamEscape
	case 0x90, 0x98, 0x9e, 0x9f:
		state.mode = terminalStreamControlString
	case 0x9b:
		state.mode = terminalStreamCSI
	case 0x9d:
		state.mode = terminalStreamOSC
	default:
		switch {
		case value >= 0xc2 && value <= 0xdf:
			state.utf8Remaining = 1
		case value >= 0xe0 && value <= 0xef:
			state.utf8Remaining = 2
		case value >= 0xf0 && value <= 0xf4:
			state.utf8Remaining = 3
		}
	}
}

func appendUniqueRow(rows []int, row int) []int {
	for _, existing := range rows {
		if existing == row {
			return rows
		}
	}
	return append(rows, row)
}

// requestHandoff asks the attach client to move to another session. It reports
// whether this request was the one recorded, so a second request cannot
// override a handoff already under way.
func (bar *attachStatusBar) requestHandoff(sessionID string) bool {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	if bar.handoffSessionID != "" {
		return false
	}
	bar.handoffSessionID = sessionID
	return true
}

func (bar *attachStatusBar) terminalColumns() int {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	return bar.columns
}

// columnsAndContentRows is the size the session's terminal should be given the
// rows Falkn is currently drawing on.
func (bar *attachStatusBar) columnsAndContentRows() (int, int) {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	return bar.columns, bar.rows - bar.reservedRowsLocked()
}

// setPrefixName records the hotkey prefix, so the footer can advertise the
// binding that nothing else reveals and show when it is being held.
func (bar *attachStatusBar) setPrefixName(name string) {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	bar.content.prefixName = name
}

// setPrefixArmed reports whether the prefix has been pressed and Falkn is
// waiting for the key that completes it. Without this the prefix is silent, and
// whether it registered is a guess until the next keystroke resolves it.
func (bar *attachStatusBar) setPrefixArmed(armed bool) error {
	bar.mu.Lock()
	defer bar.mu.Unlock()
	if bar.content.prefixArmed == armed {
		return nil
	}
	bar.content.prefixArmed = armed
	bar.renderPending = true
	return bar.renderLocked()
}
