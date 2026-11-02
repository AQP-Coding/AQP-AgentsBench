import subprocess

import pytest

from aqp_bench.evaluation import evaluate


class Runtime:
    def __init__(self, xml):
        self.xml = xml
        self.starts = []

    def start(self, image, workspace, **options):
        self.starts.append(options)
        return "evaluator"

    def exec(self, identity, command, **options):
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
