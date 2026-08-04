# Worktree from any branch + --commit auto-commit

## Overview

- `--worktree` currently fails with `worktree creation requires main branch, currently on "X"` when run from any branch other than the resolved default branch. Remove that guard: the worktree and its feature branch are cut from the **current HEAD**, whatever branch (or detached HEAD) the checkout is on.
- Add `-c` / `--commit`: when the working tree is dirty at worktree-creation time, auto-commit everything (`git add -A`, respecting gitignore) on the **current branch** before cutting the feature branch, instead of failing with the dirty-tree error.
- The base branch is deliberately **not recorded**. Review diffs and `--merge` keep their existing base resolution (`--base-ref` / `default_branch` config / auto-detected main-master). Non-worktree branch mode is unchanged.
- Design spec: `docs/superpowers/specs/2026-08-04-worktree-any-branch-design.md`.

## Context (from discovery)

- `pkg/git/service.go` — `preparePlanBranch` (line ~337) holds the guard: `requireDefault=true` (worktree path) errors when not on the default branch; `CreateWorktreeForPlan` (~437) passes `requireDefault=true`; `CreateBranchForPlan` (~392) passes `false` and must stay unchanged.
- `pkg/git/external.go` — `createInitialCommit` already demonstrates the `add -A` + porcelain-check + commit sequence to reuse for the auto-commit backend method.
- `cmd/loopai/main.go` — opts struct (~line 39): `CodexOnly` owns `short:"c"`; worktree creation call at ~1031 (`CreateWorktreeForPlan`); `resolveBranchBase` (~3225) has two worktree-specific error paths that become obsolete.
- `.loopai/.gitignore` (written by `EnsureLocalGitignore`) already excludes `progress/` and `worktrees/`, so `git add -A` cannot capture runtime artifacts.
- `appendTrailer` in `pkg/git/service.go` is the existing helper for loopai commit trailers.

## Development Approach

- **Testing approach**: Regular (implementation with tests in the same task), table-driven with `testify`, filesystem via `t.TempDir()`.
- Complete each task fully before moving to the next.
- Make small, focused changes.
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task; tests cover both success and error scenarios.
- **CRITICAL: all tests must pass before starting next task** — no exceptions.
- **CRITICAL: update this plan file when scope changes during implementation.**
- Tests must redirect HOME/config paths to `t.TempDir()` and never touch real user configuration directories.
- Maintain backward compatibility for non-worktree mode: its branch behavior must not change.

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above).
- **E2E tests**: dashboard Playwright tests are unrelated to this change; no e2e updates expected. `make test` runs asset checks, race-enabled unit tests, and wrapper suites — all must pass.

## Progress Tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix.
- Document issues/blockers with ⚠️ prefix.
- Update plan if implementation deviates from original scope.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code changes, tests, documentation updates in this repo.
- **Post-Completion** (no checkboxes): manual smoke testing in a real repository.
- Checkboxes belong only in Task sections.

## Implementation Steps

### Task 1: Remove default-branch guard from worktree creation

- [x] in `pkg/git/service.go` `preparePlanBranch`: when `requireDefault=true`, stop comparing the current branch against the default branch (drop the `worktree creation requires %s branch` error); keep the `requireDefault=false` path byte-for-byte unchanged
- [x] add a replacement guard: when the current branch equals the plan-derived (or `--branch` override) branch name, return a clear error like `plan branch %q is already checked out here; switch to the base branch or run without --worktree` (git would otherwise refuse with a cryptic "already checked out" error)
- [x] keep dirty-tree checks and plan-file auto-commit detection exactly as they are
- [x] extend the creation log line in `CreateWorktreeForPlan` to name the source branch: `creating worktree with new branch: X (from ch_main)`; for detached HEAD log the short commit hash instead
- [x] update godoc comments on `CreateWorktreeForPlan` and `preparePlanBranch` (they currently document the main-branch requirement)
- [x] write tests: worktree created successfully from a non-default branch (feature branch parent commit is that branch's HEAD)
- [x] write tests: error when standing on the plan branch itself; creation from detached HEAD works; dirty tree still errors without auto-commit; existing-branch re-run behavior unchanged
- [x] verify existing `CreateBranchForPlan` (non-worktree) tests still pass unmodified
- [x] run `go test ./pkg/git/...` — must pass before task 2

### Task 2: Add auto-commit-all support to git service

- [x] add backend method in `pkg/git/external.go`: stage everything with `git add -A`, check `status --porcelain`, commit when non-empty (mirror `createInitialCommit`, but returning a "nothing to commit" signal instead of an error)
- [x] add `Service.AutoCommitAll(message string) (committed bool, err error)` in `pkg/git/service.go`: applies `appendTrailer` to the message, no-op returning `false` on a clean tree
- [x] write tests: dirty tree (modified + untracked files) gets committed on the current branch and the tree is clean afterwards; gitignored files are not committed; clean tree returns `committed=false` with no new commit
- [x] run `go test ./pkg/git/...` — must pass before task 3

### Task 3: Add -c/--commit CLI flag and wire it into worktree creation

- [x] in `cmd/loopai/main.go` opts: remove `short:"c"` from `CodexOnly` (keep long `--codex-only` working) and add `Commit bool` with `short:"c" long:"commit"` and a description like "auto-commit dirty working tree on the current branch before creating the worktree (requires --worktree)"
- [x] validate flag combinations: `--commit` without worktree mode fails fast with a clear error; place the check alongside the existing mode-flag validation
- [x] in the fresh-worktree creation path (before `CreateWorktreeForPlan` at ~main.go:1031): when `--commit` is set, call `AutoCommitAll` with message `auto-commit working tree before plan: <branch>`; log whether a commit was made; the resume path (`--resume-worktree`) must not auto-commit
- [x] confirm the plan-file flow: after auto-commit the plan file is already committed on the base branch, so `planNeedsCommit` comes back false and the copy-into-worktree path is skipped naturally (add a test asserting this)
- [x] write tests: flag parsing (`-c` sets Commit, `--codex-only` long form still works and no longer reacts to `-c`), `--commit` without `--worktree` rejected
- [x] write integration-style test: dirty tree + `--worktree --commit` creates the worktree with changes committed on the base branch
- [x] run `go test ./cmd/... ./pkg/git/...` — must pass before task 4

### Task 4: Simplify resolveBranchBase for worktree mode

- [x] in `cmd/loopai/main.go` `resolveBranchBase`: remove the two worktree-specific error paths (`--base-ref %q is not a branch; worktree creation needs a branch...` and the `checkout is on %q` mismatch error for worktree mode); in worktree mode `--base-ref` is now purely a diff base and branch creation always uses current HEAD
- [x] audit user-facing messages that promise the default-branch requirement for worktrees (grep for `requires` / `default branch` in `cmd/loopai`) and update or drop those that no longer apply; leave non-worktree messages alone
- [x] update `--base-ref` flag description to reflect that worktree creation no longer uses it as a branch base
- [x] update `resolveBranchBase` / `resolveBaseRefs` godoc comments
- [x] update existing `resolveBranchBase` tests: removed error cases now succeed; add cases asserting worktree mode returns a usable result regardless of `--base-ref` form
- [x] run `go test ./cmd/...` — must pass before task 5

### Task 5: Verify acceptance criteria

- [x] verify all requirements from Overview are implemented (worktree from any branch, plan-branch collision error, `-c/--commit` semantics, non-worktree mode untouched)
- [x] verify edge cases: detached HEAD, dirty tree without `--commit` still errors, clean tree with `--commit` is a no-op
- [x] run full test suite: `make test` — must pass
- [x] run `make lint` — all issues fixed
- [x] cross-compile check: `GOOS=windows GOARCH=amd64 go build ./...`
- [x] verify no test touched real `~/.config/loopai/` or `~/.config/ralphex/`

### Task 6: [Final] Update documentation

- [x] README.md: remove/replace statements that worktree requires the default branch; document cutting from the current branch and the new `-c/--commit` flag; note that review diffs and `--merge` still resolve against the default branch or `--base-ref`
- [x] llms.txt: same updates in compact form
- [x] embedded config comments in `pkg/config/defaults` if they mention the worktree default-branch requirement
- [x] CLAUDE.md architecture notes: update the worktree behavior description
- [x] move the design spec reference if docs structure requires it; keep `docs/superpowers/specs/2026-08-04-worktree-any-branch-design.md` linked from this plan

## Technical Details

- Guard removal is scoped by the existing `requireDefault` boolean: `true` (worktree) drops the branch comparison, `false` (branch mode) keeps the "skip when not on default" behavior — the two callers stay behaviorally independent.
- Plan-branch collision check compares the current branch against `EffectiveBranchName(planFile, branchOverride)` before any mutation.
- Auto-commit sequence: `git add -A` → `status --porcelain` → commit with trailer; runs in the main checkout before `git worktree add`, so `.loopai/worktrees/` does not exist yet and runtime artifacts are gitignored via `.loopai/.gitignore`.
- Flag note: go-flags treats defaults carefully in this repo (`isFlagSet`); `Commit` is a plain bool with no default, so no `markFlagsSet` entry is needed unless validation requires distinguishing "set" (it does not — false is inert).
- `--merge` back into a non-default base requires an explicit base argument (`loopai --merge <base>`); this is accepted and documented, not changed.

## Post-Completion

**Manual verification**:
- In a real repository on a non-default branch with local edits: `loopai --worktree -c <plan.md>` creates the worktree, commits the edits on the source branch, and the run proceeds; `tail -f .loopai/progress/...` shows normal phases.
- `loopai --merge <source-branch>` from the finished feature branch lands the work back on the source branch.

**External system updates**: none — no consuming projects, no config migrations.
