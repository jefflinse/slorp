"""Native V2 transcript transfer, checkpoint compaction, and Slorp backfill."""

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import shutil
import subprocess
import time
import uuid
import urllib.parse

from smoke import environment, native, port, request, server, stop, version, wait_for, workspace


def transcript(info, auto_compaction=False, single_followup=False):
    now = int(time.time() * 1000) - 60000
    messages = []
    model = {'providerID': 'mock', 'id': 'fixture'}
    tokens = {'input': 0, 'output': 0, 'reasoning': 0, 'cache': {'read': 0, 'write': 0}}

    def turn(prompt, answer):
        at = now + len(messages) * 1000
        messages.append({'id': 'msg_' + uuid.uuid4().hex, 'type': 'user',
                         'time': {'created': at}, 'text': prompt})
        messages.append({'id': 'msg_' + uuid.uuid4().hex, 'type': 'assistant',
                         'time': {'created': at + 100, 'completed': at + 500},
                         'agent': 'build', 'model': model, 'finish': 'stop', 'tokens': tokens, 'cost': 0,
                         'content': [{'type': 'text', 'text': answer}]})

    turn('Inspect the cache', 'The cache needs a fix.')
    messages[-1]['content'].append({'type': 'tool', 'id': 'call_fixture', 'name': 'shell',
        'time': {'created': now + 101, 'completed': now + 400},
        'state': {'status': 'completed', 'input': {'command': 'echo fixture'},
                  'content': [{'type': 'text', 'text': 'Ignored tool output. ' * 10000}]}})
    compact = {'id': 'msg_' + uuid.uuid4().hex, 'type': 'compaction', 'status': 'completed',
               'reason': 'auto' if auto_compaction else 'manual', 'time': {'created': now + 2000},
               'summary': 'The cache was inspected; apply the fix.', 'model': model}
    messages.append(compact)
    if auto_compaction:
        messages.append({'id': 'msg_' + uuid.uuid4().hex, 'type': 'synthetic',
                         'time': {'created': now + 2500}, 'text': 'Continue after compaction.'})
    turn('Apply the fix', 'The fix is applied.')
    if not single_followup:
        turn('Apply the fix', 'Confirmed the repeated request.')
    info = dict(info, id='ses_' + uuid.uuid4().hex, title='V2 history fixture',
                time={'created': now, 'updated': now + 10000})
    return {'info': info, 'messages': messages}, compact


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--auto-compaction', action='store_true', help='automatic V2 checkpoint plus synthetic continuation')
    parser.add_argument('--single-followup', action='store_true', help='one post-checkpoint turn instead of repeated user prompts')
    parser.add_argument('--revert', action='store_true', help='stage a native revert before the last repeated user turn')
    parser.add_argument('--native-only', action='store_true', help='verify native transfer without building Slorp')
    args = parser.parse_args()
    assert not (args.revert and args.single_followup), '--revert requires the repeated final turn'
    executable = shutil.which('opencode')
    assert executable
    repo = Path(__file__).resolve().parents[2]
    with workspace() as temporary:
        root = Path(temporary).resolve()
        env = environment(root)
        version(executable, root, env)
        with server(executable, root, env) as agent:
            seed = native(agent, '/api/session', {'location': {'directory': str(root)}})
            doc, compact = transcript(seed, args.auto_compaction, args.single_followup)
            imported = native(agent, '/api/experimental/session/import', doc)
            sid = imported['id']
            assert sid == doc['info']['id'], imported
            exported = native(agent, f'/api/experimental/session/{sid}/export')
            (root/'export.json').write_text(json.dumps(exported, indent=2))
            assert len((root/'export.json').read_bytes()) > 65536
            assert exported['messages'] == doc['messages'], exported
            messages = native(agent, f'/api/session/{sid}/message?order=asc&limit=100')
            assert messages == doc['messages'], messages
            paginated, cursors = [], set()
            query = {'order': 'asc', 'limit': 2}
            while True:
                page = native(agent, f'/api/session/{sid}/message?' + urllib.parse.urlencode(query), unwrap=False)
                paginated.extend(page['data'])
                cursor = page['cursor']['next']
                if not page['data'] or not cursor:
                    break
                assert cursor not in cursors, page['cursor']
                cursors.add(cursor)
                query = {'limit': 2, 'cursor': cursor}
            assert paginated == messages, paginated
            print('PASS: V2 native import/export and message API preserve large tool output, compaction, synthetic and repeated turns', flush=True)
            if args.revert:
                boundary = [m for m in messages if m['type'] == 'user'][-1]
                staged = native(agent, f'/api/session/{sid}/revert/stage',
                                {'messageID': boundary['id'], 'files': False})
                assert staged['messageID'] == boundary['id'], staged
                exported = native(agent, f'/api/experimental/session/{sid}/export')
                (root/'reverted-export.json').write_text(json.dumps(exported, indent=2))
                assert exported['info']['revert']['messageID'] == boundary['id'], exported['info']
                assert exported['messages'] == doc['messages'], 'Staged revert must retain the native transcript'
                messages = messages[:messages.index(boundary)]
                print('PASS: real native staged revert retains history with the expected export boundary', flush=True)
            if args.native_only:
                return
            binary = root/'slorp'
            subprocess.run(['go', 'build', '-o', str(binary), '.'], cwd=repo, check=True)
            base = f'http://127.0.0.1:{port()}'
            with open(root/'slorp.log', 'w') as log:
                proc = subprocess.Popen([str(binary), '-addr', base.removeprefix('http://'),
                    '-data', str(root/'slorp-data'), '-claude', str(root/'missing'),
                    '-opencode', executable, '-llm', ''], cwd=root, env=env,
                    stdout=log, stderr=log, start_new_session=True)
                try:
                    wait_for(lambda: request(base, '/api/sessions'))
                    latest = [m for m in messages if m['type'] == 'user'][-1]
                    snapshot = {'agent': 'opencode', 'sessionId': sid, 'snapshot': {
                        'status': 'idle', 'lastPrompt': latest['text'], 'promptAt': latest['time']['created'],
                        'compacted': {'id': compact['id'], 'at': datetime.fromtimestamp(
                            compact['time']['created']/1000, timezone.utc).isoformat(timespec='milliseconds'),
                             'auto': args.auto_compaction}}}
                    request(base, '/api/bridge/poll', snapshot)
                    assert request(base, '/api/sessions')['capabilities']['opencode']['history']
                    expected = ['Inspect the cache', 'Apply the fix']
                    if not args.single_followup and not args.revert:
                        expected.append('Apply the fix')
                    for _ in range(2):
                        response = request(base, f'/api/sessions/{sid}/backfill', {})
                        assert response['exchanges'] == len(expected) + 1, response
                        request(base, '/api/bridge/poll', snapshot)
                        session = next(s for s in request(base, '/api/sessions')['sessions'] if s['id'] == sid)
                        timeline = session['timeline']
                        assert len(timeline) == len(expected) + 1, timeline
                        assert [m.get('prompt') for m in timeline if not m.get('compacted')] == expected, timeline
                        assert sum(bool(m.get('compacted')) for m in timeline) == 1, timeline
                    print('PASS: native V2 history backfill is idempotent and preserves repeated prompts and one checkpoint', flush=True)
                finally:
                    stop(proc)


if __name__ == '__main__':
    main()
