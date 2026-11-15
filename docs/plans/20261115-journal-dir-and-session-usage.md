# Journal directory and per-session usage for foreground runs

## Overview
- Add `--journal-dir <dir>`: an ordinary foreground run of one plan (full or `--tasks-only`) writes
  `events.jsonl`, `outcome.json`, and `report.md` into `<dir>` in the schema a detached run already
  uses, so a host process that spawned loopai can follow progress and read the result without parsing
  the progress log.
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
    foreground request and shares the outcome construction and status classification that live in
    `detachedChild.complete` (`cmd/loopai/detach_child.go:289`) and `classifyDetachedRun` (`:381`).
  - The run is owned by its host process: it is not registered for `--runs`, `--attach`, or `--stop`,
    and writes no `run.json`, `run.lock`, or `console.log`.
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
  - `prepareNonInteractiveRequest` (`main.go:805-844`) already builds a request with
    `Outcome: &planExecutionOutcome{}` and `NonInteractive: true`; its comment says the caller may add
    `LogDecorator` and `PhaseObserver`.
  - `AttemptStarted` fires only inside `runWithWorktree` (`main.go:2266`, `fireAttemptStarted` in
    `cmd/loopai/detach_worktree.go:28-44`). The in-place path (`prepareSelectedPlanBranch` at
    `main.go:1037`, then `executePlan` at `:1042`) has no attempt-start point.
  - `attempt_start.WorktreePlan` is load-bearing: `journalHooks.startAttempt` stores it
    (`detach_journal.go:49`) and `parsePlan` re-reads that file for task titles and the `tasks`
    statuses (`:182-200`). Empty means no titles and no `tasks` events. No loopai reader uses
    `Worktree`, `ProgressLog`, or `ConsoleOffset`.
  - A non-nil `Outcome` enables `capturePlanOutcome`, which runs `GitSvc.HeadHash()` and returns its
    error as a run failure (`main.go:1951-1953`). That is the only behaviour change of turning it on.
  - SIGINT and SIGTERM cancel the same context (`main.go:469`). `startInterruptWatcher`
    (`main.go:6614-6631`) force-exits with `os.Exit(1)` five seconds after the signal, running the
    cleanup holders first (bounded to two seconds) and skipping defers. A graceful stop returns the
    wrapped `ctx.Err()` and the process exits 1. `classifyDetachedRun` reads a cancelled context as
    `stopped` with exit code 2.
  - Every journal reader ignores an unknown event type: `runs.Snapshot.Apply`
    (`pkg/runs/observer.go:188-210`, `default: return`), `Follow` (`:32-70`), `classify.go:128`, the T3
    bridge (`cmd/loopai/t3_bridge.go:287-298`), the ACP renderer (`cmd/loopai/acp_render.go:104-122`),
    `--attach` and `--runs`. `parseEvents` (`pkg/runs/journal.go:362-378`) decodes into the flat
    `Event` struct and drops unknown keys.
  - `Event.payload()` (`journal.go:144-203`) writes only the header for a type it has no case for, so
    a new type needs a constant, a case, and fields on `Event`. The journal has no schema version;
    `schemaVersion` (`pkg/runs/descriptor.go:12`) covers the descriptor and the outcome only.
  - `OpenJournal` continues `seq` from existing lines and assumes a single writer under the detached
    lifetime lock (`journal.go:220-260`). A foreground run holds no such lock.
  - Claude: `streamEvent` (`pkg/executor/executor.go:396-422`) decodes no `usage`, `total_cost_usd`,
    or `modelUsage`; the `result` record is handled at `:705` and `:831`. `Result` (`:28-35`) has no
    usage field. The fixtures were stripped of usage on purpose (`pkg/executor/testdata/claude/README.md`).
  - Codex: invoked as `codex exec` without `--json` (`pkg/executor/codex.go:211-264`); usage is only in
    the rollout file. `parseRolloutRecord` (`:1193-1203`) keeps `response_item` and `event_msg` and
    drops `turn_context`; `processRolloutLine` (`:1205-1227`) handles only `task_complete` among
    `event_msg` records. A rollout written by the installed codex was inspected during planning: an
    `event_msg` record with `payload.type = "token_count"` carries
    `payload.info.total_token_usage.{input_tokens, cached_input_tokens, cache_write_input_tokens,
    output_tokens, reasoning_output_tokens, total_tokens}`, and a `turn_context` record carries
    `payload.model`. Codex reports no cost.
  - A new visible flag changes the `TestSkillContract` surface
    (`cmd/loopai/skill_contract.go:70-131`, `cmd/loopai/testdata/skill-contract.golden`). No skill
    depends on `--journal-dir`, so CLAUDE.md's rule applies: run the test with `-update` without
    bumping `skillContract`.
  - The flag must also join the execution-flag lists in `markFlagsSet` (`main.go:226-239`) and
    `hasExecutionMode` (`main.go:3932-3953`), or `--merge`, `--pr`, `--report`, `--runs`, `--attach`,
    `--stop`, and `--clear` will not reject it. `stripDetachArgs` (`cmd/loopai/detach_launch.go:166`)
    forwards unknown flags to the detached child, so `--detach --journal-dir` needs an explicit
    refusal.
  - Fork policy: the module path `github.com/umputun/ralphex` and the `<<<RALPHEX:...>>>` signals stay;
    no release infrastructure is added.
- **Not verified**: whether Claude's `total_cost_usd` includes subagent sessions, and whether a parent
  codex rollout's `token_count` includes its child (subagent) rollouts. The plan reports the main
  session's numbers and the documentation says so.

## Context (from discovery)
- Files/components involved:
  - `pkg/runs/journal.go`, `pkg/runs/observer.go`: event type, payload, readers.
  - `pkg/executor/executor.go`, `pkg/executor/codex.go`: usage extraction, `Result`.
  - `pkg/processor/execution_policy.go`, `pkg/processor/runner.go`,
    `pkg/processor/executor_factory.go`: the session choke point and handler wiring.
  - `cmd/loopai/main.go`, `cmd/loopai/detach_journal.go`, `cmd/loopai/detach_child.go`,
    `cmd/loopai/detach_worktree.go`, `cmd/loopai/detach_launch.go`: flag, validation, request wiring,
    shared completion.
  - `cmd/loopai/t3_bridge.go`, `cmd/loopai/acp_render.go`: readers that must keep ignoring the new type.
  - `README.md`, `llms.txt`, `CLAUDE.md`, `docs/t3-code.md`, a new `docs/host-integration.md`.
- Related patterns found:
  - Conflict tables in `validateDetachFlags` (`detach_launch.go:73-128`), `validatePlanChain`
    (`main.go:3772-3785`), `validateGenAgentsFlags` (`main.go:3831-3853`); `validateFlags`
    (`main.go:3646-3691`) returns the first error.
  - `CommandTimingHandler` wiring from `executePlan` through `createRunner` to the executors
    (`main.go:1885`, `:4065`; `pkg/processor/executor_factory.go:97`, `:282`, `:334`).
  - Tests: `detach_journal_test.go` (real `runs.Journal` in `t.TempDir()`, `LoggerMock`),
    `detach_child_test.go` (`completingChild` fixture), `detach_child_unix_test.go` (re-executes the
    test binary with a shell fake `claude` and a real git repository).
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
- Maintain backward compatibility: a run without `--journal-dir` writes no new files, prints the same
  output, and exits with the same codes
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

## Implementation Steps

### Task 1: Add the `usage` event type to the journal
- [ ] add `EventUsage EventType = "usage"` with its doc comment to the constants in
  `pkg/runs/journal.go`, listing the fields it carries
- [ ] add the fields to `Event`: `Provider`, `Model`, `InputTokens`, `CachedInputTokens`,
  `CacheWriteInputTokens`, `OutputTokens`, and `CostUSD *float64` (nil when the CLI reported no cost);
  reuse the existing `Phase` field
- [ ] add a `case EventUsage` to `Event.payload()` writing `provider`, `model`, `phase`,
  `input_tokens`, `cached_input_tokens`, `cache_write_input_tokens`, `output_tokens`, and `cost_usd`
  only when it is not nil
- [ ] write tests in `pkg/runs/journal_test.go`: a `usage` event round-trips through `Append` and
  `parseEvents` with and without a cost, and an event with zero cache counts
- [ ] write tests in `pkg/runs/observer_test.go`: `Snapshot.Apply` and `Fold` advance `Seq` on a
  `usage` event and change nothing else; `FollowUntilFinish` does not end on it
- [ ] write tests in `cmd/loopai/t3_bridge_test.go` and `cmd/loopai/acp_render_test.go`: a `usage`
  event between two known events changes no title and sends no update
- [ ] run `make test` - must pass before task 2

### Task 2: Extract usage from a Claude session
- [ ] add an exported `Usage` struct to `pkg/executor` (`Model`, `InputTokens`, `CachedInputTokens`,
  `CacheWriteInputTokens`, `OutputTokens`, `CostUSD *float64`) and a `Usage *Usage` field to `Result`
- [ ] extend `streamEvent` in `pkg/executor/executor.go` to decode the `result` record's `usage`
  (`input_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `output_tokens`),
  `total_cost_usd`, and `modelUsage` keys
- [ ] set `Result.Usage` in `parseStream` when a `result` record carries a `usage` object; `Model` is
  the single `modelUsage` key when there is exactly one, else the executor's configured model, else
  empty; a `result` record without `usage` (the wrapper scripts emit `{"type":"result","result":""}`)
  leaves it nil
- [ ] keep `resolveRunResult` from dropping `Usage` when it rewrites the error
- [ ] add a synthetic fixture with usage under `pkg/executor/testdata/claude/` and note it in that
  directory's README
- [ ] write tests in `pkg/executor/executor_test.go`: usage with cost, usage with several `modelUsage`
  keys, a `result` record without usage, a stream that ends without a `result` record
- [ ] run `make test` - must pass before task 3

### Task 3: Extract usage from a Codex session
- [ ] extend `rolloutPayload` and `parseRolloutRecord` in `pkg/executor/codex.go` to keep the
  `token_count` `event_msg` record's `info.total_token_usage` and the `turn_context` record's `model`
- [ ] in `processRolloutLine`, remember the latest `total_token_usage` and model of the main session
  rollout; child rollouts are not added
- [ ] set `Result.Usage` when the run ends: tokens from the last `token_count` record, `Model` from
  `turn_context` or else the configured model, `CostUSD` nil; no rollout file or no `token_count`
  record leaves it nil
- [ ] guard the new state with the mutex the rollout tail already uses, so the tail goroutine and the
  result construction do not race
- [ ] add synthetic rollout fixtures with `token_count` and `turn_context` records to the codex
  testdata
- [ ] write tests in `pkg/executor/codex_test.go`: several `token_count` records (the last wins), no
  `token_count` record, a missing rollout file, model taken from `turn_context` and from configuration
- [ ] run `make test` (it runs with `-race`) - must pass before task 4

### Task 4: Report each session's usage to a handler
- [ ] add `UsageHandler func(SessionUsage)` to `processor.Config` in `pkg/processor/runner.go`, where
  `SessionUsage` carries the provider name, the phase, and the `executor.Usage`
- [ ] call it in `retryPolicy.runWithSessionTimeout` (`pkg/processor/execution_policy.go`) after each
  `run` returns with a non-nil `Result.Usage`, passing `toolName` and `phaseHolder.Get()`; a nil
  handler or nil usage does nothing; a limit retry is a separate session and reports separately
- [ ] add a `UsageHandler` field to `executePlanRequest`, copy it in `worktreeExecuteRequest`, and pass
  it to `createRunner` the way `CommandTimingHandler` is passed
- [ ] add `journalHooks.observeUsage` in `cmd/loopai/detach_journal.go` appending an `EventUsage`
  event; a failed append follows the existing warn-once rule
- [ ] set the request's `UsageHandler` to `hooks.observeUsage` in `detachedChild.execute`
- [ ] write tests in `pkg/processor/execution_policy_test.go`: one call per session with usage, none
  for a session without it, two calls across a limit retry, the phase and provider passed through
- [ ] write tests in `cmd/loopai/detach_journal_test.go`: `observeUsage` writes the event with and
  without a cost and warns once when the append fails
- [ ] run `make test` - must pass before task 5

### Task 5: Share the completion of an attempt between detached and foreground runs
- [ ] extract from `detachedChild.complete` (`cmd/loopai/detach_child.go`) a function that, given the
  journal, the hooks, the run id, the attempt, the branch, whether a stop was requested, the git
  service, the `planExecutionOutcome`, and the run error, appends the closing `finalize`, `report`,
  and `finish` events, writes `report.md`, and saves `outcome.json`, returning the outcome record
- [ ] keep the detached-only steps in `detachedChild.complete`: the stop marker, `keptWorktree`,
  `reconcileDetachedSourcePlan`, and `beforeExit`
- [ ] make the shared function safe to call once: a second call is a no-op, so the foreground run can
  call it both at the normal end and from an interrupt cleanup holder
- [ ] confirm the existing `detach_child_test.go` and `detach_child_unix_test.go` pass unchanged
- [ ] write tests for the shared function: succeeded, failed, stopped by a cancelled context, stopped
  by a stop request, and the second-call no-op
- [ ] run `make test` - must pass before task 6

### Task 6: Add and validate the `--journal-dir` flag
- [ ] add `JournalDir string` with `long:"journal-dir"`, `value-name:"dir"`, and a description to
  `opts` in `cmd/loopai/main.go`
- [ ] add `journal-dir` to the execution-flag lists in `markFlagsSet` and `hasExecutionMode`
- [ ] add `validateJournalDirFlags` and call it from `validateFlags`: refuse the flag with `--review`,
  `--external-only`, `--plan`, `--gen-agents`, `--serve`, `--watch`, `--detach`, `--cmux-workspace`,
  and every standalone command, with a plan chain, and without a plan file, using the
  `"%s cannot be combined with %s"` form of `validateDetachFlags`
- [ ] resolve the directory to an absolute path before any worktree `chdir`, create it with mode
  `0700` when missing, and refuse it before any provider process starts when it cannot be created or
  written or when it already contains `events.jsonl` or `outcome.json`
- [ ] make a `--journal-dir` run skip the automatic cmux hand-off, since a hand-off would re-execute
  the command in another process
- [ ] run `go test ./cmd/loopai -run TestSkillContract -update` and commit the changed
  `cmd/loopai/testdata/skill-contract.golden`; do not bump `skillContract`
- [ ] write tests in `cmd/loopai/main_test.go`: one case per incompatible flag, a chain, a missing
  plan, an unwritable directory, a directory holding an earlier journal, and that `--merge`, `--pr`,
  `--runs`, `--attach`, and `--stop` reject the flag
- [ ] run `make test` - must pass before task 7

### Task 7: Write the journal and the outcome on a foreground run
- [ ] when `JournalDir` is set, build the request through `prepareNonInteractiveRequest`, open the
  journal with `runs.OpenJournal(dir, 1)`, create `journalHooks`, and set `LogDecorator`,
  `PhaseObserver`, `UsageHandler`, and `AttemptStarted` on the request
- [ ] fire the attempt start on the in-place path: after `prepareSelectedPlanBranch` and once the
  progress log path is known, append `attempt_start` with `Plan` and `WorktreePlan` set to the
  absolute plan path, `Worktree` empty, `ConsoleOffset` 0, `ProgressLog` the absolute progress-log
  path, and `Branch` the checked-out branch; a `--worktree` run keeps using `fireAttemptStarted`
- [ ] complete the attempt with the shared function from task 5 right after execution returns: `run_id`
  a fresh `runs.NewID` value, `attempt` 1, `worktree_kept` the kept worktree path of a `--worktree`
  run or empty
- [ ] register the completion as an interrupt cleanup holder, so a forced exit five seconds after
  SIGINT or SIGTERM still records a `finish` event and an `outcome.json` with status `stopped`
- [ ] leave the process exit code as it is today on every path, including a stop (exit 1 while the
  outcome records `stopped` with `exit_code` 2); close the journal before exit
- [ ] write tests in `cmd/loopai/journal_dir_test.go` with the fake-provider pattern of
  `detach_child_unix_test.go`: an in-place run yields `attempt_start`, `phase`, `task`, `tasks`,
  `review`, `usage`, `report`, and `finish` in the order a detached run of the same plan yields them,
  with task titles present; `outcome.json` for success and for a failing provider
- [ ] write tests for the stop path: SIGTERM during a session yields a `finish` event and an
  `outcome.json` with status `stopped`, and the process exits 1
- [ ] write a test that the same plan run without the flag creates no journal files and prints the same
  output
- [ ] run `make test` - must pass before task 8

### Task 8: Verify acceptance criteria
- [ ] verify `loopai --journal-dir <dir> <plan>` and `loopai --journal-dir <dir> --tasks-only <plan>`
  write `events.jsonl`, `outcome.json`, and, for a full run with reporting on, `report.md`
- [ ] verify a detached run's journal now carries `usage` events and that `--attach`, `--runs`, the T3
  bridge, and the ACP renderer behave as before
- [ ] verify every refusal from task 6 happens before a provider process starts
- [ ] run `make test`
- [ ] run `make lint` - all issues must be fixed
- [ ] run `GOOS=windows GOARCH=amd64 go build ./...`
- [ ] verify test coverage of the new code meets the project standard (80%+)

### Task 9: [Final] Update documentation
- [ ] add `docs/host-integration.md`: how a host spawns a foreground run with `--journal-dir`, the
  `events.jsonl` event types with their fields including `usage`, the `outcome.json` fields, which
  fields are empty or generated for a foreground run (`run_id`, `attempt`, `worktree_kept`,
  `attempt_start.worktree`, `console_offset`), the exit-code note for a stopped run, and the limits
  (main session usage only, no Codex cost, no usage for a session killed before its result)
- [ ] update `README.md` and `llms.txt`: the flag in the options list and in the detached-runs
  paragraph, with a pointer to `docs/host-integration.md`
- [ ] update `CLAUDE.md` where it describes the journal, the event types, and the executors' `Result`
- [ ] update `docs/t3-code.md` "The run directory" to mention the `usage` event

*Note: loopai automatically moves completed plans to `docs/plans/completed/`*

## Technical Details
- **`usage` event** (one line of `events.jsonl`):
  `{"seq":N,"time":"...","attempt":1,"type":"usage","provider":"claude","model":"...","phase":"task",
  "input_tokens":N,"cached_input_tokens":N,"cache_write_input_tokens":N,"output_tokens":N,
  "cost_usd":0.42}`. `cost_usd` is omitted when the CLI reported none (Codex, wrappers). Claude maps
  `cache_read_input_tokens` to `cached_input_tokens` and `cache_creation_input_tokens` to
  `cache_write_input_tokens`. The custom executor reports no usage.
- **Session**: one `run(ctx, prompt)` call in `retryPolicy.runWithSessionTimeout`. A limit retry is a
  new session. A session killed by a timeout or an interrupt has no result record and yields no event.
- **Foreground `attempt_start`**: `plan` and `worktree_plan` are the same absolute path for an
  in-place run; `worktree` is empty; `console_offset` is 0.
- **Foreground outcome**: `run_id` is generated per invocation and names nothing in the run registry;
  `attempt` is always 1; `status`, `reason`, `exit_code`, `branch`, `commit`, `report`,
  `report_summary`, `risk`, `finalize`, `plan_moved`, `archive_incomplete`, and `notes` are filled as
  for a detached run; `version` stays `schemaVersion`.
- **Processing flow**: `validateFlags` → resolve and check the directory → open the journal →
  `prepareNonInteractiveRequest` plus hooks → branch or worktree preparation → `attempt_start` →
  `executePlan` (sections and phase changes become events, each session's usage becomes a `usage`
  event) → shared completion (closing events, `report.md`, `outcome.json`) → exit with today's code.
- **Interruption**: the completion runs on the graceful path when execution returns; the cleanup
  holder covers the forced exit. Whichever runs first writes; the other is a no-op.

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
