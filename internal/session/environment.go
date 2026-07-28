package session

import "strings"

var terminalEnvironmentKeys = map[string]bool{
	"TERM":           true,
	"COLORTERM":      true,
	"NO_COLOR":       true,
	"CLICOLOR":       true,
	"CLICOLOR_FORCE": true,
	"FORCE_COLOR":    true,
}

// terminalEnvironment prevents the daemon's own launch context from disabling
// presentation features in persistent PTYs. A user can still opt out again
// from inside the Falkn shell before launching an agent.
func terminalEnvironment(environment []string, additional ...string) []string {
	result := make([]string, 0, len(environment)+len(additional)+2)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if terminalEnvironmentKeys[key] {
			continue
		}
		result = append(result, entry)
	}
	result = append(result, "TERM=xterm-256color", "COLORTERM=truecolor")
	return append(result, additional...)
}
