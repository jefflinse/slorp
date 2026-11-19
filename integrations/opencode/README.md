# Slorp for OpenCode

An OpenCode plugin that connects the sessions of an OpenCode instance to the local [Slorp](../../) service. It includes a server bridge and a small terminal companion.

- Reports each active top-level session's turns, title, directory, branch, model and reasoning variant, token usage, estimated cost, and context use. Existing message usage is read when the session is first observed; repeated message updates do not count twice. Context capacity becomes available when OpenCode makes a model request.
- Shows retries and failures, and marks compactions in the timeline.
- Delivers replies written in Slorp as your own prompts, preserving the selected agent, provider, model and reasoning variant.
- Offers permission prompts and structured questions, including subagents', in Slorp as well as OpenCode. Questions support single and multiple choices, custom text when allowed, and dismissal. Whichever you answer first decides it. Permissions allow one call or reject it; they never allow "always". Concurrent requests under one top-level session are presented one at a time.
- Repeats the current state on every poll and restores outstanding prompts after Slorp restarts. Failed reply acknowledgements and prompt settlements are retried without resubmitting your reply.

OpenCode's session list includes saved conversations, rather than a roster of open sessions, so the plugin is Slorp's view of what is live. In the local terminal, a conversation appears as soon as you open or resume it, even while idle. Opening it preserves the original exchange timestamps. Conversations observed by an instance remain listed until its bridge stops reporting; they leave about 30 seconds later. Unopened saved conversations are not listed.

The terminal companion announces the selected session to the server bridge over an in-process channel. This supports the normal local `opencode` terminal; `opencode attach` uses a separate server process and currently still needs activity for discovery. Usage counts the top-level conversation; child-session usage and reports, historical timeline backfill, and starting sessions from Slorp are still to come. Provider account usage limits are not reported.

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
```

Run it from the repository root. It builds a standalone Slorp executable, installs the bundled plugin into isolated configuration, opens two idle conversations in pseudoterminals, and checks discovery, restart recovery and retirement. It makes no model calls and does not use your OpenCode or Slorp data.
