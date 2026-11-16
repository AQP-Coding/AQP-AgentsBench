package bench

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AQP-Coding/AQP-AgentsBench/aqp_bench/example"
)

// demoAPIKey is a deliberately non-secret credential used only with the mock provider.
const demoAPIKey = "offline-demo-not-a-real-key"

// demoWarning labels persisted simulation results so they are not agent-compatibility evidence.
const demoWarning = "Offline simulation: no Docker, real agent, harness, or paid provider was exercised."

type demoContainer struct {
	workspace string
	options   StartOptions
}

// demoRuntime is private to RunDemo. It can execute only the embedded trusted
// checker; it is not a sandbox for user manifests or arbitrary commands.
type demoRuntime struct {
	mu         sync.Mutex
	containers map[string]demoContainer
	next       int
	python     string
}

func newDemoRuntime() (*demoRuntime, error) {
	python, err := exec.LookPath("python3")
	if err != nil {
		return nil, errors.New("Offline demo requires host python3 to run its preserved trusted checker")
	}
	return &demoRuntime{containers: map[string]demoContainer{}, python: python}, nil
}

// Start registers a simulated container after checking ctx; image is ignored.
// Workspace and options are retained for the trusted fixture, not mounted by Docker.
func (r *demoRuntime) Start(ctx context.Context, _ string, workspace string, options StartOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	id := fmt.Sprintf("demo-%d", r.next)
	r.containers[id] = demoContainer{workspace: workspace, options: options}
	return id, nil
}

// Exec accepts only the trusted fixture verifier and its preflight commands.
// It never grants general host command execution to a campaign.
func (r *demoRuntime) Exec(ctx context.Context, id string, command []string, _ time.Duration) (ExecResult, error) {
	r.mu.Lock()
	container, exists := r.containers[id]
	r.mu.Unlock()
	if !exists {
		return ExecResult{}, fmt.Errorf("Unknown offline demo container")
	}
	if len(command) >= 5 && command[0] == "sh" && command[1] == "-c" && command[3] == "aqp-evaluation-preflight" {
		for _, name := range command[4:] {
			if !strings.HasPrefix(name, "/evaluation/") || strings.Contains(name, "..") {
				return ExecResult{}, errors.New("Offline demo executes only its fixed trusted evaluator")
			}
			path := filepath.Join(container.options.Evaluation, filepath.FromSlash(strings.TrimPrefix(name, "/evaluation/")))
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o444 == 0 {
				return ExecResult{ReturnCode: 1, Stderr: "missing or unreadable: " + name + "\n"}, nil
			}
		}
		return ExecResult{}, nil
	}
	if len(command) != 3 || command[0] != "python" || command[1] != "/evaluation/check.py" || command[2] != "total" && command[2] != "format" {
		return ExecResult{}, errors.New("Offline demo executes only its fixed trusted evaluator")
	}
	trusted, err := example.FS.ReadFile("check.py")
	if err != nil {
		return ExecResult{}, fmt.Errorf("read embedded demo checker: %w", err)
	}
	checkerPath := filepath.Join(container.options.Evaluation, "check.py")
	actual, err := os.ReadFile(checkerPath)
	if err != nil || !bytes.Equal(actual, trusted) {
		return ExecResult{}, errors.New("Offline demo checker does not match the embedded trusted fixture")
	}
	cmd := exec.CommandContext(ctx, r.python, checkerPath, command[2])
	cmd.Dir = container.workspace
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err == nil {
		return ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}, nil
	}
	if ctx.Err() != nil {
		return ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ExecResult{ReturnCode: exitErr.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}, nil
	}
	return ExecResult{}, fmt.Errorf("run embedded demo checker: %w", err)
}

// ExecAs uses the same trusted host execution as Exec; user isolation is not
// provided by the offline simulator.
func (r *demoRuntime) ExecAs(ctx context.Context, id, _ string, command []string, timeout time.Duration) (ExecResult, error) {
	return r.Exec(ctx, id, command, timeout)
}

// Spawn rejects process launching because the offline fixture does not run an agent.
func (r *demoRuntime) Spawn(context.Context, string, []string, bool) (Process, error) {
	return nil, errors.New("Offline demo does not spawn agent processes")
}

// Address rejects network address discovery because no container exists.
func (r *demoRuntime) Address(string) (string, error) {
	return "", errors.New("Offline demo has no container network address")
}

// TransportFor returns bind semantics because the trusted fixture shares host files.
func (r *demoRuntime) TransportFor(string) string { return "bind" }

// ExportWorkspace needs no copy-back because the trusted fixture already uses host files.
func (r *demoRuntime) ExportWorkspace(context.Context, string, string) error { return nil }

// Stop ends the simulated container lifecycle without contacting Docker.
func (r *demoRuntime) Stop(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.containers[id]; !ok {
		return fmt.Errorf("Unknown offline demo container")
	}
	return nil
}

// Remove releases the simulated container record without deleting its workspace.
func (r *demoRuntime) Remove(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.containers[id]; !ok {
		return fmt.Errorf("Unknown offline demo container")
	}
	delete(r.containers, id)
	return nil
}

type demoAdapter struct {
	runtime   *demoRuntime
	container string
}

// Run derives the fixed report through the loopback mock gateway and emits
// simulated lifecycle events. Unlike real adapters, it requires a non-nil onEvent.
func (a *demoAdapter) Run(ctx context.Context, _ string, model string, onEvent EventHandler, _ AskHandler) error {
	a.runtime.mu.Lock()
	container, exists := a.runtime.containers[a.container]
	a.runtime.mu.Unlock()
	if !exists {
		return errors.New("Offline demo container is unavailable")
	}
	onEvent(Record{"type": "agent_start", "agent_id": "main"})
	file, err := os.Open(filepath.Join(container.workspace, "data", "numbers.csv"))
	if err != nil {
		return fmt.Errorf("read offline demo CSV fixture: %w", err)
	}
	reader := csv.NewReader(file)
	rows, readErr := reader.ReadAll()
	closeErr := file.Close()
	if readErr != nil {
		return fmt.Errorf("read offline demo CSV fixture: %w", readErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if len(rows) < 2 || len(rows[0]) != 1 || rows[0][0] != "value" {
		return errors.New("Offline demo CSV fixture has an unsupported format")
	}
	total := 0
	for _, row := range rows[1:] {
		if len(row) != 1 {
			return errors.New("Offline demo CSV fixture has an unsupported format")
		}
		value, err := strconv.Atoi(row[0])
		if err != nil {
			return fmt.Errorf("parse offline demo CSV fixture: %w", err)
		}
		total += value
	}
	configPath := filepath.Join(container.options.Home, ".pi", "agent", "models.json")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read offline demo gateway configuration: %w", err)
	}
	var config Record
	if err := decodeJSON(configBytes, &config); err != nil {
		return fmt.Errorf("read offline demo gateway configuration: %w", err)
	}
	baseURL := text(object(object(config["providers"])["aqp"])["baseUrl"])
	baseURL = strings.ReplaceAll(baseURL, "host.docker.internal", "127.0.0.1")
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") && !strings.HasPrefix(baseURL, "http://localhost:") {
		return errors.New("Offline demo gateway must use a loopback address")
	}
	token := container.options.Environment["AQP_GATEWAY_TOKEN"]
	if token == "" {
		return errors.New("Offline demo gateway capability is missing")
	}
	requestBody, err := json.Marshal(Record{"model": model, "messages": []any{Record{"role": "user", "content": strconv.Itoa(total)}}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-AQP-Agent-ID", "main")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("offline demo mock gateway request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("offline demo mock gateway returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if err != nil {
		return err
	}
	var reply Record
	if err := decodeJSON(data, &reply); err != nil {
		return fmt.Errorf("decode offline demo mock response: %w", err)
	}
	choices := records(reply["choices"])
	if len(choices) != 1 {
		return errors.New("Offline demo mock response must include exactly one choice")
	}
	answer := text(object(choices[0]["message"])["content"])
	if answer == "" {
		return errors.New("Offline demo mock response omitted its answer")
	}
	if err := os.WriteFile(filepath.Join(container.workspace, "report.txt"), []byte(answer+"\n"), 0o644); err != nil {
		return err
	}
	onEvent(Record{"type": "message", "agent_id": "main", "text": "Offline simulation wrote report.txt"})
	onEvent(Record{"type": "agent_end", "agent_id": "main"})
	return nil
}

// RunDemo executes only the unchanged embedded campaign, CSV, and checker.
// Its mock provider and gateway are loopback-only; no Docker/agent/provider call
// is made. The retained run is marked as a simulation after runner persistence.
func RunDemo(ctx context.Context, store *Store) (Record, error) {
	if store == nil {
		return nil, errors.New("A campaign store is required")
	}
	runtime, err := newDemoRuntime()
	if err != nil {
		return nil, err
	}
	fixtureDir, err := os.MkdirTemp("", "aqp-bench-demo-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(fixtureDir)
	for _, name := range []string{"campaign.json", "numbers.csv", "check.py"} {
		data, err := example.FS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read embedded offline demo fixture %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, name), data, 0o644); err != nil {
			return nil, err
		}
	}
	campaign, err := LoadCampaign(filepath.Join(fixtureDir, "campaign.json"))
	if err != nil {
		return nil, err
	}
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+demoAPIKey {
			http.Error(w, "mock provider key required", http.StatusUnauthorized)
			return
		}
		var payload Record
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		messages := array(payload["messages"])
		if len(messages) == 0 {
			http.Error(w, "messages are required", http.StatusBadRequest)
			return
		}
		content := object(messages[len(messages)-1])["content"]
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Record{"id": "demo-generation", "model": "demo/offline",
			"choices": []any{Record{"message": Record{"content": content}}},
			"usage":   Record{"prompt_tokens": 10, "completion_tokens": 2, "cost": 0}})
	}))
	defer provider.Close()
	results, err := RunCampaign(ctx, campaign, store, RunOptions{
		APIKey:         demoAPIKey,
		RuntimeFactory: func() (Runtime, error) { return runtime, nil },
		GatewayFactory: func(key string, cost *float64) (RunGateway, error) {
			gateway, err := NewGateway(key, GatewayOptions{Host: "127.0.0.1", Upstream: provider.URL + "/api/v1", CostLimit: cost})
			if err != nil {
				return RunGateway{}, err
			}
			return RunGateway{Token: gateway.Token, URL: gateway.BaseURL(), Attempts: gateway.Attempts,
				LimitReached: gateway.LimitReached, InfrastructureFailed: gateway.InfrastructureFailed, Close: gateway.Close}, nil
		},
		AdapterFactory: func(_ Runtime, container string, _ []string, kind string) AgentAdapter {
			if kind != "pi" {
				return demoRejectAdapter{message: "Offline demo supports only its fixed Pi-protocol simulation"}
			}
			return &demoAdapter{runtime: runtime, container: container}
		},
		Notify: func(string) {},
	})
	if err != nil {
		return nil, err
	}
	if len(results) != 1 {
		return nil, fmt.Errorf("Offline demo expected one run, got %d", len(results))
	}
	if count := requests.Load(); count != 1 {
		return nil, fmt.Errorf("Offline demo expected exactly one mock provider request, got %d", count)
	}
	result := results[0]
	result["simulation"] = true
	warnings := array(result["warnings"])
	warnings = append(warnings, demoWarning)
	result["warnings"] = warnings
	if err := store.Save(result); err != nil {
		return nil, err
	}
	return result, nil
}

type demoRejectAdapter struct{ message string }

// Run rejects an unsupported demo adapter instead of launching a real agent.
func (a demoRejectAdapter) Run(context.Context, string, string, EventHandler, AskHandler) error {
	return errors.New(a.message)
}
