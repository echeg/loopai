# Report: Codex reviewer on Windows: unelevated sandbox and no endless hangs

Plan: `C:\Users\necro\.t3\worktrees\loopai\codex-reviewer-windows-hang\docs\plans\20261003-codex-reviewer-windows-hang.md` | Branch: `codex-reviewer-windows-hang` | Base: `master` | Mode: `full` | Executor: `codex` | Task model: `codex:gpt-6-astra:high` | Review model: `codex:gpt-6-astra:high` | Started: `2026-10-03T07:26:36Z` | Finished: `2026-10-03T08:42:29Z`

## Summary

Delivered the Windows Codex reviewer hang fix:

- Windows read-only external reviewers default to the unelevated sandbox. User-supplied `codex_args` retain precedence.
- Codex invocations that remain running for 60 seconds after the main session’s `task_complete` event terminate their process tree. Results preserve stdout or fall back to `last_agent_message`, while retaining typed stderr limit/error diagnostics.
- Windows executors use Job Objects, suspended startup, and a `taskkill` fallback to terminate descendants. Pipe cleanup prevents cancellation from leaving output readers blocked.
- Regression tests, Windows test portability fixes, and documentation cover the new behavior.

All 5 task iterations completed with 0 failed retries. Internal review first ran: true; internal review loop iterations: 1; internal review ended by: `review_done`. Post-review ran: true; post-review iterations: 1.

| Phase | duration_ms |
|---|---:|
| evaluation | 372394 |
| external review | 387805 |
| internal review | 1271764 |
| other | 0 |
| tasks | 2521027 |

The finish timestamp and timing snapshot cover work through finalize, before report assessment and archival.

## Change scope

The supplied `master...HEAD` scope contains **31 files**, **1817 additions**, and **149 deletions**, across **10 commits**.

| Status | Files | Change |
|---|---|---|
| M | `pkg/executor/codex.go`, `pkg/executor/codex_test.go` | Windows sandbox override, completion monitoring, grace termination, diagnostic preservation, and regression coverage. |
| M | `pkg/executor/executor.go`, `pkg/executor/executor_test.go`, `pkg/executor/custom.go`, `pkg/executor/custom_test.go`, `pkg/executor/procgroup_unix.go`, `pkg/executor/procgroup_windows.go` | Shared process startup, tree termination, pipe cleanup, and lifecycle tests. |
| A | `pkg/executor/procgroup_windows_test.go` | Windows process-tree containment, fallback, handle cleanup, and retained-pipe tests. |
| M | `pkg/config/agents_test.go`, `pkg/config/colors_test.go`, `pkg/config/config_test.go`, `pkg/config/defaults_test.go`, `pkg/config/prompts_test.go`, `pkg/config/values_test.go` | Windows test isolation, permission-test portability, and documented override coverage. |
| M | `cmd/loopai/main.go`, `cmd/loopai/main_test.go`, `pkg/awake/awake_test.go`, `pkg/git/repository_lock_windows_test.go`, `pkg/processor/executor_factory_test.go` | Lint fixes, portable fixtures, filesystem-aware testing, and deterministic awake-expiry testing. |
| M | `pkg/t3/client.go`, `pkg/t3/client_test.go`, `pkg/t3/reporter.go`, `pkg/t3/reporter_test.go` | Active-run thread pinning, preservation of existing user pins, best-effort unpinning, and tests. |
| M | `CLAUDE.md`, `README.md`, `docs/t3-code.md`, `llms.txt`, `pkg/config/defaults/config` | Sandbox, completion cleanup, configuration, and T3 behavior documentation. |
| A | `docs/plans/20261003-acp-agent-mode.md`, `docs/plans/20261003-codex-reviewer-windows-hang.md` | ACP design plan and the completed Windows reviewer implementation plan. |

Commits, in supplied order:

- `f7596a08ef5ae763fdc5fde1e8cb16c398804950` — fix: address external review findings
- `48e10eacab2479e52f01f02e83cdb72014215c97` — fix: address code review findings
- `2bae4245c1aff42308c271fd163db2a3c94ec0f8` — feat: document Windows Codex reviewer sandbox and completion cleanup
- `d0ee97f8c93d60d6157fcb6fc4b1666af14add09` — feat: verify Windows codex reviewer acceptance criteria
- `19606e5105339234d8352a1c3358ef0c0ea7b8fc` — feat: finish stuck codex sessions after task completion
- `f43c40db42fa1d224df4de8322755a49db4750d3` — feat: terminate Windows executor process trees
- `10a62c1e666e2df96529c61eed4f1530c2e91c05` — feat: default Windows codex reviewers to unelevated sandbox
- `76de2f7a0b01788a8de8ce8ac962ba4a2ac3c5c6` — docs: save plan 20261003-codex-reviewer-windows-hang
- `ff756590cc3a31e3505093a8f2310903017991d6` — docs: save plan 20261003-acp-agent-mode
- `7f0e383885498b19491208948d8900c5e397a700` — feat(t3): pin the run's thread while loopai is active

## Risk

**medium**

The principal risk is concurrency and process lifecycle behavior: suspended Windows startup, Job Object assignment, exit monitoring, cancellation, pipe closure, and completion timers affect shared executor infrastructure. Serialized handle cleanup and regression coverage mitigate these risks.

Public API changes in the broader branch are additive T3 pin/unpin command constructors and a `PinnedAt` field. The T3 JSON representation gains `pinnedAt`; no persisted data schema or database migration is introduced.

Configuration changes are limited to the Windows read-only reviewer default. Existing user overrides remain effective, phase sandbox defaults remain unchanged, and no new configuration key or dependency is required.

Completion recovery depends on locating the rollout and observing the main session’s `task_complete`. Without that event, existing timeout behavior still governs. The supplied facts do not establish completion of the plan’s live Windows reviewer checks.

## Migrations and operational steps

none

## Plan deviation

All five implementation tasks are marked complete. The implementation follows the planned sandbox override, 60-second completion grace period, Windows tree termination, acceptance coverage, and documentation work. Explicit pipe closure implements the plan’s allowed alternative to `WaitDelay`.

Recorded drift:

- Added: none.
- Blocked: none.
- Skipped: none.

Supporting work documented in the plan includes Windows fixture and environment isolation fixes, lint corrections, and an awake-expiry scheduling test fix. Internal review hardened suspended startup and job closure when launchers exit. External review added preservation of stderr limit/error diagnostics after grace termination.

The complete `make test` target ran in an isolated, unprivileged WSL checkout because native shell fixtures encountered Git Bash symlink normalization issues. An additional native whole-repository run was stopped after eight minutes and was explicitly not counted as passing.

The broader branch diff also contains the earlier T3 thread-pinning implementation and ACP plan document. These are included in the supplied change totals, rather than recorded as added tasks for this plan.

The plan leaves live Windows reviewer checks and removal of temporary unelevated-sandbox command overrides as post-completion follow-up. Their completion is not established by the supplied facts; they are not recorded as blocked or skipped implementation items.

## Backlog

There were no supplied backlog entries or backlog files. No external-review finding was filed to backlog.

## External review

### claude:opus:high

- label: claude
- iterations: 2
- duration_ms: 760002
- ended by: done
- had findings: true
- Iteration 1 truncated: false
- Iteration 2 truncated: false

Finding disposition:

- The 60-second grace kill could turn a failed Codex session into a successful empty review by discarding stderr usage-limit or error diagnostics -> fixed. The result now preserves prefix-gated stderr `LimitPatternError` and `PatternMatchError`, with limits taking precedence. Stdout and `last_agent_message` remain excluded from pattern matching, and captured output is retained. Seven regression cases cover failure diagnostics, precedence, output preservation, and false-positive avoidance.

Iteration 1’s evaluator accepted the finding and reported the fix. Iteration 2 reported **NO ISSUES FOUND** and confirmed the correction. No findings were dismissed or backlogged.

The reviewer’s `duration_ms: 760002` and the phase snapshot’s external-review duration of `387805` are retained independently as supplied.

## Validation

The deterministic validation record contains:

- Commands: none.
- duration_ms: 0
- runs: 0

Separately, the plan and supplied review narratives report successful validation with the following commands; individual timings were not supplied:

- `go test ./pkg/executor/... ./pkg/processor/...`
- `go test -race ./pkg/executor/...`
- `go test ./pkg/config/... ./pkg/executor/... ./pkg/processor/...`
- `go test -race ./pkg/executor/... ./pkg/processor/... ./pkg/config/...`
- `make test`
- `make lint`
- `GOOS=windows GOARCH=amd64 go build ./...`
- `GOOS=linux GOARCH=amd64 go build ./...`
- `GOOS=darwin GOARCH=arm64 go build ./...`
- `git diff --check`
- `go test -race -count=1 -run 'TestCodexExecutor_Run_TaskComplete|TestCodexExecutor_Run_WithoutMainTaskComplete' ./pkg/executor/`
- `go vet ./pkg/executor/`
- `GOOS=linux go build ./...`

The plan’s internal-review follow-up reports native executor coverage of 90.5% and Windows cleanup coverage of 82.5%. External-review evaluation reports repository-wide lint with 0 issues. These narrative results do not alter the deterministic validation totals above.