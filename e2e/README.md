# Browser tests

These tests load Slorp's real page in headless Chromium against a small fixture HTTP service. They exercise question selection, custom text, rerendering, dismissal, and the Claude Code answer format. They do not start agent sessions or use your Slorp database.

From this directory:

```sh
npm ci
npx playwright install chromium
npm test
```

The Go HTTP tests cover bridge-to-hub behavior; `integrations/opencode` contains the plugin's SDK and reconnect tests.
