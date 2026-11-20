"""Native lifecycle smoke for installed OpenCode 1.19.6; no paid model calls.

Run with python3 -B integrations/opencode/lifecycle-smoke.py. TMPDIR selects
the temporary parent; SLORP_SMOKE_KEEP=1 retains diagnostics and fixture data.
"""

import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shlex
import shutil
import signal
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from smoke import port, request, stop, terminal, wait_for, workspace


ANSWER = 'Fixture answer'
BLOCK = 'lifecycle-smoke: block this turn'
OPTIONS = {'agentName': 'review', 'model': 'mock/fixture', 'variant': 'high'}


class Fixture(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self):
        super().__init__(('127.0.0.1', 0), FixtureHandler)
        self.records = []
        self.lock = threading.Lock()
        self.blocked = threading.Event()
        self.release = threading.Event()
        self.thread = threading.Thread(target=self.serve_forever, daemon=True)
        self.thread.start()

    def captured(self):
        with self.lock:
            return list(self.records)

    def close(self):
        self.release.set()
        self.shutdown()
        self.server_close()
        self.thread.join(timeout=5)


class FixtureHandler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get('Content-Length', '0'))))
        with self.server.lock:
            self.server.records.append({'path': self.path, 'body': body})
        if self.path != '/v1/chat/completions':
            self.send_error(404, 'Only fixture chat completions are supported')
            return
        if BLOCK in json.dumps(body.get('messages', [])[-1:]):
            self.server.blocked.set()
            if not self.server.release.wait(90):
                return
        result = {'id': 'chatcmpl-fixture', 'created': int(time.time()),
                  'model': body.get('model', 'fixture')}
        usage = {'prompt_tokens': 12, 'completion_tokens': 3, 'total_tokens': 15}
        try:
            self.send_response(200)
            if body.get('stream'):
                self.send_header('Content-Type', 'text/event-stream')
                self.end_headers()
                for delta, finish in [({'role': 'assistant', 'content': ''}, None),
                                      ({'content': ANSWER}, None), ({}, 'stop')]:
                    chunk = dict(result, object='chat.completion.chunk', choices=[
                        {'index': 0, 'delta': delta, 'finish_reason': finish}])
                    self.wfile.write(('data: ' + json.dumps(chunk) + '\n\n').encode())
                    self.wfile.flush()
                chunk = dict(result, object='chat.completion.chunk', choices=[], usage=usage)
                self.wfile.write(('data: ' + json.dumps(chunk) + '\n\ndata: [DONE]\n\n').encode())
            else:
                self.send_header('Content-Type', 'application/json')
                self.end_headers()
                result.update(object='chat.completion', usage=usage, choices=[
                    {'index': 0, 'message': {'role': 'assistant', 'content': ANSWER},
                     'finish_reason': 'stop'}])
                self.wfile.write(json.dumps(result).encode())
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass  # Expected when Stop aborts the deliberately blocked request.


def api(base, path, body=None, expected=200, timeout=120):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=data, headers={'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            raw = response.read().decode()
            assert response.status == expected, (path, response.status, raw)
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as error:
        raise AssertionError(f'{path}: HTTP {error.code}: {error.read().decode()}') from error


def process_table():
    output = subprocess.check_output(['ps', '-axo', 'pid=,ppid=,pgid=,stat='], text=True)
    return {int(pid): (int(parent), int(group))
            for pid, parent, group, status in (line.split() for line in output.splitlines())
            if not status.startswith('Z')}


class ProcessTrees:
    """Track fixture descendants, including separate native supervisor groups."""

    def __init__(self):
        self.groups = {}
        self.lock = threading.Lock()
        self.closed = threading.Event()
        self.error = None
        self.thread = threading.Thread(target=self.watch, daemon=True)
        self.thread.start()

    def add(self, proc):
        with self.lock:
            self.groups[proc.pid] = {proc.pid}

    def watch(self):
        try:
            while not self.closed.wait(.1):
                table = process_table()
                with self.lock:
                    for root, groups in self.groups.items():
                        descendants = {root}
                        while True:
                            more = {pid for pid, (parent, _) in table.items() if parent in descendants}
                            if more <= descendants:
                                break
                            descendants.update(more)
                        groups.update(group for pid, (_, group) in table.items()
                                      if pid in descendants and group in descendants)
        except BaseException as error:
            self.error = error

    def remaining(self, proc):
        assert self.error is None, self.error
        with self.lock:
            groups = set(self.groups[proc.pid])
        return {pid: group for pid, (_, group) in process_table().items() if group in groups}

    def close(self):
        self.closed.set()
        self.thread.join(timeout=5)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def prompts(session):
    return [m['prompt'] for m in session.get('timeline', []) if not m.get('compacted')]


def main():
    executable = shutil.which('opencode')
    assert executable, 'OpenCode must be installed'
    repo = Path(__file__).resolve().parents[2]
    urllib.request.install_opener(urllib.request.build_opener(urllib.request.ProxyHandler({})))
    with workspace() as temporary:
        root = Path(temporary).resolve()
        home, config, project = root/'home', root/'config'/'opencode', root/'project \'quoted" $literal'
        for directory in (home, config, project, root/'tmp'):
            directory.mkdir(parents=True)
        # The build deliberately uses the ambient Go cache, not the fixture HOME.
        binary = root/'slorp'
        subprocess.run(['go', 'build', '-o', str(binary), '.'], cwd=repo, check=True)
        env = {key: os.environ[key] for key in ('PATH', 'LANG', 'LC_ALL') if key in os.environ}
        env.update(HOME=str(home), OPENCODE_TEST_HOME=str(home), TMPDIR=str(root/'tmp'),
                   XDG_CONFIG_HOME=str(root/'config'), XDG_DATA_HOME=str(root/'data'),
                   XDG_STATE_HOME=str(root/'state'), XDG_CACHE_HOME=str(root/'cache'),
                   npm_config_cache=str(root/'npm-cache'), TERM='xterm-256color',
                   OPENCODE_DISABLE_EXTERNAL_SKILLS='true', OPENCODE_DISABLE_DEFAULT_PLUGINS='true',
                   OPENCODE_DISABLE_MODELS_FETCH='true', OPENCODE_DISABLE_UPDATE_CHECK='true',
                   GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull)
        version = subprocess.check_output([executable, '--version'], env=env, cwd=root, text=True).strip()
        assert version == '1.19.6', f'This smoke targets OpenCode 1.19.6, installed: {version}'
        subprocess.run(['git', 'init', '--quiet', str(project)], env=env, cwd=root, check=True)
        fixture = Fixture()
        trees = ProcessTrees()
        children = []
        session_id = None
        base = f'http://127.0.0.1:{port()}'
        log_path = root/'processes.log'
        log = log_path.open('w')

        def service(command, cwd=root):
            proc = subprocess.Popen(command, cwd=cwd, env=env, stdout=log, stderr=log, start_new_session=True)
            children.append(proc)
            trees.add(proc)
            return proc

        def state():
            return request(base, '/api/sessions')

        def live():
            return next((s for s in state()['sessions'] if s['id'] == session_id), None)

        def export():
            result = subprocess.run([executable, 'export', '--pure', session_id], cwd=project,
                                    env=env, text=True, capture_output=True, timeout=30)
            assert result.returncode == 0, result.stderr
            data = json.loads(result.stdout)
            (root/'native-export.json').write_text(json.dumps(data, indent=2))
            return data

        def native_turns(expected):
            data = export()
            assert data['info']['id'] == session_id, data['info']
            assert data['info']['title'] == 'Native lifecycle fixture', data['info']
            users = [m for m in data['messages'] if m['info']['role'] == 'user']
            texts = ['\n'.join(p['text'] for p in m['parts'] if p['type'] == 'text'
                               and not p.get('synthetic')) for m in users]
            assert texts == expected, ('native user turns', texts, expected)
            for message in users:
                info = message['info']
                assert info['agent'] == OPTIONS['agentName'], info
                assert info['model'] == {'providerID': 'mock', 'modelID': 'fixture', 'variant': 'high'}, info
                answers = [m for m in data['messages'] if m['info'].get('parentID') == info['id']
                           and m['info']['role'] == 'assistant']
                assert len(answers) == 1, ('native assistant count', answers)
                answer = answers[0]
                if any(p.get('text') == BLOCK for p in message['parts']):
                    assert answer['info'].get('error', {}).get('name') == 'MessageAbortedError', answer
                else:
                    assert answer['info']['time'].get('completed') and answer['info'].get('finish') == 'stop', answer
                    assert ''.join(p['text'] for p in answer['parts'] if p['type'] == 'text') == ANSWER, answer
            return [m['info']['id'] for m in users]

        def completed(expected):
            def ready():
                s = live()
                return s if s and s['lastPrompt'] == expected[-1] and s['lastReply'] == ANSWER \
                    and s['canReply'] and s['status'] == 'idle' and prompts(s) == expected \
                    and s['used']['in'] == 12 * len(expected) and s['used']['out'] == 3 * len(expected) else None
            s = wait_for(ready, seconds=45)
            assert s['effort'] == effort and s['role'] == role, s
            assert s['model'] == OPTIONS['model'] and s['reasoning'] == OPTIONS['variant'], s
            assert s['pluginRevision'] and s['canStop'], s
            native_turns(expected)
            for text in expected:
                calls = [r for r in fixture.captured() if text in json.dumps(r['body'].get('messages', [])[-1:])]
                assert len(calls) == 1, ('expected exactly one provider call for turn', text, len(calls))
                assert calls[0]['path'] == '/v1/chat/completions' and calls[0]['body']['model'] == 'fixture', calls
            return s

        def stop_session(expected):
            before = live()
            api(base, f'/api/sessions/{session_id}/stop', {}, expected=204)
            wait_for(lambda: live() is None)
            archived = api(base, f'/api/archive/{session_id}')
            assert archived['id'] == session_id and archived['effort'] == effort, archived
            assert prompts(archived) == expected, archived
            assert archived['startedAt'] == before['startedAt'], (before, archived)
            assert archived['used'] == before['used'], (before, archived)
            assert api(base, f'/api/sessions/{session_id}/resume-command') == command
            return archived

        def resume(prompt=None):
            body = {'requestId': str(uuid.uuid4())}
            if prompt is not None:
                body['prompt'] = prompt
            result = api(base, f'/api/archive/{session_id}/resume', body, expected=202)
            assert result['sessionId'] == session_id, result
            wait_for(lambda: (live() or {}).get('canReply'), seconds=45)

        try:
            unrelated = root/'unrelated-plugin'
            unrelated.mkdir()
            (unrelated/'package.json').write_text(json.dumps(
                {'name': 'unrelated-smoke-plugin', 'type': 'module', 'main': 'index.js'}))
            (unrelated/'index.js').write_text('export default async () => ({})\n')
            global_file = config/'opencode.jsonc'
            global_file.write_text('// Preserve fixture user config, comments, and plugin tuple.\n' + json.dumps({
                'username': 'lifecycle-smoke-user', 'autoupdate': False, 'share': 'disabled',
                'enabled_providers': ['mock'], 'model': 'mock/fixture', 'small_model': 'mock/fixture',
                'plugin': [[str(unrelated), {'marker': 'kept'}]],
                'provider': {'mock': {'name': 'Mock fixture', 'npm': '@ai-sdk/openai-compatible',
                    'options': {'apiKey': 'fixture-only-not-a-secret', 'includeUsage': True,
                                'baseURL': f'http://127.0.0.1:{fixture.server_port}/v1'},
                    'models': {'fixture': {'name': 'Fixture', 'limit': {'context': 32000, 'output': 1000},
                                           'variants': {'high': {}}}}}}}, indent=2))
            project_file = project/'opencode.json'
            project_file.write_text(json.dumps({'$schema': 'https://opencode.ai/config.json',
                'default_agent': 'review', 'agent': {
                'review': {'description': 'Fixture review agent', 'mode': 'primary', 'variant': 'high'}}}))
            sentinel = home/'unrelated-user-file'
            sentinel.write_text('User-owned fixture data must survive.\n')
            protected = {path: digest(path) for path in
                         (global_file, project_file, sentinel, unrelated/'package.json', unrelated/'index.js')}
            originals = {path: path.read_text() for path in protected}

            def unchanged():
                for path, original in protected.items():
                    assert path.exists() and digest(path) == original, (
                        f'User file changed: {path}', originals[path], path.read_text() if path.exists() else None)
                assert not state()['plugins']['opencode'].get('installed'), state()['plugins']
                for directory in (config, project/'.opencode', home/'.opencode'):
                    for path in directory.rglob('*.json*'):
                        content = path.read_text()
                        assert 'slorp-opencode-run-' not in content and 'opencode-slorp' not in content, path

            slorp = service([str(binary), '-addr', base.removeprefix('http://'),
                             '-data', str(root/'slorp-data'), '-claude', str(root/'no-claude'),
                             '-opencode', executable, '-llm', ''])
            wait_for(lambda: state().get('plugins', {}).get('opencode'), seconds=30)
            catalog = api(base, '/api/agents/opencode/launch-options?' + urllib.parse.urlencode({'dir': str(project)}))
            assert catalog['defaults'] == OPTIONS, catalog
            assert any(a['name'] == 'review' for a in catalog['agents']), catalog
            assert [m['id'] for m in catalog['models']] == ['mock/fixture'], catalog
            assert catalog['models'][0]['variants'] == ['high'], catalog
            assert not state()['sessions'] and not fixture.captured(), state()
            unchanged()
            print('PASS: native catalog resolves project agent/model/variant without activating sessions or rewriting config', flush=True)

            # A real, unrelated terminal must not acquire the runtime-only bridge.
            other_url = f'http://127.0.0.1:{port()}'
            seed = service([executable, 'serve', '--pure', '--hostname', '127.0.0.1',
                            '--port', other_url.rsplit(':', 1)[1]], cwd=project)
            wait_for(lambda: request(other_url, '/global/health'))
            other = request(other_url, '/session', {'title': 'Unrelated terminal root'}, timeout=120)
            stop(seed)
            term = terminal([executable, '--session', other['id']], project, env)
            children.append(term)
            trees.add(term)
            time.sleep(3)
            assert term.poll() is None and not state()['sessions'], state()
            unchanged()

            api(base, '/api/efforts', {'name': 'Lifecycle smoke', 'about': 'Isolated fixture'}, expected=204)
            api(base, '/api/kb/roles', {'name': 'Smoke reviewer', 'text': 'Fixture role'}, expected=204)
            knowledge = state()['knowledge']
            effort = next(e['id'] for e in state()['efforts'] if e['name'] == 'Lifecycle smoke')
            role = next(r['id'] for r in knowledge['roles'] if r['name'] == 'Smoke reviewer')
            expected = ['lifecycle-smoke: initial prompt before the first plugin tick']
            started = api(base, '/api/sessions', dict(OPTIONS, agent='opencode', dir=str(project),
                          prompt=expected[0], name='Native lifecycle fixture', effort=effort,
                          role=role, requestId=str(uuid.uuid4())), expected=202)
            session_id = started['sessionId']
            assert session_id.startswith('ses_'), started
            completed(expected)
            print(f'PASS: initial prompt and deterministic SSE response observed through Slorp ({session_id}); native fields and usage verified', flush=True)

            command = api(base, f'/api/sessions/{session_id}/resume-command')
            assert command['cwd'] == str(project), command
            assert shlex.split(command['command']) == ['cd', '--', str(project), '&&', executable, '--session', session_id], command
            quoted = ["'" + value.replace("'", "'\"'\"'") + "'" for value in (str(project), executable, session_id)]
            assert command['command'] == f'cd -- {quoted[0]} && {quoted[1]} --session {quoted[2]}', command
            expected.append('lifecycle-smoke: reply through Slorp')
            api(base, f'/api/sessions/{session_id}/reply', {'text': expected[-1]}, expected=202)
            completed(expected)
            print('PASS: Slorp reply reaches native provider exactly once with selected options', flush=True)
            stop_session(expected)
            print('PASS: Stop archives the same ID, effort, timeline, usage and shell-quoted resume command', flush=True)

            before_ids = native_turns(expected)
            calls = len(fixture.captured())
            resume()
            time.sleep(3)
            assert native_turns(expected) == before_ids and len(fixture.captured()) == calls
            assert live()['role'] == role and live()['effort'] == effort, live()
            expected.append('lifecycle-smoke: reply after promptless reopen')
            api(base, f'/api/sessions/{session_id}/reply', {'text': expected[-1]}, expected=202)
            completed(expected)
            print('PASS: promptless reopen adds no turn/provider call, retains KB role/effort and remains replyable', flush=True)
            stop_session(expected)

            expected.append('lifecycle-smoke: resume with one new prompt')
            resume(expected[-1])
            completed(expected)
            print('PASS: prompted resume appends exactly one native user turn and preserves agent/model/variant', flush=True)

            api(base, f'/api/sessions/{session_id}/reply', {'text': BLOCK}, expected=202)
            assert fixture.blocked.wait(30), 'Blocked turn never reached the fixture provider'
            expected.append(BLOCK)
            wait_for(lambda: prompts(live() or {}) == expected)
            stop_session(expected)
            fixture.release.set()
            time.sleep(3)
            assert live() is None, 'A late plugin event resurrected the stopped session'
            native_turns(expected)
            print('PASS: Stop aborts a blocked provider turn without late resurrection', flush=True)

            resume()
            unchanged()
            assert {s['id'] for s in state()['sessions']} == {session_id}, state()['sessions']
            assert term.poll() is None, 'Managed lifecycle stopped the unrelated terminal'
            wait_for(lambda: len(trees.remaining(slorp)) > 1)
            # Signal only Slorp: group-wide cleanup would conceal orphaned native children.
            slorp.send_signal(signal.SIGINT)
            slorp.wait(timeout=15)
            assert slorp.returncode == 0, slorp.returncode
            wait_for(lambda: not trees.remaining(slorp), seconds=10)
            assert term.poll() is None, 'Slorp shutdown stopped the unrelated terminal'
            assert not list((root/'tmp').glob('slorp-opencode-run-*')), 'Private runtime directories leaked'
            for path, original in protected.items():
                assert digest(path) == original, f'User file changed: {path}'
            print('PASS: graceful Slorp shutdown reaps its native children, removes private runtimes and preserves unrelated terminal/config', flush=True)
            print('SKIP: optional interactive tool-permission allow/deny scenario', flush=True)
            print(f'PASS: OpenCode {version}; {len(fixture.captured())} loopback fixture requests; no external model configured', flush=True)
        except BaseException:
            log.flush()
            print('\n=== Slorp / standalone OpenCode logs ===\n' + log_path.read_text(), flush=True)
            for proc in children:
                print(f'=== Owned process tree {proc.pid}: {trees.remaining(proc)} ===', flush=True)
            try:
                print('=== Last Slorp state ===\n' + json.dumps(state(), indent=2), flush=True)
            except (OSError, ValueError) as error:
                print('State unavailable:', error, flush=True)
            if session_id:
                try:
                    print('=== Native export ===\n' + json.dumps(export(), indent=2), flush=True)
                except (OSError, ValueError, AssertionError, subprocess.SubprocessError) as error:
                    print('Export unavailable:', error, flush=True)
            (root/'fixture-requests.json').write_text(json.dumps(fixture.captured(), indent=2))
            captured = []
            for record in fixture.captured():
                body = record['body']
                captured.append({'path': record['path'], 'model': body.get('model'),
                                 'stream': body.get('stream'), 'messages': [m for m in body.get('messages', [])
                                                                          if m['role'] != 'system']})
            print('=== Fixture requests (without system prompt/tool schemas) ===\n' + json.dumps(captured, indent=2), flush=True)
            for path in (root/'data').glob('opencode/log/*.log'):
                print(f'=== {path.name} ===\n{path.read_text(errors="replace")}', flush=True)
            raise
        finally:
            fixture.close()
            try:
                for proc in reversed(children):
                    stop(proc)
                    # A failed lifecycle assertion must not leave fixture processes behind.
                    for group in set(trees.remaining(proc).values()):
                        try:
                            os.killpg(group, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                    wait_for(lambda: not trees.remaining(proc), seconds=10)
            finally:
                trees.close()
                log.close()


if __name__ == '__main__':
    main()
