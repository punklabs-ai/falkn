package notifications

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/punklabs-ai/falkn/internal/agent"
	"github.com/punklabs-ai/falkn/internal/protocol"
)

const configurationVersion = 1

// registrationRetention drops a device that has stopped introducing itself. The
// mobile app configures every host it can reach each time it launches, so a
// registration only goes quiet when the app that owns it is gone: reinstalled
// under new credentials, restored onto another phone, or deleted. Those ghosts
// are still perfectly deliverable — the relay holds a live token for each of
// them — so nothing but silence distinguishes them, and every one that survives
// adds another copy of every notification.
const registrationRetention = 30 * 24 * time.Hour

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
var sessionIDPattern = regexp.MustCompile(`^falkn-[0-9a-f]{16}$`)

type Registration struct {
	RelayURL      string                 `json:"relay_url"`
	DeviceID      string                 `json:"device_id"`
	DeviceSecret  string                 `json:"device_secret"`
	EncryptionKey string                 `json:"encryption_key"`
	HostID        string                 `json:"host_id"`
	HostName      string                 `json:"host_name,omitempty"`
	EnabledStates []agent.AttentionState `json:"enabled_states"`
	UpdatedAt     int64                  `json:"updated_at,omitempty"`
}

type Configuration struct {
	Version       int            `json:"version"`
	Registrations []Registration `json:"registrations"`
}

func Configure(path string, params protocol.NotificationConfigureParams) error {
	return configureAt(path, params, time.Now())
}

func configureAt(path string, params protocol.NotificationConfigureParams, now time.Time) error {
	var enabledStates []agent.AttentionState
	if params.EnabledStates != nil {
		enabledStates = make([]agent.AttentionState, len(params.EnabledStates))
		for index, state := range params.EnabledStates {
			enabledStates[index] = agent.AttentionState(state)
		}
	}
	registration := Registration{
		RelayURL:      strings.TrimRight(strings.TrimSpace(params.RelayURL), "/"),
		DeviceID:      strings.ToLower(strings.TrimSpace(params.DeviceID)),
		DeviceSecret:  strings.TrimSpace(params.DeviceSecret),
		EncryptionKey: strings.TrimSpace(params.EncryptionKey),
		HostID:        strings.ToLower(strings.TrimSpace(params.HostID)),
		HostName:      strings.TrimSpace(params.HostName),
		EnabledStates: enabledStates,
		UpdatedAt:     now.Unix(),
	}
	if err := validateRegistration(registration); err != nil {
		return err
	}

	configuration, err := LoadConfiguration(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if configuration.Version == 0 {
		configuration.Version = configurationVersion
	}
	kept := make([]Registration, 0, len(configuration.Registrations)+1)
	for _, existing := range configuration.Registrations {
		if existing.DeviceID == registration.DeviceID && existing.HostID == registration.HostID {
			continue
		}
		if existing.UpdatedAt == 0 {
			// A registration saved before Falkn recorded this gets one full
			// retention window from here rather than expiring on a timestamp
			// it never had.
			existing.UpdatedAt = now.Unix()
		}
		if registrationExpired(existing, now) {
			continue
		}
		kept = append(kept, existing)
	}
	configuration.Registrations = append(kept, registration)
	return writeJSONFile(path, configuration)
}

func registrationExpired(registration Registration, now time.Time) bool {
	if registration.UpdatedAt == 0 {
		return false
	}
	return now.Sub(time.Unix(registration.UpdatedAt, 0)) > registrationRetention
}

// Forget removes the registrations for one device, or for every device when
// deviceID is empty. A phone that reinstalls Falkn arrives under new
// credentials and cannot retire the ones it left behind, so the machine that
// holds them needs its own way to let a device go.
func Forget(path, deviceID string) (int, error) {
	deviceID = strings.ToLower(strings.TrimSpace(deviceID))
	configuration, err := LoadConfiguration(path)
	if err != nil {
		return 0, err
	}
	kept := make([]Registration, 0, len(configuration.Registrations))
	for _, registration := range configuration.Registrations {
		if deviceID == "" || registration.DeviceID == deviceID {
			continue
		}
		kept = append(kept, registration)
	}
	forgotten := len(configuration.Registrations) - len(kept)
	if forgotten == 0 {
		return 0, nil
	}
	configuration.Registrations = kept
	if err := writeJSONFile(path, configuration); err != nil {
		return 0, err
	}
	return forgotten, nil
}

func LoadConfiguration(path string) (Configuration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Configuration{}, err
	}
	var configuration Configuration
	if err := json.Unmarshal(data, &configuration); err != nil {
		return Configuration{}, fmt.Errorf("decode notification configuration: %w", err)
	}
	if configuration.Version != configurationVersion {
		return Configuration{}, fmt.Errorf("unsupported notification configuration version %d", configuration.Version)
	}
	for _, registration := range configuration.Registrations {
		if err := validateRegistration(registration); err != nil {
			return Configuration{}, fmt.Errorf("invalid saved notification registration: %w", err)
		}
	}
	return configuration, nil
}

func validateRegistration(registration Registration) error {
	relayURL, err := url.Parse(registration.RelayURL)
	if err != nil || relayURL.Scheme != "https" || relayURL.Host == "" || relayURL.User != nil {
		return errors.New("relay_url must be a valid HTTPS URL")
	}
	if !uuidPattern.MatchString(registration.DeviceID) {
		return errors.New("device_id must be a UUID")
	}
	if len(registration.DeviceSecret) < 32 || len(registration.DeviceSecret) > 128 {
		return errors.New("device_secret has an invalid length")
	}
	key, err := decodeBase64URL(registration.EncryptionKey)
	if err != nil || len(key) != 32 {
		return errors.New("encryption_key must be a Base64URL-encoded 256-bit key")
	}
	if !uuidPattern.MatchString(registration.HostID) {
		return errors.New("host_id must be a UUID")
	}
	if len(registration.HostName) > 200 {
		return errors.New("host_name is too long")
	}
	seenStates := make(map[agent.AttentionState]bool, len(registration.EnabledStates))
	for _, state := range registration.EnabledStates {
		if state != agent.AttentionNeedsInput &&
			state != agent.AttentionCompleted &&
			state != agent.AttentionFailed {
			return fmt.Errorf("unsupported notification state %q", state)
		}
		if seenStates[state] {
			return fmt.Errorf("duplicate notification state %q", state)
		}
		seenStates[state] = true
	}
	return nil
}

func registrationAllows(registration Registration, state agent.AttentionState) bool {
	if registration.EnabledStates == nil {
		// Registrations written by releases before event preferences existed
		// continue to receive every event.
		return true
	}
	for _, enabled := range registration.EnabledStates {
		if enabled == state {
			return true
		}
	}
	return false
}

func decodeBase64URL(value string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(value)
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create notification directory: %w", err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode notification data: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".falknd-notifications-*")
	if err != nil {
		return fmt.Errorf("create notification file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure notification file: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write notification file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync notification file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close notification file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace notification file: %w", err)
	}
	return nil
}

func randomBytes(count int) ([]byte, error) {
	value := make([]byte, count)
	if _, err := rand.Read(value); err != nil {
		return nil, err
	}
	return value, nil
}

func MarkActive(directory, sessionID string) error {
	if !sessionIDPattern.MatchString(sessionID) {
		return errors.New("session_id has an invalid format")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create notification activity directory: %w", err)
	}
	path := filepath.Join(directory, sessionID)
	value := []byte(fmt.Sprintf("%d\n", time.Now().UnixNano()))
	if err := os.WriteFile(path, value, 0o600); err != nil {
		return fmt.Errorf("record notification activity: %w", err)
	}
	return nil
}
