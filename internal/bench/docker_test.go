package bench

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

type fakeDockerDaemon struct {
	t                 *testing.T
	server            *httptest.Server
	mu                sync.Mutex
	created           map[string]map[string]any
	execs             map[string]map[string]any
	execCount         int
	failProbe         bool
	copies            [][]dockerTestTarEntry
	archive           []byte
	unsafeArchive     bool
	failCopy          bool
	failExport        bool
	failHomeTransfer  bool
	missingContainers bool
	failStop          bool
	failRemove        bool
	stopCalls         int
	removeCalls       int
	processKilled     bool
	processKilledCh   chan struct{}
	stopped           bool
	removed           bool
}

func newFakeDockerDaemon(t *testing.T) *fakeDockerDaemon {
	t.Helper()
	d := &fakeDockerDaemon{t: t, created: map[string]map[string]any{}, execs: map[string]map[string]any{}, processKilledCh: make(chan struct{})}
	d.server = httptest.NewServer(http.HandlerFunc(d.serveHTTP))
	t.Cleanup(d.server.Close)
	return d
}

func (d *fakeDockerDaemon) runtime(t *testing.T) *DockerRuntime {
	t.Helper()
	address := strings.TrimPrefix(d.server.URL, "http://")
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}}
	httpClient := &http.Client{Transport: transport}
	c, err := client.New(client.WithHost("tcp://"+address), client.WithHTTPClient(httpClient), client.WithAPIVersion("1.57"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return newDockerRuntimeWithClient(c)
}

func (d *fakeDockerDaemon) serveHTTP(w http.ResponseWriter, r *http.Request) {
	d.t.Helper()
	path := strings.TrimPrefix(r.URL.Path, "/v1.57")
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/"):
		writeDockerJSON(w, http.StatusOK, map[string]any{"Id": "sha256:image"})
	case r.Method == http.MethodPost && path == "/containers/create":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			d.t.Errorf("decode create request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.created[r.URL.Query().Get("name")] = body
		d.mu.Unlock()
		writeDockerJSON(w, http.StatusCreated, map[string]any{"Id": "container-id"})
	case r.Method == http.MethodPost && path == "/containers/container-id/start":
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && path == "/containers/container-id/archive":
		if d.failCopy {
			writeDockerJSON(w, http.StatusForbidden, map[string]any{"message": "copy permission denied"})
			return
		}
		entries, err := dockerTestReadTar(r.Body)
		if err != nil {
			d.t.Errorf("decode copy archive: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.copies = append(d.copies, entries)
		d.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && path == "/containers/container-id/archive":
		if d.failExport {
			writeDockerJSON(w, http.StatusInternalServerError, map[string]any{"message": "copy failed"})
			return
		}
		archive := d.archive
		if archive == nil {
			name := "answer.txt"
			if d.unsafeArchive {
				name = "../outside.txt"
			}
			archive = dockerTestTar(d.t, []dockerTestTarEntry{{name: name, body: []byte("updated\n"), mode: 0o644, kind: tar.TypeReg}})
		}
		stat, _ := json.Marshal(map[string]any{"name": "workspace", "size": 0, "mode": 0o755, "mtime": "1970-01-01T00:00:00Z"})
		w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(archive)
	case r.Method == http.MethodGet && path == "/containers/container-id/json":
		writeDockerJSON(w, http.StatusOK, map[string]any{"NetworkSettings": map[string]any{"Networks": map[string]any{"bridge": map[string]any{"IPAddress": "172.18.0.2"}}}})
	case r.Method == http.MethodPost && path == "/containers/container-id/exec":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			d.t.Errorf("decode exec request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.execCount++
		id := fmt.Sprintf("exec-%d", d.execCount)
		d.execs[id] = body
		if strings.Contains(fmt.Sprint(body["Cmd"]), "kill -") {
			if !d.processKilled {
				d.processKilled = true
				close(d.processKilledCh)
			}
		}
		failHome := d.failHomeTransfer && strings.Contains(fmt.Sprint(body["Cmd"]), "aqp-stage")
		if failHome {
			d.execs[id]["failed"] = true
		}
		d.mu.Unlock()
		writeDockerJSON(w, http.StatusCreated, map[string]any{"Id": id})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/exec/") && strings.HasSuffix(path, "/start"):
		d.streamExec(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/exec/") && strings.HasSuffix(path, "/json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/exec/"), "/json")
		d.mu.Lock()
		cmd := fmt.Sprint(d.execs[id]["Cmd"])
		fail := (d.failProbe && strings.Contains(cmd, "aqp-bind-probe")) || d.execs[id]["failed"] == true
		running := strings.Contains(cmd, "agent-long") && !d.processKilled
		d.mu.Unlock()
		exitCode := 0
		if fail {
			exitCode = 1
		}
		writeDockerJSON(w, http.StatusOK, map[string]any{"ID": id, "Running": running, "ExitCode": exitCode, "Pid": 321})
	case r.Method == http.MethodPost && path == "/containers/container-id/stop":
		d.mu.Lock()
		d.stopCalls++
		missing, fail := d.missingContainers, d.failStop
		d.mu.Unlock()
		if fail {
			writeDockerJSON(w, http.StatusInternalServerError, map[string]any{"message": "daemon unavailable"})
			return
		}
		if missing {
			writeDockerJSON(w, http.StatusNotFound, map[string]any{"message": "No such container"})
			return
		}
		if r.URL.Query().Get("t") != "2" {
			d.t.Errorf("stop timeout = %q, want 2", r.URL.Query().Get("t"))
		}
		d.mu.Lock()
		d.stopped = true
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && path == "/containers/container-id":
		d.mu.Lock()
		d.removeCalls++
		missing, fail := d.missingContainers, d.failRemove
		d.mu.Unlock()
		if fail {
			writeDockerJSON(w, http.StatusInternalServerError, map[string]any{"message": "daemon unavailable"})
			return
		}
		if missing {
			writeDockerJSON(w, http.StatusNotFound, map[string]any{"message": "No such container"})
			return
		}
		if r.URL.Query().Get("force") != "1" {
			d.t.Errorf("remove force = %q, want 1", r.URL.Query().Get("force"))
		}
		d.mu.Lock()
		d.removed = true
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		d.t.Errorf("unexpected Docker API request: %s %s", r.Method, r.URL.String())
		writeDockerJSON(w, http.StatusNotFound, map[string]any{"message": "not found"})
	}
}

type dockerTestTarEntry struct {
	name string
	body []byte
	mode int64
	kind byte
	link string
}

func dockerTestTar(t *testing.T, entries []dockerTestTarEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, entry := range entries {
		h := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: entry.kind, Size: int64(len(entry.body)), Linkname: entry.link}
		if entry.kind == tar.TypeDir {
			h.Size = 0
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) > 0 {
			if _, err := w.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func dockerTestReadTar(input io.Reader) ([]dockerTestTarEntry, error) {
	r := tar.NewReader(input)
	var entries []dockerTestTarEntry
	for {
		h, err := r.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		entries = append(entries, dockerTestTarEntry{name: h.Name, body: body, mode: h.Mode, kind: h.Typeflag, link: h.Linkname})
	}
}

func (d *fakeDockerDaemon) streamExec(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	hj, ok := w.(http.Hijacker)
	if !ok {
		d.t.Error("test server does not support hijacking")
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		d.t.Errorf("hijack exec stream: %v", err)
		return
	}
	defer conn.Close()
	if _, err := io.WriteString(rw, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"); err != nil {
		d.t.Errorf("write upgrade response: %v", err)
		return
	}
	_ = rw.Flush()
	id := strings.TrimSuffix(pathWithoutVersion(r.URL.Path), "/start")
	id = strings.TrimPrefix(id, "/exec/")
	d.mu.Lock()
	cmd := fmt.Sprint(d.execs[id]["Cmd"])
	fail := d.failProbe && strings.Contains(cmd, "aqp-bind-probe")
	d.mu.Unlock()
	if !fail && !strings.Contains(cmd, "aqp-bind-probe") && !strings.Contains(cmd, "agent-long") {
		writeDockerFrame(rw, 1, []byte("stdout"))
		writeDockerFrame(rw, 2, []byte("stderr"))
		_ = rw.Flush()
	}
}

func pathWithoutVersion(value string) string { return strings.TrimPrefix(value, "/v1.57") }

func writeDockerFrame(w io.Writer, stream byte, body []byte) {
	header := make([]byte, 8)
	header[0] = stream
	binary.BigEndian.PutUint32(header[4:], uint32(len(body)))
	_, _ = w.Write(header)
	_, _ = w.Write(body)
}

func writeDockerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestDockerStartUsesRestrictedEngineConfigAndDoesNotPull(t *testing.T) {
	d := newFakeDockerDaemon(t)
	runtime := d.runtime(t)
	workspace := t.TempDir()
	evaluation := t.TempDir()
	identity, err := runtime.Start(context.Background(), "image:test", workspace, StartOptions{
		Environment: map[string]string{"AQP_GATEWAY_TOKEN": "ephemeral"}, Evaluation: evaluation, ReadonlyWorkspace: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if identity != "container-id" || runtime.TransportFor(identity) != "bind" {
		t.Fatalf("start identity/transport = %q/%q", identity, runtime.TransportFor(identity))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.created) != 1 {
		t.Fatalf("created containers = %d", len(d.created))
	}
	var config map[string]any
	for _, req := range d.created {
		config = req
	}
	containerConfig := config
	hostConfig, ok := config["HostConfig"].(map[string]any)
	if !ok {
		t.Fatalf("missing Engine HostConfig: %#v", config)
	}
	if containerConfig["Image"] != "image:test" || containerConfig["WorkingDir"] != "/workspace" || containerConfig["Entrypoint"].([]any)[0] != "sh" {
		t.Fatalf("unexpected container config: %#v", containerConfig)
	}
	if containerConfig["Cmd"].([]any)[0] != "-c" || containerConfig["Cmd"].([]any)[1] != "exec sleep infinity" {
		t.Fatalf("unexpected command: %#v", containerConfig["Cmd"])
	}
	if containerConfig["User"] != fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) || strings.Join(dockerTestStrings(containerConfig["Env"]), "\n") == "" {
		t.Fatalf("missing uid or environment: %#v", containerConfig)
	}
	if !strings.Contains(strings.Join(dockerTestStrings(containerConfig["Env"]), "\n"), "HOME=/home/agent") || hostConfig["Privileged"] == true || hostConfig["NetworkMode"] == "host" {
		t.Fatalf("container isolation settings violated: %#v %#v", containerConfig, hostConfig)
	}
	mounts := hostConfig["Mounts"].([]any)
	if len(mounts) != 2 || mounts[1].(map[string]any)["Target"] != "/evaluation" || mounts[1].(map[string]any)["ReadOnly"] != true {
		t.Fatalf("unexpected mounts: %#v", mounts)
	}
}

func dockerTestStrings(v any) []string {
	values, _ := v.([]any)
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, fmt.Sprint(value))
	}
	return out
}

func TestDockerStartRejectsRemoteSocketAndUnsafeConfiguration(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://remote:2375")
	if _, err := NewDockerRuntime(); err == nil || !strings.Contains(err.Error(), "local Unix") {
		t.Fatalf("NewDockerRuntime remote host error = %v", err)
	}
	d := newFakeDockerDaemon(t)
	runtime := d.runtime(t)
	workspace := t.TempDir()
	for _, tc := range []struct {
		name    string
		image   string
		options StartOptions
	}{
		{name: "missing image", image: ""},
		{name: "flag image", image: "--privileged"},
		{name: "invalid env key", image: "image:test", options: StartOptions{Environment: map[string]string{"BAD=KEY": "x"}}},
		{name: "nul env value", image: "image:test", options: StartOptions{Environment: map[string]string{"A": "x\x00y"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := runtime.Start(context.Background(), tc.image, workspace, tc.options); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid configuration left visibility marker(s): %#v", entries)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(context.Background(), "image:test", home, StartOptions{}); err == nil {
		t.Fatal("home directory mount accepted")
	}
	if err := os.WriteFile(filepath.Join(workspace, "docker.sock"), []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(context.Background(), "image:test", workspace, StartOptions{}); err == nil {
		t.Fatal("socket-bearing mount accepted")
	}
}

func TestNewDockerRuntimeIgnoresSelectedRemoteContext(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "remote")
	runtime, err := NewDockerRuntime()
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.client.Close()
	if host := runtime.client.DaemonHost(); !strings.HasPrefix(host, "unix:///") {
		t.Fatalf("Docker context selected a non-local endpoint: %q", host)
	}
}

func TestDockerStartForcesPrivateContainerHome(t *testing.T) {
	d := newFakeDockerDaemon(t)
	runtime := d.runtime(t)
	if _, err := runtime.Start(context.Background(), "image:test", t.TempDir(), StartOptions{Environment: map[string]string{"HOME": "invalid\x00host-value"}}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, config := range d.created {
		if !strings.Contains(strings.Join(dockerTestStrings(config["Env"]), "\n"), "HOME=/home/agent") {
			t.Fatalf("container HOME was not pinned: %#v", config["Env"])
		}
	}
}

func TestDockerSpawnKeepsStreamsSeparateAndWaitsForExec(t *testing.T) {
	d := newFakeDockerDaemon(t)
	runtime := d.runtime(t)
	proc, err := runtime.Spawn(context.Background(), "container-id", []string{"agent", "--mode", "rpc"}, true)
	if err != nil {
		t.Fatal(err)
	}
	type readResult struct {
		data []byte
		err  error
	}
	stdout, stderr := make(chan readResult, 1), make(chan readResult, 1)
	go func() { b, err := io.ReadAll(proc.Stdout()); stdout <- readResult{b, err} }()
	go func() { b, err := io.ReadAll(proc.Stderr()); stderr <- readResult{b, err} }()
	code, err := proc.Wait()
	out, errOut := <-stdout, <-stderr
	if err != nil || out.err != nil || errOut.err != nil || code != 0 || string(out.data) != "stdout" || string(errOut.data) != "stderr" {
		t.Fatalf("wait = %d, %v; stdout=%q/%v stderr=%q/%v", code, err, out.data, out.err, errOut.data, errOut.err)
	}
	if code, done := proc.Poll(); !done || code != 0 {
		t.Fatalf("poll = %d, %v", code, done)
	}
	if d.stopped || d.removed {
		t.Fatal("closing/waiting on an exec stream must not stop or remove its container")
	}
}

func TestDockerStopUsesGraceTimeoutAndRemoveForces(t *testing.T) {
	d := newFakeDockerDaemon(t)
	runtime := d.runtime(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Stop(ctx, "container-id"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Remove(ctx, "container-id"); err != nil {
		t.Fatal(err)
	}
	if !d.stopped || !d.removed {
		t.Fatal("expected engine stop and remove calls")
	}
}

func TestDockerCopyFallbackPreservesPermissionsAndExportsOnlyAfterStop(t *testing.T) {
	d := newFakeDockerDaemon(t)
	d.failProbe = true
	runtime := d.runtime(t)
	root := t.TempDir()
	workspace, evaluation, home := filepath.Join(root, "workspace"), filepath.Join(root, "evaluation"), filepath.Join(root, "home")
	for _, dir := range []string{workspace, evaluation, home, filepath.Join(evaluation, "nested")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, "draft.txt"), []byte("draft"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "host-secret")
	if err := os.WriteFile(secret, []byte("must not be copied through symlink"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(workspace, "external-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evaluation, "check.py"), []byte("pass"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evaluation, "nested", "run.sh"), []byte("run"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "models.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := runtime.Start(context.Background(), "image:test", workspace, StartOptions{Home: home, Evaluation: evaluation, CopyBack: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := runtime.TransportFor(identity); got != "docker_cp" {
		t.Fatalf("transport = %q", got)
	}
	mode := func(name string) int64 {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, archive := range d.copies {
			for _, entry := range archive {
				if entry.name == name {
					return entry.mode
				}
			}
		}
		return -1
	}
	if mode("evaluation/check.py") != 0o444 || mode("evaluation/nested") != 0o555 || mode("evaluation/nested/run.sh") != 0o554 {
		t.Fatalf("read-only evaluator archive modes: check=%o dir=%o executable=%o", mode("evaluation/check.py"), mode("evaluation/nested"), mode("evaluation/nested/run.sh"))
	}
	if mode("workspace/draft.txt") != 0o600 {
		t.Fatalf("workspace mode = %o", mode("workspace/draft.txt"))
	}
	d.mu.Lock()
	var symlinkFound bool
	for _, archive := range d.copies {
		for _, entry := range archive {
			if entry.name == "workspace/external-link" && entry.kind == tar.TypeSymlink && entry.link == secret {
				symlinkFound = true
			}
		}
	}
	var fallbackConfig map[string]any
	for _, config := range d.created {
		if config["WorkingDir"] == "/tmp" {
			fallbackConfig = config
		}
	}
	d.mu.Unlock()
	if !symlinkFound {
		t.Fatal("copy fallback did not preserve symlink without following it")
	}
	if fallbackConfig == nil || fallbackConfig["User"] != fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()) || fallbackConfig["HostConfig"].(map[string]any)["Tmpfs"].(map[string]any)["/home/agent"] != fmt.Sprintf("uid=%d,gid=%d,mode=0700", os.Getuid(), os.Getgid()) {
		t.Fatalf("copy fallback did not retain writable agent-owned home tmpfs: %#v", fallbackConfig)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".aqp-bind-probe-") {
			t.Fatalf("visibility probe leaked: %s", entry.Name())
		}
	}
	if err := runtime.ExportWorkspace(context.Background(), identity, workspace); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("export before stop error = %v", err)
	}
	if err := runtime.Stop(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if err := runtime.ExportWorkspace(context.Background(), identity, workspace); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "answer.txt")); err != nil || string(got) != "updated\n" {
		t.Fatalf("exported answer = %q, %v", got, err)
	}
	if info, err := os.Stat(workspace); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("exported workspace root mode = %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "draft.txt")); !os.IsNotExist(err) {
		t.Fatalf("old workspace contents were not replaced: %v", err)
	}
	if got, err := os.Stat(filepath.Join(evaluation, "check.py")); err != nil || got.Mode().Perm() != 0o640 {
		t.Fatalf("host evaluator permissions changed: %v, %v", got, err)
	}
}

func TestDockerArchiveExtractionRejectsTraversalAndSymlinkParent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []dockerTestTarEntry
	}{
		{name: "traversal", entries: []dockerTestTarEntry{{name: "../outside", body: []byte("bad"), mode: 0o600, kind: tar.TypeReg}}},
		{name: "escaping symlink", entries: []dockerTestTarEntry{{name: "link", mode: 0o777, kind: tar.TypeSymlink, link: "../outside"}}},
		{name: "symlink parent", entries: []dockerTestTarEntry{{name: "real/", mode: 0o755, kind: tar.TypeDir}, {name: "alias", mode: 0o777, kind: tar.TypeSymlink, link: "real"}, {name: "alias/outside", body: []byte("bad"), mode: 0o600, kind: tar.TypeReg}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			archive := dockerTestTar(t, tc.entries)
			if err := dockerExtractArchive(bytes.NewReader(archive), root); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(root), "outside")); !os.IsNotExist(err) {
				t.Fatalf("archive wrote outside root: %v", err)
			}
		})
	}
}

func TestDockerAddressReadsIPv4FromEngineInspection(t *testing.T) {
	runtime := newFakeDockerDaemon(t).runtime(t)
	if got, err := runtime.Address("container-id"); err != nil || got != "172.18.0.2" {
		t.Fatalf("address = %q, %v", got, err)
	}
}

func TestDockerExecPreservesArgvAndUsesSeparateNonTTYStreams(t *testing.T) {
	d := newFakeDockerDaemon(t)
	runtime := d.runtime(t)
	command := []string{"printf", "x; echo not-a-shell"}
	result, err := runtime.Exec(context.Background(), "container-id", command, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReturnCode != 0 || result.Stdout != "stdout" || result.Stderr != "stderr" {
		t.Fatalf("exec result = %#v", result)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var request map[string]any
	for _, exec := range d.execs {
		if fmt.Sprint(exec["Cmd"]) == "[printf x; echo not-a-shell]" {
			request = exec
		}
	}
	if request == nil || request["Tty"] != false || request["WorkingDir"] != "/workspace" || fmt.Sprint(request["Cmd"]) != "[printf x; echo not-a-shell]" {
		t.Fatalf("unexpected ExecCreate options: %#v", request)
	}
}

func TestDockerCopyFallbackReadonlyWorkspaceDoesNotChangeHostModes(t *testing.T) {
	d := newFakeDockerDaemon(t)
	d.failProbe = true
	runtime := d.runtime(t)
	workspace := t.TempDir()
	file := filepath.Join(workspace, "input.txt")
	if err := os.WriteFile(file, []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(context.Background(), "image:test", workspace, StartOptions{ReadonlyWorkspace: true}); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.copies) != 1 || len(d.copies[0]) != 2 {
		t.Fatalf("workspace staging archive = %#v", d.copies)
	}
	if d.copies[0][0].mode != 0o555 || d.copies[0][1].mode != 0o444 {
		t.Fatalf("workspace staging modes = %#v", d.copies[0])
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("host workspace permissions changed: %v, %v", info, err)
	}
}

func TestDockerCopyFallbackFailureReportsAndDiscardsContainers(t *testing.T) {
	d := newFakeDockerDaemon(t)
	d.failProbe, d.failCopy = true, true
	runtime := d.runtime(t)
	if _, err := runtime.Start(context.Background(), "image:test", t.TempDir(), StartOptions{}); err == nil ||
		!strings.Contains(err.Error(), "Docker bind mount visibility probe failed") || !strings.Contains(err.Error(), "Docker cp fallback failed") {
		t.Fatalf("copy fallback error = %v", err)
	}
	if d.stopCalls != 2 || d.removeCalls != 2 || runtime.TransportFor("container-id") != "" {
		t.Fatalf("failed containers were not discarded: stop=%d remove=%d transport=%q", d.stopCalls, d.removeCalls, runtime.TransportFor("container-id"))
	}
}

func TestDockerHomeTransferFailureDiscardsStartedContainer(t *testing.T) {
	d := newFakeDockerDaemon(t)
	d.failHomeTransfer = true
	runtime := d.runtime(t)
	workspace, home := t.TempDir(), t.TempDir()
	if _, err := runtime.Start(context.Background(), "image:test", workspace, StartOptions{Home: home}); err == nil || !strings.Contains(err.Error(), "Container staging failed") {
		t.Fatalf("home transfer error = %v", err)
	}
	if d.stopCalls != 1 || d.removeCalls != 1 {
		t.Fatalf("failed home transfer cleanup = stop:%d remove:%d", d.stopCalls, d.removeCalls)
	}
}

func TestDockerExportFailureLeavesOriginalWorkspaceUntouched(t *testing.T) {
	d := newFakeDockerDaemon(t)
	d.failProbe = true
	runtime := d.runtime(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(workspace, "original.txt")
	if err := os.WriteFile(original, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := runtime.Start(context.Background(), "image:test", workspace, StartOptions{CopyBack: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Stop(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	d.unsafeArchive = true
	if err = runtime.ExportWorkspace(context.Background(), identity, workspace); err == nil {
		t.Fatal("unsafe container archive was accepted")
	}
	if got, err := os.ReadFile(original); err != nil || string(got) != "keep" {
		t.Fatalf("original workspace was not preserved: %q, %v", got, err)
	}
}

func TestDockerArchiveCopyFailurePreservesOriginalSnapshot(t *testing.T) {
	d := newFakeDockerDaemon(t)
	d.failProbe = true
	runtime := d.runtime(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(workspace, "original.txt")
	if err := os.WriteFile(original, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := runtime.Start(context.Background(), "image:test", workspace, StartOptions{CopyBack: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Stop(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	d.failExport = true
	if err = runtime.ExportWorkspace(context.Background(), identity, workspace); err == nil || !strings.Contains(err.Error(), "Docker workspace export failed") {
		t.Fatalf("archive export error = %v", err)
	}
	if got, err := os.ReadFile(original); err != nil || string(got) != "preserve me" {
		t.Fatalf("original snapshot was not preserved: %q, %v", got, err)
	}
}

func TestDockerCleanupToleratesMissingContainerButReportsDaemonFailures(t *testing.T) {
	d := newFakeDockerDaemon(t)
	runtime := d.runtime(t)
	d.missingContainers = true
	if err := runtime.Stop(context.Background(), "container-id"); err != nil {
		t.Fatalf("stop missing container: %v", err)
	}
	if err := runtime.Remove(context.Background(), "container-id"); err != nil {
		t.Fatalf("remove missing container: %v", err)
	}
	d.missingContainers, d.failStop, d.failRemove = false, true, true
	if err := runtime.Stop(context.Background(), "container-id"); err == nil || !strings.Contains(err.Error(), "Docker stop failed") {
		t.Fatalf("daemon stop failure = %v", err)
	}
	if err := runtime.Remove(context.Background(), "container-id"); err == nil || !strings.Contains(err.Error(), "Docker rm failed") {
		t.Fatalf("daemon remove failure = %v", err)
	}
}

func TestDockerProcessTerminationTargetsExecNotContainer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal func(Process) error
	}{
		{name: "terminate", signal: func(p Process) error { return p.Terminate() }},
		{name: "kill", signal: func(p Process) error { return p.Kill() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeDockerDaemon(t)
			runtime := d.runtime(t)
			process, err := runtime.Spawn(context.Background(), "container-id", []string{"agent-long"}, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.signal(process); err != nil {
				t.Fatal(err)
			}
			if d.stopped || !d.processKilled {
				t.Fatalf("termination affected the container instead of its exec: stopped=%v processKilled=%v", d.stopped, d.processKilled)
			}
			code, err := process.Wait()
			if err != nil || code != 0 {
				t.Fatalf("wait after termination = %d, %v", code, err)
			}
		})
	}
}

func TestDockerSpawnCancellationTerminatesExecBeforeReturning(t *testing.T) {
	d := newFakeDockerDaemon(t)
	runtime := d.runtime(t)
	ctx, cancel := context.WithCancel(context.Background())
	process, err := runtime.Spawn(ctx, "container-id", []string{"agent-long"}, true)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-d.processKilledCh:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel did not terminate the Docker exec")
	}
	if d.stopped {
		t.Fatal("canceling a process must not stop its container")
	}
	if _, err := process.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestDockerHomeCopyFailureStillRemovesWhenStopFails(t *testing.T) {
	d := newFakeDockerDaemon(t)
	d.failHomeTransfer, d.failStop = true, true
	runtime := d.runtime(t)
	if _, err := runtime.Start(context.Background(), "image:test", t.TempDir(), StartOptions{Home: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "Container staging failed") {
		t.Fatalf("home transfer error = %v", err)
	}
	if d.stopCalls != 1 || d.removeCalls != 1 {
		t.Fatalf("cleanup did not attempt remove after failed stop: stop=%d remove=%d", d.stopCalls, d.removeCalls)
	}
}
