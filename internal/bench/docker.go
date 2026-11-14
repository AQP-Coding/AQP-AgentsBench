package bench

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const dockerDefaultExecTimeout = 300 * time.Second

type DockerRuntime struct {
	client *client.Client
	mu     sync.Mutex
	state  map[string]*dockerContainerState
}

type dockerContainerState struct {
	workspace         string
	copyBack          bool
	transport         string
	execUser          string
	readonlyWorkspace bool
	stopped           bool
}

type dockerMount struct {
	source   string
	target   string
	readonly bool
}

func NewDockerRuntime() (*DockerRuntime, error) {
	uid := os.Getuid()
	defaultHost := "/var/run/docker.sock"
	rootless := fmt.Sprintf("/run/user/%d/docker.sock", uid)
	if _, err := os.Stat(rootless); err == nil {
		defaultHost = rootless
	}
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix://" + defaultHost
	}
	if !strings.HasPrefix(host, "unix:///") || strings.ContainsAny(host, "\x00\n\r") {
		return nil, fmt.Errorf("Only a local Unix-socket Docker daemon is supported")
	}
	c, err := client.New(client.WithHost(host))
	if err != nil {
		return nil, fmt.Errorf("create local Docker Engine client: %w", err)
	}
	return newDockerRuntimeWithClient(c), nil
}

func newDockerRuntimeWithClient(c *client.Client) *DockerRuntime {
	return &DockerRuntime{client: c, state: map[string]*dockerContainerState{}}
}

func (r *DockerRuntime) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Close()
}

func (r *DockerRuntime) Start(ctx context.Context, image, workspace string, options StartOptions) (string, error) {
	if r == nil || r.client == nil {
		return "", fmt.Errorf("Docker Engine client is unavailable")
	}
	if image == "" || strings.HasPrefix(image, "-") || strings.ContainsAny(image, "\x00\n\r") {
		return "", fmt.Errorf("A Docker image is required")
	}
	workspace, err := dockerDedicatedDir(workspace, "Container mounts must be dedicated workspace/configuration directories")
	if err != nil {
		return "", err
	}
	mounts := []dockerMount{{source: workspace, target: "/workspace", readonly: options.ReadonlyWorkspace}}
	if options.Evaluation != "" {
		evaluation, err := dockerDedicatedDir(options.Evaluation, "Container mounts must be dedicated workspace/configuration directories")
		if err != nil {
			return "", err
		}
		mounts = append(mounts, dockerMount{source: evaluation, target: "/evaluation", readonly: true})
	}
	for _, m := range mounts {
		if strings.ContainsAny(m.source, ",\n\r") {
			return "", fmt.Errorf("Container mount paths cannot contain commas or newlines")
		}
		if _, err := os.Lstat(filepath.Join(m.source, "docker.sock")); err == nil {
			return "", fmt.Errorf("Docker socket mounts are forbidden")
		}
	}
	home := ""
	if options.Home != "" {
		home, err = dockerDedicatedDir(options.Home, "Container staging sources must be dedicated directories")
		if err != nil {
			return "", err
		}
		if _, err := os.Lstat(filepath.Join(home, "docker.sock")); err == nil {
			return "", fmt.Errorf("Docker socket staging is forbidden")
		}
	}
	env := make(map[string]string, len(options.Environment)+1)
	for key, value := range options.Environment {
		env[key] = value
	}
	env["HOME"] = "/home/agent"
	for key, value := range env {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(key) || strings.ContainsRune(value, 0) {
			return "", fmt.Errorf("Invalid explicit container environment")
		}
	}
	uid, gid := os.Getuid(), os.Getgid()
	markers, err := dockerWorkspaceMarkers(mounts)
	if err != nil {
		return "", err
	}
	defer dockerRemoveMarkers(markers)
	identity, err := r.dockerLaunch(ctx, image, env, mounts, uid, gid, "/workspace")
	if err != nil {
		return "", err
	}
	transport := "bind"
	if probeErr := r.dockerProbeMounts(ctx, identity, markers); probeErr != nil {
		_ = r.discard(context.Background(), identity)
		dockerRemoveMarkers(markers)
		identity, err = r.dockerLaunch(ctx, image, env, nil, uid, gid, "/tmp")
		if err != nil {
			return "", fmt.Errorf("Docker bind mount visibility probe failed (%v); Docker cp fallback failed (%w)", probeErr, err)
		}
		if stageErr := r.dockerStageMounts(ctx, identity, mounts); stageErr != nil {
			_ = r.discard(context.Background(), identity)
			return "", fmt.Errorf("Docker bind mount visibility probe failed (%v); Docker cp fallback failed (%w)", probeErr, stageErr)
		}
		transport = "docker_cp"
	}
	if home != "" {
		stage := "/tmp/aqp-stage-" + newID()
		if err := r.dockerCopyDirectory(ctx, identity, home, stage, "", false); err != nil {
			_ = r.discard(context.Background(), identity)
			return "", fmt.Errorf("stage container home: %w", err)
		}
		transfer, err := r.ExecAs(ctx, identity, fmt.Sprintf("%d:%d", uid, gid), []string{"sh", "-c", `cp -a "$1"/. "$2"/ || exit $?; rm -rf "$1"`, "aqp-stage", stage, "/home/agent"}, 30*time.Second)
		if err != nil || transfer.ReturnCode != 0 {
			_ = r.discard(context.Background(), identity)
			detail := strings.TrimSpace(transfer.Stdout + " " + transfer.Stderr)
			if err != nil {
				return "", fmt.Errorf("Container staging failed for /home/agent: %w", err)
			}
			if detail == "" {
				detail = "copy command failed"
			}
			return "", fmt.Errorf("Container staging failed for /home/agent (exit code %d): %s", transfer.ReturnCode, detail)
		}
	}
	r.mu.Lock()
	r.state[identity] = &dockerContainerState{workspace: workspace, copyBack: options.CopyBack, transport: transport,
		execUser: fmt.Sprintf("%d:%d", uid, gid), readonlyWorkspace: options.ReadonlyWorkspace}
	r.mu.Unlock()
	return identity, nil
}

func dockerDedicatedDir(value, message string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\x00\n\r") {
		return "", fmt.Errorf("%s", message)
	}
	resolved, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("%s", message)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s", message)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		home, _ = filepath.EvalSymlinks(home)
		rel, relErr := filepath.Rel(home, resolved)
		relHome, homeErr := filepath.Rel(resolved, home)
		if resolved == home || (relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) || (homeErr == nil && relHome != ".." && !strings.HasPrefix(relHome, ".."+string(os.PathSeparator))) {
			return "", fmt.Errorf("%s", message)
		}
	}
	return resolved, nil
}

func dockerWorkspaceMarkers(mounts []dockerMount) ([]dockerMarker, error) {
	markers := make([]dockerMarker, 0, len(mounts))
	for _, m := range mounts {
		name := ".aqp-bind-probe-" + newID()
		value := newID()
		filename := filepath.Join(m.source, name)
		f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			dockerRemoveMarkers(markers)
			return nil, fmt.Errorf("Cannot create temporary Docker visibility probes: %w", err)
		}
		if _, err = io.WriteString(f, value+"\n"); err != nil {
			_ = f.Close()
			_ = os.Remove(filename)
			dockerRemoveMarkers(markers)
			return nil, fmt.Errorf("Cannot create temporary Docker visibility probes: %w", err)
		}
		if err = f.Close(); err != nil {
			_ = os.Remove(filename)
			dockerRemoveMarkers(markers)
			return nil, fmt.Errorf("Cannot create temporary Docker visibility probes: %w", err)
		}
		markers = append(markers, dockerMarker{filename: filename, name: name, target: m.target, value: value})
	}
	return markers, nil
}

type dockerMarker struct{ filename, name, target, value string }

func dockerRemoveMarkers(markers []dockerMarker) {
	for _, marker := range markers {
		_ = os.Remove(marker.filename)
	}
}

func (r *DockerRuntime) dockerLaunch(ctx context.Context, image string, env map[string]string, mounts []dockerMount, uid, gid int, workdir string) (string, error) {
	if _, err := r.client.ImageInspect(ctx, image); err != nil {
		return "", fmt.Errorf("Docker image %q is not available locally (pulls are disabled): %w", image, err)
	}
	containerMounts := make([]mount.Mount, 0, len(mounts))
	for _, m := range mounts {
		containerMounts = append(containerMounts, mount.Mount{Type: mount.TypeBind, Source: m.source, Target: m.target, ReadOnly: m.readonly})
	}
	vars := make([]string, 0, len(env))
	for key, value := range env {
		vars = append(vars, key+"="+value)
	}
	sort.Strings(vars)
	init := true
	uidgid := fmt.Sprintf("%d:%d", uid, gid)
	created, err := r.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "aqp-" + newID(),
		Config: &container.Config{Image: image, User: uidgid, Env: vars, WorkingDir: workdir,
			Entrypoint: []string{"sh"}, Cmd: []string{"-c", "exec sleep infinity"}},
		HostConfig: &container.HostConfig{CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
			ExtraHosts: []string{"host.docker.internal:host-gateway"}, Mounts: containerMounts,
			Tmpfs: map[string]string{"/home/agent": fmt.Sprintf("uid=%d,gid=%d,mode=0700", uid, gid)}, Init: &init},
	})
	if err != nil {
		return "", fmt.Errorf("Docker operation create failed: %w", err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("Docker create returned an empty container identity")
	}
	if _, err = r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = r.client.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("Docker operation start failed: %w", err)
	}
	return created.ID, nil
}

func (r *DockerRuntime) dockerProbeMounts(ctx context.Context, identity string, markers []dockerMarker) error {
	for _, marker := range markers {
		command := []string{"sh", "-c", `IFS= read -r observed < "$1" && [ "$observed" = "$2" ]`, "aqp-bind-probe", path.Join(marker.target, marker.name), marker.value}
		result, err := r.Exec(ctx, identity, command, 10*time.Second)
		if err != nil {
			return fmt.Errorf("Docker cannot read bind mount %s: %w", marker.target, err)
		}
		if result.ReturnCode != 0 {
			detail := strings.TrimSpace(result.Stdout + " " + result.Stderr)
			return fmt.Errorf("Docker cannot read bind mount %s (exit code %d: %s)", marker.target, result.ReturnCode, detail)
		}
	}
	return nil
}

func (r *DockerRuntime) dockerStageMounts(ctx context.Context, identity string, mounts []dockerMount) error {
	for _, m := range mounts {
		if err := r.dockerCopyDirectory(ctx, identity, m.source, "/", path.Base(m.target), m.readonly); err != nil {
			return err
		}
	}
	return nil
}

func (r *DockerRuntime) dockerCopyDirectory(ctx context.Context, identity, source, destination, root string, readonly bool) error {
	reader, writer := io.Pipe()
	writeDone := make(chan error, 1)
	go func() {
		err := dockerWriteTarDirectory(source, root, readonly, writer)
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		writeDone <- err
	}()
	_, copyErr := r.client.CopyToContainer(ctx, identity, client.CopyToContainerOptions{DestinationPath: destination, Content: reader, CopyUIDGID: false})
	_ = reader.Close()
	writeErr := <-writeDone
	if copyErr != nil {
		return fmt.Errorf("Docker archive copy to %s failed: %w", destination, copyErr)
	}
	if writeErr != nil {
		return fmt.Errorf("archive host directory for Docker copy: %w", writeErr)
	}
	return nil
}

func dockerWriteTarDirectory(source, root string, readonly bool, out io.Writer) error {
	w := tar.NewWriter(out)
	err := filepath.WalkDir(source, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(filename)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, filename)
		if err != nil {
			return err
		}
		name := ""
		if rel != "." {
			name = filepath.ToSlash(rel)
			if root != "" {
				name = path.Join(root, name)
			}
		} else if root != "" {
			name = root + "/"
		}
		if name == "" {
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(filename)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = name
		if readonly && info.Mode()&os.ModeSymlink == 0 {
			mode := info.Mode().Perm()
			if info.IsDir() {
				header.Mode = int64((mode & 0o111) | 0o555)
			} else {
				header.Mode = int64((mode & 0o111) | 0o444)
			}
		}
		if err = w.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(filename)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(w, f)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		return nil
	})
	closeErr := w.Close()
	if err != nil {
		return fmt.Errorf("archive host directory: %w", err)
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func (r *DockerRuntime) TransportFor(identity string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state := r.state[identity]; state != nil {
		return state.transport
	}
	return ""
}

func (r *DockerRuntime) Exec(ctx context.Context, identity string, command []string, timeout time.Duration) (ExecResult, error) {
	return r.exec(ctx, identity, "", command, timeout)
}

func (r *DockerRuntime) ExecAs(ctx context.Context, identity, user string, command []string, timeout time.Duration) (ExecResult, error) {
	if !regexp.MustCompile(`^\d+(?::\d+)?$`).MatchString(user) {
		return ExecResult{}, fmt.Errorf("Container execution user must be a numeric UID[:GID]")
	}
	return r.exec(ctx, identity, user, command, timeout)
}

func (r *DockerRuntime) exec(ctx context.Context, identity, user string, command []string, timeout time.Duration) (ExecResult, error) {
	if err := dockerValidate(identity, command); err != nil {
		return ExecResult{}, err
	}
	if timeout <= 0 {
		timeout = dockerDefaultExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if user == "" {
		r.mu.Lock()
		if state := r.state[identity]; state != nil && state.transport == "docker_cp" {
			user = state.execUser
		}
		r.mu.Unlock()
	}
	execID, err := r.client.ExecCreate(ctx, identity, client.ExecCreateOptions{User: user, WorkingDir: "/workspace", Cmd: command,
		AttachStdout: true, AttachStderr: true, TTY: false})
	if err != nil {
		return ExecResult{}, fmt.Errorf("Docker operation exec create failed: %w", err)
	}
	attached, err := r.client.ExecAttach(ctx, execID.ID, client.ExecAttachOptions{TTY: false})
	if err != nil {
		r.terminateExec(identity, execID.ID, "TERM")
		return ExecResult{}, fmt.Errorf("Docker operation exec attach failed: %w", err)
	}
	var stdout, stderr bytes.Buffer
	_, streamErr := stdcopy.StdCopy(&stdout, &stderr, attached.Reader)
	attached.Close()
	if streamErr != nil {
		r.terminateExec(identity, execID.ID, "TERM")
		return ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}, fmt.Errorf("Docker exec stream failed: %w", streamErr)
	}
	inspection, err := r.waitExec(ctx, identity, execID.ID)
	if err != nil {
		r.terminateExec(identity, execID.ID, "TERM")
		return ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}, err
	}
	return ExecResult{ReturnCode: inspection.ExitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

func dockerValidate(identity string, command []string) error {
	if identity == "" || strings.HasPrefix(identity, "-") || len(command) == 0 {
		return fmt.Errorf("Container identity and nonempty argv are required")
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("Container command arguments cannot contain NUL")
		}
	}
	return nil
}

func (r *DockerRuntime) waitExec(ctx context.Context, identity, execID string) (client.ExecInspectResult, error) {
	for {
		inspection, err := r.client.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return client.ExecInspectResult{}, fmt.Errorf("Docker operation exec inspect failed: %w", err)
		}
		if !inspection.Running {
			return inspection, nil
		}
		select {
		case <-ctx.Done():
			r.terminateExec(identity, execID, "TERM")
			return client.ExecInspectResult{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (r *DockerRuntime) terminateExec(identity, execID, signal string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	info, err := r.client.ExecInspect(ctx, execID, client.ExecInspectOptions{})
	if err != nil || !info.Running || info.PID <= 0 {
		return
	}
	for _, action := range []string{signal, "KILL"} {
		command := []string{"sh", "-c", `kill -` + action + ` "$1"`, "aqp-signal", strconv.Itoa(info.PID)}
		if _, err := r.exec(ctx, identity, "", command, time.Second); err != nil {
			return
		}
		deadline := time.Now().Add(750 * time.Millisecond)
		for time.Now().Before(deadline) {
			inspection, err := r.client.ExecInspect(ctx, execID, client.ExecInspectOptions{})
			if err != nil || !inspection.Running {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
}

func (r *DockerRuntime) Spawn(ctx context.Context, identity string, command []string, captureStderr bool) (Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := dockerValidate(identity, command); err != nil {
		return nil, err
	}
	user := ""
	r.mu.Lock()
	if state := r.state[identity]; state != nil && state.transport == "docker_cp" {
		user = state.execUser
	}
	r.mu.Unlock()
	execID, err := r.client.ExecCreate(ctx, identity, client.ExecCreateOptions{User: user, WorkingDir: "/workspace", Cmd: command,
		AttachStdin: true, AttachStdout: true, AttachStderr: captureStderr, TTY: false})
	if err != nil {
		return nil, fmt.Errorf("Docker operation exec create failed: %w", err)
	}
	attached, err := r.client.ExecAttach(ctx, execID.ID, client.ExecAttachOptions{TTY: false})
	if err != nil {
		r.terminateExec(identity, execID.ID, "TERM")
		return nil, fmt.Errorf("Docker operation exec attach failed: %w", err)
	}
	process := newDockerProcess(r, identity, execID.ID, attached, captureStderr, ctx)
	go process.watchCancellation()
	return process, nil
}

type dockerProcess struct {
	runtime     *DockerRuntime
	containerID string
	execID      string
	attached    client.ExecAttachResult
	stdin       io.WriteCloser
	stdout      *io.PipeReader
	stderr      *io.PipeReader
	streamDone  chan error
	doneCh      chan struct{}
	ctx         context.Context
	mu          sync.Mutex
	done        bool
	code        int
	waitErr     error
}

func newDockerProcess(runtime *DockerRuntime, containerID, execID string, attached client.ExecAttachResult, captureStderr bool, ctx context.Context) *dockerProcess {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	if !captureStderr {
		_ = errW.Close()
	}
	p := &dockerProcess{runtime: runtime, containerID: containerID, execID: execID, attached: attached,
		stdin: dockerExecStdin{response: attached}, stdout: outR, stderr: errR, streamDone: make(chan error, 1), doneCh: make(chan struct{}), ctx: ctx}
	go func() {
		var dstErr io.Writer = errW
		if !captureStderr {
			dstErr = io.Discard
		}
		_, err := stdcopy.StdCopy(outW, dstErr, attached.Reader)
		_ = outW.Close()
		if captureStderr {
			_ = errW.Close()
		}
		p.streamDone <- err
		close(p.streamDone)
	}()
	return p
}

func (p *dockerProcess) watchCancellation() {
	select {
	case <-p.ctx.Done():
		_ = p.Terminate()
	case <-p.doneCh:
	}
}

type dockerExecStdin struct{ response client.ExecAttachResult }

func (s dockerExecStdin) Write(p []byte) (int, error) {
	if s.response.Conn == nil {
		return 0, net.ErrClosed
	}
	return s.response.Conn.Write(p)
}
func (s dockerExecStdin) Close() error { return s.response.CloseWrite() }

func (p *dockerProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *dockerProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *dockerProcess) Stderr() io.ReadCloser { return p.stderr }

func (p *dockerProcess) Wait() (int, error) {
	p.mu.Lock()
	if p.done {
		code, err := p.code, p.waitErr
		p.mu.Unlock()
		return code, err
	}
	p.mu.Unlock()
	if streamErr, ok := <-p.streamDone; ok && streamErr != nil {
		p.finish(0, fmt.Errorf("Docker exec stream failed: %w", streamErr))
		return 0, p.waitErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), dockerDefaultExecTimeout)
	defer cancel()
	inspection, err := p.runtime.waitExec(ctx, p.containerID, p.execID)
	if err != nil {
		p.finish(0, err)
		return 0, err
	}
	p.finish(inspection.ExitCode, nil)
	return inspection.ExitCode, nil
}

func (p *dockerProcess) finish(code int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.done {
		p.done, p.code, p.waitErr = true, code, err
		close(p.doneCh)
	}
}

func (p *dockerProcess) Poll() (int, bool) {
	p.mu.Lock()
	if p.done {
		code := p.code
		p.mu.Unlock()
		return code, true
	}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inspection, err := p.runtime.client.ExecInspect(ctx, p.execID, client.ExecInspectOptions{})
	if err != nil || inspection.Running {
		return 0, false
	}
	select {
	case <-p.streamDone:
		p.finish(inspection.ExitCode, nil)
		return inspection.ExitCode, true
	default:
		return 0, false
	}
}

func (p *dockerProcess) Terminate() error { return p.signal("TERM") }
func (p *dockerProcess) Kill() error      { return p.signal("KILL") }
func (p *dockerProcess) signal(signal string) error {
	if _, done := p.Poll(); done {
		return nil
	}
	p.runtime.terminateExec(p.containerID, p.execID, signal)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := p.runtime.waitExec(ctx, p.containerID, p.execID)
	if err != nil {
		return err
	}
	_ = p.attached.CloseWrite()
	return nil
}

func (r *DockerRuntime) Address(identity string) (string, error) {
	if err := dockerValidate(identity, []string{"inspect"}); err != nil {
		return "", err
	}
	result, err := r.client.ContainerInspect(context.Background(), identity, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("Docker operation inspect failed: %w", err)
	}
	if result.Container.NetworkSettings != nil {
		names := make([]string, 0, len(result.Container.NetworkSettings.Networks))
		for name := range result.Container.NetworkSettings.Networks {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			endpoint := result.Container.NetworkSettings.Networks[name]
			if endpoint != nil && endpoint.IPAddress.IsValid() && endpoint.IPAddress.Is4() {
				return endpoint.IPAddress.String(), nil
			}
		}
	}
	return "", fmt.Errorf("Docker container has no IPv4 address")
}

func (r *DockerRuntime) ExportWorkspace(ctx context.Context, identity, workspace string) error {
	r.mu.Lock()
	state := r.state[identity]
	if state == nil || !state.copyBack || state.transport != "docker_cp" {
		r.mu.Unlock()
		return nil
	}
	if !state.stopped {
		r.mu.Unlock()
		return fmt.Errorf("Container must be stopped before workspace export")
	}
	workspace, err := filepath.Abs(workspace)
	if err == nil {
		workspace, err = filepath.EvalSymlinks(workspace)
	}
	if err != nil || filepath.Clean(workspace) != state.workspace {
		r.mu.Unlock()
		return fmt.Errorf("Workspace export target does not match the started container")
	}
	r.mu.Unlock()
	parent := filepath.Dir(workspace)
	stage, err := os.MkdirTemp(parent, ".aqp-workspace-export-")
	if err != nil {
		return err
	}
	defer dockerRemoveTree(stage)
	exported := filepath.Join(stage, "workspace")
	if err := os.Mkdir(exported, 0o700); err != nil {
		return err
	}
	archive, err := r.client.CopyFromContainer(ctx, identity, client.CopyFromContainerOptions{SourcePath: "/workspace"})
	if err != nil {
		return fmt.Errorf("Docker workspace export failed: %w", err)
	}
	err = dockerExtractArchive(archive.Content, exported)
	closeErr := archive.Content.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if archive.Stat.Mode.Perm() != 0 {
		if err := os.Chmod(exported, archive.Stat.Mode.Perm()); err != nil {
			return err
		}
	}
	backup := workspace + ".before-" + newID()
	if err := os.Rename(workspace, backup); err != nil {
		return err
	}
	if err := os.Rename(exported, workspace); err != nil {
		rollbackErr := os.Rename(backup, workspace)
		if rollbackErr != nil {
			return fmt.Errorf("install exported workspace: %v; restore backup: %w", err, rollbackErr)
		}
		return err
	}
	if err := dockerRemoveTree(backup); err != nil {
		return fmt.Errorf("workspace exported but old workspace cleanup failed: %w", err)
	}
	return nil
}

func dockerExtractArchive(input io.Reader, destination string) error {
	r := tar.NewReader(input)
	root, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	type directoryMode struct {
		path  string
		mode  os.FileMode
		mtime time.Time
	}
	directories := []directoryMode{}
	for {
		header, err := r.Next()
		if err == io.EOF {
			sort.Slice(directories, func(i, j int) bool { return len(directories[i].path) > len(directories[j].path) })
			for _, directory := range directories {
				if err := os.Chmod(directory.path, directory.mode); err != nil {
					return err
				}
				if !directory.mtime.IsZero() {
					if err := os.Chtimes(directory.path, directory.mtime, directory.mtime); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Docker workspace archive: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe Docker archive path: %s", header.Name)
		}
		target := filepath.Join(root, name)
		rel, err := filepath.Rel(root, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe Docker archive path: %s", header.Name)
		}
		if err := dockerEnsureNoSymlinkParents(root, filepath.Dir(target)); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			directories = append(directories, directoryMode{path: target, mode: os.FileMode(header.Mode) & 0o777, mtime: header.ModTime})
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(f, r, header.Size)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if err := os.Chmod(target, os.FileMode(header.Mode)&0o777); err != nil {
				return err
			}
			if !header.ModTime.IsZero() {
				if err := os.Chtimes(target, header.ModTime, header.ModTime); err != nil {
					return err
				}
			}
		case tar.TypeSymlink:
			linkTarget := filepath.FromSlash(header.Linkname)
			resolvedLink := path.Clean(path.Join(path.Dir(header.Name), filepath.ToSlash(linkTarget)))
			if filepath.IsAbs(linkTarget) || resolvedLink == ".." || strings.HasPrefix(resolvedLink, "../") || strings.ContainsRune(linkTarget, '\\') {
				return fmt.Errorf("unsafe Docker archive symlink: %s", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			link := filepath.Clean(filepath.FromSlash(header.Linkname))
			if filepath.IsAbs(link) || link == ".." || strings.HasPrefix(link, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("unsafe Docker archive link: %s", header.Linkname)
			}
			if err := os.Link(filepath.Join(root, link), target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported Docker archive entry type %d", header.Typeflag)
		}
	}
}

func dockerEnsureNoSymlinkParents(root, directory string) error {
	rel, err := filepath.Rel(root, directory)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("unsafe Docker archive path")
	}
	current := root
	if rel == "." {
		return nil
	}
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Docker archive traverses symlink parent %s", current)
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func dockerRemoveTree(name string) error {
	err := filepath.WalkDir(name, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return os.Remove(path)
		}
		if entry.IsDir() {
			if err := os.Chmod(path, 0o700); err != nil {
				return err
			}
			return nil
		}
		return os.Chmod(path, 0o600)
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(name)
}

func (r *DockerRuntime) Stop(ctx context.Context, identity string) error {
	if identity == "" || strings.HasPrefix(identity, "-") {
		return fmt.Errorf("Container identity and nonempty argv are required")
	}
	timeout := 2
	_, err := r.client.ContainerStop(ctx, identity, client.ContainerStopOptions{Timeout: &timeout})
	if err != nil && !errdefs.IsNotFound(err) && !strings.Contains(err.Error(), "No such container") {
		return fmt.Errorf("Docker stop failed: %w", err)
	}
	r.mu.Lock()
	if state := r.state[identity]; state != nil {
		state.stopped = true
	}
	r.mu.Unlock()
	return nil
}

func (r *DockerRuntime) Remove(ctx context.Context, identity string) error {
	if identity == "" || strings.HasPrefix(identity, "-") {
		return fmt.Errorf("Container identity and nonempty argv are required")
	}
	_, err := r.client.ContainerRemove(ctx, identity, client.ContainerRemoveOptions{Force: true})
	if err != nil && !errdefs.IsNotFound(err) && !strings.Contains(err.Error(), "No such container") {
		return fmt.Errorf("Docker rm failed: %w", err)
	}
	r.mu.Lock()
	delete(r.state, identity)
	r.mu.Unlock()
	return nil
}

func (r *DockerRuntime) discard(ctx context.Context, identity string) error {
	_ = r.Stop(ctx, identity)
	return r.Remove(ctx, identity)
}

var _ Runtime = (*DockerRuntime)(nil)
var _ Process = (*dockerProcess)(nil)
