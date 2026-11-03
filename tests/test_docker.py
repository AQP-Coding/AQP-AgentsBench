import subprocess
import os
from pathlib import Path
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
    args = run.call_args_list[0].args[0]
    assert '--pull=never' in args
    assert '--privileged' not in args and '-p' not in args and '--network=host' not in args
    assert not any('docker.sock' in x for x in args if x.startswith('type=bind'))
    assert args[1:3] == ['--host', 'unix:///var/run/docker.sock'] or args[2].startswith('unix:///run/user/')
    assert 'host.docker.internal:host-gateway' in args
    assert 'HOME=/home/agent' in args
    assert args[-2:] == ['-c', 'exec sleep infinity']
    assert args[args.index('--entrypoint') + 1] == 'sh'


def test_start_stages_home_and_keeps_evaluator_on_readonly_bind_mount(tmp_path):
    workspace, home, evaluation = (tmp_path / name for name in ('work', 'home', 'evaluation'))
    for directory in (workspace, home, evaluation):
        directory.mkdir()
    calls = []
    def fake_run(command, **kwargs):
        calls.append(command)
        if 'aqp-bind-probe' in command:
            source = workspace if command[-2].startswith('/workspace/') else evaluation
            marker_path = source / Path(command[-2]).name
            assert marker_path.read_text() == command[-1] + '\n'
        if command[3] == 'run':
            return subprocess.CompletedProcess(command, 0, 'container-id\n', '')
        return subprocess.CompletedProcess(command, 0, '', '')
    with patch('aqp_bench.docker.subprocess.run', side_effect=fake_run):
        DockerRuntime().start('image:test', workspace, home=home, evaluation=evaluation)
    launch = calls[0]
    assert any(arg.startswith('/home/agent:uid=') for arg in launch)
    assert f'type=bind,src={evaluation},dst=/evaluation,readonly' in launch
    assert not any(str(home) in arg for arg in launch)
    assert '/workspace/' in calls[1][-2]
    assert '/evaluation/' in calls[2][-2]
    assert calls[3][-2] == f'{home}/.' and calls[3][-1].startswith('container-id:/tmp/aqp-stage-')
    assert any('cp -a "$1"/. "$2"/' in arg for arg in calls[4])
    assert calls[4][-1] == '/home/agent'


def test_exec_and_spawn_are_argv_and_stderr_is_available_for_diagnostics():
    runtime = DockerRuntime()
    with patch('aqp_bench.docker.subprocess.run', return_value=subprocess.CompletedProcess([], 0, 'ok', '')) as run:
        runtime.exec('cid', ['printf', 'a; echo unsafe'])
    assert run.call_args.args[0] == runtime._argv('exec', '--workdir', '/workspace', 'cid', 'printf', 'a; echo unsafe')
    with patch('aqp_bench.docker.subprocess.Popen') as popen:
        runtime.spawn('cid', ['pi', '--mode', 'rpc'])
    assert popen.call_args.kwargs['stderr'] == subprocess.DEVNULL
    with patch('aqp_bench.docker.subprocess.Popen') as popen:
        runtime.spawn('cid', ['pi', '--mode', 'rpc'], capture_stderr=True)
    assert popen.call_args.kwargs['stderr'] == subprocess.PIPE
    assert popen.call_args.args[0] == runtime._argv('exec', '-i', '--workdir', '/workspace', 'cid', 'pi', '--mode', 'rpc')


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
    args = run.call_args_list[0].args[0]
    assert f'type=bind,src={evaluation},dst=/evaluation,readonly' in args
    assert f'type=bind,src={workspace},dst=/workspace,readonly' in args
    assert args[args.index('--user')+1] == f'{os.getuid()}:{os.getgid()}'


def test_unreadable_bind_falls_back_to_copy_and_keeps_evaluator_readonly(tmp_path):
    workspace, evaluation = (tmp_path / name for name in ('workspace', 'evaluation'))
    workspace.mkdir()
    evaluation.mkdir()
    (workspace / 'input.txt').write_text('agent input')
    (evaluation / 'check.py').write_text('trusted check')
    evaluation.chmod(0o700)
    (evaluation / 'check.py').chmod(0o600)
    calls = []
    readonly_staging_modes = []

    def fake_run(command, **kwargs):
        calls.append(command)
        if command[3] == 'run':
            return subprocess.CompletedProcess(command, 0,
                                               'bind-container\n' if len([c for c in calls if c[3] == 'run']) == 1
                                               else 'copy-container\n', '')
        if command[3] == 'exec' and 'aqp-bind-probe' in command:
            return subprocess.CompletedProcess(command, 1, '', 'marker missing')
        if command[3] == 'cp' and command[-1] == 'copy-container:/':
            staged = Path(command[-2])
            readonly_staging_modes.append((staged.name,
                                           staged.stat().st_mode & 0o777,
                                           [p.stat().st_mode & 0o777 for p in staged.iterdir()]))
        return subprocess.CompletedProcess(command, 0, '', '')

    runtime = DockerRuntime()
    with patch('aqp_bench.docker.subprocess.run', side_effect=fake_run):
        identity = runtime.start('image', workspace, evaluation=evaluation, readonly_workspace=True)

    launches = [call for call in calls if call[3] == 'run']
    assert identity == 'copy-container'
    assert any(arg.startswith('type=bind,') for arg in launches[0])
    assert not any(arg.startswith('type=bind,') for arg in launches[1])
    # Copy mode starts as the same unprivileged UID as agent commands so Docker
    # assigns copied workspace ownership correctly.
    assert launches[1][launches[1].index('--user') + 1] == f'{os.getuid()}:{os.getgid()}'
    copies = [call for call in calls if call[3] == 'cp']
    assert len(copies) == 2
    assert any(call[-1] == 'copy-container:/' and Path(call[-2]).name == 'workspace' for call in copies)
    assert any(call[-1] == 'copy-container:/' and Path(call[-2]).name == 'evaluation' for call in copies)
    assert len(readonly_staging_modes) == 2
    assert ('evaluation', 0o555, [0o444]) in readonly_staging_modes
    assert ('workspace', 0o555, [0o444]) in readonly_staging_modes
    assert runtime.transport_for(identity) == 'docker_cp'
    assert not list(workspace.glob('.aqp-bind-probe-*'))


def _start_copy_fallback(runtime, workspace, **options):
    launches = 0

    def fake_run(command, **kwargs):
        nonlocal launches
        if command[3] == 'run':
            launches += 1
            return subprocess.CompletedProcess(command, 0,
                                               'bind-container\n' if launches == 1 else 'copy-container\n', '')
        if command[3] == 'exec' and 'aqp-bind-probe' in command:
            return subprocess.CompletedProcess(command, 1, '', 'marker missing')
        return subprocess.CompletedProcess(command, 0, '', '')

    with patch('aqp_bench.docker.subprocess.run', side_effect=fake_run):
        return runtime.start('image', workspace, copy_back=True, **options)


def test_copy_fallback_keeps_home_tmpfs_writable_by_agent_uid(tmp_path):
    workspace, home = tmp_path / 'workspace', tmp_path / 'home'
    workspace.mkdir()
    home.mkdir()
    calls = []

    def fake_run(command, **kwargs):
        calls.append(command)
        if command[3] == 'run':
            return subprocess.CompletedProcess(command, 0,
                                               'bind-container\n' if len([c for c in calls if c[3] == 'run']) == 1
                                               else 'copy-container\n', '')
        if command[3] == 'exec' and 'aqp-bind-probe' in command:
            return subprocess.CompletedProcess(command, 1, '', 'marker missing')
        return subprocess.CompletedProcess(command, 0, '', '')

    with patch('aqp_bench.docker.subprocess.run', side_effect=fake_run):
        DockerRuntime().start('image', workspace, home=home)

    fallback_launch = [call for call in calls if call[3] == 'run'][1]
    assert f'/home/agent:uid={os.getuid()},gid={os.getgid()},mode=0700' in fallback_launch
    assert fallback_launch[fallback_launch.index('--user') + 1] == f'{os.getuid()}:{os.getgid()}'
    assert fallback_launch[fallback_launch.index('--workdir') + 1] == '/tmp'


def test_copy_workspace_export_replaces_snapshot_and_preserves_deletions(tmp_path):
    workspace = tmp_path / 'workspace'
    workspace.mkdir()
    (workspace / 'stale.txt').write_text('must be removed')
    runtime = DockerRuntime()
    identity = _start_copy_fallback(runtime, workspace)

    def fake_run(command, **kwargs):
        if command[0] == 'cp' and command[-2] == 'copy-container:/workspace':
            destination = Path(command[-1]) / 'workspace'
            destination.mkdir(mode=0o755)
            (destination / 'new.txt').write_text('agent result')
        return subprocess.CompletedProcess(command, 0, '', '')

    with patch.object(runtime, 'exec', return_value=subprocess.CompletedProcess([], 0, '', '')) as run, \
         patch.object(runtime, '_run', side_effect=fake_run):
        runtime.export_workspace(identity, workspace)
    assert not run.called

    assert (workspace / 'new.txt').read_text() == 'agent result'
    assert not (workspace / 'stale.txt').exists()
    assert workspace.stat().st_mode & 0o777 == 0o755
    assert not list(tmp_path.glob('.aqp-workspace-export-*'))


def test_copy_workspace_export_failure_preserves_original_snapshot(tmp_path):
    workspace = tmp_path / 'workspace'
    workspace.mkdir()
    original = workspace / 'original.txt'
    original.write_text('preserve me')
    runtime = DockerRuntime()
    identity = _start_copy_fallback(runtime, workspace)
    with patch.object(runtime, 'exec', return_value=subprocess.CompletedProcess([], 0, '', '')), \
         patch.object(runtime, '_run', side_effect=RuntimeError('Docker operation cp failed: copy failed')):
        with pytest.raises(RuntimeError, match='Docker operation cp failed'):
            runtime.export_workspace(identity, workspace)
    assert original.read_text() == 'preserve me'


def test_copy_fallback_failure_is_reported_and_container_is_discarded(tmp_path):
    workspace = tmp_path / 'workspace'
    workspace.mkdir()
    calls = []
    launches = 0

    def fake_run(command, **kwargs):
        nonlocal launches
        calls.append(command)
        if command[3] == 'run':
            launches += 1
            return subprocess.CompletedProcess(command, 0,
                                               f'container-{launches}\n', '')
        if command[3] == 'exec' and 'aqp-bind-probe' in command:
            return subprocess.CompletedProcess(command, 1, '', 'bind marker missing')
        if command[3] == 'cp':
            return subprocess.CompletedProcess(command, 1, '', 'copy permission denied')
        return subprocess.CompletedProcess(command, 0, '', '')

    with patch('aqp_bench.docker.subprocess.run', side_effect=fake_run):
        with pytest.raises(RuntimeError, match='bind mount visibility probe failed.*Docker cp fallback failed.*copy permission denied'):
            DockerRuntime().start('image', workspace)

    cleaned = [call for call in calls if call[3] in ('stop', 'rm')]
    assert any(call[-1] == 'container-1' for call in cleaned)
    assert any(call[-1] == 'container-2' for call in cleaned)
    assert not list(workspace.glob('.aqp-bind-probe-*'))


def test_invalid_environment_does_not_leave_workspace_marker(tmp_path):
    workspace = tmp_path / 'workspace'
    workspace.mkdir()
    with pytest.raises(ValueError, match='Invalid explicit container environment'):
        DockerRuntime().start('image', workspace, environment={'BAD-KEY': 'value'})
    assert not list(workspace.glob('.aqp-bind-probe-*'))


def test_start_attempts_container_removal_when_stop_fails(tmp_path):
    workspace = tmp_path / 'workspace'
    home = tmp_path / 'home'
    workspace.mkdir(); home.mkdir()
    runtime = DockerRuntime()
    with patch.object(runtime, '_run', side_effect=[subprocess.CompletedProcess([], 0, 'cid', ''),
                                                    RuntimeError('copy failed')]), \
         patch.object(runtime, 'exec', return_value=subprocess.CompletedProcess([], 0, '', '')), \
         patch.object(runtime, 'stop', side_effect=RuntimeError('stop failed')) as stop, \
         patch.object(runtime, 'remove') as remove:
        with pytest.raises(RuntimeError, match='copy failed'):
            runtime.start('image', workspace, home=home)
    stop.assert_called_once_with('cid')
    remove.assert_called_once_with('cid')


def test_remote_docker_transport_is_rejected_without_probing(monkeypatch):
    monkeypatch.setenv('DOCKER_HOST', 'ssh://production.invalid')
    with patch('aqp_bench.docker.subprocess.run') as run:
        with pytest.raises(ValueError, match='local'):
            DockerRuntime()
        run.assert_not_called()
