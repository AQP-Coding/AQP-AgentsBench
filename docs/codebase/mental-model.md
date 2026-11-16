# Mental model

[Back to the Codebase Guide](../CODEBASE-GUIDE.md)

## What the project is

A local benchmark platform: manifests in, scored and fully retained run records out. It measures agent *outcomes* (hidden tests on the final workspace), plus tokens, cost, requests, agent time, and human interventions.

## What it is not

- Not a hosted service: no remote Docker contexts, no cloud storage, no telemetry.
- Not proof an agent works: the `demo` is explicitly `simulation: true`.
- Not a sandbox against hostile code: containers use ordinary Docker isolation (dropped capabilities, no-new-privileges, host UID/GID).
- Not a place that invents data: missing subagent attribution or unknown billing stays unknown.

## What must be preserved

| Invariant | Where it lives |
|---|---|
| Hidden tests run separately, after the agent container stops, on a fresh read-only snapshot | `internal/bench/evaluation.go`, [security](../security.md) |
| Every provider attempt (including retries and failures) is counted | `internal/bench/gateway.go`, [gateway](../gateway.md) |
| Real provider key stays on the host; containers get an ephemeral capability | `gateway.go`, `docker.go` |
| Only explicitly selected harness files are copied; no whole-HOME inheritance | `harness.go` |
| Human-wait time is excluded; overlapping active work counts once | `metrics.go` |
| Per-test weighted scoring; broken evaluator is an evaluation failure, not a zero score | `scoring.go`, `evaluation.go` |
| Persisted SQLite/JSON/JSONL formats stay compatible | `store.go`, `artifacts.go` |
| Infrastructure-interrupted runs are excluded from aggregate scores and never auto-retried | `runner.go` |
| Only committed repository contents are used; the source repo is never modified | `workspace.go` |

## Language boundary

Go owns the host engine and its tests. Python remains only for selected task fixtures and verifiers (`aqp_bench/example/`, `benchmarks/*/check.py`). Docker recipes live in `images/<name>/`.

## Verification levels

Offline `demo` → Docker gateway probe → live `run`. Each proves less than the next costs; never present a lower level as evidence of a higher one.
