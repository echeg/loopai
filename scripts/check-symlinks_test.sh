#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
checker="$script_dir/check-symlinks.sh"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

add_skill() {
	local name="$1"
	mkdir -p "$fixture/assets/claude/skills/$name"
	printf '%s\n' '---' "name: $name" 'description: fixture skill' '---' >"$fixture/assets/claude/skills/$name/SKILL.md"
	ln -s "./skills/$name/SKILL.md" "$fixture/assets/claude/$name.md"
}

expect_failure() {
	local expected="$1"
	local output
	if output="$("$checker" "$fixture" 2>&1)"; then
		printf 'expected check to fail with: %s\n' "$expected" >&2
		exit 1
	fi
	if [[ "$output" != *"$expected"* ]]; then
		printf 'expected failure containing %q, got: %s\n' "$expected" "$output" >&2
		exit 1
	fi
}

add_skill loopai
add_skill loopai-adopt
add_skill loopai-brainstorm
add_skill loopai-grill
add_skill loopai-merge
add_skill loopai-orca
add_skill loopai-plan
add_skill loopai-update
"$checker" "$fixture"

rm "$fixture/assets/claude/skills/loopai-plan/SKILL.md" "$fixture/assets/claude/loopai-plan.md"
expect_failure "missing skill file"
add_skill loopai-plan

printf '%s\n' '# missing frontmatter' >"$fixture/assets/claude/skills/loopai/SKILL.md"
expect_failure "invalid skill frontmatter"
printf '%s\n' '---' 'name: loopai' 'description: fixture skill' '---' >"$fixture/assets/claude/skills/loopai/SKILL.md"

printf '%s\n' '---' 'name: loopai' 'description: ""' '---' >"$fixture/assets/claude/skills/loopai/SKILL.md"
expect_failure "invalid skill frontmatter"
printf '%s\n' '---' 'name: loopai' 'description: # missing value' '---' >"$fixture/assets/claude/skills/loopai/SKILL.md"
expect_failure "invalid skill frontmatter"
printf '%s\n' '---' 'name: other' 'description: fixture skill' '---' >"$fixture/assets/claude/skills/loopai/SKILL.md"
expect_failure "invalid skill frontmatter"
printf '%s\n' '---' 'name: loopai' 'description: fixture skill' '---' >"$fixture/assets/claude/skills/loopai/SKILL.md"

rm "$fixture/assets/claude/loopai.md"
expect_failure "missing skill symlink"
ln -s "./skills/other/SKILL.md" "$fixture/assets/claude/loopai.md"
expect_failure "incorrect skill symlink"

rm "$fixture/assets/claude/loopai.md"
ln -s "./skills/loopai/SKILL.md" "$fixture/assets/claude/loopai.md"
ln -s "./skills/loopai/SKILL.md" "$fixture/assets/claude/orphan.md"
expect_failure "orphan skill asset"
rm "$fixture/assets/claude/orphan.md"

printf '%s\n' 'stale standalone command' >"$fixture/assets/claude/loopai-old.md"
expect_failure "orphan skill asset"
rm "$fixture/assets/claude/loopai-old.md"

rm -rf "$fixture/assets/claude/skills/loopai-brainstorm" "$fixture/assets/claude/loopai-brainstorm.md"
expect_failure "unexpected skill inventory"
add_skill loopai-brainstorm

add_skill loopai-extra
expect_failure "unexpected skill inventory"
rm -rf "$fixture/assets/claude/skills/loopai-extra" "$fixture/assets/claude/loopai-extra.md"

# a skill teaching a removed loopai flag or key would stop loopai at startup
for spelling in 'loopai --codex docs/plans/x.md' \
	'loopai --codex' \
	'loopai --codex-only' \
	'--external-review-tool codex' \
	'--external-review-model opus' \
	'external_review_tool = auto' \
	'external_review_model = opus' \
	'codex_model = gpt-5.5' \
	'codex_reasoning_effort = xhigh' \
	'set `executor` to codex' \
	'executor = codex'; do
	printf '%s\n' "$spelling" >>"$fixture/assets/claude/skills/loopai-orca/SKILL.md"
	expect_failure "removed loopai flag or key in skill"
	printf '%s\n' '---' 'name: loopai-orca' 'description: fixture skill' '---' >"$fixture/assets/claude/skills/loopai-orca/SKILL.md"
done
# the surviving codex flag, the per-phase spec grammar, and a migration hint stay allowed
printf '%s\n' "loopai --codex-args='-c x=1' --task-model codex:gpt-6-astra:medium" 'the codex executor' \
	'`--codex` was removed; write `--task-model codex:<model>`' \
	>>"$fixture/assets/claude/skills/loopai-orca/SKILL.md"
"$checker" "$fixture"
printf '%s\n' '---' 'name: loopai-orca' 'description: fixture skill' '---' >"$fixture/assets/claude/skills/loopai-orca/SKILL.md"

mkdir -p "$fixture/assets/claude/nested"
ln -s ./missing.asset "$fixture/assets/claude/nested/broken.asset"
expect_failure "broken symlink"

printf 'check-symlinks tests passed\n'
