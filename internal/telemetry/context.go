package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const maxRolloutTailBytes int64 = 512 * 1024

// ContextRemainingPercent returns Codex's latest active-context percentage.
// Codex keeps this counter in the rollout file held open by its native process.
// A nil result means the agent does not expose compatible telemetry yet.
func ContextRemainingPercent(sessionID string) *int {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	paths := rolloutPathsForSession(sessionID)
	sort.SliceStable(paths, func(left, right int) bool {
		leftInfo, leftError := os.Stat(paths[left])
		rightInfo, rightError := os.Stat(paths[right])
		if leftError != nil || rightError != nil {
			return leftError == nil
		}
		return leftInfo.ModTime().After(rightInfo.ModTime())
	})
	for _, path := range paths {
		if percent, ok := contextRemainingPercentFromRollout(path); ok {
			return &percent
		}
	}
	return nil
}

func contextRemainingPercentFromRollout(path string) (int, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, false
	}
	if info.Size() > maxRolloutTailBytes {
		_, _ = file.Seek(info.Size()-maxRolloutTailBytes, 0)
	}

	var latest *codexTokenInfo
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), int(maxRolloutTailBytes))
	for scanner.Scan() {
		var event codexEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil ||
			event.Type != "event_msg" ||
			event.Payload.Type != "token_count" ||
			event.Payload.Info == nil ||
			event.Payload.Info.LastTokenUsage == nil {
			continue
		}
		latest = event.Payload.Info
	}
	if latest == nil || latest.ModelContextWindow <= 0 {
		return 0, false
	}

	used := latest.LastTokenUsage.TotalTokens
	remaining := 100 - (float64(used) / float64(latest.ModelContextWindow) * 100)
	return min(100, max(0, int(math.Round(remaining)))), true
}

type codexEvent struct {
	Type    string `json:"type"`
	Payload struct {
		Type string          `json:"type"`
		Info *codexTokenInfo `json:"info"`
	} `json:"payload"`
}

type codexTokenInfo struct {
	LastTokenUsage *struct {
		TotalTokens int64 `json:"total_tokens"`
	} `json:"last_token_usage"`
	ModelContextWindow int64 `json:"model_context_window"`
}

func rolloutPathsForSession(sessionID string) []string {
	switch runtime.GOOS {
	case "linux":
		return linuxRolloutPaths(sessionID)
	case "darwin":
		return darwinRolloutPaths(sessionID)
	default:
		return nil
	}
}

func linuxRolloutPaths(sessionID string) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	marker := []byte("FALKN_SESSION_ID=" + sessionID + "\x00")
	paths := map[string]struct{}{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		processPath := filepath.Join("/proc", entry.Name())
		environment, err := os.ReadFile(filepath.Join(processPath, "environ"))
		if err != nil || !bytes.Contains(environment, marker) {
			continue
		}
		collectRolloutFDs(filepath.Join(processPath, "fd"), paths)
	}
	return mapKeys(paths)
}

func darwinRolloutPaths(sessionID string) []string {
	output, err := exec.Command("/bin/ps", "eww", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	marker := "FALKN_SESSION_ID=" + sessionID
	paths := map[string]struct{}{}
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, marker) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		lsof, err := exec.Command("/usr/sbin/lsof", "-a", "-p", fields[0], "-Fn").Output()
		if err != nil {
			continue
		}
		for _, record := range strings.Split(string(lsof), "\n") {
			if strings.HasPrefix(record, "n") {
				addRolloutPath(strings.TrimPrefix(record, "n"), paths)
			}
		}
	}
	return mapKeys(paths)
}

func collectRolloutFDs(directory string, paths map[string]struct{}) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(directory, entry.Name()))
		if err == nil {
			addRolloutPath(target, paths)
		}
	}
}

func addRolloutPath(path string, paths map[string]struct{}) {
	if strings.Contains(path, string(filepath.Separator)+".codex"+string(filepath.Separator)+"sessions"+string(filepath.Separator)) &&
		strings.HasSuffix(path, ".jsonl") {
		paths[path] = struct{}{}
	}
}

func mapKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
