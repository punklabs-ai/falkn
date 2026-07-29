package protocol

import "encoding/json"

const Version = 1

type Request struct {
	Version   int             `json:"version"`
	RequestID string          `json:"request_id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	Version   int       `json:"version"`
	RequestID string    `json:"request_id"`
	OK        bool      `json:"ok"`
	Result    any       `json:"result,omitempty"`
	Error     *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func Success(requestID string, result any) Response {
	return Response{Version: Version, RequestID: requestID, OK: true, Result: result}
}

func Failure(requestID, code, message string) Response {
	return Response{
		Version:   Version,
		RequestID: requestID,
		OK:        false,
		Error:     &RPCError{Code: code, Message: message},
	}
}

type EmptyParams struct{}

type DirectoryListParams struct {
	Path string `json:"path"`
}

type CreateParams struct {
	Title               string   `json:"title"`
	Directory           string   `json:"directory"`
	AgentID             string   `json:"agent_id"`
	PermissionMode      string   `json:"permission_mode,omitempty"`
	AdditionalArguments []string `json:"additional_arguments,omitempty"`
	TerminalColumns     int      `json:"terminal_columns,omitempty"`
	TerminalRows        int      `json:"terminal_rows,omitempty"`
}

type ShellCreateParams struct {
	Title           string `json:"title"`
	Directory       string `json:"directory"`
	TerminalColumns int    `json:"terminal_columns,omitempty"`
	TerminalRows    int    `json:"terminal_rows,omitempty"`
}

type AgentStartParams struct {
	SessionID      string `json:"session_id"`
	AgentID        string `json:"agent_id,omitempty"`
	Resume         bool   `json:"resume,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
}

type AttachParams struct {
	SessionID string `json:"session_id"`
	Columns   int    `json:"columns,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

type SessionParams struct {
	SessionID string `json:"session_id"`
}

type RenameParams struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
}

type TranscriptParams struct {
	SessionID    string `json:"session_id"`
	HistoryLines int    `json:"history_lines,omitempty"`
	BeforeLine   *int   `json:"before_line,omitempty"`
}

type ResizeParams struct {
	SessionID string `json:"session_id"`
	Columns   int    `json:"columns"`
	Rows      int    `json:"rows"`
}

type SendParams struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

type KeyParams struct {
	SessionID string `json:"session_id"`
	Key       string `json:"key"`
}

type UploadPrepareParams struct {
	SessionID string `json:"session_id"`
	Filename  string `json:"filename"`
}

type UploadPrepareResult struct {
	Directory string `json:"directory"`
	Path      string `json:"path"`
}

type UploadCacheDeleteParams struct {
	SessionID string `json:"session_id"`
	Filename  string `json:"filename"`
}

type UploadCacheItem struct {
	SessionID string `json:"session_id"`
	Filename  string `json:"filename"`
	Path      string `json:"path"`
	ByteCount int64  `json:"byte_count"`
	Modified  int64  `json:"modified_at"`
}

type UploadCacheListResult struct {
	Items     []UploadCacheItem `json:"items"`
	ByteCount int64             `json:"byte_count"`
}

type NotificationConfigureParams struct {
	RelayURL      string   `json:"relay_url"`
	DeviceID      string   `json:"device_id"`
	DeviceSecret  string   `json:"device_secret"`
	EncryptionKey string   `json:"encryption_key"`
	HostID        string   `json:"host_id"`
	HostName      string   `json:"host_name,omitempty"`
	EnabledStates []string `json:"enabled_states"`
}

type Capability struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Path        string `json:"path,omitempty"`
}

type PreflightResult struct {
	DaemonVersion   string       `json:"daemon_version"`
	ProtocolVersion int          `json:"protocol_version"`
	Capabilities    []Capability `json:"capabilities"`
	Features        []string     `json:"features,omitempty"`
}

type SessionRecord struct {
	ID                 string `json:"id"`
	Title              string `json:"title"`
	Directory          string `json:"directory"`
	AgentID            string `json:"agent_id"`
	PreferredAgentID   string `json:"preferred_agent_id,omitempty"`
	AgentState         string `json:"agent_state,omitempty"`
	CanStartAgent      bool   `json:"can_start_agent"`
	CanResumeAgent     bool   `json:"can_resume_agent"`
	CreatedAt          int64  `json:"created_at"`
	Status             string `json:"status"`
	Process            string `json:"process"`
	AttachedClients    int    `json:"attached_clients"`
	FocusedClients     int    `json:"focused_clients,omitempty"`
	NotificationsMuted bool   `json:"notifications_muted,omitempty"`
	Attention          string `json:"attention,omitempty"`
	Archived           bool   `json:"archived"`
	PermissionMode     string `json:"permission_mode,omitempty"`
	Kind               string `json:"kind,omitempty"`
}

type ListResult struct {
	Sessions []SessionRecord `json:"sessions"`
}

type CreateResult struct {
	Session SessionRecord `json:"session"`
}

type DirectoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type DirectoryListResult struct {
	Path        string           `json:"path"`
	ParentPath  string           `json:"parent_path,omitempty"`
	HomePath    string           `json:"home_path"`
	Directories []DirectoryEntry `json:"directories"`
	Truncated   bool             `json:"truncated,omitempty"`
}

type TranscriptResult struct {
	Transcript              string `json:"transcript"`
	Status                  string `json:"status"`
	AgentID                 string `json:"agent_id,omitempty"`
	PreferredAgentID        string `json:"preferred_agent_id,omitempty"`
	AgentState              string `json:"agent_state,omitempty"`
	CanStartAgent           bool   `json:"can_start_agent"`
	CanResumeAgent          bool   `json:"can_resume_agent"`
	PermissionMode          string `json:"permission_mode,omitempty"`
	Process                 string `json:"process,omitempty"`
	ContextRemainingPercent *int   `json:"context_remaining_percent,omitempty"`
	StartLine               int    `json:"start_line"`
	EndLine                 int    `json:"end_line"`
	TotalLines              int    `json:"total_lines"`
	HasEarlier              bool   `json:"has_earlier"`
	HistoryID               string `json:"history_id"`
}

type AttachResult struct {
	Session  SessionRecord `json:"session"`
	Features []string      `json:"features,omitempty"`
}

type AckResult struct {
	Accepted bool `json:"accepted"`
}
