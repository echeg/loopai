# Resumable review pipeline

## Overview

When a loopai run dies inside the review pipeline (a `claude_error_patterns` hit, a machine sleep, a
kill), rerunning the same command restarts every review from scratch: internal review First and Loop,
every external reviewer in the chain from iteration 1, then post-review. On a long plan that is hours
of repeated work. Nothing in the review phases is durable today: `firstCompleted`, iteration counters,
and stalemate state are locals in `ExternalReviewPhase.runLoop`, and the runner re-enters `runFull`
unconditionally from the task phase even on a resumed worktree.

This plan adds a review checkpoint: a small JSON file in the main checkout's `.loopai/progress/`
directory, next to the run's progress log, written by the runner after each review stage completes on
a clean tree, and read on the next run to skip the stages already done. The same command resumes: no
new flag, no plan-file changes, no prompt changes. Stages are recorded with the branch HEAD they
completed on and are honored only while that commit is still an ancestor of the branch tip, so a
reset or a rewritten branch drops them. New task-phase commits in the resuming run drop them too,
because reviews of stale code are not reviews.

Key benefits: hours saved after a crash, honest state (a stage counts only if its fixes are committed),
zero behavior change for runs that never crash (the file is created on the first stage and removed on
success), and precedent-aligned storage (`cmd/loopai/plan_chain_state.go` already keeps a versioned,
atomically written JSON checkpoint in the same directory).

## Decisions

- **Context**: reviews had to resume after a crash without new flags and with state that survives
  `--force` removal of the worktree.
- **Chosen approach**: JSON checkpoint in `.loopai/progress/` of the main checkout, written by the
  runner through a small store interface implemented in `cmd/loopai`, mirroring the plan-chain
  checkpoint. Stages: `internal_review`, one `external_review` per reviewer in chain order,
  `post_review`. Finalize is not checkpointed: it is best-effort, runs once, and is cheap to repeat.
- **Rejected: review stages as plan checkboxes** (the original idea). `task.txt` tells the model to
  keep working while any `[ ]` remains anywhere in the file and to emit `ALL_TASKS_DONE` only when none
  do, so pending review checkboxes would make the task phase try to "implement" reviews. A non-checkbox
  `## Review Log` would work but needs a plan mutation layer that `pkg/plan` does not have, a commit
  per stage inside the review loop, and cannot run in `--review`/`--external-only`, which execute in
  the user's own checkout where loopai must not commit the plan.
- **Rejected: reconstructing state from the progress log**. The log is a human format that changes
  freely, its section lines are written before fixes are committed, so there is no SHA to validate
  against.
- **HEAD advanced past the recorded SHA**: resume anyway when the recorded commit is an ancestor of
  the current HEAD (manual commit, `--commit` start-commit merge), with a log warning naming both
  commits. Discard from the first stage whose commit is no longer contained. Exception: a commit made
  by the task phase of the *resuming* run invalidates the whole checkpoint.
- **Clean-tree gate**: a stage is saved only when `DiffFingerprint()` reports no uncommitted changes.
  The final evaluation round commits accumulated fixes before `EXTERNAL_REVIEW_DONE`, and the internal
  review prompts commit their fixes, so a clean tree is the normal end state. A dirty tree at stage end
  logs `review checkpoint skipped: uncommitted changes` and leaves the stage unrecorded, so the next run
  repeats that reviewer instead of trusting fixes that the worktree removal destroyed.
- **Reviewer chain mismatch**: the checkpoint stores the reviewer keys (`provider:model[:effort]`) in
  order. If the current chain differs, external and post-review stages are dropped; the internal
  review stage is kept because it does not depend on the chain.
- **Worktree removal on failure is unchanged**. A fresh `--worktree` run still removes its worktree on
  failure (`cmd/loopai/main.go:1755`, `defer cleanup(true)`), taking uncommitted mid-reviewer fixes with
  it. The clean-tree gate keeps the checkpoint honest about that; preserving the worktree after review
  has begun is a separate decision and is filed under Post-Completion.
- **Verified facts**: `runFull` order is task → `review.First` → `review.Loop("")` →
  `runExternalAndPostReview` (`pkg/processor/runner.go:332-369`); post-review runs only when
  `outcome.HadFindings` is true (`runner.go:421`); `ExternalReviewPhase.Run` iterates `p.reviewers` in
  order and ORs `HadFindings` (`external_review.go:84-110`); `git.Service` already exposes
  `HeadHash`, `DiffFingerprint`, and `ContainsRevisionContext` (`pkg/git/service.go:238-262`); the
  progress logger path is resolved in the main checkout before loopai changes into a worktree, so
  `filepath.Dir(log.Path())` is a worktree-independent anchor; `readProgressAssociations` scans only
  `*.txt`, so a `.json` sibling is invisible to close-out lookups.

## Context (from discovery)

- Files/components involved:
  - `pkg/processor/runner.go`: `Config`, `Runner`, `runnerPhases`, `runFull`, `runReviewOnly`,
    `runCodexOnly`, `runExternalAndPostReview`, `SetGitChecker`, `GitChecker` interface.
  - `pkg/processor/phase/external_review.go`: `ExternalReviewPhaseOpts`, `Run`, `runLoop`,
    `reviewerLabel`, `ExternalReviewOutcome`.
  - `pkg/processor/phase/review.go`: `First`, `Loop`.
  - `pkg/processor/mocks/git_checker.go`: moq-generated, regenerate with `go generate`.
  - `cmd/loopai/main.go`: `createRunner` (~2890), `executePlan` (1314), `runWithWorktree` cleanup
    (1738-1760), `plan_chain_state.go` as the checkpoint pattern.
  - `pkg/git/service.go`: `HeadHash`, `DiffFingerprint`, `ContainsRevisionContext`.
  - Docs: `README.md` (execution pipeline ~356, worktree resume 783-793), `llms.txt:183`, `CLAUDE.md`
    architecture section.
- Related patterns found: `savePlanChainCheckpoint` (temp file + `os.Rename`, `0600`, version field,
  `validatePlanChainCheckpoint`); `stalemateState` in `phase/git_state.go` already consumes
  `HeadHash`/`DiffFingerprint`; runner tests build real phases with mock executors through
  `NewWithExecutors` and assert `[]status.Phase` transitions
  (`TestRunner_CodexAndPostReview_PipelineOrder`).
- Dependencies identified: none new. `testify`, `moq`, existing `pkg/git` helpers.

## Development Approach

- **Testing approach**: Regular (code first, then tests in the same task)
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
- Run focused tests after each change; `make test` and `make lint` at the end of every task
- Maintain backward compatibility: a run without a checkpoint file behaves byte-for-byte as today
- Tests must use `t.TempDir()` and never touch `~/.config/loopai/` or `~/.config/ralphex/`

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: not applicable; the dashboard is untouched. Cross-compile with
  `GOOS=windows GOARCH=amd64 go build ./...` in the final verification task because the store writes
  files with `filepath` and `os.Rename`.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, and documentation changes in this repo
- **Post-Completion** (no checkboxes): manual smoke test, backlog follow-ups
- **Checkbox placement**: checkboxes belong only in Task sections

## Implementation Steps

### Task 1: Review checkpoint model and resume resolution in pkg/processor
- [x] create `pkg/processor/review_checkpoint.go` with `ReviewCheckpoint{Version, Mode, Branch, Plan, Reviewers []string, Stages []ReviewStage}` and `ReviewStage{Stage, Reviewer, Index, Head, HadFindings, EndedBy, CompletedAt}`; stage constants `reviewStageInternal = "internal_review"`, `reviewStageExternal = "external_review"`, `reviewStagePostReview = "post_review"`; `const reviewCheckpointVersion = 1`
- [x] define `ReviewCheckpointStore` interface at the consumer: `Load() (ReviewCheckpoint, bool, error)`, `Save(ReviewCheckpoint) error`, `Remove() error`; add `//go:generate moq` line and generate `mocks/review_checkpoint_store.go`
- [x] add `reviewerKeys(cfg Config) []string` returning `provider:modelspec` per `cfg.ExternalReviewers`, falling back to the legacy `ExternalReviewTool`/`Model`/`Effort` triple when the chain is empty and the tool is not `none`
- [x] add `reviewResume{skipInternal bool, completedReviewers int, hadFindings bool, skipPostReview bool, notes []string}` and a pure `resolveReviewResume(cp ReviewCheckpoint, current reviewResumeInput, contains func(head string) (bool, error)) (reviewResume, error)` where `reviewResumeInput{Mode, Branch, Reviewers []string, Head string}`: reject version/mode/branch mismatch as no-resume with a note; on reviewer-key mismatch keep only `internal_review`; walk stages in order and stop at the first whose `Head` is not contained; require external stages to be contiguous from index 0 in chain order (an `external_review` for index 1 without index 0 is ignored from there on); `post_review` counts only when every reviewer in the chain is completed; collect a note when `Head != current.Head` for an honored stage naming both short SHAs
- [x] write table-driven tests for `resolveReviewResume`: empty checkpoint, exact-HEAD match through every stage, ancestor match with note, non-ancestor drops from that stage, reviewer chain mismatch keeps internal only, mode/branch/version mismatch, non-contiguous external indexes, `post_review` without full chain, `contains` returning an error
- [x] write tests for `reviewerKeys` (chain, legacy triple, none)
- [x] run `go test ./pkg/processor/...` - must pass before task 2

### Task 2: Ancestry check on the runner's git checker
- [x] extend `processor.GitChecker` with `ContainsRevisionContext(ctx context.Context, revision string) (bool, error)`; `git.Service` already implements it at `pkg/git/service.go:244`
- [x] leave `phase.GitChecker` and `phase.Deps.Git` unchanged; store the checker on the runner in a new `git GitChecker` field set by `SetGitChecker` alongside the existing `deps.Git` assignment
- [x] regenerate `pkg/processor/mocks/git_checker.go` with `go generate ./pkg/processor/...`; do not hand-edit
- [x] update every hand-written `GitChecker` fake in `pkg/processor/*_test.go` and `cmd/loopai/*_test.go` that stops compiling
- [x] write a test that `SetGitChecker` populates both the runner field and `deps.Git`
- [x] run `go build ./... && go test ./pkg/processor/... ./cmd/...` - must pass before task 3

### Task 3: External review phase resume hooks
- [x] add `ReviewerCompletion{Index int, Reviewer ExternalReviewer, Label string, HadFindings bool, EndedBy string}` to `pkg/processor/phase/external_review.go`, with `EndedBy` one of `done` (evaluator emitted `EXTERNAL_REVIEW_DONE`), `stalemate`, `max_iterations`; make `runLoop` return the reason alongside its outcome
- [x] add `OnReviewerDone func(ctx context.Context, done ReviewerCompletion) error` to `ExternalReviewPhaseOpts`; `Run` calls it after a reviewer's `runLoop` returns with no error and no interruption, and a hook error aborts the chain with a wrapped error
- [x] add `SetResume(completed int, hadFindings bool)` on `ExternalReviewPhase`: `Run` skips the first `completed` reviewers, printing `status.NewGenericSection("external review (" + label + ") - skipped, completed in an earlier run")` for each, and seeds `outcome.HadFindings` with `hadFindings`; `completed >= len(reviewers)` skips the whole chain; a manual break or error still leaves the hook uncalled
- [x] add `SetResume` to the runner's `externalReviewPhaseRunner` interface
- [x] write tests: hook receives index, label, `HadFindings`, and each `EndedBy` value; hook not called on reviewer error or interruption; hook error aborts the chain; `SetResume(1, true)` skips reviewer 0, runs reviewer 1, and reports `HadFindings` true even when reviewer 1 is clean; `SetResume(len, ...)` runs nothing
- [x] run `go test ./pkg/processor/...` - must pass before task 4

### Task 4: Runner wiring: load, skip, save, invalidate, remove
- [x] add `SetReviewCheckpoints(store ReviewCheckpointStore)` to `Runner` and pass `OnReviewerDone` into `phase.NewExternalReviewPhase` from `NewWithExecutors` through a runner method so the phase can call back into the runner
- [x] create `pkg/processor/review_resume.go` with `loadReviewResume(ctx) reviewResume` (nil store or missing file → zero value; load/parse error → `Print("review checkpoint unreadable, starting reviews from scratch: %v")` and zero value; every note printed as `review checkpoint: ...`), `saveReviewStage(ctx, stage ReviewStage)` (no-op without store or git checker; skips with `review checkpoint skipped: uncommitted changes` when `DiffFingerprint()` is non-empty; records `HeadHash()`, `CompletedAt` UTC, merges into the loaded checkpoint replacing a stage with the same key, writes `Mode`, `Branch`, `Plan`, `Reviewers`; a save error is logged and never fails the run), and `clearReviewCheckpoint(reason string)`
- [x] obtain `Branch` for the checkpoint through the git checker: extend `processor.GitChecker` with `CurrentBranch() (string, error)` (implemented by `git.Service` at `service.go:270`), regenerate the mock, and treat an error or detached HEAD as an empty branch that still resumes when the checkpoint's branch is also empty
- [x] in `runFull`: capture `HeadHash()` before `task.Run`; after it, if the hash changed, call `clearReviewCheckpoint("task phase committed new work")` and skip resume; otherwise `loadReviewResume`
- [x] in `runReviewOnly` and `runCodexOnly`: `loadReviewResume` at entry
- [x] apply the resume: when `skipInternal`, print `review checkpoint: internal review completed in an earlier run, skipping` and skip `review.First` + `review.Loop`; otherwise run them and `saveReviewStage(internal_review)` after `Loop` returns nil; call `external.SetResume(completedReviewers, hadFindings)` before `external.Run`; when `skipPostReview`, skip the post-review `review.Loop(ctx, commitPrefix)` and go to finalize; otherwise run it and `saveReviewStage(post_review)` after it returns nil
- [x] implement the `OnReviewerDone` callback as `saveReviewStage(external_review, Index, Reviewer key, HadFindings, EndedBy)`
- [x] after `finalize.Run` returns in `runExternalAndPostReview` (all three callers), call `clearReviewCheckpoint("")` silently; a removal error is logged, not returned
- [x] write runner tests with the generated store mock and a `GitChecker` mock: no store → identical phase sequence and executor call counts to `TestRunner_CodexAndPostReview_PipelineOrder`; full-mode crash simulation (first run saves `internal_review` and reviewer 0, second run with a fresh runner skips both, runs reviewer 1, saves it, runs post-review, saves, finalizes, removes); task-phase commit invalidates; dirty tree skips save; unreadable checkpoint starts from scratch; `--review` and `--external-only` modes honor the checkpoint; `skipPostReview` path; save error does not fail the run
- [x] run `go test -race ./pkg/processor/...` - must pass before task 5

### Task 5: File-backed store in cmd/loopai and wiring
- [x] create `cmd/loopai/review_checkpoint_state.go` with `reviewCheckpointStore{path string}`: path is `filepath.Join(filepath.Dir(progressLogPath), strings.TrimSuffix(filepath.Base(progressLogPath), ".txt") + ".review.json")`; `Load` returns `found=false` on `os.IsNotExist`, a wrapped error on other read/parse failures; `Save` writes temp file + `Chmod(0o600)` + `os.Rename` like `savePlanChainCheckpoint`, stamping `Version`; `Remove` ignores not-exist
- [x] wire it in `executePlan` right after `createRunner`: `r.SetReviewCheckpoints(newReviewCheckpointStore(runnerLog.Path()))`, guarded so plan-creation and gen-agents modes (no review phases) skip it; the anchor is the progress log path, which is resolved in the main checkout before any worktree `chdir`, so the file survives worktree removal without consulting `MainGitSvc`
- [x] make `progressRecordRoots`/`readProgressAssociations` behavior explicit in a test: a `*.review.json` file in `.loopai/progress/` is ignored by the close-out association scan
- [x] write tests for the store with `t.TempDir()`: round-trip, missing file, corrupt JSON error, remove idempotent, temp file cleaned up, `0600` mode on Unix; and for the path derivation for `progress-<plan>.txt`, `progress-review.txt`, `progress-codex.txt`
- [x] run `go test ./cmd/...` - must pass before task 6

### Task 6: Verify acceptance criteria
- [x] verify a run without a checkpoint file produces the same phase order, prompts, and log lines as before (compare `TestRunner_CodexAndPostReview_PipelineOrder` and the full-mode tests still pass unchanged apart from the new setter)
- [x] verify resume after an external-reviewer crash skips internal review and the completed reviewers, and the next reviewer's first iteration still gets the full branch diff (`firstCompleted` is per `runLoop` and starts false)
- [x] verify the checkpoint is removed after a successful run and after a task-phase invalidation
- [x] run `make test` (asset checks, race-enabled Go suite, wrapper suites)
- [x] run `make lint` - all issues fixed
- [x] run `GOOS=windows GOARCH=amd64 go build ./...`
- [x] verify test coverage for `pkg/processor` and `cmd/loopai` new files meets the project standard (80%+)

### Task 7: [Final] Update documentation
- [x] `README.md`: in the worktree resume section (around line 783) and the execution pipeline section (around line 356), describe review checkpoints: where the file lives, which stages it records, the clean-tree rule, the ancestor rule, task-phase invalidation, removal on success, and that `--review`/`--external-only` reruns honor it too
- [x] `llms.txt`: extend the resume paragraph (line 183) with one sentence on review checkpoints
- [x] `CLAUDE.md`: add an architecture note under the worktree/chain paragraph naming `review_checkpoint.go`, `review_resume.go`, `review_checkpoint_state.go`, the stage keys, the clean-tree and ancestry rules, and why finalize is not checkpointed
- [x] `docs/backlog/`: file an entry proposing that a fresh `--worktree` run preserve its worktree on failure once review phases have begun, so uncommitted mid-reviewer fixes are not lost; note that the clean-tree gate is what keeps the checkpoint honest until then
- [x] confirm `make check-symlinks` and `make check-plugin` still pass (no skill changed, no manifest bump needed)

## Technical Details

### Checkpoint file

Path: `<main checkout>/.loopai/progress/<progress log stem>.review.json`, e.g.
`progress-abilities-engine-v2.review.json`. Written with `0600` via temp file and rename. Only the
runner writes it; only the runner removes it.

```json
{
  "version": 1,
  "mode": "full",
  "branch": "abilities-engine-v2",
  "plan": "docs/plans/20260901-abilities-engine-v2.md",
  "reviewers": ["claude:opus:high", "codex:gpt-6-astra:high", "claude:fable:high"],
  "stages": [
    {"stage": "internal_review", "head": "5f1c...", "completed_at": "2026-09-05T20:12:41Z"},
    {"stage": "external_review", "index": 0, "reviewer": "claude:opus:high", "head": "9a2e...",
     "had_findings": true, "ended_by": "done", "completed_at": "2026-09-05T21:03:09Z"},
    {"stage": "external_review", "index": 1, "reviewer": "codex:gpt-6-astra:high", "head": "c77d...",
     "had_findings": false, "ended_by": "done", "completed_at": "2026-09-05T21:40:52Z"}
  ]
}
```

### Resume resolution

Inputs: loaded checkpoint, current `Mode`, `Branch`, reviewer keys, `HeadHash`, and
`ContainsRevisionContext`. Output: `skipInternal`, `completedReviewers`, `hadFindings`,
`skipPostReview`, notes.

1. Version, mode, or branch mismatch → nothing resumes; one note.
2. Reviewer keys differ → only `internal_review` is eligible; one note.
3. Stages are walked in the fixed order `internal_review`, `external_review[0..n-1]`,
   `post_review`. A stage is honored when present and its `head` is contained in HEAD. The walk stops
   at the first missing or non-contained stage.
4. `hadFindings` is the OR of honored external stages' `had_findings`.
5. `skipPostReview` is true only when `post_review` is honored, which requires all reviewers honored.

### Processing flow (full mode)

```text
task.Run
  HEAD changed?  → clear checkpoint, resume = none
  else           → resume = load + resolve
skipInternal? skip First+Loop : run First, Loop, save(internal_review)
external.SetResume(completedReviewers, hadFindings); external.Run
  per reviewer done on clean tree → save(external_review)
hadFindings? (skipPostReview? skip : Loop(commitPrefix), save(post_review)) : skip post-review
finalize.Run
clear checkpoint
```

`--review` and `--external-only` follow the same flow without the task step.

### Logging

All checkpoint messages go through `Logger.Print` with the prefix `review checkpoint:` so they land in
the progress log and the dashboard: `resuming after <stage>`, `branch moved from <sha7> to <sha7>
since <stage>; resuming anyway`, `skipped: uncommitted changes`, `unreadable, starting reviews from
scratch`, `cleared: task phase committed new work`.

## Post-Completion

**Manual verification**:
- On a toy repo from `make e2e-prep`, run `loopai --worktree` with a two-reviewer chain, kill the
  process during the second reviewer, rerun the same command, and confirm the log shows the skipped
  internal review and first reviewer, then the second reviewer starting at iteration 1 with the full
  branch diff.
- Repeat with a manual commit on the branch between runs and confirm the `branch moved` note and
  resume.
- Repeat with `git reset --hard` of the branch to an earlier commit and confirm reviews restart.

**Follow-ups filed in `docs/backlog/`**:
- Preserve a fresh `--worktree` run's worktree on failure once review phases have begun, so
  uncommitted mid-reviewer fixes survive for the resumed run. Today `defer cleanup(true)` removes it,
  which is why the clean-tree gate exists.
- Consider a `--fresh-review` flag to ignore an existing checkpoint deliberately; omitted here because
  deleting the `.review.json` file achieves the same and the user asked for no new flags.

## Validation Commands

- `make test`
- `make lint`
- `GOOS=windows GOARCH=amd64 go build ./...`
