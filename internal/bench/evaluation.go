package bench

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// copySnapshot preserves links as links rather than reading their host targets.
func copySnapshot(source, destination string) error {
	return filepath.WalkDir(source, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name != source && entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(source, name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(name)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		input, err := os.Open(name)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

func pytestCaseStatus(report string) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(report))
	count, depth, testDepth := 0, 0, 0
	status := ""
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch token := token.(type) {
		case xml.StartElement:
			depth++
			if token.Name.Local == "testcase" {
				count++
				testDepth = depth
			}
			if testDepth > 0 {
				switch token.Name.Local {
				case "skipped":
					status = "skipped"
				case "error", "failure":
					if status != "skipped" {
						status = "failed"
					}
				}
			}
		case xml.EndElement:
			if depth == testDepth {
				testDepth = 0
			}
			depth--
		}
	}
	if count != 1 {
		return "", fmt.Errorf("Each evaluation entry must select exactly one pytest unit test")
	}
	return status, nil
}

// Evaluate scores trusted test commands in separate evaluator containers using
// a private workspace snapshot and separately staged hidden files. The agent must
// already be stopped. Each runnable test gets its own container; unreadable required mounts
// are infrastructure errors, not failing assertions. Temporary snapshots are removed
// on return, while results retain per-test output and scoring.
func Evaluate(ctx context.Context, runtime Runtime, workspace string, definition Record, scratchParent string) (Record, error) {
	root, err := os.MkdirTemp(scratchParent, "aqp-evaluate-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	if err = os.Chmod(root, 0755); err != nil {
		return nil, err
	}
	snapshot := filepath.Join(root, "workspace")
	if err = copySnapshot(workspace, snapshot); err != nil {
		return nil, err
	}
	trusted := filepath.Join(root, "tests")
	if err = os.Mkdir(trusted, 0755); err != nil {
		return nil, err
	}
	files := records(definition["files"])
	if err = CopyFiles(files, trusted); err != nil {
		return nil, err
	}
	outcomes := []Record{}
	for _, test := range records(definition["tests"]) {
		outcome, err := evaluateOne(ctx, runtime, snapshot, trusted, definition, test, files)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return ScoreTests(records(definition["tests"]), outcomes)
}

func evaluateOne(ctx context.Context, runtime Runtime, snapshot, trusted string, definition, test Record, files []Record) (outcome Record, returnErr error) {
	container, err := runtime.Start(ctx, text(definition["image"]), snapshot, StartOptions{Evaluation: trusted, ReadonlyWorkspace: true})
	if err != nil {
		return nil, err
	}
	defer func() {
		stopErr := runtime.Stop(context.Background(), container)
		removeErr := runtime.Remove(context.Background(), container)
		if returnErr == nil {
			if stopErr != nil {
				returnErr = stopErr
			} else if removeErr != nil {
				returnErr = removeErr
			}
		}
	}()
	uid := 65534
	if os.Getuid() == uid {
		uid = 65533
	}
	user := fmt.Sprintf("%d:%d", uid, uid)
	execute := func(command []string, timeout time.Duration) (ExecResult, error) {
		if runtime.TransportFor(container) == "docker_cp" {
			return runtime.ExecAs(ctx, container, user, command, timeout)
		}
		return runtime.Exec(ctx, container, command, timeout)
	}
	timedOut := func(err error) bool { return errors.Is(err, context.DeadlineExceeded) }
	if len(files) > 0 {
		command := []string{"sh", "-c", `for path do if [ ! -e "$path" ] || [ ! -r "$path" ]; then printf "missing or unreadable: %s\n" "$path"; exit 1; fi; done`, "aqp-evaluation-preflight"}
		for _, file := range files {
			command = append(command, "/evaluation/"+text(file["target"]))
		}
		probe, err := execute(command, 10*time.Second)
		if timedOut(err) {
			return Record{"id": test["id"], "status": "timed_out", "exit_code": nil}, nil
		}
		if err != nil {
			return nil, err
		}
		if probe.ReturnCode != 0 {
			detail := strings.TrimSpace(strings.TrimSpace(probe.Stdout) + " " + strings.TrimSpace(probe.Stderr))
			if detail == "" {
				detail = "file availability check failed"
			}
			return nil, fmt.Errorf("Evaluator mount preflight failed (exit code %d): %s", probe.ReturnCode, detail)
		}
	}
	command := stringsOf(test["command"])
	if len(command) == 0 {
		return nil, fmt.Errorf("Commands must be nonempty argv arrays, not shell strings")
	}
	pytest := command[0] == "pytest" || command[0] == "py.test" || (len(command) > 2 && command[1] == "-m" && command[2] == "pytest")
	if pytest {
		command = append(append([]string{"env", "PYTEST_DISABLE_PLUGIN_AUTOLOAD=1"}, command...), "-c", "/dev/null", "--noconftest", "--confcutdir=/evaluation", "-o", "cache_dir=/tmp/aqp-pytest-cache", "--junitxml=/tmp/aqp-unit.xml")
	}
	result, err := execute(command, seconds(definition, "timeout_seconds", 60))
	if timedOut(err) {
		return Record{"id": test["id"], "status": "timed_out", "exit_code": nil}, nil
	}
	if err != nil {
		return nil, err
	}
	if result.ReturnCode == 125 || result.ReturnCode == 126 || result.ReturnCode == 127 {
		return nil, fmt.Errorf("Evaluation command/runtime is unavailable")
	}
	status := "failed"
	if result.ReturnCode == 0 {
		status = "passed"
	}
	if pytest {
		report, err := execute([]string{"cat", "/tmp/aqp-unit.xml"}, 5*time.Second)
		if timedOut(err) {
			return Record{"id": test["id"], "status": "timed_out", "exit_code": nil}, nil
		}
		if err != nil {
			return nil, err
		}
		if report.ReturnCode != 0 {
			return nil, fmt.Errorf("Missing trusted pytest result report")
		}
		parsed, err := pytestCaseStatus(report.Stdout)
		if err != nil {
			return nil, err
		}
		if parsed != "" {
			status = parsed
		}
	}
	return Record{"id": test["id"], "status": status, "exit_code": result.ReturnCode, "stdout": result.Stdout, "stderr": result.Stderr}, nil
}
