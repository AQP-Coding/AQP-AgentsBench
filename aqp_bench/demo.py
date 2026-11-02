"""Explicit offline simulation; never evidence of real agent compatibility."""

import csv
import json
import subprocess
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.request import ProxyHandler, Request, build_opener

from .gateway import Gateway
from .manifest import load_campaign
from .runner import run_campaign


def manifest_path():
    return Path(__file__).with_name("example") / "campaign.json"


class _DemoRuntime:
    """Trusted built-in example only, not a sandbox for user workloads."""
    def __init__(self):
        self.containers = {}

    def start(self, image, workspace, **options):
        identity = str(len(self.containers))
        self.containers[identity] = (Path(workspace), options)
        return identity

    def exec(self, container, command, *, timeout=60):
        workspace, options = self.containers[container]
        if len(command) != 3 or command[0] != "python" or command[1] != "/evaluation/check.py":
            raise ValueError("Offline demo executes only its fixed trusted evaluator")
        return subprocess.run([sys.executable, str(options["evaluation"] / "check.py"), command[2]],
                              cwd=workspace, capture_output=True, text=True, timeout=timeout)

    def stop(self, container):
        pass

    def remove(self, container):
        pass


class _DemoAdapter:
    def __init__(self, runtime, container, command=None):
        self.runtime, self.container = runtime, container

    def run(self, prompt, model, on_event, ask, stop):
        workspace, options = self.runtime.containers[self.container]
        on_event({"type": "agent_start", "agent_id": "main"})
        with (workspace / "data/numbers.csv").open() as stream:
            total = sum(int(row["value"]) for row in csv.DictReader(stream))
        config = json.loads((options["home"] / ".pi/agent/models.json").read_text())
        url = config["providers"]["aqp"]["baseUrl"].replace("host.docker.internal", "127.0.0.1")
        request = Request(url + "/chat/completions", method="POST",
                          data=json.dumps({"model": model, "messages": [{"role": "user", "content": str(total)}]}).encode(),
                          headers={"Authorization": "Bearer " + options["environment"]["AQP_GATEWAY_TOKEN"],
                                   "Content-Type": "application/json", "X-AQP-Agent-ID": "main"})
        with build_opener(ProxyHandler({})).open(request, timeout=5) as response:
            answer = json.load(response)["choices"][0]["message"]["content"]
        (workspace / "report.txt").write_text(answer + "\n")
        on_event({"type": "message", "agent_id": "main", "text": "Offline simulation wrote report.txt"})
        on_event({"type": "agent_end", "agent_id": "main"})


def run_demo(store):
    class MockProvider(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            payload = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            data = json.dumps({"id": "demo-generation", "model": "demo/offline",
                               "choices": [{"message": {"content": payload["messages"][-1]["content"]}}],
                               "usage": {"prompt_tokens": 10, "completion_tokens": 2, "cost": 0}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    server = ThreadingHTTPServer(("127.0.0.1", 0), MockProvider)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    try:
        def gateway(key, **kwargs):
            kwargs["host"] = "127.0.0.1"
            return Gateway(key, upstream=f"http://127.0.0.1:{server.server_port}/api/v1", **kwargs)
        result = run_campaign(load_campaign(manifest_path()), store, api_key="offline-demo-not-a-real-key",
                              runtime_factory=_DemoRuntime, gateway_factory=gateway,
                              adapter_factory=_DemoAdapter, notify=lambda _: None)[0]
        result["simulation"] = True
        result["warnings"].append("Offline simulation: no Docker, real agent, harness, or paid provider was exercised.")
        store.save(result)
        return result
    finally:
        server.shutdown()
        server.server_close()
        worker.join(timeout=5)
