package server

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	attachHandoffPrefix = "\x1b]1337;FalknAttach="
	attachHandoffSuffix = "\x07"
	maxSessionIDBytes   = 256
)

var errAttachHandoff = errors.New("switch local Falkn attachment")

// AttachHandoffError asks the top-level CLI process to replace itself with an
// attachment to SessionID. Replacing the process ensures the input reader from
// the previous attachment cannot consume input intended for the new session.
type AttachHandoffError struct {
	SessionID string
}

func (e *AttachHandoffError) Error() string {
	return fmt.Sprintf("switch local Falkn attachment to %s", e.SessionID)
}

// WriteAttachHandoff emits a private OSC command. A top-level Falkn attach
// client consumes it as a request to switch sessions; ordinary terminals treat
// it as invisible terminal metadata.
func WriteAttachHandoff(output io.Writer, sessionID string) error {
	if !validAttachHandoffSessionID(sessionID) {
		return errors.New("invalid Falkn session ID")
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(sessionID))
	_, err := fmt.Fprint(output, attachHandoffPrefix, encoded, attachHandoffSuffix)
	return err
}

func attachHandoffSessionID(data []byte) string {
	value := string(data)
	start := strings.LastIndex(value, attachHandoffPrefix)
	if start < 0 {
		return ""
	}
	encodedStart := start + len(attachHandoffPrefix)
	end := strings.Index(value[encodedStart:], attachHandoffSuffix)
	if end < 0 {
		return ""
	}
	encoded := value[encodedStart : encodedStart+end]
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	sessionID := string(decoded)
	if !validAttachHandoffSessionID(sessionID) {
		return ""
	}
	return sessionID
}

// stripAttachHandoffs removes completed Falkn handoff commands from replayed
// terminal history. A handoff is an event for the client that is attached when
// it is emitted, not durable terminal state: replaying it on a later attach
// would immediately send that client away from the session it selected.
//
// Unknown or malformed control strings are preserved byte-for-byte. They may
// belong to the shell or an application, and only a validated Falkn command is
// ours to consume.
func stripAttachHandoffs(data []byte) []byte {
	prefix := []byte(attachHandoffPrefix)
	suffix := []byte(attachHandoffSuffix)
	cursor := 0
	searchFrom := 0
	var stripped []byte

	for searchFrom < len(data) {
		relativeStart := bytes.Index(data[searchFrom:], prefix)
		if relativeStart < 0 {
			break
		}
		start := searchFrom + relativeStart
		encodedStart := start + len(prefix)
		relativeEnd := bytes.Index(data[encodedStart:], suffix)
		if relativeEnd < 0 {
			break
		}
		end := encodedStart + relativeEnd + len(suffix)
		if attachHandoffSessionID(data[start:end]) == "" {
			searchFrom = encodedStart
			continue
		}
		if stripped == nil {
			stripped = make([]byte, 0, len(data)-(end-start))
		}
		stripped = append(stripped, data[cursor:start]...)
		cursor = end
		searchFrom = end
	}
	if stripped == nil {
		return data
	}
	return append(stripped, data[cursor:]...)
}

func validAttachHandoffSessionID(sessionID string) bool {
	if len(sessionID) <= len("falkn-") || len(sessionID) > maxSessionIDBytes ||
		!strings.HasPrefix(sessionID, "falkn-") {
		return false
	}
	for _, character := range sessionID {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}
