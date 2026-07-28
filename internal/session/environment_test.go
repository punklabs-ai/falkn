package session

import (
	"strings"
	"testing"
)

func TestTerminalEnvironmentEnablesColourWithoutDuplicates(t *testing.T) {
	environment := terminalEnvironment(
		[]string{
			"HOME=/tmp/home",
			"TERM=dumb",
			"COLORTERM=",
			"NO_COLOR=1",
			"FORCE_COLOR=0",
		},
		"FALKN_SESSION_ID=falkn-test",
	)
	joined := strings.Join(environment, "\n")
	for _, expected := range []string{
		"HOME=/tmp/home",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"FALKN_SESSION_ID=falkn-test",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("terminal environment %q does not contain %q", environment, expected)
		}
	}
	for _, removed := range []string{"TERM=dumb", "NO_COLOR=", "FORCE_COLOR="} {
		if strings.Contains(joined, removed) {
			t.Fatalf("terminal environment retained %q: %q", removed, environment)
		}
	}
	termEntries := 0
	for _, entry := range environment {
		if strings.HasPrefix(entry, "TERM=") {
			termEntries++
		}
	}
	if termEntries != 1 {
		t.Fatalf("terminal environment contains %d TERM entries: %q", termEntries, environment)
	}
}
