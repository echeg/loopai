# ACP thread chat

## Overview
- In T3 Code agent mode a thread runs loopai through the Grok driver (`cmd/loopai-acp`, `loopai --acp`).
  After a run the thread shows the completion report, but any further message that is not a plan
  launch gets the fixed `acpNotALaunch` reply ("open a new session on this worktree"), and the report's
  `## Next steps` says the same. T3 Code locks a thread to its provider, so the user cannot ask about
  the report, request a fix, or say "давай мержить" in the thread that holds the report and worktree.
- This plan keeps the thread usable after a run: non-launch messages become chat turns answered by the
  session's `review_model` in the thread's worktree, with provider-session resume across turns and
  restarts; `merge`/`pr` (typed directly or requested through the model) run loopai-owned actions that
  never remove the thread's worktree.

## Decisions
- **Context**: `acpRunner.run` (`cmd/loopai/acp.go:165`) answers every non-launch message with
  `acpNotALaunch`; each ACP prompt is a self-contained run and `session/load` restores nothing. T3 Code
  forbids switching providers inside a thread ("Claude is unavailable in this thread. Start a new
  thread to switch providers"), so the guidance forces the user into a new thread without the report.
  A plain `loopai --merge` cannot be used from the thread either: it removes the feature worktree,
  which T3 Code owns and which is the ACP process cwd.
- **Chosen approach**:
  1. Routing in `acpRunner.run`: `acpLooksLikeLaunch` → plan run as today; the exact commands `merge`,
     `мерж`, `pr`, `пр` (case-insensitive, trimmed) → the action directly, no model; everything else → a
     chat turn. `acpNotALaunch` is removed; `acpNextSteps` says "write here: questions, edits, `merge`, `pr`".
  2. Session state `.loopai/acp/<session-id>.json` in the session cwd, atomic write, mode 0600:
     `provider`, `provider_session_id`, and `last_run` (plan path, branch, base, tip from
     `planExecutionOutcome.branchTip`, auto-merge result and skip reason, report text). Survives a T3
     restart and `session/load` (same session id). A new plan run overwrites `last_run` and clears
     `provider_session_id`. Chat with no `last_run` still works with branch plus `git log <base>..HEAD`.
  3. Chat turn execution: `processor.ModeACPChat` + `phase.ChatPhase`, modeled on
     `ModeGenAgents`/`GenAgentsPhase`, so retry, `wait_on_limit`, and claude-swap match other phases.
     One review-executor session per turn, from the session's `review_model` (falling back to
     `task_model` as today).
  4. Provider resume: `pkg/executor` gains an optional resume session id and reports the session id in
     `Result`. Claude: `--resume <id>` appended to that invocation on top of `claude_args`; id from the
     stream-json `system/init` event. Codex: `codex exec resume <id>` instead of `codex exec`; id capture
     already exists. Wrapper commands (`IsRealClaudeCommand`/`IsRealCodexCommand` false) and `custom`:
     no resume — fresh session per turn with the report context re-sent and one note line in the reply.
     Resume failure: one retry with a fresh session, id cleared. A canceled turn persists no new id.
  5. Prompt `acp_chat.txt` (embedded default, overridable in `.loopai/prompts/`): first turn of a
     provider session carries branch, base, `git log <base>..HEAD --oneline`, the `last_run` report and
     auto-merge outcome; resumed turns send only the user message. Allowed: read, answer, edit, and
     pathspec-commit on this branch. Forbidden: `loopai --merge`, `git worktree remove`,
     `git checkout`/`switch` to another branch, `push`, `reset --hard`. Merge/PR only when the user asks
     in this message: end the reply with `<<<RALPHEX:ACP_MERGE>>>` or `<<<RALPHEX:ACP_PR>>>`; loopai
     performs the action. Both signals join `knownSignals` and keep the RALPHEX name per fork policy.
  6. Thread output: reasoning through `sink.WrapLogger` as `agent_thought_chunk`; the final answer is
     the final `agent_message_chunk` with signals stripped. No tool-call cards (accepted limit).
  7. `acpRequestedMerge` from `acpAutoMerge` parts: skips all policy gates (`acp_auto_merge`,
     `finalize` mode, report Risk) because a human asked; keeps every repository gate (resolvable
     non-detached feature, local base distinct, `OperationInProgress` before `IsDirty`/`UntrackedFiles`
     on both checkouts with the `isLocalOverridePath` exemption, base checked out in some worktree,
     under `AcquireWorktreeCreationLockContext` bounded by `acpMergeLockTimeout`). Merges the feature's
     current HEAD by hash through `MergeBranchTipContext` with `Merge branch '<feature>'`; lists commits
     added after the `last_run` tip; already merged (HEAD ancestor of base) → "already in <base>", no-op.
     No push, no branch or worktree deletion, no checkout switch.
  8. `acpRequestedPR` reuses `createPullRequest` (`cmd/loopai/main.go:5962`; origin/gh repository match,
     push, `gh pr create`), body from the `last_run` report via `closeoutTarget.report`, legacy body when
     absent; an existing PR for the branch → reply with its URL. Worktree untouched.
  9. Errors: gate refusals (dirty tree, base not checked out, conflict aborted as today) end the turn
     normally with the reason and next step; unexpected errors (repository cannot open, broken config)
     fail the turn. An unrequested merge signal is guarded only by the prompt: the merge is local,
     reversible, and gated.
- **Rejected alternatives**:
  - chat models in the T3 picker (`claude-chat`, `codex-chat`) through `loopai-acp models` and
    `session/set_model`: depends on undocumented Grok-driver behavior; filed as
    `docs/backlog/acp-chat-models-in-t3-picker.md` (258b1ef);
  - commands only: cannot answer questions about the report;
  - full chat without built-in commands: nothing stops `loopai --merge` or `git worktree remove`;
  - loopai-owned transcript re-injected per turn, or no memory: the user chose provider resume for full
    tool context;
  - a new `acp_chat_model` key or `task_model` as the chat model: the user chose `review_model`;
  - a model-invoked CLI flag such as `--merge-in-place`: no recorded run tip and the model could merge
    unprompted mid-task;
  - auto-merge gates for an explicit request: blocks exactly the high-risk and post-report-fix cases;
  - a risk gate with a `merge force` confirmation: extra turn and pending-confirmation state;
  - a direct executor call from `cmd/loopai`: loses limit, retry, and claude-swap policy.
- **Verified facts**:
  - `acpNotALaunch`, `acpLooksLikeLaunch`, and routing at `cmd/loopai/acp.go:165-269`; `acpNextSteps` and
    `acpRunResult` at `cmd/loopai/acp.go:362-398`.
  - `acp.PromptRequest{SessionID, Cwd, Text}` and `acp.Result{Message}` at `pkg/acp/server.go:25-38`;
    `Sink.Message` at `pkg/acp/sink.go:248`, `Sink.WrapLogger` at `:679`; `session/set_model` answered
    with `{}` at `pkg/acp/server.go:91`; `session/load` replays nothing (`docs/t3-code.md:310-313`).
  - `executor.Result` (`pkg/executor/executor.go:28`) has no session id; `ClaudeExecutor.Run` at
    `pkg/executor/executor.go:447`; `CodexExecutor.Run` at `pkg/executor/codex.go:198` starts
    `args := []string{"exec"}`; Codex session id extraction at `codex.go:577` and `:737`;
    `knownSignals` lives in `pkg/executor/executor.go`.
  - `codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]` exists in the installed codex CLI.
  - `ModeGenAgents` at `pkg/processor/runner.go:30` and its dispatch at `:416`; phase in
    `pkg/processor/phase/gen_agents.go`; prompt file constant `genAgentsPromptFile` at
    `pkg/config/config.go:28`; `promptBuilder.GenAgentsPrompt` at `pkg/processor/prompt_builder.go:121`.
  - `acpAutoMerge` and `acpMergePolicySkip` at `cmd/loopai/acp_merge.go:32,124`; `ACPAutoMerge` at
    `pkg/config/config.go:192`.
  - `loopai-acp models` advertises only `loopai` (`cmd/loopai-acp/main.go:72`).
- **Non-goals**: model picker entries in T3; tool-call cards; push or branch/worktree deletion on merge;
  chat outside ACP; a new config key for the chat model; a code-level guard against an unrequested
  merge signal.
- **Success criteria**: in a T3 thread after a report, "почему риск medium?" gets a model answer;
  "поправь X" produces a commit on the branch; "давай мержить" merges the current HEAD into the local
  base while the thread worktree stays alive; after a T3 restart the next message continues the same
  provider conversation.

## Context (from discovery)
- Files/components involved:
  - `cmd/loopai/acp.go`, `cmd/loopai/acp_merge.go`, new `cmd/loopai/acp_state.go`, `cmd/loopai/acp_chat.go`
  - `pkg/executor/executor.go`, `pkg/executor/codex.go`
  - `pkg/processor/runner.go`, `pkg/processor/prompt_builder.go`, new `pkg/processor/phase/chat.go`
  - `pkg/config/config.go`, new `pkg/config/defaults/prompts/acp_chat.txt`
  - `pkg/acp/sink.go` (only if final-message ordering needs a change)
  - `docs/t3-code.md`, `CLAUDE.md`, `llms.txt`
- Related patterns found:
  - `--gen-agents` is a one-session mode with its own phase, prompt, and progress log — the template
    for `ModeACPChat`.
  - `acpAutoMerge` already separates policy gates (`acpMergePolicySkip`) from repository gates
    (`acpCleanWorktreeSkip`, `acpCompletedTip`), so the explicit merge reuses the latter.
  - `.run.json` and review checkpoints use atomic `0600` writes through `writeFileAtomic`.
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
- Run tests after each change (`make test </dev/null`)
- Maintain backward compatibility
- Tests redirect `HOME` and `USERPROFILE` to `t.TempDir()`; Git fixtures use temp repos and bare
  remotes; `gh` is a `PATH`-injected stub.

## Testing Strategy
- **Unit tests**: executor resume arguments and session id capture; `ChatPhase`; ACP routing, state file,
  explicit merge and PR; sink ordering.
- **E2E tests**: not affected (no dashboard change).

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): tasks achievable within this codebase - code changes, tests, documentation updates
- **Post-Completion** (no checkboxes): items requiring external action - manual testing, changes in consuming projects, deployment configs, third-party verifications
- **Checkbox placement**: Checkboxes belong only in Task sections (`### Task N:` or `### Iteration N:`). Do not put checkboxes in Success criteria, Overview, Context, Decisions, or Decision Log — they cause extra loop iterations.

## Decision Log
- 2026-10-09 loopai-brainstorm: **accepted** - commands plus model chat after a run
- 2026-10-09 loopai-brainstorm: **rejected** - "commands only" - cannot answer questions about the report
- 2026-10-09 loopai-brainstorm: **rejected** - "full chat without built-in commands" - nothing prevents a thread-breaking merge
- 2026-10-09 loopai-brainstorm: **accepted** - `review_model` answers the chat
- 2026-10-09 loopai-brainstorm: **rejected** - "new acp_chat_model key" and "task_model as chat model" - extra key, or the author judging its own work
- 2026-10-09 loopai-brainstorm: **accepted** - provider session resume for memory between turns
- 2026-10-09 loopai-brainstorm: **rejected** - "loopai-owned transcript" and "no memory" - lose tool context or break follow-up questions
- 2026-10-09 loopai-brainstorm: **accepted** - model emits `ACP_MERGE`/`ACP_PR` signals, loopai executes; bare `merge`/`pr` bypass the model
- 2026-10-09 loopai-brainstorm: **rejected** - "model-invoked --merge-in-place flag" - no recorded run tip, unprompted merges possible
- 2026-10-09 loopai-brainstorm: **accepted** - explicit merge skips risk and policy gates and merges the current HEAD
- 2026-10-09 loopai-brainstorm: **rejected** - "auto-merge gates for explicit merge" and "risk gate with merge force" - block the cases that need an explicit merge, or add a confirmation turn
- 2026-10-09 loopai-brainstorm: **accepted** - non-launch messages route to chat by default
- 2026-10-09 loopai-brainstorm: **rejected** - "chat models in the T3 picker" - undocumented driver contract; moved to backlog
- 2026-10-09 loopai-brainstorm: **accepted** - `ChatPhase` modeled on gen-agents
- 2026-10-09 loopai-brainstorm: **rejected** - "direct executor call in cmd/loopai" - loses limit, retry, and claude-swap policy

## Implementation Steps

### Task 1: Add provider session resume to the executors
- [x] add `SessionID string` to `executor.Result` (`pkg/executor/executor.go:28`) and an optional resume id on the Claude and Codex executors (a field or per-run option, matching how per-run state is passed today)
- [x] Claude: capture `session_id` from the stream-json `system/init` event into `Result.SessionID`; when a resume id is set, append `--resume <id>` to that invocation after the configured arguments, including when `claude_args` replaces the defaults
- [x] Codex: return the already extracted session id in `Result.SessionID`; when a resume id is set, build `exec resume <id>` instead of `exec`, keeping loopai's `-c` overrides, sandbox flags, and `codex_args` extras valid for the `resume` subcommand (verify option placement against `codex exec resume --help` and adjust if `resume` rejects any flag loopai emits)
- [x] ignore the resume id when `IsRealClaudeCommand`/`IsRealCodexCommand` is false, so wrapper commands always start fresh
- [x] write tests: Claude args with and without resume, with replaced `claude_args`; session id from a fixture `system/init` line; Codex args for `exec` versus `exec resume <id>` with overrides and extras; session id surfaced in `Result`; wrapper commands ignore the resume id
- [x] run `make test </dev/null` and `make lint` - must pass before task 2
- [x] ➕ fix pre-existing failures surfaced by validation: macOS `/var` symlink in `codex_image_test.py` and `cmd/loopai` fixtures (`TMPDIR` resolved in `TestMain`; product-side issue filed as `docs/backlog/symlinked-checkout-path-containment.md`), `unparam` lint in `pkg/git/external.go`

### Task 2: Add the chat signals and the acp_chat prompt
- [x] add `ACP_MERGE` and `ACP_PR` signal constants (`<<<RALPHEX:ACP_MERGE>>>`, `<<<RALPHEX:ACP_PR>>>`) beside the existing ones in `pkg/status` and register both in `knownSignals` (`pkg/executor/executor.go`)
- [x] add a signal-stripping helper that removes recognized signal lines from text shown to the user
- [x] create `pkg/config/defaults/prompts/acp_chat.txt` with the header comment listing its variables, the first-turn context block (branch, base, log, report, auto-merge outcome), the allowed and forbidden actions from Decisions item 5, the pathspec-commit rule, and the explicit-request rule for the two signals; add `acpChatPromptFile` in `pkg/config/config.go` and load it like `gen_agents.txt` (local → global → embedded)
- [x] add `promptBuilder.ACPChatPrompt(ctx ACPChatContext, message string, resumed bool)` in `pkg/processor/prompt_builder.go` that renders the full template on a fresh session and only the user message when resumed
- [x] write tests: both signals detected by the executor parser; stripping keeps surrounding text; prompt renders context on a fresh turn and only the message on a resumed turn; local override wins; `--dump-defaults` and `--init` include the new prompt (update their expected file lists)
- [x] run `make test </dev/null` and `make lint` - must pass before task 3

### Task 3: Add ModeACPChat and ChatPhase
- [x] add `processor.ModeACPChat` in `pkg/processor/runner.go` and dispatch it like `ModeGenAgents`, through the review executor
- [x] create `pkg/processor/phase/chat.go` with `ChatPhase` running one session with the rendered prompt and resume id, under the shared retry and limit policy, returning the answer text, the detected signal, and the provider session id
- [x] on a resume failure (non-zero exit or error result while a resume id was set, not a limit or cancellation), retry once with a fresh session and the full context, clearing the id
- [x] return no session id when the context was canceled, so the caller keeps the previous one
- [x] write tests with a mock executor: fresh turn, resumed turn, resume failure then fresh success, limit error goes through the limit policy, cancellation returns no id, `ACP_MERGE`/`ACP_PR` detected and stripped from the answer
- [x] run `make test </dev/null` and `make lint` - must pass before task 4

### Task 4: Add the ACP session state file
- [ ] create `cmd/loopai/acp_state.go` with `acpSessionState{Provider, ProviderSessionID, LastRun *acpLastRun}` and `acpLastRun{Plan, Branch, Base, Tip, Report, MergeSummary, MergeSkipped}`, stored at `.loopai/acp/<session-id>.json` relative to the session cwd
- [ ] implement `loadACPState` (missing file → empty state; corrupt file → warning on `a.out` and empty state) and `saveACPState` through `writeFileAtomic` with mode `0600`; reject a session id that is not a safe single path segment
- [ ] after a plan run in `acpRunner.run`, write `last_run` from the outcome and merge result and clear `ProviderSessionID`
- [ ] ➕ add `.loopai/acp` to `loopaiRuntimePaths` (`pkg/git`) so `EnsureLocalGitignore` ignores the state directory; otherwise the state file is an untracked path that `acpCleanWorktreeSkip`'s `UntrackedFiles` gate counts, skipping every merge
- [ ] write tests: create, atomic rewrite, mode 0600, corrupt file recovery, unsafe session id rejected, a new run resets the provider session id, state reloads for the same session id in a fresh `acpRunner` (simulating `session/load` after restart)
- [ ] run `make test </dev/null` and `make lint` - must pass before task 5

### Task 5: Add explicit merge and PR actions
- [ ] in `cmd/loopai/acp_merge.go` add `acpRequestedMerge(ctx, gitSvc, cfg, base, lastRun)` reusing the repository gates and lock of `acpAutoMerge` without `acpMergePolicySkip`; merge the feature's current HEAD by hash through `MergeBranchTipContext`; list commits after `lastRun.Tip` when HEAD moved; return "already in <base>" when HEAD is an ancestor of base
- [ ] add `acpRequestedPR` calling `createPullRequest` with the `last_run` report as `closeoutTarget.report` (legacy body when absent); detect an existing PR for the branch (`gh pr view <branch> --json url` before creating) and reply with its URL
- [ ] resolve the base from `last_run.Base`, falling back to the configured or detected default branch as `--merge` does; both actions run in the session cwd and never remove the worktree, delete the branch, push on merge, or switch checkouts
- [ ] write tests on the existing `acp_merge_test.go` fixtures: gate table (dirty feature, dirty base, operation in progress, detached HEAD, base not checked out, conflict aborted), high risk and `acp_auto_merge = false` still merge, HEAD moved past the report tip lists commits, already merged is a no-op, the feature worktree still exists afterwards
- [ ] write PR tests with a bare remote and a `PATH`-stub `gh`: created PR returns its URL; existing PR returns the existing URL without `gh pr create`; `gh` missing is a refusal message, not a failed turn
- [ ] run `make test </dev/null` and `make lint` - must pass before task 6

### Task 6: Route ACP prompts to launch, command, or chat
- [ ] in `cmd/loopai/acp.go` add `acpCommand(text) (kind string, ok bool)` matching trimmed, case-insensitive `merge`, `мерж`, `pr`, `пр`
- [ ] in `acpRunner.run` route launch → existing path; command → enter the session cwd, load session config and state, run `acpRequestedMerge`/`acpRequestedPR`, return its message; anything else → `runChat`
- [ ] create `cmd/loopai/acp_chat.go` with `runChat`: enter cwd, load session config and state, resolve the review spec, build the chat request through the same non-interactive wiring the run uses (`LogDecorator`/`PhaseObserver` on the sink, keep-awake holder), run `ModeACPChat` with the state's provider session id (dropped when the resolved provider differs from the stored one), save the new session id unless canceled, execute the signaled action and append its message, and return the stripped answer as `acp.Result.Message`; add the wrapper note line when resume is unavailable
- [ ] gate refusals end the turn with a message; repository-open and config errors fail the turn
- [ ] remove `acpNotALaunch`; rewrite `acpNextSteps` to "write here: questions, edits, `merge`, `pr`"; keep `acpPromptUsage` for malformed launches
- [ ] write tests: routing table (`docs/plans/x.md` launch, `--task-model ...` launch, `merge`, ` PR `, `мерж`, `test` → chat, `thanks` → chat); chat turn with a stub executor saves the session id and returns the stripped answer; second turn passes the stored id; canceled chat keeps the old id; provider change drops the id; `ACP_MERGE` signal runs the merge and appends its result; update the existing test asserting `acpNotALaunch` (`cmd/loopai/acp_test.go:865`)
- [ ] verify in `pkg/acp` that a chat turn's final message is sent after its thought chunks, and add a sink test if ordering needed a change
- [ ] run `make test </dev/null` and `make lint` - must pass before task 7

### Task 7: Verify acceptance criteria
- [ ] verify all requirements from Overview are implemented
- [ ] verify a plan launch still behaves exactly as before (existing ACP tests unchanged except the removed guidance)
- [ ] verify no code path in a chat or command turn calls `cleanupMergedWorktree`, `DeleteBranch`, or a checkout switch
- [ ] run full test suite (`make test </dev/null`)
- [ ] run linter (`make lint`) - all issues must be fixed
- [ ] cross-compile `GOOS=windows GOARCH=amd64 go build ./...`

### Task 8: [Final] Update documentation
- [ ] `docs/t3-code.md`: add a "Thread chat" section (routing, `merge`/`pr` commands, `review_model`, resume and its absence for wrappers, the state file, explicit merge versus auto-merge gates) and replace every "open a new session" statement, including the table row for non-launch messages
- [ ] `CLAUDE.md`: update the ACP paragraph for routing, `ModeACPChat`/`ChatPhase`, the `ACP_MERGE`/`ACP_PR` signals in `knownSignals`, the state file, `acpRequestedMerge` versus `acpAutoMerge`, and executor resume
- [ ] `llms.txt`: one line on chatting in a loopai T3 thread
- [ ] update embedded config or prompt header comments if they list prompt files

## Technical Details

**State file** `.loopai/acp/<session-id>.json`:

```json
{
  "provider": "claude",
  "provider_session_id": "6f1c…",
  "last_run": {
    "plan": "docs/plans/20261009-feature.md",
    "branch": "echeg/feature",
    "base": "main",
    "tip": "db78b1ba…",
    "report": "# Report: …",
    "merge_summary": "merged into main",
    "merge_skipped": ""
  }
}
```

**Chat turn flow**:
1. `acpRunner.run` → not a launch, not a command → `runChat`.
2. Load state; resolve the review spec; drop `provider_session_id` when the provider changed.
3. Render `acp_chat.txt` in full (fresh) or the message only (resume).
4. `ModeACPChat` → `ChatPhase` → executor with the resume id; on resume failure, one fresh retry.
5. Save the new session id unless canceled.
6. If the answer carries `ACP_MERGE`/`ACP_PR`, run `acpRequestedMerge`/`acpRequestedPR` and append the result.
7. Return the stripped answer (plus action result and wrapper note) as the final message.

**Command words**: `merge`, `мерж`, `pr`, `пр` — whole message, trimmed, case-insensitive; anything
longer goes to the model, which can still emit the signal.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- In T3 Code: launch a plan through `--t3-launch=agent`, then in the same thread ask about the report,
  request a small fix, type `merge`, and confirm the thread worktree still exists and the base advanced.
- Restart T3 Code and confirm the next message continues the same provider conversation.
- Re-check after the next T3 Code update, since the Grok driver contract is undocumented.
