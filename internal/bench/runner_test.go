package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type runnerTestRuntime struct {
	containers map[string]runnerTestContainer
	next       int
	lifecycle  []string
	execs      []string
	exportErr  error
	transport  string
	stopStore  *Store
}

type runnerTestContainer struct {
	workspace string
	options   StartOptions
}

func newRunnerTestRuntime() *runnerTestRuntime {
	return &runnerTestRuntime{containers: map[string]runnerTestContainer{}, transport: "docker_cp"}
}

func (r *runnerTestRuntime) Start(_ context.Context, _ string, workspace string, options StartOptions) (string, error) {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("runner test runtime requires an absolute workspace: %q", workspace)
	}
	if options.Evaluation != "" {
		for _, existing := range r.containers {
			if existing.options.Home != "" && fileExists(existing.options.Home) {
				return "", errors.New("agent harness home remained available to the evaluator")
			}
		}
	}
	r.next++
	id := fmt.Sprintf("container-%d", r.next)
	r.containers[id] = runnerTestContainer{workspace: workspace, options: options}
	r.lifecycle = append(r.lifecycle, "start:"+id)
	if options.Evaluation != "" {
		r.lifecycle = append(r.lifecycle, "start-evaluation:"+id)
	}
	return id, nil
}

func (r *runnerTestRuntime) Exec(ctx context.Context, id string, command []string, _ time.Duration) (ExecResult, error) {
	container := r.containers[id]
	if container.workspace == "" || !filepath.IsAbs(container.workspace) {
		return ExecResult{}, fmt.Errorf("runner test exec requires an absolute workspace: %q", container.workspace)
	}
	_, answerErr := os.Stat(filepath.Join(container.workspace, "answer.txt"))
	r.execs = append(r.execs, fmt.Sprintf("%s: %s (answer=%v)", id, strings.Join(command, " "), answerErr == nil))
	if len(command) == 1 && command[0] == "wait-for-stop" {
		rows, err := r.stopStore.List()
		if err != nil || len(rows) == 0 {
			return ExecResult{}, fmt.Errorf("read run record for setup stop: %w", err)
		}
		if err := r.stopStore.Stop(text(rows[0]["id"])); err != nil {
			return ExecResult{}, err
		}
		select {
		case <-ctx.Done():
			return ExecResult{ReturnCode: 1, Stderr: "stopped"}, nil
		case <-time.After(2 * time.Second):
			return ExecResult{}, errors.New("watcher did not stop setup")
		}
	}
	if len(command) > 0 && command[0] == "sh" {
		return ExecResult{}, nil
	}
	if _, err := os.Stat(filepath.Join(container.workspace, "answer.txt")); err == nil {
		return ExecResult{}, nil
	}
	return ExecResult{ReturnCode: 1}, nil
}

func (r *runnerTestRuntime) ExecAs(ctx context.Context, id, _ string, command []string, timeout time.Duration) (ExecResult, error) {
	return r.Exec(ctx, id, command, timeout)
}

func (r *runnerTestRuntime) Spawn(context.Context, string, []string, bool) (Process, error) {
	return nil, errors.New("unexpected spawn")
}

func (r *runnerTestRuntime) Address(string) (string, error) { return "", nil }
func (r *runnerTestRuntime) TransportFor(string) string     { return r.transport }
func (r *runnerTestRuntime) ExportWorkspace(_ context.Context, id, _ string) error {
	if container := r.containers[id]; container.workspace == "" || !filepath.IsAbs(container.workspace) {
		return fmt.Errorf("runner test export requires an absolute workspace: %q", container.workspace)
	}
	r.lifecycle = append(r.lifecycle, fmt.Sprintf("export:%s answer=%t", id, fileExists(filepath.Join(r.containers[id].workspace, "answer.txt"))))
	return r.exportErr
}

func fileExists(filename string) bool { _, err := os.Stat(filename); return err == nil }
func (r *runnerTestRuntime) Stop(_ context.Context, id string) error {
	r.lifecycle = append(r.lifecycle, "stop:"+id)
	return nil
}
func (r *runnerTestRuntime) Remove(_ context.Context, id string) error {
	r.lifecycle = append(r.lifecycle, "remove:"+id)
	return nil
}
func (r *runnerTestRuntime) Close() error { r.lifecycle = append(r.lifecycle, "close"); return nil }

type runnerTestAdapter struct {
	runtime   *runnerTestRuntime
	container string
	failure   error
}

func (a runnerTestAdapter) Run(_ context.Context, _, _ string, event EventHandler, _ AskHandler) error {
	workspace := a.runtime.containers[a.container].workspace
	if workspace == "" || !filepath.IsAbs(workspace) {
		return fmt.Errorf("runner test adapter requires an absolute workspace: %q", workspace)
	}
	a.runtime.lifecycle = append(a.runtime.lifecycle, "agent:"+a.container+" workspace="+workspace)
	if err := os.WriteFile(filepath.Join(workspace, "answer.txt"), []byte("42\n"), 0o644); err != nil {
		return err
	}
	event(Record{"type": "agent_start", "agent_id": "main"})
	event(Record{"type": "message", "text": "provider-key local-gateway-token"})
	event(Record{"type": "agent_end", "agent_id": "main"})
	return a.failure
}

type runnerTestQuestionAdapter struct {
	runtime   *runnerTestRuntime
	container string
	store     *Store
}

func (a runnerTestQuestionAdapter) Run(_ context.Context, _, _ string, event EventHandler, ask AskHandler) error {
	event(Record{"type": "agent_start", "agent_id": "main"})
	event(Record{"type": "agent_wait", "agent_id": "main"})
	answer, err := ask(context.Background(), "Continue?", Record{"method": "confirm", "agent_id": "main"})
	if err != nil {
		return err
	}
	if answer != true {
		return fmt.Errorf("typed confirmation was lost: %#v", answer)
	}
	event(Record{"type": "agent_resume", "agent_id": "main"})
	workspace := a.runtime.containers[a.container].workspace
	if workspace == "" || !filepath.IsAbs(workspace) {
		return fmt.Errorf("runner test question adapter requires an absolute workspace: %q", workspace)
	}
	if err := os.WriteFile(filepath.Join(workspace, "answer.txt"), []byte("42\n"), 0o644); err != nil {
		return err
	}
	event(Record{"type": "agent_end", "agent_id": "main"})
	return nil
}

type runnerTestBlockedAdapter struct{}

func (runnerTestBlockedAdapter) Run(ctx context.Context, _, _ string, event EventHandler, _ AskHandler) error {
	event(Record{"type": "agent_start", "agent_id": "main"})
	<-ctx.Done()
	return ctx.Err()
}

type runnerTestFailureAdapter struct{ message string }

func (a runnerTestFailureAdapter) Run(_ context.Context, _, _ string, event EventHandler, _ AskHandler) error {
	event(Record{"type": "agent_start", "agent_id": "main"})
	return errors.New(a.message)
}

type runnerTestManualStopAdapter struct {
	runtime   *runnerTestRuntime
	container string
	store     *Store
}

func (a runnerTestManualStopAdapter) Run(_ context.Context, _, _ string, event EventHandler, _ AskHandler) error {
	event(Record{"type": "agent_start", "agent_id": "main"})
	workspace := a.runtime.containers[a.container].workspace
	if workspace == "" || !filepath.IsAbs(workspace) {
		return fmt.Errorf("runner test stop adapter requires an absolute workspace: %q", workspace)
	}
	if err := os.WriteFile(filepath.Join(workspace, "answer.txt"), []byte("42\n"), 0o644); err != nil {
		return err
	}
	rows, err := a.store.List()
	if err != nil || len(rows) == 0 {
		return fmt.Errorf("read run record for manual stop: %w", err)
	}
	if err := a.store.Stop(text(rows[0]["id"])); err != nil {
		return err
	}
	event(Record{"type": "agent_end", "agent_id": "main"})
	return nil
}

func runnerTestCampaign(hidden string) Record {
	return Record{
		"concurrency": 1, "trials": 1, "limits": Record{}, "models": []any{"vendor/model"},
		"agents":    []any{Record{"id": "pi", "kind": "pi"}},
		"harnesses": []any{Record{"id": "plain", "mode": "explicit", "files": []any{}}},
		"tasks": []any{Record{
			"id": "answer", "prompt": "Write answer.txt", "image": "fixture", "attachments": []any{}, "setup": []any{},
			"evaluation": Record{"image": "fixture-eval", "files": []any{Record{"source": hidden, "target": "hidden.py"}},
				"timeout_seconds": 1, "tests": []any{Record{"id": "answer", "score": 5, "command": []any{"python", "/evaluation/hidden.py"}}}},
		}},
	}
}

func runnerTestOptions(runtime *runnerTestRuntime, adapter AgentAdapter) RunOptions {
	return RunOptions{
		APIKey:         "provider-key",
		RuntimeFactory: func() (Runtime, error) { return runtime, nil },
		GatewayFactory: func(string, *float64) (RunGateway, error) {
			return RunGateway{Token: "local-gateway-token", URL: "http://0.0.0.0:1234/v1",
				Attempts: func() []Record {
					return []Record{{"attempt_id": "one", "input_tokens": 2, "output_tokens": 3, "cost_usd": 0.01}}
				},
				LimitReached: func() bool { return false }, InfrastructureFailed: func() bool { return false }, Close: func() error { return nil }}, nil
		},
		AdapterFactory: func(rt Runtime, container string, _ []string, _ string) AgentAdapter {
			runtime := rt.(*runnerTestRuntime)
			switch current := adapter.(type) {
			case runnerTestAdapter:
				current.runtime, current.container = runtime, container
				return current
			case runnerTestQuestionAdapter:
				current.runtime, current.container = runtime, container
				return current
			case runnerTestManualStopAdapter:
				current.runtime, current.container = runtime, container
				return current
			default:
				return adapter
			}
		},
		Notify: func(string) {},
	}
}

func TestRunCampaignLifecycleAndRedaction(t *testing.T) {
	temp := t.TempDir()
	hidden := filepath.Join(temp, "hidden.py")
	if err := os.WriteFile(hidden, []byte("assert True\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(temp, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := newRunnerTestRuntime()
	options := runnerTestOptions(runtime, runnerTestAdapter{runtime: runtime})
	results, err := RunCampaign(context.Background(), runnerTestCampaign(hidden), store, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || text(results[0]["status"]) != "completed" || number(object(results[0]["evaluation"])["earned_score"]) != 5 {
		t.Fatalf("unexpected campaign result: %#v; commands=%v lifecycle=%v", results, runtime.execs, runtime.lifecycle)
	}
	if text(results[0]["input_sha256"]) == "" || number(object(results[0]["metrics"])["request_count"]) != 1 {
		t.Fatalf("missing frozen input identity or usage metrics: %#v", results[0])
	}
	if _, exists := results[0]["status_before_evaluation"]; exists {
		t.Fatalf("completed runs should not retain evaluation transition state: %#v", results[0])
	}
	runDir, err := store.RunDir(text(results[0]["id"]))
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := os.ReadFile(filepath.Join(runDir, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transcript), "provider-key") || strings.Contains(string(transcript), "local-gateway-token") {
		t.Fatalf("event secrets were not redacted: %s", transcript)
	}
	if strings.Contains(string(transcript), "hidden.py") {
		t.Fatalf("hidden evaluation file leaked into the agent transcript: %s", transcript)
	}
	resultBytes, err := os.ReadFile(filepath.Join(runDir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(resultBytes), "provider-key") || strings.Contains(string(resultBytes), "local-gateway-token") {
		t.Fatalf("result artifact secrets were not redacted: %s", resultBytes)
	}
	requests, err := os.ReadFile(filepath.Join(runDir, "requests.json"))
	if err != nil || strings.Contains(string(requests), "provider-key") || strings.Contains(string(requests), "local-gateway-token") {
		t.Fatalf("request artifact missing or contains secrets: %s (%v)", requests, err)
	}
	if !strings.Contains(strings.Join(stringsOf(results[0]["warnings"]), " "), "Docker API copy transport") {
		t.Fatalf("copy transport warning missing: %#v", results[0]["warnings"])
	}
	if !fileExists(filepath.Join(runDir, "workspace", "answer.txt")) || !fileExists(filepath.Join(runDir, "changes.diff")) {
		t.Fatal("workspace or diff artifact was not retained")
	}
	if !(indexLifecycle(runtime.lifecycle, "stop:") < indexLifecycle(runtime.lifecycle, "export:") && indexLifecycle(runtime.lifecycle, "export:") < indexLifecycle(runtime.lifecycle, "remove:")) {
		t.Fatalf("runtime lifecycle order was not stop, export, remove: %v", runtime.lifecycle)
	}
	if !(indexLifecycle(runtime.lifecycle, "remove:") < indexLifecycle(runtime.lifecycle, "start-evaluation:") && indexLifecycle(runtime.lifecycle, "start-evaluation:") < indexLifecycle(runtime.lifecycle, "close")) {
		t.Fatalf("evaluation/close lifecycle order changed: %v", runtime.lifecycle)
	}
}

func TestRunCampaignExportFailureSkipsEvaluation(t *testing.T) {
	temp := t.TempDir()
	hidden := filepath.Join(temp, "hidden.py")
	if err := os.WriteFile(hidden, []byte("assert True\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(temp, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := newRunnerTestRuntime()
	runtime.exportErr = errors.New("copy-back failed")
	results, err := RunCampaign(context.Background(), runnerTestCampaign(hidden), store, runnerTestOptions(runtime, runnerTestAdapter{runtime: runtime}))
	if err != nil {
		t.Fatal(err)
	}
	if text(results[0]["status"]) != "infrastructure_failed" || results[0]["evaluation"] != nil || results[0]["infrastructure_incidents"] != true {
		t.Fatalf("copy-back failure must exclude evaluation: %#v", results[0])
	}
}

func TestRunCampaignSetupStopStillEvaluates(t *testing.T) {
	temp := t.TempDir()
	hidden := filepath.Join(temp, "hidden.py")
	if err := os.WriteFile(hidden, []byte("assert True\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(temp, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := newRunnerTestRuntime()
	runtime.stopStore = store
	campaign := runnerTestCampaign(hidden)
	records(campaign["tasks"])[0]["setup"] = []any{[]any{"wait-for-stop"}}
	results, err := RunCampaign(context.Background(), campaign, store, runnerTestOptions(runtime, runnerTestAdapter{runtime: runtime}))
	if err != nil {
		t.Fatal(err)
	}
	if text(results[0]["status"]) != "incomplete" || results[0]["evaluation"] == nil || number(object(results[0]["evaluation"])["earned_score"]) != 0 {
		t.Fatalf("setup stop must retain evaluation: %#v", results[0])
	}
	if len(runtime.execs) < 3 || !strings.Contains(runtime.execs[0], "wait-for-stop") {
		t.Fatalf("setup command was not executed: %v", runtime.execs)
	}
}

func TestRunCampaignSetupFailureSkipsEvaluation(t *testing.T) {
	temp := t.TempDir()
	hidden := filepath.Join(temp, "hidden.py")
	if err := os.WriteFile(hidden, []byte("assert True\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(temp, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := newRunnerTestRuntime()
	campaign := runnerTestCampaign(hidden)
	records(campaign["tasks"])[0]["setup"] = []any{[]any{"fail-setup"}}
	results, err := RunCampaign(context.Background(), campaign, store, runnerTestOptions(runtime, runnerTestAdapter{runtime: runtime}))
	if err != nil {
		t.Fatal(err)
	}
	if text(results[0]["status"]) != "infrastructure_failed" || results[0]["evaluation"] != nil {
		t.Fatalf("failed setup should exclude evaluation: %#v", results[0])
	}
}

func TestRunCampaignManualStopStillEvaluatesWorkspace(t *testing.T) {
	temp := t.TempDir()
	hidden := filepath.Join(temp, "hidden.py")
	if err := os.WriteFile(hidden, []byte("assert True\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(temp, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := newRunnerTestRuntime()
	options := runnerTestOptions(runtime, runnerTestManualStopAdapter{runtime: runtime, store: store})
	results, err := RunCampaign(context.Background(), runnerTestCampaign(hidden), store, options)
	if err != nil {
		t.Fatal(err)
	}
	if text(results[0]["status"]) != "incomplete" || text(results[0]["stop_reason"]) != "manual_stop" || number(object(results[0]["evaluation"])["earned_score"]) != 5 {
		t.Fatalf("manual stop must keep and evaluate the workspace: %#v", results[0])
	}
}

func TestRunCampaignNetworkFailureRetainsUnrankedEvaluationAndProviderErrors(t *testing.T) {
	temp := t.TempDir()
	hidden := filepath.Join(temp, "hidden.py")
	if err := os.WriteFile(hidden, []byte("assert True\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(temp, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := newRunnerTestRuntime()
	options := runnerTestOptions(runtime, runnerTestFailureAdapter{message: "provider-key transport failed local-gateway-token"})
	options.GatewayFactory = func(string, *float64) (RunGateway, error) {
		return RunGateway{Token: "local-gateway-token", URL: "http://0.0.0.0:1234/v1",
			Attempts: func() []Record {
				return []Record{{"attempt_id": "network", "status": 502, "model": "vendor/model", "error_kind": "network", "provider_error_body": "network detail provider-key local-gateway-token"}}
			},
			LimitReached: func() bool { return false }, InfrastructureFailed: func() bool { return true }, Close: func() error { return nil }}, nil
	}
	results, err := RunCampaign(context.Background(), runnerTestCampaign(hidden), store, options)
	if err != nil {
		t.Fatal(err)
	}
	result := results[0]
	if text(result["status"]) != "infrastructure_failed" || result["evaluation"] == nil || number(object(result["evaluation"])["earned_score"]) != 0 {
		t.Fatalf("network failure should preserve a zero-score evaluation: %#v", result)
	}
	if !result["infrastructure_incidents"].(bool) || number(object(result["metrics"])["request_count"]) != 1 {
		t.Fatalf("network incident or observed request was lost: %#v", result)
	}
	providerErrors := records(result["provider_errors"])
	if len(providerErrors) != 1 || text(providerErrors[0]["body"]) != "network detail [redacted] [redacted]" {
		t.Fatalf("provider diagnostics should propagate with both secrets redacted: %#v", result["provider_errors"])
	}
	if strings.Contains(text(result["error"]), "provider-key") || strings.Contains(text(result["error"]), "local-gateway-token") {
		t.Fatalf("run error was not redacted: %#v", result["error"])
	}
}

func TestRunCampaignProviderRejectionIsAgentFailure(t *testing.T) {
	temp := t.TempDir()
	hidden := filepath.Join(temp, "hidden.py")
	if err := os.WriteFile(hidden, []byte("assert True\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(temp, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := newRunnerTestRuntime()
	options := runnerTestOptions(runtime, runnerTestFailureAdapter{message: "provider-key rejected local-gateway-token"})
	options.GatewayFactory = func(string, *float64) (RunGateway, error) {
		return RunGateway{Token: "local-gateway-token", URL: "http://0.0.0.0:1234/v1",
			Attempts: func() []Record {
				return []Record{{"attempt_id": "rejected", "status": 404, "model": "vendor/model", "provider_error_body": "guardrail provider-key local-gateway-token"}}
			},
			LimitReached: func() bool { return false }, InfrastructureFailed: func() bool { return false }, Close: func() error { return nil }}, nil
	}
	results, err := RunCampaign(context.Background(), runnerTestCampaign(hidden), store, options)
	if err != nil {
		t.Fatal(err)
	}
	result := results[0]
	if text(result["status"]) != "agent_failed" || result["evaluation"] == nil {
		t.Fatalf("provider rejection should retain evaluator evidence: %#v", result)
	}
	providerErrors := records(result["provider_errors"])
	if len(providerErrors) != 1 || text(providerErrors[0]["body"]) != "guardrail [redacted] [redacted]" {
		t.Fatalf("provider diagnostics should be exposed only in redacted form: %#v", result["provider_errors"])
	}
}

func TestRunCampaignHumanRepliesAreTypedAndWaitIsExcluded(t *testing.T) {
	temp := t.TempDir()
	hidden := filepath.Join(temp, "hidden.py")
	if err := os.WriteFile(hidden, []byte("assert True\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(temp, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := newRunnerTestRuntime()
	options := runnerTestOptions(runtime, runnerTestQuestionAdapter{store: store, runtime: runtime})
	options.Notify = func(message string) {
		var notification Record
		if err := json.Unmarshal([]byte(message), &notification); err != nil {
			t.Errorf("decode question notification: %v", err)
			return
		}
		time.Sleep(35 * time.Millisecond)
		if ok, err := store.Answer(text(notification["run_id"]), text(notification["request_id"]), "yes"); err != nil || !ok {
			t.Errorf("store human answer: ok=%v err=%v", ok, err)
		}
	}
	results, err := RunCampaign(context.Background(), runnerTestCampaign(hidden), store, options)
	if err != nil {
		t.Fatal(err)
	}
	result := results[0]
	waitSeconds := number(object(result["timing"])["human_wait_seconds"])
	if number(result["human_interventions"]) != 1 || waitSeconds < 0.025 {
		t.Fatalf("human reply count or wait timing was lost: %#v", result)
	}
	if number(object(result["timing"])["agent_seconds"]) >= waitSeconds {
		t.Fatalf("human wait was included in active agent time: %#v", result["timing"])
	}
	if text(result["status"]) != "completed" || number(object(result["evaluation"])["earned_score"]) != 5 {
		t.Fatalf("typed confirmation did not complete the trial: %#v", result)
	}
}

func TestRunCampaignAgentAndCostLimitsStopAndEvaluate(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		limit     string
		costLimit bool
		reason    string
	}{
		{name: "agent time", limit: "agent_seconds", reason: "agent_time_limit"},
		{name: "gateway cost", limit: "cost_usd", costLimit: true, reason: "cost_limit"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			temp := t.TempDir()
			hidden := filepath.Join(temp, "hidden.py")
			if err := os.WriteFile(hidden, []byte("assert True\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := NewStore(filepath.Join(temp, "artifacts"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			runtime := newRunnerTestRuntime()
			options := runnerTestOptions(runtime, runnerTestBlockedAdapter{})
			campaign := runnerTestCampaign(hidden)
			limits := object(campaign["limits"])
			if scenario.limit == "agent_seconds" {
				limits[scenario.limit] = 0.01
			} else {
				limits[scenario.limit] = 0.01
			}
			if scenario.costLimit {
				options.GatewayFactory = func(string, *float64) (RunGateway, error) {
					return RunGateway{Token: "local-gateway-token", URL: "http://0.0.0.0:1234/v1",
						Attempts: func() []Record { return nil }, LimitReached: func() bool { return true },
						InfrastructureFailed: func() bool { return false }, Close: func() error { return nil }}, nil
				}
			}
			results, err := RunCampaign(context.Background(), campaign, store, options)
			if err != nil {
				t.Fatal(err)
			}
			result := results[0]
			if text(result["status"]) != "incomplete" || text(result["stop_reason"]) != scenario.reason || result["evaluation"] == nil {
				t.Fatalf("limit should stop work and retain evaluation: %#v", result)
			}
		})
	}
}

func TestRunCampaignDoesNotMutateManifestAndUsesPythonInputHash(t *testing.T) {
	temp := t.TempDir()
	hidden := filepath.Join(temp, "hidden.py")
	if err := os.WriteFile(hidden, []byte("assert True\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	campaign := runnerTestCampaign(hidden)
	before, err := json.Marshal(campaign)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(temp, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runtime := newRunnerTestRuntime()
	if _, err := RunCampaign(context.Background(), campaign, store, runnerTestOptions(runtime, runnerTestAdapter{runtime: runtime})); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(campaign)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("runner mutated its caller's campaign:\nbefore=%s\nafter=%s", before, after)
	}
	hash, err := pythonJSONHash(Record{"files": Record{"café.txt": Record{"kind": "file", "sha256": "abc", "executable": false}},
		"commit": nil, "prompt": "Write <answer> 🐈"})
	if err != nil {
		t.Fatal(err)
	}
	if hash != "a668bc11bdee36d17970b737e13108fe4711f6845d78872a73782be28998a73c" {
		t.Fatalf("input hash does not match Python sorted-ASCII JSON: %s", hash)
	}
}

func TestRunnerTestAdapterRejectsMissingWorkspaceWithoutPackageWrite(t *testing.T) {
	runtime := newRunnerTestRuntime()
	adapter := runnerTestAdapter{runtime: runtime, container: "missing"}
	if err := adapter.Run(context.Background(), "prompt", "model", func(Record) {}, func(context.Context, string, Record) (any, error) { return nil, nil }); err == nil {
		t.Fatal("adapter accepted an absent workspace")
	}
	if _, err := os.Stat("answer.txt"); !os.IsNotExist(err) {
		t.Fatalf("runner test adapter left a worktree answer.txt behind: %v", err)
	}
}

func indexLifecycle(events []string, prefix string) int {
	for i, event := range events {
		if strings.HasPrefix(event, prefix) {
			return i
		}
	}
	return len(events)
}
