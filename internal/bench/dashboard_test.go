package bench

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDashboardLiveReadOnlyAndSafeDisplay(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Save(Record{"id": "run", "status": "running", "task": "<script>bad()</script>"}); err != nil {
		t.Fatal(err)
	}
	dash, err := NewDashboard(store, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dash.Close()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request := func(path, method, host string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, dash.URL+path, nil)
		if host != "" {
			req.Host = host
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("missing headers")
		}
		return res.StatusCode, string(data)
	}
	status, body := request("/api/runs", "GET", "")
	var runs []Record
	if status != 200 || decodeJSON([]byte(body), &runs) != nil || runs[0]["status"] != "running" {
		t.Fatal(status, body)
	}
	if err := store.Save(Record{"id": "run", "status": "completed"}); err != nil {
		t.Fatal(err)
	}
	status, body = request("/api/runs/run", "GET", "")
	var run Record
	if status != 200 || decodeJSON([]byte(body), &run) != nil || run["status"] != "completed" {
		t.Fatal(status, body)
	}
	_, body = request("/", "GET", "")
	if strings.Contains(body, "<script>bad()") {
		t.Fatal("injected HTML")
	}
	_, body = request("/app.js", "GET", "")
	if !strings.Contains(body, "textContent") || strings.Contains(body, "innerHTML") {
		t.Fatal("unsafe display")
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		if status, _ := request("/api/runs/run", method, ""); status != 405 {
			t.Fatal(method, status)
		}
	}
	if status, _ := request("/", "GET", "attacker.invalid"); status != 403 {
		t.Fatal(status)
	}
	for _, path := range []string{"/api/runs/missing", "/../../etc/passwd", "/api/runs/a/b"} {
		if status, _ := request(path, "GET", ""); status != 404 {
			t.Fatal(path, status)
		}
	}
}
