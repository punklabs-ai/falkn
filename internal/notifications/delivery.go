package notifications

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/punklabs-ai/falkn/internal/agent"
	"github.com/punklabs-ai/falkn/internal/protocol"
)

const maxRelayResponseBytes = 8 * 1024

// errRelayForgotDevice reports a registration the relay will never accept
// again: the device it names has been retired there, or its credentials no
// longer authenticate. Retrying cannot recover either one, and the app registers
// afresh the next time it opens.
var errRelayForgotDevice = errors.New("the relay no longer knows this device")

type encryptedEvent struct {
	DeviceID   string `json:"device_id"`
	EventID    string `json:"event_id"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type eventContents struct {
	Version   int                  `json:"version"`
	HostID    string               `json:"host_id"`
	HostName  string               `json:"host_name,omitempty"`
	SessionID string               `json:"session_id"`
	Title     string               `json:"title"`
	AgentID   string               `json:"agent_id"`
	State     agent.AttentionState `json:"state"`
	CreatedAt int64                `json:"created_at"`
}

func deliver(
	ctx context.Context,
	client *http.Client,
	registration Registration,
	session protocol.SessionRecord,
	state agent.AttentionState,
	fingerprint string,
) error {
	contents := eventContents{
		Version:   1,
		HostID:    registration.HostID,
		HostName:  registration.HostName,
		SessionID: session.ID,
		Title:     session.Title,
		AgentID:   session.AgentID,
		State:     state,
		CreatedAt: time.Now().Unix(),
	}
	plaintext, err := json.Marshal(contents)
	if err != nil {
		return fmt.Errorf("encode notification: %w", err)
	}
	key, err := decodeBase64URL(registration.EncryptionKey)
	if err != nil {
		return fmt.Errorf("decode notification key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("create notification cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("create notification encryption: %w", err)
	}
	nonce, err := randomBytes(gcm.NonceSize())
	if err != nil {
		return fmt.Errorf("create notification nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	eventIDHash := sha256.Sum256([]byte(strings.Join([]string{
		registration.DeviceID, session.ID, string(state), fingerprint,
	}, ":")))
	event := encryptedEvent{
		DeviceID:   registration.DeviceID,
		EventID:    hex.EncodeToString(eventIDHash[:]),
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	}
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode relay event: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, registration.RelayURL+"/v1/events", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create relay request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+registration.DeviceSecret)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxRelayResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read relay response: %w", err)
	}
	if len(responseBody) > maxRelayResponseBytes {
		return fmt.Errorf("relay response exceeded the safety limit")
	}
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w: HTTP %d", errRelayForgotDevice, response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("relay rejected notification with HTTP %d", response.StatusCode)
	}
	return nil
}
