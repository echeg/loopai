# Design: per-section duration logging + end-of-run phase summary

Date: 2026-08-05
Status: approved

## Problem

Progress logs timestamp every line and mark section starts (`--- task iteration 1 ---`), but nothing records how long a section took or where a run spent its time. Analysis showed runs spend 60-75% of wall-clock in review loops, but extracting that requires an external parser. Total elapsed exists only in the `Completed:` footer.

## Decision summary

1. A logger-chain wrapper (`progress.SectionTimer`) measures the time between `PrintSection` calls and emits a normal timestamped line when a section closes: `task iteration 1 took 3m38s`.
2. At run end a one-line phase summary is emitted: `phase durations: tasks 32m26s (8), internal review 2h23m (10), external review 42m16s (7), evaluation 31m21s (7)` — buckets with zero sections are omitted. `(N)` counts section occurrences (a retried iteration counts twice). Bucket sums do not equal the footer `Elapsed()`: time before the first section is unattributed. In the main flow the summary lands above the `DIFFSTATS:` line and footer block; in the plan-creation flow it is emitted right after that phase's `Run` returns and is not adjacent to the footer (accepted).
3. Buckets group by `status.SectionType`: tasks (TaskIteration), internal review (InternalReview), external review (ExternalReviewIteration), evaluation (ExternalEvaluation), planning (PlanIteration), other (Generic, CustomIteration).
4. No changes to section header format, web dashboard, cmux reporter, or phase engines. Old logs stay parseable; new lines are ordinary timestamped text.
5. Durations are wall-clock and include rate-limit waits; that is intentional and documented.

## Architecture

- `pkg/progress`: new `SectionTimer` wrapping an exported local interface `progress.SectionLogger` (mirror of the shared logger method set — required because `cmux`/`processor` → `plan` → `progress` makes importing their interfaces a cycle; compile-time compatibility assertions live in `cmd/loopai` tests). Constructor takes the inner logger and a `now func() time.Time` for tests; a `sync.Mutex` guards state (race-enabled suite). `PrintSection`: if a section is open, emit `inner.Print("%s took %s", label, dur)` (printf-safe), accumulate into its bucket, then forward the header. `FinishRun()` (named to avoid colliding with `cmux.Reporter.Finish`): closes the last open section and emits the summary line; idempotent; no-op when no sections were seen.
- Duration formatting follows the `Elapsed` conventions (>= 1h truncate to minutes, else seconds); the shared helper lives in `section_timer.go` and `Elapsed` delegates to it.
- Chain placement in `cmd/loopai/main.go`: wrap **after** dashboard setup and **before** `rep.WrapLogger`, via a small extracted seam (`buildRunnerLogger`) so the order is unit-testable — timer output then flows through the broadcast logger (file + stdout + SSE), while the cmux wrapper stays outermost so its optional-interface methods (`LogLimitWait`, `LogLimitRecovery`) remain discoverable by retryPolicy (`pkg/processor/execution_policy.go:137,151`).
- `FinishRun()` call sites: runner path — hoist the run error out of the `if` at main.go:804 so a single call covers success, failure, and user abort; `defer` is forbidden because `keepDashboardAlive` closes the log explicitly (main.go:703) before deferred calls fire. Plan-creation path — call immediately after that flow's `Run` returns (main.go:2102); the flow continues into full implementation afterwards, so the summary is not footer-adjacent there. Hard aborts that never return from Run produce no summary — accepted.
- Known cosmetic mismatch, accepted: phase engines set the phase holder before `PrintSection`, so live SSE tags the `took` line with the new phase while dashboard file replay attributes it to the previous section. The line is visible in both views, just under different phase filters.

## Error handling

- Nil inner logger is not a supported configuration (same as the rest of the chain); the wrapper does not add nil-guards beyond what the chain already has. A nil `now` defaults to `time.Now`.
- Zero-duration and sub-second sections log as `0s`/`Ns` — no special-casing.

## Testing

- Table-driven tests in `pkg/progress/section_timer_test.go`: sequences of sections with a fake clock → exact emitted lines and summary; single section closed only by `FinishRun`; `FinishRun` with no sections emits nothing; `FinishRun` idempotence; bucket mapping for every `SectionType` (including the Generic `external review (label)` chain header landing in `other`); formatting boundaries (59s, 1h+).
- `cmd/loopai` wiring tests target the extracted `buildRunnerLogger` seam (no chain-order tests exist today): recording inner logger receives the `took` lines, outermost logger keeps the rate-limit optional interface, compile-time `processor.Logger`/`cmux.Logger` assertions.

## Out of scope

- `--stats` CLI command aggregating across progress files (explicitly deferred).
- Dashboard UI changes; per-test (`make test`) timing inside an iteration.
- Any change to the `--- label ---` header format.
