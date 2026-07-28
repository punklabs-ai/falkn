package agent

import (
	"regexp"
	"strings"
)

type AttentionState string

const (
	AttentionNone       AttentionState = ""
	AttentionNeedsInput AttentionState = "needs_input"
	AttentionCompleted  AttentionState = "completed"
	AttentionFailed     AttentionState = "failed"
)

var codexIdleFooter = regexp.MustCompile(`(?m)^\s*(?:gpt-[^\n]+|o[134][^\n]*)\s+[·•]\s+[^\n]*$`)

// AttentionStateFor keeps agent-specific terminal heuristics behind one
// boundary so new coding agents can provide their own detector later. The
// session kind separates a shell the user drives from an agent Falkn started
// for them; the two finish for entirely different reasons.
func AttentionStateFor(kind, agentID, transcript, processStatus string) AttentionState {
	if processStatus == "stopped" {
		return AttentionNone
	}
	if processStatus == "ended" {
		// A Falkn shell ends because someone typed exit or closed its last
		// command. That is the ordinary way to finish local work, not an agent
		// that stopped unexpectedly, so it is nothing to report.
		if kind == "shell" {
			return AttentionNone
		}
		return AttentionFailed
	}
	normalized := strings.ToLower(strings.TrimSpace(transcript))
	if normalized == "" {
		return AttentionNone
	}

	switch agentID {
	case "codex":
		return codexAttentionState(normalized)
	case "claude":
		return claudeAttentionState(normalized)
	case "pi":
		return piAttentionState(normalized)
	default:
		return AttentionNone
	}
}

func codexAttentionState(transcript string) AttentionState {
	tail := terminalTail(transcript, 4_000)
	if containsAny(tail,
		"press enter to confirm or esc to cancel",
		"would you like to run the following command",
		"do you want to allow",
		"yes, and don't ask again",
		"approve this command",
	) {
		return AttentionNeedsInput
	}
	if containsAny(lastNonEmptyLine(tail),
		"esc to interrupt",
		"working (",
		"thinking (",
		"running command",
		"booting mcp server:",
	) {
		return AttentionNone
	}
	if hasStableCodexFooter(tail) {
		return AttentionCompleted
	}
	return AttentionNone
}

func claudeAttentionState(transcript string) AttentionState {
	tail := terminalTail(transcript, 4_000)
	if containsAny(tail,
		"do you want to proceed?",
		"allow this command?",
		"yes, allow once",
		"yes, and don't ask again",
		"esc to cancel",
	) {
		return AttentionNeedsInput
	}
	line := lastNonEmptyLine(tail)
	if (strings.HasPrefix(line, "❯") || strings.HasPrefix(line, ">")) &&
		!containsAny(line, "esc to interrupt", "ctrl+c to interrupt") {
		return AttentionCompleted
	}
	return AttentionNone
}

func piAttentionState(transcript string) AttentionState {
	tail := terminalTail(transcript, 4_000)
	if containsAny(tail, "confirm?", "approve?", "allow this") {
		return AttentionNeedsInput
	}
	line := lastNonEmptyLine(tail)
	if line == ">" || strings.HasPrefix(line, "> ") {
		return AttentionCompleted
	}
	return AttentionNone
}

func hasStableCodexFooter(transcript string) bool {
	lines := strings.Split(transcript, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		return codexIdleFooter.MatchString(line)
	}
	return false
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func lastNonEmptyLine(value string) string {
	lines := strings.Split(value, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(lines[index]); line != "" {
			return line
		}
	}
	return ""
}

func terminalTail(value string, maximumBytes int) string {
	if len(value) <= maximumBytes {
		return value
	}
	start := len(value) - maximumBytes
	for start < len(value) && value[start]&0xc0 == 0x80 {
		start++
	}
	return value[start:]
}
