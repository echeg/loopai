# Codex rollout usage fixtures

Codex prints no token usage on stdout or stderr when it runs without `--json`, so
`CodexExecutor` reads usage from the session rollout file
(`$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<timestamp>-<session id>.jsonl`). These
fixtures are synthetic, but keep the record shapes and the arithmetic of a rollout
written by codex-cli: `total_tokens == input_tokens + output_tokens`, and
`cached_input_tokens` and `cache_write_input_tokens` are counted inside
`input_tokens`. Unrelated metadata is minimized. `{{SESSION_ID}}` and `{{CHILD_ID}}`
are replaced by the tests when they copy a fixture into a temporary `CODEX_HOME`.

| Record | Shape | Use |
| --- | --- | --- |
| `turn_context` | `payload.model`, plus settings such as `summary` as a string | The model the session ran. Decoded separately because its `summary` is not a reasoning summary list. |
| `event_msg` `token_count` | `payload.info.total_token_usage.{input_tokens, cached_input_tokens, cache_write_input_tokens, output_tokens, reasoning_output_tokens, total_tokens}`; `info` is `null` before the first response | Running session totals: the last record wins. `InputTokens` is normalized to `input_tokens - cached_input_tokens - cache_write_input_tokens`. |
| `event_msg` `task_complete` | `payload.last_agent_message` | Starts the exit grace; the tests use it to know the tail has read the records before it. |

- `usage-rollout.jsonl`: a main session with a `null` `token_count`, two totals
  records, and a completion.
- `no-usage-rollout.jsonl`: a main session whose only `token_count` record has
  `info: null`.
- `usage-child-rollout.jsonl`: a subagent rollout whose `session_meta` names the
  main session as its parent. Its totals belong to the child and must not be
  counted for the main session. Its one timed `exec_command` lets a test prove the
  child rollout was read.
