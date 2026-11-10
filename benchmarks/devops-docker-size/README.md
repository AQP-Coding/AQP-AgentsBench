# devops-docker-size

Modification task, simple category: **1 prompt + 1 repository + 1 attachment**. Domain: DevOps / container images.

The agent receives a small Flask service whose Dockerfile produces a 2.24 GB image, plus a size report of that image, and must shrink it by editing the `Dockerfile` and adding a `.dockerignore`.

## Inputs

| Input | Value |
|---|---|
| Prompt | Inline in `campaign.json` |
| Repository | [gtorres777/docker-optimize-demo](https://github.com/gtorres777/docker-optimize-demo) at `32ee5550a26daa5b0c47a2fa4074a1ec82ad3dec` |
| Attachment | `layers-report.txt` → `reports/layers-report.txt` |

The attachment is required to solve the task well: the prompt does not name what to exclude from the build context. The report shows that `COPY . .` adds 394 MB, almost all of it `data/` (376 MB of generated CSVs that are git-ignored, so they are absent from the agent's clone), and that the base `python:3.11` image accounts for ~1.2 GB.

The report was produced with Docker 29.9.2 on linux/x86_64 after running `python3 scripts/generate_data.py` in the repository, using `docker image ls`, `docker history`, and `du` inside the built image.

## Intentional problems in the original Dockerfile

1. Full `python:3.11` base instead of `python:3.11-slim`.
2. `build-essential curl git vim` installed in the final image.
3. `apt-get update` in its own layer; apt lists never removed.
4. `COPY . .` without `.dockerignore`: copies `data/`, `.git`, `tests/`.
5. Development dependencies (`requirements-dev.txt`, pytest) installed.
6. `pip install` without `--no-cache-dir`.

## Evaluation (pending)

The hidden evaluation is not written yet; another contributor owns it. `campaign.json` currently has a single dummy test (`dummy-workspace-visible`, 1 point): `check.py` only confirms that the evaluator sees the final workspace and the staged attachment, so the manifest validates and the evaluation pipeline can be exercised end to end. **Do not run this campaign for results until real tests replace it**: every run would score 100%.

Constraints for whoever writes the tests:

- Neither the agent nor the evaluator container has Docker, so checks cannot build the image or measure its size. They can inspect the final `Dockerfile` and `.dockerignore` in the workspace (the evaluator runs from `/workspace`, without `.git`).
- The evaluator image `aqp-agentsbench/pi-gentle-python:0.1.0` has Python 3.11 without PyYAML.
- Multi-stage builds should not be required: with a slim base and no compiled dependencies, a single stage is a valid optimal answer (verified: 212 MB single-stage vs 242 MB multi-stage, down from 2.24 GB).
