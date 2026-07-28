package server

import (
	"fmt"
	"os"
	"path/filepath"
)

type Paths struct {
	Directory            string
	Socket               string
	Lock                 string
	Log                  string
	SessionState         string
	NotificationConfig   string
	NotificationState    string
	NotificationLock     string
	NotificationLog      string
	NotificationActivity string
	NotificationPresence string
	NotificationFollow   string
	Uploads              string
}

func RuntimePaths() (Paths, error) {
	directory := os.Getenv("FALKND_RUNTIME_DIR")
	if directory == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return Paths{}, fmt.Errorf("find user cache directory: %w", err)
		}
		directory = filepath.Join(cache, "falkn")
	}
	return Paths{
		Directory:            directory,
		Socket:               filepath.Join(directory, "falknd.sock"),
		Lock:                 filepath.Join(directory, "falknd.lock"),
		Log:                  filepath.Join(directory, "falknd.log"),
		SessionState:         filepath.Join(directory, "sessions.json"),
		NotificationConfig:   filepath.Join(directory, "notifications.json"),
		NotificationState:    filepath.Join(directory, "notification-state.json"),
		NotificationLock:     filepath.Join(directory, "notification-watcher.lock"),
		NotificationLog:      filepath.Join(directory, "notification-watcher.log"),
		NotificationActivity: filepath.Join(directory, "notification-activity"),
		NotificationPresence: filepath.Join(directory, "notification-presence"),
		NotificationFollow:   filepath.Join(directory, "notification-follow"),
		Uploads:              filepath.Join(directory, "uploads"),
	}, nil
}

func prepareRuntimeDirectory(paths Paths) error {
	if err := os.MkdirAll(paths.Directory, 0o700); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}
	if err := os.Chmod(paths.Directory, 0o700); err != nil {
		return fmt.Errorf("secure runtime directory: %w", err)
	}
	return nil
}
