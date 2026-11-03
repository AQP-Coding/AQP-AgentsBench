import json
import subprocess
import threading
import shlex
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from aqp_bench.manifest import expand_runs, load_campaign
from aqp_bench.metrics import Timeline, summarize_attempts
from aqp_bench.scoring import aggregate, score_tests
from aqp_bench.store import Store
from aqp_bench.workspace import prepare


def campaign(tmp_path):
    attachment = tmp_path / "input.csv"
    attachment.write_bytes(b"a,b\r\n1,2\r\n")
    data = {
        "version": 1,
        "tasks": [{"id": "sum", "prompt": "Add numbers", "image": "python:3.13",
                   "attachments": [{"source": "input.csv", "target": "data/input.csv"}],
                   "evaluation": {"tests": [{"id": "answer", "score": 3,
                                               "command": ["python", "-c", "pass"]}]}}],
        "agents": [{"id": "pi", "kind": "pi"}],
        "models": ["vendor/model"],
    }
    path = tmp_path / "campaign.json"
    path.write_text(json.dumps(data))
    return path, data


def test_manifest_matrix_and_original_attachment(tmp_path):
    path, data = campaign(tmp_path)
    data.update(trials=2, models=["vendor/a", "vendor/b"])
    path.write_text(json.dumps(data))
    loaded = load_campaign(path)
    assert len(expand_runs(loaded)) == 4
    workspace = tmp_path / "workspace"
    metadata = prepare(loaded["tasks"][0], workspace)
    assert metadata["commit"] is None
    assert (workspace / "data/input.csv").read_bytes() == b"a,b\r\n1,2\r\n"
    assert metadata["attachments"][0]["sha256"]


@pytest.mark.parametrize("field,value", [("concurrency", 0), ("trials", True),
                                         ("models", []), ("extra", 1)])
def test_manifest_rejects_invalid_without_creating_workspaces(tmp_path, field, value):
    path, data = campaign(tmp_path)
    data[field] = value
    path.write_text(json.dumps(data))
    with pytest.raises(ValueError):
        load_campaign(path)
    assert not (tmp_path / "workspace").exists()


@pytest.mark.parametrize("target", ["../escape", "/absolute", ".git/config", "a/../../b"])
def test_manifest_rejects_unsafe_attachment_targets(tmp_path, target):
    path, data = campaign(tmp_path)
    data["tasks"][0]["attachments"][0]["target"] = target
    path.write_text(json.dumps(data))
    with pytest.raises(ValueError):
        load_campaign(path)


def test_repository_uses_commit_and_warns_without_mutating_source(tmp_path):
    source = tmp_path / "source"
    source.mkdir()
    subprocess.run(["git", "init", "-q", str(source)], check=True)
    (source / "answer.txt").write_text("committed")
    subprocess.run(["git", "-C", str(source), "add", "answer.txt"], check=True)
    subprocess.run(["git", "-C", str(source), "-c", "user.name=Test", "-c",
                    "user.email=test@example.invalid", "commit", "-qm", "initial"], check=True)
    expected = subprocess.check_output(["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()
    (source / "answer.txt").write_text("dirty")
    (source / "untracked.txt").write_text("private")
    result = prepare({"repository": {"source": str(source), "revision": "HEAD"}}, tmp_path / "snapshot")
    assert result["commit"] == expected
    assert result["warnings"]
    assert (tmp_path / "snapshot/answer.txt").read_text() == "committed"
    assert not (tmp_path / "snapshot/untracked.txt").exists()
    assert (source / "answer.txt").read_text() == "dirty"
    assert (source / "untracked.txt").read_text() == "private"


def test_timing_excludes_only_all_waiting_and_counts_children():
    timeline = Timeline(clock=lambda: 0)
    timeline.event({"type": "agent_start", "agent_id": "main"}, now=0)
    timeline.event({"type": "agent_start", "agent_id": "child", "parent_id": "main"}, now=1)
    timeline.event({"type": "agent_wait", "agent_id": "main"}, now=2)
    timeline.event({"type": "agent_wait", "agent_id": "child"}, now=4)
    timeline.event({"type": "agent_resume", "agent_id": "main"}, now=7)
    timeline.event({"type": "agent_end", "agent_id": "child"}, now=8)
    timeline.event({"type": "agent_end", "agent_id": "main"}, now=10)
    result = timeline.snapshot(now=10)
    assert result["agent_seconds"] == 7
    assert result["human_wait_seconds"] == 3
    assert result["cumulative_agent_seconds"] == 8
    assert result["agents"]["child"]["agent_seconds"] == 3


def test_unknown_usage_and_per_attempt_deduplication():
    attempts = [{"attempt_id": "a", "agent_id": "main", "input_tokens": 2,
                 "output_tokens": 3, "cost_usd": 0.1},
                {"attempt_id": "a", "agent_id": "main", "input_tokens": 2,
                 "output_tokens": 3, "cost_usd": 0.1},
                {"attempt_id": "b", "agent_id": "child", "input_tokens": None,
                 "output_tokens": None, "cost_usd": None, "error_kind": "network"}]
    result = summarize_attempts(attempts)
    assert result["request_count"] == 2
    assert result["cost_usd"] is None
    assert result["known_cost_usd"] == 0.1
    assert result["agents"]["main"]["cost_usd"] == 0.1
    assert result["agents"]["child"]["input_tokens"] is None
    assert result["accounting_complete"] is False


def test_weighted_unit_tests_and_infrastructure_exclusion():
    tests = [{"id": "a", "score": 2}, {"id": "b", "score": 8}]
    scored = score_tests(tests, [{"id": "a", "status": "passed"}, {"id": "b", "status": "failed"}])
    assert scored["earned_score"] == 2
    assert scored["max_score"] == 10
    assert scored["score_percent"] == 20
    assert scored["passed_percent"] == 50
    assert aggregate([{"status": "completed", "evaluation": scored},
                      {"status": "infrastructure_failed", "evaluation": {"score_percent": 100}}])["mean_score_percent"] == 20


def test_store_persists_runs_events_and_cli_answers(tmp_path):
    store = Store(tmp_path / "artifacts")
    store.save({"id": "run1", "status": "running"})
    store.event("run1", {"type": "message", "text": "hello"})
    request_id = store.question("run1", "Continue?", {"agent_id": "main"})
    assert store.answer("run1", request_id, "yes") is True
    assert store.answer("run1", request_id, "yes") is False
    assert store.response("run1", request_id) == "yes"
    assert Store(tmp_path / "artifacts").get("run1")["status"] == "running"
    assert store.list()[0]["id"] == "run1"
    assert store.events("run1")[0]["text"] == "hello"
    with pytest.raises(ValueError):
        store.run_dir("../../outside")


def test_git_preparation_never_invokes_ambient_credential_helpers(tmp_path, monkeypatch):
    marker = tmp_path / "helper-invoked"
    home = tmp_path / "home"
    home.mkdir()
    helper = tmp_path / "helper.sh"
    helper.write_text("#!/bin/sh\nprintf invoked > " + shlex.quote(str(marker)) + "\n")
    config = home / ".gitconfig"
    config.write_text('[credential]\n    helper = !/bin/sh ' + str(helper) + '\n')
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("GIT_CONFIG_GLOBAL", str(config))
    monkeypatch.setenv("GIT_TERMINAL_PROMPT", "0")
    monkeypatch.setenv("NO_PROXY", "127.0.0.1")

    class Denied(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            self.send_response(401)
            self.send_header("WWW-Authenticate", 'Basic realm="fixture"')
            self.send_header("Content-Length", "0")
            self.end_headers()

    server = ThreadingHTTPServer(("127.0.0.1", 0), Denied)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        with pytest.raises(ValueError, match="Git input preparation"):
            prepare({"repository": {"source": f"http://127.0.0.1:{server.server_port}/repo.git",
                                     "revision": "a" * 40}}, tmp_path / "clone")
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=5)
    assert not marker.exists(), "Git invoked an unselected credential helper"
