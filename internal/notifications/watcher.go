package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/punklabs-ai/falkn/internal/agent"
	"github.com/punklabs-ai/falkn/internal/protocol"
)

const (
	pollInterval        = 2 * time.Second
	maxRPCResponseSize  = 4 * 1024 * 1024
	fastCompletionGrace = 4 * time.Second
	visibleSessionGrace = 5 * time.Second
	markerRetention     = 2 * time.Minute
)

var readActivityTimestamp = activityTimestamp

type WatcherPaths struct {
	Socket   string
	Config   string
	State    string
	Lock     string
	Activity string
	Presence string
	Follow   string
}

type watcherState struct {
	Version  int                     `json:"version"`
	Sessions map[string]sessionState `json:"sessions"`
}

type sessionState struct {
	Attention    agent.AttentionState `json:"attention,omitempty"`
	Fingerprint  string               `json:"fingerprint,omitempty"`
	Activity     int64                `json:"activity,omitempty"`
	AwaitingWork bool                 `json:"awaiting_work,omitempty"`
}

type rpcEnvelope struct {
	Version int                `json:"version"`
	OK      bool               `json:"ok"`
	Result  json.RawMessage    `json:"result"`
	Error   *protocol.RPCError `json:"error,omitempty"`
}

func Watch(paths WatcherPaths) error {
	executable, _ := os.Executable()
	executableInfo, _ := os.Stat(executable)
	lock, err := os.OpenFile(paths.Lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open notification watcher lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	client := &http.Client{Timeout: 12 * time.Second}
	// Whatever is already on screen when a watcher starts is history. An
	// upgrade, a crash, or a machine that was asleep can leave hours of settled
	// sessions behind, and announcing all of them at once is the one moment a
	// phone is guaranteed to be wrong about what just happened.
	baseline := true
	for {
		// Release the singleton lock after an atomic binary upgrade before
		// another poll can apply policy from the previous release. A normal
		// client request or the installer will then launch the new watcher.
		if watcherExecutableReplaced(executable, executableInfo) {
			return nil
		}
		if err := poll(context.Background(), client, paths, baseline); err != nil {
			fmt.Fprintln(os.Stderr, "notification watcher:", err)
		} else {
			baseline = false
		}
		time.Sleep(pollInterval)
	}
}

func watcherExecutableReplaced(path string, original os.FileInfo) bool {
	if path == "" || original == nil {
		return false
	}
	current, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return err == nil && !os.SameFile(original, current)
}

func poll(ctx context.Context, client *http.Client, paths WatcherPaths, baseline bool) error {
	configuration, err := LoadConfiguration(paths.Config)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(configuration.Registrations) == 0 {
		return nil
	}

	var list protocol.ListResult
	if err := call(paths.Socket, "list", protocol.EmptyParams{}, &list); err != nil {
		return err
	}
	state := loadWatcherState(paths.State)
	changed := false
	seen := make(map[string]bool, len(list.Sessions))
	for _, session := range list.Sessions {
		seen[session.ID] = true
		var transcript protocol.TranscriptResult
		if err := call(paths.Socket, "transcript", protocol.TranscriptParams{
			SessionID: session.ID, HistoryLines: 600,
		}, &transcript); err != nil {
			continue
		}
		attention := agent.AttentionStateFor(
			session.Kind, session.AgentID, transcript.Transcript, transcript.Status,
		)
		fingerprint := transcriptFingerprint(transcript.Transcript, attention)
		previous := state.Sessions[session.ID]
		activity := readActivityTimestamp(paths.Activity, session.ID)
		if baseline {
			state.Sessions[session.ID] = sessionState{
				Attention: attention, Fingerprint: fingerprint, Activity: activity,
			}
			changed = true
			continue
		}
		if activity > previous.Activity {
			state.Sessions[session.ID] = sessionState{
				Activity: activity, AwaitingWork: true, Fingerprint: previous.Fingerprint,
			}
			changed = true
			continue
		}
		if previous.AwaitingWork {
			if attention == agent.AttentionNone {
				previous.AwaitingWork = false
				previous.Fingerprint = fingerprint
				state.Sessions[session.ID] = previous
				changed = true
				continue
			}
			if !readyAfterFastCompletion(previous, attention, fingerprint, time.Now()) {
				continue
			}
			previous = sessionState{Activity: activity}
		}
		if attention == agent.AttentionNone {
			if previous.Attention != attention {
				state.Sessions[session.ID] = sessionState{Fingerprint: fingerprint, Activity: activity}
				changed = true
			}
			continue
		}
		if previous.Attention != agent.AttentionNone {
			continue
		}
		if !sessionFollowed(paths.Follow, session.ID) {
			// The phone has never opened this session, so it has no thread of
			// work to interrupt anyone about. Record the episode as reported
			// anyway: adopting the session later must not replay what the
			// terminal already dealt with.
			state.Sessions[session.ID] = sessionState{
				Attention: attention, Fingerprint: fingerprint, Activity: activity,
			}
			changed = true
			continue
		}
		if session.AttachedClients > 0 || session.NotificationsMuted ||
			sessionRecentlyViewed(paths.Presence, session.ID, time.Now()) {
			continue
		}
		// Input can arrive after the transcript was classified but before the
		// relay request begins. Recheck the activity marker at the last possible
		// moment so a prompt the user already answered cannot emit a stale push.
		latestActivity := readActivityTimestamp(paths.Activity, session.ID)
		if latestActivity > activity {
			state.Sessions[session.ID] = sessionState{
				Activity: latestActivity, AwaitingWork: true, Fingerprint: previous.Fingerprint,
			}
			changed = true
			continue
		}

		allDelivered := true
		for _, registration := range configuration.Registrations {
			if !registrationAllows(registration, attention) ||
				registrationExpired(registration, time.Now()) {
				continue
			}
			deliveryContext, cancel := context.WithTimeout(ctx, 12*time.Second)
			err := deliver(deliveryContext, client, registration, session, attention, fingerprint)
			cancel()
			if err == nil {
				continue
			}
			if errors.Is(err, errRelayForgotDevice) {
				// Holding the episode open for a device that no longer exists
				// would repeat this request every two seconds for as long as
				// the session sits there. Let the device go instead.
				fmt.Fprintf(os.Stderr, "notification watcher: releasing device %s: %v\n",
					registration.DeviceID, err)
				if _, forgetErr := Forget(paths.Config, registration.DeviceID); forgetErr != nil {
					fmt.Fprintln(os.Stderr, "notification watcher:", forgetErr)
				}
				continue
			}
			allDelivered = false
			fmt.Fprintf(os.Stderr, "notification watcher: session %s: %v\n", session.ID, err)
		}
		if allDelivered {
			state.Sessions[session.ID] = sessionState{
				Attention: attention, Fingerprint: fingerprint, Activity: activity,
			}
			changed = true
		}
	}
	for sessionID := range state.Sessions {
		if !seen[sessionID] {
			delete(state.Sessions, sessionID)
			changed = true
		}
	}
	pruneMarkers(paths.Activity, seen, time.Now())
	pruneMarkers(paths.Presence, seen, time.Now())
	pruneMarkers(paths.Follow, seen, time.Now())
	if changed {
		return writeJSONFile(paths.State, state)
	}
	return nil
}

// pruneMarkers removes the per-session files left behind by sessions the daemon
// no longer knows about. A marker written for a session created since this
// poll's list is kept until it is old enough to be certain it was not simply
// missed by a snapshot taken moments earlier.
func pruneMarkers(directory string, keep map[string]bool, now time.Time) {
	if directory == "" {
		return
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || keep[entry.Name()] {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < markerRetention {
			continue
		}
		_ = os.Remove(filepath.Join(directory, entry.Name()))
	}
}

// sessionFollowed reports whether the mobile app has taken up this session.
func sessionFollowed(directory, sessionID string) bool {
	if directory == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(directory, sessionID))
	return err == nil
}

func sessionRecentlyViewed(directory, sessionID string, now time.Time) bool {
	timestamp := activityTimestamp(directory, sessionID)
	if timestamp == 0 {
		return false
	}
	age := now.UnixNano() - timestamp
	return age >= 0 && age <= visibleSessionGrace.Nanoseconds()
}

func loadWatcherState(path string) watcherState {
	state := watcherState{Version: 1, Sessions: make(map[string]sessionState)}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &state) != nil || state.Version != 1 {
		return watcherState{Version: 1, Sessions: make(map[string]sessionState)}
	}
	if state.Sessions == nil {
		state.Sessions = make(map[string]sessionState)
	}
	return state
}

func call(socket, method string, params any, destination any) error {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return err
	}
	request := protocol.Request{
		Version: protocol.Version, RequestID: fmt.Sprintf("watch-%d", time.Now().UnixNano()),
		Method: method, Params: paramsJSON,
	}
	connection, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("connect to falknd: %w", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return fmt.Errorf("send watcher request: %w", err)
	}
	if unixConnection, ok := connection.(*net.UnixConn); ok {
		_ = unixConnection.CloseWrite()
	}
	responseData, err := io.ReadAll(io.LimitReader(connection, maxRPCResponseSize+1))
	if err != nil {
		return fmt.Errorf("read watcher response: %w", err)
	}
	if len(responseData) > maxRPCResponseSize {
		return errors.New("watcher response exceeded the safety limit")
	}
	var response rpcEnvelope
	if err := json.Unmarshal(responseData, &response); err != nil {
		return fmt.Errorf("decode watcher response: %w", err)
	}
	if !response.OK {
		if response.Error != nil {
			return fmt.Errorf("falknd rejected watcher request: %s", response.Error.Message)
		}
		return errors.New("falknd rejected watcher request")
	}
	if err := json.Unmarshal(response.Result, destination); err != nil {
		return fmt.Errorf("decode watcher result: %w", err)
	}
	return nil
}

func transcriptFingerprint(transcript string, attention agent.AttentionState) string {
	normalized := strings.TrimSpace(strings.ReplaceAll(transcript, "\r\n", "\n"))
	if len(normalized) > 16*1024 {
		normalized = normalized[len(normalized)-16*1024:]
	}
	digest := sha256.Sum256([]byte(string(attention) + "\n" + normalized))
	return hex.EncodeToString(digest[:])
}

func activityTimestamp(directory, sessionID string) int64 {
	if directory == "" {
		return 0
	}
	info, err := os.Stat(filepath.Join(directory, sessionID))
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

func readyAfterFastCompletion(
	previous sessionState,
	attention agent.AttentionState,
	fingerprint string,
	now time.Time,
) bool {
	return previous.AwaitingWork &&
		attention != agent.AttentionNone &&
		fingerprint != previous.Fingerprint &&
		now.UnixNano()-previous.Activity >= fastCompletionGrace.Nanoseconds()
}
