import json

from aqp_bench.cli import main


def test_offline_demo_through_public_cli(tmp_path, capsys):
    assert main(["--artifacts", str(tmp_path / "artifacts"), "demo"]) == 0
    result = json.loads(capsys.readouterr().out)
    assert result["simulation"] is True
    assert result["evaluation"]["earned_score"] == 5
    assert result["evaluation"]["passed_percent"] == 100
    assert result["metrics"]["request_count"] == 1
    assert result["metrics"]["cost_usd"] == 0
    assert main(["--artifacts", str(tmp_path / "artifacts"), "show", result["id"]]) == 0
    assert json.loads(capsys.readouterr().out)["simulation"] is True


def test_invalid_manifest_is_reported_without_creating_storage(tmp_path, capsys):
    manifest = tmp_path / "bad.json"
    manifest.write_text("{}")
    assert main(["--artifacts", str(tmp_path / "artifacts"), "validate", str(manifest)]) == 2
    assert not (tmp_path / "artifacts").exists()
    assert "error" in capsys.readouterr().err.lower()


def test_missing_key_does_not_start_live_execution(tmp_path, capsys, monkeypatch):
    monkeypatch.delenv("AQP_MISSING_KEY", raising=False)
    from aqp_bench.demo import manifest_path
    assert main(["--artifacts", str(tmp_path / "artifacts"), "run", str(manifest_path()),
                 "--api-key-env", "AQP_MISSING_KEY"]) == 2
    assert not (tmp_path / "artifacts").exists()
