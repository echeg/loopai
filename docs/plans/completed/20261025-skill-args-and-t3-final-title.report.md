# Report: Fix skill positional-argument substitution and the T3 final-title wait
plan: `docs/plans/20261025-skill-args-and-t3-final-title.md` · branch: `skill-args-and-t3-final-title` · base: `master` · mode: full · executor: claude · task model: `claude:opus:high` · review model: `claude:opus:xhigh` · started: 2026-10-25T09:28:49Z · finished: 2026-10-25T10:20:14Z

## Summary
The branch fixes the two backlog entries filed on 2026-10-25.

- **Skill argument substitution (major).** Claude Code replaces `$0`, `$1`, … in a skill body with the words of the skill's arguments. That broke the shell snippets in `loopai-plan`, `loopai-orca`, and `loopai-t3` whenever those skills were invoked with arguments. In all six copies (Claude and Codex), the launch-history awk snippet now reads fields as `$(2)`/`$(3)`. `loopai-plan`'s `cfgval` helper now takes its key from a named variable (`key=task_model; cfgval`) instead of `$1`. A new rule in `scripts/check-symlinks.sh` rejects any Claude `SKILL.md` line matching `\$\{?[0-9]`, which keeps the problem from coming back.
- **T3 final-title wait (minor).** `syncTitle` in `cmd/loopai/t3_bridge.go` now skips the title-regeneration wait (up to about 2 minutes) once the attempt has finished (`!b.final`). The final `[loopai: <state>]` title, the unpin, and `thread.mark-unread` are now sent inside the 10 s before-exit stop instead of being lost.

Phase durations: tasks 1413978 ms (≈23m34s, 5 task iterations, 0 failed retries); internal review 1555466 ms (≈25m55s; first review ran, then 1 loop iteration, ended by `review_done`); external review 102398 ms (≈1m42s); evaluation 12854 ms (≈12.9s); other 0 ms. Post-review did not run (0 iterations). Wall-clock was about 51m25s. Finalize did not run, so there was no base sync with `origin/master`.

## Change scope
**Diff:** 19 files changed, 177 additions, 102 deletions, across 6 commits.

Files by area:
- **Skills (6 modified):** `assets/claude/skills/{loopai-plan,loopai-orca,loopai-t3}/SKILL.md` and their Codex counterparts under `assets/codex/skills/`.
- **Checkers and shell tests (3 modified):** `scripts/check-symlinks.sh` (new positional-parameter rule), `scripts/check-symlinks_test.sh` (regression cases), `scripts/check-launch-history-skills_test.sh` (new checks and a `cfgval` fixture run, described under Evidence).
- **Go (2 modified):** `cmd/loopai/t3_bridge.go` (one condition plus two comments) and `cmd/loopai/t3_bridge_test.go` (new `TestT3BridgeFinalTitleSkipsRegenerationWait` with three subtests; the old finished-run subtest is removed; the `finish` harness helper no longer returns the event).
- **Docs (3 modified):** `CLAUDE.md`, `docs/t3-code.md`, and the plan file (checkboxes ticked, plus one recorded change of scope).
- **Manifests (2 modified):** `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json`, both 0.9.3 → 0.9.4.
- **Backlog:** 1 entry added (`plugin-skills-positional-parameter-check.md`) and 2 deleted (`skill-positional-args-substituted.md`, `t3-bridge-regeneration-wait-outlasts-stop.md`).

Commits, oldest first:
- `bcc89670` feat: remove positional parameters from skill snippets
- `f4a6a828` feat: reject positional parameters in Claude skill bodies
- `109ae3ce` feat: write the final T3 title without waiting for regeneration
- `13aa448a` feat: verify skill-args and T3 final-title acceptance criteria (plan file only)
- `94558559` feat: document skill-args and T3 final-title fixes, close backlog entries
- `bb4323d2` fix: address code review findings

## Evidence
`none`: the run facts record 0 validation runs, so this run produced no test output showing a failure before and a pass after.

The diff does encode the before/after behavior in tests. This is what the tests assert, not something this run executed:
- **T3 bridge, before:** the removed subtest of `TestT3BridgeWaitsForTitleRegeneration` asserted that a finished attempt with a pending regeneration had sent only `["thread.pin", "thread.unpin"]`. The mark-unread was held back by the final title, which was held back by the generated one.
- **T3 bridge, after:** `TestT3BridgeFinalTitleSkipsRegenerationWait` asserts `["thread.pin", "thread.metadata.update", "thread.unpin", "thread.mark-unread"]` and the title `[loopai: failed] New thread`, without advancing the fake clock. It covers three cases:
  - the finish arrives before any title write;
  - the finish arrives while a progress title is waiting (the progress title is never written);
  - the final title is retried after a lost dispatch, reusing the same command id.
- **Skills, before:** the snippets read `$2 ~ /^(orca|t3|cli)$/ && ok($3) …` and `grep -E "^[[:space:]]*$1[[:space:]]*="`.
- **Skills, after:** the snippets read `$(2) … ok($(3))` and `${key}`.
  - `check-launch-history-skills_test.sh` now asserts that no history snippet or `cfgval` helper matches `\$\{?[0-9]`.
  - It also runs `cfgval` against fixture configs and expects:
    - local `task_model` wins over global (`codex:gpt-6:high`);
    - `review_model` resolves from the global config (`claude:sonnet`);
    - a commented `external_reviewers` is empty;
    - commented and longer-prefix keys never match.
  - `check-symlinks_test.sh` expects a failure for `$1`, `$0`, `${2}`, and awk `print $3`, and a pass for `$(2)`, `${key}`, `$HOME`, `${CLAUDE_SKILL_DIR}`, and `$(git …)`.

## Risk
**low**

- **Public APIs:** no CLI flag, config key, or report heading changed. `skillContract` and the `TestSkillContract` golden file are untouched, which is correct because the fingerprinted surface did not change.
- **Data schemas:** the launch-history file format is unchanged. Only the readers' awk spelling changed (`$(N)` is POSIX awk), and the existing fixture runs still execute the snippets.
- **Configuration:** no config changes. The plugin version moves 0.9.3 → 0.9.4 in both manifests, as the skill-change rule requires.
- **Concurrency:** the bridge change adds no goroutine, lock, or timer. It reads the existing `b.final` field, which `apply` sets when it folds the finish event. Both orderings (finish before the first title, and finish during the wait) have tests.
- **Migrations:** none.
- **Behavior trade-off:** a Mode 1 run that ends within about 2 minutes of starting now cancels T3 Code's title generation. The plan accepts this explicitly.
- **Checker false positives:** the new `check-symlinks.sh` rule could reject harmless prose such as `$5`. It fails loudly at `make test`, so it cannot fail silently.
- **Untested against live T3 Code:** the bridge behavior is proven only against the `pkg/t3/t3test` fake. The live-T3 check is still listed under Post-Completion.

## Merge danger
**Door:** two-way
A plain revert restores the old snippets, the checker, and the wait. No persisted format, published flag, or config key changed. Installed plugin copies would only need another version bump to pick up a revert.

**Blast radius:** narrow
Three launch skills (six copies), one checker rule, and one condition in the Mode 1 bridge's title sync. The branch sits directly on local `master`, and a trial merge against `origin/master` is clean.

## Migrations and operational steps
- No database, configuration, or data migrations.
- **Deployment:** after merge, run `make install` from the checkout. This rebuilds the `loopai` binary with the bridge fix, updates the `loopai` Claude plugin to 0.9.4, and reinstalls the Codex skills. Then run `/reload-plugins` in open Claude Code sessions.
- **Recommended manual checks** (the plan's Post-Completion):
  - Invoke `/loopai:loopai-plan some words here` and `/loopai:loopai-t3 docs/plans/<plan>.md`, and confirm the rendered snippets keep `$(2)`/`$(3)` and recent flag combinations are offered.
  - Launch a Mode 1 plan that fails within its first minute from a new T3 thread, and confirm it ends with `[loopai: failed]` and the "Done" badge.

## Plan deviation
Plan drift facts: added none, blocked none, skipped none. All checkboxes in Tasks 1–5 are ticked.

Accounted differences:
- **Recorded change of scope (➕, Task 3):** the plan said to keep `TestT3BridgeWaitsForTitleRegeneration` passing unchanged. Its two in-progress subtests are unchanged. Its third subtest asserted the old wait for a finished run, so it was replaced by the new `TestT3BridgeFinalTitleSkipsRegenerationWait` cases. The plan file records this.
- **Additions from the internal-review fix commit `bb4323d2`, beyond the plan's wording:**
  - The plan required only that the history snippets carry no positional parameter. `check-launch-history-skills_test.sh` now also extracts `loopai-plan`'s `cfgval` from both hosts, asserts it has no positional parameters and documents its calling convention, and runs it against fixture configs.
  - A third bridge subtest covers a final title retried after an unknown outcome.
  - `CLAUDE.md` wording was widened, including the `make test-launch-history-skills` description.
  - A new backlog entry was filed (see Backlog).
- **Coverage:** Task 4 records `syncTitle` at 88.2% and `awaitRegeneration` at 100%. The plan reports these itself; the run facts do not record them.

## Backlog
- `docs/backlog/plugin-skills-positional-parameter-check.md`, "Further marketplace plugins' skills escape the positional-parameter check" (added, minor, from internal review): **filed, open.** `check-plugin.sh` never reads the `SKILL.md` bodies under `plugins/<name>/`. No plugin skill uses a positional parameter today.
- `docs/backlog/skill-positional-args-substituted.md` (deleted): **resolved** by Tasks 1–2.
- `docs/backlog/t3-bridge-regeneration-wait-outlasts-stop.md` (deleted): **resolved** by Task 3.

## External review
### codex:gpt-6-astra:xhigh
- label: codex
- iterations: 1
- duration_ms: 115184
- ended by: done
- had findings: false

There were no findings. In iteration 1 the reviewer returned `NO ISSUES FOUND`. The evaluator had nothing to evaluate, fix, or file. It confirmed a clean tree (`git diff HEAD` and `git status --porcelain` both empty), made no commit, and the branch stayed at `bb4323d2`.

## Validation
- Commands: none. The plan has no `## Validation Commands` section.
- Timings: duration_ms 0, runs 0.
- The plan's Task 4 checkboxes say `make test`, `make lint` (0 issues), and `GOOS=windows GOARCH=amd64 go build ./...` were run. Those runs are not in the run's validation facts, and this report does not confirm them.