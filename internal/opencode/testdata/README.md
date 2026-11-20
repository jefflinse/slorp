# OpenCode history fixtures

These are synthetic conversations in OpenCode 2.3.2's native transcript format, verified against isolated imports and exports. They contain no user session data. Messages have flat `id`, `type`, and `time` fields; users carry `text`, assistants carry `content`, and compactions are standalone messages.

- `history.json`: sequential replies, private reasoning, synthetic continuation, an incomplete assistant, completed/failed/running compactions, and an unanswered repeated prompt.
- `overflow.json`: an automatic compaction checkpoint and synthetic continuation, followed by an assistant reply to the original prompt and a distinct repeated user request.

The Go tests exercise parsing, pairing, filtering, clipping, limits, and cancellation. `integrations/opencode/history-smoke.py` constructs its own native transcript, imports it into an isolated OpenCode database, and verifies export, message pagination, and idempotent Slorp HTTP backfill. `--auto-compaction` selects an automatic checkpoint with synthetic continuation; `--single-followup` uses one post-checkpoint user turn instead of repeated prompts. `--revert` stages a native revert before the final repeated turn and cannot be combined with `--single-followup`. `--native-only` skips Slorp build and backfill verification.
