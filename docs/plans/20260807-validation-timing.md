# Validation Command Timing

## Overview

Log how much wall-clock time each run spends executing the plan's validation commands (tests, linters) inside executor sessions, for both Claude and Codex.

Today `phase durations:` aggregates only loopai-emitted section boundaries (tasks, internal review, external review, evaluation, planning, other). Everything inside an executor session is opaque: `parseStream` extracts only text blocks from the Claude stream, and the Codex rollout parser intentionally drops all `function_call` records, so loopai never sees that `go test ./...` ran, let alone how long it took.

This plan adds:

- executor-level command timing: both executors emit a `(command, duration)` event for every completed shell command in the session (Claude: `tool_use`/`tool_result` pairs linked by `tool_use_id`; Codex: rollout `function_call`/`function_call_output` pairs linked by `call_id`)
- a matcher that classifies a command as validation when it equals, or starts with (token-boundary), an entry from the plan's `## Validation Commands` section
- a `ValidationTimer` in `pkg/progress` that logs each matched run as it completes (`validation: go test ./... took 1m12s`) and prints an aggregate next to the phase summary (`validation: 14m32s (23 runs)`)

Validation time is included in (not additive to) the phase buckets; with parallel review agents the sum may exceed section wall-clock. Runs with zero matched commands print no aggregate line. Providers whose streams carry no tool events degrade silently to zero.

## Context (from discovery)

- Files/components involved:
  - `pkg/executor/executor.go` — `parseStream` (~line 456) and `extractText`: only text blocks are consumed; `tool_use`/`tool_result` blocks are currently ignored
  - `pkg/executor/codex.go` — `formatRolloutEvent` (~line 798) drops `function_call` records by design; rollout tail (`startRolloutTail`) already delivers events live with a final drain after exit
  - `pkg/progress/section_timer.go` — `SectionTimer` pattern to mirror: wraps `SectionLogger`, injectable `now()`, `FinishRun` prints the summary
  - `pkg/plan` — plan parsing; needs extraction of the `## Validation Commands` section entries
  - `cmd/loopai/main.go` — logger chain assembly and `runWithSectionTiming`; executors are constructed in the executor factory and support optional handlers (`OutputHandler` pattern)
- Related patterns found:
  - `OutputHandler` callback on executors is the model for a new optional `CommandTimingHandler`
  - `SectionTimer` placement in the logger chain (below cmux wrapper, above dashboard broadcast) documented in `CLAUDE.md`
  - executor tests use synthetic stream fixtures
- Dependencies identified: none new

## Development Approach

- **Testing approach**: TDD (write failing tests first, then implement to green)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility: with no handler wired or no `## Validation Commands` section, executor and log output are unchanged
- cmux reporting and dashboard behavior must be unaffected; the new timer must not disturb the documented logger chain order
- Tests must redirect HOME/config paths to `t.TempDir()` and never touch real `~/.config/loopai/` or legacy `~/.config/ralphex/`
- No changes to `pkg/status` protocol signals
- Keep comments lowercase except exported godoc; table-driven tests with testify

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: no web changes planned; if the dashboard replay parser needs to tolerate the new `validation:` log lines, cover that with unit tests rather than new e2e
- Executor timing is tested with synthetic stream fixtures and injectable clocks; no real provider processes

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## Validation Commands

- `make test`
- `make lint`

## Implementation Steps

### Task 1: Extract validation commands from plan and add matcher

- [x] write failing tests in `pkg/plan`: extract entries from `## Validation Commands` (list markers and backticks stripped, whitespace normalized); section missing → empty list; section empty → empty list; non-list content ignored
- [x] implement `ValidationCommands` extraction in `pkg/plan`
- [x] write failing table-driven tests for the matcher: exact match; prefix match with token boundary (`go test ./...` matches `go test ./... -count=1`); `make test` does NOT match `make test-wrappers`; backtick/whitespace normalization; empty command; empty entry list
- [x] implement the matcher as a pure function (in `pkg/plan` or `pkg/progress`, wherever it avoids import cycles)
- [x] run `go test ./pkg/plan/...` - must pass before task 2

### Task 2: Command timing events from the Claude executor

- [x] write failing tests for `parseStream` with synthetic stream-json fixtures: `tool_use` (name Bash, input.command) paired with `tool_result` by `tool_use_id` emits `(command, duration)` via the handler; overlapping concurrent pairs resolve independently; `tool_result` without a matching id is ignored; unclosed `tool_use` at stream end emits nothing; non-Bash tools emit nothing; nil handler keeps current behavior byte-for-byte
- [x] add optional `CommandTimingHandler func(command string, d time.Duration)` to `ClaudeExecutor` following the `OutputHandler` pattern
- [x] extend stream parsing to decode `tool_use` blocks in assistant events and `tool_result` blocks, with an injectable `now()` for arrival-time stamping
- [x] verify existing executor tests pass unmodified
- [x] run `go test ./pkg/executor/...` - must pass before task 3

### Task 3: Command timing events from the Codex executor

- [x] inspect the rollout JSONL format for native per-event timestamps; if present use them for durations, otherwise use tail arrival time and document Codex durations as approximate (final drain can flush events late)
- [x] write failing tests with synthetic rollout fixtures: `function_call` (exec_command) paired with `function_call_output` by `call_id` emits `(command, duration)`; command text extracted from exec_command arguments; unmatched outputs ignored; unclosed calls emit nothing; assistant-message rendering (`formatRolloutEvent`) unchanged; nil handler keeps current behavior
- [x] add the same optional `CommandTimingHandler` to `CodexExecutor` and extend the rollout parser to track call pairs without changing what is forwarded to `OutputHandler`
- [x] run `go test ./pkg/executor/...` - must pass before task 4

### Task 4: ValidationTimer aggregation and wiring

- [x] write failing tests for `ValidationTimer` in `pkg/progress`: matched command completion logs `validation: <command> took <duration>`; aggregate line `validation: <total> (<N> runs)` on finish; zero matched runs → no aggregate line; unmatched commands ignored; fixed handler durations replace a fake clock because executors supply elapsed durations; concurrent handler calls are safe
- [x] implement `ValidationTimer` alongside `SectionTimer`: constructed with the plan's validation command list and a logger, exposes the handler func and a `FinishRun`-style summary method
- [x] wire it in `cmd/loopai`: build from the selected plan, attach the handler to both executors via the factory, emit the summary immediately after `runWithSectionTiming` closes the section timer; modes without a plan (e.g. `--review` without validation section) run with the feature disabled
- [x] verify the logger chain order from `CLAUDE.md` is preserved (cmux wrapper outermost interfaces intact, dashboard broadcast unaffected); add a unit test if the dashboard replay parser must tolerate the new lines
- [x] run `go test ./cmd/... ./pkg/...` - must pass before task 5

### Task 5: Verify acceptance criteria

- [x] verify all requirements from Overview: per-run lines and aggregate for Claude and Codex, token-boundary matching, silent degradation without tool events or validation section
- [x] verify edge cases: overlapping parallel agent commands (sum may exceed wall-clock — documented), session death mid-command, `--tasks-only`/`--external-only` runs, plan without validation section
- [x] run full test suite via `make test`
- [x] run `make lint` - all issues must be fixed
- [x] cross-compile check: `GOOS=windows GOARCH=amd64 go build ./...`
- [x] verify test coverage of new code paths meets project standard

### Task 6: Update documentation

- [x] update `CLAUDE.md`: command timing events, `ValidationTimer` placement in the logger chain, overlap semantics
- [x] update `llms.txt`: progress log now includes `validation:` per-run lines and the aggregate summary
- [x] update `README.md` user documentation for the new progress output
- [x] update `docs/custom-providers.md` with optional timing event shapes and wrapper degradation
- [x] godoc on new exported types documents the overlap caveat and approximate Codex timing (if arrival-time fallback is used)

### Task 7: Address code review findings

- [x] support legacy and current Codex rollout command formats, yielded sessions, child-agent rollouts, final unterminated records, and invalid timestamps
- [x] omit background Claude commands and log canonical validation labels without raw arguments or control characters
- [x] remove duplicate plan scanning and validation-only runtime plumbing from unrelated modes
- [x] add focused cancellation, integration, long-line, multiline, and provider-format regression tests
- [x] infer safe output-only Codex completions before the yield threshold and attach ambiguous calls to later continuations
- [x] complete user, custom-provider, architecture, plan, and changelog documentation

## Technical Details

- Event shape: `(command string, duration time.Duration)`; executors do not classify — all completed shell commands are reported, the `ValidationTimer` filters by the plan's list
- Claude pairing: `tool_use` block (name `Bash`, `input.command`) → `tool_result` with same `tool_use_id`; arrival-time stamping with injectable `now()`
- Codex pairing: legacy rollout `function_call` (`exec_command`) or current custom `exec` tool call → matching output; yielded sessions remain pending through continuation/wait records, and child-agent rollouts are followed; prefer valid native rollout timestamps
- Matching rule: normalized command equals a validation entry, or starts with entry + whitespace separator (token boundary); entries come from `## Validation Commands` list items with backticks stripped
- Per-run log line: `validation: go test ./... took 1m12s`; aggregate: `validation: 14m32s (23 runs)` printed after `phase durations:`
- Validation time overlaps phase buckets by design; it is reported as a separate line, never added to the phase sum
- Wrapper providers (`copilot-as-claude`, `pi-as-claude`) that do not emit tool events degrade to zero matched runs; note it in `docs/custom-providers.md` if their docs list supported features
- No changes to protocol signals, module path, or notification behavior

## Post-Completion

**Manual verification** (deferred; automated fixtures cover both provider formats without spending live-provider quota):

- [ ] run a real plan on a toy repo (`make e2e-prep`) with `go test ./...` in Validation Commands and confirm per-run lines and the aggregate appear in `.loopai/progress/`
- [ ] repeat with `--codex` and compare aggregate plausibility against phase durations
- [ ] run once through `pi-as-claude` or `copilot-as-claude` to confirm silent degradation (no validation lines, no errors)

**External system updates**: none — feature is fully local to loopai
