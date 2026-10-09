#!/usr/bin/env bash

# Executes the skill contract snippet against stub loopai binaries. The checkers prove
# every skill carries the snippet byte-for-byte; this suite proves the snippet itself
# reads every binary the way the skills' instructions assume, an old binary that answers
# `unknown flag` included.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

skill_file="$repo_root/assets/claude/skills/loopai/SKILL.md"
awk '
	/^```bash$/ { block = ""; inside = 1; next }
	inside && /^```$/ { inside = 0; if (block ~ /--skill-contract/) { printf "%s", block; exit } next }
	inside { block = block $0 "\n" }
' "$skill_file" >"$tmp/snippet.sh"
[[ -s "$tmp/snippet.sh" ]] || fail "no skill contract snippet in $skill_file"

contract="$(sed -nE 's/^const skillContract = ([0-9]+)$/\1/p' "$repo_root/cmd/loopai/skill_contract.go")"
[[ -n "$contract" ]] || fail "cannot read skillContract"
grep -Fqx "expected=$contract" "$tmp/snippet.sh" || fail "snippet expected= differs from skillContract $contract"

# pin expected= to a value with room on both sides for older and newer binaries
sed -E 's/^expected=[0-9]+$/expected=5/' "$tmp/snippet.sh" >"$tmp/pinned.sh"
grep -Fqx 'expected=5' "$tmp/pinned.sh" || fail "could not pin expected="

bash_bin="$(command -v bash)"

# stub writes an executable loopai into its own directory and prints that directory.
stub() {
	local name="$1" body="$2"
	mkdir -p "$tmp/$name"
	printf '#!/bin/sh\n%s\n' "$body" >"$tmp/$name/loopai"
	chmod +x "$tmp/$name/loopai"
	printf '%s\n' "$tmp/$name"
}

expect_line() {
	local label="$1" path_dir="$2" expected="$3"
	local output
	output="$(PATH="$path_dir" "$bash_bin" "$tmp/pinned.sh" 2>&1)" || fail "$label: snippet exited non-zero: $output"
	[[ "$output" == "$expected" ]] || fail "$label: expected '$expected', got '$output'"
}

mkdir -p "$tmp/empty"
expect_line "loopai absent" "$tmp/empty" "skill contract: skill=5 binary=missing"
expect_line "unknown flag" "$(stub unknown 'echo "unknown flag skill-contract" >&2; exit 1')" \
	"skill contract: skill=5 binary=0"
expect_line "banner instead of a number" "$(stub banner 'echo "loopai master-75ed2fd"')" \
	"skill contract: skill=5 binary=0"
expect_line "empty output" "$(stub empty-out 'exit 0')" "skill contract: skill=5 binary=0"
expect_line "number with a failing exit" "$(stub failing '[ "$1" = --skill-contract ] && echo 5; exit 1')" \
	"skill contract: skill=5 binary=0"
expect_line "older contract" "$(stub older '[ "$1" = --skill-contract ] && echo 4')" \
	"skill contract: skill=5 binary=4"
expect_line "equal contract" "$(stub equal '[ "$1" = --skill-contract ] && echo 5')" \
	"skill contract: skill=5 binary=5"
expect_line "newer contract" "$(stub newer '[ "$1" = --skill-contract ] && echo 6')" \
	"skill contract: skill=5 binary=6"
expect_line "stderr noise is not read" "$(stub noisy 'echo warning >&2; echo 5')" \
	"skill contract: skill=5 binary=5"

# loopai-update is the one command that updates every channel: both copies must locate
# the checkout through the marketplace record and run its installer. A rewrite that
# drops either would leave the contract warning pointing at a command that installs nothing.
# the merge must extract defaults from the binary just installed, not whatever PATH finds first.
for update_skill in "$repo_root/assets/claude/skills/loopai-update/SKILL.md" \
	"$repo_root/assets/codex/skills/loopai-update/SKILL.md"; do
	for needle in 'make -C <checkout> install' 'known_marketplaces.json' 'LOOPAI_SRC' 'git pull --ff-only' \
		'"$LOOPAI_BIN" --dump-defaults'; do
		grep -Fq -- "$needle" "$update_skill" || fail "$update_skill does not mention $needle"
	done
done

printf 'skill contract snippet tests passed\n'
