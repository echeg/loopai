#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
installer="$script_dir/install-codex-skills.sh"
source_dir="$(cd "$script_dir/.." && pwd)/assets/codex/skills"
sandbox="$(mktemp -d)"
trap 'rm -rf "$sandbox"' EXIT

expected_skills="$(find "$source_dir" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; | sort)"

fail() {
	printf '%s\n' "$*" >&2
	exit 1
}

assert_installed() {
	local dest="$1" actual
	actual="$(find "$dest" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; | sort)"
	[[ "$actual" == "$expected_skills" ]] || fail "unexpected install inventory: $actual"
	while IFS= read -r name; do
		[[ -f "$dest/$name/SKILL.md" ]] || fail "missing installed SKILL.md for $name"
		[[ -f "$dest/$name/agents/openai.yaml" ]] || fail "missing installed interface for $name"
	done <<<"$expected_skills"
}

make_legacy() {
	local dest="$1" name="$2"
	mkdir -p "$dest/$name/agents"
	printf '%s\n' '---' "name: $name" 'description: stale' '---' >"$dest/$name/SKILL.md"
	printf '%s\n' 'interface:' '  display_name: "stale"' >"$dest/$name/agents/openai.yaml"
}

# a dry run must leave the destination untouched
dest="$sandbox/dry"
"$installer" --dry-run --dest "$dest" >/dev/null
[[ ! -e "$dest" ]] || fail "dry run created $dest"

# fresh install
dest="$sandbox/fresh"
"$installer" --dest "$dest" >/dev/null
assert_installed "$dest"

# reinstall drops files removed from the source tree instead of leaving them behind
printf 'stale\n' >"$dest/loopai/LEFTOVER.md"
"$installer" --dest "$dest" >/dev/null
[[ ! -e "$dest/loopai/LEFTOVER.md" ]] || fail "reinstall kept a stale file"
assert_installed "$dest"

# a pre-rename skill in its original shape is removed
dest="$sandbox/legacy"
make_legacy "$dest" ralphex-plan
make_legacy "$dest" ralphex-run
"$installer" --dest "$dest" >/dev/null
[[ ! -e "$dest/ralphex-plan" ]] || fail "pristine legacy skill survived"
[[ ! -e "$dest/ralphex-run" ]] || fail "pristine legacy skill survived"
assert_installed "$dest"

# a pre-rename skill the user extended is kept, since removing it would drop their work
dest="$sandbox/legacy-edited"
make_legacy "$dest" ralphex-plan
mkdir -p "$dest/ralphex-plan/scripts"
printf 'echo mine\n' >"$dest/ralphex-plan/scripts/helper.sh"
output="$("$installer" --dest "$dest")"
[[ -f "$dest/ralphex-plan/scripts/helper.sh" ]] || fail "edited legacy skill was removed"
[[ "$output" == *"local edits"* ]] || fail "edited legacy skill was not reported: $output"

# --keep-legacy leaves even a pristine one alone
dest="$sandbox/legacy-kept"
make_legacy "$dest" ralphex-adopt
"$installer" --keep-legacy --dest "$dest" >/dev/null
[[ -d "$dest/ralphex-adopt" ]] || fail "--keep-legacy removed a legacy skill"

# an unrelated skill is never touched
dest="$sandbox/unrelated"
make_legacy "$dest" some-other-skill
"$installer" --dest "$dest" >/dev/null
[[ -d "$dest/some-other-skill" ]] || fail "installer removed an unrelated skill"

# unknown options fail loudly rather than being ignored
if "$installer" --nope --dest "$sandbox/never" >/dev/null 2>&1; then
	fail "installer accepted an unknown option"
fi
[[ ! -e "$sandbox/never" ]] || fail "installer wrote despite an unknown option"

printf 'install-codex-skills tests passed\n'
