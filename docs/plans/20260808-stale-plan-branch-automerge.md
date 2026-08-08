# Auto-Merge Stale Plan Branch on Worktree Reuse

## Overview
- When a plan is re-run and its existing branch no longer contains the source HEAD (an unrelated plan advanced the source branch between runs), loopai currently aborts with `existing plan branch "X" does not include current HEAD; merge or rebase ...`. Change this to attempt the merge automatically: reuse the branch, merge the current source HEAD into it inside the fresh worktree, and proceed. Only a real merge conflict aborts, with the current actionable error text.
- Problem it solves: interleaved plans on one source branch make every resume fail with a manual-merge chore, even though in practice the merge is clean (the divergence is usually plan-bookkeeping commits like `move completed plan: ...`). The error message tells the human to do exactly what the tool can do itself.
- On Git 2.38+, conflict prediction happens in preflight via a `git merge-tree` dry-run, so a predicted conflict is rejected BEFORE `--commit` mutates the source checkout. Older Git skips prediction and relies on the real merge backstop; that path removes the new worktree on conflict, but a requested source auto-commit may already have occurred.

## Context (from discovery)
- Files/components involved:
  - `pkg/git/service.go` — `validateExistingPlanBranchAt` (~730, the strict ancestor check), `preflightWorktreeForPlan` (~672, early check before auto-commit), `createExistingPlanWorktree` (~620), `mergeAutoCommittedSource` (~643, existing merge-into-branch machinery, currently gated to the auto-commit path), `ErrMergeConflict` (~67)
  - `pkg/git/external.go` — `mergeRevision` (~409, aborts on conflict and wraps `ErrMergeConflict`), `isAncestor` (~451); new `mergeTree` dry-run primitive goes here
  - `pkg/git/service_test.go` — existing "does not include current HEAD" expectations (~1567, ~1586) flip to the new semantics
  - `CLAUDE.md`, `llms.txt`, `README.md` — describe the current strict reuse rule; must be rewritten
- Related patterns found:
  - `createExistingPlanWorktree` already handles merge-failure cleanup: merge error → `removeWorktree` → propagate; the generalized merge reuses this path as the backstop when the dry-run missed (e.g. racing writes)
  - `mergeRevision(ctx, revision, expectedHead)` performs the real merge with conflict abort semantics
  - `git merge-tree --write-tree` (git ≥ 2.38) detects conflicts without touching any worktree; exit status distinguishes clean/conflict
- Dependencies identified: none new; git ≥ 2.38 for `merge-tree --write-tree` (fallback: skip the dry-run and rely on the post-creation merge failure path)

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
- Run tests after each change
- Maintain backward compatibility: branch-creation path, `--resume-worktree`, and non-worktree branch mode keep their current behavior; only stale-branch *reuse* changes
- Tests must use `t.TempDir()` repositories and never touch real user configuration

## Testing Strategy
- **Unit tests**: table-driven tests in `pkg/git/service_test.go` with real temp git repos covering clean-divergence reuse, conflicting-divergence rejection, and the unchanged fast paths
- **E2E tests**: none (no dashboard changes)

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## Implementation Steps

### Task 1: Add merge-tree dry-run primitive to the git backend
- [x] add `mergeWouldConflict(ctx, base, branch string) (bool, error)` to `pkg/git/external.go` using `git merge-tree --write-tree <branch> <revision>`; conflict → true, clean → false; unsupported git (exit on unknown flag) → `(false, errUnsupported sentinel)` so callers can skip prediction
- [x] expose it through the repo interface consumed by `Service` (follow the existing `isAncestor` pattern)
- [x] write tests: clean merge predicted clean, conflicting file predicted conflicting, both against real temp repos; unsupported-git path covered with a stubbed runner if the suite's git is always modern
- [x] run tests - must pass before next task

### Task 2: Replace the hard ancestor rejection with auto-merge semantics
- [x] in `preflightWorktreeForPlan` / `validateExistingPlanBranchAt`: when the existing branch does not contain the required ancestor, run the merge-tree dry-run instead of returning the error; predicted-clean → preflight passes; predicted-conflict → return the current error text extended with the conflicting outcome ("would conflict; merge or rebase the source changes into it, or choose another --branch"); dry-run unsupported → preflight passes (post-creation merge is the backstop)
- [x] generalize `mergeAutoCommittedSource` into a source-sync merge that runs for every reused branch, not only when `existingBranchAncestor != ""`: after `addWorktree`, if the branch lacks the current source HEAD, `mergeRevision` it in; keep the existing conflict → `removeWorktree` → propagate cleanup path
- [x] make sure the no-divergence fast path stays commit-free (isAncestor check already short-circuits before merging)
- [x] log the auto-merge visibly: `merging source HEAD <short> into existing plan branch <name>` so progress files explain the extra merge commit
- [x] update existing service tests expecting the "does not include current HEAD" error on clean divergence to expect successful reuse with a merge commit; keep/adjust an error-path test with genuinely conflicting content
- [x] write new tests: clean divergence reused (worktree HEAD contains both branch work and source HEAD), conflicting divergence rejected in preflight before any auto-commit (source checkout unmutated), conflict discovered post-creation removes the worktree
- [x] run tests - must pass before next task

### Task 3: Verify acceptance criteria
- [x] verify all requirements from Overview are implemented (clean reuse just works; predicted conflicts fail early with actionable text; `--commit` sources not mutated on predicted conflict)
- [x] verify edge cases: detached HEAD source, branch identical to HEAD (no merge), plan branch checked out in another worktree (existing guard unaffected), git older than 2.38 fallback
- [x] run full test suite (`make test`)
- [x] run linter (`make lint`) - all issues must be fixed
- [x] verify test coverage meets project standard (80%+) for the new code paths
- [x] cross-compile `GOOS=windows GOARCH=amd64 go build ./...`

### Task 4: [Final] Update documentation
- [x] update CLAUDE.md worktree paragraph: reuse now auto-merges a source HEAD the plan branch does not already contain; only conflicts abort
- [x] update llms.txt `--worktree` paragraph ("An existing plan branch is reused only when it contains the source HEAD ... otherwise merge or rebase first" → new semantics)
- [x] update README.md if it repeats the strict rule

## Technical Details
- Reuse decision flow after the change: branch exists → on Git 2.38+, dry-run `merge-tree` against current HEAD → predicted conflict: fail preflight before source mutation; clean or prediction unsupported: create worktree on branch → `mergeRevision(sourceHead)` inside it (no-op when already contained) → success proceeds, while a real merge failure removes the worktree and aborts.
- The `-c/--commit` auto-commit case needs no special handling anymore: the generalized source-sync merge covers both "HEAD advanced by auto-commit" and "HEAD advanced by unrelated plans" with one code path — `existingBranchAncestor` plumbing can likely be simplified once both callers use the merged semantics (do it only if it falls out naturally; do not force a refactor).
- Preflight runs twice under the repository lock (before runtime ignores and after auto-commit, per `PreflightWorktreeForPlan` docs); the dry-run must be cheap enough for that (merge-tree is in-memory, no worktree I/O).
- Merge commit authored by the run inherits the same identity as today's auto-commit merge (`mergeRevision` path), so progress logs and `--merge` close-out behavior are unchanged.
- Real-world reproduction for manual testing exists: `fluxgenenerator` branch `r2-input-upload-dedup` diverged from `main` by plan-bookkeeping commits only.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- In a repo with an interrupted plan branch + advanced main (e.g. fluxgenenerator `r2-input-upload-dedup`): rerun the original loopai command → run proceeds, progress log shows the source-sync merge, worktree history contains both lines
- Create an artificial conflict (edit the same line on main and the plan branch) → rerun fails in preflight with the "would conflict" message and the dirty source checkout is not auto-committed

**External system updates**: none
