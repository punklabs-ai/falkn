package notifications

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/punklabs-ai/falkn/internal/agent"
	"github.com/punklabs-ai/falkn/internal/protocol"
)

func TestConfigurePersistsAndUpdatesRegistration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "notifications.json")
	params := protocol.NotificationConfigureParams{
		RelayURL:      "https://falkn.example.com/",
		DeviceID:      "74f79d71-c0b5-4d0b-8f1d-3392c8b8de25",
		DeviceSecret:  "abcdefghijklmnopqrstuvwxyzABCDEFGH",
		EncryptionKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		HostID:        "ead47eba-68bb-42d0-90a4-0a51ae68574b",
		HostName:      "Build Mac",
		EnabledStates: []string{"needs_input", "failed"},
	}
	if err := Configure(path, params); err != nil {
		t.Fatal(err)
	}
	params.HostName = "Updated Mac"
	if err := Configure(path, params); err != nil {
		t.Fatal(err)
	}
	configuration, err := LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.Registrations) != 1 || configuration.Registrations[0].HostName != "Updated Mac" {
		t.Fatalf("unexpected registrations: %#v", configuration.Registrations)
	}
	if got := configuration.Registrations[0].EnabledStates; len(got) != 2 ||
		got[0] != agent.AttentionNeedsInput || got[1] != agent.AttentionFailed {
		t.Fatalf("enabled states = %#v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("configuration permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestNotificationEventPreferencesPreserveLegacyAndEmptySemantics(t *testing.T) {
	legacy := Registration{}
	if !registrationAllows(legacy, agent.AttentionCompleted) {
		t.Fatal("legacy registration did not allow completion events")
	}

	disabled := Registration{EnabledStates: []agent.AttentionState{}}
	if registrationAllows(disabled, agent.AttentionNeedsInput) {
		t.Fatal("explicitly empty preferences allowed an event")
	}

	filtered := Registration{
		EnabledStates: []agent.AttentionState{
			agent.AttentionNeedsInput,
			agent.AttentionFailed,
		},
	}
	if !registrationAllows(filtered, agent.AttentionNeedsInput) {
		t.Fatal("enabled needs-input event was rejected")
	}
	if registrationAllows(filtered, agent.AttentionCompleted) {
		t.Fatal("disabled completion event was allowed")
	}
}

func TestConfigurePersistsAnExplicitlyEmptyEventList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	err := Configure(path, protocol.NotificationConfigureParams{
		RelayURL:      "https://falkn.example.com",
		DeviceID:      "74f79d71-c0b5-4d0b-8f1d-3392c8b8de25",
		DeviceSecret:  "abcdefghijklmnopqrstuvwxyzABCDEFGH",
		EncryptionKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		HostID:        "ead47eba-68bb-42d0-90a4-0a51ae68574b",
		EnabledStates: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Registrations[0].EnabledStates == nil {
		t.Fatal("explicitly empty event list was converted to legacy all-events behavior")
	}
}

func TestConfigureRejectsUnknownNotificationEvent(t *testing.T) {
	err := Configure(filepath.Join(t.TempDir(), "notifications.json"), protocol.NotificationConfigureParams{
		RelayURL:      "https://falkn.example.com",
		DeviceID:      "74f79d71-c0b5-4d0b-8f1d-3392c8b8de25",
		DeviceSecret:  "abcdefghijklmnopqrstuvwxyzABCDEFGH",
		EncryptionKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		HostID:        "ead47eba-68bb-42d0-90a4-0a51ae68574b",
		EnabledStates: []string{"unexpected"},
	})
	if err == nil {
		t.Fatal("Configure() accepted an unknown notification event")
	}
}

func TestConfigureRejectsInsecureRelay(t *testing.T) {
	err := Configure(filepath.Join(t.TempDir(), "notifications.json"), protocol.NotificationConfigureParams{
		RelayURL:      "http://relay.example.com",
		DeviceID:      "74f79d71-c0b5-4d0b-8f1d-3392c8b8de25",
		DeviceSecret:  "abcdefghijklmnopqrstuvwxyzABCDEFGH",
		EncryptionKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		HostID:        "ead47eba-68bb-42d0-90a4-0a51ae68574b",
	})
	if err == nil {
		t.Fatal("Configure() succeeded with an insecure relay")
	}
}

func configureParams(deviceID, hostID string) protocol.NotificationConfigureParams {
	return protocol.NotificationConfigureParams{
		RelayURL:      "https://falkn.example.com",
		DeviceID:      deviceID,
		DeviceSecret:  "abcdefghijklmnopqrstuvwxyzABCDEFGH",
		EncryptionKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		HostID:        hostID,
		HostName:      "Build Mac",
		EnabledStates: []string{"needs_input", "completed", "failed"},
	}
}

func TestDeviceThatStopsIntroducingItselfIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	host := "ead47eba-68bb-42d0-90a4-0a51ae68574b"
	replaced := "74f79d71-c0b5-4d0b-8f1d-3392c8b8de25"
	reinstalled := "0d4b1d05-9d0e-4a6a-95b4-2c9b6bcb2f0f"
	installed := time.Now().Add(-registrationRetention - 24*time.Hour)

	if err := configureAt(path, configureParams(replaced, host), installed); err != nil {
		t.Fatal(err)
	}
	if err := configureAt(path, configureParams(reinstalled, host), time.Now()); err != nil {
		t.Fatal(err)
	}

	configuration, err := LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.Registrations) != 1 {
		t.Fatalf("registrations = %#v, want only the device still introducing itself", configuration.Registrations)
	}
	if got := configuration.Registrations[0].DeviceID; got != reinstalled {
		t.Fatalf("remaining device = %s, want %s", got, reinstalled)
	}
	if configuration.Registrations[0].UpdatedAt == 0 {
		t.Fatal("a saved registration did not record when it was last seen")
	}
}

func TestSecondDeviceKeepsItsRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	host := "ead47eba-68bb-42d0-90a4-0a51ae68574b"
	phone := "74f79d71-c0b5-4d0b-8f1d-3392c8b8de25"
	tablet := "0d4b1d05-9d0e-4a6a-95b4-2c9b6bcb2f0f"

	if err := configureAt(path, configureParams(phone, host), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := configureAt(path, configureParams(tablet, host), time.Now()); err != nil {
		t.Fatal(err)
	}

	configuration, err := LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.Registrations) != 2 {
		t.Fatalf("registrations = %#v, want both devices", configuration.Registrations)
	}
}

func TestRegistrationSavedBeforeFalknRecordedTimeKeepsAFullWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	host := "ead47eba-68bb-42d0-90a4-0a51ae68574b"
	undated := Registration{
		RelayURL:      "https://falkn.example.com",
		DeviceID:      "74f79d71-c0b5-4d0b-8f1d-3392c8b8de25",
		DeviceSecret:  "abcdefghijklmnopqrstuvwxyzABCDEFGH",
		EncryptionKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		HostID:        host,
	}
	if err := writeJSONFile(path, Configuration{
		Version: configurationVersion, Registrations: []Registration{undated},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if err := configureAt(path, configureParams("0d4b1d05-9d0e-4a6a-95b4-2c9b6bcb2f0f", host), now); err != nil {
		t.Fatal(err)
	}

	configuration, err := LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.Registrations) != 2 {
		t.Fatalf("registrations = %#v, want the undated device kept", configuration.Registrations)
	}
	if configuration.Registrations[0].UpdatedAt != now.Unix() {
		t.Fatalf("undated registration was not given a window: %#v", configuration.Registrations[0])
	}
}

func TestForgetReleasesOneDeviceOrEveryDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.json")
	host := "ead47eba-68bb-42d0-90a4-0a51ae68574b"
	phone := "74f79d71-c0b5-4d0b-8f1d-3392c8b8de25"
	tablet := "0d4b1d05-9d0e-4a6a-95b4-2c9b6bcb2f0f"
	for _, deviceID := range []string{phone, tablet} {
		if err := Configure(path, configureParams(deviceID, host)); err != nil {
			t.Fatal(err)
		}
	}

	forgotten, err := Forget(path, strings.ToUpper(phone))
	if err != nil {
		t.Fatal(err)
	}
	if forgotten != 1 {
		t.Fatalf("forgot %d registrations, want 1", forgotten)
	}
	configuration, err := LoadConfiguration(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.Registrations) != 1 || configuration.Registrations[0].DeviceID != tablet {
		t.Fatalf("registrations = %#v, want only the tablet", configuration.Registrations)
	}

	if forgotten, err = Forget(path, ""); err != nil || forgotten != 1 {
		t.Fatalf("forgot %d registrations (%v), want 1", forgotten, err)
	}
	if configuration, err = LoadConfiguration(path); err != nil {
		t.Fatal(err)
	}
	if len(configuration.Registrations) != 0 {
		t.Fatalf("registrations = %#v, want none", configuration.Registrations)
	}
}
