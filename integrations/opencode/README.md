# Slorp for OpenCode

An OpenCode 2.x plugin that connects sessions to the local [Slorp](../../) service. It includes a server bridge and a small terminal companion.

- Reports each active top-level session's turns, title, directory, branch, model and reasoning variant, token usage, estimated cost, and context use. Existing message usage is read when the session is first observed; repeated message updates do not count twice. Context capacity becomes available when OpenCode makes a model request.
- Shows retries and failures, and marks compactions in the timeline.
- Shows nested subagents with their task, live status, current tool or wait, timing, model, reasoning variant, tools, token usage and cost. Task prompts and returned reports remain available after the session closes or Slorp restarts.
- Fills in earlier history on request: the latest forty exchanges and compaction markers from the local OpenCode record, with summaries written by Slorp's local model. Replies are paired with their original prompts; internal summaries, reasoning, tool output, automatic replay turns, and reverted content are excluded.
- Delivers replies written in Slorp as your own prompts, preserving the selected agent, provider, model and reasoning variant.
- Offers permission prompts and structured questions, including subagents', in Slorp as well as OpenCode. Questions support single and multiple choices, custom text when allowed, and dismissal. Whichever you answer first decides it. Permissions allow one call or reject it; they never allow "always". Concurrent requests under one top-level session are presented one at a time.
- Repeats the current state on every poll and restores outstanding prompts after Slorp restarts. Failed reply acknowledgements and prompt settlements are retried without resubmitting your reply.
- Starts, stops and reopens conversations from Slorp, with choices drawn from the selected directory's OpenCode configuration.
- Reports supported provider account allowances after observing their native model-request context, with percentages, reset times and reading age.

The plugin is Slorp's view of live OpenCode conversations. In a local terminal or `opencode attach`, a conversation appears as soon as you open or resume it, even while idle. Opening it preserves the original exchange timestamps. Conversations observed by an instance remain listed until its bridge stops reporting; they leave about 30 seconds later. Unopened saved conversations are not listed. V2 normally uses a shared background service, so closing a terminal does not necessarily stop its bridge.

The terminal companion announces its selected session through OpenCode's authenticated server client, supporting both local terminals and separate-process `opencode attach`. Session usage includes descendants once; each subagent's usage counts only its own work.

## Provider account limits

Plugin **0.6.0** adds account-allowance readings for these native provider configurations:

| Provider | Supported connection | Reported allowances |
| --- | --- | --- |
| OpenAI Codex | OpenCode's selected ChatGPT OAuth connection | Primary and secondary windows, using the durations and resets reported by the provider |
| GitHub Copilot | OpenCode's selected GitHub.com OAuth connection | Finite premium-request, chat and completion quotas when the response supplies a usable entitlement |
| OpenCode Go | API key with the official `opencode-go` endpoint | Rolling, weekly and monthly subscription windows |

Collection begins after the first model request exposes the runtime provider context. An opened but idle conversation has no reading until then. Subagent contexts are attributed to their root conversation. The collector normally refreshes once a minute, backs off on errors, and honors throttling delays up to 24 hours. It does not delay model requests. The server plugin resolves the active connection through OpenCode's credential API, leaving OAuth refresh to OpenCode, and uses credentials only at the fixed provider usage endpoints.

Top-bar meters identify the provider; their tooltips identify the source session, measurement age and reset time. Different sessions' readings are kept separate because Slorp does not assume they use the same account. Under one root, the latest observed context supplies each provider's reading. Credential and provider changes invalidate affected readings.

Unavailable, authentication-required, unsupported and throttled readings have distinct status text. They are never interpreted as zero usage or exhaustion. Readings become stale after five minutes and lose their gauges after ten minutes or when the window resets. Transient Go failures can retain a stale reading for the same key; failed OAuth rereads clear the previous windows. Credentials, account identifiers and raw responses stay out of Slorp's bridge payloads and database; normalized readings are held only for open sessions.

OpenAI API keys, Copilot Enterprise, custom/proxy endpoints, Zen billing and Anthropic subscription OAuth are not supported by this collector. Unlimited or pool-backed Copilot quotas without a finite entitlement do not produce percentage gauges. These provider-specific endpoints are not a universal OpenCode quota API: unknown response shapes are treated as unavailable, including Go accounts returning an unrecognized migrated schema. No limits are inferred from token totals, estimated costs or generic rate-limit errors.

## Attached terminals

The attached OpenCode server needs the Slorp server plugin, and the terminal needs its TUI companion. Update both installations and reload the plugin. The server bridge sends sessions to its configured Slorp address; on a remote host, that address must be reachable from the server.

Every two seconds, the companion publishes a namespaced presence marker through the same OpenCode client used by the terminal, preserving attach credentials and server-side directory/workspace routing. Each terminal has its own identity and sequence, so switching conversations, returning home, and reconnecting can be handled independently. Server-side checks verify the selected conversation and its parent chain before discovery. Managed runtimes continue to admit only their privately assigned root.

Presence uses the plugin's `slorp` RPC through OpenCode's authenticated client. The companion indicator expires after about ten seconds without validated presence. Previously observed conversations keep the existing server-lifetime behavior: closing or switching an attached terminal does not retire them while the server bridge is still reporting.

The standard OpenCode V2 TUI is supported. The distinct Mini interface is not covered.

## Start, stop and reopen

Choose **New session** from Sessions or an effort, select **OpenCode**, and enter a directory. Slorp loads that directory's available root agents, connected provider/models, and reasoning variants. Default choices follow native configuration and recent-model selection. The OpenCode agent is separate from the Slorp role; the reasoning variant is separate from the effort where the conversation is filed.

Each launch runs a private, authenticated loopback OpenCode server with Slorp's bundled bridge. It remains available for replies and permissions while idle. **Stop** interrupts its work and closes that owned runtime while retaining the native conversation and Slorp record. Terminal-owned sessions are not stoppable from Slorp. Managed runtimes close when Slorp exits, including parent-process loss; conversations can be reopened afterwards.

Reopen a closed session with an optional prompt. Leaving the prompt empty restores an idle, replyable conversation without adding a turn. The session keeps its native ID, selected agent/model/variant, effort, role, timeline and subagent reports. **Copy resume command** uses the configured OpenCode executable and quotes the directory and session ID for a shell.

The bridge is activated only for the managed conversation and its descendants. Late requests from a stopped runtime cannot reopen it or consume the replacement runtime's replies. Starting and reopening use durable request IDs: retries of the same operation do not create another conversation or submit the prompt twice. If a response is uncertain, the page offers the retained conversation for inspection. Preparing another attempt is explicit, since a previous prompt may already have run.

Managed launches use private configuration snapshots and dependency scopes, keeping the original configuration files intact while preserving native instructions, skills and supported custom tools. There are explicit limits:

- `OPENCODE_PURE` is unsupported because managed sessions require the Slorp bridge.
- Custom tools must keep their install dependencies within their own configuration package. Workspace layouts, `workspace:`/`link:` dependencies, dependencies outside that private scope, and dependency-bearing ancestor packages or populated ancestor `node_modules` are rejected with an explanation.
- Slorp plugin references supplied through remote or inline configuration must be moved to a local configuration file so the managed runtime can replace them with its bundled plugin.

These limits apply to Slorp-managed launches; normal terminal bridge operation is unchanged. Slorp supplies its bundled version to each managed runtime; update and reload existing terminal installations separately.

## Subagents and reports

Opening a session discovers its existing child sessions recursively. New child activity updates the same subagent view used for Claude Code. Children remain under their root conversation, with nested parent relationships; they do not become separate session cards.

The report view keeps the original task prompt and the latest returned result. Until a result is handed back, it can show the child's latest assistant text. Background launch acknowledgements are not final reports, and an idle child is not assumed to have completed its task. Resuming a child reuses its row and cumulative usage while clearing the previous result for the new run. If OpenCode truncates a returned foreground result, Slorp preserves that returned text and its truncation notice.

Slorp retains up to 200 child records per root, with task prompts and reports limited to 60,000 characters each. Retained records survive bridge disconnection, archive retirement and restarts, including sessions without local-model summaries. Reports are fetched when expanded. Failed writes are retried, and a Slorp restart causes the bridge to resend records. Children that Slorp has never observed require opening their root in OpenCode before their records can be retained.

Update the plugin in Settings and reload it in OpenCode if the running bridge has not refreshed.

## Earlier history

Open a live session's page and choose **Fill in earlier history** beside its timeline. A local model must be available to summarize the exchanges. Slorp reads the native OpenCode V2 transcript, retains the latest forty exchanges/compaction markers, and queues answered exchanges for the existing summarizer. This happens only when requested. Re-reading keeps existing summaries and distinct repeated prompts.

History export has a thirty-second timeout and a 64 MiB parsing limit. Export failures leave the timeline intact and appear on the page. This reads local history; conversations stored only on a remote OpenCode server are not supported. Closed-session history backfill is not yet exposed.

Compaction records retain stable native IDs. Reverted and internal synthetic content is excluded from backfill.

## Install

Start Slorp and open **Settings → Integrations → OpenCode**. Slorp detects whether OpenCode has the plugin and offers **Install** when it is missing.

This works with `go install github.com/jefflinse/slorp@latest`: the Slorp executable contains the plugin files. Installation writes a managed package to `~/.slorp/plugins/opencode` (or under `-data`) and registers its local directory in the global `opencode.json(c)` `plugins` array. OpenCode loads both server and terminal exports from that entry. The managed package uses the address Slorp is listening on, including a custom `-addr` port. Use `-opencode` if the executable is not on Slorp's PATH.

Settings distinguishes installed files from a reporting bridge and a running session that needs reloading. When a newer Slorp executable carries different plugin files, it offers **Update**. **Uninstall** removes the managed package's configuration references. OpenCode watches configuration and reloads automatically; use `/reload` if needed. Managed files are retained for reinstallation.

OpenCode's `plugin add` command accepts npm/Git packages, so Slorp registers the embedded local package directly.

### Development checkout

For development, add this repository's plugin directory to the global `opencode.json(c)`:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": ["/path/to/slorp/integrations/opencode"]
}
```

Slorp recognizes this local installation and keeps it separate from a managed copy. One configuration entry loads both parts.

After updating the plugin, use `/reload` if the running bridge has not refreshed. The live-session features use OpenCode's V2 API and require an updated Slorp service as well.

## Config

The service address defaults to `http://127.0.0.1:4747`. Set `SLORP_URL` to change it, or give the plugin a `url` option:

```json
{ "plugins": [{ "package": "/path/to/slorp/integrations/opencode", "options": { "url": "http://127.0.0.1:4747" } }] }
```

`SLORP_URL` wins when both are set.

### Unregistered headless servers

Shared-service sessions, interactive terminals, and Slorp-managed runtimes obtain their native connection automatically. OpenCode 2.3.2's server-plugin API omits complete transcript and form operations, so interactive standalone and attached terminals provide those operations through their authenticated client.

For a manually started `opencode serve` that must keep full bridge functionality without an attached terminal, configure its loopback URL in the plugin's `opencodeURL` option and start that server with `OPENCODE_PASSWORD` set:

```json
{
  "plugins": [{
    "package": "/path/to/slorp/integrations/opencode",
    "options": { "opencodeURL": "http://127.0.0.1:4096" }
  }]
}
```

The endpoint must identify the same OpenCode process. The password stays in the server environment; it is not sent to Slorp or stored in plugin options.

## Develop

```sh
npm install
npm run check
npm test
```

Tests run on Node 22.6 or later.

The checked-in `service.js` bundles the public discovery SDK pinned to OpenCode
2.3.2. Regenerate it only when updating the pinned client/discovery dependency:

```sh
node integrations/opencode/build-service.mjs
```

Run that command from the repository root, or `node build-service.mjs` from this
directory. Ordinary `go install` consumers use the embedded helper and need no
Node/npm checkout.

The opt-in runtime smoke tests need Python 3, Go, and OpenCode 2.3.2 on macOS or
Linux. The quota fixture also uses `openssl` for its isolated loopback TLS proxy.

```sh
python3 -B integrations/opencode/native-smoke.py
python3 -B integrations/opencode/native-smoke.py --integration
python3 -B integrations/opencode/native-smoke.py --integration --attach
python3 -B integrations/opencode/smoke.py --exercise
python3 -B integrations/opencode/smoke.py --shared --exercise
python3 -B integrations/opencode/history-smoke.py
python3 -B integrations/opencode/history-smoke.py --auto-compaction
python3 -B integrations/opencode/history-smoke.py --auto-compaction --single-followup
python3 -B integrations/opencode/history-smoke.py --auto-compaction --revert
python3 -B integrations/opencode/subagents-smoke.py
python3 -B integrations/opencode/lifecycle-smoke.py
python3 -B integrations/opencode/attach-smoke.py
python3 -B integrations/opencode/quotas-smoke.py
SLORP_NATIVE_TEST=1 go test -race ./internal/opencode -run Native -count=1
```

Run these from the repository root. The native contract gate checks real prompt admission, loopback SSE, transcript/token reads, export, permissions, and supported forms. `--integration` answers permissions/forms through the bundled Slorp plugin; `--attach` uses an authenticated TUI with a dynamically allocated server port instead of an explicit headless plugin URL.

The terminal smoke builds Slorp, installs its bundled plugin into native V2 configuration, verifies JSONC comments and nested plugin-object options are preserved, and opens two idle conversations in pseudoterminals. It checks discovery, restart recovery, and retirement. `--exercise` adds a loopback prompt/reply and transcript/usage checks. `--shared` uses normal shared-service discovery and verifies another reply after both TUIs exit; the default tests standalone TUI transport. Neither injects an OpenCode URL.

The history tests import V2 transcripts, verify paginated reads and a large export, then backfill twice through Slorp's HTTP API. They cover repeated prompts, checkpoint compaction, synthetic continuation, and a real staged revert. The subagent test executes native nested `subagent` tools against a deterministic loopback provider, checking background completion, exact Unicode reports, usage, restart recovery, and archived reports without OpenCode running.

The lifecycle smoke exercises actual prompts, replies, stop, same-ID reopening, and process cleanup. The attach smoke checks authenticated idle discovery, switching/home, independent selections, reconnect recovery, unchanged native records, and server-owned liveness after terminal exit. The quota smoke uses real V2 credentials and native model transport with synthetic secrets and loopback responses, including throttling, disposal, and secret-free API/database output; it does not validate a real account's current endpoint response. History, subagent, and quota scripts also accept `--native-only`; quotas accepts `--case codex|go|copilot|api-key` for focused diagnosis.

Native Go tests compare ordinary and managed configuration behavior, including instructions, skills, custom tools, and defaults. All use isolated HOME/XDG and Slorp data; no paid provider calls are made. Set `SLORP_SMOKE_KEEP=1` to retain Python smoke diagnostics and `TMPDIR` to choose their temporary parent.
