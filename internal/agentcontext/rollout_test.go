package agentcontext

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemainingPercentUsesLatestActiveTokenCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := "" +
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"total_tokens":50000},"model_context_window":100000}}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"total_tokens":18824},"model_context_window":258400}}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	percent, ok := contextRemainingPercentFromRollout(path)
	if !ok {
		t.Fatal("expected a Codex context percentage")
	}
	if percent != 93 {
		t.Fatalf("remaining context was %d%%, want 93%%", percent)
	}
}

func TestRemainingPercentRejectsMissingTelemetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"event_msg","payload":{"type":"other"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := contextRemainingPercentFromRollout(path); ok {
		t.Fatal("unexpected context percentage")
	}
}
