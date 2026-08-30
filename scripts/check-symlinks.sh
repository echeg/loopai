#!/usr/bin/env bash

set -euo pipefail

repo_root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
claude_dir="$repo_root/assets/claude"
skills_dir="$claude_dir/skills"
status=0
expected_skills="$(printf '%s\n' loopai loopai-adopt loopai-brainstorm loopai-grill loopai-orca loopai-plan loopai-update | sort)"

fail() {
	printf '%s\n' "$*" >&2
	status=1
}

check_frontmatter() {
	local skill_file="$1"
	local skill_name="$2"

	awk -v expected_name="$skill_name" '
		function trim(value) {
			sub(/^[[:space:]]+/, "", value)
			sub(/[[:space:]]+$/, "", value)
			return value
		}
		function scalar(value) {
			value = trim(value)
			if ((value ~ /^".*"$/) || (value ~ /^\047.*\047$/)) {
				value = substr(value, 2, length(value) - 2)
			}
			return trim(value)
		}
		NR == 1 {
			if ($0 != "---") exit 1
			next
		}
		$0 == "---" {
			closed = 1
			exit
		}
		/^description:[[:space:]]*/ {
			value = $0
			sub(/^description:[[:space:]]*/, "", value)
			value = scalar(value)
			if (value != "" && value !~ /^#/ && value != "null" && value != "~") description = 1
		}
		/^name:[[:space:]]*/ {
			value = $0
			sub(/^name:[[:space:]]*/, "", value)
			name = scalar(value)
			name_seen = 1
		}
		END {
			exit !(closed && description && (!name_seen || name == expected_name))
		}
	' "$skill_file"
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

	if ! check_frontmatter "$skill_file" "$skill_name"; then
		fail "invalid skill frontmatter: $skill_file (description is required and name must match the directory)"
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

while IFS= read -r asset; do
	skill_name="$(basename "$asset" .md)"
	if ! grep -Fqx "$skill_name" <<<"$expected_skills"; then
		fail "orphan skill asset: $asset"
	fi
done < <(find "$claude_dir" -mindepth 1 -maxdepth 1 -name '*.md' -print | sort)

while IFS= read -r link; do
	fail "broken symlink: $link"
done < <(find -L "$claude_dir" -type l -print | sort)

exit "$status"
