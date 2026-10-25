# Mark the Mode 1 T3 thread unread when a detached run finishes

## Overview
- In Mode 1 (`loopai --detach --t3-thread`) the T3 Code thread shows the end of a run only as the
  `[loopai: done|failed|stopped]` title prefix. The sidebar's green "Done" badge, which Mode 2 threads
  get, never appears.
- T3 Code computes "Done" rather than storing it: `hasUnseenCompletion` shows it while the thread's
  latest run completed after `lastVisitedAt` and the thread is not working, waiting, failed, or woke.
  In Mode 1 the latest run is the agent turn that launched loopai, which the user saw hours before the
  run ends, so nothing is unseen.
- Fix: when the attempt's `finish` event arrives, the bridge dispatches `thread.mark-unread` as its
  very last command, after the final title and the unpin. T3 Code then rewinds `lastVisitedAt` to
  one millisecond before the latest run's completion, and the thread shows "Done" until the user
  opens it.
- This happens on every finish: succeeded, failed, and stopped. The badge is green for all three
  because T3 Code derives red "Failed" from the latest run's own status, which loopai cannot set; the
  title prefix carries the outcome.

## Decisions
- **Context**: T3 Code offers no command that sets a thread badge; the badge is derived from
  `latestRun.completedAt` versus `lastVisitedAt`.
- **Chosen approach**: dispatch `thread.mark-unread` (orchestration protocol 2, present in 0.1.1 and
  0.1.2) through the existing `orchestration.dispatchCommand` path, on every finish, as the last
  command of the bridge.
- **Rejected alternatives**:
  - sending a message into the thread (`t3_thread_send`/a new turn) so a fresh run completes: it costs
    a model turn, puts loopai text into the user's conversation, and the agent may act on it.
  - marking unread only on success: the user chose every finish, so a failed or stopped run also draws
    attention; the green badge on a failure is accepted because the title prefix says `failed`.
- **Verified facts** (T3 Code source, `/Users/echeg/Projects/t3code` at
  `v0.1.2-nightly.20261022.3526`; the installed app is 0.1.2):
  - `apps/web/src/components/Sidebar.logic.ts:716` `hasUnseenCompletion`, and `resolveSidebarV2TopStatus`
    returning `"done"` for an unread thread with status `ready`.
  - `apps/server/src/orchestration-v2/Orchestrator.ts:3040`: `thread.mark-unread` sets
    `lastVisitedAt = runs.at(-1).completedAt - 1ms`, and fails with a typed
    `OrchestratorDispatchError` ("has no completed run to mark unread") when the latest run has no
    `completedAt`, which is the case while a new agent turn is running in the thread.
  - `packages/contracts/src/orchestrationV2.ts`: the command is `{type: "thread.mark-unread",
    commandId, threadId}`, with no other fields.
  - `thread.unpin` and `thread.metadata.update` advance the thread's `updatedAt`; `thread.mark-unread`
    does not. The open thread's `ThreadVisitWatermark` sends `thread.visit` whenever `updatedAt` is
    newer than `lastVisitedAt`, and its per-`updatedAt` dedupe keeps a later mark-unread in place.
    So mark-unread must come after the title and the unpin, or a write after it would get the thread
    visited again. A thread the user has open may still clear the badge at once, which is acceptable
    since the user is looking at it.

## Context (from discovery)
- Files/components involved:
  - `pkg/t3/client.go`: thread commands `thread.metadata.update`, `thread.pin`, `thread.unpin`
    (`commandMetadataUpdate`/`commandPin`/`commandUnpin`, `SetTitle`/`Pin`/`Unpin`, `dispatch`).
  - `pkg/t3/t3test/fake.go`: the fake protocol-2 server, `Thread` model, and the dispatch switch that
    answers unsupported command types with a failure.
  - `cmd/loopai/t3_bridge.go`: `t3Bridge` state, `step()` order `syncPin, syncTitle, syncUnpin,
    syncTerminal`, `ended()`, `closeOut()` (best-effort last write when the bridge is disabled).
  - `cmd/loopai/t3_bridge_test.go`: `newBridgeHarness`, `TestT3BridgePin`,
    `TestT3BridgeFinalStates`, `TestT3BridgeDisableLeavesTheThread`, which assert exact command lists.
  - Docs: `docs/t3-code.md` ("What the thread shows" and the restart/stop paragraph), `llms.txt`
    (Mode 1 paragraph), `README.md` (Mode 1 description), `CLAUDE.md` (Mode 1 paragraph).
- Related patterns found: `syncPin`/`syncUnpin` treat a typed `*t3.RemoteError` as a definite answer
  (stop trying, no progress-log line) and send transport errors to `fail` for a reconnect and retry.
  `closeOut` ignores every error.
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

## Testing Strategy
- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: the dashboard Playwright suite is unaffected; no UI changes

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

## Implementation Steps

### Task 1: Add thread.mark-unread to the T3 client and the fake server
- [x] in `pkg/t3/client.go` add `commandMarkUnread = "thread.mark-unread"` beside the other command
      constants and a `MarkUnread(ctx, threadID)` method that dispatches it through `dispatch`, with a
      godoc saying it makes T3 Code show the thread's latest completed run as unseen
- [x] in `pkg/t3/t3test/fake.go` add `LatestRunCompletedAt` and `LastVisitedAt` (strings, like
      `PinnedAt`) to `Thread`, handle `thread.mark-unread` in the dispatch switch: a typed failure
      worded like T3's ("Thread <id> has no completed run to mark unread.") when
      `LatestRunCompletedAt` is empty, otherwise `LastVisitedAt` set to that time minus one
      millisecond in the same timestamp format; update the package comment's list of modeled commands
- [x] write tests in `pkg/t3/client_test.go` for `MarkUnread`: the frame carries exactly `type`,
      `commandId`, `threadId`, and a fresh command id per call
- [x] write tests in `pkg/t3/t3test/fake_test.go` for mark-unread: success rewinds `LastVisitedAt`,
      a thread without a completed run gets the typed failure, an unknown thread fails as other commands do
- [x] run `go test ./pkg/t3/...` - must pass before task 2

### Task 2: Mark the thread unread as the bridge's last command
- [x] in `cmd/loopai/t3_bridge.go` add a `markedUnread` field to `t3Bridge` and a `syncUnread` step,
      run after `syncUnpin` in `step()`: it acts only when `final` is set, the title is no longer
      dirty, and an unpin the bridge owes is done; it calls `MarkUnread`, treats success and a typed
      `*t3.RemoteError` (no completed run, an archived or busy thread) as done without a progress-log
      line, and passes any other error to `fail` so it is retried after the reconnect
- [x] extend `ended()` so a finished bridge waits for `markedUnread`
- [x] in `closeOut()`, when `final` is set, send a best-effort `MarkUnread` after the title and the
      unpin, ignoring its error, so a bridge disabled after the finish event still marks the thread
- [x] update the `t3Bridge` doc comment and the `t3BridgeStopTimeout` comment to name the mark-unread
- [x] give the default thread in `newBridgeHarness` a `LatestRunCompletedAt`, then update the exact
      command lists in `TestT3BridgePin` and any other test asserting `h.commands()` so
      `thread.mark-unread` is the last command
- [x] write tests: succeeded, failed, and stopped finishes each end with `thread.mark-unread` after
      the final title and the unpin, and the fake thread's `LastVisitedAt` precedes its
      `LatestRunCompletedAt`; no mark-unread is sent before the finish event
- [x] write tests for the error paths: a thread with no completed run (typed refusal) still lets the
      bridge end with no progress-log line; a `DieNext` on the mark-unread dispatch is retried and
      the thread ends marked; `TestT3BridgeDisableLeavesTheThread` "a finished run" also marks the
      thread unread while "a run still going" does not
- [x] run `go test ./cmd/loopai -run 'T3'` - must pass before task 3

### Task 3: Verify acceptance criteria
- [ ] verify the Overview: every finish state marks the thread unread, after the final title and
      the unpin, and a refusal never disables the bridge or affects the run
- [ ] run `make test </dev/null` (full suite)
- [ ] run `make lint` - all issues must be fixed
- [ ] run `GOOS=windows GOARCH=amd64 go build ./...`
- [ ] verify test coverage of the changed functions in `pkg/t3` and `cmd/loopai/t3_bridge.go` meets
      the project standard (80%+)

### Task 4: [Final] Update documentation
- [ ] `docs/t3-code.md`: add a bullet to "What the thread shows" describing the unread mark and the
      green "Done" badge on every finish (the title prefix carries the outcome; opening the thread
      clears it; T3 Code refuses it while a new agent turn runs in the thread), adjust "There is no
      completion message in Mode 1", and mention the mark in the paragraph on a bridge that stops
      after a refused request
- [ ] `llms.txt` Mode 1 paragraph, `README.md` Mode 1 description, and the Mode 1 paragraph of
      `CLAUDE.md`: one clause each about marking the thread unread at the end
- [ ] leave the `loopai-t3` skills unchanged (their wording stays accurate), so no manifest bump

*Note: loopai automatically moves completed plans to `docs/plans/completed/`*

## Technical Details
- Command frame: `orchestration.dispatchCommand` with
  `{"type":"thread.mark-unread","commandId":"<32 hex>","threadId":"<opaque id>"}`.
- Bridge end sequence: final `thread.metadata.update` (when the title changes) → `thread.unpin`
  (only when the bridge pinned the thread) → `thread.mark-unread`. `ended()` becomes
  `final && !titleDirty && (!pinned || unpinned) && markedUnread`.
- `dispatch`'s unknown-outcome command-id reuse already makes a retried mark-unread idempotent; its
  fingerprint is the type plus an empty title.
- T3's refusal arrives as a typed `*t3.RemoteError`, which `t3BridgeFatal` would otherwise count as
  disabling; `syncUnread` must check for it before calling `fail`, as `syncPin` and `syncUnpin` do.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- Run a short plan through `/loopai:loopai-t3` in Mode 1 from a T3 Code thread, switch to another
  thread, and confirm the thread shows `[loopai: done]` and the green "Done" badge when the run ends;
  repeat with `loopai --stop <run>` and confirm the badge with `[loopai: stopped]`.
- Confirm that opening the thread clears the badge and that the badge stays when the thread was not
  open at the finish.

**External system updates**:
- After T3 Code updates, re-check `thread.mark-unread` and `hasUnseenCompletion` at the new tag along
  with the other contracts listed in `docs/t3-code.md`.
