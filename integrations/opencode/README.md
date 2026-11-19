# Slorp for OpenCode

An OpenCode plugin that connects the sessions of an OpenCode instance to the local [Slorp](../../) service. It includes a server bridge and a small terminal companion.

- Reports each active top-level session's turns, title, directory, branch, model and reasoning variant, token usage, estimated cost, and context use. Existing message usage is read when the session is first observed; repeated message updates do not count twice. Context capacity becomes available when OpenCode makes a model request.
- Shows retries and failures, and marks compactions in the timeline.
- Shows nested subagents with their task, live status, current tool or wait, timing, model, reasoning variant, tools, token usage and cost. Task prompts and returned reports remain available after the session closes or Slorp restarts.
- Fills in earlier history on request: the latest forty exchanges and compaction markers from the local OpenCode record, with summaries written by Slorp's local model. Replies are paired with their original prompts; internal summaries, reasoning, tool output, automatic replay turns, and reverted content are excluded.
- Delivers replies written in Slorp as your own prompts, preserving the selected agent, provider, model and reasoning variant.
- Offers permission prompts and structured questions, including subagents', in Slorp as well as OpenCode. Questions support single and multiple choices, custom text when allowed, and dismissal. Whichever you answer first decides it. Permissions allow one call or reject it; they never allow "always". Concurrent requests under one top-level session are presented one at a time.
- Repeats the current state on every poll and restores outstanding prompts after Slorp restarts. Failed reply acknowledgements and prompt settlements are retried without resubmitting your reply.

OpenCode's session list includes saved conversations, rather than a roster of open sessions, so the plugin is Slorp's view of what is live. In the local terminal, a conversation appears as soon as you open or resume it, even while idle. Opening it preserves the original exchange timestamps. Conversations observed by an instance remain listed until its bridge stops reporting; they leave about 30 seconds later. Unopened saved conversations are not listed.

The terminal companion announces the selected session to the server bridge over an in-process channel. This supports the normal local `opencode` terminal; `opencode attach` uses a separate server process and currently still needs activity for discovery. Session usage includes descendants once; each subagent's usage counts only its own work. Starting sessions from Slorp and provider account usage limits are not yet supported.

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
```

Run these from the repository root. The first builds a standalone Slorp executable, installs the bundled plugin into isolated configuration, opens two idle conversations in pseudoterminals, and checks discovery, restart recovery and retirement. The history tests import synthetic native-format transcripts into an isolated OpenCode database, export them with the real CLI, and backfill twice through Slorp's HTTP API. They cover ordinary continuation and overflow replay, including a large export. The subagent test imports a nested task tree and uses native OpenCode API updates to verify live completion, background results, usage, restart recovery and archived reports without OpenCode running. None makes model calls or uses your OpenCode or Slorp data.
