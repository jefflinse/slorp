"""Native, authenticated, cross-process OpenCode 2.3.2 --server smoke; no prompts.

Run with python3 -B integrations/opencode/attach-smoke.py after the bundled
cross-process presence change is ready. TMPDIR selects the temporary parent;
SLORP_SMOKE_KEEP=1 retains isolated config, native data, route traces and logs.
The TUI observer records routes without fabricating presence. Only
the single-terminal phase sends native navigation commands. Managed-runtime
presence is covered by the existing Go/native and plugin suites, not a fake
prompt or fabricated bridge request here.
"""

import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from smoke import environment, port, request, stop, terminal, wait_for, workspace, version as require_version


OBSERVER = """import { appendFileSync, existsSync, readFileSync, unlinkSync } from 'node:fs'
import { Plugin } from '@opencode/plugin/tui'
export default Plugin.define({
  id: 'attach-smoke-observer',
  setup: api => {
    const file = process.env.ATTACH_SMOKE_ROUTE_FILE
    if (!file) throw new Error('Missing isolated route trace path')
    const record = () => {
      const command = file + '.navigate'
      if (existsSync(command)) {
        const route = JSON.parse(readFileSync(command, 'utf8'))
        unlinkSync(command)
        api.ui.router.navigate(route)
      }
      appendFileSync(file, JSON.stringify({
        at: Date.now(), pid: process.pid, route: api.ui.router.current(),
        directory: (api.location ?? api.data.location.default())?.directory,
      }) + '\\n')
    }
    record()
    const timer = setInterval(record, 250)
    timer.unref?.()
    return () => clearInterval(timer)
  },
})
"""


def until(label, check, seconds=40):
    try:
        return wait_for(check, seconds=seconds)
    except AssertionError as error:
        raise AssertionError(f'{label}: {error}') from error


def consistently(seconds, check):
    deadline = time.monotonic() + seconds
    while True:
        check()
        if time.monotonic() >= deadline:
            return
        time.sleep(.25)


def records(path):
    if not path.exists():
        return []
    # A concurrent append may leave one incomplete final line in this read.
    return [json.loads(line) for line in path.read_text().splitlines(keepends=True)
            if line.endswith('\n')]


def main():
    executable = shutil.which('opencode')
    assert executable, 'OpenCode must be installed'
    repo = Path(__file__).resolve().parents[2]
    expected_version = json.loads((Path(__file__).with_name('package.json')).read_text())['version']
    urllib.request.install_opener(urllib.request.build_opener(urllib.request.ProxyHandler({})))
    with workspace() as temporary:
        root = Path(temporary).resolve()
        home, config, project = root/'home', root/'config'/'opencode', root/'project'
        for directory in (home, config, project, root/'tmp'):
            directory.mkdir(parents=True)
        env = environment(root)
        version = require_version(executable, root, env)
        help_text = subprocess.check_output([executable, '--help'], env=env, cwd=root,
                                            text=True, stderr=subprocess.STDOUT, timeout=30)
        (root/'attach-help.txt').write_text(help_text)
        for flag in ('--session', '--server'):
            assert flag in help_text, f'Installed attach CLI does not document {flag}'
        # --session is the CLI flag; sessionID is the native API field.
        subprocess.run(['git', 'init', '--quiet', str(project)], env=env, cwd=root, check=True)
        binary = root/'slorp'
        # Keep the ambient Go build/module caches, not the isolated runtime HOME.
        subprocess.run(['go', 'build', '-o', str(binary), '.'], cwd=repo, check=True)

        observer = root/'observer'
        observer.mkdir()
        (observer/'package.json').write_text(json.dumps({
            'name': 'attach-smoke-observer', 'type': 'module',
            'exports': {'./tui': './tui.js'},
        }))
        (observer/'tui.js').write_text(OBSERVER)
        (config/'opencode.json').write_text(json.dumps({'update': 'disable'}))
        (config/'cli.json').write_text(json.dumps({'plugins': [str(observer)]}))
        slorp_port, agent_port = port(), port()
        while agent_port == slorp_port:
            agent_port = port()
        slorp_url, agent_url = f'http://127.0.0.1:{slorp_port}', f'http://127.0.0.1:{agent_port}'
        username, password = 'opencode', uuid.uuid4().hex
        server_env = dict(env, OPENCODE_PASSWORD=password)
        slorp_command = [str(binary), '-addr', f'127.0.0.1:{slorp_port}', '-data', str(root/'slorp-data'),
                         '-claude', str(root/'no-claude'), '-opencode', executable, '-llm', '']
        server_command = [executable, 'serve', '--hostname', '127.0.0.1', '--port', str(agent_port),
                          '--print-logs', '--log-level', 'debug']
        children, terminals = [], {}
        log_path = root/'processes.log'
        log = log_path.open('w')
        stage = 'setup'
        baseline = None

        def service(command, child_env):
            proc = subprocess.Popen(command, cwd=project, env=child_env, stdout=log, stderr=log,
                                    start_new_session=True)
            children.append(proc)
            return proc

        def native(path, body=None, credential=password, expected=200, timeout=10):
            headers = {'Content-Type': 'application/json'}
            if credential is not None:
                token = base64.b64encode(f'{username}:{credential}'.encode()).decode()
                headers['Authorization'] = 'Basic ' + token
            query = urllib.parse.urlencode({'directory': str(project)})
            req = urllib.request.Request(agent_url + path + '?' + query,
                                         data=None if body is None else json.dumps(body).encode(), headers=headers)
            try:
                response = urllib.request.urlopen(req, timeout=timeout)
            except urllib.error.HTTPError as error:
                if error.code != expected:
                    with error:
                        raise AssertionError(f'{path}: HTTP {error.code}: {error.read().decode()}') from error
                response = error
            with response:
                raw = response.read()
                assert response.status == expected, (path, response.status, raw)
                result = json.loads(raw) if raw and expected == 200 else None
                return result.get('data', result) if isinstance(result, dict) else result

        def state():
            return request(slorp_url, '/api/sessions')

        def listed():
            return {s['id']: s for s in state()['sessions']}

        def ready(ids, tui=True):
            current = listed()
            return set(current) == set(ids) and all(
                s.get('canReply') and (bool(s.get('pluginTui')) == tui if isinstance(tui, bool)
                                      else s['id'] not in tui or s.get('pluginTui')) and s['status'] == 'idle'
                and not s.get('lastPrompt') and not s.get('lastReply')
                and s.get('pluginVersion') == expected_version
                and s.get('pluginRevision') == installed['revision']
                for s in current.values())

        def assert_ready(ids, tui=True):
            assert ready(ids, tui), ('unexpected Slorp roots/capabilities', listed(), ids, tui)

        def snapshot():
            sessions = native('/api/session')
            return {s['id']: {'time': {k: v for k, v in s['time'].items() if k != 'viewed'}, 'parentID': s.get('parentID'),
                              'messages': len(native(f'/api/session/{s["id"]}/message'))}
                    for s in sessions}

        def unchanged():
            current = snapshot()
            assert current == baseline, ('native timestamps/message counts changed', baseline, current)

        def attach(name, session_id):
            path = root/f'{name}-{len(children)}.routes.jsonl'
            child_env = dict(env, ATTACH_SMOKE_ROUTE_FILE=str(path), OPENCODE_PASSWORD=password)
            proc = terminal([executable, '--server', agent_url, '--session', session_id,
                             str(project)], project, child_env)
            children.append(proc)
            terminals[name] = (proc, path)
            return proc

        def route(name, session_id):
            proc, path = terminals[name]
            assert proc.poll() is None, f'{name} attach exited: {proc.returncode}'
            trace = records(path)
            if not trace:
                return False
            last = trace[-1]
            assert last['pid'] != agent.pid, 'TUI and server must be separate processes'
            expected = {'type': 'home'} if session_id is None else {
                'type': 'session', 'sessionID': session_id}
            return last['route'] == expected and last['directory'] == str(project) \
                and time.time() * 1000 - last['at'] < 3000

        def independent(selections, cursors):
            for name, session_id in selections.items():
                assert route(name, session_id), (name, 'route changed or observer stopped')
                trace = records(terminals[name][1])
                for record in trace[cursors[name]:]:
                    assert record['route'] == {'type': 'session', 'sessionID': session_id}, (
                        name, 'presence navigated another terminal', record)

        def warmup():
            until('authenticated native health', lambda: native('/api/info'))
            for credential in (None, 'wrong-password'):
                native('/api/info', credential=credential, expected=401)

        def navigate(session_id):
            path = Path(str(terminals['first'][1]) + '.navigate')
            path.write_text(json.dumps({'type': 'home'} if session_id is None else
                                       {'type': 'session', 'sessionID': session_id}))

        try:
            slorp = service(slorp_command, env)
            until('Slorp plugin setup', lambda: state().get('plugins', {}).get('opencode'))
            request(slorp_url, '/api/plugins/opencode', {}, timeout=120)
            installed = state()['plugins']['opencode']
            assert installed['installed'] and not installed.get('needsSetup'), installed
            assert installed['source'] == 'managed' and installed['version'] == expected_version, installed
            assert Path(installed['path']).resolve().is_relative_to(root), installed
            assert installed['revision'], installed
            stage = 'server warmup and saved-only baseline'
            agent = service(server_command, server_env)
            warmup()
            sessions = [native('/api/session', {'title': title, 'location': {'directory': str(project)}}) for title in
                        ('First attached root', 'Second attached root', 'Single-terminal switch', 'Saved unopened')]
            first_id, second_id, switched_id, saved_id = [s['id'] for s in sessions]
            baseline = snapshot()
            assert len(baseline) == 4 and all(not s['parentID'] and s['messages'] == 0 for s in baseline.values())
            (root/'native-before.json').write_text(json.dumps(baseline, indent=2))
            consistently(6, lambda: assert_ready([]))
            print('PASS: BasicAuth rejects missing/wrong credentials; four native empty saved roots stay absent', flush=True)

            stage = 'first authenticated remote terminal'
            attach('first', first_id)
            until('first CLI route', lambda: route('first', first_id), seconds=120)
            until('first selected root presence', lambda: ready([first_id]))
            unchanged()
            print('PASS: CLI-authenticated cross-process attach discovers only its selected idle root, canReply/pluginTui true', flush=True)

            stage = 'single-terminal switch and home navigation'
            navigate(switched_id)
            until('native selectSession route', lambda: route('first', switched_id))
            until('switched idle root presence', lambda: ready([first_id, switched_id], {switched_id}))
            navigate(None)
            until('native home route', lambda: route('first', None))
            until('home companion presence', lambda: ready([first_id, switched_id]))
            consistently(12, lambda: assert_ready([first_id, switched_id]))
            assert route('first', None), 'Periodic presence navigated away from home'
            navigate(first_id)
            until('return to first route', lambda: route('first', first_id))
            unchanged()
            print('PASS: native switch/home/return works with one TUI; observed roots remain live and native history is unchanged', flush=True)

            stage = 'independent simultaneous CLI attach routes'
            selections = {'first': first_id, 'second': second_id}
            cursors = {'first': len(records(terminals['first'][1]))}
            attach('second', second_id)
            until('second CLI route', lambda: route('second', second_id), seconds=120)
            cursors['second'] = len(records(terminals['second'][1]))
            until('second selected root presence', lambda: ready([first_id, second_id, switched_id], {first_id, second_id}))
            consistently(12, lambda: independent(selections, cursors))
            assert_ready([first_id, second_id, switched_id], {first_id, second_id})
            unchanged()
            print('PASS: two attach PTYs retain distinct initial CLI routes through periodic presence; saved-unopened root stays absent', flush=True)

            stage = 'Slorp restart recovery'
            stop(slorp)
            slorp = service(slorp_command, env)
            until('Slorp restart reclaims observed roots', lambda: ready([first_id, second_id, switched_id], {first_id, second_id}))
            independent(selections, cursors)
            unchanged()
            print('PASS: Slorp restart recovers all roots observed by the still-running source server', flush=True)

            stage = 'native server restart and selected-only recovery'
            stop(agent)
            until('source silence retires all observed roots (30-second policy)', lambda: not listed(), seconds=45)
            agent = service(server_command, server_env)
            warmup()
            for name, session_id in selections.items():
                proc, _ = terminals[name]
                if proc.poll() is not None:
                    print(f'NOTE: native {name} attach exited ({proc.returncode}) on server loss; relaunching its original CLI route', flush=True)
                    attach(name, session_id)
                    until(f'{name} relaunched route', lambda n=name, s=session_id: route(n, s), seconds=120)
                    cursors[name] = len(records(terminals[name][1]))
                else:
                    print(f'NOTE: native {name} attach stayed alive; requiring automatic reconnect without navigation', flush=True)
            until('periodic presence rediscovers selected roots only', lambda: ready([first_id, second_id]), seconds=60)
            consistently(12, lambda: (assert_ready([first_id, second_id]), independent(selections, cursors)))
            assert switched_id not in listed() and saved_id not in listed()
            unchanged()
            print('PASS: same-URL/data server restart rediscovers selected roots only in the new server context', flush=True)

            stage = 'one terminal stops while the other companion stays fresh'
            stop(terminals['first'][0])
            consistently(12, lambda: (assert_ready([first_id, second_id], {second_id}),
                                     independent({'second': second_id}, cursors)))
            unchanged()
            print('PASS: stopping one TUI leaves the other companion fresh; already-observed roots remain live', flush=True)

            stage = 'last terminal stops, source retains roots'
            stop(terminals['second'][0])
            until('companion freshness expires without retiring roots', lambda: ready([first_id, second_id], tui=False))
            consistently(4, lambda: assert_ready([first_id, second_id], tui=False))
            unchanged()
            (root/'native-after.json').write_text(json.dumps(snapshot(), indent=2))
            print('PASS: last TUI exit expires pluginTui, not server-owned root liveness; timestamps/message counts unchanged', flush=True)

            stage = 'final source stop retirement'
            stop(agent)
            until('all roots retire after source silence', lambda: not listed(), seconds=45)
            consistently(3, lambda: assert_ready([]))
            print(f'PASS: source stop retires all observed roots; OpenCode {version}, plugin {expected_version}; no prompts/model calls', flush=True)
        except BaseException:
            print(f'\nFAIL during: {stage}', flush=True)
            log.flush()
            print('=== Service logs (last 40 lines) ===\n' +
                  '\n'.join(log_path.read_text(errors='replace').splitlines()[-40:]), flush=True)
            for proc in children:
                print(f'Owned process {proc.pid}: exit={proc.poll()}', flush=True)
            for path in root.glob('*.routes.jsonl'):
                print(f'=== {path.name} (last 20 records) ===\n' + '\n'.join(path.read_text().splitlines()[-20:]), flush=True)
            try:
                print('=== Slorp state ===\n' + json.dumps(state(), indent=2), flush=True)
            except (OSError, ValueError) as error:
                print('Slorp state unavailable:', error, flush=True)
            if baseline is not None:
                try:
                    print('=== Native state ===\n' + json.dumps(snapshot(), indent=2), flush=True)
                except (OSError, ValueError, AssertionError) as error:
                    print('Native state unavailable:', error, flush=True)
            for path in (root/'data').glob('opencode/log/*.log'):
                print(f'=== {path.name} (last 20 lines) ===\n' +
                      '\n'.join(path.read_text(errors='replace').splitlines()[-20:]), flush=True)
            raise
        finally:
            try:
                for proc in reversed(children):
                    stop(proc)
            finally:
                log.close()


if __name__ == '__main__':
    main()
