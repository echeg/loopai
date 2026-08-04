# Design: worktree from any branch + `--commit` auto-commit

Date: 2026-08-04
Status: approved (pending spec review)

## Problem

`loopai --worktree plan.md` fails with `worktree creation requires main branch, currently on "ch_main"` when run from any branch other than the resolved default branch. Users working on long-lived feature branches (e.g. `ch_main`) cannot use worktree mode at all without checking out main first.

Separately, a dirty working tree blocks worktree creation with "cannot create worktree: worktree has uncommitted changes other than the plan file", forcing a manual stash/commit round-trip.

## Decision summary

1. `--worktree` creates the worktree and feature branch from the **current HEAD**, whatever branch (or detached HEAD) the checkout is on. The default-branch guard is removed for worktree mode only.
2. The base branch is **not recorded**. Review diffs and `--merge` keep their existing base resolution: `--base-ref` / `default_branch` config / auto-detected main-master. Consequence (accepted): after running from a non-default branch, `--merge` needs an explicit base argument to land back on that branch, and review diffs compare against the default branch unless `--base-ref` is passed.
3. Non-worktree branch mode (`loopai plan.md`) is **unchanged**: from a non-default branch it still works in place without creating a plan branch.
4. New flag `-c` / `--commit`: before creating the worktree, auto-commit the entire dirty working tree (including untracked files, `git add -A`, respecting gitignore) **on the current branch**. The feature branch is then cut from that clean HEAD and includes those changes.

## Behavior details

### Worktree creation (`pkg/git/service.go`)

- `preparePlanBranch` with `requireDefault=true` no longer compares the current branch against the default branch. Dirty-tree checks and the plan-file auto-commit logic stay as-is.
- New guard replacing the old one: if the current branch equals the plan-derived branch name, fail with a clear message ("plan branch already checked out here; switch to the base branch or run without --worktree") instead of letting `git worktree add` fail cryptically — git refuses to check out one branch in two worktrees.
- Existing plan branch (re-run): unchanged — the worktree is created on the existing branch; the current branch is irrelevant.
- Detached HEAD: allowed; the new branch is cut from HEAD.
- Creation log line names the source: `creating worktree with new branch: X (from ch_main)`.

### Base-ref resolution (`cmd/loopai/main.go`)

- `resolveBranchBase` loses its worktree-specific error paths (`--base-ref %q is not a branch; worktree creation needs a branch...` and `--base-ref names a branch but the checkout is on...`). In worktree mode `--base-ref` is purely a diff base for reviews and templates; branch creation always uses the current HEAD.
- The not-on-default-branch informational message in the worktree startup path is updated or dropped to match the new behavior.

### `--commit` flag

- `-c` / `--commit`, only valid together with `--worktree`; using it without `--worktree` is a CLI error.
- The short flag `-c` is taken from the deprecated `--codex-only` alias; `--codex-only` (long form) keeps working, only its short form is reassigned.
- Semantics: if the working tree is dirty at worktree-creation time, run `git add -A` + commit on the **current branch** before cutting the feature branch. Commit message: `auto-commit working tree before plan: <branch>` plus the standard loopai trailer (`appendTrailer`). Clean tree: flag is a no-op.
- `.loopai/` runtime artifacts are already excluded via `.loopai/.gitignore` (progress/, worktrees/), so `git add -A` cannot capture them.
- With `--commit`, the plan file (if new/modified) is committed on the current branch as part of the auto-commit; the existing plan-copy/commit-in-worktree path then sees it already committed and skips.

## Testing

- Table-driven tests in `pkg/git/service_test.go`: worktree creation from a non-default branch, collision when standing on the plan branch itself, detached HEAD, auto-commit path (dirty tree committed on current branch, worktree includes changes), auto-commit no-op on clean tree.
- `cmd/loopai` tests: `resolveBranchBase` simplification, `--commit` without `--worktree` rejected, `-c` no longer maps to `--codex-only`.
- No regression changes to non-worktree branch-mode tests; that behavior is unchanged.

## Documentation

- README, `llms.txt`, and embedded config comments: remove "worktree requires the default branch" statements; document "worktree is cut from the current branch; reviews diff against the default branch or `--base-ref`" and the new `--commit` flag.
- `CLAUDE.md` architecture notes mentioning worktree behavior updated accordingly.

## Out of scope

- Recording the base branch for later use by reviews or `--merge` (explicitly declined).
- Changing non-worktree branch creation.
- Any migration or new config keys.
