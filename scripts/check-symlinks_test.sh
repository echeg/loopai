#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
checker="$script_dir/check-symlinks.sh"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

add_skill() {
	local name="$1"
	mkdir -p "$fixture/assets/claude/skills/$name"
	printf '%s\n' '---' 'description: fixture skill' '---' >"$fixture/assets/claude/skills/$name/SKILL.md"
	ln -s "./skills/$name/SKILL.md" "$fixture/assets/claude/$name.md"
}

expect_failure() {
	local expected="$1"
	local output
	if output="$($checker "$fixture" 2>&1)"; then
		printf 'expected check to fail with: %s\n' "$expected" >&2
		exit 1
	fi
	if [[ "$output" != *"$expected"* ]]; then
		printf 'expected failure containing %q, got: %s\n' "$expected" "$output" >&2
		exit 1
	fi
}

add_skill loopai
"$checker" "$fixture"

printf '%s\n' '# missing frontmatter' >"$fixture/assets/claude/skills/loopai/SKILL.md"
expect_failure "invalid skill frontmatter"
printf '%s\n' '---' 'description: fixture skill' '---' >"$fixture/assets/claude/skills/loopai/SKILL.md"

rm "$fixture/assets/claude/loopai.md"
expect_failure "missing skill symlink"
ln -s "./skills/other/SKILL.md" "$fixture/assets/claude/loopai.md"
expect_failure "incorrect skill symlink"

rm "$fixture/assets/claude/loopai.md"
ln -s "./skills/loopai/SKILL.md" "$fixture/assets/claude/loopai.md"
ln -s "./skills/loopai/SKILL.md" "$fixture/assets/claude/orphan.md"
expect_failure "orphan skill symlink"

printf 'check-symlinks tests passed\n'
