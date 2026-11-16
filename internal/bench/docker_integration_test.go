package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

const dockerIntegrationImage = "aqp-agentsbench/pi-gentle-python:0.1.0"

func TestDockerIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Docker integration requires an explicit non-short run")
	}
	if os.Getenv("AQP_DOCKER_INTEGRATION") != "1" {
		t.Skip("set AQP_DOCKER_INTEGRATION=1 to use the local Docker daemon")
	}

	t.Run("bind", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
		runtime, err := NewDockerRuntime()
		if err != nil {
			t.Fatalf("create local Docker runtime: %v", err)
		}
		t.Cleanup(func() { _ = runtime.Close() })
		dockerIntegrationExercise(t, runtime, dockerIntegrationImage, "bind", nil)
	})

	t.Run("forced-copy-fallback", func(t *testing.T) {
		roundTripper := newDockerIntegrationRoundTripper(t, true)
		transport := &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
			},
		}
		roundTripper.base = transport
		httpClient := &http.Client{Transport: roundTripper}
		engine, err := client.New(client.WithHost("unix:///var/run/docker.sock"), client.WithHTTPClient(httpClient))
		if err != nil {
			t.Fatalf("create SDK client for local Docker socket: %v", err)
		}
		runtime := newDockerRuntimeWithClient(engine)
		t.Cleanup(func() { _ = runtime.Close() })
		dockerIntegrationExercise(t, runtime, dockerIntegrationImage, "docker_cp", roundTripper)
	})
}

func dockerIntegrationExercise(t *testing.T, runtime *DockerRuntime, image, expectedTransport string, roundTripper *dockerIntegrationRoundTripper) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	home := filepath.Join(root, "home")
	evaluation := filepath.Join(root, "evaluation")
	for _, dir := range []string{workspace, home, evaluation} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(workspace, "seed.txt"), "workspace-seed\n", 0o600)
	write(filepath.Join(workspace, "remove-me.txt"), "delete-me\n", 0o600)
	write(filepath.Join(home, "settings.txt"), "private-home\n", 0o600)
	evaluatorPath := filepath.Join(evaluation, "evaluator.txt")
	write(evaluatorPath, "private-evaluator\n", 0o444)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	identity := ""
	defer func() {
		cancel()
		identities := []string{}
		if roundTripper != nil {
			identities = roundTripper.createdIDs()
		}
		if identity != "" {
			found := false
			for _, existing := range identities {
				if existing == identity {
					found = true
					break
				}
			}
			if !found {
				identities = append(identities, identity)
			}
		}
		dockerIntegrationCleanup(t, runtime, identities)
	}()
	var err error
	identity, err = runtime.Start(ctx, image, workspace, StartOptions{
		Home: home, Evaluation: evaluation, CopyBack: true,
		Environment: map[string]string{"AQP_DOCKER_INTEGRATION_VALUE": "present"},
	})
	if err != nil {
		t.Fatalf("start local fixture container: %v", err)
	}
	t.Logf("created fixture container id=%s expected_transport=%s", identity, expectedTransport)
	if got := runtime.TransportFor(identity); got != expectedTransport {
		if expectedTransport == "bind" && got == "docker_cp" {
			t.Logf("real bind visibility probe was unavailable; runtime selected docker_cp")
			t.Skip("local Docker daemon cannot read the test workspace through a bind mount; forced copy fallback is tested separately")
		}
		t.Fatalf("Docker transport = %q, want %q", got, expectedTransport)
	}
	if roundTripper != nil {
		if got := roundTripper.forcedProbeFailures(); got != 1 {
			t.Fatalf("forced bind-probe failures = %d, want 1", got)
		}
		ids := roundTripper.createdIDs()
		if len(ids) != 2 || ids[1] != identity {
			t.Fatalf("forced fallback created container ids = %v, want discarded bind container then %s", ids, identity)
		}
		if err := dockerIntegrationAssertRemoved(ctx, runtime, ids[0]); err != nil {
			t.Fatalf("first bind-probe container was not discarded: %v", err)
		}
		t.Logf("forced bind probe failure; copy-fallback created ids=%v; discarded first container verified", ids)
	}

	result, err := runtime.Exec(ctx, identity, []string{"sh", "-c", "printf 'stdout-value\\n'; printf 'stderr-value\\n' >&2; exit 23"}, 10*time.Second)
	if err != nil {
		t.Fatalf("Docker exec: %v", err)
	}
	if result.ReturnCode != 23 || result.Stdout != "stdout-value\n" || result.Stderr != "stderr-value\n" {
		t.Fatalf("Docker exec streams/status = (%d, %q, %q)", result.ReturnCode, result.Stdout, result.Stderr)
	}
	started := time.Now()
	if _, err := runtime.Exec(ctx, identity, []string{"sh", "-c", "exec sleep 60"}, 300*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("long Docker exec timeout error = %v, want context deadline", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("long Docker exec did not terminate promptly: %s", time.Since(started))
	}
	if result, err := runtime.Exec(ctx, identity, []string{"true"}, 5*time.Second); err != nil || result.ReturnCode != 0 {
		t.Fatalf("container unavailable after timed-out exec: result=%+v err=%v", result, err)
	}
	result, err = runtime.Exec(ctx, identity, []string{"sh", "-c", "test \"$HOME\" = /home/agent && test \"$AQP_DOCKER_INTEGRATION_VALUE\" = present && cat /workspace/seed.txt && cat /home/agent/settings.txt && cat /evaluation/evaluator.txt"}, 10*time.Second)
	if err != nil || result.ReturnCode != 0 || !strings.Contains(result.Stdout, "workspace-seed") || !strings.Contains(result.Stdout, "private-home") || !strings.Contains(result.Stdout, "private-evaluator") {
		t.Fatalf("mounted workspace/home/evaluation or environment unavailable: result=%+v err=%v", result, err)
	}
	result, err = runtime.Exec(ctx, identity, []string{"sh", "-c", "printf 'forbidden' >> /evaluation/evaluator.txt"}, 10*time.Second)
	if err != nil || result.ReturnCode == 0 {
		t.Fatalf("read-only evaluation mount write result=%+v err=%v; want nonzero exit", result, err)
	}
	if body, err := os.ReadFile(evaluatorPath); err != nil || string(body) != "private-evaluator\n" {
		t.Fatalf("host evaluator changed after container write attempt: body=%q err=%v", body, err)
	}

	spawnCtx, cancelSpawn := context.WithTimeout(ctx, 20*time.Second)
	defer cancelSpawn()
	process, err := runtime.Spawn(spawnCtx, identity, []string{"sh", "-c", "IFS= read -r line; printf 'spawn:%s\\n' \"$line\"; printf 'spawn-stderr\\n' >&2"}, true)
	if err != nil {
		t.Fatalf("spawn stdin/stdout/stderr fixture: %v", err)
	}
	if _, err := io.WriteString(process.Stdin(), "input-line\n"); err != nil {
		t.Fatalf("write process stdin: %v", err)
	}
	if err := process.Stdin().Close(); err != nil {
		t.Fatalf("close process stdin: %v", err)
	}
	type streamResult struct {
		body []byte
		err  error
	}
	stdoutCh, stderrCh := make(chan streamResult, 1), make(chan streamResult, 1)
	go func() { body, err := io.ReadAll(process.Stdout()); stdoutCh <- streamResult{body: body, err: err} }()
	go func() { body, err := io.ReadAll(process.Stderr()); stderrCh <- streamResult{body: body, err: err} }()
	var stdoutResult, stderrResult streamResult
	select {
	case stdoutResult = <-stdoutCh:
	case <-time.After(8 * time.Second):
		t.Fatal("timed out reading spawned process stdout")
	}
	select {
	case stderrResult = <-stderrCh:
	case <-time.After(8 * time.Second):
		t.Fatal("timed out reading spawned process stderr")
	}
	if stdoutResult.err != nil || stderrResult.err != nil {
		t.Fatalf("read process streams: stdout=%v stderr=%v", stdoutResult.err, stderrResult.err)
	}
	stdout, stderr := stdoutResult.body, stderrResult.body
	code, err := dockerIntegrationWait(t, process)
	if err != nil || code != 0 || string(stdout) != "spawn:input-line\n" || string(stderr) != "spawn-stderr\n" {
		t.Fatalf("spawn results = (%d, %q, %q), err=%v", code, stdout, stderr, err)
	}

	terminatedCtx, cancelTerminated := context.WithTimeout(ctx, 15*time.Second)
	defer cancelTerminated()
	terminated, err := runtime.Spawn(terminatedCtx, identity, []string{"sh", "-c", "printf READY; exec sleep 60"}, false)
	if err != nil {
		t.Fatalf("spawn terminable process: %v", err)
	}
	dockerIntegrationReadReady(t, terminated)
	if err := terminated.Terminate(); err != nil {
		t.Fatalf("terminate spawned process: %v", err)
	}
	if code, err := dockerIntegrationWait(t, terminated); err != nil || code == 0 {
		t.Fatalf("wait after termination = %d, %v", code, err)
	}

	processCtx, cancelProcess := context.WithTimeout(ctx, 15*time.Second)
	defer cancelProcess()
	canceled, err := runtime.Spawn(processCtx, identity, []string{"sh", "-c", "printf READY; exec sleep 60"}, false)
	if err != nil {
		cancelProcess()
		t.Fatalf("spawn cancellable process: %v", err)
	}
	ifReady := make(chan error, 1)
	go func() { ifReady <- dockerIntegrationReadReadyError(canceled) }()
	select {
	case err := <-ifReady:
		if err != nil {
			cancelProcess()
			t.Fatalf("wait for cancellable process readiness: %v", err)
		}
	case <-time.After(5 * time.Second):
		cancelProcess()
		t.Fatal("timed out waiting for cancellable process readiness")
	}
	cancelProcess()
	if code, err := dockerIntegrationWait(t, canceled); err != nil || code == 0 {
		t.Fatalf("wait after context cancellation = %d, %v", code, err)
	}

	result, err = runtime.Exec(ctx, identity, []string{"sh", "-c", "printf 'created-by-agent\\n' > /workspace/created.txt && rm /workspace/remove-me.txt"}, 10*time.Second)
	if err != nil || result.ReturnCode != 0 {
		t.Fatalf("mutate workspace in container: result=%+v err=%v", result, err)
	}
	result, err = runtime.Exec(ctx, identity, []string{"sh", "-c", "cat /workspace/created.txt && test ! -e /workspace/remove-me.txt"}, 10*time.Second)
	if err != nil || result.ReturnCode != 0 || result.Stdout != "created-by-agent\n" {
		t.Fatalf("container workspace mutation was not visible before export: result=%+v err=%v", result, err)
	}
	createdPath := filepath.Join(workspace, "created.txt")
	if expectedTransport == "docker_cp" {
		if _, err := os.Stat(createdPath); !os.IsNotExist(err) {
			t.Fatalf("copy transport unexpectedly changed host before export: stat err=%v", err)
		}
		if _, err := os.Stat(filepath.Join(workspace, "remove-me.txt")); err != nil {
			t.Fatalf("copy transport removed host file before export: %v", err)
		}
	} else {
		if _, err := os.Stat(createdPath); err != nil {
			t.Fatalf("bind transport did not expose created file: %v", err)
		}
		if _, err := os.Stat(filepath.Join(workspace, "remove-me.txt")); !os.IsNotExist(err) {
			t.Fatalf("bind transport did not expose deletion: stat err=%v", err)
		}
	}
	if err := runtime.Stop(ctx, identity); err != nil {
		t.Fatalf("stop fixture container: %v", err)
	}
	if expectedTransport == "docker_cp" {
		if err := runtime.ExportWorkspace(ctx, identity, workspace); err != nil {
			t.Fatalf("export copied workspace: %v", err)
		}
	}
	if body, err := os.ReadFile(createdPath); err != nil || string(body) != "created-by-agent\n" {
		t.Fatalf("workspace export missing agent-created file: body=%q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "remove-me.txt")); !os.IsNotExist(err) {
		t.Fatalf("workspace export did not preserve agent deletion: stat err=%v", err)
	}
	t.Logf("container id=%s transport=%s workspace export and stop verified", identity, expectedTransport)
}

func dockerIntegrationReadReady(t *testing.T, process Process) {
	t.Helper()
	ready := make(chan error, 1)
	go func() { ready <- dockerIntegrationReadReadyError(process) }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("read spawned process readiness: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for spawned process readiness")
	}
}

func dockerIntegrationReadReadyError(process Process) error {
	var ready [5]byte
	if _, err := io.ReadFull(process.Stdout(), ready[:]); err != nil {
		return err
	}
	if string(ready[:]) != "READY" {
		return fmt.Errorf("readiness output = %q, want READY", ready[:])
	}
	return nil
}

func dockerIntegrationWait(t *testing.T, process Process) (int, error) {
	t.Helper()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := process.Wait()
		done <- result{code: code, err: err}
	}()
	select {
	case got := <-done:
		return got.code, got.err
	case <-time.After(8 * time.Second):
		_ = process.Kill()
		t.Fatal("Docker process did not settle within 8 seconds")
		return 0, fmt.Errorf("unreachable")
	}
}

func dockerIntegrationCleanup(t *testing.T, runtime *DockerRuntime, identities []string) {
	t.Helper()
	for _, identity := range identities {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := runtime.Stop(ctx, identity); err != nil {
			t.Errorf("stop test-owned Docker container %s: %v", identity, err)
		}
		if err := runtime.Remove(ctx, identity); err != nil {
			t.Errorf("remove test-owned Docker container %s: %v", identity, err)
		}
		if err := dockerIntegrationAssertRemoved(ctx, runtime, identity); err != nil {
			t.Errorf("verify removal of test-owned Docker container %s: %v", identity, err)
		} else {
			t.Logf("test-owned Docker container id=%s removed and absence verified", identity)
		}
		cancel()
	}
}

func dockerIntegrationAssertRemoved(ctx context.Context, runtime *DockerRuntime, identity string) error {
	_, err := runtime.client.ContainerInspect(ctx, identity, client.ContainerInspectOptions{})
	if err == nil {
		return fmt.Errorf("container still exists")
	}
	if strings.Contains(strings.ToLower(err.Error()), "no such container") || strings.Contains(strings.ToLower(err.Error()), "not found") {
		return nil
	}
	return err
}

type dockerIntegrationRoundTripper struct {
	base       http.RoundTripper
	forceProbe bool
	mu         sync.Mutex
	forced     int
	created    []string
}

func newDockerIntegrationRoundTripper(t *testing.T, forceProbe bool) *dockerIntegrationRoundTripper {
	t.Helper()
	return &dockerIntegrationRoundTripper{forceProbe: forceProbe}
}

func (r *dockerIntegrationRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil && (strings.HasSuffix(req.URL.Path, "/exec") || strings.HasSuffix(req.URL.Path, "/containers/create")) {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	if r.forceProbe && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/exec") {
		var payload struct {
			Cmd []string `json:"Cmd"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("decode Docker exec request in integration shim: %w", err)
		}
		for _, arg := range payload.Cmd {
			if arg == "aqp-bind-probe" {
				r.mu.Lock()
				r.forced++
				r.mu.Unlock()
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"message":"forced integration bind-probe failure"}`)),
					Request:    req,
				}, nil
			}
		}
	}
	response, err := r.base.RoundTrip(req)
	if err != nil || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/containers/create") || response.StatusCode != http.StatusCreated {
		return response, err
	}
	created, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	var payload struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(created, &payload); err != nil {
		return nil, err
	}
	if payload.ID != "" {
		r.mu.Lock()
		r.created = append(r.created, payload.ID)
		r.mu.Unlock()
	}
	response.Body = io.NopCloser(bytes.NewReader(created))
	return response, nil
}

func (r *dockerIntegrationRoundTripper) forcedProbeFailures() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forced
}

func (r *dockerIntegrationRoundTripper) createdIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.created...)
}
