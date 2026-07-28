package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/punklabs-ai/falkn/internal/protocol"
)

func TestCreateUploadPathUsesPrivateRuntimeStorage(t *testing.T) {
	runtimeDirectory := t.TempDir()
	paths := Paths{
		Directory: runtimeDirectory,
		Uploads:   filepath.Join(runtimeDirectory, "uploads"),
	}
	result, err := createUploadPath(paths, protocol.UploadPrepareParams{
		SessionID: "falkn-0123456789abcdef",
		Filename:  "attachment-1234.jpg",
	})
	if err != nil {
		t.Fatal(err)
	}

	expectedDirectory := filepath.Join(
		runtimeDirectory,
		"uploads",
		"falkn-0123456789abcdef",
	)
	if result.Directory != expectedDirectory {
		t.Fatalf("directory = %q, want %q", result.Directory, expectedDirectory)
	}
	if result.Path != filepath.Join(expectedDirectory, "attachment-1234.jpg") {
		t.Fatalf("path = %q", result.Path)
	}
	for _, directory := range []string{paths.Uploads, expectedDirectory} {
		info, statErr := os.Stat(directory)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if permissions := info.Mode().Perm(); permissions != 0o700 {
			t.Fatalf("%s permissions = %o, want 700", directory, permissions)
		}
	}
}

func TestCreateUploadPathRejectsTraversal(t *testing.T) {
	runtimeDirectory := t.TempDir()
	paths := Paths{
		Directory: runtimeDirectory,
		Uploads:   filepath.Join(runtimeDirectory, "uploads"),
	}
	for _, params := range []protocol.UploadPrepareParams{
		{SessionID: "../another-session", Filename: "photo.jpg"},
		{SessionID: "falkn-safe", Filename: "../../photo.jpg"},
		{SessionID: "falkn-safe", Filename: "nested/photo.jpg"},
	} {
		if _, err := createUploadPath(paths, params); err == nil {
			t.Fatalf("createUploadPath(%+v) succeeded", params)
		}
	}
}

func TestSuccessfulSessionDeleteRemovesItsUploads(t *testing.T) {
	runtimeDirectory := t.TempDir()
	sessionID := "falkn-0123456789abcdef"
	paths := Paths{Uploads: filepath.Join(runtimeDirectory, "uploads")}
	sessionUploads := filepath.Join(paths.Uploads, sessionID)
	if err := os.MkdirAll(sessionUploads, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionUploads, "photo.jpg"), []byte("photo"), 0o600); err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(protocol.SessionParams{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(protocol.Success("delete", protocol.AckResult{Accepted: true}))
	if err != nil {
		t.Fatal(err)
	}

	cleanupDeletedSessionUploads(protocol.Request{
		Version: protocol.Version, RequestID: "delete", Method: "delete", Params: params,
	}, response, paths)

	if _, err := os.Stat(sessionUploads); !os.IsNotExist(err) {
		t.Fatalf("session uploads still exist: %v", err)
	}
}

func TestMediaCacheListsImagesNewestFirstAndSkipsOtherFiles(t *testing.T) {
	runtimeDirectory := t.TempDir()
	paths := Paths{Uploads: filepath.Join(runtimeDirectory, "uploads")}
	olderDirectory := filepath.Join(paths.Uploads, "falkn-older")
	newerDirectory := filepath.Join(paths.Uploads, "falkn-newer")
	for _, directory := range []string{olderDirectory, newerDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	olderPath := filepath.Join(olderDirectory, "older.jpg")
	newerPath := filepath.Join(newerDirectory, "newer.png")
	if err := os.WriteFile(olderPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newerPath, []byte("newer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newerDirectory, "notes.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	olderTime := time.Unix(100, 0)
	newerTime := time.Unix(200, 0)
	if err := os.Chtimes(olderPath, olderTime, olderTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newerPath, newerTime, newerTime); err != nil {
		t.Fatal(err)
	}

	listing, err := mediaCacheListing(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Items) != 2 {
		t.Fatalf("items = %#v, want two images", listing.Items)
	}
	if listing.Items[0].Filename != "newer.png" ||
		listing.Items[1].Filename != "older.jpg" {
		t.Fatalf("items are not newest first: %#v", listing.Items)
	}
	if listing.ByteCount != 8 {
		t.Fatalf("byte_count = %d, want 8", listing.ByteCount)
	}
}

func TestMediaCacheDeleteRemovesOnlySelectedImage(t *testing.T) {
	runtimeDirectory := t.TempDir()
	paths := Paths{Uploads: filepath.Join(runtimeDirectory, "uploads")}
	sessionDirectory := filepath.Join(paths.Uploads, "falkn-session")
	if err := os.MkdirAll(sessionDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"one.jpg", "two.jpg"} {
		if err := os.WriteFile(filepath.Join(sessionDirectory, filename), []byte(filename), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := removeCachedUpload(paths, protocol.UploadCacheDeleteParams{
		SessionID: "falkn-session",
		Filename:  "one.jpg",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sessionDirectory, "one.jpg")); !os.IsNotExist(err) {
		t.Fatalf("selected image still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionDirectory, "two.jpg")); err != nil {
		t.Fatalf("other image was removed: %v", err)
	}
}

func TestClearMediaCachePreservesNonmediaAttachments(t *testing.T) {
	runtimeDirectory := t.TempDir()
	paths := Paths{Uploads: filepath.Join(runtimeDirectory, "uploads")}
	sessionDirectory := filepath.Join(paths.Uploads, "falkn-session")
	if err := os.MkdirAll(sessionDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(sessionDirectory, "photo.jpg")
	documentPath := filepath.Join(sessionDirectory, "report.pdf")
	if err := os.WriteFile(imagePath, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(documentPath, []byte("document"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := clearMediaCache(paths); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(imagePath); !os.IsNotExist(err) {
		t.Fatalf("image still exists: %v", err)
	}
	if _, err := os.Stat(documentPath); err != nil {
		t.Fatalf("nonmedia attachment was removed: %v", err)
	}
}

func TestMediaCacheDeleteRejectsTraversalAndSymlinks(t *testing.T) {
	runtimeDirectory := t.TempDir()
	paths := Paths{Uploads: filepath.Join(runtimeDirectory, "uploads")}
	sessionDirectory := filepath.Join(paths.Uploads, "falkn-session")
	if err := os.MkdirAll(sessionDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(runtimeDirectory, "outside.jpg")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(sessionDirectory, "link.jpg")); err != nil {
		t.Fatal(err)
	}

	for _, params := range []protocol.UploadCacheDeleteParams{
		{SessionID: "../outside", Filename: "photo.jpg"},
		{SessionID: "falkn-session", Filename: "../outside.jpg"},
		{SessionID: "falkn-session", Filename: "link.jpg"},
	} {
		if err := removeCachedUpload(paths, params); err == nil {
			t.Fatalf("removeCachedUpload(%+v) succeeded", params)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("outside target was changed: %v", err)
	}
}
