#!/usr/bin/env bash

# Validates the Codex skill tree under assets/codex/skills.
#
# The Codex skills are deliberately NOT generated from assets/claude/skills: the
# bodies differ because the host tool contract differs. Claude Code offers
# AskUserQuestion, Task subagents, and named Read/Write/Glob tools; Codex offers
# none of them, and spawn_agent exists in loopai's own codex invocations only
# because pkg/executor/codex.go registers the agent through -c overrides, which
# an interactive Codex session does not do. A mechanical copy would therefore
# produce a skill that stalls at its first interactive step.
#
# What is enforced instead:
#   - the Codex inventory equals the Claude inventory minus an explicit exemption
#     list, so a skill added on one side cannot silently exist on one host only;
#   - every skill carries a name matching its directory and a real description
#     (Codex requires name; the Claude checker treats it as optional);
#   - every skill carries agents/openai.yaml, which is what gives Codex the UI
#     entry and default prompt;
#   - no body reintroduces a Claude-only tool through copy-paste.
#   - no body names a loopai flag or config key that was removed.

set -euo pipefail

repo_root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
claude_skills_dir="$repo_root/assets/claude/skills"
codex_dir="$repo_root/assets/codex"
codex_skills_dir="$codex_dir/skills"
status=0

# loopai-grill is Claude-only on purpose. Its safety model is the read-only
# Claude tool pin plus scripts addressed through ${CLAUDE_SKILL_DIR}; Codex has
# no equivalent way to restrict a session to read-only repository tools, and a
# half-ported grill is worse than an absent one.
exempt_skills=(loopai-grill)
exempt_list="${exempt_skills[*]}"

# Claude-only constructs. Each would be silently inert or misleading in a Codex
# session, and every one of them appears in the matching Claude skill, so this
# list is what catches an unadapted copy-paste.
forbidden_tokens=(
	AskUserQuestion
	TaskOutput
	TodoWrite
	subagent_type
	spawn_agent
	CLAUDE_SKILL_DIR
	allowed-tools
	run_in_background
	'Task tool'
	'/loopai:'
)

# Spellings loopai removed when the provider moved into every model spec
# (provider[:model[:effort]]). A skill still teaching one would make loopai stop at
# startup, and $loopai-orca splices its flags into a command a new Orca tab runs,
# so the failure would surface only after the worktree and tab already exist.
# Extended regular expressions; the bare --codex pattern leaves --codex-args alone.
# A line that itself says the spelling was removed is a migration hint, not a use.
removed_spellings=(
	'--codex([^-[:alnum:]_]|$)'
	'--codex-only'
	'--external-review-(tool|model)'
	'external_review_(tool|model)'
	'codex_model'
	'codex_reasoning_effort'
	'`executor`'
	'executor[[:space:]]*='
)

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
		}
		END {
			exit !(closed && description && name == expected_name)
		}
	' "$skill_file"
}

check_interface() {
	local interface_file="$1"
	local key

	for key in display_name short_description default_prompt; do
		if ! grep -Eq "^[[:space:]]+$key:[[:space:]]*[\"']?[^\"'[:space:]]" "$interface_file"; then
			return 1
		fi
	done
	return 0
}

if [[ ! -d "$claude_skills_dir" ]]; then
	fail "missing claude skills directory: $claude_skills_dir"
	exit "$status"
fi

if [[ ! -d "$codex_skills_dir" ]]; then
	fail "missing codex skills directory: $codex_skills_dir"
	exit "$status"
fi

claude_skills="$(find "$claude_skills_dir" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; | sort)"
expected_skills="$(grep -Fxv -f <(printf '%s\n' "${exempt_skills[@]}") <<<"$claude_skills" || true)"
actual_skills="$(find "$codex_skills_dir" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; | sort)"

if [[ "$actual_skills" != "$expected_skills" ]]; then
	fail "unexpected codex skill inventory (expected: $(tr '\n' ' ' <<<"$expected_skills"))"
	fail "  the codex tree must mirror assets/claude/skills minus: $exempt_list"
fi

while IFS= read -r skill_name; do
	[[ -n "$skill_name" ]] || continue
	skill_file="$codex_skills_dir/$skill_name/SKILL.md"
	interface_file="$codex_skills_dir/$skill_name/agents/openai.yaml"

	if [[ ! -f "$skill_file" ]]; then
		fail "missing codex skill file: $skill_file"
		continue
	fi

	if ! check_frontmatter "$skill_file" "$skill_name"; then
		fail "invalid codex skill frontmatter: $skill_file (name must equal the directory and description is required)"
	fi

	if [[ ! -f "$interface_file" ]]; then
		fail "missing codex skill interface: $interface_file"
	elif ! check_interface "$interface_file"; then
		fail "invalid codex skill interface: $interface_file (display_name, short_description and default_prompt are required)"
	fi

	for token in "${forbidden_tokens[@]}"; do
		if grep -Fq -- "$token" "$skill_file"; then
			fail "claude-only construct in codex skill: $skill_file contains '$token'"
		fi
	done

	for spelling in "${removed_spellings[@]}"; do
		if grep -E -- "$spelling" "$skill_file" | grep -Fv removed >/dev/null; then
			fail "removed loopai flag or key in codex skill: $skill_file matches '$spelling'"
		fi
	done
done <<<"$expected_skills"

for exempt in "${exempt_skills[@]}"; do
	if [[ -e "$codex_skills_dir/$exempt" ]]; then
		fail "exempt skill present in codex tree: $codex_skills_dir/$exempt"
	fi
done

# A symlink into the Claude tree would reintroduce exactly the coupling this
# split exists to avoid, so the codex tree must hold real files only.
while IFS= read -r link; do
	fail "symlink in codex skill tree: $link"
done < <(find "$codex_dir" -type l -print | sort)

exit "$status"
