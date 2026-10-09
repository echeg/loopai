# shellcheck shell=bash
# Shared skill contract helpers, sourced by check-symlinks.sh and check-codex-skills.sh.
#
# Every Claude and Codex skill opens with the same fenced snippet asking the installed
# binary for its contract number (`loopai --skill-contract`) and comparing it with the
# number the skill was written for. The canonical text lives here only, so the two
# checkers cannot drift apart; only its `expected=` line changes on a bump, and it must
# equal the skillContract constant in cmd/loopai/skill_contract.go.

# skill_contract_value prints the skillContract constant of the given repository root,
# or fails when the declaration cannot be found.
skill_contract_value() {
	local source_file="$1/cmd/loopai/skill_contract.go"
	local value

	value="$(sed -nE 's/^const skillContract = ([0-9]+)$/\1/p' "$source_file" 2>/dev/null)" || true
	if [[ -z "$value" || "$value" == *$'\n'* ]]; then
		printf 'cannot read skillContract from %s\n' "$source_file" >&2
		return 1
	fi
	printf '%s\n' "$value"
}

# skill_contract_snippet prints the canonical fenced snippet carrying expected=<N>.
skill_contract_snippet() {
	local contract="$1"

	printf '```bash\nexpected=%s\n' "$contract"
	cat <<'SNIPPET'
if ! command -v loopai >/dev/null 2>&1; then
  actual=missing
else
  actual=$(loopai --skill-contract 2>/dev/null) || actual=0
  case $actual in ''|*[!0-9]*) actual=0 ;; esac
fi
echo "skill contract: skill=$expected binary=$actual"
```
SNIPPET
}

# check_skill_contract_snippet validates one skill file against the canonical snippet.
# Status 0: present and current; 1: missing or modified; 2: present with a stale
# expected= value, which it prints on stdout.
check_skill_contract_snippet() {
	local skill_file="$1"
	local contract="$2"
	local snippet content normalized

	snippet="$(skill_contract_snippet "$contract")"
	content="$(<"$skill_file")"
	if [[ "$content" == *"$snippet"* ]]; then
		return 0
	fi

	normalized="$(sed -E "s/^expected=[0-9]+\$/expected=$contract/" "$skill_file")"
	if [[ "$normalized" == *"$snippet"* ]]; then
		sed -nE 's/^expected=([0-9]+)$/\1/p' "$skill_file" | grep -vx -- "$contract" | head -n 1
		return 2
	fi
	return 1
}
