# Repository map

[Back to the Codebase Guide](../CODEBASE-GUIDE.md)

## Top level

| Path | Owns | Rule |
|---|---|---|
| `cmd/aqp-bench/` | CLI entrypoint (`main.go` calls `bench.CLI`) | No logic here. |
| `internal/bench/` | The whole host engine and its Go tests | Internal package, not a published library API. |
| `aqp_bench/example/` | Embedded offline demo task (CSV, `check.py`, `campaign.json`) | Loaded by `demo`; keep it deterministic and offline. |
| `benchmarks/<task>/` | Campaign manifests, fixtures, Python verifiers per benchmark | One directory per task with a README stating inputs and checks. `benchmarks/artifacts/` is local output, git-ignored. |
| `images/<name>/` | Dockerfile and dependency manifests per agent image | One stack per image; no secrets in build args or layers. |
| `docs/` | Contracts and reference docs | Update in the same PR as behavior. |
| `odd/tasks/` | ODD feature records: specs, tasks, evidence | One file per feature; the log is append-only. |

## `internal/bench/` files

| Responsibility | Files |
|---|---|
| Inputs and snapshots | `manifest.go`, `workspace.go` |
| Timing, accounting, scoring | `metrics.go`, `scoring.go` |
| Persistence and change capture | `store.go`, `artifacts.go` |
| Container runtime and configuration | `docker.go`, `harness.go` |
| Agent protocols | `adapters.go` (Pi RPC, OpenCode HTTP/SSE) |
| Model proxy | `gateway.go` |
| Checks and orchestration | `evaluation.go`, `runner.go`, `answers.go` |
| User surfaces | `cli.go`, `dashboard.go` (+ `dashboard.html`, `dashboard.js`) |
| Offline simulation | `demo.go` |
| Shared types and package docs | `types.go` |

Tests sit beside each file (`*_test.go`). Docker and gateway integration tests are opt-in via `AQP_DOCKER_INTEGRATION=1`.

## Placement rules

- Agent-specific protocol code belongs in `adapters.go`; reusable orchestration belongs in `runner.go`.
- Anything that handles credentials or isolation must be reflected in [security](../security.md).
- A new CLI command touches `cli.go`, its tests, and the README.
- A new benchmark touches only `benchmarks/<task>/` unless the manifest schema changes.
