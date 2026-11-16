## Linked issue

<!-- Closes #N  |  Fixes #N  |  Refs #N -->

## Type

- [ ] `feat`  - [ ] `fix`  - [ ] `docs`  - [ ] `refactor`  - [ ] `test`  - [ ] `chore`

## Summary

<!-- What does this PR do, and why? -->

## Changes

| File / area | What changed |
|---|---|
| `path/to/file` | Brief description |

## AI assistance

Select exactly one. See [AI_POLICY.md](../AI_POLICY.md).

- [ ] **None** — no material AI assistance was used.
- [ ] **Material assistance used** — complete all fields below.

**Tool/model (if known):**

**Material scope:**

**Verification performed:**

## Test plan

```bash
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```

- [ ] `gofmt`, `go vet`, `go test ./...`, `go test -race ./...` pass
- [ ] Docker integration (`AQP_DOCKER_INTEGRATION=1 ...`) passed, or `N/A` (explain)
- [ ] Live agent/OpenRouter run performed (command, image, model, result), or `N/A`

## Checklist

- [ ] Linked to an issue agreed with maintainers (or trivial doc fix)
- [ ] About 400 changed lines or fewer, or the split is explained
- [ ] Docs updated in the same PR if behavior changed
- [ ] Commits follow Conventional Commits; no AI `Co-Authored-By` trailers
- [ ] No keys, tokens, artifacts, or transcripts committed
- [ ] I understand, reviewed, and take responsibility for the complete submission

## Notes for reviewers

<!-- Optional -->
