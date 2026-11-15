# Journal directory and per-session usage for foreground runs

## Overview
- Add `--journal-dir <dir>`: an ordinary foreground run of one plan (full or `--tasks-only`), executed
  in the current checkout, writes `events.jsonl` and `outcome.json` into `<dir>` in the schema a
  detached run already uses, so a host process that spawned loopai can follow progress and read the
  result without parsing the progress log.
- Add a journal event type `usage`, appended once per finished provider session that reported usage:
  provider, model, phase, token counts, and the cost when the CLI reports one. Detached runs get it
  too, because they share the journal hooks.
- Problem it solves: bridex (a separate agent platform) will execute a plan as one of its own runs. It
  spawns loopai as a foreground child inside its bwrap/Seatbelt jail, in the task's own git worktree
  and branch. It needs live structured progress, a structured outcome, and the spend of every session
  for its run accounting and budget cap. Today only `--detach` writes a journal, and `--detach` cannot
  be used there: the jail runs with `--die-with-parent --unshare-pid`, so the detached child dies with
  the launcher, and `--detach` always creates a worktree of its own. loopai parses no usage at all.
- Nothing changes for a run without the flag, apart from the new `usage` events in a detached run's
  journal, which every existing reader ignores.

## Decisions
- **Context**: the bridex integration needs a file contract between the two repositories. This plan is
  the loopai half and a prerequisite of the bridex plan, which pins the loopai commit that contains it.
- **Chosen approach**:
  - `--journal-dir <dir>` attaches the existing `journalHooks` (`cmd/loopai/detach_journal.go:20`) to a
    foreground request built in `run()` and shares the outcome construction and status classification
    that live in `detachedChild.complete` (`cmd/loopai/detach_child.go:289`) and
    `classifyDetachedRun` (`:381`).
  - The run executes in the current checkout only; `--worktree` is refused with the flag.
  - The run is owned by its host process: it is not registered for `--runs`, `--attach`, or `--stop`,
    and writes no `run.json`, `run.lock`, `console.log`, or `report.md`.
  - The outcome is written only when execution returns, as for a detached run. A forced exit leaves
    no `finish` event and no `outcome.json`.
  - Usage travels as `executor.Result.Usage` to the one choke point every provider session passes,
    `retryPolicy.runWithSessionTimeout` (`pkg/processor/execution_policy.go:207`), which knows the
    provider (`toolName`) and the phase (`phaseHolder`), and from there through a `UsageHandler`
    wired like `CommandTimingHandler` (`cmd/loopai/main.go:4065`, `pkg/processor/runner.go:60`).
- **Alternatives not taken** (not judged by the user; open to critique): a JSON event stream on
  stdout; making `--detach` work inside a sandbox; registering the foreground run as a detached-style
  run; supporting every mode; computing prices inside loopai; an optional logger interface to reach the
  journal (a type assertion on the outermost logger never reaches the decorated inner one, because
  every wrapper embeds `Logger` as an interface: `cmd/loopai/cmux.go:718`, `orca.go:214`,
  `awake.go:180`).
- **Verified facts**:
  - The request struct is `executePlanRequest` (`cmd/loopai/main.go:293-356`) with the hook fields
    `Outcome`, `NonInteractive`, `LogDecorator`, `PhaseObserver`, `KeepWorktreeOnFailure`,
    `ArchiveOnRunBranch`, `AttemptStarted`. A plain foreground run sets none of them
    (`main.go:662-680`). `worktreeExecuteRequest` (`main.go:2330-2373`) copies the hook fields one by
    one, so a new field must be added there.
  - `prepareNonInteractiveRequest` (`main.go:805-844`) does not fit `run()`: it creates its own
    `WtCleanup` (`:836`) instead of the holder the interrupt watcher knows (`:521`, `:532`), sets no
    `KeepAwake` (`run()` creates one at `:587`), records no launch history (`:616`), resolves the
    execution dependencies and opens the repository a second time (`:820`, `:824`), and moves the
    human output to stderr (`:809`).
  - `NonInteractive` (`main.go:334-338`) means: a missing plan is an error, the pause handler and
    break signal are not installed, and the cmux and orca reporters are not constructed.
  - `AttemptStarted` fires only inside `runWithWorktree` (`main.go:2266`, `fireAttemptStarted` in
    `cmd/loopai/detach_worktree.go:28-44`), and `worktreeExecuteRequest` passes it on to `executePlan`
    (`main.go:2371`). The in-place path (`prepareSelectedPlanBranch` at `main.go:1037`, then
    `executePlan` at `:1042`) has no attempt-start point, and its progress logger is created only
    inside `executePlan` by `setupProgressLogger` (`main.go:1772`).
  - `attempt_start.WorktreePlan` is load-bearing: `journalHooks.startAttempt` stores it
    (`detach_journal.go:49`) and `parsePlan` re-reads that file for task titles and the `tasks`
    statuses (`:182-200`). Empty means no titles and no `tasks` events. No loopai reader uses
    `Worktree`, `ProgressLog`, or `ConsoleOffset`.
  - A non-nil `Outcome` enables `capturePlanOutcome`, which runs `GitSvc.HeadHash()` and returns its
    error as a run failure (`main.go:1951-1953`).
  - `detachedChild.complete` (`detach_child.go:289-335`) interleaves shared and detached-only steps:
    it appends the closing events, then sets `rec.WorktreeKept` from `keptWorktree` (`:323`), then
    runs `reconcileDetachedSourcePlan` (`:325`), then `runs.SaveOutcome` (`:328`), then
    `endProgressLog` and `beforeExit`, and returns an `exitCodeError`.
  - A detached run completes only on the normal return (`detach_child.go:136-143`); it has no
    forced-exit path. `startInterruptWatcher` (`main.go:6614-6631`) force-exits with `os.Exit(1)` five
    seconds after SIGINT or SIGTERM, giving the cleanup holders two seconds, and `planExecutionOutcome`
    has no mutex. A graceful stop returns the wrapped `ctx.Err()` and the process exits 1, while
    `classifyDetachedRun` reads a cancelled context as `stopped` with exit code 2.
  - A fresh foreground worktree is removed by a deferred `cleanup(true)` (`main.go:2319`) before a
    completion that runs after execution returns could re-read the plan file in it.
  - Every journal reader ignores an unknown event type: `runs.Snapshot.Apply`
    (`pkg/runs/observer.go:188-210`, `default: return`), `Follow` (`:32-70`), `classify.go:128`, the T3
    bridge (`cmd/loopai/t3_bridge.go:287-298`), the ACP renderer (`cmd/loopai/acp_render.go:104-122`),
    `--attach` and `--runs`. `parseEvents` (`pkg/runs/journal.go:362-378`) decodes into the flat
    `Event` struct and drops unknown keys.
  - `Event.payload()` (`journal.go:144-203`) writes only the header for a type it has no case for, so
    a new type needs a constant, a case, and fields on `Event`. The journal has no schema version;
    `schemaVersion` (`pkg/runs/descriptor.go:12`) covers the descriptor and the outcome only.
  - `OpenJournal` opens with `os.O_RDWR|os.O_APPEND|os.O_CREATE` (`journal.go:242`), continues `seq`
    from existing lines, and assumes a single writer under the detached lifetime lock
    (`journal.go:220-260`). A foreground run holds no such lock.
  - `diffFingerprint` (`pkg/git/external.go:295`) hashes tracked diffs plus untracked file content, and
    review stalemate detection compares it between rounds, so a journal written into a non-ignored
    path of the checkout would read as a code change. The git package has no ignored-path helper.
  - Claude: `streamEvent` (`pkg/executor/executor.go:396-422`) decodes no `usage`, `total_cost_usd`,
    or `modelUsage`; the `result` record is handled at `:705` and `:831`. `parseStream` builds its
    `Result` on two return paths (`:685`, `:689`), and a runner start failure returns
    `Result{Error: err}` (`:510`). `resolveRunResult` (`:537-576`) changes the result in place, so it
    keeps a new field. `Result` (`:28-35`) has no usage field. The executor's `Model` (`:432`) is the
    configured override and may be an alias such as `opus`. The fixtures were stripped of usage on
    purpose (`pkg/executor/testdata/claude/README.md`).
  - Codex: invoked as `codex exec` without `--json` (`pkg/executor/codex.go:211-264`); usage is only in
    the rollout file. `parseRolloutRecord` (`:1193-1203`) keeps `response_item` and `event_msg` and
    drops `turn_context`; `processRolloutLine` (`:1205`) handles only `task_complete` among
    `event_msg` records. The only mutex is `callbackMu` (`:120`), which serializes the output and
    timing handlers; the per-file rollout state is local to the tail goroutine, `startRolloutTail`
    (`:758`) returns only a cancel function and a done channel, and `Run` waits on `<-tailDone`
    (`:330`) before it builds the result. One executor serves several `Run` calls.
  - A rollout written by the installed codex was inspected during planning: an `event_msg` record with
    `payload.type = "token_count"` carries `payload.info.total_token_usage.{input_tokens,
    cached_input_tokens, cache_write_input_tokens, output_tokens, reasoning_output_tokens,
    total_tokens}`, and a `turn_context` record carries `payload.model`. In that rollout
    `total_tokens == input_tokens + output_tokens` and `cached_input_tokens <= input_tokens`, so
    Codex's `input_tokens` includes the cached tokens. Codex reports no cost.
  - The limit retry loop is in `retryPolicy.Run` (`pkg/processor/execution_policy.go:41-46`), which
    calls `runWithSessionTimeout` once per session. On a session timeout `runWithSessionTimeout`
    clears the error and keeps the rest of the result (`:227-229`).
  - The test fake provider prints `{"type":"result","result":""}`
    (`cmd/loopai/detach_child_unix_test.go:73`) and the fixture sets `report_enabled = false` (`:146`).
    Its command builder passes `--detached-run` and the handshake files, and the fixture writes a
    descriptor.
  - A new visible flag changes the `TestSkillContract` surface
    (`cmd/loopai/skill_contract.go:70-131`, `cmd/loopai/testdata/skill-contract.golden`). No skill
    depends on `--journal-dir`, so CLAUDE.md's rule applies: run the test with `-update` without
    bumping `skillContract`.
  - The flag must also join the execution-flag lists in `markFlagsSet` (`main.go:226-239`) and
    `hasExecutionMode` (`main.go:3932-3953`), or `--merge`, `--pr`, `--report`, `--runs`, `--attach`,
    `--stop`, and `--clear` will not reject it. `stripDetachArgs` (`cmd/loopai/detach_launch.go:166`)
    forwards unknown flags to the detached child, so `--detach --journal-dir` needs an explicit
    refusal. `validateFlags` runs at `main.go:544`, before `prepareRunBeforeConfig` (`:895`), which
    decides the cmux hand-off through `ensureAutoWorkspaceReservation` (`:4885`).
  - Fork policy: the module path `github.com/umputun/ralphex` and the `<<<RALPHEX:...>>>` signals stay;
    no release infrastructure is added.
- **Not verified**: whether Claude's `total_cost_usd` includes subagent sessions, and whether a parent
  codex rollout's `token_count` includes its child (subagent) rollouts. The plan reports the main
  session's numbers and the documentation says so.

## Context (from discovery)
- Files/components involved:
  - `pkg/runs/journal.go`, `pkg/runs/observer.go`: event type, payload, exclusive creation, readers.
  - `pkg/executor/executor.go`, `pkg/executor/codex.go`: usage extraction, `Result`.
  - `pkg/processor/execution_policy.go`, `pkg/processor/runner.go`,
    `pkg/processor/executor_factory.go`: the session choke point and handler wiring.
  - `pkg/git`: an ignored-path check for the journal directory.
  - `cmd/loopai/main.go`, `cmd/loopai/detach_journal.go`, `cmd/loopai/detach_child.go`,
    `cmd/loopai/detach_launch.go`: flag, validation, request wiring, shared completion.
  - `cmd/loopai/t3_bridge.go`, `cmd/loopai/acp_render.go`: readers that must keep ignoring the new type.
  - `README.md`, `llms.txt`, `CLAUDE.md`, `docs/t3-code.md`, a new `docs/host-integration.md`.
- Related patterns found:
  - Conflict tables in `validateDetachFlags` (`detach_launch.go:73-128`), `validatePlanChain`
    (`main.go:3772-3785`), `validateGenAgentsFlags` (`main.go:3831-3853`); `validateFlags`
    (`main.go:3646-3691`) returns the first error.
  - `CommandTimingHandler` wiring from `executePlan` through `createRunner` to the executors
    (`main.go:1885`, `:4065`; `pkg/processor/executor_factory.go:97`, `:282`, `:334`).
  - Tests: `detach_journal_test.go` (real `runs.Journal` in `t.TempDir()`, `LoggerMock`, the
    `eventTypes` helper in `detach_child_test.go:258`), `detach_child_test.go` (`completingChild`
    fixture), `detach_child_unix_test.go` (re-executes the test binary through
    `TestDetachChildHelper` with a shell fake `claude` and a real git repository, `awaitSession` at
    `:176`).
- Dependencies identified: none new. `e2e/` holds dashboard tests only and is unaffected.

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
- Maintain backward compatibility: a run without `--journal-dir` writes no new files and exits with
  the same codes
- Follow CLAUDE.md: lowercase comments except exported godoc, `fmt.Errorf("context: %w", err)`,
  table-driven `testify` tests, `t.TempDir()`, one `_test.go` per source file, mocks generated with
  `moq` and never hand-edited, `filepath` for paths, no new dependencies, never edit `CHANGELOG.md`

## Validation Commands
- `make test`
- `make lint`
- `GOOS=windows GOARCH=amd64 go build ./...`

## Testing Strategy
- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: the project's `e2e/` suite covers the web dashboard only; this plan adds no UI and
  changes no dashboard behaviour, so no e2e test is added

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): tasks achievable within this codebase - code changes,
  tests, documentation updates
- **Post-Completion** (no checkboxes): items requiring external action - the bridex plan, manual runs
  against real providers
- **Checkbox placement**: Checkboxes belong only in Task sections. Do not put checkboxes in Success
  criteria, Overview, Context, Decisions, or Decision Log.

## Decision Log
- 2026-11-15 loopai-brainstorm-v2: **accepted** - `--journal-dir <dir>` makes a foreground run write
  `events.jsonl` and `outcome.json` in the detached schema (journal hooks are separate from the
  detached process: `cmd/loopai/detach_journal.go:19`; schemas `pkg/runs/journal.go:74`,
  `pkg/runs/outcome.go:32`; `--detach` refuses a sandbox: `cmd/loopai/detach_launch.go:201`)
- 2026-11-15 loopai-brainstorm-v2: **accepted** - the foreground run is not registered for `--runs`,
  `--attach`, or `--stop` and writes no `run.json` or `run.lock` (`--attach` only watches a run living
  in a process of its own: `cmd/loopai/main.go:509`)
- 2026-11-15 loopai-brainstorm-v2: **accepted** - the flag covers one plan, full or `--tasks-only`, and
  is refused with plan chains, review-only modes, `--plan`, `--serve`, `--watch`, `--gen-agents`,
  `--detach`, and standalone commands (`--detach` has the same restrictions: `cmd/loopai/main.go:101`,
  `cmd/loopai/detach_launch.go:73-128`)
- 2026-11-15 loopai-brainstorm-v2: **accepted** - outcome status and record come from the same code as
  a detached run; the foreground process's own exit code is unchanged
  (`cmd/loopai/detach_child.go:289`, `:381`)
- 2026-11-15 loopai-brainstorm-v2: **accepted** - usage comes from the Claude stream-json `result`
  record and from the Codex rollout file; Codex cost is absent
  (`pkg/executor/executor.go:705`, `:831`; `pkg/executor/codex.go:752`)
- 2026-11-15 loopai-plan: **accepted** - a directory that already holds `events.jsonl` or
  `outcome.json` is refused; this replaces the design's "continue with the next attempt", because the
  journal assumes a single writer under the detached lifetime lock, which a foreground run does not
  hold, and a second run would overwrite `outcome.json` (`pkg/runs/journal.go:220-260`)
- 2026-11-15 loopai-plan: **accepted** - a `--journal-dir` run is non-interactive, built through
  `prepareNonInteractiveRequest`, so a host-spawned run never prompts
  (`cmd/loopai/main.go:805-844`)
- 2026-11-15 loopai-plan: **accepted** - usage reaches the journal through `executor.Result.Usage` and
  a `UsageHandler` called in `retryPolicy.runWithSessionTimeout`, mirroring `CommandTimingHandler`
  (`pkg/processor/execution_policy.go:207-233`, `cmd/loopai/main.go:4065`)
- 2026-11-15 loopai-plan: **accepted** - `TestSkillContract` golden is regenerated with `-update` and
  `skillContract` is not bumped, because no skill depends on the new flag (CLAUDE.md, the paragraph on
  `TestSkillContract`)
- 2026-11-15 grill: **accepted** - `--journal-dir` is refused with `--worktree`; the run executes in
  the current checkout only (Tasks 6, 8, Technical Details, Overview)
- 2026-11-15 grill: **accepted** - the request is built from `run()`'s own request with `Outcome` and
  `NonInteractive` set, not through `prepareNonInteractiveRequest`; the run stays non-interactive, so
  only the mechanism of the 2026-11-15 loopai-plan entry changes (Task 8) - decided by grill:
  `prepareNonInteractiveRequest` creates its own `WtCleanup`, sets no `KeepAwake`, records no launch
  history, re-resolves dependencies, and moves output to stderr (`cmd/loopai/main.go:809`, `:820`,
  `:836`; `run()` at `:521`, `:587`, `:616`)
- 2026-11-15 grill: **accepted** - the in-place `attempt_start` fires inside `executePlan` right after
  `setupProgressLogger`, through a request hook of its own that `worktreeExecuteRequest` does not copy
  (Task 8) - decided by grill: the progress log path is known only there (`cmd/loopai/main.go:1772`)
  and `AttemptStarted` already fires for worktree runs (`:2266`, `:2371`)
- 2026-11-15 grill: **accepted** - no completion on a forced exit: the cleanup-holder path and the
  call-once guard are removed, and a forced exit leaves no `finish` event or `outcome.json` (Tasks 5,
  8, 9, 11) - decided by grill: a detached run completes only on the normal return
  (`cmd/loopai/detach_child.go:136-143`), the forced exit allows two seconds
  (`cmd/loopai/main.go:6621-6624`), and `planExecutionOutcome` has no mutex
- 2026-11-15 grill: **accepted** - the shared completion is split into building the outcome record
  with the closing events and persisting it, with a before-save step for the caller, and returns the
  record and an error instead of an `exitCodeError` (Task 5) - decided by grill: `keptWorktree` and
  `reconcileDetachedSourcePlan` run between the closing events and `SaveOutcome`
  (`cmd/loopai/detach_child.go:321-331`)
- 2026-11-15 grill: **accepted** - `report.md` is not written for a foreground run (Overview, Task 5,
  Task 11) - decided by grill: the accepted design names two files, `outcome.json` already carries the
  report (`pkg/runs/outcome.go:42`), and the only readers of `report.md` look inside a registered run
  directory (`cmd/loopai/acp_render.go:385`, `cmd/loopai/runs_cmd.go:303`)
- 2026-11-15 grill: **accepted** - a foreground outcome has an empty `run_id` (Task 8, Technical
  Details) - decided by grill: `runs.NewID` names run directories (`pkg/runs/id.go:31`, `:37`) and
  `LoadOutcome` does not check the id (`pkg/runs/outcome.go:70-85`)
- 2026-11-15 grill: **accepted** - Codex usage is collected in an accumulator local to one `Run` call,
  passed through the rollout tail, written for the main rollout only, and read after `<-tailDone`
  (Task 3) - decided by grill: there is no rollout mutex to reuse and one executor serves several
  `Run` calls (`pkg/executor/codex.go:120`, `:330`, `:758`, `:1205`)
- 2026-11-15 grill: **accepted** - Claude usage is set on both `parseStream` return paths, and the
  `resolveRunResult` checkbox became tests (Task 2) - decided by grill:
  `pkg/executor/executor.go:685-690`, `:537-576`
- 2026-11-15 grill: **accepted** - `input_tokens` excludes cached tokens for both providers; Codex
  counts are normalized by subtracting `cached_input_tokens` (Tasks 1, 3, 11) - decided by grill: in a
  real rollout `total_tokens == input_tokens + output_tokens` with `cached_input_tokens` inside
  `input_tokens`
- 2026-11-15 grill: **accepted** - usage is reported whenever the result carries it, also after a
  session timeout, and the limit-retry test drives `retryPolicy.Run` (Task 4, Technical Details) -
  decided by grill: `pkg/processor/execution_policy.go:41-46`, `:227-229`
- 2026-11-15 grill: **accepted** - `events.jsonl` is created exclusively for a foreground run (Task
  7) - decided by grill: `OpenJournal` opens without `O_EXCL`, so two processes could pass an
  existence check (`pkg/runs/journal.go:242`)
- 2026-11-15 grill: **accepted** - a journal directory inside the checkout is refused unless git
  ignores it (Task 7) - decided by grill: `diffFingerprint` includes untracked file content, so
  journal writes would defeat review stalemate detection (`pkg/git/external.go:295`)
- 2026-11-15 grill: **accepted** - the directory preparation is a function of its own called after
  `validateFlags`, the uncreatable case uses a regular file, and the cmux hand-off skip has a test
  (Tasks 6, 7) - decided by grill: `cmd/loopai/main.go:544`, `:895`, `:4885`
- 2026-11-15 grill: **accepted** - the former Task 7 is split into wiring with in-process tests (Task
  8) and subprocess tests with a foreground fixture (Task 9) - decided by grill: the re-exec fixture
  is detach-specific (`cmd/loopai/detach_child_unix_test.go:154-162`, `:291-302`)
- 2026-11-15 grill: **accepted** - the fake provider emits usage on request, reports are enabled in
  the journal tests, and the event assertion is a literal expected list (Task 9) - decided by grill:
  `cmd/loopai/detach_child_unix_test.go:73`, `:146`
- 2026-11-15 grill: **accepted** - "prints the same output" and the manual checks of the acceptance
  task became automatable assertions (Tasks 9, 10) - decided by grill: the repository has no baseline
  output for a foreground run

## Implementation Steps

### Task 1: Add the `usage` event type to the journal
- [x] add `EventUsage EventType = "usage"` with its doc comment to the constants in
  `pkg/runs/journal.go`, listing the fields it carries and stating that `input_tokens` excludes
  cached tokens
- [x] add the fields to `Event`: `Provider`, `Model`, `InputTokens`, `CachedInputTokens`,
  `CacheWriteInputTokens`, `OutputTokens`, and `CostUSD *float64` (nil when the CLI reported no cost);
  reuse the existing `Phase` field
- [x] add a `case EventUsage` to `Event.payload()` writing `provider`, `model`, `phase`,
  `input_tokens`, `cached_input_tokens`, `cache_write_input_tokens`, `output_tokens`, and `cost_usd`
  only when it is not nil
- [x] write tests in `pkg/runs/journal_test.go`: a `usage` event round-trips through `Append` and
  `parseEvents` with and without a cost, and an event with zero cache counts
- [x] write tests in `pkg/runs/observer_test.go`: `Snapshot.Apply` and `Fold` advance `Seq` on a
  `usage` event and change nothing else; `FollowUntilFinish` does not end on it
- [x] write tests in `cmd/loopai/t3_bridge_test.go` and `cmd/loopai/acp_render_test.go`: a `usage`
  event between two known events changes no title and sends no update
- [x] run `make test` - must pass before task 2

### Task 2: Extract usage from a Claude session
- [x] add an exported `Usage` struct to `pkg/executor` (`Model`, `InputTokens`, `CachedInputTokens`,
  `CacheWriteInputTokens`, `OutputTokens`, `CostUSD *float64`) and a `Usage *Usage` field to `Result`
- [x] extend `streamEvent` in `pkg/executor/executor.go` to decode the `result` record's `usage`
  (`input_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `output_tokens`),
  `total_cost_usd`, and `modelUsage` keys
- [x] collect the usage in a variable inside `parseStream`'s line callback when a `result` record
  carries a `usage` object, and set it on both return paths (the stream-read-error return and the
  success return); a `result` record without `usage` leaves it nil
- [x] set `Model` to the single `modelUsage` key when there is exactly one, else the executor's
  configured model (which may be an alias such as `opus`), else empty
- [x] add a synthetic fixture with usage under `pkg/executor/testdata/claude/` and note it in that
  directory's README
- [x] write tests in `pkg/executor/executor_test.go`: usage with cost, usage with several `modelUsage`
  keys, a `result` record without usage, a stream that ends without a `result` record, a stream read
  error after the `result` record
- [x] write tests that `Usage` survives `resolveRunResult` for a pattern-error result and for a
  non-zero-exit result
- [x] run `make test` - must pass before task 3

### Task 3: Extract usage from a Codex session
- [x] extend `rolloutPayload` and `parseRolloutRecord` in `pkg/executor/codex.go` to keep the
  `token_count` `event_msg` record's `info.total_token_usage` and the `turn_context` record's `model`
- [x] create a usage accumulator in `Run` for that one call, pass it through `startRolloutTail` and
  `tailRolloutFile` to `processRolloutLine`, and write it only for the main rollout (`render` true);
  update the existing `processRolloutLine` test callers for the changed signature
- [x] read the accumulator in `Run` after `<-tailDone` and set `Usage` once on the result `runResult`
  returns: the last `token_count` totals with `InputTokens = input_tokens - cached_input_tokens`,
  `Model` from `turn_context` or else the configured model, `CostUSD` nil; no rollout file or no
  `token_count` record leaves it nil
- [x] add rollout fixtures to the codex testdata, synthetic but with the record shapes and the
  arithmetic of a real rollout (`total_tokens == input_tokens + output_tokens`)
- [x] write tests in `pkg/executor/codex_test.go`: several `token_count` records (the last wins), the
  cached-token subtraction, no `token_count` record, a missing rollout file, model from
  `turn_context` and from configuration
- [x] write tests for isolation: two consecutive `Run` calls on one executor where only the first
  reports usage, `token_count` records in a child rollout that must not be counted, and a
  `token_count` record that arrives in the final drain
- [x] run `make test` (it runs with `-race`) - must pass before task 4

### Task 4: Report each session's usage to a handler
- [x] add `UsageHandler func(SessionUsage)` to `processor.Config` in `pkg/processor/runner.go`, where
  `SessionUsage` carries the provider name, the phase, and the `executor.Usage`
- [x] call it in `retryPolicy.runWithSessionTimeout` (`pkg/processor/execution_policy.go`) after each
  `run` returns with a non-nil `Result.Usage`, whatever the result's error or timeout state, passing
  `toolName` and the phase from a nil-safe read of `phaseHolder`; a nil handler or nil usage does
  nothing
- [x] add a `UsageHandler` field to `executePlanRequest`, copy it in `worktreeExecuteRequest`, and pass
  it to `createRunner` the way `CommandTimingHandler` is passed
- [x] add `journalHooks.observeUsage` in `cmd/loopai/detach_journal.go` appending an `EventUsage`
  event; a failed append follows the existing warn-once rule
- [x] set the request's `UsageHandler` to `hooks.observeUsage` in `detachedChild.execute`
- [x] write tests in `pkg/processor/execution_policy_test.go` driving `policy.Run` with a short
  `waitOnLimit`: a `run` stub whose first call returns a limit error with usage and whose second
  returns usage yields two handler calls
- [x] write tests on `runWithSessionTimeout`: one call for a session with usage, none without it, one
  call when the stub returns usage after the session timeout fired, and the provider and a
  `PhaseReview` phase passed through
- [x] write tests in `cmd/loopai/detach_journal_test.go`: `observeUsage` writes the event with and
  without a cost and warns once when the append fails
- [x] run `make test` - must pass before task 5

### Task 5: Share the completion of an attempt between detached and foreground runs
- [ ] extract from `detachedChild.complete` (`cmd/loopai/detach_child.go`) a function that, given the
  journal, the hooks, the run id, the attempt, the branch, whether a stop was requested, the git
  service, the `planExecutionOutcome`, and the run error, classifies the run, builds the
  `runs.Outcome` record, and appends the closing `finalize`, `report`, and `finish` events
- [ ] extract a second function that stamps `FinishedAt` and saves `outcome.json`; both return the
  record and an error, never an `exitCodeError`
- [ ] keep in `detachedChild.complete`, in today's order: the stop marker read, `runs.WriteReport`,
  `keptWorktree` and `reconcileDetachedSourcePlan` between the two shared functions,
  `endProgressLog`, `beforeExit`, and the `exitCodeError` it returns
- [ ] confirm the existing `detach_child_test.go` and `detach_child_unix_test.go` pass unchanged
- [ ] write tests for the shared functions: succeeded, failed, stopped by a cancelled context, stopped
  by a stop request
- [ ] write a test that a note recorded by `reconcileDetachedSourcePlan` and the kept-worktree path
  still reach a detached run's `outcome.json`
- [ ] run `make test` - must pass before task 6

### Task 6: Add and validate the `--journal-dir` flag
- [ ] add `JournalDir string` with `long:"journal-dir"`, `value-name:"dir"`, and a description to
  `opts` in `cmd/loopai/main.go`
- [ ] add `journal-dir` to the execution-flag lists in `markFlagsSet` and `hasExecutionMode`
- [ ] add `validateJournalDirFlags` and call it from `validateFlags`: refuse the flag with
  `--worktree`, `--review`, `--external-only`, `--plan`, `--gen-agents`, `--serve`, `--watch`,
  `--detach`, `--cmux-workspace`, and every standalone command, with a plan chain, and without a plan
  file, using the `"%s cannot be combined with %s"` form of `validateDetachFlags`; it touches no file
- [ ] run `go test ./cmd/loopai -run TestSkillContract -update` and commit the changed
  `cmd/loopai/testdata/skill-contract.golden`; do not bump `skillContract`
- [ ] write tests in `cmd/loopai/main_test.go`: one case per incompatible flag, a chain, a missing
  plan, and that `--merge`, `--pr`, `--runs`, `--attach`, and `--stop` reject the flag
- [ ] run `make test` - must pass before task 7

### Task 7: Prepare the journal directory and keep the run in this process
- [ ] add an exclusive constructor to `pkg/runs` that creates `events.jsonl` with
  `O_CREATE|O_EXCL` and fails when the file exists; `OpenJournal` is unchanged
- [ ] add an ignored-path check to `pkg/git` (through `git check-ignore`) reporting whether a path
  inside the work tree is ignored
- [ ] add `prepareJournalDir(dir string) (string, error)` in `cmd/loopai`, called after
  `validateFlags` and before `prepareRunBeforeConfig`: it makes the path absolute, creates the
  directory with mode `0700` when missing, and refuses a path that is a regular file, a directory
  that holds `events.jsonl` or `outcome.json`, and a directory inside the repository work tree that
  git does not ignore
- [ ] make a run with `JournalDir` set ineligible for the automatic cmux hand-off, since a hand-off
  re-executes the command in another process
- [ ] write tests in `pkg/runs/journal_test.go`: the exclusive constructor fails on an existing file,
  and of two goroutines calling it for one directory exactly one succeeds
- [ ] write tests for the ignored-path check: an ignored path, a tracked path, an untracked
  non-ignored path
- [ ] write table tests for `prepareJournalDir`: a relative path made absolute, a directory created
  with `0700`, an existing `events.jsonl` refused, an existing `outcome.json` refused, a regular file
  refused, an in-tree non-ignored directory refused, an in-tree ignored directory accepted
- [ ] write a test that the auto-workspace eligibility is false when `JournalDir` is set
- [ ] run `make test` - must pass before task 8

### Task 8: Write the journal and the outcome on a foreground run
- [ ] in `run()`, when `JournalDir` is set, open the journal with the exclusive constructor (attempt
  1), create `journalHooks`, and set on the existing request `Outcome: &planExecutionOutcome{}`,
  `NonInteractive: true`, `LogDecorator`, `PhaseObserver`, and `UsageHandler`; keep `run()`'s own
  `WtCleanup`, `KeepAwake`, reporters holders, and launch-history call; do not call
  `prepareNonInteractiveRequest`
- [ ] add a request hook for the in-place attempt start, not copied by `worktreeExecuteRequest`, and
  call it in `executePlan` right after `setupProgressLogger` and before the runner is built; a hook
  error ends the run before any provider process starts
- [ ] make that hook append `attempt_start` through `attemptStartEvent` with `Plan` and
  `WorktreePlan` set to the absolute plan path, `Worktree` empty, `ConsoleOffset` 0, `ProgressLog`
  the absolute progress-log path, and `Branch` the checked-out branch
- [ ] after `runSelectedPlans` returns, complete the attempt with the two shared functions from task
  5: `run_id` empty, `attempt` 1, `worktree_kept` empty, a stop when the context was cancelled; then
  call `endProgressLog` and close the journal
- [ ] leave the process exit code as it is today on every path, including a stop (exit 1 while the
  outcome records `stopped` with `exit_code` 2); a failure to save the outcome is printed as a
  warning and does not change the exit code
- [ ] write in-process tests in `cmd/loopai/journal_dir_test.go` against a real `runs.Journal` in
  `t.TempDir()`: the in-place hook writes `attempt_start` with the fields above and task titles are
  then present in `task` events; a hook error is returned before the runner is built
- [ ] write a test that with `JournalDir` empty the request built in `run()` has nil `Outcome`,
  `LogDecorator`, `PhaseObserver`, `UsageHandler`, and in-place hook, and `NonInteractive` false
- [ ] write a test that a detached run still records exactly one `attempt_start`
- [ ] run `make test` - must pass before task 9

### Task 9: Cover the foreground journal with subprocess tests
- [ ] add a foreground fixture and command builder to `cmd/loopai/detach_child_unix_test.go` (or a
  sibling `journal_dir_unix_test.go`) that reuses `TestDetachChildHelper`, the fake `claude`, and the
  real git repository, passes no `--detached-run` and no handshake files, and writes no descriptor
- [ ] extend the fake `claude` so that, when an environment variable is set, its `result` line carries
  `usage`, `total_cost_usd`, and one `modelUsage` key
- [ ] write a success test for the fixture's one-task plan in full mode with `report_enabled = true`:
  compare `eventTypes(events)` with a literal expected list that includes `attempt_start`, `task`,
  `usage`, `report`, and `finish`, assert the `task` event's title and the `usage` event's tokens and
  cost, and assert `outcome.json` has status `succeeded`, an empty `run_id`, and the report
- [ ] write a failing-provider test: `outcome.json` has status `failed` with a reason and the process
  exits 1
- [ ] write a graceful-stop test: SIGTERM sent inside a live session (use `awaitSession`) yields a
  `finish` event and an `outcome.json` with status `stopped`, and the process exits 1
- [ ] write a refusal test: with a pre-existing `events.jsonl` in the directory the process exits 1
  with the error line and the fake provider's dump file does not exist
- [ ] write a no-flag test: the same plan without `--journal-dir` exits with the same code and leaves
  no `events.jsonl` or `outcome.json` anywhere under the repository or the test's temporary directory
- [ ] write a detached-run variant with the usage-emitting fake: its journal holds at least one
  `usage` event and the existing detached assertions still hold
- [ ] run `make test` - must pass before task 10

### Task 10: Verify acceptance criteria
- [ ] verify every requirement from Overview has a test from tasks 1-9: in-place journal and outcome
  for full and `--tasks-only`, `usage` events for Claude and Codex, refusals before a provider starts,
  no change without the flag
- [ ] run `make test`
- [ ] run `make lint` - all issues must be fixed
- [ ] run `GOOS=windows GOARCH=amd64 go build ./...`
- [ ] run `go test -race -coverprofile=c.out ./cmd/loopai ./pkg/runs ./pkg/executor ./pkg/processor
  ./pkg/git && go tool cover -func=c.out` and confirm the new functions are covered (80%+), then
  remove `c.out`

### Task 11: [Final] Update documentation
- [ ] add `docs/host-integration.md`: how a host spawns a foreground run with `--journal-dir`, the
  directory rules (empty, outside the checkout or git-ignored), the `events.jsonl` event types with
  their fields including `usage`, the `outcome.json` fields, which fields are empty for a foreground
  run (`run_id`, `worktree_kept`, `attempt_start.worktree`, `console_offset`), the token convention
  (`input_tokens` excludes cached tokens for both providers), the exit-code note for a stopped run,
  that a forced exit leaves no `finish` event or `outcome.json` and must be read with the non-zero
  exit as not completed, that the run is non-interactive, and the limits (main session usage only, no
  Codex cost, `model` possibly an alias or empty, no usage for a session killed before its result)
- [ ] update `README.md` and `llms.txt`: the flag in the options list and in the detached-runs
  paragraph, with a pointer to `docs/host-integration.md`
- [ ] update `CLAUDE.md` where it describes the journal, the event types, and the executors' `Result`
- [ ] update `docs/t3-code.md` "The run directory" to mention the `usage` event

*Note: loopai automatically moves completed plans to `docs/plans/completed/`*

## Technical Details
- **`usage` event** (one line of `events.jsonl`):
  `{"seq":N,"time":"...","attempt":1,"type":"usage","provider":"claude","model":"...","phase":"task",
  "input_tokens":N,"cached_input_tokens":N,"cache_write_input_tokens":N,"output_tokens":N,
  "cost_usd":0.42}`. `cost_usd` is omitted when the CLI reported none (Codex, wrappers).
- **Token convention**: `input_tokens` excludes cached tokens, so the billable input is
  `input_tokens + cached_input_tokens + cache_write_input_tokens` at their separate prices. Claude
  maps `cache_read_input_tokens` to `cached_input_tokens` and `cache_creation_input_tokens` to
  `cache_write_input_tokens`. Codex's `input_tokens` includes the cached ones, so loopai subtracts
  `cached_input_tokens`. The custom executor reports no usage.
- **Session**: one `run(ctx, prompt)` call in `retryPolicy.runWithSessionTimeout`. A limit retry is a
  new session. Usage is reported whenever the result carries it, also when the session then counts as
  timed out; a session killed before its result record (Claude) or without a `token_count` record
  (Codex) yields no event.
- **Foreground `attempt_start`**: `plan` and `worktree_plan` are the same absolute path; `worktree` is
  empty; `console_offset` is 0.
- **Foreground outcome**: `run_id` and `worktree_kept` are empty; `attempt` is always 1; `status`,
  `reason`, `exit_code`, `branch`, `commit`, `report`, `report_summary`, `risk`, `finalize`,
  `plan_moved`, `archive_incomplete`, and `notes` are filled as for a detached run; `version` stays
  `schemaVersion`.
- **Processing flow**: `validateFlags` → `prepareJournalDir` → config and repository as today →
  exclusive journal creation and hooks on the request → branch preparation → `executePlan`
  (`attempt_start` after the progress logger exists; sections and phase changes become events; each
  session's usage becomes a `usage` event) → shared completion (closing events, `outcome.json`) →
  exit with today's code.
- **Interruption**: SIGINT or SIGTERM cancels the context; when execution returns within the five
  seconds before the forced exit, the completion records `stopped`. A forced exit or a kill leaves
  the journal without a `finish` event and no `outcome.json`.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- Run a short real plan with `--journal-dir` against Claude and against Codex and compare the `usage`
  events with the providers' own usage output; confirm the Claude cost matches `total_cost_usd`.
- Check whether Claude's `total_cost_usd` and a parent Codex rollout's `token_count` include subagent
  sessions, and record the answer in `docs/host-integration.md`.
- Run the same plan inside a bwrap jail started with `--die-with-parent --unshare-pid` to confirm the
  foreground run and its journal behave there.

**External system updates**:
- The bridex plan (a `plan` run engine on the instance and on worker machines) is written against
  `docs/host-integration.md` and pins the loopai commit that contains this change in its Dockerfile.
  It must pass a journal directory outside the task's checkout.
