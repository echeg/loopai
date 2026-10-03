# Report: Finalize: sync with base, open a PR, optionally merge it

**Plan:** `docs/plans/20261003-finalize-sync-pr-merge.md` · **Branch:** `finalize-sync-pr-merge` · **Base:** `master` · **Mode:** full · **Executor:** claude · **Task model:** not recorded · **Review model:** not recorded · **Started:** 2026-10-03T08:10:25Z · **Finished:** 2026-10-03T09:25:08Z

## Summary
This branch replaces the old finalize step with a deterministic close-out. The old step was a best-effort, model-driven rebase that was off by default. The new one is selected by `finalize = none|sync|pr|merge` (default `none`) or `--finalize=<mode>`, and `--skip-finalize` still forces `none`.

- **`sync`:** fetches and merges `origin/<base>` into the plan branch in the run's own checkout. It uses a merge, never a rebase or force-push.
  - A clean merge is committed by Go, then a model session runs the plan's validation commands.
  - A conflicted merge goes to the review provider to resolve.
  - The result is accepted only if the `FINALIZE_DONE` signal is present, the tree is clean with no Git operation in progress, the checkout is still on the original branch, HEAD's parents are exactly `[pre-merge HEAD, fetched base]`, and `ChangedOutside` (the check that only conflicted paths were edited) reports nothing.
  - Anything else restores the pre-merge state and records `blocked`.
- **`pr`:** after a successful sync, pushes the branch and opens a PR through the code factored out of `--pr`, including T3 linking.
- **`merge`:** also waits on `gh pr checks --watch --fail-fast` (bounded by `finalize_checks_timeout`), then runs `gh pr merge --<method> --match-head-commit <pushed HEAD>` and confirms the state is `MERGED`.
- **Failures:** every finalize failure keeps the run green. It is reported as `finalize incomplete: <reason>` in the stats, in notifications, and in the cmux, Orca, and T3 final status.
- **Removed key:** `finalize_enabled` is now rejected at every config layer.
- **Other surfaces:** new protocol signals `FINALIZE_DONE` and `FINALIZE_BLOCKED`, and a `## Finalize` section in the run facts.

Run statistics: 1 task iteration with no failed retries; internal review ran once and ended with `review_done`; post-review ran 1 iteration; external review took 5 iterations. Every plan checkbox, Tasks 1–7, is ticked.

Phase durations (through finalize, before report assessment and archival):

| phase | duration_ms | ≈ |
|---|---:|---|
| tasks | 21470 | 21 s |
| internal review | 2081869 | 34 m 42 s |
| external review | 457002 | 7 m 37 s |
| evaluation | 1922614 | 32 m 03 s |
| other | 0 | 0 |

The phases add up to about 74 m 43 s, which matches the start-to-finish span. The task phase took only 21 s in one iteration, so the implementation tasks were evidently already complete when this run started. Almost all of the run's time went to review and evaluation.

## Change scope
- **Diff size:** 80 files, +6213 / −459 (2 added, 78 modified).
- **Commits:** 11.
  - `956e43c` saves the plan.
  - Seven feature commits, from `29d05d7` (finalize modes) through `fb7565e` (docs).
  - `5efd5ab` fixes code-review findings; `a46a6c1` fixes Codex review findings.
  - `8ca1c4a` (`fix(codex-imagegen): support UTF-8 output and proxy image profiles`) predates the plan commit and is unrelated to finalize. It accounts for the five `plugins/codex-imagegen/...` files (one of them the new `codex_image_settings_test.py`) and part of the `.claude-plugin/marketplace.json` change.
- **Main areas changed:**
  - **Config:** `pkg/config/{values,config}.go`, the defaults `config`, and the prompts `finalize.txt` (rewritten) and `report.txt`.
  - **Git:** `pkg/git/{service,external}.go` gained fetch, no-commit merge, stage-0 snapshot and `ChangedOutside`, merge abort and restore, `OperationInProgress`, `CommitMergeContext`, and `CommitParents`.
  - **Finalize phase:** `pkg/processor/phase/{finalize,phase,signals}.go`.
  - **Processor:** runner, prompts, prompt builder, run record and recorder, run facts, and completion report.
  - **Signals:** `pkg/status`, plus `knownSignals` in `pkg/executor`.
  - **CLI:** `cmd/loopai/main.go`, for flags, base and mode resolution, `createPullRequest`, `runFinalizeCloseout`, `archiveAfterFinalize`, and the stats output.
  - **Status surfaces:** `pkg/notify`, `pkg/cmux`, `pkg/orca`, `pkg/t3`, and `pkg/awake` (pin during the checks wait).
  - **Assets and docs:** skills (Claude and Codex: `loopai`, `loopai-merge`, `loopai-orca`, `loopai-t3`), `removed_spellings` in both checkers, manifests bumped to 0.5.11, and `README.md`, `llms.txt`, `CLAUDE.md`, `docs/{t3-code,custom-providers,notifications}.md`.
  - Each source change has matching test updates.

## Risk
**medium**

- **Public APIs / CLI:**
  - New `--finalize=<mode>` flag.
  - `finalize_enabled` is removed and fails loudly. That is a breaking change for anyone who set it, though the error names the replacement.
  - `--t3-launch` now rejects `--finalize` and `--skip-finalize`.
  - `--pr` was refactored onto the shared `createPullRequest`; the plan required its output and error wording to stay unchanged, and tests cover that.
  - Two new `<<<RALPHEX:...>>>` signals were added under the existing prefix.
  - New `notify.Result` fields (`PRURL`, `Finalize`) can change notification and webhook content.
- **Data schemas:** `RunRecord` (the `.run.json` sidecar) gains a finalize outcome, and the report facts gain a `## Finalize` section. No database is involved.
- **Configuration:** three new keys: `finalize`, `finalize_merge_method`, `finalize_checks_timeout`. The default `none` keeps existing runs unchanged unless a user opts in.
- **Concurrency:** the sync and the PR step run sequentially inside one run. The `merge` mode waits on `gh pr checks` under a timeout, with the keep-awake hold pinned.
  - The real risk is concurrent repository state rather than goroutines: pre-existing merges or rebases, a model session switching branches, or a PR head moving. The final review rounds hardened these paths: operation-in-progress is checked before the dirty-tree check, `Unrestored` suppresses archival, the branch-identity check runs before restore, and `--match-head-commit` refuses a moved PR head.
- **Migrations:** no data migration. Only the config spelling change described below.
- **Why medium and not low:**
  - When enabled, the feature merges into the user's branch, pushes, and can merge a PR on GitHub.
  - Restoring a rejected merge is subtle (`merge --abort` versus `reset --keep`), and the external reviewer found four separate state-preservation defects in it before the final round came back clean.
  - The deterministic run facts record no validation commands.
- **Why not high:** the feature is opt-in, defaults to `none`, and a finalize failure never turns the run red.

## Migrations and operational steps
- **Configuration:** remove `finalize_enabled` from any global (`~/.config/loopai/config`) or local (`.loopai/config`) config. Replace it with `finalize = sync|pr|merge`, or omit it to keep the default `none`. loopai now refuses to start while the old key is present.
- **Customized prompts:** a local or global copy of `prompts/finalize.txt` made from the old rebase prompt lacks `FINALIZE_DONE`. loopai warns once, and every sync will block. Refresh such copies (for example with `loopai-update`) or delete them to fall back to the embedded default.
- **`pr` / `merge` modes:** need an authenticated `gh` CLI and an `origin` whose push URLs match the GitHub repository `gh` uses.
- **Plugin users:** pick up the 0.5.11 skill update through the marketplace. Codex users rerun `make install-codex-skills`.

## Plan deviation
All checkboxes in Tasks 1–7 are complete. No items were blocked or skipped. Each added (➕) item:

1. **Test `HOME` redirects paired with `USERPROFILE`:** done. It fixes `TestReset_EmptyConfigDirFallback` resetting the real `~/.config/loopai` on Windows.
2. **Pre-existing Linux test failures and lint findings:** done (`TestRunWithWorktreeAutoResume` skip, `pkg/t3` `TestRunName` path).
3. **`{{FINALIZE_BASE}}` instead of `{{DEFAULT_BRANCH}}`:** done for the finalize prompt and `report.txt`, through `Config.FinalizeBase`, with a once-per-run warning when `FINALIZE_DONE` is missing. This departs from Task 3's original wording, which named `origin/{{DEFAULT_BRANCH}}`.
4. **`FinalizePrompt(conflicts []string)`, `ParseFinalizeBlockedReason`, and `FINALIZE_BLOCKED` before `FINALIZE_DONE` in `detectSignal`:** done.
5. **Git helpers and wiring:** done. `OperationInProgress`, `CommitMergeContext`, and `CommitParents` added; the `phase.FinalizeGit` consumer interface is wired through `Runner.SetFinalizeGit`.
6. **Base resolution and chain flag:** done. `resolveFinalize` and `finalizeBaseBranch` resolve the base, including the remote-only `origin/<branch>` case; `executePlanRequest.ChainNotLast` marks chain members.
7. **Cancellation restore:** done. The pre-merge state is restored under a detached one-minute context, and a failed restore is appended to the reason.
8. **`Runner.DiffBase()` and run facts:** done. After a successful sync, facts are measured against `origin/<base>`; stored finalize state is cleared at run start; the facts gain `## Finalize` and the fallback gains a `- finalize:` line.
9. **`createPullRequest(ctx, ghPath, gitSvc, branch, base, target)`:** done. This is a wider signature than Task 5 specified, and `closeoutTarget.statsBase` sets the base for the PR body's diff stats.
10. **`runFinalizeCloseout` / `finalizeResult`:** done. A PR opens only after a successful sync; `finalizeModeFor` handles degradation and `finalizeStartupWarning` reports it.
11. **`merge` mode details:** done. `--fail-fast`, a 1-minute grace for "no checks reported", `--match-head-commit`, and no branch deletion.
12. **`SetFinishNote` on cmux, Orca, and T3:** done, and the summary's diff stats now use `DiffBase()`.
13. **Code-review fixes:** done. Refuses a checkout on the base branch; checks branch identity in verify and restore; `BLOCKED` anywhere wins; leftover edits are kept and disclosed; `AbortCleanMergeContext` handles a failed snapshot; a non-ancestor merge without `MERGE_HEAD` is an error; keep-awake is renewed during the checks wait.
14. **Worktree path dropping `ChainNotLast`:** fixed through `worktreeExecuteRequest`.
15. **`loopai-orca` skills and `docs/notifications.md`:** updated.
16. **Manifests:** bumped to 0.5.11. Also adds the `/loopai-merge` `gh pr list --head` check. The asset suites pass in WSL.

Other differences between the plan and the code:
- **External-review fixes not recorded in the plan:** they came after the plan's last update (commit `a46a6c1`).
  - `--t3-launch` rejects `--finalize` and `--skip-finalize`.
  - A failed merge inside Git now goes through `rejectFailedMerge` with restore verification, and a canceled merge goes through the same restore path.
  - Finalize checks for a Git operation already in progress before the dirty-tree check, leaves it untouched, and marks the outcome `Unrestored` so the plan is not archived.
  - The archive-skip message names the outcome's reason.
- **Out-of-plan commit:** `8ca1c4a` (the codex-imagegen UTF-8 and proxy-profile fix) is in the `master...HEAD` range but falls outside the plan's scope.
- **Task 6 caveat:** the plan records `make test` as run component by component, with a ⚠️ that the Go suite is POSIX-only and was run in full only under WSL.

## Backlog
No backlog entries were filed.

## External review
### codex:gpt-6-astra:high
- label: codex
- iterations: 5
- duration_ms: 2379392
- ended by: done
- had findings: true

- **Iteration 1:** [P2] T3 launcher silently drops finalize overrides (`t3LaunchArgs` forwards neither `--finalize` nor `--skip-finalize`) -> fixed. `validateT3LaunchFlags` now rejects both flags, with tests and a `docs/t3-code.md` update.
- **Iteration 1:** [P2] Failed merge cleanup bypasses the archive guard (`blocked` with `Unrestored=false` while the merge may still be in place) -> fixed. A new `rejectFailedMerge` restores and verifies the checkout, marks `Unrestored` on failure, and routes cancellation through `cancel`. Covered by `TestFinalizePhase_FailedMergeCleanupIsVerified`.
- **Iteration 2:** [P2] Cleanup can abort a pre-existing merge (`rejectFailedMerge` → `AbortCleanMergeContext` on a user's own merge that passes `IsDirty`) -> fixed. An `OperationInProgress` preflight runs before the fetch, with the regression case "pre-existing merge with a clean tree stays intact".
- **Iteration 3:** [P2] Pre-existing operations still allow archival (preflight `blocked` left `Unrestored=false`) -> fixed. The preflight now sets `Unrestored`, `archiveAfterFinalize` reports the outcome's reason, and a new archival subtest checks that the plan, HEAD, `MERGE_HEAD`, and the index are unchanged.
- **Iteration 4:** [P2] Dirty pending operations bypass the archive guard (`OperationInProgress` ran after the dirty-tree early return) -> fixed. The operation check now runs before the dirty check, with the regression case "pre-existing merge with staged changes stays intact" and an `Unrestored=false` assertion for a plain dirty tree.
- **Iteration 5:** no issues found. The fixes were committed as `a46a6c1`.

## Validation
The supplied facts list no validation commands: 0 runs, duration_ms 0, so no validation timings were recorded.

The evaluator responses mention test and lint runs that are not supplied run facts:
- `go test` on `cmd/loopai`, `pkg/processor/...`, and `pkg/processor/phase`
- `golangci-lint` reporting 0 issues
- a full `go test -race ./...` in WSL in iteration 2

They also said the full `make test` and `make lint` were not run in the final rounds, and in iteration 4 they ran tests natively on Windows because WSL was unavailable.