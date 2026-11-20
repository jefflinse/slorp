"""Offline native OpenCode 1.19.6 account-quota smoke.

Run: python3 -B integrations/opencode/quotas-smoke.py. TMPDIR selects the
temporary parent; SLORP_SMOKE_KEEP=1 retains synthetic-only diagnostics.
No auth loader, provider.options.fetch, collector, or bridge payload is mocked.
Only the final global-fetch network boundary is redirected to loopback.
"""

from contextlib import closing
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import signal
import sqlite3
import subprocess
import threading
import time
import urllib.parse
import urllib.request

from smoke import port, request, stop, wait_for, workspace


ACCESS = 'quota-fixture-codex-access-not-a-credential'
REFRESH = 'quota-fixture-codex-refresh-not-a-credential'
ACCOUNT = 'quota-fixture-account-not-an-account'
GITHUB = 'quota-fixture-github-token-not-a-credential'
GO_KEY = 'quota-fixture-go-key-not-a-credential'
API_KEY = 'quota-fixture-openai-key-not-a-credential'
RAW = 'quota-fixture-private-response-marker'
CODEX = 'https://chatgpt.com/backend-api/wham/usage'
COPILOT = 'https://api.github.com/copilot_internal/user'
GO = 'https://opencode.ai/zen/go/v1/usage'
REFRESH_URL = 'https://auth.openai.com/oauth/token'
MODEL_URLS = {
    'https://chatgpt.com/backend-api/codex/responses',
    'https://api.openai.com/v1/responses',
    'https://api.githubcopilot.com/chat/completions',
    'https://api.githubcopilot.com/responses',
    'https://api.individual.githubcopilot.com/chat/completions',
    'https://api.individual.githubcopilot.com/responses',
    'https://opencode.ai/zen/go/v1/chat/completions',
}
CATALOG_URLS = {
    'https://api.githubcopilot.com/models',
    'https://api.individual.githubcopilot.com/models',
}

# This plugin observes the real hook without changing its input or output. In
# particular it has no auth.loader and never assigns provider.options.fetch.
PLUGIN = r'''
import { appendFileSync } from 'node:fs'
const settings = SETTINGS
const record = data => appendFileSync(settings.audit, JSON.stringify({at: Date.now(), ...data}) + '\n')
const original = globalThis.fetch
const allowed = new Set(settings.urls)
const local = new Set(settings.local)
let outstanding = 0
globalThis.fetch = async (input, init) => {
  const url = new URL(input instanceof Request ? input.url : String(input))
  if (local.has(url.origin)) {
    if (url.pathname === '/api/bridge/poll') record({type: 'heartbeat'})
    return original(input, init)
  }
  if (!allowed.has(url.href)) {
    record({type: 'denied', url: url.href})
    throw new Error('Offline quota fixture denied external fetch: ' + url.href)
  }
  const req = new Request(input, init)
  const headers = new Headers(req.headers)
  record({type: 'fetch', url: url.href, method: req.method, redirect: req.redirect})
  outstanding++
  try {
    const response = await original(settings.fixture + '/fetch?url=' + encodeURIComponent(url.href), {
      method: req.method, headers, signal: req.signal, redirect: 'error',
      ...(req.method !== 'GET' && req.method !== 'HEAD' ? {body: await req.arrayBuffer()} : {}),
    })
    // Preserve the original URL so the collector's redirect defense is tested,
    // rather than accidentally rejecting the loopback transport substitution.
    Object.defineProperty(response, 'url', {value: url.href})
    return response
  } finally {
    outstanding--
    record({type: 'settled', url: url.href, outstanding})
  }
}
// Codex may prefer WebSocket; force its native HTTP/SSE fallback, never a socket
// to the real provider. No replacement provider fetch is supplied.
globalThis.WebSocket = class {
  constructor(url) {
    record({type: 'websocket-blocked', url: String(url)})
    throw new Error('Offline fixture requires native HTTP fallback')
  }
}
export default async () => ({
  'chat.params': async input => {
    const p = input.provider, m = input.model
    record({type: 'params', sessionID: input.sessionID, provider: p.id,
      apiURL: m.api.url, model: m.id, context: m.limit.context,
      nativeFetch: typeof p.options?.fetch === 'function',
      keyKind: p.options?.apiKey === 'opencode-oauth-dummy-key' ? 'oauth-sentinel' :
        p.options?.apiKey === '' ? 'empty-sentinel' : 'other'})
  },
  dispose: async () => record({type: 'dispose', outstanding}),
})
'''


class Fixture(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self):
        super().__init__(('127.0.0.1', 0), Handler)
        self.lock = threading.Lock()
        self.records = []
        self.errors = []
        self.throttle = False
        self.reset = int(time.time()) + 86400 * 7
        self.reset_iso = datetime.fromtimestamp(self.reset, timezone.utc).isoformat().replace('+00:00', 'Z')
        self.thread = threading.Thread(target=self.serve_forever, daemon=True)
        self.thread.start()

    def captured(self, url=None):
        with self.lock:
            return [r.copy() for r in self.records if url is None or r['url'] == url]

    def close(self):
        self.shutdown()
        self.server_close()
        self.thread.join(timeout=5)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_CONNECT(self):
        with self.server.lock:
            self.server.errors.append('Denied proxy CONNECT: ' + self.path)
        self.send_error(502)

    def do_GET(self):
        self.handle_fixture()

    def do_POST(self):
        self.handle_fixture()

    def respond(self, status, body, headers=None):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(data)

    def handle_fixture(self):
        if self.path.startswith('/registry/'):
            self.respond(404, {'error': 'Dependency downloads disabled in offline smoke'})
            return
        url = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query).get('url', [''])[0]
        body = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        headers = {key.lower(): value for key, value in self.headers.items()}
        with self.server.lock:
            self.server.records.append({'url': url, 'method': self.command, 'headers': headers,
                                        'at': time.time()})
        try:
            assert self.path.startswith('/fetch?'), ('Unexpected direct network/proxy call', self.path)
            if url in (CODEX, COPILOT, GO):
                assert self.command == 'GET', (url, self.command)
                token = {CODEX: ACCESS, COPILOT: GITHUB, GO: GO_KEY}[url]
                assert headers.get('authorization') == 'Bearer ' + token, (url, headers)
                if url == CODEX:
                    assert headers.get('chatgpt-account-id') == ACCOUNT, headers
                if self.server.throttle:
                    self.respond(429, {'private': RAW}, {'Retry-After': '3600'})
                    return
                if url == CODEX:
                    result = {'rate_limit': {
                        'primary_window': {'used_percent': 25, 'limit_window_seconds': 18000,
                                           'reset_at': self.server.reset},
                        'secondary_window': {'used_percent': 70, 'limit_window_seconds': 604800,
                                             'reset_at': self.server.reset}}}
                elif url == COPILOT:
                    result = {'quota_reset_date_utc': self.server.reset_iso, 'quota_snapshots': {
                        'premium_interactions': {'percent_remaining': 60, 'unlimited': False,
                                                 'entitlement': '300', 'quota_remaining': 180},
                        'chat': {'unlimited': True, 'entitlement': 0, 'percent_remaining': 0}}}
                else:
                    result = {'usage': {kind: {'percent': percent, 'status': 'ok',
                                               'resetsAt': self.server.reset_iso}
                                        for kind, percent in [('rolling', 15), ('weekly', 50), ('monthly', 90)]}}
                self.respond(200, dict(result, private=RAW))
            elif url in CATALOG_URLS:
                assert self.command == 'GET', self.command
                assert headers.get('authorization') == 'Bearer ' + GITHUB, headers
                self.respond(200, {'data': [{'id': 'gpt-4.1', 'name': 'Quota fixture',
                    'version': 'fixture', 'model_picker_enabled': True,
                    'supported_endpoints': ['/chat/completions'],
                    'capabilities': {'type': 'chat', 'family': 'gpt-4.1',
                        'limits': {'max_context_window_tokens': 32000, 'max_output_tokens': 1000},
                        'supports': {'tool_calls': True, 'streaming': True}}}]})
            elif url == REFRESH_URL:
                assert self.command == 'POST' and REFRESH in body.decode(), body
                self.respond(200, {'access_token': ACCESS, 'refresh_token': REFRESH,
                                   'expires_in': 86400, 'token_type': 'Bearer'})
            elif url in MODEL_URLS:
                assert self.command == 'POST', (url, self.command)
                model_body = json.loads(body)
                assert model_body.get('model') and model_body.get('stream') is True, model_body
                token = ACCESS if 'chatgpt.com' in url else GITHUB if 'githubcopilot.com' in url \
                    else GO_KEY if 'opencode.ai' in url else API_KEY
                assert headers.get('authorization') == 'Bearer ' + token, (url, headers)
                if token == ACCESS:
                    assert headers.get('chatgpt-account-id') == ACCOUNT, headers
                # A nonretryable provider error still leaves a real, replyable
                # native root and its chat.params capability available to Slorp.
                self.respond(400, {'error': {'message': 'Synthetic offline model error',
                                            'type': 'invalid_request_error', 'code': 'fixture'}})
            else:
                raise AssertionError(('Unexpected fixture URL', url))
        except (AssertionError, ValueError) as error:
            with self.server.lock:
                self.server.errors.append(str(error))
            self.respond(500, {'error': 'Fixture assertion failed'})
        except (BrokenPipeError, ConnectionResetError):
            pass  # Native disposal may cancel a loopback response.


def audit(path):
    return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []


def sanitized(value):
    text = json.dumps(value) if not isinstance(value, str) else value
    for secret in (ACCESS, REFRESH, ACCOUNT, GITHUB, GO_KEY, API_KEY, RAW,
                   'opencode-oauth-dummy-key', 'quota_snapshots', 'rate_limit',
                   'provider.options', '"apiKey"', '"accountId"', '"fetch"'):
        assert secret not in text, f'Private provider data leaked: {secret}'


def main():
    if not __debug__:
        raise SystemExit('Run without -O: this smoke uses assertions')
    executable = shutil.which('opencode')
    assert executable, 'OpenCode 1.19.6 must be installed'
    repo = Path(__file__).resolve().parents[2]
    urllib.request.install_opener(urllib.request.build_opener(urllib.request.ProxyHandler({})))
    with workspace() as temporary:
        root = Path(temporary).resolve()
        config, project = root/'config'/'opencode', root/'project'
        for directory in (root/'home', config, project, root/'tmp', root/'data'/'opencode'):
            directory.mkdir(parents=True)
        binary = root/'slorp'
        # Retain ambient Go caches; disallow fetching dependencies during build.
        subprocess.run(['go', 'build', '-o', str(binary), '.'], cwd=repo,
                       env=dict(os.environ, GOPROXY='off', GOSUMDB='off'), check=True)
        env = {key: os.environ[key] for key in ('PATH', 'LANG', 'LC_ALL') if key in os.environ}
        env.update(HOME=str(root/'home'), OPENCODE_TEST_HOME=str(root/'home'), TMPDIR=str(root/'tmp'),
                   XDG_CONFIG_HOME=str(root/'config'), XDG_DATA_HOME=str(root/'data'),
                   XDG_STATE_HOME=str(root/'state'), XDG_CACHE_HOME=str(root/'cache'),
                   npm_config_cache=str(root/'npm-cache'), TERM='xterm-256color',
                   OPENCODE_DISABLE_EXTERNAL_SKILLS='true', OPENCODE_DISABLE_DEFAULT_PLUGINS='false',
                   OPENCODE_DISABLE_MODELS_FETCH='true', OPENCODE_DISABLE_UPDATE_CHECK='true',
                   OPENCODE_DISABLE_LSP_DOWNLOAD='true',
                   GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull)
        version = subprocess.check_output([executable, '--version'], env=env, cwd=root, text=True).strip()
        assert version == '1.19.6', f'Expected native OpenCode 1.19.6, got {version}'
        subprocess.run(['git', 'init', '--quiet', str(project)], env=env, cwd=root, check=True)
        fixture = Fixture()
        fixture_url = f'http://127.0.0.1:{fixture.server_port}'
        base, native = f'http://127.0.0.1:{port()}', f'http://127.0.0.1:{port()}'
        env.update(SLORP_URL=base, HTTP_PROXY=fixture_url, HTTPS_PROXY=fixture_url,
                   ALL_PROXY=fixture_url, NO_PROXY='127.0.0.1,localhost,::1',
                   npm_config_registry=fixture_url + '/registry/', NPM_CONFIG_REGISTRY=fixture_url + '/registry/')
        plugin = root/'network-fixture'
        plugin.mkdir()
        (plugin/'package.json').write_text(json.dumps({'name': 'quota-network-fixture',
                                                      'type': 'module', 'main': 'index.js'}))
        audit_path = root/'audit.jsonl'
        settings = {'audit': str(audit_path), 'fixture': fixture_url, 'local': [base, native, fixture_url],
                    'urls': sorted(MODEL_URLS | CATALOG_URLS | {CODEX, COPILOT, GO, REFRESH_URL})}
        (plugin/'index.js').write_text(PLUGIN.replace('SETTINGS', json.dumps(settings)))
        config_file = config/'opencode.json'
        config_file.write_text(json.dumps({'plugin': [str(plugin)], 'autoupdate': False, 'share': 'disabled'}))
        children = []
        log_path = root/'processes.log'
        log = log_path.open('w')

        def service(command):
            proc = subprocess.Popen(command, env=env, cwd=project, stdout=log, stderr=log, start_new_session=True)
            children.append(proc)
            return proc

        def state():
            result = request(base, '/api/sessions')
            sanitized(result)
            return result

        def live(sid):
            return next((s for s in state()['sessions'] if s['id'] == sid), None)

        def report(sid):
            return next((r for r in state().get('providerLimits', []) if r['sessionId'] == sid), None)

        try:
            slorp = service([str(binary), '-addr', base.removeprefix('http://'), '-data', str(root/'slorp-data'),
                             '-claude', str(root/'no-claude'), '-opencode', executable, '-llm', ''])
            wait_for(lambda: state().get('plugins', {}).get('opencode'))
            request(base, '/api/plugins/opencode', {}, timeout=120)
            installed = state()['plugins']['opencode']
            assert installed['installed'], installed
            installed_config = json.loads(config_file.read_text())
            cases = [
                ('codex', 'openai', 'gpt-5.4', 'openai-codex', CODEX),
                ('go', 'opencode-go', 'quota-fixture', 'opencode-go', GO),
                ('copilot', 'github-copilot', 'gpt-4.1', 'github-copilot', COPILOT),
                ('api-key', 'openai', 'gpt-5.4', 'openai-codex', None),
            ]
            for name, provider, model, source, quota_url in cases:
                fixture.throttle = False
                auth = {'openai': {'type': 'oauth', 'access': ACCESS, 'refresh': REFRESH,
                                  'expires': int(time.time() * 1000) + 86400000, 'accountId': ACCOUNT},
                        'github-copilot': {'type': 'oauth', 'access': GITHUB, 'refresh': GITHUB,
                                           'expires': int(time.time() * 1000) + 86400000},
                        'opencode-go': {'type': 'api', 'key': GO_KEY}}
                if name == 'api-key':
                    auth['openai'] = {'type': 'api', 'key': API_KEY}
                (root/'data'/'opencode'/'auth.json').write_text(json.dumps({provider: auth[provider]}))
                official = {'openai': 'https://api.openai.com/v1',
                            'github-copilot': 'https://api.githubcopilot.com',
                            'opencode-go': 'https://opencode.ai/zen/go/v1'}[provider]
                provider_config = {'api': official, 'whitelist': [model], 'models': {model: {
                    'name': 'Quota fixture', 'limit': {'context': 32000, 'output': 1000}}}}
                if name == 'go':
                    provider_config.update(npm='@ai-sdk/openai-compatible',
                                           options={'baseURL': 'https://opencode.ai/zen/go/v1'})
                configured = dict(installed_config, enabled_providers=[provider],
                                  model=f'{provider}/{model}', small_model=f'{provider}/{model}',
                                  provider={provider: provider_config})
                config_file.write_text(json.dumps(configured))
                before = len(fixture.captured())
                audit_start = len(audit(audit_path))
                agent = service([executable, 'serve', '--hostname', '127.0.0.1',
                                 '--port', native.rsplit(':', 1)[1], '--print-logs'])
                wait_for(lambda: request(native, '/global/health'))
                session = request(native, '/session', {'title': f'Quota smoke {name}'}, timeout=120)
                sid = session['id']
                assert not report(sid), 'Catalog/config inferred account quotas before chat.params'
                assert not [r for r in fixture.captured()[before:] if r['url'] in (CODEX, COPILOT, GO)]
                request(native, f'/session/{sid}/message', {
                    'model': {'providerID': provider, 'modelID': model},
                    'parts': [{'type': 'text', 'text': 'Return the offline fixture answer.'}]}, timeout=120)
                params = wait_for(lambda: next((e for e in audit(audit_path)[audit_start:]
                    if e['type'] == 'params' and e['sessionID'] == sid), None))
                assert params['provider'] == provider and params['context'] == 32000 and params['apiURL'] == official, params
                if name in ('codex', 'copilot'):
                    assert params['nativeFetch'], ('Native auth loader did not provide fetch', params)
                    assert params['keyKind'] == ('oauth-sentinel' if name == 'codex' else 'empty-sentinel'), params
                else:
                    assert not params['nativeFetch'], params
                wait_for(lambda: (live(sid) or {}).get('canReply'))
                expected_status = 'ok' if quota_url else 'unsupported'
                reading = wait_for(lambda: r if (r := report(sid)) and r['status'] == expected_status else None)
                assert reading['provider'] == source and reading['sessionLabel'], reading
                session_state = live(sid)
                assert session_state['contextWindow'] == 32000 and session_state['model'] == f'{provider}/{model}', session_state
                assert not any(session_state['used'].values()), session_state
                assert not state()['limits'], 'Model/context metadata became inferred account limits'
                if quota_url:
                    assert fixture.captured(quota_url), ('Quota did not use the native runtime capability', params)
                    quota_fetches = [e for e in audit(audit_path)[audit_start:]
                                     if e['type'] == 'fetch' and e['url'] == quota_url]
                    assert quota_fetches and all(e['method'] == 'GET' and e['redirect'] == 'error'
                                                 for e in quota_fetches), quota_fetches
                    expected = {'codex': [('primary_window', 25), ('secondary_window', 70)],
                                'go': [('rolling', 15), ('weekly', 50), ('monthly', 90)],
                                'copilot': [('premium_interactions', 40)]}[name]
                    assert [(w['kind'], w['percentUsed']) for w in reading['windows']] == expected, reading
                    assert all(w['resetsAt'] == fixture.reset_iso for w in reading['windows']), reading
                    assert not reading['stale'] and reading['measuredAt'], reading
                    if name == 'codex':
                        assert [(w['label'], w['windowSeconds']) for w in reading['windows']] == [('5-hour', 18000), ('7-day', 604800)]
                    if name == 'copilot':
                        assert any(r['url'] in CATALOG_URLS for r in fixture.captured()[before:]), 'Native Copilot catalog not exercised'
                else:
                    assert not reading['windows'] and not reading.get('measuredAt'), reading
                    assert not [r for r in fixture.captured()[before:] if r['url'] in (CODEX, COPILOT, GO)]
                assert any(r['url'] in MODEL_URLS for r in fixture.captured()[before:]), 'Native model transport not exercised'
                assert not fixture.errors, fixture.errors
                print(f'PASS: {name}: real chat.params -> {source} {expected_status}; native transport headers and UIState verified', flush=True)

                if name == 'codex':
                    fixture.throttle = True
                    throttled = wait_for(lambda: r if (r := report(sid)) and r['status'] == 'throttled' else None, seconds=80)
                    checked = datetime.fromisoformat(throttled['checkedAt'].replace('Z', '+00:00')).timestamp()
                    retry = datetime.fromisoformat(throttled['retryAt'].replace('Z', '+00:00')).timestamp()
                    assert 3595 <= retry - checked <= 3605, throttled
                    assert not throttled['windows'] and not throttled.get('measuredAt') and throttled['stale'], throttled
                    count = len(fixture.captured(CODEX))
                    heartbeats = sum(e['type'] == 'heartbeat' for e in audit(audit_path))
                    time.sleep(65)
                    assert len(fixture.captured(CODEX)) == count, 'Ignored Retry-After'
                    assert sum(e['type'] == 'heartbeat' for e in audit(audit_path)) > heartbeats
                    assert live(sid)['canReply'], live(sid)
                    print('PASS: 429 Retry-After: 3600 clears OAuth measurements without breaking heartbeats or canReply', flush=True)

                request(native, '/instance/dispose', {}, timeout=30)
                wait_for(lambda: any(e['type'] == 'dispose' for e in audit(audit_path)[audit_start:]))
                time.sleep(1)
                after_dispose = len(fixture.captured())
                audit_end = len(audit(audit_path))
                # Leave the process alive beyond a collector period: process exit
                # must not conceal a leaked interval. Go has no Retry-After delay.
                time.sleep(65 if name == 'go' else 3)
                assert agent.poll() is None and request(native, '/global/health')
                assert len(fixture.captured()) == after_dispose, 'Provider polling survived native disposal'
                assert not any(e['type'] in ('heartbeat', 'fetch') for e in audit(audit_path)[audit_end:])
                assert all(e['outstanding'] == 0 for e in audit(audit_path)[audit_start:] if e['type'] == 'dispose')
                assert not any(e['type'] == 'denied' for e in audit(audit_path)[audit_start:]), audit(audit_path)[audit_start:]
                stop(agent)
            slorp.send_signal(signal.SIGINT)
            slorp.wait(timeout=15)
            assert slorp.returncode == 0, slorp.returncode
            databases = list((root/'slorp-data').rglob('*.db')) + list((root/'slorp-data').rglob('*.sqlite*'))
            databases = [p for p in databases if p.is_file() and p.read_bytes().startswith(b'SQLite format 3')]
            assert databases, 'No Slorp SQLite database inspected'
            for database in databases:
                with closing(sqlite3.connect(database.as_uri() + '?mode=ro', uri=True)) as db:
                    sanitized('\n'.join(db.iterdump()))
                for path in database.parent.glob(database.name + '*'):
                    sanitized(path.read_bytes().decode('utf8', errors='replace'))
            assert not fixture.errors, fixture.errors
            print('PASS: native disposal stops polling in a live process; no private provider data in Slorp API or SQLite', flush=True)
            print('SKIP: live account endpoint verification; all credentials and provider responses were synthetic', flush=True)
        except BaseException:
            log.flush()
            print('=== Native / Slorp logs ===\n' + log_path.read_text(), flush=True)
            print('=== Native hook / transport audit ===\n' + json.dumps(audit(audit_path), indent=2), flush=True)
            print('=== Synthetic fixture requests ===\n' + json.dumps(fixture.captured(), indent=2), flush=True)
            try:
                print('=== Slorp state ===\n' + json.dumps(state(), indent=2), flush=True)
            except (OSError, ValueError, AssertionError) as error:
                print('State unavailable:', error, flush=True)
            raise
        finally:
            for child in reversed(children):
                stop(child)
            fixture.close()
            log.close()


if __name__ == '__main__':
    main()
