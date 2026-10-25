# Further marketplace plugins' skills escape the positional-parameter check

- found: 2026-10-25, plan: 20261025-skill-args-and-t3-final-title, phase: internal review
- severity: minor
- area: scripts/check-plugin.sh

`check-symlinks.sh` (`scripts/check-symlinks.sh:125-131`) rejects any line matching
`\$\{?[0-9]` in a Claude `SKILL.md`, because Claude Code substitutes the words of a skill's
arguments into `$0`, `$1`, ... in the skill body. It walks `assets/claude/skills/` only, by
design: further marketplace plugins live under `plugins/<name>/` so loopai's skill inventory
checks never see them. Their skills are Claude skills too and get the same substitution, but
`check-plugin.sh` validates manifests only (`scripts/check-plugin.sh:46-69`) and never reads a
skill body, so a `$1` added to `plugins/codex-imagegen/skills/generating-images-with-codex/SKILL.md`
would pass `make test` and break whenever that skill is invoked with arguments. No plugin skill
uses one today.

Suggested direction: in `check-plugin.sh`'s per-plugin loop, apply the same
`grep -nE '\$\{?[0-9]'` rule to every `plugins/<name>/<skills>/*/SKILL.md`, failing with
`positional parameter in skill body: <file>:<line>`; add one failing and one passing fixture to
`scripts/check-plugin_test.sh`, and widen the `CLAUDE.md` sentence that says `check-symlinks.sh`
alone carries the rule.
