# Maintainer playbook

[Back to the Codebase Guide](../CODEBASE-GUIDE.md)

## Checklists by change type

| Change | Do | Also update |
|---|---|---|
| New or changed CLI command/flag | Edit `cli.go`; test exit code, stdout, stderr; confirm sibling commands unchanged | README, usage text |
| Manifest schema | Reject unknown keys; keep version 1 data loading | [manifests](../manifests.md), tests |
| Agent adapter | Normalize to lifecycle events; never fabricate attribution | [agents](../agents.md) |
| Gateway or accounting | Count every attempt; no double-counted usage; redact keys | [gateway](../gateway.md) |
| Docker runtime / harness | Keep UID/GID, dropped caps, no socket or HOME mounts | [security](../security.md) |
| Evaluation / scoring | Keep hidden tests separate; per-test weights | README scoring section |
| Persisted format | Keep existing SQLite/JSON/JSONL readable (high risk) | migration notes in `odd/tasks/` |
| New benchmark | Add `benchmarks/<task>/` with README, manifest, verifier that checks real outcomes | none unless schema changes |
| New image | Add `images/<name>/` with its own dockerignore | [images README](../../images/README.md) |

## Checks before merging

```bash
gofmt -l .      # prints nothing
go vet ./...
go test ./...
go test -race ./...
```

Docker-dependent changes also run `AQP_DOCKER_INTEGRATION=1 go test ./internal/bench -run '^TestDockerIntegration' -count=1`.

## PR review guardrails

- Is there an agreed issue, and does the diff stay within it (about 400 changed lines)?
- Is the AI-assistance declaration complete ([AI policy](../../AI_POLICY.md))?
- Does the change fix the responsible invariant rather than mask a symptom?
- Do tests assert observable effects through the public interface, and was a prior-behavior test added for shared code?
- Were scores, costs, or compatibility claims actually observed? Simulation is not live evidence.
- No credentials, run artifacts, transcripts, or Docker socket/HOME exposure introduced.
- Docs updated in the same PR.

## High-risk changes

Treat these as needing a second maintainer's review: persisted data formats, security/isolation, CLI/manifest contracts others consume, concurrency in runner/gateway/store, and CI, release, or dependency changes.
