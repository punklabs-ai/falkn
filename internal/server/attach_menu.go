package server

import (
	"fmt"
	"strings"
	"sync"

	"github.com/punklabs-ai/falkn/internal/protocol"
)

const (
	menuMaximumRows      = 12
	menuHeaderStyle      = "\x1b[0;38;5;75m"
	menuSelectedStyle    = "\x1b[0;7;38;5;75m"
	menuDimStyle         = "\x1b[0;38;5;245m"
	menuSelectionMarker  = "▸"
	menuAvailableColumns = minimumStatusBarColumns
)

// attachMenu is the session switcher. It never repaints what it covers: opening
// it shrinks the session's terminal instead, so the agent redraws itself into
// the smaller area, and closing it grows the terminal back. That is why it works
// the same whether the session is a plain shell or an agent holding the
// alternate screen, where there is nothing Falkn could have restored.
type attachMenu struct {
	mu        sync.Mutex
	bar       *attachStatusBar
	writer    *attachFrameWriter
	socket    string
	sessionID string
	open      bool
	sessions  []protocol.SessionRecord
	selection int
}

func newAttachMenu(bar *attachStatusBar, writer *attachFrameWriter, socket, sessionID string) *attachMenu {
	return &attachMenu{bar: bar, writer: writer, socket: socket, sessionID: sessionID}
}

func (m *attachMenu) isOpen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.open
}

// toggle opens the switcher, or closes it when it is already open.
func (m *attachMenu) toggle() {
	if m.isOpen() {
		m.close()
		return
	}
	records, err := fetchAttachSessions(m.socket)
	if err != nil {
		return
	}
	sessions := switchableSessions(records)
	if len(sessions) == 0 {
		return
	}

	m.mu.Lock()
	m.open = true
	m.sessions = sessions
	m.selection = 0
	for index, record := range sessions {
		if record.ID == m.sessionID {
			m.selection = index
			break
		}
	}
	m.mu.Unlock()
	m.repaint()
}

func (m *attachMenu) move(delta int) {
	m.mu.Lock()
	if !m.open || len(m.sessions) == 0 {
		m.mu.Unlock()
		return
	}
	m.selection = (m.selection + delta) % len(m.sessions)
	if m.selection < 0 {
		m.selection += len(m.sessions)
	}
	m.mu.Unlock()
	m.repaint()
}

// choose switches to the highlighted session, or closes the switcher when it is
// already the attached one.
func (m *attachMenu) choose() {
	m.mu.Lock()
	target := ""
	if m.open && m.selection < len(m.sessions) {
		target = m.sessions[m.selection].ID
	}
	m.mu.Unlock()

	m.close()
	if target == "" || target == m.sessionID {
		return
	}
	if !m.bar.requestHandoff(target) {
		return
	}
	_ = m.writer.frame(attachDetachFrame, nil)
	_ = m.writer.connection.Close()
}

func (m *attachMenu) close() {
	m.mu.Lock()
	if !m.open {
		m.mu.Unlock()
		return
	}
	m.open = false
	m.sessions = nil
	m.mu.Unlock()
	m.repaint()
}

// repaint hands the switcher's rows to the status bar, which owns every write to
// the terminal, then tells the session how tall it now is.
func (m *attachMenu) repaint() {
	m.mu.Lock()
	lines := m.linesLocked()
	m.mu.Unlock()

	_ = m.bar.setMenuLines(lines)
	_ = m.writer.resize(m.bar.columnsAndContentRows())
}

func (m *attachMenu) linesLocked() []string {
	if !m.open {
		return nil
	}
	columns := m.bar.terminalColumns()
	width := max(columns-1, menuAvailableColumns-1)

	visible := m.sessions
	first := 0
	if len(visible) > menuMaximumRows {
		// Keep the selection in view when there are more sessions than rows.
		first = min(max(m.selection-menuMaximumRows/2, 0), len(visible)-menuMaximumRows)
		visible = visible[first : first+menuMaximumRows]
	}

	lines := make([]string, 0, len(visible)+1)
	for index, record := range visible {
		lines = append(lines, m.sessionLine(record, first+index, width))
	}
	return append(lines, menuFooterLine(width))
}

func (m *attachMenu) sessionLine(record protocol.SessionRecord, index, width int) string {
	flag, flagStyle := attentionFlag(record)
	selected := index == m.selection

	marker := " "
	if selected {
		marker = menuSelectionMarker
	}
	number := " "
	if index < 9 {
		number = fmt.Sprintf("%d", index+1)
	}

	title := cleanStatusLabel(record.Title)
	agentID := record.AgentID
	if agentID == "shell" {
		agentID = ""
	}

	// Build the visible text first so the width is measured without styling.
	left := fmt.Sprintf("%s %s [%s] %s", marker, number, flag, title)
	right := strings.TrimSpace(agentID + " " + attentionLabel(record))
	if record.ID == m.sessionID {
		right = strings.TrimSpace(right + " · attached")
	}

	gap := width - runeCount(left) - runeCount(right) - 1
	if gap < 1 {
		left = truncateRunes(left, max(width-runeCount(right)-2, 1))
		gap = max(width-runeCount(left)-runeCount(right)-1, 1)
	}
	plain := left + strings.Repeat(" ", gap) + right + " "

	if selected {
		return menuSelectedStyle + plain + statusBarStyleSequence
	}
	styled := plain
	if flagStyle != "" {
		// Colour only the flag, so the row stays quiet while the state does not.
		styled = strings.Replace(plain, "["+flag+"]", flagStyle+"["+flag+"]"+menuDimStyle, 1)
	}
	return menuDimStyle + styled + statusBarStyleSequence
}

func attentionLabel(record protocol.SessionRecord) string {
	if record.Status != "running" {
		return record.Status
	}
	switch record.Attention {
	case "needs_input":
		return "needs input"
	case "failed":
		return "failed"
	case "completed":
		return "done"
	default:
		return ""
	}
}

func menuFooterLine(width int) string {
	hint := "↑↓ select · ⏎ switch · esc close"
	if runeCount(hint) > width {
		hint = truncateRunes(hint, width)
	}
	return menuHeaderStyle + "─ sessions " + strings.Repeat("─", max(width-runeCount(hint)-13, 1)) +
		" " + hint + " " + statusBarStyleSequence
}

// selectIndex switches straight to a numbered session, matching the Ctrl-\ 1-9
// binding that works without opening the switcher at all.
func (m *attachMenu) selectIndex(index int) {
	m.mu.Lock()
	if !m.open || index < 1 || index > len(m.sessions) {
		m.mu.Unlock()
		return
	}
	m.selection = index - 1
	m.mu.Unlock()
	m.choose()
}
