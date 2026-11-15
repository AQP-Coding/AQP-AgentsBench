package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestDockerIntegrationGateway(t *testing.T) {
	if testing.Short() || os.Getenv("AQP_DOCKER_INTEGRATION") != "1" {
		t.Skip("set AQP_DOCKER_INTEGRATION=1 for the local Docker gateway probe")
	}
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer local-fake-key" {
			t.Error("gateway did not use the mock upstream credential")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"fixture/no-provider"}]}`)
		case "/chat/completions":
			_, _ = io.WriteString(w, `{"model":"fixture/no-provider","choices":[{"message":{"content":"MOCK_OK"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"cost":0}}`)
		default:
			t.Errorf("unexpected mock upstream route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	gateway := gatewayForTest(t, "local-fake-key", upstream.URL, GatewayOptions{Host: "0.0.0.0"})
	runtime, err := NewDockerRuntime()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	identity, err := runtime.Start(ctx, dockerIntegrationImage, t.TempDir(), StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dockerIntegrationCleanup(t, runtime, []string{identity}) })
	t.Logf("gateway probe container id=%s listener=%s", identity, gateway.listener.Addr())
	base, _ := json.Marshal(gateway.ContainerURL())
	token, _ := json.Marshal(gateway.Token)
	// Only Node's HTTP client runs: every destination is the local mock-backed gateway.
	script := fmt.Sprintf(`
const base = %s, token = %s;
const request = (path, options = {}) => fetch(base + path, {...options, signal: AbortSignal.timeout(5000)});
(async () => {
  const denied = await request('/models');
  await denied.text();
  const auth = {Authorization: 'Bearer ' + token};
  const models = await request('/models', {headers: auth});
  const modelBody = await models.json();
  const completion = await request('/chat/completions', {
    method: 'POST', headers: {...auth, 'Content-Type': 'application/json', 'X-AQP-Agent-ID': 'local-probe'},
    body: JSON.stringify({model: 'fixture/no-provider', messages: [{role: 'user', content: 'local probe'}]})
  });
  const body = await completion.json();
  const result = {denied: denied.status, models: models.status, completion: completion.status,
    model: modelBody.data?.[0]?.id, text: body.choices?.[0]?.message?.content};
  console.log(JSON.stringify(result));
  if (result.denied !== 401 || result.models !== 200 || result.completion !== 200 ||
      result.model !== 'fixture/no-provider' || result.text !== 'MOCK_OK') process.exitCode = 1;
})().catch(error => { console.error(error.cause?.code || error.name); process.exitCode = 1; });
`, base, token)
	result, err := runtime.Exec(ctx, identity, []string{"node", "-e", script}, 20*time.Second)
	if err != nil || result.ReturnCode != 0 {
		t.Fatalf("container-to-gateway probe: exit=%d stdout=%q stderr=%q error=%v", result.ReturnCode, result.Stdout, result.Stderr, err)
	}
	var response Record
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil ||
		number(response["denied"]) != 401 || number(response["models"]) != 200 ||
		number(response["completion"]) != 200 || response["text"] != "MOCK_OK" {
		t.Fatalf("unexpected probe response: %s (%v)", result.Stdout, err)
	}
	deadline := time.Now().Add(time.Second)
	attempts := gateway.Attempts()
	for len(attempts) == 1 && attempts[0]["pending"] == true && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		attempts = gateway.Attempts()
	}
	if upstreamCalls.Load() != 2 || len(attempts) != 1 || number(attempts[0]["status"]) != 200 ||
		attempts[0]["agent_id"] != "local-probe" || attempts[0]["usage_complete"] != true || number(attempts[0]["cost_usd"]) != 0 {
		t.Fatalf("incorrect forwarding/accounting: upstream calls=%d attempts=%v", upstreamCalls.Load(), attempts)
	}
	t.Logf("discovery/completion HTTP 200, unauthorized HTTP 401, completed attempts=%d", len(attempts))
}
