# ralphex

Autonomous plan execution with Claude Code - Go rewrite of ralph.py.

## LLM Documentation

See @llms.txt for usage instructions and Claude Code integration commands.

## Build Commands

```bash
make build      # build binary to .bin/ralphex
make test       # run tests with coverage
make lint       # run golangci-lint
make fmt        # format code
```

### Updating Dependencies

`go get -u ./...` does NOT update dependencies behind build tags. The `e2e/` package uses `//go:build e2e`, so playwright-go and other e2e-only deps require a separate update:

```bash
go get -u ./...                                          # update main deps
go get -u -tags=e2e github.com/playwright-community/playwright-go  # update e2e deps
go mod tidy && go mod vendor                             # tidy and re-vendor
```

## Project Structure

```
cmd/ralphex/        # main entry point, CLI parsing
pkg/cmux/           # best-effort cmux terminal sidebar reporting (spinner, phase pill, progress, notifications)
pkg/config/         # configuration loading, defaults, prompts, agents
pkg/executor/       # claude and codex CLI execution
pkg/git/            # git operations (external git CLI)
pkg/input/          # terminal input collector (fzf/fallback, draft review)
pkg/notify/         # notification delivery (telegram, email, slack, webhook, custom)
pkg/plan/           # plan file selection, parsing, and manipulation
pkg/processor/      # pipeline coordinator, prompt rendering, executor policy, signal wrappers
pkg/processor/phase/ # task/review/external/finalize/plan phase engines
pkg/progress/       # timestamped logging with color
pkg/status/         # shared execution model types: signals, phases, sections
pkg/web/            # web dashboard, SSE streaming, session management
e2e/                # playwright e2e tests for web dashboard
scripts/            # utility scripts organized by function
scripts/ralphex-dk/ # Docker wrapper script (Python) with tests
scripts/codex-as-claude/ # codex wrapper for Claude-compatible output
scripts/copilot-as-claude/ # GitHub Copilot CLI wrapper for Claude-compatible output
scripts/gemini-as-claude/ # gemini wrapper for Claude-compatible output
scripts/agy-as-claude/ # Antigravity (agy) CLI wrapper for Claude-compatible output
scripts/pi-as-claude/ # pi wrapper for Claude-compatible output
scripts/hg2git/     # Mercurial-to-git translation script with tests
scripts/opencode/   # opencode wrapper scripts with tests
scripts/internal/   # internal dev/CI scripts (prep-toy-test, init-docker, etc.)
docs/plans/         # plan files location
```

## Code Style

- Use jessevdk/go-flags for CLI parsing
- All comments lowercase except godoc
- Table-driven tests with testify
- 80%+ test coverage target
- Documentation: use `--flag=value` form for long CLI flags with values (not `--flag value`)

## Key Patterns

- Plan format: Checkboxes (`- [ ]` / `- [x]`) belong only in Task sections (`### Task N:` or `### Iteration N:`). The `Task` / `Iteration` keywords are structural tokens matched by `pkg/plan/parse.go` (`taskHeaderPattern`) and MUST stay in English even when plan content is written in another language — task titles and body text may be localized, but the section header keyword is fixed. Success criteria, Overview, and Context should not use checkboxes — they cause extra loop iterations. The task prompt handles them when present, but plan authors should avoid them.
- Plan file rename tolerance: two layers prevent the task phase looping when a plan file is renamed mid-run. (a) `make_plan.txt` does not ask the LLM to `git mv` the plan into `completed/` — the framework calls `MovePlanToCompleted` at end-of-run idempotently using `r.cfg.PlanFile`'s exact basename. (b) `planLocator.Path` (`pkg/processor/plan_locator.go`) and `MovePlanToCompleted` (`pkg/git/service.go`) probe an alternate-date-format basename (`YYYY-MM-DD-<slug>` ↔ `YYYYMMDD-<slug>`) both alongside the original path (in-place rename) and under `completed/`; the in-place alternate is probed before the `completed/` paths so a current renamed file wins over a stale completed copy. `MovePlanToCompleted` also treats an alternate-named file in the original directory as the move source. `TaskPhase.HasUncompletedTasks` (`pkg/processor/phase/task.go`) treats `fs.ErrNotExist` from `ParsePlanFile` as "no uncompleted tasks" rather than "assume incomplete".
- Signal-based completion detection (COMPLETED, FAILED, REVIEW_DONE signals) — constants in `pkg/status/`
- `status.PhaseHolder.OnChange` appends observers rather than replacing one callback — the web dashboard (`pkg/web/broadcast_logger.go`) and the cmux sidebar reporter (`pkg/cmux`) both subscribe, so registering does not clobber an earlier subscriber. Observers fire in registration order, only on an actual phase change, and outside the mutex (so they may call back into the holder); the slice is snapshotted under the lock and a nil callback is ignored at registration
- Processor phase architecture: `Runner` in `pkg/processor/runner.go` coordinates mode sequencing only. Task, internal review, external review, finalize, and plan creation behavior lives in `pkg/processor/phase` and is injected through consumer-side interfaces in `pkg/processor`. Shared prompt rendering, executor retry/timeout policy, and plan location stay in `pkg/processor`; break handling and git snapshots are phase-owned shared support. Keep new phase behavior out of `Runner`; preserve late-bound setters through `phase.Deps`.
- Plan creation signals: QUESTION (with JSON payload), PLAN_DRAFT (full draft content), and PLAN_READY
- Streaming output with timestamps
- Progress logging to files
- Progress file locking (flock) for active session detection
- Watch-mode dashboard reactivates completed sessions on fsnotify Write events, resuming tailing from `Session.lastOffset` — recovery path for the flock race in `RefreshStates` that can prematurely mark a running session completed. `Session.Reactivate()` is idempotent and scoped to the written path; `loadProgressFileIntoSession` records `lastOffset` after the initial load so reactivation does not re-emit replayed events
- Progress file fresh start: files ending in a `Completed:` footer are truncated on reuse; files ending in a `Failed:` footer (written by `Logger.SetFailed` before `Close`) or with no footer preserve content and write a `--- restarted at ... ---` separator, so retried failed/aborted runs keep history. `SetFailed` is called in `cmd/ralphex/main.go` for `r.Run` errors (including `ErrUserAborted`), dashboard start errors, and errors from `runWithWorktree`
- `--codex` is an executor switch (not a new pipeline mode): sets `cfg.Executor = config.ExecutorCodex` so task, both internal reviews, external-finding evaluation, and finalize run through `CodexExecutor`(s) with `MultiAgent=true` (enables `features.multi_agent`, registers the `reviewer` agent for spawn_agent calls). `external_review_tool = auto` resolves to external Claude `opus:xhigh`; the reverse default is external Codex for a Claude primary. `--codex --external-only` and deprecated `--codex --codex-only` are valid. `--pass-claude-md` (codex executor only) sets `CodexExecutor.PassClaudeMd = true`. The `Mode` enum is unchanged; the `Executors` struct uses role-named fields (`Task`/`Review`/`External`/`Custom`), and `buildCodexExecutors` wires one codex instance into both `Task` and `Review` when the resolved review model/effort matches task, or two distinct instances when they differ. Review prompts are shared with claude — the `{{agent:<name>}}` expansion in `pkg/processor/prompts.go` reads `cfg.AppConfig.Executor` and emits `Use the Task tool` (claude) or `spawn_agent(agent='reviewer', task='...')` (codex). Codex config is passed as additive `-c` overrides per invocation by `(*CodexExecutor).configOverrides()` in `pkg/executor/codex.go`, layered on top of the user's `~/.codex/config.toml` so user customizations are preserved. ralphex never writes to `~/.codex/`; for user-level CLAUDE.md it prints a one-time hint to `ln -s ~/.claude/CLAUDE.md ~/.codex/AGENTS.md`
- Codex review-phase directives: `prependCodexReviewGuidance` (`pkg/processor/prompts.go`) injects a `=== Codex orchestration directives ===` block through `promptBuilder.FirstReviewPrompt` and `promptBuilder.SecondReviewPrompt` when `cfg.isCodexExecutor()` is true (no-op for claude). Covers two codex multi_agent quirks: (a) spawn_agent must pass only `agent` and `task` — `fork_context=true` with explicit `agent_type` is rejected by the codex API; (b) on a `wait_agent` timeout for a sub-agent that died mid-tool-call, re-spawn that agent ONCE then proceed with partial results. Section-level injection works for embedded and customized review prompts alike; `phase.ReviewPhase` consumes the final prompts.
- Codex task-phase skill-conflict directive: `prependCodexTaskGuidance` (`pkg/processor/prompts.go`) injects the `=== Codex task-execution directives ===` block (`codexTaskGuidance`) through `promptBuilder.TaskPrompt` when `cfg.isCodexExecutor()` is true (no-op for claude). `phase.TaskPhase` consumes the final prompt. It tells codex that ralphex's task prompt is authoritative and a conflicting auto-activated skill from `~/.codex/skills/` must not be followed. Deliberately generic (names no specific skill); a soft prompt-level mitigation, not a hard guard — codex 0.133.0 has no per-invocation skill-disable flag. Task-phase only
- Codex output streaming: codex has no `stream-json` equivalent, so assistant message text + tool dispatch land only in the session rollout file at `~/.codex/sessions/<y>/<m>/<d>/rollout-<ts>-<session-id>.jsonl`. `CodexExecutor.Run` extracts the session id from the stderr header banner (`extractSessionID` + buffered `sessionIDCh`) and spawns `tailRolloutFile` to follow it. `formatRolloutEvent` forwards only assistant message text — reasoning records are covered by the stderr bold-summary stream, `function_call` records are skipped as tool-machinery noise. `tailCtx` is canceled after stdout EOF so the tailer drains once more and exits
- Codex stderr filtering: `shouldDisplay` (`pkg/executor/codex.go`) suppresses the per-iteration startup banner, but on the executor's first `Run()` call (`headerEmitted atomic.Bool`) whitelists three header lines — `model:`, `sandbox:`, `reasoning effort:` — so users see what codex resolved from `~/.codex/config.toml`. Bold reasoning summaries always flow through. The ralphex-side banner (`printExecutorInfo`, `cmd/ralphex/main.go`) emits `sandbox:` (and `model:` / `reasoning effort:` when `codex_model` / `codex_reasoning_effort` are set; empty values skipped)
- `--plan-model`/`--task-model`/`--review-model` resolve per-phase model/effort. `plan_model` falls back to `task_model`; `review_model` falls back to `task_model`. Claude mode injects `--model`/`--effort` into `claude_command`. Codex mode: `ResolveCodexModelEffort` (`pkg/processor/executor_factory.go`) resolves the `model[:effort]` spec against `codex_model`/`codex_reasoning_effort` defaults; `buildCodexExecutors` builds a separate review `CodexExecutor` when review differs from task. `max` effort does not exist in codex — kept default, `maxDropped` reported, `codexModelBanner` / `codexPlanBanner` (`cmd/ralphex/main.go`) warns
- External review resolution is centralized in `resolveExternalReviewSelection` (`cmd/ralphex/main.go`) for CLI/config precedence, dependency checks, banners, run metadata, and processor construction. `auto` selects the other first-class provider; `codex_enabled = false` disables only auto selection except in `ModeCodexOnly`. Missing auto-selected external binaries warn and disable the external phase, while missing primary or explicitly selected binaries fail startup. `external_review_model` is independent of `review_model`: Claude defaults to `opus:xhigh`; Codex defaults to `codex_model`/`codex_reasoning_effort`; each explicit `model[:effort]` half overlays the provider default. `custom` plus a non-empty explicit external model is invalid; explicit same-provider review is allowed with a weak-signal warning
- External Claude uses `ClaudeExecutor.ExternalReview`: configured command/wrapper args, auth, `CLAUDE.md`, skills, hooks, MCP, Bash, streaming, patterns, cancellation, and timeouts are preserved, while permission bypass/conflicting flags are sanitized before adding `--permission-mode=plan` and denying built-in `Edit`, `Write`, and `NotebookEdit`. This is prompt/tool protection, not an OS-level sandbox, because Bash remains available. External Codex keeps `read-only` sandboxing. External reviewers report findings only; the primary `Review` executor owns all edits and commits
- External review completion uses `status.ExternalReviewDone` (`<<<RALPHEX:EXTERNAL_REVIEW_DONE>>>`); `status.CodexDone` remains accepted for customized prompts and historical logs. Provider-aware status/section labels name the reviewer and evaluator, while legacy Codex/Claude phase parsing remains for old progress files. External completion is non-terminal for the whole run

### Finalize Step

Optional post-completion step after successful review phases. Triggers on `ModeFull`, `ModeReview`, `ModeCodexOnly`. Disabled by default (`finalize_enabled`). Runs once, no signal loop — best effort (failures logged, don't block success). Default behavior when enabled: rebase commits onto default branch, optionally squash, run tests.

Key files:
- `pkg/processor/phase/finalize.go` - `FinalizePhase.Run()` method called at end of review modes
- `pkg/config/defaults/prompts/finalize.txt` - default finalize prompt

### External Review

`external_review_tool` accepts `auto`, `claude`, `codex`, `custom`, or `none`. `auto` chooses the provider opposite the primary executor. Custom scripts receive the prompt file path as their single argument and output findings to stdout for the primary executor to evaluate.

- `{{DIFF_INSTRUCTION}}` expands per iteration: first `git diff main...HEAD`, subsequent `git diff` (uncommitted only)
- `max_external_iterations` 0 = auto, `max(3, max_iterations/5)`
- `review_patience` stalemate detection: terminates after N consecutive rounds without commits or working-tree changes (0 = disabled)
- `session_timeout`/`idle_timeout` (see Configuration): in default Claude mode external Codex/custom review retains legacy exclusions; under `--codex` timeout policy covers executor calls, including external Claude
- Manual break: Ctrl+\ pauses task phase (fresh session re-reads plan on resume), terminates external review immediately. Break channel is repeatable (send-on-channel, not close-once); `SetPauseHandler()` sets the task pause callback. Not on Windows
- `codex_enabled = false` backward compatibility: disables `auto` external selection outside `ModeCodexOnly`; explicit providers remain enabled
- New evaluation prompts emit `EXTERNAL_REVIEW_DONE`; legacy `CODEX_REVIEW_DONE` remains accepted

Key files:
- `pkg/executor/custom.go` - CustomExecutor for running external scripts
- `pkg/config/defaults/prompts/codex_review.txt` / `codex.txt` - external Codex prompts
- `pkg/config/defaults/prompts/external_claude_review.txt` / `external_claude_eval.txt` - findings-only external Claude prompts
- `pkg/config/defaults/prompts/custom_review.txt` / `custom_eval.txt` - custom reviewer prompts
- `pkg/processor/prompts.go` and `pkg/processor/prompt_builder.go` - `getDiffInstruction()`, `buildPreviousContext()`, prompt assembly
- `pkg/processor/phase/external_review.go` - tool selection and external review loop

### Alternative Providers for Claude Phases

`claude_command`/`claude_args` replace Claude Code with any `stream-json`-compatible CLI. Included wrappers: `scripts/codex-as-claude/codex-as-claude.sh`, `scripts/copilot-as-claude/copilot-as-claude.sh`, `scripts/gemini-as-claude/gemini-as-claude.sh`, `scripts/agy-as-claude/agy-as-claude.sh`, `scripts/opencode/opencode-as-claude.sh`, `scripts/pi-as-claude/pi-as-claude.sh`. Wrappers must ignore unknown flags gracefully (`*) shift ;;`) — default Claude flags may still be passed via config fallback. See `docs/custom-providers.md`.

Env vars:
- Codex: `CODEX_MODEL`, `CODEX_SANDBOX`, `CODEX_VERBOSE`
- Copilot: `COPILOT_MODEL`, `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_TOKEN`
- pi: `PI_PROVIDER`, `PI_MODEL`, `PI_THINKING`, `PI_VERBOSE`, `PI_EXTRA_ARGS`
Copilot wrapper: native autopilot mode — `--autopilot --no-ask-user --allow-all` for task/review, `--autopilot --allow-all` for plan runs (so `QUESTION` signals surface).
pi wrapper: line-buffers pi's token-level text deltas so `<<<RALPHEX:...>>>` signals land intact in one `content_block_delta`; suppressed events emit empty keepalive deltas so `idle_timeout` doesn't fire during silent tool runs; literal `<<<RALPHEX:` on re-emitted stderr is neutralized to `<<< RALPHEX:`; the prompt reaches pi via stdin (temp-file redirect), never argv; translation jq runs in the background with an interruptible `wait` so the TERM-forwarding trap fires while pi is alive. Task/review phases only — plan creation mode has no pi adapter.

### AWS Bedrock Provider (Docker Wrapper Only)

`scripts/ralphex-dk.sh` supports AWS Bedrock as a Claude provider (`--claude-provider bedrock` / `RALPHEX_CLAUDE_PROVIDER`). See `docs/bedrock-setup.md`.

Key functions in `scripts/ralphex-dk.sh`:
- `get_claude_provider()` - returns provider from CLI flag or env var
- `build_bedrock_env_args()` - builds docker -e flags for BEDROCK_ENV_VARS
- `export_aws_profile_credentials()` - exports credentials from AWS profile
- `validate_bedrock_config()` - validates bedrock config, returns warnings

### Docker Socket Support (Docker Wrapper Only)

`--docker` flag (or `RALPHEX_DOCKER_SOCKET`) mounts the host Docker socket for testcontainers. Socket path from `DOCKER_HOST` (unix://) or `/var/run/docker.sock`; GID auto-detected and passed via `DOCKER_GID`. Missing socket = fail-fast error.

Key functions in `scripts/ralphex-dk.sh`:
- `is_docker_enabled()` - checks CLI flag and `RALPHEX_DOCKER_SOCKET` env var
- `resolve_docker_socket()` - resolves socket path from `DOCKER_HOST` or default
- `get_docker_socket_gid()` - detects socket file GID via `os.stat()`

### Docker Network Mode (Docker Wrapper Only)

`--network MODE` flag (or `RALPHEX_DOCKER_NETWORK`) passes `--network <value>` to `docker run` — lets the container reach docker-compose services on localhost.

### Git Package API

Single public entry point: `git.NewService(path, logger, vcsCmd...) (*Service, error)`
- All git operations are methods on `Service` (CreateBranchForPlan, CreateWorktreeForPlan, MovePlanToCompleted, EnsureLocalGitignore, etc.)
- `Logger` interface for dependency injection, compatible with `*color.Color`
- Uses `backend` interface internally, implemented by `externalBackend` which shells out to the configured VCS command
- Optional `vcsCmd` parameter overrides the default `"git"` command (e.g., path to `hg2git.sh` translation script)

Key files:
- `pkg/git/service.go` - `Service` type, `backend` interface
- `pkg/git/external.go` - VCS CLI backend (`externalBackend` type)

### Worktree Isolation Mode

`--worktree` flag or `use_worktree = true` config option runs each plan in an isolated git worktree, enabling parallel execution of multiple plans on the same repo. `--branch` flag overrides the branch name derived from the plan filename (useful when auto-detection is fragile, e.g. generic filenames or spec-driven layouts).

- Worktrees created at `.ralphex/worktrees/<branch-name>` inside main repo
- Progress logger created before chdir so files land in main repo's `.ralphex/progress/`
- `MainGitSvc` in `executePlanRequest` handles cross-boundary ops (plan file moves in main repo)
- Worktree auto-removed on completion, failure, or SIGINT; branch preserved for PR
- Only active for `ModeFull` and `ModeTasksOnly` (review/plan/external modes skip worktree)
- `runWithWorktree()` in `cmd/ralphex/main.go` encapsulates the full lifecycle
- Base branch resolution: `resolveBaseRefs()` (`cmd/ralphex/main.go`) produces two values from the same sources — `defaultBranch` for branch/worktree creation and `baseRef` for review diffs and `{{DEFAULT_BRANCH}}`. `--base-ref` feeds both when it names a local branch (resolved by `localBranchRef()`, which accepts the remote-tracking form `origin/<name>` because auto-detection produces exactly that), which is what lets a plan run off a release branch (`--worktree --base-ref release/13.0.0`) while diff, rebase, and worktree keep a single shared base. A non-branch revision (commit hash) stays diff-only, and in worktree mode `resolveBranchBase()` rejects it outright rather than silently falling back to the auto-detected default, which would surface later as a confusing "requires main branch" error. A branch base is honored only from that branch itself: from the default branch `resolveBranchBase()` errors, because branch creation cuts from HEAD and `CreateBranchForPlan` would read the mismatch as "already on a feature branch" and skip, leaving the whole run committing onto the default branch. The `branchMode` parameter gates all of this — review modes create no branch, so their `--base-ref` stays a pure diff base; plan mode counts as a branch mode because it hands off to `runWithWorktree`
- Case-insensitive path handling: `CreateBranchForPlan()`, `CreateWorktreeForPlan()`, and `CommitPlanFile()` resolve plan file paths to actual on-disk case via `resolveFilesystemCase()` to handle macOS APFS case-insensitive filesystems. `hasChangesOtherThan()` uses case-insensitive comparison for plan file exclusion

Key files:
- `cmd/ralphex/main.go` - `runWithWorktree()`, `selectAndExecutePlan()`, interrupt cleanup
- `pkg/git/service.go` - `CreateWorktreeForPlan()`, `CommitPlanFile()`, `RemoveWorktree()`, `resolveFilesystemCase()`
- `pkg/git/external.go` - `addWorktree()`, `removeWorktree()`, `pruneWorktrees()` (unexported backend methods)

### Plan Creation Mode

The `--plan "description"` flag enables interactive plan creation:

- Claude explores codebase and asks clarifying questions
- Questions use QUESTION signal with JSON: `{"question": "...", "options": [...]}`
- User answers via fzf picker (or numbered fallback); an "Other" option allows typing a custom answer
- Q&A history stored in progress file for context
- When ready, Claude emits PLAN_DRAFT signal with full plan content for user review
- User can Accept, Revise (with feedback), Interactive review, or Reject the draft
- Interactive review opens `$EDITOR` with the plan content; on save, a unified diff is computed and fed back as revision feedback
- If revised (manually or via interactive review), feedback is passed to Claude for plan modifications
- Loop continues until user accepts and Claude emits PLAN_READY signal
- Plan file written to docs/plans/
- After completion, prompts user: "Continue with plan implementation?"
- If "Yes", creates branch and runs full execution mode on the new plan

Plan creation signals:
- `QUESTION` - asks user a question with options (JSON payload)
- `PLAN_DRAFT` - presents plan draft for review (plan content between markers)
- `PLAN_READY` - indicates plan file was written successfully

Key files:
- `pkg/input/input.go` - terminal input collector (fzf/fallback, draft review)
- `pkg/status/status.go` - shared signal constants (COMPLETED, FAILED, REVIEW_DONE, etc.)
- `pkg/processor/phase/signals.go` - runtime phase signal parsers for QUESTION and PLAN_DRAFT, plus signal helpers
- `pkg/processor/signals.go` - processor compatibility wrappers around phase signal helpers
- `pkg/config/defaults/prompts/make_plan.txt` - plan creation prompt

### cmux Sidebar Integration

`pkg/cmux` reports run state to the sidebar of the [cmux](https://github.com/manaflow-ai/cmux) terminal: a spinner for the duration of the run, a status pill with the current phase, a progress bar over plan tasks, and notifications when the run waits for input or finishes. Entirely on the ralphex side — it shells out to the public `cmux` CLI, so no cmux-side changes and no dependency on its private socket protocol.

- **Auto-detected, no config flag.** `cmux.New(planFile)` returns `nil` when `CMUX_WORKSPACE_ID` is empty/blank or `cmux` is not in `PATH`. Every exported method is nil-safe with an early `if r == nil { return }`, so callers never nil-check and outside cmux the integration costs nothing. `WrapInput` on a nil reporter returns the original collector unchanged
- **Best-effort by design.** Indication, not functionality: each call gets a 2s timeout, stdout/stderr are discarded, and errors are swallowed without logging — logging them would pollute the progress file. Nothing here can fail a run
- **`pkg/processor` and `pkg/processor/phase` are untouched.** Events come from existing points: `status.PhaseHolder` for phases, the plan file for progress, `InputCollector` for questions
- **Status key is `ralphex`, not `claude_code`.** cmux shows pills with a non-allowlisted key unconditionally, while allowlisted agent keys are hidden unless cmux sees a live PID for that agent. `workspace loading on --id ralphex` is used for the spinner because the workspace lane is computed from live signals, not pills — it is the only route to a real `running` signal that needs neither the agent allowlist nor vault registration. The `needs-attention` lane requires the private socket, so `cmux notify` (blue ring, tab highlight, `Cmd+Shift+U`) substitutes for it
- **Progress comes from polling the plan file** every 10s in a goroutine started by `Start(ctx)`, counting `plan.TaskStatusDone` against total tasks — so the bar keeps moving during a long task phase without hooking into the phase engines. `Start` reports once up front before the ticker, so the bar is present from the beginning and short runs are covered too; with an empty plan file (plan creation mode) it starts no goroutine at all. A missing/unparsable plan file (it may have moved to `completed/`), zero parsed tasks, or a `done/total` pair unchanged since the last tick skip the tick silently; the goroutine keeps running. `Stop()` is `sync.Once`-guarded, waits for the goroutine, then clears the sidebar
- **Only `New`, `Start`, `Stop`, `OnPhase`, `Notify`, and `WrapInput` are exported.** The individual sidebar commands (`loadingOn`/`loadingOff`/`setStatus`/`clearStatus`/`setProgress`/`clearProgress`/`clearAll`) are internal: driving them from outside would bypass the `stopOnce`/clear ordering `Stop` relies on
- **Cleanup MUST hang off the interrupt handler.** cmux has no TTL on the `workspace loading` spinner: a run that exits without clearing it leaves the spinner in the sidebar until cmux itself restarts. `startInterruptWatcher` force-exits via `os.Exit(1)`, where defers never run — so `rep.Stop` is registered in the `cmuxStop` `cleanupHolder` and invoked from the interrupt cleanup callback built by `forceExitCleanup`. That callback runs the holders concurrently and **waits for all of them**: `runCleanupBounded`'s 2s timeout is what bounds them, so a stuck worktree removal cannot be what leaves the spinner hanging, while a fire-and-forget goroutine would simply lose the race against `os.Exit` (an unset `wtCleanup` returns instantly, so the callback would return before the first `cmux` call landed)
- **`--serve` clears the sidebar before the dashboard idles.** After a successful run `keepDashboardAlive` blocks until Ctrl+C, so `executePlan` calls `rep.Stop()` before it — otherwise the spinner would report a finished run as still working for as long as the dashboard is up
- `cleanupHolder` (formerly `worktreeCleanupFn`) is the shared mutex-guarded holder type for both worktree removal and the sidebar reset; `set` before `call` is not guaranteed, so `call` on an unset holder is a no-op

Wiring (both execution funnels in `cmd/ralphex/main.go`):
- `executePlan` — reporter created after `setupProgressLogger`, `Start(ctx)`, `defer rep.Stop()` next to `defer plr.closeLog()`, `plr.holder.OnChange(rep.OnPhase)` registered after the dashboard so both subscriptions coexist
- `runPlanMode` — same, plus `collector = rep.WrapInput(collector)` before `r.SetInputCollector`, and an explicit `rep.Stop()` before handing off to `runWithWorktree`/`executePlan`: otherwise the deferred plan-mode `Stop` would tear down the sidebar of the already-started implementation run
- `notifyCmuxCompletion` / `cmuxCompletionNotice` own the end-of-run notification for success and failure; `ErrUserAborted` (including wrapped) sends nothing — the decision lives in `cmuxCompletionNotice`, not at the call sites. Plan-mode success is deliberately NOT a completion notice: it sends `plan created` instead, because the implementation run may still follow and the continue prompt is what the user is actually needed for

Key files:
- `pkg/cmux/cmux.go` - `Reporter`, `commandRunner` (fake in tests, real binary never spawned), phase→pill mapping, plan polling, `WrapInput`
- `pkg/status/phase_holder.go` - `OnChange` appends observers (see Key Patterns)
- `cmd/ralphex/main.go` - `cleanupHolder`, `cmuxStop` wiring, `notifyCmuxCompletion`

## Platform Support

- **Linux/macOS:** fully supported
- **Windows:** builds and runs, but with limitations:
  - Process group signals not available (graceful shutdown kills direct process only, not child processes)
  - File locking not available (active session detection disabled)
  - Prompts are passed to the claude CLI via stdin (not `-p` flag) to avoid the cmd.exe 8191-character command-line limit

### Cross-Platform Development

When adding platform-specific code (syscalls, signals, file locking):
1. Use build tags: `//go:build !windows` for Unix-only code, `//go:build windows` for Windows stubs
2. Create separate files: `foo_unix.go` and `foo_windows.go`
3. Keep common code in the main file, extract platform-specific functions
4. Windows stubs can be no-ops where functionality is optional

Example files:
- `pkg/executor/procgroup_unix.go` / `procgroup_windows.go` - process group management
- `pkg/progress/flock_unix.go` / `flock_windows.go` - file locking helpers

Cross-compile to verify Windows builds:
```bash
GOOS=windows GOARCH=amd64 go build ./...
```

## Configuration

- Global config location: `~/.config/ralphex/` (override with `--config-dir` or `RALPHEX_CONFIG_DIR`)
- Local config location: `.ralphex/` (per-project, optional)
- Config file format: INI (using gopkg.in/ini.v1)
- Embedded defaults in `pkg/config/defaults/`
- Precedence: CLI flags > local config > global config > embedded defaults
- Custom prompts: `~/.config/ralphex/prompts/*.txt` or `.ralphex/prompts/*.txt`
- Custom agents: `~/.config/ralphex/agents/*.txt` or `.ralphex/agents/*.txt`
- `plan_model` / `task_model` / `review_model` config options: `model[:effort]` for plan creation / task / review phases; `plan_model` and `review_model` fall back to `task_model`. CLI flags `--plan-model`/`--task-model`/`--review-model` take precedence. Parsed by executor setup (pkg/processor/executor_factory.go). See the Key Patterns bullet for claude- vs codex-executor behavior. Disabled by default (empty = Claude CLI defaults)
- `external_review_tool` defaults to `auto`; `external_review_model` independently selects the external provider model/effort. CLI flags `--external-review-tool`/`--external-review-model` take precedence. Empty external model means `opus:xhigh` for Claude or `codex_model`/`codex_reasoning_effort` for Codex
- `default_branch` config option: override auto-detected default branch for review diffs and for branch/worktree creation
- `max_iterations` config option: override CLI default (50) for maximum task iterations per plan (CLI flag `--max-iterations` takes precedence)
- `vcs_command` config option: override the VCS binary used by the git backend (default: `"git"`). Set to a translation script path (e.g., `scripts/hg2git/hg2git.sh`) to use ralphex with Mercurial repos. See `docs/hg-support.md`
- `commit_trailer` config option: trailer line appended to all ralphex-orchestrated git commits (both Go-code commits and LLM-prompted commits). When set, the trailer is appended after a blank line at the end of every commit message. Example: `commit_trailer = Co-authored-by: ralphex <noreply@ralphex.com>`. Disabled by default (empty)
- Notification config: `notify_channels`, `notify_on_error`, `notify_on_complete`, `notify_timeout_ms`, plus channel-specific `notify_*` fields (see `docs/notifications.md`)
- `review_patience` config option: terminate external review after N consecutive unchanged rounds (0 = disabled). CLI flag `--review-patience` takes precedence
- `wait_on_limit` config option: duration to wait before retrying on rate limit (e.g., "1h", "30m"). CLI flag `--wait` takes precedence. Disabled by default
- `session_timeout` config option: per-session timeout (e.g., "30m"). Applies to claude in default mode and to every executor call under `--codex` (task/review/finalize/eval); external codex/custom review in Claude mode is not affected. Kills hanging sessions, continues to next iteration. Applied in `retryPolicy.runWithSessionTimeout` via `context.WithTimeout`, gated on `Executor==ExecutorCodex || toolName=="claude"`. CLI flag `--session-timeout` takes precedence. Disabled by default
- `idle_timeout` config option: kills claude/codex executor sessions when no output for a given duration (e.g., "5m"). Resets on each output line; only fires when the session goes silent. Implemented in `ClaudeExecutor.Run()`/`CodexExecutor.Run()` via `time.AfterFunc`. Wired by `buildCodexExecutor` for first-class `--codex`; NOT by `buildExternalCodexExecutor`, so external codex review in default-claude mode has no idle timeout. Custom external review unaffected. CLI flag `--idle-timeout` takes precedence. Disabled by default
- `move_plan_on_completion` config option: controls whether completed plans move to `docs/plans/completed/` on success. Default `true`. Disable for workflows that manage plan lifecycle externally (spec-driven tooling with separate archive steps)
- `preserve_anthropic_api_key` config option / `--preserve-anthropic-api-key` CLI flag: when true, `ANTHROPIC_API_KEY` is passed through to the child claude process (needed for API-key auth rather than OAuth/keychain). Default `false` strips the key. The merge sentinel `PreserveAnthropicAPIKeySet` lives only on `Values` (load-bearing for local-overrides-global merge); `Config` carries the resolved bool. Plumbed: `Config.PreserveAnthropicAPIKey` → `pkg/processor/executor_factory.go` → `ClaudeExecutor.PreserveAPIKey` → `execClaudeRunner.preserveAPIKey` → `claudeChildEnv()` (`pkg/executor/executor.go`). When enabled, the startup banner emits `auth: ANTHROPIC_API_KEY passthrough enabled`. `CLAUDECODE` is always stripped regardless (prevents nested-session errors)

### Local Project Config (.ralphex/)

Projects can have local configuration that overrides global settings. Run `ralphex --init` to create the `.ralphex/` directory with commented-out defaults:

```
project/
├── .ralphex/           # optional, project-local config (created by --init)
│   ├── config          # overrides specific settings (per-field merge)
│   ├── prompts/        # per-file fallback: local → global → embedded
│   │   └── task.txt    # only override task prompt
│   └── agents/         # per-file fallback: local → global → embedded
│       └── custom.txt  # project-specific agent
```

**Merge strategy:**
- **Config file**: per-field override (local values override global, missing fields fall back)
- **Prompts**: per-file fallback (local → global → embedded for each prompt file)
- **Agents**: per-file fallback (local → global → embedded for each agent file, same as prompts)

### Config Defaults Behavior

- **Commented templates**: config file, prompts, and agents are installed with all content commented out (prefixed `# `)
- **Auto-update**: files with only comments/whitespace are safe to overwrite on updates - users get new defaults automatically
- **User customization**: uncommenting any line marks the file as customized - it will be preserved and never overwritten
- **Fallback loading**: when loading config/prompts/agents, if file content is all-commented (no actual values), embedded defaults are used
- **Comment handling**: leading meta-comment block (2+ contiguous `# ...` lines at top of file) is stripped when loading prompts and embedded defaults; a single `# Title` at the top is preserved (treated as markdown header, not meta-comment). Full `stripComments` is only used for emptiness detection to trigger fallback
- **scalars/colors**: per-field fallback to embedded defaults if missing
- `*Set` flags (e.g., `CodexEnabledSet`) distinguish explicit `false`/`0` from "not set"

### Error Pattern Detection

Configurable patterns detect rate limit and quota errors in claude/codex output:
- `claude_error_patterns` / `codex_error_patterns`: comma-separated error patterns (default strings in `llms.txt` and the embedded config). Codex phrases are tightened so review findings that *talk about* rate limiting do not trip a false positive
- Matching is case-insensitive substring search
- Whitespace is trimmed from each pattern
- For claude: patterns checked against the last 10 text blocks (not full output) to avoid false positives when analysis text mentions rate limit phrases. Context cancellation paths bypass pattern checks
- For codex: patterns checked against stdout AND a live per-line scan of stderr. Stderr scanning runs inside `processStderr` before the 5-line / 256-rune tail truncation, so detection is eviction- and truncation-resistant. The scan is gated by `isCodexErrorLine` (matches `error:`/`fatal:`/`panic:` prefix, case-insensitive) so progress chatter cannot trigger false positives. The first matching limit/error pattern per category is recorded on `stderrResult.{limitMatch,errorMatch}` and consumed by `CodexExecutor.checkPatterns`. Priority is limit-class first across both sources: `stdout limit → stderr limit → stdout error → stderr error` (within a class, stdout wins). Patterns are evaluated only when the process exits non-zero and context is not canceled. Stderr is scanned because OpenAI/ChatGPT plan-quota errors are emitted on stderr while stdout is empty on failure
- For custom executors: stderr is merged into stdout by the executor itself (`cmd.Stderr = cmd.Stdout`), so the same pattern check covers both streams. Patterns checked only when process exits non-zero and context is not canceled
- On match, ralphex exits gracefully with pattern info and help command suggestion

Transient retry patterns for wrapper-level stalls:
- `claude_retry_patterns`: comma-separated transient Claude/fya markers retried like executor timeouts. Default: `FYA_TRANSIENT_TIMEOUT,API Error: 529,API Error: 502,API Error: 503,API Error: 504`. The transient HTTP errors (529 Overloaded, 502/503/504 gateway) live here, not in `claude_limit_patterns`: they are short-lived server hiccups, so they auto-retry without requiring `--wait` (500 is intentionally excluded — it can be a deterministic server failure, caught by the broad `API Error:` error pattern). Precedence is retry → limit → error, so a no-signal 529 matches the retry tier first; a 529 with a signal present falls through to the broad `API Error:` error pattern
- Retry patterns are checked before limit and error patterns. They do not use `wait_on_limit`; the phase receives timeout-style metadata and applies its existing bounded retry behavior. The task loop (`pkg/processor/phase/task.go`) and review-iteration loop (`pkg/processor/phase/review.go`) wait a short fixed `retryBackoff` (5s, defined in `pkg/processor/phase/phase.go`) before re-running the timed-out/transiently-failed iteration; the first-review soft-skip path is not a retry and has no backoff
- Retry detection is suppressed when `result.Signal` is non-empty: a completed run that emitted a structured signal (e.g. `ALL_TASKS_DONE`) must not be discarded and re-run just because the output text mentions a retry marker. `patternError(recentText, signal)` (`pkg/executor/executor.go`) gates only the retry tier on the signal; limit and error patterns still fire regardless (they surface loudly rather than silently re-running)

Limit patterns for wait+retry behavior:
- `claude_limit_patterns` / `codex_limit_patterns`: comma-separated limit patterns (default strings in `llms.txt` and the embedded config)
- `wait_on_limit`: duration string (e.g., "1h", "30m"), disabled by default
- `--wait` CLI flag overrides `wait_on_limit` config
- Priority: retry patterns checked first, then limit patterns; if a limit pattern matches AND wait > 0, wait and retry; if match AND wait == 0, fall through to error pattern behavior
- Limit patterns intentionally overlap with error patterns — `wait_on_limit` acts as the toggle

Implementation:
- `PatternMatchError` type in `pkg/executor/executor.go` with `Pattern` and `HelpCmd` fields
- `LimitPatternError` type in `pkg/executor/executor.go` with `Pattern` and `HelpCmd` fields
- `RetryPatternError` type in `pkg/executor/executor.go` with a `Pattern` field
- `matchPattern()` helper for case-insensitive matching (used by error, limit, and retry pattern checks)
- Patterns passed via `ClaudeExecutor.ErrorPatterns`/`LimitPatterns`/`RetryPatterns` and `CodexExecutor.ErrorPatterns`/`LimitPatterns` (codex has no retry patterns)
- `retryPolicy.Run()` in `pkg/processor/execution_policy.go` wraps executor calls with retry logic

### Agent System

5 default agents are installed on first run to `~/.config/ralphex/agents/` as commented-out templates:
- `implementation.txt` - verifies code achieves stated goals
- `quality.txt` - reviews for bugs, security issues, race conditions
- `documentation.txt` - checks if docs need updates
- `simplification.txt` - detects over-engineering
- `testing.txt` - reviews test coverage and quality

**Loading behavior:** agents are loaded with per-file fallback: local `.ralphex/agents/` → global `~/.config/ralphex/agents/` → embedded default. The 5 embedded agents are always the baseline — deleting an agent file from disk does not disable it, the embedded version is used as fallback. To disable a specific agent, remove its `{{agent:name}}` reference from the prompt files (`review_first.txt`, `review_second.txt`), not the agent file itself.

**Frontmatter options:** Agent files support optional YAML frontmatter (`---` delimited) for per-agent model and subagent type:
- `model: haiku|sonnet|opus|fable` — Claude model for this agent
- `agent: <type>` — Claude Code Task tool subagent type (default: `general-purpose`)
- Parsed by `parseOptions()` in `pkg/config/frontmatter.go`, validated by `Options.Validate()`
- Full model IDs (e.g. `claude-sonnet-4-5-20250929`) are normalized to short keywords (`sonnet`)
- Invalid model values are dropped with a warning, falling back to defaults

**Template variables:** Prompt files support variable expansion via `replacePromptVariables()` in `pkg/processor/prompts.go`:
- `{{PLAN_FILE}}` - path to plan file or fallback text
- `{{PROGRESS_FILE}}` - path to progress log or fallback text
- `{{GOAL}}` - human-readable goal (plan-based or branch comparison)
- `{{DEFAULT_BRANCH}}` - detected default branch (main, master, origin/main, etc.), overridable via `--base-ref` CLI flag or `default_branch` config option
- `{{DIFF_INSTRUCTION}}` - git diff command for current iteration (first: `git diff main...HEAD`, subsequent: `git diff`)
- `{{PREVIOUS_REVIEW_CONTEXT}}` - previous evaluator context block for external review iterations (empty on first iteration, formatted context on subsequent)
- `{{CODEX_OUTPUT}}` / `{{CLAUDE_OUTPUT}}` / `{{CUSTOM_OUTPUT}}` - findings from the selected external reviewer for its matching evaluation prompt
- `{{agent:name}}` - expands to Task tool instructions for the named agent

Variables are also expanded inside agent content, so custom agents can use `{{DEFAULT_BRANCH}}` etc.

**Customization:**
- Edit files in `~/.config/ralphex/agents/` to modify agent prompts
- Add new `.txt` files to create custom agents
- Run `ralphex --init` to create local `.ralphex/` project config with commented-out defaults
- Run `ralphex --reset` to interactively restore defaults, or delete ALL `.txt` files manually
- Run `ralphex --dump-defaults <dir>` to extract raw embedded defaults for comparison or merging
- Use `/ralphex-update` skill for smart merging of updated defaults into customized configs
- Use `/ralphex-adopt` skill to convert plans from other formats (OpenSpec, spec-kit, GitHub/GitLab issues, task-lists, free-form markdown) into ralphex format
- Alternatively, reference agents installed in your Claude Code directly in prompt files (like `qa-expert`, `go-smells-expert`)

## Testing

```bash
go test ./...           # run all tests
go test -cover ./...    # with coverage
```

### Web UI E2E Tests

Playwright-based e2e tests for the web dashboard are in `e2e/` directory:

```bash
# install playwright browsers (first time only)
go run github.com/playwright-community/playwright-go/cmd/playwright@latest install --with-deps chromium

# run web ui e2e tests
go test -tags=e2e -timeout=10m -count=1 -v ./e2e/...

# run with visible browser (for debugging)
E2E_HEADLESS=false go test -tags=e2e -timeout=10m -count=1 -v ./e2e/...
```

Tests cover: dashboard loading, SSE connection and reconnection, phase sections, plan panel, session sidebar, keyboard shortcuts, error/warning event rendering, signal events (COMPLETED/FAILED/REVIEW_DONE), task and iteration boundary rendering, auto-scroll behavior, plan parsing edge cases.

## End-to-End Testing

Unit tests mock external calls. After ANY code changes, ask the user before running an e2e test with a toy project because it can take time and consume claude/codex credits. Run it only after explicit approval to verify actual claude/codex integration and output streaming.

### Create Toy Project

```bash
./scripts/internal/prep-toy-test.sh
```

This creates `/tmp/ralphex-test` with a buggy Go file and a plan to fix it.

### Test Full Mode

```bash
cd /tmp/ralphex-test
.bin/ralphex docs/plans/fix-issues.md
```

**Expected behavior:**
1. Creates branch `fix-issues`
2. Phase 1: executes Task 1, then Task 2
3. Phase 2: first Claude review
4. Phase 2.5: executor-aware external review (Codex by default for a Claude primary)
5. Phase 3: second Claude review
6. Moves plan to `docs/plans/completed/`

### Test Review-Only Mode

```bash
cd /tmp/ralphex-test
git checkout -b feature-test

# make some changes
echo "// comment" >> main.go
git add -A && git commit -m "add comment"

# run review-only (no plan needed)
go run <ralphex-project-root>/cmd/ralphex --review
```

### Test Codex-Primary External-Only Mode

```bash
cd /tmp/ralphex-test

# run Claude external review with Codex evaluation/fixes/finalize
go run <ralphex-project-root>/cmd/ralphex --codex --external-only
```

`--codex-only` remains a deprecated alias for `--external-only`; it does not select the Codex primary executor by itself.

### Monitor Progress

```bash
# live stream (use actual filename from ralphex output)
tail -f .ralphex/progress/progress-fix-issues.txt

# recent activity
tail -50 .ralphex/progress/progress-*.txt
```

## Development Workflow

**CRITICAL: After ANY code changes to ralphex:**

1. Run unit tests: `make test`
2. Run linter: `make lint`
3. **MUST** ask the user before running the toy end-to-end test (see above); run it only after explicit approval
4. Monitor `tail -f .ralphex/progress/progress-*.txt` to verify output streaming works

Unit tests don't verify actual codex/claude integration or output formatting. The toy project test is the only way to verify streaming output works correctly.

## Before Submitting a PR

If you're an AI agent preparing a contribution, complete this checklist:

**Code Quality:**
- [ ] Run `make test` - all tests must pass
- [ ] Run `make lint` - fix all linter issues
- [ ] Run `make fmt` - code is properly formatted
- [ ] New code has tests with 80%+ coverage

**Project Patterns:**
- [ ] Studied existing code to understand project conventions
- [ ] One `_test.go` file per source file (not `foo_something_test.go`)
- [ ] Tests use table-driven pattern with testify
- [ ] Test helper functions call `t.Helper()`
- [ ] Mocks generated with moq, stored in `mocks/` subdirectory
- [ ] Interfaces defined at consumer side, not provider
- [ ] Context as first parameter for blocking/cancellable methods
- [ ] Private struct fields for internal state, accessor methods if needed
- [ ] Regex patterns compiled once at package level
- [ ] Deferred cleanup for resources (files, contexts, connections)
- [ ] No new dependencies unless directly needed - avoid accidental additions

**PR Scope:**
- [ ] Changes are focused on the requested feature/fix only
- [ ] No "general improvements" to unrelated code
- [ ] PR is reasonably sized for human review
- [ ] Large changes split into logical, focused PRs

**Self-Review:**
- [ ] Can explain every line of code if asked
- [ ] Checked for security issues (injection, secrets exposure, etc.)
- [ ] Commit messages describe "why", not just "what"

## Documentation Site (Zensical)

- Site source: `site/` directory with `mkdocs.yml` (read natively by Zensical)
- Builder: `zensical` (replaced mkdocs-material; `requirements.txt` lists only `zensical`)
- **Landing page**: `site/docs/index.html` is a manually crafted HTML page, not generated by the SSG. Edit it directly to update the landing page.
- Template overrides: `site/overrides/` with `custom_dir: overrides` in mkdocs.yml
- **Python version**: Zensical requires Python ≥ 3.10. Local builds use a venv at `site/.venv/` (auto-created by `make prep_site`); Cloudflare Pages requires `PYTHON_VERSION` env var ≥ 3.10
- **Brand color**: dark-mode palette uses Material's `teal` keyword, then `site/docs/stylesheets/extra.css` overrides `--md-primary-fg-color` / `--md-accent-fg-color` to `#2dd4bf` (Tailwind teal-400) so the docs match the landing page brand color
- **Raw .md files**: SSG renders ALL `.md` files in `docs_dir` as HTML pages. To serve raw markdown (e.g., `assets/claude/*.md` for Claude Code skills), copy them AFTER `zensical build` - see `prep_site` target in Makefile

## Testing Safety Rules

- **CRITICAL: Tests must NEVER touch real user config directory** (`~/.config/ralphex/`)
- All tests MUST use `t.TempDir()` for any file operations
- Config pollution is hard to debug - corrupted files cause cryptic errors
- Verify tests are clean: compare MD5 checksums of config files before/after `go test ./...`

## Workflow Rules

- **Plugin version**: bump `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` versions on release if skill files (`assets/claude/`) changed since last plugin version bump
- **CHANGELOG**: Never modify during development - updates are part of release process only
- **Version sections**: Never add entries to existing version sections - versions are immutable once released
- **Linter warnings**: Add exclusions to `.golangci.yml` instead of `_, _ =` prefixes for fmt.Fprintf/Fprintln
- **Exporting functions**: When changing visibility (lowercase to uppercase), check ALL callers including test files
- **Completed plans are immutable**: Plans in `docs/plans/completed/` represent historical record of changes. Never modify completed plans. If further changes are needed (refactoring, fixes, etc.), create a new plan
