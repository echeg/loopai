#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
checker="$script_dir/check-codex-skills.sh"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

claude_skills="$fixture/assets/claude/skills"
codex_skills="$fixture/assets/codex/skills"

add_claude_skill() {
	local name="$1"
	mkdir -p "$claude_skills/$name"
	printf '%s\n' '---' "name: $name" 'description: fixture skill' '---' >"$claude_skills/$name/SKILL.md"
}

write_codex_skill() {
	local name="$1"
	mkdir -p "$codex_skills/$name/agents"
	printf '%s\n' '---' "name: $name" 'description: fixture skill' '---' >"$codex_skills/$name/SKILL.md"
	printf '%s\n' 'interface:' \
		"  display_name: \"$name\"" \
		'  short_description: "fixture"' \
		'  default_prompt: "Use it."' >"$codex_skills/$name/agents/openai.yaml"
}

add_pair() {
	add_claude_skill "$1"
	write_codex_skill "$1"
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

add_pair loopai
add_pair loopai-adopt
add_pair loopai-brainstorm
add_pair loopai-orca
add_pair loopai-plan
add_pair loopai-update
# exempt: present on the claude side only
add_claude_skill loopai-grill
"$checker" "$fixture"

# the codex tree must exist at all
mv "$fixture/assets/codex" "$fixture/assets/codex-away"
expect_failure "missing codex skills directory"
mv "$fixture/assets/codex-away" "$fixture/assets/codex"

# inventory must track the claude side in both directions
rm -rf "$codex_skills/loopai-plan"
expect_failure "unexpected codex skill inventory"
write_codex_skill loopai-plan

write_codex_skill loopai-extra
expect_failure "unexpected codex skill inventory"
rm -rf "$codex_skills/loopai-extra"

add_claude_skill loopai-fresh
expect_failure "unexpected codex skill inventory"
rm -rf "$claude_skills/loopai-fresh"

# an exempt skill must stay out of the codex tree
write_codex_skill loopai-grill
expect_failure "unexpected codex skill inventory"
rm -rf "$codex_skills/loopai-grill"

rm -f "$codex_skills/loopai/SKILL.md"
expect_failure "missing codex skill file"
write_codex_skill loopai

# frontmatter: name is required here even though the claude checker allows it to be absent
printf '%s\n' '# no frontmatter' >"$codex_skills/loopai/SKILL.md"
expect_failure "invalid codex skill frontmatter"
printf '%s\n' '---' 'description: fixture skill' '---' >"$codex_skills/loopai/SKILL.md"
expect_failure "invalid codex skill frontmatter"
printf '%s\n' '---' 'name: other' 'description: fixture skill' '---' >"$codex_skills/loopai/SKILL.md"
expect_failure "invalid codex skill frontmatter"
printf '%s\n' '---' 'name: loopai' 'description: ""' '---' >"$codex_skills/loopai/SKILL.md"
expect_failure "invalid codex skill frontmatter"
write_codex_skill loopai

# the openai interface file is what gives codex the UI entry
rm -f "$codex_skills/loopai/agents/openai.yaml"
expect_failure "missing codex skill interface"
printf '%s\n' 'interface:' '  display_name: "loopai"' '  short_description: "fixture"' \
	>"$codex_skills/loopai/agents/openai.yaml"
expect_failure "invalid codex skill interface"
printf '%s\n' 'interface:' '  display_name: "loopai"' '  short_description: "fixture"' \
	'  default_prompt: ""' >"$codex_skills/loopai/agents/openai.yaml"
expect_failure "invalid codex skill interface"
write_codex_skill loopai

# every claude-only construct must be caught, since each marks an unadapted copy
for token in AskUserQuestion TaskOutput TodoWrite subagent_type spawn_agent \
	CLAUDE_SKILL_DIR allowed-tools run_in_background "Task tool" "/loopai:"; do
	write_codex_skill loopai-plan
	printf '%s\n' "use $token here" >>"$codex_skills/loopai-plan/SKILL.md"
	expect_failure "claude-only construct in codex skill"
done
write_codex_skill loopai-plan

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
	write_codex_skill loopai-orca
	printf '%s\n' "$spelling" >>"$codex_skills/loopai-orca/SKILL.md"
	expect_failure "removed loopai flag or key in codex skill"
done
# the surviving codex flag, the per-phase spec grammar, and a migration hint stay allowed
write_codex_skill loopai-orca
printf '%s\n' "loopai --codex-args='-c x=1' --task-model codex:gpt-6-astra:medium" 'the codex executor' \
	'`--codex` was removed; write `--task-model codex:<model>`' \
	>>"$codex_skills/loopai-orca/SKILL.md"
"$checker" "$fixture"
write_codex_skill loopai-orca

# a symlink would reintroduce the coupling the split exists to avoid
ln -s "../../claude/skills/loopai/SKILL.md" "$fixture/assets/codex/skills/loopai-orca/CLAUDE.md"
expect_failure "symlink in codex skill tree"
rm -f "$fixture/assets/codex/skills/loopai-orca/CLAUDE.md"

"$checker" "$fixture"
printf 'check-codex-skills tests passed\n'
