# AQP-AgentsBench Codebase Guide

> [!NOTE]
> These docs track `main`, which may include unreleased changes. Status claims follow the [README](../README.md): the offline demo and deterministic tests are verified; real agent/OpenRouter compatibility is not.

This guide helps contributors and maintainers find the right code path before changing AQP-AgentsBench. It is an index, not a full API reference.

## Who this is for

| Reader | Use this guide to |
|---|---|
| New maintainer | Build the project mental model before touching files. |
| Contributor | Find the package or file that owns a behavior. |
| Benchmark author | Learn where manifests, task fixtures, and verifiers live. |
| Reviewer | Check whether a PR changes the right layer. |

## 90-second mental model

AQP-AgentsBench is a Go CLI that runs outcome-based benchmarks for AI coding agents (Pi, OpenCode, optionally with harnesses such as Gentle-ai) on **Linux x86_64**. A campaign manifest expands into runs (agent × model × harness × trial). Each run gets an isolated workspace and container, all model traffic goes through a local gateway that accounts for every request, and hidden checks are run separately afterwards. Everything stays on your machine.

```text
campaign.json (manifest)
  |
  v
aqp-bench CLI  (validate | run | demo | list | show | stop | questions | answer | rerun | dashboard)
  |
  +--> manifest + workspace: validate inputs, snapshot committed repo contents
  +--> runner: bounded concurrency, limits, interrupted-run handling
  |      +--> docker runtime + harness: isolated container, selected config only
  |      +--> adapter: Pi JSONL RPC / OpenCode HTTP+SSE -> normalized events
  |      +--> gateway: per-run capability, fixed OpenRouter upstream, per-attempt accounting
  |      +--> metrics: agent time (human-wait excluded), tokens, cost, requests
  |
  +--> evaluation (separate container, hidden tests) --> scoring (per-test weights)
  |
  v
store (SQLite) + artifacts (diff, transcript, results)  <--  read-only dashboard on loopback
```

Golden rule: **the host engine (Go, `internal/bench`) owns orchestration, accounting, and scoring; task images and Python verifiers own only the task's tools and outcome checks.** Agents never see hidden tests, provider keys, the Docker socket, or the host HOME.

## Guide pages

| Page | Job |
|---|---|
| [Mental model](codebase/mental-model.md) | Understand what the project is, is not, and must preserve. |
| [Repository map](codebase/repository-map.md) | Find file ownership and placement rules. |
| [Maintainer playbook](codebase/maintainer-playbook.md) | Use checklists by change type and PR review guardrails. |

## Recommended reading path

1. Start with [Mental model](codebase/mental-model.md).
2. Use [Repository map](codebase/repository-map.md) before editing code.
3. Read the contract page for your area: [manifests](manifests.md), [agents](agents.md), [gateway](gateway.md), or [security](security.md).
4. Finish with [Maintainer playbook](codebase/maintainer-playbook.md) before opening or reviewing a PR.

## Existing references

| Page | Job |
|---|---|
| [README](../README.md) | Quick start, campaign workflow, scoring, artifacts. |
| [Manifests](manifests.md) | Version 1 campaign JSON schema and validation rules. |
| [Agents](agents.md) | Pi/OpenCode adapters, Docker runtime boundary, attribution gaps. |
| [Gateway](gateway.md) | OpenRouter forwarding, accounting, and protocol evidence. |
| [Security](security.md) | What the platform isolates and what it does not. |
| [Contracts](contracts.md) | Internal Go engine boundaries between units. |
| [Images](../images/README.md) | Building local agent task images. |
| [CONTRIBUTING](../CONTRIBUTING.md) / [AI policy](../AI_POLICY.md) | Workflow and contribution rules. |

## Next step

Read [Mental model](codebase/mental-model.md) next.
