#!/usr/bin/env bash

set -euo pipefail

repo_root=${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}
manifest_dir="$repo_root/.claude-plugin"
marketplace="$manifest_dir/marketplace.json"
plugin="$manifest_dir/plugin.json"

fail() {
    printf 'plugin manifest check failed: %s\n' "$1" >&2
    exit 1
}

command -v jq >/dev/null 2>&1 || fail "jq is required"

for manifest in "$marketplace" "$plugin"; do
    [ -f "$manifest" ] || fail "missing ${manifest#"$repo_root"/}"
    jq empty "$manifest" >/dev/null 2>&1 || fail "invalid JSON in ${manifest#"$repo_root"/}"
done

jq -e '
    (.name == "loopai") and
    (.description | type == "string" and length > 0) and
    (.owner | type == "object") and
    (.owner.name | type == "string" and length > 0) and
    (.plugins | type == "array" and length == 1) and
    (.plugins[0].name == "loopai") and
    (.plugins[0].source == "./") and
    (.plugins[0].version | type == "string" and length > 0)
' "$marketplace" >/dev/null || fail "marketplace.json must describe the owned single loopai plugin with source ./ and a version"

jq -e '
    (.name == "loopai") and
    (.version | type == "string" and length > 0) and
    (.skills == "./assets/claude/skills/")
' "$plugin" >/dev/null || fail "plugin.json must name loopai and use ./assets/claude/skills/"

marketplace_version=$(jq -r '.plugins[0].version' "$marketplace")
plugin_version=$(jq -r '.version' "$plugin")
[ "$marketplace_version" = "$plugin_version" ] || fail "marketplace and plugin versions must match"

skills_path=$(jq -r '.skills' "$plugin")
[ -d "$repo_root/$skills_path" ] || fail "skills directory does not exist: $skills_path"

printf 'plugin manifests are valid\n'
