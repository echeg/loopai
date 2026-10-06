# Report: Per-task external review cadence

Plan: `docs/plans/20261006-per-task-external-review.md` · Branch: `per-task-external-review` · Base: `master` · Mode: full · Executor: claude · Task model: `claude:opus:high` · Review model: `claude:opus:high` · Started: 2026-10-06T12:21:40Z · Finished: 2026-10-06T14:56:52Z

## Summary
This branch adds a new setting, `review_cadence = end|task`, and a matching `--review-cadence` flag. The default is `end`. With `task`, a full-mode run also runs the external reviewer chain after every completed plan task. Each of those reviews diffs against the commit the task started from and gets a `{{REVIEW_SCOPE}}` note so the reviewer knows it is looking at one task. The review block at the end of the run is unchanged and still owns the review checkpoint.

The delivered pieces:
- **Hook:** an `AfterTask` hook in `TaskPhase`.
- **Diff base:** a per-task diff-base and scope override in the prompt builder.
- **Prompts:** `{{REVIEW_SCOPE}}` in all six external prompts.
- **Checkpoints:** per-task review blocks never save a checkpoint stage.
- **Run record:** repeated reviewer completions are added together (`Blocks`, a `Block` index per iteration), and per-task reviewer records survive the record reset that follows each task commit.
- **Startup:** a startup warning for cases where the setting has no effect, and a banner line when it is active.

The external review found gaps in the plan's handling of interruptions, and these fixes followed:
- Fixes a per-task review left uncommitted are now stored as `PendingReviewFixes` in the run record. The flag survives a stop and resume and is cleared after the post-review loop.
- A task that ticked its checkboxes but was interrupted before committing is no longer reviewed against a diff that misses its work. It is reported through `UncommittedTask` instead.
- When the flag is set, the final block's first review commits the leftover work through a pathspec-restricted commit prefix.

Phase durations: tasks 3,381,086 ms (~56m21s), internal review 3,405,831 ms (~56m46s), external review 495,133 ms (~8m15s), evaluation 2,030,080 ms (~33m50s), other 0 ms. Wall clock was about 2h35m12s through finalize. The run had 8 task iterations with 0 failed retries. The internal review ran once and ended by `review_done`. Post-review ran for 3 iterations. Finalize did not run, so no base sync took place.

## Change scope
35 files changed: 2,219 additions and 151 deletions, in 12 commits.
- **Config:** `pkg/config/config.go`, `values.go`, `defaults/config`, plus tests. Adds the key, its validation, the `Set` flag, `EffectiveReviewCadence`, and documentation in the commented defaults.
- **CLI:** `cmd/loopai/main.go` and `main_test.go`. Adds the flag, the override, the startup warning, the banner line, and passes the setting to the processor.
- **Task phase:** `pkg/processor/phase/task.go` and `task_test.go`. Adds the `AfterTask` hook, the diff fingerprint taken when a task starts, and the `UncommittedTask` reporting on timeout, break-abort, and executor-error/cancel exits.
- **Review phase:** `pkg/processor/phase/review.go`, `review_test.go`, `phase_test.go`. `First` now takes a prefix.
- **Prompts:** `pkg/processor/prompt_builder.go`, `prompts.go`, `prompts_test.go`, and the six external prompts (`codex.txt`, `codex_review.txt`, `custom_eval.txt`, `custom_review.txt`, `external_claude_eval.txt`, `external_claude_review.txt`).
- **Runner and record:** `runner.go`, `review_resume.go`, `run_record.go`, `run_recorder.go`, `run_facts.go`, plus tests. Adds `afterTaskReview`, `perTaskReview`, `perTaskLeftovers`/`PendingReviewFixes`, `leftoverCommitPrefix`, `currentExternal`, and the aggregation by `Blocks`/`Block`.
- **Docs:** `CLAUDE.md`, `README.md`, `llms.txt`, `docs/t3-code.md`, and the plan itself.

Commits, oldest first: `8ca4426` config key, `ec6237f` flag and startup wiring, `ab738de` AfterTask hook, `c442814` diff-base override and REVIEW_SCOPE, `0ebcd7d` per-task external chain, `239ba53` record aggregation, `61fc99d` acceptance verification, `99e0f6e` docs, then review fixes `ea0109f`, `ebf256b` (codex findings), `8645187`, and `fb51c5c`.

## Evidence
none

The supplied validation facts record no commands and no runs. The evaluator's own text says the focused `go test ./pkg/processor/...` runs and `golangci-lint` passed after each fix, and it names the new regression tests, for example `TestTaskPhase_Run_AfterTaskTimeoutBeforeCommitSkipsReview`, `TestTaskPhase_Run_AfterTaskBreakAbort`, `TestTaskPhase_Run_AfterTaskExecutorError`, `TestRunner_AfterTaskReviewErrorKeepsLeftovers`, and `TestRunRecorderPersistsPerTaskLeftoversAcrossResume`. No run shows those tests failing before the fix, and the full `make test` was never run because WSL would not start.

## Risk
**medium**

- **Public API and config:** the change is additive. The new key and flag default to `end`, which the plan requires to leave prompt rendering and pipeline order byte-identical, and the prompt tests check that. Customized external prompts without `{{REVIEW_SCOPE}}` still work and only trigger a warning.
- **Internal interfaces:** `ReviewPhase.First` now takes a prefix. `TaskPhaseOpts` gains `AfterTask` and `UncommittedTask`.
- **Data schema:** the persisted `.run.json` gains `PendingReviewFixes`, `ExternalReviewerRecord.Blocks`, and a per-iteration `Block`. Older readers ignore unknown JSON fields.
- **Behavior on default runs:** the task phase's interruption paths (timeout, break-abort, executor error or cancellation) now compare diff fingerprints. Those paths run under every cadence, but they only act when a callback is installed.
- **Concurrency:** none new. The prompt builder's scope is one mutable field shared by every phase, but phases run one after another and a deferred restore clears it.
- **Migrations:** none.

The main reason this is medium rather than low is validation coverage. The full `make test` (race detector, `cmd/loopai`, and the shell suites) never ran on a POSIX host. Only the processor and config packages were run natively on Windows. The interruption and resume paths are subtle, and the external review needed four rounds to close them.

## Merge danger
**Door:** two-way
Reverting removes an opt-in key that defaults to off. The added run-record fields are ignored by older binaries, and no released consumers depend on them yet.

**Blast radius:** processor
The changes are in the full-mode task and review pipeline. The extra reviewer cost and the new commit prefix only apply when `review_cadence = task`.

## Migrations and operational steps
none

## Plan deviation
- **Added:** this invocation's per-task reviewer records are kept across the record reset after a task-phase commit (`currentExternal`, restored by `resetRunRecord` and `adoptLoadedRunRecord`). Without this, every per-task block was dropped before the report. The plan records it as a ➕ item in Task 6.
- **Beyond the plan's checkboxes, from external review:**
  - `PendingReviewFixes` is persisted in the run record and cleared after post-review.
  - The task phase gained an `UncommittedTask` callback. An interrupted task that ticked its checkboxes but did not commit is skipped for per-task review, including on break-abort and on executor error or cancellation, and is reported instead.
  - `afterTaskReview` records leftovers on error exits too.
  - `ReviewPhase.First` takes a `leftoverCommitPrefix` so the final block commits the leftover work.

  The plan's Technical Details reflect only the leftover-flag part.
- **Task 3 contract changed:** the plan said the hook must not run on a timed-out session or a manual break. It now does run in those cases when the task ticked and committed, and it is skipped (with the task reported) when the work was left uncommitted.
- **Task 8:** the plan said T3 would mark a "task stage" complete early. The ACP sink has no task stage, so the docs instead say the first per-task block marks the Review stage completed and External review as in progress.
- **Tasks 2 and 7:** `make test` in WSL could not run because WSL fails to start. Native Windows runs were used instead. The plan records `cmd/loopai` showing the same 70 POSIX-only failures as master and none new.
- **Blocked:** none.
- **Skipped:** none.
- **Post-completion:** the manual smoke tests (`make e2e-prep` run, and T3 under `--t3` and `--acp`) are not checkbox items and have no recorded result.

## Backlog
None. No backlog entries were filed.

## External review
### codex:gpt-6-astra:high
- label: codex
- iterations: 5
- duration_ms: 2525111
- ended by: done
- had findings: true

Findings:
- Iteration 1, [P2] pending review fixes are forgotten after a restart (`perTaskLeftovers` was kept only in memory) -> fixed: persisted as `RunRecord.PendingReviewFixes`
- Iteration 1, [P2] an interrupted task could be reviewed before its commit existed (it ticks the plan before committing) -> fixed: start-of-task fingerprint comparison skips the per-task review when work is uncommitted
- Iteration 2, [P2] a skipped interrupted task could stay unreviewed, because final reviewers read `base...HEAD` -> fixed: `UncommittedTask` callback sets the pending flag, and the final block's first review commits the work through `leftoverCommitPrefix`
- Iteration 2, [P2] the persisted pending-fix flag never cleared -> fixed: cleared and saved after the post-review loop, or when a checkpoint shows it already ran
- Iteration 3, [P2] an aborted per-task review never persisted its leftover fixes on error exits -> fixed: fingerprint is compared before the error is returned
- Iteration 3, [P2] aborting after a break bypassed the uncommitted-task callback -> fixed: the abort path reports an advanced, uncommitted task without running a review
- Iteration 4, [P2] task cancellation or an executor error bypassed pending-work recording -> fixed: `reportAdvancedUncommittedTask` runs on the executor-error exit
- Iteration 5: no issues found. The accumulated fixes were committed as `ebf256b`.

## Validation
- Commands: none
- Timings: duration_ms 0, runs 0