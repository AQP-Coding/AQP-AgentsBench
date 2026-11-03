# Benchmark images

Keep each image recipe and its non-Python dependency manifests in `images/<name>/`. Build from the **repository root**, so the recipe can install this project's Python dependencies from `pyproject.toml`. Add future images as sibling directories; do not combine unrelated agent stacks into one image.

| Image directory | Local tag | Contents |
|---|---|---|
| [pi-gentle-python](pi-gentle-python/README.md) | `aqp-agentsbench/pi-gentle-python:0.1.0` | Python 3.11, Node 24, Pi, Gentle Shell and its pinned Gentle-ai |

```bash
docker --host unix:///var/run/docker.sock build --platform linux/amd64 \
  -f images/pi-gentle-python/Dockerfile \
  -t aqp-agentsbench/pi-gentle-python:0.1.0 .
```

Each Dockerfile has its own `Dockerfile.dockerignore` to keep host credentials, agent configuration, Git history, benchmark artifacts and local caches out of the build context. Never pass model keys as build arguments or bake them into layers. Builds download public dependencies but do not make model requests or push images. Use an empty Docker config directory if you do not want to reuse local registry authentication.
