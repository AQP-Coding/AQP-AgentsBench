"""Loopback-only, read-only live monitoring; all controls remain in the CLI."""

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


HTML = """<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>AQP AgentsBench</title>
<style>body{font:16px system-ui;max-width:1100px;margin:3rem auto;padding:0 1rem;background:#111827;color:#e5e7eb}
button,select{padding:.5rem}pre{white-space:pre-wrap;overflow-wrap:anywhere;border:1px solid #374151;padding:1rem}
a{color:#93c5fd}#status{color:#a7f3d0}</style>
<h1>AQP AgentsBench</h1><p>Live monitoring · run controls and human replies are CLI-only.</p>
<label for="runs">Run </label><select id="runs"></select><p id="status">Connecting…</p>
<h2>Metrics and evaluation</h2><pre id="details"></pre><h2>Recent events</h2><pre id="events"></pre>
<script src="/app.js" defer></script></html>"""

SCRIPT = """const select=document.querySelector('#runs');
async function refresh(){try{
 const runs=await fetch('/api/runs').then(r=>{if(!r.ok)throw Error(r.status);return r.json()});
 const previous=select.value;select.replaceChildren();
 for(const run of runs){const option=document.createElement('option');option.value=run.id;
 option.textContent=run.id+' · '+run.status;select.append(option)}
 if(runs.some(r=>r.id===previous))select.value=previous;
 if(select.value){const base='/api/runs/'+encodeURIComponent(select.value);
 const detail=await fetch(base).then(r=>r.json());
 document.querySelector('#details').textContent=JSON.stringify(detail,null,2);
 const events=await fetch(base+'/events').then(r=>r.json());
 document.querySelector('#events').textContent=events.map(e=>JSON.stringify(e)).join('\\n');}
 document.querySelector('#status').textContent=runs.length+' runs · updated '+new Date().toLocaleTimeString();
}catch(e){document.querySelector('#status').textContent='Connection unavailable: '+e.message}}
select.addEventListener('change',refresh);refresh();setInterval(refresh,1000);"""


class Dashboard:
    def __init__(self, store, port=8765):
        self.store = store
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def send(self, status, body, content_type="application/json"):
                data = body.encode() if isinstance(body, str) else json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", content_type + "; charset=utf-8")
                self.send_header("Content-Length", str(len(data)))
                self.send_header("Cache-Control", "no-store")
                self.send_header("X-Content-Type-Options", "nosniff")
                self.send_header("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'")
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self):
                if self.headers.get("Host") not in (f"127.0.0.1:{owner.server.server_port}",
                                                     f"localhost:{owner.server.server_port}"):
                    self.send(403, {"error": "Loopback Host required"})
                    return
                path = self.path.split("?", 1)[0]
                if path == "/":
                    self.send(200, HTML, "text/html")
                elif path == "/app.js":
                    self.send(200, SCRIPT, "application/javascript")
                elif path == "/api/runs":
                    self.send(200, owner.store.list())
                elif path.startswith("/api/runs/"):
                    pieces = path[len("/api/runs/"):].split("/")
                    try:
                        run = owner.store.get(pieces[0])
                        if len(pieces) == 1:
                            self.send(200, run)
                        elif len(pieces) == 2 and pieces[1] == "events":
                            self.send(200, owner.store.events(pieces[0]))
                        else:
                            self.send(404, {"error": "Not found"})
                    except KeyError:
                        self.send(404, {"error": "Unknown run"})
                else:
                    self.send(404, {"error": "Not found"})

            def do_POST(self):
                self.send(405, {"error": "Monitoring only; use CLI controls"})

            do_PUT = do_POST
            do_DELETE = do_POST
            do_PATCH = do_POST

        self.server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *args):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
