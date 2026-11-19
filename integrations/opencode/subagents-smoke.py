"""Exercise native OpenCode subagents and Slorp persistence without model calls."""

from datetime import datetime
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

from smoke import port, request, stop, wait_for, workspace


ROOT, CHILD, GRAND = ('ses_smokeroot', 'ses_smokechild', 'ses_smokegrand')
MODEL = {'providerID': 'test', 'modelID': 'fixture'}
PROMPTS = {CHILD: 'Review the synthetic cache.\nKeep the original task verbatim: 界.',
           GRAND: 'Inspect the nested synthetic entry.\nReturn an exact report.'}
FALLBACKS = {CHILD: 'Child progress, not the handed report.', GRAND: 'Grandchild progress, not final.'}
REPORTS = {CHILD: 'Child final report.\nSynthetic cache reviewed: 界.',
           GRAND: 'Grandchild final report.\nSynthetic entry verified.'}


def fixture(directory, version):
    start = int(time.time() * 1000) - 60000
    docs = {}
    for i, sid in enumerate((ROOT, CHILD, GRAND)):
        created = start + i * 3000
        user, assistant = f'msg_smoke{i}user', f'msg_smoke{i}assistant'
        info = {'id': sid, 'slug': f'smoke-{i}', 'projectID': 'global', 'directory': str(directory),
                'title': f'Synthetic subagent smoke {i}', 'version': version,
                'time': {'created': created, 'updated': created + 2500}}
        if i:
            info['parentID'] = (ROOT, CHILD)[i - 1]
        messages = [
            {'info': {'id': user, 'sessionID': sid, 'role': 'user', 'agent': 'build',
                      'model': MODEL, 'time': {'created': created + 1000}},
             'parts': [{'type': 'text', 'text': f'Synthetic session {i} user text.'}]},
            {'info': {'id': assistant, 'sessionID': sid, 'role': 'assistant', 'parentID': user,
                      'agent': 'build', 'mode': 'build', **MODEL, 'variant': f'fixture-{i}',
                      'path': {'cwd': str(directory), 'root': str(directory)},
                      'time': {'created': created + 2000, 'completed': created + 2500},
                      'finish': 'stop', 'cost': (i + 1) / 8,
                      'tokens': {'input': 100 * (i + 1), 'output': 10 * (i + 1),
                                 'reasoning': i + 1, 'cache': {'read': 20 * (i + 1), 'write': 2 * (i + 1)}}},
             'parts': [{'type': 'text', 'text': FALLBACKS.get(sid, 'Synthetic root progress.')}]},
        ]
        if i < 2:
            child = (CHILD, GRAND)[i]
            messages[1]['parts'].append({
                'type': 'tool', 'tool': 'task', 'callID': f'call_smoke{i}',
                'state': {'status': 'running', 'title': f'Synthetic task {i}',
                          'input': {'subagent_type': 'general', 'description': f'Synthetic task {i}',
                                    'prompt': PROMPTS[child], 'background': i == 1},
                          'metadata': {'sessionId': child, 'parentSessionId': sid, 'background': i == 1},
                          'time': {'start': created + 3000}},
            })
        for n, message in enumerate(messages):
            for j, part in enumerate(message['parts']):
                part.update(id=f'prt_smoke{i}{n}{j}', sessionID=sid, messageID=message['info']['id'])
        docs[sid] = {'info': info, 'messages': messages}
    return docs


def patch_part(base, part):
    path = f"/session/{part['sessionID']}/message/{part['messageID']}/part/{part['id']}"
    req = urllib.request.Request(base + path, data=json.dumps(part).encode(), method='PATCH',
                                 headers={'Content-Type': 'application/json'})
    try:
        with urllib.request.urlopen(req, timeout=15) as response:
            result = json.load(response)
    except urllib.error.HTTPError as error:
        raise AssertionError(f'PATCH {path}: HTTP {error.code}: {error.read().decode()}') from error
    assert result == part, ('native PartUpdate response', result, part)
    native = request(base, f"/session/{part['sessionID']}/message/{part['messageID']}")
    assert next(p for p in native['parts'] if p['id'] == part['id']) == part, native


def eventually(check, seconds=30):
    last = None

    def attempt():
        nonlocal last
        try:
            check()
            return True
        except (AssertionError, OSError, ValueError) as error:
            last = error
            return False

    try:
        wait_for(attempt, seconds=seconds)
    except AssertionError:
        raise AssertionError(f'{check.__name__}: {last}') from last


def milliseconds(value):
    return round(datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp() * 1000)


def main():
    executable = shutil.which('opencode')
    assert executable, 'OpenCode must be installed'
    repo = Path(__file__).resolve().parents[2]
    with workspace() as temporary:
        root = Path(temporary)
        # An allowlist avoids inheriting credentials, alternate configs or provider endpoints.
        env = {key: os.environ[key] for key in ('PATH', 'LANG', 'LC_ALL') if key in os.environ}
        env.update(HOME=str(root/'home'), OPENCODE_TEST_HOME=str(root/'home'), TMPDIR=tempfile.gettempdir(),
                   XDG_CONFIG_HOME=str(root/'config'), XDG_DATA_HOME=str(root/'data'),
                   XDG_STATE_HOME=str(root/'state'), XDG_CACHE_HOME=str(root/'cache'),
                   npm_config_cache=str(root/'npm-cache'), BUN_INSTALL_CACHE_DIR=str(root/'bun-cache'),
                   OPENCODE_DISABLE_PROJECT_CONFIG='1', OPENCODE_DISABLE_EXTERNAL_SKILLS='1',
                   OPENCODE_DISABLE_DEFAULT_PLUGINS='1', OPENCODE_DISABLE_MODELS_FETCH='1',
                   OPENCODE_DISABLE_AUTOUPDATE='1', GOTOOLCHAIN='local')
        (root/'home').mkdir()
        config = root/'config'/'opencode'
        config.mkdir(parents=True)
        (config/'opencode.json').write_text(json.dumps({'username': 'synthetic-smoke', 'enabled_providers': []}))
        processes = []
        log = open(root/'processes.log', 'w')
        slorp_port, agent_port = port(), port()
        slorp_url, agent_url = f'http://127.0.0.1:{slorp_port}', f'http://127.0.0.1:{agent_port}'
        binary = root/'slorp'

        def run(command, cwd=root, timeout=120):
            result = subprocess.run(command, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout)
            log.write(f'COMMAND {command!r}\n{result.stdout}\n{result.stderr}\n')
            log.flush()
            assert result.returncode == 0, (command, result.returncode, result.stdout, result.stderr)
            return result.stdout

        def service(command):
            proc = subprocess.Popen(command, cwd=root, env=env, stdout=log, stderr=log, start_new_session=True)
            processes.append(proc)
            return proc

        slorp_command = [str(binary), '-addr', f'127.0.0.1:{slorp_port}', '-data', str(root/'slorp-data'),
                         '-claude', str(root/'no-claude'), '-opencode', executable, '-llm', '']

        def start_agent():
            proc = service([executable, 'serve', '--hostname', '127.0.0.1', '--port', str(agent_port), '--print-logs'])
            wait_for(lambda: request(agent_url, '/global/health'))
            request(agent_url, '/path', timeout=120)
            return proc

        def prompt(sid, text, synthetic=False):
            result = request(agent_url, f'/session/{sid}/message', {
                'noReply': True, 'agent': 'build', 'model': MODEL,
                'variant': f'fixture-{(ROOT, CHILD, GRAND).index(sid)}',
                'parts': [{'type': 'text', 'text': text, 'synthetic': synthetic}],
            }, timeout=30)
            assert result['info']['role'] == 'user' and result['info']['sessionID'] == sid, result
            assert any(p.get('text') == text and bool(p.get('synthetic')) == synthetic for p in result['parts']), result
            return result

        def listed():
            return request(slorp_url, '/api/sessions')['sessions']

        def live():
            sessions = listed()
            assert len(sessions) == 1 and sessions[0]['id'] == ROOT, sessions
            return sessions[0]

        expected_reports = {sid: {'prompt': PROMPTS[sid], 'report': FALLBACKS[sid], 'handed': False}
                            for sid in (CHILD, GRAND)}
        statuses = {CHILD: 'running', GRAND: 'running'}
        completed_at = {}

        def check_tree(session):
            children = {a['id']: a for a in session.get('subagents', [])}
            assert len(session.get('subagents', [])) == 2 and set(children) == {CHILD, GRAND}, session
            assert session['used'] == {'in': 600, 'out': 66, 'cacheRead': 120, 'cacheWrite': 12}, session
            assert math.isclose(session['costUsd'], .75), session
            for i, sid in enumerate((CHILD, GRAND), 1):
                child = children[sid]
                assert child.get('parentId', '') == ('' if sid == CHILD else CHILD) and child['depth'] == i, child
                assert child['status'] == statuses[sid] and child['done'] == (statuses[sid] == 'completed'), child
                assert bool(child.get('background')) == (sid == GRAND), child
                assert child['type'] == 'general' and child['description'] == f'Synthetic task {i - 1}', child
                assert child['model'] == 'test/fixture' and child['effort'] == f'fixture-{i}', child
                assert child['tools'] == (1 if sid == CHILD else 0), child
                assert child['used'] == {'in': 100 * (i + 1), 'out': 11 * (i + 1),
                                         'cacheRead': 20 * (i + 1), 'cacheWrite': 2 * (i + 1)}, child
                assert math.isclose(child['costUsd'], (i + 1) / 8), child
                assert milliseconds(child['startedAt']) == docs[sid]['info']['time']['created'], child
                active = milliseconds(child['activeAt'])
                assert active >= docs[sid]['messages'][1]['info']['time']['completed'], child
                if sid in completed_at:
                    assert completed_at[sid] <= active <= int(time.time() * 1000), (child, completed_at[sid])
                assert child.get('reportRevision', 0) > 0, child
                report = request(slorp_url, f'/api/sessions/{ROOT}/subagents/{sid}')
                assert report == expected_reports[sid], (sid, report, expected_reports[sid])
                state = json.dumps(session, ensure_ascii=False)
                for text in (PROMPTS[sid], expected_reports[sid]['report']):
                    assert json.dumps(text, ensure_ascii=False) not in state, 'Report text leaked into session state'

        def check_live():
            check_tree(live())

        def check_native():
            for sid, doc in docs.items():
                info = request(agent_url, f'/session/{sid}')
                assert info['id'] == sid and info.get('parentID') == doc['info'].get('parentID'), info
                messages = request(agent_url, f'/session/{sid}/message')
                original = {m['info']['id']: m for m in messages}
                for expected in doc['messages']:
                    actual = original[expected['info']['id']]
                    assert actual == expected, (actual, expected)
                assert sum(m['info']['role'] == 'assistant' for m in messages) == 1, messages
                (root/f'native-{sid}.json').write_text(json.dumps({'info': info, 'messages': messages}, indent=2))
            assert {s['id'] for s in request(agent_url, f'/session/{ROOT}/children')} == {CHILD}
            assert {s['id'] for s in request(agent_url, f'/session/{CHILD}/children')} == {GRAND}

        try:
            version = run([executable, '--version']).strip()
            subprocess.run(['go', 'build', '-o', str(binary), '.'], cwd=repo,
                           stdout=log, stderr=log, check=True, timeout=300)
            slorp = service(slorp_command)
            wait_for(lambda: request(slorp_url, '/api/sessions').get('plugins', {}).get('opencode'))
            request(slorp_url, '/api/plugins/opencode', {})
            installed = request(slorp_url, '/api/sessions')['plugins']['opencode']
            assert installed['installed'] and installed['source'] == 'managed' and not installed.get('needsSetup'), installed
            assert Path(installed['path']).is_relative_to(root), installed
            print(f'PASS: isolated standalone Slorp installs bundled plugin (OpenCode {version})', flush=True)

            docs = fixture(root, version)
            for sid, doc in docs.items():
                source = root/f'{sid}.json'
                source.write_text(json.dumps(doc))
                imported = run([executable, 'import', '--pure', str(source)])
                assert f'Imported session: {sid}' in imported, imported
                exported = json.loads(run([executable, 'export', '--pure', sid]))
                assert exported['info']['id'] == sid, exported
                assert exported['info'].get('parentID') == doc['info'].get('parentID'), exported
                assert exported['messages'] == doc['messages'], exported
            agent = start_agent()
            check_native()
            assert listed() == [], 'Import alone must not create live Slorp cards'
            print('PASS: native import/export and HTTP preserve IDs, parent links, parts and usage', flush=True)

            prompt(ROOT, 'Discover the synthetic root without a model call.')
            eventually(check_live)
            assert live()['pluginRevision'] == installed['revision'], live()
            print('PASS: chat.message discovers one root with two nested children; own/inclusive usage, reports, timing/model', flush=True)

            background = docs[CHILD]['messages'][1]['parts'][1]
            background['state'].update(status='completed', output=f'<task id="{GRAND}" state="running">\n<task_result>\nBackground launch only.\n</task_result>\n</task>')
            background['state']['time']['end'] = int(time.time() * 1000)
            revision = next(a['reportRevision'] for a in live()['subagents'] if a['id'] == GRAND)
            patch_part(agent_url, background)

            def check_background():
                check_live()
                grand = next(a for a in live()['subagents'] if a['id'] == GRAND)
                assert grand['reportRevision'] > revision, grand

            eventually(check_background)
            # Observe several real poll cycles, not merely the state before the event arrives.
            for _ in range(3):
                time.sleep(2)
                check_background()
            print('PASS: native completed task with state="running" stays running, unhanded and non-final', flush=True)

            foreground = docs[ROOT]['messages'][1]['parts'][1]
            completed_at[CHILD] = int(time.time() * 1000)
            foreground['state'].update(status='completed', output=f'task_id: {CHILD}\n\n<task_result>\n{REPORTS[CHILD]}\n</task_result>')
            foreground['state']['time']['end'] = completed_at[CHILD]
            patch_part(agent_url, foreground)
            statuses[CHILD] = 'completed'
            expected_reports[CHILD].update(report=REPORTS[CHILD], handed=True)
            eventually(check_live)
            print('PASS: native PartUpdate running→completed replaces fallback with exact handed report', flush=True)

            notification = prompt(CHILD, f'<task id="{GRAND}" state="completed">\n<task_result>\n{REPORTS[GRAND]}\n</task_result>\n</task>', synthetic=True)
            completed_at[GRAND] = notification['info']['time']['created']
            # A synthetic notification also advances its parent's native activity timestamp.
            completed_at[CHILD] = completed_at[GRAND]
            statuses[GRAND] = 'completed'
            expected_reports[GRAND].update(report=REPORTS[GRAND], handed=True)
            eventually(check_live)
            check_native()
            print('PASS: native synthetic user notification finalizes background grandchild; no assistant/model turns added', flush=True)

            stop(slorp)
            slorp = service(slorp_command)
            eventually(check_live, seconds=40)
            print('PASS: live plugin replays nested records/reports after Slorp restart', flush=True)

            stop(agent)
            agent = start_agent()
            check_native()
            prompt(ROOT, 'Rediscover the persisted synthetic tree without a model call.')
            eventually(check_live)
            check_native()
            print('PASS: OpenCode restart rehydrates the native tree and completed reports', flush=True)

            stop(agent)
            wait_for(lambda: not listed(), seconds=45)
            check_tree(request(slorp_url, f'/api/archive/{ROOT}'))
            stop(slorp)
            offline_command = slorp_command.copy()
            offline_command[offline_command.index('-opencode') + 1] = str(root/'no-opencode')
            slorp = service(offline_command)

            def check_archive():
                assert listed() == [], listed()
                check_tree(request(slorp_url, f'/api/archive/{ROOT}'))

            eventually(check_archive)
            (root/'archive.json').write_text(json.dumps(request(slorp_url, f'/api/archive/{ROOT}'), indent=2))
            for sid in (CHILD, GRAND):
                report = request(slorp_url, f'/api/sessions/{ROOT}/subagents/{sid}')
                (root/f'report-{sid}.json').write_text(json.dumps(report, indent=2))
            print('PASS: agent retirement and offline Slorp restart retain archive, usage and both reports', flush=True)
        except Exception:
            log.flush()
            diagnostics = [('slorp-state', slorp_url, '/api/sessions'),
                           ('slorp-archive', slorp_url, f'/api/archive/{ROOT}')]
            diagnostics += [(f'native-failure-{sid}', agent_url, f'/session/{sid}/message') for sid in (ROOT, CHILD, GRAND)]
            diagnostics += [(f'report-{sid}', slorp_url, f'/api/sessions/{ROOT}/subagents/{sid}') for sid in (CHILD, GRAND)]
            for name, base, path in diagnostics:
                try:
                    (root/f'{name}.json').write_text(json.dumps(request(base, path), indent=2))
                except (OSError, ValueError):
                    pass  # The failed service may not be reachable; process logs remain available.
            print('FAIL: native subagent smoke; set SLORP_SMOKE_KEEP=1 to retain diagnostics', flush=True)
            print('Diagnostics:', root, flush=True)
            raise
        finally:
            for proc in reversed(processes):
                stop(proc)
            log.close()


if __name__ == '__main__':
    main()
