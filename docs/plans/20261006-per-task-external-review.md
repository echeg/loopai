# Per-task external review cadence

## Overview
- Add `review_cadence = end | task` (default `end`) and the matching `--review-cadence` flag. With `task`, the full-mode pipeline runs the external reviewer chain after every completed plan task instead of only once at the end of the task phase.
- Problem: today every external finding arrives after the whole plan is implemented, so an early design mistake is reviewed only after later tasks were built on top of it. Reviewing each task right after its commit catches such mistakes while the fix is still cheap.
- Integration: `TaskPhase.Run` gains an `AfterTask` hook; `runFull` uses it to run `ExternalReviewPhase.Run` against the commit the task started from. The end-of-run block (internal review, external chain, post-review, finalize, report) is unchanged, so the final whole-branch review still runs and still owns the review checkpoint.

## Decisions
- **Context**: external review runs only in `runExternalAndPostReview`, after `TaskPhase.Run` has executed every task; the first reviewer round diffs `<default branch>...HEAD`, later rounds `git diff`.
- **Chosen approach**: an `AfterTask` callback on `TaskPhase`, invoked only when the next uncompleted task position advanced, wired by the runner to a per-task external review block. Least invasive: break handling, retries, and timeouts stay inside `TaskPhase.Run`.
- **Per-task block content**: the external chain only. The evaluator fixes and commits findings inside that loop (`fix: address external review findings` before `EXTERNAL_REVIEW_DONE`), so the tree is clean again before the next task. The internal five-agent review and the `review_second.txt` post-review loop run only in the final block, to keep the cost per task bounded by the chain itself.
- **Final block kept**: per-task reviews see tasks in isolation; cross-task integration defects are only visible to the whole-branch review at the end.
- **No checkpoints for per-task blocks**: a process death between tasks loses only that task's review, which the final whole-branch review repeats. `onReviewerDone` must not save a review stage from a per-task block.
- **Rejected alternatives**: `## Phase N:` grouping in plans (format extension touching the parser, drift detection, and plan-writing skills); moving the task loop into the runner (`RunOne`) (cleaner but moves break/retry/timeout logic); a full review block per task (multiplies the heaviest phases by the task count).
- **Verified facts**: the plan parser (`pkg/plan/parse.go:50`) knows only `### Task N:`/`### Iteration N:`; `promptBuilder` is one instance shared by all phases, so a diff-base override is a mutable field reset after the block; `runRecorder.ExternalDone` (`pkg/processor/run_recorder.go:53`) overwrites `Duration`, `EndedBy`, and `HadFindings` per reviewer key, so repeated calls would hide earlier blocks; the ACP sink never reopens a completed stage (`pkg/acp/sink.go:505`), so the first per-task block completes the T3 task stage early; `ExternalReviewPhase.SetResume` state applies to every `Run` call until changed.

## Context (from discovery)
- Files/components involved: `pkg/processor/phase/task.go` (task loop), `pkg/processor/runner.go` (`runFull`, `runExternalAndPostReview`, phase wiring around line 263), `pkg/processor/prompts.go` (`getDiffInstruction`, `getDefaultBranch`, `replaceExternalVariablesWithIteration`), `pkg/processor/prompt_builder.go` (builder struct), `pkg/processor/review_resume.go` (`onReviewerDone`), `pkg/processor/run_recorder.go`, `pkg/config/config.go`, `pkg/config/values.go` (`parseFinalizeValues` is the pattern for an enumerated string key), `pkg/config/defaults/config`, the six external prompts under `pkg/config/defaults/prompts/` (`codex_review.txt`, `external_claude_review.txt`, `custom_review.txt`, `codex.txt`, `external_claude_eval.txt`, `custom_eval.txt`), `cmd/loopai/main.go` (`opts`, `applyCLIOverrides` ~6747, `applyFinalizeOverride` ~6731 as the pattern, startup banner `phaseBanner` ~4179).
- Related patterns found: `finalize` is an enumerated string key with `IsValidFinalizeMode`, `FinalizeSet`, a `choice:` flag, and an override function; `ReportEnabled` is copied into `phase.Config` through `toPhaseConfig`; `OnReviewerDone` is a runner closure passed in `ExternalReviewPhaseOpts`.
- Dependencies identified: `status.PhaseHolder` transitions task → external review → task are observed by cmux, Orca, T3, keep-awake, and the ACP sink; `RunRecord.External` feeds the completion report facts.

## Development Approach
- **Testing approach**: Regular (code first, then tests)
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
- Maintain backward compatibility: `review_cadence` unset or `end` must leave every existing code path and prompt rendering byte-identical

## Testing Strategy
- **Unit tests**: required for every task (see Development Approach above); table-driven with `testify`, one `_test.go` per source file, config tests redirect `HOME` and `USERPROFILE` to `t.TempDir()`
- **E2E tests**: not applicable, no dashboard change

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code, tests, documentation in this repository
- **Post-Completion** (no checkboxes): manual smoke test of a real run
- **Checkbox placement**: checkboxes belong only in Task sections

## Implementation Steps

### Task 1: Add the `review_cadence` config key
- [x] in `pkg/config/config.go` add constants `ReviewCadenceEnd = "end"`, `ReviewCadenceTask = "task"`, a `ReviewCadenceModes` slice, `IsValidReviewCadence`, fields `ReviewCadence string` and `ReviewCadenceSet bool` on `Config`, and `EffectiveReviewCadence()` returning `end` for an empty value, following the `Finalize` pattern
- [x] in `pkg/config/values.go` add `ReviewCadence`/`ReviewCadenceSet` to `Values`, reset them with the other `*Set` flags, and parse the key with a validation error naming the allowed values, following `parseFinalizeValues`
- [x] copy the value into `Config` where `Finalize` is copied (`config.go` ~434)
- [x] document the key in `pkg/config/defaults/config` next to `review_patience`: `end` (default) reviews once after the task phase; `task` additionally runs the external reviewer chain after every completed task in full mode, with the cost note that `max_external_iterations` and `review_patience` apply to every block separately
- [x] write tests in `pkg/config/values_test.go` and `config_test.go` for: unset → `end`, `end`, `task`, an invalid value error, and the `Set` flag
- [x] run `go test ./pkg/config/...` - must pass before task 2

### Task 2: Add the `--review-cadence` flag and startup wiring
- [x] in `cmd/loopai/main.go` add `ReviewCadence string` to `opts` with `long:"review-cadence" choice:"end" choice:"task"` and a description, and list it in `markFlagsSet` where `finalize` is listed
- [x] in `applyCLIOverrides` copy a set flag into `cfg.ReviewCadence` with `ReviewCadenceSet = true`, following `applyFinalizeOverride`
- [x] add `reviewCadenceStartupWarning`: when the effective cadence is `task` and the mode is not full (`--review`, `--external-only`, `--tasks-only`), warn that the cadence is ignored; when the external chain resolves to empty, warn that `review_cadence = task` has no reviewers to run
- [x] print the effective cadence in the startup banner only when it is `task`, as one line under the external reviewers line
- [x] pass the effective cadence into `processor.Config` (new field `ReviewCadence`) where the other config fields are copied (~4101)
- [x] write tests in `cmd/loopai/main_test.go` for the override (flag wins over config, unset leaves config), the mode warning for the three non-full modes, the empty-chain warning, and the banner line present/absent
- [x] run `go test ./cmd/loopai/...` - must pass before task 3 (WSL unavailable; native Windows run shows the same 70 POSIX-only failures as clean HEAD and none new)

### Task 3: Add the `AfterTask` hook to `TaskPhase`
- [x] in `pkg/processor/phase/task.go` add `AfterTask func(ctx context.Context, taskNum int, headBefore string) error` to `TaskPhaseOpts` and the struct
- [x] in `Run`, record `NextPlanTaskPosition()` and the HEAD hash (through the existing `GitState`/`Deps` git access, or a new `HeadHash` on `TaskPhaseOpts` if the phase has none) before each iteration; after a successful iteration (no timeout, no `SignalFailed`, no manual break) call the hook when the plan position advanced or the iteration ended with `SignalCompleted` and no uncompleted tasks remain; the head is kept from the first attempt at a position, so a retried or non-advancing attempt does not move the diff base past that task's own commits
- [x] on a hook error return `fmt.Errorf("after task %d: %w", taskNum, err)`; a `context.Canceled` from the hook propagates unchanged (`ErrUserAborted` too, so a break aborted inside the block reads as an abort)
- [x] do not call the hook for a retried failure, a timed-out session, a manual break, or an iteration that did not advance the plan (the model ticked nothing)
- [x] write tests in `task_test.go`: hook called once per advanced task with the right number and head; not called on retry, timeout, break, or no-advance; hook error aborts the phase with the wrapped message; `SignalCompleted` on the last task calls the hook before `Run` returns
- [x] run `go test ./pkg/processor/phase/...` - must pass before task 4

### Task 4: Diff base override and `{{REVIEW_SCOPE}}` in the prompt builder
- [x] in `pkg/processor/prompt_builder.go` add `diffBase string` and `reviewScope string` fields with `SetReviewScope(diffBase, scope string)` and `ClearReviewScope()`; `getDefaultBranch` returns `diffBase` when set, so `{{DIFF_INSTRUCTION}}` (first iteration `git diff <base>...HEAD`) and `{{DEFAULT_BRANCH}}` both name the task's start commit; `{{FINALIZE_BASE}}` is untouched
- [x] expand a new `{{REVIEW_SCOPE}}` variable in `replaceExternalVariablesWithIteration` and in the evaluation prompt path (`ExternalEvaluationPrompt`): empty when no scope is set, otherwise the scope text
- [x] add `reviewScopeForTask(taskNum int, planFile string) string` building: this review covers only task N of the plan; the diff base is the commit before that task started; the plan is still being executed, so later tasks not yet implemented and missing integration with them are not findings
- [x] add `{{REVIEW_SCOPE}}` to the six external prompts (`codex_review.txt`, `external_claude_review.txt`, `custom_review.txt`, `codex.txt`, `external_claude_eval.txt`, `custom_eval.txt`) right after the goal/diff instruction lines, and add the variable to each prompt's header comment
- [x] write tests in `prompts_test.go`: without a scope every external and evaluation prompt renders byte-identical to before; with a scope `DIFF_INSTRUCTION` uses the hash, `DEFAULT_BRANCH` uses the hash, `REVIEW_SCOPE` carries the task number, and `ClearReviewScope` restores the default branch
- [x] add a check that the embedded prompts contain `{{REVIEW_SCOPE}}` and that `review_first.txt`, `review_second.txt`, `task.txt`, `finalize.txt`, and `report.txt` do not
- [x] run `go test ./pkg/processor/...` - must pass before task 5

### Task 5: Run the per-task external block from the runner
- [x] in `pkg/processor/runner.go` add `ReviewCadence string` to `Config` (already added in task 2), and in the phase wiring set `TaskPhaseOpts.AfterTask` to `runner.afterTaskReview` when the effective cadence is `task` and `externalPhase.Enabled()`
- [x] implement `afterTaskReview(ctx, taskNum, headBefore)`: log `review cadence: external review after task N`, set `r.perTaskReview = true`, `prompts.SetReviewScope(headBefore, scope)`, `external.SetResume(0, false)`, `phaseHolder.Set(status.PhaseExternalReview)`, call `external.Run`, then restore `phaseHolder.Set(status.PhaseTask)`, `ClearReviewScope`, `perTaskReview = false`, and re-apply the resume state from `r.resume` so the final block still honors the checkpoint; use `defer` for the restores so a failure leaves no scope set
- [x] in `review_resume.go` make `onReviewerDone` skip `saveReviewStage` while `perTaskReview` is set (the recorder call in the phase still runs)
- [x] when `headBefore` is empty (no git), skip the block with a warning instead of diffing against the default branch
- [x] make `runFull` set `TaskPhaseOpts.AfterTask` only in full mode; `runTasksOnly` keeps the hook nil
- [x] write tests in `runner_test.go` with mocked phases: cadence `end` never calls the external phase before the internal review; cadence `task` calls it once per advanced task with the task's head as diff base and then again in the final block with the default branch; a per-task block never saves a review stage; the phase holder returns to task after each block; an external error from a block fails the run as `task phase: after task N: ...`; cadence `task` with an empty chain installs no hook
- [x] run `go test ./pkg/processor/...` - must pass before task 6

### Task 6: Aggregate repeated reviewer completions in the run record
- [x] in `pkg/processor/run_recorder.go` make `ExternalDone` add the duration to the existing record's `Duration`, OR `HadFindings` into it, keep `EndedBy` from the latest completion, and increment a new `Blocks int` field on `ExternalReviewerRecord` (`run_record.go`) counting completed loops; iterations already append
- [x] in `completion_report.go`/`run_facts.go` render `Blocks` in the external review facts when it is greater than one, as `N review blocks` after the reviewer label, so the report tells per-task reviews from a single end review
- [x] write tests in `run_recorder_test.go` for one completion (unchanged output), two completions (summed duration, OR-ed findings, latest `EndedBy`, `Blocks == 2`), and in `completion_report_test.go` for the rendered label
- [x] ➕ keep this invocation's per-task reviewer records across the record reset that follows a task-phase commit (`currentExternal`, restored by `resetRunRecord` and `adoptLoadedRunRecord`); without it every per-task block was discarded before the report
- [x] run `go test ./pkg/processor/...` - must pass before task 7

### Task 7: Verify acceptance criteria
- [x] verify all requirements from Overview are implemented and `review_cadence` unset leaves prompts and pipeline order unchanged
- [x] verify edge cases: last task with `SignalCompleted`, a hook error, review checkpoint resume with cadence `task` (final block still resumes), `--tasks-only` ignores the cadence (➕ added `TestRunnerReviewCheckpoint_CadenceTaskFinalBlockResumes`)
- [x] run `make test` (in WSL on Windows) (⚠️ WSL fails to start; native Windows run with isolated HOME: config, processor, phase pass; cmd/loopai shows the same 70 POSIX-only failures as master and none new; other failing packages and the shell suites (python3, symlinks) are untouched by this branch)
- [x] run `make lint` - all issues must be fixed
- [x] run `GOOS=windows GOARCH=amd64 go build ./...`

### Task 8: Update documentation
- [ ] `README.md`: document `review_cadence`/`--review-cadence` with the cost note and the two v1 limitations (per-task blocks are not checkpointed; the T3 Code plan view marks the task stage complete at the first per-task block)
- [ ] `llms.txt`: add the key and flag
- [ ] `CLAUDE.md`: add a paragraph on the cadence mechanics (hook, diff base override, no checkpoint, recorder aggregation) in the architecture section
- [ ] `docs/t3-code.md` Limitations: note the stage behavior under `review_cadence = task`
- [ ] run `make test-report-docs` - must pass

## Technical Details
- **Hook contract**: `AfterTask` runs after the task session's own commit, so the block starts on a clean tree; the chain's evaluation prompts commit their fixes before `EXTERNAL_REVIEW_DONE`, so the next task starts clean too. A block that leaves uncommitted fixes (stalemate or iteration cap) is tolerated: the next task's `task.txt` stages only its own paths, and the final block picks the rest up through the existing `commitPrefix`.
- **Diff base**: the HEAD hash recorded before the task iteration. `git diff <hash>...HEAD` and `git log <hash>..HEAD` both accept a hash, so `{{DEFAULT_BRANCH}}` can carry it without prompt changes.
- **Resume interaction**: `markReviewTaskStarted` already invalidates a review checkpoint when the task phase commits, so per-task blocks need no new invalidation; `onReviewerDone` just must not write stages while a block runs.
- **Status**: `PhaseHolder` goes task → external review → task per block; cmux, Orca, T3 titles follow it. ACP sink keeps its never-reopen rule; the limitation is documented.
- **Cost**: for a chain of R reviewers and T tasks the run spends up to T×R extra reviewer loops, each capped by `max_external_iterations` and `review_patience`.

## Post-Completion
**Manual verification**:
- Run `make e2e-prep`, set `review_cadence = task` and an `external_reviewers` chain in `/tmp/loopai-test/.loopai/config`, run the toy plan, and confirm the progress log shows `external review (...)` sections between task iterations and once more after the internal review.
- Run the same plan under `--t3` and `--acp` in T3 Code and confirm the titles and plan view behave as documented.
