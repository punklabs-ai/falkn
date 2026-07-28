package server

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/punklabs-ai/falkn/internal/protocol"
)

const (
	attachInputFrame  byte = 'i'
	attachResizeFrame byte = 'r'
	attachDetachFrame byte = 'd'
	attachFocusFrame  byte = 'f'
	attachNotifyFrame byte = 'n'
	maxAttachFrame         = 256 * 1024
)

var attachFeatures = []string{"notification_mute"}

func (s *Server) handleAttach(connection net.Conn, request protocol.Request) {
	if request.Version != protocol.Version {
		_ = json.NewEncoder(connection).Encode(protocol.Failure(
			request.RequestID, "unsupported_protocol",
			fmt.Sprintf("falknd supports protocol %d, not %d", protocol.Version, request.Version),
		))
		return
	}
	if request.RequestID == "" {
		_ = json.NewEncoder(connection).Encode(protocol.Failure("", "invalid_request", "request_id is required"))
		return
	}
	var params protocol.AttachParams
	if response := decodeParams(request, &params); response != nil {
		_ = json.NewEncoder(connection).Encode(*response)
		return
	}
	history, updates, detach, record, err := s.sessions.Attach(params.SessionID, params.Columns, params.Rows)
	if err != nil {
		_ = json.NewEncoder(connection).Encode(sessionFailure(request.RequestID, err))
		return
	}
	defer detach()
	// Nested `falkn` commands ask the currently attached client to hand its
	// terminal to another session with a private OSC command. The raw transcript
	// retains that command, but it must not become active again when this history
	// is replayed: doing so would bounce a user straight back out of a session
	// they deliberately returned to.
	history = stripAttachHandoffs(history)

	if err := json.NewEncoder(connection).Encode(protocol.Success(
		request.RequestID, protocol.AttachResult{Session: record, Features: attachFeatures},
	)); err != nil {
		return
	}
	if len(history) > 0 {
		if _, err := connection.Write(history); err != nil {
			return
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.readAttachFrames(connection, params.SessionID)
	}()

	for {
		select {
		case <-done:
			return
		case chunk, ok := <-updates:
			if !ok {
				return
			}
			if _, err := connection.Write(chunk); err != nil {
				return
			}
		}
	}
}

func (s *Server) readAttachFrames(connection net.Conn, sessionID string) error {
	header := make([]byte, 5)
	for {
		if _, err := io.ReadFull(connection, header); err != nil {
			return err
		}
		length := int(binary.BigEndian.Uint32(header[1:]))
		if length < 0 || length > maxAttachFrame {
			return errors.New("attach frame exceeded the safety limit")
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(connection, payload); err != nil {
			return err
		}
		switch header[0] {
		case attachInputFrame:
			if err := s.sessions.WriteRaw(sessionID, payload); err != nil {
				return err
			}
		case attachResizeFrame:
			if len(payload) != 4 {
				return errors.New("invalid resize frame")
			}
			columns := int(binary.BigEndian.Uint16(payload[0:2]))
			rows := int(binary.BigEndian.Uint16(payload[2:4]))
			if err := s.sessions.Resize(sessionID, columns, rows); err != nil {
				return err
			}
		case attachDetachFrame:
			if len(payload) != 0 {
				return errors.New("invalid detach frame")
			}
			return nil
		case attachFocusFrame:
			if len(payload) != 1 || (payload[0] != 0 && payload[0] != 1) {
				return errors.New("invalid focus frame")
			}
			// Accepted for compatibility. Notification ownership now follows
			// attachment, since terminal focus events are not reliable everywhere.
		case attachNotifyFrame:
			if len(payload) != 1 || (payload[0] != 0 && payload[0] != 1) {
				return errors.New("invalid notification frame")
			}
			if err := s.sessions.SetNotificationsMuted(sessionID, payload[0] == 1); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown attach frame %q", header[0])
		}
	}
}
