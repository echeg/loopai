# --t3-launch starts a T3 Code provider session by default

## Overview

`loopai --t3-launch` (and the `loopai-t3` skill around it) creates a T3 Code worktree and thread,
then types `loopai --t3 <plan>` into a thread terminal. T3 Code derives "working" only from a
provider session, so such a thread never shows the working state, plan steps, streamed reasoning,
or the completion report as a message — only a title and a pin. The experimental ACP provider mode
(`loopai-acp` behind a `grok` provider instance) gives all of that, but today it can only be
started by hand: open a thread with the `loopai` instance and type the plan path as a message.

This plan makes the launcher start that provider session itself. T3 Code's public orchestration
dispatch accepts `thread.turn.start` — the command the UI sends when the user submits a message —
with a `modelSelection` naming a provider instance. When an enabled `grok` instance whose
`binaryPath` is `loopai-acp` exists in `<T3 home>/userdata/settings.json`, `--t3-launch` creates
the worktree and thread as before, carries the plan and `.loopai` overrides over (which a hand-made
thread never gets), and dispatches a user turn `<plan> [flags]` to the thread instead of opening a
terminal. T3 Code then spawns `loopai --acp` in the thread's worktree, and the thread reads
"Working" for the whole run. Without such an instance, or with `--t3-launch=terminal`, the
terminal launch is unchanged.

## Decisions

- **Context**: the user wants the ACP-backed thread by default from the skill, not by typing the
  plan path into a new thread by hand.
- **Chosen approach**: extend `--t3-launch` with an optional mode value, `--t3-launch[=auto|agent|terminal]`,
  attached with `=` like `--cmux-workspace[=always|auto]`. `auto` (the bare flag) picks `agent`
  when a usable loopai provider instance is configured and `terminal` otherwise. `agent` fails when
  no instance is configured, naming `docs/t3-code.md`. The skill keeps calling the bare flag.
- **Rejected alternatives**: a config key (`t3_launch_mode`) — nothing else in the launcher is
  configurable and the auto rule already does the right thing; keeping the terminal launch and
  additionally dispatching a turn — the two runs would race for the same worktree and run lock;
  changing the skill to create the thread through the T3 UI — no public command does that for a
  user turn except the one used here.
- **Instance discovery**: `pkg/t3` reads `providerInstances` from `<T3 home>/userdata/settings.json`
  read-only, the file `docs/t3-code.md` already tells users to edit; loopai never writes below the
  T3 home. A usable instance has `driver == "grok"`, `enabled != false`, `config.enabled != false`,
  and a `config.binaryPath` whose basename is `loopai-acp` or `loopai-acp.exe` (case-insensitive).
  The map key is the `instanceId` T3 Code expects in `modelSelection` (the schema decodes
  `instanceId` falling back to `provider`; built-in instances use ids such as `claudeAgent`,
  custom ones their settings key). The
  model is `loopai`, the single model `loopai-acp models` advertises. With several usable instances
  the first key in sorted order wins and the launcher says which.
- **Message text**: `<plan relative path, forward slashes> [--task-model SPEC] [--review-model SPEC] [--external-reviewers LIST]`
  — exactly the grammar `parseACPPrompt` (`cmd/loopai/acp.go:224`) reads. That grammar splits on
  whitespace, so agent mode refuses a plan path containing whitespace before creating anything;
  terminal mode keeps accepting it.
- **No token in the thread**: agent mode opens no terminal and places no environment in the
  thread. The bearer token is used only for the launcher's own dispatches. The ACP run forces `t3`
  off, so the thread's title is not updated by loopai; in agent mode `thread.create` sets
  the plain run name, matching the turn's `titleSeed` so T3 can replace it with a generated title.
  If generation does not replace it, the run name remains. Terminal mode keeps `<plan> · starting`.
- **Partial failure**: a failed `thread.turn.start` is a `PartialLaunchError` listing the worktree
  and thread, as today; nothing is rolled back.
- **Verified facts** (T3 Code build of October 2026, `server.asar`): the dispatch payload union
  includes `thread.turn.start` with fields `commandId`, `threadId`, `message{messageId, role:"user",
  text, attachments[], context?}`, `modelSelection?{instanceId, model}`, `titleSeed?`,
  `runtimeMode`, `interactionMode`, `bootstrap?`, `createdAt`; an authenticated probe of that command
  with a nonexistent thread passed schema decoding and reached the orchestrator before failing,
  while `thread.message.assistant.*` commands are not in the union. `CommandId` and `MessageId`
  are branded strings; `NewID` (`pkg/t3/client.go:80`) already produces the accepted UUID shape
  for `commandId`. `Client.Dispatch` fills `createdAt` only for `*ThreadCreate`
  (`client.go:203`). `--t3-launch` is a bool `opts` field (`cmd/loopai/main.go:84`), validated by
  `validateT3LaunchFlags` (3759), run by `runT3LaunchCommand` (3859), built in `t3.Launch`
  (`pkg/t3/launch.go:88`) with `carryInputs` (169) before thread creation and the terminal steps
  at 134-147. `modelSelection` (`pkg/t3/reporter.go:474`) maps the task provider to
  `InstanceClaude`/`InstanceCodex`. `HomeDir` (`pkg/t3/runtime.go:52`) resolves the T3 home.

## Context (from discovery)

- Files involved: `pkg/t3/launch.go`, `pkg/t3/client.go` (`ModelSelection`, `NewThreadCreate`,
  `Dispatch`), `pkg/t3/runtime.go` (home resolution; new settings reader beside it),
  `pkg/t3/reporter.go` (`runName`, `modelSelection`), `cmd/loopai/main.go` (`opts.T3Launch`,
  `validateT3LaunchFlags`, `t3LaunchArgs`, `runT3LaunchCommand`, `isStandaloneCommand`),
  `cmd/loopai/acp.go` (`parseACPPrompt` grammar the message must satisfy),
  `assets/claude/skills/loopai-t3/SKILL.md`, `assets/codex/skills/loopai-t3/SKILL.md`,
  `docs/t3-code.md` (sections "Launching a plan in T3 Code", "Running a plan", "Limitations"),
  `README.md:1308-1320`, `llms.txt:48,216,244`, `CLAUDE.md:98` and the `--t3-launch` paragraph
  near line 795, `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json`.
- Tests: `pkg/t3/launch_test.go` (`fakeDispatcher`, `fakeRPC`, `launchFixture`, `TestLaunch`,
  `TestLaunchPartialFailures`), `pkg/t3/runtime_test.go`, `cmd/loopai/main_test.go`
  (`TestValidateT3LaunchFlags` 3865, `TestT3LaunchArgs` 3909, `TestRunT3LaunchCommand` 3942,
  `TestRunT3LaunchCommandErrors` 3983; `newT3Session` is replaced so no test reaches a server).
- Related pattern: `--cmux-workspace[=always|auto]` is the existing optional-value flag.
- Concurrent work: plan `20261005-report-pr-body-and-retro` also bumps both plugin manifests and
  edits `README.md`/`llms.txt`/`CLAUDE.md` in other sections; the version bump is the one expected
  merge conflict and resolves to the higher number.

## Development Approach

- **Testing approach**: Regular (code first, then tests)
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
- Maintain backward compatibility: the bare `--t3-launch` on a machine without a loopai provider
  instance behaves exactly as before
- Tests must point `T3CODE_HOME` at `t.TempDir()`; never read the real `~/.t3`

## Testing Strategy

- **Unit tests**: required for every task; table-driven `testify` cases, fake dispatcher and RPC
  as in `launch_test.go`, settings fixtures under a temporary `T3CODE_HOME`
- **Shell regression suites**: skill edits are covered by `make check-symlinks check-codex-skills
  check-plugin` and their test suites
- **E2E tests**: none; the live T3 Code check is in Post-Completion

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): Go, skill, and documentation changes here
- **Post-Completion** (no checkboxes): a live launch against a running T3 Code server
- **Checkbox placement**: checkboxes belong only in Task sections

## Validation Commands

- `make test`
- `make lint`

## Implementation Steps

### Task 1: Discover the loopai provider instance from T3 Code settings

- [x] add `pkg/t3/settings.go` with `ProviderInstance{ID, Driver, BinaryPath string; Enabled bool}` and
      `FindLoopaiInstance(getenv func(string) string) (ProviderInstance, bool, error)`: resolve
      `HomeDir(getenv)`, read `userdata/settings.json` with a size cap, decode only
      `providerInstances` (unknown keys ignored), and return the first usable instance in sorted
      key order per the Decisions rule; a missing file or missing `providerInstances` is
      `(_, false, nil)`, malformed JSON is an error
- [x] export `LoopaiModel = "loopai"` in `pkg/t3` and make `cmd/loopai-acp`'s `models` output
      reference the same value so the two cannot drift
- [x] write `pkg/t3/settings_test.go`: missing file, no instances, disabled instance, other driver,
      binary that is not `loopai-acp`, `loopai-acp.exe`, two usable instances (sorted first wins),
      `config.enabled: false`, malformed JSON, oversized file, `T3CODE_HOME` override
- [x] repair validation portability: accept both Git worktree conflict messages and explicitly
      advance the directory timestamp in the rollout-cache refresh test; synchronize the
      worktree cancellation test with completed Git registration instead of a fixed timeout;
      make the Pi EOF assertion check the complete JSON stream across jq versions
- [x] run `go test ./pkg/t3/...` - must pass before task 2

### Task 2: Add the thread.turn.start command

- [x] in `pkg/t3/client.go`, add `ThreadTurnStart` with `threadId`, `message{messageId, role,
      text, attachments: []}` (attachments serialized as an empty array, never null),
      `modelSelection`, `titleSeed`, `runtimeMode: "full-access"`, `interactionMode: "default"`,
      `createdAt`, and `NewThreadTurnStart(threadID, text, titleSeed string, model ModelSelection)`
      using `NewID()` for the message id
- [x] generalize the `createdAt` fill in `Client.Dispatch` to an unexported `stampCreatedAt(string)`
      interface implemented by `ThreadCreate` and `ThreadTurnStart`
- [x] extend `pkg/t3/client_test.go`: JSON shape of the new command (field names, empty
      `attachments` array, `createdAt` stamped, `modelSelection` present), and that
      `ThreadTitleUpdate` is still not stamped
- [x] run `go test ./pkg/t3/...` - must pass before task 3

### Task 3: Agent-mode launch in pkg/t3

- [x] add `LaunchMode` (`LaunchAuto`, `LaunchAgent`, `LaunchTerminal`) and `Mode LaunchMode` plus
      `Instance ProviderInstance` (set by the caller when found) to `LaunchRequest`; add
      `Mode LaunchMode` to `LaunchResult` reporting the effective mode
- [x] in `Launch`, resolve the effective mode first: `auto` → agent when `req.Instance` is set, else
      terminal; `agent` with no instance → error before any RPC; agent mode with whitespace in the
      plan's relative path → error before any RPC
- [x] in agent mode, after `carryInputs`, create the thread with
      `ModelSelection{InstanceID: instance.ID, Model: LoopaiModel}`, then dispatch
      `NewThreadTurnStart(threadID, agentPrompt(rel, req.Args), runName(req.PlanFile), sameSelection)`
      where `agentPrompt` joins `filepath.ToSlash(rel)` and the args with single spaces; skip
      `OpenTerminal` and `WriteTerminal` entirely
- [x] keep terminal mode byte-identical to today, including `modelSelection(req.Executor, req.Model)`
      on the thread
- [x] extend `pkg/t3/launch_test.go`: agent mode dispatches `thread.create` with the instance
      selection then `thread.turn.start` with the expected text and title seed and makes no RPC
      terminal calls; auto with and without an instance; `agent` without instance fails before
      `CreateWorktree`; whitespace plan path fails before `CreateWorktree` in agent mode and
      succeeds in terminal mode; a failed turn dispatch yields `PartialLaunchError` naming worktree
      and thread; `LaunchResult.Mode` is set in every success case
- [x] run `go test ./pkg/t3/...` - must pass before task 4

### Task 4: Wire the mode through the CLI

- [x] change `opts.T3Launch` to `string` with `long:"t3-launch" optional:"true" optional-value:"auto"
      choice:"auto" choice:"agent" choice:"terminal"`, mirroring `--cmux-workspace`; update every
      `o.T3Launch` boolean use (`validateT3LaunchFlags`, `isStandaloneCommand`, the
      `runConfiguredStandaloneCommand` case, `acp.go:63`)
- [x] in `runT3LaunchCommand`, call `t3.FindLoopaiInstance(os.Getenv)` unless the mode is
      `terminal`; a settings read error is fatal for `agent` and a warning that falls back to
      terminal for `auto`; pass `Mode` and `Instance` in the `LaunchRequest`
- [x] print the mode after the thread line: agent mode says the thread runs loopai as a provider
      session and the stop button cancels the run; terminal mode on `auto` adds one line naming
      the `providerInstances` setup in `docs/t3-code.md` that enables agent mode
- [x] update `cmd/loopai/main_test.go`: `TestValidateT3LaunchFlags` for the three values and a
      rejected fourth, `TestIsStandaloneCommandT3Launch`, `TestRunT3LaunchCommand` split into
      terminal (no settings file under a temporary `T3CODE_HOME`) and agent (settings file with a
      usable instance; assert no terminal opened, the dispatched turn text contains the forward-slash
      plan path and `--task-model codex:gpt-5:high`, and the output names agent mode), plus
      `agent` with no instance and `auto` with malformed settings in `TestRunT3LaunchCommandErrors`
- [x] run `go test ./cmd/loopai/ -run 'T3Launch'` - must pass before task 5

### Task 5: Update the loopai-t3 skills

- [x] `assets/claude/skills/loopai-t3/SKILL.md`: SCOPE and Step 3 describe both modes; Step 4's
      report reads the mode from the command output and, in agent mode, replaces the terminal
      sentence with the provider-session behavior (working state, plan steps, report as final
      message, stop button cancels) and drops the token-in-terminal note; add an `--t3-launch=terminal`
      escape hatch to Pitfalls for runs that need the terminal
- [x] mirror the changes in `assets/codex/skills/loopai-t3/SKILL.md`
- [x] bump `.claude-plugin/plugin.json` and the loopai entry in `.claude-plugin/marketplace.json`
      by one patch level over their current value
- [x] add mode/report regression checks for both skills in `scripts/check-loopai-t3-skill_test.sh`,
      wired into `make test-codex-skills`
- [x] run `make check-symlinks check-codex-skills check-plugin test-symlinks test-codex-skills test-plugin`
      - must pass before task 6

### Task 6: Verify acceptance criteria

- [x] verify the bare `--t3-launch` with no settings file reproduces today's terminal launch
      exactly (same RPC calls, same typed command)
- [x] verify `--t3-launch` with a usable instance opens no terminal and dispatches the turn
- [x] verify `--t3-launch=terminal` ignores a configured instance
- [x] ➕ synchronize `TestReporterExistingThread` with the completed pin operation before
      stopping the reporter, fixing the intermittent full-suite failure found during acceptance
      validation; verify the test with 100 race-enabled repetitions
- [x] run `make test`
- [x] run `make lint` - all issues must be fixed
- [x] run `GOOS=windows GOARCH=amd64 go build ./...`

### Task 7: [Final] Update documentation

- [x] `docs/t3-code.md`: rewrite "Launching a plan in T3 Code" for the two modes and the auto
      rule, point "Running a plan" at `--t3-launch` as the normal way to start a provider-session
      thread, and update "Limitations" to say which thread shape each mode produces
- [x] `README.md:1308-1320`, `llms.txt:48,216,244`, and the `--t3-launch` paragraphs in
      `CLAUDE.md` (line 98 and near 795): describe the mode value, the settings lookup, and that
      agent mode places no token in the thread
- [x] update the `--t3-launch` flag description string in `opts`

## Technical Details

- **Flag**: `--t3-launch[=auto|agent|terminal]`; the value must be attached with `=`, as with
  `--cmux-workspace`, because the plan path follows as a positional.
- **Dispatch sequence (agent mode)**: `vcs.createWorktree` → HEAD check → `carryInputs` →
  `thread.create{modelSelection:{instanceId, model:"loopai"}, branch, worktreePath, title:"<run>"}`
  → `thread.turn.start{threadId, message:{messageId, role:"user", text, attachments:[]},
  modelSelection:{instanceId, model:"loopai"}, titleSeed:"<run>", runtimeMode:"full-access",
  interactionMode:"default", createdAt}`.
- **Turn text**: `docs/plans/20261005-demo.md --task-model codex:gpt-5:high --external-reviewers claude:opus`
  — forward slashes on every platform; T3 resolves it against the thread's worktree.
- **Settings subset read**:

  ```json
  {"providerInstances": {"<id>": {"driver": "grok", "enabled": true,
                                  "config": {"enabled": true, "binaryPath": ".../loopai-acp.exe"}}}}
  ```

- **Mode output lines**:

  ```text
  started loopai in T3 Code thread <id>
  mode: agent (loopai provider session, instance <id>; the stop button cancels the run)
  worktree: ...
  branch:   ...
  close out from this checkout with: ...
  ```

  or, in terminal mode under `auto`:

  ```text
  mode: terminal
  add a loopai provider instance in providerInstances for agent mode; see docs/t3-code.md
  ```

## Post-Completion

**Manual verification**:

- With the `loopai` provider instance enabled, run `/loopai:loopai-t3 <plan>` and confirm the
  thread reads "Working", shows plan steps, streams reasoning, and ends with the report as a
  message; confirm the plan and uncommitted `.loopai/` overrides reached the worktree.
- The installed server's `canReplaceThreadTitle` requires the initial title to match `titleSeed`
  (or be `New thread`); agent mode now uses the run name for both. Confirm live title generation
  after launch; if unavailable, the plain run name remains without a stale status suffix.
- Re-verify the `thread.turn.start` schema after the next T3 Code update, as `docs/t3-code.md`
  already requires for every Grok-driver contract.

**External system updates**:

- Reinstall the plugin and rerun `make install-codex-skills` for the updated `loopai-t3` skill.
