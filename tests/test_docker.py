import subprocess
from unittest.mock import patch
import pytest
from aqp_bench.docker import DockerRuntime


def test_start_isolated_and_does_not_pull(tmp_path):
    workspace = tmp_path / 'work'
    home = tmp_path / 'home'
    workspace.mkdir()
    home.mkdir()
    with patch('aqp_bench.docker.subprocess.run', return_value=subprocess.CompletedProcess([], 0, 'container-id\n', '')) as run:
        assert DockerRuntime().start('image:test', workspace, home=home, environment={'AQP_GATEWAY_TOKEN':'cap'}) == 'container-id'
    args = run.call_args.args[0]
    assert '--pull=never' in args
    assert '--privileged' not in args and '-p' not in args and '--network=host' not in args
    assert not any('docker.sock' in x for x in args if x.startswith('type=bind'))
    assert args[1:3] == ['--host', 'unix:///var/run/docker.sock'] or args[2].startswith('unix:///run/user/')
    assert 'host.docker.internal:host-gateway' in args
    assert 'HOME=/home/agent' in args
    assert args[-2:] == ['-c', 'exec sleep infinity']
    assert args[args.index('--entrypoint') + 1] == 'sh'


def test_exec_and_spawn_are_argv_and_stderr_cannot_block():
    runtime = DockerRuntime()
    with patch('aqp_bench.docker.subprocess.run', return_value=subprocess.CompletedProcess([], 0, 'ok', '')) as run:
        runtime.exec('cid', ['printf', 'a; echo unsafe'])
    assert run.call_args.args[0] == runtime._argv('exec', 'cid', 'printf', 'a; echo unsafe')
    with patch('aqp_bench.docker.subprocess.Popen') as popen:
        runtime.spawn('cid', ['pi', '--mode', 'rpc'])
    assert popen.call_args.kwargs['stderr'] == subprocess.DEVNULL
    assert popen.call_args.args[0] == runtime._argv('exec', '-i', 'cid', 'pi', '--mode', 'rpc')


@pytest.mark.parametrize('operation', ['stop', 'remove'])
def test_cleanup_only_tolerates_missing_containers(operation):
    runtime = DockerRuntime()
    with patch('aqp_bench.docker.subprocess.run', return_value=subprocess.CompletedProcess([], 1, '', 'Error: No such container: cid')):
        getattr(runtime, operation)('cid')
    with patch('aqp_bench.docker.subprocess.run', return_value=subprocess.CompletedProcess([], 1, '', 'Cannot connect to Docker daemon')):
        with pytest.raises(RuntimeError, match='Docker'):
            getattr(runtime, operation)('cid')


def test_runtime_rejects_mounting_host_home_or_docker_socket(tmp_path):
    from pathlib import Path
    with pytest.raises(ValueError):
        DockerRuntime().start('x', tmp_path, home=Path.home())
    with pytest.raises(ValueError):
        DockerRuntime().start('x', Path('/'))


def test_evaluation_mount_is_readonly_and_container_uses_host_identity(tmp_path):
    import os
    evaluation = tmp_path / 'tests'
    workspace = tmp_path / 'snapshot'
    evaluation.mkdir()
    workspace.mkdir()
    with patch('aqp_bench.docker.subprocess.run', return_value=subprocess.CompletedProcess([],0,'cid','')) as run:
        DockerRuntime().start('image',workspace,evaluation=evaluation,readonly_workspace=True)
    args = run.call_args.args[0]
    assert f'type=bind,src={evaluation},dst=/evaluation,readonly' in args
    assert f'type=bind,src={workspace},dst=/workspace,readonly' in args
    assert args[args.index('--user')+1] == f'{os.getuid()}:{os.getgid()}'


def test_remote_docker_transport_is_rejected_without_probing(monkeypatch):
    monkeypatch.setenv('DOCKER_HOST', 'ssh://production.invalid')
    with patch('aqp_bench.docker.subprocess.run') as run:
        with pytest.raises(ValueError, match='local'):
            DockerRuntime()
        run.assert_not_called()
