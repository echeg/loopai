#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
skill_file="$repo_root/assets/claude/skills/loopai-grill/SKILL.md"
symlink_checker="$repo_root/scripts/check-symlinks.sh"

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

"$symlink_checker" "$repo_root" >/dev/null || fail "shared skill/frontmatter validation failed"
grep -Fqx "name: loopai-grill" "$skill_file" || fail "frontmatter name must match the skill"
grep -Fqx "argument-hint: 'plan file or: compare <description>'" "$skill_file" ||
	fail "argument hint must document grill and compare inputs"
grep -Fqx 'allowed-tools: [Bash, Read, Write, Edit, Glob, Grep, Agent, AskUserQuestion]' "$skill_file" ||
	fail "allowed tools must support routing, critics, prompts, and selected edits"

assert_contains "missing grill-mode path routing" 'treat the complete argument as a plan path and use grill mode'
assert_contains "missing no-plan handling" 'If no valid candidate exists, explain that no active plan was found and stop.'
assert_contains "missing case-sensitive path handling" "preserves the path's real case"
assert_contains "missing symlink rejection" 'reject symbolic links with'
assert_contains "missing canonical path containment" 'canonical parent is exactly that plans directory'
assert_contains "missing canonical plans-root containment" 'canonical plans directory is inside the canonical repository root'
assert_contains "missing empty compare handling" 'If it is empty or whitespace, stop and ask the user'
assert_contains "missing Codex preflight" 'command -v codex'
assert_contains "missing Codex sandbox" 'codex exec --sandbox read-only --ephemeral -C <canonical-repository-root>'
assert_contains "missing delegated .loopai protection" 'Repeat in every Agent and Codex prompt that it must not read or modify'
assert_contains "missing grill-mode Codex degradation" 'continue with the three Claude critics'
assert_contains "missing zero-findings handling" 'If no verified findings survive, skip AskUserQuestion'
assert_contains "missing plan-off Codex requirement" 'Plan-off requires Codex.'
assert_contains "missing shared loopai-plan template" 'Both candidates must receive the same requirements and the same template.'
assert_contains "missing plugin-root template lookup" 'CLAUDE_PLUGIN_ROOT'
assert_contains "missing symmetric cross-judging" 'Have Claude and Codex each score both complete drafts'
assert_contains "missing blind Claude judge" 'Launch the Claude judgment in a fresh Agent'
assert_contains "missing judge validation" 'Validate each judgment before calculating totals'
assert_contains "missing completed-plan collision check" 'docs/plans/completed/'
assert_contains "missing format-consistent final output" 'Ensure the final result follows the loopai template'

printf 'loopai-grill acceptance tests passed\n'
