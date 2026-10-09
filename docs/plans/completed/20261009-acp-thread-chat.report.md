# Report: ACP thread chat
plan: `docs/plans/20261009-acp-thread-chat.md` · branch: `echeg/acp-thread-chat` · base: `master` · mode: full · executor: claude · task model: `claude:opus:high` · review model: `claude:opus:high` · started: 2026-10-09T10:25:42Z · finished: 2026-10-09T12:43:44Z

## Summary
A T3 Code agent-mode thread now stays usable after a loopai run. Before this change, any message that was not a plan launch got the fixed `acpNotALaunch` reply telling the user to open a new session. Now each ACP prompt is routed in one of three ways:

- **Launch:** goes to the plan run, which is unchanged.
- **Bare `merge`/`мерж`/`pr`/`пр`:** runs a loopai-owned action with no model call.
- **Anything else:** becomes a chat turn. The `review_model` provider answers it in the thread's worktree and can read, edit, and commit with a pathspec on the branch.

What was built:

- **Executor resume.** `ClaudeExecutor` appends `--resume <id>` after the configured args. `CodexExecutor` builds `codex exec <options> resume <id> -`. Both report `Result.SessionID`. Wrapper commands always start fresh.
- **Chat mode.** A new `processor.ModeACPChat` runs `phase.ChatPhase`, modeled on gen-agents. It uses the review executor's retry, limit, and claude-swap policy. If a resumed session fails, it retries once as a fresh session. A canceled turn saves no session id.
- **Prompt and signals.** A new embedded `acp_chat.txt` prompt is added. Two signals, `<<<RALPHEX:ACP_MERGE>>>` and `<<<RALPHEX:ACP_PR>>>`, are registered in `knownSignals` and stripped from the answer before it is shown.
- **Session state.** It is stored at `.loopai/acp/<session-id>.json`, written atomically with mode 0600. It holds the provider, the provider session id, the last run (plan, branch, base, tip, report, auto-merge outcome, launch model overrides), and pending action outcomes. The state survives a T3 restart through `session/load`.
- **Requested merge and PR.** `acpRequestedMerge` shares the repository gates and the lock with `acpAutoMerge` but skips the policy gates. It merges the branch's current head by hash and reports commits added after the last run. `acpRequestedPR` returns the URL of an existing PR or calls `createPullRequest` with the last run's report. Neither pushes on merge, deletes a branch, removes a worktree, or switches the checkout.

Phase durations (the finish time and timings cover work through finalize, before report assessment and archival):

| phase | duration_ms | ≈ |
|---|---:|---|
| tasks | 5063457 | 84m 23s |
| internal review | 2462044 | 41m 02s |
| evaluation | 553686 | 9m 14s |
| external review | 202054 | 3m 22s |
| other | 0 | 0s |

The run had 8 task iterations and 0 failed retries. Internal review ran its first pass plus 2 loop iterations and ended by `review_done`. Post-review ran 1 iteration.

## Change scope
51 files changed, +3501 / −198, in 11 commits:

- 7 feature commits: `19515ef` executor resume, `f243c0e` signals and prompt, `72ce41d` ModeACPChat/ChatPhase, `f75ebea` state file, `87cba55` merge/PR actions, `8f3fc2e` routing, `10cd69f` acceptance verification.
- 1 docs commit: `9a6bb45`.
- 3 review-fix commits: `36cd2f0` and `71e19ee` (internal code review), `a8126eb` (Codex review).

**New files**
- `cmd/loopai/acp_chat.go`, `cmd/loopai/acp_state.go`
- `pkg/processor/phase/chat.go`
- `pkg/config/defaults/prompts/acp_chat.txt`
- Their tests: `acp_chat_test.go`, `acp_state_test.go`, `chat_test.go`
- Backlog entry `docs/backlog/symlinked-checkout-path-containment.md`

**Modified Go files**
- ACP routing and merge: `cmd/loopai/acp.go`, `acp_merge.go`, `main.go`
- Executors: `pkg/executor/executor.go`, `codex.go`
- Processor: `pkg/processor/runner.go`, `prompt_builder.go`, `executor_factory.go`
- Config: `pkg/config/config.go`, `defaults.go`, `prompts.go`
- Signals: `pkg/status/status.go`
- Progress filename: `pkg/progress/progress.go`
- Runtime paths: `pkg/git/service.go` and `external.go`. The `external.go` change also includes an `unparam` lint refactor of `classifyNoCommitMerge`'s `abandon` closure.
- The matching `_test.go` files for all of the above.

**Docs and assets**
- `CLAUDE.md`, `README.md`, `llms.txt`, `docs/t3-code.md`
- The Claude and Codex `loopai-t3` skills
- Both plugin manifests, bumped 0.7.1 → 0.7.2
- `.gitignore`, which now ignores `.loopai/acp/`
- The plan file
- A pre-existing test fix in `plugins/codex-imagegen/.../codex_image_test.py`

## Risk
**medium**

- **Public behavior and APIs.** The ACP protocol surface is unchanged: the same methods and the same launch syntax. What changes is what a non-launch message does: it used to return fixed guidance and now runs a model that can edit and commit on the branch. `merge`/`pr`, typed or requested by the model, now mutate the local base branch or push and open a PR. The only guard against an unrequested model merge is the prompt. This was a deliberate decision: the merge is local, reversible, and still passes every repository gate, and it never deletes a worktree or branch.
- **Internal Go API.** `executor.Result` gained `SessionID` and `Reply`, and the executors gained a per-run resume context value. Resume is only applied when a session id is passed, so existing invocations are byte-identical. Claude strips any `--resume` already in `claude_args`. Codex moves every option before the `resume` subcommand because `resume` rejects `--sandbox`. This depends on the CLI shapes as they were verified, and the resume paths are new in both executors.
- **Data schema.** There is a new JSON state file under `.loopai/acp/`. A missing file, or a corrupt one (with a warning), loads as empty state, and unsafe session ids are rejected. The file is gitignored through `loopaiRuntimePaths`. An untracked `.loopai/.gitignore` that still holds the earlier generated content is rewritten in place. That is a small write into users' checkouts, limited to an exact match of the old content.
- **Configuration.** No new config keys. A new embedded prompt `acp_chat.txt` is added and falls back to the embedded default, so prompt directories copied by an older `--init` keep working.
- **Concurrency.** ACP still runs one prompt at a time. The requested merge runs under `AcquireWorktreeCreationLockContext` bounded by `acpMergeLockTimeout`, the same protection as the auto-merge. `OperationInProgress` is checked before the dirty and untracked gates.
- **Migrations.** None needed. The ignore rules and the gitignore upgrade apply automatically.
- **Outside the test suite.** The feature still depends on undocumented Grok-driver contracts in T3 Code. The plan's post-completion manual T3 checks are not covered by the automated suite.

## Migrations and operational steps
none

The plan's Post-Completion items are manual checks, not required actions:
- In a T3 thread, ask a question, request a fix, and run `merge`.
- Confirm resume works after a T3 Code restart.
- Re-check after the next T3 Code update.

## Plan deviation
All 8 tasks are complete. No items were blocked or skipped.

**Added items, as recorded in the plan:**
- ➕ **Pre-existing validation failures fixed:**
  - the macOS `/var` symlink problem in `codex_image_test.py` and the `cmd/loopai` fixtures, fixed by resolving `TMPDIR` in `TestMain`; the product-side issue was filed as `docs/backlog/symlinked-checkout-path-containment.md`
  - the `unparam` lint in `pkg/git/external.go`
- ➕ **`.loopai/acp` added to `loopaiRuntimePaths`.** Without it, the state file would count as an untracked path and `acpCleanWorktreeSkip` would skip every merge.
- ➕ **In-place upgrade of an untracked `.loopai/.gitignore`** that still holds the earlier generated content (`progress/`, `worktrees/`), so existing checkouts pick up `acp/`. `--init` writes the new content.
- ➕ **Signatures as built.**
  - `acpRequestedMerge(ctx, gitSvc, base, lastRun)` takes no config, because it applies no policy gate. The plan specified a `cfg` parameter.
  - `acpRequestedPR(ctx, gitSvc, cfg, base, lastRun)` takes config for `plans_dir`.
  - `acpActionBase` resolves the base.
  - A branch the base already contains returns `acpMergeResult.upToDate`.
  - The auto-merge and the requested merge share `acpFeatureBranch` and `acpMergeIntoBase`.
- ➕ **Routing as built.**
  - `acpRoute` returns launch, `merge`, `pr`, or chat, and `run` dispatches to `runPlan`, `runCommand`, or `runChat`.
  - `openSession` is shared by commands and chat.
  - `ModeACPChat` joins the review-provider branches of `modePhaseProviders`, `modePhaseBanners`, and `runHeaderParams`, skips reviewer resolution, and logs to `progress-acp-chat.txt`.
  - The sink needed no ordering change: `Finish` flushes reasoning before `Message`.
- ➕ **README.** The provider-mode paragraph and the keep-awake note now describe thread chat and per-turn holds.

**Changes not recorded in the plan, made during external review (`a8126eb`):**
- The state file goes beyond the plan's Technical Details schema:
  - `acpLastRun` gained `TaskModel`/`ReviewModel`, so chat and commands use the launch's model overrides.
  - `acpSessionState` gained `Actions`: up to five pending merge/PR outcomes, each capped. They are prepended to the next chat message and cleared after a successful chat turn.
- `EnsureLocalGitignore` moved into `openSession`, because command turns now write the state file too.

**Other unlisted changes:**
- Both plugin manifests bumped to 0.7.2. The repository policy requires this whenever a skill changes, and the `loopai-t3` Claude and Codex skills were updated to describe thread chat.
- The plan's Decisions refer to `docs/backlog/acp-chat-models-in-t3-picker.md` as already filed (`258b1ef`). It is not part of this branch's diff.

## Backlog
- `docs/backlog/symlinked-checkout-path-containment.md`: "Path containment checks reject plans in a checkout reached through a symlink". **Filed to backlog** and committed on this branch, still open. Only the test-side workaround (`TMPDIR` resolved in `TestMain`) was applied here; the product-side containment fix is deferred.

## External review
### codex:gpt-6-astra:high
- label: codex
- iterations: 2
- duration_ms: 755702
- ended by: done
- had findings: true

Iteration 1:
- [P2] Chat loses the plan launch's model overrides (`cmd/loopai/acp_chat.go:64`): chat reloaded config without the launch's `--task-model`/`--review-model` -> fixed. `acpLastRun` now records `TaskModel`/`ReviewModel`, and `openSession` applies them through `acpSessionState.sessionOpts` after revalidating them with `validatePassThroughValues`. Covered by `TestACPStateSessionOpts` and `TestServeACPChatUsesLaunchModels`.
- [P2] Merge/PR outcomes never reach the resumed conversation (`cmd/loopai/acp_chat.go:199`) -> fixed. `acpSessionState.Actions` records outcomes through `withAction`, `acpChatMessage` prepends them to the next chat turn, and a successful turn clears them. Covered by `TestACPStateWithAction` and `TestServeACPChatSeesActionOutcomes`.

Iteration 2:
- No issues found. The reviewer confirmed both fixes, and they were committed as `a8126eb`.

## Validation
- Commands: none. The plan defines no `## Validation Commands` section.
- Timings: duration_ms 0, runs 0.

The deterministic facts record no validation timings. What follows is only what the evaluator said it ran, and was not measured:

- **External-review iteration 1:** `make test` passed and `make lint` reported 0 issues.
- **Iteration 2, before commit `a8126eb`:** only `go test -run ACP` in `cmd/loopai` and `golangci-lint` on `cmd/loopai` were run. The full `make test` was not re-run after that final commit.