# Detached runs and T3 integration: review leftovers

- found: 2026-10-23, plan: 20261022-t3-detached-runs, phase: review
- severity: minor
- area: cmd/loopai (detach_*, runs_cmd, t3_bridge, acp_*), pkg/runs, pkg/t3

Non-blocking findings from the per-task and final reviews of the detached-runs branch, kept here
after the plan workspace was removed. None loses work or leaks a token.

Mode 2 (ACP provider):
- A Stop pressed while `acpLaunchDetached` is still waiting for the launcher's ack (up to 120 s) is
  answered only after the launch returns, and a launcher timeout leaves the run unbound (recoverable
  through T3's continuation text). Fix direction: make the launch asynchronous inside the provider.
- `acpThreadMayClaim` lets a thread keep claiming a run another thread took over when either thread id
  is unknown (Stop is unaffected; it requires `acpOwnsRun`).
- `cmd/loopai-acp`: `LOOPAI_ACP_LOOPAI` with a bare name is not PATH-resolved under `syscall.Exec`;
  resolve with `exec.LookPath` or document that it must be a path.
- `acp_provider.go`: the thread-id lookup error is wrapped twice ("look up the thread: ..."); the T3
  auth-word regex is copied in three test packages; `unbindRun` has a dead error branch; a project's
  own `.loopai/acp/.gitignore` without `*` keeps bindings visible in `git status`.

Mode 1 (bridge) and the T3 client:
- A typed error on the terminal metadata stream still disables the whole bridge (close-out cleans the
  prefix and pin).
- Late `progressLogger()` lines reach only `console.log` after the progress logger is closed; the
  comment and test name should say so.
- With `LOOPAI_T3_URL` set, a runtime file that flips between readable and unreadable flips the server
  identity, costing one extra terminal reopen.
- `pkg/t3/t3test` keys Ack/Interrupt by a normalized id (real T3 keys by the raw value); a Defect sends
  no Interrupt for live streams; two assertion gaps in `rpc_test.go` / `client_test.go`.

Run core:
- Two racing stops (`--stop` and a Mode 2 Stop) can clear the first stop's marker, so a force-killed
  run reads crashed instead of stopped.
- Ctrl+C during `--attach` of a finished run prints "the run continues"; the start-token read error is
  reported as pid reuse; a signal-0 check in `runs_signal_unix_test.go` passes on an unreaped zombie;
  the "re-exec main() with unit-separator args" test helper exists in four copies.
- `pkg/runs/journal.go`: truncating through the `O_APPEND` handle fails on native Windows (latent;
  Windows detach is refused).
- `mergeIntoBase` keeps a `merge func` parameter that now has a single value.

Docs:
- llms.txt names the sandbox variables without their value rules; README lists detached runs under
  "Without `--worktree`" although they always use one; one plan bullet reads "truncated with the
  report path kept" unconditionally.
