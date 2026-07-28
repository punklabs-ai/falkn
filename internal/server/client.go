package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/punklabs-ai/falkn/internal/notifications"
	"github.com/punklabs-ai/falkn/internal/protocol"
	"github.com/punklabs-ai/falkn/internal/telemetry"
)

const maxResponseBytes = 4 * 1024 * 1024

// A request carries no device identity, so where it entered Falkn is the only
// evidence of who is asking. `falkn rpc` is the mobile app's entry point over
// SSH; the CLI answers its own questions in process.
type requestOrigin int

const (
	remoteOrigin requestOrigin = iota
	localOrigin
)

// Call serves one request from a remote client, which in practice is the mobile
// app reaching this host over SSH.
func Call(encodedRequest string, output io.Writer) error {
	return serve(encodedRequest, output, remoteOrigin)
}

// CallLocal serves one request the falkn CLI makes on its own behalf. Local
// work never asks the phone to follow a session: someone is already sitting at
// the terminal it belongs to.
func CallLocal(encodedRequest string, output io.Writer) error {
	return serve(encodedRequest, output, localOrigin)
}

func serve(encodedRequest string, output io.Writer, origin requestOrigin) error {
	request, err := base64.StdEncoding.DecodeString(encodedRequest)
	if err != nil {
		return errors.New("request is not valid Base64")
	}
	if len(request) == 0 || len(request) > maxRequestBytes {
		return fmt.Errorf("request must contain between 1 and %d bytes", maxRequestBytes)
	}
	var envelope protocol.Request
	if err := json.Unmarshal(request, &envelope); err != nil {
		return errors.New("decoded request is not valid JSON")
	}

	paths, err := RuntimePaths()
	if err != nil {
		return err
	}
	if envelope.Method == "notifications.configure" {
		return configureNotifications(envelope, paths, output)
	}
	switch envelope.Method {
	case "uploads.prepare":
		return prepareUpload(envelope, paths, output)
	case "uploads.list":
		return listUploads(envelope, paths, output)
	case "uploads.delete":
		return deleteUpload(envelope, paths, output)
	case "uploads.clear":
		return clearUploads(envelope, paths, output)
	}
	// Watchers are independent of the daemon so an interrupted or upgraded
	// watcher can be recovered by any normal Falkn client request.
	_ = ensureNotificationWatcher(paths)
	connection, err := connect(paths.Socket)
	if err != nil {
		if err := startDaemon(paths); err != nil {
			return err
		}
		connection, err = waitForDaemon(paths.Socket, 3*time.Second)
		if err != nil {
			return err
		}
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(20 * time.Second))

	if _, err := connection.Write(append(request, '\n')); err != nil {
		return fmt.Errorf("send request to falknd: %w", err)
	}
	if unixConnection, ok := connection.(*net.UnixConn); ok {
		_ = unixConnection.CloseWrite()
	}
	response, err := io.ReadAll(io.LimitReader(connection, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read falknd response: %w", err)
	}
	if len(response) > maxResponseBytes {
		return errors.New("falknd response exceeded the safety limit")
	}
	if !json.Valid(bytes.TrimSpace(response)) {
		return errors.New("falknd returned malformed JSON")
	}
	markNotificationActivity(envelope, response, paths)
	markNotificationPresence(envelope, response, paths)
	if origin == remoteOrigin {
		markNotificationFollow(envelope, response, paths)
	}
	cleanupDeletedSessionUploads(envelope, response, paths)
	response = enrichTranscriptResponse(envelope, response)
	_, err = output.Write(response)
	return err
}

func markNotificationPresence(request protocol.Request, response []byte, paths Paths) {
	if request.Method != "transcript" {
		return
	}
	var responseEnvelope struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(response, &responseEnvelope) != nil || !responseEnvelope.OK {
		return
	}
	var params protocol.TranscriptParams
	if json.Unmarshal(request.Params, &params) != nil || params.SessionID == "" {
		return
	}
	_ = notifications.MarkActive(paths.NotificationPresence, params.SessionID)
}

// markNotificationFollow records that the mobile app has taken up a session:
// it started one, opened the conversation, or answered it. Only those sessions
// are notified about, because a notification is how the app reports on work it
// is already carrying. A session created and used at the terminal is never
// news to a phone that has never seen it.
func markNotificationFollow(request protocol.Request, response []byte, paths Paths) {
	var responseEnvelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Session protocol.SessionRecord `json:"session"`
		} `json:"result"`
	}
	if json.Unmarshal(response, &responseEnvelope) != nil || !responseEnvelope.OK {
		return
	}
	var sessionID string
	switch request.Method {
	case "create", "shell.create":
		sessionID = responseEnvelope.Result.Session.ID
	case "transcript", "send", "key":
		var params struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(request.Params, &params) != nil {
			return
		}
		sessionID = params.SessionID
	default:
		return
	}
	if sessionID == "" {
		return
	}
	_ = notifications.MarkActive(paths.NotificationFollow, sessionID)
}

func markNotificationActivity(request protocol.Request, response []byte, paths Paths) {
	if request.Method != "send" && request.Method != "key" {
		return
	}
	var responseEnvelope struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(response, &responseEnvelope) != nil || !responseEnvelope.OK {
		return
	}
	var sessionID string
	if request.Method == "send" {
		var params protocol.SendParams
		if json.Unmarshal(request.Params, &params) != nil {
			return
		}
		sessionID = params.SessionID
	} else {
		var params protocol.KeyParams
		if json.Unmarshal(request.Params, &params) != nil ||
			(params.Key == "Up" || params.Key == "Down") {
			return
		}
		sessionID = params.SessionID
	}
	_ = notifications.MarkActive(paths.NotificationActivity, sessionID)
}

func configureNotifications(request protocol.Request, paths Paths, output io.Writer) error {
	var response protocol.Response
	switch {
	case request.Version != protocol.Version:
		response = protocol.Failure(request.RequestID, "unsupported_protocol", fmt.Sprintf("falknd supports protocol %d, not %d", protocol.Version, request.Version))
	case request.RequestID == "":
		response = protocol.Failure("", "invalid_request", "request_id is required")
	default:
		var params protocol.NotificationConfigureParams
		if len(request.Params) == 0 || json.Unmarshal(request.Params, &params) != nil {
			response = protocol.Failure(request.RequestID, "invalid_params", "params did not match the method")
		} else if err := notifications.Configure(paths.NotificationConfig, params); err != nil {
			response = protocol.Failure(request.RequestID, "notification_configuration_error", err.Error())
		} else if err := startNotificationWatcher(paths); err != nil {
			response = protocol.Failure(request.RequestID, "notification_watcher_error", err.Error())
		} else {
			response = protocol.Success(request.RequestID, protocol.AckResult{Accepted: true})
		}
	}
	return json.NewEncoder(output).Encode(response)
}

func enrichTranscriptResponse(request protocol.Request, response []byte) []byte {
	if request.Method != "transcript" {
		return response
	}
	var params protocol.TranscriptParams
	if json.Unmarshal(request.Params, &params) != nil || params.SessionID == "" {
		return response
	}
	percent := telemetry.ContextRemainingPercent(params.SessionID)
	if percent == nil {
		return response
	}

	var envelope map[string]any
	if json.Unmarshal(response, &envelope) != nil {
		return response
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		return response
	}
	result["context_remaining_percent"] = *percent
	enriched, err := json.Marshal(envelope)
	if err != nil {
		return response
	}
	return append(enriched, '\n')
}

func connect(socket string) (net.Conn, error) {
	return net.DialTimeout("unix", socket, 250*time.Millisecond)
}

func waitForDaemon(socket string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastError error
	for time.Now().Before(deadline) {
		connection, err := connect(socket)
		if err == nil {
			return connection, nil
		}
		lastError = err
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("falknd did not become ready: %w", lastError)
}

func startDaemon(paths Paths) error {
	if err := prepareRuntimeDirectory(paths); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate falknd executable: %w", err)
	}
	log, err := os.OpenFile(paths.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open falknd log: %w", err)
	}
	defer log.Close()

	command := exec.Command(executable, "serve")
	command.Stdin = nil
	command.Stdout = log
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start falknd: %w", err)
	}
	return command.Process.Release()
}

func startNotificationWatcher(paths Paths) error {
	if err := prepareRuntimeDirectory(paths); err != nil {
		return err
	}
	if notificationWatcherRunning(paths.NotificationLock) {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate falknd executable: %w", err)
	}
	log, err := os.OpenFile(paths.NotificationLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open notification watcher log: %w", err)
	}
	defer log.Close()
	command := exec.Command(executable, "notify-watch")
	command.Stdin = nil
	command.Stdout = log
	command.Stderr = log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start notification watcher: %w", err)
	}
	return command.Process.Release()
}

func ensureNotificationWatcher(paths Paths) error {
	if _, err := os.Stat(paths.NotificationConfig); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return startNotificationWatcher(paths)
}

func notificationWatcherRunning(lockPath string) bool {
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return false
}
