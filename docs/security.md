# Local benchmark security boundaries

## What the platform isolates

- Agent containers receive only their dedicated workspace and selected configuration HOME. They do not receive the host Docker socket, host HOME, upstream provider credentials, inherited SSH agents, or ambient environment variables.
- Docker operations are pinned to a local Unix socket. Remote SSH/TCP Docker transports are refused without probing; images are already local (`--pull=never`). A Docker daemon is a powerful host service: use trusted local images.
- Git preparation uses an isolated configuration HOME, disables credential helpers, prompts, executable hooks and file-system monitors, and removes ambient Git/SSH/proxy overrides. Only public HTTP(S) repository URLs and explicitly selected local repositories are supported; private-repository authentication is not inherited.
- Capabilities are dropped, no-new-privileges is set, and containers run as the invoking UID/GID. This is ordinary Docker isolation, not a virtual machine or a hardened hostile-code research sandbox.
- Hidden evaluator files are unavailable during agent execution. The agent container is stopped before a separate evaluator sees read-only tests and a fresh read-only workspace snapshot. The agent never writes the host score record directly.
- The model proxy requires a random per-run capability and forwards only to a fixed upstream. The real provider key is never passed to containers. Known provider keys and gateway tokens are redacted from platform event/error artifacts. Provider prompts/bodies are not logged by the gateway.
- The dashboard binds to loopback, validates Host, renders text rather than injected HTML, and exposes no mutation routes. Run controls and human responses are CLI-only.

## What is not guaranteed

Normal container internet access is intentional. An executable harness plugin can create its own clients, acquire credentials, or change its behavior. Known conflicting routing settings are rejected, but configuration is not an egress firewall. Only audited harnesses whose requests use the gateway can provide complete usage. Missing request/subagent identities are explicitly unknown; no synthesized child billing is added to parent totals.

Selected images, setup commands, evaluator commands, and executable harness resources are trusted operator inputs. Treat arbitrary repositories and their generated code as untrusted. In particular, importing hostile generated Python/JavaScript into the same process as an evaluator can compromise that process, even with read-only test files. Outcome tests should isolate generated applications behind subprocess/service boundaries and assert results; this initial platform does not offer a universal adversarial anti-cheating guarantee.

Secrets embedded in arbitrary user prompts, attachments, generated files, tool output, or unrecognized harness code cannot be detected reliably. Do not put credentials in benchmark inputs. Keep the artifact directory private. Recognized credential files and literal JSON secrets are rejected during harness selection, but this is not a comprehensive secret scanner. The offline simulation is trusted local code, not a sandbox for user-supplied workloads.

The gateway may bind all host interfaces for Docker bridge reachability; its capability is required, but use host firewall rules to restrict who can connect. No claim is made that it is suitable for public Internet hosting. Provider timing, in-flight cost overshoot, and missing final billing prevent a hard spend guarantee. An interrupted request remains visible with unknown usage rather than disappearing.

## Live proof still required

A real smoke test must explicitly select the local prepared image, agent version, harness configuration, model, credential environment variable, and spending limits. It must exercise a human approval, subagent behavior where available, cancellation, hidden evaluation, and gateway usage. No such billable run was authorized or performed during implementation. Gentle-ai compatibility and exact per-subagent measurements must not be claimed from mocked protocol tests alone.
