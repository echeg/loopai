#!/usr/bin/env bash

set -euo pipefail

repo_root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
claude_dir="$repo_root/assets/claude"
skills_dir="$claude_dir/skills"
status=0

fail() {
	printf '%s\n' "$*" >&2
	status=1
}

check_loopai_plan_contract() {
	local skill_file="$1"
	local overview_line decisions_line context_line

	overview_line="$(grep -n -m1 '^## Overview$' "$skill_file" | cut -d: -f1 || true)"
	decisions_line="$(grep -n -m1 '^## Decisions$' "$skill_file" | cut -d: -f1 || true)"
	context_line="$(grep -n -m1 '^## Context (from discovery)$' "$skill_file" | cut -d: -f1 || true)"

	if [[ -z "$overview_line" || -z "$decisions_line" || -z "$context_line" ]] ||
		(( decisions_line <= overview_line || decisions_line >= context_line )); then
		fail "invalid loopai-plan template: Decisions must appear between Overview and Context"
	fi

	for required_text in \
		'**Context**:' \
		'**Chosen approach**:' \
		'**Rejected alternatives**:' \
		'**Verified facts**:'; do
		if ! grep -Fq -- "$required_text" "$skill_file"; then
			fail "invalid loopai-plan template: missing Decisions field $required_text"
		fi
	done

	if ! grep -Fq 'When the request comes from `loopai-brainstorm`, skip every question already answered' "$skill_file"; then
		fail "invalid loopai-plan flow: missing brainstorm question-skip guidance"
	fi
}

if [[ ! -d "$skills_dir" ]]; then
	fail "missing skills directory: $skills_dir"
	exit "$status"
fi

while IFS= read -r skill_file; do
	skill_name="$(basename "$(dirname "$skill_file")")"
	link="$claude_dir/$skill_name.md"
	expected_target="./skills/$skill_name/SKILL.md"

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

	if [[ "$skill_name" == "loopai-plan" ]]; then
		check_loopai_plan_contract "$skill_file"
	fi
done < <(find "$skills_dir" -mindepth 2 -maxdepth 2 -type f -name SKILL.md -print | sort)

while IFS= read -r link; do
	skill_name="$(basename "$link" .md)"
	if [[ ! -f "$skills_dir/$skill_name/SKILL.md" ]]; then
		fail "orphan skill symlink: $link"
	fi
done < <(find "$claude_dir" -mindepth 1 -maxdepth 1 -type l -name '*.md' -print | sort)

exit "$status"
