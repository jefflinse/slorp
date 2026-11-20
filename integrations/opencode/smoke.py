"""Isolated OpenCode/Slorp terminal smoke; --exercise adds loopback-only model calls."""

import fcntl
import argparse
import importlib.util
import base64
from contextlib import contextmanager
import json
import os
from pathlib import Path
import pty
import re
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
import urllib.error


PASSWORD = 'isolated-native-smoke'


def environment(root):
    root = Path(root)
    for name in ('home', 'tmp', 'config/opencode'):
        (root/name).mkdir(parents=True, exist_ok=True)
    env = {key: os.environ[key] for key in ('PATH', 'LANG', 'LC_ALL') if key in os.environ}
    env.update(HOME=str(root/'home'), TMPDIR=str(root/'tmp'),
               XDG_CONFIG_HOME=str(root/'config'), XDG_DATA_HOME=str(root/'data'),
               XDG_CACHE_HOME=str(root/'cache'), XDG_STATE_HOME=str(root/'state'),
               npm_config_cache=str(root/'npm-cache'), BUN_INSTALL_CACHE_DIR=str(root/'bun-cache'),
               TERM='xterm-256color', OPENCODE_PASSWORD=PASSWORD,
               OPENCODE_DISABLE_EXTERNAL_SKILLS='1', OPENCODE_DISABLE_MODELS_FETCH='1',
               OPENCODE_DISABLE_CLAUDE_CODE='1', OPENCODE_DISABLE_UPDATE_CHECK='1',
               GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull)
    return env


def version(executable, root, env):
    result = subprocess.check_output([executable, '--version'], cwd=root, env=env, text=True).strip()
    assert result == 'opencode v2.3.2', f'Expected installed OpenCode 2.3.2, got {result}'
    return result


def native(base, path, body=None, method=None, password=PASSWORD, timeout=120, unwrap=True):
    headers = {'Content-Type': 'application/json'}
    if password is not None:
        headers['Authorization'] = 'Basic ' + base64.b64encode(f'opencode:{password}'.encode()).decode()
    req = urllib.request.Request(base + path, headers=headers, method=method,
                                 data=None if body is None else json.dumps(body).encode())
    try:
        with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=timeout) as response:
            raw = response.read()
            result = json.loads(raw) if raw else None
    except urllib.error.HTTPError as error:
        raise AssertionError(f'{req.get_method()} {path}: HTTP {error.code}: {error.read().decode()}') from error
    return result['data'] if unwrap and isinstance(result, dict) and 'data' in result else result


def model_config(url):
    return {'$schema': 'https://opencode.ai/config.json', 'update': 'disable',
            'policies': [{'action': 'provider.use', 'resource': '*', 'effect': 'deny'},
                         {'action': 'provider.use', 'resource': 'mock', 'effect': 'allow'}],
            'model': 'mock/fixture', 'agents': {'title': {'disabled': True}},
            'providers': {'mock': {'name': 'Loopback fixture',
                'package': '@opencode/ai/providers/openai-compatible',
                'settings': {'baseURL': url + '/v1', 'apiKey': 'fixture-only'},
                'models': {'fixture': {'name': 'Fixture',
                    'limit': {'context': 32000, 'output': 1000},
                    'capabilities': {'tools': True},
                    'variants': [{'id': 'high'}]}}}}}


def headless_config(config, package, url):
    plugins = []
    found = False
    for plugin in config.get('plugins', []):
        entry = {'package': plugin} if isinstance(plugin, str) else dict(plugin)
        if entry['package'] == package:
            entry['options'] = dict(entry.get('options', {}), opencodeURL=url)
            found = True
        plugins.append(entry)
    assert found, f'Installed Slorp plugin missing from headless config: {package}'
    return dict(config, plugins=plugins)


@contextmanager
def server(executable, root, env, plugin=None, dynamic_port=False):
    """Run an intentionally headless fixture with an explicit plugin transport."""
    assert not (plugin and dynamic_port), 'Dynamic fixture URLs are discovered by the terminal'
    base = f'http://127.0.0.1:{port()}'
    if plugin:
        config_file = Path(root)/'config/opencode/opencode.json'
        config_file.write_text(json.dumps(headless_config(json.loads(config_file.read_text()), plugin, base)))
    log_path = Path(root)/'native-server.log'
    with open(log_path, 'a') as log:
        start = log_path.stat().st_size
        proc = subprocess.Popen([executable, 'serve', '--hostname', '127.0.0.1',
                                 '--port', '0' if dynamic_port else base.rsplit(':', 1)[1]], cwd=root,
                                env=env,
                                stdout=log, stderr=log, start_new_session=True)
        try:
            if dynamic_port:
                def listening():
                    with log_path.open() as output:
                        output.seek(start)
                        match = re.search(r'server listening on (http://127\.0\.0\.1:\d+)', output.read())
                        return match.group(1) if match else None
                base = wait_for(listening)
            wait_for(lambda: native(base, '/api/info'))
            yield base
        finally:
            stop(proc)


def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


@contextmanager
def workspace():
    parent = Path(tempfile.gettempdir())/'opencode'
    path = tempfile.mkdtemp(prefix='slorp-opencode-smoke-', dir=parent if parent.is_dir() else None)
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
    try:
        with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=timeout) as response:
            raw = response.read()
            return json.loads(raw) if raw else True
    except urllib.error.HTTPError as error:
        raise AssertionError(f'{req.get_method()} {path}: HTTP {error.code}: {error.read().decode()}') from error


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
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--shared', action='store_true', help='exercise default TUI shared-service discovery')
    parser.add_argument('--exercise', action='store_true', help='also verify replies/transcript/usage against a loopback model')
    args = parser.parse_args()
    executable = shutil.which('opencode')
    assert executable, 'OpenCode must be installed'
    repo = Path(__file__).resolve().parents[2]
    with workspace() as temporary:
        root = Path(temporary).resolve()
        env = environment(root)
        if args.shared:
            env.pop('OPENCODE_PASSWORD')
        version(executable, root, env)
        binary = root / 'slorp'
        subprocess.run(['go', 'build', '-o', str(binary), '.'], cwd=repo, check=True)
        config = root/'config'/'opencode'
        config.mkdir(parents=True, exist_ok=True)
        unrelated = root/'unrelated-plugin'
        unrelated.mkdir()
        (unrelated/'package.json').write_text(json.dumps({'name': 'unrelated-smoke-plugin', 'type': 'module', 'main': 'index.js'}))
        (unrelated/'index.js').write_text("import { Plugin } from '@opencode/plugin'; export default Plugin.define({id:'smoke.unrelated',setup(){}})\n")
        config_file = config/'opencode.jsonc'
        fixture = None
        configured = {'update': 'disable'}
        if args.exercise:
            spec = importlib.util.spec_from_file_location('lifecycle_smoke', Path(__file__).with_name('lifecycle-smoke.py'))
            lifecycle = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(lifecycle)
            fixture = lifecycle.Fixture()
            configured = model_config(f'http://127.0.0.1:{fixture.server_port}')
        unrelated_entry = {'package': str(unrelated), 'options': {
            'marker': 'kept', 'nested': {'enabled': True, 'labels': ['one', 'two']}}}
        configured['plugins'] = [unrelated_entry]
        user_config = configured
        comment = '// Preserve this native V2 config comment.\n'
        config_file.write_text(comment + json.dumps(user_config, indent=2))
        slorp_port, agent_port = port(), port()
        slorp_url, agent_url = f'http://127.0.0.1:{slorp_port}', f'http://127.0.0.1:{agent_port}'
        if args.shared:
            subprocess.run([executable, 'service', 'set', 'port', str(agent_port)],
                           cwd=root, env=env, check=True, capture_output=True, timeout=30)
        processes = []
        log = open(root/'processes.log', 'w')

        def service(command):
            p = subprocess.Popen(command, cwd=root, env=env, stdout=log, stderr=log, start_new_session=True)
            processes.append(p)
            return p

        def native_api(path, body=None):
            if not args.shared:
                return native(agent_url, path, body)
            result = subprocess.run([executable, 'api', 'get' if body is None else 'post', path,
                                     *([] if body is None else ['--data', json.dumps(body)])],
                                    cwd=root, env=env, capture_output=True, text=True, timeout=30)
            assert result.returncode == 0, result.stderr
            value = json.loads(result.stdout)
            return value.get('data', value) if isinstance(value, dict) else value

        def exercise(sid, prompt, turns):
            request(slorp_url, f'/api/sessions/{sid}/reply', {'text': prompt})
            def answered():
                row = next((s for s in listed() if s['id'] == sid), {})
                return row.get('lastPrompt') == prompt and row.get('lastReply') == lifecycle.ANSWER \
                    and row.get('status') == 'idle' and row.get('used', {}).get('in') == 12 * turns \
                    and row.get('used', {}).get('out') == 3 * turns and row.get('canReply')
            wait_for(answered, seconds=45)
            assert len(fixture.captured()) == turns, fixture.captured()

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
            assert comment in configured, configured
            preserved = json.loads(configured.replace(comment, '', 1))
            assert unrelated_entry in preserved['plugins'], preserved
            assert {k: v for k, v in preserved.items() if k != 'plugins'} == {
                k: v for k, v in user_config.items() if k != 'plugins'}, preserved
            print('PASS: standalone binary installs both bundled entry points', flush=True)
            print('PASS: native V2 installer preserves JSONC comments, unrelated config and nested plugin-object options', flush=True)

            agent = service([executable, 'serve', '--service', '--print-logs'] if args.shared else
                            [executable, 'serve', '--port', str(agent_port), '--print-logs'])
            if args.shared:
                wait_for(lambda: ('server listening on ' + agent_url) in (root/'processes.log').read_text())
            wait_for(lambda: native_api('/api/info'))
            sessions = [native_api('/api/session', {'title': title, 'location': {'directory': str(root)}}) for title in ('First idle conversation', 'Second idle conversation', 'Saved only')]
            if not args.shared:
                stop(agent)
            flags = [] if args.shared else ['--standalone']
            first = terminal([executable, *flags, '--session', sessions[0]['id']], root, env)
            processes.append(first)
            listed = lambda: request(slorp_url, '/api/sessions')['sessions']
            wait_for(lambda: any(s['id'] == sessions[0]['id'] and s['canReply'] for s in listed()))
            second = terminal([executable, *flags, '--session', sessions[1]['id']], root, env)
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
            if fixture:
                exercise(sessions[1]['id'], 'Terminal transport fixture prompt', 1)
                print('PASS: normal terminal transport delivers a Slorp reply and reads native transcript/usage', flush=True)
            stop(first)
            if args.shared:
                stop(second)
                expected = {s['id'] for s in sessions[:2]}
                wait_for(lambda: {s['id'] for s in listed()} == expected and
                         all(s['canReply'] and not s.get('pluginTui') for s in listed()), seconds=40)
                assert native_api('/api/info')['pid'] == agent.pid
                print('PASS: default shared discovery keeps observed roots replyable after both TUIs exit', flush=True)
                if fixture:
                    exercise(sessions[1]['id'], 'Shared transport after both terminals exited', 2)
                    print('PASS: shared discovery reads the next reply/transcript/usage with no TUI running', flush=True)
                stop(agent)
                wait_for(lambda: not listed(), seconds=40)
                print('PASS: shared service exit retires its observed roots', flush=True)
            else:
                wait_for(lambda: {s['id'] for s in listed()} == {sessions[1]['id']}, seconds=40)
                print('PASS: standalone terminal exit retires only its session', flush=True)
        except Exception:
            log.flush()
            print('=== Service logs (last 40 lines) ===\n' +
                  '\n'.join((root/'processes.log').read_text().splitlines()[-40:]), flush=True)
            try:
                state = request(slorp_url, '/api/sessions')
                (root/'slorp-state.json').write_text(json.dumps(state, indent=2))
                print('=== Slorp sessions ===\n' + json.dumps(state['sessions'], indent=2), flush=True)
            except (OSError, ValueError, AssertionError) as error:
                print('State unavailable:', error, flush=True)
            if fixture:
                (root/'fixture-requests.json').write_text(json.dumps(fixture.captured(), indent=2))
            raise
        finally:
            for proc in reversed(processes):
                stop(proc)
            if fixture:
                fixture.close()
            log.close()


if __name__ == '__main__':
    main()
