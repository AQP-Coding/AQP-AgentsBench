import json
from urllib.error import HTTPError
from urllib.request import Request, urlopen

import pytest

from aqp_bench.dashboard import Dashboard
from aqp_bench.store import Store


def test_live_read_only_api_and_escaped_display(tmp_path):
    store = Store(tmp_path)
    store.save({"id": "run", "status": "running", "task": "<script>bad()</script>"})
    with Dashboard(store, port=0) as dashboard:
        assert json.load(urlopen(dashboard.url + "/api/runs"))[0]["status"] == "running"
        store.save({"id": "run", "status": "completed"})
        assert json.load(urlopen(dashboard.url + "/api/runs/run"))["status"] == "completed"
        html = urlopen(dashboard.url).read().decode()
        assert "<script>bad()" not in html
        script = urlopen(dashboard.url + "/app.js").read().decode()
        assert "textContent" in script and "innerHTML" not in script
        with pytest.raises(HTTPError) as err:
            urlopen(Request(dashboard.url + "/api/runs/run", data=b"{}", method="POST"))
        assert err.value.code == 405
        with pytest.raises(HTTPError) as err:
            urlopen(Request(dashboard.url, headers={"Host": "attacker.invalid"}))
        assert err.value.code == 403


def test_dashboard_unknown_runs_and_paths(tmp_path):
    with Dashboard(Store(tmp_path), port=0) as dashboard:
        for path in ("/api/runs/missing", "/../../etc/passwd", "/api/runs/a/b"):
            with pytest.raises(HTTPError) as err:
                urlopen(dashboard.url + path)
            assert err.value.code == 404
