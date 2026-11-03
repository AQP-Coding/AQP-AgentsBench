"""Evaluate a quiescent workspace in separate, read-only test containers."""

import shutil
import os
import subprocess
import tempfile
import xml.etree.ElementTree as ET
from pathlib import Path

from .scoring import score_tests
from .workspace import copy_files


def evaluate(runtime, workspace, definition, *, scratch_parent=None):
    outcomes = []
    temporary_options = {} if scratch_parent is None else {"dir": scratch_parent}
    with tempfile.TemporaryDirectory(prefix="aqp-evaluate-", **temporary_options) as scratch:
        root = Path(scratch)
        root.chmod(0o755)
        snapshot = root / "workspace"
        # Preserve symlinks as symlinks: never copy host content they point to.
        shutil.copytree(workspace, snapshot, symlinks=True, ignore=shutil.ignore_patterns(".git"))
        trusted = root / "tests"
        trusted.mkdir()
        copy_files(definition.get("files", []), trusted)
        for test in definition["tests"]:
            container = None
            try:
                container = runtime.start(definition["image"], snapshot, evaluation=trusted, readonly_workspace=True)
                transport_for = getattr(runtime, "transport_for", None)
                copied = callable(transport_for) and transport_for(container) == "docker_cp"
                exec_as = getattr(runtime, "exec_as", None)
                evaluator_uid = 65534 if os.getuid() != 65534 else 65533

                def evaluator_exec(command, *, timeout):
                    if copied and callable(exec_as):
                        return exec_as(container, f"{evaluator_uid}:{evaluator_uid}", command, timeout=timeout)
                    return runtime.exec(container, command, timeout=timeout)

                if definition.get("files"):
                    paths = ["/evaluation/" + entry["target"] for entry in definition["files"]]
                    script = 'for path do if [ ! -e "$path" ] || [ ! -r "$path" ]; then printf "missing or unreadable: %s\\n" "$path"; exit 1; fi; done'
                    probe = evaluator_exec(["sh", "-c", script, "aqp-evaluation-preflight", *paths], timeout=10)
                    if probe.returncode:
                        detail = " ".join(part.strip() for part in (probe.stdout, probe.stderr) if part and part.strip())
                        raise RuntimeError(f"Evaluator mount preflight failed (exit code {probe.returncode}): {detail or 'file availability check failed'}")
                command = list(test["command"])
                pytest_command = command[0] in ("pytest", "py.test") or (len(command) > 2 and command[1:3] == ["-m", "pytest"])
                if pytest_command:
                    command = ["env", "PYTEST_DISABLE_PLUGIN_AUTOLOAD=1", *command,
                               "-c", "/dev/null", "--noconftest", "--confcutdir=/evaluation",
                               "-o", "cache_dir=/tmp/aqp-pytest-cache", "--junitxml=/tmp/aqp-unit.xml"]
                result = evaluator_exec(command, timeout=definition.get("timeout_seconds", 60))
                if result.returncode in (125, 126, 127):
                    raise RuntimeError("Evaluation command/runtime is unavailable")
                status = "passed" if result.returncode == 0 else "failed"
                if pytest_command:
                    report = evaluator_exec(["cat", "/tmp/aqp-unit.xml"], timeout=5)
                    if report.returncode:
                        raise RuntimeError("Missing trusted pytest result report")
                    cases = ET.fromstring(report.stdout).findall(".//testcase")
                    if len(cases) != 1:
                        raise RuntimeError("Each evaluation entry must select exactly one pytest unit test")
                    if cases[0].find("skipped") is not None:
                        status = "skipped"
                    elif cases[0].find("failure") is not None or cases[0].find("error") is not None:
                        status = "failed"
                outcomes.append({"id": test["id"], "status": status, "exit_code": result.returncode,
                                 "stdout": result.stdout, "stderr": result.stderr})
            except subprocess.TimeoutExpired:
                outcomes.append({"id": test["id"], "status": "timed_out", "exit_code": None})
            finally:
                if container is not None:
                    try:
                        runtime.stop(container)
                    finally:
                        runtime.remove(container)
    return score_tests(definition["tests"], outcomes)
