package bench

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func invokeCLI(t *testing.T, ctx context.Context, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := CLI(ctx, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func cliStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, root
}

func TestCLIValidateAndMissingKeyDoNotCreateStorage(t *testing.T) {
	base := t.TempDir()
	invalidManifest := filepath.Join(base, "invalid.json")
	if err := os.WriteFile(invalidManifest, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(base, "campaign.json")
	if err := os.WriteFile(manifest, []byte(`{"version":1,"models":["demo/offline"],"agents":[{"id":"pi","kind":"pi"}],"tasks":[{"id":"task","prompt":"do it","image":"fixture","evaluation":{"tests":[{"id":"check","score":1,"command":["python3","check.py"]}]}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(base, "artifacts")
	code, _, stderr := invokeCLI(t, context.Background(), "--artifacts", artifacts, "validate", invalidManifest)
	if code != 2 || !strings.Contains(strings.ToLower(stderr), "error:") {
		t.Fatalf("invalid manifest returned code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(artifacts); !os.IsNotExist(err) {
		t.Fatalf("invalid validation created artifact storage: stat err=%v", err)
	}
	code, stdout, stderr := invokeCLI(t, context.Background(), "--artifacts", artifacts, "validate", manifest)
	if code != 0 || stderr != "" {
		t.Fatalf("validate returned code=%d stderr=%q", code, stderr)
	}
	var validation Record
	if err := json.Unmarshal([]byte(stdout), &validation); err != nil || validation["valid"] != true || number(validation["runs"]) != 1 {
		t.Fatalf("unexpected validate output %q (%v)", stdout, err)
	}
	if _, err := os.Stat(artifacts); !os.IsNotExist(err) {
		t.Fatalf("validate created artifact storage: stat err=%v", err)
	}
	t.Setenv("AQP_CLI_TEST_MISSING_KEY", "")
	code, _, stderr = invokeCLI(t, context.Background(), "--artifacts", artifacts, "run", manifest, "--api-key-env", "AQP_CLI_TEST_MISSING_KEY")
	if code != 2 || !strings.Contains(stderr, "Set the explicitly selected API-key environment variable AQP_CLI_TEST_MISSING_KEY") {
		t.Fatalf("missing key returned code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(artifacts); !os.IsNotExist(err) {
		t.Fatalf("missing key created artifact storage: stat err=%v", err)
	}
}

func TestCLIDemoAndShowUsePublicCommandSurface(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("trusted offline checker requires host python3")
	}
	artifacts := filepath.Join(t.TempDir(), "artifacts")
	code, stdout, stderr := invokeCLI(t, context.Background(), "--artifacts", artifacts, "demo")
	if code != 0 || stderr != "" {
		t.Fatalf("demo returned code=%d stderr=%q", code, stderr)
	}
	var result Record
	if err := json.Unmarshal([]byte(stdout), &result); err != nil || result["simulation"] != true || number(object(result["evaluation"])["earned_score"]) != 5 || number(object(result["evaluation"])["passed_percent"]) != 100 || number(object(result["metrics"])["request_count"]) != 1 {
		t.Fatalf("unexpected public demo result %q (%v)", stdout, err)
	}
	code, stdout, stderr = invokeCLI(t, context.Background(), "--artifacts", artifacts, "show", text(result["id"]))
	if code != 0 || stderr != "" {
		t.Fatalf("show returned code=%d stderr=%q", code, stderr)
	}
	var retained Record
	if err := json.Unmarshal([]byte(stdout), &retained); err != nil || retained["simulation"] != true {
		t.Fatalf("show did not retain demo simulation: %q (%v)", stdout, err)
	}
}

func TestCLIControlsPreserveQuestionsAndOnlyStopExecutingRuns(t *testing.T) {
	store, artifacts := cliStore(t)
	if err := store.Save(Record{"id": "running", "status": "running"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Record{"id": "completed", "status": "completed"}); err != nil {
		t.Fatal(err)
	}
	metadata := Record{"method": "confirm", "agent_id": "main", "parent_id": "root", "title": "Proceed?", "custom": true}
	requestID, err := store.Question("running", "Proceed?", metadata)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := invokeCLI(t, context.Background(), "--artifacts", artifacts, "questions", "--run-id", "running")
	if code != 0 || stderr != "" {
		t.Fatalf("questions returned code=%d stderr=%q", code, stderr)
	}
	var questions []Record
	if err := json.Unmarshal([]byte(stdout), &questions); err != nil || len(questions) != 1 || questions[0]["metadata"].(map[string]any)["parent_id"] != "root" {
		t.Fatalf("question metadata was not preserved: %q (%v)", stdout, err)
	}
	code, _, stderr = invokeCLI(t, context.Background(), "--artifacts", artifacts, "answer", "running", requestID, "perhaps")
	if code != 2 || !strings.Contains(stderr, "Confirmation requires yes/no or true/false") {
		t.Fatalf("invalid answer returned code=%d stderr=%q", code, stderr)
	}
	if reply, exists, err := store.Response("running", requestID); err != nil || !exists || reply != nil {
		t.Fatalf("invalid answer changed pending question: reply=%#v exists=%v err=%v", reply, exists, err)
	}
	code, stdout, stderr = invokeCLI(t, context.Background(), "--artifacts", artifacts, "answer", "running", requestID, "yes")
	if code != 0 || stderr != "" || !strings.Contains(stdout, requestID) {
		t.Fatalf("valid answer returned code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if reply, _, err := store.Response("running", requestID); err != nil || reply != "yes" {
		t.Fatalf("answer was not persisted: reply=%#v err=%v", reply, err)
	}
	code, stdout, stderr = invokeCLI(t, context.Background(), "--artifacts", artifacts, "stop", "running")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "stop_requested") {
		t.Fatalf("stop returned code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if requested, err := store.StopRequested("running"); err != nil || !requested {
		t.Fatalf("stop was not persisted: requested=%v err=%v", requested, err)
	}
	code, _, stderr = invokeCLI(t, context.Background(), "--artifacts", artifacts, "stop", "completed")
	if code != 2 || !strings.Contains(stderr, "Run is not currently executing an agent") {
		t.Fatalf("completed run stop returned code=%d stderr=%q", code, stderr)
	}
}

func TestCLIRejectsUnknownAndSiblingOptionsWithoutMutation(t *testing.T) {
	base := t.TempDir()
	manifest := filepath.Join(base, "campaign.json")
	if err := os.WriteFile(manifest, []byte(`{"version":1,"models":["demo/offline"],"agents":[{"id":"pi","kind":"pi"}],"tasks":[{"id":"task","prompt":"do it","image":"fixture","evaluation":{"tests":[{"id":"check","score":1,"command":["python3","check.py"]}]}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(base, "artifacts")
	for _, args := range [][]string{
		{"--artifacts", artifacts, "run", manifest, "--unknown"},
		{"--artifacts", artifacts, "validate", manifest, "--api-key-env", "KEY"},
		{"--artifacts", artifacts, "list", "--unknown"},
	} {
		code, _, stderr := invokeCLI(t, context.Background(), args...)
		if code != 2 || !strings.Contains(stderr, "error:") {
			t.Errorf("args %q returned code=%d stderr=%q", args, code, stderr)
		}
	}
	if _, err := os.Stat(artifacts); !os.IsNotExist(err) {
		t.Fatalf("rejected arguments created artifact storage: stat err=%v", err)
	}
}

func TestCLIRerunRefusesSimulationRecords(t *testing.T) {
	store, artifacts := cliStore(t)
	if err := store.Save(Record{"id": "offline", "status": "completed", "simulation": true}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AQP_CLI_TEST_KEY", "not-used")
	code, _, stderr := invokeCLI(t, context.Background(), "--artifacts", artifacts, "rerun", "offline", "--api-key-env", "AQP_CLI_TEST_KEY")
	if code != 2 || !strings.Contains(stderr, "Use demo for offline simulation; rerun is for live definitions") {
		t.Fatalf("rerun returned code=%d stderr=%q", code, stderr)
	}
}

func TestCLIShowPreservesMissingRunKeyErrorText(t *testing.T) {
	_, artifacts := cliStore(t)
	code, _, stderr := invokeCLI(t, context.Background(), "--artifacts", artifacts, "show", "missing")
	if code != 2 || stderr != "Error: 'missing'\n" {
		t.Fatalf("missing run returned code=%d stderr=%q", code, stderr)
	}
}

func TestCLIHelpAndDashboardCancellation(t *testing.T) {
	code, stdout, stderr := invokeCLI(t, context.Background(), "--help")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Offline simulation") || !strings.Contains(stdout, "--artifacts") {
		t.Fatalf("help returned code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	writer := &cancelOnWrite{cancel: cancel}
	var errorOutput strings.Builder
	code = CLI(ctx, []string{"--artifacts", filepath.Join(t.TempDir(), "artifacts"), "dashboard", "--port", "0"}, writer, &errorOutput)
	stdout, stderr = writer.String(), errorOutput.String()
	if code != 130 || stderr != "" || !strings.HasPrefix(stdout, "Live monitoring: http://127.0.0.1:") {
		t.Fatalf("dashboard returned code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

type cancelOnWrite struct {
	buffer strings.Builder
	cancel context.CancelFunc
}

func (w *cancelOnWrite) Write(data []byte) (int, error) {
	n, err := w.buffer.Write(data)
	w.cancel()
	return n, err
}

func (w *cancelOnWrite) String() string { return w.buffer.String() }

var _ io.Writer = (*cancelOnWrite)(nil)
