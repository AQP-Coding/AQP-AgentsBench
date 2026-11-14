package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return output
}

func TestPrepareWorkspacePinsCommittedSnapshotWithoutMutatingSource(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "answer.txt"), []byte("committed"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = source
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	if output, err := exec.Command("git", "-C", source, "add", "answer.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, output)
	}
	cmd = exec.Command("git", "-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "initial")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, output)
	}
	expected := strings.TrimSpace(string(mustOutput(t, source, "-C", source, "rev-parse", "HEAD")))
	inSource := filepath.Join(source, "snapshot")
	if _, err := PrepareWorkspace(context.Background(), Record{"repository": Record{"source": source, "revision": "HEAD"}}, inSource); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("destination inside source error = %v", err)
	}
	if _, err := os.Stat(inSource); !os.IsNotExist(err) {
		t.Fatalf("workspace destination modified source repository: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "answer.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "untracked.txt"), []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "snapshot")
	result, err := PrepareWorkspace(context.Background(), Record{"repository": Record{"source": source, "revision": "HEAD"}}, destination)
	if err != nil {
		t.Fatal(err)
	}
	if result["commit"] != expected {
		t.Fatalf("commit = %v, want %s", result["commit"], expected)
	}
	warnings := array(result["warnings"])
	if len(warnings) == 0 || !strings.Contains(text(warnings[0]), "Uncommitted files") {
		t.Fatalf("warnings = %#v", result["warnings"])
	}
	if got, err := os.ReadFile(filepath.Join(destination, "answer.txt")); err != nil || string(got) != "committed" {
		t.Fatalf("snapshot answer = %q, err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "untracked.txt")); !os.IsNotExist(err) {
		t.Fatalf("untracked source file appeared in snapshot: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(source, "answer.txt")); err != nil || string(got) != "dirty" {
		t.Fatalf("source changed: %q, err %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(source, "untracked.txt")); err != nil || string(got) != "private" {
		t.Fatalf("source untracked file changed: %q, err %v", got, err)
	}
}

func TestPrepareWorkspaceCopiesOriginalAttachmentBytesAndHash(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "input.csv")
	content := []byte("a,b\r\n1,2\r\n")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "workspace")
	result, err := PrepareWorkspace(context.Background(), Record{"attachments": []Record{{"source": source, "target": "data/input.csv"}}}, destination)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "data", "input.csv"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("copied bytes = %q, err %v", got, err)
	}
	digest := sha256.Sum256(content)
	wantHash := hex.EncodeToString(digest[:])
	attachments := records(result["attachments"])
	if len(attachments) != 1 || text(attachments[0]["target"]) != "data/input.csv" || text(attachments[0]["sha256"]) != wantHash {
		t.Fatalf("attachments = %#v, want target and sha256 %s", attachments, wantHash)
	}
}

func TestCopyFilesRejectsUnsafeTargetsAndSymlinkSourcesBeforeWriting(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "workspace")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source.txt")
	if err := os.WriteFile(source, []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"../escape", "/absolute", ".git/config", "a/../../b"} {
		if err := CopyFiles([]Record{{"source": source, "target": target}}, destination); err == nil {
			t.Errorf("CopyFiles(%q) unexpectedly succeeded", target)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
		t.Fatalf("unsafe target created outside workspace: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := CopyFiles([]Record{{"source": link, "target": "link.txt"}}, destination); err == nil {
		t.Fatal("CopyFiles accepted a symlink source")
	}
	if _, err := os.Stat(filepath.Join(destination, "link.txt")); !os.IsNotExist(err) {
		t.Fatalf("symlink source was copied: %v", err)
	}
}
