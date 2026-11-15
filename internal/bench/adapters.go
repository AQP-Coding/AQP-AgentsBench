package bench

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// adapterPiStderrLimit caps startup and premature-EOF diagnostics.
	adapterPiStderrLimit = 64 * 1024
	// adapterPiRecordLimit bounds one Pi JSONL record, not the full stream.
	adapterPiRecordLimit = 8 * 1024 * 1024
	// adapterOCBodyLimit bounds a single OpenCode HTTP response body.
	adapterOCBodyLimit = 8 * 1024 * 1024
	// adapterOCDefaultPort is the authenticated server port inside the container;
	// it is not published on a host interface.
	adapterOCDefaultPort = 4096
)

// PiAdapter translates Pi RPC into benchmark lifecycle events and human requests.
// Prompt acceptance and agent_end are not completion; agent_settled or an explicit
// handled disposition is required.
type PiAdapter struct {
	runtime   Runtime
	container string
	command   []string
}

// NewPiAdapter binds Pi RPC to an existing container and copies command.
// An empty command selects pi; Run owns the resulting process, not the runtime.
func NewPiAdapter(runtime Runtime, container string, command []string) *PiAdapter {
	if len(command) == 0 {
		command = []string{"pi"}
	}
	return &PiAdapter{runtime: runtime, container: container, command: append([]string(nil), command...)}
}

// Run drives one Pi prompt until settlement, a handled disposition, or an error.
// A nil context uses context.Background; cancellation cleans up and returns nil.
// Events are delivered synchronously; ask must honor its context so pending dialogs
// can finish before Run returns. A nil ask fails only when a dialog is received.
func (a *PiAdapter) Run(ctx context.Context, prompt, model string, onEvent EventHandler, ask AskHandler) (runErr error) {
	ctx = adapterContext(ctx)
	argv := append(append([]string(nil), a.command...), "--mode", "rpc", "--no-session", "--provider", "aqp", "--model", model)
	process, err := a.runtime.Spawn(ctx, a.container, argv, true)
	if err != nil {
		return fmt.Errorf("start Pi RPC process: %w", err)
	}
	if process == nil {
		return errors.New("Pi RPC process was not created")
	}
	waitDone := adapterWaitProcess(process)
	if process.Stdin() == nil || process.Stdout() == nil {
		adapterFinishProcess(process, waitDone)
		return errors.New("Pi RPC process did not provide stdin and stdout")
	}
	stdin, stdout := process.Stdin(), process.Stdout()
	stderr := process.Stderr()
	if stderr == nil {
		stderr = io.NopCloser(strings.NewReader(""))
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stderrCapture := &adapterStderrCapture{}
	stderrDone := make(chan struct{})
	go func() { adapterDrainStderr(stderr, stderrCapture); close(stderrDone) }()
	readRecords := make(chan adapterPiRead, 4)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), adapterPiRecordLimit)
		for scanner.Scan() {
			var record Record
			if err := decodeJSON(scanner.Bytes(), &record); err != nil || record == nil {
				if err == nil {
					err = errors.New("Pi RPC records must be objects")
				}
				if !adapterSendPiRead(runCtx, readRecords, adapterPiRead{err: err}) {
					return
				}
				return
			}
			if !adapterSendPiRead(runCtx, readRecords, adapterPiRead{record: record}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = adapterSendPiRead(runCtx, readRecords, adapterPiRead{err: err})
			return
		}
		_ = adapterSendPiRead(runCtx, readRecords, adapterPiRead{eof: true})
	}()

	adapterEmit(onEvent, "agent_start", "main", "", nil)
	started := true
	defer func() {
		cancel()
		if ctx.Err() != nil {
			adapterStopContainer(a.runtime, a.container)
		}
		adapterFinishProcess(process, waitDone)
		_ = stdout.Close()
		_ = stderr.Close()
		select {
		case <-readerDone:
		case <-time.After(time.Second):
		}
		select {
		case <-stderrDone:
		case <-time.After(time.Second):
		}
		if started {
			adapterEmit(onEvent, "agent_end", "main", "", nil)
		}
	}()

	writeMu := &sync.Mutex{}
	send := func(record Record) error { return adapterWriteJSONL(writeMu, stdin, record) }
	if err := send(Record{"id": "prompt-1", "type": "prompt", "message": prompt}); err != nil {
		return fmt.Errorf("send Pi prompt: %w", err)
	}
	dialogs := newAdapterDialogs(runCtx, ask, onEvent)
	modelFailure := ""
	for {
		if err := dialogs.check(); err != nil {
			return err
		}
		select {
		case <-runCtx.Done():
			return nil
		case err := <-dialogs.errors:
			return err
		case output := <-readRecords:
			if output.eof {
				code, waitErr := adapterExitBeforeSettled(waitDone)
				_ = waitErr
				select {
				case <-stderrDone:
				case <-time.After(time.Second):
				}
				stderrDetail, truncated := stderrCapture.value()
				message := fmt.Sprintf("Pi RPC stdout closed before agent_settled (exit code %s).", code)
				if stderrDetail != "" {
					message += " Pi stderr: " + stderrDetail
				}
				if truncated {
					message += " [stderr truncated at 65536 bytes]"
				}
				return errors.New(message)
			}
			if output.err != nil {
				return fmt.Errorf("Invalid Pi RPC output: %w", output.err)
			}
			if err := a.handlePiRecord(output.record, onEvent, dialogs, send, &modelFailure); err != nil {
				return err
			}
			if output.record["type"] == "response" && object(output.record["data"])["disposition"] == "handled" {
				return dialogs.wait()
			}
			if output.record["type"] == "agent_settled" {
				if modelFailure != "" {
					return errors.New(modelFailure)
				}
				return dialogs.wait()
			}
		}
	}
}

func (a *PiAdapter) handlePiRecord(record Record, onEvent EventHandler, dialogs *adapterDialogs, send func(Record) error, modelFailure *string) error {
	switch text(record["type"]) {
	case "response":
		if record["success"] == false {
			return fmt.Errorf("Pi command rejected: %s", defaultText(record, "error", "unknown error"))
		}
	case "message_update":
		update := object(record["assistantMessageEvent"])
		if update["type"] == "text_delta" {
			adapterEmit(onEvent, "message", "main", "", Record{"text": update["delta"]})
		}
	case "message_end":
		message := object(record["message"])
		if message["stopReason"] == "error" || message["stopReason"] == "aborted" {
			*modelFailure = defaultText(message, "errorMessage", "Pi model work failed or was aborted")
		}
	case "extension_ui_request":
		method := text(record["method"])
		switch method {
		case "select", "confirm", "input", "editor":
			request := cloneRecord(record)
			question := defaultText(request, "title", defaultText(request, "message", defaultText(request, "permission", "Agent requests input")))
			dialogs.submit(request, question, "main", "", func(reply any) error {
				response := Record{"type": "extension_ui_response", "id": request["id"]}
				if reply == nil {
					response["cancelled"] = true
				} else if method == "confirm" {
					confirmed, ok := reply.(bool)
					if !ok {
						return errors.New("Pi confirmation requires a boolean reply")
					}
					response["confirmed"] = confirmed
				} else {
					value, ok := reply.(string)
					if !ok {
						return fmt.Errorf("Pi %s reply requires a string", method)
					}
					response["value"] = value
				}
				return send(response)
			})
		case "notify":
			adapterEmit(onEvent, "message", "main", "", Record{"text": record["message"]})
		}
	}
	return nil
}

// OpenCodeAdapter bridges an authenticated OpenCode v1.2 HTTP/SSE session to
// benchmark events. Requests use an ephemeral password rather than ambient credentials.
type OpenCodeAdapter struct {
	runtime     Runtime
	container   string
	command     []string
	password    string
	passwordErr error
	port        int
}

// NewOpenCodeAdapter binds a session to an existing container and copies command.
// An empty command selects opencode; the server stays on the container network.
func NewOpenCodeAdapter(runtime Runtime, container string, command []string) *OpenCodeAdapter {
	if len(command) == 0 {
		command = []string{"opencode"}
	}
	password, err := adapterRandomPassword()
	return &OpenCodeAdapter{runtime: runtime, container: container, command: append([]string(nil), command...), password: password, passwordErr: err, port: adapterOCDefaultPort}
}

// Run starts the authenticated server, subscribes to events, and submits one prompt.
// It waits for pending dialog callbacks before returning. A nil context uses
// context.Background; cancellation cleans up and returns nil. Event callbacks are
// synchronous, and ask must honor its context; a nil ask fails on a dialog.
func (a *OpenCodeAdapter) Run(ctx context.Context, prompt, model string, onEvent EventHandler, ask AskHandler) (runErr error) {
	ctx = adapterContext(ctx)
	if a.passwordErr != nil {
		return fmt.Errorf("create OpenCode server password: %w", a.passwordErr)
	}
	if a.password == "" {
		password, err := adapterRandomPassword()
		if err != nil {
			return fmt.Errorf("create OpenCode server password: %w", err)
		}
		a.password = password
	}
	port := a.port
	if port <= 0 || port > 65535 {
		return errors.New("invalid OpenCode server port")
	}
	argv := append([]string{"env", "OPENCODE_SERVER_PASSWORD=" + a.password}, a.command...)
	argv = append(argv, "serve", "--hostname", "0.0.0.0", "--port", fmt.Sprint(port))
	process, err := a.runtime.Spawn(ctx, a.container, argv, false)
	if err != nil {
		return fmt.Errorf("start OpenCode server: %w", err)
	}
	if process == nil {
		return errors.New("OpenCode server process was not created")
	}
	waitDone := adapterWaitProcess(process)
	if process.Stdin() == nil || process.Stdout() == nil {
		adapterFinishProcess(process, waitDone)
		return errors.New("OpenCode server process did not provide stdin and stdout")
	}
	stdout := process.Stdout()
	stdin, stderr := process.Stdin(), process.Stderr()
	if stderr != nil {
		defer stderr.Close()
	}
	baseURL, err := adapterOpenCodeURL(a.runtime, a.container, port)
	if err != nil {
		adapterFinishProcess(process, waitDone)
		_ = stdin.Close()
		_ = stdout.Close()
		return err
	}
	client := adapterOpenCodeClient()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ending := make(chan struct{})
	var endingOnce sync.Once
	stopReaders := func() { endingOnce.Do(func() { close(ending) }) }
	started := false
	defer func() {
		cancel()
		stopReaders()
		if ctx.Err() != nil {
			adapterStopContainer(a.runtime, a.container)
		}
		adapterFinishProcess(process, waitDone)
		_ = stdout.Close()
		client.CloseIdleConnections()
		if started {
			adapterEmit(onEvent, "agent_end", "main", "", nil)
		}
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		if runCtx.Err() != nil {
			return nil
		}
		requestCtx, requestCancel := context.WithTimeout(runCtx, 2*time.Second)
		health, healthErr := adapterOpenCodeRequest(requestCtx, client, baseURL, a.password, http.MethodGet, "/global/health", nil)
		requestCancel()
		if healthErr == nil {
			if health["healthy"] != true {
				return errors.New("OpenCode health check failed")
			}
			version := text(health["version"])
			if version != "" && !strings.HasPrefix(version, "1.2.") {
				return errors.New("Unsupported OpenCode API version; adapter targets v1.2")
			}
			break
		}
		if !adapterRetryableOpenCodeError(healthErr) {
			return healthErr
		}
		if time.Now().After(deadline) {
			if runCtx.Err() != nil {
				return nil
			}
			if _, exited := process.Poll(); exited || time.Now().After(deadline) {
				return errors.New("OpenCode server did not become ready; check image and API version")
			}
		}
		select {
		case <-runCtx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}

	requestCtx, requestCancel := context.WithTimeout(runCtx, 5*time.Second)
	session, err := adapterOpenCodeRequest(requestCtx, client, baseURL, a.password, http.MethodPost, "/session", Record{})
	requestCancel()
	if err != nil {
		return fmt.Errorf("OpenCode session creation failed: %w", err)
	}
	sessionID := text(session["id"])
	if sessionID == "" {
		return errors.New("OpenCode session response did not include an id")
	}

	events := make(chan adapterOpenCodeRead, 32)
	ready := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		adapterReadOpenCodeEvents(runCtx, client, baseURL, a.password, events, ready, ending)
	}()
	select {
	case readyErr := <-ready:
		if readyErr != nil {
			return fmt.Errorf("OpenCode event subscription failed: %w", readyErr)
		}
	case <-time.After(10 * time.Second):
		return errors.New("OpenCode event subscription did not become ready")
	case <-runCtx.Done():
		return nil
	}

	adapterEmit(onEvent, "agent_start", "main", "", nil)
	started = true
	dialogs := newAdapterDialogs(runCtx, ask, onEvent)
	children := make(map[string]string)
	messageResult := make(chan adapterOpenCodeResult, 1)
	go func() {
		messageCtx, messageCancel := context.WithTimeout(runCtx, 24*time.Hour)
		defer messageCancel()
		body := Record{"model": Record{"providerID": "aqp", "modelID": model}, "parts": []any{Record{"type": "text", "text": prompt}}}
		result, err := adapterOpenCodeRequest(messageCtx, client, baseURL, a.password, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/message", body)
		select {
		case messageResult <- adapterOpenCodeResult{record: result, err: err}:
		case <-runCtx.Done():
		}
	}()
	messageDone := false
	for !messageDone {
		if err := dialogs.check(); err != nil {
			return err
		}
		// Drain events already observed before accepting a completed message response.
		select {
		case event := <-events:
			if err := a.handleOpenCodeEvent(event, sessionID, children, dialogs, client, baseURL, onEvent); err != nil {
				return err
			}
			continue
		default:
		}
		select {
		case <-runCtx.Done():
			return nil
		case err := <-dialogs.errors:
			return err
		case event := <-events:
			if event.err != nil {
				return fmt.Errorf("OpenCode event protocol failed: %w", event.err)
			}
			if err := a.handleOpenCodeEvent(event, sessionID, children, dialogs, client, baseURL, onEvent); err != nil {
				return err
			}
		case response := <-messageResult:
			if response.err != nil {
				return fmt.Errorf("OpenCode message request failed: %w", response.err)
			}
			if response.record == nil || response.record["info"] == nil || response.record["parts"] == nil {
				return errors.New("OpenCode returned no completed message; check v1.2 HTTP API capability")
			}
			info := object(response.record["info"])
			if agentError := info["error"]; agentError != nil {
				encoded, _ := json.Marshal(agentError)
				return fmt.Errorf("OpenCode agent returned an error: %s", encoded)
			}
			for _, part := range records(response.record["parts"]) {
				if text(part["type"]) == "text" {
					adapterEmit(onEvent, "message", "main", "", Record{"text": part["text"]})
				}
			}
			if err := dialogs.wait(); err != nil {
				return err
			}
			messageDone = true
		}
	}
	return nil
}

func (a *OpenCodeAdapter) handleOpenCodeEvent(event adapterOpenCodeRead, sessionID string, children map[string]string, dialogs *adapterDialogs, client *http.Client, baseURL string, onEvent EventHandler) error {
	if event.err != nil {
		return fmt.Errorf("OpenCode event protocol failed: %w", event.err)
	}
	kind := text(event.record["type"])
	props := object(event.record["properties"])
	if kind == "session.created" {
		child := object(props["info"])
		childID, parentID := text(child["id"]), text(child["parentID"])
		if childID != "" && (parentID == sessionID || children[parentID] != "") {
			parentAgent := "main"
			if parentID != sessionID {
				parentAgent = parentID
			}
			children[childID] = parentAgent
			adapterEmit(onEvent, "agent_start", childID, parentAgent, nil)
		}
	}
	sessionEventID := text(props["sessionID"])
	if sessionEventID != sessionID {
		parent, child := children[sessionEventID]
		if !child {
			return nil
		}
		if kind == "session.status" && text(object(props["status"])["type"]) == "idle" {
			adapterEmit(onEvent, "agent_end", sessionEventID, parent, nil)
		}
	}
	if sessionEventID != sessionID && children[sessionEventID] == "" {
		return nil
	}
	agentID, parentID := "main", ""
	if sessionEventID != sessionID {
		agentID, parentID = sessionEventID, children[sessionEventID]
	}
	if kind != "permission.asked" && kind != "question.asked" {
		return nil
	}
	request := cloneRecord(props)
	if kind == "question.asked" {
		questions := records(props["questions"])
		if len(questions) == 0 {
			return errors.New("OpenCode question request did not include questions")
		}
		var titles []string
		for _, question := range questions {
			titles = append(titles, text(question["question"]))
		}
		request["title"] = strings.Join(titles, "\n")
	}
	question := text(request["title"])
	if question == "" {
		question = defaultText(request, "permission", "Agent requests input")
	}
	dialogs.submit(request, question, agentID, parentID, func(reply any) error {
		requestID := url.PathEscape(text(props["id"]))
		if requestID == "" {
			return errors.New("OpenCode dialog request did not include an id")
		}
		requestCtx, cancel := context.WithTimeout(dialogs.ctx, 30*time.Second)
		defer cancel()
		if kind == "permission.asked" {
			permission, ok := reply.(string)
			if !ok || (permission != "once" && permission != "always" && permission != "reject") {
				return errors.New("OpenCode permission reply must be once, always or reject")
			}
			_, err := adapterOpenCodeRequest(requestCtx, client, baseURL, a.password, http.MethodPost, "/permission/"+requestID+"/reply", Record{"reply": permission})
			return err
		}
		if reply == nil {
			_, err := adapterOpenCodeRequest(requestCtx, client, baseURL, a.password, http.MethodPost, "/question/"+requestID+"/reject", Record{})
			return err
		}
		answers, ok := adapterStringLists(reply)
		if !ok || len(answers) != len(records(props["questions"])) {
			return errors.New("OpenCode question reply requires one string-list per question")
		}
		_, err := adapterOpenCodeRequest(requestCtx, client, baseURL, a.password, http.MethodPost, "/question/"+requestID+"/reply", Record{"answers": answers})
		return err
	})
	return nil
}

type adapterDialogs struct {
	ctx     context.Context
	ask     AskHandler
	onEvent EventHandler
	errors  chan error
	changed chan struct{}
	mu      sync.Mutex
	seen    map[string]struct{}
	active  int
}

func newAdapterDialogs(ctx context.Context, ask AskHandler, onEvent EventHandler) *adapterDialogs {
	return &adapterDialogs{ctx: ctx, ask: ask, onEvent: onEvent, errors: make(chan error, 8), changed: make(chan struct{}, 1), seen: make(map[string]struct{})}
}

func (d *adapterDialogs) submit(request Record, question, agentID, parentID string, respond func(any) error) {
	id := text(request["id"])
	if id == "" {
		select {
		case d.errors <- errors.New("dialog request did not include an id"):
		case <-d.ctx.Done():
		}
		return
	}
	d.mu.Lock()
	if _, exists := d.seen[id]; exists {
		d.mu.Unlock()
		return
	}
	d.seen[id] = struct{}{}
	d.active++
	d.mu.Unlock()
	metadata := cloneRecord(request)
	metadata["agent_id"] = agentID
	if parentID == "" {
		metadata["parent_id"] = nil
	} else {
		metadata["parent_id"] = parentID
	}
	adapterEmit(d.onEvent, "agent_wait", agentID, parentID, nil)
	go func() {
		defer func() {
			d.mu.Lock()
			d.active--
			d.mu.Unlock()
			select {
			case d.changed <- struct{}{}:
			default:
			}
		}()
		if d.ask == nil {
			select {
			case d.errors <- errors.New("human dialog handler is unavailable"):
			case <-d.ctx.Done():
			}
			return
		}
		reply, err := d.ask(d.ctx, question, metadata)
		if err == nil && d.ctx.Err() == nil {
			err = respond(reply)
		}
		if err != nil && d.ctx.Err() == nil {
			select {
			case d.errors <- fmt.Errorf("Human reply could not be delivered: %w", err):
			case <-d.ctx.Done():
			}
			return
		}
		if d.ctx.Err() == nil {
			adapterEmit(d.onEvent, "agent_resume", agentID, parentID, nil)
		}
	}()
}

func (d *adapterDialogs) check() error {
	select {
	case err := <-d.errors:
		return err
	default:
		return nil
	}
}

func (d *adapterDialogs) wait() error {
	for {
		if err := d.check(); err != nil {
			return err
		}
		d.mu.Lock()
		active := d.active
		d.mu.Unlock()
		if active == 0 || d.ctx.Err() != nil {
			return nil
		}
		select {
		case <-d.ctx.Done():
			return nil
		case err := <-d.errors:
			return err
		case <-d.changed:
		}
	}
}

type adapterPiRead struct {
	record Record
	err    error
	eof    bool
}
type adapterOpenCodeRead struct {
	record Record
	err    error
}
type adapterOpenCodeResult struct {
	record Record
	err    error
}
type adapterProcessWait struct {
	code int
	err  error
}

func adapterContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func adapterSendPiRead(ctx context.Context, records chan<- adapterPiRead, value adapterPiRead) bool {
	select {
	case records <- value:
		return true
	case <-ctx.Done():
		return false
	}
}

type adapterProcessWatcher struct {
	done   chan struct{}
	result adapterProcessWait
}

func adapterWaitProcess(process Process) *adapterProcessWatcher {
	watcher := &adapterProcessWatcher{done: make(chan struct{})}
	go func() {
		watcher.result.code, watcher.result.err = process.Wait()
		close(watcher.done)
	}()
	return watcher
}

func adapterExitBeforeSettled(waitDone *adapterProcessWatcher) (string, error) {
	select {
	case <-waitDone.done:
		return fmt.Sprint(waitDone.result.code), waitDone.result.err
	case <-time.After(time.Second):
		return "unknown", nil
	}
}

func adapterFinishProcess(process Process, waitDone *adapterProcessWatcher) {
	if process == nil {
		return
	}
	if stdin := process.Stdin(); stdin != nil {
		_ = stdin.Close()
	}
	select {
	case <-waitDone.done:
		return
	default:
	}
	if _, exited := process.Poll(); !exited {
		_ = process.Terminate()
	}
	select {
	case <-waitDone.done:
		return
	case <-time.After(2 * time.Second):
	}
	_ = process.Kill()
	select {
	case <-waitDone.done:
	case <-time.After(2 * time.Second):
	}
}

type adapterStderrCapture struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (c *adapterStderrCapture) add(chunk []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := adapterPiStderrLimit - len(c.data)
	if room > 0 {
		if len(chunk) > room {
			c.data = append(c.data, chunk[:room]...)
		} else {
			c.data = append(c.data, chunk...)
		}
	}
	if len(chunk) > room {
		c.truncated = true
	}
}

func (c *adapterStderrCapture) value() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	detail := bytes.ToValidUTF8(c.data, []byte("�"))
	truncated := c.truncated
	if len(detail) > adapterPiStderrLimit {
		detail = detail[:adapterPiStderrLimit]
		for !utf8.Valid(detail) {
			detail = detail[:len(detail)-1]
		}
		truncated = true
	}
	return strings.TrimSpace(string(detail)), truncated
}

func adapterDrainStderr(reader io.Reader, capture *adapterStderrCapture) {
	buffer := make([]byte, 4096)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			capture.add(buffer[:n])
		}
		if err != nil {
			return
		}
	}
}

func adapterWriteJSONL(mu *sync.Mutex, writer io.Writer, record Record) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	mu.Lock()
	defer mu.Unlock()
	for len(encoded) > 0 {
		n, err := writer.Write(encoded)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		encoded = encoded[n:]
	}
	return nil
}

func adapterEmit(handler EventHandler, kind, agentID, parentID string, fields Record) {
	if handler == nil {
		return
	}
	record := Record{"type": kind, "agent_id": agentID, "parent_id": nil}
	if parentID != "" {
		record["parent_id"] = parentID
	}
	for key, value := range fields {
		record[key] = value
	}
	handler(record)
}

func adapterStopContainer(runtime Runtime, container string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = runtime.Stop(ctx, container)
}

func adapterRandomPassword() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

func adapterOpenCodeURL(runtime Runtime, container string, port int) (string, error) {
	address, err := runtime.Address(container)
	if err != nil {
		return "", fmt.Errorf("resolve OpenCode container address: %w", err)
	}
	parsed := net.ParseIP(strings.Trim(address, "[]"))
	if parsed == nil {
		return "", errors.New("OpenCode container address must be an IP literal")
	}
	host := net.JoinHostPort(parsed.String(), fmt.Sprint(port))
	return "http://" + host, nil
}

func adapterOpenCodeClient() *http.Client {
	return &http.Client{
		Transport:     &http.Transport{Proxy: nil, ForceAttemptHTTP2: false, MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func adapterOpenCodeRequest(ctx context.Context, client *http.Client, baseURL, password, method, path string, body any) (Record, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("create OpenCode API request: %w", err)
	}
	request.SetBasicAuth("opencode", password)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, errors.New("OpenCode API redirects are not allowed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("OpenCode API returned HTTP status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, adapterOCBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read OpenCode API response: %w", err)
	}
	if len(data) > adapterOCBodyLimit {
		return nil, errors.New("OpenCode API response exceeded size limit")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var value any
	if err := decodeJSON(data, &value); err != nil {
		return nil, fmt.Errorf("invalid OpenCode JSON response: %w", err)
	}
	result, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("OpenCode API response must be a JSON object")
	}
	return result, nil
}

func adapterRetryableOpenCodeError(err error) bool {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		if urlError.Err == context.Canceled || urlError.Err == context.DeadlineExceeded {
			return false
		}
		return true
	}
	var netError net.Error
	return errors.As(err, &netError)
}

func adapterReadOpenCodeEvents(ctx context.Context, client *http.Client, baseURL, password string, events chan<- adapterOpenCodeRead, ready chan<- error, ending <-chan struct{}) {
	readySent := false
	sendReady := func(err error) {
		if !readySent {
			ready <- err
			readySent = true
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/event", nil)
	if err != nil {
		sendReady(err)
		return
	}
	request.SetBasicAuth("opencode", password)
	request.Header.Set("Accept", "text/event-stream")
	response, err := client.Do(request)
	if err != nil {
		sendReady(err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		err := fmt.Errorf("OpenCode event stream returned HTTP status %d", response.StatusCode)
		sendReady(err)
		return
	}
	sendReady(nil)
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), adapterOCBodyLimit)
	var data []string
	for scanner.Scan() {
		select {
		case <-ending:
			return
		case <-ctx.Done():
			return
		default:
		}
		line := strings.TrimSuffix(strings.TrimSuffix(scanner.Text(), "\n"), "\r")
		if line == "" {
			if len(data) > 0 {
				var record Record
				if err := decodeJSON([]byte(strings.Join(data, "\n")), &record); err != nil || record == nil {
					if err == nil {
						err = errors.New("OpenCode SSE data must be an object")
					}
					if !adapterSendOpenCodeRead(ctx, ending, events, adapterOpenCodeRead{err: err}) {
						return
					}
				} else if !adapterSendOpenCodeRead(ctx, ending, events, adapterOpenCodeRead{record: record}) {
					return
				}
				data = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		_ = adapterSendOpenCodeRead(ctx, ending, events, adapterOpenCodeRead{err: err})
		return
	}
	select {
	case <-ending:
	case <-ctx.Done():
	default:
		_ = adapterSendOpenCodeRead(ctx, ending, events, adapterOpenCodeRead{err: errors.New("OpenCode event stream closed before completion")})
	}
}

func adapterSendOpenCodeRead(ctx context.Context, ending <-chan struct{}, events chan<- adapterOpenCodeRead, event adapterOpenCodeRead) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	case <-ending:
		return false
	}
}

func adapterStringLists(value any) ([][]string, bool) {
	switch lists := value.(type) {
	case [][]string:
		return lists, true
	case []any:
		result := make([][]string, len(lists))
		for i, item := range lists {
			var values []any
			switch typed := item.(type) {
			case []any:
				values = typed
			case []string:
				result[i] = append([]string(nil), typed...)
				continue
			default:
				return nil, false
			}
			for _, entry := range values {
				str, ok := entry.(string)
				if !ok {
					return nil, false
				}
				result[i] = append(result[i], str)
			}
		}
		return result, true
	default:
		return nil, false
	}
}
