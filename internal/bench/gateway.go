package bench

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxGatewayRequest bounds client JSON before it can reach the provider.
	maxGatewayRequest = 8 * 1024 * 1024
	// maxGatewayResponse bounds nonstreaming provider JSON retained for forwarding and usage.
	maxGatewayResponse = 16 * 1024 * 1024
	// maxGatewayErrorBody bounds redacted diagnostics surfaced to clients and artifacts.
	maxGatewayErrorBody = 4096
	// maxGatewaySSEFrame bounds one event without buffering the complete SSE stream.
	maxGatewaySSEFrame = 1024 * 1024
	// maxGatewayConnections caps active sockets; excess connections are closed, not queued.
	maxGatewayConnections = 32
	// maxGatewayNotifications bounds best-effort observer delivery without blocking requests.
	maxGatewayNotifications = 128
	// gatewayIdleTimeout bounds inactivity, not the total lifetime of a streaming response.
	gatewayIdleTimeout = 30 * time.Second
)

// GatewayOptions configures one run-scoped, fixed-upstream model proxy.
type GatewayOptions struct {
	// Host is the listen address; empty selects loopback. Explicit IPv4 addresses
	// use IPv4 sockets; IPv6 addresses and hostnames retain normal TCP resolution.
	Host string
	// Port is the listen port; zero requests an ephemeral port.
	Port int
	// Upstream is fixed for the gateway lifetime; empty selects OpenRouter.
	Upstream string
	// OnAttempt observes accounting updates through a bounded, best-effort queue.
	// Nil disables notifications; slow callbacks never block model forwarding.
	OnAttempt func(Record)
	// CostLimit caps admission using known billed cost. Nil disables the limit;
	// zero rejects requests immediately. In-flight requests may overshoot it.
	CostLimit *float64
}

// Gateway forwards only the supported model routes and retains bounded
// accounting for submitted completions. Token is a local bearer capability;
// the provider API key is never made available to clients of the gateway.
type Gateway struct {
	// Token authenticates local clients, not upstream requests. Treat it as read-only.
	Token string

	apiKey        string
	upstream      *url.URL
	client        *http.Client
	transport     *http.Transport
	listener      *limitedListener
	server        *http.Server
	baseURL       string
	containerURL  string
	callback      func(Record)
	notifications chan Record
	closed        chan struct{}
	serveDone     chan struct{}
	closeOnce     sync.Once

	mu        sync.RWMutex
	records   []Record
	pending   map[string]Record
	knownCost float64
	costLimit *float64
	limit     atomic.Bool
	infrastr  atomic.Bool
	dropped   atomic.Int64
}

// NewGateway binds and starts the gateway before returning. Empty Host,
// Upstream, and Port select the loopback address, default OpenRouter endpoint,
// and an ephemeral local port respectively.
func NewGateway(apiKey string, options GatewayOptions) (*Gateway, error) {
	if apiKey == "" || strings.ContainsAny(apiKey, "\r\n") {
		return nil, errors.New("an explicit nonempty API key is required")
	}
	if options.CostLimit != nil && (math.IsNaN(*options.CostLimit) || math.IsInf(*options.CostLimit, 0) || *options.CostLimit < 0) {
		return nil, errors.New("cost limit must be finite and nonnegative")
	}
	if options.Port < 0 || options.Port > 65535 {
		return nil, errors.New("invalid gateway port")
	}
	upstreamText := options.Upstream
	if upstreamText == "" {
		upstreamText = "https://openrouter.ai/api/v1"
	}
	upstream, err := validateGatewayUpstream(upstreamText)
	if err != nil {
		return nil, err
	}
	host := options.Host
	if host == "" {
		host = "127.0.0.1"
	}

	token, err := gatewayCapability()
	if err != nil {
		return nil, fmt.Errorf("create gateway capability: %w", err)
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		DisableKeepAlives:     true,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: gatewayIdleTimeout,
		TLSHandshakeTimeout:   gatewayIdleTimeout,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: gatewayIdleTimeout, KeepAlive: gatewayIdleTimeout}
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return idleTimeoutConn{Conn: conn, timeout: gatewayIdleTimeout}, nil
		},
	}
	g := &Gateway{
		Token:         token,
		apiKey:        apiKey,
		upstream:      upstream,
		transport:     transport,
		client:        &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		callback:      options.OnAttempt,
		notifications: make(chan Record, maxGatewayNotifications),
		closed:        make(chan struct{}),
		serveDone:     make(chan struct{}),
		pending:       make(map[string]Record),
		costLimit:     options.CostLimit,
	}
	if options.CostLimit != nil && *options.CostLimit == 0 {
		g.limit.Store(true)
	}
	network := "tcp"
	if address, err := netip.ParseAddr(host); err == nil && address.Is4() {
		// A dual-stack wildcard can bypass Docker Desktop's WSL IPv4 forwarding.
		network = "tcp4"
	}
	listener, err := net.Listen(network, net.JoinHostPort(host, strconv.Itoa(options.Port)))
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("listen for gateway: %w", err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		transport.CloseIdleConnections()
		return nil, errors.New("gateway listener did not bind a TCP address")
	}
	port := strconv.Itoa(addr.Port)
	g.baseURL = "http://" + net.JoinHostPort(host, port) + "/v1"
	g.containerURL = "http://" + net.JoinHostPort("host.docker.internal", port) + "/v1"
	g.listener = &limitedListener{Listener: listener, slots: make(chan struct{}, maxGatewayConnections)}
	g.server = &http.Server{
		Handler:           g,
		ReadHeaderTimeout: gatewayIdleTimeout,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	if g.callback != nil {
		go g.notifyLoop()
	}
	go func() {
		defer close(g.serveDone)
		_ = g.server.Serve(g.listener)
	}()
	return g, nil
}

func gatewayCapability() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// Close stops accepting requests and closes active connections without
// waiting for a user callback that may be blocked.
func (g *Gateway) Close() error {
	var closeErr error
	g.closeOnce.Do(func() {
		close(g.closed)
		closeErr = g.server.Close()
		g.transport.CloseIdleConnections()
		<-g.serveDone
	})
	return closeErr
}

// BaseURL returns the loopback or configured local address ending in /v1.
func (g *Gateway) BaseURL() string { return g.baseURL }

// ContainerURL returns the same listener using Docker's host-gateway name.
func (g *Gateway) ContainerURL() string { return g.containerURL }

// Attempts returns isolated copies of completed and currently pending model
// attempts. Callers may mutate the returned records without changing state.
func (g *Gateway) Attempts() []Record {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Record, 0, len(g.records)+len(g.pending))
	for _, record := range g.records {
		out = append(out, cloneRecord(record))
	}
	for _, record := range g.pending {
		out = append(out, cloneRecord(record))
	}
	return out
}

// LimitReached reports whether observed billing has reached the configured
// best-effort spend limit. Unknown billing is not treated as zero cost.
func (g *Gateway) LimitReached() bool { return g.limit.Load() }

// InfrastructureFailed reports whether provider transport or infrastructure errors occurred.
func (g *Gateway) InfrastructureFailed() bool { return g.infrastr.Load() }

// NotificationsDropped returns the number of observer callbacks dropped because
// the bounded notification queue was full.
func (g *Gateway) NotificationsDropped() int { return int(g.dropped.Load()) }

// ServeHTTP checks the run capability and accepts only fixed-upstream model routes.
// Client credentials, arbitrary targets, and unsupported query parameters are not forwarded.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	values := r.Header.Values("Authorization")
	want := []byte("Bearer " + g.Token)
	if len(values) != 1 || !hmac.Equal([]byte(values[0]), want) {
		writeGatewayError(w, http.StatusUnauthorized, "Gateway capability required")
		return
	}
	completion := r.Method == http.MethodPost && r.URL.RequestURI() == "/v1/chat/completions"
	discovery := r.Method == http.MethodGet && r.URL.RequestURI() == "/v1/models"
	if !completion && !discovery {
		writeGatewayError(w, http.StatusNotFound, "Unsupported gateway route")
		return
	}
	if completion && g.limit.Load() {
		writeGatewayError(w, http.StatusPaymentRequired, "Run cost limit reached")
		return
	}
	lengths := r.Header.Values("Content-Length")
	if len(r.TransferEncoding) != 0 || len(lengths) > 1 || r.ContentLength < 0 {
		writeGatewayError(w, http.StatusBadRequest, "Valid Content-Length required")
		return
	}
	length := r.ContentLength
	if len(lengths) == 1 {
		parsed, err := strconv.ParseInt(strings.TrimSpace(lengths[0]), 10, 64)
		if err != nil || parsed < 0 || parsed != r.ContentLength {
			writeGatewayError(w, http.StatusBadRequest, "Valid Content-Length required")
			return
		}
		length = parsed
	}
	if length > maxGatewayRequest {
		writeGatewayError(w, http.StatusRequestEntityTooLarge, "Request exceeds 8 MiB")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxGatewayRequest+1))
	if err != nil {
		writeGatewayError(w, http.StatusBadRequest, "Valid Content-Length required")
		return
	}
	if len(body) > maxGatewayRequest {
		writeGatewayError(w, http.StatusRequestEntityTooLarge, "Request exceeds 8 MiB")
		return
	}
	payload := Record{}
	if completion {
		if int64(len(body)) != length || decodeJSON(body, &payload) != nil || text(payload["model"]) == "" && !isString(payload["model"]) {
			writeGatewayError(w, http.StatusBadRequest, "JSON object with model required")
			return
		}
		if _, exists := payload["model"]; !exists || !isString(payload["model"]) {
			writeGatewayError(w, http.StatusBadRequest, "JSON object with model required")
			return
		}
	}
	g.forward(w, r, body, payload, completion)
}

func (g *Gateway) forward(w http.ResponseWriter, incoming *http.Request, body []byte, payload Record, completion bool) {
	started := time.Now()
	var record Record
	if completion {
		record = g.newAttempt(incoming, payload)
		defer func() { g.finish(record, started) }()
	}

	suffix := "/models"
	if completion {
		suffix = "/chat/completions"
	}
	target := *g.upstream
	target.Path = strings.TrimRight(target.Path, "/") + suffix
	target.RawPath = ""
	var requestBody io.Reader
	if completion {
		requestBody = bytes.NewReader(body)
	}
	upstreamRequest, err := http.NewRequestWithContext(incoming.Context(), incoming.Method, target.String(), requestBody)
	if err != nil {
		g.failNetwork(record)
		writeGatewayError(w, http.StatusBadGateway, "Upstream connection failed")
		return
	}
	upstreamRequest.Header.Set("Authorization", "Bearer "+g.apiKey)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("Accept-Encoding", "identity")
	if completion {
		upstreamRequest.ContentLength = int64(len(body))
	}
	response, err := g.client.Do(upstreamRequest)
	if err != nil {
		g.failNetwork(record)
		writeGatewayError(w, http.StatusBadGateway, "Upstream connection failed")
		return
	}
	defer response.Body.Close()
	if record != nil {
		record["status"] = response.StatusCode
		identifier := response.Header.Get("X-Generation-Id")
		if identifier == "" {
			identifier = response.Header.Get("X-Request-ID")
		}
		if identifier != "" {
			record["provider_request_id"] = truncateRunes(g.safe(identifier), 512)
		}
	}
	if response.StatusCode >= 300 {
		g.forwardProviderError(w, response, record)
		return
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		g.forwardSSE(w, response.Body, record, response.StatusCode)
		return
	}
	g.forwardJSON(w, response, record)
}

func (g *Gateway) newAttempt(request *http.Request, payload Record) Record {
	model := text(payload["model"])
	agentID := request.Header.Get("X-AQP-Agent-ID")
	if agentID == "" {
		agentID = "unattributed"
	}
	record := Record{
		"attempt_id":     newID(),
		"agent_id":       truncateRunes(g.safe(agentID), 512),
		"parent_id":      nil,
		"model":          nil,
		"status":         nil,
		"error_kind":     nil,
		"input_tokens":   nil,
		"output_tokens":  nil,
		"cost_usd":       nil,
		"usage_complete": false,
	}
	if model != "" {
		record["model"] = truncateRunes(g.safe(model), 512)
	}
	if parent := request.Header.Get("X-AQP-Parent-ID"); parent != "" {
		record["parent_id"] = truncateRunes(g.safe(parent), 512)
	}
	pending := cloneRecord(record)
	pending["pending"] = true
	pending["duration_seconds"] = 0.0
	g.mu.Lock()
	g.pending[text(record["attempt_id"])] = pending
	g.mu.Unlock()
	return record
}

func (g *Gateway) observe(record Record, value any) {
	if record == nil {
		return
	}
	data := object(value)
	if data == nil {
		return
	}
	for source, dest := range map[string]string{"id": "provider_request_id", "model": "model"} {
		if value, ok := data[source].(string); ok {
			record[dest] = truncateRunes(g.safe(value), 512)
		}
	}
	if truthy(data["error"]) {
		record["error_kind"] = "provider"
		if rawCode, ok := object(data["error"])["code"]; ok {
			code := number(rawCode)
			if code == 429 || integer(rawCode) && code >= 500 {
				g.infrastr.Store(true)
			}
		}
	}
	usage := object(data["usage"])
	if usage == nil {
		return
	}
	for source, dest := range map[string]string{"prompt_tokens": "input_tokens", "completion_tokens": "output_tokens", "cost": "cost_usd"} {
		record[dest] = safeUsageNumber(usage[source])
	}
	for detail, field := range map[string]string{"prompt_tokens_details": "cached_tokens", "completion_tokens_details": "reasoning_tokens"} {
		if values := object(usage[detail]); values != nil {
			record[field] = safeUsageNumber(values[field])
		}
	}
	record["usage_complete"] = record["input_tokens"] != nil && record["output_tokens"] != nil && record["cost_usd"] != nil
}

func (g *Gateway) safe(value string) string {
	secrets := []string{g.apiKey, g.Token}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	value = strings.ToValidUTF8(value, "\uFFFD")
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
}

func (g *Gateway) finish(record Record, started time.Time) {
	if record == nil {
		return
	}
	record["duration_seconds"] = time.Since(started).Seconds()
	record["pending"] = false
	key := text(record["attempt_id"])
	g.mu.Lock()
	delete(g.pending, key)
	g.records = append(g.records, cloneRecord(record))
	if cost, ok := record["cost_usd"]; ok && cost != nil {
		g.knownCost += number(cost)
		if g.costLimit != nil && g.knownCost >= *g.costLimit {
			g.limit.Store(true)
		}
	}
	g.mu.Unlock()
	if g.callback != nil {
		select {
		case g.notifications <- cloneRecord(record):
		default:
			g.dropped.Add(1)
		}
	}
}

func (g *Gateway) failNetwork(record Record) {
	g.infrastr.Store(true)
	if record != nil {
		record["error_kind"] = "network"
	}
}

func (g *Gateway) forwardProviderError(w http.ResponseWriter, response *http.Response, record Record) {
	if record != nil {
		record["error_kind"] = "provider"
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		g.infrastr.Store(true)
	}
	longestSecret := max(len(g.apiKey), len(g.Token))
	scanLimit := maxGatewayErrorBody + min(longestSecret, maxGatewayErrorBody) + 1
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(scanLimit)))
	if err != nil {
		g.failNetwork(record)
		writeGatewayError(w, http.StatusBadGateway, "Upstream connection failed")
		return
	}
	truncated := len(raw) > maxGatewayErrorBody
	var diagnostic string
	if truncated && longestSecret > maxGatewayErrorBody {
		diagnostic = "[provider error body omitted: credential exceeds diagnostic scan bound]"
	} else {
		diagnostic = g.safe(strings.ToValidUTF8(string(raw), "\uFFFD"))
		if len([]byte(diagnostic)) > maxGatewayErrorBody {
			diagnostic = truncateUTF8(diagnostic, maxGatewayErrorBody)
			truncated = true
		}
	}
	if record != nil {
		record["provider_error_body"] = diagnostic
		record["provider_error_truncated"] = truncated
		var data Record
		if decodeJSON([]byte(diagnostic), &data) == nil {
			g.observe(record, data)
		}
	}
	var clientBody []byte
	contentType := "application/json"
	if truncated {
		clientBody = boundedProviderErrorJSON(diagnostic)
	} else {
		clientBody = []byte(diagnostic)
		if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
			contentType = "text/plain; charset=utf-8"
		}
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(clientBody)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	if err := writeGatewayChunk(w, clientBody, false); err != nil && record != nil {
		record["error_kind"] = "client_disconnect"
	}
}

func (g *Gateway) forwardJSON(w http.ResponseWriter, response *http.Response, record Record) {
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_ = http.NewResponseController(w).Flush()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxGatewayResponse+1))
	if err != nil {
		g.failNetwork(record)
		return
	}
	if len(data) > maxGatewayResponse {
		if record != nil {
			record["error_kind"] = "unknown"
		}
		return
	}
	if record != nil {
		var value any
		if decodeJSON(data, &value) != nil {
			record["error_kind"] = "unknown"
		} else {
			g.observe(record, value)
		}
	}
	safe := []byte(g.safe(strings.ToValidUTF8(string(data), "\uFFFD")))
	if err := writeGatewayChunk(w, safe, true); err != nil && record != nil {
		record["error_kind"] = "client_disconnect"
	}
}

func (g *Gateway) forwardSSE(w http.ResponseWriter, source io.Reader, record Record, status int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := http.NewResponseController(w).Flush(); err != nil {
		if record != nil {
			record["error_kind"] = "client_disconnect"
		}
		return
	}
	reader := bufio.NewReaderSize(source, 32*1024)
	frame := make([][]byte, 0, 2)
	frameSize := 0
	done := false
	for {
		line, err := readGatewayLine(reader, maxGatewaySSEFrame+1)
		if errors.Is(err, errGatewaySSELineTooLarge) {
			if record != nil {
				record["error_kind"] = "unknown"
			}
			return
		}
		if len(line) == 0 && err != nil {
			if err != io.EOF {
				g.failNetwork(record)
				return
			}
			break
		}
		frameSize += len(line)
		if frameSize > maxGatewaySSEFrame {
			if record != nil {
				record["error_kind"] = "unknown"
			}
			return
		}
		forwarded := []byte(g.safe(strings.ToValidUTF8(string(line), "\uFFFD")))
		if writeGatewayChunk(w, forwarded, true) != nil {
			if record != nil {
				record["error_kind"] = "client_disconnect"
			}
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			if len(frame) == 1 && bytes.Equal(frame[0], []byte("[DONE]")) {
				done = true
			} else if len(frame) != 0 {
				joined := bytes.Join(frame, []byte("\n"))
				var value any
				if decodeJSON(joined, &value) == nil {
					g.observe(record, value)
				}
			}
			frame = frame[:0]
			frameSize = 0
		} else if bytes.HasPrefix(line, []byte("data:")) {
			frame = append(frame, bytes.TrimSpace(line[len("data:"):]))
		}
		if err == io.EOF {
			break
		}
	}
	if !done && (record == nil || record["error_kind"] != "provider") {
		g.failNetwork(record)
	}
}

func (g *Gateway) notifyLoop() {
	for {
		select {
		case <-g.closed:
			return
		case record := <-g.notifications:
			func() {
				defer func() { _ = recover() }()
				g.callback(cloneRecord(record))
			}()
		}
	}
}

func validateGatewayUpstream(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid upstream URL")
	}
	host := target.Hostname()
	loopback := net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	if (target.Scheme != "https" && target.Scheme != "http") || host == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || strings.ContainsAny(raw, "\r\n\\") || strings.Contains(target.Host, "%") || target.Scheme == "http" && !loopback {
		return nil, errors.New("upstream requires HTTPS, or loopback HTTP for tests, without credentials/query/fragment")
	}
	if _, err := strconv.Atoi(target.Port()); target.Port() != "" && err != nil {
		return nil, errors.New("invalid upstream port")
	}
	if target.Port() != "" {
		port, _ := strconv.Atoi(target.Port())
		if port < 0 || port > 65535 {
			return nil, errors.New("invalid upstream port")
		}
	}
	return target, nil
}

func writeGatewayError(w http.ResponseWriter, status int, message string) {
	body := []byte(`{"error":"` + message + `"}`)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeGatewayChunk(w http.ResponseWriter, data []byte, flush bool) error {
	if len(data) != 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
	}
	if flush {
		return http.NewResponseController(w).Flush()
	}
	return nil
}

func readGatewayLine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > limit {
			return nil, errGatewaySSELineTooLarge
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

var errGatewaySSELineTooLarge = errors.New("SSE line exceeds frame bound")

func boundedProviderErrorJSON(diagnostic string) []byte {
	runes := []rune(diagnostic)
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		candidate, _ := json.Marshal(Record{
			"error":               "Provider error response truncated",
			"provider_error_body": string(runes[:middle]),
		})
		if len(candidate) <= maxGatewayErrorBody {
			low = middle
		} else {
			high = middle - 1
		}
	}
	body, _ := json.Marshal(Record{
		"error":               "Provider error response truncated",
		"provider_error_body": string(runes[:low]),
	})
	return body
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !bytes.Equal([]byte(strings.ToValidUTF8(value, "")), []byte(value)) {
		value = value[:len(value)-1]
	}
	return value
}

func safeUsageNumber(value any) any {
	if !numeric(value) {
		return nil
	}
	n := number(value)
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	return n
}

func truthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case json.Number:
		n, _ := value.Float64()
		return n != 0
	case float64:
		return value != 0
	case int:
		return value != 0
	case map[string]any:
		return len(value) != 0
	case []any:
		return len(value) != 0
	default:
		return true
	}
}

func isString(value any) bool {
	_, ok := value.(string)
	return ok
}

func truncateRunes(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return value
}

type idleTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

// Read refreshes the idle read deadline so progress, rather than total stream duration,
// determines when a slow connection times out.
func (c idleTimeoutConn) Read(data []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(data)
}

// Write refreshes the idle write deadline before forwarding bytes.
func (c idleTimeoutConn) Write(data []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(data)
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
}

// Accept admits connections while slots are available and closes excess sockets
// instead of blocking request-serving goroutines on a slot.
func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &limitedConn{Conn: idleTimeoutConn{Conn: conn, timeout: gatewayIdleTimeout}, release: func() { <-l.slots }}, nil
		default:
			_ = conn.Close()
		}
	}
}

type limitedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

// Close closes the connection and releases its admission slot exactly once.
func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
