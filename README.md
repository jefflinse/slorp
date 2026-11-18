# slorp - organize your slop

Slorp helps make sense of the dozens of agent sessions running on your machine.

![Slorp's Attention view: the sessions that need you on the left, one of them open with its question, suggested replies and timeline](docs/slorp-screenshot.png)

## Getting Started

You need [Go](https://go.dev/dl/) 1.27 or later and [Claude Code](https://claude.com/claude-code).

1. Install Slorp.

   ```sh
   go install github.com/jefflinse/slorp@latest
   ```

2. Run it and open <http://127.0.0.1:4747>.

   ```sh
   slorp
   ```

3. If Claude Code doesn't have the Slorp plugin yet, the page says so and offers to install it. Sessions already open load it after `/reload-plugins` or a restart.

To try Slorp without installing it, `go run github.com/jefflinse/slorp@latest` does the same as steps 1 and 2.

Slorp lists Claude Code sessions with or without the plugin. The plugin is what lets you reply to a session and answer its permission prompts from the page. See [its README](integrations/claude-code/README.md) to install it by hand or configure it.

Summaries, suggested replies, automatic filing and wrap-ups are written by a local model. Slorp looks for an OpenAI-compatible LLM server, such as LM Studio, Ollama or llama.cpp, at `http://localhost:1234` and uses the model it has loaded. Settings on the page points it at another server, names the model where a server offers several, turns it off, and checks that it answers. `-llm` and `-model` do the same when Slorp starts, and stand over what Settings holds for as long as it runs. Without a model Slorp still lists your sessions and lets you answer them, and the page marks what is missing. `slorp -h` lists the other options.

OpenCode sessions are supported through [a plugin of their own](integrations/opencode/README.md).

## Features

- **Every session on one page.** Each open Claude Code and OpenCode session with what it is doing, what it last said, its model, how full its context is, and what it has used.
- **Attention.** The sessions that need you, whether for a question, a permission prompt or finished work to look at, each summarized by a local model.
- **Answer from the page.** Reply to a session, or allow or deny a permission prompt, without finding its terminal.
- **Efforts and roles.** Sessions are filed under the efforts they belong to, by a directory rule, by you, or by the model's guess. A role marks a kind of session, such as a reviewer on a loop, and how much of your attention it may ask for.
- **Timelines and wrap-ups.** What you asked each session and how it came out, and an account of the day by effort.
- **Closed sessions and search.** Sessions are kept after they close, and `⌘K` searches all of them.
- **Start, stop and reopen.** Claude Code background sessions can be started under an effort, stopped, and reopened from the page.
- **One conversation, one session.** A Claude Code conversation keeps its name, effort, role and timeline when it is cleared, handed off or moved to the background.
- **Pull requests.** With the GitHub integration turned on, your pull requests and review requests are listed, matched to the sessions working on them, and raised when one needs you.
- **Usage.** Tokens, estimated cost and your usage limits, as the agents report them.
- **Local.** Slorp listens only on this machine and keeps what it knows in `~/.slorp`.
