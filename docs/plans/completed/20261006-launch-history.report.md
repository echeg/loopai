# Report: Launch History: Recent Model Combinations and Launcher Recommendation
plan: `docs/plans/20261006-launch-history.md` · branch: `launch-history` · base: `master` · mode: full · executor: claude · task model: `claude:opus:high` · review model: `claude:opus:high` · started: 2026-10-06T12:32:50Z · finished: 2026-10-06T13:29:35Z

## Summary
loopai now writes a line to a global `<config dir>/launch-history` file every time it launches a plan for execution. Each line holds the effective `--task-model` / `--review-model` / `--external-reviewers` combination, rendered as pass-through flags, and the launcher that started the run (`t3`, then `orca`, then `cli`). The file is newest first, deduplicated by the flags string, capped at 10 entries, and written atomically with mode `0600` through a new `writeFileAtomic` helper that is now shared with `saveJSONState`. Recording is best-effort: if it fails, loopai prints one `warning: launch history not recorded` line on stderr and the run continues unchanged.

Launches are recorded at three points:
- `run()`, gated on `modeRequiresBranch`, so only full and tasks-only runs are recorded.
- `runPlanMode`, when plan creation continues into execution.
- `acpRunner.run`, which always records `t3`.

The three skills `loopai-plan`, `loopai-orca`, and `loopai-t3`, and their Codex copies, read the file with one shared snippet and offer recent combinations as ready-made options. `loopai-plan` marks the launcher of the newest `orca`/`t3` line "(Recommended)". The plugin and marketplace versions went from 0.6.2 to 0.7.0, and the documentation was updated.

The run used 7 task iterations with 0 failed retries. Internal review ran one loop iteration and ended `review_done`. Post-review did not run. Finalize did not run, so there was no base sync.

Phase durations:

| phase | duration_ms |
|---|---:|
| tasks | 2738090 |
| internal review | 573893 |
| external review | 82160 |
| evaluation | 10770 |
| other | 0 |

## Change scope
24 files changed: 1261 additions and 69 deletions, across 8 commits.

**Go code**
- **New files:** `cmd/loopai/launch_history.go` (152 lines) with `cmd/loopai/launch_history_test.go` (520 lines), and `cmd/loopai/json_state_test.go`.
- **Modified:**
  - `cmd/loopai/main.go`: two hook calls, `resolvedReviewer.Spec`, and `externalReviewSelection.flagValue()`.
  - `cmd/loopai/acp.go`: the ACP hook.
  - `cmd/loopai/json_state.go`: the `writeFileAtomic` refactor.
  - `cmd/loopai/main_test.go` and `cmd/loopai/acp_test.go`.

**Skills:** the `SKILL.md` of `loopai-plan`, `loopai-orca`, and `loopai-t3`, under both `assets/claude/skills/` and `assets/codex/skills/`.

**Tests and CI:**
- New `scripts/check-launch-history-skills_test.sh`.
- `Makefile` gains a `test-launch-history-skills` target, wired into `test`.
- `.github/workflows/ci.yml` runs that target.

**Manifests:** `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json`.

**Docs:** `README.md`, `CLAUDE.md`, `docs/t3-code.md`, `llms.txt`, and the plan file.

**Commits, oldest first:**
- `5162e07` launch-history model and atomic file I/O
- `871a683` flag and launcher rendering
- `a55f998` recording on every plan-executing path
- `3cd8b7d` Claude skills
- `42aa088` Codex skills
- `2f8cca9` acceptance verification
- `1a38a2e` documentation
- `2bbc1af` review fixes:
  - The reviewer chain is now rendered from the configured `Spec` instead of the resolved model and effort, so provider defaults are not frozen into the history.
  - Unneeded nil guards were removed.
  - The "No flags" option label in `loopai-plan` was reworded.

## Evidence
none

The run facts contain no validation commands or runs, and no recorded test that failed before and passes now. The plan's own notes say `make test` exited 0 in a Linux container, but that is a pass-only record, not before/after proof.

## Risk
**low**

- **Public APIs:** no new CLI flag or config key, and the `<<<RALPHEX:...>>>` signals are untouched. The only new user-visible artifact is the `launch-history` file.
- **Data schema:** the file is a new, unversioned, tab-separated text format. The six skills parse it with one shared snippet, and a test requires that snippet to be identical everywhere.
- **Configuration:** the file lives under the config directory, honoring `--config-dir`/`LOOPAI_CONFIG_DIR`. The skills read only `${LOOPAI_CONFIG_DIR:-$HOME/.config/loopai}`, so a launch made with `--config-dir` is recorded where no skill looks. Values outside `^[A-Za-z0-9._:,+-]+$` are dropped, and the file holds no tokens or paths.
- **Concurrency:** there is no lock. Two launches at the same moment can lose one history line. The atomic rename means readers never see a partial file.
- **Migrations:** none.
- **Execution impact:** recording returns no error and writes only to stderr, never to the stdout ACP owns.
- **Behavior change:**
  - `loopai-orca` and `loopai-t3` invoked without flags now ask a flag question before launching, where they used to launch silently.
  - A full-mode run that falls into auto-plan records at the `run()` hook even if the description prompt is then canceled.

## Merge danger
**Door:** two-way
A plain revert removes the recorder and the skill changes. A leftover `launch-history` file is inert, and plugin installs pick up the next version bump.

**Blast radius:** local
It affects only the user's own config directory and the launch flow of three skills. No execution path depends on the history.

## Migrations and operational steps
- Reinstall or update the `loopai` plugin so Claude Code picks up 0.7.0, and run `make install-codex-skills` for the Codex copies.
- Rebuild and install the binary with `make build` so launches start being recorded.
- Optional manual checks from the plan's Post-Completion section:
  - Run `/loopai:loopai-plan` once and confirm the launcher recommendation and the flag options.
  - Run one T3 Code agent-mode launch and confirm it writes a `t3` line.

## Plan deviation
All seven tasks are complete; nothing was skipped. The implementation follows the plan, with these differences:

**Review flag rule.** `launchFlags` renders `--review-model` from `resolveSpec(o.ReviewModel, cfg.ReviewModel)` whenever a review spec is set, rather than through `resolveReviewSpec` with a "differs from task" comparison. The result is the same: an inherited review spec is never rendered, and an explicit one is.

**Reviewer chain rendering.** The plan called for `provider[:model[:effort]]` built from the resolved reviewer. The review fix renders the configured remainder from a new `resolvedReviewer.Spec` field instead, so Claude's `opus`/`xhigh` default and a dropped Codex `max` are not frozen into the history. A chain containing a `custom` reviewer renders empty, as the plan intended.

**Stricter skill snippet.** The shipped snippet also drops a line with an unknown or repeated token, or with a spec lacking a `claude`/`codex` provider. The plan's Technical Details were updated to match.

**Added items:**
- ➕ The regression cases went into a new `scripts/check-launch-history-skills_test.sh` (`make test-launch-history-skills`, wired into `make test` and CI) instead of the t3 suite, because the Claude and Codex t3 variants differed until Task 5. The suite executes each skill's extracted snippet against fixture files and requires it to be identical.
- ➕ Task 5 extended that suite to all six skills. It requires the AskUserQuestion wording (four-option cap, "Other") in the Claude copies and the numbered-list / typed-flags wording in the Codex copies.
- ➕ `CLAUDE.md` also lists `make test-launch-history-skills` among the build commands. This was a docs-only change, verified with the asset and skill suites.

**Blocked items:**
- ⚠️ Task 4: on the Windows host, `test-symlinks` and `test-grill-skill` failed identically on an unmodified HEAD (Git Bash cannot create the fixture symlinks, and the grill path helper is POSIX-only), and WSL was unusable. Both were deferred to a POSIX `make test`. This was resolved in Task 6.
- ⚠️ Task 5: `test-codex-skills` and `test-symlinks` passed with `MSYS=winsymlinks:nativestrict`. `test-grill-skill` stayed deferred and was resolved in Task 6.
- ⚠️ Task 6: WSL failed to start (`Wsl/Service/E_UNEXPECTED`). `make test` instead ran in a `golang:1.26` Linux container on a `git archive HEAD` snapshot, as a non-root user with an isolated `HOME`. It exited 0, including the two deferred suites.

**Skipped items:** none.

## Backlog
None.

## External review
### codex:gpt-6-astra:high
- label: codex
- iterations: 1
- duration_ms: 92598
- ended by: done
- had findings: false

The reviewer reported `NO ISSUES FOUND` in iteration 1, so there are no findings to list. The evaluator confirmed that `git diff HEAD` and `git status --porcelain` were both empty and made no fix commit.

## Validation
- Commands: none
- Timings: duration_ms 0, runs 0