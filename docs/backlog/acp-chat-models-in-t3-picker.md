# Offer chat models in the T3 Code model picker for the loopai provider

- found: 2026-10-09, plan: acp-thread-chat, phase: planning
- severity: minor
- area: cmd/loopai-acp/main.go

T3 Code locks a thread to its provider ("Claude is unavailable in this thread. Start a new thread to
switch providers"), but lets the user switch models within the provider. `loopai-acp models`
(`cmd/loopai-acp/main.go:72`) advertises a single `loopai` model, and `pkg/acp/server.go:91` answers
`session/set_model` with `{}` without recording the choice.

The ACP thread chat routes non-launch messages to `review_model`. An optional follow-up: advertise
extra models (for example `claude-chat`, `codex-chat`) from the `models` probe, record the
`session/set_model` choice per session, and route every message under a chat model to that provider
instead of `review_model`. This depends on undocumented Grok-driver behavior (how the probe output is
parsed, the `set_model` payload), so start with a `--debug` spike against the current T3 Code build.
