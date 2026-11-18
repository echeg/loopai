# Detached Run Hop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Start the `--detach` child through a short-lived loopai "hop" process so T3 Code's
`ps`-based descendant tracking of a Claude thread can no longer reach the run.

**Architecture:** The launcher starts `loopai --detached-hop <child argv>` instead of the child. The hop
(dispatched in `main` before flag parsing and the version banner) starts the child with `Setsid`,
passing fds 3 and 4, and exits at once, so the child is reparented to launchd/init. The launcher waits
for the hop (bounded), treats a non-zero exit as a start error, then sends the secret and awaits the
ack as today.

**Tech Stack:** Go 1.26, `os/exec`, `syscall`, `golang.org/x/sys/unix`, testify.

**Spec:** `docs/superpowers/specs/2026-11-18-detach-hop-design.md`

## Global Constraints

- The hop is the loopai binary itself, never `/bin/sh -c '… &'` (ignored SIGINT/SIGQUIT would be inherited).
- The hop argument is checked before `versionBannerWriter`: the hop's stdout is the console log.
- Handshake unchanged: fd 3 secret read end, fd 4 ack write end; the token travels only over fd 3.
- The systemd scope prefix wraps the hop; the retry-without-scope keeps working.
- Windows: detach stays refused; the hop entry point returns non-zero there.
- No `opts` field is added, so `TestSkillContract` is unaffected.
- After every task: `make test </dev/null` and `make lint` (see Testing in CLAUDE.md).

## Review Focus

1. A hop that never exits (a hanging scope wrapper): the launcher stops waiting after `detachAckTimeout`, reports it, and kills nothing — Task 2 test `a hop that does not finish is reported`.
2. A hop that fails: the secret is never written to a pipe nobody reads, and the error reaches the user (retry warning under a scope) — Task 2 updates the `exit 1` scope case.
3. The hop prints nothing to the console log (no version banner) — Task 1 test `TestDetachHopThroughMainPrintsNothing`.
4. The child is a session leader after the hop, not merely reparented — Task 2 asserts `SID == PID` in the stub dump.
5. A T3-style walk from the launcher after a successful launch does not reach the child — Task 2 test `TestDetachLaunchChildLeavesLauncherTree`.

---

### Task 1: Hop entry point

**Files:**
- Create: `cmd/loopai/detach_hop_unix.go`, `cmd/loopai/detach_hop_windows.go`
- Modify: `cmd/loopai/main.go` (top of `main`, before the banner)
- Modify: `cmd/loopai/main_test.go` (`TestMain`: set `detachHopArgs`)
- Test: `cmd/loopai/detach_hop_unix_test.go`

**Interfaces:**
- Produces: `const detachHopArg = "--detached-hop"` (in `detach_hop_unix.go` and the windows file, or once in a
  shared file if preferred); `func runDetachHop(argv []string, stderr io.Writer) int` — starts `argv` with
  `SysProcAttr{Setsid: true}`, `ExtraFiles` = fds 3 and 4 (each first checked with `checkHandshakeFD`),
  inherited stdout/stderr/env/cwd, stdin on `os.DevNull`; returns 0 once `Start` succeeds, 1 with
  `start the detached child: <err>` on stderr otherwise, 2 for an empty argv. Windows: writes
  `errDetachUnsupported` and returns 1.
- `main`: `if len(os.Args) > 1 && os.Args[1] == detachHopArg { os.Exit(runDetachHop(os.Args[2:], os.Stderr)) }`
  as the first statement.

- [ ] **Step 1: Write the failing tests** in `detach_hop_unix_test.go`:
  - `TestDetachHopHelper`: helper, not a test — `i := slices.Index(os.Args, detachHopArg)`; skip when `< 0`;
    else `os.Exit(runDetachHop(os.Args[i+1:], os.Stderr))`.
  - helper `startHop(t, argv []string) (exitCode int, stderr string, secretW, ackR *os.File)`: runs
    `os.Executable()` with `-test.run=^TestDetachHopHelper$ -- --detached-hop <argv>`, `ExtraFiles` two fresh
    pipes, waits for it.
  - `TestRunDetachHopPassesHandshake`: argv `/bin/sh -c 'read l <&3; echo "got $l" >&4'`; assert exit 0;
    after the hop exited, write `x\n` to the secret, read `got x\n` from the ack.
  - `TestRunDetachHopStartFailure`: argv `/nonexistent/loopai`; assert exit 1, stderr contains
    `start the detached child:`.
  - `TestRunDetachHopRequiresHandshake`: run the helper without `ExtraFiles`; assert exit 1, stderr contains
    `handshake descriptor 3`.
  - `TestDetachHopThroughMainPrintsNothing`: run the test binary via `TestDetachLaunchMainHelper`
    (`detachLaunchMainEnv` = `--detached-hop\x1f/bin/sh\x1f-c\x1fexit 0`) with the two pipes; assert exit 0
    and empty stdout.
- [ ] **Step 2: Run** `go test ./cmd/loopai -run 'DetachHop' -v` — expected FAIL (undefined `runDetachHop`).
- [ ] **Step 3: Implement** `runDetachHop`, the windows stub, and the `main` dispatch.
- [ ] **Step 4: Run** `go test ./cmd/loopai -run 'DetachHop' -v` — expected PASS; `GOOS=windows GOARCH=amd64 go build ./...` succeeds.
- [ ] **Step 5: Commit** `feat: add the detached-run hop entry point`.

### Task 2: Launcher starts the child through the hop

**Files:**
- Modify: `cmd/loopai/detach_launch.go` (`detachHopArgs` var beside `detachChildArgs`; `detachChildArgv`; `spawn`)
- Modify: `cmd/loopai/detach_launch_unix.go` (`startDetachedProcess` no longer needs `Setsid`; keep it harmless or drop it)
- Modify: `cmd/loopai/main_test.go` (`TestMain`: `detachHopArgs = []string{"-test.run=^TestDetachHopHelper$", "--"}`)
- Test: `cmd/loopai/detach_launch_unix_test.go`, `cmd/loopai/detach_launch_test.go` (argv expectations)
- Docs: `CLAUDE.md` (detached-run paragraph at "The child is the hidden `--detached-run <id>`"), `docs/t3-code.md` ("The run lives in its own session" paragraph)

**Interfaces:**
- Consumes: `detachHopArg`, the hop helper from Task 1.
- Produces: `var detachHopArgs []string` — inserted between the executable and `detachHopArg`: nothing in
  production, the hop helper selector in tests. `detachChildArgv(prefix []string, exe, id string, argv []string) []string`
  now returns `prefix + [exe] + detachHopArgs + [detachHopArg, exe] + detachChildArgs + ["--detached-run="+id] + argv`.
- `spawn`: start the hop; wait for it bounded by `detachAckTimeout` and `ctx` (a timeout or cancel returns
  `detachStart{waitErr: ...}` with `the start did not finish within <d>` / `stopped waiting for the start: <ctx err>`,
  leaving the hop alone); a non-zero exit returns `detachStart{startErr: fmt.Errorf("start the detached child: %w", err)}`
  and closes the secret pipe unwritten; only on a zero exit send the secret and `awaitDetachAck`. Drop `Process.Release`.

- [ ] **Step 1: Write the failing tests**:
  - Extend `detachStubDump` with `PPID int` (`os.Getppid()`) and `SID int` (`unix.Getsid(0)`).
  - In `TestDetachLaunchStartsRun` (or a new `TestDetachLaunchChildLeavesLauncherTree`): after a successful
    launch, assert `dump.PPID != os.Getpid()`, `dump.SID == dump.PID`, and that walking
    `ps -A -o pid=,ppid=` by `ppid` edges from `os.Getpid()` (helper `psDescendants(t, root int) []int`,
    same algorithm as T3's `claudeDescendants`) does not contain `dump.PID`.
  - `TestDetachLaunchScope`: the `"/bin/sh", "-c", "exit 1", "scope"` case now expects
    `warning: the run did not start in a systemd user scope (start the detached child: exit status 1); starting it without one\n`.
  - `TestDetachLaunchChildFailures`, new subtest `a hop that does not finish is reported`: prefix
    `/bin/sh -c 'sleep 30' scope`, `detachAckTimeout = 2 * time.Second`; assert the error contains
    `the start did not finish within 2s`.
  - Update the three `detachChildArgv` expectations in `detach_launch_test.go:285-302` to the new argv
    shape; those subtests set `detachHopArgs = nil` (restored in `t.Cleanup`) since `TestMain` sets the selector.
- [ ] **Step 2: Run** `go test ./cmd/loopai -run 'DetachLaunch|DetachChildArgv' -v` — expected FAIL on the new assertions.
- [ ] **Step 3: Implement** `detachHopArgs`, the new `detachChildArgv`, and the `spawn` changes.
- [ ] **Step 4: Run** `go test ./cmd/loopai -run 'Detach|ACP' -v` — expected PASS.
- [ ] **Step 5: Update docs**: CLAUDE.md — the child is started through the hidden `--detached-hop` process,
  dispatched in `main` before the banner, which starts it with `Setsid` and exits, so T3 Code's
  once-a-second `ps` walk of a Claude thread's descendants (`ClaudeAdapterV2.ts`, macOS and Linux
  without a user manager) can reach it only within about one `exec`; why not `sh … &`. `docs/t3-code.md` —
  one sentence that a run started from a T3 Code Claude thread survives that thread's Claude CLI restart.
- [ ] **Step 6: Run** `make test </dev/null` and `make lint` — expected: all pass, no lint issues.
- [ ] **Step 7: Commit** `fix: start detached runs outside the agent's process tree`.
