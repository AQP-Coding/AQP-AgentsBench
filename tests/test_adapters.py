import io
import json
import threading
from unittest.mock import Mock
import pytest
from aqp_bench.adapters import PiAdapter, OpenCodeAdapter


class Process:
    def __init__(self, events):
        self.stdin = io.BytesIO()
        self.stdout = io.BytesIO(b''.join(json.dumps(x).encode() + b'\n' for x in events))
        self.terminated = False
    def poll(self):
        return None
    def terminate(self):
        self.terminated = True
    def wait(self, timeout=None):
        return 0
    def kill(self):
        self.terminated = True


def pi_run(events, ask=lambda *args: 'yes'):
    process = Process(events)
    runtime = Mock()
    runtime.spawn.return_value = process
    seen = []
    PiAdapter(runtime, 'cid').run('prompt', 'org/model', seen.append, ask, threading.Event())
    return process, runtime, seen


def test_pi_waits_for_settled_not_agent_end_and_streams_text():
    process, runtime, seen = pi_run([
        {'type':'response','id':'prompt-1','success':True,'data':{'disposition':'started'}},
        {'type':'agent_end'},
        {'type':'message_update','assistantMessageEvent':{'type':'text_delta','delta':'after retry'}},
        {'type':'agent_settled'},
    ])
    assert [e['type'] for e in seen] == ['agent_start','message','agent_end']
    assert seen[1]['text'] == 'after retry'
    assert '--provider' in runtime.spawn.call_args.args[1]
    assert 'aqp' in runtime.spawn.call_args.args[1]


def test_pi_prompt_acceptance_without_completion_is_error():
    with pytest.raises(RuntimeError, match='settled|completion'):
        pi_run([{'type':'response','success':True,'data':{'disposition':'started'}}])


def test_pi_dialog_and_handled_prompt():
    replies = []
    process, _, seen = pi_run([
        {'type':'extension_ui_request','id':'confirm-1','method':'confirm','title':'Allow?'},
        {'type':'agent_settled'},
    ], lambda question, meta: replies.append((question, meta)) or True)
    assert replies[0][0] == 'Allow?'
    assert 'agent_wait' in [x['type'] for x in seen]
    assert 'agent_resume' in [x['type'] for x in seen]
    _, _, seen = pi_run([{'type':'response','success':True,'data':{'disposition':'handled'}}])
    assert seen[-1]['type'] == 'agent_end'


def test_pi_reports_provider_failure_as_agent_error_not_infrastructure():
    with pytest.raises(RuntimeError, match='provider'):
        pi_run([{'type':'message_end','message':{'role':'assistant','stopReason':'error','errorMessage':'provider error'}}, {'type':'agent_settled'}])


def test_opencode_uses_blocking_message_not_async_acceptance(monkeypatch):
    runtime = Mock()
    runtime.spawn.return_value = Process([])
    adapter = OpenCodeAdapter(runtime, 'cid')
    calls = []
    def request(method, path, body=None, **kwargs):
        calls.append((method, path, body))
        if path == '/global/health': return {'healthy':True,'version':'1.2.27'}
        if path == '/session': return {'id':'ses-1'}
        if path == '/session/ses-1/message':
            return {'info':{'role':'assistant','finish':'stop'},'parts':[{'type':'text','text':'done'}]}
        return []
    runtime.address.return_value = '127.0.0.1'
    monkeypatch.setattr(adapter, '_request', request)
    monkeypatch.setattr(adapter, '_events', lambda events, ready, ending: ready.set())
    events = []
    adapter.run('hello','org/model',events.append,lambda *args:None,threading.Event())
    assert ('POST','/session/ses-1/message', {'model':{'providerID':'aqp','modelID':'org/model'},'parts':[{'type':'text','text':'hello'}]}) in calls
    assert [x['type'] for x in events] == ['agent_start','message','agent_end']


def test_pi_prompt_failure_is_actionable():
    with pytest.raises(RuntimeError, match='Pi command rejected'):
        pi_run([{'type':'response','success':False,'error':'wrong model'}])


def test_pi_exit_reports_exit_code_and_captured_stderr():
    import subprocess
    import sys
    program = "import sys; sys.stdin.buffer.readline(); print('startup diagnostic: missing provider config', file=sys.stderr); sys.exit(17)"
    runtime = Mock()
    runtime.spawn.return_value = subprocess.Popen(
        [sys.executable, '-c', program], stdin=subprocess.PIPE,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    with pytest.raises(RuntimeError, match=r'exit code 17.*missing provider config'):
        PiAdapter(runtime, 'cid').run('prompt', 'org/model', lambda _: None,
                                      lambda *args: None, threading.Event())


def test_pi_real_jsonl_transport_dialog_reply_and_unicode():
    import subprocess
    import sys
    program = '''import sys,json
json.loads(sys.stdin.buffer.readline())
print(json.dumps({'type':'extension_ui_request','id':'confirm','method':'confirm','title':'Allow?'}),flush=True)
reply=json.loads(sys.stdin.buffer.readline())
assert reply == {'type':'extension_ui_response','id':'confirm','confirmed':True},reply
print(json.dumps({'type':'agent_end'}),flush=True)
print(json.dumps({'type':'message_update','assistantMessageEvent':{'type':'text_delta','delta':'line\\u2028text'}}),flush=True)
print(json.dumps({'type':'agent_settled'}),flush=True)
sys.stdin.buffer.read()
'''
    process = subprocess.Popen([sys.executable,'-u','-c',program],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
    runtime = Mock()
    runtime.spawn.return_value = process
    seen = []
    PiAdapter(runtime,'cid').run('hello','org/model',seen.append,lambda *args:True,threading.Event())
    assert [x['type'] for x in seen] == ['agent_start','agent_wait','agent_resume','message','agent_end']
    assert seen[-2]['text'] == 'line\u2028text'
    assert process.poll() is not None


def test_pi_stop_remains_observable_while_user_callback_is_blocked():
    import subprocess
    import sys
    program = '''import sys,json
sys.stdin.buffer.readline()
print(json.dumps({'type':'extension_ui_request','id':'input','method':'input','title':'Value?'}),flush=True)
sys.stdin.buffer.read()
'''
    process = subprocess.Popen([sys.executable,'-u','-c',program],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
    runtime = Mock()
    runtime.spawn.return_value = process
    entered, release, stop = threading.Event(), threading.Event(), threading.Event()
    def ask(*args):
        entered.set()
        release.wait(2)
        return 'answer'
    errors = []
    def run():
        try:
            PiAdapter(runtime,'cid').run('hello','org/model',lambda x:None,ask,stop)
        except Exception as error:
            errors.append(error)
    worker = threading.Thread(target=run)
    worker.start()
    assert entered.wait(2)
    stop.set()
    worker.join(3)
    release.set()
    assert not worker.is_alive() and not errors
    runtime.stop.assert_called_once_with('cid')


def test_opencode_http_transport_auth_and_sse():
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
    import base64
    import queue
    class Handler(BaseHTTPRequestHandler):
        def log_message(self,*args): pass
        def do_GET(self):
            expected = 'Basic '+base64.b64encode(b'opencode:test-password').decode()
            assert self.headers['Authorization'] == expected
            self.send_response(200)
            self.end_headers()
            if self.path == '/event':
                self.wfile.write(b'data: {"type":"server.connected","properties":{}}\n\n')
            else:
                self.wfile.write(b'{"healthy":true,"version":"1.2.27"}')
    server = ThreadingHTTPServer(('127.0.0.1',0), Handler)
    serving = threading.Thread(target=server.serve_forever,daemon=True)
    serving.start()
    try:
        adapter = OpenCodeAdapter(Mock(),'cid')
        adapter.password = 'test-password'
        adapter.base_url = f'http://127.0.0.1:{server.server_port}'
        assert adapter._request('GET','/global/health')['healthy'] is True
        events, ready, ending = queue.Queue(), threading.Event(), threading.Event()
        adapter._events(events,ready,ending)
        assert ready.is_set()
        assert events.get_nowait()['type'] == 'server.connected'
        assert isinstance(events.get_nowait(),RuntimeError)
    finally:
        server.shutdown()
        server.server_close()
        serving.join()
