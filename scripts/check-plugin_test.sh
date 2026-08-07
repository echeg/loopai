#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checker="$script_dir/check-plugin.sh"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/.claude-plugin" "$fixture/assets/claude/skills"

write_valid_manifests() {
    printf '%s\n' '{"name":"loopai","description":"fixture marketplace","plugins":[{"name":"loopai","source":"./","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
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

expect_failure "missing manifests are rejected" "missing .claude-plugin/marketplace.json"

write_valid_manifests
expect_success "valid manifests are accepted"

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
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","plugins":[{"name":"loopai","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "a missing marketplace source is rejected" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"other","description":"fixture marketplace","plugins":[{"name":"loopai","source":"./","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "the marketplace name must be loopai" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","plugins":[{"name":"ralphex","source":"./","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "the marketplace plugin name must be loopai" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","plugins":[{"name":"loopai","source":"../","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "the marketplace source must be repository-local" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","plugins":[{"name":"loopai","source":"./","version":"0.1.2"},{"name":"other","source":"./other","version":"0.1.2"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "multiple marketplace plugins are rejected" "marketplace.json must describe"

write_valid_manifests
printf '%s\n' '{"name":"loopai","description":"fixture marketplace","plugins":[{"name":"loopai","source":"./"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "a missing marketplace version is rejected" "marketplace.json must describe"

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
