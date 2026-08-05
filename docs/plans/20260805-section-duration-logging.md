# Per-section duration logging and phase summary

## Overview

- Progress logs mark section starts (`--- task iteration 1 ---`) but never record durations; run-time analysis requires an external parser. Add automatic timing: when a section closes (the next `PrintSection` arrives or the run ends), emit a normal timestamped line `task iteration 1 took 3m38s`.
- At run end, emit a one-line phase summary: `phase durations: tasks 32m26s (8), internal review 2h23m (10), external review 42m16s (7), evaluation 31m21s (7)` — zero-count buckets omitted. `(N)` counts section occurrences (a retried `task iteration 3` counts twice).
- Everything is derived from the existing structured `status.Section` values flowing through `PrintSection`; no phase engine changes, no header-format changes, old logs remain parseable.
- Design spec: `docs/superpowers/specs/2026-08-05-section-timing-design.md`.

### Acceptance criteria

- Every section that is followed by another section or by run completion gets a `<label> took <dur>` line in the progress file, on stdout, and in the live dashboard stream.
- The summary line appears once per run — in the main execution flow after `Runner.Run` returns (success, failure, and user abort), above the `DIFFSTATS:` line and the `Completed:`/`Failed:` footer block; in the interactive plan-creation flow right after its `Run` returns (not adjacent to the footer — accepted).
- Buckets map by `SectionType`; only non-empty buckets are printed. Bucket sums do NOT add up to the footer `Elapsed()`: time before the first section is unattributed — documented, not a bug.
- cmux rate-limit enrichment (`LogLimitWait`/`LogLimitRecovery` optional-interface discovery on the outermost logger) keeps working — the cmux wrapper stays outermost in the chain.
- Durations are wall-clock (rate-limit waits included); format follows `Elapsed` conventions (>=1h truncated to minutes, else seconds).
- Known cosmetic mismatch, accepted and documented: live SSE tags the `took` line with the NEW phase (phase holder is set before `PrintSection`, `pkg/web/broadcast_logger.go:63`), while dashboard file replay attributes it to the PREVIOUS section (`pkg/web/parse.go` phaseFromSection). The line is visible in both views, just under different phase filters.

## Context (from discovery)

- `pkg/progress/progress.go` — `PrintSection` (~349) writes `\n--- label ---\n`; `Elapsed` (~614) defines duration format conventions; `Complete`/`Fail` footers written on Close (~646-649); `LogDiffStats` writes the `DIFFSTATS:` line via `displayStats` (main.go ~663) after the summary point.
- `pkg/status/section.go` — `Section{Type, Iteration, Label}` with `SectionTaskIteration`, `SectionInternalReview`, `SectionExternalReviewIteration`, `SectionExternalEvaluation`, `SectionPlanIteration`, `SectionGeneric`, `SectionCustomIteration`. Note: the chain-header `external review (label)` (external_review.go:99) is a `SectionGeneric` and lands in the `other` bucket — documented choice.
- `pkg/cmux/cmux.go` — `Logger` interface (~401-410) and `WrapLogger` (~414); its `reportingLogger` adds optional `LogLimitWait`/`LogLimitRecovery`, discovered by retryPolicy via type assertion on the **outermost** logger (`pkg/processor/execution_policy.go:137,151`) — the timer must sit **below** it.
- Import cycle constraint: `pkg/cmux` → `pkg/plan` → `pkg/progress`, and `pkg/processor` → `pkg/plan`. Therefore the timer's interface must be a **local mirror** in `pkg/progress` (cannot reference `cmux.Logger`/`processor.Logger`), and compile-time compatibility assertions must live in `cmd/loopai` tests, which already import all three.
- `cmd/loopai/main.go` — runner chain: dashboard `Start` returns `runnerLog` (~756), then `runnerLog = rep.WrapLogger(runnerLog)` (~758); timer goes between. `r.Run` at ~804 is `if runErr := r.Run(ctx); runErr != nil {` with early returns for abort (~812) and failure (~820) — `runErr` must be hoisted for a single `FinishRun` call site. `keepDashboardAlive` (~699-703) calls `closeLog()` explicitly on `--serve`, so `defer` for the summary is FORBIDDEN (would write to a closed log). Plan-creation chain: `planLog = rep.WrapLogger(baseLog)` (~2042), its `r.Run` at ~2102, flow continues into full implementation afterwards with early returns at ~2123 and ~2133.
- `cmd/loopai/main_test.go` — there are NO existing chain-order tests (the only `executePlan` test forces a dashboard-bind failure); a small seam must be extracted to make the wiring testable.
- `pkg/web/broadcast_logger.go` (~61-64), `pkg/web/parse.go` (phaseFromSection), `pkg/web/static/app.js` phase filter — background for the accepted live/replay phase-tag mismatch.

## Development Approach

- **Testing approach**: Regular (implementation with tests in the same task), table-driven with `testify`.
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task; tests cover both success and error scenarios.
- **CRITICAL: all tests must pass before starting next task** — no exceptions.
- **CRITICAL: update this plan file when scope changes during implementation.**
- One `_test.go` file per source file: new wrapper in `pkg/progress/section_timer.go` + `pkg/progress/section_timer_test.go` (the duration-format helper lives in `section_timer.go`, so its boundary tests belong in `section_timer_test.go`).
- Fake clock via injected `now func() time.Time`; never sleep in tests.
- Maintain backward compatibility: header format, web dashboard parsing, and cmux behavior unchanged.

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above).
- **E2E tests**: dashboard Playwright tests do not assert on log line content; no e2e changes expected. `make test` (asset checks, race-enabled suite, wrapper suites) must pass.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code changes, tests, documentation updates in this repo.
- **Post-Completion** (no checkboxes): manual smoke run.
- Checkboxes belong only in Task sections.

## Implementation Steps

### Task 1: SectionTimer wrapper in pkg/progress

- [x] create `pkg/progress/section_timer.go`: exported interface `SectionLogger` (local mirror of the shared logger method set: Print, PrintRaw, PrintSection, PrintAligned, LogQuestion, LogAnswer, LogDraftReview, Path) with a comment documenting the import cycle (`cmux`/`processor` → `plan` → `progress`) as the reason the mirror exists and that drift is caught by compile-time assertions in `cmd/loopai` tests
- [x] `SectionTimer` struct: embeds/forwards `SectionLogger`, guarded by a `sync.Mutex` (the suite is race-enabled; the mutex is cheap — unconditional, not "if needed"); constructor `NewSectionTimer(inner SectionLogger, now func() time.Time)` with nil `now` defaulting to `time.Now`
- [x] `PrintSection`: when a section is open, emit the closing line via `inner.Print("%s took %s", label, dur)` (printf-safe — labels can contain `%` from reviewer names), add duration to the `SectionType` bucket, then forward the new header; first section just records the start
- [x] `FinishRun()` (named to avoid colliding with `cmux.Reporter.Finish` semantics): close the last open section (emit its `took` line), then emit `phase durations: ...` with non-empty buckets in fixed order (tasks, internal review, external review, evaluation, planning, other); idempotent; silent no-op when no sections were ever seen
- [x] duration-format helper in `section_timer.go` following `Elapsed` conventions (>=1h → `1h23m`, else `5m30s`/`45s`); `Elapsed` delegates to the helper so section and footer formatting cannot drift
- [x] write tests in `pkg/progress/section_timer_test.go`: fake-clock sequences → exact emitted lines and summary; single section closed by `FinishRun`; `FinishRun` idempotence; `FinishRun` with zero sections emits nothing; bucket-mapping table covering every `SectionType` (incl. `SectionGeneric` "external review (...)" landing in other); format boundaries (59s, 61s, 1h+); one smoke assertion that a forwarded method delegates (no 7-method table — embedding covers the rest)
- [x] run `go test ./pkg/progress/...` — must pass before task 2

### Task 2: Wire SectionTimer into the runner and plan-creation chains

- [x] extract a small seam in `cmd/loopai/main.go`: `buildRunnerLogger(rep *cmux.Reporter, inner progress.SectionLogger-compatible) (outermost processor.Logger, *progress.SectionTimer)` that applies timer-then-cmux order; use it in the runner path (replacing the inline `runnerLog = rep.WrapLogger(runnerLog)` at ~758)
- [x] runner path `FinishRun` wiring: hoist the run error out of the `if` at ~804 — `runErr := r.Run(ctx); timer.FinishRun(); if runErr != nil { ... }` — a single call site covering success, failure, and user abort; `defer` is FORBIDDEN (`keepDashboardAlive` closes the log explicitly at ~703 before deferred calls would fire)
- [x] plan-creation path: wrap `baseLog` with the timer before `rep.WrapLogger` (~2042) and call `FinishRun()` immediately after its `r.Run(ctx)` returns (~2102), before the error branch — NOT "at flow completion" (the flow continues into full implementation; early returns at ~2123/~2133 must not skip the summary)
- [x] compile-time interface assertions in `cmd/loopai` tests: `var _ processor.Logger = ...` / `var _ cmux.Logger = ...` on the timer-wrapped chain (this is where the import cycle allows it)
- [x] write tests against the new seam: recording inner logger receives `took` lines and summary in order; the outermost logger still satisfies the rate-limit optional interface (`_, ok := out.(interface{ LogLimitWait(string, string, string) })` with a non-nil reporter); nil reporter returns the timer itself unchanged
- [x] run `go test ./cmd/... ./pkg/progress/...` — must pass before task 3

### Task 3: Verify acceptance criteria

- [x] verify every item in Overview → Acceptance criteria (including the documented live/replay phase-tag mismatch and the unattributed pre-first-section time)
- [x] verify old progress files (no `took` lines) are unaffected anywhere logs are read (dashboard file replay renders new lines as plain text; `taskIterationRegex`/`sectionRegex`/`diffStatsPattern` are anchored and unaffected)
- [x] run full test suite: `make test` — must pass
- [x] run `make lint` — all issues fixed
- [x] cross-compile check: `GOOS=windows GOARCH=amd64 go build ./...`

### Task 4: [Final] Update documentation

- [x] README.md: document the `took` lines and the `phase durations:` summary in the progress-log section, including: `(N)` = section occurrences, bucket sums < total elapsed (pre-first-section time unattributed), durations include rate-limit waits
- [x] llms.txt: one-line mention of section timing in the progress format description
- [x] CLAUDE.md: note the SectionTimer position in the logger chain (below cmux WrapLogger, above dashboard broadcast) and the FinishRun call sites
- [x] keep `docs/superpowers/specs/2026-08-05-section-timing-design.md` linked from this plan
- [x] run `make test` and `make lint` one final time — must pass

## Technical Details

- Timing source: wall-clock between consecutive `PrintSection` calls on the wrapper, plus `FinishRun()` for the last section. Rate-limit waits and pauses are included by design.
- Bucket mapping: `SectionTaskIteration`→tasks, `SectionInternalReview`→internal review, `SectionExternalReviewIteration`→external review, `SectionExternalEvaluation`→evaluation, `SectionPlanIteration`→planning, `SectionGeneric`+`SectionCustomIteration`→other. The reviewer-chain header `external review (claude)` is Generic → other; acceptable because per-iteration sections carry the real external-review time.
- The wrapper emits through the inner logger's `Print`, so lines get the standard timestamp and current phase color; nothing writes to the file directly. The `took` line lands inside the previous section's collapsible block on dashboard replay (emitted before the new section event) — desired grouping.
- Chain order (runner): dashboard broadcast → SectionTimer → cmux WrapLogger (outermost). retryPolicy type-asserts the outermost logger for `LogLimitWait`; `cmux.Logger`, `processor.Logger`, and `web.Logger` have identical method sets, so assignments stay clean.
- No config keys, no CLI flags: timing is always on (log-only, cheap, no behavior change). No moq regeneration — no existing interface gains methods.

## Post-Completion

**Manual verification**:
- Run a small plan (`make e2e-prep` toy repo) and check the progress file: `took` lines after each section, `phase durations:` line, then `DIFFSTATS:`/footer; dashboard live view shows the lines.
- Re-run the scratchpad `progstats.py` on a new log and confirm its numbers match the built-in lines.

**External system updates**: none.
