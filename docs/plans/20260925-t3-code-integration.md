# T3 Code integration

## Overview
- Make loopai runs visible in T3 Code (the "agent harness control surface" at `C:\Projects\AI\t3code`,
  a Node WebSocket server with web, desktop, and mobile clients) without forking T3 Code.
- Three pieces:
  1. `--t3` status reporter (`pkg/t3`): each run gets a T3 thread whose title tracks the loopai
     phase the same way the Orca OSC title does (task N/M, review iteration, waiting, done/failed);
     `--pr` links the created PR to the thread so T3 settles the thread when the PR merges.
  2. `--t3-launch` standalone mode plus a `loopai-t3` skill, the T3 counterpart of `loopai-orca`:
     create a T3-managed worktree cut from the current branch, carry the plan and untracked
     `.loopai/` overrides into it, create a thread bound to that worktree, open a terminal inside
     the thread, and start `loopai --t3 [flags] <plan>` there. The worktree persists after the run
     for review and `--merge`, exactly like the Orca flow.
  3. Documentation of `t3.json` project actions (review, dashboard with `previewUrl`) that run
     loopai inside a thread terminal.
- Everything goes through T3 Code's public, authenticated HTTP and WebSocket APIs. Failures never
  affect the run.

## Decisions
- **Context**: T3 Code has no plugin, hook, or custom-provider mechanism; adding a provider, custom
  thread activity rows, extra MCP tools, or OSC title parsing requires a fork, and upstream does not
  accept large contributions. The public API (`POST /api/orchestration/dispatch`, WebSocket RPC at
  `/ws`) is enough for thread creation, title updates, PR links, worktree creation, and terminals.
- **Chosen approach**: primary goal is run visibility. Status goes into the thread title via
  `thread.meta.update`. The skill follows the Orca model: T3 owns a persistent worktree and loopai
  runs in it without `--worktree`. Launch happens in the thread's own terminal over WebSocket RPC, so
  output is visible in T3 on every client, including mobile.
- **Rejected alternatives**:
  - Fork T3 Code to add an ACP/custom provider or activity rows: maintenance burden of a fork.
  - Clearing `worktreePath` on a plain `--t3 --worktree` run before loopai removes its worktree:
    not chosen; plain `--worktree` runs simply register the branch with `worktreePath: null`.
  - Launching loopai as a detached background process, or leaving launch to a manual `t3.json`
    button: rejected in favor of the thread terminal, which gives visible output.
  - Extracting the Orca state machine into a shared package: rejected; `pkg/t3` duplicates the
    small state/label logic and `pkg/orca` stays untouched.
  - loopai invoking `t3 auth session issue` itself: rejected; loopai reads the token only from
    `LOOPAI_T3_TOKEN`. The skill mints a short-lived token and passes it.
- **Verified facts** (T3 Code HEAD `d5d48742c9`):
  - `<T3 home>/userdata/server-runtime.json` (`apps/server/src/serverRuntimeState.ts`): one-line JSON
    `{version:1, pid, host?, port, origin, devUrl?, startedAt, serviceManaged?}`; T3 home is
    `T3CODE_HOME` (trim, `~` expansion, resolve) or `~/.t3`. Deleted on clean shutdown, may be stale
    after a crash. Current machine: `{"origin":"http://127.0.0.1:3773",...}`.
  - Auth: every route needs `Authorization: Bearer <token>`; no loopback exception. 401 body carries
    `code:"auth_invalid"`, 403 `code:"insufficient_scope"`. `t3 auth session issue --token-only
    [--ttl 12h]` prints a full-scope token and works with the server stopped. The `t3` CLI is not on
    `PATH` on this machine (desktop install only); `npx t3@latest` provides it.
  - `POST /api/orchestration/dispatch` body is one `ClientOrchestrationCommand` tagged by `type`;
    success `{"sequence":N}`; domain rule violations return **500** `orchestration_dispatch_failed`.
    All ids (`commandId`, `threadId`, `projectId`) are client-generated non-empty strings; each
    command needs a fresh `commandId` (idempotency key). `createdAt` is required where present and
    overwritten by the server.
  - `thread.create {commandId, threadId, projectId, title, modelSelection:{instanceId, model},
    runtimeMode, interactionMode?, branch: string|null, worktreePath: string|null, createdAt}`
    creates no provider session; built-in instance ids are `codex` and `claudeAgent`.
  - `thread.meta.update {commandId, threadId, title?, branch?, worktreePath?}`: each call is a
    persisted event; title-only updates are cheap, but `branch`/`worktreePath` changes re-trigger a
    source-control PR lookup. No title length or rate limit. Update only on state change.
  - `thread.pull-request.link {commandId, threadId, host, repository, number, url, source}`; no URL
    validation over HTTP, duplicate link is a 500; use `source:"manual"`, host `github.com`,
    repository `owner/repo`. A turn-less thread auto-settles when its linked PR merges.
  - `GET /api/orchestration/shell` returns `{projects[{id, workspaceRoot, ...}], threads[...]}`;
    `workspaceRoot` is a resolved absolute path; T3 compares paths after trimming trailing slashes
    and, for drive/UNC paths, converting `/` to `\` and lowercasing (`packages/shared/src/path.ts`).
  - `/ws` accepts the Bearer header on upgrade (or `?wsTicket=` from
    `POST /api/auth/websocket-ticket`). Framing is Effect RPC JSON, one message per frame:
    `{"_tag":"Request","id":"1","tag":"terminal.open","payload":{...},"headers":[]}` →
    `{"_tag":"Exit","requestId":"1","exit":{"_tag":"Success"|"Failure",...}}`; `Ping`/`Pong`
    keepalive. The exact `id`/`headers` shape is inferred from `packages/client-runtime` tests and
    must be confirmed against a live server before being relied on.
  - `terminal.open {threadId, terminalId (≤128, "term-N"), cwd, worktreePath?, cols?, rows?, env?
    (≤128 keys), providerInstanceId?}` and `terminal.write {threadId, terminalId, data ≤65536}`
    (`packages/contracts/src/terminal.ts`); the terminal strips only `T3CODE_*` variables from env.
  - `vcs.createWorktree {cwd, refName, newRefName, baseRefName, path}` is a WS RPC
    (`packages/contracts/src/rpc.ts`); T3-managed worktrees default to `~/.t3/worktrees`.
  - `github.com/gorilla/websocket v1.5.3` is already vendored as an indirect dependency; promoting
    it to direct adds no new module.
  - `pkg/orca` is the pattern to mirror: nil-safe `Reporter`, `OnPhase`/`OnSection`/`WrapLogger`/
    `WrapInput`/`WithInputWait`/`Finish`/`Quiesce`/`Stop`; wired through `orcaReporter`,
    `startOrcaReporter`, `initialOrcaPhase`, `setOrcaCleanup`, `finishOrcaFailure`,
    `buildRunnerLogger` in `cmd/loopai/main.go`. The PR URL is known only inside `runPRCommand`
    (`cmd/loopai/main.go` ~5139).

## Context (from discovery)
- Files/components involved:
  - new `pkg/t3/` (runtime discovery, HTTP client, WS RPC client, reporter, launcher)
  - `cmd/loopai/main.go` (`opts`, `applyCLIOverrides`, `buildRunnerLogger`, reporter lifecycle next to
    orca, `runPRCommand`, `isStandaloneCommand`, `cmuxEnvOptions`, flag validation)
  - `pkg/config/config.go`, `pkg/config/values.go`, `pkg/config/defaults/config` (new `t3` key)
  - `assets/claude/skills/loopai-t3/SKILL.md`, `assets/claude/loopai-t3.md` symlink,
    `assets/codex/skills/loopai-t3/{SKILL.md,agents/openai.yaml}`
  - `assets/claude/skills/loopai-plan/SKILL.md` and the Codex `loopai-plan` Step 3
  - `scripts/check-symlinks.sh`, `scripts/check-symlinks_test.sh`, `.claude-plugin/plugin.json`,
    `.claude-plugin/marketplace.json` (0.5.7 → 0.5.8)
  - `README.md`, `llms.txt`, `CLAUDE.md`, new `docs/t3-code.md`
- Related patterns found: `pkg/orca` reporter and tests; `pkg/claudeswap/coordinator.go` JSON state
  reads; `pkg/executor/codex.go` home-override pattern (`CODEX_HOME` else `~/.codex`);
  `TestCmuxEnvOptionsCoversOptionTags` requires every `env:` tag in `cmuxEnvOptions`;
  `--gen-agents` as a standalone mode routed before branch/worktree setup and notifications.
- Dependencies identified: standard `net/http`, `encoding/json`, vendored `gorilla/websocket`,
  `crypto/rand` for ids (`go.mod` has no uuid module; do not add one).

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
- Maintain backward compatibility: without `--t3`/`t3 = true` nothing contacts T3 Code
- Best-effort contract, same as Orca and cmux: any T3 error (no runtime file, stale pid, refused
  connection, 401/403/500, timeout) disables the reporter for the rest of the run; one short warning
  line at most, details only under debug logging; every call has a short timeout (2s HTTP) and the
  run never waits on T3
- Never write under the T3 home: loopai only reads `server-runtime.json`; every change goes through
  the API. Tests use `httptest` servers and `t.TempDir()` T3 homes via `T3CODE_HOME`; no test may
  read the real `~/.t3` or contact a live server
- Windows is first-class: path normalization matches T3's comparison rules; the launched terminal
  command must work in the default T3 shell on Windows (PowerShell) and on POSIX shells

## Testing Strategy
- **Unit tests**: required for every task; HTTP and WS behavior tested against `httptest.Server`
  fakes that record requests and assert JSON shapes
- **E2E tests**: the Playwright suite covers the loopai dashboard only; no T3 UI e2e. Live-server
  checks are listed under Post-Completion

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code, tests, skills, docs in this repository
- **Post-Completion** (no checkboxes): checks against a live T3 Code server and the T3 UI

## Implementation Steps

### Task 1: T3 runtime discovery and HTTP client in `pkg/t3`
- [ ] add `pkg/t3/runtime.go`: `homeDir()` (`T3CODE_HOME` with trim and `~` expansion, else
      `~/.t3`), `ReadRuntime(home)` parsing `userdata/server-runtime.json` (`version` must be 1);
      `LOOPAI_T3_URL` overrides the discovered origin; token only from `LOOPAI_T3_TOKEN`
- [ ] add `pkg/t3/client.go`: `Client` with a 2s-timeout `http.Client`, Bearer header, `Dispatch(ctx,
      cmd any) (sequence int64, err error)`, `Shell(ctx)` decoding only the fields loopai needs
      (projects `id`, `workspaceRoot`, `deletedAt`/archived state; threads `id`, `projectId`,
      `branch`, `worktreePath`, `title`, archived state), typed errors for auth (401/403) vs other
      failures; fresh client-generated `commandId` per dispatch
- [ ] add command builders with exact wire shapes: `threadCreate`, `threadMetaUpdateTitle` (title
      only), `threadPullRequestLink` (`source:"manual"`)
- [ ] add `NormalizePath` mirroring `packages/shared/src/path.ts` (trim, strip trailing separators
      except a drive root, drive/UNC → backslashes and lowercase) and `FindProject(shell, root)`
- [ ] write tests for runtime discovery (valid file, missing, bad version, `T3CODE_HOME`, URL
      override, missing token) using `t.TempDir()`
- [ ] write tests for the client against `httptest.Server`: request JSON shapes, Bearer header,
      401/403/500 mapping, timeout, `NormalizePath` table (Windows drive, UNC, trailing slash, POSIX)
- [ ] run `make test` - must pass before task 2

### Task 2: WebSocket RPC client in `pkg/t3`
- [ ] promote `github.com/gorilla/websocket` to a direct dependency (`go.mod`, `vendor/modules.txt`
      via `go mod tidy && go mod vendor`); no other new module
- [ ] add `pkg/t3/rpc.go`: dial `/ws` with the Bearer header (ticket flow only if the header is
      rejected), send `{"_tag":"Request","id","tag","payload","headers":[]}`, wait for the matching
      `Exit`, answer `Ping` with `Pong`, ignore unrelated messages, context-bounded deadlines
- [ ] add typed calls `CreateWorktree`, `OpenTerminal`, `WriteTerminal` using the payloads from
      `packages/contracts/src/{rpc,terminal}.ts`; surface `Failure` exits as errors with the server
      message
- [ ] write tests with an `httptest` WebSocket fake: success exit, failure exit, ping handling,
      unrelated frames, auth header present, context cancellation
- [ ] run `make test` - must pass before task 3

### Task 3: thread status reporter `pkg/t3.Reporter`
- [ ] add `pkg/t3/reporter.go` duplicating the small Orca state model (`phase`, task/total,
      iteration, waiting input/limit, final done/failed/stopped) with its own label function; title
      format `"<plan name> · <state>"` (for example `"t3-code-integration · task 2/5"`,
      `"… · review · iteration 1"`, `"… · waiting for input"`, `"… · done"`, `"… · failed"`)
- [ ] nil-safe methods matching the Orca surface: `OnPhase`, `OnSection`, `WrapLogger`, `WrapInput`,
      `WithInputWait`, `Finish(success)`, `Quiesce`, `Stop`; dispatch `thread.meta.update` only
      when the rendered title changes, asynchronously through a single worker with a bounded queue
      that coalesces to the latest title, so executor output never blocks
- [ ] thread binding at start: use `LOOPAI_T3_THREAD_ID` when set; otherwise find the project whose
      normalized `workspaceRoot` equals the main checkout root and create a thread with `branch`
      set and `worktreePath` = the run directory, or `null` when loopai runs with `--worktree` (that
      directory is removed after success); no matching project → one warning, reporter disabled
- [ ] `modelSelection` for `thread.create`: `{instanceId:"codex"|"claudeAgent", model:<effective
      task model or "default">}`, `runtimeMode:"full-access"`, `interactionMode:"default"`
- [ ] any error disables the reporter for the rest of the run; `Stop` drains the queue with a short
      deadline so the final title lands without delaying exit
- [ ] write tests: title table for every state, change-only dispatch, coalescing under a burst,
      nil receiver, disable-after-error, thread binding (env id, project match, `--worktree` null
      path, no project), final title on `Finish`/`Stop`
- [ ] run `make test` - must pass before task 4

### Task 4: `--t3` flag, `t3` config key, and run wiring
- [ ] `pkg/config`: `T3 bool`/`T3Set` in `Config` and `Values`, parse `t3` with `invalid t3: %w`,
      `mergeFrom`, commented default in `pkg/config/defaults/config` (`# t3 = false`, ignored unless
      a T3 Code server and `LOOPAI_T3_TOKEN` are available); update the JSON key list tests
- [ ] `cmd/loopai`: `T3 bool \`long:"t3" env:"LOOPAI_T3"\``, `applyCLIOverrides` via
      `enabledByCLI`, add `LOOPAI_T3` to `cmuxEnvOptions`
- [ ] construct the reporter next to Orca's in `executePlan` and plan-creation mode (seam
      `var newT3Reporter`), subscribe `OnPhase` to the phase holder, add it to `buildRunnerLogger`
      directly below the orca wrapper, wrap input collectors and the pause handler like Orca, call
      `Finish` where `finishCmuxCompletion` finishes Orca and `Stop` on abort and neutral stops
      (reuse `isNeutralOrcaStop` semantics)
- [ ] standalone commands, watch-only mode, and `--gen-agents` never construct the reporter
- [ ] write tests: config load/merge/default dump, flag and env parsing (invalid boolean),
      `TestCmuxEnvOptionsCoversOptionTags` still passes, logger chain order, reporter disabled when
      config off, finish/stop mapping for success, failure, and neutral stops
- [ ] run `make test` - must pass before task 5

### Task 5: link PRs created by `--pr` to T3 threads
- [ ] in `runPRCommand`, after `gh pr create` returns the URL and when `t3` is enabled, parse
      `github.com/<owner>/<repo>/pull/<n>` and dispatch `thread.pull-request.link` for every
      non-archived thread of the matching project whose `branch` equals the feature branch;
      best-effort, a failure prints one warning and never changes the command's exit status
- [ ] write tests: URL parsing (valid, non-GitHub, malformed), matching thread selection, dispatch
      body, T3 unreachable leaves `--pr` successful, disabled config sends nothing
- [ ] run `make test` - must pass before task 6

### Task 6: `--t3-launch` standalone mode
- [ ] add `--t3-launch` (mode `ModeT3Launch` or an early standalone route, following `--gen-agents`
      routing: before branch/worktree setup, notifications, external-review resolution; included in
      `isStandaloneCommand`); accepts a plan path plus only `--codex`, `--task-model`,
      `--review-model`, `--external-reviewers`; reject `--worktree`, `--serve`, `--watch`,
      `--cmux-workspace`, `--commit`, close-out flags
- [ ] `pkg/t3/launch.go`: require a clean-enough source (plan file may be untracked/dirty, as in the
      Orca skill), resolve the project by the main checkout root, create a T3 worktree via
      `vcs.createWorktree` with base = current branch (detached HEAD: current commit) and branch
      name derived by `git.Service.EffectiveBranchName`, verify its HEAD equals the source HEAD
- [ ] copy the plan and untracked `.loopai/{config,prompts,agents}` into the new worktree
      (never overwrite tracked files), create the thread (`branch`, `worktreePath` = new worktree)
- [ ] open a terminal in that thread (`cwd` = worktree, env `LOOPAI_T3=true`, `LOOPAI_T3_TOKEN`,
      `LOOPAI_T3_THREAD_ID`) and write the `loopai --t3 [flags] <plan>` command with the absolute
      loopai path, quoted for the target shell (PowerShell `&` call form on Windows, POSIX single
      quotes elsewhere)
- [ ] print the thread id, worktree path, and branch; a failure after worktree creation reports
      what was created instead of deleting it
- [ ] write tests: flag validation matrix, shell quoting for both shells (paths with spaces,
      quotes), copy of untracked overrides, request sequence against HTTP and WS fakes, partial
      failure reporting
- [ ] run `make test` - must pass before task 7

### Task 7: `loopai-t3` skill (Claude and Codex) and the `loopai-plan` offer
- [ ] add `assets/claude/skills/loopai-t3/SKILL.md` modeled on `loopai-orca`: preflight
      (`loopai --help` lists `--t3-launch`, `server-runtime.json` present), token from
      `LOOPAI_T3_TOKEN` or minted with `t3 auth session issue --token-only --ttl 24h`
      (`npx --yes t3@latest` when `t3` is not on `PATH`, only after telling the user), plan
      selection, the same argument validation and value regex as `loopai-orca`, run
      `loopai --t3-launch`, report thread/worktree/branch and close-out hints (`loopai --merge`)
- [ ] add the symlink `assets/claude/loopai-t3.md`, extend `expected_skills` in
      `scripts/check-symlinks.sh` and the fixture inventory in `scripts/check-symlinks_test.sh`
- [ ] add `assets/codex/skills/loopai-t3/SKILL.md` and `agents/openai.yaml` (hand-written, no
      Claude-only constructs)
- [ ] extend Step 3 of both `loopai-plan` skills: when a T3 runtime file exists, print the
      `/loopai:loopai-t3 <plan> <FLAGS>` (Codex: `$loopai-t3`) line and offer it alongside Orca
- [ ] bump `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` to `0.5.8`
- [ ] run `make check-symlinks test-symlinks check-codex-skills test-codex-skills check-plugin
      test-plugin` - must pass before task 8

### Task 8: Verify acceptance criteria
- [ ] verify all requirements from Overview are implemented
- [ ] verify best-effort behavior: runs with `--t3` and no server, no token, stale runtime file, or
      401 complete exactly like runs without `--t3`
- [ ] verify no test reads the real `~/.t3` or `~/.config/loopai` (grep tests for `UserHomeDir`
      without `HOME`/`T3CODE_HOME` redirection)
- [ ] run `make test`
- [ ] run `make lint` - all issues must be fixed
- [ ] cross-compile `GOOS=windows GOARCH=amd64 go build ./...` and `GOOS=linux go build ./...`
- [ ] verify test coverage of `pkg/t3` is at least 80%

### Task 9: [Final] Update documentation
- [ ] add `docs/t3-code.md`: setup (token, `T3CODE_HOME`, `LOOPAI_T3_URL`), `--t3`, `--t3-launch`,
      the skill, PR linking and auto-settle, limitations (title-only status, no fork features),
      and `t3.json` examples: a "loopai review" action (`loopai --t3 --review`) and a "loopai
      dashboard" action (`loopai --serve --watch .` with `previewUrl: http://localhost:8080`,
      noting that T3 on Windows auto-detects only common ports)
- [ ] update `README.md` (features, plugin skill list, configuration env list with `LOOPAI_T3`,
      progress/dashboard section) and `llms.txt`
- [ ] update `CLAUDE.md`: `pkg/t3` in the project structure, the skill inventory (nine skills,
      `loopai-t3` description), and the reporter's place in the logger chain

## Technical Details
- Logger chain after the change: cmux → orca → t3 → awake → `SectionTimer` → dashboard/base.
- Title updates are a few per run (phase and task transitions); never on output lines.
- Env contract between launcher and run: `LOOPAI_T3=true`, `LOOPAI_T3_TOKEN`, `LOOPAI_T3_THREAD_ID`.
  `LOOPAI_T3_TOKEN` and `LOOPAI_T3_THREAD_ID` are read directly by `pkg/t3`, not `opts` tags, so
  cmux hand-off does not type the token into another shell.
- Launch sequence: runtime → shell snapshot → project → `vcs.createWorktree` → copy inputs →
  `thread.create` → `terminal.open` → `terminal.write "<command>\r"`.

## Post-Completion
**Manual verification** (live T3 Code server, not automatable here):
- confirm the Effect RPC frame shape (`id`, `headers`) against a real `/ws` session before trusting
  the fake-based tests
- run `/loopai:loopai-t3` on a toy repo: thread appears with the worktree badge, terminal shows
  loopai output, title follows phases, final title is `done`/`failed`, worktree survives the run
- check the thread on the mobile app and via app.t3.codes over the relay
- run `loopai --pr` for that branch and confirm the PR appears on the thread and the thread settles
  after merge
- confirm T3 archive/cleanup behavior for the T3-managed worktree matches expectations
