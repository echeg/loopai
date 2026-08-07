# cmux Workspace Hand-off Flag (--cmux-workspace)

## Overview
- Add a `--cmux-workspace` boolean CLI flag. When set and loopai runs inside cmux, loopai creates a new cmux workspace and relaunches itself there (`cmux new-workspace --name <branch> --cwd <dir> --command "<self relaunch>"`), then exits. The actual run inherits the new workspace's `CMUX_WORKSPACE_ID`, so the sidebar card, status pill, spinner, and progress bar belong to that run alone.
- Problem it solves: multiple parallel loopai runs started from one cmux workspace share one sidebar card and overwrite each other's status (fixed `loopai` status key, keyless per-workspace progress bar). Hand-off gives each run its own card with zero changes to the reporting path.
- Outside cmux (no `CMUX_WORKSPACE_ID` or no `cmux` binary in PATH), or when workspace creation fails, loopai logs a warning and continues the run normally in the current terminal (best-effort, matching the existing cmux integration philosophy).

## Context (from discovery)
- Files/components involved:
  - `cmd/loopai/main.go` — options struct (go-flags, kebab-case long names, lines ~40-79), startup routing before dependency construction (`--clear`/`--merge` pattern), reporter wiring
  - `pkg/cmux/cmux.go` — `commandRunner` abstraction, `New()` availability detection (`CMUX_WORKSPACE_ID` env + `exec.LookPath("cmux")`), best-effort exec helpers
  - `pkg/plan` — `plan.ExtractBranchName(planFile)` for deriving the workspace name
- Related patterns found:
  - cmux CLI: `cmux new-workspace [--name <title>] [--cwd <path>] [--command <text>] [--focus <true|false>]`; `--command` sends text+Enter to the new workspace's shell after creation
  - tests replace `commandRunner` with a fake recording argv (`pkg/cmux/cmux_test.go`); `cmd/loopai` tests unset `CMUX_WORKSPACE_ID` in TestMain and use `t.Setenv` plus PATH-injected stubs
- Dependencies identified: none new; only os/exec and existing packages

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
- Maintain backward compatibility (flag is additive; default behavior unchanged)
- Tests must never touch real `~/.config/loopai/` or `~/.config/ralphex/` and must not talk to a live cmux socket (always stub the runner or the binary)

## Testing Strategy
- **Unit tests**: required for every task (see Development Approach above)
- **E2E tests**: dashboard Playwright suite is unaffected (no web changes); no e2e additions needed

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## Implementation Steps

### Task 1: Add workspace spawn support to pkg/cmux
- [x] add `SpawnWorkspace(name, cwd string, argv []string) error` to `pkg/cmux/cmux.go`: package-level function that verifies availability (`CMUX_WORKSPACE_ID` set and `cmux` in PATH; return a distinct sentinel error such as `ErrNotInCmux` when unavailable), shell-quotes `argv` into a single command string, and runs `cmux new-workspace --name <name> --cwd <cwd> --focus true --command <cmd>` with the existing 2s timeout
- [x] unlike the best-effort reporter methods, propagate the CLI error to the caller (the caller decides to fall back to a local run); route execution through `commandRunner` so tests can inject a fake
- [x] add a small POSIX single-quote shell-quoting helper (unexported) for building the `--command` string
- [x] write tests for `SpawnWorkspace` success: fake runner records argv; assert exact `new-workspace` arguments including quoted command
- [x] write tests for `SpawnWorkspace` errors: missing env → `ErrNotInCmux`, missing binary → `ErrNotInCmux`, runner failure propagated
- [x] write table-driven tests for the shell-quoting helper (spaces, single quotes, empty string, unicode)
- [x] run tests - must pass before next task
- ➕ `new-workspace` is bounded by a new `spawnTimeout` of 10s rather than the 2s `execTimeout` this task assumed: it starts a terminal instead of updating a label, and a premature kill is ambiguous rather than cosmetic, because cmux may already have created the workspace while the caller reads the error as a failure and runs the plan locally too

### Task 2: Wire --cmux-workspace flag and hand-off in cmd/loopai
- [x] add `CmuxWorkspace bool` option with `long:"cmux-workspace"` and description "relaunch this run in a new cmux workspace so it gets its own sidebar card (no-op with a warning outside cmux)" to the options struct in `cmd/loopai/main.go`
- [x] add an arg-rebuild helper that takes `os.Args[1:]` and strips `--cmux-workspace` (bare and `=value` forms) — this is the recursion guard for the relaunched command
- [x] resolve the self-executable via `os.Executable()` and build `argv = [exe, filteredArgs...]`
- [x] add hand-off routing early in the run path (before executor resolution, config-independent, alongside the standalone close-out routing): when the flag is set, derive the workspace name from `plan.ExtractBranchName(planFile)` (fallback: `loopai`), call `cmux.SpawnWorkspace(name, cwd, argv)`; on success print "handed off to cmux workspace <name>" and exit 0; on `ErrNotInCmux` or any spawn failure print a warning and continue the normal run in the current terminal
- [x] ensure hand-off applies to both plan-execution and interactive `--plan` creation paths (interaction then happens in the new workspace terminal)
- [x] write table-driven tests for the arg-strip helper (flag absent, bare flag, `--cmux-workspace=true`, flag repeated, flag mixed among other args)
- [x] write tests for hand-off routing: outside cmux → warning and normal run continues (env unset); inside cmux with PATH-injected `cmux` stub → stub receives `new-workspace` call with expected name/cwd/command and process path exits before executor resolution; stub failing → warning and normal run continues
- [x] run tests - must pass before next task
- ➕ hand-off is routed from `handleEarlyFlags` (still config-independent, before executor resolution) instead of a separate branch in `run`, which keeps `run` under the gocyclo limit
- ➕ standalone commands (`--clear`, `--merge`, `--pr`, `--init`, `--dump-defaults`, reset-only) are never handed off; the existing predicate in `clearStaleCmuxStatus` was extracted as `isStandaloneCommand` and shared
- ➕ a `--branch` override wins over the plan-derived name, matching the branch the worktree run will actually use

### Task 3: Verify acceptance criteria
- [x] verify all requirements from Overview are implemented (own card per run inside cmux, warning fallback outside cmux, no recursion, no behavior change without the flag)
- [x] verify edge cases are handled (plan file missing/empty branch name, args with spaces and quotes, `os.Executable` error → warning fallback)
- [x] run full test suite (`make test`)
- [x] run linter (`make lint`) - all issues must be fixed
- [x] verify test coverage meets project standard (80%+) for the new code paths
- [x] cross-compile `GOOS=windows GOARCH=amd64 go build ./...` (quoting helper and exec path are platform-sensitive)
- ➕ added a `cmuxWorkspaceName` case for a plan path deriving an empty branch (`docs/plans/.md` → `loopai`), which was the one uncovered fallback
- ➕ added a hand-off case for an unresolvable working directory; it self-skips where the platform still resolves a removed cwd (macOS does), so the branch is exercised only where it can be
- ⚠️ the `os.Executable` failure branch stays verified by inspection: it cannot be provoked without injecting a seam into production code, and it mirrors the covered `os.Getwd` branch exactly

### Task 4: [Final] Update documentation
- [x] update README.md flag list/usage with `--cmux-workspace`
- [x] update the cmux section of CLAUDE.md (hand-off is part of the cmux integration contract: best-effort, never affects execution)
- [x] update llms.txt if it enumerates CLI flags
- ➕ README gains a feature bullet, a `Common commands` example, and a hand-off subsection in `Progress and dashboard` next to the existing cmux paragraph
- ➕ llms.txt gains a hand-off paragraph after the cmux pill paragraph plus a `--cmux-workspace` entry in `Useful options`

## Technical Details
- Hand-off command shape: `cmux new-workspace --name <branch> --cwd <pwd> --focus true --command '<exe> <filtered args>'`
- The `--command` string is sent to the new workspace's shell as text+Enter, so every argv element must be single-quote shell-escaped (`'` → `'\''`); macOS/zsh and POSIX shells both accept this form. cmux is macOS-only, so Windows quoting is out of scope (the flag falls back with a warning there because the binary is absent).
- Recursion guard: the relaunched command omits `--cmux-workspace`, so the child performs a normal run.
- The workspace name intentionally matches the plan branch (same derivation as worktree branches) so sidebar cards line up with `.loopai/worktrees/<branch>` runs.
- `SpawnWorkspace` must not reuse `Reporter`'s swallow-errors exec: hand-off needs the error to decide between exit-after-hand-off and local fallback.
- Progress/pill isolation rationale: `cmux set-progress` has no key (one bar per workspace) and the pill key is the fixed constant `loopai`; per-run isolation is therefore only achievable at the workspace boundary, which is exactly what the hand-off provides.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- Inside cmux: run `loopai --cmux-workspace <plan>` from an existing workspace; confirm a new sidebar card appears, focus switches to it, and status pill/progress track that run only
- Run two plans in parallel with the flag; confirm two independent cards
- Outside cmux (plain Terminal.app): confirm the warning is printed and the run proceeds normally

**External system updates**: none — feature is fully local to loopai
