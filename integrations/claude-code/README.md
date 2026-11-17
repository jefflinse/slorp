# Slorp for Claude Code

A Claude Code plugin that connects each session to the local [Slorp](../../) service.

- Reports each turn as it starts and ends, so Slorp can show what the session is doing and what it last said.
- Delivers replies written in Slorp to the session as your own prompts.
- Offers each permission prompt to Slorp as well as the terminal. Whichever you answer first decides it. Slorp can allow or deny a single call; questions and plan approvals are shown in Slorp but answered in the terminal.

Slorp finds the sessions themselves with `claude agents --json`, so a session without the plugin is still listed. It just cannot be replied to or answered from Slorp.

## Install

```sh
claude plugin install slorp --marketplace https://github.com/jefflinse/slorp.git
```

Then restart Claude Code, or run `/reload-plugins` in an open session.

To update:

```sh
claude plugin marketplace update slorp
claude plugin update slorp@slorp
```

## Config

| Key | Default | Notes |
|---|---|---|
| `url` | `http://127.0.0.1:4747` | Where the Slorp service listens. Env: `SLORP_URL`, which wins when set. |

Set it at install time with `--config url=...`, or later with `/plugin configure slorp@slorp`.

## Develop

```sh
claude --plugin-dir ./integrations/claude-code
```

The folder is watched, so saving `hooks/register.ts` reloads the plugin in that session. `claude plugin validate ./integrations/claude-code` checks the manifest and the hooks module.
