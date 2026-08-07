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
    (.name | type == "string" and length > 0) and
    (.plugins | type == "array" and length > 0) and
    (all(.plugins[]; (.name | type == "string" and length > 0) and
                     (.source | type == "string" and length > 0)))
' "$marketplace" >/dev/null || fail "marketplace.json requires name and a non-empty plugins array with name and source"

jq -e '
    (.name | type == "string" and length > 0) and
    (.version | type == "string" and length > 0) and
    (.skills | type == "string" and length > 0)
' "$plugin" >/dev/null || fail "plugin.json requires name, version, and skills"

if jq -e '
    ([.name] + [.plugins[].name]) |
    any(.[]; test("ralphex"; "i"))
' "$marketplace" >/dev/null || jq -e '.name | test("ralphex"; "i")' "$plugin" >/dev/null; then
    fail "marketplace and plugin names must not contain ralphex"
fi

skills_path=$(jq -r '.skills' "$plugin")
[ -d "$repo_root/$skills_path" ] || fail "skills directory does not exist: $skills_path"

printf 'plugin manifests are valid\n'
