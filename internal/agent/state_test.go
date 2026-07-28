package agent

import "testing"

func TestCodexAttentionStates(t *testing.T) {
	tests := []struct {
		name       string
		transcript string
		want       AttentionState
	}{
		{
			name: "idle after response",
			transcript: "• Review complete.\n\n› Run /review on my current changes\n\n" +
				"gpt-5.6-sol xhigh · ~",
			want: AttentionCompleted,
		},
		{
			name:       "approval",
			transcript: "Run rm generated.txt?\nPress enter to confirm or esc to cancel",
			want:       AttentionNeedsInput,
		},
		{
			name:       "working",
			transcript: "• Reviewing files (12s • esc to interrupt)",
			want:       AttentionNone,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := AttentionStateFor("agent", "codex", test.transcript, "running"); got != test.want {
				t.Fatalf("AttentionStateFor() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEndedSessionNeedsAttention(t *testing.T) {
	if got := AttentionStateFor("agent", "codex", "anything", "ended"); got != AttentionFailed {
		t.Fatalf("AttentionStateFor() = %q, want %q", got, AttentionFailed)
	}
}

func TestEndedShellDoesNotNeedAttention(t *testing.T) {
	transcript := "• Review complete.\n\ngpt-5.6-sol xhigh · ~"
	if got := AttentionStateFor("shell", "codex", transcript, "ended"); got != AttentionNone {
		t.Fatalf("AttentionStateFor() = %q, want %q", got, AttentionNone)
	}
}

func TestStoppedSessionDoesNotNeedAttention(t *testing.T) {
	transcript := "Would you like to run the following command?\nPress enter to confirm or esc to cancel"
	if got := AttentionStateFor("agent", "codex", transcript, "stopped"); got != AttentionNone {
		t.Fatalf("AttentionStateFor() = %q, want %q", got, AttentionNone)
	}
}

func TestHistoricalApprovalDoesNotOverrideCurrentCodexIdleState(t *testing.T) {
	transcript := "Press enter to confirm or esc to cancel" + string(make([]byte, 4_100)) +
		"\n• Review complete.\n\ngpt-5.6-sol xhigh · ~"
	if got := AttentionStateFor("agent", "codex", transcript, "running"); got != AttentionCompleted {
		t.Fatalf("AttentionStateFor() = %q, want %q", got, AttentionCompleted)
	}
}
