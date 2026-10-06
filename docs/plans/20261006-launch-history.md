# Launch History: Recent Model Combinations and Launcher Recommendation

## Overview
- loopai records every plan-executing launch in a global `launch-history` file under the
  loopai config directory: the effective `task_model`/`review_model`/`external_reviewers`
  combination rendered as the three pass-through flags, plus the launcher that started the
  run (`orca`, `t3`, or `cli`).
- The `loopai-plan`, `loopai-t3`, and `loopai-orca` skills read that file and offer the most
  recent combinations as ready-made options instead of asking the user to type
  `--task-model ... --review-model ... --external-reviewers ...` by hand on every launch. A
  model name typed once becomes an option from the next launch on.
- `loopai-plan` marks the most recently used launcher as "(Recommended)" instead of always
  recommending Orca, which today is only the first option in the list.
- Recording is best-effort like cmux, Orca, and T3 reporting: it never fails, delays, or
  changes a run. The history file holds flag strings only, never tokens or paths.

## Decisions
- **Context**: the user runs plans from `loopai-plan` with no `task_model`/`review_model`/
  `external_reviewers` keys set, so Step 3 asks for flags every time and always recommends
  Orca. New model names appear regularly, so a static preset list would need manual upkeep.
- **Chosen approach**: Go records the launch (so manual CLI runs, skill launches, and T3
  agent-mode runs all contribute); the skills read the file with shell and present options.
  The launcher is derived from the existing `--orca`/`--t3` flags and the ACP path, with no
  new CLI flag. One global file only, no per-repository history.
- **Rejected alternatives**: setting the three config keys (one fixed combination, new
  models still typed by hand); named presets in a config file (manual upkeep); an explicit
  `--launcher=<name>` flag (more skill and launcher changes for the same information);
  per-repository history (the user's model choice does not depend on the project).
- **Verified facts**:
  - `cfg.TaskModel`/`cfg.ReviewModel` hold config-file values only; effective specs come from
    `resolveSpec(o.TaskModel, cfg.TaskModel)` (`cmd/loopai/main.go:4259`) and
    `resolveReviewSpec(o, cfg)` (`:4317`), which falls back to the task spec.
  - Specs and the reviewer chain are final once `resolveExecutionDeps` (`:589–617`) returns;
    `run()` calls it at `:510`. `modeRequiresBranch` (`:3326`) already names the two
    plan-executing modes, `ModeFull` and `ModeTasksOnly`.
  - `runPlanMode` (`:4372`) can continue from plan creation straight into a `ModeFull` run
    (`:4524–4584`), and T3 agent mode runs through `acpRunner.run` (`cmd/loopai/acp.go:163`)
    and `prepareNonInteractiveRequest` (`main.go:708–747`), never through the `run()` hook
    point. `loadACPSessionConfig` forces `cfg.T3` and `cfg.Orca` false (`acp.go:217`), so the
    ACP path must pass its launcher tag explicitly.
  - `applyCLIOverrides` (`:6747`) ORs `--orca`/`--t3` into `cfg.Orca`/`cfg.T3` (`:6752–6753`);
    both may come from config or `LOOPAI_ORCA`/`LOOPAI_T3`, and both can be true at once.
  - The global config dir is `o.ConfigDir` (from `--config-dir`/`LOOPAI_CONFIG_DIR`) or
    `config.DefaultConfigDir()` (`pkg/config/config.go:518`); `Config` has no getter for it.
  - `saveJSONState` (`cmd/loopai/json_state.go`) is the existing temp-file-plus-rename
    writer; there is no line-append helper.
  - `externalReviewSelection` (`main.go:2969`) has `chainLabel()`/`bannerLabel()` but no
    `provider:model:effort,...` renderer; `resolvedReviewer.modelSpec()` (`:2955`) gives
    `model[:effort]`.
  - `scripts/check-loopai-t3-skill_test.sh` requires the exact `loopai --t3-launch $FLAGS "$PLAN"`
    line and identical report sections in the Claude and Codex t3 skills.
  - Plugin and marketplace manifests are both at `0.6.2`.

## Context (from discovery)
- Files/components involved: `cmd/loopai/main.go` (`run`, `runPlanMode`,
  `prepareNonInteractiveRequest`, `applyCLIOverrides`, `resolveSpec`, `resolveReviewSpec`,
  `externalReviewSelection`), `cmd/loopai/acp.go`, new `cmd/loopai/launch_history.go`,
  `assets/claude/skills/{loopai-plan,loopai-t3,loopai-orca}/SKILL.md` and their
  `assets/codex/skills/` counterparts, `.claude-plugin/plugin.json`,
  `.claude-plugin/marketplace.json`, `scripts/check-loopai-t3-skill_test.sh`, `README.md`,
  `docs/t3-code.md`, `llms.txt`, `CLAUDE.md`.
- Related patterns found: best-effort integrations that never affect execution (cmux, orca,
  t3, awake); atomic state files written through temp-plus-rename with mode `0600`; tests
  isolate `HOME` and `USERPROFILE` together and pass `ConfigDir: t.TempDir()`.
- Dependencies identified: none new.

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
- Maintain backward compatibility

## Testing Strategy
- **Unit tests**: required for every task (see Development Approach above)
- **Shell suites**: skill contract changes are covered by the existing
  `scripts/check-*_test.sh` suites run by `make test`; extend them where a new contract is
  introduced (the history lookup in the three skills)
- **E2E tests**: not applicable (no dashboard change)

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): tasks achievable within this codebase - code changes, tests, documentation updates
- **Post-Completion** (no checkboxes): items requiring external action - manual testing, changes in consuming projects, deployment configs, third-party verifications
- **Checkbox placement**: Checkboxes belong only in Task sections (`### Task N:` or `### Iteration N:`). Do not put checkboxes in Success criteria, Overview, Context, Decisions, or Decision Log — they cause extra loop iterations.

## Implementation Steps

### Task 1: Launch-history model and file I/O
- [ ] create `cmd/loopai/launch_history.go` with `launchEntry{When time.Time; Launcher string; Flags string}`, the constants `launcherOrca`/`launcherT3`/`launcherCLI`, `launchHistoryLimit = 10`, and `launchHistoryFile = "launch-history"`
- [ ] implement `launchHistoryPath(configDir string) string`: `configDir` when non-empty, else `config.DefaultConfigDir()`, joined with `launch-history` (mirror how `--config-dir` reaches `config.Load`, do not copy the `claudeswap.Detect(config.DefaultConfigDir())` precedent that ignores the override)
- [ ] implement `readLaunchHistory(path) []launchEntry`: one entry per line, format `<RFC3339 UTC>\t<launcher>\t<flags>` newest first; skip blank, malformed, or unknown-launcher lines instead of failing; a missing file is an empty history
- [ ] implement `recordLaunch(path string, entry launchEntry) error`: read the existing entries, drop any entry whose `Flags` equal the new one (dedup by combination, launcher ignored), prepend the new entry, truncate to `launchHistoryLimit`, and write the whole file with the `saveJSONState` pattern (`MkdirAll 0750`, `CreateTemp` in the same directory, chmod `0600`, write, `Rename`) so a concurrent reader never sees a partial file; factor the temp-plus-rename part into a small `writeFileAtomic(path, tempPattern, label string, write func(io.Writer) error) error` in `json_state.go` and have `saveJSONState` call it
- [ ] an entry with empty `Flags` (every phase on loopai defaults) is still recorded, so "no flags" can be the most recent combination
- [ ] write tests for `readLaunchHistory` (missing file, blank and malformed lines, unknown launcher skipped, order preserved)
- [ ] write tests for `recordLaunch` (new file created with mode `0600` on POSIX, dedup moves an existing combination to the top and keeps the newer launcher, cap at ten, directory created, unwritable directory returns an error and leaves no temp file behind)
- [ ] write a test that `saveJSONState` output is unchanged after the refactor
- [ ] run `go test ./cmd/loopai/ -run 'Launch|JSONState'` - must pass before task 2

### Task 2: Render the effective launch flags and launcher
- [ ] add `launchFlags(o opts, cfg *config.Config, sel externalReviewSelection) string`: `--task-model <spec>` from `resolveSpec(o.TaskModel, cfg.TaskModel)` when non-empty, `--review-model <spec>` from `resolveReviewSpec(o, cfg)` only when it differs from the task spec or was set explicitly (an inherited review spec must not be frozen into the history as an explicit flag), and `--external-reviewers <chain>` from a new `externalReviewSelection.flagValue()` that joins each resolved reviewer as `provider[:model[:effort]]` with `,` — only when the chain is explicit (`sel.Explicit`), since an auto-selected reviewer is a consequence of the task provider and not a choice to replay; joined with single spaces
- [ ] every rendered value must match `^[A-Za-z0-9._:,+-]+$`, the charset the three skills accept; when a value does not, omit that flag from the entry (a `custom_review_script`-based chain is the known case) so the skills never offer a string they would reject
- [ ] add `launcherFor(cfg *config.Config) string`: `launcherT3` when `cfg.T3`, else `launcherOrca` when `cfg.Orca`, else `launcherCLI` (T3 wins because `--t3-launch` terminal mode runs `loopai --t3` while Orca's skill runs `loopai --orca`; a config enabling both is a reporting choice, not a launcher)
- [ ] write table-driven tests for `launchFlags` (no flags set, task only, explicit review equal to task, explicit review differing, inherited review omitted, explicit chain with efforts, auto-selected chain omitted, custom reviewer omitted, CLI override beating config)
- [ ] write tests for `launcherFor` (none, orca, t3, both)
- [ ] run `go test ./cmd/loopai/ -run 'LaunchFlags|LauncherFor'` - must pass before task 3

### Task 3: Record launches on every plan-executing path
- [ ] add `recordLaunchHistory(o opts, cfg *config.Config, sel externalReviewSelection, launcher string, stderr io.Writer)`: builds the entry with `time.Now().UTC()`, calls `recordLaunch(launchHistoryPath(o.ConfigDir), ...)`, and on error prints one `warning: launch history not recorded: <err>` line to stderr (through the request's `Out`/stderr writer, never `os.Stdout`, so ACP stdout discipline holds) and returns; it must never return an error or abort the run
- [ ] call it in `run()` right after `resolveExecutionDeps` succeeds and the gen-agents early return, gated on `mode == processor.ModeFull || mode == processor.ModeTasksOnly` (reuse `modeRequiresBranch`), with `launcherFor(cfg)`; a plan chain records once per invocation
- [ ] call it in `runPlanMode` at the point where plan creation continues into a `ModeFull` run (`main.go:4524–4584`), using the same selection the continuation reuses, so a `--plan` session that proceeds to execution is recorded too; plan creation that stops at the draft records nothing
- [ ] call it from `acpRunner.run` after `prepareNonInteractiveRequest` succeeds, with launcher `launcherT3` passed explicitly (the ACP config forces `cfg.T3` off) and the session's `ConfigDir`; the warning goes to the stderr the ACP runner already uses
- [ ] `--review`, `--external-only`, `--gen-agents`, watch-only, close-out, `--t3-launch`, and the config utilities record nothing
- [ ] write tests through `run()` with `ConfigDir: t.TempDir()` and isolated `HOME`/`USERPROFILE`: a full run records one entry with the expected flags and `cli`; `--t3` records `t3`; `--orca` records `orca`; `--review` and `--external-only` record nothing; a second run with the same flags keeps one entry; a failing recorder (read-only config dir) prints the warning and the run outcome is unchanged
- [ ] write an ACP test (extend `acp_test.go` fixtures) asserting an entry with launcher `t3` after a prompt runs
- [ ] write a `runPlanMode` continuation test asserting the entry is written when execution follows plan creation and absent when the user stops at the draft
- [ ] run `go test ./cmd/loopai/` - must pass before task 4

### Task 4: Offer recent combinations in the Claude skills
- [ ] in `assets/claude/skills/loopai-plan/SKILL.md` Step 3, add a `HISTORY` step after `FLAGS`: read `${LOOPAI_CONFIG_DIR:-$HOME/.config/loopai}/launch-history` with a `cut`/`awk` snippet, keep lines whose launcher is `orca`, `t3`, or `cli`, and take the first three distinct flag strings in file order (newest first); a missing or empty file yields no history. Validate every flag string against the same `--task-model|--review-model|--external-reviewers` grammar and `^[A-Za-z0-9._:,+-]+$` charset already in the skill and drop a line that fails
- [ ] launcher recommendation: `LAST_LAUNCHER` is the launcher of the newest line whose launcher is `orca` or `t3`; "(Recommended)" goes to that launcher's option when it is available, otherwise to the first launcher offered as today; the order of the options stays Orca, T3 Code, here, not now
- [ ] after the user picks Orca or T3 Code, replace the current "type the flags" second question with: options for each `HISTORY` entry labelled by its flags, newest first and the newest marked "(Recommended)"; when `FLAGS` from config is non-empty and not already among them, it is listed too as "From .loopai/config: <FLAGS>"; then "No flags (loopai defaults: Claude for every phase)" and "Cancel"; "Other" remains the manual-entry path with the existing token validation. When both `HISTORY` and `FLAGS` are empty the question is the current one. Keep the rule that a selected or typed string is passed to `loopai-orca`/`loopai-t3` exactly
- [ ] in `loopai-t3/SKILL.md` and `loopai-orca/SKILL.md` Step 0b, when `$ARGUMENTS` carries no pass-through flag, read the same `HISTORY` and ask the same question before launching (this is new: today an empty `FLAGS` launches on config values silently); when invoked from `loopai-plan` with flags the question is skipped as before. Keep the exact `loopai --t3-launch $FLAGS "$PLAN"` line that `check-loopai-t3-skill_test.sh` pins
- [ ] state in each skill that the file is written by loopai itself, holds flag strings only, and that a line the skill cannot validate is skipped, never offered
- [ ] write regression cases in `scripts/check-loopai-t3-skill_test.sh` (and a new `scripts/check-launch-history-skills_test.sh` wired into the `Makefile` `test` target and CI if the t3 suite's shape does not fit) asserting the three Claude skills reference `launch-history`, the charset check, and the `(Recommended)` rule text
- [ ] run `make check-symlinks test-symlinks test-grill-skill` and the t3 skill suite - must pass before task 5

### Task 5: Mirror the Codex skills and bump the manifests
- [ ] apply the Task 4 changes to `assets/codex/skills/{loopai-plan,loopai-t3,loopai-orca}/SKILL.md` in Codex phrasing (plain numbered choices instead of `AskUserQuestion`, no Task subagents), keeping report sections identical where `check-loopai-t3-skill_test.sh` compares them
- [ ] bump `.claude-plugin/plugin.json` and the `loopai` entry in `.claude-plugin/marketplace.json` from `0.6.2` to `0.7.0` (new user-visible skill behavior)
- [ ] run `make check-codex-skills test-codex-skills check-plugin test-plugin` - must pass before task 6

### Task 6: Verify acceptance criteria
- [ ] verify all requirements from Overview are implemented
- [ ] verify edge cases are handled (missing file, malformed lines, read-only config dir, ACP path, plan-mode continuation, custom reviewer chain, dedup across launchers)
- [ ] run full test suite (`make test`, in WSL on Windows)
- [ ] run `GOOS=windows GOARCH=amd64 go build ./...`
- [ ] run linter (`make lint`) - all issues must be fixed
- [ ] verify test coverage of `launch_history.go` is at or above the project standard (80%+)

### Task 7: [Final] Update documentation
- [ ] `README.md`: add `launch-history` to the `~/.config/loopai/` tree in the Configuration section and one paragraph under the plugin section describing what the skills offer and how to clear the history (delete the file)
- [ ] `docs/t3-code.md`: in the `--t3-launch` section, note that launches are recorded with launcher `t3` (agent mode included) and that `loopai-plan`'s recommendation follows the last launcher
- [ ] `llms.txt`: update the plugin paragraph and the "Configuration and data" block
- [ ] `CLAUDE.md`: add a short paragraph after the `--gen-agents` paragraph describing the recorder's contract (best-effort, three hook points, launcher precedence, explicit-only chain, charset bound, dedup and cap) so future changes keep it
- [ ] update project knowledge docs if new patterns discovered

## Technical Details
- **File**: `<config dir>/launch-history`, text, newest first, one entry per line:
  `2026-10-06T12:34:56Z\tt3\t--task-model claude:opus:high --review-model claude:opus:high --external-reviewers codex:gpt-6-astra:high`.
  Tabs separate the three fields; flag strings never contain tabs because every value is
  bounded by the skill charset. Written whole through temp-plus-rename, mode `0600`, at most
  ten lines. No lock: a lost update between two simultaneous launches costs one history line.
- **Dedup key**: the flags string. A repeated combination moves to the top and takes the new
  launcher and timestamp.
- **Launcher precedence**: `t3` > `orca` > `cli`, from `cfg.T3`/`cfg.Orca` after CLI overrides,
  except the ACP path, which always records `t3`.
- **Explicit-only external chain**: the history replays what the user chose, so an
  auto-selected reviewer is omitted and the next launch selects it again the same way.
- **Skill reading**: `awk -F'\t' '$2 ~ /^(orca|t3|cli)$/ {print $3}'` then first three
  distinct non-empty strings; the launcher recommendation takes the first line whose second
  field is `orca` or `t3`.
- **Failure policy**: recorder errors are one stderr warning; reader errors in the skills
  mean "no history" and fall back to today's behavior.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- Run `/loopai:loopai-plan` once with the installed plugin after `make build` and installing the
  binary: confirm the launcher recommendation follows the previous launch and the flag
  options list the last combinations.
- Launch one plan in T3 Code agent mode and confirm a `t3` line appears in
  `~/.config/loopai/launch-history`.

**External system updates**:
- Reinstall or update the `loopai` plugin from the marketplace so Claude Code picks up `0.7.0`;
  run `make install-codex-skills` for the Codex copies.
