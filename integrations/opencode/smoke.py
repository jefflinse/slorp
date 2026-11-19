"""Opt-in local OpenCode/Slorp smoke test; uses isolated config and no model calls."""

import fcntl
from contextlib import contextmanager
import json
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import socket
import struct
import subprocess
import tempfile
import termios
import threading
import time
import urllib.request


def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


@contextmanager
def workspace():
    path = tempfile.mkdtemp(prefix='slorp-opencode-smoke-')
    try:
        yield path
    finally:
        if os.environ.get('SLORP_SMOKE_KEEP'):
            print('Smoke-test files:', path, flush=True)
        else:
            shutil.rmtree(path)


def wait_for(check, seconds=30):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            value = check()
            if value:
                return value
        except (OSError, ValueError):
            pass
        time.sleep(.2)
    raise AssertionError('Timed out waiting for smoke-test condition')


def request(base, path, body=None, timeout=10):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=data, headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=timeout) as response:
        raw = response.read()
        return json.loads(raw) if raw else True


def stop(proc):
    if proc.poll() is not None:
        return
    os.killpg(proc.pid, signal.SIGINT)
    try:
        proc.wait(timeout=4)
    except subprocess.TimeoutExpired:
        os.killpg(proc.pid, signal.SIGKILL)
        proc.wait()


def terminal(command, cwd, env):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 120, 0, 0))
    proc = subprocess.Popen(command, cwd=cwd, env=env, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
    os.close(slave)

    def drain():
        try:
            while proc.poll() is None:
                if not select.select([master], [], [], .2)[0]:
                    continue
                chunk = os.read(master, 65536)
                if b'\x1b[c' in chunk:
                    os.write(master, b'\x1b[?62;4;22c')
                if b'\x1b[6n' in chunk:
                    os.write(master, b'\x1b[1;1R')
        except OSError:
            pass
        finally:
            os.close(master)

    threading.Thread(target=drain, daemon=True).start()
    return proc


def main():
    executable = shutil.which('opencode')
    assert executable, 'OpenCode must be installed'
    repo = Path(__file__).resolve().parents[2]
    with workspace() as temporary:
        root = Path(temporary)
        (root / 'home').mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith(('OPENCODE_', 'CMUX_', 'SLORP_'))}
        env.update(HOME=str(root/'home'), OPENCODE_TEST_HOME=str(root/'home'), npm_config_cache=str(root/'npm-cache'),
                   XDG_CONFIG_HOME=str(root/'config'), XDG_DATA_HOME=str(root/'data'),
                   XDG_STATE_HOME=str(root/'state'), XDG_CACHE_HOME=str(root/'cache'), TERM='xterm-256color',
                   OPENCODE_DISABLE_PROJECT_CONFIG='1', OPENCODE_DISABLE_EXTERNAL_SKILLS='1',
                   OPENCODE_DISABLE_DEFAULT_PLUGINS='1', OPENCODE_DISABLE_MODELS_FETCH='1')
        binary = root / 'slorp'
        subprocess.run(['go', 'build', '-o', str(binary), '.'], cwd=repo, check=True)
        config = root/'config'/'opencode'
        config.mkdir(parents=True)
        unrelated = root/'unrelated-plugin'
        unrelated.mkdir()
        (unrelated/'package.json').write_text(json.dumps({'name': 'unrelated-smoke-plugin', 'type': 'module', 'main': 'index.js'}))
        (unrelated/'index.js').write_text('export default async () => ({})\n')
        config_file = config/'opencode.jsonc'
        config_file.write_text('// Preserve this comment.\n'+json.dumps({'username': 'smoke-user', 'plugin': [[str(unrelated), {'marker': 'kept'}]]}, indent=2))
        slorp_port, agent_port = port(), port()
        slorp_url, agent_url = f'http://127.0.0.1:{slorp_port}', f'http://127.0.0.1:{agent_port}'
        processes = []
        log = open(root/'processes.log', 'w')

        def service(command):
            p = subprocess.Popen(command, cwd=root, env=env, stdout=log, stderr=log, start_new_session=True)
            processes.append(p)
            return p

        slorp_command = [str(binary), '-addr', f'127.0.0.1:{slorp_port}', '-data', str(root/'slorp-data'),
                         '-claude', str(root/'no-claude'), '-opencode', executable, '-llm', '']
        try:
            slorp = service(slorp_command)
            wait_for(lambda: request(slorp_url, '/api/sessions').get('plugins', {}).get('opencode'))
            request(slorp_url, '/api/plugins/opencode', {})
            installed = request(slorp_url, '/api/sessions')['plugins']['opencode']
            assert installed['installed'] and not installed.get('needsSetup') and installed['source'] == 'managed', installed
            assert installed['path'].startswith(str(root)), installed
            configured = config_file.read_text()
            assert '// Preserve this comment.' in configured and 'smoke-user' in configured and 'kept' in configured
            print('PASS: standalone binary installs both bundled entry points', flush=True)
            print('PASS: real installer preserves existing config comments, plugin and tuple options', flush=True)

            agent = service([executable, 'serve', '--port', str(agent_port), '--print-logs'])
            wait_for(lambda: request(agent_url, '/global/health'))
            # OpenCode 1.19.6 can leave a 60-second dependency lock after its first plugin install.
            request(agent_url, '/path', timeout=120)
            sessions = [request(agent_url, '/session', {'title': title}) for title in ('First idle conversation', 'Second idle conversation', 'Saved only')]
            stop(agent)
            first = terminal([executable, '--session', sessions[0]['id']], root, env)
            processes.append(first)
            listed = lambda: request(slorp_url, '/api/sessions')['sessions']
            wait_for(lambda: any(s['id'] == sessions[0]['id'] and s['canReply'] for s in listed()))
            second = terminal([executable, '--session', sessions[1]['id']], root, env)
            processes.append(second)
            wait_for(lambda: len(listed()) == 2)
            for s in listed():
                assert s['status'] == 'idle' and not s['lastPrompt'], s
                assert s['pluginRevision'] == installed['revision'], s
            assert sessions[2]['id'] not in {s['id'] for s in listed()}
            print('PASS: two idle resumed sessions appear without prompts; saved-only session stays absent', flush=True)

            stop(slorp)
            slorp = service(slorp_command)
            wait_for(lambda: len(listed()) == 2)
            print('PASS: Slorp restart recovers both idle sessions', flush=True)
            stop(first)
            wait_for(lambda: {s['id'] for s in listed()} == {sessions[1]['id']}, seconds=40)
            print('PASS: terminal exit retires only its session', flush=True)
        except Exception:
            log.flush()
            print((root/'processes.log').read_text())
            raise
        finally:
            for proc in reversed(processes):
                stop(proc)
            log.close()


if __name__ == '__main__':
    main()
