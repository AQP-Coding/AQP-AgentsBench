"""Small argv-only Docker CLI boundary; image installation is never implicit."""
from __future__ import annotations

import ipaddress
import os
from pathlib import Path
import re
import subprocess
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
              readonly_workspace: bool = False) -> str:
        mounts = [(Path(workspace).resolve(), '/workspace', readonly_workspace)]
        if home is not None:
            mounts.append((Path(home).resolve(), '/home/agent', False))
        if evaluation is not None:
            mounts.append((Path(evaluation).resolve(), '/evaluation', True))
        for source, _, _ in mounts:
            if source == Path.home().resolve() or source in Path.home().resolve().parents or not source.is_dir():
                raise ValueError('Container mounts must be dedicated workspace/configuration directories')
            if ',' in str(source) or '\n' in str(source):
                raise ValueError('Container mount paths cannot contain commas or newlines')
            if (source / 'docker.sock').exists():
                raise ValueError('Docker socket mounts are forbidden')
        if not image or image.startswith('-'):
            raise ValueError('A Docker image is required')
        args = ['run', '--detach', '--pull=never', '--init', '--name', 'aqp-' + uuid.uuid4().hex,
                '--cap-drop=ALL', '--security-opt=no-new-privileges', '--user', f'{os.getuid()}:{os.getgid()}',
                '--add-host', 'host.docker.internal:host-gateway', '--workdir', '/workspace']
        for source, target, readonly in mounts:
            suffix = ',readonly' if readonly else ''
            args += ['--mount', f'type=bind,src={source},dst={target}{suffix}']
        if home is None:
            args += ['--tmpfs', f'/home/agent:uid={os.getuid()},gid={os.getgid()},mode=0700']
        env = dict(environment or {})
        env['HOME'] = '/home/agent'
        for key, value in env.items():
            if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', key) or not isinstance(value, str) or '\0' in value:
                raise ValueError('Invalid explicit container environment')
            args += ['--env', f'{key}={value}']
        # Explicit entrypoint avoids invoking an arbitrary image startup service.
        args += ['--entrypoint', 'sh', image, '-c', 'exec sleep infinity']
        return self._run(args).stdout.strip()

    def exec(self, container: str, command: list[str], *, timeout: float = 300):
        self._validate(container, command)
        return subprocess.run(self._argv('exec', container, *command), env=self.environment,
                              capture_output=True, text=True, timeout=timeout)

    def spawn(self, container: str, command: list[str]):
        self._validate(container, command)
        return subprocess.Popen(self._argv('exec', '-i', container, *command), env=self.environment,
                                stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL)

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
