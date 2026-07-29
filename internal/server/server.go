package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/punklabs-ai/falkn/internal/agent"
	"github.com/punklabs-ai/falkn/internal/protocol"
	"github.com/punklabs-ai/falkn/internal/session"
	"github.com/punklabs-ai/falkn/internal/workspace"
)

const maxRequestBytes = 1024 * 1024

type Server struct {
	version  string
	agents   *agent.Registry
	sessions *session.Manager
	shutdown chan struct{}
	stopOnce sync.Once
}

func New(version string) *Server {
	registry := agent.DefaultRegistry()
	return &Server{
		version: version, agents: registry, sessions: session.NewManager(registry),
		shutdown: make(chan struct{}),
	}
}

func (s *Server) Serve() error {
	paths, err := RuntimePaths()
	if err != nil {
		return err
	}
	if err := prepareRuntimeDirectory(paths); err != nil {
		return err
	}
	lock, err := os.OpenFile(paths.Lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("falknd is already running")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := s.sessions.EnablePersistence(paths.SessionState); err != nil {
		return fmt.Errorf("load persisted sessions: %w", err)
	}

	if err := os.Remove(paths.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}
	listener, err := net.Listen("unix", paths.Socket)
	if err != nil {
		return fmt.Errorf("listen on daemon socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(paths.Socket)
	if err := os.Chmod(paths.Socket, 0o600); err != nil {
		return fmt.Errorf("secure daemon socket: %w", err)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
		case <-s.shutdown:
		}
		listener.Close()
	}()

	for {
		connection, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept daemon connection: %w", err)
		}
		go s.handle(connection)
	}
}

func (s *Server) handle(connection net.Conn) {
	defer connection.Close()
	decoder := json.NewDecoder(io.LimitReader(connection, maxRequestBytes+1))
	var request protocol.Request
	if err := decoder.Decode(&request); err != nil {
		_ = json.NewEncoder(connection).Encode(protocol.Failure("", "invalid_request", "The request was not valid JSON."))
		return
	}
	if request.Method == "attach" {
		s.handleAttach(connection, request)
		return
	}
	response := s.dispatch(request)
	responseError := json.NewEncoder(connection).Encode(response)
	if request.Method == "daemon.shutdown" && response.OK && responseError == nil {
		s.stopOnce.Do(func() { close(s.shutdown) })
	}
}

func (s *Server) dispatch(request protocol.Request) protocol.Response {
	if request.Version != protocol.Version {
		return protocol.Failure(request.RequestID, "unsupported_protocol", fmt.Sprintf("falknd supports protocol %d, not %d", protocol.Version, request.Version))
	}
	if request.RequestID == "" {
		return protocol.Failure("", "invalid_request", "request_id is required")
	}

	switch request.Method {
	case "preflight":
		capabilities := make([]protocol.Capability, 0, len(s.agents.All()))
		for _, spec := range s.agents.All() {
			resolution, available := agent.ResolveExecutable(spec)
			path := resolution.Executable
			if !available {
				path = ""
			}
			capabilities = append(capabilities, protocol.Capability{ID: spec.ID, DisplayName: spec.DisplayName, Path: path})
		}
		return protocol.Success(request.RequestID, protocol.PreflightResult{
			DaemonVersion: s.version, ProtocolVersion: protocol.Version, Capabilities: capabilities,
			Features: []string{"shell", "attach", "daemon_shutdown", "uploads", "attention", "agent_restart", "agent_start"},
		})
	case "daemon.shutdown":
		running := s.sessions.RunningSessions()
		if len(running) > 0 {
			return protocol.Failure(
				request.RequestID,
				"daemon_busy",
				fmt.Sprintf("cannot restart falknd while %d session(s) are running", len(running)),
			)
		}
		return protocol.Success(request.RequestID, protocol.AckResult{Accepted: true})
	case "list":
		return protocol.Success(request.RequestID, protocol.ListResult{Sessions: s.sessions.List()})
	case "directories.list":
		var params protocol.DirectoryListParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		listing, err := workspace.List(params.Path)
		if err != nil {
			return workspaceFailure(request.RequestID, err)
		}
		directories := make([]protocol.DirectoryEntry, len(listing.Directories))
		for index, directory := range listing.Directories {
			directories[index] = protocol.DirectoryEntry{Name: directory.Name, Path: directory.Path}
		}
		return protocol.Success(request.RequestID, protocol.DirectoryListResult{
			Path: listing.Path, ParentPath: listing.ParentPath, HomePath: listing.HomePath,
			Directories: directories, Truncated: listing.Truncated,
		})
	case "create":
		var params protocol.CreateParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		record, err := s.sessions.Create(params)
		if err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.CreateResult{Session: record})
	case "shell.create":
		var params protocol.ShellCreateParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		record, err := s.sessions.CreateShell(params)
		if err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.CreateResult{Session: record})
	case "agent.start":
		var params protocol.AgentStartParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		record, err := s.sessions.StartAgent(params)
		if err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.CreateResult{Session: record})
	case "transcript":
		var params protocol.TranscriptParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		page, record, err := s.sessions.TranscriptPage(
			params.SessionID,
			params.HistoryLines,
			params.BeforeLine,
		)
		if err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.TranscriptResult{
			Transcript: page.Transcript, Status: record.Status, AgentID: record.AgentID, Process: record.Process,
			PreferredAgentID: record.PreferredAgentID, AgentState: record.AgentState,
			CanStartAgent: record.CanStartAgent, CanResumeAgent: record.CanResumeAgent,
			PermissionMode: record.PermissionMode,
			StartLine:      page.StartLine, EndLine: page.EndLine, TotalLines: page.TotalLines,
			HasEarlier: page.HasEarlier, HistoryID: page.HistoryID,
		})
	case "resize":
		var params protocol.ResizeParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		if err := s.sessions.Resize(params.SessionID, params.Columns, params.Rows); err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.AckResult{Accepted: true})
	case "send":
		var params protocol.SendParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		if err := s.sessions.Send(params.SessionID, params.Text); err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.AckResult{Accepted: true})
	case "key":
		var params protocol.KeyParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		if err := s.sessions.SendKey(params.SessionID, params.Key); err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.AckResult{Accepted: true})
	case "stop":
		var params protocol.SessionParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		if err := s.sessions.Stop(params.SessionID); err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.AckResult{Accepted: true})
	case "delete", "end":
		var params protocol.SessionParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		if err := s.sessions.Delete(params.SessionID); err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.AckResult{Accepted: true})
	case "archive":
		var params protocol.SessionParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		if err := s.sessions.Archive(params.SessionID); err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.AckResult{Accepted: true})
	case "restore":
		var params protocol.SessionParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		if err := s.sessions.Restore(params.SessionID); err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.AckResult{Accepted: true})
	case "rename":
		var params protocol.RenameParams
		if response := decodeParams(request, &params); response != nil {
			return *response
		}
		if err := s.sessions.Rename(params.SessionID, params.Title); err != nil {
			return sessionFailure(request.RequestID, err)
		}
		return protocol.Success(request.RequestID, protocol.AckResult{Accepted: true})
	default:
		return protocol.Failure(request.RequestID, "unknown_method", fmt.Sprintf("unknown method %q", request.Method))
	}
}

func decodeParams(request protocol.Request, destination any) *protocol.Response {
	if len(request.Params) == 0 {
		response := protocol.Failure(request.RequestID, "invalid_params", "params are required")
		return &response
	}
	if err := json.Unmarshal(request.Params, destination); err != nil {
		response := protocol.Failure(request.RequestID, "invalid_params", "params did not match the method")
		return &response
	}
	return nil
}

func sessionFailure(requestID string, err error) protocol.Response {
	var pathError *workspace.PathError
	if errors.As(err, &pathError) {
		return protocol.Failure(requestID, string(pathError.Kind), pathError.Error())
	}
	code := "session_error"
	switch {
	case errors.Is(err, session.ErrSessionNotFound):
		code = "session_not_found"
	case errors.Is(err, session.ErrSessionNotRunning):
		code = "session_not_running"
	}
	message := err.Error()
	if len(message) > 1_000 {
		message = message[:1_000]
	}
	return protocol.Failure(requestID, code, message)
}

func workspaceFailure(requestID string, err error) protocol.Response {
	var pathError *workspace.PathError
	if errors.As(err, &pathError) {
		return protocol.Failure(requestID, string(pathError.Kind), pathError.Error())
	}
	return protocol.Failure(requestID, string(workspace.ErrorUnavailable), "The directory is unavailable.")
}
