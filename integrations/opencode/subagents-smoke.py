"""Real V2 background subagents, nested reports, usage and restart persistence."""

import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import shutil
import subprocess
import threading
import time

from smoke import environment, model_config, native, port, request, server, stop, version, wait_for, workspace


PROMPTS = ['ROOT_FIXTURE: delegate the cache review.',
           'CHILD_FIXTURE: review the cache and delegate its nested entry. 界',
           'GRAND_FIXTURE: inspect the nested entry and return an exact report.']
REPORTS = ['Root fixture report.', 'Child final report.\nCache reviewed: 界.',
           'Grandchild final report.\nNested entry verified.']


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        assert self.path == '/v1/chat/completions', self.path
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        with self.server.lock:
            self.server.records.append(body)
        users = next(json.dumps(m.get('content'), ensure_ascii=False) for m in body['messages'] if m['role'] == 'user')
        level = 2 if 'GRAND_FIXTURE:' in users else 1 if 'CHILD_FIXTURE:' in users else 0
        delegated = any(m.get('tool_calls') for m in body['messages'] if m['role'] == 'assistant')
        if level == 2:
            self.server.blocked.set()
            if not self.server.release.wait(90):
                return
        if level < 2 and not delegated:
            delta = {'role': 'assistant', 'tool_calls': [{'index': 0,
                'id': f'call_fixture_{level}', 'type': 'function', 'function': {'name': 'subagent',
                'arguments': json.dumps({'agent': 'general', 'description': f'Fixture task {level}',
                                         'prompt': PROMPTS[level + 1]})}}]}
            finish = 'tool_calls'
        else:
            delta, finish = {'role': 'assistant', 'content': REPORTS[level]}, 'stop'
        try:
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            for chunk in (
                {'choices': [{'index': 0, 'delta': delta, 'finish_reason': None}]},
                {'choices': [{'index': 0, 'delta': {}, 'finish_reason': finish}]},
                {'choices': [], 'usage': {'prompt_tokens': 12, 'completion_tokens': 3, 'total_tokens': 15}},
            ):
                chunk.update(id='chatcmpl-subagent', object='chat.completion.chunk',
                             created=int(time.time()), model='fixture')
                self.wfile.write(('data: ' + json.dumps(chunk) + '\n\n').encode())
            self.wfile.write(b'data: [DONE]\n\n')
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass


def eventually(check, seconds=45):
    last = None
    def attempt():
        nonlocal last
        try:
            check()
            return True
        except (AssertionError, OSError, ValueError) as error:
            last = error
    try:
        wait_for(attempt, seconds)
    except AssertionError:
        raise AssertionError(f'{check.__name__}: {last}') from last


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--native-only', action='store_true')
    args = parser.parse_args()
    executable = shutil.which('opencode')
    assert executable
    repo = Path(__file__).resolve().parents[2]
    with workspace() as temporary:
        root = Path(temporary).resolve()
        env = environment(root)
        version(executable, root, env)
        fixture = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        fixture.daemon_threads = True
        fixture.records, fixture.lock = [], threading.Lock()
        fixture.release, fixture.blocked = threading.Event(), threading.Event()
        thread = threading.Thread(target=fixture.serve_forever, daemon=True)
        thread.start()
        config = model_config(f'http://127.0.0.1:{fixture.server_port}')
        config['experimental'] = {'subagent_depth': 3}
        config['permissions'] = [{'action': '*', 'resource': '*', 'effect': 'allow'}]
        (root/'config/opencode/opencode.json').write_text(json.dumps(config))
        base = f'http://127.0.0.1:{port()}'
        children = []
        log = open(root/'processes.log', 'w')
        binary = root/'slorp'
        command = [str(binary), '-addr', base.removeprefix('http://'), '-data', str(root/'slorp-data'),
                   '-claude', str(root/'missing'), '-opencode', executable, '-llm', '']
        def start_slorp(command=command):
            proc = subprocess.Popen(command, cwd=root, env=env, stdout=log, stderr=log, start_new_session=True)
            children.append(proc)
            wait_for(lambda: request(base, '/api/sessions'))
            return proc
        try:
            plugin = None
            if not args.native_only:
                subprocess.run(['go', 'build', '-o', str(binary), '.'], cwd=repo, check=True)
                slorp = start_slorp()
                request(base, '/api/plugins/opencode', {}, timeout=120)
                plugin = request(base, '/api/sessions')['plugins']['opencode']['path']
            with server(executable, root, env, plugin=plugin) as agent:
                parent = native(agent, '/api/session', {'title': 'Native subagent fixture',
                                                       'location': {'directory': str(root)}})
                sid = parent['id']
                native(agent, f'/api/session/{sid}/prompt', {'text': PROMPTS[0]})
                if not fixture.blocked.wait(45):
                    family = native(agent, '/api/session?limit=100')
                    diagnostics = {s['id']: native(agent, f'/api/experimental/session/{s["id"]}/export') for s in family}
                    (root/'native-failure.json').write_text(json.dumps(diagnostics, indent=2))
                    (root/'fixture-requests.json').write_text(json.dumps(fixture.records, indent=2))
                    raise AssertionError(f'Nested native subagent never reached the loopback provider: {root}')
                family = native(agent, '/api/session?limit=100')
                child = next(s for s in family if s.get('parentID') == sid)
                grand = next(s for s in family if s.get('parentID') == child['id'])
                ids = [sid, child['id'], grand['id']]
                assert len(family) == 3, family
                (root/'native-running-family.json').write_text(json.dumps(
                    {id: native(agent, f'/api/experimental/session/{id}/export') for id in ids}, indent=2))
                def messages(id):
                    return native(agent, f'/api/session/{id}/message?order=asc&limit=100')
                for index, id in enumerate(ids):
                    expected = PROMPTS[index] if index == 0 else 'You are a subagent spawned by another session.\n' + PROMPTS[index]
                    assert any(m['type'] == 'user' and m['text'] == expected for m in messages(id)), messages(id)
                print('PASS: real native subagent tool creates nested sessions with verbatim Unicode prompts', flush=True)
                def live():
                    rows = request(base, '/api/sessions')['sessions']
                    assert len(rows) == 1 and rows[0]['id'] == sid, rows
                    return rows[0]
                if not args.native_only:
                    def running():
                        rows = {s['id']: s for s in live().get('subagents', [])}
                        assert set(rows) == set(ids[1:]), rows
                        assert rows[grand['id']]['status'] == 'running' and not rows[grand['id']]['done'], rows
                        assert all(s['background'] for s in rows.values()), rows
                    eventually(running)
                    for _ in range(3):
                        time.sleep(2)
                        running()
                    print('PASS: completed background launch remains running while the grandchild provider is blocked', flush=True)
                fixture.release.set()
                def finished():
                    for id in ids:
                        info = native(agent, f'/api/session/{id}')
                        assert info.get('outcome') == 'succeeded' and info['time'].get('idle', 0) >= info['time'].get('busy', 0), info
                    for index, id in enumerate(ids[:-1]):
                        assert any(m['type'] == 'synthetic' and '<subagent' in m['text']
                                   and REPORTS[index + 1] in m['text'] for m in messages(id)), messages(id)
                eventually(finished)
                exports = {id: native(agent, f'/api/experimental/session/{id}/export') for id in ids}
                (root/'native-family.json').write_text(json.dumps(exports, indent=2))
                print('PASS: native background completion sends exact <subagent> reports and persists typed transcripts', flush=True)
                if args.native_only:
                    return
                def check_tree(session):
                    rows = {s['id']: s for s in session.get('subagents', [])}
                    assert set(rows) == set(ids[1:]), rows
                    for index, id in enumerate(ids[1:], 1):
                        row = rows[id]
                        assert row['done'] and row['status'] == 'completed' and row['depth'] == index, row
                        assert row.get('parentId', '') == ('' if index == 1 else child['id']), row
                        report = request(base, f'/api/sessions/{sid}/subagents/{id}')
                        assert report == {'prompt': PROMPTS[index], 'report': REPORTS[index], 'handed': True}, report
                        own = exports[id]['info']['tokens']
                        assert row['used']['in'] == own['input'] and row['used']['out'] == own['output'] + own['reasoning'], row
                        assert row['model'] == 'mock/fixture', row
                        assert PROMPTS[index] not in json.dumps(session, ensure_ascii=False), 'Report leaked into session state'
                    assert session['used']['in'] == sum(e['info']['tokens']['input'] for e in exports.values()), session
                    assert session['used']['out'] == sum(e['info']['tokens']['output'] + e['info']['tokens']['reasoning'] for e in exports.values()), session
                eventually(lambda: check_tree(live()))
                stop(slorp)
                slorp = start_slorp()
                eventually(lambda: check_tree(live()))
                print('PASS: own/inclusive usage, final reports and nested tree survive Slorp restart', flush=True)
            wait_for(lambda: not request(base, '/api/sessions')['sessions'], seconds=45)
            check_tree(request(base, f'/api/archive/{sid}'))
            stop(slorp)
            offline = command.copy()
            offline[offline.index('-opencode') + 1] = str(root/'missing-opencode')
            slorp = start_slorp(offline)
            eventually(lambda: check_tree(request(base, f'/api/archive/{sid}')))
            print('PASS: source retirement and offline restart preserve archive usage and reports', flush=True)
        except BaseException:
            (root/'fixture-requests.json').write_text(json.dumps(fixture.records, indent=2))
            if not args.native_only:
                try:
                    state = request(base, '/api/sessions')
                    (root/'slorp-state.json').write_text(json.dumps(state, indent=2))
                    print('=== Slorp sessions ===\n' + json.dumps(state['sessions'], indent=2), flush=True)
                except (OSError, ValueError, AssertionError) as error:
                    print('State unavailable:', error, flush=True)
            raise
        finally:
            fixture.release.set()
            for proc in reversed(children):
                stop(proc)
            fixture.shutdown()
            fixture.server_close()
            thread.join(timeout=5)
            log.close()


if __name__ == '__main__':
    main()
