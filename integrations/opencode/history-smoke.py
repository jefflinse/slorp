"""Verify native OpenCode import/export and Slorp backfill with a synthetic transcript."""

import json
import argparse
from datetime import datetime, timezone
import os
from pathlib import Path
import re
import shutil
import subprocess

from smoke import port, request, stop, wait_for, workspace


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--overflow', action='store_true', help='verify native automatic overflow-replay history')
    parser.add_argument('--replay-last', action='store_true', help='end at the replay answer and reconcile an older bridge snapshot')
    args = parser.parse_args()
    executable = shutil.which('opencode')
    assert executable, 'OpenCode must be installed'
    repo = Path(__file__).resolve().parents[2]
    with workspace() as temporary:
        root = Path(temporary)
        (root/'home').mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith(('OPENCODE_', 'CMUX_', 'SLORP_'))}
        env.update(HOME=str(root/'home'), OPENCODE_TEST_HOME=str(root/'home'), npm_config_cache=str(root/'npm-cache'),
                   XDG_CONFIG_HOME=str(root/'config'), XDG_DATA_HOME=str(root/'data'), XDG_STATE_HOME=str(root/'state'),
                   XDG_CACHE_HOME=str(root/'cache'), OPENCODE_DISABLE_PROJECT_CONFIG='1',
                   OPENCODE_DISABLE_EXTERNAL_SKILLS='1', OPENCODE_DISABLE_MODELS_FETCH='1')
        doc = json.loads((repo/'internal/opencode/testdata'/('overflow.json' if args.overflow else 'history.json')).read_text())
        sid = doc['info']['id']
        doc['info']['directory'] = str(root)
        doc['messages'] = [m for m in doc['messages'] if m['info']['sessionID'] == sid]
        if args.overflow:
            doc['messages'][1]['parts'].append({'type': 'tool', 'tool': 'bash', 'state': {'status': 'completed', 'input': {}}})
        if args.replay_last:
            assert args.overflow, '--replay-last requires --overflow'
            doc['messages'] = [m for m in doc['messages'] if m['info']['id'] not in ('msg_repeat', 'msg_repeatanswer')]
        for n, m in enumerate(doc['messages']):
            info = m['info']
            info['agent'] = 'build'
            if info['role'] == 'user':
                info['model'] = {'providerID': 'test', 'modelID': 'fixture'}
            else:
                info.update(mode='build', providerID='test', modelID='fixture', path={'cwd': str(root), 'root': str(root)},
                            cost=0, tokens={'input': 0, 'output': 0, 'reasoning': 0, 'cache': {'read': 0, 'write': 0}})
            for j, p in enumerate(m['parts']):
                p.setdefault('id', f'prt_fixture{n:03d}{j:03d}')
                p.update(sessionID=sid, messageID=info['id'])
                if p['type'] == 'reasoning':
                    p['time'] = {'start': info['time']['created'], 'end': info['time']['completed']}
                if p['type'] == 'tool':
                    p['callID'] = 'call_fixture'
                    p['state'].update(title='inspect', metadata={}, time={'start': info['time']['created'], 'end': info['time']['completed']})
                    p['state']['output'] = 'Ignored tool output. ' * 10000
        source = root/'source.json'
        source.write_text(json.dumps(doc))
        imported = subprocess.run([executable, 'import', '--pure', str(source)], cwd=root, env=env, capture_output=True, text=True, timeout=60)
        assert imported.returncode == 0, imported.stdout + imported.stderr
        found = re.search(r'Imported session:\s*(ses_[\w-]+)', imported.stdout)
        assert found, imported.stdout
        sid = found.group(1)
        with open(root/'export.json', 'wb') as output:
            subprocess.run([executable, 'export', '--pure', sid], cwd=root, env=env, stdout=output, stderr=subprocess.PIPE, check=True, timeout=60)
        native = json.loads((root/'export.json').read_text())
        assert len((root/'export.json').read_bytes()) > 65536
        compact = next(p for m in native['messages'] for p in m['parts'] if p['type'] == 'compaction')
        compacted_at = next(m['info']['time']['completed'] for m in native['messages'] if m['info'].get('summary') is True)
        latest_user = max((m for m in native['messages'] if m['info']['role'] == 'user' and any(p['type'] == 'text' and not p.get('synthetic') for p in m['parts'])), key=lambda m: m['info']['time']['created'])
        latest_prompt_at = latest_user['info']['time']['created']
        latest_prompt = '\n\n'.join(p['text'] for p in latest_user['parts'] if p['type'] == 'text' and not p.get('synthetic') and not p.get('ignored'))
        print('PASS: native import/export preserves the transcript, including a large tool result', flush=True)

        binary = root/'slorp'
        subprocess.run(['go', 'build', '-o', str(binary), '.'], cwd=repo, check=True)
        address = f'127.0.0.1:{port()}'
        base = 'http://' + address
        with open(root/'slorp.log', 'w') as log:
            service = subprocess.Popen([str(binary), '-addr', address, '-data', str(root/'slorp-data'), '-claude', str(root/'missing'),
                                        '-opencode', executable, '-llm', ''], cwd=root, env=env, stdout=log, stderr=log, start_new_session=True)
            try:
                wait_for(lambda: request(base, '/api/sessions'))
                snapshot = {'agent': 'opencode', 'sessionId': sid, 'snapshot': {
                    'status': 'idle', 'lastPrompt': latest_prompt, 'promptAt': latest_prompt_at,
                    'compacted': {'id': compact['id'], 'at': datetime.fromtimestamp((compacted_at+1)/1000, timezone.utc).isoformat(timespec='milliseconds'), 'auto': True},
                }}
                request(base, '/api/bridge/poll', snapshot)
                assert request(base, '/api/sessions')['capabilities']['opencode']['history']
                for _ in range(2):
                    response = request(base, f'/api/sessions/{sid}/backfill', {})
                    assert response == {'exchanges': 3 if args.replay_last else 4, 'queued': 3 if args.overflow and not args.replay_last else 2}, response
                    request(base, '/api/bridge/poll', snapshot)
                    session = request(base, '/api/sessions')['sessions'][0]
                    timeline = session['timeline']
                    assert len(timeline) == (3 if args.replay_last else 4), timeline
                    prompts = ['Inspect the cache', 'Apply the fix'] if args.replay_last else ['Inspect the cache', 'Apply the fix', 'Apply the fix']
                    assert [m.get('prompt') for m in timeline if not m.get('compacted')] == prompts, timeline
                    assert sum(bool(m.get('compacted')) for m in timeline) == 1, timeline
                print('PASS: live API backfills and queues answered exchanges without duplicate prompts, replay turns or compactions', flush=True)
            finally:
                stop(service)


if __name__ == '__main__':
    main()
