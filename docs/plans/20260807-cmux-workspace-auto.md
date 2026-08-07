# cmux Workspace Auto Mode (--cmux-workspace=auto)

## Overview
- Extend the shipped `--cmux-workspace` flag with an optional value: bare `--cmux-workspace` keeps today's behavior (always hand off to a new cmux workspace), while `--cmux-workspace=auto` hands off only when the current workspace is already busy with another active loopai run; otherwise the run stays in the current terminal and workspace.
- Problem it solves: with `always`, every run spawns a workspace even when the current one is free, which litters the sidebar. `auto` gives "first run stays here, parallel runs get their own card" without the user tracking which workspace is occupied.
- Busy detection reads loopai's own protocol: an active run has a non-final `loopai` status pill (`starting` during startup, then a phase label such as `task`, `review`, `external review · iteration N`, `planning`, or `rate limited`), while a finished run's pill starts with `done` or `failed` (installed by `Reporter.Finish`, deliberately persistent). A pill with a non-final text therefore means the workspace is busy. Verified empirically against live workspaces: active run shows `loopai=external review (gpt-5.6-sol:high) · iteration 2`, idle ones show `loopai=done in 3h39m` or no pill.

## Context (from discovery)
- Files/components involved:
  - `pkg/cmux/cmux.go` — `SpawnWorkspace`, `ErrNotInCmux`, availability detection (`CMUX_WORKSPACE_ID` + `exec.LookPath`), `commandRunner` abstraction (currently discards all output; busy check needs stdout capture), `Reporter.Finish` (source of the `done`/`failed` pill texts), `phaseStyles`
  - `cmd/loopai/main.go` — `CmuxWorkspace bool` option (line ~80), `handOffToCmuxWorkspace` routing (~2278, ~2318-2450), `stripCmuxWorkspaceArg` (~2517)
- Related patterns found:
  - `cmux list-status` prints one `key=value [icon=... color=... priority=...]` line per pill for the caller's workspace; `progress=` and spinner state are NOT reliable busy signals (no progress bar during plan creation; sidebar-state exposes no loading field at all)
  - go-flags optional-value pattern already used by `--merge`/`--pr`: `optional:"true" optional-value:"..."`
  - `spawnWorkspace` is the runner-injectable core kept separate for tests; follow the same split for the busy query
- Dependencies identified: none new
- Known accepted limitations (document, do not solve): a `kill -9`-ed run leaves a stale phase pill → false "busy" → one extra workspace (harmless); two simultaneous `auto` starts can both see "free" and stay in one workspace (best-effort, consistent with the whole cmux integration)

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
- Maintain backward compatibility: bare `--cmux-workspace` must behave exactly as the shipped bool flag
- Tests must never touch real `~/.config/loopai/` or `~/.config/ralphex/` and must never talk to a live cmux socket (fake runner or PATH-injected stub only)

## Testing Strategy
- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: dashboard Playwright suite unaffected (no web changes); no e2e additions needed

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## Implementation Steps

### Task 1: Add busy-workspace query to pkg/cmux
- [x] extend the runner layer with output capture: add an output-returning call (e.g. `runOutput(ctx, args...) (string, error)` on `execRunner`, surfaced through a small interface the query function accepts) — keep the existing discard-output `run` untouched for all reporter paths; set `cmd.WaitDelay` on the capturing path so a grandchild inheriting the pipe cannot block `Wait` past the context deadline (the exact hazard the discard-output design avoids)
- [x] extract the final-pill prefixes used by `Reporter.Finish` (`done`, `failed`) into shared unexported constants so `Finish` and the busy check cannot drift apart
- [x] add `WorkspaceBusy() (bool, error)`: availability check like `SpawnWorkspace` (missing env or binary → `ErrNotInCmux`); run `cmux list-status`, parse lines for a `loopai=` entry (key up to first `=`, text up to the ` icon=`/` color=`/` priority=` suffix or line end); busy = pill present and its text does not start with a final-pill prefix; no pill or final pill → not busy
- [x] write table-driven tests for the list-status line parsing (no loopai pill, final `done in 3h39m` pill, final `failed · detail` pill, phase pill with model and iteration suffix, other agents' pills present, empty output, malformed lines)
- [x] write tests for `WorkspaceBusy` availability errors (missing env, missing binary → `ErrNotInCmux`) and runner failure propagation
- [x] run tests - must pass before next task

### Task 2: Evolve --cmux-workspace flag to optional always/auto
- [x] change `CmuxWorkspace` in the options struct from `bool` to `string` with `long:"cmux-workspace" optional:"true" optional-value:"always" choice:"always" choice:"auto"` and an updated description ("relaunch in a new cmux workspace: bare/always = unconditionally, auto = only when the current workspace already runs loopai")
- [x] update every `o.CmuxWorkspace` reference: hand-off gate becomes mode-aware (`always` → current behavior; `auto` → call `cmux.WorkspaceBusy()` first: busy → hand off, free → continue locally without warning; `ErrNotInCmux` or query failure in auto mode → continue locally, debug-log only)
- [x] update `stripCmuxWorkspaceArg` to strip `--cmux-workspace=auto`/`=always` value forms as well (bare form already handled) — the relaunched child must never re-evaluate hand-off
- [x] update existing flag/hand-off tests for the bool→string change; keep a test proving bare `--cmux-workspace` still means unconditional hand-off (backward compatibility)
- [x] write tests for auto mode: busy workspace (stubbed list-status with phase pill) → spawn invoked and hand-off reported; free workspace (final pill or no pill) → no spawn, normal run proceeds; outside cmux → normal run proceeds without warning
- [x] ➕ reserve a free/query-fallback workspace with a non-final `starting` pill before lengthy startup; clear it on startup failure and hand cleanup ownership to the normal reporter
- [x] ➕ after a positive busy verdict, stop on every pre-spawn refusal or spawn failure instead of falling back into the occupied workspace
- [x] write tests for arg stripping of the value forms (table-driven: `=auto`, `=always`, mixed with other flags, repeated)
- [x] run tests - must pass before next task

### Task 3: Verify acceptance criteria
- [x] verify all requirements from Overview are implemented (auto hands off only on busy; bare flag unchanged; free workspace runs locally; no recursion)
- [x] verify edge cases are handled (stale phase pill treated as busy by design; list-status output with unrelated pills; query failure falls back to local run)
- [x] run full test suite (`make test`)
- [x] run linter (`make lint`) - all issues must be fixed
- [x] verify test coverage meets project standard (80%+) for the new code paths
- [x] cross-compile `GOOS=windows GOARCH=amd64 go build ./...`

### Task 4: [Final] Update documentation
- [x] update README.md: `--cmux-workspace[=always|auto]` semantics and the busy-detection rule
- [x] update the cmux section of CLAUDE.md (auto mode, final-pill-based busy detection, accepted limitations: stale pill after force-kill, simultaneous-start race)
- [x] update llms.txt if it enumerates CLI flags

## Technical Details
- Busy signature: startup → `loopai=starting`; active phase (verified live) → `loopai=external review (gpt-5.6-sol:high) · iteration 2 icon=person.2 color=#a855f7 priority=90`; finished → `loopai=done in 3h39m icon=bolt color=#34c759 priority=90`; never ran → no `loopai=` line.
- Detection deliberately keys on pill text, not progress or spinner: plan creation has no progress bar, and cmux exposes no queryable loading state (`sidebar-state` has no loading field even for an active workspace).
- The checker and the pill producer live in the same package: `Finish` writes `done ...`/`failed ...`, phase pills come from `phaseStyles`; shared prefix constants make the coupling explicit.
- Output capture is new to the runner layer: the existing discard-output design exists to avoid pipe-EOF hangs from grandchildren; the capturing path must set `WaitDelay` (or equivalent) to keep the same bound. `cmux list-status` spawns no children, so this is defense in depth.
- go-flags note: with `optional:"true"`, the value must be attached (`--cmux-workspace=auto`); a space-separated `auto` would parse as a positional argument. Document the `=` form.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- In a free cmux workspace: `loopai --cmux-workspace=auto <plan>` → runs in place, no new workspace
- While that run is active, same command again from the same workspace → second run hands off to a new workspace with its own card
- Bare `--cmux-workspace` → always hands off regardless of busy state
- Outside cmux: `--cmux-workspace=auto` runs locally without warning noise

**External system updates**: none — feature is fully local to loopai
