#!/usr/bin/env bash

# Runs scripts/install.sh against a copy of the files it reads, with stub make, claude,
# and install-codex-skills.sh, in a temporary HOME. PATH is built from the stubs plus
# /usr/bin and /bin only, so neither the real claude nor an installed loopai can leak
# into a case: both are what the installer inspects.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

command -v jq >/dev/null 2>&1 || fail "jq is required"
contract="$(sed -nE 's/^const skillContract = ([0-9]+)$/\1/p' "$repo_root/cmd/loopai/skill_contract.go")"
[[ -n "$contract" ]] || fail "cannot read skillContract"
plugin_version="$(jq -r .version "$repo_root/.claude-plugin/plugin.json")"

# the installer resolves its repository from its own path, so it runs from a copy
fake_repo="$tmp/repo"
mkdir -p "$fake_repo/scripts/lib" "$fake_repo/cmd/loopai" "$fake_repo/.claude-plugin"
cp "$repo_root/scripts/install.sh" "$fake_repo/scripts/"
cp "$repo_root/scripts/lib/skill-contract.sh" "$fake_repo/scripts/lib/"
cp "$repo_root/cmd/loopai/skill_contract.go" "$fake_repo/cmd/loopai/"
cp "$repo_root/.claude-plugin/plugin.json" "$fake_repo/.claude-plugin/"
cat >"$fake_repo/scripts/install-codex-skills.sh" <<'STUB'
#!/bin/sh
printf '%s\n' "$CODEX_HOME" >>"$STUB_LOG_DIR/codex"
STUB
chmod +x "$fake_repo/scripts/install-codex-skills.sh"

tools="$tmp/tools"
mkdir -p "$tools"
ln -s "$(command -v bash)" "$tools/bash"
ln -s "$(command -v jq)" "$tools/jq"

# stub make builds .bin/loopai answering --skill-contract with $STUB_CONTRACT, or with
# `unknown flag` when that is `unknown`, the way a binary predating the flag does
stubs="$tmp/stubs"
mkdir -p "$stubs"
cat >"$stubs/make" <<'STUB'
#!/bin/sh
dir=.
if [ "$1" = -C ]; then
	dir=$2
	shift 2
fi
[ "$1" = build ] || { echo "stub make: unexpected arguments: $*" >&2; exit 2; }
[ -z "$STUB_MAKE_FAIL" ] || { echo "stub make: build failed" >&2; exit 1; }
mkdir -p "$dir/.bin"
if [ "$STUB_CONTRACT" = unknown ]; then
	answer='echo "unknown flag skill-contract" >&2; exit 1'
else
	answer="echo $STUB_CONTRACT"
fi
printf '#!/bin/sh\n[ "$1" = --skill-contract ] && { %s; exit 0; }\necho loopai stub\n' "$answer" >"$dir/.bin/loopai"
printf '#!/bin/sh\necho loopai-acp stub\n' >"$dir/.bin/loopai-acp"
chmod +x "$dir/.bin/loopai" "$dir/.bin/loopai-acp"
STUB
cat >"$stubs/claude" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >>"$STUB_LOG_DIR/claude"
case "$*" in
"plugin list --json") cat "$STUB_PLUGINS" ;;
"plugin update "*) [ -z "$STUB_UPDATE_FAIL" ] || exit 1 ;;
esac
STUB
chmod +x "$stubs/make" "$stubs/claude"

# a stub directory without claude, for the case where Claude Code is not installed
stubs_noclaude="$tmp/stubs-noclaude"
mkdir -p "$stubs_noclaude"
cp "$stubs/make" "$stubs_noclaude/"

plugins_json() {
	local version="$1"
	shift
	local entries=() id
	for id in "$@"; do
		entries+=("{\"id\":\"$id\",\"version\":\"0.0.1\",\"folderVersion\":\"$version\",\"scope\":\"user\"}")
	done
	local IFS=,
	printf '[%s]\n' "${entries[*]-}"
}

# new_case creates a fresh HOME, bin directory, Codex home, and logs for one case.
new_case() {
	case_dir="$tmp/case-$1"
	home="$case_dir/home"
	bin="$case_dir/bin"
	codex="$case_dir/codex"
	logs="$case_dir/log"
	mkdir -p "$home" "$codex" "$logs"
	: >"$logs/claude"
	plugins="$case_dir/plugins.json"
	plugins_json "$plugin_version" loopai@loopai codex-imagegen@loopai other@elsewhere >"$plugins"
	path="$stubs:$bin:$tools:/usr/bin:/bin"
}

# run_install runs the installer for the current case; extra VAR=value arguments
# override the defaults. Sets output and status.
run_install() {
	status=0
	output="$(env -i HOME="$home" PATH="$path" LOOPAI_BIN_DIR="$bin" CODEX_HOME="$codex" \
		STUB_LOG_DIR="$logs" STUB_PLUGINS="$plugins" STUB_CONTRACT="$contract" "$@" \
		"$fake_repo/scripts/install.sh" 2>&1 </dev/null)" || status=$?
}

expect_status() {
	[[ "$status" == "$2" ]] || fail "$1: expected exit $2, got $status: $output"
}

expect_output() {
	[[ "$output" == *"$2"* ]] || fail "$1: output lacks '$2': $output"
}

reject_output() {
	[[ "$output" != *"$2"* ]] || fail "$1: output unexpectedly has '$2': $output"
}

expect_claude_call() {
	grep -Fqx -- "$2" "$logs/claude" || fail "$1: claude was not called with '$2': $(cat "$logs/claude")"
}

reject_claude_call() {
	if grep -Fq -- "$2" "$logs/claude"; then
		fail "$1: claude was called with '$2': $(cat "$logs/claude")"
	fi
}

warning_lines() {
	printf '%s\n' "$output" | grep -c '^! ' || true
}

# success: every channel updated and verified, no warning
new_case success
run_install
expect_status success 0
[[ -x "$bin/loopai" && -x "$bin/loopai-acp" ]] || fail "success: binaries not installed"
expect_claude_call success "plugin marketplace update loopai"
expect_claude_call success "plugin update loopai@loopai"
expect_claude_call success "plugin update codex-imagegen@loopai"
reject_claude_call success "other@elsewhere"
reject_claude_call success "plugin install"
grep -Fqx "$codex" "$logs/codex" || fail "success: Codex skills not installed into $codex"
expect_output success "✓ skill contract $contract matches $bin/loopai"
expect_output success "✓ plugin loopai@loopai is at $plugin_version"
expect_output success "/reload-plugins"
expect_output success "install complete"
[[ "$(warning_lines)" == 0 ]] || fail "success: unexpected warnings: $output"

# LOOPAI_BIN_DIR unset installs into ~/.local/bin
new_case default-bin
path="$stubs:$home/.local/bin:$tools:/usr/bin:/bin"
run_install LOOPAI_BIN_DIR=
expect_status default-bin 0
[[ -x "$home/.local/bin/loopai" && -x "$home/.local/bin/loopai-acp" ]] || fail "default-bin: not installed into ~/.local/bin"
[[ ! -e "$bin" ]] || fail "default-bin: installed into the case bin directory"

# a failed build installs nothing and touches no other channel
new_case build-failure
run_install STUB_MAKE_FAIL=1
expect_status build-failure 1
expect_output build-failure "make build failed"
[[ ! -e "$bin" ]] || fail "build-failure: bin directory was created"
[[ ! -s "$logs/claude" ]] || fail "build-failure: claude was called"
[[ ! -e "$logs/codex" ]] || fail "build-failure: Codex skills were installed"

# no claude on PATH is a warning, not a failure
new_case no-claude
path="$stubs_noclaude:$bin:$tools:/usr/bin:/bin"
run_install
expect_status no-claude 0
expect_output no-claude "! claude not on PATH"
expect_output no-claude "install complete"

# loopai@loopai not installed: the install command is printed, never run
new_case not-installed
plugins_json "$plugin_version" codex-imagegen@loopai >"$plugins"
run_install
expect_status not-installed 0
expect_output not-installed "claude plugin install loopai@loopai"
expect_claude_call not-installed "plugin update codex-imagegen@loopai"
reject_claude_call not-installed "plugin install"
reject_claude_call not-installed "plugin update loopai@loopai"

# no plugin from the marketplace installed at all
new_case none-installed
plugins_json "$plugin_version" >"$plugins"
run_install
expect_status none-installed 0
expect_output none-installed "claude plugin install loopai@loopai"
reject_claude_call none-installed "plugin update"

# a failed plugin update is a warning
new_case update-failure
run_install STUB_UPDATE_FAIL=1
expect_status update-failure 0
expect_output update-failure "! claude plugin update loopai@loopai failed"

# an installed plugin version that differs from the checkout is a warning
new_case plugin-version
plugins_json 0.0.1 loopai@loopai >"$plugins"
run_install
expect_status plugin-version 0
expect_output plugin-version "! plugin loopai@loopai reports 0.0.1, checkout has $plugin_version"

# a binary reporting another contract fails the install
new_case contract-mismatch
run_install STUB_CONTRACT=999
expect_status contract-mismatch 1
expect_output contract-mismatch "skill contract mismatch: skills expect $contract, $bin/loopai reports 999"
expect_output contract-mismatch "install failed"

# a binary predating the flag reads as contract 0
new_case contract-unknown
run_install STUB_CONTRACT=unknown
expect_status contract-unknown 1
expect_output contract-unknown "reports 0"

# another loopai earlier in PATH shadows the installed one
new_case shadowed
mkdir -p "$case_dir/shadow"
printf '#!/bin/sh\necho old\n' >"$case_dir/shadow/loopai"
chmod +x "$case_dir/shadow/loopai"
path="$stubs:$case_dir/shadow:$bin:$tools:/usr/bin:/bin"
run_install
expect_status shadowed 0
expect_output shadowed "! loopai on PATH resolves to $case_dir/shadow/loopai, not $bin/loopai"

# the bin directory missing from PATH is reported
new_case not-on-path
path="$stubs:$tools:/usr/bin:/bin"
run_install
expect_status not-on-path 0
expect_output not-on-path "! loopai is not on PATH; add $bin to PATH"

# standalone skill copies are reported with an rm command and left in place
new_case standalone
mkdir -p "$home/.claude/skills/loopai-merge" "$home/.claude/skills/unrelated"
printf 'mine\n' >"$home/.claude/skills/loopai-merge/SKILL.md"
run_install
expect_status standalone 0
expect_output standalone "! standalone skill copies duplicate the loopai plugin"
expect_output standalone "rm -rf $home/.claude/skills/loopai-merge"
reject_output standalone "skills/unrelated"
[[ -f "$home/.claude/skills/loopai-merge/SKILL.md" ]] || fail "standalone: copy was removed"

# no Codex home: Codex skills are skipped, not installed into a new one
new_case no-codex
rm -rf "$codex"
run_install
expect_status no-codex 0
expect_output no-codex "Codex skills skipped"
[[ ! -e "$logs/codex" ]] || fail "no-codex: install-codex-skills.sh was run"
[[ ! -e "$codex" ]] || fail "no-codex: Codex home was created"

printf 'install tests passed\n'
