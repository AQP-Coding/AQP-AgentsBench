package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type copyPlan struct {
	source string
	target string
	dir    bool
}

func gitEnvironment(home string) []string {
	blocked := map[string]bool{
		"SSH_AUTH_SOCK": true, "SSH_ASKPASS": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true,
		"http_proxy": true, "https_proxy": true, "all_proxy": true,
	}
	env := make([]string, 0, len(os.Environ())+8)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "GIT_") || blocked[key] || key == "HOME" || key == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, item)
	}
	env = append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_ALLOW_PROTOCOL=file:http:https",
		"GCM_INTERACTIVE=never",
	)
	return env
}

func runGit(ctx context.Context, args ...string) (string, error) {
	home, err := os.MkdirTemp("", "aqp-git-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(home)
	commandCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, "git", append([]string{
		"-c", "credential.helper=",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
	}, args...)...)
	command.Env = gitEnvironment(home)
	output, err := command.Output()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if exitErr, ok := err.(*exec.ExitError); ok {
			message = strings.TrimSpace(string(exitErr.Stderr))
		}
		if commandCtx.Err() != nil {
			return "", fmt.Errorf("Git input preparation failed: %w", commandCtx.Err())
		}
		return "", fmt.Errorf("Git input preparation failed: %s", message)
	}
	return strings.TrimSpace(string(output)), nil
}

func preflightRepository(ctx context.Context, repository Record) (Record, []any, error) {
	source := text(repository["source"])
	if source == "" {
		return nil, nil, fmt.Errorf("Repository source must be its Git root")
	}
	parsed, parseErr := url.Parse(source)
	if parseErr == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		return cloneRecord(repository), []any{}, nil
	}
	root, err := filepath.Abs(source)
	if err != nil {
		return nil, nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, fmt.Errorf("Git input preparation failed: %w", err)
	}
	actual, err := runGit(ctx, "-C", root, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, nil, err
	}
	actual, err = filepath.EvalSymlinks(actual)
	if err != nil {
		return nil, nil, fmt.Errorf("Git input preparation failed: %w", err)
	}
	if actual != root {
		return nil, nil, fmt.Errorf("Repository source must be its Git root")
	}
	revision := "HEAD"
	if value, ok := repository["revision"]; ok {
		revision = text(value)
	}
	if strings.HasPrefix(revision, "-") {
		return nil, nil, fmt.Errorf("Invalid Git revision")
	}
	commit, err := runGit(ctx, "-C", root, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return nil, nil, err
	}
	dirty, err := runGit(ctx, "-C", root, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return nil, nil, err
	}
	warnings := []any{}
	if dirty != "" {
		warnings = append(warnings, fmt.Sprintf("Uncommitted files in %s are excluded; using commit %s.", filepath.Base(root), commit))
	}
	resolved := cloneRecord(repository)
	resolved["source"], resolved["revision"] = root, commit
	return resolved, warnings, nil
}

// PreflightRepository resolves a local source to its immutable commit and
// includes any snapshot warnings in the returned record.
func PreflightRepository(repository Record) (Record, error) {
	resolved, warnings, err := preflightRepository(context.Background(), repository)
	if err != nil {
		return nil, err
	}
	resolved["warnings"] = warnings
	return resolved, nil
}

func resolveExisting(pathname string) (string, error) {
	abs, err := filepath.Abs(pathname)
	if err != nil {
		return "", err
	}
	suffix := []string{}
	current := abs
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func inside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func hasWindowsDrivePrefix(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':'
}

func rejectSourceTargetOverlap(plan copyPlan) error {
	if !plan.dir {
		return nil
	}
	source, err := filepath.Abs(plan.source)
	if err != nil {
		return err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return err
	}
	if inside(source, plan.target) || inside(plan.target, source) {
		return fmt.Errorf("Source and target must not overlap")
	}
	return nil
}

func rejectSymlinkAncestors(root, target string) error {
	current := root
	rel, err := filepath.Rel(root, filepath.Dir(target))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("Target escapes the workspace")
	}
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("Target escapes through a symlink")
			}
			if !info.IsDir() {
				return fmt.Errorf("Target parent is not a directory")
			}
		}
	}
	return nil
}

func planCopyFiles(files []Record, destination string) (string, []copyPlan, error) {
	root, err := resolveExisting(destination)
	if err != nil {
		return "", nil, err
	}
	plans := make([]copyPlan, 0, len(files))
	for _, entry := range files {
		source := text(entry["source"])
		targetName := text(entry["target"])
		if hasWindowsDrivePrefix(targetName) {
			return "", nil, fmt.Errorf("Expected a nonempty relative POSIX path")
		}
		relative, err := RelativePath(targetName)
		if err != nil {
			return "", nil, err
		}
		sourceInfo, err := os.Lstat(source)
		if err != nil || sourceInfo.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("Sources must exist and cannot be symbolic links")
		}
		plan := copyPlan{source: source, target: filepath.Join(root, filepath.FromSlash(relative)), dir: sourceInfo.IsDir()}
		if !plan.dir && !sourceInfo.Mode().IsRegular() {
			return "", nil, fmt.Errorf("Sources must be regular files or directories")
		}
		if plan.dir {
			err = filepath.WalkDir(source, func(filename string, item os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if filename == source {
					return nil
				}
				info, err := os.Lstat(filename)
				if err != nil {
					return err
				}
				if info.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("Attachment trees cannot contain symbolic links")
				}
				if !info.IsDir() && !info.Mode().IsRegular() {
					return fmt.Errorf("Attachment entries must be regular files or directories")
				}
				return nil
			})
			if err != nil {
				return "", nil, err
			}
		}
		if err := rejectSymlinkAncestors(root, plan.target); err != nil {
			return "", nil, err
		}
		if err := rejectSourceTargetOverlap(plan); err != nil {
			return "", nil, err
		}
		if _, err := os.Lstat(plan.target); err == nil {
			return "", nil, fmt.Errorf("Input target already exists: %s", targetName)
		} else if !os.IsNotExist(err) {
			return "", nil, err
		}
		for _, prior := range plans {
			if plan.target == prior.target || inside(plan.target, prior.target) || inside(prior.target, plan.target) {
				return "", nil, fmt.Errorf("File targets must not overlap")
			}
		}
		plans = append(plans, plan)
	}
	return root, plans, nil
}

func copyPlanned(plans []copyPlan, root string) ([]Record, error) {
	hashes := []Record{}
	for _, plan := range plans {
		if err := rejectSymlinkAncestors(root, plan.target); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(plan.target), 0o755); err != nil {
			return nil, err
		}
		if err := rejectSymlinkAncestors(root, plan.target); err != nil {
			return nil, err
		}
		if plan.dir {
			if err := os.Mkdir(plan.target, 0o755); err != nil {
				return nil, err
			}
			err := filepath.WalkDir(plan.source, func(filename string, item os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if filename == plan.source {
					return nil
				}
				rel, err := filepath.Rel(plan.source, filename)
				if err != nil {
					return err
				}
				target := filepath.Join(plan.target, rel)
				info, err := os.Lstat(filename)
				if err != nil {
					return err
				}
				if info.IsDir() {
					return os.Mkdir(target, info.Mode().Perm())
				}
				return copyOne(filename, target, root, info.Mode().Perm())
			})
			if err != nil {
				return nil, err
			}
			var paths []string
			err = filepath.WalkDir(plan.target, func(filename string, item os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if !item.IsDir() {
					paths = append(paths, filename)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			sort.Strings(paths)
			for _, filename := range paths {
				if err := appendHash(&hashes, root, filename); err != nil {
					return nil, err
				}
			}
		} else {
			info, err := os.Lstat(plan.source)
			if err != nil {
				return nil, err
			}
			if err := copyOne(plan.source, plan.target, root, info.Mode().Perm()); err != nil {
				return nil, err
			}
			if err := appendHash(&hashes, root, plan.target); err != nil {
				return nil, err
			}
		}
	}
	return hashes, nil
}

func copyOne(source, target, root string, mode os.FileMode) error {
	if err := rejectSymlinkAncestors(root, target); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("Sources must be regular files and cannot be symbolic links")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(target)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func appendHash(hashes *[]Record, root, filename string) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("Attachment targets must be regular files")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	relative, err := filepath.Rel(root, filename)
	if err != nil {
		return err
	}
	*hashes = append(*hashes, Record{"target": filepath.ToSlash(relative), "sha256": hex.EncodeToString(digest[:])})
	return nil
}

// CopyFiles copies validated attachment sources into an existing or new workspace.
func CopyFiles(files []Record, destination string) error {
	root, plans, err := planCopyFiles(files, destination)
	if err != nil {
		return err
	}
	_, err = copyPlanned(plans, root)
	return err
}

// PrepareWorkspace snapshots a pinned local Git commit, then adds selected
// attachments without changing the source repository.
func PrepareWorkspace(ctx context.Context, task Record, destination string) (Record, error) {
	if _, err := os.Lstat(destination); err == nil {
		return nil, fmt.Errorf("Workspace destination must not already exist")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	files := records(task["attachments"])
	// Reject invalid attachments before creating a destination or cloning a repository.
	if _, _, err := planCopyFiles(files, absolute); err != nil {
		return nil, err
	}
	result := Record{"commit": nil, "warnings": []any{}, "attachments": []Record{}}
	repository := object(task["repository"])
	if len(repository) > 0 {
		resolved, warnings, err := preflightRepository(ctx, repository)
		if err != nil {
			return nil, err
		}
		repositoryURL, _ := url.Parse(text(resolved["source"]))
		if repositoryURL.Scheme != "http" && repositoryURL.Scheme != "https" {
			resolvedDestination, err := resolveExisting(absolute)
			if err != nil {
				return nil, err
			}
			if inside(text(resolved["source"]), resolvedDestination) {
				return nil, fmt.Errorf("Workspace destination must not overlap its repository source")
			}
		}
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			return nil, err
		}
		if err := os.Mkdir(absolute, 0o755); err != nil {
			return nil, err
		}
		cleanup := true
		defer func() {
			if cleanup {
				_ = os.RemoveAll(absolute)
			}
		}()
		if _, err := runGit(ctx, "clone", "--quiet", "--no-hardlinks", "--no-checkout", "--", text(resolved["source"]), absolute); err != nil {
			return nil, err
		}
		if _, err := runGit(ctx, "-C", absolute, "checkout", "--quiet", "--detach", text(resolved["revision"])); err != nil {
			return nil, err
		}
		commit, err := runGit(ctx, "-C", absolute, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		result["commit"], result["warnings"] = commit, warnings
		root, plans, err := planCopyFiles(files, absolute)
		if err != nil {
			return nil, err
		}
		attachments, err := copyPlanned(plans, root)
		if err != nil {
			return nil, err
		}
		result["attachments"] = attachments
		cleanup = false
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(absolute, 0o755); err != nil {
		return nil, err
	}
	root, plans, err := planCopyFiles(files, absolute)
	if err != nil {
		_ = os.RemoveAll(absolute)
		return nil, err
	}
	attachments, err := copyPlanned(plans, root)
	if err != nil {
		_ = os.RemoveAll(absolute)
		return nil, err
	}
	result["attachments"] = attachments
	return result, nil
}

func safeRelativeTarget(value string) (string, error) {
	if value == "" || strings.Contains(value, `\`) || path.IsAbs(value) || hasWindowsDrivePrefix(value) {
		return "", fmt.Errorf("Harness targets must be safe nonempty relative paths")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", fmt.Errorf("Harness targets must be safe nonempty relative paths")
		}
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("Harness targets must be safe nonempty relative paths")
	}
	return clean, nil
}
