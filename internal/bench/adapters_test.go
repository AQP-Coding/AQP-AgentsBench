package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type adapterTestRuntime struct {
	process Process
	address string
	mu      sync.Mutex
	stops   int
	argv    []string
	stderr  bool
}

func (r *adapterTestRuntime) Start(context.Context, string, string, StartOptions) (string, error) {
	return "", errors.New("unexpected Start")
}
func (r *adapterTestRuntime) Exec(context.Context, string, []string, time.Duration) (ExecResult, error) {
	return ExecResult{}, errors.New("unexpected Exec")
}
func (r *adapterTestRuntime) ExecAs(context.Context, string, string, []string, time.Duration) (ExecResult, error) {
	return ExecResult{}, errors.New("unexpected ExecAs")
}
func (r *adapterTestRuntime) Spawn(_ context.Context, _ string, argv []string, captureStderr bool) (Process, error) {
	r.argv, r.stderr = append([]string(nil), argv...), captureStderr
	return r.process, nil
}
func (r *adapterTestRuntime) Address(string) (string, error)                        { return r.address, nil }
func (r *adapterTestRuntime) TransportFor(string) string                            { return "" }
func (r *adapterTestRuntime) ExportWorkspace(context.Context, string, string) error { return nil }
func (r *adapterTestRuntime) Stop(context.Context, string) error {
	r.mu.Lock()
	r.stops++
	r.mu.Unlock()
	return nil
}
func (r *adapterTestRuntime) Remove(context.Context, string) error { return nil }

type adapterTestWriter struct {
	mu     sync.Mutex
	bytes  bytes.Buffer
	closed bool
	write  func([]byte) error
}

func (w *adapterTestWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	if _, err := w.bytes.Write(p); err != nil {
		return 0, err
	}
	if w.write != nil {
		if err := w.write(bytes.Clone(p)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
func (w *adapterTestWriter) Close() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	return nil
}
func (w *adapterTestWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes.String()
}

type adapterTestProcess struct {
	stdin       *adapterTestWriter
	stdout      io.ReadCloser
	stderr      io.ReadCloser
	waitCode    int
	waitErr     error
	mu          sync.Mutex
	running     bool
	terminated  bool
	killed      bool
	exited      chan struct{}
	exitOnce    sync.Once
	closeOutput func()
}

func (p *adapterTestProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *adapterTestProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *adapterTestProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *adapterTestProcess) Wait() (int, error) {
	p.mu.Lock()
	if p.exited == nil {
		p.exited = make(chan struct{})
	}
	exited, running := p.exited, p.running
	code, err := p.waitCode, p.waitErr
	p.mu.Unlock()
	if running {
		<-exited
	}
	return code, err
}
func (p *adapterTestProcess) Poll() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitCode, !p.running
}
func (p *adapterTestProcess) Terminate() error {
	p.mu.Lock()
	p.terminated, p.running = true, false
	closeOutput := p.closeOutput
	exited := p.exited
	p.mu.Unlock()
	if exited != nil {
		p.exitOnce.Do(func() { close(exited) })
	}
	if closeOutput != nil {
		closeOutput()
	}
	return nil
}
func (p *adapterTestProcess) Kill() error {
	p.mu.Lock()
	p.killed, p.running = true, false
	closeOutput := p.closeOutput
	exited := p.exited
	p.mu.Unlock()
	if exited != nil {
		p.exitOnce.Do(func() { close(exited) })
	}
	if closeOutput != nil {
		closeOutput()
	}
	return nil
}

func adapterTestJSONLine(v any) []byte {
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

func adapterTestPiProcess(t *testing.T, stderr string, exitCode int, input func([]byte, *io.PipeWriter) error) *adapterTestProcess {
	t.Helper()
	reader, writer := io.Pipe()
	stdin := &adapterTestWriter{}
	stdin.write = func(p []byte) error { return input(p, writer) }
	return &adapterTestProcess{
		stdin: stdin, stdout: reader, stderr: io.NopCloser(strings.NewReader(stderr)), waitCode: exitCode,
		running: true, closeOutput: func() { _ = writer.Close() },
	}
}

func TestPiAdapterWaitsForSettledAndStreamsJSONLRPC(t *testing.T) {
	var once sync.Once
	process := adapterTestPiProcess(t, "", 0, func(line []byte, output *io.PipeWriter) error {
		var message Record
		if err := json.Unmarshal(bytes.TrimSpace(line), &message); err != nil {
			return err
		}
		if message["type"] == "prompt" {
			_, err := output.Write(adapterTestJSONLine(Record{"type": "agent_end"}))
			if err != nil {
				return err
			}
			_, err = output.Write(adapterTestJSONLine(Record{"type": "extension_ui_request", "id": "confirm-1", "method": "confirm", "title": "Allow?"}))
			return err
		}
		if message["type"] == "extension_ui_response" {
			once.Do(func() {
				_, _ = output.Write(adapterTestJSONLine(Record{"type": "message_update", "assistantMessageEvent": Record{"type": "text_delta", "delta": "line\u2028text"}}))
				_, _ = output.Write(adapterTestJSONLine(Record{"type": "agent_settled"}))
			})
		}
		return nil
	})
	runtime := &adapterTestRuntime{process: process}
	var eventsMu sync.Mutex
	var events []Record
	appendEvent := func(event Record) { eventsMu.Lock(); events = append(events, event); eventsMu.Unlock() }
	err := NewPiAdapter(runtime, "cid", nil).Run(context.Background(), "prompt", "org/model", appendEvent, func(_ context.Context, question string, _ Record) (any, error) {
		if question != "Allow?" {
			return nil, fmt.Errorf("unexpected question %q", question)
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.stderr {
		t.Fatal("Pi must request separately captured stderr")
	}
	if strings.Join(runtime.argv, " ") != "pi --mode rpc --no-session --provider aqp --model org/model" {
		t.Fatalf("unexpected argv: %q", runtime.argv)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	var names []string
	for _, event := range events {
		names = append(names, text(event["type"]))
	}
	if names[0] != "agent_start" || names[len(names)-1] != "agent_end" {
		t.Fatalf("unexpected lifecycle: %v", names)
	}
	if !strings.Contains(strings.Join(names, ","), "agent_wait") || !strings.Contains(strings.Join(names, ","), "agent_resume") {
		t.Fatalf("dialog lifecycle missing: %v", names)
	}
	foundMessage := false
	for _, event := range events {
		if event["type"] == "message" && event["text"] == "line\u2028text" {
			foundMessage = true
		}
	}
	if !foundMessage {
		t.Fatalf("Unicode text delta was not emitted: %#v", events)
	}
	var responses []Record
	for scanner := bufio.NewScanner(strings.NewReader(process.stdin.String())); scanner.Scan(); {
		var record Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err == nil {
			responses = append(responses, record)
		}
	}
	if len(responses) != 2 || responses[1]["confirmed"] != true {
		t.Fatalf("unexpected RPC writes: %#v", responses)
	}
}

func TestPiAdapterHandledAndModelFailures(t *testing.T) {
	t.Run("handled prompt is terminal", func(t *testing.T) {
		process := adapterTestPiProcess(t, "", 0, func(line []byte, output *io.PipeWriter) error {
			var record Record
			_ = json.Unmarshal(bytes.TrimSpace(line), &record)
			if record["type"] == "prompt" {
				_, err := output.Write(adapterTestJSONLine(Record{"type": "response", "success": true, "data": Record{"disposition": "handled"}}))
				return err
			}
			return nil
		})
		var events []Record
		if err := NewPiAdapter(&adapterTestRuntime{process: process}, "cid", nil).Run(context.Background(), "p", "m", func(e Record) { events = append(events, e) }, nil); err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 || events[1]["type"] != "agent_end" {
			t.Fatalf("unexpected handled lifecycle: %#v", events)
		}
	})
	t.Run("command rejection is surfaced", func(t *testing.T) {
		process := adapterTestPiProcess(t, "", 0, func(line []byte, output *io.PipeWriter) error {
			var record Record
			_ = json.Unmarshal(bytes.TrimSpace(line), &record)
			if record["type"] == "prompt" {
				_, err := output.Write(adapterTestJSONLine(Record{"type": "response", "success": false, "error": "wrong model"}))
				return err
			}
			return nil
		})
		err := NewPiAdapter(&adapterTestRuntime{process: process}, "cid", nil).Run(context.Background(), "p", "m", func(Record) {}, nil)
		if err == nil || !strings.Contains(err.Error(), "Pi command rejected: wrong model") {
			t.Fatalf("expected actionable command rejection, got %v", err)
		}
	})
	t.Run("model error remains an agent error", func(t *testing.T) {
		process := adapterTestPiProcess(t, "", 0, func(line []byte, output *io.PipeWriter) error {
			var record Record
			_ = json.Unmarshal(bytes.TrimSpace(line), &record)
			if record["type"] == "prompt" {
				_, err := output.Write(adapterTestJSONLine(Record{"type": "message_end", "message": Record{"stopReason": "error", "errorMessage": "provider error"}}))
				if err != nil {
					return err
				}
				_, err = output.Write(adapterTestJSONLine(Record{"type": "agent_settled"}))
				return err
			}
			return nil
		})
		err := NewPiAdapter(&adapterTestRuntime{process: process}, "cid", nil).Run(context.Background(), "p", "m", func(Record) {}, nil)
		if err == nil || !strings.Contains(err.Error(), "provider error") {
			t.Fatalf("expected provider error, got %v", err)
		}
	})
	t.Run("malformed JSON fails closed", func(t *testing.T) {
		process := adapterTestPiProcess(t, "", 0, func(line []byte, output *io.PipeWriter) error {
			var record Record
			_ = json.Unmarshal(bytes.TrimSpace(line), &record)
			if record["type"] == "prompt" {
				_, err := output.Write([]byte("[]\n"))
				return err
			}
			return nil
		})
		err := NewPiAdapter(&adapterTestRuntime{process: process}, "cid", nil).Run(context.Background(), "p", "m", func(Record) {}, nil)
		if err == nil || !strings.Contains(err.Error(), "Invalid Pi RPC output") {
			t.Fatalf("expected protocol error, got %v", err)
		}
	})
}

func TestPiAdapterCancellationDoesNotWaitForHumanCallback(t *testing.T) {
	entered := make(chan struct{})
	process := adapterTestPiProcess(t, strings.Repeat("x", 70*1024), 17, func(line []byte, output *io.PipeWriter) error {
		var record Record
		_ = json.Unmarshal(bytes.TrimSpace(line), &record)
		if record["type"] == "prompt" {
			_, err := output.Write(adapterTestJSONLine(Record{"type": "extension_ui_request", "id": "q", "method": "input", "title": "Value?"}))
			return err
		}
		return nil
	})
	runtime := &adapterTestRuntime{process: process}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewPiAdapter(runtime, "cid", nil).Run(ctx, "p", "m", func(Record) {}, func(ctx context.Context, _ string, _ Record) (any, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("human callback was not invoked")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation should end cleanly, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run blocked behind the human callback")
	}
	runtime.mu.Lock()
	stops := runtime.stops
	runtime.mu.Unlock()
	if stops != 1 {
		t.Fatalf("container stop calls = %d, want 1", stops)
	}
	if process.killed || !process.terminated {
		t.Fatalf("unexpected process cleanup: terminated=%t killed=%t", process.terminated, process.killed)
	}
}

func TestPiAdapterEOFReportsBoundedStderrAndExitCode(t *testing.T) {
	malformedUTF8 := string(bytes.Repeat([]byte{0xff, 'd'}, adapterPiStderrLimit))
	process := &adapterTestProcess{stdin: &adapterTestWriter{}, stdout: io.NopCloser(strings.NewReader("")), stderr: io.NopCloser(strings.NewReader(malformedUTF8)), waitCode: 17}
	err := NewPiAdapter(&adapterTestRuntime{process: process}, "cid", nil).Run(context.Background(), "p", "m", func(Record) {}, nil)
	if err == nil || !strings.Contains(err.Error(), "exit code 17") || !strings.Contains(err.Error(), "stderr truncated at 65536 bytes") {
		t.Fatalf("expected bounded startup diagnostic, got %v", err)
	}
	if got := len(err.Error()); got > 66*1024 {
		t.Fatalf("diagnostic exceeded bound: %d bytes", got)
	}
}

func adapterTestPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	n, err := net.LookupPort("tcp", port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOpenCodeHTTPAuthSSEChildrenDialogsAndBlockingMessage(t *testing.T) {
	const password = "not-a-real-secret"
	permissionSent := make(chan struct{})
	permissionReplied := make(chan struct{})
	questionReplied := make(chan struct{})
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:"+password)); got != want {
			t.Errorf("authorization = %q", got)
		}
		mu.Lock()
		requests = append(requests, r.Method+" "+r.RequestURI)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/global/health":
			_, _ = io.WriteString(w, `{"healthy":true,"version":"1.2.27"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_, _ = io.WriteString(w, `{"id":"session/main"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			flusher.Flush()
			_, _ = io.WriteString(w, "data: {\"type\":\"session.created\",\n")
			_, _ = io.WriteString(w, "data: \"properties\":{\"info\":{\"id\":\"child-1\",\"parentID\":\"session/main\"}}}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"permission.asked\",\"properties\":{\"id\":\"permission/1\",\"sessionID\":\"child-1\",\"permission\":\"write\"}}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"question.asked\",\"properties\":{\"id\":\"question-1\",\"sessionID\":\"child-1\",\"questions\":[{\"question\":\"Continue?\"}]}}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"session.status\",\"properties\":{\"sessionID\":\"child-1\",\"status\":{\"type\":\"idle\"}}}\n\n")
			flusher.Flush()
			<-r.Context().Done()
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/permission/"):
			if !strings.Contains(r.RequestURI, "permission%2F1") {
				t.Errorf("permission ID was not escaped: %q", r.RequestURI)
			}
			var body Record
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["reply"] != "once" {
				t.Errorf("permission reply = %#v", body)
			}
			close(permissionReplied)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/question/"):
			var body Record
			_ = json.NewDecoder(r.Body).Decode(&body)
			answers, _ := json.Marshal(body["answers"])
			if string(answers) != `[["yes"]]` {
				t.Errorf("question answers = %s", answers)
			}
			close(questionReplied)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message"):
			if !strings.Contains(r.RequestURI, "session%2Fmain") {
				t.Errorf("session ID was not escaped: %q", r.RequestURI)
			}
			select {
			case <-permissionSent:
			case <-time.After(time.Second):
				t.Error("SSE subscription was not established before prompt")
			}
			select {
			case <-permissionReplied:
			case <-time.After(time.Second):
				t.Error("message completed before permission reply")
			}
			select {
			case <-questionReplied:
			case <-time.After(time.Second):
				t.Error("message completed before question reply")
			}
			var body Record
			_ = json.NewDecoder(r.Body).Decode(&body)
			if object(body["model"])["providerID"] != "aqp" {
				t.Errorf("unexpected model body: %#v", body)
			}
			_, _ = io.WriteString(w, `{"info":{"role":"assistant","finish":"stop"},"parts":[{"type":"text","text":"done"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	process := &adapterTestProcess{stdin: &adapterTestWriter{}, stdout: io.NopCloser(strings.NewReader("")), stderr: io.NopCloser(strings.NewReader(""))}
	address, _, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	runtime := &adapterTestRuntime{process: process, address: address}
	adapter := NewOpenCodeAdapter(runtime, "cid", nil)
	adapter.password, adapter.port = password, adapterTestPort(t, server)
	var eventsMu sync.Mutex
	var events []Record
	appendEvent := func(event Record) { eventsMu.Lock(); events = append(events, event); eventsMu.Unlock() }
	ask := func(_ context.Context, question string, metadata Record) (any, error) {
		if metadata["agent_id"] != "child-1" || metadata["parent_id"] != "main" {
			return nil, fmt.Errorf("unexpected dialog: %q %#v", question, metadata)
		}
		switch question {
		case "write":
			select {
			case <-permissionSent:
			default:
				close(permissionSent)
			}
			return "once", nil
		case "Continue?":
			return [][]string{{"yes"}}, nil
		default:
			return nil, fmt.Errorf("unexpected question %q", question)
		}
	}
	err := adapter.Run(context.Background(), "hello", "org/model", appendEvent, ask)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	captured := append([]string(nil), requests...)
	mu.Unlock()
	if len(captured) < 5 || captured[0] != "GET /global/health" || captured[1] != "POST /session" || captured[2] != "GET /event" {
		t.Fatalf("unexpected request order: %v", captured)
	}
	if !strings.Contains(strings.Join(captured, "\n"), "POST /session/session%2Fmain/message") {
		t.Fatalf("blocking message request missing: %v", captured)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	var starts, ends, waits, resumes, messages int
	for _, e := range events {
		switch e["type"] {
		case "agent_start":
			starts++
		case "agent_end":
			ends++
		case "agent_wait":
			waits++
		case "agent_resume":
			resumes++
		case "message":
			if e["text"] == "done" {
				messages++
			}
		}
	}
	if starts != 2 || ends != 2 || waits != 2 || resumes != 2 || messages != 1 {
		t.Fatalf("unexpected normalized events: %#v", events)
	}
	var childStart, childEnd bool
	for _, e := range events {
		if e["agent_id"] == "child-1" && e["parent_id"] == "main" && e["type"] == "agent_start" {
			childStart = true
		}
		if e["agent_id"] == "child-1" && e["parent_id"] == "main" && e["type"] == "agent_end" {
			childEnd = true
		}
	}
	if !childStart || !childEnd {
		t.Fatalf("child attribution missing: %#v", events)
	}
}

func TestOpenCodeRejectsUnsupportedVersionAndRedirect(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "unsupported version", status: 200, body: `{"healthy":true,"version":"2.0.0"}`, want: "Unsupported OpenCode API version"},
		{name: "redirect", status: 302, body: "", want: "redirect"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if test.status == 302 {
					w.Header().Set("Location", "http://example.invalid/")
					w.WriteHeader(302)
					return
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			address, _, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			adapter := NewOpenCodeAdapter(&adapterTestRuntime{process: &adapterTestProcess{stdin: &adapterTestWriter{}, stdout: io.NopCloser(strings.NewReader("")), stderr: io.NopCloser(strings.NewReader("")), running: true}, address: address}, "cid", nil)
			adapter.port = adapterTestPort(t, server)
			err := adapter.Run(context.Background(), "p", "m", func(Record) {}, nil)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
			if requests != 1 {
				t.Fatalf("redirect followed or endpoint retried unexpectedly: %d calls", requests)
			}
		})
	}
}

func TestOpenCodeCancellationStopsContainerWhileAskWaits(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_, _ = io.WriteString(w, `{"healthy":true,"version":"1.2.27"}`)
		case "/session":
			_, _ = io.WriteString(w, `{"id":"ses-1"}`)
		case "/event":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"question.asked\",\"properties\":{\"id\":\"q1\",\"sessionID\":\"ses-1\",\"questions\":[{\"question\":\"Continue?\"}]}}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		case "/session/ses-1/message":
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	address, _, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	runtime := &adapterTestRuntime{process: &adapterTestProcess{stdin: &adapterTestWriter{}, stdout: io.NopCloser(strings.NewReader("")), stderr: io.NopCloser(strings.NewReader("")), running: true}, address: address}
	adapter := NewOpenCodeAdapter(runtime, "cid", nil)
	adapter.port = adapterTestPort(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- adapter.Run(ctx, "p", "m", func(Record) {}, func(ctx context.Context, _ string, _ Record) (any, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("question callback did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancellation should end cleanly, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run blocked behind question callback")
	}
	runtime.mu.Lock()
	stops := runtime.stops
	runtime.mu.Unlock()
	if stops != 1 {
		t.Fatalf("container stop calls = %d, want 1", stops)
	}
}
