package agent

import (
	"errors"
	"fmt"
	"strings"
)

const (
	PermissionStandard   = "standard"
	PermissionFullAccess = "full_access"
	maxAdditionalArgs    = 64
	maxArgumentBytes     = 4_096
	maxTotalArgBytes     = 16_384
)

func LaunchArguments(spec Spec, permissionMode string, additional []string) ([]string, error) {
	if permissionMode == "" {
		permissionMode = PermissionStandard
	}
	if permissionMode != PermissionStandard && permissionMode != PermissionFullAccess {
		return nil, fmt.Errorf("unsupported permission mode %q", permissionMode)
	}
	if err := validateAdditionalArguments(spec.ID, permissionMode, additional); err != nil {
		return nil, err
	}

	arguments := append([]string(nil), additional...)
	if permissionMode != PermissionFullAccess {
		return arguments, nil
	}
	switch spec.ID {
	case "codex":
		return append([]string{"--dangerously-bypass-approvals-and-sandbox"}, arguments...), nil
	case "claude":
		return append([]string{"--dangerously-skip-permissions"}, arguments...), nil
	default:
		return nil, fmt.Errorf("%s does not support Full Access sessions", spec.DisplayName)
	}
}

func validateAdditionalArguments(agentID, permissionMode string, arguments []string) error {
	if len(arguments) > maxAdditionalArgs {
		return fmt.Errorf("additional arguments are limited to %d values", maxAdditionalArgs)
	}
	total := 0
	for _, argument := range arguments {
		if argument == "" {
			return errors.New("additional arguments cannot contain an empty value")
		}
		if strings.IndexByte(argument, 0) >= 0 {
			return errors.New("additional arguments cannot contain a null byte")
		}
		if len(argument) > maxArgumentBytes {
			return fmt.Errorf("an additional argument exceeded %d bytes", maxArgumentBytes)
		}
		total += len(argument)
	}
	if total > maxTotalArgBytes {
		return fmt.Errorf("additional arguments exceeded %d bytes", maxTotalArgBytes)
	}
	if permissionMode == PermissionStandard && requestsFullAccess(agentID, arguments) {
		return errors.New("enable Full Access instead of supplying its bypass flag as an additional argument")
	}
	return nil
}

func requestsFullAccess(agentID string, arguments []string) bool {
	for index, argument := range arguments {
		lower := strings.ToLower(strings.TrimSpace(argument))
		next := ""
		if index+1 < len(arguments) {
			next = strings.ToLower(strings.TrimSpace(arguments[index+1]))
		}
		switch agentID {
		case "claude":
			if lower == "--dangerously-skip-permissions" ||
				lower == "--permission-mode=bypasspermissions" ||
				(lower == "--permission-mode" && next == "bypasspermissions") {
				return true
			}
		case "codex":
			if lower == "--dangerously-bypass-approvals-and-sandbox" || lower == "--yolo" ||
				lower == "--sandbox=danger-full-access" ||
				lower == "--ask-for-approval=never" ||
				(lower == "--sandbox" && next == "danger-full-access") ||
				((lower == "--ask-for-approval" || lower == "-a") && next == "never") ||
				((lower == "--config" || lower == "-c") && dangerousConfig(next)) ||
				(strings.HasPrefix(lower, "--config=") && dangerousConfig(strings.TrimPrefix(lower, "--config="))) ||
				(strings.HasPrefix(lower, "-c=") && dangerousConfig(strings.TrimPrefix(lower, "-c="))) {
				return true
			}
		}
	}
	return false
}

func dangerousConfig(value string) bool {
	compact := strings.ReplaceAll(value, " ", "")
	return strings.Contains(compact, "sandbox_mode=\"danger-full-access\"") ||
		strings.Contains(compact, "sandbox_mode='danger-full-access'") ||
		strings.Contains(compact, "sandbox_mode=danger-full-access") ||
		strings.Contains(compact, "approval_policy=\"never\"") ||
		strings.Contains(compact, "approval_policy='never'") ||
		strings.Contains(compact, "approval_policy=never")
}
