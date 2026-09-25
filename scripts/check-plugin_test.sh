#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checker="$script_dir/check-plugin.sh"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/.claude-plugin" "$fixture/assets/claude/skills"

write_valid_manifests() {
    printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","source":"./","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
    printf '%s\n' '{"name":"loopai","version":"0.1.2","skills":"./assets/claude/skills/"}' > "$fixture/.claude-plugin/plugin.json"
}

expect_failure() {
    description=$1
    expected=$2
    if output=$("$checker" "$fixture" 2>&1); then
        printf 'FAIL: %s\n' "$description" >&2
        exit 1
    fi
    if [[ "$output" != *"$expected"* ]]; then
        printf 'FAIL: %s (expected %q, got %s)\n' "$description" "$expected" "$output" >&2
        exit 1
    fi
    printf 'PASS: %s\n' "$description"
}

expect_success() {
    description=$1
    if ! "$checker" "$fixture" >/dev/null; then
        printf 'FAIL: %s\n' "$description" >&2
        exit 1
    fi
    printf 'PASS: %s\n' "$description"
}

write_valid_manifests
expect_success "valid manifests are accepted"

printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","source":"./","version":"1.2.3-rc.1+build.5"}]}' > "$fixture/.claude-plugin/marketplace.json"
printf '%s\n' '{"name":"loopai","version":"1.2.3-rc.1+build.5","skills":"./assets/claude/skills/"}' > "$fixture/.claude-plugin/plugin.json"
expect_success "semantic prerelease manifest versions are accepted"

printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","source":"./","version":"banana"}]}' > "$fixture/.claude-plugin/marketplace.json"
printf '%s\n' '{"name":"loopai","version":"banana","skills":"./assets/claude/skills/"}' > "$fixture/.claude-plugin/plugin.json"
expect_failure "non-semantic manifest versions are rejected" "marketplace.json must describe"

rm "$fixture/.claude-plugin/marketplace.json"
expect_failure "a missing marketplace manifest is rejected" "missing .claude-plugin/marketplace.json"

write_valid_manifests
rm "$fixture/.claude-plugin/plugin.json"
expect_failure "a missing plugin manifest is rejected" "missing .claude-plugin/plugin.json"

write_valid_manifests
printf '%s\n' '{broken' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "invalid marketplace JSON is rejected" "invalid JSON in .claude-plugin/marketplace.json"

write_valid_manifests
printf '%s\n' '{broken' > "$fixture/.claude-plugin/plugin.json"
expect_failure "invalid plugin JSON is rejected" "invalid JSON in .claude-plugin/plugin.json"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "a missing marketplace source is rejected" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"other","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","source":"./","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "the marketplace name must be loopai" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"ralphex","source":"./","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "the marketplace plugin name must be loopai" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","source":"../","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "the marketplace source must be repository-local" "marketplace.json must describe"

write_extra_plugin() {
    version=${1:-0.3.0}
    mkdir -p "$fixture/plugins/extra/.claude-plugin" "$fixture/plugins/extra/skills"
    printf '{"name":"extra","version":"%s","skills":"./skills/"}\n' "$version" > "$fixture/plugins/extra/.claude-plugin/plugin.json"
    printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","source":"./","version":"0.1.2"},{"name":"extra","source":"./plugins/extra","description":"extra fixture","version":"0.3.0"}]}' > "$fixture/.claude-plugin/marketplace.json"
}

write_valid_manifests
write_extra_plugin
expect_success "an extra plugin under plugins/<name> is accepted"

write_valid_manifests
write_extra_plugin
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","source":"./","version":"0.1.2"},{"name":"other","source":"./other","description":"extra fixture","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "an extra plugin outside plugins/<name> is rejected" "marketplace plugin 1 must use source ./plugins/<name>"

write_valid_manifests
write_extra_plugin
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"extra","source":"./plugins/extra","description":"extra fixture","version":"0.3.0"},{"name":"loopai","source":"./","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "loopai must stay the first marketplace plugin" "marketplace.json must describe"

write_valid_manifests
write_extra_plugin
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","source":"./","version":"0.1.2"},{"name":"extra","source":"./plugins/extra","version":"0.3.0"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "an extra plugin without a description is rejected" "marketplace plugin 1 must use"

write_valid_manifests
write_extra_plugin
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","source":"./","version":"0.1.2"},{"name":"extra","source":"./plugins/extra","description":"extra fixture","version":"0.3.0"},{"name":"extra","source":"./plugins/extra","description":"extra fixture","version":"0.3.0"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "duplicate marketplace plugin names are rejected" "marketplace plugin names must be unique"

write_valid_manifests
write_extra_plugin
rm "$fixture/plugins/extra/.claude-plugin/plugin.json"
expect_failure "an extra plugin without a manifest is rejected" "missing plugins/extra/.claude-plugin/plugin.json"

write_valid_manifests
write_extra_plugin 0.9.9
expect_failure "extra plugin version mismatches are rejected" "marketplace and plugins/extra versions must match"

write_valid_manifests
write_extra_plugin
printf '%s\n' '{"name":"extra","version":"0.3.0","skills":"../../assets/"}' > "$fixture/plugins/extra/.claude-plugin/plugin.json"
expect_failure "an extra plugin with a traversing skills path is rejected" "must name extra and use a local skills path"

write_valid_manifests
write_extra_plugin
rm -rf "$fixture/plugins/extra/skills"
expect_failure "an extra plugin without its skills directory is rejected" "skills directory does not exist: plugins/extra/./skills/"
rm -rf "$fixture/plugins"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{"name":"fixture owner"},"plugins":[{"name":"loopai","source":"./"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "a missing marketplace version is rejected" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","plugins":[{"name":"loopai","source":"./","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "a missing marketplace owner is rejected" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":{},"plugins":[{"name":"loopai","source":"./","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "an owner without a name is rejected" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","owner":"fixture owner","plugins":[{"name":"loopai","source":"./","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "a non-object marketplace owner is rejected" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"ralphex","version":"0.1.2","skills":"./assets/claude/skills/"}' > "$fixture/.claude-plugin/plugin.json"
expect_failure "the plugin name must be loopai" "plugin.json must name loopai"

write_valid_manifests
printf '%s\n' '{"name":"loopai","skills":"./assets/claude/skills/"}' > "$fixture/.claude-plugin/plugin.json"
expect_failure "a missing plugin version is rejected" "plugin.json must name loopai"

write_valid_manifests
printf '%s\n' '{"name":"loopai","version":"0.1.2","skills":"../"}' > "$fixture/.claude-plugin/plugin.json"
expect_failure "a traversing skills path is rejected" "plugin.json must name loopai"

write_valid_manifests
rm -rf "$fixture/assets/claude/skills"
expect_failure "a missing skills directory is rejected" "skills directory does not exist"
mkdir -p "$fixture/assets/claude/skills"

write_valid_manifests
printf '%s\n' '{"name":"loopai","version":"9.9.9","skills":"./assets/claude/skills/"}' > "$fixture/.claude-plugin/plugin.json"
expect_failure "manifest version mismatches are rejected" "marketplace and plugin versions must match"
