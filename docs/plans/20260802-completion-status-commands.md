# Persistent cmux completion status and close-out commands (--clear, --merge, --pr)

## Overview

After a run finishes, loopai currently tears down every cmux sidebar artifact and only
raises a transient banner, so the workspace shows nothing about the finished run and the
user cannot tell that a merge/PR decision is pending. This plan adds:

- a persistent cmux status pill on completion: green `bolt` "done in <elapsed>" on
  success, red `exclamationmark.triangle` "failed" on error; abort (Ctrl+C) keeps the
  current full-cleanup behavior
- `loopai --clear`: remove the loopai status pill and exit
- `loopai --merge [base]`: merge the current feature branch into the base branch
  (explicit arg, else `main` then `master`), clean up worktree and branch, clear the pill
- `loopai --pr [base]`: push the current branch and create a GitHub PR via `gh` with
  title/body derived from the completed plan, then clear the pill

The pill is self-clearing in practice: any next loopai run in the workspace overwrites
it, `--clear`/`--merge`/`--pr` remove it explicitly, and cmux restart or workspace close
drops it. No TTL hacks (detached sleep processes) are used.

## Context (from discovery)

- `pkg/cmux/cmux.go`: `Reporter` with `setStatus`/`clearStatus` (key `loopai`),
  `Stop()` clears spinner, pill, and progress unconditionally; `commandRunner` fake
  already exists in tests. cmux CLI: `set-status <key> <text> --icon <sf-symbol>
  --color <#hex> --priority <n>`; no TTL support (verified).
- `cmd/loopai/main.go`: `displayStats` prints the terminal summary;
  `notifyCmuxCompletion`/`cmuxCompletionNotice` build the transient banner and already
  distinguish success / failure / user abort (`processor.ErrUserAborted`,
  `context.Canceled`); opts struct uses go-flags with boolean mode flags (`--review`,
  `--init`, `--reset`).
- `pkg/git/service.go` + `pkg/git/external.go`: `CurrentBranch`, `GetDefaultBranch`,
  `BranchExists`, `RemoveWorktree`, `isDirty`, `checkoutBranch` exist; merge, safe
  branch delete, and push are missing.
- Worktrees live under `.loopai/worktrees/<branch>`; a branch checked out in a worktree
  cannot be deleted until the worktree is removed.
- Completed plans live in `docs/plans/completed/`; plan title is the first `# ` heading.

## Development Approach

- **Testing approach**: Regular (implementation with tests in the same task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- cmux calls stay best-effort: failures must never affect execution or exit codes
- git operations must be cross-platform (`filepath`, no shell-isms)
- follow existing patterns: table-driven tests with `testify`, `t.TempDir()` git repos,
  fake `commandRunner` for cmux, one `_test.go` per source file

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: dashboard Playwright suite is unaffected (no web changes); do not touch

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs in this repo
- **Post-Completion** (no checkboxes): manual cmux visual verification
- Checkboxes belong only in Task sections

## Implementation Steps

### Task 1: Add final-status support to cmux Reporter

- [x] add `Finish(success bool, detail string)` to `pkg/cmux/cmux.go`: sets the pill to
      green `#34c759` icon `bolt` text `done in <detail>` on success, red `#ff3b30`
      icon `exclamationmark.triangle` text `failed` (detail appended when short) on
      error; marks the reporter finished under `statusMu`
- [x] change `Stop()` to skip `clearStatus()` when `Finish` ran (spinner and progress
      are still cleared); abort path keeps current behavior because `Finish` is not
      called there
- [x] make `Start()` overwrite any leftover final pill from a previous run (explicit
      `clearStatus` before `loadingOn`)
- [x] keep nil-reporter and post-Stop safety: `Finish` on nil or stopped reporter is a
      no-op
- [x] write tests with the fake runner: finish-success argv (color/icon/text),
      finish-failure argv, Stop-after-Finish preserves pill, Stop-without-Finish clears,
      Finish-after-Stop no-op, nil reporter no-op
- [x] run tests - must pass before next task

### Task 2: Wire Finish into run completion paths

- [x] in `cmd/loopai/main.go`, call `rep.Finish` alongside `notifyCmuxCompletion` using
      the same outcome classification as `cmuxCompletionNotice` (success → elapsed,
      failure → error, user abort / context canceled → no Finish, plain Stop)
- [x] cover plan-execution and review-mode completion paths; plan-creation mode keeps
      its current transient notify only
- [x] write tests for outcome classification → Finish invocation (success, run error,
      ErrUserAborted, context.Canceled)
- [x] run tests - must pass before next task

### Task 3: Add --clear command

- [x] add `Clear bool` flag (`--clear`, description: remove loopai cmux status pill) to
      opts in `cmd/loopai/main.go` and handle it early like `--init`/`--reset` modes
- [x] implement clear: nil reporter (outside cmux) prints a short note and exits 0;
      inside cmux runs `clear-status loopai` and exits 0
- [x] reject combining `--clear` with a plan file or other mode flags with a clear error
- [x] write tests for flag parsing, in-cmux argv, outside-cmux no-op exit
- [x] run tests - must pass before next task

### Task 4: Add merge, safe branch delete, and base detection to pkg/git

- [ ] add `ResolveBaseBranch(explicit string) (string, error)` to `pkg/git`: explicit
      name must exist; otherwise first existing of `main`, `master`; error naming both
      when neither exists
- [ ] add `MergeBranch(branch string) error`: runs `git merge <branch>` on the current
      HEAD (default ff-when-possible behavior); on conflict runs `git merge --abort`
      and returns a distinct error (e.g. `ErrMergeConflict`) wrapping git output
- [ ] add `DeleteBranch(name string) error` using safe `git branch -d`
- [ ] add `Push(branch string) error` running `git push -u origin <branch>` (needed by
      Task 6; kept here to finish pkg/git changes in one task)
- [ ] write table-driven tests with `t.TempDir()` repos: base detection (explicit,
      main, master, neither), clean merge, conflicting merge aborts and preserves both
      branches, safe delete of merged/unmerged branch; push tested against a local bare
      remote
- [ ] run tests - must pass before next task

### Task 5: Add --merge command

- [ ] add `Merge string` flag (`--merge`, optional value = base branch,
      `optional-argument` style so bare `--merge` auto-detects) handled as an early
      standalone mode
- [ ] implement flow in `cmd/loopai`: require clean working tree (`isDirty` surface),
      resolve base via `ResolveBaseBranch`, error when current branch equals base;
      checkout base → `MergeBranch(feature)`
- [ ] on success: remove `.loopai/worktrees/<branch>` via `RemoveWorktree` when it
      exists, `DeleteBranch(feature)`, clear the cmux pill, print summary (base, branch,
      ff/merge)
- [ ] on conflict: merge already aborted by pkg/git; checkout the original branch back,
      print actionable error, keep the pill, exit non-zero
- [ ] write tests: happy path with and without worktree, dirty-tree refusal,
      current==base refusal, conflict path restores original branch and keeps branch
      alive
- [ ] run tests - must pass before next task

### Task 6: Add --pr command

- [ ] add `PR string` flag (`--pr`, optional value = base branch) handled as an early
      standalone mode; require `gh` in PATH with an install-hint error otherwise
- [ ] derive PR title/body: locate the plan for the current branch in
      `docs/plans/completed/` (match by branch-derived plan name, fallback: newest
      completed plan mentioning the branch); title = plan `# ` heading (fallback:
      branch name), body = plan Overview section + diff stats vs base; body generation
      lives in a testable helper
- [ ] implement flow: resolve base, `Push(branch)`, run `gh pr create --base <base>
      --head <branch> --title ... --body ...`, print the PR URL from gh output, clear
      the cmux pill; branch and worktree stay
- [ ] write tests: title/body builder (plan found, plan missing, no Overview section);
      flow tests with a stub `gh` binary on PATH (success prints URL and clears pill,
      gh failure keeps pill and exits non-zero)
- [ ] run tests - must pass before next task

### Task 7: Verify acceptance criteria

- [ ] verify all Overview requirements are implemented (pill on success/failure, abort
      unchanged, --clear/--merge/--pr semantics including pill clearing)
- [ ] verify mode-flag exclusivity errors are consistent across --clear/--merge/--pr
- [ ] run full test suite (`make test`) - must pass
- [ ] run linter (`make lint`) - all issues must be fixed
- [ ] cross-compile `GOOS=windows GOARCH=amd64 go build ./...` (git/path handling)
- [ ] verify test coverage of new code meets project standard (80%+)

### Task 8: [Final] Update documentation

- [ ] update README.md: completion status pill behavior and the three new commands
- [ ] update llms.txt with the new flags
- [ ] update CLAUDE.md cmux section note (final pill is intentionally left after
      completion; Stop no longer clears it on finished runs)
- [ ] update embedded config comments only if any new config keys appeared (none
      expected)

## Technical Details

- Pill spec: key `loopai` (existing `statusKey`), priority unchanged; success
  `set-status loopai "done in 2h24m" --icon bolt --color #34c759`, failure
  `set-status loopai "failed" --icon exclamationmark.triangle --color #ff3b30`.
  Icons are SF Symbols names accepted by cmux.
- `Finish` participates in the existing `statusMu`/`stopped` gating so a Finish racing
  Stop cannot re-add a pill after cleanup on the abort path.
- `--merge`/`--pr` run without a plan file and never start the processor pipeline; they
  reuse `progress.Colors` for output and exit through the standard error path.
- Merge uses default git semantics (fast-forward when possible, merge commit
  otherwise) - no `--no-ff` flag in scope.
- `--pr` targets GitHub only (`gh`); other forges are out of scope.
- No automatic post-run merge/PR (`on_complete`-style config) - explicitly out of scope.

## Post-Completion

**Manual verification**:

- inside a cmux workspace: run a plan to completion, confirm the green bolt pill
  persists after exit and survives until `loopai --clear` / next run; force a failure
  and confirm the red pill; Ctrl+C a run and confirm no pill remains
- confirm `loopai --merge` on a real finished branch cleans worktree and branch and
  removes the pill; `loopai --pr` on a GitHub-hosted repo opens a well-formed PR

**External system updates**: none
