# Detached runs with a T3 Code terminal window and a Working loopai thread

## Overview
- T3 Code 0.1.x (Orchestrator V2, orchestration protocol 2) broke every T3 path in loopai. The
  WebSocket handshake needs `?orchestrationProtocol=2`, the HTTP dispatch route is gone,
  `thread.meta.update` became `thread.metadata.update`, the thread shell lost the fields settlement
  used, the state database moved to `statev2.sqlite`, and ACP prompts arrive wrapped.
- A loopai run can last about 40 hours. That is longer than any agent turn, provider process, terminal
  shell, or T3 app session the run could live inside, and the old integration hosted the run inside
  exactly those.
- This plan replaces the old T3 and ACP integration instead of porting it:
  1. **Detached run core, usable without T3.** `loopai --detach [flags] <plan>` starts the run as an
     independent process with its own session, its own loopai worktree, and durable state under
     `.loopai/progress/runs/<run-id>/`. `--attach`, `--runs`, and `--stop` follow, list, and stop
     runs from any shell. Launching the same plan again resumes a run that did not succeed.
  2. **Mode 1, same-thread terminal.** `loopai --detach --t3-thread <id>` also opens a terminal tab
     in the caller's T3 thread that follows the run. It keeps a replaceable `[loopai: task 3/7]`
     title prefix and restores the follow command after a T3 restart. The thread's agent stays free
     for conversation.
  3. **Mode 2, new thread that shows Working.** `loopai --acp` becomes a provider for T3's
     `acpRegistry` driver, registered through the argument-free `loopai-acp` launcher. The thread
     shows Working for the whole run, along with the plan's tasks and the current phase. It ends with
     a bounded report summary. The user then continues by switching the thread's model to Claude or
     Codex.
  4. The `loopai-t3` skills (Claude and Codex) are rewritten around the two modes.
- Integration: the detached child reuses the existing non-interactive execution path
  (`prepareNonInteractiveRequest`, `executePlanRequest.NonInteractive`, `LogDecorator`,
  `PhaseObserver`) in worktree mode. One observer package reads the run's event journal. It is the
  only structured state source for the follow view, the Mode 1 title bridge, and the Mode 2 provider.

## Decisions
- **Context**: the approved design is `docs/superpowers/specs/2026-10-22-t3-detached-runs-design.md`.
  Its "Amendments from plan discovery" section overrides earlier sections, and its "Amendments from the
  T3 v0.1.1 source study" section overrides both.
  - Goals: survive about 40 hours, and survive every launcher, terminal, agent, provider, and T3
    restart. T3 is a window onto the run, not its host.
  - Anti-goals. Each of these is a failure even if the feature "works":
    - the run dies because a terminal closed, a turn ended, a provider exited, or T3 restarted;
    - a Mode 2 thread cannot be talked to after the run;
    - a Mode 2 thread shows or stops a run it did not start;
    - the T3 database grows by gigabytes;
    - a T3 token reaches a file, a log, the progress log, a transcript, or the environment of model
      processes;
    - Stop or a late failure loses committed or uncommitted work;
    - a detached run commits into the checkout the user is working in;
    - a progress title overwrites a name the user gave the thread.
- **Chosen approach** (decided with the user, plus calls made in the design):
  - Replace the previous integration and preserve none of its behaviour. There are two modes; the run
    is a detached process and T3 is only a window onto it.
  - A detached run takes exactly one plan, in full or tasks-only mode; chains are refused. `--detach`
    always implies worktree mode.
  - A detached run never commits into the invoking checkout. The plan archive and report land on the
    run's branch. The source plan copy is reconciled the way successful worktree chains do it: an
    unchanged untracked copy is removed, an unchanged tracked copy is restored to source HEAD, and
    concurrent edits are never overwritten.
  - A detached run keeps its worktree on every exit except success.
    - "Iteration cap" means the task phase's `max iterations (%d) reached without completion` error
      (`pkg/processor/phase/task.go:158`). It is a run failure, so the worktree is kept.
    - A review stalemate or review max-iterations ends only that review
      (`pkg/processor/phase/external_review.go:220`, `:241-242` return nil). The run continues through
      the post-review loop, whose `commitPrefix` commits leftover fixes on the branch, to finalize and
      success. Removing the worktree then loses nothing.
    - Builder call protecting the "late failure loses work" anti-goal: a successful detached run
      still keeps its worktree when the finalize close-out is incomplete, when the plan archive is
      incomplete (an `Unrestored` finalize lands here), or when the worktree is dirty or has a Git
      operation in progress. Examples are edits a blocked sync's `reset --keep` left behind, or a
      staged plan move after a hook-rejected archive commit. Removal is
      `git worktree remove --force` (`pkg/git/external.go:1802`), so these would otherwise be lost.
  - Run identity: a run id names one plan on one branch across attempts. Launching the same plan
    again while its latest run is not live and did not succeed resumes it: same id, new attempt, and
    the previous outcome kept as attempt history. A succeeded run is never resumed. There is no
    resume flag.
  - A resume keeps the run's recorded origin unless the relaunch passes `--t3-thread`,
    `--acp-thread`, or `--acp-session`. A Mode 2 run resumed from a plain shell therefore stays
    `t3-thread` and stays stoppable from its thread. A resume from a plain shell has no token, so it
    has no Mode 1 bridge.
  - Run state lives in the invoking checkout under `.loopai/progress/runs/<run-id>/`, not in a global
    registry. That path is already ignored and excluded from dirty checks.
  - `--merge` and `--pr` refuse while a detached run of that branch is live. Two independent checks
    apply:
    1. the runs directory of every checkout `progressRecordRoots` returns, because a run started in a
       linked worktree records there;
    2. a non-blocking probe of the feature worktree's run lock.
  - Mode 1 runs in its own loopai worktree. It re-attaches its terminal automatically after a T3
    restart, so the run holds a T3 token in memory for its whole life. The token is minted with a
    3-day lifetime and least-privilege scopes. It travels over a launcher-to-child pipe, never through
    the environment or a file. Mode 1 sends no completion message: the final title prefix and the
    terminal show the result. The terminal shows the run's full live output (the console log).
  - Mode 2 isolation and routing:
    - A thread only ever shows or stops its own run.
    - A message naming a plan always starts that plan's own run.
    - Without a binding, adoption is limited to T3's continuation text, and only to unattached live
      runs of origin `t3-thread` started from the same cwd.
  - Mode 2 Stop answers `cancelled` at once. It then stops the run gracefully after a short grace
    period, which lets a steered turn through. The worktree is kept and the run can be resumed with
    `resume`.
  - Mode 2 after the run: the provider only reports status. The final message is a summary of at most
    about 12 KB plus the report path.
  - Mode 2 registration uses the T3 settings UI and the argument-free `loopai-acp`. Never hand-edit
    `settings.json`: a schema error there silently resets T3's settings.
  - From a sandboxed agent shell, Mode 1 is refused and Mode 2 offered. `--detach` itself refuses to
    start inside a detected sandbox. Windows detach is refused, but the code builds there.
  - Both modes record the launch-history label `t3`. `loopai-t3` asks for the mode. `loopai-plan`
    keeps one T3 option that hands over to `loopai-t3`.
  - No extra keep-awake or closed-lid handling. The first iteration targets macOS; Linux uses a
    transient systemd user scope when available.
  - Builder choices fixed by this plan:
    - run id `<plan-stem>-<YYYYMMDD-HHMMSS>-<4 hex>`;
    - package `pkg/runs` for the registry, journal, and observer;
    - console log `console.log`;
    - Stop grace 5 s, held in one named constant;
    - liveness retitle every 20 minutes;
    - no token revoke at finish (the token expires);
    - retention keeps the newest 20 succeeded run directories;
    - sandbox detection from environment signals, plus the caller's T3 `runtimeMode` in the skills;
    - exit codes 0 succeeded, 1 failed or crashed, 2 stopped;
    - T3 thread ids are treated as opaque strings: they are validated against a shell-safe superset
      charset and hashed whenever they become part of a file name.
- **Rejected alternatives**:
  - Porting the protocol-1 integration: the `--t3` reporter, `--t3-launch`, the Grok-impersonating
    `loopai-acp`, ACP chat/merge/PR, settlement, PR linking, and the Codex current-thread resolver. It
    hosts the run inside processes that die, and the old `pkg/t3` speaks the wrong protocol.
  - Hosting the run inside an agent turn, a T3 provider process, or a T3 terminal shell. Each dies
    before a 40-hour run ends.
  - A Mode 2 provider that accepts only plans, or an empty thread with only a terminal. Earlier
    attempts failed exactly there: one had no conversation, the other no visible Working.
  - Chat, merge, PR, or parallel conversation inside the loopai provider. Also rejected: T3 merge
    settlement, PR linking, creating T3 projects, plan chains in detached runs, a global run registry,
    a separate resume flag, and hand-editing `settings.json`.
  - Deriving state by parsing the human progress log. It is archived and truncated when a plan runs
    again, and it lacks the banner, stats, finalize, and PR lines.
  - Keeping the worktree after a review stalemate on an otherwise successful run. The post-review
    loop commits leftover fixes on the branch, so this would be a design change; it is not part of
    this plan.
- **Verified facts** (T3 Code 0.1.1 server bundle, re-verified, not exercised live; `B:` = bundle line;
  items the v0.1.1 source study corrected are stated in their corrected form):
  - **Transport**
    - `/ws` requires `?orchestrationProtocol=2` before authentication (426 otherwise).
      `Authorization: Bearer` on the upgrade works.
    - Commands go only through WS Effect-RPC `orchestration.dispatchCommand`. HTTP orchestration read
      endpoints require the header `x-t3-orchestration-protocol: 2`.
    - Every WS Request carries `headers: []`. Stream RPCs need a client `Ack` after every `Chunk`, or
      the stream stalls after its snapshot.
    - Each logical command gets a fresh `commandId`, which is an idempotency key.
  - **ACP turns**
    - `session/prompt` is awaited with no timeout.
    - There is no turn-duration limit and no no-progress watchdog for `acpRegistry`.
    - A busy session is never idle-reaped; an idle provider is reaped 30 minutes after a turn.
    - Updates sent after the `session/prompt` response are dropped.
  - **Restart continuation**
    - On shutdown or crash, T3 marks the running turn cancelled. At the next start it dispatches
      `Continue where you left off.` (possibly after a leading restart note) to the same thread and
      provider instance.
    - It then spawns a fresh provider, calls `session/load` with the saved session id, then
      `session/prompt`.
    - A failed or slow `session/load` (90 s timeout) falls back: T3 starts a new process with
      `session/new` (a new session id) and prepends a `Context handoff (full_thread_summary):` block
      (up to 16k tokens) before `\n\nUser message:\nContinue where you left off.`. Without
      `loadSession` advertised, the continuation fails.
    - Continuation is skipped: after a user Stop; with a pending permission request; when the
      instance changed or is disabled; when `settings.json` fails to decode; and after more than 3
      continuations within 5 minutes.
    - It also needs a running provider turn, an active provider thread, a session not stopped or
      errored, and a thread not archived, settled, or snoozed. A provider that dies by itself while T3
      is up has its turn finalized `failed`, never continued.
    - The web Resume button after Stop (latest run `interrupted`) and usage-limit auto-resume send the
      same `Continue where you left off.`. T3 never sends a restart continuation after a Stop.
  - **Process model**
    - On macOS the `acpRegistry` provider leads its own process group (Effect's default `detached` on
      non-Windows).
    - T3 retires it by signalling the group with SIGTERM, then SIGKILL after 1 second, without
      `session/cancel`.
    - A `setsid` process whose stdio is not tied to the provider survives.
    - On Linux under a systemd user manager, providers live in a slice that is stopped at the next
      server start.
  - **Stop**
    - One `session/cancel` notification, then a 10-second wait for stopReason `cancelled`. The same
      process and session serve the next prompt.
    - Stop disables continuation for that run and records a `run_interrupt_request`.
    - Steering is interrupt plus restart (`supportsSteeringByInterruptRestart:true`,
      B:175125-175133). Messages typed during a turn are queued.
    - Steer, promote-to-steer, and MCP `t3_thread_send` (auto) send `session/cancel`, wait for the
      prompt to end, then send a new `session/prompt`, usually unwrapped. A steer records no
      `run_interrupt_request`. v0.1.2 previews also send `session/cancel` when T3's own event
      ingestion fails a run.
  - **`acpRegistry`**
    - Enabled by default, with no CLI probes.
    - A local instance is `commandPath` plus `commandArgs`, spawned with the thread's cwd. The add
      dialog seeds `commandArgs: []`; the settings form has an Arguments editor (one literal argument
      per row). The default instance id is `acpRegistry_local_acp`.
    - Readiness probes (`initialize` + `session/new`, cwd = server cwd, `mcpServers: []`) refresh
      about every minute while provider status is in demand. Only `ready` is cached (15 minutes);
      failures retry every minute.
    - Probe clientInfo names: `t3-code-provider-test`, `t3-code-session-manager`, and
      `t3-code-provider-auth` (`initialize` only, then EOF).
    - `initialize` must answer ACP v1 without an `info` key (its presence switches T3 to V2 methods)
      and with `loadSession: true`. `session/load` must succeed quickly for any id.
    - Plan entries require `priority`, and `tool_call` requires `title`. Use kind `other`/`think`,
      never `execute`.
    - Error code -32000, or error text matching
      `/\b(?:authenticat(?:e|ed|es|ing|ion)|credentials?|log(?:[\s-]+)?in)\b/iu` from
      `initialize`/`session/new`, marks the instance unauthenticated (B:173700-173709).
  - **Prompt envelope**
    - A prompt is wrapped in
      `<t3_code_instructions>…</t3_code_instructions>\n\n<user_request>\n…\n</user_request>`, with a
      separate `<runtime_info>` block, until the session's instruction state is recorded. That happens
      after any non-error prompt response (a cancelled one counts), so a steered prompt after a cancel
      usually arrives unwrapped.
    - Inside `user_request`, a handoff history or restart note may precede the last
      `\n\nUser message:\n`.
    - Text starting with `/` arrives unwrapped.
    - Neither block carries a thread id.
  - **Thread identity for Mode 2**
    - The thread-scoped MCP endpoint is streamable HTTP. The `t3-code` entry of
      `session/new`/`session/load` `mcpServers` is a stdio `acp-mcp-bridge` whose `env` array carries
      `T3_ACP_MCP_ENDPOINT` and `T3_ACP_MCP_AUTHORIZATION`; it is preferred over the spawn-time
      `T3_ACP_MCP_*` env. Never advertise `mcpCapabilities.acp`, which makes T3 send an entry with no
      URL.
    - It answers `tools/call t3_thread_list` with `currentThreadId` = the ACP thread
      (B:334265-334281).
    - The credential is per provider session; read it right after `session/new`/`session/load`. It
      is touched hourly during a running run, and issuing a new one revokes the old.
  - **Thread ids are not always UUIDs.** Subagent and provider-native threads have ids like
    `thread:provider:claudeAgent:native-thread:thread%3Aprovider%3A…%3Aagent%3A3` (about 330
    characters, containing `:` and `%`). `ThreadId` is a branded `TrimmedNonEmptyString` with no
    charset or length limit. This was observed live through `t3_thread_list` in this session.
  - **Storage growth**
    - `turn-item.updated` events are never compacted, and streaming text grows the database
      quadratically.
    - Every tool-call retitle or status change writes events, and every plan update writes three.
    - One message chunk is truncated at 65,536 UTF-16 code units.
    - Terminal history is capped at 5,000 lines or 8 MiB.
  - **Model switch handoff.** It carries whole history items within 16,000 bytes of JSON-escaped
    UTF-8, prioritising the last user and last assistant message. A final message above roughly 12 KB
    of ASCII (about half that in Cyrillic) is dropped from the handoff.
  - **Same-thread terminal**
    - No MCP tool opens or writes a thread terminal, so Mode 1 needs a bearer token and WS
      `terminal.open`/`terminal.write`.
    - Terminal sessions live only in server memory. After a restart the bridge's terminal is absent.
      `terminal.open` of an unknown id creates a fresh shell seeded from the persisted history. A UI
      re-attach is not guaranteed: it needs that client's localStorage and the thread being viewed.
    - Re-opening an existing terminal id with a different cwd, env, or worktree path resets its shell
      and history; with an identical launch context it only resizes.
    - `terminal.write` fails with `TerminalNotRunningError` while `starting`/`error` and drops data
      once `exited`.
    - The terminal shell has T3's `PATH`.
    - Titles have no compare-and-set, and any title write clears a pending `titleRegeneration`.
    - Background work in an ordinary thread never shows Working: a held Monitor shows a grey
      "Waiting", and a background Bash shows nothing and dies about 4.5 h after the last provider
      event. Mode 1 keeps its bearer-token bridge; no Monitor or scheduler follower is added.
  - **Skills**
    - `orchestrator_capabilities` returns `parentThreadId` (null outside T3), the caller's
      `runtimeMode` and `interactionMode`, and `providers[]` without any command path.
    - An `acpRegistry` instance's model id is `default` before its first probe (B:181241-181256).
    - `t3_thread_launch` requires `title`.
    - Claude names the tools `mcp__t3-code__*`, Codex names them `mcp__t3_code__*`.
    - Mode 1 is offered only when `runtimeMode` is `full-access`.
    - Launch-time sandbox signals: `SANDBOX_RUNTIME=1` (Claude Code), `CODEX_SANDBOX` (Codex, value
      `seatbelt` on macOS), and `CODEX_SANDBOX_NETWORK_DISABLED=1`.
  - **Token**
    - `t3 auth session issue` takes a repeatable `--scope` that replaces the default, over-broad set.
    - The mint uses `T3CODE_CLI_PATH` when set, else `<T3 home>/bin/t3`. The `t3` on `PATH` may be a
      stale 0.0.45 that writes into the frozen V1 database.
    - Threads idle for 3 days are auto-settled unless pinned. The pin protects in code, contrary to
      T3's user docs; `thread.auto-settle.set` also exists.
  - **Codebase**
    - The old integration is about 13.1k lines: `pkg/t3` (20 files), `pkg/acp` (6 files),
      `cmd/loopai-acp` (2 files), and `cmd/loopai` `acp*.go` and `t3_settle*.go`.
    - `runWithWorktree` force-removes a fresh worktree on every non-success exit.
    - A single-plan worktree run archives into the invoking checkout through `MainGitSvc`.
    - Source-plan capture and reconcile exist only for chains.
    - `startInterruptWatcher` force-exits 5 s after cancel.
    - `worktreeExecuteRequest` copies fields one by one.
    - Inherited `ExtraFiles` descriptors lack `FD_CLOEXEC`.
    - An ignored SIGHUP is inherited across exec.
    - `--merge`/`--pr` never check a run lock.
  - **Status of the design's "unverified" items**
    - Resolved by discovery from the bundle:
      - the model id is `default` before the first probe;
      - `<runtime_info>` and `<t3_code_instructions>` carry no thread id;
      - the continuation text form is `Continue where you left off.`, possibly after the restart
        note or, after a `session/load` fallback, a context handoff block;
      - the `acpRegistry` provider is killed SIGTERM then SIGKILL after 1 s (corrected by the
        source study: on macOS it leads its own process group, which T3 signals as a whole);
      - steering is interrupt plus restart (B:175125-175133);
      - `currentThreadId` from the provider's thread-scoped credential is the ACP thread.
    - Still open until a live check:
      - that the provider's MCP endpoint actually returns `currentThreadId` live;
      - that steering arrives as `session/cancel` followed by `session/prompt` live (the source
        study confirms it from source);
      - whether Working shows during `session/load`;
      - the live handoff budget;
      - token minting from a Codex sandbox;
      - Linux slice behaviour.
    - The Mode 2 tasks keep the open behaviour isolated so a live mismatch is a scoped change: thread
      id lookup with the session-id fallback, and the Stop grace in one constant.

## Context (from discovery)
- Files and components involved:
  - Removal:
    - `cmd/loopai/acp.go`, `acp_chat.go`, `acp_merge.go`, `acp_state.go`, `t3_settle.go` (and their
      tests);
    - `pkg/acp/server.go`, `pkg/acp/sink.go` (and tests); `pkg/acp/transport.go` stays;
    - `pkg/t3/*` (20 files);
    - `cmd/loopai-acp/main.go` (and its test): the `models` probe in Task 2, rewritten in Task 13;
    - `pkg/processor/phase/chat.go`, `pkg/processor/runner.go`, `prompt_builder.go`,
      `executor_factory.go`;
    - `pkg/executor/executor.go`, `codex.go`;
    - `pkg/status/status.go`;
    - `pkg/progress/progress.go`;
    - `pkg/config/config.go`, `pkg/config/prompts.go`, `values.go`, `defaults/config`,
      `defaults/prompts/acp_chat.txt`;
    - `cmd/loopai/main.go`, `launch_history.go`, `skill_contract.go`,
      `testdata/skill-contract.golden`.
  - New:
    - `pkg/runs/` (registry, locks, journal, observer);
    - `pkg/t3/` (protocol-2 client and MCP client, recreated after deletion);
    - `pkg/t3/t3test/` (importable fake protocol-2 server for tests in `pkg/t3` and `cmd/loopai`);
    - the `pkg/acp/` provider protocol on top of `transport.go`;
    - `cmd/loopai/detach*.go`, `cmd/loopai/runs_cmd*.go`, `cmd/loopai/t3_bridge.go`,
      `cmd/loopai/acp_provider*.go`, `cmd/loopai/acp_binding.go`, `cmd/loopai/acp_route.go`.
  - Skills:
    - `assets/claude/skills/loopai-t3/SKILL.md`;
    - `assets/codex/skills/loopai-t3/SKILL.md` plus `agents/openai.yaml`, with `references/` and
      `scripts/` deleted;
    - both `loopai-plan`, the Codex `loopai`, both `loopai-update`, and every `expected=` line in all
      19 skills;
    - `scripts/check-*.sh`, `scripts/check-loopai-t3-skill_test.sh`, `scripts/t3-*_test.py`
      (deleted), `Makefile`;
    - `.claude-plugin/plugin.json`, `.claude-plugin/marketplace.json`.
  - Docs: `docs/t3-code.md`, `README.md`, `llms.txt`, `CLAUDE.md`, `docs/backlog/*`.
- Related patterns found:
  - Removed-spellings policy: hidden `opts` fields (`hidden:"true"`, e.g. `Codex` main.go:74) plus
    `rejectRemovedFlags` (main.go:3661). Config keys go in `removedKeys` (pkg/config/values.go:981),
    which is checked on every layer, the embedded one included.
  - Non-interactive in-process driver: `prepareNonInteractiveRequest` (main.go:733). The deleted ACP
    driver at `acp.go:209-252` can be recovered from commit `0f3a570`, for reference only.
  - Re-exec of the test binary: `skill_contract_test.go:30-66`,
    `pkg/git/worktree_run_lock_test.go:90-140`.
  - Argv/env hand-off precedent: `cmuxHandOffArgv` (main.go:5233), `stripCmuxWorkspaceArg`
    (:5249, which handles only an optional-value flag), and `cmuxEnvOptions` (:5227) with
    `TestCmuxEnvOptionsCoversOptionTags`. Refusal helpers: `executableHandOffRefusal` (:5153),
    `planFileRefusal` (:5171).
  - Multi-checkout progress scanning: `progressRecordRoots` (main.go:5607) and `sameProgressRoot`.
  - Run lock: `(*git.Service).AcquireWorktreeRunLockContext` (pkg/git/worktree_run_lock.go:50,
    non-blocking, `ErrWorktreeBusy`); `git.Service.Worktrees()` returns `[]git.Worktree{Path, Branch}`.
  - Chain source-plan handling: `CapturePlanChainSourceState` and `ReconcilePlanChainSourceState`
    (pkg/git/service.go:2839/2890), `PlanSourceState` (:139).
  - Atomic writes: `writeFileAtomic`/`saveJSONState` (cmd/loopai/json_state.go). The flock helpers
    are unexported (`pkg/git/repository_lock_unix.go`, `pkg/progress/flock_unix.go`).
  - TestMain stubs for network constructors and `newAwakeHolder` (main_test.go:93-120).
  - Functions already at or near gocyclo 20 (.golangci.yml): `executePlan` (20),
    `prepareWorktreeRunContext` (20), `applyCLIOverrides` (20), `runReportCommand` (20),
    `runWithWorktree` (18).
- Dependencies identified: none new.
  - `github.com/gorilla/websocket` stays vendored and direct. Do not run `go mod tidy` in the removal
    task, because the new `pkg/t3` imports it again.
  - Random ids use `crypto/rand`; there is no uuid dependency.
  - `golang.org/x/sys/unix` (vendored) provides `SysctlKinfoProc` on darwin.

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
- Run tests after each change with `make test </dev/null`. The wrapper suites hang on an open stdin.
- Every task leaves `make test`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` green on
  its own. Respect the ordering traps called out in each task.
- Every task that adds, removes, or hides a flag or an embedded config key runs
  `go test ./cmd/loopai -run TestSkillContract -update`, as a deliberate, diff-visible acceptance. The
  `skillContract` bump, the 19 `expected=` lines, and the manifest bump happen once, in the skills
  task.
- Tests and fixtures:
  - Redirect `HOME` and `USERPROFILE` together to `t.TempDir()`.
  - Never touch `~/.config/loopai/` or `~/.config/ralphex/`.
  - Git fixtures use temp repos with `core.autocrlf=false` wherever file contents are asserted.
  - No test reaches a real T3 server.
- Subprocess helpers (re-exec of the test binary):
  - wait generously, since spawns take 300-500 ms under `-race`;
  - always stop and wait for every spawned child;
  - run under the same TestMain stubs.
- Platform-specific code:
  - Unix-only code (`setsid`, `flock`, `syscall.Kill`, fd close-on-exec, process start time,
    `syscall.Exec`) goes into `//go:build !windows` files.
  - Every symbol that untagged code calls from a `!windows` file has a same-signature definition in
    the matching `_windows.go` file, usually returning `errDetachUnsupported`.
- Lint: `unused` counts test files. An unexported function added in a task without a production caller
  needs a test that calls it in that same task. Otherwise, add the function in the task that first
  calls it.
- Code style:
  - new exported identifiers get godoc;
  - comments are lowercase otherwise;
  - errors are wrapped with context;
  - small interfaces live at the consumer;
  - one `_test.go` per source file.
- Keep `module github.com/umputun/ralphex`, Go import paths, and every `<<<RALPHEX:...>>>` signal
  unchanged. Only the two fork-added ACP chat signals are deleted.
- Do not edit `CHANGELOG.md` or `docs/plans/completed/*`.

## Testing Strategy
- **Unit tests**: required for every task (see Development Approach above). Key scenario families:
  - **Removal**
    - removed flags and keys fail with the replacement named;
    - `LOOPAI_T3=1` in the environment does not fail a run;
    - the embedded config carries no removed key.
  - **Registry**
    - classification: running, succeeded, failed, stopped, crashed, starting;
    - resume vs new id; stop marker;
    - a lock probe never blocks a run;
    - resolution by id, plan, or branch.
  - **Execution knobs**
    - the worktree is kept on failure, stop, and the task-phase iteration cap;
    - it is also kept on a success with a blocked finalize, an incomplete archive, or a dirty tree;
    - it is removed on a clean success;
    - the archive lands on the run branch with the invoking checkout's HEAD unchanged.
  - **Detach**
    - handshake ack and startup-failure relay;
    - fd 3/4 are not inherited by grandchildren;
    - env scrub (no `T3_ACP_MCP_*`, `LOOPAI_T3_TOKEN`, `CMUX_*`);
    - concurrent launches of one plan;
    - outcome written with the report text;
    - sandbox and Windows refusals;
    - the recorded argv strips both forms of the value flags;
    - a plain-shell resume keeps the recorded origin;
    - `--merge`/`--pr` refuse while a run of that branch is live, including a run recorded in another
      registered worktree.
  - **T3 client**: an importable fake protocol-2 server enforcing:
    - the query parameter (426 before auth);
    - the Bearer header (401 JSON);
    - `headers: []`;
    - ack-gated streaming.
  - **Mode 1 bridge**
    - the title prefix keeps a user rename;
    - pin only when unpinned;
    - a terminal absent after a server restart is re-opened once with the identical UI launch
      context and receives the follow command;
    - a `remove` during a live connection is never re-typed;
    - token rejection disables the bridge with one line and never affects the run;
    - a thread id containing `:` and `%` works.
  - **Mode 2 provider**
    - scripted JSON-RPC sessions replaying T3's exact `initialize` and prompt shapes;
    - envelope forms; continuation with and without the restart note;
    - probe-safe `session/new`; `session/load` for an unknown id;
    - cancel with and without a following prompt within the grace window;
    - SIGTERM and EOF never touch the run;
    - a plan message never adopts another run;
    - a thread never stops another thread's run;
    - the session-id fallback when MCP fails;
    - a hashed binding file name for long thread ids.
  - **Skills**
    - the rewritten `check-loopai-t3-skill_test.sh`, including the report and resume phrases for both
      modes;
    - the existing launch-history and contract-snippet suites;
    - removed-spellings fixtures.
- **E2E tests**: the Playwright dashboard suite is unaffected, since there is no dashboard change.
  Names under `.loopai/progress/runs/` deliberately avoid the `progress-*.txt` pattern the dashboard
  discovers.

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

## Validation Commands
- `make test </dev/null`
- `make lint`
- `GOOS=windows GOARCH=amd64 go build ./...`

## Decision Log
- 2026-10-22 pre-flight scan: **accepted** - C1: the `AttemptStarted` hook logs and ignores a failed ack write; only descriptor or journal errors fail it, so a launcher that timed out or died never kills a 40-hour run (Tasks 6 and 8).
- 2026-10-22 pre-flight scan: **accepted** - C2: a hook error keeps a resumed worktree (`keep = wt.resumed`), protecting the previous attempt's work (Task 4).
- 2026-10-22 pre-flight scan: **accepted** - C3: the keep-worktree decision is computed once, before removal, and memoized, so no phantom "preserved" path is recorded (Task 4).
- 2026-10-22 pre-flight scan: **accepted** - C4: `recordFinalize` sets `finalizeIncomplete` before the keep decision, and a clean-tree incomplete close-out keeps the worktree (Task 4).
- 2026-10-22 pre-flight scan: **accepted** - C5: the Mode 1 bridge ends on the journal `finish` event, not on `Follow`'s lock-based finish, and the child stops it (bounded) through a before-exit hook added in Task 7, so the final title and the unpin are sent (Tasks 7 and 11).
- 2026-10-22 pre-flight scan: **accepted** - C6: `journalHooks.progressLogger()` queues lines written before `LogDecorator` runs and flushes them when it does, because the bridge starts before `executePlan` (Task 11).
- 2026-10-22 pre-flight scan: **accepted** - C7: the source plan is reconciled before `outcome.json` is written, so reconcile warnings land in its `notes` (Task 7).
- 2026-10-22 pre-flight scan: **accepted** - C8: the docs-removal grep excludes `docs/plans` and `docs/superpowers`, which name the removed spellings as history (Task 18).
- 2026-10-22 pre-flight scan: **accepted** - C9: the docs-removal grep uses `LOOPAI_T3([^_[:alnum:]]|$)`, because `LOOPAI_T3\b` matches nothing under `git grep -E` here (Task 18).
- 2026-10-22 pre-flight scan: **accepted** - C10: the tautological descriptor-token test is dropped; Task 11 instead scans the run directory, console log, progress log, and child environment for the token (Tasks 3 and 11).
- 2026-10-22 pre-flight scan: **accepted** - C11: `pkg/runs` exports `WriteFileAtomic` and `cmd/loopai/json_state.go` delegates to it instead of keeping a verbatim copy (Task 3).
- 2026-10-22 pre-flight scan: **accepted** - C12: review stalemate and review max-iterations are not run failures and get no keep rule of their own; the dirty-tree and incomplete-success keep rules cover uncommitted work (Decisions, Task 4).
- 2026-10-22 pre-flight scan: **accepted** - C13: `errDetachUnsupported` lives in `detach_child_windows.go` from Task 6, so `unused` passes on darwin (Tasks 6, 8, and 9).
- 2026-10-22 pre-flight scan: **accepted** - C14: without a thread id, `--acp-thread` is omitted; when present it is validated with the `--t3-thread` charset. Stop matches the run through the binding key, the thread id when known, else the session id (Tasks 14 and 15).
- 2026-10-22 pre-flight scan: **accepted** - C15: the descriptor records `argv` and `env_options` on every launch, not only on resume, because Mode 2 relaunches every run from them (Task 8).
- 2026-10-22 pre-flight scan: **accepted** - C16: the launcher detects child exit through fd-4 EOF, since `Process.Release` precludes `Wait`, and retries without the systemd scope only on EOF with no handshake line (Task 8).
- 2026-10-22 pre-flight scan: **accepted** - C17: Task 2 adds a direct `validatePassThroughValues` table test, because the function has no caller until Task 14 (Task 2).
- 2026-10-22 pre-flight scan: **accepted** - C18: "never patch cfg" means `WorktreeEnabled`; setting `cfg.Orca = false` after load stays (Task 6).
- 2026-10-22 pre-flight scan: **accepted** - C19: a bound run in status `starting` counts as live for routing rule 1 and is attached (Task 14; Technical Details "Mode 2 routing").
- 2026-10-22 pre-flight scan: **accepted** - C20: the skills run `loopai --detach` with a Bash timeout of at least 180 s, above the launcher's 120 s ack wait (Task 16; Technical Details "Skills").
- 2026-10-22 pre-flight scan: **accepted** - SIGTERM or EOF during the Stop grace window abandons the pending stop, consistent with the spec's "steered or restarted" reasoning, since a T3 restart sends SIGTERM (Task 15; Technical Details "Mode 2 routing").
- 2026-10-22 Task 3 review: **accepted** - F1: `LatestFor`, the launcher's query under its own launch lock, skips that lock's tier, so a crashed run reads `crashed` and can be resumed. `List`, `DefaultRun`, and `LiveRunForBranch` keep the tier, so every in-lock launcher decision in Task 8 uses `LatestFor` (Tasks 3 and 8).
- 2026-10-22 Task 3 review: **accepted** - F2: `Classify` probes the launch lock before the lifetime lock, closing the window in which a starting run read `crashed`; result precedence is unchanged (Task 3).
- 2026-10-22 Task 3 review: **accepted** - F3: `launchLockPath` hashes the normalized plan path (`filepath.ToSlash(filepath.Clean(planRel))`), so every spelling of a plan shares one launch lock (Task 3).
- 2026-10-22 Task 3 review: **accepted** - F4: the flock helper retries on EINTR, so `Probe` never reports a false `held` (Task 3).
- 2026-10-22 Task 3 review: **accepted** - F5 (deferred to Task 8): `List` fails as a whole on one corrupt run directory; Task 8 names the run id in that error or lets plan- and branch-filtered queries skip unrelated broken runs (Task 8).
- 2026-10-22 Task 3 review: **accepted** - F6 (deferred to Task 8): Task 8 creates run directories with `os.Mkdir` and retries with a new id on `EEXIST`, never `MkdirAll` (Task 8).
- 2026-10-22 T3 source study: **accepted** - facts F1-F13 from the T3 Code v0.1.1 source replace bundle-derived facts in Decisions (Verified facts) and Technical Details, and are recorded in the spec's "Amendments from the T3 v0.1.1 source study".
- 2026-10-22 T3 source study: **accepted** - Task 10: `GetThreadProjection` also returns `titleRegeneration`; a 426 or a decode failure is a typed "protocol unsupported" error that disables the bridge, never a crash (Task 10; Technical Details "T3 WebSocket protocol 2", "Mode 1 bridge behaviour").
- 2026-10-22 T3 source study: **accepted** - Task 11: replaces "never re-open, wait for the UI". After a reconnect where the server restarted, if the bridge's terminal is absent and no `remove` was seen during a live connection, it calls `terminal.open` once with the identical UI launch context, waits until it runs (bounded retry on `TerminalNotRunningError`), then types the follow command. A `remove` seen while connected still means never recreate. The follow command is never `exec`'d and never ends in `; exit`. Before the first prefix write the bridge waits (about 2 minutes) for `titleRegeneration` to be null. The pin stays (Task 11; Technical Details "Mode 1 bridge behaviour"; Testing Strategy; Post-Completion).
- 2026-10-22 T3 source study: **accepted** - Task 12: tests cover a `user_request` carrying a ~16k-token `Context handoff (full_thread_summary):` block before `User message:\nContinue where you left off.`, and both wrapped and unwrapped steered prompts (Task 12).
- 2026-10-22 T3 source study: **accepted** - Task 13: probe detection includes clientInfo `t3-code-provider-auth`, whose initialize-only connection ends in EOF with no side effects or error output; `loopai --acp` never exits by itself during a held turn (handler panics recovered) and exits promptly on SIGTERM or EOF; the `loopai-acp` package doc states that the process group is signalled as a whole and `exec` keeps one stable process; the MCP thread id is the required binding key and the session id only a fallback (Task 13; Technical Details "ACP v1 surface", "Mode 2 routing").
- 2026-10-22 T3 source study: **accepted** - Task 14: routing rule 1 refined. A live bound run, `starting` included, is attached whatever the text says; one not live and not succeeded (crashed, stopped, or failed) is relaunched from its recorded argv on continuation text (restart or Resume button) or `resume`; a succeeded one gets the status reply. Tests cover a stopped run plus continuation text relaunching (Task 14; Technical Details "Mode 2 routing").
- 2026-10-22 T3 source study: **accepted** - Task 15: the Stop grace is measured from loopai's `cancelled` answer; a steered prompt naming a plan while the bound run is live re-attaches it; before stopping, a best-effort, isolated `t3_thread_read` (≤5 s, session credential) requires a recorded `run_interrupt_request`, and a failed read or unavailable item kind stops as before; every finished run ends with `end_turn`, never a JSON-RPC error (Task 15; Technical Details "Mode 2 routing").
- 2026-10-22 T3 source study: **accepted** - Task 16: the read-only `settings.json` lookup also accepts `loopai`/`loopai.exe` with `--acp` in `commandArgs`, never assumes the instance id, and tolerates unknown keys and value types; only `models[].id` is read; after an error or lost reply from `t3_thread_launch`, `t3_thread_list` is inspected before a retry; the Mode 1 launcher is a foreground Bash call with a timeout of at least 180 s; registration text names the absolute `loopai-acp` path (or `loopai` with argument `--acp`) and keeps the default instance id, which supersedes the recommended instance id `loopai` in Task 18 (Tasks 16 and 18; Technical Details "Skills").
- 2026-10-22 T3 source study: **accepted** - Task 18: the docs cover Resume-button support, the restart-continuation preconditions (the per-project switch, archived, settled, or snoozed threads, and typing `resume` after a provider killed while T3 runs), queued messages held after a restart, switching the model only after the run, the 1-minute probe, the process-group fact, pin versus auto-settle, and that background commands and Monitors do not give Working (Task 18).

## Implementation Steps

### Task 1: Remove the old ACP integration (keep the JSON-RPC transport)
- [x] move `versionBannerWriter` and `rawFlagRequested` from `cmd/loopai/acp.go:29-56` into `cmd/loopai/skill_contract.go`:
  - `versionBannerWriter` keeps only the `--skill-contract` → `io.Discard` branch and otherwise returns `os.Stdout`; the stderr branch for `--acp` returns in Task 13;
  - move `TestVersionBannerWriter` (acp_test.go:49) into `skill_contract_test.go`, keeping only the `--skill-contract` and plain-run cases;
  - drop `TestACPRequested`.
- [x] delete `cmd/loopai/acp.go`, `acp_chat.go`, `acp_merge.go`, `acp_state.go` and their four `_test.go` files:
  - also delete the test-only helpers that live only there: `revParse`, `lockedBuffer`, `containsPrefix`, `sigpipeHelperEnv`, `acpFixture`/`newACPFixture`/`acpTaskClaude`/`acpTwoTaskPlan`, `writeACPOverrides`;
  - keep `splitReportSections`/`reportRiskLevel` (pr_body.go) with their existing tests;
  - keep `prepareNonInteractiveRequest`, `errNonInteractivePlanRequired`, the nil-stdin `executionIO`, and every `planExecutionOutcome` field.
- [x] delete the ACP→T3 entanglements that would not compile:
  - `prepareT3MergedBranchSettlement` and `t3MergedDispatcher` (`cmd/loopai/t3_settle.go:48-101`), with `TestPreparedT3SettlementRechecksGit` (`t3_settle_test.go:84-118`, uses `setupACPMergeRepo`);
  - the `parseACPPrompt` asserts at `main_test.go:4400-4403` inside `TestRunT3LaunchCommand`;
  - keep the other three `t3_settle` tests for Task 2.
- [x] remove the `--acp` flag and its plumbing from `cmd/loopai/main.go`:
  - the `ACP` opts field (main.go:88), its routing in `run()` (:482-487), and the banner comment (:390);
  - `validateACPFlags` from `validateFlags`, and `o.ACP` from `isStandaloneCommand` (:5274);
  - the `{"--acp", o.ACP}` row in `validateT3ProgressThreadFlags` (:3844);
  - the `--acp` entries in `main_test.go:3733` and `:3822`;
  - update the `executePlanRequest` comment (:317-320) and the `launch_history.go` comment (:144-146).
- [x] delete `pkg/acp/server.go`, `pkg/acp/sink.go` and their tests. Keep `pkg/acp/transport.go` and `transport_test.go`, and rewrite the package doc at `transport.go:1-2` to describe a generic newline-delimited JSON-RPC 2.0 stdio connection.
- [x] delete the chat mode:
  - `pkg/processor/phase/chat.go` (and its test);
  - `ModeACPChat` and the chat request/result plumbing in `pkg/processor/runner.go` (:31, :155-163, :222-224, :235, :333-339, :380-389, :423, :450-456);
  - `ACPChatContext`/`ACPChatPrompt` (`pkg/processor/prompt_builder.go:126-158`);
  - `pkg/config/defaults/prompts/acp_chat.txt`;
  - in ONE edit so `pkg/config` compiles: `acpChatPromptFile`/`ACPChatPrompt` in `pkg/config/config.go` (:29, :257, :517) and `ACPChat` in `pkg/config/prompts.go` (:22 field, :77-80 loader);
  - the `acp-chat` case and suffix in `pkg/progress/progress.go` (:925, :942-943);
  - the `ModeACPChat` branches in `cmd/loopai/main.go` (:3173-3176, :3253, :4369, :4429-4433).
- [x] delete executor resume-session support; only the chat phase used it.
  - In `pkg/executor/executor.go`: `Result.SessionID`, `Result.Reply`, `resumeSessionKey`/`WithResumeSession`/`ResumeSession`, `ClaudeExecutor.NoResume`, the `--resume` argument code, `trackSession`, `streamEvent.SessionID`.
  - In `pkg/executor/codex.go`:
    - `NoResume`, `resumeID()`, and the `resume <id> -` arguments;
    - `rolloutStart`/`rolloutEnd`, together with the then always-zero `start` parameter of `startRolloutTail`/`tailRolloutFile` and its seek.
  - In `pkg/processor/executor_factory.go`: the `e.NoResume = ...` lines (:268, :342).
  - Keep `extractSessionID`, `sessionIDPattern`, `sessionIDCh`, and `findRolloutFile`: rollout tailing and the 60-second `task_complete` grace use them.
- [x] delete the two fork-added chat signals `ACPMerge`/`ACPPR` (`pkg/status/status.go:23-26`) and their `knownSignals` entries (`executor.go:935-937`). Delete `StripSignals` together with the unexported `signals` var (status.go:30-57) and `TestStripSignals`, or the `unused` linter fails.
- [x] remove the `acp_auto_merge` key in ONE edit:
  - delete the active `acp_auto_merge = true` block in `pkg/config/defaults/config:207-216`;
  - add `{name: "acp_auto_merge", replacement: "merge with loopai --merge or the loopai-merge skill"}` to `removedKeys` (`pkg/config/values.go:981`);
  - remove `ACPAutoMerge`/`ACPAutoMergeSet` from `config.go` (:193-194, :458-459) and from `values.go` (:63-64, :205, :496-506 and its doc comment, :715-725).
- [x] delete `docs/backlog/acp-chat-models-in-t3-picker.md`, which is obsolete. Reword the comment on `AcquireWorktreeCreationLock` (`pkg/git/service.go:235-237`) that cites ACP auto-merge. Leave the exported `MergeBranchTipContext`/`UntrackedFiles` helpers in place.
- [x] run `go test ./cmd/loopai -run TestSkillContract -update`. This drops `--acp` and `acp_auto_merge` from the golden; there is no contract bump.
- [x] update tests: delete the chat, resume, signal, and `acp_auto_merge` tests listed in the discovery:
  - `executor_test.go:49-50`, `:781-783`, `:2792-2924`;
  - `codex_test.go:151-152`, `:4271-4405`, and the `rolloutStart{}` args at `:3375`/`:3426`;
  - `executor_factory_test.go:1165-1200`; `runner_test.go:1441-1515`;
  - `prompt_builder_test.go:74-160`; `pkg/processor/prompts_test.go:2102`/`:2134`/`:2185`;
  - `phase_test.go:93-96`/`:103`; `task_test.go:1077-1082`;
  - `config_test.go:128-130`/`:170`/`:264`/`:1952-2016` and the `acp_auto_merge` wantKey;
  - `values_test.go:191`/`:373-400`;
  - `defaults_test.go` expectedPrompts; `pkg/config/prompts_test.go:916-946`;
  - `progress_test.go:1401`; `status_test.go:24-50`;
  - the `TestRunHeaderParams` chat subtest and the `TestDumpDefaults` prompt list.
- [x] write tests:
  - `acp_auto_merge = true` in a local config fails `config.Load` naming the replacement (extend `TestLoad_RemovedKeysRejected` and the values removed-key table);
  - `TestValuesLoader_EmbeddedDefaultsCarryNoRemovedKeys` stays green;
  - `versionBannerWriter` returns discard for `--skill-contract` and stdout for a plain run.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 2

### Task 2: Remove the old T3 integration
- [x] delete `pkg/t3` (all 20 files). Do not run `go mod tidy`/`go mod vendor`: gorilla/websocket stays as is for Task 10.
- [x] in `cmd/loopai-acp/main.go`, delete only the `models` probe case and the `pkg/t3` import so the launcher stays buildable. Delete `TestRunModelsHasNoLoginVerdict` and the `models` row of `TestRunProbeCommands`. The full rewrite comes with the provider in Task 13.
- [x] delete the rest of `cmd/loopai/t3_settle.go` and `t3_settle_test.go`, and both `--merge` settlement call sites (main.go:5696 `t3MergeRoots`, :5711 `settleT3Threads`).
- [x] delete from `cmd/loopai/main.go`:
  - the functions `newT3Reporter`, `startT3Reporter`, `t3TaskModel`, `finishT3Failure`, `t3ProgressThreadRequested`, `validateT3ProgressThreadFlags`, `validateT3LaunchFlags`, `t3LaunchArgs`, `t3Session`/`newT3Session`, `t3LaunchInstance`, `runT3LaunchCommand`, `newT3Dispatcher`, `linkT3PullRequest`;
  - their call sites in `validateFlags`, `runConfiguredStandaloneCommand` (and its comment, plus the run() :498 comment), `isStandaloneCommand` (`o.T3Launch != ""`), `applyReportingOverrides` (`cfg.T3 = ...`), `closeoutTarget.linkT3` (:5403, :6198), `runPRCommand` (:6030-6032), the `createPullRequest` doc (:6041), `openFinalizePR` (:7310-7312), and the `statusNote` comment (:7208);
  - keep `validatePassThroughValues`/`passThroughValue`, which Mode 2 plan-message parsing reuses. Their existing tests keep `unused` quiet; if none remain, add a direct table test here;
  - remove the `runtime` and `regexp` imports when their last use goes.
- [x] remove the T3 reporter plumbing:
  - `startRunReporters` returns `(*cmux.Reporter, *orca.Reporter)` without the `branch` parameter;
  - `executePlan` loses `threads` (:1768-1770, :1791, :1810, :1840, :1844, :1850, :1888, :1910, :1933, :1949, :1964);
  - `buildRunnerLogger` loses the `threads` parameter and its `WrapLogger` layer;
  - `installBreakSignal` and `makePauseHandler` lose `threads`; the latter returns `titles.WithInputWait(wait)`;
  - fix the test call sites (main_test.go:138, :160, :168, :1902, :14717, :15891, :10493-10529, :15985, :15992, :16020) and remove the `T3: true` literals at :15933 and :15983.
- [x] apply the removed-spellings policy to the four flags (main.go:84-87):
  - mark `--t3`, `--no-t3`, `--t3-progress-thread`, `--t3-launch` `hidden:"true"` with "removed; …" descriptions;
  - in the same edit, DROP `env:"LOOPAI_T3"` and remove `"LOOPAI_T3"` from `cmuxEnvOptions` (main.go:5227);
  - drop the `choice` tags on `--t3-launch` but keep `optional:"true" optional-value:"auto"`;
  - keep the `t3ProgressThreadSet` marker and add a `t3LaunchSet` marker in `markFlagsSet`;
  - extend `rejectRemovedFlags` with the messages in Technical Details;
  - delete the pre-version `validateT3ProgressThreadFlags` call in `main()` (:407-415) and fix its comment.
- [x] remove the `t3` key:
  - delete the commented block in `pkg/config/defaults/config:266-270`;
  - add `{name: "t3", replacement: "use loopai --detach --t3-thread <thread-id> or the loopai-t3 skill"}` to `removedKeys`;
  - remove `T3`/`T3Set` from `config.go` (:214-215, :469-470) and `values.go` (:77-78, :417-424, :677-680).
- [x] re-key launch history: drop the `cfg.T3` case from `launcherFor` (`launch_history.go:129-141`, comment included). Keep the `launcherT3` constant, which parse validation uses.
- [x] update `TestMain`:
  - delete the T3 stubs (main_test.go:110-118: `newT3Reporter`, `newT3Dispatcher`, `newT3SettleDispatcher`, `newT3Session`) and reword the comment;
  - add `os.Unsetenv` for `LOOPAI_T3_TOKEN` and `LOOPAI_T3_URL`, so a developer shell cannot leak them into tests.
- [x] skill contract:
  - delete the `--t3-launch [agent|auto|terminal]` assertion (`skill_contract_test.go:177`); `--finalize` on :178 already covers choice flags;
  - add `"--t3"`, `"--no-t3"`, `"--t3-launch"`, `"--t3-progress-thread"` to the hidden removed-flag `NotContains` list (:179);
  - reword the comment at `skill_contract.go:83`;
  - run `go test ./cmd/loopai -run TestSkillContract -update`.
- [x] edit `docs/backlog/symlinked-checkout-path-containment.md`: move its `area:` line to `pkg/git/external.go` and drop the `pkg/t3/launch.go`/`--t3-launch` half of the prose.
- [x] write tests for the removal:
  - `rejectRemovedFlags` table for `--t3`, `--no-t3`, `--t3-progress-thread=x`, `--t3-progress-thread=`, bare `--t3-launch`, `--t3-launch=agent`, and `--t3-launch=`; each message names the replacement;
  - a run with `LOOPAI_T3=true` in the environment and no flag passes validation;
  - `t3 = true` in config fails `config.Load` with the replacement;
  - `TestCmuxEnvOptionsCoversOptionTags` still passes;
  - `launcherFor` returns only `orca`/`cli`.
- [x] delete the obsolete tests:
  - the T3 test block (main_test.go:3690-4523);
  - the T3 subtests of `TestRunFinalizeCloseout` (:15542-15601);
  - the `launch_history_test.go` rows at :272-273 and :401;
  - the config/values T3 tests.
- [x] run `make test </dev/null`, `make lint`, `make build`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 3

### Task 3: Add the run registry package pkg/runs
- [x] create `pkg/runs/paths.go`: `RunsDir(root)` = `<root>/.loopai/progress/runs`, `Dir(root, id)`, and the per-run file names from Technical Details (none matches `progress-*.txt`). Every path is made absolute before any chdir.
- [x] create `pkg/runs/id.go`:
  - `NewID(planPath, now)` produces `<sanitized plan stem>-<YYYYMMDD-HHMMSS>-<4 hex from crypto/rand>`;
  - `ValidID` accepts one safe path segment: `^[a-z0-9][a-z0-9._-]{0,99}$`;
  - `ThreadKey(threadID)` = the first 32 hex characters of `sha256(threadID)`. T3 thread ids can contain `:` and `%` and exceed 300 characters, so they never become file names raw.
- [x] create `pkg/runs/atomic.go` and `pkg/runs/descriptor.go`:
  - `atomic.go` is the package's own copy of the temp-file + chmod 0600 + rename write; package main's `writeFileAtomic` stays;
  - `descriptor.go` implements the descriptor schema in Technical Details, with `LoadDescriptor` and `SaveDescriptor`;
  - the descriptor has no token field, and the writer is never given one.
- [x] create build-tagged lock helpers:
  - `lock_unix.go` (`//go:build !windows`, flock) with:
    - `AcquireBounded(path, timeout)`, which polls `LOCK_EX|LOCK_NB` every 25 ms until the deadline, so an observer probe never makes a run fail;
    - `Probe(path) (held bool, err error)`, which does a non-blocking acquire-and-release;
  - `lock_windows.go` defines the same symbols returning `ErrUnsupported`;
  - the run's lock fd is opened with Go's default `O_CLOEXEC`.
- [x] add `AcquireLaunchLock(root, planRel, branch, timeout)` and `LaunchInProgress(root, planRel, branch)` (a probe). The lock file is `.launch-<first 16 hex of sha256(planRel + "\x00" + branch)>.lock` in the runs dir.
- [x] create the stop marker, outcome, and report helpers:
  - `pkg/runs/stop.go`: write, detect, and clear the `stop-requested` marker;
  - `pkg/runs/outcome.go`: the outcome schema from Technical Details, with `SaveOutcome`/`LoadOutcome` and `ArchiveOutcome(dir, attempt)`, which renames `outcome.json` to `outcome-attempt-<n>.json`;
  - `report.md` read/write helpers.
- [x] create `pkg/runs/classify.go`: `Classify(root, dir)` returns, in order:
  - `running` when the lifetime lock is held;
  - `succeeded`/`failed`/`stopped` when the lock is free and an outcome exists for the descriptor's current attempt;
  - `stopped` when the lock is free, there is no outcome, and the stop marker is present;
  - `starting` when the lock is free, there is no outcome, and the launch lock for its plan and branch is held;
  - otherwise `crashed`.
- [x] create `pkg/runs/resolve.go`:
  - `List(root)`, newest first by `created_at`;
  - `Resolve(root, ident)`: by exact id, then plan (path relative to cwd or root, or matching basename/stem), then branch; the newest match wins;
  - `DefaultRun(root)`: the single live run, else the newest;
  - `LatestFor(root, planRel, branch)`;
  - `LiveRunForBranch(root, branch)`;
  - `PruneSucceeded(root, keep)`: delete succeeded run directories beyond the newest `keep` whose lock is free.
- [x] create process identity helpers behind `ProcessStartToken(pid) (string, error)`, which returns an opaque string compared for equality:
  - `procstart_darwin.go`: `unix.SysctlKinfoProc("kern.proc.pid", pid)` → `Proc.P_starttime`;
  - `procstart_linux.go`: field 22 of `/proc/<pid>/stat`;
  - `procstart_other.go`: unsupported.
- [x] write tests (success):
  - ids are valid and unique per call;
  - `ThreadKey` is stable, 32 hex characters, and differs for two distinct `thread:provider:…%3A…` ids;
  - the descriptor and outcome round-trip with mode 0600;
  - a classification table for every status, including: a stop marker with no outcome = stopped; an outcome from a previous attempt does not count for the current one;
  - `AcquireBounded` succeeds while another goroutine or process only probes;
  - resolution by id, plan path, plan stem, and branch;
  - `DefaultRun` prefers the single live run;
  - prune keeps the newest 20 succeeded runs and never touches live, failed, or crashed runs;
  - `ProcessStartToken(os.Getpid())` is stable across calls.
- [x] write tests (errors/edge):
  - `ValidID` rejects `..`, slashes, and the empty string;
  - a corrupt descriptor or outcome is reported, not a panic;
  - `AcquireBounded` times out while a helper process holds the lock (re-exec pattern from `pkg/git/worktree_run_lock_test.go:90-140`) and succeeds after the helper is killed;
  - a busy launch lock returns a distinct error;
  - a saved descriptor never contains a token-shaped string (the schema has no field for it).
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 4
- ⚠️ ruling C11: `atomic.go` exports `WriteFileAtomic(path, data, perm)` and package main's `writeFileAtomic` delegates to it instead of keeping a second copy; main's temp files are now named `.<file name>-*.tmp`.
- ⚠️ ruling C10: the token-shaped-string descriptor test was dropped as tautological; the descriptor and outcome tests pin the exact JSON key sets instead, and the real token-leak scan of the run directory, logs, and child environment lands in Task 11.

### Task 4: Add execution knobs for detached runs (inert unless set)
- [x] lint constraint: `executePlan` and `prepareWorktreeRunContext` are already at gocyclo 20 and `runWithWorktree` at 18 (limit 20). Add no `if`/`&&`/`||` to the first two. Add at most one branch to `runWithWorktree`, and move all new decisions into small helpers. Run `make lint` early in this task.
- [x] add three fields to `executePlanRequest` (main.go:276) and copy all three in `worktreeExecuteRequest` (main.go:2308-2347):
  - `KeepWorktreeOnFailure bool`;
  - `ArchiveOnRunBranch bool`;
  - `AttemptStarted func(attemptStart) error`, with `attemptStart{PlanFile (worktree plan copy), MainPlanFile, Branch, WorktreePath, ProgressLog string; Resumed bool}`.
- [x] add a helper `fireAttemptStarted(req, wt, baseLog) (keep bool, err error)` and call it once in `runWithWorktree`, after the progress logger exists (after main.go:2235) and before `os.Chdir(wt.path)` (:2252), passing `baseLog.Path()`. It returns `keep = wt.resumed || (req.AttemptStarted != nil && req.KeepWorktreeOnFailure)` after a successful hook. A hook error aborts the run before execution, and a fresh worktree is then removed as today.
- [x] honour the knob with `keep` only at the sites that run after the hook:
  - when `keep` is true for a fresh worktree, re-set `req.WtCleanup` to the non-removing `restoreCWD` variant;
  - the `:2197` safety-net defer uses `!keep`;
  - `worktreeCmuxFinishCleanup` (:2271-2272) receives a `keepWorktree func(success bool) bool` decision instead of `wt.resumed`;
  - the branch at `:2281-2298` uses that decision for fresh and resumed worktrees alike, printing `worktree preserved for resume: <path>` whenever it keeps one.
  - Leave `:2183-2189` and `prepareWorktreeRunContext` (:2452-2459) unchanged: setup failures before the hook keep today's removal.
- [x] implement the decision in a helper `keepDetachedWorktree(req, wtSvc, success bool) bool`:
  - a non-success always keeps (when `keep` holds);
  - a success keeps only when `KeepWorktreeOnFailure` is set and any of these hold: `req.Outcome.finalizeIncomplete != nil`, `req.Outcome.archiveIncomplete != ""`, `wtSvc.IsDirtyAll()` is true or errors, or `wtSvc.OperationInProgress()` is non-empty or errors;
  - otherwise a success removes it;
  - without the knob, today's `wt.resumed` behaviour is unchanged.
  - The finalize and archive fields are written into `req.Outcome` before `completeCmux` (main.go:1957) triggers the cleanup.
- [x] honour `ArchiveOnRunBranch` in `moveCompletedPlan` (main.go:2043-2073) and `displayStats` (:1573-1575), like `chainRun`: archive through `req.GitSvc` and `req.PlanFile`, which is the run branch. Keep the single-plan semantics: an archive error becomes `incomplete` and is never fatal. Update the comments at `pkg/git/service.go:978-981` and `:1696-1706`, which say a single plan always archives in the invoking checkout.
- [x] extend `planExecutionOutcome`:
  - new fields: `finalizeMode`, `finalizeStatusNote`, `prURL`, `prOpened`, `merged` (from `finalizeResult`, :1939), `planMoved`, `archiveIncomplete` (from `moveCompletedPlan`), and `worktreeKept` (a path);
  - write them through nil-safe methods `func (o *planExecutionOutcome) recordArchive(moved bool, incomplete error)` and `recordFinalize(fin finalizeResult)` (each with the `if o == nil` guard inside), called in `executePlan` right after archival and right after `runFinalizeCloseout`, so `executePlan` gains no branch;
  - `runWithWorktree` sets `worktreeKept` through `recordWorktreeKept(path)` when it keeps the worktree under the knob;
  - all of this goes through the shared `Outcome` pointer, so it survives `worktreeExecuteRequest`.
- [x] add `captureDetachedSourcePlan(svc, planFile, statePath)` and `reconcileDetachedSourcePlan(svc, statePath)` in `cmd/loopai/detach_source.go`:
  - capture through `CapturePlanChainSourceState([]string{planFile})`, only when `statePath` does not exist yet (a resume reuses the first attempt's capture), and persist via `saveJSONState`;
  - reconcile through `ReconcilePlanChainSourceState`, returning joined errors without failing.
- [x] write tests (success) with the fake-claude worktree flow (`runFinalizePlanWithReport` pattern, main_test.go:15710-15762) and `NonInteractive: true` plus `WorktreeEnabled` (a combination never exercised before):
  - the hook fires once, with absolute worktree, plan, branch, and progress-log paths, before the chdir;
  - a clean success with the knob removes the worktree;
  - `ArchiveOnRunBranch` puts `docs/plans/completed/<plan>` and `<stem>.report.md` on the run branch while the invoking checkout's HEAD and index stay unchanged;
  - the outcome fields carry the finalize, archive, and report results;
  - a table test sets every knob and asserts that `worktreeExecuteRequest` copies it;
  - the nil-safe recorders accept a nil outcome.
- [x] write tests (errors/edge):
  - with `KeepWorktreeOnFailure`:
    - a failing task keeps the fresh worktree with its uncommitted file;
    - a task-phase `max iterations … reached` failure (`MaxIterations: 1` with a never-completing fake claude) keeps the worktree;
    - a canceled context keeps it;
  - also with the knob, a success still keeps the worktree when:
    - a blocked finalize sync left an uncommitted file;
    - a `commit-msg` hook rejected the run-branch archive commit, which is `incomplete`, not a failure;
    - the worktree is dirty;
  - a hook error before execution removes the fresh worktree;
  - with the knobs unset, today's behaviour holds: the worktree is removed on failure and the archive lands in the invoking checkout;
  - source reconcile:
    - removes an unchanged untracked plan;
    - restores an unchanged tracked plan;
    - leaves a concurrently edited plan untouched;
    - a second capture call does not overwrite the first.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 5
- ⚠️ `fireAttemptStarted` also takes the derived `branch`, which `worktreeRun` does not carry; the hook receives every path absolute (a relative plan path is resolved against the worktree and the invoking checkout).
- ⚠️ `recordWorktreeKept` is called from `runWithWorktree`'s non-removing cleanup, so the outcome names every worktree the run leaves in place, the safety-net keep after a failed hook on a resumed worktree (C2) and a resumed worktree without the knob included, not only a kept fresh one.
- ⚠️ the knob helpers live in `cmd/loopai/detach_worktree.go`; `source-plans.json` holds `{"source_plans": [...]}` (the captured `git.PlanSourceState` list, empty for a clean tracked plan).

### Task 5: Add the event journal writer and the observer
- [x] create `pkg/runs/journal.go`:
  - an append-only `events.jsonl` writer with a monotonically increasing `seq` across attempts, resumed from the last line on open;
  - mutex-safe, with one `O_APPEND` write per event;
  - a reader `ReadFrom(dir, seq)`;
  - the event schema from Technical Details.
- [x] create `pkg/runs/observer.go`:
  - `Follow(ctx, root, dir, fromSeq, func(Event))` tails the journal (250 ms poll) and returns after the lifetime lock is free and the journal is drained;
  - a `Snapshot` fold (attempt, phase, task n/m and title, task statuses, stages, last section, limit wait, finish) computed from events only;
  - finish is reported from the lock plus the outcome record (`Classify`).
- [x] create `cmd/loopai/detach_journal.go` with a `journalHooks` value providing two hooks:
  - `LogDecorator` forwards every `processor.Logger` method unchanged. On `PrintSection` it emits:
    - `task` for `SectionTaskIteration`, with `Iteration` as the plan position; it then re-reads the worktree plan with `plan.ParsePlanFile` to emit `tasks` statuses;
    - `review` for `SectionInternalReview`, `SectionExternalReviewIteration`, `SectionCustomIteration`, and `SectionExternalEvaluation`;
  - `PhaseObserver` emits `phase`. For `status.PhaseLimitWait` it adds `wait_seconds` from `cfg.WaitOnLimit`, because the duration label never reaches loggers below section timing.
- [x] add `attemptStartEvent(...)`, which builds the `attempt_start` payload:
  - plan, branch, worktree, worktree plan, progress log, console log byte offset, mode, tasks with titles, and task total;
  - stages, computed mode-aware: none for tasks-only; otherwise `review`, plus `external_review` when the resolved reviewer chain is non-empty, `finalize` when `cfg.EffectiveFinalize() != config.FinalizeNone`, and `report` when `cfg.ReportEnabled`.
  - The `finalize`, `report`, and `finish` builders are added in Task 7, where they are first called.
- [x] write tests (success):
  - sequence numbers continue after a reopen;
  - concurrent appends from several goroutines stay valid JSON lines;
  - the decorator forwards every logger call to the wrapped logger and emits the expected events for each section type;
  - a limit-wait phase records `wait_seconds` equal to `WaitOnLimit`;
  - `Follow` from a mid-journal `seq` delivers only later events, follows appends, and returns after finish;
  - `Snapshot` reproduces task n/m and stage states;
  - `attemptStartEvent` stages: full mode with a chain, finalize, and report yields all four; an empty chain drops `external_review`; tasks-only yields none.
- [x] write tests (errors/edge):
  - a truncated final line (crash mid-write) is skipped, not fatal;
  - `Follow` returns on context cancel;
  - a crashed run (lock free, no outcome) ends follow with status `crashed`;
  - a missing plan file during re-parse emits no `tasks` event and no error to the run.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 6
- ⚠️ beyond the brief: `runs.FollowUntilFinish(ctx, dir, attempt, fromSeq, fn)` ends at that attempt's `finish` event without the lock (ruling C5, for the in-child bridge); `journalHooks.startAttempt(ev)` appends `attempt_start` and records the worktree plan the task sections re-read. `Follow` returns the final `Status`; task and stage progress uses `pending`/`active`/`done`.
- ⚠️ review fix: the task phase prints its section only when an iteration starts, so `PhaseObserver` also emits a re-parsed `tasks` event whenever the phase leaves `PhaseTask` (a review block, a limit wait, or the review stage). The `Snapshot` derives `Tasks` from the recorded statuses: the running task is `active` until recorded done, and every task is `done` once a post-task stage is entered or the attempt succeeds (tasks-only changes no phase after its last task); a finish that is not a success keeps the recorded statuses.

### Task 6: Add the hidden detached child skeleton and the handshake
- [x] add an exit-code error type `exitCodeError{code int; err error}`. `main()` (main.go:436-439) unwraps it with `errors.As`, prints `error: …` only when the wrapped error is non-nil, then calls `os.Exit(code)`. Every other error keeps exit status 1.
- [x] add the hidden flag `DetachedRun string` (`long:"detached-run" hidden:"true"`, no env tag) to `opts`:
  - route it in `run()` before `loadRunConfig` through one helper, `runPreConfigCommand`, which will also host `--acp` in Task 13; this keeps `run()` under gocyclo 20;
  - add it to `isStandaloneCommand`;
  - add `launcherForOrigin(kind string) string` to `cmd/loopai/launch_history.go`: `t3` for origins `t3-terminal` and `t3-thread`, else `cli`, with a table test.
- [x] create `cmd/loopai/detach_child_unix.go` (`//go:build !windows`):
  - as the very first step, call `syscall.CloseOnExec(3)` and `syscall.CloseOnExec(4)`, because handshake descriptors from `ExtraFiles` arrive without `FD_CLOEXEC`;
  - read one JSON line from fd 3, then close it;
  - install `signal.Notify` for SIGHUP. Never use `signal.Ignore`, whose disposition exec inherits.
  - `detach_child_windows.go` defines the same entry point and returns `detached runs are not supported on Windows`.
- [x] implement `runDetachedChild(ctx, o)`:
  - load the descriptor for `o.DetachedRun` from `<cwd>/.loopai/progress/runs/`;
  - acquire the lifetime lock with `AcquireBounded(…, 2s)`. If it is busy, send the ack error `run <id> is already running` and exit without writing an outcome;
  - update the descriptor with the pid and `ProcessStartToken`;
  - set `o.Worktree = true` BEFORE `loadRunConfig`, so `resolveBaseRefs` and `selectAndExecutePlan` see worktree mode. Never patch `cfg` afterwards;
  - force `cfg.Orca = false`;
  - build the request with `prepareNonInteractiveRequest`, writing to `os.Stdout` (the console log);
  - create and stop its own `newAwakeHolder(cfg.KeepAwake)` and set `req.KeepAwake`;
  - set `KeepWorktreeOnFailure`, `ArchiveOnRunBranch`, the journal hooks, and `AttemptStarted`;
  - capture the source plan into `source-plans.json` before `selectAndExecutePlan`.
- [x] in `AttemptStarted`:
  - update the descriptor (branch, worktree path, progress log);
  - append `attempt_start`;
  - write the ack line `{"ok":true,"run_id":…,"attempt":…}` to fd 4 and close it.
- [x] handle errors before the ack: any error is written to fd 4 as `{"ok":false,"error":…}` and also recorded as a `failed` outcome with reason `startup: …` and exit code 1. After `selectAndExecutePlan` returns in this task, the child returns `exitCodeError` with 0 on `Outcome.succeeded`, else 1. Full outcome classification comes in Task 7.
- [x] add to `TestMain`: unset `CODEX_SANDBOX`, `CODEX_SANDBOX_NETWORK_DISABLED`, `SANDBOX_RUNTIME`, every `T3_ACP_MCP_*`, and `CMUX_*`.
- [x] write tests (success). They re-execute the test binary as the child: `TestDetachChildHelper` sets `os.Args` from an env var and calls `main()`, with HOME/USERPROFILE/LOOPAI_CONFIG_DIR redirected, a fake claude on `PATH`, and two `os.Pipe` pairs passed as `ExtraFiles`.
  - the ack arrives after `attempt_start`, carrying the run id;
  - the descriptor gains pid and start token;
  - `exitCodeError` makes `main()` exit with the carried code and prints nothing for a nil error.
- [x] write tests (errors/edge):
  - fd 3 and fd 4 are not open in a grandchild the child spawns: the helper runs `sh -c 'ls /dev/fd'` or checks `fcntl(F_GETFD)` for `FD_CLOEXEC`;
  - the launcher side sees EOF on fd 4 only after the explicit ack;
  - a missing plan or a broken config reaches fd 4 as `{"ok":false}` and produces a `failed` outcome with reason `startup: …`;
  - a held lifetime lock yields `already running` with no outcome;
  - SIGHUP does not terminate the child.
- ⚠️ the child logic that is not Unix-specific lives in an untagged `cmd/loopai/detach_child.go`; `detach_child_unix.go` holds only the handshake entry point (`runDetachedChild`, `takeHandshakePipes`) and `detach_child_windows.go` the stub with `errDetachUnsupported`.
- ⚠️ before `CloseOnExec`, the child checks that fds 3 and 4 are pipes that arrived without `FD_CLOEXEC` (inherited) and refuses otherwise: without a launcher, the Go runtime's own Darwin signal-delivery pipe lands on fds 3/4 and reading or closing it hangs or breaks the process. Nothing can start a process between the check and the marking.
- ⚠️ errors before the lifetime lock is held (a malformed secret line, an invalid id or unreadable descriptor, a busy lock) are answered on fd 4 but record no outcome, since the run directory may belong to a live attempt; a child without usable handshake descriptors cannot answer and exits 1. Every error after the lock and before the ack records the `startup: …` outcome.
- ⚠️ the fd-inheritance test probes Git processes (a `vcs_command` wrapper) that run while the ack pipe is still open; the claude session starts only after both pipes are closed, so probing it would prove nothing. fd 3 is closed before any process starts, so its `FD_CLOEXEC` marking is checked in-process with `fcntl(F_GETFD)`.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 7

### Task 7: Complete the detached child's outcome, report, and source-plan lifecycle
- [x] add the `finalize`, `report`, and `finish` event builders to `cmd/loopai/detach_journal.go`. The `report` builder takes the bounded summary (≤ 4 KiB) via `splitReportSections` and the risk via `reportRiskLevel`.
- [x] after `errors.Join(selectAndExecutePlan(...), release())` returns, classify:
  - check the returned error first, then `Outcome.failure`;
  - `stopped` when the stop marker exists or `errors.Is(err, context.Canceled)`;
  - `succeeded` only on `Outcome.succeeded`;
  - otherwise `failed`.
- [x] complete the run after classification, in this order:
  - fill the commit from `Outcome.branchTip`, or `BranchHash(branch)`;
  - write `report.md`;
  - append the `finalize`, `report`, and `finish` events;
  - write `outcome.json` with exit code 0/1/2 and `worktree_kept`;
  - reconcile the source plan on success only, writing warnings to the console log and the outcome's `notes`;
  - only then let process exit release the lock, returning `exitCodeError` with the outcome's code.
- [x] record launch history in the child right after `prepareNonInteractiveRequest`, with `recordLaunchHistory(o, cfg, req.ExternalReview, launcherForOrigin(desc.Origin.Kind), os.Stderr)`.
- [x] write tests (success) with the Task 6 helper and a fake claude. A successful run:
  - writes `outcome.json` (status, exit code 0, branch, commit, report text) and `report.md`;
  - removes the worktree;
  - archives on the run branch;
  - reconciles the untracked source plan;
  - appends one launch-history line with launcher `cli`; a descriptor with origin `t3-thread` records `t3`;
  - writes events ending in `finish` with exit code 0.
- [x] write tests (errors/edge):
  - a failing task keeps the worktree and writes `failed` with exit code 1;
  - SIGTERM after a stop marker yields `stopped` with exit code 2 and a kept worktree;
  - a second attempt appends `attempt_start` with attempt 2 and reuses the first `source-plans.json`;
  - a report longer than 256 KiB is capped in `outcome.json` while `report.md` holds the full text;
  - a failed run does not reconcile the source plan.
- ⚠️ a run that returned no error and recorded its success is `succeeded` even when the stop marker exists: the stop arrived after the run completed, its worktree is gone and its plan archived, so `stopped` would invite a resume of finished work.
- ⚠️ `report.md` and the `report` event are written only when the run produced a report, and the `finalize` event only when the close-out ran, so a later attempt without a report never replaces an earlier `report.md` with an empty one.
- ⚠️ `cfg.Orca = false` has no observable effect on the non-interactive path, which builds no title reporter, so it is pinned by a unit test of the extracted `loadDetachedConfig` rather than through the child.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 8

### Task 8: Add the --detach launcher, the launch lock, and the --merge/--pr guard
- [x] add `Detach bool` (`long:"detach"`, visible) to `opts`:
  - add `detach` to `markFlagsSet`'s `executionModeSet` list and to `hasExecutionMode`, so close-out and other standalone commands reject it;
  - add it to `isStandaloneCommand`.
- [x] add `validateDetachFlags` in table style, inserted in `validateFlags` BEFORE `validatePlanChain`. It applies to `--detach` and `--detached-run`:
  - require a plan;
  - refuse `len(o.PlanFiles) > 1` ("a detached run takes exactly one plan; plan chains are not supported") and any `o.extraArgs`;
  - refuse `--review`, `--external-only`, `--plan`, `--gen-agents`, `--serve`, `--watch`, `--cmux-workspace`, `--init`, `--reset`, `--dump-defaults`, `--clear`, and `--merge`/`--pr`/`--report`;
  - refuse `--commit`: a detached run never commits into the invoking checkout;
  - accept `--tasks-only` and an explicit `--worktree`.
- [x] route `--detach` through `runConfiguredStandaloneCommand`'s switch, after config and before main.go:543, so launch history is not recorded twice. Every symbol the untagged `detach_launch.go` calls from `detach_launch_unix.go` gets a same-signature stub in `detach_launch_windows.go` returning `detached runs are not supported on Windows`.
- [x] add `stripDetachArgs(args []string) []string` with these rules:
  - remove `--detach`;
  - remove both forms of each value flag: `--detached-run=v` and `--detached-run v`, `--t3-thread=v` and `--t3-thread v`, `--acp-thread=v` and `--acp-thread v`, `--acp-session=v` and `--acp-session v` (the next token is consumed);
  - stop scanning at `--`, which is kept along with everything after it;
  - keep explicit empty values such as `--external-reviewers=`.
- [x] implement the launcher preflight (`cmd/loopai/detach_launch.go` + `_unix.go`), in this order:
  1. refuse inside a detected sandbox. `detectSandbox(getenv)` reports `CODEX_SANDBOX` non-empty, `CODEX_SANDBOX_NETWORK_DISABLED == "1"`, or `SANDBOX_RUNTIME == "1"`; the message names the signal and points to Mode 2 of the loopai-t3 skill;
  2. apply `planFileRefusal` and `executableHandOffRefusal`;
  3. open the repository with `vcs_command` and require the repo root;
  4. call `EnsureLocalGitignore()` before creating the run directory;
  5. resolve the branch: the `--branch` override, else `git.Service.EffectiveBranchName`.
- [x] decide the run under `AcquireLaunchLock(root, planRel, branch, 5s)`; if it is busy, report "another launch of this plan is in progress". Then `LatestFor` gives one of three cases:
  - live run: refuse with its id and `loopai --attach <id>`;
  - not succeeded and not live: resume with the same id. Call `ArchiveOutcome`, clear the stop marker, increment the attempt, and record the new argv. Keep the recorded origin (kind, thread id, ACP session id) unless the relaunch passes `--t3-thread`, `--acp-thread`, or `--acp-session`;
  - succeeded or none: create a `NewID`, create the directory, call `PruneSucceeded(root, 20)`, and give the descriptor origin `cli`.
- [x] spawn the child:
  - command: `detachExecutable()` with `--detached-run=<id>` plus `stripDetachArgs(os.Args[1:])`;
  - `cmd.Dir` = the invoking checkout; `SysProcAttr{Setsid: true}`; stdin nil (the null device);
  - stdout and stderr = `console.log`, opened `O_APPEND|O_CREATE` 0600 and preceded by an attempt separator line. It is an `*os.File`, so os/exec hands the descriptor over directly;
  - `ExtraFiles` = [secret pipe read end (fd 3), ack pipe write end (fd 4)];
  - environment = `os.Environ()` minus `T3_ACP_MCP_*`, `LOOPAI_T3_TOKEN`, and `CMUX_*`, plus the descriptor's recorded env-tag options when relaunching.
- [x] add the Linux transient scope:
  - the prefix comes from `var detachScopePrefix = defaultDetachScopePrefix`, wrapping a pure `systemdScopePrefix(goos string, lookPath func(string) (string, error), stat func(string) (os.FileInfo, error), getenv func(string) string) []string`;
  - it returns `systemd-run --user --scope --quiet --collect --` only on Linux, when `systemd-run` is on `PATH` and `$XDG_RUNTIME_DIR/systemd/private` exists. Descriptors are kept across its exec;
  - if a scoped start fails before any ack, retry once without it and print one warning;
  - stub `detachScopePrefix` to return nil in `TestMain`.
- [x] after `Start`:
  - close the child ends;
  - write the secret JSON line (`{"t3_token":""}` until Task 11) and close the write end;
  - wait for the ack line (bounded 120 s) while also watching for child exit;
  - on `ok`: print `run: <id>` and `follow: loopai --attach <id>`, call `Process.Release`, release the launch lock, and exit 0;
  - on an error message or child exit: print the reason, or the console log tail, and exit 1;
  - on timeout: print the console log path and exit 1 without killing the detached child.
- [x] make the executable and the child argv prefix replaceable package variables (`detachExecutable = os.Executable`, `detachChildArgs`) so tests can point them at the test binary.
- [x] add the guard `refuseLiveDetachedRun(ctx, gitSvc, feature)` to `runMergeCommand` and `runPRCommand` (main.go:5652, :6001), right after the feature branch is resolved. It refuses in either of two cases:
  1. `runs.LiveRunForBranch(root, feature)` finds a live run in the runs directory of any root from `progressRecordRoots(gitSvc)`;
  2. the registered worktree that has `feature` checked out (from `gitSvc.Worktrees()`) answers a non-blocking `AcquireWorktreeRunLockContext` with `ErrWorktreeBusy`. Release the lock at once when it was acquired.

  The error names the run id (when known) and `loopai --stop <id>`. `runs.ErrUnsupported` on Windows counts as "no live run" for check 1.
- [x] run `go test ./cmd/loopai -run TestSkillContract -update` (adds `--detach`).
- [x] write tests (success) with a stub child, a test-binary helper that reads fd 3, dumps its environment and argv to a file, and writes an ack:
  - the launch prints the run id and follow command and exits 0;
  - the argv keeps raw explicit empty values (`--external-reviewers=`) and drops `--detach`;
  - the environment contains no `T3_ACP_MCP_ENDPOINT`, `T3_ACP_MCP_AUTHORIZATION`, `LOOPAI_T3_TOKEN`, or `CMUX_WORKSPACE_ID`;
  - a failed previous run is resumed with the same id and attempt 2, and its outcome archived;
  - a resume from a plain launcher of a run whose descriptor has origin `t3-thread` keeps origin kind, thread id, and ACP session id unchanged;
  - a succeeded previous run gets a new id with origin `cli`;
  - `.loopai/.gitignore` exists before the run directory;
  - `stripDetachArgs` table: both forms of each value flag, `--` handling, and empty values. The stripped argv parses back to the same `o.PlanFile` with empty `extraArgs`;
  - `systemdScopePrefix` table: darwin → nil; linux without `systemd-run` → nil; linux without the private socket → nil; linux with both → the prefix.
- [x] write tests (errors/edge):
  - a validation table: chain, surplus positional, `--review`, `--external-only`, `--commit`, `--serve`, `--cmux-workspace`, `--merge`, no plan;
  - a `detectSandbox` table for each signal and for none;
  - launching inside a sandbox fails before any directory is created;
  - two launchers started concurrently against the same plan yield exactly one child; the other reports a live run or the launch lock;
  - a child that writes `{"ok":false,"error":"x"}` makes the launcher print `x` and exit 1;
  - a child that exits without an ack makes the launcher print the console log tail.
  - `--merge <branch>` and `--pr`:
    - refuse while a live run of that branch holds its lock, and proceed once it is released;
    - refuse when the run directory lives in a second registered worktree and `--merge <branch>` is invoked from the primary checkout;
    - refuse when only the feature worktree's run lock is held, with no run directory at all.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 9

⚠️ Check 2 of the `--merge`/`--pr` guard says "a loopai run of <branch> is live: <busy error>; stop it first (loopai --stop <run-id> for a detached run)" instead of the detached-run message minus the id, since the worktree-lock probe also catches a foreground `--worktree` run and has no id to name.

⚠️ F5 is handled once in `pkg/runs`: `List` wraps an unreadable run directory in an error naming the run id, its directory, and the fix (check that no loopai process uses it, then remove it); filtered queries (`LatestFor`, `LiveRunForBranch`, `PruneSucceeded`) still fail closed, because a broken run could belong to the same plan or branch. F2 is handled in `Classify`: a current-attempt `finish` journal event with status `succeeded` and no outcome classifies as succeeded.

⚠️ Review fix round 1: the sandbox refusal runs in `runPreConfigCommand`, before config loading, because the defaults installer writes under HOME and a sandbox's write denial would otherwise replace the refusal; the rest of the preflight still runs after config. `--detach` with `--detached-run` is rejected. `PruneSucceeded` never removes a succeeded run whose kept worktree (`runs.KeptWorktree`) is still in place, and such a run does not count toward the 20; the launcher also refuses a new run when the latest run succeeded and another registered worktree (not the invoking checkout) still has the branch checked out.

### Task 9: Add --attach, --runs, and --stop
- [x] add visible `Attach bool` (`long:"attach"`), `Runs bool` (`long:"runs"`), and `Stop bool` (`long:"stop"`), with the identifier as the positional:
  - extend `opts.applyPositionalArgs` (main.go:140) so `--attach`/`--stop` identifiers are never comma-split;
  - add validators that reject execution flags (reuse the `bare := o; bare.PlanFile = ""` trick and the surplus-positional rejection from close-out), require an identifier for `--stop`, and refuse combinations of the three;
  - add all three to `isStandaloneCommand`.
- [x] `--attach` returns the Task 6 `exitCodeError` carrying the run's exit code. Route `--runs` and `--stop` through `runConfiguredStandaloneCommand`'s switch.
- [x] route `--attach` at the top of `run()`, before `disableCtrlCEcho` and `startInterruptWatcher`. Ctrl+C then only detaches quietly: it prints `detached; the run continues: loopai --attach <id>` and exits 0. `runAttachCommand` calls `validateFlags` and loads config read-only for colors itself.
- [x] implement `--attach [run|plan|branch]` in `cmd/loopai/runs_cmd.go`:
  1. resolve with `runs.Resolve`; with no argument, use `runs.DefaultRun`;
  2. print a header from the observer `Snapshot`: run id, attempt, status, phase, task n/m;
  3. replay `console.log` from byte 0 and follow it (250 ms poll) until the observer reports finish and the file is drained;
  4. print the outcome block: status, reason, branch, commit, report location (`<run dir>/report.md` and `loopai --report <branch>`), PR URL, kept worktree;
     - ⚠️ `loopai --report <branch>` is shown only when the outcome records the plan archived (`plan_moved`), since only then is the report sidecar on the branch.
  5. exit with the run's code (crashed → 1).
- [x] implement `--runs`: one row per run, newest first, with id, status, attempt, origin (`cli`, `t3-terminal:<thread>`, `t3-thread:<thread>`), plan, branch, and start time.
- [x] implement `--stop <run>` through an untagged `stopRun(root string, desc runs.Descriptor) error`, which the Mode 2 provider also uses:
  1. require the lifetime lock to be held and the descriptor pid's `ProcessStartToken` to equal the recorded one; otherwise refuse as not running or pid reused;
  2. write the stop marker FIRST;
  3. then call `signalTerm(pid)`, the pid only and never the group;
  4. wait up to 15 s for the lock to be released and print the resulting status; otherwise print that the stop was requested and how to follow it.

  `signalTerm(pid int) error` lives in `runs_signal_unix.go` (`syscall.Kill(pid, syscall.SIGTERM)`) and `runs_signal_windows.go` (returns `errDetachUnsupported`).
  - ⚠️ `stopRun` does steps 1-3 and returns without waiting (it has no writer, and Mode 2 must neither print nor block); the `--stop` command (`stopDetachedRun`) does step 4. `stopRun` rereads the descriptor from the run directory, so a caller's stale copy cannot misread a resumed attempt's pid.
- [x] on Windows, `--attach`, `--runs`, and `--stop` return `detached runs are not supported on Windows`.
- [x] run `go test ./cmd/loopai -run TestSkillContract -update` (adds `--attach`, `--runs`, `--stop`).
- [x] write tests (success):
  - `--attach` on a finished run replays the whole console log and exits with its code (0 succeeded, 1 failed, 2 stopped), through the re-exec helper;
  - `--attach` on a live run (a helper holding the lock and appending) follows appended lines and returns when the helper writes the outcome and exits;
  - no-argument resolution picks the single live run, else the newest;
  - `--runs` lists statuses including `crashed` and `starting`;
  - `--stop` on a helper child writes the marker, the child exits on SIGTERM, and the run classifies as `stopped`.
- [x] write tests (errors/edge):
  - Ctrl+C (context cancel) during `--attach` returns quickly with code 0 and leaves the run untouched;
  - `--stop` refuses a finished run, an unknown id, and a descriptor whose start token does not match (pid reuse), without sending a signal;
  - a run killed by the force exit after `--stop` (no outcome, marker present) classifies as `stopped`, not `crashed`;
  - positional identifiers with commas are not split;
  - `--attach --detach` and `--runs plan.md` are rejected.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 10

### Task 10: Add the protocol-2 T3 client and an importable fake server
- [x] create `pkg/t3/runtime.go` (a new package; nothing is ported):
  - T3 home = trimmed `T3CODE_HOME`, else `~/.t3`;
  - read `<home>/userdata/server-runtime.json` (`origin`, `pid`, `startedAt`, `serverVersion`) on every connect and reconnect; `LOOPAI_T3_URL` overrides the origin;
  - WS URL = the origin with `http`→`ws` and `https`→`wss`, path `/ws`, query `orchestrationProtocol=2`;
  - the client never writes below the T3 home.
- [x] create `pkg/t3/rpc.go`:
  - dial with `Authorization: Bearer <token>` using gorilla/websocket;
  - on a failed upgrade, parse the JSON body (`code`/`reason`/`message`) into a typed `HandshakeError` (426 `orchestration_protocol_incompatible`; 401 `auth_invalid` with `reason`) instead of a bare status;
  - the codec follows Technical Details:
    - one JSON object per text frame;
    - each Request carries `headers: []` and a numeric id counter;
    - responses are routed by `requestId`;
    - Exit decoding: Success, and Failure, where Fail surfaces `error._tag` and `message`/`detail` while Die and Interrupt are transport errors;
    - `Defect` fails every pending call;
    - Ping/Pong;
  - streams send `{"_tag":"Ack","requestId":id}` after every Chunk, and `Interrupt` on unsubscribe.
- [x] create `pkg/t3/client.go` methods:
  - `TerminalOpen(TerminalOpenInput)`, `TerminalWrite(threadID, terminalID, data)`;
  - `SubscribeTerminalMetadata(ctx)`, with events `snapshot`/`upsert`/`remove`;
  - `GetThreadProjection(threadID)`, returning title, pinnedAt, worktreePath, projectId, archivedAt;
  - `SetTitle(threadID, title)` (title-only `thread.metadata.update`), `Pin(threadID)`, `Unpin(threadID)`. Each dispatch gets a fresh 32-hex `commandId` from `crypto/rand`, reused only for a retry of the same update;
  - HTTP `Projects()` (`GET /api/projects`, Bearer), returning `{id, workspaceRoot}`;
  - never send `providerInstanceId`, `createdBy`, or `creationSource`; never call `terminal.close`.
- [x] create `pkg/t3/t3test/fake.go` (package `t3test`, a non-test file importable from `pkg/t3` and `cmd/loopai` tests). It exports `NewServer(t testing.TB, opts ...Option) *Server`, backed by `httptest`. The server:
  - answers 426 with the exact incompatible-protocol JSON when the query parameter is missing, before auth;
  - answers 401 `auth_invalid` for a missing or wrong Bearer;
  - rejects a Request without `headers`;
  - replies Exit/Chunk/Defect per the shapes in Technical Details;
  - delivers the next metadata Chunk only after the client's Ack;
  - provides options and hooks for: a token, thread projections, projects, the terminal list, an injected upsert/remove, a forced 401 on the next connection, a simulated restart (drop connections and clear terminals), and recorded requests (dispatches, opens, writes);
  - has its own `fake_test.go`.
- [x] write tests (success) in `pkg/t3` using `t3test.NewServer`:
  - the handshake carries the query and the Bearer;
  - terminal open/write payloads match the schema;
  - the metadata stream delivers snapshot then upserts, only with acks;
  - dispatch payloads for `thread.metadata.update` (title only), `thread.pin`, and `thread.unpin` carry distinct `commandId`s;
  - the projection and projects decode;
  - the runtime file is re-read on reconnect (port change);
  - `LOOPAI_T3_URL` wins.
- [x] write tests (errors):
  - 426 and 401 surface code and message;
  - a Fail exit surfaces `_tag` and message (e.g. `TerminalNotRunningError`);
  - a Defect fails all pending calls;
  - a missing runtime file is a typed "T3 not running" error;
  - a stalled stream without Ack (fake server mode) proves the client acks.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 11
- ⚠️ every client method takes a leading `context.Context` (Go convention), and `Projects` uses the origin of the current connection; with `LOOPAI_T3_URL` set, a missing or unreadable runtime file only leaves the PID and start time zero. The fake additionally fails a request carrying `providerInstanceId`, `createdBy`, `creationSource`, or `createdAt`, so a violation is loud; T3 v0.1.1 itself tolerates a Request without `headers` (`Headers.fromInput(undefined)`), so the fake's rejection is stricter than the server.

### Task 11: Add Mode 1 (--t3-thread) and the title/terminal bridge
- [x] add the visible `T3Thread string` (`long:"t3-thread" value-name:"thread-id"`, no env tag):
  - valid only with `--detach`/`--detached-run`;
  - validated against `^[A-Za-z0-9_.:%-]{1,1024}$`. T3 ids include UUIDs and provider-native ids such as `thread:provider:claudeAgent:native-thread:thread%3A…%3Aagent%3A3`;
  - the launcher requires `LOOPAI_T3_TOKEN` when the flag is set ("--t3-thread needs LOOPAI_T3_TOKEN");
  - the launcher records origin `t3-terminal` with the thread id, and sends `{"t3_token":"<token>"}` over fd 3;
  - the recorded descriptor argv excludes `--t3-thread` in either form (`stripDetachArgs`).
- [x] create `cmd/loopai/t3_bridge.go` with `var newT3Bridge = startT3Bridge`, stubbed to a no-op in `TestMain` in this same task:
  - the child starts it after a successful `AttemptStarted` when the origin is `t3-terminal` and a token arrived;
  - the token lives only in the bridge's memory and is never logged;
  - add a `progressLogger()` accessor to `journalHooks` that returns the decorated logger, so the bridge can write one line into the progress log.
- [x] bridge titles:
  - follow the journal through the observer;
  - desired title = `[loopai: <state>] ` + the current title with any `^\[loopai: [^\]]*\] ?` prefix stripped;
  - re-read the title with `GetThreadProjection` before each update, so a user rename is kept;
  - one goroutine coalesces to the latest desired title and sends only changed titles, on transitions only (task change, phase change, finish);
  - states are listed in Technical Details; the final `done`/`failed`/`stopped` is left in place.
- [x] bridge pin: pin the thread when `pinnedAt` is null at start and remember that; unpin at finish only if the bridge pinned.
- [x] bridge terminal:
  - terminal id `loopai-<run-id>` (≤128 chars, never `term-1`);
  - on the first metadata snapshot of this child process, reuse the terminal if it is present. Otherwise call `terminal.open` once with:
    - cwd = `thread.worktreePath ?? project.workspaceRoot`, the root taken from `Projects()` matched by `projectId`;
    - `worktreePath` only when non-null;
    - env `{T3CODE_PROJECT_ROOT: workspaceRoot, T3CODE_WORKTREE_PATH: worktreePath (omitted when null)}`;
  - then write the follow command `" cd '<invoking checkout>' && '<absolute loopai>' --attach <run-id>\r"`, with a leading space and POSIX single-quote escaping.
- [x] bridge reconnect:
  - back off from 1 s to 30 s, re-reading `server-runtime.json`;
  - never call `terminal.open` again in this process;
  - after a server restart (runtime pid/startedAt changed, or the connection was lost), a missing terminal is pending, and the bridge waits for its `upsert`;
  - a `remove` for its terminal seen during a live connection means the user closed it, so it is never re-typed;
  - re-type the follow command only when the terminal is running, `hasRunningSubprocess` has been false for 60 s, and nothing was typed in the last 90 s. This tolerates the ~30 s metadata lag.
- [x] bridge failure: a `HandshakeError` with `auth_invalid`, or any other unrecoverable T3 error, disables the bridge. It writes one line through `journalHooks.progressLogger()` (and so the console log) and never affects the run.
- [x] run `go test ./cmd/loopai -run TestSkillContract -update` (adds `--t3-thread`).
- [x] write tests (success) against `t3test.NewServer`:
  - titles go `[loopai: task 1/2] My thread` → `[loopai: task 2/2] My thread` → `[loopai: done] My thread`;
  - a user rename to `Renamed` between updates yields `[loopai: …] Renamed`;
  - an already pinned thread is neither pinned nor unpinned;
  - the terminal is opened once with the UI's cwd/env/worktreePath and receives the follow command;
  - the launcher hands the token over fd 3, and the child's environment and descriptor never contain it;
  - a thread id containing `:` and `%` passes validation and reaches every request unchanged;
  - `progressLogger()` writes exactly one line to the wrapped logger.
- [x] write tests (errors/edge):
  - after a simulated restart the snapshot lacks the terminal and no `terminal.open` is sent; a later `upsert` with `hasRunningSubprocess:false` for 60 s (injected clock) re-types once;
  - a `remove` during a live connection is never re-typed;
  - no title update is sent when the desired title is unchanged;
  - a 401 on reconnect disables the bridge with exactly one progress-log line, and the run still succeeds;
  - `--t3-thread` is rejected without `--detach`, with an id containing whitespace or `'`, and without `LOOPAI_T3_TOKEN`.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 12
- ⚠️ Go's regexp caps a repeat count at 1000, so `{1,1024}` does not compile: the id is checked as `^[A-Za-z0-9_.:%-]+$` plus a length of at most 1024, the same accepted set.
- ⚠️ per the T3 v0.1.1 source-study ruling, "never call `terminal.open` again in this process" and the restart test's "no `terminal.open` is sent" are superseded: after a restart (or a lost connection) an absent terminal is reopened once per server start with the identical UI launch context, the follow command retried on `TerminalNotRunningError` until it runs (bounded 30 s); the restart test checks exactly that and the later idle re-type. A terminal found present on the first snapshot is reused and typed into only through the re-type rule.
- ⚠️ a re-type also needs the previous follow command to have been seen running since the bridge last typed, so a command that fails at once is not typed every 90 s for the life of the run; a pin or unpin T3 refuses with a typed error (an archived thread) is skipped instead of disabling the bridge.
- ⚠️ `progressLogger()` returns a forwarder to the decorated logger (C6): lines queue until `LogDecorator` runs, and a run that ends without building its logger writes them to the console log. The bridge, the child's wiring, and the 401 and token-leak checks also have end-to-end tests through the real child against `t3test`.
- ⚠️ Review fix round 1: a `terminal.write` answered with `TerminalWriteError` (the pty write threw because the shell died) means the terminal is gone, like `TerminalSessionLookupError`; it no longer disables the bridge, and the bridge forgets the terminal's idle time until T3 reports it again. A runtime naming neither a pid nor a start time (`LOOPAI_T3_URL` with no readable runtime file) cannot tell a restart from a lost connection, so every connection then counts as a new server start, and an absent terminal is reopened at most once per connection.

### Task 12: Add the ACP provider protocol and the MCP thread-id client (packages only)
- [x] create `pkg/acp/provider.go` on top of `transport.go`:
  - `initialize` answers exactly the v1 response in Technical Details: no `info` key, `loadSession: true`, no `mcpCapabilities.acp`, `authMethods: []`;
  - `session/new` returns a fresh `sessionId` through a handler hook, without loading config, opening a repository, or touching the filesystem, for any cwd;
  - `session/load` registers any id and answers `{}` promptly;
  - `session/prompt` dispatches to a handler with the session id, cwd, the raw prompt blocks, and a context;
  - `session/cancel` (a notification, also answered when sent as a request) is surfaced to the handler through its own callback, separately from shutdown;
  - unknown methods get -32601;
  - no error carries code -32000, and no error text matches the auth regex;
  - the provider never sends permission requests;
  - it exposes `Shutdown` for EOF and SIGTERM.
- [x] add update encoders, sent as `session/update` notifications:
  - `plan`: entries with `content`, `priority`, `status`;
  - `tool_call`: `toolCallId`, `title`, `kind: "other"`, `status`;
  - `tool_call_update`;
  - `agent_message_chunk` with a stable `messageId`.
- [x] add the envelope parser `ParsePrompt(blocks)`:
  - take the first text block;
  - use the inner text of `<user_request>\n…\n</user_request>` when present;
  - take the text after the LAST `\n\nUser message:\n`;
  - trim it.

  Also add `IsContinuation(text)`: true when the last non-empty line equals `Continue where you left off.`.
- [x] create `pkg/t3/mcp.go`, a streamable-HTTP client per Technical Details:
  - sequence: `initialize` without a session id, capture `mcp-session-id`, `notifications/initialized`, then `tools/call t3_thread_list {"limit":1}`;
  - bodies may be JSON or SSE `data:` lines;
  - headers: `Accept: application/json, text/event-stream`, `Content-Type: application/json`, no `Origin`; an optional `DELETE` at the end;
  - the input is `{endpoint, authorization}`, and header values are never logged;
  - 401 or any failure means "no thread id".
- [x] add `MCPCredentialFromServers(mcpServers, getenv)`: the `t3-code` entry's `env` array from the given `mcpServers` wins over `T3_ACP_MCP_ENDPOINT`/`T3_ACP_MCP_AUTHORIZATION`.
- [x] write tests (success):
  - T3's exact `initialize` (protocolVersion 2 with `info`, `capabilities`, `clientCapabilities`, `clientInfo`) gets a response without `info` and with `loadSession: true`;
  - `session/new` with cwd `/` and `mcpServers: []` succeeds;
  - `session/load` with an unknown id answers `{}`;
  - each encoder emits its required fields;
  - envelope parsing covers the wrapped first prompt, a handoff history containing `User message:` twice, the restart-note continuation, plain continuation, and a raw `/command`;
  - the MCP fake (JSON and SSE replies) yields `currentThreadId`, and params credentials win over env.
- [x] write tests (errors/edge):
  - unknown methods get -32601;
  - a cancel is reported through the cancel callback and never through shutdown;
  - MCP 401/406/415 or a timeout returns "no thread id" without logging the authorization value;
  - no `initialize`/`session/new`/`session/load` reply uses error code -32000;
  - no error text matches the Go regex `(?i)\b(authenticat(e|ed|es|ing|ion)|credentials?|log([\s-]+)?in)\b`.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 13
- ⚠️ The MCP client also sends `Mcp-Protocol-Version` (the version `initialize` negotiated, `2025-06-18`) on every request after `initialize`, as T3's own bridge client (`AcpMcpStdioBridge.ts`) does and MCP `2025-06-18` requires: the effect MCP runtime in T3's v0.1.1 tree (`.repos/effect-smol`, `requiresVersionHeader: true` for `2025-06-18`) answers a session request without it with 400. The Technical Details header list omitted it.
- ⚠️ `pkg/acp/transport.go` gained `Conn.HandleUnknownRequest`, so the provider answers -32601 with the fixed text `method not found`: the transport's default echoes the method name, and `authenticate` would match T3's auth regex. Other provider error texts are fixed too and never echo client strings.
- ⚠️ `ParsePrompt` returns "" when the first text block starts with `<runtime_info>`: T3 sends that block first only for a message without text (image only).

### Task 13: Wire `loopai --acp` and rewrite loopai-acp as an exec launcher
- [x] re-add visible `ACP bool` (`long:"acp"`), routed through the Task 6 `runPreConfigCommand`:
  - `validateACPFlags` rejects a plan argument, execution flags, close-out, and the other standalone modes;
  - add `o.ACP` to `isStandaloneCommand`;
  - restore the stderr branch in `versionBannerWriter` for `--acp`, with a raw-args check before parsing.
- [x] recover `catchSIGPIPE` and `redirectStdioForACP` from commit `0f3a570` (`cmd/loopai/acp.go:107-140`) into `cmd/loopai/acp_provider.go`, so stdout carries only JSON-RPC and every human line goes to stderr.
- [x] create the binding store `cmd/loopai/acp_binding.go` under `<session cwd>/.loopai/acp/`:
  - files: `binding-thread-<runs.ThreadKey(threadID)>.json` and `binding-session-<session-id>.json` (session id checked as one safe segment);
  - schema: `{thread_id, session_id, run_id, cwd, updated_at}`, with the raw thread id kept inside the JSON;
  - written atomically with mode 0600;
  - functions `loadBinding(cwd, threadID, sessionID)`, `saveSessionThread(...)`, and `bindRun(...)`/`unbindRun(...)`, all tested here;
  - files that fail to decode, such as the deleted chat's `<session-id>.json` state, are ignored.
- [x] wire the provider in `cmd/loopai/acp_provider.go`:
  - after a non-probe `session/new` or `session/load`, look up `currentThreadId` with a 5 s bound through `var lookupACPThreadID` (stubbed in `TestMain`), and persist the session→thread mapping;
  - a call is a probe when the clientInfo name is `t3-code-provider-test`/`t3-code-session-manager`, or when `mcpServers` is empty and no `T3_ACP_MCP_*` is set;
  - `session/load` restores the binding purely from disk;
  - every prompt in this task answers the status reply (rule 4 in Technical Details) and `end_turn`.
- [x] SIGTERM or stdin EOF: exit at once, well within 1 s, without touching any run and without answering the open prompt.
- [x] rewrite `cmd/loopai-acp` as an argument-free launcher that runs `loopai --acp` for any argv:
  - keep `resolveLoopai` (`LOOPAI_ACP_LOOPAI`, then sibling `loopai`/`loopai.exe`, then `PATH`) and the `env` struct test seam;
  - Unix: `exec_unix.go` (`//go:build !windows`) defines `var platformLaunch = execReplace`, where `execReplace` calls `syscall.Exec(bin, []string{bin, "--acp"}, os.Environ())`. T3 gives the provider no process group of its own and sends SIGKILL 1 s after SIGTERM;
  - Windows: `exec_windows.go` defines `var platformLaunch = launchInherited`;
  - `launchInherited` stays in the untagged `main.go`, called in production only on Windows and exercised on Unix by the existing `TestLaunchInherited`, `TestLaunchInheritedSignalTermination`, and `TestLaunchInheritedStartError`;
  - delete `isStdioSession`, the `--version`/`inspect`/`update` probes, `resolveVersion`, the `revision` var, `TestRunVersionUsesRevision`, `TestRunProbeCommands`, and `TestRunStdioSessionShapes`;
  - drop `-X main.revision=$(REV)` from the `cmd/loopai-acp` build line in the `Makefile`;
  - rewrite the package doc and the missing-binary message so they never contain "login", "credential", or "authenticate";
  - never name the binary anything matching `claude-agent-acp` or `copilot`.
- [x] run `go test ./cmd/loopai -run TestSkillContract -update` (adds `--acp`).
- [x] write tests (success) as scripted JSON-RPC sessions over the real `--acp` wiring:
  - the probe sequence (`t3-code-provider-test`, cwd `/`, `mcpServers: []`) succeeds without config or repository access and writes no binding;
  - a non-probe `session/new` persists the session→thread mapping from the stubbed lookup;
  - a new process's `session/load` restores it from disk;
  - a binding for a ~330-character `thread:provider:…%3A…` id is stored under its hashed name and reads back the raw id;
  - `versionBannerWriter([]string{"--acp"})` is `os.Stderr` again.
- [x] write tests (success) for `cmd/loopai-acp`:
  - resolution order: env override, sibling, PATH;
  - empty argv and arbitrary argv both call the injected launch with `[bin, "--acp"]` semantics and the inherited environment.
- [x] write tests (errors/edge):
  - the MCP lookup failing falls back to the session-id key;
  - a corrupt or old-schema `.loopai/acp/<id>.json` is ignored;
  - stdout carries only JSON-RPC lines: a stray `fmt.Print` in a handler goes to stderr;
  - EOF and SIGTERM end the process promptly, with no prompt answer and no run touched;
  - `validateACPFlags` rejects a plan and `--detach`;
  - `cmd/loopai-acp` with no loopai found exits non-zero with a message free of auth words.
- [x] run `make test </dev/null`, `make lint`, `make build`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 14
- ⚠️ `pkg/acp` now calls a session's `Prompt` only after the `NewSession`/`LoadSession` hook that opened the session returned (a per-session ready channel the read loop records): a prompt pipelined after a `session/load` would otherwise route without the thread id the load restores. The Task 12 review's ordering test uses the same wait to force `Cancel` before `Prompt` instead of making it likely.
- ⚠️ Recovered panics (Task 12 review ruling) answer `session/new`/`session/load` with -32603 `internal error` and a prompt with `end_turn`; a panic in `Cancel` is recovered on the read loop. `Provider.SetErrorLog` names where the panic and its stack go (stderr under `--acp`).
- ⚠️ The status reply already carries rule 4's bound-run branch: a binding's `run_id` reads `<session cwd>/.loopai/progress/runs/<id>/outcome.json` for the status, reason, report summary, and branch. A non-probe session whose `mcpServers` has no complete `t3-code` entry (and no `T3_ACP_MCP_*`) is bound by session id with a warning line. Every binding write first installs `.loopai/acp/.gitignore` (`*`) through `git.EnsureRuntimeDirectoryIgnored`, which needs no config or repository, and is refused when it cannot: the generated `.loopai/.gitignore` ignores itself and is never committed, so a thread's worktree never has it (Task 13 review F1).
- ⚠️ The MCP session-ending DELETE is bounded at 1 s (`mcpEndTimeout`). The exit-time tests run the `--acp` child with `GORACE=atexit_sleep_ms=0`, since the race detector otherwise sleeps a second before every exit.

### Task 14: Add Mode 2 launch and routing
- [x] add hidden flags `ACPThread` (`--acp-thread`) and `ACPSession` (`--acp-session`), with no env tags:
  - `validateDetachFlags` accepts them only together with `--detach`/`--detached-run` and rejects them with `--t3-thread`;
  - they make the launcher record origin `t3-thread` with both ids, and are excluded from the recorded argv by `stripDetachArgs`.
- [x] launch through `var acpLaunchDetached` (stubbed in tests):
  - it runs `<os.Executable()> --detach --acp-thread=<tid> --acp-session=<sid> <args>` with `cmd.Dir` = the session cwd and the Task 8 environment filter, capturing its output;
  - the launcher's refusal text becomes the final message.
- [x] implement routing in `cmd/loopai/acp_route.go`, exactly as in Technical Details:
  1. **bound run.** If it is live, attach whatever the text says. If it is crashed and the text is a continuation, or it ended without success and the text is `resume`, relaunch from the recorded argv and env options, then attach.
  2. **a message naming a plan** (first token a plan path; the rest only `--task-model`/`--review-model`/`--external-reviewers`, with values validated by `validatePassThroughValues`) starts that plan's own detached run, binds it, and attaches. It never attaches to another run.
  3. **no binding.** Only continuation text may adopt, and only a live run of origin `t3-thread` whose `cwd` equals the session cwd and whose `attach.lock` is free. Exactly one candidate is bound and attached; otherwise the reply lists the candidates.
  4. **otherwise** a status reply.

  In this task, "attach" holds `attach.lock` (non-blocking, bounded 2 s) and waits on the observer until finish, then answers with a plain outcome line. The rendering comes in Task 15.
- [x] write tests (success) with scripted sessions, a fake registry, and the stubbed launcher:
  - a plan message starts and binds a run with origin `t3-thread` and both ids;
  - a new process after idle reaping (`session/load`, then `resume`) relaunches the bound failed run with the recorded argv;
  - continuation after a restart, with and without the restart note, re-attaches the bound live run;
  - continuation with no binding adopts exactly one unattached live `t3-thread` run from this cwd;
  - a status question after the run gets the outcome summary and the model-switch hint.
- [x] write tests (errors/edge):
  - a plan message never adopts another run, even a live one from the same cwd;
  - continuation with two candidates lists both and binds neither;
  - a run of origin `cli` or `t3-terminal`, from another cwd, or held by another provider's `attach.lock` is never adopted;
  - an unparsable flag in a plan message gets the usage text, not a run;
  - `--acp-thread` without `--detach`, or together with `--t3-thread`, is rejected.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 15
- ⚠️ adoption (rule 3) also skips a run recorded for another thread id when both ids are known, which never blocks the lookup-failed recovery; a relaunch restores the recorded env options over the provider's own; a plan path is a first word ending in `.md`.

### Task 15: Add Mode 2 attach rendering, the final message, and Stop
- [x] attach rendering through the observer:
  - send the plan entries at once: tasks plus stages from `attempt_start`, with priority `medium`;
  - open one `tool_call` (kind `other`) for the current phase;
  - close, open, or retitle it and update the plan only on transitions;
  - retitle with the elapsed time every 20 minutes without a transition;
  - stream no reasoning and no executor output.
- [x] final message on finish:
  - close the open tool call (`completed` or `failed`);
  - send one `agent_message_chunk` with one `messageId`, containing the summary, risk, outcome (status, reason), branch, and the full report path (`<run dir>/report.md`);
  - cap it so the JSON-escaped UTF-8 text is ≤ 12,000 bytes, truncating at a rune boundary with a pointer to the report path;
  - then answer `{"stopReason":"end_turn"}`.
- [x] Stop, with the grace held in one constant, `acpStopGrace = 5 * time.Second`. Its basis (steering arrives as cancel then prompt) is bundle-verified but not confirmed live, so a live mismatch only changes this constant or this handler.
  - On `session/cancel`, answer the open prompt `{"stopReason":"cancelled"}` at once and start the grace timer.
  - A new `session/prompt` for the same session within the window cancels the pending stop (steer or restart).
  - Otherwise stop the run through `stopRun`, but only when it is this thread's bound run with origin `t3-thread` and a matching thread id. The worktree is kept, and `resume` relaunches the run.
  - SIGTERM or EOF during the window abandons the pending stop.
- [x] write tests (success):
  - the plan and tool-call updates arrive with `priority` and `title`;
  - only transitions and the 20-minute liveness tick (injected clock) send updates;
  - the final message precedes the `end_turn` response;
  - cancel followed by a prompt within the grace window keeps the run, and the new prompt re-attaches it;
  - cancel without a prompt stops this thread's own `t3-thread` run, which classifies as `stopped` with a kept worktree.
- [x] write tests (errors/edge):
  - cancel never stops another thread's run or a `cli` run, even when it is the only live run in the cwd;
  - a final message above 12,000 bytes (ASCII and Cyrillic) is truncated with the report path kept;
  - a crashed run ends the turn with the crash status and no hang;
  - SIGTERM during the grace window leaves the run running.
- [x] run `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 16
- ⚠️ An external review's evaluation phase stays on the external review's tool call (retitled, not a new card), so each reviewer/evaluator iteration does not open two cards; a limit wait retitles the card it interrupts.
- ⚠️ The Stop guard reads `t3_thread_read` twice (thread state, then the latest 50 activity items), reading the session's latest MCP credential when the grace ends; a reply without an `items` list counts as the item kind being unavailable and stops.
- ⚠️ The final message names `<run dir>/report.md` only when that file exists (a run without a report, such as tasks-only or an early failure, writes none); otherwise the line is omitted and the truncation pointer names the run directory instead of a missing report.

### Task 16: Rewrite the T3 skills, checks, and contract
- [ ] rewrite `assets/claude/skills/loopai-t3/SKILL.md` around the mode question, per Technical Details:
  - keep the frontmatter `name`; write a new description; keep `argument-hint`; add `mcp__t3-code__orchestrator_capabilities`, `mcp__t3-code__t3_thread_list`, `mcp__t3-code__t3_thread_launch`, and `ToolSearch` to `allowed-tools`;
  - the contract snippet stays the first fenced block;
  - keep the Step 0b flag rules, with reworded rejects naming `loopai --detach` and the loopai provider;
  - keep the launch-history paragraph and bash block byte for byte, appending "(the mode question in Step 3 still follows)" after the pinned "then launch with the empty `FLAGS` as before, without asking";
  - keep Step 1;
  - replace the token, launch, report, close-out, and pitfalls steps with the Mode 1/Mode 2 flows;
  - keep "STOP HERE after reporting. Do not monitor automatically."
- [ ] add the report blocks, byte-identical on both hosts, from Technical Details "Skill report blocks":
  - Mode 1: run id, `loopai --attach <run-id>`, that the terminal tab and the `[loopai: …]` title prefix show progress, and that running this skill again for the same plan resumes a failed or stopped run with a fresh token;
  - Mode 2: the thread link `[title](t3-thread://v1/<threadId>)`, typing `resume` in that thread after Stop or a failure, and switching that thread's model to Claude or Codex after the report.
- [ ] rewrite `assets/codex/skills/loopai-t3/SKILL.md` and `agents/openai.yaml` (display_name, short_description, default_prompt for the two modes):
  - same flows and report blocks, using `mcp__t3_code__*` names;
  - T3's bounded direct attempt rule (in code mode, `tools.mcp__t3_code__orchestrator_capabilities({})`);
  - numbered-list questions, and no Claude-only tokens;
  - delete `references/current-thread.md`, `scripts/observe.py`, and `scripts/resolve-thread.py`, keeping every instruction in SKILL.md, the only file the checker scans.
- [ ] update Step 3 of both `loopai-plan` skills:
  - offer "Run in T3 Code now" only when the T3 MCP tools are present (deferred names count in Claude) or one bounded capabilities call succeeds (Codex);
  - it hands over to `loopai-t3` and its mode question, not a flag re-ask;
  - delete the Codex current-thread label, the preference override, and the `--t3-launch` text (lines 188-197);
  - keep every phrase `check-launch-history-skills_test.sh` pins.
- [ ] update the Codex `loopai` skill (lines 36-42): a T3 plan run goes to `$loopai-t3` and its mode question; status questions use `loopai --runs` and `loopai --attach`; remove the observer and native-subagent wording.
- [ ] add bullets to both `loopai-update` removed-key lists: `t3` and `acp_auto_merge` "were removed". T3 visibility now comes from `loopai-t3`, and merges go through `loopai --merge` or `loopai-merge`.
- [ ] add identical `removed_spellings` patterns to `scripts/check-symlinks.sh` and `scripts/check-codex-skills.sh`:
  - patterns: `'--t3([^-[:alnum:]_]|$)'`, `'--no-t3'`, `'--t3-launch'`, `'--t3-progress-thread'`, `'acp_auto_merge'`;
  - never a bare `--t3` prefix, which would reject `--t3-thread`;
  - extend the fixtures in `check-symlinks_test.sh` and `check-codex-skills_test.sh`, including a case proving `--t3-thread` passes.
- [ ] rewrite `scripts/check-loopai-t3-skill_test.sh` (still run by `test-codex-skills`) to assert the new contract for both hosts:
  - the mode question offers both modes and Cancel;
  - Mode 1:
    - uses `orchestrator_capabilities` `parentThreadId`, cross-checked with `t3_thread_list` `currentThreadId`;
    - checks the `runtimeMode` `full-access` gate and the sandbox and Windows refusals;
    - validates the thread id with `^[A-Za-z0-9_.:%-]{1,1024}$` and single-quotes it;
    - uses the least-privilege inline mint with `--ttl 3d`, and `loopai --detach --t3-thread`;
  - Mode 2:
    - uses the read-only `settings.json` instance lookup;
    - calls `t3_thread_launch` with `title`, with `modelSelection` from `models` entry 0, and with `workspaceStrategy` `existing_worktree`/`root`;
    - includes the registration text;
  - no `--t3-launch`, `--t3-progress-thread`, `--no-t3`, `resolve-thread`, or `current-thread`;
  - the report blocks are identical across hosts and contain the pinned phrases `loopai --attach`, `fresh token`, ``type `resume` in that thread``, and `switch that thread's model to Claude or Codex`.
- [ ] delete `scripts/t3-observer_test.py` and `scripts/t3-thread-resolver_test.py`, and remove the `test-t3-observer` target from the `Makefile`: target lines 69-71, the `test:` prerequisites on line 102, and `.PHONY` on line 163.
- [ ] bump the contract:
  - set `const skillContract = 4`, in the exact one-line form the checkers parse;
  - set `expected=4` in every `expected=` line of all 19 skills (10 Claude, 9 Codex);
  - run `go test ./cmd/loopai -run TestSkillContract -update`;
  - bump `.claude-plugin/plugin.json` and the loopai entry in `.claude-plugin/marketplace.json` to `0.9.0`; codex-imagegen stays unchanged.
- [ ] write and extend tests:
  - the rewritten `check-loopai-t3-skill_test.sh` fails on a fixture still containing `--t3-launch` and on one missing a report phrase;
  - the removed-spellings fixtures reject `--no-t3` and `acp_auto_merge` lines and accept "were removed" lines;
  - `make check-plugin test-plugin` passes with equal versions.
- [ ] run, in order, and they must pass before task 17:
  1. `make check-symlinks test-symlinks check-plugin test-plugin test-launch-history-skills test-skill-contract test-report-docs check-codex-skills test-codex-skills test-install </dev/null`;
  2. `scripts/install-codex-skills.sh --dry-run --dest "$(mktemp -d)"`;
  3. the guard `grep -rn -E -- '--t3-launch|--t3-progress-thread|--no-t3|resolve-thread|current-thread|observe\.py|t3-current' assets/ scripts/ Makefile` (only "were removed" lines and checker patterns may match);
  4. `make test </dev/null`, `make lint`, and `GOOS=windows GOARCH=amd64 go build ./...`.

### Task 17: Verify acceptance criteria
- [ ] verify all requirements from the Overview are implemented:
  - a detached run survives its launcher: the launcher exits and the child keeps running in its own session;
  - `--attach`/`--runs`/`--stop` work from another shell;
  - resume reuses the id with a new attempt and keeps the origin;
  - Mode 1 titles and terminal;
  - Mode 2 provider routing and the final message.
- [ ] verify the anti-goals in code:
  - no token in the descriptor, outcome, journal, console log, progress log, or child environment (grep the writers; tests assert it);
  - the worktree is kept on every non-success exit and on a success with an incomplete close-out or a dirty tree;
  - no detached-run code path commits through `MainGitSvc`;
  - title updates keep the user's base name;
  - only transitions send title, plan, or tool-call updates;
  - a Mode 2 thread never stops or shows another thread's run;
  - `--merge`/`--pr` check every progress root and the feature worktree's run lock.
- [ ] verify the removal in code and assets:
  - `git grep -n -E 'thread\.meta\.update|LOOPAI_T3_THREAD_ID|env:"LOOPAI_T3"|ModeACPChat|WithResumeSession|ACPMerge|acp_chat\.txt' -- cmd pkg scripts assets Makefile` finds nothing, except lines saying "was removed" or "were removed";
  - `pkg/t3` contains only the protocol-2 client, `t3test`, and the MCP client;
  - `cmd/loopai-acp` only runs `loopai --acp`.
- [ ] verify edge cases: plan-chain, Windows, and sandbox refusals; concurrent launches; `--merge`/`--pr` refused while a run is live; the continuation and steering paths.
- [ ] run full test suite (`make test </dev/null`)
- [ ] run e2e tests if affected (`make e2e`; skip with a note if no dashboard file changed)
- [ ] run linter (`make lint`) - all issues must be fixed
- [ ] cross-compile `GOOS=windows GOARCH=amd64 go build ./...` and `make build`
- [ ] verify that test coverage of `pkg/runs`, `pkg/t3`, `pkg/t3/t3test`, `pkg/acp`, and the new `cmd/loopai` files meets 80%+ (`go test -cover`)

### Task 18: [Final] Update documentation
- [ ] rewrite `docs/t3-code.md`:
  - detached runs: `--detach`, `--attach`, `--runs`, `--stop`, the run directory, resume and origin preservation, and the worktree kept on non-success or an incomplete close-out;
  - archive on the run branch;
  - Mode 1: the token mint with scopes, the terminal, the title prefix, the pin, restart behaviour, and resume by re-running the skill;
  - Mode 2: registration through the settings UI (strings in Technical Details, recommended instance id `loopai`, never hand-edit `settings.json`), routing, continuation, Stop and `resume`, and the model switch after the run;
  - `--merge`/`--pr` refusal during a live run;
  - limitations: Windows, sandbox, the Linux slice, lid-closed sleep, the Mode 2 run environment being T3's, the terminal history cap, and the items still unverified live;
  - removal of the old `--t3`/`--t3-launch`, ACP chat, settlement, PR linking, and the `t3.json` `--t3` actions.
- [ ] update `README.md`:
  - features, install (`loopai-acp`), and the skill list entries. Keep the check-report-docs blocks intact, naming only existing skills: from "The plugin provides <N> skills:" to "Use `/loopai:loopai-merge", and from "<N> skills are installed:" to "Invoke them as";
  - the Codex current-thread paragraph, the launch-history paragraph, and the env vars (remove `LOOPAI_T3`, `LOOPAI_T3_THREAD_ID`);
  - the `--acp` keep-awake note, the T3 section, the cmux env list, and the resumed-ACP-chat sentence;
  - every link to the removed anchors `#keep-the-current-codex-thread`, `#limitations`, `#experimental-loopai-as-a-t3-code-provider`, pointed at the new headings.
- [ ] update the `llms.txt` T3/ACP lines and flags. Keep the "The plugin provides" … "The CLI remains" and "It installs" … "`loopai-grill` stays" blocks, and "Manually invoke"/"explicit `$loopai-retro`".
- [ ] update `CLAUDE.md`:
  - the project-structure lines for `cmd/loopai-acp`, `pkg/acp`, `pkg/t3` (with `t3test`), the new `pkg/runs`, and the phase package;
  - replace the loopai-t3/current-thread, T3 reporting, settlement, and ACP agent-mode/chat paragraphs with the detached-run, Mode 1, and Mode 2 architecture: hidden `--detached-run`, handshake fds, journal and observer, knobs on `executePlanRequest`, the keep-worktree decision, the `--merge`/`--pr` guard, the binding store with hashed thread keys, and the TestMain stubs;
  - update the removed-spellings sentence, the launch-history hook paragraph (the child records with `launcherForOrigin`), and the codex resume lines;
  - keep every check-report-docs anchor ("The current set is" … "; every added skill", and the report-contract phrases).
- [ ] update `docs/backlog/preserve-review-worktree-on-failure.md`: detached runs keep their worktree on every non-success exit, while foreground `--worktree` runs are unchanged. Update the embedded config comments if any still mention `t3` or ACP auto-merge.
- [ ] verify the docs removal: `git grep -n -E 'thread\.meta\.update|LOOPAI_T3_THREAD_ID|LOOPAI_T3\b|--t3-launch|--t3-progress-thread|ModeACPChat|acp_chat\.txt|acp_auto_merge' -- docs README.md llms.txt CLAUDE.md ':!docs/plans/completed' ':!CHANGELOG.md'` matches only lines that say "was removed" or "were removed".
- [ ] run `make test-report-docs </dev/null`, `make test </dev/null`, and `make lint` - must pass

## Technical Details

### Run directory (`<invoking checkout>/.loopai/progress/runs/<run-id>/`)
- Files:
  - `run.json`: the descriptor;
  - `run.lock`: the lifetime lock, held by the child for its whole life and probed non-blocking by observers. It is separate from the worktree run lock;
  - `events.jsonl`: the journal;
  - `outcome.json`: the current attempt's outcome, with `outcome-attempt-<n>.json` as history;
  - `report.md`;
  - `console.log`: the child's stdout and stderr across all attempts, with a separator line per attempt;
  - `stop-requested`: the stop marker;
  - `attach.lock`: Mode 2 attachment;
  - `source-plans.json`.
- Launch lock: `<runs dir>/.launch-<first 16 hex of sha256(planRel + "\x00" + branch)>.lock`.
- No file matches `progress-*.txt`, which the dashboard discovers recursively.
- Run id: `<sanitized plan stem>-<YYYYMMDD-HHMMSS>-<4 hex>`, matching `^[a-z0-9][a-z0-9._-]{0,99}$`.
- Status values: `starting`, `running`, `succeeded`, `failed`, `stopped`, `crashed`.
- Exit codes: 0 succeeded, 1 failed (or crashed, for `--attach`), 2 stopped.

### Keep-worktree decision (detached runs only)
- Before `AttemptStarted` succeeds, today's behaviour holds: a fresh worktree is removed on a setup
  failure.
- After it, the worktree is kept on:
  - every non-success: task failure, the task-phase iteration cap, stop or cancel, or an error;
  - a success where the finalize close-out is incomplete, the plan archive is incomplete, the
    worktree is dirty (`IsDirtyAll`), or a Git operation is in progress.
- It is removed only on a clean success.
- A review stalemate or review max-iterations is not a failure: the post-review loop commits leftovers
  and the run proceeds.

### Descriptor schema (`run.json`, mode 0600, never contains a token)
```json
{
  "version": 1,
  "id": "feature-x-20261022-153000-a1b2",
  "attempt": 2,
  "created_at": "2026-10-22T15:30:00Z",
  "updated_at": "2026-10-23T09:12:00Z",
  "pid": 41231,
  "pid_start": "1729610400.123456",
  "executable": "/Users/u/.local/bin/loopai",
  "argv": ["--task-model", "codex:gpt-6-astra:high", "docs/plans/20261022-feature-x.md"],
  "env_options": {"LOOPAI_CONFIG_DIR": "/Users/u/alt"},
  "cwd": "/Users/u/repo",
  "mode": "full",
  "plan": "docs/plans/20261022-feature-x.md",
  "branch": "feature-x",
  "progress_log": "/Users/u/repo/.loopai/progress/progress-feature-x.txt",
  "worktree": "/Users/u/repo/.loopai/worktrees/feature-x",
  "console_log": "/Users/u/repo/.loopai/progress/runs/feature-x-20261022-153000-a1b2/console.log",
  "origin": {"kind": "t3-thread", "thread_id": "8e8b69fc-3359-47c8-a5a3-6dd91c245a86", "acp_session_id": "sess_123"}
}
```
- `argv` is `stripDetachArgs(raw argv)`:
  - removed: `--detach`, plus `--detached-run`, `--t3-thread`, `--acp-thread`, and `--acp-session`,
    each in both its `=v` and its `v`-as-next-token form;
  - kept: explicit empty values, and everything after `--`.
- `env_options` holds the set values of `cmuxEnvOptions` (`LOOPAI_CONFIG_DIR`, `LOOPAI_ORCA`,
  `LOOPAI_WEB_HOST`).
- `origin.kind` is one of:
  - `cli`;
  - `t3-terminal`, with `thread_id`;
  - `t3-thread`, with `thread_id` and `acp_session_id`.
- A new id takes its origin from the launch flags. A resume keeps the recorded origin unless the
  launch passes `--t3-thread`, `--acp-thread`, or `--acp-session`.

### Outcome schema (`outcome.json`)
```json
{
  "version": 1, "run_id": "…", "attempt": 2,
  "status": "failed", "reason": "runner: task 3: …", "exit_code": 1,
  "branch": "feature-x", "commit": "db78b1ba…", "worktree_kept": "/Users/u/repo/.loopai/worktrees/feature-x",
  "report": "# Report: …", "report_summary": "…", "risk": "medium",
  "finalize": {"mode": "sync", "status_note": "synced", "pr_url": "", "merged": false, "incomplete": ""},
  "plan_moved": false, "archive_incomplete": "", "notes": [], "finished_at": "2026-10-23T09:12:00Z"
}
```
- `report` holds `Runner.Report()`, capped at 256 KiB.
- `report.md` holds the same text uncapped. A sidecar may not exist (tasks-only,
  `move_plan_on_completion = false`), and runs on later attempts must not lose the report.
- `report_summary` is at most 4 KiB, taken from the `Summary` section.

### Journal (`events.jsonl`)
- One JSON object per line: `{"seq": N, "time": RFC3339Nano, "attempt": k, "type": "…", …}`.
- Event types and their fields:

  | type | fields |
  |---|---|
  | `attempt_start` | mode, plan, branch, worktree, worktree_plan, progress_log, console_offset, tasks:[{n,title}], task_total, stages:[…] |
  | `phase` | phase, wait_seconds? |
  | `task` | n, m, title |
  | `tasks` | statuses:[{n,status}] |
  | `review` | kind, iteration, label |
  | `finalize` | mode, status_note, pr_url, merged, incomplete |
  | `report` | summary, risk |
  | `finish` | status, reason, exit_code |

### Handshake (launcher ↔ child, Unix)
- **fd 3** (`ExtraFiles[0]`), launcher to child: one JSON line `{"t3_token":"…"}` (the token is empty
  without `--t3-thread`), then EOF.
- **fd 4** (`ExtraFiles[1]`), child to launcher: exactly one JSON line, either
  `{"ok":true,"run_id":"…","attempt":2}` or `{"ok":false,"error":"…"}`.
- Child rules:
  - call `syscall.CloseOnExec(3)` and `syscall.CloseOnExec(4)` before anything else, and close both
    after use;
  - success is the explicit `ok` message, never EOF;
  - writes to a pipe on fd 3/4 return EPIPE, not SIGPIPE.
- Child process setup:
  - `setsid`; stdin is the null device; stdout and stderr go to `console.log` (`O_APPEND`);
  - the environment has no `T3_ACP_MCP_*`, `LOOPAI_T3_TOKEN`, or `CMUX_*`;
  - SIGHUP is handled via `signal.Notify`.
- Linux with a systemd user manager (decided by `systemdScopePrefix`, replaceable through
  `detachScopePrefix`): `systemd-run --user --scope --quiet --collect -- <exe> <args>`.

### --merge/--pr guard
- Runs after the feature branch is resolved, in both `runMergeCommand` and `runPRCommand`.
- Check 1: for each root in `progressRecordRoots(gitSvc)`, `runs.LiveRunForBranch(root, feature)`. A
  live run means refuse with `a detached run of <branch> is live: <id>; stop it with loopai --stop <id>`.
- Check 2: find the `git.Worktree` whose `Branch == feature`, open a `git.Service` on its `Path`, and
  call `AcquireWorktreeRunLockContext`:
  - `ErrWorktreeBusy` means refuse, with the same message minus the id;
  - a successful acquire is released at once.
  - The probe also catches a foreground `--worktree` run, which is correct.

### Removed spellings
- `--t3`: "--t3 was removed; show a run in a T3 thread with loopai --detach --t3-thread <thread-id> or the loopai-t3 skill"
- `--no-t3`: "--no-t3 was removed; omit it, loopai reports to T3 only with --t3-thread"
- `--t3-progress-thread`: "--t3-progress-thread was removed; use loopai --detach --t3-thread <thread-id>"
- `--t3-launch[=…]`: "--t3-launch was removed; use the loopai-t3 skill (a terminal in this thread or a new loopai thread)"
- key `t3`: `use loopai --detach --t3-thread <thread-id> or the loopai-t3 skill`
- key `acp_auto_merge`: `merge with loopai --merge or the loopai-merge skill`
- `LOOPAI_T3` loses its env tag and is inert. `LOOPAI_T3_TOKEN` and `LOOPAI_T3_URL` stay live inputs.

### T3 WebSocket protocol 2 (Mode 1 bridge)
- **Origin and endpoint**
  - The origin comes from `${T3CODE_HOME:-~/.t3}/userdata/server-runtime.json` =
    `{version:1, pid, ownerId?, host?, port, origin:"http://127.0.0.1:<port>", devUrl?, startedAt, serviceManaged?, serverVersion?}`.
    It is re-read on every reconnect, and `LOOPAI_T3_URL` overrides it.
  - Endpoint `ws://host:port/ws?orchestrationProtocol=2`, with the header
    `Authorization: Bearer <token>`.
- **Errors at connect**
  - The protocol check precedes auth. A mismatch is HTTP 426
    `{"code":"orchestration_protocol_incompatible","message":"Update this client to one that supports orchestration protocol 2.","orchestrationProtocolVersion":2}`.
  - An auth failure is 401
    `{"code":"auth_invalid","reason":"missing_credential"|"invalid_credential",…}`.
  - The socket closes when the auth session is invalidated (expiry or revoke).
- **Framing**: plain JSON, one message per text frame, no JSON-RPC envelope.
  - Client messages:
    - Request: `{"_tag":"Request","id":<number|string>,"tag":"<method>","payload":{…},"headers":[]}`;
    - `{"_tag":"Ack","requestId":<id>}` after EVERY Chunk, because the server waits on it;
    - `{"_tag":"Interrupt","requestId":<id>}` to cancel a stream;
    - `{"_tag":"Ping"}`, answered with `{"_tag":"Pong"}`;
    - `{"_tag":"Eof"}`.
  - Server messages:
    - `{"_tag":"Chunk","requestId":<id>,"values":[…]}`;
    - `{"_tag":"Exit","requestId":<id>,"exit":{"_tag":"Success","value":<v>}}`, with no `value` for Void
      and for finished streams;
    - `{"_tag":"Exit","requestId":<id>,"exit":{"_tag":"Failure","cause":[{"_tag":"Fail","error":{"_tag":"<ErrorTag>",…}}|{"_tag":"Die","defect":…}|{"_tag":"Interrupt",…}]}}`;
    - `{"_tag":"Defect","defect":…}`;
    - `{"_tag":"Pong"}`.
  - `requestId` echoes the JSON type of the request id.
- **Scopes**: `orchestration:read`, `orchestration:operate`, `terminal:read`, `terminal:operate`. A
  missing scope yields
  `EnvironmentAuthorizationError{message:"The authenticated token is missing required scope: <s>.", requiredScope}`.
- **`terminal.open`** (terminal:operate)
  - Payload: `{threadId, terminalId (≤128, client-chosen), cwd (existing directory), worktreePath?: string|null, cols? 1..1000 (120), rows? 1..500 (30), env?: {NAME: value} (≤128 keys, names ^[A-Za-z_][A-Za-z0-9_]*$, values ≤8192), providerInstanceId?}`.
  - Success: `{threadId, terminalId, cwd, worktreePath|null, status:"starting"|"running"|"exited"|"error", pid|null, history, exitCode|null, exitSignal|null, label, updatedAt, sequence?}`.
  - On an existing live session, a different `cwd`, normalized `env`, or `worktreePath` stops the
    process, clears history, and restarts. An identical launch context only resizes it.
  - An unknown id creates a fresh shell seeded from the persisted history.
  - Never send `providerInstanceId`; never use `term-1`.
- **`terminal.write`** (terminal:operate)
  - Payload `{threadId, terminalId, data}`, with `data` non-empty and ≤65536. Returns Void.
  - `TerminalNotRunningError` is returned while the terminal is `starting` or `error`; data written
    once it has `exited` is dropped. `TerminalSessionLookupError` and `TerminalWriteError` mean the
    terminal is gone.
  - UI convention: ` ${command}\r`. The leading space keeps the command out of shell history.
- **`terminal.close`** without `terminalId` closes EVERY terminal of the thread. The bridge never
  calls it.
- **`subscribeTerminalMetadata`** (terminal:read, a stream, NOT auto-acked)
  - Payload `{}`.
  - Items:
    - `{type:"snapshot", terminals:[TerminalSummary]}`, always first;
    - `{type:"upsert", terminal}`;
    - `{type:"remove", threadId, terminalId}`.
  - TerminalSummary =
    `{threadId, terminalId, cwd, worktreePath|null, status, pid|null, exitCode|null, exitSignal|null, hasRunningSubprocess, label, updatedAt}`.
  - `hasRunningSubprocess` can lag by about 30 s. Filter by threadId and terminalId on the client
    side.
- **`orchestration.dispatchCommand`** (orchestration:operate)
  - Payload: one command. Success `{sequence}`.
  - Error: `OrchestrationV2DispatchCommandError{commandId, commandType, message, detail?}`.
  - Use a fresh `commandId` per logical update; a reused id replays the old receipt. Commands:
    - `{"type":"thread.metadata.update","commandId":"…","threadId":"…","title":"…"}`, title only.
      Omit `regenerateTitle`, `branch`, `worktreePath`, `expected*`, `limitRecovery`, and
      `linkedPullRequest`;
    - `{"type":"thread.pin","commandId":"…","threadId":"…"}`;
    - `{"type":"thread.unpin","commandId":"…","threadId":"…"}`.
  - Never send `createdBy`/`creationSource`/`createdAt`. Pin or unpin on an archived thread fails.
- **`orchestration.getThreadProjection`** (orchestration:read)
  - Payload `{threadId}`.
  - Read `thread.title`, `thread.pinnedAt` (absent = null), `thread.worktreePath`,
    `thread.projectId`, `thread.archivedAt`, and `thread.titleRegeneration`.
  - Error: `OrchestrationV2GetThreadProjectionError{threadId, message}`.
  - A 426 or a reply that fails to decode is a typed "protocol unsupported" error that disables the
    bridge, never a crash.
- **`GET /api/projects`** (Bearer, orchestration:read; no protocol header needed)
  - Returns `{projects:[{id, title, workspaceRoot, …, deletedAt}], updatedAt}`.
  - Match `id == thread.projectId`.
- **Restarts**
  - Terminal sessions live only in server memory. After a restart the metadata snapshot lacks the
    bridge's terminal. A UI re-attach is not guaranteed (it needs that client's localStorage and the
    thread being viewed), so the bridge re-opens its own terminal (see Mode 1 bridge behaviour).
  - A UI re-attach uses `terminal.attach`, never a reset, with cwd = `worktreePath ?? workspaceRoot`
    and env `{T3CODE_PROJECT_ROOT, T3CODE_WORKTREE_PATH?}`.
  - The terminal shell inherits the T3 server environment minus `T3CODE_*`, `VITE_*`, and a few
    multiplexer variables, so the follow command uses absolute paths.

### Mode 1 bridge behaviour
- **Title prefix**
  - States: `task <n>/<m>`, `review`, `external review`, `finalize`, `report`, `limit wait`, and the
    final `done` (succeeded), `failed`, or `stopped`.
  - Format: `[loopai: <state>] <base>`. Strip regex: `^\[loopai: [^\]]*\] ?`. When the base is empty,
    the title is the prefix alone.
  - Titles have no compare-and-set, and any title write clears a pending `titleRegeneration`. Before
    the first prefix write, wait (bounded, about 2 minutes) until `titleRegeneration` is null. A later
    regeneration is simply overwritten at the next transition.
- **Pin**: pin at start only when `pinnedAt` is null, to avoid the 3-day auto-settle (the pin protects
  in code, contrary to T3's user docs). Unpin at finish only if the bridge pinned.
- **Terminal state machine**
  - On the first snapshot: if the terminal is present, reuse it; if absent, call `terminal.open` once
    with the UI cwd/env, then write the follow command.
  - After a reconnect where the server restarted (runtime pid or `startedAt` changed, or the
    connection was lost): if the terminal is absent and no `remove` for it was seen during a live
    connection, call `terminal.open` ONCE with the identical UI launch context (cwd =
    `thread.worktreePath ?? project.workspaceRoot`, `worktreePath` only when non-null, the same
    two-key env, no `providerInstanceId`). Wait until it reports running, retrying on
    `TerminalNotRunningError` within a bound, then type the follow command. This replaces the earlier
    "never re-open, wait for the UI" rule, because a UI re-attach is not guaranteed.
  - A `remove` seen on a live connection means the user closed the terminal; never recreate it and
    never re-type.
  - Re-type only when the terminal is running, `hasRunningSubprocess` has been false for ≥60 s, and
    nothing was typed in the last 90 s.
  - At most one `terminal.open` per server start. Never `exec` the follow command and never append
    `; exit` to it.
- **Failure**: a 401 `auth_invalid` (expired or revoked token), a 426 or undecodable reply ("protocol
  unsupported"), or another unrecoverable error disables the bridge with one progress-log line. The
  run is unaffected.
- **Thread id**: an opaque string matching `^[A-Za-z0-9_.:%-]{1,1024}$`. It is sent unchanged in
  payloads and always single-quoted in shell text.

### Token mint (skill side, Mode 1)
```bash
T3_CLI="${T3CODE_CLI_PATH:-${T3CODE_HOME:-$HOME/.t3}/bin/t3}"
LOOPAI_T3_TOKEN=${LOOPAI_T3_TOKEN:-$("$T3_CLI" auth session issue --scope orchestration:read --scope orchestration:operate --scope terminal:read --scope terminal:operate --ttl 3d --label loopai --token-only)} \
  loopai --detach --t3-thread "$THREAD_ID" $FLAGS "$PLAN"
```
- Stop when `$T3_CLI` is missing. There is no `PATH` `t3` and no `npx` fallback.
- The token is never echoed.
- `--scope` replaces the default set, which includes settings and provider management.
- No revoke at finish; the token expires after 3 days.
- `$THREAD_ID` matches `^[A-Za-z0-9_.:%-]{1,1024}$` and is always passed quoted.

### ACP v1 surface (Mode 2 provider, newline-delimited JSON-RPC 2.0 on stdio)
- **`initialize`**
  - T3 sends:
    `{"protocolVersion":2,"info":{"name":"t3-code","version":"0.0.0"},"capabilities":{"elicitation":{"form":{},"url":{}}},"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false,"elicitation":{"form":{},"url":{}}},"clientInfo":{"name":"t3-code","version":"0.0.0"}}`
  - loopai answers exactly:
    `{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"promptCapabilities":{"image":false,"audio":false,"embeddedContext":false},"mcpCapabilities":{"http":false,"sse":false}},"authMethods":[],"agentInfo":{"name":"loopai","version":"<version>"}}`
  - Never an `info` key, never `mcpCapabilities.acp`, no `sessionCapabilities`, no models.
- **`session/new`**: params `{cwd, mcpServers, additionalDirectories?}` → `{sessionId}`. The id must
  be stable and unique; T3 stores it as the native thread id.
- **`session/load`**: params `{sessionId, cwd, mcpServers}` → `{}`.
  - T3 allows 90 s.
  - Updates sent during load are suppressed.
  - It must never fail.
  - A failed or slow load does not fail the turn: T3 falls back to a new process with `session/new`
    (a new session id) and prepends a `Context handoff (full_thread_summary):` block (up to 16k
    tokens) before `\n\nUser message:\nContinue where you left off.`. Without `loadSession` advertised,
    the continuation fails. The MCP thread id is therefore the required binding key.
- **`session/prompt`**: params `{sessionId, prompt:[ContentBlock]}` →
  `{"stopReason":"end_turn"|"cancelled"}`. T3 maps `cancelled` to interrupted. Never return a
  usage-limit-shaped failure.
- **`session/cancel`** notification:
  `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"…"}}`.
  - Stop sends one and waits ≤10 s for `cancelled`; the same process and session serve the next
    prompt.
  - Steer, promote-to-steer, and MCP `t3_thread_send` (auto) send one, wait for the prompt to end, then
    send a new `session/prompt`. Only a Stop records a `run_interrupt_request`. v0.1.2 previews also
    cancel when T3's own event ingestion fails a run.
- **Auth classification by T3**: error code -32000, or detail matching
  `/\b(?:authenticat(?:e|ed|es|ing|ion)|credentials?|log(?:[\s-]+)?in)\b/iu`, marks the instance
  unauthenticated. The Go equivalent is
  `(?i)\b(authenticat(e|ed|es|ing|ion)|credentials?|log([\s-]+)?in)\b`.
- **`mcpServers` entry injected by T3**:
  `{"name":"t3-code","command":<T3 executable>,"args":[…,"acp-mcp-bridge"],"env":[{"name":"ELECTRON_RUN_AS_NODE","value":"1"},{"name":"T3_ACP_MCP_ENDPOINT","value":<endpoint>},{"name":"T3_ACP_MCP_AUTHORIZATION","value":"Bearer <token>"}]}`
  (no `type` key). It is a stdio bridge, never a URL entry; advertising `mcpCapabilities.acp` would
  make T3 send an entry with no URL instead.
  - The same values are in the spawn environment, frozen at spawn time. Prefer the latest params.
  - Never log header values.
  - The credential is touched hourly during a running run; issuing a new one revokes the old.
- **Probe**
  - `initialize` with `clientInfo {"name":"t3-code-provider-test"}`, then `session/new` with
    cwd = the T3 server cwd and `mcpServers: []`.
  - T3 waits ≤500 ms for `available_commands_update` and has a 60 s timeout. Readiness is refreshed
    about every minute while provider status is in demand; only `ready` is cached (15 minutes), and
    failures retry every minute.
  - The session manager uses clientInfo `t3-code-session-manager`.
  - `t3-code-provider-auth` sends `initialize` only, then EOF. That connection has no side effects and
    no error output.
  - A connection is a probe when its clientInfo name is one of these three, or when `mcpServers` is
    empty and no `T3_ACP_MCP_*` is set.
- **Process lifetime**
  - `loopai --acp` never exits by itself during a held turn: handler panics are recovered and serving
    continues, because a provider that dies while T3 is up has its turn finalized `failed` and never
    continued.
  - It exits promptly on SIGTERM or stdin EOF.
  - On macOS the provider leads its own process group, which T3 signals as a whole (SIGTERM, then
    SIGKILL after 1 s). `loopai-acp` `exec`s `loopai --acp` to stay one stable process.
- **`session/update`** notification:
  `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":…,"update":{…}}}`, where `update`
  is one of:
  - `{"sessionUpdate":"plan","entries":[{"content":"Task 1: …","priority":"medium","status":"pending"|"in_progress"|"completed"}]}`
  - `{"sessionUpdate":"tool_call","toolCallId":"…","title":"Task 3/7: …","kind":"other","status":"in_progress"}`
  - `{"sessionUpdate":"tool_call_update","toolCallId":"…","status":"completed"|"failed","title":"…"}`
  - `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"…"},"messageId":"loopai-final-<hex>"}`

  Chunks with one `messageId` form one message. Text outside an active prompt is dropped.
- **Prompt envelope**
  - `prompt = [{type:"text", text:<wrapped or raw>}, …, {type:"text", text:"<runtime_info>…"}]`.
  - Wrapper:
    `<t3_code_instructions>\n…\n</t3_code_instructions>\n\n<user_request>\n<message>\n</user_request>`.
  - Wrapping follows per-session instruction state, recorded after any non-error prompt response (a
    cancelled one counts). A steered prompt after a cancel therefore usually arrives unwrapped.
  - `<message>` may be `<context>\n\nUser message:\n<text>`. After a `session/load` fallback,
    `<context>` is the `Context handoff (full_thread_summary):` block.
  - Restart note:
    `Note: the T3 server restarted, and this background work was cancelled before it finished. It will not report back on its own, so resume or restart what you still need and wait for its result:`,
    followed by `- <kind>: <label>` lines.
  - The final continuation text is `Continue where you left off.`, or the note, a blank line, and
    `Continue where you left off.`.
  - The web Resume button after Stop and usage-limit auto-resume send the same `Continue where you
    left off.`. T3 never sends a restart continuation after a Stop.

### MCP thread id (streamable HTTP, protocol `2025-06-18`)
- The endpoint and authorization come from the `env` array of the `t3-code` `mcpServers` entry, a
  stdio `acp-mcp-bridge` (`T3_ACP_MCP_ENDPOINT`, `T3_ACP_MCP_AUTHORIZATION`), else from the spawn-time
  environment. loopai never spawns the bridge: it speaks streamable HTTP to the endpoint directly,
  following the sequence of T3's own bridge client (`AcpMcpStdioBridge.ts:82-134`, the reference
  client):

1. `POST <endpoint>` with `Authorization: Bearer …` exactly as given,
   `Content-Type: application/json`, `Accept: application/json, text/event-stream`, and no `Origin`:
   `{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"loopai","version":"<version>"}}}`.
   The response sets `mcp-session-id`; send it on every later request.
2. `POST {"jsonrpc":"2.0","method":"notifications/initialized"}`, answered with 202.
3. `POST {"jsonrpc":"2.0","id":"2","method":"tools/call","params":{"name":"t3_thread_list","arguments":{"limit":1}}}`
   → `{"isError":false,"structuredContent":{"projectId":…,"currentThreadId":"…","threads":[…],"nextCursor":…,"total":…},"content":[{"type":"text","text":"<same JSON>"}]}`.
   Fall back to parsing `content[0].text`. Bodies are JSON or SSE (`data:` lines).
- Errors:
  - 401 `{"error":"invalid_mcp_credential"}`;
  - 415 without the JSON content type;
  - 406 without both Accept types;
  - 403 with an Origin;
  - 400 without `mcp-session-id`.
- The credential is per provider session. It expires after 24 h unused and is lost on a T3 restart.
  It is touched hourly during a running run, and issuing a new one for the thread revokes the old.
- The same client calls `t3_thread_read` for the Mode 2 Stop guard.

### Mode 2 routing and turn behaviour
- **Binding keys**
  - The T3 thread id from MCP is the required key. The ACP session id is only a fallback, because T3
    replaces the session id when `session/load` falls back.
  - Bindings live in `<session cwd>/.loopai/acp/binding-thread-<sha256(thread id)[:32] hex>.json` and
    `binding-session-<session id>.json`. The raw thread id is stored inside the file.
- **Routing order**
  1. **Bound run.**
     - Live, including `starting`: attach, whatever the text says (this covers continuation and a
       steered prompt that names a plan).
     - Not live and not succeeded (crashed, stopped, or failed), with continuation text (a restart
       continuation or the Resume button) or the text `resume` (trimmed, case-insensitive): relaunch
       from the recorded argv and env options, then attach.
     - Succeeded: never relaunched; continuation text or `resume` gets the status reply (rule 4).
  2. **A message naming a plan**: `loopai --detach` of that plan (origin `t3-thread`), bind, attach.
     Never another run.
  3. **No binding.** Only a continuation may adopt, and only a live run that has origin `t3-thread`,
     descriptor `cwd` equal to the session cwd, and a free `attach.lock`. Exactly one candidate: bind
     and attach. Otherwise list the candidates.
  4. **Otherwise** a status reply: the bound run's last outcome summary, or how to start a run, plus
     "continue the conversation by switching this thread's model to Claude or Codex".
- **Turn**
  - One `session/prompt` stays open for the whole run.
  - The final message is at most 12,000 bytes of JSON-escaped UTF-8: summary, risk, status and reason,
    branch, and `<run dir>/report.md`. It is sent before `end_turn`.
  - Every finished run ends with `end_turn`, a failed or crashed status stated in the final message.
    Never answer with a JSON-RPC error.
- **Stop**
  - Answer `cancelled` at once, then wait the grace (`acpStopGrace`, 5 s), measured from that answer.
  - A new prompt within the window keeps the run. One that names a plan while the bound run is live
    re-attaches it (rule 1) and never launches another run.
  - Guard: after the window, read the thread's latest run through MCP `t3_thread_read` with the
    session's credential (best-effort, isolated, ≤5 s). Stop only when a user interrupt request
    (`run_interrupt_request`) is recorded for it, since a steer or T3's own ingestion-failure cancel
    records none. If the read fails or the item kind is unavailable, stop.
  - Stop only this thread's bound `t3-thread` run, matched through the binding key (stop marker, then
    SIGTERM). The worktree is kept.
  - SIGTERM or EOF during the window abandons the pending stop.
  - SIGTERM or EOF without a cancel: exit at once and never touch the run.

### Skills (Mode selection and Mode 2 launch)
- **First T3 step**: call `orchestrator_capabilities` and read:
  - `parentThreadId` (null means not a T3 thread: stop);
  - `runtimeMode`, `interactionMode`;
  - `providers[]`: `providerInstanceId`, `driverKind`, `displayName` (may be null),
    `models[{id,label?,options?}]` (read only `models[].id`), `constraints[]`.
- **Mode 1 gate**: offered only when `runtimeMode` is `full-access`, `SANDBOX_RUNTIME`/`CODEX_SANDBOX`
  are unset, and the OS is not Windows. The thread id must match `^[A-Za-z0-9_.:%-]{1,1024}$` and is
  always single-quoted.
- **Mode question** (Claude: AskUserQuestion; Codex: numbered list): "Terminal in this thread", "New
  loopai thread (shows Working)", "Cancel".
- **Mode 1 launch**: the token mint and `loopai --detach --t3-thread` run as one FOREGROUND Bash call,
  never `run_in_background`, with a timeout of at least 180 s, above the launcher's 120 s ack wait.
- **Finding the loopai instance**
  - Read `${T3CODE_HOME:-~/.t3}/userdata/settings.json`, read-only.
  - Pick, in sorted key order, a `providerInstances.<id>` with `driver: "acpRegistry"`,
    `config.source: "local"`, and `enabled` and `config.enabled` not false, whose `config.commandPath`
    basename is in {`loopai-acp`, `loopai-acp.exe`}, or is in {`loopai`, `loopai.exe`} with
    `config.commandArgs` containing `--acp`.
  - Never assume the instance id (the T3 default is `acpRegistry_local_acp`). Tolerate unknown keys
    and value types.
  - Cross-check that `<id>` appears in `providers[]` with empty `constraints`.
  - Instance shape:
    `{driver:"acpRegistry", displayName?, accentColor?, environment?, enabled?, config:{source:"registry"|"local", enabled, agentId, commandPath, commandArgs, authMethodId, distribution, customModels}}`.
- **No instance**: explain registration, and never edit `settings.json`:
  1. T3 Settings → Providers → Add provider → ACP Registry;
  2. Instance ID: the default is fine (the skill never assumes it);
  3. Display name `loopai`;
  4. ACP source `Local command`;
  5. Executable override = the absolute path of `loopai-acp` (the output of `command -v loopai-acp`),
     or the absolute path of `loopai` with the argument `--acp` in the Arguments editor;
  6. Add instance.
- **`t3_thread_launch`**
  - Params:
    - `title` (REQUIRED, e.g. `"loopai: <plan stem>"`);
    - `modelSelection: {instanceId: <id>, model: <providers[i].models[0].id>}`;
    - `message: "<plan> <FLAGS>"`;
    - `workspaceStrategy`: `{type:"existing_worktree", worktreePath:<git toplevel>}` when the caller
      is in a linked worktree (git-dir differs from git-common-dir), else `{type:"root"}`.
  - Omit `runtimeMode`/`interactionMode`.
  - Refuse plan paths with whitespace.
  - When `interactionMode` is `plan`, stop and ask the user to leave plan mode.
  - Returns `{threadId, projectId, modelSelection, runId, status}`.
  - There is no idempotency key: after an error or a lost reply, inspect `t3_thread_list` before
    retrying.
  - Link format: `[title](t3-thread://v1/<threadId>)`.
- **Tool names**
  - Claude names them `mcp__t3-code__<tool>`. They may be deferred: load them with ToolSearch, or call
    them directly, before concluding T3 is absent. `t3_thread_launch` is not allowed in a read-only
    Claude thread.
  - Codex names them `mcp__t3_code__<tool>`. T3's rule: "Make one bounded direct attempt using the
    known T3 tool name on the next tool step. In Codex code mode, for example, call
    `tools.mcp__t3_code__orchestrator_capabilities({})` before reporting that the capability is
    absent."

### Skill report blocks (identical on both hosts)
- Mode 1:
  ```text
  loopai run started: <run-id>
  follow: loopai --attach <run-id>
  Progress shows in this thread's terminal tab and as the [loopai: …] title prefix.
  If the run fails or is stopped, ask me to run the same plan again in this mode: it resumes the run with a fresh token.
  ```
- Mode 2:
  ```text
  loopai thread started: [<title>](t3-thread://v1/<threadId>)
  The thread shows Working until the run ends. After Stop or a failure, type `resume` in that thread.
  After the report, switch that thread's model to Claude or Codex to talk about the result.
  ```

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification** (live T3 Code 0.1.1 checklist):
- **If possible, check these before the Mode 2 tasks (12-15) run; otherwise check them first after
  completion.** The design asks for an early check of three items, and Tasks 12-15 depend on them:
  - whether the provider's injected MCP endpoint returns `currentThreadId`;
  - whether steering arrives as `session/cancel` followed by `session/prompt` (the basis of the 5 s
    Stop grace);
  - whether Working shows during `session/load`.

  The code isolates each one: the session-id fallback, the `acpStopGrace` constant, and status-only
  `session/load`. A mismatch is therefore a scoped change.
- **Mode 1 end to end**, from a Claude thread and from a Codex thread in `full-access`:
  - the run id is reported;
  - the terminal follows the run;
  - the title prefix advances and ends `done`/`failed`;
  - the thread is pinned and unpinned;
  - the agent stays usable;
  - restart T3 mid-run and confirm the bridge re-opens its terminal and types the follow command,
    without the thread being opened first;
  - close the terminal and confirm it is not recreated;
  - run Mode 1 from a subagent thread whose id contains `:` and `%`.
- **Mode 2**:
  - register `loopai-acp` through the settings UI;
  - launch both from `loopai-t3` and directly, by picking loopai in a new thread;
  - confirm Working for the whole run, the plan entries, the phase card, and the final summary;
  - restart T3 mid-run and confirm Working returns without user action;
  - press Stop, then `resume`;
  - after the report, switch the model to Claude or Codex and continue the conversation.
- **Remaining unverified items**:
  - whether the composer accepts model `default` before the first probe;
  - the live handoff budget;
  - whether a token can be minted from a Codex sandbox, and which sandbox signals appear in agent
    shells;
  - Linux slice behaviour for the transient scope.
- Watch `statev2.sqlite` growth over a long run in both modes.

**External system updates**:
- Re-verify the protocol-2 and ACP contracts after every T3 Code update. They come from reading the
  0.1.1 server bundle, not from a documented API.
- Users with an old Grok-driver `loopai` instance in T3 settings should remove it and register the new
  ACP Registry instance.
- Stale `.loopai/progress/t3-current-*` directories and leftover `acp_chat.txt` prompt copies are
  harmless and can be deleted by hand.
