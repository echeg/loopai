# Detached run start through a short-lived hop

Approved 2026-11-18.

## Intent

A plan run started with `loopai --detach` from a T3 Code Claude thread must not be killed when T3
Code restarts that thread's Claude CLI (idle release after an hour, app restart, access-mode switch).

## Why it dies today

T3 Code 0.1.10 (`apps/server/src/orchestration-v2/Adapters/ClaudeAdapterV2.ts`) runs, whenever the
Claude CLI has no systemd scope (macOS, Linux without a user manager), `ps -A -o pid=,ppid=,pgid=,lstart=`
every second (`CLAUDE_DESCENDANT_POLL`). It walks `ppid` edges from the CLI and from every tracked
process still alive, and remembers each descendant by pid plus start time. When the CLI exits it
SIGKILLs every tracked process, the whole group of a group leader. The detached child is the
launcher's direct child for up to 120 s while the launcher waits for the ack, so it is tracked almost
certainly, and its later descendants (codex, `caffeinate`) are tracked through it. `Setsid` does not
help: the walk ignores sessions.

## Goals and non-goals

- Goal: the detached child is no longer reachable from the Claude CLI through `ppid` edges in any `ps`
  snapshot except during a window of about a millisecond per launch.
- Goal: the handshake (fd 3 secret, fd 4 ack), the token's never-on-disk property, start-error
  reporting, and the systemd-scope fallback behave as today.
- Non-goal: a 100% guarantee (launchd spawning), an upstream T3 Code change, Windows (detach is
  refused there).

## Approaches considered

- A, chosen: a short-lived intermediate process that starts the child and exits at once, so the child
  is reparented to launchd/init. Small change, keeps the handshake.
- B: spawn through launchd (`launchctl submit`/`bootstrap`). Full guarantee, but no fd inheritance:
  the handshake and token transfer would move to a unix socket, environment passed explicitly,
  macOS-only, Linux without systemd still open. Several times the work.
- C: leave loopai alone; rely on the skills' background-wait guard (at most 4 h) or Mode 2. Not a fix.

Within A, the hop is the loopai binary itself rather than `/bin/sh -c '"$@" &'`: a non-interactive
shell starts an asynchronous list with SIGINT and SIGQUIT ignored, and an ignored disposition survives
`exec` into every process the run starts. The cost is a window of about one `exec` (~1 ms) instead of
~0.1 ms, roughly one tracked launch in a thousand.

## Design

```
before:  claude → bash → loopai --detach → loopai --detached-run
after:   claude → bash → loopai --detach → loopai <hop> → loopai --detached-run
                                            └ exits right after starting the child
```

- `main` checks a hidden first argument before flag parsing and before the version banner; in that
  mode the process starts the remaining argv with `Setsid`, passing on fds 3 and 4 (checked with
  `checkHandshakeFD`), its environment, working directory, stdout and stderr (the console log), and
  exits 0, or non-zero with the error on stderr when the start fails.
- The launcher starts the hop instead of the child and waits for it. A hop that exits non-zero is a
  start error; the ack pipe's EOF still means the child ended before answering.
- The systemd scope prefix wraps the hop; the child inherits the scope's cgroup. The existing
  retry-without-scope keeps working.
- Windows is untouched.

## Tests

- After a launch, the child's parent is not the launcher (test) process, and a reimplementation of
  T3's walk (`ps` plus `ppid` from the test process) does not reach it.
- A child that cannot be started still surfaces as a start error.

## Documentation

CLAUDE.md (detached-run paragraph) and `docs/t3-code.md`.

## Left to the builder

The hidden argument's spelling, how tests substitute the hop executable, and where the T3-walk test
helper lives.
