#!/usr/bin/env bash

# Installs the Codex skill tree into ${CODEX_HOME:-$HOME/.codex}/skills.
#
# Codex discovers skills from that directory only, so unlike the Claude side
# there is no marketplace to pull from and installation is a copy. The tree is
# validated first: an invalid skill silently does nothing useful in Codex, and
# finding that out at the first `$loopai-plan` is worse than failing here.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source_dir="$repo_root/assets/codex/skills"
dest_dir="${CODEX_HOME:-$HOME/.codex}/skills"
dry_run=0
keep_legacy=0

# Installed by hand before the fork was renamed. They are hand-written
# condensed rewrites, not copies of anything still in this repository, so
# nothing updates them and they shadow the loopai-* set with stale guidance.
legacy_skills=(ralphex-run ralphex-plan ralphex-adopt ralphex-update)

usage() {
	cat <<'USAGE'
usage: install-codex-skills.sh [--dry-run] [--keep-legacy] [--dest DIR]

  --dry-run       report what would change and write nothing
  --keep-legacy   leave pre-rename ralphex-* skills in place
  --dest DIR      install into DIR instead of ${CODEX_HOME:-$HOME/.codex}/skills
USAGE
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--dry-run) dry_run=1 ;;
	--keep-legacy) keep_legacy=1 ;;
	--dest)
		[[ $# -ge 2 ]] || {
			printf 'error: --dest requires a directory\n' >&2
			exit 2
		}
		dest_dir="$2"
		shift
		;;
	--dest=*) dest_dir="${1#--dest=}" ;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		printf 'error: unknown option: %s\n' "$1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

"$repo_root/scripts/check-codex-skills.sh" "$repo_root"

run() {
	if ((dry_run)); then
		printf '  would run: %s\n' "$*"
	else
		"$@"
	fi
}

# A legacy directory is removable only when it still has the shape the old
# hand-install produced. Anything else means the user edited or extended it,
# and dropping their work to install ours is not a trade this script may make.
legacy_is_pristine() {
	local dir="$1" files
	# LC_ALL=C so the comparison does not depend on the caller's collation:
	# en_US.UTF-8 sorts agents/openai.yaml before SKILL.md, C sorts it after.
	files="$(cd "$dir" && find . -type f -o -type l | sed 's|^\./||' | LC_ALL=C sort | tr '\n' ' ')"
	[[ "$files" == "SKILL.md " || "$files" == "SKILL.md agents/openai.yaml " ]]
}

printf 'installing codex skills into %s\n' "$dest_dir"
run mkdir -p "$dest_dir"

while IFS= read -r skill_name; do
	[[ -n "$skill_name" ]] || continue
	target="$dest_dir/$skill_name"
	if [[ -e "$target" ]]; then
		printf '  update %s\n' "$skill_name"
		run rm -rf "$target"
	else
		printf '  install %s\n' "$skill_name"
	fi
	run cp -R "$source_dir/$skill_name" "$target"
done < <(find "$source_dir" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; | sort)

for legacy in "${legacy_skills[@]}"; do
	legacy_dir="$dest_dir/$legacy"
	[[ -d "$legacy_dir" ]] || continue
	if ((keep_legacy)); then
		printf '  keeping pre-rename %s (--keep-legacy)\n' "$legacy"
	elif legacy_is_pristine "$legacy_dir"; then
		printf '  removing pre-rename %s\n' "$legacy"
		run rm -rf "$legacy_dir"
	else
		printf '  keeping pre-rename %s: it has local edits, remove it by hand\n' "$legacy"
	fi
done

if ((dry_run)); then
	printf 'dry run: nothing written\n'
else
	printf 'done. Restart Codex, then use $loopai-plan, $loopai, $loopai-orca, ...\n'
fi
