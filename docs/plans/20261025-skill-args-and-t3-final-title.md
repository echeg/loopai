# Fix skill positional-argument substitution and the T3 final-title wait

## Overview
- Fix the two backlog entries filed on 2026-10-25 and remove them once fixed:
  - `docs/backlog/skill-positional-args-substituted.md` (major): Claude Code replaces `$0`, `$1`, `$2`, ...
    in a skill body with the words of the skill's arguments, so the config and launch-history shell
    snippets in `loopai-plan`, `loopai-orca`, and `loopai-t3` break whenever those skills are invoked
    with arguments. `loopai-orca` and `loopai-t3` are normally invoked with a plan path and flags, so
    their history offer silently never works.
  - `docs/backlog/t3-bridge-regeneration-wait-outlasts-stop.md` (minor): a Mode 1 run that finishes
    while T3 Code is still generating the thread title waits up to 2 minutes before its first title
    write, but the before-exit stop gives the bridge 10 s, so the thread gets neither the final
    `[loopai: <state>]` title nor the mark-unread "Done" badge.
- Result: the skill snippets work identically with and without arguments, a check keeps `$<digit>`
  out of Claude skill bodies for good, and a finished attempt writes its final title at once.

## Decisions
- **Context**: both entries are small, well-understood defects found on 2026-10-25; the older backlog
  entries are larger and stay out of this plan.
- **Chosen approach (skills)**: rewrite the snippets so no `$` is followed by a digit or by `{` and
  a digit: the `cfgval` helper takes its key through a named shell variable instead of a positional
  parameter, and awk addresses fields as `$(2)`/`$(3)`. Change the Codex copies the same way, because
  `scripts/check-launch-history-skills_test.sh` requires the history snippet to be byte-identical across
  all six skills. Add a rule to `scripts/check-symlinks.sh` rejecting `\$\{?[0-9]` in a Claude
  `SKILL.md` body, with regression cases in `scripts/check-symlinks_test.sh`.
- **Chosen approach (T3)**: once the attempt's `finish` event has arrived (`b.final`), `syncTitle`
  skips the title-regeneration wait and writes the final title immediately, followed by the unpin and
  the mark-unread as today. Writing the title cancels T3 Code's pending generation; for a run that ends
  within about 2 minutes of its start this is accepted.
- **Rejected alternatives**:
  - escaping instead of rewriting (for example `\$1`): Claude Code documents no escape for skill
    argument substitution, so it cannot be relied on.
  - letting `t3Bridge.stop` run a bounded `closeOut` with a fresh context when its timeout fires: by
    then `run()` has closed the client in its defers, so it needs a reconnect outside the bridge loop
    and stretches process exit past `t3BridgeStopTimeout`; more code and more risk for the same fix.
- **Verified facts**:
  - Invoking `/loopai:loopai-plan` with the arguments `да давай делать на любом.` rendered
    `$0`→`да`, `$1`→`давай`, `$2`→`делать`, `$3`→`на` in the skill body, so `$0` is substituted
    too and indexing starts at 0. Invoked without arguments the snippets were left intact.
  - Affected lines (all in `SKILL.md` bodies): `assets/claude/skills/loopai-plan/SKILL.md:323,345,347`,
    `assets/claude/skills/loopai-orca/SKILL.md:84`, `assets/claude/skills/loopai-t3/SKILL.md:78`, and the
    Codex copies `assets/codex/skills/loopai-plan/SKILL.md:197,220,222`,
    `assets/codex/skills/loopai-orca/SKILL.md:84`, `assets/codex/skills/loopai-t3/SKILL.md:78`.
  - Bundled scripts are not substituted (`assets/claude/skills/loopai-grill/scripts/*.sh` use `$1` and
    work), so the rule applies to `SKILL.md` bodies only.
  - Whether Claude Code also substitutes `${1}` is undocumented; forbidding it too costs nothing.
  - `cmd/loopai/t3_bridge.go`: `syncTitle` gates the wait on `!b.titled && th.TitleRegeneration != nil
    && b.awaitRegeneration()`; `regenWait` is 2 minutes (`defaultT3BridgeTiming`);
    `t3BridgeStopTimeout` is 10 s; `stop` cancels without `closeOut`.

## Context (from discovery)
- Files/components involved:
  - skills: `assets/claude/skills/{loopai-plan,loopai-orca,loopai-t3}/SKILL.md` and
    `assets/codex/skills/{loopai-plan,loopai-orca,loopai-t3}/SKILL.md`
  - checks: `scripts/check-symlinks.sh` (body checks beside `removed_spellings`, around line 119),
    `scripts/check-symlinks_test.sh` (`expect_failure`, `write_skill`), and
    `scripts/check-launch-history-skills_test.sh`, which executes the history snippet against fixtures
  - bridge: `cmd/loopai/t3_bridge.go` (`syncTitle`, `awaitRegeneration`), `cmd/loopai/t3_bridge_test.go`
    (`TestT3BridgeWaitsForTitleRegeneration`, `newBridgeHarness`, fake clock)
  - docs: `CLAUDE.md` (the `check-symlinks` paragraph and the Mode 1 paragraph), `docs/t3-code.md`
    ("What the thread shows"), `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json`
- Related patterns found: body rules in `check-symlinks.sh` loop over every skill file and call `fail`;
  `check-codex-skills.sh` mirrors some of them but need not get this rule, because Codex does not
  substitute arguments. Bridge tests drive a fake T3 server (`pkg/t3/t3test`) and a fake clock.
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
- **Shell suites**: the skill and checker changes are covered by `make test-launch-history-skills`,
  `make check-symlinks`, `make test-symlinks`, and `make check-codex-skills`
- **E2E tests**: the dashboard Playwright suite is unaffected

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

### Task 1: Remove positional parameters from the skill snippets
- [x] rewrite the launch-history snippet in all six skills (`loopai-plan`, `loopai-orca`, `loopai-t3`,
      Claude and Codex) so awk reads fields as `$(2)` and `$(3)` instead of `$2` and `$3`, keeping the
      six copies byte-identical and the output unchanged
- [x] rewrite `cfgval` in `assets/claude/skills/loopai-plan/SKILL.md` and its Codex counterpart so it
      reads the key from a named variable (for example `key=task_model; cfgval`) instead of `$1`, and
      update the surrounding instructions that describe calling it
- [x] confirm `grep -nE '\$\{?[0-9]' assets/claude/skills/*/SKILL.md assets/codex/skills/*/SKILL.md`
      prints nothing
- [x] extend `scripts/check-launch-history-skills_test.sh` so each extracted history snippet must not
      contain `$` followed by a digit or by `{` and a digit (the fixture executions already prove the
      output is unchanged)
- [x] run `make test-launch-history-skills check-codex-skills </dev/null` - must pass before task 2

### Task 2: Reject positional parameters in Claude skill bodies
- [x] in `scripts/check-symlinks.sh`, beside the `removed_spellings` loop, fail with
      `positional parameter in skill body: <file>:<line>` for any line of a Claude `SKILL.md` matching
      `\$\{?[0-9]`, with a comment saying Claude Code substitutes skill arguments into `$N`
- [x] write regression cases in `scripts/check-symlinks_test.sh`: `$1`, `$0`, `${2}`, and an awk
      `print $3` line each fail; `$(2)`, `${key}`, `$HOME`, and `${CLAUDE_SKILL_DIR}` pass
- [x] run `make check-symlinks test-symlinks </dev/null` - must pass before task 3

### Task 3: Write the final T3 title without waiting for regeneration
- [x] in `cmd/loopai/t3_bridge.go` `syncTitle`, skip the `awaitRegeneration` wait when `b.final` is set,
      and update the comments on `syncTitle` and `awaitRegeneration` to say a finished attempt never waits
- [x] write a test in `cmd/loopai/t3_bridge_test.go`: a thread with a pending title regeneration and an
      attempt that finishes before any title write ends with the final `[loopai: failed]` title, then
      `thread.unpin`, then `thread.mark-unread`, without advancing the fake clock
- [x] write a test where the first progress title is waiting for the regeneration and the finish event
      arrives during the wait: the final title is written at once, the intermediate title never is
- [x] keep `TestT3BridgeWaitsForTitleRegeneration` passing unchanged for a run still in progress
      (its two in-progress subtests are unchanged; ➕ its third subtest asserted the old wait for a
      finished run and was replaced by the two new cases in `TestT3BridgeFinalTitleSkipsRegenerationWait`)
- [x] run `go test ./cmd/loopai -run 'T3'` - must pass before task 4

### Task 4: Verify acceptance criteria
- [x] verify both backlog problems are fixed as described in the Overview (no `$<digit>` in any SKILL.md; `syncTitle` gates the wait on `!b.final`)
- [x] run `make test </dev/null` (full suite)
- [x] run `make lint` - all issues must be fixed (0 issues)
- [x] run `GOOS=windows GOARCH=amd64 go build ./...`
- [x] verify test coverage of the changed bridge code meets the project standard (80%+) (`syncTitle` 88.2%, `awaitRegeneration` 100%; the one uncovered block is the pre-existing projection-error path)

### Task 5: [Final] Update documentation and close the backlog entries
- [ ] `CLAUDE.md`: add the positional-parameter rule to the paragraph describing what
      `make check-symlinks` rejects, and say in the Mode 1 paragraph that a finished attempt skips the
      title-regeneration wait
- [ ] `docs/t3-code.md`: in "What the thread shows", say the ~2 minute wait does not apply once the run
      has finished, so a short run's final title cancels T3 Code's generation
- [ ] bump the version in `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` to the same
      next patch value, since skill files changed
- [ ] delete `docs/backlog/skill-positional-args-substituted.md` and
      `docs/backlog/t3-bridge-regeneration-wait-outlasts-stop.md`, staged with `git rm` in the task commit

*Note: loopai automatically moves completed plans to `docs/plans/completed/`*

## Technical Details
- Forbidden pattern in Claude `SKILL.md` bodies: `\$\{?[0-9]` (covers `$0`, `$1`, `${2}`). Allowed:
  `$(2)` in awk, `${key}`, `$HOME`, `${CLAUDE_SKILL_DIR}`, `$(command)`.
- awk field syntax: `$(2)` is POSIX awk and works in BSD awk (macOS) and gawk; `$2 ~ /re/` becomes
  `$(2) ~ /re/`, `ok($3)` becomes `ok($(3))`, `dup[$3]` becomes `dup[$(3)]`, `print $3` becomes
  `print $(3)`.
- Bridge condition after the change:
  `if !b.final && !b.titled && th.TitleRegeneration != nil && b.awaitRegeneration() { return }`.
  `ended()` and `syncUnread` are unchanged; the existing order title → unpin → mark-unread holds.

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- After `make install` and `/reload-plugins`, invoke `/loopai:loopai-plan some words here` and
  `/loopai:loopai-t3 docs/plans/<plan>.md` and confirm the rendered snippets keep `$(2)`/`$(3)` and that
  recent flag combinations are offered.
- Launch a Mode 1 plan that fails in its first minute from a freshly created T3 thread and confirm the
  thread ends with `[loopai: failed]` and the green "Done" badge.
