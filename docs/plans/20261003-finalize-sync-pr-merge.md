# Finalize: sync with base, open a PR, optionally merge it

## Overview
- Replace the current best-effort finalize step (a model-driven rebase onto the default branch, off by default and unused) with a deterministic close-out of a finished run:
  1. merge the up-to-date remote base into the plan branch inside the run's own checkout;
  2. let the review provider resolve conflicts only when they are clear-cut, verified by deterministic checks and the plan's validation commands;
  3. push the branch and open a pull request through the existing `--pr` logic (including T3 Code thread linking);
  4. optionally wait for the PR checks and merge the PR on GitHub.
- Problem it solves: after a multi-hour task/review run the branch is behind `origin/<base>`, and bringing it current, opening the PR, and merging it is manual work that only needs a human when the merge actually requires a decision.
- Integration: the sync/validation part stays a processor phase (`phase.FinalizePhase`, same slot as today, before the report, so the report sees the merge). Push/PR/merge runs in `cmd/loopai` after plan archival and before worktree removal, reusing the `--pr` implementation.

## Decisions
- **Context**: the user does not use the current finalize and wants it replaced, not supplemented; merging is delegated to the agent, so syncing happens locally in the run's checkout, then a PR is created, then it is merged when a flag asks for it.
- **Chosen approach**:
  - config key `finalize = none | sync | pr | merge` (default `none`) and CLI `--finalize=<mode>`; `sync` = merge base + validate only, `pr` = sync + PR, `merge` = pr + merge the PR. `--skip-finalize` stays and forces `none`.
  - `finalize_merge_method = merge | squash | rebase` (default `merge`) and `finalize_checks_timeout` (Go duration, default `30m`).
  - sync always uses `git fetch origin <base>` + `git merge origin/<base>` into the plan branch, never rebase, never force-push: history and review-checkpoint ancestry stay valid and a rerun is safe.
  - a clean merge is committed by Go; a model session then runs the plan's validation commands. A conflicted merge is left in place for the review provider, which resolves, validates, and commits.
  - acceptance requires all of: the model's success signal, the deterministic "only conflicted paths changed" check, and validation passing. Anything else restores the pre-merge HEAD and stops before the PR.
  - validation failing after a clean merge stops; finalize never tries to fix code broken by base changes.
  - `merge` mode waits for checks with `gh pr checks --watch` bounded by `finalize_checks_timeout`, then `gh pr merge --<method>`; it never touches the local base branch.
  - a finalize stop leaves the run green (the plan's work succeeded) and is reported like `plan archive incomplete`: last line of the summary, in the notification, and in the cmux/Orca/T3 final status text.
  - loopai's own `--worktree` is removed as today; a T3 Code–managed worktree is never removed.
- **Rejected alternatives**:
  - keeping the old rebase finalize alongside: unused, and rebase conflicts with checkpoint ancestry and requires force-push.
  - local `--merge` into the base branch as the merge step: writes into the user's primary checkout and removes T3-managed worktrees; GitHub merge keeps CI as the final gate.
  - `gh pr merge --auto` without waiting: merges instantly without branch protection, and loopai could not report the real outcome.
  - a new name such as `closeout`: already the internal name of the standalone `--merge`/`--pr`/`--report` commands in `cmd/loopai`.
- **Verified facts**:
  - finalize currently runs in `Runner.runExternalAndPostReview` (`pkg/processor/runner.go:448-511`) before `runReport` and `clearReviewCheckpoint` on all three exit paths, through the review executor; `--tasks-only` never reaches it.
  - `cmd/loopai` `executePlan` archives the plan (`moveCompletedPlan`, `main.go:1704`) after `Runner.Run` returns, then notifies, prints stats, and only then removes the worktree in `BeforeCmuxFinish`; a single-plan worktree run archives through `MainGitSvc` in the source checkout, a chain on the plan branch.
  - `runPRCommand` (`main.go:5502`) prints the PR URL but returns only an error; `linkT3PullRequest` (`main.go:5583`) links the PR when `cfg.T3` is set.
  - `pkg/git` has no fetch helper, no public merge abort, and its merge path (`mergeRevision`, `external.go:504`) always aborts on conflict and accepts only `refs/heads/` revisions.
  - new protocol signals must be added both in `pkg/status/status.go` and to `knownSignals` in `pkg/executor/executor.go:870`, otherwise they are never detected.
  - `pkg/notify.Result` has fixed fields and `Send` gates on status `success`/`failure` only.
  - plan validation commands are parsed into `plan.Plan.ValidationCommands` (`pkg/plan/parse.go:43`); no prompt variable exposes them yet.

## Context (from discovery)
- Files/components involved:
  - config: `pkg/config/values.go`, `pkg/config/config.go`, `pkg/config/prompts.go`, `pkg/config/defaults/config`, `pkg/config/defaults/prompts/finalize.txt`, `pkg/config/defaults/prompts/report.txt`
  - processor: `pkg/processor/runner.go`, `pkg/processor/prompt_builder.go`, `pkg/processor/prompts.go`, `pkg/processor/run_recorder.go`, `pkg/processor/phase/finalize.go`, `pkg/processor/phase/phase.go`, `pkg/processor/phase/signals.go`
  - signals: `pkg/status/status.go`, `pkg/executor/executor.go`
  - git: `pkg/git/service.go`, `pkg/git/external.go`
  - CLI: `cmd/loopai/main.go` (opts, `rejectRemovedFlags`, `applyCLIOverrides`, `createRunner`, `executePlan`, `moveCompletedPlan`, `displayStats`, `runPRCommand`, `linkT3PullRequest`, `sendNotification`, `buildNotifyResult`)
  - status surfaces: `pkg/notify/notify.go`, `pkg/cmux/cmux.go`, `pkg/orca/orca.go`, `pkg/t3/reporter.go`
  - assets/docs: `scripts/check-symlinks.sh`, `scripts/check-codex-skills.sh` (`removed_spellings`), `assets/claude/skills/{loopai,loopai-merge,loopai-t3}/SKILL.md`, `assets/codex/skills/{loopai,loopai-merge,loopai-t3}/SKILL.md`, `.claude-plugin/plugin.json`, `.claude-plugin/marketplace.json`, `README.md`, `llms.txt`, `CLAUDE.md`, `docs/t3-code.md`, `docs/custom-providers.md`
- Related patterns found:
  - removed config keys: `removedKeys` + `checkRemovedKeys` (`pkg/config/values.go:894-903`)
  - non-fatal outcome reporting: `moveCompletedPlan` returning `incomplete`, `displayStats` printing it last
  - PR tests: local bare remotes, Git push stubs, `PATH`-injected `gh` stubs (existing `--pr` tests in `cmd/loopai/main_test.go`)
  - phase engines with retry/limit policy in `pkg/processor/phase`
- Dependencies identified: `gh` CLI (only for `pr`/`merge`), Git 2.38+ not required (conflict detection uses a real `git merge --no-commit`).

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
- Tests must redirect HOME/config paths to `t.TempDir()` and never touch real `~/.config/loopai/` or `~/.config/ralphex/`
- Keep the module path and `<<<RALPHEX:...>>>` signal prefix unchanged
- Do not edit `CHANGELOG.md`

## Testing Strategy
- **Unit tests**: required for every task; table-driven with `testify`, one `_test.go` per source file
- Git behavior is tested against real temporary repositories with a local bare `origin` (clean merge, conflicting merge, unrelated-path tampering, already up to date)
- `gh` is stubbed through `PATH` injection; no test reaches GitHub or a model
- **E2E tests**: dashboard Playwright tests are unaffected unless the finalize section label changes; run `make e2e` only if web code is touched

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code changes, tests, documentation updates in this repository
- **Post-Completion** (no checkboxes): manual smoke tests against a real GitHub repository and T3 Code

## Implementation Steps

### Task 1: Replace the finalize configuration surface
- [x] in `pkg/config/values.go` and `pkg/config/config.go` replace `FinalizeEnabled`/`FinalizeEnabledSet` with `Finalize` (`none|sync|pr|merge`), `FinalizeMergeMethod` (`merge|squash|rebase`), `FinalizeChecksTimeout` (duration), each with its `...Set` merge handling and value validation errors naming the key
- [x] add `finalize_enabled` to `removedKeys` with replacement text pointing at `finalize = sync|pr|merge`
- [x] add `--finalize=<mode>` to `opts` in `cmd/loopai/main.go`, keep `--skip-finalize` forcing `none`, update the execution-flag lists (`main.go:205`, `:3675`), `applyCLIOverrides`, and `createRunner`
- [x] update `pkg/config/defaults/config` comments for the three keys and the removed one
- [x] add `finalize_enabled` to both `removed_spellings` lists (`scripts/check-symlinks.sh`, `scripts/check-codex-skills.sh`), keeping them identical
- [x] write tests: parsing and merging of the new keys across layers, invalid values, `finalize_enabled` rejected at every layer, `--finalize` and `--skip-finalize` precedence
- [x] run `go test ./pkg/config/... ./cmd/loopai/...` - must pass before task 2
- ➕ [x] pair every test `HOME` redirect with `USERPROFILE`: on Windows `os.UserHomeDir` reads only `USERPROFILE`, so `TestReset_EmptyConfigDirFallback` reset the real `~/.config/loopai`
- ➕ [x] fix pre-existing Linux test failures (case-sensitive skip in `TestRunWithWorktreeAutoResume`, platform-native path in `pkg/t3` `TestRunName`) and pre-existing lint findings

### Task 2: Git helpers for syncing with the remote base
- [x] add `Service.FetchContext(ctx, remote, ref)` honoring `vcs_command` (returns the fetched SHA; explicit `+refs/heads/<ref>:refs/remotes/<remote>/<ref>` refspec)
- [x] add `Service.MergeRemoteNoCommitContext(ctx, rev)` that runs `git merge --no-commit --no-ff` (branch merge options neutralized like `mergeRevision`), does NOT abort on conflict, and returns the merge state: already up to date, clean, or conflicted with the list of unmerged paths
- [x] add `Service.StageZeroSnapshot()` recording blob ids of all non-conflicted index entries right after the merge, and `Service.ChangedOutside(snapshot, allowed []string, commit)` reporting paths whose blob in `commit` differs from the snapshot outside the allowed set
- [x] add `Service.MergeAbortContext` and `Service.RestoreHeadContext(ctx, sha)` (`git reset --keep`) that refuse to discard changes not produced by the merge (`MergeAbortContext(ctx, snapshot)` takes the post-merge `MergeSnapshot`, because `git merge --abort` silently drops staged changes whose worktree file matches the index; `RestoreHeadContext` also requires `sha` to be an ancestor of HEAD and no operation in progress)
- [x] write tests with a temporary repo and bare origin: fetch, up to date, clean merge, conflicted merge with unmerged path list, snapshot/ChangedOutside detecting a tampered unrelated file, abort and restore
- [x] write tests for error cases: missing remote, unknown ref, restore refusing a dirty unrelated path
- [x] run `go test ./pkg/git/...` - must pass before task 3

### Task 3: Finalize signals and prompt
- [x] add `FinalizeDone = "<<<RALPHEX:FINALIZE_DONE>>>"` and `FinalizeBlocked = "<<<RALPHEX:FINALIZE_BLOCKED>>>"` to `pkg/status/status.go`, `knownSignals` in `pkg/executor/executor.go`, and helpers in `pkg/processor/phase/signals.go`
- [x] rewrite `pkg/config/defaults/prompts/finalize.txt`: Go has already merged `origin/{{DEFAULT_BRANCH}}`; if `{{FINALIZE_CONFLICTS}}` lists files, resolve only those (keep both sides' intent, no unrelated edits, no `git add -A`, stage the listed paths and commit the merge); run every command in `{{VALIDATION_COMMANDS}}`; emit `FINALIZE_DONE` only when all pass, otherwise `FINALIZE_BLOCKED` followed by a one-line reason; a conflict that needs a product or design decision is `FINALIZE_BLOCKED`
- [x] add `{{VALIDATION_COMMANDS}}` and `{{FINALIZE_CONFLICTS}}` expansion to the finalize prompt builder (`pkg/processor/prompt_builder.go`/`prompts.go`) and update the prompt header comment; update `report.txt`'s finalize mention
- [x] write tests: signal detection for both providers, prompt expansion with and without conflicts and with no validation commands
- [x] run `go test ./pkg/status/... ./pkg/executor/... ./pkg/processor/...` - must pass before task 4
- ➕ [x] `phase.FinalizePrompts.FinalizePrompt(conflicts []string)` now takes the conflicted paths (the current phase passes nil until task 4); `phase.ParseFinalizeBlockedReason` extracts the bounded one-line reason after `FINALIZE_BLOCKED`; `detectSignal` checks `FINALIZE_BLOCKED` before `FINALIZE_DONE` so output carrying both reads as blocked

### Task 4: Rewrite the finalize phase as base sync
- [x] rewrite `pkg/processor/phase/finalize.go`: when `Finalize != none`, require a clean tree, record pre-merge HEAD, fetch and merge `origin/<base>`; up to date → validation session only; clean → commit the merge with a fixed message, then validation session; conflicted → snapshot, then resolution session
- [x] accept only when the session signals `FINALIZE_DONE`, the tree is clean, HEAD is a merge commit whose second parent is the fetched base (for conflict and clean cases), and `ChangedOutside` reports nothing; otherwise abort/restore to pre-merge HEAD and record a `FinalizeOutcome{Status: blocked, Reason, Files}`
- [x] return a `FinalizeOutcome` (`skipped|up_to_date|merged|resolved|blocked`, reason, conflicted files, base SHA) through the runner; expose it as `Runner.FinalizeOutcome()`; context cancellation still propagates as an error, every other failure becomes `blocked`
- [x] in a plan chain run finalize only for the last plan (pass a processor config flag from `runPlanChain`)
- [x] record the outcome in `RunRecord` so the report's facts mention the sync result
- [x] write tests with fake executors and a real temp repo: each outcome, signal missing, tampering rejected, validation failure after clean merge restoring HEAD, cancellation, chain non-last plan skipping
- [x] run `go test ./pkg/processor/...` - must pass before task 5
- ➕ [x] `pkg/git` gained `OperationInProgress`, `CommitMergeContext` (commits the in-progress merge with the commit trailer, refuses unresolved paths), and `CommitParents`; the phase reaches git through the consumer interface `phase.FinalizeGit`, wired by `Runner.SetFinalizeGit(req.GitSvc)` (the worktree service in worktree mode)
- ➕ [x] the base is `resolveFinalize`/`finalizeBaseBranch` in `cmd/loopai`: the local branch `--base-ref` names, else the configured or auto-detected default branch, without `origin/`; the chain flag is `executePlanRequest.ChainNotLast`
- ➕ [x] cancellation restores the pre-merge state under a detached one-minute context before returning the error, so a resumed run never meets a half-done merge; a failed restore is appended to the blocked reason
- ➕ [x] after a `merged`/`resolved` sync, report facts measure commits and diffs against `origin/<base>` (`Runner.factsBase`) so merged-in base changes are not counted as the run's work; a stored finalize result is cleared at run start; the facts gain a `## Finalize` section and the facts-only fallback a `- finalize:` summary line

### Task 5: Push, open the PR, and optionally merge after archival
- [x] refactor `runPRCommand` into a reusable `createPullRequest(ctx, gitSvc, base, target) (prURL string, err error)` used by both `--pr` and finalize; keep `--pr` output unchanged
- [x] in `executePlan` after `moveCompletedPlan` and before `BeforeCmuxFinish`, when `Finalize` is `pr|merge` and the outcome is not `blocked`: create the PR on the run's `GitSvc` (link to T3 when `cfg.T3`), then for `merge` run `gh pr checks --watch` under `finalize_checks_timeout` and `gh pr merge --<method>`
- [x] treat every failure (no `gh`, origin mismatch, push rejected, checks failed or timed out, merge refused) as non-fatal: collect it as `finalizeIncomplete` and keep the run green; never remove a worktree that loopai did not create
- [x] show the outcome in `displayStats` (PR URL, `merged`, or `finalize incomplete: <reason>` printed last), add `PRURL`/`Finalize` fields to `notify.Result` and `formatMessage`, and include the outcome in the cmux/Orca/T3 final status text
- [x] write tests with bare remotes and `PATH`-injected `gh` stubs: PR created, merge after green checks, checks failing, checks timeout, merge refused, blocked sync skipping the PR, T3 link invoked, `--pr` output unchanged
- [x] run `go test ./cmd/loopai/... ./pkg/notify/... ./pkg/cmux/... ./pkg/orca/... ./pkg/t3/...` - must pass before task 6
- ➕ [x] `createPullRequest(ctx, ghPath, gitSvc, branch, base, target)` takes the resolved `gh` path, branch, and base, so `--pr` keeps its own error wording and ordering; T3 linking stays with each caller; `closeoutTarget.statsBase` measures the finalize PR body against `origin/<base>`
- ➕ [x] the post-archival step is `runFinalizeCloseout` returning a `finalizeResult` (sync outcome, PR URL, merged, `incomplete`); a PR opens only after a successful sync (`up_to_date|merged|resolved`), so a `blocked` or `skipped` sync reports `finalize incomplete` instead; `finalizeModeFor` degrades `pr|merge` to `sync` under `--review`/`--external-only` and to `none` under `--tasks-only`, with a startup warning from `finalizeStartupWarning`
- ➕ [x] `merge` mode: `gh pr checks <url> --watch --fail-fast` bounded by `finalize_checks_timeout`; a fresh PR that reports "no checks reported" is retried for `finalizeNoChecksGrace` (1m) and then treated as having none; `gh pr merge <url> --<method> --match-head-commit <pushed HEAD>`, no branch deletion
- ➕ [x] cmux/Orca/T3 reporters gained `SetFinishNote` (`synced`, `PR opened`, `PR merged`, `finalize incomplete`) appended to the done status; the completion summary's diff stats use `Runner.DiffBase()` so they agree with the report after a base merge
- ➕ [x] fixed task 4's worktree path dropping `ChainNotLast` (every worktree chain member would have synced); the request rebuilt inside the worktree is now `worktreeExecuteRequest`

### Task 6: Verify acceptance criteria
- [x] verify all requirements from Overview are implemented, including `sync`, `pr`, `merge` modes and `--skip-finalize`
- [x] verify edge cases: already up to date, no validation commands, review-only modes (sync only, no PR), plan chain, `--worktree` and T3-managed worktree preservation
- [x] run `make test` (every component run separately: the Go suite on Linux in WSL passes in full; `-race` was run on Windows and reports no data race; the asset, manifest, grill, codex-skill, completion, imagegen, and wrapper suites pass. ⚠️ the Go suite is POSIX-only: on Windows it fails on shell-script stubs, chmod permission tests, and Git for Windows' default `core.autocrlf=true`, in pre-existing tests and the new ones alike, and the symlink regression suites need real symlinks)
- [x] run `make lint` - all issues must be fixed (0 issues, also with `GOOS=linux`)
- [x] run `GOOS=windows GOARCH=amd64 go build ./...` (linux/amd64 and darwin/arm64 build too)
- [x] verify test coverage of the new code is 80%+ (91.1% of the non-test Go statements this branch adds, 809/888; every file at or above 84.5%; overall 88.1%)

### Task 7: [Final] Update documentation and skills
- [x] update `README.md`, `llms.txt`, `docs/t3-code.md`, `docs/custom-providers.md`, and the finalize sections of `CLAUDE.md`
- [x] update `assets/claude/skills/{loopai,loopai-merge,loopai-t3}/SKILL.md` and the Codex counterparts under `assets/codex/skills/` to describe `finalize = pr|merge` and when `/loopai-merge` is still needed (finalize blocked)
- [x] bump both manifest versions in `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` to the same value
- [x] run `make check-symlinks check-codex-skills check-plugin`
- ➕ [x] manifests bumped to 0.5.11; `/loopai-merge` gained a read-only `gh pr list --head` check that stops on an already merged PR and drops `Open PR` for an open one; the asset regression suites (`check-symlinks_test.sh`, `check-codex-skills_test.sh`, `check-plugin_test.sh`) pass in WSL, since the symlink fixtures cannot be created in Git Bash on Windows

## Technical Details
- **Modes**: `none` (nothing), `sync` (fetch + merge base + validation), `pr` (sync + push + `gh pr create`), `merge` (pr + `gh pr checks --watch` + `gh pr merge --<method>`). Review-only modes (`--review`, `--external-only`) support `sync` only; `pr`/`merge` degrade to `sync` there with a startup warning.
- **Base**: the resolved default branch (`GetDefaultBranch`, stripped of `origin/`), or the explicit `--base-ref` when it names a local branch.
- **Acceptance check for conflict resolution**: after `git merge --no-commit` with conflicts, record stage-0 blob ids of every non-conflicted path; after the model's commit, every path outside the conflicted set must carry the recorded blob, and HEAD's parents must be the pre-merge HEAD and the fetched base SHA.
- **Restore**: `git merge --abort` while the merge is uncommitted, otherwise `git reset --keep <pre-merge HEAD>`; both refuse to discard anything not produced by the merge.
- **Order in `executePlan`**: `Runner.Run` (task, reviews, finalize sync, report) → archive plan → PR/merge → notification and stats → worktree cleanup.
- **Outcome surfacing**: `finalize: merged origin/master (3 files resolved)`, `PR: <url>`, `PR merged`, or `finalize incomplete: <reason>` as the last summary line.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- run a full plan with `--finalize=pr` against a real GitHub repository whose base moved during the run (clean merge and conflict cases)
- run `--finalize=merge` on a repository with required checks, both green and failing
- run through `/loopai:loopai-t3` and confirm the thread gets the PR linked and settles after the merge, and the T3 worktree is kept
