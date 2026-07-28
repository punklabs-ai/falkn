package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxDirectoryEntries = 1_000

type ErrorKind string

const (
	ErrorRequired     ErrorKind = "path_required"
	ErrorNotFound     ErrorKind = "path_not_found"
	ErrorNotDirectory ErrorKind = "path_not_directory"
	ErrorPermission   ErrorKind = "path_permission_denied"
	ErrorUnavailable  ErrorKind = "path_unavailable"
)

type PathError struct {
	Kind ErrorKind
	Path string
	Err  error
}

func (e *PathError) Error() string {
	switch e.Kind {
	case ErrorRequired:
		return "A working directory is required."
	case ErrorNotFound:
		return fmt.Sprintf("The directory %q does not exist.", e.Path)
	case ErrorNotDirectory:
		return fmt.Sprintf("%q is not a directory.", e.Path)
	case ErrorPermission:
		return fmt.Sprintf("You do not have permission to open %q.", e.Path)
	default:
		return fmt.Sprintf("The directory %q is unavailable.", e.Path)
	}
}

func (e *PathError) Unwrap() error { return e.Err }

type Directory struct {
	Name string
	Path string
}

type Listing struct {
	Path        string
	ParentPath  string
	HomePath    string
	Directories []Directory
	Truncated   bool
}

func Resolve(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", &PathError{Kind: ErrorRequired}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", &PathError{Kind: ErrorUnavailable, Path: value, Err: err}
	}
	switch {
	case value == "~":
		value = home
	case strings.HasPrefix(value, "~/"):
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	case !filepath.IsAbs(value):
		value = filepath.Join(home, value)
	}
	value = filepath.Clean(value)
	info, err := os.Stat(value)
	if err != nil {
		return "", classifyError(value, err)
	}
	if !info.IsDir() {
		return "", &PathError{Kind: ErrorNotDirectory, Path: value}
	}
	return value, nil
}

func List(value string) (Listing, error) {
	path, err := Resolve(value)
	if err != nil {
		return Listing{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Listing{}, &PathError{Kind: ErrorUnavailable, Path: path, Err: err}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return Listing{}, classifyError(path, err)
	}

	directories := make([]Directory, 0, len(entries))
	for _, entry := range entries {
		childPath := filepath.Join(path, entry.Name())
		isDirectory := entry.IsDir()
		if entry.Type()&os.ModeSymlink != 0 {
			info, statError := os.Stat(childPath)
			isDirectory = statError == nil && info.IsDir()
		}
		if !isDirectory {
			continue
		}
		directories = append(directories, Directory{Name: entry.Name(), Path: childPath})
	}
	sort.Slice(directories, func(left, right int) bool {
		leftHidden := strings.HasPrefix(directories[left].Name, ".")
		rightHidden := strings.HasPrefix(directories[right].Name, ".")
		if leftHidden != rightHidden {
			return !leftHidden
		}
		return strings.ToLower(directories[left].Name) < strings.ToLower(directories[right].Name)
	})

	truncated := len(directories) > maxDirectoryEntries
	if truncated {
		directories = directories[:maxDirectoryEntries]
	}
	parent := filepath.Dir(path)
	if parent == path {
		parent = ""
	}
	return Listing{
		Path:        path,
		ParentPath:  parent,
		HomePath:    filepath.Clean(home),
		Directories: directories,
		Truncated:   truncated,
	}, nil
}

func classifyError(path string, err error) error {
	kind := ErrorUnavailable
	switch {
	case errors.Is(err, fs.ErrNotExist):
		kind = ErrorNotFound
	case errors.Is(err, fs.ErrPermission):
		kind = ErrorPermission
	}
	return &PathError{Kind: kind, Path: path, Err: err}
}
