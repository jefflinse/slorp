# Browser tests

These tests load Slorp's real page in headless Chromium against small fixture HTTP services. They exercise question answers, plugin setup, and history capabilities, loading states, and failures, along with effort cards. They do not start agent sessions or use your Slorp database.

From this directory:

```sh
npm ci
npx playwright install chromium
npm test
```

The Go HTTP tests cover bridge-to-hub behavior; `integrations/opencode` contains the plugin's SDK and reconnect tests.
