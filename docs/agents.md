# Agent and container integration

The adapters implement Pi JSONL RPC and OpenCode v1.2 HTTP APIs. Deterministic
fixtures prove the local protocol behavior; **a real Docker/agent/model run has
not been verified**. The offline demo is not agent-compatibility evidence.

## Prepare an image

Provide an already-built Linux x86_64 image with `sh`, `sleep`, the chosen agent,
and the task's tools installed. Images must work under the invoking host UID/GID,
without root-only caches or installation steps. The runtime never installs tools
or pulls an image. Images and selected executable harness extensions are trusted
inputs, not a security boundary against hostile programs.

Only local Unix-socket Docker daemons are supported. `DOCKER_HOST=unix:///...`
can explicitly select a local/rootless socket; SSH/TCP Docker hosts are rejected
without probing them. The runtime ignores ambient Docker context selection and
pins the selected local socket for every operation. It does not inspect or reuse
SSH sessions, registry logins, or remote daemon credentials.

| Runtime boundary | Behavior |
|---|---|
| Workspace | Dedicated host directory mounted at `/workspace` |
| Configuration | Selected isolated home at `/home/agent`; no host HOME inheritance |
| User | Host UID/GID, dropped capabilities, no new privileges |
| Networking | Normal bridge egress and `host.docker.internal` host-gateway alias |
| Server | OpenCode listens inside its bridge container, authenticated with an ephemeral password; no published host port |
| Evaluation | Separate container with a fresh snapshot, optional read-only `/workspace`, and trusted read-only `/evaluation` |
| Cleanup | Caller stops container before evaluation and removes it afterward |

`DockerRuntime.start(image, workspace, *, home=None, environment=None,
evaluation=None, readonly_workspace=False)` returns a container identity.
`exec(container, argv, timeout=300)` returns a text `CompletedProcess` so the caller
checks the exit code. `spawn(container, argv)` returns binary stdin/stdout pipes;
stderr is discarded rather than risking a blocked unread pipe. Protocol errors
remain actionable; discarded diagnostic stderr is a troubleshooting limitation.
`address(container)` returns its bridge IPv4 address. `stop`/`remove` tolerate
missing containers, but do not suppress daemon failures. Host-to-bridge HTTP
requires native Linux Docker networking; Docker Desktop/WSL is not proven.

## Select a harness without inheriting secrets

Both explicit and inherit modes stage the manifest's selected `files` only.
`harness.stage(files, home)` preserves bytes and directory trees; it rejects
traversal, symlinks, overlapping targets, credential filenames, recognized private
keys, and literal credential fields in JSON. It does not infer or copy host
configuration automatically. Review arbitrary selected text/code yourself:
secret-pattern checks are not a universal secret detector.

`harness.configure(home, kind, model, gateway_url, token)` preserves unrelated
settings, instructions, hooks, skills, and plugins. It adds the `aqp` provider and
returns the **only** explicit environment the caller should pass to the container.
The local gateway token is an environment reference, never literal configuration;
the upstream OpenRouter key must never enter the container. Conflicting providers,
model defaults, credential stores, and ambiguous JSONC are rejected instead of
silently discarded. Pi gets gateway model defaults; OpenCode also gets runtime
inline overrides. [Pi model configuration](https://raw.githubusercontent.com/badlogic/pi-mono/main/packages/coding-agent/docs/models.md)
and [OpenCode provider configuration](https://opencode.ai/docs/providers/) describe
these custom-provider APIs. [OpenCode precedence](https://opencode.ai/docs/config/)
puts runtime inline configuration after project configuration.

Call `harness.validate_workspace(workspace, kind, model)` before execution to
reject known repository-level provider/model overrides. Executable plugins may
spawn their own clients, acquire credentials, or alter networking; normal outbound
egress is NOT a forced-routing firewall. Such harness behavior is unsupported and
must be audited before comparing runs. Do not claim complete accounting for an
arbitrary harness whose requests bypass the gateway.

## Completion and human replies

Pi starts with `--mode rpc --no-session --provider aqp --model MODEL`. It consumes
UTF-8 LF-delimited JSON continuously. Prompt acceptance is not completion:
`agent_end` may precede retries/compaction, so the adapter requires `agent_settled`.
A handled prompt is the documented no-run exception. Earlier Pi versions lacking
that event fail closed when the process exits; configure an agent-time limit to
bound an older long-lived process. See [Pi RPC](https://raw.githubusercontent.com/badlogic/pi-mono/main/packages/coding-agent/docs/rpc.md).

Pi dialog `ask` metadata contains the complete request. Replies are booleans for
`confirm`, strings for `select`/`input`/`editor`, or `None` to cancel. Notification
methods do not count as interventions. Unsupported terminal-only extension widgets
cannot be preserved through RPC; see [Pi extension UI](https://raw.githubusercontent.com/badlogic/pi-mono/main/packages/coding-agent/docs/rpc-extension-ui.md).

OpenCode targets the v1.2 server surface, with v1.2.27 source as the contract
reference, not a live-tested version pin. Other API families are rejected by the
health version check. It creates a session, subscribes to SSE before dispatch,
and uses the blocking `/session/:id/message` response, not asynchronous prompt
acceptance. A daemon HTTP reader remains active during human input.
[Server API](https://opencode.ai/docs/server/) documents this lifecycle.
Permission replies must be `once`, `always`, or `reject`; see the
[v1.2.27 permission route](https://raw.githubusercontent.com/anomalyco/opencode/v1.2.27/packages/opencode/src/server/routes/permission.ts).
Question replies are `string[][]`, one string-list per question, or `None` to
reject; see the [question route](https://raw.githubusercontent.com/anomalyco/opencode/v1.2.27/packages/opencode/src/server/routes/question.ts)
and [generated types](https://raw.githubusercontent.com/anomalyco/opencode/v1.2.27/packages/sdk/js/src/v2/gen/types.gen.ts).

Human callbacks run separately from readers; stop remains observable while a
callback waits. The CLI owns counting submitted replies. Cancellation stops the
container, not just the local `docker exec` client. A blocked host callback cannot
be forcibly killed and must respect the enclosing CLI's cancellation lifecycle.

## Attribution and measurement limits

Pi RPC provides no standardized child-agent identity in these records; only its
observed main lifecycle is normalized. OpenCode child sessions are attributed by
actual `session.created` parent IDs and observed child idle statuses. Opaque
external subprocesses are not invented as measured children. Child restarts or
missed events need separate capability proof before claiming exact child timing.
Neither provider configuration invents an `X-AQP-Agent-ID`: a provider shared by
main and child sessions cannot honestly label every call `main`. Gateway requests
remain `unattributed` unless an audited harness supplies real request identities.
Run-level gateway totals still include every observed provider attempt.

No adapter automatically retries a whole run, classifies every protocol error as
infrastructure failure, or uses agent-supplied token/cost estimates as billing
truth. Configuring one provider is not proof of live compatibility or exhaustive
subagent accounting.

## Local checks

```bash
PYTHONPATH=. pytest -q -p no:cacheprovider tests/test_adapters.py tests/test_docker.py tests/test_harness.py
```

These checks use fake Docker calls, local Python subprocesses, and a loopback HTTP
fixture. No agent image, network provider, paid request, or ambient credential is
used. A user-authorized live smoke test remains required for each selected image,
agent version, and executable harness combination.
