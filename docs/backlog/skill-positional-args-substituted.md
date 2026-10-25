# Skill shell snippets use $1/$2/$3, which Claude Code replaces with skill arguments

- found: 2026-10-25, plan: 20261025-t3-thread-done-badge, phase: planning
- severity: major
- area: assets/claude/skills/loopai-plan/SKILL.md

Claude Code substitutes positional skill arguments into the skill body (`$1`, `$2`, ... are
shorthands for `$ARGUMENTS[N]`). When a skill is invoked with arguments, every bare `$N` in its
shell snippets is rewritten before the model reads it. Invoking `/loopai:loopai-plan да давай
делать на любом.` turned the `cfgval` grep (`assets/claude/skills/loopai-plan/SKILL.md:323`,
`"^[[:space:]]*$1[[:space:]]*="`) into a grep for the word `давай`, and the launch-history awk
(`:345`, `:347`) into `делать ~ /^(orca|t3|cli)$/ && ok(на) ...`, which is not valid awk. Copied
verbatim, the snippets silently yield empty `FLAGS`, `HISTORY`, and `LAST_LAUNCHER`.

The launch-history snippet is shared byte-identically with `loopai-orca` and `loopai-t3`
(`scripts/check-launch-history-skills_test.sh`), and both of those skills are normally invoked
with arguments (a plan path and flags), so they are affected too. Codex skills do not substitute
arguments and are not affected, but the snippet must stay identical across all six.

Fix direction: spell the fields so no `$<digit>` sequence appears in a skill body — `${1}` in the
shell function, `$(2)`/`$(3)` or named variables (`launcher = $ 2`, or `-v` plus `split`) in awk —
update the shared snippet in all six skills together, and add a check to `check-symlinks.sh` /
`check-codex-skills.sh` rejecting `\$[0-9]` in Claude skill bodies.
