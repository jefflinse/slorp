# OpenCode history fixtures

These are synthetic conversations in OpenCode 1.19.6's export format, modeled on an actual export and the native compaction/revert implementations. They contain no user session data.

- `history.json`: parent-linked replies, injected/ignored text, tool output, a completed compaction, synthetic continuation, an unanswered repeated prompt, and a foreign-session message.
- `overflow.json`: automatic overflow compaction replaying text, media, an agent mention and synthetic instructions, followed by a genuine repeated request.

The Go tests use minimal metadata. `integrations/opencode/history-smoke.py` supplies the remaining native metadata, imports the fixtures into an isolated OpenCode database, and verifies the real export and Slorp HTTP backfill paths. `--overflow --replay-last` additionally tests a session reopened after replay without a subsequent genuine user turn.
