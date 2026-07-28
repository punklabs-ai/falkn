package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergedPathPrefersInteractiveEntriesAndRemovesDuplicates(t *testing.T) {
	separator := string(os.PathListSeparator)
	result := mergedPath(
		strings.Join([]string{"/nvm/bin", "/usr/bin"}, separator),
		strings.Join([]string{"/usr/bin", "/bin"}, separator),
	)
	want := strings.Join([]string{"/nvm/bin", "/usr/bin", "/bin"}, separator)
	if result != want {
		t.Fatalf("mergedPath() = %q, want %q", result, want)
	}
}

func TestLookPathFindsExecutableInInteractivePath(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "codex")
	if err := os.WriteFile(executable, []byte("#!/usr/bin/env node\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	resolved, ok := lookPath("codex", directory)
	if !ok || resolved != executable {
		t.Fatalf("lookPath() = %q, %t; want %q, true", resolved, ok, executable)
	}
}

func TestEnvironmentWithPathReplacesInheritedPath(t *testing.T) {
	environment := environmentWithPath([]string{"HOME=/tmp/home", "PATH=/usr/bin"}, "/nvm/bin:/usr/bin")
	joined := strings.Join(environment, "\n")
	if strings.Count(joined, "PATH=") != 1 || !strings.Contains(joined, "PATH=/nvm/bin:/usr/bin") {
		t.Fatalf("unexpected environment: %q", environment)
	}
}
