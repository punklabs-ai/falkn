package session

import "strings"

func cleanTranscript(raw []byte, historyLines int) string {
	lines := cleanedTranscriptLines(raw)
	if historyLines <= 0 {
		historyLines = 600
	}
	if historyLines > maximumTerminalHistoryLines {
		historyLines = maximumTerminalHistoryLines
	}
	if len(lines) > historyLines {
		lines = lines[len(lines)-historyLines:]
	}
	return strings.Join(lines, "\n")
}

func cleanedTranscriptLines(raw []byte) []string {
	clean := stripTerminalControls(raw)
	clean = strings.ToValidUTF8(clean, "")
	clean = strings.ReplaceAll(clean, "\r\n", "\n")
	clean = strings.ReplaceAll(clean, "\r", "\n")
	clean = applyBackspaces(clean)
	clean = strings.TrimRight(clean, "\n")
	if clean == "" {
		return nil
	}

	lines := strings.Split(clean, "\n")
	if len(lines) > maximumTerminalHistoryLines {
		lines = lines[len(lines)-maximumTerminalHistoryLines:]
	}
	return lines
}

func stripTerminalControls(raw []byte) string {
	result := make([]byte, 0, len(raw))
	for index := 0; index < len(raw); {
		if raw[index] != 0x1b {
			if raw[index] == 0 || (raw[index] < 0x20 && raw[index] != '\n' && raw[index] != '\r' && raw[index] != '\t' && raw[index] != '\b') {
				index++
				continue
			}
			result = append(result, raw[index])
			index++
			continue
		}

		index++
		if index >= len(raw) {
			break
		}
		switch raw[index] {
		case '[':
			index++
			for index < len(raw) {
				value := raw[index]
				index++
				if value >= 0x40 && value <= 0x7e {
					break
				}
			}
		case ']':
			index++
			for index < len(raw) {
				if raw[index] == 0x07 {
					index++
					break
				}
				if raw[index] == 0x1b && index+1 < len(raw) && raw[index+1] == '\\' {
					index += 2
					break
				}
				index++
			}
		default:
			index++
		}
	}
	return string(result)
}

func applyBackspaces(value string) string {
	runes := make([]rune, 0, len(value))
	for _, value := range value {
		if value == '\b' {
			if len(runes) > 0 && runes[len(runes)-1] != '\n' {
				runes = runes[:len(runes)-1]
			}
			continue
		}
		runes = append(runes, value)
	}
	return string(runes)
}
