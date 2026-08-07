# Merge/PR Explicit Feature Branch Argument

## Overview

Allow `--merge` and `--pr` to accept an optional positional argument naming the feature to close out, so both commands can run from the primary checkout (or anywhere in the repository) instead of requiring the feature worktree to be the current directory.

Accepted argument forms:

- branch name: `loopai --merge dynamic-review-agents`
- plan basename with or without `.md`: `loopai --merge 20260806-dynamic-review-agents`
- plan path: `loopai --merge docs/plans/20260806-dynamic-review-agents.md`
- combined with explicit base: `loopai --merge=release/13 dynamic-review-agents`

Without the argument both commands behave exactly as today (feature = current branch, run from the feature worktree/checkout). With the argument:

- `--merge <feature>`: merges the resolved branch into the base worktree; if a feature worktree is registered it is verified clean and safely removed after the merge (current behavior); if no worktree exists the merge still proceeds and only the branch is deleted after ancestry verification
- `--pr <feature>`: pushes the resolved branch and creates the PR without requiring it to be checked out anywhere

This directly fixes the workflow gap where a finished worktree run cannot be closed out from the main checkout (`error: current branch "master" is already the base branch`).

## Context (from discovery)

- Files/components involved:
  - `cmd/loopai/main.go` — `runMergeCommand` (~line 2290), `runPRCommand` (~line 2520), `prepareMergeWorktrees` (~line 2343), standalone-mode routing and `PlanFile` positional arg handling
  - `pkg/plan` — `ExtractBranchName(filename)` derives branch from plan filename (already used for worktree creation and `findPRPlan` matching at `cmd/loopai/main.go:2809`)
  - `pkg/git/service.go` — `CurrentBranch`, `ResolveBaseBranch`, `Worktrees`, `EffectiveBranchName`, branch existence checks
- Related patterns found:
  - positional `PlanFile` arg is unused/rejected in `--merge`/`--pr` standalone modes, so the slot is free
  - close-out routing happens before executor and notification dependencies are constructed; `--merge`/`--pr` load config for `vcs_command` and colors only
  - existing merge tests use local bare remotes, git push stubs, and `PATH`-injected `gh` stubs
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
- Maintain backward compatibility: `--merge`/`--pr` without the argument must behave byte-for-byte as today
- Tests must redirect HOME/config paths to `t.TempDir()` and never touch real `~/.config/loopai/` or legacy `~/.config/ralphex/`
- Keep behavior cross-platform: use `filepath` for all path handling in the resolver
- Keep comments lowercase except exported godoc; table-driven tests with testify

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: dashboard Playwright suite unaffected (no web changes); do not add e2e tests
- Merge/PR flows are covered with the existing harness style: local bare remotes, temp repositories, git stubs, and `PATH`-injected `gh` stubs

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

### Task 1: Add feature identifier resolver

- [x] write failing table-driven tests for `resolveFeatureBranch(gitSvc, plansDir, arg)` in `cmd/loopai`: existing branch name wins; plan path resolves via `plan.ExtractBranchName`; plan basename with and without `.md`; plan found in `plansDir/completed/`; branch match takes priority over plan match; unknown identifier returns error listing searched locations; plan resolving to a nonexistent branch returns "already merged?" error; resolved branch equal to base is rejected by callers (covered in later tasks)
- [x] implement `resolveFeatureBranch` in `cmd/loopai/main.go`: check local branch existence first, then plan file lookup in `plansDir` and `plansDir/completed/`, derive branch with `plan.ExtractBranchName`, verify derived branch exists
- [x] use `filepath` for all path handling; accept both relative and absolute plan paths
- [x] run `go test ./cmd/...` - must pass before task 2

### Task 2: Wire positional argument into standalone mode routing

- [x] write failing tests: `loopai --merge <arg>` and `loopai --pr <arg>` route the positional value into close-out handling instead of rejecting it; `--merge`/`--pr` without positional arg keep current behavior; positional arg without `--merge`/`--pr` still means plan file for a run
- [x] pass the positional `PlanFile` value into `runMergeCommand`/`runPRCommand` as the optional feature identifier when the standalone modes are active
- [x] update flag descriptions/usage text for `--merge` and `--pr` to document the optional feature argument
- [x] run `go test ./cmd/...` - must pass before task 3

### Task 3: Explicit feature support in runMergeCommand

- [x] write failing tests for `--merge <feature>` run from the primary checkout on the base branch: feature worktree registered and clean → merged, worktree removed, branch deleted; feature worktree missing → merged directly in base worktree, branch deleted, no worktree cleanup attempted; dirty feature worktree → clean-tree error; feature resolving to base → "already the base branch" error; unknown feature → resolver error propagated
- [x] refactor `runMergeCommand`: when an explicit feature is given, resolve it via `resolveFeatureBranch` instead of `CurrentBranch()`, and locate the feature worktree from `Worktrees()` instead of requiring the current checkout to be it
- [x] adjust `prepareMergeWorktrees` (or add a sibling path) to support: feature checked out in a registered worktree elsewhere, and feature not checked out anywhere (merge executes in the base worktree; skip worktree cleanup)
- [x] keep ancestry verification, conflict abort as `git.ErrMergeConflict`, branch deletion only after verified cleanup, and pill clearing unchanged
- [x] verify no-argument behavior is untouched (existing tests must pass unmodified)
- [x] run `go test ./cmd/... ./pkg/git/...` - must pass before task 4
- ➕ added `git.Service.BranchHash` (plus backend `branchHash`) so the merge can read the feature head without the branch being checked out; covered by new `pkg/git` tests
- ➕ `prepareMergeWorktrees` now returns a `mergeTargets` struct (merge worktree, optional feature worktree/path, primary path) instead of four values; feature-worktree cleanliness is validated before the merge, not only before cleanup

### Task 4: Explicit feature support in runPRCommand

- [x] write failing tests for `--pr <feature>` run from the primary checkout: named branch is pushed and PR created via `gh` stub with correct `--head`; plan metadata derived through existing `buildPRTitleBody` for the named branch; feature resolving to base → error; unknown feature → resolver error; no-argument behavior unchanged
- [x] refactor `runPRCommand`: with an explicit feature, skip the `CurrentBranch()` requirement, resolve the identifier, and use the resolved branch for diff stats, metadata, push, and `gh pr create --head`
- [x] keep origin validation, pill retention on failure, and branch/worktree preservation unchanged
- [x] run `go test ./cmd/...` - must pass before task 5
- ➕ `git.Service.DiffStats` was HEAD-only, so an explicit feature reported zero changes from the base checkout; the backend `diffStats` now takes an explicit head ref and `git.Service.BranchDiffStats(base, branch)` measures a branch tip without checking it out (covered by new `pkg/git` tests)
- ➕ the "already the base branch" message now matches `--merge`: explicit features get "name a different feature" instead of "check out the feature branch first"

### Task 5: Verify acceptance criteria

- [x] verify all requirements from Overview: all four argument forms work for both commands, explicit base combines with explicit feature, worktree-less merge works
- [x] verify edge cases: plan in `completed/` only, branch-over-plan priority, resolver errors are actionable, detached HEAD in primary checkout with explicit feature still works
- [x] run full test suite via `make test`
- [x] run `make lint` - all issues must be fixed
- [x] cross-compile check: `GOOS=windows GOARCH=amd64 go build ./...` (path handling in resolver)
- [x] verify test coverage of new code paths meets project standard
- ➕ added two gap-filling tests: `--merge=release/13 feature` (explicit base combined with an explicit feature, asserting the default base stays untouched) and `--pr <plan path>` (plan-path argument form at the command level)
- 📊 coverage of the new paths: `resolveFeatureBranch`/`findFeaturePlanFile`/`existingPlanFile` and `git.BranchHash`/`git.BranchDiffStats` at 100%, `runMergeCommand` 82.9%, `runPRCommand` 85.7%, `prepareMergeWorktrees` 80.6%; the remainder are git-command failure branches

### Task 6: Update documentation

- [ ] update `llms.txt`: new argument forms for `--merge`/`--pr` with examples
- [ ] update `CLAUDE.md` close-out section: explicit feature resolution, worktree-less merge behavior
- [ ] update `README.md` user documentation for close-out commands
- [ ] update `--help` texts if not already done in task 2

## Technical Details

- Resolver order is deterministic: (1) exact local branch match, (2) plan file in `plansDir` then `plansDir/completed/` (basename normalized, `.md` optional, paths accepted), deriving the branch via `plan.ExtractBranchName` and requiring it to exist, (3) error listing searched locations
- `--merge` execution site is always the base branch's worktree (today's behavior); the change only relaxes where the command may be invoked from and how the feature is named
- When no feature worktree is registered, the merge path skips `leaveWorktreeBeforeRemoval`/worktree removal entirely; branch deletion still requires the ancestry check to pass
- `--pr` needs no checkout at all for an explicit feature: `DiffStats(base)` and `buildPRTitleBody(root, branch, stats)` already operate on names, and `gh pr create --head <branch>` does not depend on the checked-out branch; push uses the existing `PushContext(ctx, branch)`
- Base semantics unchanged: `--merge=<base>`/`--pr=<base>` keep accepting only the base value via `=`
- No changes to `pkg/status` signals or the module path

## Post-Completion

**Manual verification**:

- on this repository: `loopai --merge dynamic-review-agents` from the primary checkout (current real pending branch without a worktree) and confirm merge, branch deletion, and pill clearing
- worktree round-trip on a toy repo (`make e2e-prep`): run `--worktree`, then `loopai --merge <plan-basename>` from the main checkout with the worktree still present
- `loopai --pr <branch>` against a real GitHub remote once, confirming PR base/head and metadata

**External system updates**: none — feature is fully local to loopai
