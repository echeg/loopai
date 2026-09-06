# Review-mode preflight guards

## Overview

Two guards for the review-only modes `--review`, `--external-only`, and `--codex-only`, which run
in the user's own checkout and create neither a branch nor a worktree:

1. **Warn when an explicit `--worktree` is ignored.** `selectAndExecutePlan` enters
   `runWithWorktree` only when `modeRequiresBranch(req.Mode)` is true
   (`cmd/loopai/main.go:670`), so `loopai --worktree -e plan.md` silently drops the flag and runs
   the review wherever HEAD happens to be. Today there is no message at all; the only comparable
   case, `-c/--commit` on a resumed worktree, does warn (`main.go:1919`).
2. **Refuse a review whose diff range is empty.** The first review iteration diffs
   `git diff <base>...HEAD` (`pkg/processor/prompts.go:80`). When HEAD is the base branch itself,
   or a feature branch already merged into it, that range is empty. The reviewer honestly reports
   "nothing to review", the evaluator treats "NO ISSUES FOUND" as the completion signal, and loopai
   prints `completed` after one minute: a green result with zero code inspected. The run should
   fail before the progress log and reporters exist, naming the base and HEAD and pointing at the
   fix (check out the feature branch or pass `--base-ref`).

Observed on 2026-09-06 in IdleMergeWeb: a full `--worktree` run failed inside the second external
reviewer, its worktree was removed, and the retry
`loopai --worktree --codex ... -e docs/plans/20260905-dataforge-fold-in-tails.md` was launched
from the primary checkout on `main`. Output showed `branch: main`, the reviewer wrote
"`git diff main...HEAD` is empty", and the run completed successfully having reviewed nothing.

Both guards are preflight only. They change no prompt, no phase, no signal handling, and nothing
for full, `--tasks-only`, `--plan`, or `--gen-agents` runs.

## Decisions

- **Context**: `--worktree` and the review modes compose silently, and an empty review diff ends
  green. Both are cheap to detect before any executor starts.
- **Chosen approach for the warning**: a pure `worktreeIgnoredWarning(o opts, mode processor.Mode)
  string` in `cmd/loopai/main.go`, printed to stderr right after `determineMode` next to
  `printExternalReviewWarnings`. It fires only on the explicit CLI flag `o.Worktree`, never on the
  `use_worktree` config key: a config default is meant for every run and would nag on every review.
  When review flags are combined, the warning names the flag that selected the effective mode,
  following `determineMode` precedence.
- **Chosen approach for the refusal**: a new `git.Service.DiffRangeEmptyContext(ctx, base)
  (bool, error)` built on exact context-aware revision validation and
  `git diff --quiet <base>...HEAD --` backend operation, plus a `checkReviewDiffRange(ctx, gitSvc,
  mode, baseRef) error` helper in `cmd/loopai`. Review-only runs select their optional plan and run
  the guard before executor dependency checks or reporter creation; `selectAndExecutePlan` retains
  the check for direct callers. Testing the actual tree diff also catches empty commits and branches
  whose net changes were reverted, while a missing merge base remains an error. Uncommitted changes
  are deliberately not consulted: the first iteration diff is commit-to-commit and never shows
  them either.
- **Rejected: `IsDefaultBranch` check**. Misses `--base-ref <hash>`, a non-default base, and a
  merged feature branch, all of which produce the same empty range.
- **Rejected: reusing `DiffStats`**. `externalBackend.diffStats` returns zero stats for an
  unresolvable base (`pkg/git/external.go:1186`), which would make "base branch does not exist"
  indistinguishable from "nothing to review". The new method returns an error for an unresolvable
  base so the message names the real problem.
- **Rejected: turning the empty range into a warning**. The failure mode is a false green; a
  warning above a `completed` line is exactly what gets skimmed past. `--base-ref` is the escape
  hatch for anyone who wants a different range.
- **Rejected: making the warning an error**. `--worktree -e` is harmless when HEAD already is the
  feature branch, and the user may keep one command line for both phases.
- **Verified facts**: `modeRequiresBranch` is true only for `ModeFull` and `ModeTasksOnly`
  (`main.go:2638`); `-e` maps to `ModeCodexOnly` (`main.go:2627`); `req.BaseRef` for review modes
  is `resolveDefaultBranch(cliBaseRef, configBranch, autoDetected)` and may carry an `origin/`
  prefix (`main.go:5473`, `5485`); the preflight must validate that literal value rather than use
  `externalBackend.resolveRef`, because the review prompt receives the literal value too; backend
  Git commands support context-aware
  cancellation and preserve diagnostics; the `repo` interface at `service.go:39-70` is the place
  to expose the exact quiet diff operation; `setupTestRepo` and `runGit` in
  `cmd/loopai/main_test.go:5200-5218` build real temporary repositories for tests;
  `printExternalReviewWarnings` is the stderr-warning precedent and is tested by capturing a
  `bytes.Buffer` (`main_test.go:3254`).

## Context (from discovery)

- Files/components involved:
  - `cmd/loopai/main.go`: `determineMode` call site (~440), `printExternalReviewWarnings` (2503),
    `selectAndExecutePlan` (652-683), `modeRequiresBranch` (2638), `applyCLIOverrides` (5360).
  - `pkg/git/service.go`: `repo` interface (39-70), `ContainsRevisionContext` (244), `DiffStats`
    (2137).
  - `pkg/git/external.go`: `resolveRef` (1239), `isAncestor` (537), `diffStats` (1183).
  - Docs: `README.md` (usage examples 262-280, `--base-ref` review passage 796-806, review-mode
    paragraph ~500), `llms.txt` (usage list 78-86, review-mode sentence in 171), `CLAUDE.md`
    architecture section.
- Related patterns found: stderr warnings via an `io.Writer` parameter for testability; git
  helpers exposed as `...Context` methods that wrap `repo` calls with `fmt.Errorf("...: %w")`;
  table-driven tests with real temporary repositories.
- Dependencies identified: none new.

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
- Maintain backward compatibility: full, `--tasks-only`, `--plan`, and `--gen-agents` runs are
  byte-for-byte unchanged; review modes with a non-empty range are unchanged apart from the new
  warning line when `--worktree` is passed
- Tests must use `t.TempDir()` and never touch `~/.config/loopai/` or `~/.config/ralphex/`

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: not applicable; the dashboard is untouched. Cross-compile with
  `GOOS=windows GOARCH=amd64 go build ./...` in the verification task since `cmd/loopai` has
  platform-specific files.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, and documentation changes in this repo
- **Post-Completion** (no checkboxes): manual smoke test
- **Checkbox placement**: checkboxes belong only in Task sections

## Implementation Steps

### Task 1: Warn when an explicit --worktree is ignored by a review-only mode
- [x] add `worktreeIgnoredWarning(o opts, mode processor.Mode) string` to `cmd/loopai/main.go`: returns `""` unless `o.Worktree` is true and `mode` is `processor.ModeReview` or `processor.ModeCodexOnly`; otherwise returns `warning: --worktree is ignored by <flag>; review modes run in the current checkout and create no branch or worktree`, where `<flag>` is the review flag that selected the effective mode (`--external-only`/`--codex-only` take precedence over `--review`, matching `determineMode`)
- [x] print the non-empty result to `os.Stderr` immediately after `mode := determineMode(o)` (~`main.go:440`), before `resolveExternalReviewSelection`, so it appears with the other startup warnings and before any executor check can fail
- [x] write table-driven tests for `worktreeIgnoredWarning`: `--worktree` with each of the three review flags names the right flag; `--worktree` in full, `--tasks-only`, `--plan`, and `--gen-agents` modes returns `""`; review modes without `--worktree` return `""`; `use_worktree` in config alone (`o.Worktree` false) returns `""`
- [x] run `go test ./cmd/...` - must pass before task 2

### Task 2: git.Service.DiffRangeEmptyContext
- [x] add `DiffRangeEmptyContext(ctx context.Context, base string) (bool, error)` to `pkg/git/service.go`: validate the literal `base` through the context-aware revision check, return a specific missing-base error when absent, then run the backend's exact `git diff --quiet <base>...HEAD --`, interpreting exit 0 as empty, exit 1 as non-empty, and other exits as errors; do not normalize to a different ref because the reviewer prompt receives the literal value
- [x] document that uncommitted changes are not considered because the review's first-iteration diff is commit-to-commit
- [x] write tests in `pkg/git/service_test.go` with temporary repositories: changed feature branch → `false`; base tip, detached base tip, merged feature, empty commit, and net-reverted branch → `true`; literal remote-tracking refs resolve while unavailable `origin/master` and bare remote-only names do not fall back; unknown base, unrelated history, and canceled context → errors
- [x] run `go test ./pkg/git/...` - must pass before task 3

### Task 3: Refuse review-only runs with an empty diff range
- [x] add `checkReviewDiffRange(ctx context.Context, gitSvc *git.Service, mode processor.Mode, baseRef string) error`: return immediately for non-review modes; wrap lookup/diff errors as `review preflight`; and report an empty range with the base plus branch or detached short hash and guidance to check out the feature branch or pass `--base-ref`
- [x] for review-only modes, select the optional plan and run the guard in `run` before executor dependency checks, progress logging, and reporter creation; mark the request so `selectAndExecutePlan` does not repeat selection, while retaining its guard for direct callers
- [x] write tests for `checkReviewDiffRange` with `setupTestRepo`/`runGit`: `ModeReview` on a feature branch ahead of the base → `nil`; `ModeReview` on the base branch → error containing `nothing to review` and the base name; `ModeCodexOnly` on a merged feature branch → error; `ModeReview` with an unknown `--base-ref` → error containing `base ref not found`; `ModeFull` and `ModeTasksOnly` on the base branch → `nil`; `ModePlan` → `nil`
- [x] add one test through `selectAndExecutePlan` (or its closest existing harness in `main_test.go`) proving a `ModeReview` request on the base branch fails before `.loopai/progress/` is created
- [x] run `go test ./cmd/...` - must pass before task 4

### Task 4: Verify acceptance criteria
- [x] verify `loopai --worktree --review` prints the warning once and still runs when HEAD is a feature branch; a run-level empty-range test proves the warning is wired and the guard precedes reporters and dependency checks
- [x] verify a review-only run on the base branch exits non-zero with the `nothing to review` message and creates no progress log
- [x] verify full-mode behavior is unchanged: existing `TestResolveBaseRefs`, `TestApplyCLIOverrides_*`, and worktree tests pass without modification
- [x] run `make test` (asset checks, race-enabled Go suite, wrapper suites)
- [x] run `make lint` - all issues fixed
- [x] run `GOOS=windows GOARCH=amd64 go build ./...`
- [x] verify test coverage for the new functions meets the project standard (80%+)

### Task 5: [Final] Update documentation
- [x] `README.md`: in the usage examples (~262-280) add a comment under `loopai --review`/`loopai --external-only` that they run in the current checkout and fail with `nothing to review` when the range has no committed changes; in the `--base-ref` review passage (~796-806) note that `--base-ref` is also how to review against a different base when the default range is empty; in the review-mode paragraph (~500) add that an explicit `--worktree` is ignored with a warning there
- [x] `llms.txt`: one sentence after the `loopai --review` / `loopai --external-only` lines (~82) stating the empty-range refusal and the ignored-`--worktree` warning
- [x] `CLAUDE.md`: one short architecture note near the worktree paragraph naming `worktreeIgnoredWarning`, `checkReviewDiffRange`, and `git.Service.DiffRangeEmptyContext`, with the exact quiet-diff rule and why `DiffStats` was not reused
- [x] confirm `make check-symlinks` and `make check-plugin` still pass (no skill changed, no manifest bump needed)

## Technical Details

### Warning

```text
warning: --worktree is ignored by --external-only; review modes run in the current checkout and create no branch or worktree
```

Emitted once, to stderr, before executor resolution. Driven by `o.Worktree` only.

### Empty-range check

```text
run (review-only modes)
  select plan
  checkReviewDiffRange(mode, req.BaseRef)     # ModeReview / ModeCodexOnly only
    revisionExists(base) == false  → error: base ref not found
    git diff --quiet base...HEAD --
      exit 0 → error: nothing to review
      exit 1 → continue
      other  → preserve Git error
  check executor dependencies and start setup reporter
  runWithWorktree | prepareSelectedPlanBranch | executePlan (unchanged)
```

Error text:

```text
nothing to review: git diff main...HEAD is empty for HEAD (main) against base "main"; check out the feature branch or pass --base-ref
```

### Exact diff rule

`git diff A...B` compares the merge-base tree to B's tree. An ancestry predicate catches the common
base-tip and merged-feature cases but misses an ahead branch made only of empty commits or changes
that net back to the merge-base tree. Running the same quiet three-dot diff used by the reviewer
tests the actual patch and also reports unrelated histories instead of treating them as reviewable.

## Post-Completion

**Manual verification**:
- In a toy repository from `make e2e-review`, run `.bin/loopai --review` on `main` and confirm the
  `nothing to review` error, exit code 1, and no new file under `.loopai/progress/`.
- Check out the feature branch and run `.bin/loopai --worktree --review`; confirm the warning line
  appears once and the review proceeds as before.
- Run `.bin/loopai --review --base-ref <feature-tip-hash>` from the feature branch to confirm the
  same refusal fires for a hash base equal to HEAD.

## Validation Commands

- `make test`
- `make lint`
- `GOOS=windows GOARCH=amd64 go build ./...`
