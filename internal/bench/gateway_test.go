package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var gatewayUsage = map[string]any{
	"prompt_tokens": 12, "completion_tokens": 3, "cost": .02,
	"prompt_tokens_details":     map[string]any{"cached_tokens": 4},
	"completion_tokens_details": map[string]any{"reasoning_tokens": 1},
}

func gatewayForTest(t *testing.T, apiKey string, upstream string, options GatewayOptions) *Gateway {
	t.Helper()
	options.Upstream = upstream
	gateway, err := NewGateway(apiKey, options)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	t.Cleanup(func() {
		if err := gateway.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return gateway
}

func gatewayRequest(t *testing.T, gateway *Gateway, method, path string, body io.Reader, headers http.Header) (*http.Response, []byte, error) {
	t.Helper()
	req, err := http.NewRequest(method, gateway.BaseURL()+strings.TrimPrefix(path, "/v1"), body)
	if err != nil {
		return nil, nil, err
	}
	if headers != nil {
		req.Header = headers.Clone()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp, data, err
}

func gatewayAuth(gateway *Gateway) http.Header {
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+gateway.Token)
	return header
}

func gatewayRawStatus(t *testing.T, gateway *Gateway, request string) int {
	t.Helper()
	base, err := url.Parse(gateway.BaseURL())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", base.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, request, gateway.Token); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read raw request response: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func gatewayJSON(t *testing.T, gateway *Gateway, payload any, headers http.Header) (*http.Response, []byte) {
	t.Helper()
	if headers == nil {
		headers = gatewayAuth(gateway)
	} else if headers.Get("Authorization") == "" {
		headers = headers.Clone()
		headers.Set("Authorization", "Bearer "+gateway.Token)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, body, err := gatewayRequest(t, gateway, http.MethodPost, "/v1/chat/completions", strings.NewReader(string(data)), headers)
	if err != nil {
		t.Fatalf("gateway request: %v", err)
	}
	return resp, body
}

func gatewayAttempts(t *testing.T, gateway *Gateway, count int) []Record {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		records := gateway.Attempts()
		if len(records) == count {
			pending := false
			for _, record := range records {
				pending = pending || record["pending"] == true
			}
			if !pending {
				return records
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempts did not settle: %#v", records)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGatewayForwardAccountsAndEnforcesCostLimit(t *testing.T) {
	var calls atomic.Int32
	var gotPath, gotAuthorization string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotPath, gotAuthorization = r.URL.Path, r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/chat/completions" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		var payload Record
		if err := decodeJSON(mustReadAll(r.Body), &payload); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		if text(payload["model"]) != "requested/model" {
			t.Errorf("forwarded model = %#v", payload["model"])
		}
		w.Header().Set("X-Generation-Id", "header-generation")
		_ = json.NewEncoder(w).Encode(Record{"id": "gen-1", "model": "actual/model", "usage": gatewayUsage})
	}))
	defer upstream.Close()
	costLimit := .02
	gateway := gatewayForTest(t, "private-key", upstream.URL+"/api/v1", GatewayOptions{CostLimit: &costLimit})

	headers := gatewayAuth(gateway)
	headers.Set("X-AQP-Agent-ID", "child")
	headers.Set("X-AQP-Parent-ID", "main")
	resp, _ := gatewayJSON(t, gateway, Record{"model": "requested/model"}, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotPath != "/api/v1/chat/completions" || gotAuthorization != "Bearer private-key" {
		t.Fatalf("upstream received path %q and authorization %q", gotPath, gotAuthorization)
	}
	records := gatewayAttempts(t, gateway, 1)
	record := records[0]
	if text(record["agent_id"]) != "child" || text(record["parent_id"]) != "main" || text(record["provider_request_id"]) != "gen-1" {
		t.Fatalf("unexpected attribution or provider ID: %#v", record)
	}
	if number(record["input_tokens"]) != 12 || number(record["output_tokens"]) != 3 || number(record["cost_usd"]) != .02 {
		t.Fatalf("usage not accounted: %#v", record)
	}
	if number(record["cached_tokens"]) != 4 || number(record["reasoning_tokens"]) != 1 || record["usage_complete"] != true {
		t.Fatalf("usage details missing: %#v", record)
	}
	if text(record["model"]) != "actual/model" || number(record["duration_seconds"]) < 0 {
		t.Fatalf("provider model or duration missing: %#v", record)
	}
	if !gateway.LimitReached() {
		t.Fatal("known cost at limit did not stop new completions")
	}
	blocked, body := gatewayJSON(t, gateway, Record{"model": "other/model"}, nil)
	if blocked.StatusCode != http.StatusPaymentRequired || !strings.Contains(string(body), "Run cost limit reached") || calls.Load() != 1 {
		t.Fatalf("cost limit response = %d %s, upstream calls = %d", blocked.StatusCode, body, calls.Load())
	}
	if strings.Contains(fmt.Sprint(gateway.Attempts()), "private-key") {
		t.Fatal("upstream credential leaked into attempts")
	}
	record["cost_usd"] = 100
	if number(gateway.Attempts()[0]["cost_usd"]) != .02 {
		t.Fatal("attempt snapshots expose mutable gateway state")
	}
}

func TestGatewayPendingAttemptAndSnapshotIsolation(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(Record{"usage": gatewayUsage})
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{})
	requestDone := make(chan error, 1)
	go func() {
		_, _, err := gatewayRequest(t, gateway, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`), gatewayAuth(gateway))
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not receive the completion")
	}
	pending := gateway.Attempts()
	if len(pending) != 1 || pending[0]["pending"] != true || pending[0]["cost_usd"] != nil {
		t.Fatalf("pending attempt should have unknown billing: %#v", pending)
	}
	id := text(pending[0]["attempt_id"])
	pending[0]["agent_id"] = "mutated"
	if text(gateway.Attempts()[0]["agent_id"]) == "mutated" {
		t.Fatal("pending attempt snapshot was not isolated")
	}
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatalf("completion request: %v", err)
	}
	settled := gatewayAttempts(t, gateway, 1)[0]
	if text(settled["attempt_id"]) != id || settled["pending"] != false || number(settled["cost_usd"]) != .02 {
		t.Fatalf("pending record was not finalized in place: %#v", settled)
	}
}

func TestGatewayCapabilityRoutesAndModelDiscovery(t *testing.T) {
	var upstreamPaths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer actual-key" {
			t.Errorf("upstream authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "actual-key", upstream.URL+"/api/v1", GatewayOptions{})
	unauthorized := make(http.Header)
	resp, _, err := gatewayRequest(t, gateway, http.MethodGet, "/v1/models", nil, unauthorized)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing capability = %v, %v", resp, err)
	}
	for _, path := range []string{"/v1/chat/completions?url=http://bad", "/other", "http://bad/v1/models"} {
		resp, _, err = gatewayRequest(t, gateway, http.MethodPost, path, strings.NewReader(`{}`), gatewayAuth(gateway))
		if err != nil || resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unsupported route %q = %v, %v", path, resp, err)
		}
	}
	resp, _, err = gatewayRequest(t, gateway, http.MethodGet, "/v1/models", nil, gatewayAuth(gateway))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("model discovery = %v, %v", resp, err)
	}
	if fmt.Sprint(upstreamPaths) != "[/api/v1/models]" || len(gateway.Attempts()) != 0 {
		t.Fatalf("discovery created completion attempts: paths=%v attempts=%#v", upstreamPaths, gateway.Attempts())
	}
}

func TestGatewaySSEIsLiveAndUsageFramesReplace(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, ": keepalive\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		<-release
		frame, _ := json.Marshal(Record{"id": "gen-sse", "model": "m", "usage": gatewayUsage})
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", frame, frame)
		flusher.Flush()
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{})
	req, _ := http.NewRequest(http.MethodPost, gateway.BaseURL()+"/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header = gatewayAuth(gateway)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != ": keepalive\n" {
		_ = resp.Body.Close()
		t.Fatalf("first SSE line was not forwarded live: %q, %v", line, err)
	}
	close(release)
	rest, err := io.ReadAll(reader)
	_ = resp.Body.Close()
	if err != nil || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("SSE completion = %q, %v", rest, err)
	}
	record := gatewayAttempts(t, gateway, 1)[0]
	if number(record["cost_usd"]) != .02 || number(record["input_tokens"]) != 12 {
		t.Fatalf("usage frames were not parsed as a final total: %#v", record)
	}
}

func TestGatewayMissingUsageAndZeroCostLimit(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{})
	response, _ := gatewayJSON(t, gateway, Record{"model": "m"}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	record := gatewayAttempts(t, gateway, 1)[0]
	if record["cost_usd"] != nil || gateway.LimitReached() {
		t.Fatalf("missing billing was treated as known zero: %#v", record)
	}
	zero := 0.0
	blocked := gatewayForTest(t, "key", upstream.URL, GatewayOptions{CostLimit: &zero})
	response, body := gatewayJSON(t, blocked, Record{"model": "m"}, nil)
	if response.StatusCode != http.StatusPaymentRequired || !strings.Contains(string(body), "Run cost limit reached") || calls.Load() != 1 || len(blocked.Attempts()) != 0 {
		t.Fatalf("zero cost limit was not enforced before provider work: status=%d calls=%d", response.StatusCode, calls.Load())
	}
}

func TestGatewayTruncatedSSEIsInfrastructureFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n")
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{})
	response, _ := gatewayJSON(t, gateway, Record{"model": "m", "stream": true}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	record := gatewayAttempts(t, gateway, 1)[0]
	if record["error_kind"] != "network" || record["cost_usd"] != nil || !gateway.InfrastructureFailed() {
		t.Fatalf("truncated stream was not retained as infrastructure failure: %#v", record)
	}
}

func TestGatewayStreamErrorEventPreservesUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		payload, _ := json.Marshal(Record{"error": Record{"code": 503}, "usage": gatewayUsage})
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{})
	_, _ = gatewayJSON(t, gateway, Record{"model": "m", "stream": true}, nil)
	record := gatewayAttempts(t, gateway, 1)[0]
	if record["error_kind"] != "provider" || number(record["cost_usd"]) != .02 || !gateway.InfrastructureFailed() {
		t.Fatalf("SSE provider error discarded usage or infra state: %#v", record)
	}
}

func TestGatewaySSEFrameBound(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+strings.Repeat("x", maxGatewaySSEFrame)+"\n\n")
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{})
	response, _ := gatewayJSON(t, gateway, Record{"model": "m", "stream": true}, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	record := gatewayAttempts(t, gateway, 1)[0]
	if record["error_kind"] != "unknown" {
		t.Fatalf("oversized SSE frame was not bounded: %#v", record)
	}
}

func TestGatewayRejectsInvalidCompletionPayloadLocally(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{})
	for _, payload := range []string{`not-json`, `{}`, `{"model":1}`, `{"model":"m"} {}`} {
		response, _, err := gatewayRequest(t, gateway, http.MethodPost, "/v1/chat/completions", strings.NewReader(payload), gatewayAuth(gateway))
		if err != nil || response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid payload %q = %v, %v", payload, response, err)
		}
	}
	if calls.Load() != 0 || len(gateway.Attempts()) != 0 {
		t.Fatalf("invalid payload reached provider: calls=%d attempts=%#v", calls.Load(), gateway.Attempts())
	}
	if !strings.Contains(gateway.ContainerURL(), "host.docker.internal") {
		t.Fatalf("container URL did not use host-gateway alias: %q", gateway.ContainerURL())
	}
}

func TestGatewayProviderErrorsRedactAndClassify(t *testing.T) {
	for _, status := range []int{400, 429, 503, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			capability := ""
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := json.Marshal(Record{"error": Record{"message": "guardrail rejected", "token": "private-key", "capability": capability}})
				w.Header().Set("Location", "http://127.0.0.1:1/secret")
				w.Header().Set("X-Request-ID", "provider-id")
				w.WriteHeader(status)
				_, _ = w.Write(body)
			}))
			defer upstream.Close()
			gateway := gatewayForTest(t, "private-key", upstream.URL, GatewayOptions{})
			capability = gateway.Token
			resp, body := gatewayJSON(t, gateway, Record{"model": "m"}, nil)
			if resp.StatusCode != status || !strings.Contains(string(body), "guardrail rejected") || !strings.Contains(string(body), "[redacted]") {
				t.Fatalf("provider error response = %d %s", resp.StatusCode, body)
			}
			if strings.Contains(string(body), "private-key") || strings.Contains(string(body), capability) {
				t.Fatal("provider credentials leaked into error response")
			}
			record := gatewayAttempts(t, gateway, 1)[0]
			if record["error_kind"] != "provider" || strings.Contains(text(record["provider_error_body"]), "private-key") || strings.Contains(text(record["provider_error_body"]), capability) {
				t.Fatalf("provider diagnostic not sanitized: %#v", record)
			}
			if record["provider_error_truncated"] != false || record["cost_usd"] != nil || record["usage_complete"] != false {
				t.Fatalf("unexpected error accounting: %#v", record)
			}
			if gateway.InfrastructureFailed() != (status == 429 || status == 503) {
				t.Fatalf("infrastructure classification for %d = %t", status, gateway.InfrastructureFailed())
			}
		})
	}
}

func TestGatewayProviderErrorDiagnosticsBoundUTF8JSONAndCredentialBoundary(t *testing.T) {
	for _, diagnostic := range [][]byte{
		[]byte(`{"error":"` + strings.Repeat("x", 6000) + `"}`),
		[]byte(strings.Repeat("\x80", 5000)),
		[]byte(strings.Repeat("\x00", 5000)),
		append([]byte(strings.Repeat("x", 4090)), []byte("private-key")...),
	} {
		t.Run(fmt.Sprintf("bytes-%d", len(diagnostic)), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(diagnostic)
			}))
			defer upstream.Close()
			key := "private-key"
			gateway := gatewayForTest(t, key, upstream.URL, GatewayOptions{})
			resp, body := gatewayJSON(t, gateway, Record{"model": "m"}, nil)
			if resp.StatusCode != http.StatusBadRequest || len(body) > 4096 {
				t.Fatalf("bounded provider response status=%d bytes=%d", resp.StatusCode, len(body))
			}
			record := gatewayAttempts(t, gateway, 1)[0]
			stored := text(record["provider_error_body"])
			if len([]byte(stored)) > 4096 || !record["provider_error_truncated"].(bool) {
				t.Fatalf("diagnostic size/truncation incorrect: bytes=%d record=%#v", len([]byte(stored)), record)
			}
			if strings.Contains(stored, key) || strings.HasSuffix(stored, "privat") {
				t.Fatalf("credential crossed truncation boundary: %q", stored[len(stored)-min(32, len(stored)):])
			}
		})
	}
}

func TestGatewayNetworkFailureRetainsAttempt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstreamURL := upstream.URL
	upstream.Close()
	gateway := gatewayForTest(t, "key", upstreamURL, GatewayOptions{})
	resp, body := gatewayJSON(t, gateway, Record{"model": "m"}, nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "Upstream connection failed") {
		t.Fatalf("network failure = %d %s", resp.StatusCode, body)
	}
	record := gatewayAttempts(t, gateway, 1)[0]
	if record["error_kind"] != "network" || !gateway.InfrastructureFailed() {
		t.Fatalf("network failure was not retained: %#v", record)
	}
}

func TestGatewayRejectsUnsafeConfiguration(t *testing.T) {
	for _, upstream := range []string{
		"http://example.com/api/v1", "https://user:pass@example.com", "https://example.com/?x=1",
		"file:///tmp/x", "https://example.com/#x", "https://ex%61mple.com/path",
	} {
		t.Run(upstream, func(t *testing.T) {
			if _, err := NewGateway("key", GatewayOptions{Upstream: upstream}); err == nil {
				t.Fatalf("unsafe upstream accepted: %q", upstream)
			}
		})
	}
	for _, key := range []string{"", "bad\nkey", "bad\rkey"} {
		if _, err := NewGateway(key, GatewayOptions{Upstream: "https://example.com/api/v1"}); err == nil {
			t.Fatalf("invalid API key accepted: %q", key)
		}
	}
	for _, limit := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, err := NewGateway("key", GatewayOptions{CostLimit: &limit}); err == nil {
			t.Fatalf("invalid cost limit accepted: %v", limit)
		}
	}
}

func TestGatewayParallelAttemptsCallbackIsolationAndBoundedDrops(t *testing.T) {
	var callbackCalls atomic.Int32
	callback := func(record Record) {
		callbackCalls.Add(1)
		record["cost_usd"] = 999
		panic("callback failures must not break forwarding")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Record{"usage": gatewayUsage})
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{OnAttempt: callback})
	const requests = 8
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _, err := gatewayRequest(t, gateway, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`), gatewayAuth(gateway))
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Errorf("request = %v, %v", resp, err)
			}
		}()
	}
	wg.Wait()
	if records := gateway.Attempts(); len(records) != requests {
		t.Fatalf("record count = %d, want %d", len(records), requests)
	}
	for _, record := range gateway.Attempts() {
		if number(record["cost_usd"]) != .02 {
			t.Fatalf("callback mutated authoritative attempt: %#v", record)
		}
	}
}

func TestGatewayCallbackQueueDropCount(t *testing.T) {
	callbackStarted, release := make(chan struct{}), make(chan struct{})
	var callbacks atomic.Int32
	callback := func(Record) {
		if callbacks.Add(1) == 1 {
			close(callbackStarted)
			<-release
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Record{"usage": gatewayUsage})
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{OnAttempt: callback})
	for i := 0; i < maxGatewayNotifications+3; i++ {
		resp, _, err := gatewayRequest(t, gateway, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`), gatewayAuth(gateway))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d = %v, %v", i, resp, err)
		}
		if i == 0 {
			select {
			case <-callbackStarted:
			case <-time.After(time.Second):
				t.Fatal("callback did not start")
			}
		}
	}
	if gateway.NotificationsDropped() == 0 {
		t.Fatal("slow observer did not report bounded-queue drops")
	}
	close(release)
}

func TestGatewayRequestAndResponseBounds(t *testing.T) {
	upstreamCalls := atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if strings.Contains(r.URL.Path, "large") {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("x", 16*1024*1024+1)), 16*1024*1024+1)
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{})
	for _, request := range []string{
		"POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Length: 2\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}",
		"POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n2\r\n{}\r\n0\r\n\r\n",
	} {
		if status := gatewayRawStatus(t, gateway, request); status != http.StatusBadRequest {
			t.Fatalf("ambiguous or chunked request status = %d", status)
		}
	}
	// The server rejects from Content-Length before attempting to read the body.
	base, err := url.Parse(gateway.BaseURL())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", base.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", gateway.Token, 8*1024*1024+1)
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read request-bound response: %v", err)
	}
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status = %d", response.StatusCode)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	_ = conn.Close()
	if upstreamCalls.Load() != 0 || len(gateway.Attempts()) != 0 {
		t.Fatal("locally rejected request reached provider or registered an attempt")
	}

	resp, _, err := gatewayRequest(t, gateway, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`), gatewayAuth(gateway))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("large provider response status = %v, %v", resp, err)
	}
	record := gatewayAttempts(t, gateway, 1)[0]
	if record["error_kind"] != "unknown" {
		t.Fatalf("oversized non-stream response should be unknown: %#v", record)
	}
}

func TestGatewayConnectionCapIsThirtyTwo(t *testing.T) {
	started := make(chan struct{}, 33)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = io.WriteString(w, `{"usage":{"cost":0}}`)
	}))
	defer upstream.Close()
	gateway := gatewayForTest(t, "key", upstream.URL, GatewayOptions{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = gatewayRequest(t, gateway, http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`), gatewayAuth(gateway))
		}()
	}
	for i := 0; i < 32; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("fewer than 32 provider requests reached the upstream")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, gateway.BaseURL()+"/chat/completions", strings.NewReader(`{"model":"m"}`))
	request.Header = gatewayAuth(gateway)
	_, err := http.DefaultClient.Do(request)
	if err == nil {
		t.Fatal("33rd active connection was not rejected")
	}
	close(release)
	wg.Wait()
}

func mustReadAll(reader io.Reader) []byte {
	data, err := io.ReadAll(reader)
	if err != nil {
		panic(err)
	}
	return data
}
