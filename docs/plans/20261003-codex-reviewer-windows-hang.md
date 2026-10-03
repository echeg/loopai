# Codex reviewer on Windows: unelevated sandbox and no endless hangs

## Overview
- On 2026-10-03 two runs hung for about 5.5 hours in `codex external review iteration 1`. Two separate defects combined.
  - **Sandbox.** The external codex reviewer always runs `codex exec --sandbox read-only`. With `[windows] sandbox = "elevated"` in the user's `~/.codex/config.toml`, every shell command inside that sandbox hung without output when started from loopai. The reviewer could not read the diff and answered "Review blocked".
  - **Wait.** `codex exec` then never exited after its `task_complete` event. loopai waits for process exit and has no bound on the reviewer path unless a codex phase is configured: `IdleTimeout` stays unset there. Killing only the direct child on Windows would not have helped either, because the process chain is `cmd.exe → node → codex.exe` and the grandchildren keep the stdout pipe open.
- Changes:
  1. On Windows, add `-c windows.sandbox="unelevated"` to the read-only external codex reviewer by default, so it works without the elevated sandbox setup. The user's `codex_args` still override it.
  2. Treat codex's `task_complete` rollout event as the end of the session. When the process has not exited within a grace period after it, kill the whole process tree and complete the run from the result already received.
  3. Make process-tree termination real on Windows, so cancellation, idle timeout, session timeout, and the new grace kill all stop `node` and `codex.exe`, not just `cmd.exe`.

## Decisions
- **Context**: confirmed on the affected machine (codex-cli 0.160.0, Windows 11):
  - `codex exec --sandbox read-only` with the user's `elevated` setting timed out after 150 s on a one-command prompt, and codex logged `code-mode host closed its stdout`;
  - the same prompt with `-c windows.sandbox=unelevated` ran the command in 288 ms and exited 0;
  - the two hung reviewer processes had no children left and had written nothing after `task_complete`;
  - killing their trees with `taskkill /T /F` made both loopai runs continue (and fail on the reviewer error).
- **Chosen approach**:
  - **Windows override scope:** only the external codex reviewer (`ForceReadOnly`) gets the override, and only when `runtime.GOOS == "windows"`. Phase executors default to `danger-full-access`, where no sandbox is created, and they are left untouched.
  - **Placement:** the override is emitted before `codex_args`, so codex's last-occurrence-wins rule lets `codex_args = -c windows.sandbox="elevated"` restore the elevated sandbox.
  - **Grace kill:** the rollout tailer reports `task_complete` with its `last_agent_message`. A grace timer of 60 s starts there. If the process is still alive when it fires, loopai kills the tree and returns a successful result. The output is stdout when present, otherwise `last_agent_message`, and signals are detected on that text. This applies to every codex invocation (phases and reviewers), since a hang after completion is never useful.
  - **Rollout dependency:** the tailer must run whenever the rollout can be located, independent of whether display handlers are set, because the grace kill depends on it.
  - **Windows tree kill:** a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, using the already-vendored `golang.org/x/sys/windows`. The child is assigned right after start, and terminating the job kills every descendant. If assignment fails, fall back to `taskkill /T /F /PID <pid>`.
  - **No stuck pipe reads:** once the process is killed, stdout and stderr reads must return. Set `exec.Cmd.WaitDelay` or close the pipes after the kill.
- **Rejected alternatives**:
  - forcing `unelevated` for every codex invocation: phase executors do not use the sandbox by default, and a user who chose `codex_sandbox = workspace-write` with a working elevated setup should keep it.
  - enabling `IdleTimeout` on the reviewer path by default: it was deliberately left off there so long silent reviews are not cut short, and it would not cover a process that hangs after completing.
  - telling Windows users to edit their own `~/.codex/config.toml`: loopai never writes there, and the documented additive `-c` contract is the right lever.
- **Verified facts**:
  - `pkg/executor/codex.go` builds `exec` args in this order: config overrides, optional bypass, `--sandbox`, model/effort, `stream_idle_timeout_ms`, `project_doc`, then `splitArgs(e.ExtraArgs)` last.
  - `ForceReadOnly` is set only by `buildExternalCodexExecutor` in `pkg/processor/executor_factory.go`.
  - On Windows, `processGroupCleanup.killProcess` (`pkg/executor/procgroup_windows.go`) calls only `process.Kill()` on the direct child, and its doc comment already notes that Job Objects are not implemented.
  - The reviewer is launched as `cmd.exe /c codex exec ...` → `node codex.js` → `codex.exe`.
  - `Run` reads stdout to EOF, then waits for stderr, then calls `wait()`, and stops the rollout tailer only after `wait()` returns.
  - The rollout file is `$CODEX_HOME/sessions/<y>/<m>/<d>/rollout-*-<session id>.jsonl`. The completion record is an `event_msg` with `payload.type == "task_complete"` and `payload.last_agent_message`.

## Context (from discovery)
- Files/components involved:
  - `pkg/executor/codex.go`: argument building, `Run`, rollout tail (`startRolloutTail`, `tailRolloutFile`, `formatParsedRolloutEvent`)
  - `pkg/executor/procgroup_windows.go`, `pkg/executor/procgroup_unix.go`: process lifecycle and kill
  - `pkg/executor/executor.go`: shared runner and process start helpers used by the codex runner
  - `pkg/processor/executor_factory.go`: `buildExternalCodexExecutor`
  - docs: `README.md` (codex_args and Windows notes), `llms.txt`, `CLAUDE.md`, `pkg/config/defaults/config` (`codex_args` and `idle_timeout` comments)
- Related patterns found:
  - additive `-c` overrides with user extras appended last
  - the idle-timeout completion path (`idleTimeoutResult`) as the model for "completed without a clean exit"
  - injectable runners in codex tests (`e.runner`) and testdata rollout fixtures
- Dependencies identified: `golang.org/x/sys/windows` (already in `go.mod` and vendored), no new modules.

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
- The invocation on Linux and macOS stays byte-identical to today
- Tests redirect HOME/`CODEX_HOME`/config paths to `t.TempDir()` and never touch real `~/.codex`, `~/.config/loopai/`, or `~/.config/ralphex/`
- Keep the module path and `<<<RALPHEX:...>>>` signals unchanged; do not edit `CHANGELOG.md`

## Testing Strategy
- **Unit tests**: table-driven with `testify`, one `_test.go` per source file
- argument tests inject the target OS instead of depending on the test host
- grace-kill tests use a fake runner whose process never exits plus a rollout fixture containing `task_complete`, with an injectable grace duration and clock
- Windows tree-kill tests are behind `//go:build windows` and spawn a real `cmd /c` → child chain from a helper test binary, asserting that every descendant is gone after kill. A Unix counterpart asserts the existing process-group behavior still holds.
- **E2E tests**: dashboard Playwright tests are unaffected

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code, tests, and docs in this repository
- **Post-Completion** (no checkboxes): a real external review on Windows with and without the user override

## Implementation Steps

### Task 1: Default the Windows reviewer sandbox to unelevated
- [x] in `pkg/executor/codex.go`, when `ForceReadOnly` is set and the target OS is Windows, emit `-c windows.sandbox="unelevated"` after the loopai overrides and before `splitArgs(e.ExtraArgs)`; resolve the OS through an unexported field defaulting to `runtime.GOOS` so tests can inject it
- [x] update the argument-order comment in `Run` to name the new override and how `codex_args` can restore `elevated`
- [x] write tests:
  - Windows reviewer args contain the override before the extras
  - Linux/macOS reviewer args are unchanged
  - Windows phase executors (no `ForceReadOnly`) are unchanged
  - a user `codex_args` value `-c windows.sandbox="elevated"` appears after the override
- [x] run `go test ./pkg/executor/... ./pkg/processor/...` - must pass before task 2
- Validation also fixed existing Windows test portability failures: runner tests now use the test binary instead of Unix echo/cat, rollout fixtures isolate USERPROFILE as well as HOME, and the auto-selection test uses a cross-platform executable instead of true. Targeted executor/processor lint passed.

### Task 2: Real process-tree termination on Windows
- [ ] in `pkg/executor/procgroup_windows.go`, create a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` when the cleanup handler is created, assign the started process, and make `killProcess` terminate the job; fall back to `taskkill /T /F /PID <pid>` when the job cannot be created or assigned; close the job handle after `Wait`
- [ ] make sure stdout/stderr reads return after a kill: set `exec.Cmd.WaitDelay` on the codex and claude commands, or close the read ends after the kill, so `Run` cannot block on a pipe a dead tree left open
- [ ] update the outdated doc comments about orphan cleanup on Windows
- [ ] write Windows-only tests:
  - killing a `cmd /c` launcher terminates its grandchild
  - a normally exiting command leaves no handle leak
  - the fallback path is used when job assignment is forced to fail
- [ ] write a cross-platform test that a killed runner's `Run` returns within a bound, instead of blocking on stdout
- [ ] run `go test -race ./pkg/executor/...` and `GOOS=windows GOARCH=amd64 go build ./...` - must pass before task 3

### Task 3: Finish the session on `task_complete`
- [ ] in the rollout tailer, recognize `event_msg` records with `payload.type == "task_complete"` and publish the event with `last_agent_message` to `Run`; start the tailer whenever a session id is available, even when display handlers are nil
- [ ] in `Run`, start a grace timer (default 60 s, unexported field for tests) when `task_complete` arrives. If the process has not exited when it fires:
  - log one line through `OutputHandler`: `codex did not exit after task_complete; terminating`
  - kill the process tree
  - return a successful `Result` whose output is stdout when non-empty, otherwise `last_agent_message`, with the signal detected on that text
- [ ] keep current behavior when the process exits on its own: no extra delay, and the stdout result is unchanged
- [ ] make cancellation and idle-timeout paths unaffected: a parent cancel still returns the context error
- [ ] write tests with a fake runner and rollout fixtures:
  - clean exit after `task_complete`
  - hang after `task_complete` → grace kill → success with `last_agent_message`
  - hang with stdout already captured → stdout wins
  - signal detected from `last_agent_message`
  - no `task_complete` and no exit → existing timeouts still govern
  - parent cancel during grace → context error
  - tailer runs with nil display handlers
- [ ] run `go test -race ./pkg/executor/...` - must pass before task 4

### Task 4: Verify acceptance criteria
- [ ] verify all requirements from Overview are implemented
- [ ] verify edge cases: user override to `elevated`, non-Windows invocation unchanged, reviewer with idle timeout configured, missing rollout file (no grace kill possible, existing behavior)
- [ ] run `make test`
- [ ] run `make lint` - all issues must be fixed
- [ ] run `GOOS=windows GOARCH=amd64 go build ./...`, `GOOS=linux GOARCH=amd64 go build ./...`, and `GOOS=darwin GOARCH=arm64 go build ./...`
- [ ] verify test coverage of the changed executor code is 80%+

### Task 5: [Final] Update documentation
- [ ] document the Windows reviewer default and how to restore `elevated` through `codex_args` in `README.md`, `llms.txt`, and the `codex_args` comment in `pkg/config/defaults/config`
- [ ] document the `task_complete` grace kill and real Windows tree termination in `CLAUDE.md` (executor section) and the `idle_timeout` comment where relevant

## Technical Details
- **Override**: `-c windows.sandbox="unelevated"`, emitted only for `ForceReadOnly` executors on Windows, positioned before user extras.
- **Completion event**: rollout line `{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"..."}}`.
- **Grace**: 60 s from `task_complete` to tree kill. The result is treated as success because the model already finished its turn.
- **Windows termination**: Job Object with kill-on-close, then `TerminateJobObject` on kill, with `taskkill /T /F` as the fallback.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- on the affected Windows machine, run a plan with `--external-reviewers codex:<model>` and no `--codex-args`, and confirm the reviewer reads the diff and the run proceeds
- with `codex_args = -c windows.sandbox="elevated"`, confirm the override is respected (expected to hang there until the elevated setup is fixed, now bounded by the grace kill only if `task_complete` arrives)
- remove the temporary `--codex-args=-c windows.sandbox=unelevated` from the restarted runs' commands once this lands
