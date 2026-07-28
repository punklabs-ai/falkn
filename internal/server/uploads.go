package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/punklabs-ai/falkn/internal/protocol"
)

func prepareUpload(request protocol.Request, paths Paths, output io.Writer) error {
	var response protocol.Response
	switch {
	case request.Version != protocol.Version:
		response = protocol.Failure(
			request.RequestID,
			"unsupported_protocol",
			fmt.Sprintf("falknd supports protocol %d, not %d", protocol.Version, request.Version),
		)
	case request.RequestID == "":
		response = protocol.Failure("", "invalid_request", "request_id is required")
	default:
		var params protocol.UploadPrepareParams
		if len(request.Params) == 0 || json.Unmarshal(request.Params, &params) != nil {
			response = protocol.Failure(
				request.RequestID,
				"invalid_params",
				"params did not match the method",
			)
		} else if result, err := createUploadPath(paths, params); err != nil {
			code := "upload_storage_error"
			if errors.Is(err, errInvalidUploadPath) {
				code = "invalid_params"
			}
			response = protocol.Failure(request.RequestID, code, err.Error())
		} else {
			response = protocol.Success(request.RequestID, result)
		}
	}
	return json.NewEncoder(output).Encode(response)
}

func listUploads(request protocol.Request, paths Paths, output io.Writer) error {
	if response := validateUploadRequest(request); response != nil {
		return json.NewEncoder(output).Encode(response)
	}
	result, err := mediaCacheListing(paths)
	if err != nil {
		return json.NewEncoder(output).Encode(protocol.Failure(
			request.RequestID,
			"upload_storage_error",
			err.Error(),
		))
	}
	return json.NewEncoder(output).Encode(protocol.Success(request.RequestID, result))
}

func deleteUpload(request protocol.Request, paths Paths, output io.Writer) error {
	if response := validateUploadRequest(request); response != nil {
		return json.NewEncoder(output).Encode(response)
	}
	var params protocol.UploadCacheDeleteParams
	if len(request.Params) == 0 || json.Unmarshal(request.Params, &params) != nil {
		return json.NewEncoder(output).Encode(protocol.Failure(
			request.RequestID,
			"invalid_params",
			"params did not match the method",
		))
	}
	if err := removeCachedUpload(paths, params); err != nil {
		code := "upload_storage_error"
		if errors.Is(err, errInvalidUploadPath) {
			code = "invalid_params"
		} else if errors.Is(err, os.ErrNotExist) {
			code = "upload_not_found"
		}
		return json.NewEncoder(output).Encode(protocol.Failure(
			request.RequestID,
			code,
			err.Error(),
		))
	}
	return json.NewEncoder(output).Encode(protocol.Success(
		request.RequestID,
		protocol.AckResult{Accepted: true},
	))
}

func clearUploads(request protocol.Request, paths Paths, output io.Writer) error {
	if response := validateUploadRequest(request); response != nil {
		return json.NewEncoder(output).Encode(response)
	}
	if err := clearMediaCache(paths); err != nil {
		return json.NewEncoder(output).Encode(protocol.Failure(
			request.RequestID,
			"upload_storage_error",
			err.Error(),
		))
	}
	return json.NewEncoder(output).Encode(protocol.Success(
		request.RequestID,
		protocol.AckResult{Accepted: true},
	))
}

func validateUploadRequest(request protocol.Request) *protocol.Response {
	switch {
	case request.Version != protocol.Version:
		response := protocol.Failure(
			request.RequestID,
			"unsupported_protocol",
			fmt.Sprintf("falknd supports protocol %d, not %d", protocol.Version, request.Version),
		)
		return &response
	case request.RequestID == "":
		response := protocol.Failure("", "invalid_request", "request_id is required")
		return &response
	default:
		return nil
	}
}

var errInvalidUploadPath = errors.New("invalid upload path")

func createUploadPath(
	paths Paths,
	params protocol.UploadPrepareParams,
) (protocol.UploadPrepareResult, error) {
	if !isSafeUploadComponent(params.SessionID, 128) {
		return protocol.UploadPrepareResult{}, fmt.Errorf(
			"%w: session_id must be a simple filename component",
			errInvalidUploadPath,
		)
	}
	if !isSafeUploadComponent(params.Filename, 255) {
		return protocol.UploadPrepareResult{}, fmt.Errorf(
			"%w: filename must be a simple filename component",
			errInvalidUploadPath,
		)
	}
	if err := prepareRuntimeDirectory(paths); err != nil {
		return protocol.UploadPrepareResult{}, err
	}
	if err := createPrivateDirectory(paths.Uploads); err != nil {
		return protocol.UploadPrepareResult{}, fmt.Errorf("prepare uploads directory: %w", err)
	}
	directory := filepath.Join(paths.Uploads, params.SessionID)
	if err := createPrivateDirectory(directory); err != nil {
		return protocol.UploadPrepareResult{}, fmt.Errorf("prepare session uploads directory: %w", err)
	}
	return protocol.UploadPrepareResult{
		Directory: directory,
		Path:      filepath.Join(directory, params.Filename),
	}, nil
}

func createPrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func isSafeUploadComponent(value string, maximumLength int) bool {
	if value == "" || len(value) > maximumLength || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		isLetter := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z'
		isNumber := character >= '0' && character <= '9'
		if !isLetter && !isNumber && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func mediaCacheListing(paths Paths) (protocol.UploadCacheListResult, error) {
	result := protocol.UploadCacheListResult{Items: []protocol.UploadCacheItem{}}
	sessionEntries, err := os.ReadDir(paths.Uploads)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("read uploads directory: %w", err)
	}

	for _, sessionEntry := range sessionEntries {
		if !sessionEntry.IsDir() ||
			!isSafeUploadComponent(sessionEntry.Name(), 128) {
			continue
		}
		sessionDirectory := filepath.Join(paths.Uploads, sessionEntry.Name())
		fileEntries, readErr := os.ReadDir(sessionDirectory)
		if readErr != nil {
			return result, fmt.Errorf("read session uploads: %w", readErr)
		}
		for _, fileEntry := range fileEntries {
			if fileEntry.IsDir() ||
				fileEntry.Type()&os.ModeSymlink != 0 ||
				!isSafeUploadComponent(fileEntry.Name(), 255) ||
				!isMediaFilename(fileEntry.Name()) {
				continue
			}
			info, infoErr := fileEntry.Info()
			if infoErr != nil {
				return result, fmt.Errorf("inspect cached upload: %w", infoErr)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			item := protocol.UploadCacheItem{
				SessionID: sessionEntry.Name(),
				Filename:  fileEntry.Name(),
				Path:      filepath.Join(sessionDirectory, fileEntry.Name()),
				ByteCount: info.Size(),
				Modified:  info.ModTime().Unix(),
			}
			result.Items = append(result.Items, item)
			result.ByteCount += item.ByteCount
		}
	}
	sort.Slice(result.Items, func(left, right int) bool {
		if result.Items[left].Modified == result.Items[right].Modified {
			return result.Items[left].Filename < result.Items[right].Filename
		}
		return result.Items[left].Modified > result.Items[right].Modified
	})
	return result, nil
}

func removeCachedUpload(paths Paths, params protocol.UploadCacheDeleteParams) error {
	if !isSafeUploadComponent(params.SessionID, 128) ||
		!isSafeUploadComponent(params.Filename, 255) ||
		!isMediaFilename(params.Filename) {
		return fmt.Errorf("%w: cached upload identifiers are invalid", errInvalidUploadPath)
	}
	sessionDirectory := filepath.Join(paths.Uploads, params.SessionID)
	path := filepath.Join(sessionDirectory, params.Filename)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: cached upload is not a regular file", errInvalidUploadPath)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove cached upload: %w", err)
	}
	entries, err := os.ReadDir(sessionDirectory)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(sessionDirectory)
	}
	return nil
}

func clearMediaCache(paths Paths) error {
	listing, err := mediaCacheListing(paths)
	if err != nil {
		return err
	}
	for _, item := range listing.Items {
		err := removeCachedUpload(paths, protocol.UploadCacheDeleteParams{
			SessionID: item.SessionID,
			Filename:  item.Filename,
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func isMediaFilename(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".heic", ".heif", ".webp":
		return true
	default:
		return false
	}
}

func cleanupDeletedSessionUploads(request protocol.Request, response []byte, paths Paths) {
	if request.Method != "delete" && request.Method != "end" {
		return
	}
	var responseEnvelope struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(response, &responseEnvelope) != nil || !responseEnvelope.OK {
		return
	}
	var params protocol.SessionParams
	if json.Unmarshal(request.Params, &params) != nil ||
		!isSafeUploadComponent(params.SessionID, 128) {
		return
	}
	_ = os.RemoveAll(filepath.Join(paths.Uploads, params.SessionID))
}
