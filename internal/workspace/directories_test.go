package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestListReturnsDirectoriesOnlyInCaseInsensitiveOrder(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"zeta", "Alpha", ".hidden"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("ignore"), 0o600); err != nil {
		t.Fatal(err)
	}

	listing, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Directories) != 3 {
		t.Fatalf("got %d directories, want 3", len(listing.Directories))
	}
	want := []string{"Alpha", "zeta", ".hidden"}
	for index, name := range want {
		if listing.Directories[index].Name != name {
			t.Fatalf("directory %d = %q, want %q", index, listing.Directories[index].Name, name)
		}
	}
}

func TestResolveReportsMissingAndNonDirectoryPaths(t *testing.T) {
	root := t.TempDir()
	_, err := Resolve(filepath.Join(root, "missing"))
	var pathError *PathError
	if !errors.As(err, &pathError) || pathError.Kind != ErrorNotFound {
		t.Fatalf("missing path error = %v", err)
	}

	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(file)
	if !errors.As(err, &pathError) || pathError.Kind != ErrorNotDirectory {
		t.Fatalf("file path error = %v", err)
	}
}

func TestListIncludesSymlinkedDirectories(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	listing, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Directories) != 1 || listing.Directories[0].Name != "linked" {
		t.Fatalf("directories = %#v", listing.Directories)
	}
}
