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
    (.plugins | type == "array" and length >= 1) and
    (.plugins[0].name == "loopai") and
    (.plugins[0].source == "./") and
    (.plugins[0].version | type == "string" and test("^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\\+[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?$"))
' "$marketplace" >/dev/null || fail "marketplace.json must describe the owned loopai plugin first, with source ./ and a version"

jq -e '
    (.name == "loopai") and
    (.version | type == "string" and test("^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\\+[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?$")) and
    (.skills == "./assets/claude/skills/")
' "$plugin" >/dev/null || fail "plugin.json must name loopai and use ./assets/claude/skills/"

marketplace_version=$(jq -r '.plugins[0].version' "$marketplace")
plugin_version=$(jq -r '.version' "$plugin")
[ "$marketplace_version" = "$plugin_version" ] || fail "marketplace and plugin versions must match"

skills_path=$(jq -r '.skills' "$plugin")
[ -d "$repo_root/$skills_path" ] || fail "skills directory does not exist: $skills_path"

# Further plugins live under plugins/<name>/ with their own manifest and skills.
semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
extra_count=$(jq '.plugins | length - 1' "$marketplace")
for ((index = 1; index <= extra_count; index++)); do
    jq -e --argjson i "$index" --arg semver "$semver" '
        .plugins[$i] as $p |
        ($p.name | type == "string" and test("^[a-z0-9][a-z0-9-]*$")) and
        ($p.name != "loopai") and
        ($p.source == "./plugins/" + $p.name) and
        ($p.description | type == "string" and length > 0) and
        ($p.version | type == "string" and test($semver))
    ' "$marketplace" >/dev/null || fail "marketplace plugin $index must use source ./plugins/<name>, a description and a version"
    name=$(jq -r --argjson i "$index" '.plugins[$i].name' "$marketplace")
    [ "$(jq -r '[.plugins[].name] | map(select(. == "'"$name"'")) | length' "$marketplace")" = 1 ] || fail "marketplace plugin names must be unique: $name"
    extra_manifest="$repo_root/plugins/$name/.claude-plugin/plugin.json"
    [ -f "$extra_manifest" ] || fail "missing plugins/$name/.claude-plugin/plugin.json"
    jq empty "$extra_manifest" >/dev/null 2>&1 || fail "invalid JSON in plugins/$name/.claude-plugin/plugin.json"
    jq -e --arg name "$name" '
        (.name == $name) and
        (.skills | type == "string" and test("^\\./[^.]") and (contains("..") | not))
    ' "$extra_manifest" >/dev/null || fail "plugins/$name/.claude-plugin/plugin.json must name $name and use a local skills path"
    [ "$(jq -r '.version' "$extra_manifest")" = "$(jq -r --argjson i "$index" '.plugins[$i].version' "$marketplace")" ] || fail "marketplace and plugins/$name versions must match"
    extra_skills=$(jq -r '.skills' "$extra_manifest")
    [ -d "$repo_root/plugins/$name/$extra_skills" ] || fail "skills directory does not exist: plugins/$name/$extra_skills"
done

printf 'plugin manifests are valid\n'
