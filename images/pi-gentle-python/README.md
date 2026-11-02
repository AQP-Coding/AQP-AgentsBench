# Python 3.11 with Pi and Gentle

Build instructions and the local image tag are in the [image catalog](../README.md). The default command follows the requested Python example: install `.[dev]` and run `pytest -p no:tldr -v`. The benchmark replaces that command when it starts an agent container.

## Dependency ownership

| Dependencies | Manifest |
|---|---|
| Python project and pytest | Repository-root `pyproject.toml`, `[project.optional-dependencies].dev` |
| Pi 1.5.0 and gentle-pi 4.5.1 | `package.json` and generated `package-lock.json`; installed with `npm ci` |
| Gentle-ai binary | The pinned gentle-pi package's integrity-verified private installer; the global `gentle-ai` command links to that same binary |
| OS tools | `apt-packages.txt` |
| Python/Node runtimes | Dockerfile base images, on the same Debian Bookworm family |

Python dependencies are not duplicated in Dockerfile `pip install` arguments. Node packages need their native npm manifest rather than a misleading Python requirement. The npm graph is locked, including package integrity; base tags, apt repositories and Python's existing version ranges are not a promise of bit-for-bit reproducible future builds. Pin base digests and Python/apt resolutions if a benchmark campaign requires that stronger guarantee.

## Select the harness explicitly

`pi` is the plain agent. **Use `gentle-shell --link` to load the Gentle harness with the benchmark's gateway configuration**:

```json
{
  "agents": [{"id": "pi-gentle", "kind": "pi", "command": ["gentle-shell", "--link"]}]
}
```

Set each task's `image` to `aqp-agentsbench/pi-gentle-python:0.1.0`. The same image can run Python evaluation commands; stage hidden tests separately through the existing evaluation manifest.

Bare `gentle-shell` creates an isolated `~/.gentle-shell/agent` home, hiding the
benchmark's generated provider settings. Link mode preserves the explicit
`PI_CODING_AGENT_DIR=/home/agent/.pi/agent`. This was checked without a model call:
`get_state` and `get_available_models` expose the seeded `aqp` gateway model only
with `--link`. The current combination emits a benign duplicate `codemode` tool
notice; Gentle's own tool remains loaded.

Packages and the native Gentle-ai binary live under `/opt/aqp/agent`, not inside `/home/agent`, so a fresh benchmark HOME does not hide the launcher or its package-local runtime. The image defaults to a non-root user and its tool files are readable/executable by the benchmark's host UID. No model credentials, host configuration or provider defaults are baked in; the benchmark supplies its gateway configuration at runtime.

Automatic self-updates and floating first-run companion provisioning are disabled. This base includes Gentle Shell's core harness, not every optional Engram/web-access companion service. Additional companions must be selected and pinned explicitly; do not silently enable them during a measured run. Installing only a generic Gentle-ai binary would not activate Pi integration, and substituting a different binary for Gentle Shell's private runtime is unsupported.

## Offline smoke checks

```bash
docker --host unix:///var/run/docker.sock run --rm --network none \
  aqp-agentsbench/pi-gentle-python:0.1.0
docker --host unix:///var/run/docker.sock run --rm --network none \
  --entrypoint sh aqp-agentsbench/pi-gentle-python:0.1.0 \
  -c 'python --version && node --version && pi --version && gentle-ai version && gentle-shell --version'
```

These checks make no model calls. A real prompt through OpenRouter remains a separate, explicitly budgeted validation step. The image's default suite uses loopback test fixtures; `--network none` prevents external traffic, not local loopback sockets.

Offline image checks also passed as UID:GID `12345:12345` with fresh writable
tmpfs HOME/workspace. On the tested WSL/Docker setup, a host `/tmp` bind mount
appeared root-owned in a different daemon namespace; verify host-path sharing
before a full benchmark run. `/app` is the read-only/root-owned test tree, not
the writable agent workspace. These host-mount concerns are separate from image
tool availability and credential-free RPC startup.

Primary references: [Pi package](https://registry.npmjs.org/@earendil-works/pi-coding-agent/1.5.0), [Gentle-pi package](https://registry.npmjs.org/gentle-pi/4.5.1), [Gentle-ai Pi integration](https://github.com/Gentleman-Programming/gentle-ai/blob/v4.3.1/docs/pi.md), and [Gentle Shell](https://github.com/Gentleman-Programming/gentle-shell).
