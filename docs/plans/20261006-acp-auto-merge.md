# ACP Auto-Merge and Follow-Up Guidance

## Overview
- A loopai run hosted in T3 Code agent mode (`loopai --acp`) currently ends with the completion report and nothing else. Merging the plan branch takes a second session (`loopai --merge` or `/loopai:loopai-merge`), and any message typed into the thread afterwards is parsed as a plan launch, so a question like "а по итогу оно замержено?" fails with `malformed prompt: unexpected argument "по"`.
- After a successful ACP run whose report rates Risk as `low` or `medium`, loopai merges the plan branch into the local base branch on its own. It then appends the merge outcome to the final thread message. A `high`, unknown, or missing Risk, a conflict, or an unmet precondition skips the merge and says why.
- When the merge is skipped or the run failed, the final message says explicitly that follow-up work belongs in a new session, where the user can also pick another model. A thread message that is not a plan launch gets the same guidance as an ordinary answer, instead of a failed turn.
- The merge is local only: nothing is pushed. It never deletes the plan branch and never removes the T3 Code-owned worktree. Each ACP session runs inside that worktree, so removing it fails on Windows with `Permission denied`, and T3 Code owns it in any case.

## Decisions
- **Context**: the user wants T3 Code threads to close out low-risk work by themselves and to stop pretending to be a chat. Follow-up work belongs in a fresh session with a model chosen there, which keeps the loopai thread a pure launcher.
- **Chosen approach**: an ACP-only post-run step in `acpRunner.run`. It is gated by a new boolean config key `acp_auto_merge` (default `true`) and by the report's Risk level. It reuses the existing close-out merge primitive `mergeForCloseout`, which commits the merge and verifies ancestry, but runs none of `runMergeCommand`'s branch deletion or worktree cleanup.
- **Rejected alternatives**:
  - A conversational fallback that hands thread messages to the `task_model` provider with `--resume`. The user rejected it: follow-ups should happen in a new session.
  - Reusing `finalize = merge`. That pushes, opens a GitHub PR, and waits for CI, while the user wants a local merge into the base checkout.
  - Running `runMergeCommand` as is. It deletes the branch and removes the feature worktree, which is T3 Code's and is the process cwd.
- **Verified facts**:
  - `mergeForCloseout` (`cmd/loopai/main.go`) checks out the base when needed and calls `MergeBranchCommitContext`. That runs `git merge --commit` and verifies ancestry, and it aborts and rolls back on conflict or failed verification. It deletes nothing.
  - `finalizeModeFor` does not look at `NonInteractive`, so finalize already runs under ACP.
  - `splitReportSections` (`cmd/loopai/pr_body.go`) splits a report into `## ` sections and is fence-aware.
  - The report prompt asks for `low`, `medium`, or `high` in free text. The facts-only fallback writes `_assessment unavailable_`.
  - `acpRunResult` sends `Result.Message` as an `agent_message_chunk` even when it also returns an error. A nil error answers `end_turn`.
  - On 2026-10-06, `loopai --merge` from the primary checkout merged `launch-history` but then failed with `failed to delete '…\launch-history': Permission denied`, because a live session's cwd was that worktree.

## Context (from discovery)
- Files/components involved:
  - `cmd/loopai/acp.go`: `acpRunner.run`, `parseACPPrompt`, `acpRunResult`, `acpPromptUsage`, `loadACPSessionConfig`.
  - `cmd/loopai/main.go`: `mergeForCloseout`, `closeoutMergeResult`, `closeoutMergeError`, `worktreePathForBranch`, `openMergeWorktree`, `planExecutionOutcome`, `prepareNonInteractiveRequest`, `executePlanRequest.DefaultBranch`.
  - `cmd/loopai/pr_body.go`: `splitReportSections`.
  - `pkg/config/config.go` and `pkg/config/defaults/config`: the new key.
  - `pkg/git/service.go`: `Worktrees`, `IsDirtyAll`, `BranchHash`, `CurrentBranch`/`HeadHash` equivalents, `MergeBranchCommitContext`.
  - Docs: `docs/t3-code.md` (sections "Running a plan" and "What the thread shows"), the README ACP paragraph, the CLAUDE.md ACP agent-mode paragraph, and the `llms.txt` ACP paragraph.
- Related patterns found:
  - Close-out failures never fail a run. They become `finalizeResult.incomplete` and are surfaced as text. The auto-merge follows the same contract: a skipped or failed merge is reported in the message and never turns a successful run into a failed turn.
  - ACP stdout discipline: everything human-readable goes to `a.out` (stderr). The thread sees only `Result.Message` and sink updates.
- Dependencies identified: none new.

## Development Approach
- **Testing approach**: Regular (code first, then tests)
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
- Maintain backward compatibility
- Windows host safety: the Go suite assumes POSIX, and on Windows a test that redirects only `HOME` can reset the real `~/.config/loopai`. Run the Go tests in WSL or in a Linux container. If you run them natively, export both `HOME` and `USERPROFILE` to a temp dir first, and treat the known Windows-only failures in `cmd/loopai` as pre-existing.

## Testing Strategy
- **Unit tests**: required for every task. Risk parsing is table-driven. The merge step runs against real temporary Git repositories with a linked worktree, built through the existing `setupTestRepo` helpers. The ACP wiring is tested through `acpFixture`/`acpTestClient` in `cmd/loopai/acp_test.go`.
- **E2E tests**: not applicable. The dashboard is unchanged.

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

### Task 1: Extract the report's Risk level
- [ ] add `reportRiskLevel(report string) string` in `cmd/loopai/pr_body.go`, built on `splitReportSections`:
  - find the first `Risk` section and take its first non-empty line
  - strip Markdown emphasis and code marks (`*`, `_`, `` ` ``) and trailing punctuation, then lowercase it
  - return `low`, `medium`, or `high` when the first word is one of them, and `""` otherwise (absent section, `_assessment unavailable_`, free text)
- [ ] write table-driven tests in `cmd/loopai/pr_body_test.go`: `**low**`, `Low.`, `` `medium` ``, `high - …`, a Risk heading inside a code fence (ignored), missing section, the fallback `_assessment unavailable_`, a first line starting with another word, CRLF input
- [ ] run tests - must pass before next task

### Task 2: Add the `acp_auto_merge` config key
- [ ] add `ACPAutoMerge bool` to `config.Config` with loader support for `acp_auto_merge` at every config layer, defaulting to `true`. Follow how an existing boolean key with a `true` default and an explicit-set flag is handled, so a local `false` overrides a global `true`.
- [ ] document the key in `pkg/config/defaults/config` next to `finalize`: ACP agent mode only; merges into the local base after a successful run whose report rates Risk `low` or `medium`; nothing is pushed, no branch or worktree is removed; skipped when `finalize` is `pr` or `merge`
- [ ] write tests in `pkg/config/config_test.go` (or the loader's test file): default true, global false, local overriding global, invalid value error
- [ ] run tests - must pass before next task

### Task 3: Implement the local auto-merge step
- [ ] create `cmd/loopai/acp_merge.go` with `acpAutoMerge(ctx, gitSvc *git.Service, cfg *config.Config, defaultBranch, report string) acpMergeResult`. `acpMergeResult` carries `merged bool`, `kind string` (fast-forward / merge commit / already up to date), `base`, `feature`, `head` (short SHA), and `skipped string` (reason). Precondition checks, in order, each producing a `skipped` reason and no repository change:
  - `cfg.ACPAutoMerge` is false
  - `cfg.EffectiveFinalize()` is `pr` or `merge`, so GitHub owns close-out
  - Risk from `reportRiskLevel` is not `low`/`medium`; the reason names the level or "Risk level not stated"
  - the current branch cannot be resolved, or HEAD is detached
  - the base resolved from `defaultBranch` (strip `origin/`, require `refs/heads/<base>`) is missing or equals the feature branch
  - the feature checkout is dirty (`IsDirtyAll`)
  - the base is not checked out in any registered worktree (`Worktrees` + `worktreePathForBranch`); no checkout switching is done on the user's behalf
  - that base worktree is dirty
- [ ] perform the merge in the base worktree through `openMergeWorktree` and `mergeForCloseout` with the feature head from `BranchHash`. A conflict or failed verification becomes `skipped` with the wrapped reason from `closeoutMergeError` (merge aborted, base unchanged). Never call `DeleteBranch`, `cleanupMergedWorktree`, or any push.
- [ ] add `(r acpMergeResult) message() string`, which renders a `## Merge` Markdown section:
  - merged: `` Merged `<feature>` into `<base>` (<kind>, `<sha>`). Not pushed. ``
  - skipped: `` Not merged into `<base>`: <reason>. ``
- [ ] write tests in `cmd/loopai/acp_merge_test.go` against temp repos with a linked worktree on the feature branch and the base checked out in the primary:
  - low risk merges and the base advances; the feature branch and worktree still exist
  - medium risk merges
  - high, unknown, and fallback risk skip
  - `acp_auto_merge = false` skips
  - `finalize = pr` and `finalize = merge` skip
  - a dirty feature tree, a dirty base worktree, and a base checked out nowhere each skip
  - a conflict skips and leaves the base unchanged with no `MERGE_HEAD`
  - feature equal to base skips
  - `message()` output for each kind
- [ ] run tests - must pass before next task

### Task 4: Wire the merge and follow-up guidance into the ACP run result
- [ ] in `acpRunner.run`, after `selectAndExecutePlan` returns with `execReq.Outcome.succeeded` and no error:
  - open the session-cwd Git service the same way the run does (respecting `vcs_command`) and call `acpAutoMerge` with `execReq.DefaultBranch` and `execReq.Outcome.report`
  - write a one-line summary to `a.out`, never stdout
  - on cancellation (`ctx.Err() != nil`), skip the merge entirely
- [ ] extend `acpRunResult` (or add a small composer it calls) so the final message is:
  - the report
  - the `## Merge` section, when a merge was attempted or skipped after a successful run
  - a `## Next steps` section, whenever the run failed or the merge was skipped: "This thread only launches loopai plans. For follow-up work or questions, open a new session on this worktree and choose the model there." On a merged run, no follow-up block is added.
- [ ] keep the turn outcome unchanged: a successful run with a skipped or failed merge still answers `end_turn`, and a failed run still answers with the JSON-RPC error and its message
- [ ] write tests:
  - extend `TestACPRunResult` for message composition with merged, skipped, and failed-run cases
  - add an in-process `TestServeACP…` case that runs the two-task fixture plan in a linked worktree, with a fake report rating Risk `low`, and asserts that the base advanced, the branch and worktree survive, and the final `agent_message_chunk` contains `## Merge`
  - add a case with Risk `high` asserting no merge and the `## Next steps` text
- [ ] run tests - must pass before next task

### Task 5: Answer non-launch thread messages with guidance
- [ ] add `acpLooksLikeLaunch(text, cwd string) bool`. It returns true when the first whitespace-separated token starts with `-`, ends in `.md` (case-insensitive), or names an existing path relative to `cwd`. Empty text returns false.
- [ ] in `acpRunner.run`, when `parseACPPrompt` fails and `acpLooksLikeLaunch` is false, return `acp.Result{Message: <guidance>}` with a nil error, so the turn ends `end_turn`. The guidance text:
  - "This thread runs loopai plans and does not answer questions. To ask about a run or continue the work, open a new session on this worktree and choose the model there."
  - then the usage line, `acpPromptUsage`
- [ ] keep a malformed launch (bad flag, two plan files, missing value) a failed turn exactly as today
- [ ] write tests:
  - `acpLooksLikeLaunch` table: a Russian question, an English question, a `docs/plans/x.md` path, a bare existing file without `.md`, a `--task-model` first token, empty text
  - extend `TestServeACPPromptErrors` (or add a sibling test) so the question `а по итогу оно замержено?` answers `end_turn` with the guidance message and `docs/plans/two.md --worktree` still fails with `unsupported option`
- [ ] run tests - must pass before next task

### Task 6: Verify acceptance criteria
- [ ] verify all requirements from Overview are implemented
- [ ] verify edge cases are handled (cancel mid-run skips merge; finalize sync then merge works; chain prompts remain rejected)
- [ ] run full test suite (unit tests) per the Windows host safety note
- [ ] run linter - all issues must be fixed
- [ ] verify test coverage meets project standard (80%+) for the new files

### Task 7: [Final] Update documentation
- [ ] `docs/t3-code.md`: in "Running a plan" and "What the thread shows", describe the auto-merge (risk gate, local only, no push, no branch or worktree removal, `acp_auto_merge`, finalize pr/merge precedence) and the guidance reply to non-launch messages
- [ ] update the README ACP paragraph and the `llms.txt` ACP paragraph
- [ ] update the CLAUDE.md ACP agent-mode paragraph with `acpAutoMerge`, `reportRiskLevel`, `acpLooksLikeLaunch`, and why the T3 worktree and branch are never removed

## Technical Details
- Risk parsing: in `## Risk\n\n**low**\n\n- Public APIs…`, the first non-empty line is `**low**`, which normalizes to `low`.
- Merge location: the base's registered worktree (normally the primary checkout on `master`), opened with `openMergeWorktree`. The feature head is pinned by `BranchHash(feature)`, and `MergeBranchCommitContext` verifies that this head became an ancestor of the base.
- Final message layout: `<report>\n\n## Merge\n<line>` plus an optional `\n\n## Next steps\n<guidance>`.
- Non-launch replies use the `end_turn` stop reason with a message. Malformed launches keep the JSON-RPC error.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- Rebuild with `make build` and run a small plan in a T3 Code agent-mode thread. Confirm the `## Merge` section, that `master` advanced in the primary checkout, and that the thread's worktree and branch survive.
- Type a question into a finished thread and confirm the guidance reply arrives as a normal message.
- Remove the merged branch and its T3 worktree by hand, or through T3 Code, once the thread is no longer needed.
