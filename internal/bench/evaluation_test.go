package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type evaluatorRuntime struct {
	xml            string
	starts         []StartOptions
	workspaces     []string
	commands       [][]string
	users          []string
	transport      string
	missing        bool
	timeout        bool
	stops, removes int
}

func (r *evaluatorRuntime) Start(_ context.Context, _ string, workspace string, o StartOptions) (string, error) {
	r.starts = append(r.starts, o)
	r.workspaces = append(r.workspaces, workspace)
	return "eval", nil
}
func (r *evaluatorRuntime) Exec(_ context.Context, _ string, command []string, _ time.Duration) (ExecResult, error) {
	r.commands = append(r.commands, append([]string{}, command...))
	if r.timeout {
		return ExecResult{}, context.DeadlineExceeded
	}
	if r.missing && command[0] == "sh" {
		return ExecResult{ReturnCode: 1, Stderr: "missing or unreadable: /evaluation/check.py\n"}, nil
	}
	if command[0] == "cat" {
		return ExecResult{Stdout: r.xml}, nil
	}
	return ExecResult{}, nil
}
func (r *evaluatorRuntime) ExecAs(c context.Context, id, user string, command []string, d time.Duration) (ExecResult, error) {
	r.users = append(r.users, user)
	return r.Exec(c, id, command, d)
}
func (r *evaluatorRuntime) Spawn(context.Context, string, []string, bool) (Process, error) {
	return nil, fmt.Errorf("unexpected spawn")
}
func (r *evaluatorRuntime) Address(string) (string, error)                        { return "", nil }
func (r *evaluatorRuntime) TransportFor(string) string                            { return r.transport }
func (r *evaluatorRuntime) ExportWorkspace(context.Context, string, string) error { return nil }
func (r *evaluatorRuntime) Stop(context.Context, string) error                    { r.stops++; return nil }
func (r *evaluatorRuntime) Remove(context.Context, string) error                  { r.removes++; return nil }

func evaluatorDefinition() Record {
	return Record{"image": "fixture", "files": []any{}, "tests": []Record{{"id": "unit", "score": 5, "command": []string{"pytest", "/evaluation/test_unit.py::test_answer"}}}}
}

func TestEvaluatorUsesOneRealPytestCase(t *testing.T) {
	for _, tt := range []struct {
		xml, status string
		score       float64
	}{
		{`<testsuites><testsuite><testcase name="a"/></testsuite></testsuites>`, "passed", 5},
		{`<testsuites><testsuite><testcase name="a"><skipped/></testcase></testsuite></testsuites>`, "skipped", 0},
		{`<testsuites><testsuite><testcase name="a"><failure/></testcase></testsuite></testsuites>`, "failed", 0},
	} {
		t.Run(tt.status, func(t *testing.T) {
			r := &evaluatorRuntime{xml: tt.xml}
			result, err := Evaluate(context.Background(), r, t.TempDir(), evaluatorDefinition(), "")
			if err != nil || number(result["earned_score"]) != tt.score || records(result["tests"])[0]["status"] != tt.status {
				t.Fatal(result, err)
			}
			if !r.starts[0].ReadonlyWorkspace || r.stops != 1 || r.removes != 1 {
				t.Fatal("missing isolation/cleanup", r.starts, r.stops, r.removes)
			}
			args := strings.Join(r.commands[0], " ")
			if !strings.Contains(args, "PYTEST_DISABLE_PLUGIN_AUTOLOAD=1") || !strings.Contains(args, "--noconftest") {
				t.Fatal(args)
			}
		})
	}
	r := &evaluatorRuntime{xml: `<testsuites><testsuite><testcase/><testcase/></testsuite></testsuites>`}
	if _, err := Evaluate(context.Background(), r, t.TempDir(), evaluatorDefinition(), ""); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatal(err)
	}
}

func TestEvaluatorPreflightUIDAndArtifactSnapshot(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "workspace")
			_ = os.Mkdir(workspace, 0755)
			checker := filepath.Join(root, "check.py")
			_ = os.WriteFile(checker, []byte("pass\n"), 0600)
			spec := evaluatorDefinition()
			spec["files"] = []Record{{"source": checker, "target": "check.py"}}
			r := &evaluatorRuntime{xml: `<testsuites><testsuite><testcase/></testsuite></testsuites>`, transport: "docker_cp", missing: missing}
			_, err := Evaluate(context.Background(), r, workspace, spec, root)
			if missing {
				if err == nil || !strings.Contains(err.Error(), "Evaluator mount preflight failed") || len(r.commands) != 1 {
					t.Fatal(err, r.commands)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if filepath.Dir(filepath.Dir(r.workspaces[0])) != root {
				t.Fatal("snapshot parent", r.workspaces)
			}
			uid := 65534
			if os.Getuid() == uid {
				uid = 65533
			}
			for _, user := range r.users {
				if user != fmt.Sprintf("%d:%d", uid, uid) {
					t.Fatal(user)
				}
			}
			if r.commands[0][0] != "sh" || !strings.Contains(strings.Join(r.commands[0], " "), "/evaluation/check.py") {
				t.Fatal(r.commands)
			}
			if r.stops != 1 || r.removes != 1 {
				t.Fatal("cleanup missing")
			}
		})
	}
}

func TestEvaluatorTimeoutDoesNotAwardPoints(t *testing.T) {
	r := &evaluatorRuntime{timeout: true}
	result, err := Evaluate(context.Background(), r, t.TempDir(), evaluatorDefinition(), "")
	if err != nil || number(result["earned_score"]) != 0 || records(result["tests"])[0]["status"] != "timed_out" {
		t.Fatal(result, err)
	}
}
