# Slorp for OpenCode

An OpenCode plugin that connects the sessions of an OpenCode instance to the local [Slorp](../../) service.

- Reports each top-level session once something happens in it: its turns as they start and end, and its title, directory and branch.
- Delivers replies written in Slorp to the session as your own prompts, under the agent and model the session last used.
- Offers each permission prompt, a subagent's included, to Slorp as well as OpenCode's own dialog. Whichever you answer first decides it. Slorp can allow a single call or reject it; it never allows "always". Questions are shown in Slorp but answered in OpenCode.

OpenCode has no command that lists its open sessions, so the plugin is Slorp's only view of them. A session appears in Slorp at its first prompt after OpenCode starts, and leaves about 30 seconds after OpenCode quits.

## Install

From a checkout of this repository:

```sh
opencode plugin /path/to/slorp/integrations/opencode --global
```

That adds the folder to `plugin` in `~/.config/opencode/opencode.json`, so pulling the repository updates the plugin. Restart OpenCode afterwards.

Without a checkout, the plugin is one file with no dependencies, and OpenCode loads every file in its plugins folder:

```sh
curl -fsSL https://raw.githubusercontent.com/jefflinse/slorp/main/integrations/opencode/slorp.ts \
  -o ~/.config/opencode/plugins/slorp.ts
```

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
