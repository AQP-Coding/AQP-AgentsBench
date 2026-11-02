"""Protocol adapters. HTTP/RPC acceptance is never treated as completion."""
from __future__ import annotations

import base64
import json
import queue
import secrets
import subprocess
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.parse import quote
from urllib.request import Request, build_opener, ProxyHandler, HTTPRedirectHandler


def _emit(callback, kind, agent_id='main', parent_id=None, **fields):
    callback({'type':kind, 'agent_id':agent_id, 'parent_id':parent_id, **fields})


def _finish_process(process):
    # docker exec termination alone does not stop the container; caller must stop
    # the container before evaluating its workspace.
    if process.stdin and not process.stdin.closed:
        process.stdin.close()
    if process.poll() is None:
        process.terminate()
    try:
        process.wait(timeout=2)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=2)


class _Dialogs:
    """Human callbacks never block a protocol reader or stop observation."""
    def __init__(self, ask, on_event):
        self.ask, self.on_event = ask, on_event
        self.errors = queue.Queue()
        self.pending = set()
        self.lock = threading.Lock()

    def submit(self, request, respond, *, agent_id='main', parent_id=None):
        key = request['id']
        with self.lock:
            if key in self.pending:
                return
            self.pending.add(key)
        _emit(self.on_event, 'agent_wait', agent_id, parent_id)
        def answer():
            try:
                question = request.get('title') or request.get('message') or request.get('permission') or 'Agent requests input'
                reply = self.ask(question, dict(request, agent_id=agent_id, parent_id=parent_id))
                respond(reply)
                _emit(self.on_event, 'agent_resume', agent_id, parent_id)
            except Exception as error:
                self.errors.put(error)
        threading.Thread(target=answer, daemon=True).start()

    def check(self):
        try:
            error = self.errors.get_nowait()
        except queue.Empty:
            return
        raise RuntimeError('Human reply could not be delivered') from error


class PiAdapter:
    def __init__(self, runtime, container, command=None):
        self.runtime, self.container = runtime, container
        self.command = list(command or ['pi'])

    def run(self, prompt, model, on_event, ask, stop):
        process = self.runtime.spawn(self.container, [*self.command, '--mode', 'rpc', '--no-session',
                                                     '--provider', 'aqp', '--model', model])
        records = queue.Queue()
        write_lock = threading.Lock()
        def send(record):
            with write_lock:
                process.stdin.write(json.dumps(record).encode('utf-8') + b'\n')
                process.stdin.flush()
        def read():
            try:
                while line := process.stdout.readline():
                    record = json.loads(line)
                    if not isinstance(record, dict):
                        raise ValueError('Pi RPC records must be objects')
                    records.put(record)
            except Exception as error:
                records.put(error)
            finally:
                records.put(None)
        reader = threading.Thread(target=read, daemon=True)
        reader.start()
        dialogs = _Dialogs(ask, on_event)
        _emit(on_event, 'agent_start')
        failure = None
        try:
            send({'id':'prompt-1','type':'prompt','message':prompt})
            while not stop.is_set():
                dialogs.check()
                try:
                    record = records.get(timeout=.05)
                except queue.Empty:
                    continue
                if record is None:
                    raise RuntimeError('Pi exited before agent_settled completion; check RPC capability/version')
                if isinstance(record, Exception):
                    raise RuntimeError('Invalid Pi RPC output') from record
                kind = record.get('type')
                if kind == 'response':
                    if record.get('success') is False:
                        raise RuntimeError(f"Pi command rejected: {record.get('error', 'unknown error')}")
                    if record.get('data', {}).get('disposition') == 'handled':
                        break
                elif kind == 'message_update':
                    update = record.get('assistantMessageEvent', {})
                    if update.get('type') == 'text_delta':
                        _emit(on_event, 'message', text=update.get('delta', ''))
                elif kind == 'message_end':
                    message = record.get('message', {})
                    if message.get('stopReason') in ('error', 'aborted'):
                        failure = message.get('errorMessage') or 'Pi model work failed or was aborted'
                elif kind == 'extension_ui_request':
                    method = record.get('method')
                    if method in ('select', 'confirm', 'input', 'editor'):
                        def respond(reply, request=record):
                            result = {'type':'extension_ui_response', 'id':request['id']}
                            if reply is None:
                                result['cancelled'] = True
                            elif request['method'] == 'confirm':
                                if not isinstance(reply, bool):
                                    raise ValueError('Pi confirmation requires a boolean reply')
                                result['confirmed'] = reply
                            else:
                                result['value'] = reply
                            send(result)
                        dialogs.submit(record, respond)
                    elif method == 'notify':
                        _emit(on_event, 'message', text=record.get('message', ''))
                elif kind == 'agent_settled':
                    if failure:
                        raise RuntimeError(failure)
                    break
                # agent_end is deliberately not terminal: Pi may retry/compact.
            if stop.is_set():
                self.runtime.stop(self.container)
        finally:
            _finish_process(process)
            reader.join(timeout=2)
            _emit(on_event, 'agent_end')


class _NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise RuntimeError('OpenCode API redirects are not allowed')


class OpenCodeAdapter:
    """OpenCode v1 HTTP server contract with a blocking message and SSE reader."""
    def __init__(self, runtime, container, command=None):
        self.runtime, self.container = runtime, container
        self.command = list(command or ['opencode'])
        self.password = secrets.token_urlsafe(24)
        self.base_url = None
        # Never send the ephemeral server password through an ambient proxy.
        self.opener = build_opener(ProxyHandler({}), _NoRedirect())

    def _open(self, method, path, body=None, *, timeout=5):
        authorization = base64.b64encode(('opencode:' + self.password).encode()).decode()
        request = Request(self.base_url + path, data=None if body is None else json.dumps(body).encode(),
                          method=method, headers={'Content-Type':'application/json', 'Authorization':'Basic ' + authorization})
        return self.opener.open(request, timeout=timeout)

    def _request(self, method, path, body=None, *, timeout=5):
        with self._open(method, path, body, timeout=timeout) as response:
            data = response.read()
            return json.loads(data) if data else None

    def _events(self, events, ready, ending):
        try:
            # HTTP response headers arrive before prompt dispatch; events cannot
            # be missed even when a fixture/agent completes immediately.
            with self._open('GET', '/event', timeout=30) as response:
                ready.set()
                data = []
                for line in response:
                    if ending.is_set():
                        return
                    line = line.decode('utf-8').rstrip('\r\n')
                    if line.startswith('data:'):
                        data.append(line[5:].lstrip())
                    elif not line and data:
                        events.put(json.loads('\n'.join(data)))
                        data = []
            if not ending.is_set():
                events.put(RuntimeError('OpenCode event stream closed before completion'))
        except Exception as error:
            if not ending.is_set():
                events.put(error)
            ready.set()

    def run(self, prompt, model, on_event, ask, stop):
        process = self.runtime.spawn(self.container,
            ['env', 'OPENCODE_SERVER_PASSWORD=' + self.password, *self.command,
             'serve', '--hostname', '0.0.0.0', '--port', '4096'])
        self.base_url = 'http://' + self.runtime.address(self.container) + ':4096'
        ending = threading.Event()
        events, result = queue.Queue(), queue.Queue()
        dialogs = _Dialogs(ask, on_event)
        started = False
        session_id = None
        reader = None
        children = {}
        try:
            deadline = time.monotonic() + 15
            while not stop.is_set():
                try:
                    health = self._request('GET', '/global/health')
                    if health.get('healthy') is not True:
                        raise RuntimeError('OpenCode health check failed')
                    version = health.get('version', '')
                    if version and not (version.startswith('1.2.')):
                        raise RuntimeError('Unsupported OpenCode API version; adapter targets v1')
                    break
                except (URLError, OSError):
                    if process.poll() is not None or time.monotonic() >= deadline:
                        raise RuntimeError('OpenCode server did not become ready; check image and API version') from None
                    stop.wait(.05)
            if stop.is_set():
                return
            session = self._request('POST', '/session', {})
            session_id = session['id']
            ready = threading.Event()
            reader = threading.Thread(target=self._events, args=(events, ready, ending), daemon=True)
            reader.start()
            if not ready.wait(10):
                raise RuntimeError('OpenCode event subscription did not become ready')
            if not events.empty():
                first = events.get()
                if isinstance(first, Exception):
                    raise RuntimeError('OpenCode event subscription failed') from first
                events.put(first)
            _emit(on_event, 'agent_start')
            started = True
            def message():
                try:
                    result.put(self._request('POST', f'/session/{quote(session_id, safe="")}/message',
                        {'model':{'providerID':'aqp', 'modelID':model}, 'parts':[{'type':'text','text':prompt}]}, timeout=86400))
                except Exception as error:
                    result.put(error)
            threading.Thread(target=message, daemon=True).start()
            while not stop.is_set():
                dialogs.check()
                try:
                    record = events.get(timeout=.05)
                except queue.Empty:
                    record = None
                if isinstance(record, Exception):
                    raise RuntimeError('OpenCode event protocol failed') from record
                if record:
                    kind, props = record.get('type'), record.get('properties', {})
                    if kind == 'session.created':
                        child = props.get('info', {})
                        parent = child.get('parentID')
                        if parent == session_id or parent in children:
                            children[child['id']] = 'main' if parent == session_id else parent
                            _emit(on_event, 'agent_start', child['id'], children[child['id']])
                    sid = props.get('sessionID')
                    if sid == session_id or sid in children:
                        aid, parent = ('main', None) if sid == session_id else (sid, children[sid])
                        if kind in ('permission.asked', 'question.asked'):
                            request = dict(props)
                            if kind == 'question.asked':
                                request['title'] = '\n'.join(x['question'] for x in props['questions'])
                            def respond(reply, data=props, event_kind=kind):
                                rid = quote(data['id'], safe='')
                                if event_kind == 'permission.asked':
                                    if reply not in ('once', 'always', 'reject'):
                                        raise ValueError('OpenCode permission reply must be once, always or reject')
                                    self._request('POST', f'/permission/{rid}/reply', {'reply':reply})
                                elif reply is None:
                                    self._request('POST', f'/question/{rid}/reject', {})
                                else:
                                    if not isinstance(reply, list) or len(reply) != len(data['questions']) or any(not isinstance(x, list) or any(not isinstance(y, str) for y in x) for x in reply):
                                        raise ValueError('OpenCode question reply requires one string-list per question')
                                    self._request('POST', f'/question/{rid}/reply', {'answers':reply})
                            dialogs.submit(request, respond, agent_id=aid, parent_id=parent)
                        elif kind == 'session.status' and sid in children:
                            status = props.get('status', {}).get('type')
                            if status == 'idle':
                                _emit(on_event, 'agent_end', aid, parent)
                if not events.empty():
                    continue
                try:
                    response = result.get_nowait()
                except queue.Empty:
                    continue
                if isinstance(response, Exception):
                    raise RuntimeError('OpenCode message request failed') from response
                if not isinstance(response, dict) or 'info' not in response or 'parts' not in response:
                    raise RuntimeError('OpenCode returned no completed message; check v1 HTTP API capability')
                if response['info'].get('error'):
                    raise RuntimeError('OpenCode agent returned an error: ' + json.dumps(response['info']['error']))
                for part in response['parts']:
                    if part.get('type') == 'text':
                        _emit(on_event, 'message', text=part.get('text', ''))
                break
        finally:
            ending.set()
            if stop.is_set():
                self.runtime.stop(self.container)
            _finish_process(process)
            if reader:
                reader.join(timeout=1)
            if started:
                _emit(on_event, 'agent_end')
