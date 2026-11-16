# Contributing to AQP-AgentsBench

Thank you for your interest in **AQP-AgentsBench** — local, outcome-based benchmarks for AI coding agents, written in Go with Docker images and Python task verifiers.

Please read this guide before opening a pull request. A small, predictable workflow keeps a small team's review load manageable.

## Table of Contents

- [Issue-First Workflow](#issue-first-workflow)
- [AI-Assisted Contributions](#ai-assisted-contributions)
- [Development Setup](#development-setup)
- [Testing](#testing)
- [Documentation](#documentation)
- [Commit Convention](#commit-convention)
- [Pull Request Rules](#pull-request-rules)
- [Security and Credentials](#security-and-credentials)
- [Code of Conduct](#code-of-conduct)

## Issue-First Workflow

**Open an issue before a non-trivial PR.** Typos and small doc fixes may go straight to a PR.

1. Search existing issues, then open one describing the problem or proposal, with commands and observed output.
2. Wait for a maintainer to agree on scope. Behavior changes to manifests, CLI commands, persisted data, or the gateway need agreement first because others consume them.
3. Comment that you are taking it.
4. Open a PR that references the issue (`Closes #N` or `Refs #N`).

Questions and ideas belong in GitHub Discussions, not issues.

## AI-Assisted Contributions

**AI assistance is allowed, but you must understand and own the complete submission.** Before opening a PR:

- [ ] Confirm the change matches the agreed issue scope.
- [ ] Inspect every changed line.
- [ ] Remove invented, unverifiable, or unrelated output (APIs, paths, results).
- [ ] Identify the responsible cause or invariant; confirm you fixed it rather than masking the symptom.
- [ ] Keep the change proportionate: no duplicate authority or needless abstractions.
- [ ] Run the applicable tests and report actual outcomes.
- [ ] Be ready to explain the design and tradeoffs.
- [ ] Disclose material AI assistance in the PR.

The canonical rules are in the [AI-Assisted Contribution Policy](AI_POLICY.md). Benchmark scores, costs, and agent-compatibility claims must come from runs you actually performed.

## Development Setup

Requirements: Linux x86_64, Go 1.25 or later, and Python 3.11 or later (only for the retained offline verifiers/task fixtures). Docker is needed only for opt-in integration tests and image builds.

```bash
git clone https://github.com/AQP-Coding/AQP-AgentsBench.git
cd AQP-AgentsBench
go build ./...
go run ./cmd/aqp-bench --artifacts /tmp/aqp-demo demo   # offline, no key, no Docker
```

Start with the [Codebase Guide](docs/CODEBASE-GUIDE.md) to find the package that owns a behavior.

## Testing

Run these from the repository root before every PR:

```bash
gofmt -l .          # must print nothing
go vet ./...
go test ./...
go test -race ./...
```

Opt-in local Docker integration tests (they create and clean up their own containers; build the documented [image](images/README.md) first, tests never pull):

```bash
AQP_DOCKER_INTEGRATION=1 go test ./internal/bench -run '^TestDockerIntegration' -count=1
```

Rules:

- Write the failing test first when a deterministic test is possible, and assert every observable effect (output, exit code, persisted data) through the public interface.
- Never make paid model calls or require credentials in the default test suite. Use the local mock provider.
- Real agent/OpenRouter runs are billable and **separately authorized**; report the command, image, model, and observed result if you ran one.
- Python verifiers under `benchmarks/` must check real final outcomes, never skipped or empty suites.

## Documentation

If behavior changes, update the matching docs in the same PR:

| Change | Update |
|---|---|
| CLI commands, flags, or messages | `README.md` and the command's usage text |
| Manifest schema | [docs/manifests.md](docs/manifests.md) |
| Agent adapters or Docker runtime | [docs/agents.md](docs/agents.md) |
| Gateway or accounting | [docs/gateway.md](docs/gateway.md) |
| Isolation or credential handling | [docs/security.md](docs/security.md) |
| Package ownership or layout | [docs/codebase/repository-map.md](docs/codebase/repository-map.md) |

Document exported Go symbols following [Go Doc Comments](https://go.dev/doc/comment). State verified facts only; mark unproven behavior as unverified, as the README does.

## Commit Convention

This project uses [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): description`.

| Type | Use |
|---|---|
| `feat` | New feature |
| `fix` | Bug fix |
| `docs` | Documentation only |
| `refactor` | No functional change |
| `test` | Tests only |
| `chore` | Build, tooling, dependencies |
| `revert` | Reverts a previous commit |

Examples: `feat(cli): add rerun --trials`, `fix(gateway): honor IPv4 listener addresses`.

Organize commits by deliverable unit, not file type: one behavior with its code, tests, and docs. Do not add `Co-Authored-By`, `Reviewed-by`, or `Signed-off-by` trailers for AI tools.

## Pull Request Rules

- Link the issue and fill in the [PR template](.github/PULL_REQUEST_TEMPLATE.md), including the AI-assistance declaration.
- Aim for **about 400 changed lines or fewer** (additions + deletions). Split larger work into reviewable, ordered PRs.
- Keep the change within the agreed scope; unrelated cleanups go in a separate PR.
- All checks in [Testing](#testing) pass, and you report any you could not run.
- Never push artifacts, workspaces, transcripts, or keys.

## Security and Credentials

Never commit API keys, gateway tokens, or run artifacts. Agent containers must not receive the host Docker socket, host HOME, or provider keys; see [docs/security.md](docs/security.md). Report vulnerabilities privately to the maintainers (GitHub Security Advisories), not in public issues.

## Code of Conduct

Be respectful. We're building something together.

- Critique code, not people.
- Be constructive in reviews.
- Welcome newcomers.

Violations may result in removal from the project.
