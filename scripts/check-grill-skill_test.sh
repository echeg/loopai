#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
skill_file="$repo_root/assets/claude/skills/loopai-grill/SKILL.md"

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

assert_contains() {
	local description="$1"
	local expected="$2"

	grep -Fq -- "$expected" "$skill_file" || fail "$description"
}

[[ -f "$skill_file" ]] || fail "missing loopai-grill skill"

frontmatter="$(awk 'NR == 1 && $0 != "---" { exit 1 } NR > 1 && $0 == "---" { exit } NR > 1 { print }' "$skill_file")" ||
	fail "invalid frontmatter delimiters"

grep -Eq '^name: loopai-grill$' <<<"$frontmatter" || fail "frontmatter name must match the skill"
grep -Eq '^description: "[^"].*"$' <<<"$frontmatter" || fail "description must be non-empty and double quoted"
if ! awk '
	{
		value = $0
		sub(/^[^:]+:[[:space:]]*/, "", value)
		if (value ~ /^".*"$/ || value ~ /^\047.*\047$/ || value ~ /^\[.*\]$/) next
		if (index(value, ": ") || index(value, " #")) exit 1
	}
' <<<"$frontmatter"; then
	fail "frontmatter contains an unsafe unquoted ': ' or ' #' scalar"
fi

assert_contains "missing grill-mode path routing" 'treat the complete argument as a plan path and use grill mode'
assert_contains "missing no-plan handling" 'If no candidate exists, explain that no active plan was found and stop.'
assert_contains "missing case-sensitive path handling" 'Paths are case-sensitive: do not guess a differently cased path.'
assert_contains "missing empty compare handling" 'If it is empty or whitespace, stop and ask the user'
assert_contains "missing Codex preflight" 'check for it with `command -v codex`'
assert_contains "missing grill-mode Codex degradation" 'continue with the three Claude critics'
assert_contains "missing plan-off Codex requirement" 'Plan-off requires Codex.'
assert_contains "missing shared loopai-plan template" 'Both candidates must receive the same requirements and the same template.'
assert_contains "missing symmetric cross-judging" 'Have Claude and Codex each score both complete drafts'
assert_contains "missing format-consistent final output" 'Ensure the final result follows the loopai template'

printf 'loopai-grill acceptance tests passed\n'
