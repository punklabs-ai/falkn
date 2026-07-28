package server

import (
	"fmt"
	"os"
	"strings"
)

// DefaultAttachPrefix is Control-Backslash (0x1C), the escape byte used by
// dtach and abduco. Falkn binds two hotkeys rather than a multiplexer keymap,
// so it reserves a control character that shells, editors, and coding agents
// leave alone instead of tmux's heavily claimed Control-B.
const DefaultAttachPrefix byte = 0x1c

// AttachPrefixVariable names the environment variable that overrides the
// hotkey prefix for a session.
const AttachPrefixVariable = "FALKN_PREFIX"

// reservedAttachPrefixes are control characters whose loss is silent and hard
// to diagnose: binding the prefix to one of them would swallow interrupt,
// end-of-file, or suspend for every program in the session.
var reservedAttachPrefixes = map[byte]string{
	0x03: "interrupt",
	0x04: "end-of-file",
	0x1a: "suspend",
}

// AttachPrefix resolves the hotkey prefix from the environment, falling back
// to DefaultAttachPrefix when FALKN_PREFIX is unset or empty.
func AttachPrefix() (byte, error) {
	return parseAttachPrefix(os.Getenv(AttachPrefixVariable))
}

func parseAttachPrefix(value string) (byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultAttachPrefix, nil
	}
	// "^x" is a spelling of the modifier, so it is stripped only when it stands
	// in for "ctrl-"; "ctrl-^" names the caret itself.
	key := strings.ToLower(value)
	switch {
	case strings.HasPrefix(key, "ctrl-"):
		key = strings.TrimPrefix(key, "ctrl-")
	case strings.HasPrefix(key, "ctrl+"):
		key = strings.TrimPrefix(key, "ctrl+")
	case len(key) > 1 && strings.HasPrefix(key, "^"):
		key = strings.TrimPrefix(key, "^")
	}
	if len([]rune(key)) != 1 {
		return 0, fmt.Errorf(
			"%s=%q is not a control key; use a form like ctrl-\\ or ctrl-a",
			AttachPrefixVariable, value,
		)
	}
	character := key[0]
	switch {
	case character >= 'a' && character <= 'z':
	case character >= '@' && character <= '_':
	default:
		return 0, fmt.Errorf(
			"%s=%q is not a control key; use a letter or one of @ [ \\ ] ^ _",
			AttachPrefixVariable, value,
		)
	}
	prefix := character & 0x1f
	if purpose, reserved := reservedAttachPrefixes[prefix]; reserved {
		return 0, fmt.Errorf(
			"%s=%q would take over the terminal %s key; choose another prefix",
			AttachPrefixVariable, value, purpose,
		)
	}
	return prefix, nil
}

// AttachPrefixName renders a prefix byte the way it is written in
// documentation and help output, for example "Ctrl-\".
func AttachPrefixName(prefix byte) string {
	return "Ctrl-" + string(rune(prefix|0x40))
}
