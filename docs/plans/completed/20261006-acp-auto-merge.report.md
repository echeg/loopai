# Report: ACP Auto-Merge and Follow-Up Guidance

plan: `docs/plans/20261006-acp-auto-merge.md` · branch: `acp-auto-merge` · base: `master` · mode: full · executor: claude · task model: `claude:opus:high` · review model: `claude:opus:high` · started: 2026-10-06T18:12:54Z · finished: 2026-10-06T19:51:46Z

## Summary
When T3 Code hosts loopai through ACP (`loopai --acp`), a successful run now merges itself into the local base branch when its report rates Risk `low` or `medium`. The merge happens in the worktree where the base is already checked out. Nothing is pushed, and neither the plan branch nor the T3 Code worktree is removed.

- **New setting:** the `acp_auto_merge` key, default `true`, turns this on or off.
- **When the merge is skipped:** if finalize is `pr` or `merge`, if a finalize sync stopped short, if Risk is high or not stated, or if any repository check fails. In every case the final thread message gets a `## Merge` section that gives the reason, and the repository is left unchanged.
- **Follow-up guidance:** after a failed run or a skipped merge, the message adds a `## Next steps` block pointing follow-up work to a new session.
- **Chat messages:** a thread message that is not a plan launch now gets a normal `end_turn` answer with guidance instead of failing the turn. The original example was a question in Russian that failed with `malformed prompt: unexpected argument "по"`.

Run shape:
- 9 task iterations with 0 failed retries.
- Internal review ran its first pass plus 1 loop iteration and ended by `review_done`.
- 2 post-review iterations.

Phase durations (wall clock about 1 h 39 min):

| phase | duration |
|---|---:|
| tasks | 3,184,419 ms (≈53 m 04 s) |
| internal review | 1,566,395 ms (≈26 m 06 s) |
| evaluation | 1,050,161 ms (≈17 m 30 s) |
| external review | 131,256 ms (≈2 m 11 s) |
| other | 0 ms |

Finalize did not run, so there was no base sync and no changes from `origin/master` are mixed into this work. The timing covers work up to the finalize slot, before the report and archival.

## Change scope
25 files changed, with 1,288 additions and 67 deletions across 10 commits.

**Code:**
- **`cmd/loopai/acp_merge.go` (new):**
  - `acpAutoMerge` does the merge.
  - `acpMergePolicySkip` checks the config, finalize, and Risk gates.
  - `acpCompletedTip` checks that the branch still points at the recorded completed tip.
  - `acpBaseWorktree` and `acpCleanWorktreeSkip` check the worktrees; the second looks for a Git operation in progress, uncommitted changes, and untracked files, but allows `.loopai/` overrides.
  - The base checks and the merge run under the shared repository lock, with a 2-minute wait limit.
  - `acpMergeResult.summary()` and `message()` render the outcome.
- **`cmd/loopai/acp.go`:**
  - `acpLooksLikeLaunch` with the `acpNotALaunch` reply.
  - `acpRunner.autoMerge`, which is skipped when the run was canceled.
  - `acpRunResult` now builds the message as report, then `## Merge`, then `## Next steps`.
- **`cmd/loopai/main.go`:**
  - `planExecutionOutcome.finalizeIncomplete` is filled from `finalizeResult.incomplete`.
  - `mergeForCloseout` was factored into `mergeIntoBase`; `--merge` keeps the ref-based merge.
  - `--t3-launch` agent mode prints a different close-out hint.
- **`cmd/loopai/pr_body.go`:** `reportRiskLevel` reads the first line of the report's Risk section and handles code fences.
- **`pkg/git`:**
  - `Service.MergeBranchTipContext` merges by commit hash and keeps the `Merge branch '<feature>'` message.
  - `Service.UntrackedFiles` is new, along with the matching `externalBackend` methods.
  - The lock's doc comment now mentions the ACP merge.
- **`pkg/config`:**
  - `ACPAutoMerge` / `ACPAutoMergeSet` added to `Config` and `Values`, read and merged with the other finalize keys.
  - The key is documented in the embedded defaults.

**Tests:** `acp_merge_test.go` (new), plus additions to `acp_test.go`, `main_test.go`, `pr_body_test.go`, `config_test.go`, `values_test.go`, and `pkg/git/service_test.go`.

**Docs and skills:**
- `docs/t3-code.md`, `README.md`, `llms.txt`, and `CLAUDE.md` are updated.
- The `loopai-t3` close-out text is updated in both the Claude and Codex skills.
- Both plugin manifests move from 0.7.0 to 0.7.1.
- The plan file has its checkboxes ticked and Task 6 notes added.

**Commits, oldest first:**
- `f6af541`, `1804e44`, `484e0d4`, `8460204`, `2dd8b38`, `cd202e8`, `a0b06fe`: Tasks 1–7
- `6a7bc52`: internal review fixes
- `ee2db1b`: Codex review fixes
- `f684f66`: post-review fixes

## Evidence
- **Before:** the plan records that a follow-up question typed into an ACP thread was parsed as a launch and failed the turn with `malformed prompt: unexpected argument "по"`. **After:** the diff adds a test in `TestServeACPAnswersNonLaunchMessages` where `а по итогу оно замержено?` gets an `end_turn` answer with the guidance message, while `docs/plans/two.md --worktree` still fails with `unsupported option`.
- **Before:** a successful ACP run ended with the report only, and merging took a second session. The plan also records that on 2026-10-06 a `loopai --merge` from the primary checkout failed with `Permission denied` while deleting a live session's worktree. **After:** new `TestACPAutoMerge*` tests run against temporary repositories with a linked worktree. They check that the base advances on low and medium Risk, that the branch and worktree survive, and that every skip gate leaves the base unchanged.
- **Codex P2 finding (merge used the moving branch ref):** the new `TestService_MergeBranchTipContext` checks that a commit added after the recorded tip is left out of the merge, both for a fast-forward and for a merge commit. In iteration 1 the evaluator reported that this test and the `TestACPAutoMerge*` tests pass.
- The facts record 4 runs of the validation commands, taking 36,265 ms in total. They do not say which commit each run covered or what each run returned.

## Risk
**medium**

The change turns on, by default, a new automatic write to the user's own base-branch checkout. That write is guarded by several gates, local only, and limited to ACP mode.

- **Public APIs:** nothing is exported outside the module. `git.Service` gains `MergeBranchTipContext` and `UntrackedFiles`, but both are internal to loopai. The `--merge` path is unchanged: it still merges `refs/heads/<feature>` through `mergeIntoBase`.
- **Data schemas:** none. The run record and progress formats are untouched.
- **Configuration:** one new boolean key, `acp_auto_merge`, which defaults to `true`. Every existing ACP user starts getting automatic local merges after updating, with no action on their part. This is the main source of risk. An invalid value now fails config loading.
- **Concurrency:** the main new hazard is two ACP processes merging into the same base checkout at once. The Codex review caught it, and it was fixed with the shared repository lock (`AcquireWorktreeCreationLockContext`) held through the checks, the merge, and verification. The merge is pinned to the commit the run recorded when it finished, and a branch that moved since then is refused.
- **Migrations:** none.
- **Platform:** the risk is lower in practice because the merge only ever runs where the base is already checked out, never switches a checkout, and never deletes a branch or worktree. That avoids the Windows `Permission denied` failure above.
- **Remaining risk:** Risk is parsed from free text the model writes. Anything other than `low` or `medium` as the first word skips the merge, so a parse error can only fail safe toward not merging.

## Merge danger
**Door:** two-way
Reverting the change restores the old behavior, where the ACP run returns the report only. The new config key would no longer be read, and no data format changes. Merges the feature already made into users' local base branches stay, but they are ordinary local commits.

**Blast radius:** ACP
Only `loopai --acp` sessions (T3 Code agent mode) are affected. CLI, `--t3` terminal mode, Orca, and `--merge` behave as before, apart from the `--t3-launch` agent-mode close-out hint text.

## Migrations and operational steps
none

Turning the feature off is optional: set `acp_auto_merge = false` in `~/.config/loopai/config` or `.loopai/config`. The plan's post-completion manual check is still to be done: rebuild with `make build`, run a small plan in a T3 Code agent-mode thread, confirm the `## Merge` section and the base advancing, and send a question to confirm the guidance reply.

## Plan deviation
The facts record no added, blocked, or skipped items, and all seven tasks are checked off. The implementation does differ from the plan's original design in these ways, mostly because of the review rounds:

- **Merge pinning:** the plan said to pin the head through `BranchHash` and use `mergeForCloseout`/`MergeBranchCommitContext`. The final code instead merges `planExecutionOutcome.branchTip` by hash through the new `MergeBranchTipContext` (via `mergeIntoBase`), and refuses a branch that moved after the run (Codex P2). The plan's Technical Details still describe the old approach and are now out of date.
- **Repository lock:** not in the plan. It was added around the base checks and the merge (Codex P1).
- **Extra policy gate:** a stopped finalize sync (`finalizeIncomplete`) now skips the merge. `acpAutoMerge` takes the whole `planExecutionOutcome` instead of the report string the plan specified.
- **Clean-tree checks:** the plan specified `IsDirtyAll`. The code checks for a Git operation in progress first, then uncommitted changes, then untracked files, and allows `.loopai/config`, `prompts/`, and `agents/`, which `--t3-launch` copies into the worktree uncommitted.
- **Launch detection:** `acpLooksLikeLaunch` runs before parsing, not only after a parse failure, so a one-word reply that would otherwise read as a missing plan path gets guidance. A directory never counts as a launch.
- **Out-of-plan edits:**
  - the `--t3-launch` agent-mode close-out hint in `main.go`;
  - the `loopai-t3` skill close-out text, in both the Claude and Codex copies;
  - both manifest versions bumped to 0.7.1, as required whenever a skill changes.
- **Task 6:** records that `make test` passed in a Linux container started with `docker run --init`, and that the new functions have about 90% coverage.

## Backlog
None. No backlog entries were filed.

## External review
### codex:gpt-6-astra:high
- label: codex
- iterations: 2
- duration_ms: 1181307
- ended by: done
- had findings: true

Iteration 1:
- [P1] Separate T3 ACP processes could merge into the shared base worktree at the same time. A merge failing on Git's index lock could `reset --hard` away the other process's successful merge. -> fixed (`ee2db1b`: `acpAutoMerge` holds `AcquireWorktreeCreationLockContext` through the base checks, merge, and verification, waiting at most `acpMergeLockTimeout`)
- [P2] The merge used the moving `refs/heads/<feature>` ref instead of the completed commit, so commits added after the report could reach the base. -> fixed (`ee2db1b`: `acpCompletedTip` requires the branch to equal `outcome.branchTip`, and `MergeBranchTipContext` merges that hash)

Iteration 2: NO ISSUES FOUND. The reviewer confirmed both fixes.

## Validation
- `make test`
- `make lint`

Timing: 4 runs, 36,265 ms in total.

The evaluator could not run the full `make test` during external review because WSL failed to start (`Wsl/Service/E_UNEXPECTED`). Instead it ran the affected packages on Windows with `HOME` and `USERPROFILE` redirected, plus golangci-lint (0 issues), gofmt, `go build`, and `go vet`.

On Windows, these tests fail identically on the base commit because they need POSIX shell stubs:
- `TestService_MergeBranch` (2 subtests; one fails only on `\r\n` line endings)
- `TestExternalBackendMergeWouldConflict*`
- `TestRunMergeCommandExplicitFeature`

Task 6 records that `make test` passed in a Linux container before the review-fix commits. The facts do not show a full POSIX `make test` run on the final commit `f684f66`.