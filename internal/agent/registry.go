package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const interactivePathMarker = "__FALKND_PATH__"

type Spec struct {
	ID          string
	DisplayName string
	Executable  string
}

type Registry struct {
	ordered []Spec
	byID    map[string]Spec
}

type Resolution struct {
	Executable  string
	Environment []string
}

type ShellResolution struct {
	Executable  string
	Environment []string
}

func DefaultRegistry() *Registry {
	return NewRegistry([]Spec{
		{ID: "codex", DisplayName: "Codex", Executable: "codex"},
		{ID: "claude", DisplayName: "Claude Code", Executable: "claude"},
		{ID: "pi", DisplayName: "Pi", Executable: "pi"},
	})
}

func NewRegistry(specs []Spec) *Registry {
	registry := &Registry{ordered: append([]Spec(nil), specs...), byID: make(map[string]Spec, len(specs))}
	for _, spec := range specs {
		registry.byID[spec.ID] = spec
	}
	return registry
}

func (r *Registry) All() []Spec {
	return append([]Spec(nil), r.ordered...)
}

func (r *Registry) Find(id string) (Spec, bool) {
	spec, ok := r.byID[id]
	return spec, ok
}

func ResolveExecutable(spec Spec) (Resolution, bool) {
	pathValue := mergedPath(interactiveShellPath(), os.Getenv("PATH"))
	executable, ok := lookPath(spec.Executable, pathValue)
	if !ok {
		return Resolution{}, false
	}
	return Resolution{
		Executable:  executable,
		Environment: environmentWithPath(os.Environ(), pathValue),
	}, true
}

func ResolveUserShell() (ShellResolution, error) {
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	info, err := os.Stat(shell)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return ShellResolution{}, fmt.Errorf("user shell %q is not executable", shell)
	}
	pathValue := mergedPath(interactiveShellPath(), os.Getenv("PATH"))
	return ShellResolution{
		Executable:  shell,
		Environment: environmentWithPath(os.Environ(), pathValue),
	}, nil
}

func interactiveShellPath() string {
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, shell, "-ic", `printf '\n__FALKND_PATH__%s\n' "$PATH"`)
	command.Stdin = nil
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return ""
	}

	lines := strings.Split(string(output), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		if strings.HasPrefix(lines[index], interactivePathMarker) {
			return strings.TrimPrefix(lines[index], interactivePathMarker)
		}
	}
	return ""
}

func mergedPath(paths ...string) string {
	seen := make(map[string]bool)
	entries := make([]string, 0)
	for _, pathValue := range paths {
		for _, entry := range filepath.SplitList(pathValue) {
			if entry == "" || seen[entry] {
				continue
			}
			seen[entry] = true
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, string(os.PathListSeparator))
}

func lookPath(name, pathValue string) (string, bool) {
	if name == "" || strings.ContainsRune(name, filepath.Separator) {
		return "", false
	}
	for _, directory := range filepath.SplitList(pathValue) {
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}

func environmentWithPath(environment []string, pathValue string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if strings.HasPrefix(entry, "PATH=") {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "PATH="+pathValue)
}
