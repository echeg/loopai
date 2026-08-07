#!/usr/bin/env bash

set -euo pipefail

repo_root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
claude_dir="$repo_root/assets/claude"
skills_dir="$claude_dir/skills"
status=0
expected_skills="$(printf '%s\n' loopai loopai-adopt loopai-brainstorm loopai-plan loopai-update | sort)"

fail() {
	printf '%s\n' "$*" >&2
	status=1
}

if [[ ! -d "$skills_dir" ]]; then
	fail "missing skills directory: $skills_dir"
	exit "$status"
fi

actual_skills="$(find "$skills_dir" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; | sort)"
if [[ "$actual_skills" != "$expected_skills" ]]; then
	fail "unexpected skill inventory (expected: $(tr '\n' ' ' <<<"$expected_skills"))"
fi

while IFS= read -r skill_name; do
	skill_file="$skills_dir/$skill_name/SKILL.md"
	link="$claude_dir/$skill_name.md"
	expected_target="./skills/$skill_name/SKILL.md"

	if [[ ! -f "$skill_file" ]]; then
		fail "missing skill file: $skill_file"
		continue
	fi

	if [[ "$(sed -n '1p' "$skill_file")" != "---" ]] ||
		! awk 'NR > 1 && /^description:[[:space:]]*[^[:space:]]/ { description = 1 }
			NR > 1 && /^---$/ { closed = 1; exit }
			END { exit !(description && closed) }' "$skill_file"; then
		fail "invalid skill frontmatter: $skill_file (description is required)"
	fi

	if [[ ! -L "$link" ]]; then
		fail "missing skill symlink: $link"
		continue
	fi

	actual_target="$(readlink "$link")"
	if [[ "$actual_target" != "$expected_target" ]]; then
		fail "incorrect skill symlink: $link -> $actual_target (expected $expected_target)"
	fi

	if [[ ! -e "$link" ]]; then
		fail "broken skill symlink: $link -> $actual_target"
	fi

done <<<"$expected_skills"

while IFS= read -r link; do
	skill_name="$(basename "$link" .md)"
	if [[ ! -f "$skills_dir/$skill_name/SKILL.md" ]]; then
		fail "orphan skill symlink: $link"
	fi
done < <(find "$claude_dir" -mindepth 1 -maxdepth 1 -type l -name '*.md' -print | sort)

while IFS= read -r link; do
	fail "broken symlink: $link"
done < <(find -L "$claude_dir" -type l -print | sort)

exit "$status"
