# Slorp for OpenCode

An OpenCode plugin that connects the sessions of an OpenCode instance to the local [Slorp](../../) service. It includes a server bridge and a small terminal companion.

- Reports each active top-level session's turns, title, directory, branch, model and reasoning variant, token usage, estimated cost, and context use. Existing message usage is read when the session is first observed; repeated message updates do not count twice. Context capacity becomes available when OpenCode makes a model request.
- Shows retries and failures, and marks compactions in the timeline.
- Shows nested subagents with their task, live status, current tool or wait, timing, model, reasoning variant, tools, token usage and cost. Task prompts and returned reports remain available after the session closes or Slorp restarts.
- Fills in earlier history on request: the latest forty exchanges and compaction markers from the local OpenCode record, with summaries written by Slorp's local model. Replies are paired with their original prompts; internal summaries, reasoning, tool output, automatic replay turns, and reverted content are excluded.
- Delivers replies written in Slorp as your own prompts, preserving the selected agent, provider, model and reasoning variant.
- Offers permission prompts and structured questions, including subagents', in Slorp as well as OpenCode. Questions support single and multiple choices, custom text when allowed, and dismissal. Whichever you answer first decides it. Permissions allow one call or reject it; they never allow "always". Concurrent requests under one top-level session are presented one at a time.
- Repeats the current state on every poll and restores outstanding prompts after Slorp restarts. Failed reply acknowledgements and prompt settlements are retried without resubmitting your reply.
- Starts, stops and reopens conversations from Slorp, with choices drawn from the selected directory's OpenCode configuration.
- Reports supported provider account allowances after observing their native model-request context, with percentages, reset times and reading age.

OpenCode's session list includes saved conversations, rather than a roster of open sessions, so the plugin is Slorp's view of what is live. In a local terminal or `opencode attach`, a conversation appears as soon as you open or resume it, even while idle. Opening it preserves the original exchange timestamps. Conversations observed by an instance remain listed until its bridge stops reporting; they leave about 30 seconds later. Unopened saved conversations are not listed.

The terminal companion announces its selected session through OpenCode's authenticated server client, supporting both local terminals and separate-process `opencode attach`. Session usage includes descendants once; each subagent's usage counts only its own work.

## Provider account limits

Plugin **0.6.0** adds account-allowance readings for these native provider configurations:

| Provider | Supported connection | Reported allowances |
| --- | --- | --- |
| OpenAI Codex | OpenCode's native ChatGPT OAuth transport | Primary and secondary windows, using the durations and resets reported by the provider |
| GitHub Copilot | OpenCode's native GitHub.com OAuth transport | Finite premium-request, chat and completion quotas when the response supplies a usable entitlement |
| OpenCode Go | API key with the official `opencode-go` endpoint | Rolling, weekly and monthly subscription windows |

Collection begins after the first model request exposes the runtime provider context. An opened but idle conversation has no reading until then. Subagent contexts are attributed to their root conversation. The collector normally refreshes once a minute, backs off on errors, and honors throttling delays up to 24 hours. It does not delay model requests. Codex and Copilot use OpenCode's existing authenticated fetch functions, leaving OAuth refresh to OpenCode; Go uses its runtime API key only at the fixed Go usage endpoint.

Top-bar meters identify the provider; their tooltips identify the source session, measurement age and reset time. Different sessions' readings are kept separate because Slorp does not assume they use the same account. Under one root, the latest observed context supplies each provider's reading. OAuth readings are invalidated on a new observed context because a native fetch function can change accounts without changing its identity.

Unavailable, authentication-required, unsupported and throttled readings have distinct status text. They are never interpreted as zero usage or exhaustion. Readings become stale after five minutes and lose their gauges after ten minutes or when the window resets. Transient Go failures can retain a stale reading for the same key; failed OAuth rereads clear the previous windows. Credentials, account identifiers and raw responses stay out of Slorp's bridge payloads and database; normalized readings are held only for open sessions.

OpenAI API keys, Copilot Enterprise, custom/proxy endpoints, Zen billing and Anthropic subscription OAuth are not supported by this collector. Unlimited or pool-backed Copilot quotas without a finite entitlement do not produce percentage gauges. These provider-specific endpoints are not a universal OpenCode quota API: unknown response shapes are treated as unavailable, including Go accounts returning an unrecognized migrated schema. No limits are inferred from token totals, estimated costs or generic rate-limit errors.

## Attached terminals

Plugin **0.5.0** adds idle discovery for `opencode attach`. The attached OpenCode server needs the Slorp server plugin, and the terminal needs its TUI companion. Update both installations and restart their processes. The server bridge sends sessions to its configured Slorp address; on a remote host, that address must be reachable from the server.

Every two seconds, the companion publishes a namespaced presence marker through the same OpenCode client used by the terminal, preserving attach credentials and server-side directory/workspace routing. Each terminal has its own identity and sequence, so switching conversations, returning home, and reconnecting can be handled independently. Server-side checks verify the selected conversation and its parent chain before discovery. Managed runtimes continue to admit only their privately assigned root.

Presence uses `tui.publish` with the reserved `slorp.presence.v1:` command prefix. Stock OpenCode ignores this unknown command for navigation while the server bridge reads it as presence. Custom catch-all command handlers should leave that prefix alone. The companion indicator expires after about ten seconds without validated presence. Previously observed conversations keep the existing server-lifetime behavior: closing or switching an attached terminal does not retire them while the server bridge is still reporting.

This is verified with the standard OpenCode 1.19.6 TUI. The distinct `attach --mini` interface is not covered.

## Start, stop and reopen

Choose **New session** from Sessions or an effort, select **OpenCode**, and enter a directory. Slorp loads that directory's available root agents, connected provider/models, and reasoning variants. Default choices follow native configuration and recent-model selection. The OpenCode agent is separate from the Slorp role; the reasoning variant is separate from the effort where the conversation is filed.

Each launch runs a private, authenticated loopback OpenCode server with Slorp's bundled bridge. It remains available for replies and permissions while idle. **Stop** interrupts its work and closes that owned runtime while retaining the native conversation and Slorp record. Terminal-owned sessions are not stoppable from Slorp. Managed runtimes close when Slorp exits, including parent-process loss; conversations can be reopened afterwards.

Reopen a closed session with an optional prompt. Leaving the prompt empty restores an idle, replyable conversation without adding a turn. The session keeps its native ID, selected agent/model/variant, effort, role, timeline and subagent reports. **Copy resume command** uses the configured OpenCode executable and quotes the directory and session ID for a shell.

The bridge is activated only for the managed conversation and its descendants. Late requests from a stopped runtime cannot reopen it or consume the replacement runtime's replies. Starting and reopening use durable request IDs: retries of the same operation do not create another conversation or submit the prompt twice. If a response is uncertain, the page offers the retained conversation for inspection. Preparing another attempt is explicit, since a previous prompt may already have run.

Managed launches use private configuration snapshots and dependency scopes, keeping the original configuration files intact while preserving native instructions, skills and supported custom tools. There are explicit limits:

- `OPENCODE_PURE` is unsupported because managed sessions require the Slorp bridge.
- Custom tools must keep their install dependencies within their own configuration package. Workspace layouts, `workspace:`/`link:` dependencies, dependencies outside that private scope, and dependency-bearing ancestor packages or populated ancestor `node_modules` are rejected with an explanation.
- When well-known remote configuration and a resolved Slorp plugin are both present, launch is rejected because OpenCode's configuration API cannot establish that replacing the plugin will avoid loading it twice.

These limits apply to Slorp-managed launches; normal terminal bridge operation is unchanged. Plugin **0.4.0** adds managed activation. Slorp supplies its bundled version to each managed runtime; update and restart existing terminal installations separately.

## Subagents and reports

Opening a session discovers its existing child sessions recursively. New child activity updates the same subagent view used for Claude Code. Children remain under their root conversation, with nested parent relationships; they do not become separate session cards.

The report view keeps the original task prompt and the latest returned result. Until a result is handed back, it can show the child's latest assistant text. Background launch acknowledgements are not final reports, and an idle child is not assumed to have completed its task. Resuming a child reuses its row and cumulative usage while clearing the previous result for the new run. If OpenCode truncates a returned foreground result, Slorp preserves that returned text and its truncation notice.

Slorp retains up to 200 child records per root, with task prompts and reports limited to 60,000 characters each. Retained records survive bridge disconnection, archive retirement and restarts, including sessions without local-model summaries. Reports are fetched when expanded. Failed writes are retried, and a Slorp restart causes the bridge to resend records. Children that Slorp has never observed require opening their root in OpenCode before their records can be retained.

Plugin 0.3.0 adds subagent activity and retained reports. Update the plugin in Settings and restart OpenCode to load it.

## Earlier history

Open a live session's page and choose **Fill in earlier history** beside its timeline. A local model must be available to summarize the exchanges. Slorp runs `opencode export --pure` against the local OpenCode data, reads the latest forty exchanges/compaction markers, and queues answered exchanges for the existing summarizer. This happens only when requested. Re-reading keeps existing summaries and distinct repeated prompts.

History export has a thirty-second timeout and a 64 MiB parsing limit. Export failures leave the timeline intact and appear on the page. This reads local history; conversations stored only on a remote OpenCode server are not supported. Closed-session history backfill is not yet exposed.

Plugin 0.2.1 records stable compaction IDs and recognizes overflow replays when reopening a session. Update the plugin and restart OpenCode to load those changes.

## Install

Start Slorp and open **Settings → Integrations → OpenCode**. Slorp detects whether OpenCode has the plugin and offers **Install** when it is missing. Then quit and restart OpenCode.

This works with `go install github.com/jefflinse/slorp@latest`: the Slorp executable contains the plugin files. Installation writes a managed package to `~/.slorp/plugins/opencode` (or under `-data`) and uses `opencode plugin … --global` to register both its server and terminal entry points. The managed package uses the address Slorp is listening on, including a custom `-addr` port. Use `-opencode` if the executable is not on Slorp's PATH.

Settings distinguishes installed files from a reporting bridge and a running session that needs restarting. When a newer Slorp executable carries different plugin files, it offers **Update**. **Uninstall** removes the managed package's configuration references; open sessions unload it after restarting. Managed files are retained for reinstallation.

OpenCode 1.19.6 can delay its first startup after plugin installation by about a minute while recovering a dependency-install lock.

### Development checkout

For development, register this repository's plugin directory instead:

```sh
opencode plugin /path/to/slorp/integrations/opencode --global
```

That registers the package in the global `opencode.json` and `tui.json`. Slorp recognizes this local installation and keeps it separate from a managed copy. If it was installed before the terminal companion was added, Settings offers **Finish setup** to register the missing entry point. Restart OpenCode afterwards.

After updating the plugin, restart OpenCode again to load the changes. The live-session features use OpenCode's 1.19.x API and require an updated Slorp service as well.

Older installations copied a single `slorp.ts` into OpenCode's plugins folder. Slorp detects those, but they do not include terminal discovery. Remove that old file or configuration reference before installing the package to avoid duplicate bridges.

## Config

The service address defaults to `http://127.0.0.1:4747`. Set `SLORP_URL` to change it, or give the plugin a `url` option:

```json
{ "plugin": [["/path/to/slorp/integrations/opencode", { "url": "http://127.0.0.1:4747" }]] }
```

`SLORP_URL` wins when both are set.

## Develop

```sh
npm install
npm run check
npm test
```

Tests run on Node 22.6 or later.

The opt-in runtime smoke test needs Python 3, Go, and OpenCode on macOS or Linux:

```sh
python3 integrations/opencode/smoke.py
python3 integrations/opencode/history-smoke.py
python3 integrations/opencode/history-smoke.py --overflow
python3 integrations/opencode/history-smoke.py --overflow --replay-last
python3 integrations/opencode/subagents-smoke.py
python3 integrations/opencode/lifecycle-smoke.py
python3 integrations/opencode/attach-smoke.py
python3 integrations/opencode/quotas-smoke.py
SLORP_NATIVE_TEST=1 go test -race ./internal/opencode -run Native -count=1
```

Run these from the repository root. The first builds a standalone Slorp executable, installs the bundled plugin into isolated configuration, opens two idle conversations in pseudoterminals, and checks discovery, restart recovery and retirement. The history tests import synthetic native-format transcripts into an isolated OpenCode database, export them with the real CLI, and backfill twice through Slorp's HTTP API. They cover ordinary continuation and overflow replay, including a large export. The subagent test imports a nested task tree and uses native OpenCode API updates to verify live completion, background results, usage, restart recovery and archived reports without OpenCode running.

The lifecycle smoke uses a loopback fixture provider to exercise actual prompts, replies, stop, same-ID reopening and process cleanup. The attach smoke runs an authenticated server and separate terminal processes, checking idle discovery, switching/home, independent selections, reconnect recovery, unchanged native records, and retirement when the server stops. The quota smoke exercises OpenCode 1.19.6's actual OAuth loaders and Go key handling with synthetic credentials and loopback responses, including throttling and secret-free API/database output; it does not validate a real account's current endpoint response. Native Go tests compare ordinary and managed configuration behavior, including instructions, skills, custom tools and defaults. All use isolated OpenCode/Slorp data; no paid provider calls are made. Set `SLORP_SMOKE_KEEP=1` to retain Python smoke diagnostics and `TMPDIR` to choose their temporary parent.
