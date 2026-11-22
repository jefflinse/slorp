# Browser tests

These tests load Slorp's real page in headless Chromium against small fixture HTTP services. They exercise question answers, plugin setup, history and subagent reports, effort cards, a session's side pane, the local model's log and prompts, handoffs, and themes. Lifecycle tests cover starting from Sessions or an effort, directory-specific OpenCode choices, stopping/reopening, resume-command copying, and recovery from accepted-but-lost responses using journaled request IDs. They do not start agent sessions or use your Slorp database.

From this directory:

```sh
npm ci
npx playwright install chromium
npm test
```

Provider-limit tests cover source attribution, status text, stale readings and time-based expiry without new server events. The Go HTTP tests cover bridge-to-hub behavior; `integrations/opencode` contains the plugin's SDK, reconnect and provider-response tests.
