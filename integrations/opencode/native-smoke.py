"""OpenCode 2.3.2 native contract gate, independent of the Slorp build."""

import importlib.util
import argparse
import json
from pathlib import Path
import shutil
import subprocess
import urllib.parse

from smoke import environment, model_config, native, port, request, server, stop, terminal, version, wait_for, workspace


DENIAL_FEEDBACK = 'Please revise: 界 café 🧪\nKeep & + ? # = % / \\ and "quotes" intact.'


def lifecycle():
    spec = importlib.util.spec_from_file_location('lifecycle_smoke', Path(__file__).with_name('lifecycle-smoke.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def permissions_and_forms(base, sid, bridge=None):
    path = f'/api/session/{sid}'
    def answer(body, question):
        def pending():
            row = next((s for s in request(bridge, '/api/sessions')['sessions'] if s['id'] == sid), {})
            item = row.get('pending')
            return item if item and item.get('answerable') and bool(item.get('question')) == question else None
        item = wait_for(pending)
        request(bridge, '/api/permissions/' + item['id'], body)
    for decision in ('once', 'reject'):
        result = native(base, path + '/permission', {'action': 'shell', 'resources': ['echo fixture']})
        assert result['effect'] == 'ask', result
        pending = native(base, path + '/permission')
        assert any(p['id'] == result['id'] for p in pending), pending
        if bridge:
            answer({'behavior': 'allow' if decision == 'once' else 'deny'}, False)
        else:
            native(base, path + f'/permission/{result["id"]}/reply', {'decision': decision})
        wait_for(lambda: native(base, path + '/permission') == [])
    for cancel in (False, True):
        form = native(base, path + '/form', {'title': 'Fixture question', 'fields': [
            {'type': 'string', 'key': 'answer', 'title': 'Answer', 'required': True}]})
        assert any(f['id'] == form['id'] for f in native(base, path + '/form'))
        form_path = path + '/form/' + form['id']
        if bridge:
            answer({'behavior': 'deny', 'message': DENIAL_FEEDBACK} if cancel else
                   {'selections': [['Fixture answer']]}, True)
        elif cancel:
            native(base, form_path + '?' + urllib.parse.urlencode({'message': DENIAL_FEEDBACK}), method='DELETE')
        else:
            native(base, form_path + '/reply', {'answer': {'answer': 'Fixture answer'}})
        expected = 'cancelled' if cancel else 'answered'
        detail = wait_for(lambda: d if (d := native(base, form_path))['state']['status'] == expected else None)
        if cancel:
            expected_message = ('The user denied this in Slorp and said: ' if bridge else '') + DENIAL_FEEDBACK
            assert detail['state']['message'] == expected_message, detail
        else:
            assert detail['state']['answer'] == {'answer': 'Fixture answer'}, detail
        assert not native(base, path + '/form')
    print('PASS: native permission allow/reject and form answer/cancel' +
          (' through Slorp' if bridge else '') + '; Unicode/special-character denial feedback preserved', flush=True)
    return detail


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--integration', action='store_true', help='answer native permissions/forms through bundled Slorp plugin')
    parser.add_argument('--attach', action='store_true', help='use a normal authenticated TUI instead of an explicit headless plugin URL')
    args = parser.parse_args()
    assert not args.attach or args.integration, '--attach requires --integration'
    executable = shutil.which('opencode')
    assert executable
    with workspace() as temporary:
        root = Path(temporary).resolve()
        env = environment(root)
        version(executable, root, env)
        fixture = lifecycle().Fixture()
        config = model_config(f'http://127.0.0.1:{fixture.server_port}')
        config['permissions'] = [{'action': 'shell', 'resource': '*', 'effect': 'ask'}]
        (root/'config/opencode/opencode.json').write_text(json.dumps(config))
        proc, log, bridge, plugin, tui = None, None, None, None, None
        try:
            if args.integration:
                binary = root/'slorp'
                subprocess.run(['go', 'build', '-o', str(binary), '.'], cwd=Path(__file__).resolve().parents[2], check=True)
                bridge = f'http://127.0.0.1:{port()}'
                log = open(root/'slorp.log', 'w')
                proc = subprocess.Popen([str(binary), '-addr', bridge.removeprefix('http://'),
                    '-data', str(root/'slorp-data'), '-claude', str(root/'missing'),
                    '-opencode', executable, '-llm', ''], cwd=root, env=env,
                    stdout=log, stderr=log, start_new_session=True)
                wait_for(lambda: request(bridge, '/api/sessions'))
                request(bridge, '/api/plugins/opencode', {}, timeout=120)
                plugin = request(bridge, '/api/sessions')['plugins']['opencode']['path']
            with server(executable, root, env, plugin=None if args.attach else plugin,
                        dynamic_port=args.attach) as base:
                schema = native(base, '/openapi.json')
                assert '/api/session/{sessionID}/prompt' in schema['paths']
                (root/'openapi.json').write_text(json.dumps(schema, indent=2))
                session = native(base, '/api/session', {'title': 'Native fixture',
                    'location': {'directory': str(root)}, 'model': {'providerID': 'mock', 'id': 'fixture'}})
                sid = session['id']
                assert sid.startswith('ses')
                def live():
                    return next((s for s in request(bridge, '/api/sessions')['sessions'] if s['id'] == sid), {})
                if args.attach:
                    tui = terminal([executable, '--server', base, '--session', sid], root, env)
                    wait_for(lambda: live().get('pluginTui'), seconds=60)
                admitted = native(base, f'/api/session/{sid}/prompt', {'text': 'Native fixture prompt'})
                assert admitted['type'] == 'user', admitted
                def answered():
                    messages = native(base, f'/api/session/{sid}/message')
                    return next((m for m in messages if m['type'] == 'assistant'
                                 and m.get('time', {}).get('completed')), None)
                answer = wait_for(answered)
                assert answer.get('finish') == 'stop', answer
                assert answer['content'] == [{'type': 'text', 'text': lifecycle().ANSWER}], answer
                assert answer['tokens']['input'] == 12 and answer['tokens']['output'] == 3, answer
                exported = native(base, f'/api/experimental/session/{sid}/export')
                (root/'export.json').write_text(json.dumps(exported, indent=2))
                assert exported['info']['id'] == sid
                assert any(m['type'] == 'user' and m['text'] == 'Native fixture prompt'
                           for m in exported['messages'])
                assert len(fixture.captured()) == 1, fixture.captured()
                if bridge:
                    wait_for(lambda: (s := live()).get('lastReply') == lifecycle().ANSWER and
                             s.get('lastPrompt') == 'Native fixture prompt' and
                             s.get('used', {}).get('in') == 12 and s.get('used', {}).get('out') == 3)
                cancelled = permissions_and_forms(base, sid, bridge)
                (root/'cancelled-form.json').write_text(json.dumps(cancelled, ensure_ascii=False, indent=2))
                print('PASS: installed native prompt admission, loopback SSE, projected messages, tokens and export', flush=True)
        except BaseException:
            (root/'fixture-requests.json').write_text(json.dumps(fixture.captured(), indent=2))
            if bridge:
                try:
                    state = request(bridge, '/api/sessions')
                    (root/'slorp-state.json').write_text(json.dumps(state, indent=2))
                    print('=== Slorp sessions ===\n' + json.dumps(state['sessions'], indent=2), flush=True)
                except (OSError, ValueError, AssertionError) as error:
                    print('State unavailable:', error, flush=True)
            raise
        finally:
            if tui:
                stop(tui)
            fixture.close()
            if proc:
                stop(proc)
            if log:
                log.close()


if __name__ == '__main__':
    main()
