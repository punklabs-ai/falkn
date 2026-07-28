package session

import (
	"strings"
	"unicode"

	xterm "github.com/gitpod-io/xterm-go"
)

const maximumTerminalHistoryLines = 2_000

// captureTerminalHistoryLocked turns an alternate-screen TUI into stable
// scrollback for transcript clients. Alternate terminal buffers intentionally
// have no scrollback, so retaining only the active xterm buffer limits mobile
// clients to one screen and makes another client's TUI navigation replace what
// they are reading.
//
// The journal commits rows only as they leave the bottom-facing screen. If a
// client navigates to a screen that is already present in the journal, it is
// treated as that client's local viewport and does not replace the transcript.
// terminalMu must be held by the caller.
func (s *liveSession) captureTerminalHistoryLocked() {
	if s.terminal == nil || !s.terminal.IsAltBufferActive() {
		return
	}

	next := terminalBufferLines(s.terminal.AltBuffer())
	if len(next) == 0 {
		return
	}
	if len(s.lastTerminalScreen) == 0 {
		s.lastTerminalScreen = cloneLines(next)
		return
	}

	known := make([]string, 0, len(s.terminalHistory)+len(s.lastTerminalScreen))
	known = append(known, s.terminalHistory...)
	known = append(known, s.lastTerminalScreen...)
	if containsLineSequence(known, next) {
		// Scrolling or paging inside a TUI should remain local to the client
		// that initiated it. Keep the most recent bottom-facing snapshot.
		return
	}

	overlap := longestSuffixPrefix(s.lastTerminalScreen, next)
	if overlap > 0 {
		s.appendTerminalHistoryLocked(s.lastTerminalScreen[:len(s.lastTerminalScreen)-overlap])
		s.lastTerminalScreen = cloneLines(next)
		return
	}

	// Most in-place redraws retain the top of the screen while replacing a
	// status line or input area. They update the live tail without creating
	// duplicate history.
	if commonLinePrefix(s.lastTerminalScreen, next) > 0 {
		s.lastTerminalScreen = cloneLines(next)
		return
	}

	// A full-screen mode change has no reliable overlap. Prefer the new live
	// screen, but do not mistake the redraw for historical output.
	s.lastTerminalScreen = cloneLines(next)
}

// renderedTerminalLinesLocked returns stable transcript rows without changing
// the real PTY size or another client's viewport. terminalMu must be held.
func (s *liveSession) renderedTerminalLinesLocked() []string {
	if s.terminal.IsAltBufferActive() {
		s.captureTerminalHistoryLocked()
		return joinedTerminalHistory(s.terminalHistory, s.lastTerminalScreen)
	}

	normal := terminalBufferLines(s.terminal.NormalBuffer())
	if len(s.terminalHistory) == 0 && len(s.lastTerminalScreen) == 0 {
		return normal
	}
	return appendWithLineOverlap(
		joinedTerminalHistory(s.terminalHistory, s.lastTerminalScreen),
		normal,
	)
}

func (s *liveSession) appendTerminalHistoryLocked(lines []string) {
	if len(lines) == 0 {
		return
	}
	s.terminalHistory = appendWithLineOverlap(s.terminalHistory, lines)
	if len(s.terminalHistory) > maximumTerminalHistoryLines {
		s.terminalHistory = cloneLines(
			s.terminalHistory[len(s.terminalHistory)-maximumTerminalHistoryLines:],
		)
	}
}

func terminalBufferLines(buffer *xterm.Buffer) []string {
	lines := make([]string, 0, buffer.Lines.Length())
	for index := 0; index < buffer.Lines.Length(); index++ {
		line := buffer.Lines.Get(index)
		if line == nil {
			lines = append(lines, "")
			continue
		}
		lines = append(
			lines,
			strings.TrimRightFunc(line.TranslateToString(true, 0, -1), unicode.IsSpace),
		)
	}
	return trimEmptyTerminalRows(lines)
}

func trimEmptyTerminalRows(lines []string) []string {
	start := 0
	for start < len(lines) && lines[start] == "" {
		start++
	}
	end := len(lines)
	for end > start && lines[end-1] == "" {
		end--
	}
	return cloneLines(lines[start:end])
}

func joinedTerminalHistory(history, screen []string) []string {
	result := make([]string, 0, len(history)+len(screen))
	result = append(result, history...)
	result = appendWithLineOverlap(result, screen)
	return result
}

func appendWithLineOverlap(existing, addition []string) []string {
	if len(existing) == 0 {
		return cloneLines(addition)
	}
	if len(addition) == 0 || containsLineSequence(existing, addition) {
		return cloneLines(existing)
	}
	overlap := longestSuffixPrefix(existing, addition)
	result := make([]string, 0, len(existing)+len(addition)-overlap)
	result = append(result, existing...)
	result = append(result, addition[overlap:]...)
	return result
}

func containsLineSequence(lines, candidate []string) bool {
	if len(candidate) == 0 {
		return true
	}
	if len(candidate) > len(lines) {
		return false
	}
	for start := 0; start <= len(lines)-len(candidate); start++ {
		matched := true
		for offset := range candidate {
			if lines[start+offset] != candidate[offset] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func longestSuffixPrefix(left, right []string) int {
	maximum := min(len(left), len(right))
	for length := maximum; length > 0; length-- {
		matched := true
		for offset := 0; offset < length; offset++ {
			if left[len(left)-length+offset] != right[offset] {
				matched = false
				break
			}
		}
		if matched {
			return length
		}
	}
	return 0
}

func commonLinePrefix(left, right []string) int {
	maximum := min(len(left), len(right))
	for index := 0; index < maximum; index++ {
		if left[index] != right[index] {
			return index
		}
	}
	return maximum
}

func cloneLines(lines []string) []string {
	return append([]string(nil), lines...)
}
