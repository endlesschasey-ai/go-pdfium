#!/usr/bin/env python3
"""Run one bounded build and stop only its recorded container on failure."""
import json
import pathlib
import re
import signal
import subprocess
import sys
import time

IMAGE = 'emscripten/emsdk@sha256:d050f4c2adf6070cffde3384e78244b38c393834d69ecb044d1d913d5266426e'


class BuildInterrupted(Exception):
    def __init__(self, signum):
        self.signum = signum
        super().__init__(f'build interrupted by signal {signum}')


def read_state(cid, output, name):
    command = subprocess.run(['docker', 'inspect', cid], capture_output=True, text=True, timeout=30)
    (output / name).write_text(command.stdout)
    if command.returncode:
        raise RuntimeError(f'inspect failed: {command.stderr.strip()}')
    state = json.loads(command.stdout)[0]['State']
    if not isinstance(state.get('Running'), bool):
        raise RuntimeError('inspect did not report container Running state')
    return state


def run(output, wall_seconds=5400):
    recipe = pathlib.Path(__file__).resolve().parent
    output = pathlib.Path(output).resolve()
    if output.exists():
        raise RuntimeError('output must be a new task-exclusive directory')
    output.mkdir(parents=True)
    cidfile = output / 'container-id.txt'
    cid = None
    started = time.monotonic()
    result = {'image': IMAGE, 'cpus': 2, 'memory_bytes': 4294967296,
              'oom_score_adj': 500, 'timeout_seconds': wall_seconds}
    exit_code = 1
    previous = {}

    def interrupted(signum, frame):
        raise BuildInterrupted(signum)

    def remaining():
        value = wall_seconds - (time.monotonic() - started)
        if value <= 0:
            raise subprocess.TimeoutExpired('bounded build', wall_seconds)
        return value

    for signum in (signal.SIGINT, signal.SIGTERM):
        previous[signum] = signal.signal(signum, interrupted)
    try:
        subprocess.run(['docker', 'pull', '--platform=linux/amd64', IMAGE],
                       check=True, timeout=remaining())
        args = ['docker', 'create', '--platform=linux/amd64', '--cpus=2',
                '--memory=4g', '--memory-swap=4g', '--oom-score-adj=500',
                '--init', '--workdir=/work', '--cidfile', str(cidfile),
                '--mount', f'type=bind,source={recipe},target=/inputs,readonly',
                '--mount', f'type=bind,source={output},target=/work',
                IMAGE, 'bash', '/inputs/build-verified.sh']
        cid = subprocess.check_output(args, text=True, timeout=remaining()).strip()
        if not re.fullmatch(r'[0-9a-f]{64}', cid):
            raise RuntimeError('create returned an invalid container ID')
        # Real Docker writes --cidfile even if create is interrupted after daemon
        # success. Keep a fallback for other conforming Docker implementations.
        if not cidfile.exists():
            cidfile.write_text(cid + '\n')
        result['container'] = cid
        exit_code = subprocess.run(['docker', 'start', '--attach', cid], timeout=remaining()).returncode
        result['exit_code'] = exit_code
        result['reason'] = 'completed' if exit_code == 0 else 'docker_start_failed'
    except BuildInterrupted as error:
        exit_code = 128 + error.signum
        result.update(reason='signal', signal=error.signum, error=str(error))
    except subprocess.TimeoutExpired as error:
        exit_code = 124
        result.update(reason='timeout', timeout=True, error=str(error))
    except BaseException as error:
        exit_code = 130 if isinstance(error, KeyboardInterrupt) else 1
        result.update(reason=type(error).__name__, error=str(error))
    finally:
        # A second interrupt must not cut off cleanup of the owned container.
        for signum in previous:
            signal.signal(signum, signal.SIG_IGN)
        try:
            try:
                if cidfile.exists():
                    recorded = cidfile.read_text().strip()
                    if re.fullmatch(r'[0-9a-f]{64}', recorded):
                        cid = recorded
            except OSError as error:
                result['cidfile_error'] = str(error)
            if cid and re.fullmatch(r'[0-9a-f]{64}', cid):
                result['container'] = cid
                running = True
                try:
                    state = read_state(cid, output, 'container-inspect.json')
                    running = state['Running']
                    result['container_exit_code'] = state.get('ExitCode')
                except Exception as error:
                    result['inspect_error'] = str(error)
                if running:
                    try:
                        killed = subprocess.run(['docker', 'kill', cid], capture_output=True,
                                                text=True, timeout=30)
                        result['cleanup_kill_exit'] = killed.returncode
                        if killed.returncode:
                            result['cleanup_kill_error'] = killed.stderr.strip()
                        stopped = not read_state(cid, output, 'container-inspect-after-cleanup.json')['Running']
                        result['cleanup_confirmed_stopped'] = stopped
                    except Exception as error:
                        result['cleanup_error'] = str(error)
                        result['cleanup_confirmed_stopped'] = False
                    # A supposedly successful attach must not leave work running.
                    if exit_code == 0:
                        exit_code = 1
                        result['reason'] = 'unexpected_running_after_attach'
                else:
                    result['cleanup_confirmed_stopped'] = True
                    if exit_code == 0:
                        container_exit = result.get('container_exit_code')
                        if not isinstance(container_exit, int):
                            exit_code = 1
                            result['reason'] = 'container_exit_unavailable'
                        elif container_exit != 0:
                            exit_code = container_exit
                            result['reason'] = 'container_exit_failed'
                if not result.get('cleanup_confirmed_stopped') and exit_code == 0:
                    exit_code = 1
                    result['reason'] = 'cleanup_unconfirmed'
            result['wall_seconds'] = time.monotonic() - started
            result['launcher_exit_code'] = exit_code
            (output / 'run-result.json').write_text(json.dumps(result, indent=2) + '\n')
        finally:
            for signum, handler in previous.items():
                signal.signal(signum, handler)
    return exit_code


if __name__ == '__main__':
    sys.exit(run(sys.argv[1]))
