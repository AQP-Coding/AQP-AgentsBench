import json
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
IMAGE = ROOT / "images/pi-gentle-python"


def test_python_agent_image_uses_declared_dependencies_and_sample_command():
    dockerfile = (IMAGE / "Dockerfile").read_text()
    packages = json.loads((IMAGE / "package.json").read_text())
    assert "FROM python:3.11-slim-bookworm" in dockerfile
    assert "FROM node:24-bookworm-slim" in dockerfile
    assert "npm ci" in dockerfile
    assert "pip install --no-cache-dir -e '.[dev]'" in dockerfile
    assert 'CMD ["pytest", "-p", "no:tldr", "-v"]' in dockerfile
    assert "WORKDIR /app" in dockerfile
    assert "COPY . ." in dockerfile
    assert "USER agent" in dockerfile
    assert packages["dependencies"] == {"@earendil-works/pi-coding-agent": "1.5.0", "gentle-pi": "4.5.1"}
    assert "git" in (IMAGE / "apt-packages.txt").read_text().splitlines()
    assert "GENTLE_SHELL_NO_AUTO_SETUP=1" in dockerfile


def test_build_context_excludes_host_state_and_secrets():
    patterns = (IMAGE / "Dockerfile.dockerignore").read_text().splitlines()
    for pattern in (".git", ".aqp-bench", ".atl", "INIT.md", "**/.env", "**/.aws", "**/node_modules"):
        assert pattern in patterns


def test_image_npm_lock_matches_explicit_tool_versions():
    packages = json.loads((IMAGE / "package.json").read_text())
    lock = json.loads((IMAGE / "package-lock.json").read_text())
    assert lock["packages"][""]["dependencies"] == packages["dependencies"]
    for name, version in packages["dependencies"].items():
        entry = lock["packages"]["node_modules/" + name]
        assert entry["version"] == version
        assert entry["integrity"].startswith("sha512-")


def test_documented_harness_command_preserves_benchmark_gateway_home():
    readme = (IMAGE / "README.md").read_text()
    example = json.loads(readme.split("```json\n", 1)[1].split("\n```", 1)[0])
    assert example["agents"][0]["command"] == ["gentle-shell", "--link"]
