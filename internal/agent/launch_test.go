package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestLaunchArgumentsMapsFullAccessPerAgent(t *testing.T) {
	tests := []struct {
		spec Spec
		want []string
	}{
		{spec: Spec{ID: "codex", DisplayName: "Codex"}, want: []string{"--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-test"}},
		{spec: Spec{ID: "claude", DisplayName: "Claude Code"}, want: []string{"--dangerously-skip-permissions", "--model", "opus"}},
	}
	for _, test := range tests {
		got, err := LaunchArguments(test.spec, PermissionFullAccess, []string{"--model", map[string]string{"codex": "gpt-test", "claude": "opus"}[test.spec.ID]})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s arguments = %#v, want %#v", test.spec.ID, got, test.want)
		}
	}
}

func TestLaunchArgumentsRejectsBypassFlagWithoutFullAccess(t *testing.T) {
	_, err := LaunchArguments(
		Spec{ID: "codex", DisplayName: "Codex"},
		PermissionStandard,
		[]string{"--sandbox", "danger-full-access"},
	)
	if err == nil || !strings.Contains(err.Error(), "enable Full Access") {
		t.Fatalf("error = %v", err)
	}
}

func TestLaunchArgumentsDoesNotInterpretShellSyntax(t *testing.T) {
	want := []string{"--model", "$(touch /tmp/not-created)"}
	got, err := LaunchArguments(Spec{ID: "codex", DisplayName: "Codex"}, PermissionStandard, want)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
}

func TestLaunchArgumentsRejectsFullAccessForUnsupportedAgent(t *testing.T) {
	_, err := LaunchArguments(Spec{ID: "pi", DisplayName: "Pi"}, PermissionFullAccess, nil)
	if err == nil {
		t.Fatal("expected unsupported Full Access error")
	}
}
