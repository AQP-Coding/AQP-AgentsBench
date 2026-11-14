package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"
)

var errBinaryArtifact = errors.New("artifact cannot be represented as text")

var ignoredArtifactDirectories = map[string]bool{
	".git": true, "node_modules": true, ".venv": true, "__pycache__": true,
}

// Inventory returns content hashes and file kinds without following symlinks.
func Inventory(root string) (map[string]Record, error) {
	files := map[string]Record{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != root && entry.IsDir() && ignoredArtifactDirectories[entry.Name()] {
			return filepath.SkipDir
		}
		if path == root {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			files[relative] = Record{"kind": "symlink", "target": target}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		files[relative] = Record{
			"kind": "file", "sha256": hex.EncodeToString(hash.Sum(nil)),
			"executable": info.Mode().Perm()&0o111 != 0,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func artifactMode(item Record) string {
	if text(item["kind"]) == "symlink" {
		return "120000"
	}
	if executable, _ := item["executable"].(bool); executable {
		return "100755"
	}
	return "100644"
}

func artifactText(root, name string, item Record) (string, error) {
	if item == nil {
		return "", nil
	}
	if text(item["kind"]) == "symlink" {
		return text(item["target"]), nil
	}
	if root == "" {
		return "", errBinaryArtifact
	}
	path := filepath.Join(root, filepath.FromSlash(name))
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > 1024*1024 {
		return "", errBinaryArtifact
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(content) {
		return "", errBinaryArtifact
	}
	return string(content), nil
}

// WriteDiff serializes a deterministic Git-style patch and returns the new inventory.
// An empty baseline signals that existing text files cannot be read and are emitted as binary.
func WriteDiff(before map[string]Record, workspace, destination, baseline string) (map[string]Record, error) {
	after, err := Inventory(workspace)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(before)+len(after))
	seen := make(map[string]bool, len(before)+len(after))
	for name := range before {
		seen[name] = true
		names = append(names, name)
	}
	for name := range after {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	lines := []string{}
	for _, name := range names {
		old, new := before[name], after[name]
		if reflect.DeepEqual(old, new) {
			continue
		}
		a, b := jsonPath("a/"+name), jsonPath("b/"+name)
		lines = append(lines, "diff --git "+a+" "+b+"\n")
		switch {
		case old == nil:
			lines = append(lines, "new file mode "+artifactMode(new)+"\n")
		case new == nil:
			lines = append(lines, "deleted file mode "+artifactMode(old)+"\n")
		case artifactMode(old) != artifactMode(new):
			lines = append(lines, "old mode "+artifactMode(old)+"\n", "new mode "+artifactMode(new)+"\n")
		}
		oldText, oldErr := artifactText(baseline, name, old)
		newText, newErr := artifactText(workspace, name, new)
		if errors.Is(oldErr, errBinaryArtifact) || errors.Is(newErr, errBinaryArtifact) {
			lines = append(lines, "Binary files "+a+" and "+b+" differ\n")
			continue
		}
		if oldErr != nil {
			return nil, oldErr
		}
		if newErr != nil {
			return nil, newErr
		}
		from, to := a, b
		if old == nil {
			from = "/dev/null"
		}
		if new == nil {
			to = "/dev/null"
		}
		lines = append(lines, unifiedDiff(oldText, newText, from, to)...)
	}
	if err := os.WriteFile(destination, []byte(strings.Join(lines, "")), 0o644); err != nil {
		return nil, err
	}
	return after, nil
}

func jsonPath(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

type diffOperation struct {
	kind byte
	line string
}

func splitArtifactLines(value string) []string {
	if value == "" {
		return nil
	}
	lines := strings.SplitAfter(value, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func myersDiff(old, new []string) []diffOperation {
	n, m := len(old), len(new)
	max := n + m
	v := map[int]int{1: 0}
	trace := make([]map[int]int, 0, max+1)
	finish := 0
	for d := 0; d <= max; d++ {
		snapshot := make(map[int]int, len(v))
		for k, x := range v {
			snapshot[k] = x
		}
		trace = append(trace, snapshot)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1] < v[k+1]) {
				x = v[k+1]
			} else {
				x = v[k-1] + 1
			}
			y := x - k
			for x < n && y < m && old[x] == new[y] {
				x++
				y++
			}
			v[k] = x
			if x >= n && y >= m {
				finish = d
				goto backtrack
			}
		}
	}

backtrack:
	result := make([]diffOperation, 0, n+m)
	x, y := n, m
	for d := finish; d > 0; d-- {
		previous := trace[d]
		k := x - y
		var previousK int
		if k == -d || (k != d && previous[k-1] < previous[k+1]) {
			previousK = k + 1
		} else {
			previousK = k - 1
		}
		previousX := previous[previousK]
		previousY := previousX - previousK
		for x > previousX && y > previousY {
			result = append(result, diffOperation{' ', old[x-1]})
			x--
			y--
		}
		if x == previousX {
			result = append(result, diffOperation{'+', new[y-1]})
			y--
		} else {
			result = append(result, diffOperation{'-', old[x-1]})
			x--
		}
	}
	for x > 0 && y > 0 {
		result = append(result, diffOperation{' ', old[x-1]})
		x--
		y--
	}
	for x > 0 {
		result = append(result, diffOperation{'-', old[x-1]})
		x--
	}
	for y > 0 {
		result = append(result, diffOperation{'+', new[y-1]})
		y--
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}

func unifiedDiff(oldText, newText, from, to string) []string {
	oldLines, newLines := splitArtifactLines(oldText), splitArtifactLines(newText)
	operations := myersDiff(oldLines, newLines)
	changed := []int{}
	for i, operation := range operations {
		if operation.kind != ' ' {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	starts, ends := []int{}, []int{}
	for _, index := range changed {
		start, end := artifactMaximum(0, index-3), artifactMinimum(len(operations)-1, index+3)
		if len(starts) > 0 && start <= ends[len(ends)-1] {
			if end > ends[len(ends)-1] {
				ends[len(ends)-1] = end
			}
		} else {
			starts = append(starts, start)
			ends = append(ends, end)
		}
	}
	result := []string{"--- " + from + "\n", "+++ " + to + "\n"}
	for hunk := range starts {
		oldPosition, newPosition := 0, 0
		for _, operation := range operations[:starts[hunk]] {
			if operation.kind != '+' {
				oldPosition++
			}
			if operation.kind != '-' {
				newPosition++
			}
		}
		oldCount, newCount := 0, 0
		for _, operation := range operations[starts[hunk] : ends[hunk]+1] {
			if operation.kind != '+' {
				oldCount++
			}
			if operation.kind != '-' {
				newCount++
			}
		}
		oldStart, newStart := oldPosition, newPosition
		if oldCount > 0 {
			oldStart++
		}
		if newCount > 0 {
			newStart++
		}
		result = append(result, fmt.Sprintf("@@ -%s +%s @@\n", formatDiffRange(oldStart, oldCount), formatDiffRange(newStart, newCount)))
		for _, operation := range operations[starts[hunk] : ends[hunk]+1] {
			result = append(result, string(operation.kind)+operation.line)
			if !strings.HasSuffix(operation.line, "\n") {
				result = append(result, "\n\\ No newline at end of file\n")
			}
		}
	}
	return result
}

func formatDiffRange(start, count int) string {
	if count == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

func artifactMaximum(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func artifactMinimum(a, b int) int {
	if a < b {
		return a
	}
	return b
}
