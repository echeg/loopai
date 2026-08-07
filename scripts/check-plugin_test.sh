#!/usr/bin/env bash

set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checker="$script_dir/check-plugin.sh"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/.claude-plugin" "$fixture/assets/claude/skills"

write_valid_manifests() {
    printf '%s\n' '{"name":"loopai","plugins":[{"name":"loopai","source":"./"}]}' > "$fixture/.claude-plugin/marketplace.json"
    printf '%s\n' '{"name":"loopai","version":"0.1.0","skills":"./assets/claude/skills/"}' > "$fixture/.claude-plugin/plugin.json"
}

expect_failure() {
    description=$1
    if "$checker" "$fixture" >/dev/null 2>&1; then
        printf 'FAIL: %s\n' "$description" >&2
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

expect_failure "missing manifests are rejected"

write_valid_manifests
expect_success "valid manifests are accepted"

printf '%s\n' '{broken' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "invalid JSON is rejected"

write_valid_manifests
printf '%s\n' '{"name":"loopai","plugins":[{"name":"loopai"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "missing required marketplace fields are rejected"

write_valid_manifests
printf '%s\n' '{"name":"ralphex","plugins":[{"name":"loopai","source":"./"}]}' > "$fixture/.claude-plugin/marketplace.json"
expect_failure "ralphex naming is rejected"

write_valid_manifests
printf '%s\n' '{"name":"loopai","version":"0.1.0","skills":"./missing-skills/"}' > "$fixture/.claude-plugin/plugin.json"
expect_failure "a missing skills directory is rejected"
