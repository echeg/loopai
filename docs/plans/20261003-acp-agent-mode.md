# ACP agent mode: host loopai as a T3 Code provider session

## Overview
- Add an ACP (Agent Client Protocol) agent mode so T3 Code can run loopai as a real provider session instead of a terminal process behind a title-only thread. In that mode the thread shows "Working" for the whole run, plan progress in the sidebar, phases as activities, executor output as streamed reasoning, and the completion report as the final assistant message, on desktop, web, and mobile.
- Problem it solves: T3 Code derives "working" only from a running provider session, so the existing `--t3` integration can never show a loopai run as working; activity rows and messages are internal orchestration commands the public API does not accept.
- Integration: T3 Code has no custom-provider plugin API, but its Grok driver speaks ACP to a configurable `binaryPath`. A small launcher binary `loopai-acp` answers the Grok CLI's probe commands and, for `agent stdio`, starts `loopai --acp` with inherited stdio. `loopai --acp` is a JSON-RPC 2.0 server over stdin/stdout that runs one plan per `session/prompt` in-process through the normal execution path, with an ACP sink added to the logger chain.
- MVP scope: the prompt text is a plan path plus optional `--task-model`, `--review-model`, `--external-reviewers`. Review-only modes, plan creation, and interactive questions are out of scope.

## Decisions
- **Context**: a live spike (this session, T3 Code build of October 2026) proved that a provider instance of the Grok driver whose `binaryPath` points at a fake ACP agent passes T3's provider probe and gives a thread `session.status: running` with sidebar `planProgress` for the whole `session/prompt`, then `completed` on `stopReason: end_turn`.
- **Chosen approach**: launcher + in-process server.
  - `cmd/loopai-acp` is a tiny binary that only translates the Grok CLI surface. The loopai CLI keeps its own argument grammar, and positional `models` or `agent` never get hijacked.
  - `loopai --acp` runs plans in-process, reusing the real pipeline. Logger, phase, and outcome hooks give structured events without parsing text.
  - The MVP prompt grammar is a plan path plus the same three pass-through flags `--t3-launch` and the `loopai-t3` skill already accept.
  - The run happens in the session's `cwd` without `--worktree`. T3 Code owns thread worktrees, matching `--t3-launch`.
- **Rejected alternatives**:
  - single `loopai` binary sniffing Grok-shaped argv: pollutes the CLI grammar (`loopai models`, `loopai agent stdio`).
  - an ACP adapter driving a child `loopai <plan>` and parsing its progress log: fragile text parsing, no clean cancel or structured events.
  - the Cursor driver as host: its probe runs a full ACP session, reads the real `~/.cursor/cli-config.json` (a non-lab channel marks the instance as an error), forces `session/set_config_option("model")`, and needs `cursor/list_available_models`.
  - a T3 Code fork: maintenance burden, and no longer needed.
- **Verified facts** (spike and T3 source):
  - Grok driver probe: `<bin> --version` (4s, semver regex, exit 0 required), `<bin> models` (10s; output containing "You are logged in" means authenticated, "not logged in" means error, anything else unknown), `<bin> inspect --json` (failure means no skills), ACP probe `<bin> agent stdio` with `initialize` only (8s).
  - Session argv: `[--permission-mode m] agent [--always-approve] stdio`.
  - ACP sequence: `initialize {protocolVersion:1, clientCapabilities, clientInfo}` → `authenticate {methodId:"cached_token"}` (or `xai.api_key` when `XAI_API_KEY` is set) → `session/new {cwd, mcpServers}` → `session/prompt {sessionId, prompt:[text blocks], _meta:{promptId}}` → `session/cancel`. JSON-RPC ids can exceed 2^32 and requests carry extra `traceId`/`spanId` fields.
  - The prompt's first text block is the user's message. T3 appends a second block with runtime instructions, which must be ignored.
  - `session/new` carries the T3 MCP server URL with a bearer credential in `mcpServers[].headers`. It must never be logged.
  - Update mapping: `agent_message_chunk` becomes an assistant message, `agent_thought_chunk` reasoning, `tool_call`/`tool_call_update` an activity, `plan` the plan steps and sidebar progress. Identical tool-call updates are deduplicated.
  - Grok turn watchdog: a turn fails after 10 minutes without content or tool progress (30 minutes with an open tool call). It pauses during approvals and questions.
  - The provider session reaper idles out after 30 minutes but skips a thread with an active turn.
  - T3 hot-reloads `providerInstances` from `<T3 home>/userdata/settings.json`. An instance is `{driver:"grok", displayName, accentColor, enabled, config:{enabled, binaryPath}}`.
  - Plan execution never calls `InputCollector`; only `--plan` creation asks questions. The MVP therefore needs no question bridge.
  - Several places in `cmd/loopai` assume a terminal: direct stdout writes (`progress.Logger` hardwires `os.Stdout`, `fatih/color` writes to `color.Output`, the version banner in `main()`), stdin readers (plan selector, `tryAutoPlanMode`, `ensureRepoHasCommits`, `makePauseHandler`), the `startInterruptWatcher` force-exit, and `startBreakSignal` registering SIGQUIT on every run.

## Context (from discovery)
- Files/components involved:
  - `cmd/loopai/main.go`:
    - `opts` struct, `main()`, `run()`, `runConfiguredStandaloneCommand` (where `--t3-launch` routes)
    - `selectAndExecutePlan`, `executePlan`, `buildRunnerLogger`, `createRunner`
    - `printStartupInfo`, `displayStats`, `startInterruptWatcher`, `capturePlanOutcome`/`planExecutionOutcome`
  - `pkg/progress/progress.go` (hardwired stdout), `pkg/status` (phases, sections, `PhaseHolder`)
  - `pkg/processor/runner.go`: `Logger` interface, `Runner.Report()`
  - `pkg/web/broadcast_logger.go`: the closest existing model of a logger wrapper turning events into a stream
  - `pkg/t3/reporter.go`: wrapper pattern (`WrapLogger`, `OnPhase`)
  - new: `pkg/acp/` (transport, server, event sink) and `cmd/loopai-acp/` (launcher)
  - `Makefile` (build both binaries), `docs/t3-code.md`, `README.md`, `llms.txt`, `CLAUDE.md`
- Related patterns found:
  - logger wrappers that embed the inner logger and override `PrintSection`/`PrintAligned`
  - `status.PhaseHolder.OnChange` observers registered beside the cmux, orca, and t3 observers in `executePlan`
  - best-effort sinks that never fail the run
- Dependencies identified: no new Go dependencies. JSON-RPC framing is newline-delimited JSON over stdio, implemented with `encoding/json` and `bufio`.

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
- Run tests after each change
- Normal CLI behavior (non-ACP) must stay byte-identical on stdout
- Tests redirect HOME/config paths to `t.TempDir()` and never touch real `~/.config/loopai/`, `~/.config/ralphex/`, or `~/.t3`
- Keep the module path and `<<<RALPHEX:...>>>` signals unchanged; do not edit `CHANGELOG.md`

## Testing Strategy
- **Unit tests**: table-driven with `testify`, one `_test.go` per source file
- the transport and server are tested over `io.Pipe` pairs with a scripted fake client; no T3 Code process is started
- the event sink is tested with a recording transport: phase, section, output, and final events map to the expected `session/update` payloads
- the launcher is tested by running its `main` logic with injected argv, stdout, and an exec hook
- the `cmd/loopai` integration is tested with the existing fake executors through a full in-process ACP prompt against a temporary repository, asserting that nothing but JSON-RPC reaches the protocol writer
- **E2E tests**: dashboard Playwright tests are unaffected

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code, tests, and docs in this repository
- **Post-Completion** (no checkboxes): configuring a T3 Code provider instance and verifying a real run

## Implementation Steps

### Task 1: JSON-RPC stdio transport (`pkg/acp/transport.go`)
- [x] implement a newline-delimited JSON-RPC 2.0 connection over an `io.Reader`/`io.Writer`:
  - read loop with a 16 MiB line cap
  - write mutex
  - ids kept as `json.RawMessage`, so values above 2^32 and strings round-trip unchanged
  - unknown fields (`traceId`, `spanId`) ignored
- [x] dispatch incoming requests and notifications to registered handlers; reply to unknown requests with `-32601`; ignore unknown notifications
- [x] provide `Notify(method, params)`, `Reply(id, result)`, and `ReplyError(id, code, message)`; a request handler may answer later from another goroutine
- [x] write tests: request/response round-trip, large numeric and string ids, notification dispatch, unknown method, malformed line skipped, concurrent writes stay line-atomic, EOF ends the loop
- [x] run `go test -race ./pkg/acp/...` - must pass before task 2

### Task 2: ACP session server (`pkg/acp/server.go`)
- [x] implement the agent side:
  - `initialize` → `{protocolVersion:1, agentCapabilities:{loadSession:false}, authMethods:[{id:"cached_token"},{id:"xai.api_key"}], agentInfo:{name:"loopai", version}}`
  - `authenticate` → `{}` for any method id
  - `session/new` → a new session id, recording `cwd` and discarding `mcpServers`
  - `session/set_config_option`, `session/set_mode`, and `session/set_model` → `{}`
  - `session/load` → error
- [x] `session/prompt`:
  - take the first text block only and pass it with the session cwd to an injected `RunFunc(ctx, PromptRequest, Sink) (Result, error)` on its own goroutine with a per-prompt cancellable context
  - reject a second concurrent prompt with an error
  - map the outcome: success → `{stopReason:"end_turn"}`; cancel → `{stopReason:"cancelled"}`; a run failure → JSON-RPC error carrying the message
- [x] `session/cancel` cancels that session's running prompt; the request id of the prompt is answered exactly once
- [x] never log `mcpServers` contents; debug logging, if any, goes to stderr and redacts header values
- [x] write tests with a scripted client over pipes:
  - full handshake
  - prompt success
  - prompt failure → error
  - cancel → `cancelled`
  - concurrent prompt rejected
  - runtime-instructions block ignored
  - `session/load` error
  - no credential text in captured stderr
- [x] ➕ a minimal `Sink` (`Update`, `Message`) lives in `pkg/acp/sink.go` so `RunFunc` has its final type; Task 3 extends it
- [x] run `go test -race ./pkg/acp/...` - must pass before task 3

### Task 3: ACP event sink (`pkg/acp/sink.go`)
- [x] implement a `Sink` that turns loopai events into `session/update` notifications:
  - phase change → `tool_call` (new id per phase, kind `other`, title like "task 2/5" or "review · iteration 1", status `in_progress`), closing the previous one with `tool_call_update` `completed`
  - section → refresh the `plan` entries: plan tasks from the parsed plan file plus review stages, with `pending`/`in_progress`/`completed`
  - executor output (`PrintAligned`) → `agent_thought_chunk`, coalesced into at most one notification per 500 ms and capped per chunk
  - final report → `agent_message_chunk`
- [x] add a heartbeat: when nothing was sent for 4 minutes (for example during a `wait_on_limit` sleep), send a `tool_call_update` whose title carries the elapsed wait, so the Grok 10-minute watchdog never fires; stop it with the run
- [x] expose the sink as a `WrapLogger` decorator (same `Logger` shape as `t3.Logger`) plus `OnPhase(old, cur status.Phase)`, both nil-safe, forwarding every call to the inner logger
- [x] write tests with a recording transport and a fake clock: phase transitions open/close tool calls, plan entries track sections, output coalescing and capping, heartbeat fires after idle and not while output flows, final message, nil sink no-ops
- [x] ➕ the server calls `Sink.Finish` before the final message and reply, so pending output is flushed and the open tool call closes as completed or failed; `Sink.SetPlan(planFile, stages...)` lets the run function name the plan and expected stages (Task 5 calls it)
- [x] run `go test -race ./pkg/acp/...` - must pass before task 4

### Task 4: Make plan execution safe to drive non-interactively in-process (`cmd/loopai`)
- [x] add an `Stdout io.Writer` option to `progress.Logger` (default `os.Stdout`) and thread a configurable output writer through the startup banner, stats, and worktree messages instead of writing to `os.Stdout` or `color.Output` directly
- [x] add a non-interactive execution flag to `executePlanRequest`. When set:
  - a missing plan is an error instead of the selector or the auto-plan prompt
  - `ensureRepoHasCommits` errors instead of prompting
  - `makePauseHandler` and `startBreakSignal` are skipped
  - the T3 title reporter, cmux, and orca reporters are not constructed
- [x] add an extra logger decorator and phase observer to `executePlanRequest`, applied in `buildRunnerLogger` at the dashboard's position (below the section timer) and beside the other `OnChange` subscriptions
- [x] extract the post-config setup of `run()` (mode, model specs, external-review selection, deps, repo root, git service, base refs) into a reusable function so a caller can build and execute one plan request from explicit options and config, without the interrupt watcher's force-exit
- [x] record the completion report and failure reason in `planExecutionOutcome` so a caller can read them after `executePlan`
- [x] write tests: the normal CLI path's stdout is unchanged (existing tests stay green), non-interactive mode fails fast instead of reading stdin for a missing plan and an empty repository, decorator and observer receive sections and phases, outcome carries the report
- [x] ➕ `prepareNonInteractiveRequest` (built on the extracted `resolveExecutionDeps` and `openExecutionRepository`) returns the request, a plan selector, and the chain-lock release; Task 5 sets `LogDecorator`/`PhaseObserver` and calls `selectAndExecutePlan`
- [x] run `go test -race ./cmd/loopai/... ./pkg/progress/...` - must pass before task 5

### Task 5: `loopai --acp` command
- [x] add the `--acp` flag to `opts` and route it from `runConfiguredStandaloneCommand`. In ACP mode:
  - skip the version banner on stdout
  - set `color.Output` to stderr
  - reject combination with plan files and other execution flags in `validateFlags`
- [x] implement `runACPCommand`: serve `pkg/acp` on stdin/stdout. Its `RunFunc`:
  - parses the prompt into a plan path plus the three pass-through flags, with the same validation rules as `validateT3LaunchFlags`; a malformed prompt fails with usage text
  - changes into the session cwd and loads config there
  - forces `t3` off
  - builds the request through the Task 4 function with the sink as decorator and observer
  - runs `executePlan`, restores the cwd, and returns the report and outcome
- [x] serialize runs (one prompt at a time per process) and make prompt cancellation cancel only the run's context
- [x] write an in-process integration test: fake executors, a temporary repository with a two-task plan, a scripted ACP client sending handshake + prompt. Assert:
  - plan entries progress
  - tool calls open and close
  - the final message contains the report
  - `stopReason` is `end_turn`
  - the protocol writer received only valid JSON lines
- [x] write tests for errors: malformed prompt, missing plan, run failure → JSON-RPC error, cancel mid-run → `cancelled` and the plan left in place
- [x] run `go test -race ./cmd/loopai/...` - must pass before task 6
- [x] ➕ `runACPCommand` also reserves the real stdin/stdout for the protocol and points `os.Stdin` at the null device and `os.Stdout`/`color.Output` at stderr for the process lifetime, so a stray read or write cannot corrupt JSON-RPC traffic
- [x] ➕ each prompt also forces `orca` and `use_worktree` off (the plan runs in place in the session cwd) and the process cwd is restored after every prompt
- [x] ➕ `acp.Server.Shutdown` cancels a running prompt and waits for its answer without waiting for stdin EOF; `serveACP` calls it when the process context is canceled (SIGINT/SIGTERM)

### Task 6: `loopai-acp` launcher (`cmd/loopai-acp`)
- [x] answer the Grok CLI surface:
  - `--version` prints `loopai-acp <version>` and exits 0
  - `models` prints a bullet list with one `loopai` entry and never the words "logged in"
  - `inspect --json` and `update` exit non-zero without side effects
- [x] for argv containing `stdio` (`[--permission-mode m] agent [--always-approve] stdio`), start `loopai --acp` with inherited stdin, stdout, and stderr, forward its exit code, and propagate termination. The `loopai` binary is resolved in this order:
  - `LOOPAI_ACP_LOOPAI`
  - a `loopai`/`loopai.exe` beside the launcher
  - `PATH`
- [x] any other argv prints usage to stderr and exits 2
- [x] build both binaries in `make build` (`.bin/loopai`, `.bin/loopai-acp`)
- [x] write tests: every probe command's output and exit code, binary resolution order, exec argv for both stdio shapes, unknown argv
- [x] run `go test -race ./cmd/loopai-acp/...` - must pass before task 7
- [x] ➕ on Windows `make build` also writes `.bin/loopai-acp.exe` and `.bin/loopai.exe`, because process creation cannot start an extensionless binary there and the launcher's sibling lookup expects `loopai.exe`

### Task 7: Verify acceptance criteria
- [x] verify all requirements from Overview are implemented
- [x] verify edge cases: second prompt while running, cancel, malformed prompt, plan on the default branch (branch creation in place), credential redaction
- [x] run `make test` (run in WSL Ubuntu per the Windows test-safety rule; the wrapper suites need non-tty stdin there, since `wsl.exe` attaches a pty that `codex-as-claude`'s missing-prompt case blocks on)
- [x] run `make lint` - all issues must be fixed
- [x] run `GOOS=windows GOARCH=amd64 go build ./...` and `GOOS=linux GOARCH=amd64 go build ./...`
- [x] verify test coverage of `pkg/acp` and the launcher is 80%+ (pkg/acp 98.4%, cmd/loopai-acp 88.5%)

### Task 8: [Final] Update documentation
- [ ] add an "Experimental: loopai as a T3 Code provider" section to `docs/t3-code.md`. It covers:
  - the `providerInstances` entry (`driver: "grok"`, `displayName: "loopai"`, `config.binaryPath` → `loopai-acp`), added through T3 settings or `settings.json`
  - the prompt grammar
  - what the thread shows
  - the watchdog/heartbeat
  - the reliance on undocumented Grok driver contracts
  - how to remove the instance
- [ ] update `README.md`, `llms.txt` (new flag and binary), and `CLAUDE.md` (project structure, ACP mode architecture, stdout discipline in ACP mode)

## Technical Details
- **Prompt grammar**: `<plan path> [--task-model SPEC] [--review-model SPEC] [--external-reviewers LIST]`. Values match `^[A-Za-z0-9._:,+-]+$`, and model specs carry a `claude`/`codex` provider prefix. The plan path is resolved against the session cwd.
- **Event mapping**:
  - `status.Phase` change → `tool_call`/`tool_call_update`
  - `status.Section` → `plan` entries
  - `PrintAligned` → coalesced `agent_thought_chunk`
  - `Print` → ignored (already summarized by phases)
  - completion report or failure text → `agent_message_chunk`
- **Stop reasons**: `end_turn` on success, `cancelled` on `session/cancel`, JSON-RPC error on a failed run, so T3 records the turn as failed.
- **Process model**: T3 spawns `loopai-acp ... agent stdio`, which spawns `loopai --acp`. One run at a time per process, and the cwd is changed per prompt and restored afterwards.
- **stdout discipline**: in ACP mode stdout carries JSON-RPC only; every human-readable line goes to stderr or the progress log.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- add the provider instance in T3 Code pointing at `.bin/loopai-acp`, start a thread with the `loopai` instance, send a plan path, and confirm "Working", plan progress, activities, streamed reasoning, the final report, and that the T3 Code provider status shows the instance as ready
- confirm a run longer than 10 minutes with a quiet phase survives (heartbeat), and that the stop button cancels the run
- confirm a failed run shows as a failed turn
- re-check after each T3 Code update, since the Grok driver contracts are undocumented
