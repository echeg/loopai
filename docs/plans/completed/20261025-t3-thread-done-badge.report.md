# Report: Mark the Mode 1 T3 thread unread when a detached run finishes
plan: `docs/plans/20261025-t3-thread-done-badge.md` · branch: `t3-thread-done-badge` · base: `master` · mode: full · executor: claude · task model: `claude:opus:high` · review model: `claude:opus:xhigh` · started: 2026-10-25T08:04:54Z · finished: 2026-10-25T08:54:11Z

## Summary
In Mode 1 (`loopai --detach --t3-thread`), a T3 Code thread used to show the end of a run only through its `[loopai: done|failed|stopped]` title prefix. The bridge now sends `thread.mark-unread` when the attempt's `finish` event arrives. It goes out as the bridge's last command, after the final title and the unpin, on every finish: succeeded, failed, or stopped. T3 Code then sets `lastVisitedAt` to 1 ms before the latest run completed, so the sidebar shows its green "Done" badge until the user opens the thread.

What was delivered:
- **Client:** a new `Client.MarkUnread` in `pkg/t3`.
- **Fake server:** `pkg/t3/t3test` now models `thread.mark-unread`. A thread with no completed run gets a typed refusal, and an unparsable fixture timestamp is a `Die`.
- **Bridge:** a `syncUnread` step and a `markedUnread` flag in `cmd/loopai/t3_bridge.go`. `ended()` now waits for the mark, through a new `finalWritten()` helper. `closeOut()` sends a best-effort mark after the title and unpin when the attempt has finished.
- **Docs:** updated in four places.

A typed `*t3.RemoteError` counts as a definite answer, so a refusal never disables the bridge and never affects the run. A transport error goes through the existing reconnect-and-retry path.

Run: 4 task iterations with 0 failed retries. Internal review ran once (1 loop iteration) and ended `review_done`. Post-review did not run. Phase durations:

| phase | duration |
|---|---:|
| tasks | 1,355,426 ms (~22 min 35 s) |
| internal review | 1,525,475 ms (~25 min 25 s) |
| external review | 54,525 ms (~54.5 s) |
| evaluation | 21,057 ms (~21.1 s) |
| other | 0 ms |

Wall clock was 08:04:54Z–08:54:11Z (~49 min 17 s), covering work through finalize but not report assessment or archival. Finalize did not run, so there was no base sync and nothing from `origin/master` was merged into the branch.

## Change scope
14 files, +367 / −74 lines, 5 commits.

- **Production code (4 files):**
  - `pkg/t3/client.go`: `commandMarkUnread` and `MarkUnread`.
  - `pkg/t3/t3test/fake.go`: `Thread.LatestRunCompletedAt` and `LastVisitedAt`, a `timestampLayout`, and the `thread.mark-unread` dispatch case.
  - `cmd/loopai/t3_bridge.go`: the `markedUnread` field, `finalWritten()`, `syncUnread`, `ended()`, `closeOut()`, and comments.
  - `cmd/loopai/detach_child.go`: comment only.
- **Tests (4 files):**
  - `pkg/t3/client_test.go`: `TestMarkUnread`, plus an `ErrClosed` case.
  - `pkg/t3/t3test/fake_test.go`: `TestServerMarkUnread`.
  - `cmd/loopai/t3_bridge_test.go`: new `TestT3BridgeMarkUnreadFailures` and an `assertMarkedUnread` helper. Every exact command-list assertion now ends with `thread.mark-unread`. New subtests cover the final states, title regeneration, and disable/closeOut.
  - `cmd/loopai/detach_child_unix_test.go`: the end-to-end child asserts title → unpin → mark-unread and `LastVisitedAt`.
- **Docs (4 files):** `CLAUDE.md`, `README.md`, `llms.txt`, `docs/t3-code.md`. The last one gains a "What the thread shows" bullet, an update to the paragraph on a bridge that stops after a refused request, and an entry in the list of checks still open against a live T3 Code.
- **Plan and backlog:** checkbox ticks in the plan; new `docs/backlog/t3-bridge-regeneration-wait-outlasts-stop.md`.

Commits:
- `539ac1cb` feat: add thread.mark-unread to the T3 client and fake server
- `4c3b9316` feat: mark the T3 thread unread as the bridge's last command
- `c2a9d2bc` feat: verify T3 thread mark-unread acceptance criteria
- `8b8ced3a` feat: document the Mode 1 T3 thread unread mark
- `4efaf724` fix: address code review findings

## Evidence
The run facts record no validation command runs (0 runs, 0 ms). No test output is available as proof. The before/after below is the behavior the diff's tests assert, not a recorded run.

- **Command sequence.** Before, the exact command lists ended `… "thread.metadata.update", "thread.unpin"`, for example in `TestT3BridgePin`, `TestT3BridgeSurvivesAWriteIntoADeadShell`, and `TestT3BridgeTerminalErrorStopsOnlyTheTerminal`. After, they end `… "thread.unpin", "thread.mark-unread"`. `TestT3BridgeFinalStates` asserts this for succeeded, failed, and stopped, and also that no mark-unread is sent before the finish event.
- **End to end.** `TestDetachedChildBridgesToT3Thread` before checked the last two dispatches (title, unpin). After, it checks the last three (title, unpin, mark-unread). It also asserts `LastVisitedAt == "2026-10-23T10:59:59.999Z"` against a latest run completed at `2026-10-23T11:00:00.000Z`.
- **Error paths** (new `TestT3BridgeMarkUnreadFailures`):
  - A refusal (no completed run) is sent once and not retried, leaves no progress-log line, and leaves `LastVisitedAt` empty.
  - A `DieNext` on the mark-unread dispatch is retried after a reconnect with the same `commandId`.
  - An unpin with an unknown outcome is retried before the mark-unread.
- **Disable/closeOut.** In `TestT3BridgeDisableLeavesTheThread`, "a finished run" now ends with the mark, and "a run still going" sends none.

## Risk
**low**

- **Public APIs:** additive only. `pkg/t3` gains `Client.MarkUnread`, and `t3test.Thread` gains two string fields. Both packages are internal to this module. The CLI flags, the config keys, and the `report.txt` headings that `TestSkillContract` fingerprints are unchanged, and no skill text changed, so neither manifest needed a bump.
- **Data schemas:** none. `run.json`, `events.jsonl`, `outcome.json`, and bindings are untouched. The only new state is the in-memory `markedUnread` flag.
- **Configuration:** none. There is no new key or flag, and there is no opt-out: every Mode 1 finish now sends the command.
- **Concurrency:** the step runs inside the existing single-goroutine bridge loop, after `syncUnpin`. `ended()` now needs one more acknowledged request, which the before-exit stop still bounds with `t3BridgeStopTimeout` (10 s). A transport error retries through the existing reconnect path, and `dispatch`'s command-id reuse keeps that retry idempotent.
- **Migrations:** none.
- **Residual risk:**
  - The T3 Code contract (`thread.mark-unread` semantics, `hasUnseenCompletion`, and the visit watermark ordering) comes from reading T3 Code source at `v0.1.2-nightly.20261022.3526`. It has not been checked against a live app. `docs/t3-code.md` lists it as an open check, and the plan's Post-Completion manual verification is still outstanding.
  - The worst plausible failure is cosmetic: no badge, or a badge cleared at once on an open thread. The run itself is never affected.

## Merge danger
**Door:** two-way
A plain revert removes the extra command and its tests. Nothing persisted, published, or depended upon changes, and the per-thread `lastVisitedAt` it moves is transient T3 Code UI state.

**Blast radius:** narrow
Only the Mode 1 T3 bridge's end-of-run sequence, plus the in-repo test fake. At report time local `master` was one commit past the branch's merge base and touched `CLAUDE.md`, `README.md`, and `llms.txt`. `git merge-tree` predicted a clean merge.

## Migrations and operational steps
none

## Plan deviation
All four tasks are fully checked. The facts record no added, blocked, or skipped items.

The implementation follows the plan's Technical Details exactly: `ended()` is `finalWritten() && markedUnread`, with `finalWritten()` factored out. Beyond the plan's explicit bullets, consistent with its scope:

- **Fake server guard:** an unparsable `LatestRunCompletedAt` fixture answers with a `Die`, with a matching test.
- **End-to-end test:** `detach_child_unix_test.go` was updated, covered by the plan's "any other test asserting `h.commands()`".
- **Comment:** `detach_child.go` updated.
- **Review-fix commit (`4efaf724`):**
  - two new bridge subtests (an unpin with an unknown outcome is retried before the mark-unread; the mark-unread waits for title regeneration)
  - the open-checks addition in `docs/t3-code.md`
  - a `CLAUDE.md` wording fix
  - the backlog entry below

The plan's Post-Completion items (a manual T3 Code run in Mode 1, checking the badge on done and stopped runs and that opening the thread clears it) were not part of this run and remain open.

## Backlog
- `docs/backlog/t3-bridge-regeneration-wait-outlasts-stop.md`, "T3 bridge title-regeneration wait outlasts the before-exit stop": **filed, left open.**
  - Found during internal review, severity minor.
  - `syncTitle` can wait up to 2 min for a pending title regeneration, while the before-exit stop gives the bridge 10 s and runs no `closeOut`.
  - So a run that finishes before its first title write gets neither the final title nor, by design, the mark-unread.
  - The title half of the problem predates this change.
  - It was not fixed in this branch.

## External review
### codex:gpt-6-astra:xhigh
- label: codex
- iterations: 1
- duration_ms: 75516
- ended by: done
- had findings: false

Iteration 1 (truncated: false): the reviewer output was `NO ISSUES FOUND`. There were no findings to evaluate. The evaluator made no fixes, found the worktree clean, and created no commit, leaving the branch at `4efaf724`.

## Validation
- Commands: none. The plan has no `## Validation Commands` section, so no command was classified or timed.
- Timings: duration_ms 0, runs 0.

The plan's Task 3 checkboxes for `go test ./pkg/t3/...`, `go test ./cmd/loopai -run 'T3'`, `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` are ticked. No timing or output for them appears in the run facts.