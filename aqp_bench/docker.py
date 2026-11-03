"""Small argv-only Docker CLI boundary; image installation is never implicit."""
from __future__ import annotations

import ipaddress
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import uuid


class DockerRuntime:
    def __init__(self):
        default = f'unix:///run/user/{os.getuid()}/docker.sock' if Path(f'/run/user/{os.getuid()}/docker.sock').exists() else 'unix:///var/run/docker.sock'
        self.host = os.environ.get('DOCKER_HOST', default)
        if not self.host.startswith('unix:///') or '\0' in self.host or '\n' in self.host:
            raise ValueError('Only a local Unix-socket Docker daemon is supported')
        # A selected remote context must never override the local-only host.
        self.environment = {k:v for k,v in os.environ.items() if k not in
                            ('DOCKER_CONTEXT', 'DOCKER_HOST', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH')}
        self._containers = {}

    def _argv(self, *args):
        return ['docker', '--host', self.host, *args]

    def _run(self, args, *, timeout=300):
        result = subprocess.run(self._argv(*args), env=self.environment, capture_output=True, text=True, timeout=timeout)
        if result.returncode:
            # Do not reproduce argv: it can contain an ephemeral capability.
            raise RuntimeError(f'Docker operation {args[0]} failed: {result.stderr.strip()}')
        return result

    def start(self, image: str, workspace: Path, *, home: Path | None = None,
              environment: dict | None = None, evaluation: Path | None = None,
              readonly_workspace: bool = False, copy_back: bool = False) -> str:
        mounts = [(Path(workspace).resolve(), '/workspace', readonly_workspace)]
        if evaluation is not None:
            mounts.append((Path(evaluation).resolve(), '/evaluation', True))
        for source, _, _ in mounts:
            if source == Path.home().resolve() or source in Path.home().resolve().parents or not source.is_dir():
                raise ValueError('Container mounts must be dedicated workspace/configuration directories')
            if ',' in str(source) or '\n' in str(source):
                raise ValueError('Container mount paths cannot contain commas or newlines')
            if (source / 'docker.sock').exists():
                raise ValueError('Docker socket mounts are forbidden')
        if home is not None:
            home = Path(home).resolve()
            if home == Path.home().resolve() or home in Path.home().resolve().parents or not home.is_dir():
                raise ValueError('Container staging sources must be dedicated directories')
            if (home / 'docker.sock').exists():
                raise ValueError('Docker socket staging is forbidden')
        if not image or image.startswith('-'):
            raise ValueError('A Docker image is required')
        env = dict(environment or {})
        env['HOME'] = '/home/agent'
        for key, value in env.items():
            if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', key) or not isinstance(value, str) or '\0' in value:
                raise ValueError('Invalid explicit container environment')
        workspace = Path(workspace).resolve()
        markers = self._workspace_markers(mounts)
        try:
            identity = self._launch(image, env, mounts, os.getuid(), os.getgid())
            try:
                self._probe_mounts(identity, markers)
                transport = 'bind'
            except Exception as probe_error:
                self._discard(identity)
                identity = None
                # Docker Desktop's daemon may not see a WSL ext4 source even
                # though the Docker CLI can stream that same source via `cp`.
                for marker, _, _ in markers:
                    marker.unlink(missing_ok=True)
                try:
                    # Start as the agent UID and outside /workspace so Docker
                    # creates copied directories with the correct owner/mode.
                    identity = self._launch(image, env, [], os.getuid(), os.getgid(),
                                            workdir='/tmp')
                    self._stage_mounts(identity, mounts)
                    transport = 'docker_cp'
                except Exception as copy_error:
                    if identity:
                        self._discard(identity)
                    raise RuntimeError('Docker bind mount visibility probe failed '
                                       f'({probe_error}); Docker cp fallback failed ({copy_error})') from copy_error
            if home is not None:
                source, target = home, '/home/agent'
                stage = f'/tmp/aqp-stage-{uuid.uuid4().hex}'
                self._run(['cp', '-a', f'{source}/.', f'{identity}:{stage}'])
                transfer = self.exec_as(identity, f'{os.getuid()}:{os.getgid()}', [
                    'sh', '-c',
                    'cp -a "$1"/. "$2"/ || exit $?; rm -rf "$1"',
                    'aqp-stage', stage, target,
                ], timeout=30)
                if transfer.returncode:
                    detail = ' '.join(part.strip() for part in (transfer.stdout, transfer.stderr)
                                      if part and part.strip())
                    raise RuntimeError(f'Container staging failed for {target} (exit code '
                                       f'{transfer.returncode}): {detail or "copy command failed"}')
            self._containers[identity] = {
                'transport': transport,
                'workspace': workspace,
                'copy_back': copy_back,
                'exec_user': f'{os.getuid()}:{os.getgid()}',
                'readonly_workspace': readonly_workspace,
            }
            return identity
        except Exception:
            if 'identity' in locals() and identity and identity not in self._containers:
                self._discard(identity)
            raise
        finally:
            for marker, _, _ in markers:
                marker.unlink(missing_ok=True)

    @staticmethod
    def _workspace_markers(mounts):
        markers = []
        try:
            for source, target, _ in mounts:
                marker = source / f'.aqp-bind-probe-{uuid.uuid4().hex}'
                value = uuid.uuid4().hex
                descriptor = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                markers.append((marker, target, value))
                with os.fdopen(descriptor, 'w') as stream:
                    stream.write(value + '\n')
            return markers
        except OSError as error:
            for marker, _, _ in markers:
                marker.unlink(missing_ok=True)
            raise RuntimeError('Cannot create temporary Docker visibility probes') from error

    def _launch(self, image, env, mounts, uid, gid, *, workdir='/workspace'):
        args = ['run', '--detach', '--pull=never', '--init', '--name', 'aqp-' + uuid.uuid4().hex,
                '--cap-drop=ALL', '--security-opt=no-new-privileges', '--user', f'{uid}:{gid}',
                '--add-host', 'host.docker.internal:host-gateway', '--workdir', workdir]
        for source, target, readonly in mounts:
            suffix = ',readonly' if readonly else ''
            args += ['--mount', f'type=bind,src={source},dst={target}{suffix}']
        # Keep writable state in container tmpfs, even when WSL paths are not
        # visible to the Docker daemon.
        args += ['--tmpfs', f'/home/agent:uid={uid},gid={gid},mode=0700']
        for key, value in env.items():
            args += ['--env', f'{key}={value}']
        args += ['--entrypoint', 'sh', image, '-c', 'exec sleep infinity']
        identity = self._run(args).stdout.strip()
        if not identity:
            raise RuntimeError('Docker start returned an empty container identity')
        return identity

    def _probe_mounts(self, identity, markers):
        for marker, target, value in markers:
            probe = self.exec(identity, [
                'sh', '-c',
                'IFS= read -r observed < "$1" && [ "$observed" = "$2" ]',
                'aqp-bind-probe', f'{target}/{marker.name}', value,
            ], timeout=10)
            if probe.returncode:
                detail = ' '.join(part.strip() for part in (probe.stdout, probe.stderr)
                                  if part and part.strip())
                raise RuntimeError(f'Docker cannot read bind mount {target} '
                                   f'(exit code {probe.returncode}{": " + detail if detail else ""})')

    def _stage_mounts(self, identity, mounts):
        for source, target, readonly in mounts:
            # Copy a directory, not only its contents: in the fallback
            # container the target does not exist yet, so docker cp creates it
            # with the agent UID and preserves root directory permissions.
            with tempfile.TemporaryDirectory(prefix='aqp-mount-stage-') as staging:
                staged = Path(staging) / target.lstrip('/')
                shutil.copytree(source, staged, symlinks=True)
                if readonly:
                    # Evaluator commands use a distinct UID; remove write bits
                    # on the private staging tree without changing host inputs.
                    for root, directories, files in os.walk(staged, followlinks=False):
                        for name in directories + files:
                            path = Path(root) / name
                            if not path.is_symlink():
                                mode = path.stat().st_mode
                                path.chmod((mode & 0o111) | 0o444)
                        root_path = Path(root)
                        root_path.chmod((root_path.stat().st_mode & 0o111) | 0o555)
                self._run(['cp', '-a', str(staged), f'{identity}:/'])

    def transport_for(self, container: str) -> str | None:
        state = self._containers.get(container)
        return None if state is None else state['transport']

    def export_workspace(self, container: str, workspace: Path) -> None:
        state = self._containers.get(container)
        if state is None or not state['copy_back'] or state['transport'] != 'docker_cp':
            return
        workspace = Path(workspace).resolve()
        if workspace != state['workspace']:
            raise ValueError('Workspace export target does not match the started container')
        stage = Path(tempfile.mkdtemp(prefix='.aqp-workspace-export-', dir=workspace.parent))
        backup = workspace.with_name(f'.{workspace.name}-before-{uuid.uuid4().hex}')
        try:
            self._run(['cp', '-a', f'{container}:/workspace', str(stage)])
            exported = stage / 'workspace'
            os.replace(workspace, backup)
            try:
                os.replace(exported, workspace)
            except Exception:
                os.replace(backup, workspace)
                raise
            self._remove_tree(backup)
            self._remove_tree(stage)
        except Exception:
            if stage.exists():
                self._remove_tree(stage)
            raise

    @staticmethod
    def _remove_tree(path):
        def make_writable(function, target, error):
            Path(target).chmod(0o700)
            function(target)
        shutil.rmtree(path, onerror=make_writable)

    def _discard(self, container):
        for cleanup in (self.stop, self.remove):
            try:
                cleanup(container)
            except Exception:
                pass

    def exec(self, container: str, command: list[str], *, timeout: float = 300):
        self._validate(container, command)
        state = self._containers.get(container)
        args = ['exec']
        if state is not None and state['transport'] == 'docker_cp':
            args += ['--user', state['exec_user']]
        args += ['--workdir', '/workspace', container, *command]
        return subprocess.run(self._argv(*args), env=self.environment,
                              capture_output=True, text=True, timeout=timeout)

    def exec_as(self, container: str, user: str, command: list[str], *, timeout: float = 300):
        self._validate(container, command)
        if not re.fullmatch(r'\d+(?::\d+)?', user):
            raise ValueError('Container execution user must be a numeric UID[:GID]')
        return subprocess.run(self._argv('exec', '--user', user, '--workdir', '/workspace', container, *command), env=self.environment,
                              capture_output=True, text=True, timeout=timeout)

    def spawn(self, container: str, command: list[str], *, capture_stderr=False):
        self._validate(container, command)
        state = self._containers.get(container)
        args = ['exec', '-i']
        if state is not None and state['transport'] == 'docker_cp':
            args += ['--user', state['exec_user']]
        args += ['--workdir', '/workspace', container, *command]
        return subprocess.Popen(self._argv(*args), env=self.environment,
                                stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE if capture_stderr else subprocess.DEVNULL)

    @staticmethod
    def _validate(container, command):
        if not container or container.startswith('-') or not command or any(not isinstance(x, str) for x in command):
            raise ValueError('Container identity and nonempty argv are required')

    def address(self, container: str) -> str:
        self._validate(container, ['inspect'])
        result = self._run(['inspect', '--format', '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}', container])
        return str(ipaddress.IPv4Address(result.stdout.strip()))

    def _cleanup(self, operation, container):
        self._validate(container, ['cleanup'])
        args = self._argv(operation)
        if operation == 'rm':
            args += ['--force']
        else:
            args += ['--time', '2']
        result = subprocess.run([*args, container], env=self.environment, capture_output=True, text=True, timeout=30)
        if result.returncode and 'No such container' not in result.stderr:
            raise RuntimeError(f'Docker {operation} failed: {result.stderr.strip()}')

    def stop(self, container: str) -> None:
        self._cleanup('stop', container)

    def remove(self, container: str) -> None:
        self._cleanup('rm', container)
        self._containers.pop(container, None)
