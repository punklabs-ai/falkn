package server

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/punklabs-ai/falkn/internal/agent"
	"github.com/punklabs-ai/falkn/internal/protocol"
)

// Attention colours are shared by the footer and the session switcher so a
// session reads the same way wherever it appears.
const (
	attentionNeedsInputStyle = "\x1b[0;1;38;5;214m"
	attentionFailedStyle     = "\x1b[0;38;5;203m"
	attentionCompletedStyle  = "\x1b[0;38;5;114m"
	// The prefix reverses while it is held, so pressing it is visibly
	// acknowledged rather than leaving the next key a guess.
	prefixArmedStyle = "\x1b[0;7;38;5;75m"
)

// attentionFlag is the single-letter marker shown beside a session.
func attentionFlag(record protocol.SessionRecord) (string, string) {
	if record.Status != "running" {
		return "·", ""
	}
	switch agent.AttentionState(record.Attention) {
	case agent.AttentionNeedsInput:
		return "w", attentionNeedsInputStyle
	case agent.AttentionFailed:
		return "!", attentionFailedStyle
	case agent.AttentionCompleted:
		return "d", attentionCompletedStyle
	default:
		return "r", ""
	}
}

// footerContent is everything the footer can say about the attached session and
// its siblings, before any of it is fitted to the terminal width.
type footerContent struct {
	title                 string
	agent                 string
	permissionMode        string
	attachedClients       int
	notificationsMuted    bool
	supportsNotifications bool
	otherSessions         int
	othersNeedingInput    int
	othersFailed          int
	prefixName            string
	prefixArmed           bool
	sessionNumber         int
	sessionCount          int
}

// hintForms are the ways the switcher reminder can be written, longest first.
// A narrow terminal shortens it rather than dropping it: a binding nothing else
// reveals is worth a few columns, and while the prefix is held this is the only
// thing on screen saying so.
func (content footerContent) hintForms() []string {
	if content.prefixName == "" {
		return nil
	}
	// With one session there is no number worth carrying. With several, the
	// number says which one this is and doubles as the digit that returns to it.
	prefix := content.prefixName
	if content.sessionCount > 1 && content.sessionNumber > 0 {
		prefix = fmt.Sprintf("%s [%d]", prefix, content.sessionNumber)
	}
	return []string{
		prefix + " ↑ sessions",
		prefix + " ↑",
		prefix,
	}
}

// renderHint styles only the prefix itself, so the reminder reads the same
// whether or not the prefix is being held.
func (content footerContent) renderHint(hint string) string {
	if !content.prefixArmed {
		return hint
	}
	rest := strings.TrimPrefix(hint, content.prefixName)
	return prefixArmedStyle + content.prefixName + statusBarStyleSequence + rest
}

func footerContentFor(sessionID string, records []protocol.SessionRecord) footerContent {
	content := footerContent{}
	// Number sessions the way the switcher orders them, so the number in the
	// footer is the digit that returns to this session.
	switchable := switchableSessions(records)
	content.sessionCount = len(switchable)
	for index, record := range switchable {
		if record.ID == sessionID {
			content.sessionNumber = index + 1
			break
		}
	}
	for _, record := range records {
		if record.ID == sessionID {
			content.title = record.Title
			content.agent = record.AgentID
			content.permissionMode = record.PermissionMode
			content.attachedClients = record.AttachedClients
			content.notificationsMuted = record.NotificationsMuted
			continue
		}
		if record.Status != "running" {
			continue
		}
		content.otherSessions++
		switch agent.AttentionState(record.Attention) {
		case agent.AttentionNeedsInput:
			content.othersNeedingInput++
		case agent.AttentionFailed:
			content.othersFailed++
		}
	}
	return content
}

type footerSegment struct {
	text  string
	style string
}

// segments splits the footer into the parts a narrow terminal may discard and
// the parts it may not. Droppable segments are ordered least useful first.
//
// Attention outlives everything else because it is the only thing the footer
// says that cannot be learned from the terminal itself: another session is
// waiting on you. A plain shell has no agent worth naming, and a session
// running with default permissions has no mode worth repeating.
func (content footerContent) segments() (droppable, kept []footerSegment) {
	if content.attachedClients > 1 {
		// This client is one of them; the rest are the phone or a second
		// terminal, and they are why notifications stay quiet.
		droppable = append(droppable, footerSegment{
			text: fmt.Sprintf("+%d watching", content.attachedClients-1),
		})
	}
	if content.title != "" {
		droppable = append(droppable, footerSegment{text: cleanStatusLabel(content.title)})
	}
	if content.agent != "" && content.agent != "shell" {
		droppable = append(droppable, footerSegment{text: cleanStatusLabel(content.agent)})
	}
	if mode := cleanPermissionMode(content.permissionMode); mode != "" {
		droppable = append(droppable, footerSegment{text: mode, style: attentionNeedsInputStyle})
	}

	if content.supportsNotifications && content.notificationsMuted {
		kept = append(kept, footerSegment{text: "muted"})
	}
	if summary, style := content.attentionSummary(); summary != "" {
		kept = append(kept, footerSegment{text: summary, style: style})
	}
	return droppable, kept
}

func (content footerContent) attentionSummary() (string, string) {
	switch {
	case content.othersNeedingInput > 0:
		return fmt.Sprintf("%d needs input", content.othersNeedingInput), attentionNeedsInputStyle
	case content.othersFailed > 0:
		return fmt.Sprintf("%d failed", content.othersFailed), attentionFailedStyle
	case content.otherSessions > 0:
		return fmt.Sprintf("%d others", content.otherSessions), ""
	default:
		return "", ""
	}
}

// cleanPermissionMode names only a mode worth a standing reminder. Falkn runs a
// session either with standard permissions or with full access, and only the
// second is worth saying: naming the ordinary case would spend a segment on
// every session to distinguish none of them.
func cleanPermissionMode(mode string) string {
	if strings.TrimSpace(mode) == agent.PermissionFullAccess {
		return "full access"
	}
	return ""
}

// statusBarLine renders the footer, dropping the least useful segments until it
// fits and padding the remainder with a rule.
func statusBarLine(columns int, content footerContent, menuOpen bool) string {
	width := max(columns-1, 1)
	droppable, kept := content.segments()

	// The switcher binding sits to the right, away from the session's own state.
	// While the switcher is open it explains its own keys, so the invitation to
	// open it is redundant, but the number still says which session this is and
	// dropping it would blink the indicator every time the switcher is used.
	hints := content.hintForms()
	if menuOpen && len(hints) > 0 {
		hints = hints[len(hints)-1:]
	}

	// Keep the reminder if any combination allows it, preferring more of the
	// session's state and a longer reminder in that order. A user who cannot see
	// the binding forgets it, so a segment is worth surrendering for the short
	// form before the reminder itself goes.
	for dropped := 0; dropped <= len(droppable); dropped++ {
		candidate := append(append([]footerSegment{}, droppable[dropped:]...), kept...)
		visible := footerVisibleWidth(candidate)
		for _, hint := range hints {
			if visible+runeCount(hint)+3 > width {
				continue
			}
			// A space each side of the rule keeps the hint off the line work.
			gap := width - visible - runeCount(hint) - 2
			return renderFooter(candidate) + " " + strings.Repeat("─", max(gap, 1)) +
				" " + content.renderHint(hint)
		}
	}
	for dropped := 0; dropped <= len(droppable); dropped++ {
		candidate := append(append([]footerSegment{}, droppable[dropped:]...), kept...)
		if visible := footerVisibleWidth(candidate); visible <= width {
			return padFooter(renderFooter(candidate), visible, width)
		}
	}
	// Even the segments that cannot be dropped do not fit. Keep the mark and let
	// the truncation speak for itself rather than emitting a torn line.
	truncated := truncateRunes("─ Falkn", width)
	return padFooter(truncated, runeCount(truncated), width)
}

func footerVisibleWidth(segments []footerSegment) int {
	width := runeCount("─ Falkn")
	for _, segment := range segments {
		width += runeCount(" · ") + runeCount(segment.text)
	}
	return width
}

func renderFooter(segments []footerSegment) string {
	var builder strings.Builder
	builder.WriteString("─ Falkn")
	for _, segment := range segments {
		builder.WriteString(" · ")
		if segment.style == "" {
			builder.WriteString(segment.text)
			continue
		}
		// Return to the footer's own colour so the rule that follows matches.
		builder.WriteString(segment.style)
		builder.WriteString(segment.text)
		builder.WriteString(statusBarStyleSequence)
	}
	return builder.String()
}

func padFooter(line string, visible, width int) string {
	switch remaining := width - visible; {
	case remaining > 1:
		return line + " " + strings.Repeat("─", remaining-1)
	case remaining == 1:
		// A single spare column is a gap, not a rule.
		return line + " "
	default:
		return line
	}
}

func cleanStatusLabel(value string) string {
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return -1
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "session"
	}
	return value
}

func runeCount(value string) int {
	return len([]rune(value))
}

func truncateRunes(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	if maximum == 1 {
		return "…"
	}
	return string(runes[:maximum-1]) + "…"
}
