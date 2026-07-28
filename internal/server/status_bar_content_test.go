package server

import (
	"strings"
	"testing"

	"github.com/punklabs-ai/falkn/internal/agent"
	"github.com/punklabs-ai/falkn/internal/protocol"
)

func visibleFooter(line string) string {
	for {
		start := strings.Index(line, "\x1b")
		if start < 0 {
			return line
		}
		end := strings.Index(line[start:], "m")
		if end < 0 {
			return line[:start]
		}
		line = line[:start] + line[start+end+1:]
	}
}

func TestFooterNamesTheSessionAndItsAgent(t *testing.T) {
	line := visibleFooter(statusBarLine(100, footerContent{
		title: "Infrastructure", agent: "codex", supportsNotifications: true,
	}, false))
	for _, expected := range []string{"Falkn", "Infrastructure", "codex"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("footer %q does not contain %q", line, expected)
		}
	}
	for _, unwanted := range []string{"^B", "detach", "alerts", "muted"} {
		if strings.Contains(line, unwanted) {
			t.Fatalf("footer %q still contains %q", line, unwanted)
		}
	}
	if got := runeCount(line); got != 99 {
		t.Fatalf("footer width = %d, want 99", got)
	}
}

func TestFooterOmitsThePlainShellAndDefaultPermissions(t *testing.T) {
	line := visibleFooter(statusBarLine(100, footerContent{
		title: "Infrastructure", agent: "shell", permissionMode: agent.PermissionStandard,
	}, false))
	if strings.Contains(line, "shell") {
		t.Fatalf("footer %q names the plain shell", line)
	}
	if strings.Contains(line, agent.PermissionStandard) {
		t.Fatalf("footer %q names the default permission mode", line)
	}
	if !strings.Contains(line, "Infrastructure") {
		t.Fatalf("footer %q dropped the session title", line)
	}
}

func TestFooterReportsOnlyFullAccessPermissions(t *testing.T) {
	// Falkn runs a session with standard permissions or with full access, and
	// only the second is worth a standing reminder. Naming the ordinary case
	// would spend a segment on every session to distinguish none of them.
	elevated := visibleFooter(statusBarLine(100, footerContent{
		title: "Infra", agent: "codex", permissionMode: agent.PermissionFullAccess,
	}, false))
	if !strings.Contains(elevated, "full access") {
		t.Fatalf("full access was not reported: %q", elevated)
	}
	for _, mode := range []string{agent.PermissionStandard, ""} {
		line := visibleFooter(statusBarLine(100, footerContent{
			title: "Infra", agent: "codex", permissionMode: mode,
		}, false))
		if strings.Contains(line, "standard") || strings.Contains(line, "full access") {
			t.Fatalf("permission mode %q was reported: %q", mode, line)
		}
	}
}

func TestFooterNumbersTheSessionOnlyWhenThereAreSeveral(t *testing.T) {
	records := []protocol.SessionRecord{
		{ID: "falkn-1", Title: "api", Status: "running", CreatedAt: 10},
		{ID: "falkn-2", Title: "web", Status: "running", CreatedAt: 20},
		{ID: "falkn-3", Title: "db", Status: "running", CreatedAt: 30},
		{ID: "falkn-old", Title: "gone", Status: "ended", CreatedAt: 5},
	}
	content := footerContentFor("falkn-2", records)
	content.prefixName = `Ctrl-\`
	// The number has to match the digit that returns to this session, so it
	// follows the switcher's own ordering rather than the list's.
	if content.sessionNumber != 2 || content.sessionCount != 3 {
		t.Fatalf("session %d of %d, want 2 of 3", content.sessionNumber, content.sessionCount)
	}
	line := visibleFooter(statusBarLine(100, content, false))
	if !strings.Contains(line, `Ctrl-\ [2]`) {
		t.Fatalf("footer %q does not number the session", line)
	}

	// A lone session has no number worth carrying.
	alone := footerContentFor("falkn-1", records[:1])
	alone.prefixName = `Ctrl-\`
	if got := visibleFooter(statusBarLine(100, alone, false)); strings.Contains(got, "[1]") {
		t.Fatalf("footer %q numbered a lone session", got)
	}
}

func TestFooterSummarisesOtherSessionsNeedingInput(t *testing.T) {
	records := []protocol.SessionRecord{
		{ID: "falkn-1", Title: "Infra", AgentID: "codex", Status: "running"},
		{ID: "falkn-2", Title: "Web", AgentID: "claude", Status: "running", Attention: "needs_input"},
		{ID: "falkn-3", Title: "Db", AgentID: "codex", Status: "running", Attention: "failed"},
		{ID: "falkn-4", Title: "Old", AgentID: "codex", Status: "stopped", Attention: "needs_input"},
	}
	content := footerContentFor("falkn-1", records)
	if content.otherSessions != 2 {
		t.Fatalf("other sessions = %d, want 2 running ones", content.otherSessions)
	}
	if content.othersNeedingInput != 1 {
		t.Fatalf("others needing input = %d, want 1", content.othersNeedingInput)
	}

	// A session that wants input outranks one that merely failed.
	line := statusBarLine(100, content, false)
	if !strings.Contains(visibleFooter(line), "1 needs input") {
		t.Fatalf("footer %q does not report the waiting session", visibleFooter(line))
	}
	if !strings.Contains(line, attentionNeedsInputStyle) {
		t.Fatalf("waiting session was not coloured: %q", line)
	}
	// Colour has to return to the footer's own style so the rule matches.
	if !strings.Contains(line, attentionNeedsInputStyle+"1 needs input"+statusBarStyleSequence) {
		t.Fatalf("footer did not restore its own colour after the attention segment: %q", line)
	}
}

func TestFooterCountsOtherAttachedClients(t *testing.T) {
	line := visibleFooter(statusBarLine(100, footerContent{
		title: "Infra", agent: "codex", attachedClients: 2,
	}, false))
	if !strings.Contains(line, "+1 watching") {
		t.Fatalf("footer %q does not report the other client", line)
	}
	// One attached client is this one, and is not worth saying.
	alone := visibleFooter(statusBarLine(100, footerContent{title: "Infra", agent: "codex", attachedClients: 1}, false))
	if strings.Contains(alone, "watching") {
		t.Fatalf("footer %q reported the local client", alone)
	}
}

func TestFooterDropsLeastUsefulSegmentsFirstAsItNarrows(t *testing.T) {
	content := footerContent{
		title: "Infrastructure", agent: "codex", permissionMode: agent.PermissionFullAccess,
		attachedClients: 3, othersNeedingInput: 1, otherSessions: 2,
	}
	previous := ""
	for _, columns := range []int{110, 80, 64, 52, 44} {
		line := visibleFooter(statusBarLine(columns, content, false))
		if got := runeCount(line); got != columns-1 {
			t.Fatalf("width %d produced a %d-rune footer", columns, got)
		}
		// Attention is the one thing the terminal cannot otherwise tell you, so
		// it has to survive every width that still shows anything.
		if !strings.Contains(line, "needs input") {
			t.Fatalf("footer at %d columns dropped the attention summary: %q", columns, line)
		}
		if previous != "" && strings.Count(line, "·") > strings.Count(previous, "·") {
			t.Fatalf("narrowing to %d columns added segments: %q after %q", columns, line, previous)
		}
		previous = line
	}
}

func TestAttentionFlagsAreDistinctAndColoured(t *testing.T) {
	for _, testCase := range []struct {
		record protocol.SessionRecord
		flag   string
		styled bool
	}{
		{protocol.SessionRecord{Status: "running", Attention: "needs_input"}, "w", true},
		{protocol.SessionRecord{Status: "running", Attention: "failed"}, "!", true},
		{protocol.SessionRecord{Status: "running", Attention: "completed"}, "d", true},
		{protocol.SessionRecord{Status: "running"}, "r", false},
		{protocol.SessionRecord{Status: "stopped", Attention: "needs_input"}, "·", false},
	} {
		flag, style := attentionFlag(testCase.record)
		if flag != testCase.flag {
			t.Fatalf("%+v flag = %q, want %q", testCase.record, flag, testCase.flag)
		}
		if (style != "") != testCase.styled {
			t.Fatalf("%+v style = %q, styled want %v", testCase.record, style, testCase.styled)
		}
	}
}
