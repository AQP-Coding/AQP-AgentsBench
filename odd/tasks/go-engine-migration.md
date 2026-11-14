# Go engine migration
Objective: replace the Python benchmark engine and its unit tests with Go while retaining Python benchmark verifiers/task fixtures; baseline `df22db968eca520955c9e193b5f445e628e56bbf`.
Branch: tracker `feat/go-engine-migration`; delivery: `auto-chain`, `feature-branch-chain` (local branches/commits only, no push); forecast: 10,000–15,000 authored changed lines, including removal of Python engine/tests; checks: focused Go tests, `go test ./...`, `go test -race ./...`, `go vet ./...`, offline CLI/format compatibility checks.

## Specs

- **S1 — Engine language:** "I need you to rewrite the codebase in Go language, but I think some parts related to data analysis or benchmark tests would still need Python, I need you to verify that before proceeding". The assessment is complete. Migrate the host engine: campaign loading, scalar scoring/metrics, persistence/artifacts, workspace/harness preparation, Docker runtime, gateway, agent adapters, evaluation/run orchestration, CLI, and existing web dashboard server. Preserve observable behavior and existing persisted formats rather than redesigning them as part of the port.
- **S2 — Retained Python:** "Python will remain for the Existing Python benchmark verifiers and task fixtures." Preserve existing Python verifier/task-fixture behavior and generic evaluator argv execution. Retained fixture code may require Python in its selected environment; this includes the trusted offline example checker. No Python engine implementation remains after cutover.
- **S3 — Engine tests:** "No need to retain Python tests temporarily as compatibility reference." Replace the host engine's Python test suite with meaningful Go tests; do not retain a parallel Python engine test suite. Existing Python source may be inspected from the baseline during implementation, but committed Go tests must not read Git history or run Python engine code. Python task verifiers are not host engine tests and remain in scope for retention.
- **S4 — UI scope:** "The TUI will be a separate phase". Preserve the existing CLI and web dashboard behavior. No Bubble Tea/Lip Gloss TUI is introduced in this feature.
- **S5 — Workflow:** "please start with the migration. Use ODD". Use this task document and its project-scoped recovery mirror, reviewable local work-unit commits, observed task checks, and one independent end-of-feature verification for the named risks. Do not publish, push, open PRs, or make paid provider calls without separate authorization. Do not run token-consuming benchmarks.
- **S6 — Considered Go integration:** The original request specifically names "The Gateway / Streaming Reverse Proxy (gateway.py)", "Direct Docker API & Process Management (docker.py)", and "Concurrency & Coordination (runner.py)". Use Go HTTP/concurrency facilities and a supported Docker Engine Go SDK while preserving gateway accounting/security, local-only Docker socket selection, bind/copy transport behavior, process/stream separation, and stop/evaluation lifecycle. The original performance rationale is not a measured speed guarantee.

Acceptance: the Go CLI covers the current `validate`, `run`, `demo`, `list`, `show`, `stop`, `questions`, `answer`, `rerun`, and `dashboard` commands; existing manifest v1/defaults, SQLite schema, JSON/JSONL artifacts, agent protocols, gateway redaction/bounds/accounting, and evaluator isolation retain their behavior. Offline fixtures/mock services prove these contracts without launching real agents/providers. Native Go tests replace existing engine test behaviors, not just compilation checks. No new TUI or scientific-analysis subsystem is added.

## Tasks

- [x] **T1** — S1,S3,S5: establish Go module/shared contracts and port campaign validation, matrix expansion, scoring and metrics with tests; route **inline** (shared foundation must exist before independent writers); status **verified**; commit **9f14c36** (1,053 additions including tracking/module metadata); RED: missing Go APIs; GREEN: `go test ./internal/bench`, `go test ./...`, `go vet ./...`; Risk: items 3,5 (manifest/output contracts and dependency/toolchain setup).
- [x] **T2** — S1,S3,S5: port SQLite persistence, questions/events and artifact serialization with tests; route **delegated**, parallel batch A with T3/T4, disjoint persistence surfaces after T1; status **verified**; commit **85d5a42** (931 authored changes); own-file tests/race pass, parent spot check passes; native assessment unavailable while parallel untracked files exist; Risk: items 1,3,4 (existing persisted formats and concurrent access).
- [x] **T3** — S1,S2,S3,S5: port repository snapshots, attachments and harness validation/configuration with tests; route **delegated**, parallel batch A with T2/T4, disjoint workspace/harness surfaces after T1; status **verified**; commits **39d7730**, **63f48ed** (1,427 authored changes); own-file tests/race and parent spot check pass; Risk: items 2,3 (path/secret guards and routing contracts).
- [x] **T4** — S1,S3,S5,S6: port HTTP/SSE gateway, usage/cost accounting and bounded/redacted provider diagnostics with mock-provider tests; route **delegated**, parallel batch A with T2/T3, disjoint gateway surfaces after T1; status **verified**; commit **e6b0e6d** (1,485 authored changes); own-file tests/race and parent spot check pass; Risk: items 2,3,4 (capability auth, proxy contract and streaming/cancellation).
- [ ] **T5** — S1,S3,S5,S6: implement Go Docker Engine SDK boundary, local socket policy, bind/copy transport and lifecycle with fake-daemon tests; route **delegated**, parallel batch B with T6 over the T1 runtime interface, disjoint runtime surfaces; status **in progress**; commit **pending**; Risk: items 2,4,5 (isolation, process lifecycle and Docker dependency).
- [ ] **T6** — S1,S3,S5,S6: port Pi/OpenCode protocols, child events and dialogs against the shared runtime boundary with tests; route **delegated**, parallel batch B with T5 using the established interface and mocks, disjoint adapter surfaces; status **in progress**; commit **pending**; Risk: items 2,3,4 (protocol/auth boundaries and asynchronous event ordering).
- [ ] **T7** — S1,S2,S3,S5,S6: port evaluation and run orchestration, cancellation/budgets, snapshots and cleanup with tests; route **inline** (pure reply/evaluation subunits use completed T1/T3 and the shared Runtime interface independently of batch B; runner wiring waits for B); status **in progress**; commit **pending**; Risk: items 2,3,4 (evaluation isolation, results and lifecycle ordering).
- [ ] **T8** — S1,S2,S3,S4,S5: wire Go CLI, existing web dashboard and trusted offline demo, with command/output tests; route **inline** after T7; status **pending**; commit **pending**; Risk: items 2,3,4 (control requests, command compatibility and dashboard concurrency).
- [ ] **T9** — S1,S2,S3,S4,S5: switch build/image/docs instructions to Go, remove superseded Python host modules/tests/packaging, and complete offline verification; route **inline** cutover plus **delegated independent verifier** for named risks after self-checks; status **pending**; commit **pending**; Risk: items 1,2,3,4,5 (whole-feature data/security/contract/concurrency/delivery parity).

## Log

### L1 — Original migration request (verbatim)

I need you to rewrite the codebase in Go language, but I think some parts related to data analysis or benchmark tests would still need Python, I need you to verify that before proceeding, also take into this considerations:   ### 1. Where Go is Significantly Better

  #### A. The Gateway / Streaming Reverse Proxy (gateway.py)

  • In Python: You currently have ~400 lines of custom ThreadingHTTPServer and http.client code managing thread pools, socket timeouts, chunked SSE parsing, and token accounting while contending with the
  Python GIL.
  • In Go: net/http and httputil.ReverseProxy provide battle-tested, high-performance streaming reverse proxies out of the box. Inspecting, intercepting, and counting streaming SSE chunks from OpenRouter
  with zero-copy I/O and goroutines is trivial and bulletproof.

  #### B. Direct Docker API & Process Management (docker.py)

  • In Python: The engine shells out to subprocess.run(["docker", ...]) and uses probe files to detect bind mounts.
  • In Go: The official Docker Go SDK (github.com/docker/docker/client) talks directly to the Docker engine via the local Unix socket (/var/run/docker.sock). It gives native access to:
      • Multiplexed container stdout/stderr streams (stdcopy).
      • Direct PTY allocation (pseudo-terminals).
      • Robust container lifecycle management and cancellation via context.Context.

  #### C. Live Terminal Experience & TUI (Your Previous Question)

  • Go has arguably the best modern terminal UI ecosystem available today:
      • Bubble Tea https://github.com/charmbracelet/bubbletea & Lip Gloss https://github.com/charmbracelet/lipgloss (the Charm ecosystem) make it straightforward to build reactive, Elm-architecture terminal
      dashboards that can show multiple agents, subagent trees, token speeds, and live thought streams side-by-side without screen tearing or cursor flicker.

  #### D. Concurrency & Coordination (runner.py)

  • A benchmark engine coordinates multiple parallel runs, each running an agent, a gateway, an SSE reader, dialog question channels, timeouts, and cancellation signals.
  • Go's select, channels, and goroutines make fan-in / fan-out, timeouts, and context cancellation clean and deterministic, avoiding Python's threading.Thread + queue.Queue + Event race conditions.

### L2 — Implementation authorization (verbatim)

Python will remain for the Existing Python benchmark verifiers and task fixtures. No need to retain Python tests temporarily as compatibility reference. The TUI will be a separate phase, please start with the migration. Use ODD

### L3 — Initial evidence and routing

- Baseline source inventory: 2,515 host Python lines and 1,535 Python engine test lines; no third-party runtime Python dependencies or scientific-analysis packages. Existing interfaces are JSON CLI and read-only web dashboard. The host demo currently invokes its trusted Python verifier locally.
- Go toolchain is `go1.25.0 linux/amd64`. Supported SDK module paths will be pinned after checking toolchain compatibility; current Docker docs name `github.com/moby/moby/client` and `github.com/moby/moby/api`.
- Gentle AI routing was verified: explorer/worker `gpt-6-luna` at `xhigh`, verifier `gpt-6.1-sol` at `xhigh`. Batch A and B writer routes depend on fixed shared contracts and exact disjoint edit surfaces; parent owns Git/module/dependency changes.
- RDD is globally off; do not enable it. Functional checks and independent verification for the named high-risk changes still apply. Real Docker/provider benchmarks are excluded; mock/fixture tests provide offline proof, and unavailable real-environment proof must be reported honestly.
- Delivery forecast exceeds the approximately 400-line review threshold. Default `ask-on-risk` requires choosing a chain strategy before the first work-unit commit. Recommended: feature-branch chain, because intermediate language-migration stages should integrate together before landing on `main`. No PR publication is authorized.
- Initial rollback: before cutover, Go work can be removed without modifying existing persisted records; never rewrite live benchmark data. Cutover must preserve SQLite and artifact formats, and the baseline commit remains the source reference rather than a retained Python test suite.

### L4 — Delivery authorization (verbatim)

yes a feature-branch, don't commit directly on main, you are not allowed to push, I will do it manually

- Delivery is a local feature-branch chain: tracker `feat/go-engine-migration`, followed by numbered work-unit child branches. Never commit on `main`, push, or publish a PR. Cohesive ports may exceed 400 lines; do not omit tests, compress code, or artificially fragment behavior to reduce the count. Record actual per-unit line counts and any size exception needed for later manual publication.
- T1 uses one internal Go engine package with separate responsibility files and a small shared runtime interface. This mirrors the existing internal module boundary and avoids package cycles or public API expansion while independent writers own disjoint files.

### L5 — Foundation implementation

- Added `go.mod` and `internal/bench/{types,manifest,scoring,metrics}.go` plus native behavior tests. Observed test RED for missing APIs, then GREEN after implementation; focused tests, full current Go suite and vet pass.
- Shared JSON records preserve field shapes; the decoder retains number tokens so integer-only manifest fields reject `1.0` rather than silently accepting a float. Matrix specs are independent copies; unknown billing remains distinct from known zero; timeline accounting uses the union of active intervals.
- Dependency choice: Docker SDK client `v0.6.3` (Go 1.24 minimum) and pure-Go SQLite `v1.53.0` (Go 1.25 minimum). SQLite latest `v1.62.0` requires Go 1.26, so it was not selected for the installed Go 1.25 toolchain. Pure-Go SQLite avoids a new C compiler/CGO runtime requirement at the cost of a larger dependency graph. Dependency downloads are public module fetches, not benchmark/provider executions.
- Foundation is a cohesive larger-than-400-line slice: manifest validation and shared contracts need their tests alongside them. Keep the readable implementation and tests; record the actual commit size for manual publication rather than code-golfing or inventing standalone partial-validator slices.
- Batch B starts alongside the remaining independent gateway writer after the persistence/workspace workers return. Both new units depend only on the already-committed runtime interface, not on each other or the gateway implementation. At most three workers plus the parent are active; all edit surfaces are disjoint.

### L6 — Persistence unit

- T2 implements compatible WAL SQLite rows, atomic answer updates, run JSON/events/transcripts, stop controls and inventory/diff artifacts. The intervention API returns an answered-question count, matching the existing implementation. Worker observed RED stubs, GREEN focused suite and race suite; parent reran the reported focused command successfully. No source outside the declared four persistence files was changed by this writer.
- T2 native assessment returned `unassessable` because other parallel units are untracked; risk remains high. RDD stays off and no review lifecycle is started. Assessment will be retried for the final tracked feature; independent functional verification remains required.
- T3 ports Git snapshots/attachments and harness guards/configuration. Worker and parent focused checks pass, including race tests. The original credential-helper HTTP challenge regression was omitted under an overly broad interpretation of no-network; local mock HTTP is allowed and this coverage remains pending for the final suite.
- Parent restored the omitted T3 credential-helper challenge as a native local-HTTP regression; the focused suite passes without invoking the helper. Test-only addition has no new production implementation/meaningful RED stage. T3 native assessment remains unavailable because the concurrently edited untracked inventory changed; no lower risk was inferred.
- T4 ports the HTTP/SSE gateway with immutable pending-attempt snapshots, capability/key separation, bounded/redacted provider diagnostics, usage/cost accounting and backpressure. Worker observed RED then focused/race GREEN; parent reran the exact focused command successfully. Getter methods expose cost-limit/infrastructure state for runner classification.
- While batch B implements concrete Docker/adapters, the parent ports independent reply coercion and evaluator logic against the already-existing Runtime interface and completed workspace helpers. These units do not read or depend on the in-progress concrete runtime files; runner integration remains deferred until B returns.

### L7 — Independent reply and evaluator subunits

- Added native reply coercion/recursive redaction and read-only evaluator snapshots, per-test container cleanup, trusted-file mount preflight, unprivileged copy-transport execution, timeout outcomes, and exactly-one-case pytest XML scoring. Existing Python verifiers remain generic subprocess commands, not Go host tests.
- Reply tests observed missing-API RED then focused GREEN. Evaluator tests observed missing-API RED then focused GREEN and race GREEN; fixtures cover pass/skip/fail, multi-case rejection, mount failure, UID selection, artifact scratch placement, and timeout scoring. No real Docker/provider/benchmark calls ran.
- T7 is not complete: run orchestration and its native lifecycle tests remain pending. The independently testable evaluator/reply portion is a 528-line coherent code/test slice and can be committed before the concrete runtime units.
- T4 native assessment also returned `unassessable` for the active untracked inventory; no review was started and the high-risk classification is unchanged. Retry assessment only when the complete feature is tracked.

### L8 — Numeric ordering parity correction

- Found and fixed a migration-caused accounting discrepancy: iterating a Go map randomized the addition order of request costs, whereas Python retains first-insertion order even after an attempt update. A native regression observed RED (`0.59999999999999998` versus the reference-order `0.60000000000000009`) and GREEN after preserving request order. Timeline cumulative-agent summation also retains agent insertion order. This is compatibility work, not a new scoring rule.
