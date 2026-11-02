"""Stage committed Git snapshots and byte-preserving attachments."""

import hashlib
import os
import shutil
import subprocess
import tempfile
from pathlib import Path
from urllib.parse import urlparse

from .manifest import relative_path


def git(*args):
    # A public Git URL is not permission to reuse host credential helpers,
    # URL rewrites, SSH agents, askpass programs, or executable Git hooks.
    environment = {key: value for key, value in os.environ.items()
                   if not key.startswith("GIT_") and key not in
                   ("SSH_AUTH_SOCK", "SSH_ASKPASS", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                    "http_proxy", "https_proxy", "all_proxy")}
    with tempfile.TemporaryDirectory(prefix="aqp-git-") as home:
        environment.update(HOME=home, XDG_CONFIG_HOME=home, GIT_CONFIG_NOSYSTEM="1",
                           GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull,
                           GIT_TERMINAL_PROMPT="0", GIT_ASKPASS="/bin/false",
                           GIT_ALLOW_PROTOCOL="file:http:https", GCM_INTERACTIVE="never")
        command = ["git", "-c", "credential.helper=", "-c", "core.hooksPath=/dev/null",
                   "-c", "core.fsmonitor=false", *map(str, args)]
        try:
            return subprocess.run(command, env=environment, check=True, text=True, capture_output=True,
                                  timeout=120).stdout.strip()
        except subprocess.CalledProcessError as error:
            raise ValueError("Git input preparation failed: " + error.stderr.strip()) from None


def preflight_repository(repository):
    source = repository["source"]
    if urlparse(source).scheme in ("http", "https"):
        return dict(repository), []
    root = Path(source).resolve()
    actual = Path(git("-C", root, "rev-parse", "--show-toplevel")).resolve()
    if actual != root:
        raise ValueError("Repository source must be its Git root")
    revision = repository.get("revision", "HEAD")
    if revision.startswith("-"):
        raise ValueError("Invalid Git revision")
    commit = git("-C", root, "rev-parse", "--verify", revision + "^{commit}")
    dirty = git("-C", root, "status", "--porcelain", "--untracked-files=normal")
    warnings = [f"Uncommitted files in {root.name} are excluded; using commit {commit}."] if dirty else []
    return {"source": str(root), "revision": commit}, warnings


def copy_files(files, destination):
    destination = Path(destination).resolve()
    hashes = []
    for entry in files:
        source = Path(entry["source"])
        target = destination / relative_path(entry["target"])
        if source.is_symlink() or not source.exists():
            raise ValueError("Sources must exist and cannot be symbolic links")
        if not target.resolve().is_relative_to(destination):
            raise ValueError("Target escapes the workspace")
        if source.is_dir() and any(p.is_symlink() for p in source.rglob("*")):
            raise ValueError("Attachment trees cannot contain symbolic links")
        if target.exists() or target.is_symlink():
            raise ValueError(f"Input target already exists: {entry['target']}")
        target.parent.mkdir(parents=True, exist_ok=True)
        if source.is_dir():
            shutil.copytree(source, target)
            for item in sorted(target.rglob("*")):
                if item.is_file():
                    hashes.append({"target": str(item.relative_to(destination)),
                                   "sha256": hashlib.sha256(item.read_bytes()).hexdigest()})
        else:
            shutil.copyfile(source, target)
            hashes.append({"target": entry["target"], "sha256": hashlib.sha256(target.read_bytes()).hexdigest()})
    return hashes


def prepare(task, destination):
    destination = Path(destination)
    result = {"commit": None, "warnings": [], "attachments": []}
    if destination.exists():
        raise ValueError("Workspace destination must not already exist")
    if task.get("repository"):
        repository, result["warnings"] = preflight_repository(task["repository"])
        git("clone", "--quiet", "--no-hardlinks", "--no-checkout", "--", repository["source"], destination)
        git("-C", destination, "checkout", "--quiet", "--detach", repository["revision"])
        result["commit"] = git("-C", destination, "rev-parse", "HEAD")
    else:
        destination.mkdir(parents=True)
    result["attachments"] = copy_files(task.get("attachments", []), destination)
    return result
