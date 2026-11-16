package bench

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryCapturesFilesModesSymlinksAndExcludesGeneratedDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "app.txt"), []byte("content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "ignored"), []byte("git"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "ignored"), []byte("deps"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("app.txt", filepath.Join(root, "src", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	files, err := Inventory(root)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("content"))
	if got := files["src/app.txt"]; got["kind"] != "file" || got["sha256"] != fmt.Sprintf("%x", hash) || got["executable"] != true {
		t.Fatalf("file inventory = %#v", got)
	}
	if got := files["src/link"]; got["kind"] != "symlink" || got["target"] != "app.txt" {
		t.Fatalf("symlink inventory = %#v", got)
	}
	if _, ok := files[".git/ignored"]; ok {
		t.Fatal(".git contents were inventoried")
	}
	if _, ok := files["node_modules/ignored"]; ok {
		t.Fatal("node_modules contents were inventoried")
	}
}

func TestWriteDiffSerializesAddedAndChangedFiles(t *testing.T) {
	workspace, baseline := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "changed.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "added.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseline, "changed.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := map[string]Record{
		"changed.txt": {"kind": "file", "sha256": "before", "executable": false},
	}
	destination := filepath.Join(t.TempDir(), "changes.patch")
	after, err := WriteDiff(before, workspace, destination, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after["added.txt"]; !ok {
		t.Fatalf("returned inventory lacks newly-added file: %#v", after)
	}
	patch, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`diff --git "a/added.txt" "b/added.txt"`,
		"new file mode 100644",
		"+hello",
		`diff --git "a/changed.txt" "b/changed.txt"`,
		"-old",
		"+new",
	} {
		if !strings.Contains(string(patch), expected) {
			t.Fatalf("patch does not contain %q:\n%s", expected, patch)
		}
	}
}
