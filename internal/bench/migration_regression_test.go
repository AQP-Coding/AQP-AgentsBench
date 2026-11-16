package bench

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// S1/S4: compatibility probes use only the current Go public CLI and SQLite API.
func TestIndependentCLIQuestionOrder(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"older", "newer"} {
		if err := s.Save(Record{"id": id, "status": "running"}); err != nil {
			t.Fatal(err)
		}
	}
	ids := []string{}
	for _, id := range []string{"older", "newer", "older"} {
		q, e := s.Question(id, "Question", Record{"method": "input"})
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, q)
	}
	var out, stderr bytes.Buffer
	if code := CLI(context.Background(), []string{"--artifacts", s.Root, "questions"}, &out, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, &stderr)
	}
	var got []Record
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatal(got)
	}
	for i, q := range got {
		if q["id"] != ids[i] {
			t.Fatalf("questions reordered: got[%d]=%s want=%s; output=%s", i, q["id"], ids[i], &out)
		}
	}
}
func TestIndependentCLINumericReply(t *testing.T) {
	for _, reply := range []string{"-1", "-1.5", "-.5"} {
		t.Run(reply, func(t *testing.T) {
			s, e := NewStore(filepath.Join(t.TempDir(), "artifacts"))
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			if e = s.Save(Record{"id": "run", "status": "running"}); e != nil {
				t.Fatal(e)
			}
			q, e := s.Question("run", "Value?", Record{"method": "input"})
			if e != nil {
				t.Fatal(e)
			}
			var out, stderr bytes.Buffer
			if code := CLI(context.Background(), []string{"--artifacts", s.Root, "answer", "run", q, reply}, &out, &stderr); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
			value, exists, e := s.Response("run", q)
			if e != nil || !exists || value != reply {
				t.Fatalf("reply=%v exists=%v error=%v", value, exists, e)
			}
		})
	}
}
func TestIndependentCLIUnambiguousPrefixes(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{{"root", []string{"--art", "ROOT", "list"}}, {"questions", []string{"--artifacts", "ROOT", "questions", "--run", "run"}}} {
		t.Run(tc.name, func(t *testing.T) {
			s, e := NewStore(filepath.Join(t.TempDir(), "artifacts"))
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			if e = s.Save(Record{"id": "run", "status": "running"}); e != nil {
				t.Fatal(e)
			}
			args := append([]string(nil), tc.args...)
			for i, a := range args {
				if a == "ROOT" {
					args[i] = s.Root
				}
			}
			var out, stderr bytes.Buffer
			if code := CLI(context.Background(), args, &out, &stderr); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, &stderr)
			}
		})
	}
}
func TestIndependentGatewaySecurityAndUsage(t *testing.T) {
	const secret = "upstream-secret-must-not-leak"
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("wrong upstream auth")
		}
		if r.Header.Get("X-Private") != "" {
			t.Error("client private header forwarded")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"cost\":0.25}}\n\ndata: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"cost\":0.25}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	limit := 0.25
	g, e := NewGateway(secret, GatewayOptions{Upstream: upstream.URL, CostLimit: &limit})
	if e != nil {
		t.Fatal(e)
	}
	defer g.Close()
	for _, tc := range []struct {
		route, token string
		status       int
	}{{"/models", "bad", 401}, {"/models?url=http://example.invalid", g.Token, 404}, {"/chat/completions", g.Token, 200}, {"/chat/completions", g.Token, 402}} {
		req, _ := http.NewRequest("POST", g.BaseURL()+tc.route, strings.NewReader(`{"model":"fixture/no-provider","messages":[]}`))
		if strings.HasPrefix(tc.route, "/models") {
			req.Method = "GET"
			req.Body = nil
			req.ContentLength = 0
		}
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("X-Private", "private")
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatalf("%s status %d want %d", tc.route, res.StatusCode, tc.status)
		}
	}
	if calls.Load() != 1 || !g.LimitReached() {
		t.Fatal("bad request count or limit", calls.Load())
	}
	a := g.Attempts()
	if len(a) != 1 || number(a[0]["cost_usd"]) != 0.25 || a[0]["usage_complete"] != true {
		t.Fatal(a)
	}
	a[0]["cost_usd"] = 999
	if number(g.Attempts()[0]["cost_usd"]) != 0.25 {
		t.Fatal("attempts aliases internal records")
	}
}
func TestIndependentGatewayBoundedSecretDiagnostic(t *testing.T) {
	secret := "sensitive-upstream-key"
	var g *Gateway
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, strings.Repeat("x", 4089)+secret+g.Token+strings.Repeat("y", 1000))
	}))
	defer provider.Close()
	var e error
	g, e = NewGateway(secret, GatewayOptions{Upstream: provider.URL})
	if e != nil {
		t.Fatal(e)
	}
	defer g.Close()
	req, _ := http.NewRequest("POST", g.BaseURL()+"/chat/completions", strings.NewReader(`{"model":"fixture/no-provider"}`))
	req.Header.Set("Authorization", "Bearer "+g.Token)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 429 || len(body) > 4096 || strings.Contains(string(body), secret) || strings.Contains(string(body), g.Token) {
		t.Fatalf("unsafe response status=%d len=%d", res.StatusCode, len(body))
	}
	a := g.Attempts()
	b, _ := json.Marshal(a)
	if len(a) != 1 || strings.Contains(string(b), secret) || strings.Contains(string(b), g.Token) || !g.InfrastructureFailed() {
		t.Fatal("unsafe accounting diagnostic", string(b))
	}
}

type independentEvalRuntime struct {
	Runtime
	t                *testing.T
	original, hidden string
	stops, removes   int
}

func (r *independentEvalRuntime) Start(_ context.Context, _ string, workspace string, o StartOptions) (string, error) {
	if !o.ReadonlyWorkspace || o.Evaluation == "" || workspace == r.original {
		r.t.Fatal("snapshot isolation missing")
	}
	if _, e := os.Stat(filepath.Join(workspace, ".git")); !os.IsNotExist(e) {
		r.t.Fatal("git copied into evaluator")
	}
	if _, e := os.Stat(filepath.Join(workspace, "hidden.py")); !os.IsNotExist(e) {
		r.t.Fatal("hidden file copied into workspace")
	}
	b, e := os.ReadFile(filepath.Join(o.Evaluation, "hidden.py"))
	if e != nil || string(b) != "trusted" {
		r.t.Fatal("missing trusted evaluator")
	}
	if e := os.WriteFile(filepath.Join(workspace, "mutable"), []byte("snapshot-only"), 0600); e != nil {
		r.t.Fatal(e)
	}
	return "fixture", nil
}
func (r *independentEvalRuntime) ExecAs(_ context.Context, _ string, user string, argv []string, _ time.Duration) (ExecResult, error) {
	if user == "0:0" {
		r.t.Fatal("root evaluator")
	}
	return ExecResult{Stdout: "observed output", Stderr: "observed diagnostic"}, nil
}
func (r *independentEvalRuntime) TransportFor(string) string           { return "docker_cp" }
func (r *independentEvalRuntime) Stop(context.Context, string) error   { r.stops++; return nil }
func (r *independentEvalRuntime) Remove(context.Context, string) error { r.removes++; return nil }
func TestIndependentEvaluatorSnapshot(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	os.MkdirAll(filepath.Join(workspace, ".git"), 0700)
	os.WriteFile(filepath.Join(workspace, "mutable"), []byte("original"), 0600)
	hidden := filepath.Join(root, "hidden.py")
	os.WriteFile(hidden, []byte("trusted"), 0600)
	rt := &independentEvalRuntime{t: t, original: workspace, hidden: hidden}
	result, e := Evaluate(context.Background(), rt, workspace, Record{"image": "fixture", "files": []Record{{"source": hidden, "target": "hidden.py"}}, "tests": []Record{{"id": "unit", "score": 1, "command": []string{"arbitrary-tool", "--flag", "literal space"}}}}, root)
	if e != nil || number(result["score_percent"]) != 100 {
		t.Fatal(result, e)
	}
	out := records(result["tests"])[0]
	if out["stdout"] != "observed output" || out["stderr"] != "observed diagnostic" {
		t.Fatal("lost generic argv output", out)
	}
	b, _ := os.ReadFile(filepath.Join(workspace, "mutable"))
	if string(b) != "original" || rt.stops != 1 || rt.removes != 1 {
		t.Fatal("original mutated or cleanup omitted", string(b), rt)
	}
}
func TestIndependentArchiveTraversalAndLocalSocket(t *testing.T) {
	for _, entry := range []string{"../escape", "/absolute", "nested/../../escape"} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		tw.WriteHeader(&tar.Header{Name: entry, Mode: 0600, Size: 1})
		tw.Write([]byte("x"))
		tw.Close()
		if e := dockerExtractArchive(&b, t.TempDir()); e == nil {
			t.Fatal("archive escape accepted", entry)
		}
	}
	for _, host := range []string{"tcp://127.0.0.1:2375", "ssh://fixture.invalid", "https://fixture.invalid"} {
		t.Setenv("DOCKER_HOST", host)
		if rt, e := NewDockerRuntime(); e == nil {
			rt.Close()
			t.Fatal("remote transport accepted", host)
		}
	}
}

// S1/S6/S7: the fake enforces the concrete Docker export quiescence contract.
type independentStoppedExportRuntime struct {
	*runnerTestRuntime
	stopped bool
}

func (r *independentStoppedExportRuntime) Stop(ctx context.Context, id string) error {
	r.stopped = true
	return r.runnerTestRuntime.Stop(ctx, id)
}
func (r *independentStoppedExportRuntime) ExportWorkspace(ctx context.Context, id, workspace string) error {
	if !r.stopped {
		return &independentExportError{}
	}
	return r.runnerTestRuntime.ExportWorkspace(ctx, id, workspace)
}

type independentExportError struct{}

func (*independentExportError) Error() string {
	return "Container must be stopped before workspace export"
}
func TestIndependentRunnerStoppedCopyExport(t *testing.T) {
	root := t.TempDir()
	hidden := filepath.Join(root, "hidden.py")
	if e := os.WriteFile(hidden, []byte("assert True\n"), 0600); e != nil {
		t.Fatal(e)
	}
	store, e := NewStore(filepath.Join(root, "artifacts"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	inner := newRunnerTestRuntime()
	rt := &independentStoppedExportRuntime{runnerTestRuntime: inner}
	options := runnerTestOptions(inner, runnerTestAdapter{})
	options.RuntimeFactory = func() (Runtime, error) { return rt, nil }
	options.AdapterFactory = func(_ Runtime, id string, _ []string, _ string) AgentAdapter {
		return runnerTestAdapter{runtime: inner, container: id}
	}
	results, e := RunCampaign(context.Background(), runnerTestCampaign(hidden), store, options)
	if e != nil || len(results) != 1 {
		t.Fatal(results, e)
	}
	if results[0]["status"] != "completed" || number(object(results[0]["evaluation"])["score_percent"]) != 100 {
		t.Fatalf("normal copy-transport run failed before evaluation: %+v", results[0])
	}
}
func TestIndependentRunnerBudgetStillEvaluates(t *testing.T) {
	root := t.TempDir()
	hidden := filepath.Join(root, "hidden.py")
	if e := os.WriteFile(hidden, []byte("assert True\n"), 0600); e != nil {
		t.Fatal(e)
	}
	store, e := NewStore(filepath.Join(root, "artifacts"))
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	inner := newRunnerTestRuntime()
	rt := &independentStoppedExportRuntime{runnerTestRuntime: inner}
	campaign := runnerTestCampaign(hidden)
	campaign["limits"] = Record{"agent_seconds": 0.01}
	options := runnerTestOptions(inner, runnerTestBlockedAdapter{})
	options.RuntimeFactory = func() (Runtime, error) { return rt, nil }
	options.AdapterFactory = func(_ Runtime, _ string, _ []string, _ string) AgentAdapter { return runnerTestBlockedAdapter{} }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results, e := RunCampaign(ctx, campaign, store, options)
	if e != nil || len(results) != 1 {
		t.Fatal(results, e)
	}
	if results[0]["status"] != "incomplete" || results[0]["stop_reason"] != "agent_time_limit" || object(results[0]["evaluation"]) == nil {
		t.Fatal("budget cancellation lost independent evaluation", results[0])
	}
}
