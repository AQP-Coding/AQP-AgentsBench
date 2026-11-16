# Documentation conventions

Objective: adapt gentle-ai's documentation conventions to AQP-AgentsBench. Branch: `docs/adopt-documentation-conventions` (base `eccc521`). No push.
Delivery: local work-unit commits on the feature branch; push/PR not authorized. Test runner: `go test ./...` (docs-only; link and fact checks by script).

## Specs

- **S1 — LICENSE.** User chose: MIT, "Copyright (c) 2026 AQP-Coding".
- **S2 — AI_POLICY.md.** Adapt gentle-ai `AI_POLICY.md` (human responsibility, disclosure, attribution, submission quality, enforcement by reviewer judgment). Drop RDD/gentle-ai-specific sections.
- **S3 — CONTRIBUTING.md.** Adapt gentle-ai structure (workflow, AI-assisted checklist, dev setup, testing, commits, PR rules, conduct) to this Go repo's real commands: `go test ./...`, `go test -race ./...`, `go vet ./...`, opt-in `AQP_DOCKER_INTEGRATION=1`. No claims of CI, labels, or bots that do not exist here.
- **S4 — CONTRIBUTORS.md.** Maintainer + contributor table ordered by merged PRs then login; seeded only from git history evidence.
- **S5 — docs/CODEBASE-GUIDE.md.** "90-second mental model", "Guide pages", "Recommended reading path", "Existing references"; lean `docs/codebase/` pages: mental-model, repository-map, maintainer-playbook. User: "Yes: CODEBASE-GUIDE.md plus a lean docs/codebase/ set (mental-model, repository-map, maintainer-playbook) and a PR template".
- **S6 — PR template.** `.github/PULL_REQUEST_TEMPLATE.md` with AI-assistance declaration and this repo's test plan.
- **S7 — Accuracy.** Every path, command, and claim must exist in this repo; unverified live-agent status stays honest (README: real agent/OpenRouter not verified).

## Tasks

- [x] T1 — S1,S2 — inline — LICENSE, AI_POLICY.md
- [x] T2 — S3,S6 — inline — CONTRIBUTING.md, PR template
- [x] T3 — S4 — inline — CONTRIBUTORS.md
- [x] T4 — S5,S7 — inline — CODEBASE-GUIDE + docs/codebase/*; README links

## Log

- L1 (2026-11-16): "I have a small group of developers and I am launching a new open-source repo in GitHub, "AQP-AgentsBench" in this workspace, which consist on automatic benchmarks for AI agents, the codebase is 99% in Go and some files in Docker and Python. In this workspace you aslo have the codebase from another project called "gentle-ai" which has good documentation, I need you to review all the documentation conventions specially but not limited to:  AI_POLICY, CONTRIBUTING, CONTRIBUTORS, LICENSE, docs/CODEBASE-GUIDE.md (90s mental model, guide pages, recommended reading path). The idea is to adequate these conventions to the "AQP-AgentsBench" project, please use ODD and create a new branch for this, you are not alowed to push."
- L2 (2026-11-16): license answer: "MIT, "Copyright (c) 2026 AQP-Coding"". Scope answer: "Yes: CODEBASE-GUIDE.md plus a lean docs/codebase/ set ... and a PR template".
- Route: all inline; no Writer trigger (units small, sequential, share facts).
- Evidence: all relative links in README/docs/*.md resolve (script); `gofmt -l .` clean; commits T1 abf45e2, T2/T3/T4 see `git log`. Not run: `go test` (docs-only, no Go changes). Open: maintainer GitHub handles in CONTRIBUTORS.md are placeholders.
- L3 (2026-11-16): "The Issue template is missing, please add it" → S8: `.github/ISSUE_TEMPLATE/{bug_report,feature_request,config}.yml` adapted from gentle-ai (default `bug`/`enhancement` labels; no status/type label system, which this repo lacks). T5 done, YAML parses.
