# Report: ACP agent mode: host loopai as a T3 Code provider session
**Plan:** `docs/plans/20261003-acp-agent-mode.md` · **Branch:** `acp-agent-mode` · **Base:** `master` · **Mode:** full · **Executor:** claude · **Task model:** not recorded · **Review model:** not recorded · **Started:** 2026-10-03T08:09:52Z · **Finished:** 2026-10-03T09:08:29Z

## Summary
This branch adds an experimental ACP (Agent Client Protocol) agent mode. With it, T3 Code can host loopai as a real provider session, so a thread shows "Working" for the whole run, plan progress in the sidebar, phases as activities, executor output as streamed reasoning, and the completion report as the final assistant message. The pieces are:

- **`pkg/acp` transport:** newline-delimited JSON-RPC 2.0 with a 16 MiB line cap. Ids are kept raw, so ids above 2^32 and string ids survive unchanged.
- **`pkg/acp` server:** the methods are `initialize`, `authenticate`, `session/new`, `session/load`, `session/prompt` and `session/cancel`, plus no-op config, mode and model setters.
  - At most one prompt runs at a time.
  - A message steered as cancel then prompt waits for the canceled run before it starts.
  - Every prompt gets exactly one answer: `end_turn`, `cancelled`, or a JSON-RPC error.
- **`pkg/acp` event sink:**
  - Phases become `tool_call`s and sections refresh the `plan` entries.
  - Executor output becomes `agent_thought_chunk`s, at most one every 500 ms.
  - A 4-minute heartbeat stops the Grok driver's 10-minute watchdog from firing.
  - The report is sent as the final `agent_message_chunk`.
- **`loopai --acp`** (`cmd/loopai/acp.go`) runs one plan per prompt, in process, in the session's cwd. `t3`, `orca` and `use_worktree` are forced off. stdout carries only JSON-RPC: `os.Stdin` points at the null device, and `os.Stdout` and `color.Output` point at stderr.
- **Non-interactive execution** is a refactor of `cmd/loopai/main.go`. It extracts `resolveExecutionDeps`, `openExecutionRepository` and `prepareNonInteractiveRequest`, and adds `NonInteractive`, `Out`, `LogDecorator` and `PhaseObserver` to the plan request. It also adds `progress.Config.Stdout`.
- **`cmd/loopai-acp` launcher:** answers T3 Code's Grok CLI probes and runs `loopai --acp` for `agent stdio`. `make build` now builds it, with `.exe` copies on Windows.
- **Documentation:** `docs/t3-code.md`, `README.md`, `llms.txt` and `CLAUDE.md`.

The task phase took one iteration of about 17 s with no failed retries. The feature commits already existed, so the task work for this run was minimal. Internal review then ran one loop iteration, ending with `review_done`. One Codex reviewer followed, and post-review ran once.

| phase | duration_ms | ≈ |
|---|---:|---|
| tasks | 16678 | 17 s |
| internal review | 2843290 | 47 m 23 s |
| external review | 273765 | 4 m 34 s |
| evaluation | 383777 | 6 m 24 s |
| other | 0 | 0 |

The finish timestamp and these timings cover work up to and including finalize. They exclude report assessment and archival.

## Change scope
- **Diff size:** 24 files, +6061 / −237.
- **Added (10):**
  - `pkg/acp/{transport,server,sink}.go` and their tests
  - `cmd/loopai/acp.go` and `acp_test.go`
  - `cmd/loopai-acp/main.go` and `main_test.go`
- **Modified (14):**
  - **Execution path:** `cmd/loopai/main.go` (+519/−… refactor) and `main_test.go`
  - **Console writer:** `pkg/progress/progress.go` and its test
  - **T3 stop and pin:** `pkg/t3/reporter.go` and its test. The stop timeout became `4*requestTimeout + 1s`, and a run that stops while its thread bind is in flight now takes no pin.
  - **Test fixes:** `pkg/awake/awake_test.go` widens a flaky timing margin, and `pkg/git/repository_lock_windows_test.go` gains `//nolint:gosec` annotations.
  - **Build:** `Makefile` (launcher build, `.exe` handling)
  - **Docs and plan:** `CLAUDE.md`, `README.md`, `llms.txt`, `docs/t3-code.md`, and the plan file.
- **Commits (12):**
  - Seven feature commits, one per task: `d101f49` transport, `622d6f4` server, `90a8413` sink, `de0a3e6` non-interactive execution, `743e0ca` `--acp`, `fee89e0` launcher, `58978c6` acceptance.
  - `e55d1a9` docs.
  - `b9ef335` adds `session/load` and steering after cancel.
  - Three review-fix commits: `9193822`, `c6459b5`, `93b2661`.

## Risk
**medium**

- **Public APIs and CLI:** additive only. There is a new `--acp` flag and a new `loopai-acp` binary, and no existing flag or config key changed meaning. The real regression risk to the normal CLI comes from the `main.go` refactor: run setup was moved into reusable functions, and output writers were threaded through the startup banner, stats and worktree messages. The plan requires stdout on the normal path to stay byte-identical, and the existing tests are meant to guard that.
- **Concurrency:** this is the main risk.
  - The ACP server coordinates per-prompt goroutines, cancellation, waiting after a steered cancel, `Shutdown`, and answering each prompt exactly once.
  - It also changes the process-wide cwd for each prompt and swaps `os.Stdin`, `os.Stdout` and `color.Output` for the life of the serve.
  - Both external-review findings were ordering and state races in exactly this area, so subtle interleavings may remain.
  - The T3 reporter's longer stop timeout can add a little shutdown latency when the T3 server is slow.
- **External contracts:** the mode depends on undocumented Grok driver behaviour in T3 Code: the probe surface, the "logged in" wording, the watchdog timing, and `session/load` without a capability check. Any T3 Code update can break it. The feature is opt-in and labelled experimental, so this cannot affect existing users.
- **Configuration:** no new config keys. In ACP mode each prompt forces `t3`, `orca` and `use_worktree` off. A relative config directory is made absolute before the per-prompt chdir.
- **Data schemas:** no persisted format changed. `progress.Config.Stdout` is an in-process option, and progress logs, run records and checkpoints are untouched.
- **Credentials:** `mcpServers` headers carry T3's bearer credential and are never logged. A server test checks this.
- **Migrations:** none.

## Migrations and operational steps
none

Using the feature is optional and opt-in:
1. Run `make build` to produce `.bin/loopai-acp` (`.bin/loopai-acp.exe` plus `.bin/loopai.exe` on Windows).
2. Add a T3 Code `providerInstances` entry with `driver: "grok"` and `config.binaryPath` pointing at `loopai-acp`, as described in `docs/t3-code.md`.

Housekeeping: an untracked, empty `%SystemDrive%/` directory sits in the worktree. It is not part of this change and can be deleted.

## Plan deviation
All eight tasks are checked complete, and nothing was blocked or skipped. Some planned manual checks remain open (last bullet).

**Added items (8), all implemented:**
1. A minimal `Sink` (`Update`, `Message`) in `pkg/acp/sink.go` so `RunFunc` had its final type early. Task 3 extended it.
2. The server calls `Sink.Finish` before the final message and reply. `Sink.SetPlan(planFile, stages...)` was added.
3. `prepareNonInteractiveRequest`, built on the extracted `resolveExecutionDeps` and `openExecutionRepository`.
4. `runACPCommand` reserves the real stdin/stdout for the protocol and points `os.Stdin`, `os.Stdout` and `color.Output` away from it.
5. Each prompt forces `orca` and `use_worktree` off, and the cwd is restored after every prompt.
6. `acp.Server.Shutdown` cancels a running prompt and waits for its answer. It is called when the process context is canceled.
7. The Windows `make build` writes `.bin/loopai-acp.exe` and copies `.bin/loopai.exe`.
8. The flaky `TestHolder_TouchDefersExpiry` margin was widened (250 ms idle, 10 ms touches).

**Blocked:** none. **Skipped:** none.

**Changes made during review and recorded in the plan text:**
- `initialize` now advertises `loadSession: true`, and `session/load` is implemented.
- A prompt that follows its own session's cancel now waits and then runs instead of being rejected.
- `--acp` is routed in `run()` just before `loadRunConfig` instead of from `runConfiguredStandaloneCommand`.

**Changes in the diff but not in the drift list:**
- The `pkg/t3/reporter.go` stop-timeout and pin-skip changes, from code review in `9193822`/`c6459b5`.
- `//nolint:gosec` annotations in `pkg/git/repository_lock_windows_test.go`, which are lint-only.
- The `stageOrder` stage completion and the "active until answered" prompt lifetime, from the Codex review in `93b2661` and recorded in `CLAUDE.md`.

**Still open:** the plan's Post-Completion manual checks have no recorded result:
- a real T3 Code provider-instance run
- a run over 10 minutes that relies on the heartbeat
- the stop button canceling a run
- a failed run showing as a failed turn

## Backlog
None. No backlog entries were filed. The evaluator explicitly filed nothing because both findings concerned code this branch changed.

## External review
### codex:gpt-6-astra:high
- label: codex
- iterations: 2
- duration_ms: 657491
- ended by: done
- had findings: true

**Iteration 1**
- [P2] Keep prompts active until their replies are sent (`pkg/acp/server.go:329`): `s.active` was cleared before `Sink.Finish`, the final message and the response. A cancel-then-prompt arriving in that gap could start the next turn early and mix its updates with the previous turn's. -> fixed. `s.active` is now cleared only after Finish and the final message, together with the reply under `s.mu`. New test `TestServerPromptStaysActiveUntilAnswered` failed against the original code.
- [P3] Complete checkpoint-skipped review stages (`pkg/acp/sink.go:444`): a resumed run that skipped internal review left "Review" pending after success. -> fixed. Through `stageOrder`, entering a stage now completes every earlier stage, and a successful `Finish` completes all stages. A later review no longer reopens a skipped stage. New test `TestSinkCompletesCheckpointSkippedStages` failed against the original code.

**Iteration 2**
- No findings ("NO ISSUES FOUND"). The reviewer confirmed both earlier fixes. The fixes were committed as `93b2661`. The evaluator reports that it did not rerun tests or lint right before that commit. Its iteration-1 report says `make test` passed in WSL, lint reported 0 issues, and `GOOS=windows go build ./...` succeeded on the fixed code.

## Validation
- Commands: none. The plan defines no `## Validation Commands`.
- Timings: duration_ms 0, runs 0.

Outside timed validation, the plan's Task 7 records:
- `make test` passed in WSL Ubuntu.
- `make lint` was clean.
- `GOOS=windows` and `GOOS=linux` cross-builds succeeded.
- Coverage was 98.4% for `pkg/acp` and 88.5% for `cmd/loopai-acp`.

The external-review evaluator re-ran `make test`, lint and the Windows build after the iteration-1 fixes.