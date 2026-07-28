package notifications

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/punklabs-ai/falkn/internal/agent"
	"github.com/punklabs-ai/falkn/internal/protocol"
)

func TestFastCompletionReleasesAwaitingWorkAfterTranscriptChanges(t *testing.T) {
	activity := time.Now().Add(-fastCompletionGrace - time.Second)
	previous := sessionState{
		Activity: activity.UnixNano(), AwaitingWork: true, Fingerprint: "before-input",
	}
	if !readyAfterFastCompletion(previous, agent.AttentionCompleted, "after-response", time.Now()) {
		t.Fatal("fast completion remained suppressed")
	}
}

func TestFastCompletionDoesNotNotifyFromOnlyAnInputEcho(t *testing.T) {
	activity := time.Now().Add(-time.Second)
	previous := sessionState{
		Activity: activity.UnixNano(), AwaitingWork: true, Fingerprint: "before-input",
	}
	if readyAfterFastCompletion(previous, agent.AttentionCompleted, "input-echo", time.Now()) {
		t.Fatal("fast completion was released before the grace period")
	}
}

func TestFastCompletionRequiresChangedTranscript(t *testing.T) {
	activity := time.Now().Add(-fastCompletionGrace - time.Second)
	previous := sessionState{
		Activity: activity.UnixNano(), AwaitingWork: true, Fingerprint: "unchanged",
	}
	if readyAfterFastCompletion(previous, agent.AttentionNeedsInput, "unchanged", time.Now()) {
		t.Fatal("unchanged attention prompt was emitted twice")
	}
}

func TestWatcherDetectsAtomicExecutableReplacement(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "falknd")
	if err := os.WriteFile(executable, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if watcherExecutableReplaced(executable, original) {
		t.Fatal("unchanged executable was reported as replaced")
	}
	replacement := filepath.Join(directory, "falknd.new")
	if err := os.WriteFile(replacement, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, executable); err != nil {
		t.Fatal(err)
	}
	if !watcherExecutableReplaced(executable, original) {
		t.Fatal("atomic executable replacement was not detected")
	}
}

func TestPollDoesNotDeliverWhenUserAnswersDetectedPrompt(t *testing.T) {
	directory := t.TempDir()
	paths := WatcherPaths{
		Socket:   filepath.Join(directory, "falknd.sock"),
		Config:   filepath.Join(directory, "notifications.json"),
		State:    filepath.Join(directory, "notification-state.json"),
		Activity: filepath.Join(directory, "notification-activity"),
		Follow:   filepath.Join(directory, "notification-follow"),
	}
	session := protocol.SessionRecord{
		ID: "falkn-bb75e4695dd332be", Title: "Test 14", AgentID: "codex", Status: "running",
	}
	if err := MarkActive(paths.Follow, session.ID); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("unix", paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer connection.Close()
				var request protocol.Request
				if json.NewDecoder(connection).Decode(&request) != nil {
					return
				}
				var result any
				switch request.Method {
				case "list":
					result = protocol.ListResult{Sessions: []protocol.SessionRecord{session}}
				case "transcript":
					result = protocol.TranscriptResult{
						Transcript: "Would you like to run the following command?\nPress enter to confirm or esc to cancel",
						Status:     "running",
					}
				default:
					return
				}
				_ = json.NewEncoder(connection).Encode(protocol.Success(request.RequestID, result))
			}()
		}
	}()

	var deliveries atomic.Int32
	relay := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		deliveries.Add(1)
		response.WriteHeader(http.StatusAccepted)
	}))
	defer relay.Close()
	configuration := Configuration{
		Version: configurationVersion,
		Registrations: []Registration{{
			RelayURL: relay.URL, DeviceID: "22222222-2222-4222-8222-222222222222",
			DeviceSecret:  strings.Repeat("s", 32),
			EncryptionKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			HostID:        "11111111-1111-4111-8111-111111111111", HostName: "Build Host",
		}},
	}
	if err := writeJSONFile(paths.Config, configuration); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(paths.State, watcherState{
		Version: 1,
		Sessions: map[string]sessionState{
			session.ID: {Activity: 100},
		},
	}); err != nil {
		t.Fatal(err)
	}

	originalActivityReader := readActivityTimestamp
	activityReads := 0
	readActivityTimestamp = func(string, string) int64 {
		activityReads++
		if activityReads == 1 {
			return 100
		}
		return 200
	}
	defer func() { readActivityTimestamp = originalActivityReader }()

	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.Load(); got != 0 {
		t.Fatalf("relay deliveries = %d, want 0 after the user answered", got)
	}
	state := loadWatcherState(paths.State).Sessions[session.ID]
	if !state.AwaitingWork || state.Activity != 200 {
		t.Fatalf("watcher state = %+v, want awaiting work at activity 200", state)
	}
}

func TestRecentlyViewedSessionSuppressesNotificationsUntilPollingStops(t *testing.T) {
	directory := t.TempDir()
	sessionID := "falkn-bb75e4695dd332be"
	if err := MarkActive(directory, sessionID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if !sessionRecentlyViewed(directory, sessionID, now) {
		t.Fatal("actively viewed session was not recognized")
	}
	if sessionRecentlyViewed(directory, sessionID, now.Add(visibleSessionGrace+time.Second)) {
		t.Fatal("session remained visible after polling grace period elapsed")
	}
}

func TestPollDeliversOnlyAfterSessionIsUnattachedUnmutedAndNotViewed(t *testing.T) {
	directory := t.TempDir()
	paths := WatcherPaths{
		Socket:   filepath.Join(directory, "falknd.sock"),
		Config:   filepath.Join(directory, "notifications.json"),
		State:    filepath.Join(directory, "notification-state.json"),
		Activity: filepath.Join(directory, "notification-activity"),
		Presence: filepath.Join(directory, "notification-presence"),
		Follow:   filepath.Join(directory, "notification-follow"),
	}
	session := protocol.SessionRecord{
		ID: "falkn-bb75e4695dd332be", Title: "Read-only E2E", AgentID: "codex", Status: "running",
	}
	if err := MarkActive(paths.Follow, session.ID); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("unix", paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer connection.Close()
				var request protocol.Request
				if json.NewDecoder(connection).Decode(&request) != nil {
					return
				}
				var result any
				switch request.Method {
				case "list":
					result = protocol.ListResult{Sessions: []protocol.SessionRecord{session}}
				case "transcript":
					result = protocol.TranscriptResult{
						Transcript: "Completed the read-only request.\n\ngpt-5.6-sol xhigh · ~",
						Status:     "running",
					}
				default:
					return
				}
				_ = json.NewEncoder(connection).Encode(protocol.Success(request.RequestID, result))
			}()
		}
	}()

	var deliveries atomic.Int32
	var deliveredAt atomic.Int64
	relay := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		deliveries.Add(1)
		deliveredAt.Store(time.Now().UnixNano())
		if request.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 32) {
			t.Errorf("unexpected relay authorization")
		}
		response.WriteHeader(http.StatusAccepted)
	}))
	defer relay.Close()
	configuration := Configuration{
		Version: configurationVersion,
		Registrations: []Registration{{
			RelayURL: relay.URL, DeviceID: "22222222-2222-4222-8222-222222222222",
			DeviceSecret:  strings.Repeat("s", 32),
			EncryptionKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			HostID:        "11111111-1111-4111-8111-111111111111", HostName: "Build Host",
		}},
	}
	if err := writeJSONFile(paths.Config, configuration); err != nil {
		t.Fatal(err)
	}
	session.AttachedClients = 1
	session.FocusedClients = 0
	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.Load(); got != 0 {
		t.Fatalf("relay deliveries while terminal attached = %d, want 0", got)
	}
	session.AttachedClients = 0
	session.NotificationsMuted = true
	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.Load(); got != 0 {
		t.Fatalf("relay deliveries while session muted = %d, want 0", got)
	}
	session.NotificationsMuted = false

	if err := MarkActive(paths.Presence, session.ID); err != nil {
		t.Fatal(err)
	}

	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.Load(); got != 0 {
		t.Fatalf("relay deliveries while viewed = %d, want 0", got)
	}

	expired := time.Now().Add(-visibleSessionGrace - time.Second)
	presencePath := filepath.Join(paths.Presence, session.ID)
	if err := os.Chtimes(presencePath, expired, expired); err != nil {
		t.Fatal(err)
	}
	beforeDelivery := time.Now().UnixNano()
	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	afterDelivery := time.Now().UnixNano()
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("relay deliveries after leaving = %d, want 1", got)
	}
	if timestamp := deliveredAt.Load(); timestamp < beforeDelivery || timestamp > afterDelivery {
		t.Fatalf("delivery timestamp %d fell outside poll window [%d, %d]", timestamp, beforeDelivery, afterDelivery)
	}

	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("duplicate relay deliveries = %d, want 1", got)
	}
}

// fakeDaemon answers the watcher's list and transcript calls from values the
// test can change between polls.
type fakeDaemon struct {
	sessions   func() []protocol.SessionRecord
	transcript func() protocol.TranscriptResult
}

func (daemon fakeDaemon) serve(t *testing.T, socket string) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer connection.Close()
				var request protocol.Request
				if json.NewDecoder(connection).Decode(&request) != nil {
					return
				}
				var result any
				switch request.Method {
				case "list":
					result = protocol.ListResult{Sessions: daemon.sessions()}
				case "transcript":
					result = daemon.transcript()
				default:
					return
				}
				_ = json.NewEncoder(connection).Encode(protocol.Success(request.RequestID, result))
			}()
		}
	}()
}

func testRegistration(t *testing.T, relayURL string) Configuration {
	t.Helper()
	return Configuration{
		Version: configurationVersion,
		Registrations: []Registration{{
			RelayURL: relayURL, DeviceID: "22222222-2222-4222-8222-222222222222",
			DeviceSecret:  strings.Repeat("s", 32),
			EncryptionKey: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
			HostID:        "11111111-1111-4111-8111-111111111111", HostName: "Build Host",
		}},
	}
}

func TestPollNotifiesOnlyAboutSessionsTheAppHasTakenUp(t *testing.T) {
	directory := t.TempDir()
	paths := WatcherPaths{
		Socket:   filepath.Join(directory, "d.sock"),
		Config:   filepath.Join(directory, "notifications.json"),
		State:    filepath.Join(directory, "notification-state.json"),
		Activity: filepath.Join(directory, "notification-activity"),
		Presence: filepath.Join(directory, "notification-presence"),
		Follow:   filepath.Join(directory, "notification-follow"),
	}
	session := protocol.SessionRecord{
		ID: "falkn-bb75e4695dd332be", Title: "Local work", AgentID: "codex",
		Status: "running", Kind: "shell",
	}
	transcript := "Finished the local change.\n\ngpt-5.6-sol xhigh · ~"
	fakeDaemon{
		sessions: func() []protocol.SessionRecord {
			return []protocol.SessionRecord{session}
		},
		transcript: func() protocol.TranscriptResult {
			return protocol.TranscriptResult{Transcript: transcript, Status: "running"}
		},
	}.serve(t, paths.Socket)

	var deliveries atomic.Int32
	relay := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		deliveries.Add(1)
		response.WriteHeader(http.StatusAccepted)
	}))
	defer relay.Close()
	if err := writeJSONFile(paths.Config, testRegistration(t, relay.URL)); err != nil {
		t.Fatal(err)
	}

	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.Load(); got != 0 {
		t.Fatalf("relay deliveries for a session the app never opened = %d, want 0", got)
	}

	// Opening the session in the app must not replay the turn the terminal
	// already finished with.
	if err := MarkActive(paths.Follow, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.Load(); got != 0 {
		t.Fatalf("relay deliveries replaying a finished turn = %d, want 0", got)
	}

	transcript = "Working on the next request (12s • esc to interrupt)"
	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	transcript = "Finished the request from the phone.\n\ngpt-5.6-sol xhigh · ~"
	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.Load(); got != 1 {
		t.Fatalf("relay deliveries for a turn the app is following = %d, want 1", got)
	}
}

func TestFirstPollRecordsWhatIsAlreadySettledWithoutDelivering(t *testing.T) {
	directory := t.TempDir()
	paths := WatcherPaths{
		Socket:   filepath.Join(directory, "d.sock"),
		Config:   filepath.Join(directory, "notifications.json"),
		State:    filepath.Join(directory, "notification-state.json"),
		Activity: filepath.Join(directory, "notification-activity"),
		Presence: filepath.Join(directory, "notification-presence"),
		Follow:   filepath.Join(directory, "notification-follow"),
	}
	session := protocol.SessionRecord{
		ID: "falkn-bb75e4695dd332be", Title: "Left overnight", AgentID: "codex",
		Status: "running", Kind: "agent",
	}
	if err := MarkActive(paths.Follow, session.ID); err != nil {
		t.Fatal(err)
	}
	fakeDaemon{
		sessions: func() []protocol.SessionRecord {
			return []protocol.SessionRecord{session}
		},
		transcript: func() protocol.TranscriptResult {
			return protocol.TranscriptResult{
				Transcript: "Finished hours ago.\n\ngpt-5.6-sol xhigh · ~", Status: "running",
			}
		},
	}.serve(t, paths.Socket)

	var deliveries atomic.Int32
	relay := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		deliveries.Add(1)
		response.WriteHeader(http.StatusAccepted)
	}))
	defer relay.Close()
	if err := writeJSONFile(paths.Config, testRegistration(t, relay.URL)); err != nil {
		t.Fatal(err)
	}

	if err := poll(context.Background(), relay.Client(), paths, true); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.Load(); got != 0 {
		t.Fatalf("relay deliveries on the watcher's first poll = %d, want 0", got)
	}
	if recorded := loadWatcherState(paths.State).Sessions[session.ID]; recorded.Attention != agent.AttentionCompleted {
		t.Fatalf("baseline attention = %q, want %q", recorded.Attention, agent.AttentionCompleted)
	}
	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	if got := deliveries.Load(); got != 0 {
		t.Fatalf("relay deliveries after the baseline poll = %d, want 0", got)
	}
}

func TestPruneMarkersKeepsLiveAndRecentSessions(t *testing.T) {
	directory := t.TempDir()
	live := "falkn-bb75e4695dd332be"
	forgotten := "falkn-aa75e4695dd332be"
	recent := "falkn-cc75e4695dd332be"
	for _, sessionID := range []string{live, forgotten, recent} {
		if err := MarkActive(directory, sessionID); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-markerRetention - time.Minute)
	for _, sessionID := range []string{live, forgotten} {
		path := filepath.Join(directory, sessionID)
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatal(err)
		}
	}

	pruneMarkers(directory, map[string]bool{live: true}, time.Now())

	if _, err := os.Stat(filepath.Join(directory, live)); err != nil {
		t.Fatalf("marker for a live session was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, recent)); err != nil {
		t.Fatalf("marker for a session created since the last list was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, forgotten)); !os.IsNotExist(err) {
		t.Fatalf("marker for a forgotten session survived: %v", err)
	}
}

func TestWatcherReleasesADeviceTheRelayHasRetired(t *testing.T) {
	directory := t.TempDir()
	paths := WatcherPaths{
		Socket:   filepath.Join(directory, "d.sock"),
		Config:   filepath.Join(directory, "notifications.json"),
		State:    filepath.Join(directory, "notification-state.json"),
		Activity: filepath.Join(directory, "notification-activity"),
		Presence: filepath.Join(directory, "notification-presence"),
		Follow:   filepath.Join(directory, "notification-follow"),
	}
	session := protocol.SessionRecord{
		ID: "falkn-bb75e4695dd332be", Title: "Test", AgentID: "codex",
		Status: "running", Kind: "agent",
	}
	if err := MarkActive(paths.Follow, session.ID); err != nil {
		t.Fatal(err)
	}
	fakeDaemon{
		sessions: func() []protocol.SessionRecord {
			return []protocol.SessionRecord{session}
		},
		transcript: func() protocol.TranscriptResult {
			return protocol.TranscriptResult{
				Transcript: "Finished the request.\n\ngpt-5.6-sol xhigh · ~", Status: "running",
			}
		},
	}.serve(t, paths.Socket)

	var requests atomic.Int32
	relay := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		response.WriteHeader(http.StatusNotFound)
	}))
	defer relay.Close()
	if err := writeJSONFile(paths.Config, testRegistration(t, relay.URL)); err != nil {
		t.Fatal(err)
	}

	if err := poll(context.Background(), relay.Client(), paths, false); err != nil {
		t.Fatal(err)
	}
	configuration, err := LoadConfiguration(paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.Registrations) != 0 {
		t.Fatalf("registrations = %#v, want the retired device released", configuration.Registrations)
	}
	if recorded := loadWatcherState(paths.State).Sessions[session.ID]; recorded.Attention != agent.AttentionCompleted {
		t.Fatalf("attention = %q, want the episode closed rather than retried", recorded.Attention)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("relay requests = %d, want 1", got)
	}
}
