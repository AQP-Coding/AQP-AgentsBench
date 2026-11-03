import subprocess
import os
from pathlib import Path

import pytest

from aqp_bench.evaluation import evaluate


class Runtime:
    def __init__(self, xml):
        self.xml = xml
        self.starts = []
        self.workspaces = []
        self.commands = []
        self.missing_evaluator = False

    def start(self, image, workspace, **options):
        self.starts.append(options)
        self.workspaces.append(Path(workspace))
        return "evaluator"

    def exec(self, identity, command, **options):
        self.commands.append(command)
        if command[0] == "sh" and self.missing_evaluator:
            return subprocess.CompletedProcess(command, 1, "", "missing or unreadable: /evaluation/check.py\n")
        return subprocess.CompletedProcess(command, 0, self.xml if command[0] == "cat" else "", "")

    def stop(self, identity):
        pass

    def remove(self, identity):
        pass


def definition():
    return {"image": "fixture", "files": [], "tests": [
        {"id": "unit", "score": 5, "command": ["pytest", "/evaluation/test_unit.py::test_answer"]}]}


@pytest.mark.parametrize("xml,score,status", [
    ('<testsuites><testsuite><testcase name="a"/></testsuite></testsuites>', 5, "passed"),
    ('<testsuites><testsuite><testcase name="a"><skipped/></testcase></testsuite></testsuites>', 0, "skipped"),
    ('<testsuites><testsuite><testcase name="a"><failure/></testcase></testsuite></testsuites>', 0, "failed"),
])
def test_pytest_reports_one_real_unit_and_never_awards_skipped(tmp_path, xml, score, status):
    runtime = Runtime(xml)
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    result = evaluate(runtime, workspace, definition())
    assert result["earned_score"] == score
    assert result["tests"][0]["status"] == status
    assert runtime.starts[0]["readonly_workspace"] is True


def test_pytest_suite_cannot_be_scored_as_a_single_unit(tmp_path):
    runtime = Runtime('<testsuites><testsuite><testcase/><testcase/></testsuite></testsuites>')
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    with pytest.raises(RuntimeError, match="exactly one"):
        evaluate(runtime, workspace, definition())


def test_missing_evaluator_mount_is_reported_before_scoring(tmp_path):
    runtime = Runtime('<testsuites><testsuite><testcase name="a"/></testsuite></testsuites>')
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    evaluator = tmp_path / "check.py"
    evaluator.write_text("pass\n")
    spec = definition()
    spec["files"] = [{"source": str(evaluator), "target": "check.py"}]
    runtime.missing_evaluator = True
    with pytest.raises(RuntimeError, match="Evaluator mount preflight failed.*missing or unreadable"):
        evaluate(runtime, workspace, spec)


def test_evaluator_mount_is_checked_before_running_tests(tmp_path):
    runtime = Runtime('<testsuites><testsuite><testcase name="a"/></testsuite></testsuites>')
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    evaluator = tmp_path / "check.py"
    evaluator.write_text("pass\n")
    spec = definition()
    spec["files"] = [{"source": str(evaluator), "target": "check.py"}]
    evaluate(runtime, workspace, spec)
    assert runtime.commands[0][0:2] == ["sh", "-c"]
    assert "/evaluation/check.py" in runtime.commands[0]


def test_evaluator_snapshot_can_live_under_artifact_directory(tmp_path):
    runtime = Runtime('<testsuites><testsuite><testcase name="a"/></testsuite></testsuites>')
    workspace = tmp_path / "workspace"
    scratch_parent = tmp_path / "run-artifacts"
    workspace.mkdir()
    scratch_parent.mkdir()
    evaluate(runtime, workspace, definition(), scratch_parent=scratch_parent)
    assert runtime.workspaces[0].parent.parent == scratch_parent


def test_copy_transport_runs_all_evaluator_commands_as_unprivileged_uid(tmp_path):
    class CopyRuntime(Runtime):
        def __init__(self, xml):
            super().__init__(xml)
            self.users = []

        def transport_for(self, identity):
            return "docker_cp"

        def exec_as(self, identity, user, command, **options):
            self.users.append(user)
            return self.exec(identity, command, **options)

    runtime = CopyRuntime('<testsuites><testsuite><testcase name="a"/></testsuite></testsuites>')
    workspace = tmp_path / "workspace"
    workspace.mkdir()
    evaluator = tmp_path / "check.py"
    evaluator.write_text("pass\n")
    spec = definition()
    spec["files"] = [{"source": str(evaluator), "target": "check.py"}]

    result = evaluate(runtime, workspace, spec)

    assert result["earned_score"] == 5
    evaluator_uid = 65534 if os.getuid() != 65534 else 65533
    assert runtime.users == [f"{evaluator_uid}:{evaluator_uid}"] * 3
