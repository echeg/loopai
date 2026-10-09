#!/usr/bin/env bash

# Updates every installed loopai channel from this checkout: the loopai binary and
# its ACP launcher, the Claude Code plugins from the loopai marketplace, and the
# Codex skills, then verifies that the installed binary speaks the skill contract
# the skills were written for.
#
# Each step prints one line, `✓` when it did its work and `!` when it could not
# and the user has something to do. Only a failed build, a failed binary install,
# and a skill contract mismatch exit non-zero: the other channels age
# independently and a missing one (no claude, no Codex home) is not an error.
#
# Standalone ~/.claude/skills/loopai* copies are reported, never touched: they are
# foreign directories with no baseline, so they may hold the user's own edits.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin_dir="${LOOPAI_BIN_DIR:-$HOME/.local/bin}"
codex_home="${CODEX_HOME:-$HOME/.codex}"
# Windows cannot start an extensionless binary, so make build produces .exe copies there
exe=""
[[ "${OS:-}" == Windows_NT ]] && exe=".exe"

# shellcheck source=lib/skill-contract.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/skill-contract.sh"

ok_count=0
warn_count=0
failed=""

ok() {
	printf '✓ %s\n' "$*"
	ok_count=$((ok_count + 1))
}

warn() {
	printf '! %s\n' "$*"
	warn_count=$((warn_count + 1))
}

# fatal reports a step that leaves nothing sensible to continue with.
fatal() {
	printf '! %s\n' "$*"
	printf 'install failed: %s\n' "$*"
	exit 1
}

# step 1: build, before anything is installed, so a broken tree changes nothing
if ! make -C "$repo_root" build; then
	fatal "make build failed; nothing was installed"
fi
ok "built $repo_root/.bin/loopai$exe"

# step 2: install the binary and the ACP launcher
if ! install -d "$bin_dir" ||
	! install -m 0755 "$repo_root/.bin/loopai$exe" "$repo_root/.bin/loopai-acp$exe" "$bin_dir/"; then
	fatal "could not install loopai$exe and loopai-acp$exe into $bin_dir"
fi
ok "installed loopai$exe and loopai-acp$exe into $bin_dir"

# installed_loopai_plugins prints `<id> <version>` for each installed plugin from
# the loopai marketplace. A directory-sourced plugin is read live from its folder,
# so its folderVersion is what Claude Code runs, not the cached install version.
installed_loopai_plugins() {
	claude plugin list --json </dev/null |
		jq -r '[.[] | select(.id | endswith("@loopai")) | "\(.id) \(.folderVersion // .version)"] | unique | .[]'
}

# step 3: Claude Code plugins
plugin_ids=()
loopai_plugin=0
if ! command -v claude >/dev/null 2>&1; then
	warn "claude not on PATH; Claude Code plugins not updated"
elif ! command -v jq >/dev/null 2>&1; then
	warn "jq not on PATH; Claude Code plugins not updated"
else
	if claude plugin marketplace update loopai </dev/null; then
		ok "updated the loopai marketplace"
	else
		warn "claude plugin marketplace update loopai failed; if the marketplace was never added, add it with: claude plugin marketplace add $repo_root"
	fi
	if listing="$(installed_loopai_plugins)"; then
		while read -r id _; do
			if [[ -n "$id" ]]; then
				plugin_ids+=("$id")
			fi
		done <<<"$listing"
		for id in ${plugin_ids[@]+"${plugin_ids[@]}"}; do
			if claude plugin update "$id" </dev/null; then
				ok "updated plugin $id"
			else
				warn "claude plugin update $id failed"
			fi
		done
		if [[ " ${plugin_ids[*]-} " == *" loopai@loopai "* ]]; then
			loopai_plugin=1
		else
			warn "plugin loopai@loopai is not installed; install it with: claude plugin install loopai@loopai (after claude plugin marketplace add $repo_root when the marketplace is missing)"
		fi
	else
		warn "claude plugin list failed; Claude Code plugins not updated"
	fi
fi

# step 4: Codex skills, only where Codex is set up
if [[ -d "$codex_home" ]]; then
	if CODEX_HOME="$codex_home" "$repo_root/scripts/install-codex-skills.sh"; then
		ok "installed Codex skills into $codex_home/skills"
	else
		warn "scripts/install-codex-skills.sh failed"
	fi
else
	printf -- '- %s does not exist; Codex skills skipped\n' "$codex_home"
fi

# step 5: verify what is now installed
if ! expected="$(skill_contract_value "$repo_root")"; then
	failed="cannot read skillContract from the checkout"
	warn "$failed"
else
	actual="$("$bin_dir/loopai$exe" --skill-contract 2>/dev/null)" || actual=0
	case $actual in '' | *[!0-9]*) actual=0 ;; esac
	if [[ "$actual" == "$expected" ]]; then
		ok "skill contract $expected matches $bin_dir/loopai$exe"
	else
		failed="skill contract mismatch: skills expect $expected, $bin_dir/loopai$exe reports $actual"
		warn "$failed"
	fi
fi

if ((loopai_plugin)); then
	plugin_version="$(jq -r .version "$repo_root/.claude-plugin/plugin.json")"
	installed_version="$(installed_loopai_plugins | sed -n 's/^loopai@loopai //p' | head -n 1)" || installed_version=""
	if [[ -z "$installed_version" ]]; then
		warn "could not read the installed loopai@loopai version"
	elif [[ "$installed_version" == "$plugin_version" ]]; then
		ok "plugin loopai@loopai is at $plugin_version"
	else
		warn "plugin loopai@loopai reports $installed_version, checkout has $plugin_version"
	fi
fi

resolved="$(command -v "loopai$exe" 2>/dev/null)" || resolved=""
if [[ -z "$resolved" ]]; then
	warn "loopai$exe is not on PATH; add $bin_dir to PATH"
# compared as files, so a trailing slash in LOOPAI_BIN_DIR or a symlinked PATH entry is not a shadow
elif [[ ! "$resolved" -ef "$bin_dir/loopai$exe" ]]; then
	warn "loopai$exe on PATH resolves to $resolved, not $bin_dir/loopai$exe; remove it or reorder PATH"
fi

# step 6: standalone copies duplicate the plugin and never receive its updates
standalone=()
for dir in "$HOME"/.claude/skills/loopai*; do
	if [[ -e "$dir" ]]; then
		standalone+=("$dir")
	fi
done
if ((${#standalone[@]})); then
	warn "standalone skill copies duplicate the loopai plugin and are not updated by it; remove them with:"
	for dir in "${standalone[@]}"; do
		printf '    rm -rf %q\n' "$dir"
	done
fi

# step 7: running sessions keep what they loaded
printf -- '- open Claude Code sessions need /reload-plugins or a restart; open Codex sessions need a restart\n'

if [[ -n "$failed" ]]; then
	printf 'install failed: %s\n' "$failed"
	exit 1
fi
printf 'install complete: %d done, %d warnings\n' "$ok_count" "$warn_count"
