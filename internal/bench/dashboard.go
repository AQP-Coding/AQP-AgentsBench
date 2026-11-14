package bench

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed dashboard.html
var dashboardHTML string

//go:embed dashboard.js
var dashboardScript string

type Dashboard struct {
	URL    string
	server *http.Server
}

func NewDashboard(store *Store, port int) (*Dashboard, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	address := listener.Addr().String()
	_, actualPort, _ := net.SplitHostPort(address)
	send := func(w http.ResponseWriter, status int, body any, contentType string) {
		var data []byte
		var err error
		if s, ok := body.(string); ok {
			data = []byte(s)
		} else {
			data, err = json.Marshal(body)
		}
		if err != nil {
			status = 500
			data = []byte(`{"error":"Cannot encode response"}`)
		}
		w.Header().Set("Content-Type", contentType+"; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		w.WriteHeader(status)
		_, _ = w.Write(data)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			send(w, 405, Record{"error": "Monitoring only; use CLI controls"}, "application/json")
			return
		}
		if r.Host != "127.0.0.1:"+actualPort && r.Host != "localhost:"+actualPort {
			send(w, 403, Record{"error": "Loopback Host required"}, "application/json")
			return
		}
		path := r.URL.EscapedPath()
		switch path {
		case "/":
			send(w, 200, dashboardHTML, "text/html")
		case "/app.js":
			send(w, 200, dashboardScript, "application/javascript")
		case "/api/runs":
			runs, err := store.List()
			if err != nil {
				send(w, 500, Record{"error": "Cannot read runs"}, "application/json")
				return
			}
			send(w, 200, runs, "application/json")
		default:
			if !strings.HasPrefix(path, "/api/runs/") {
				send(w, 404, Record{"error": "Not found"}, "application/json")
				return
			}
			parts := strings.Split(strings.TrimPrefix(path, "/api/runs/"), "/")
			run, err := store.Get(parts[0])
			if err != nil {
				send(w, 404, Record{"error": "Unknown run"}, "application/json")
				return
			}
			if len(parts) == 1 {
				send(w, 200, run, "application/json")
				return
			}
			if len(parts) == 2 && parts[1] == "events" {
				events, err := store.Events(parts[0])
				if err != nil {
					send(w, 500, Record{"error": "Cannot read events"}, "application/json")
					return
				}
				send(w, 200, events, "application/json")
				return
			}
			send(w, 404, Record{"error": "Not found"}, "application/json")
		}
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	d := &Dashboard{URL: fmt.Sprintf("http://%s", address), server: server}
	go func() { _ = server.Serve(listener) }()
	return d, nil
}
func (d *Dashboard) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return d.server.Shutdown(ctx)
}
