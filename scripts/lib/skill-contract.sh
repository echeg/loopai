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

# first_fenced_block prints the skill's first fenced code block, fences included. The
# snippet must open the skill, so a copy placed after steps that already ran loopai
# does not count.
first_fenced_block() {
	awk '/^```/ { print; if (inside) exit; inside = 1; next } inside { print }' "$1"
}

# check_skill_contract_snippet validates one skill file against the canonical snippet.
# Status 0: present and current; 1: missing, modified, or not the first fenced block;
# 2: present with a stale expected= value, which it prints on stdout.
check_skill_contract_snippet() {
	local skill_file="$1"
	local contract="$2"
	local snippet block stale

	snippet="$(skill_contract_snippet "$contract")"
	block="$(first_fenced_block "$skill_file")"
	if [[ "$block" == "$snippet" ]]; then
		return 0
	fi

	stale="$(sed -nE 's/^expected=([0-9]+)$/\1/p' <<<"$block" | head -n 1)"
	if [[ -n "$stale" && "$(sed -E "s/^expected=[0-9]+\$/expected=$contract/" <<<"$block")" == "$snippet" ]]; then
		printf '%s\n' "$stale"
		return 2
	fi
	return 1
}
