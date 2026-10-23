#!/usr/bin/env bash

# Checks the loopai-t3 contract on both hosts: the mode question, the Mode 1 gate and inline
# least-privilege token mint, the Mode 2 provider lookup and thread launch, and report blocks that
# are byte-identical across Claude and Codex. The settings lookup and workspace snippets are
# executed against fixtures, and the checker itself is run against broken copies of the skills.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# check_skills validates the two loopai-t3 skills under the given repository root.
check_skills() {
	python3 - "$1" "$tmp/run" <<'PY'
from pathlib import Path
import json
import os
import re
import shutil
import subprocess
import sys

root = Path(sys.argv[1])
work = Path(sys.argv[2])
shutil.rmtree(work, ignore_errors=True)
work.mkdir(parents=True)

tool_prefix = {"claude": "mcp__t3-code__", "codex": "mcp__t3_code__"}
mint = ("auth session issue --scope orchestration:read --scope orchestration:operate "
        "--scope terminal:read --scope terminal:operate --ttl 3d --label loopai --token-only")
removed = ("--t3-launch", "--t3-progress-thread", "--no-t3", "resolve-thread", "current-thread",
           "observe.py", "npx --yes", "t3@latest", "--ttl 7d")
report_phrases = (
    ("Mode 1", "loopai --attach"),
    ("Mode 1", "fresh token"),
    ("Mode 1", "[loopai: …] title prefix"),
    ("Mode 2", "t3-thread://v1/<threadId>"),
    ("Mode 2", "type `resume` in that thread"),
    ("Mode 2", "switch that thread's model to Claude or Codex"),
)


def section(label, text, title):
    match = re.search(r"^## " + re.escape(title) + r"\n(.*?)(?=^## |\Z)", text, re.M | re.S)
    assert match, f"{label}: missing section: {title}"
    return match[1]


def require(label, where, text, *phrases):
    for phrase in phrases:
        assert phrase in text, f"{label}: {where} missing: {phrase}"


def bash_block(label, text, marker):
    blocks = [b for b in re.findall(r"```bash\n(.*?)```", text, re.S) if marker in b]
    assert len(blocks) == 1, f"{label}: expected one bash block with {marker}, found {len(blocks)}"
    return blocks[0]


shared = {}
for variant in ("claude", "codex"):
    label = variant
    text = (root / "assets" / variant / "skills" / "loopai-t3" / "SKILL.md").read_text(encoding="utf-8")
    other = tool_prefix["codex" if variant == "claude" else "claude"]

    for spelling in removed:
        assert spelling not in text, f"{label}: removed launcher spelling: {spelling}"
    assert other not in text, f"{label}: tool names of the other host: {other}"
    assert "**STOP HERE after reporting. Do not monitor automatically.**" in text, f"{label}: stop line"

    if variant == "claude":
        front = text.split("\n---\n", 1)[0]
        tools = re.search(r"^allowed-tools: \[(.*)\]$", front, re.M)
        assert tools, f"{label}: missing allowed-tools"
        for tool in ("ToolSearch", "mcp__t3-code__orchestrator_capabilities",
                     "mcp__t3-code__t3_thread_list", "mcp__t3-code__t3_thread_launch"):
            assert tool in [t.strip() for t in tools[1].split(",")], f"{label}: allowed-tools missing {tool}"
        assert "argument-hint:" in front and "name: loopai-t3" in front, f"{label}: frontmatter"

    context = section(label, text, "Step 2: Read the T3 Context")
    prefix = tool_prefix[variant]
    require(label, "T3 context", context, prefix + "orchestrator_capabilities", "parentThreadId",
            prefix + "t3_thread_list", "currentThreadId", "`parentThreadId` equals `currentThreadId`",
            "`runtimeMode` is `full-access`", "SANDBOX_RUNTIME", "CODEX_SANDBOX",
            "CODEX_SANDBOX_NETWORK_DISABLED", "Windows", "`^[A-Za-z0-9_.:%-]{1,1024}$`", "single-quote",
            "`models[].id`")
    environment = bash_block(label, context, "sandbox=")
    if variant == "claude":
        require(label, "T3 context", context, "ToolSearch")
    else:
        require(label, "T3 context", context, "one bounded direct attempt",
                "`tools.mcp__t3_code__orchestrator_capabilities({})`")

    mode = section(label, text, "Step 3: Choose the Mode")
    order = [mode.find(o) for o in ('"Terminal in this thread"', '"New loopai thread (shows Working)"', '"Cancel"')]
    assert -1 not in order and order == sorted(order), f"{label}: the mode question must offer both modes, then Cancel"
    require(label, "mode question", mode, "AskUserQuestion" if variant == "claude" else "numbered list")

    mode1 = section(label, text, "Step 4: Mode 1 - Terminal in This Thread")
    launch = bash_block(label, mode1, "loopai --detach")
    require(label, "Mode 1 launch", launch, mint, '"$T3_CLI" ' + mint,
            'T3_CLI="${T3CODE_CLI_PATH:-${T3CODE_HOME:-$HOME/.t3}/bin/t3}"',
            "THREAD_ID='<THREAD_ID>'", 'loopai --detach --t3-thread "$THREAD_ID"',
            "LOOPAI_T3_TOKEN=${LOOPAI_T3_TOKEN:-$(")
    assert "command -v t3" not in launch, f"{label}: the mint must not use a t3 on PATH"
    if variant == "claude":
        require(label, "Mode 1", mode1, "**foreground** Bash call", "never `run_in_background`", "180000 ms")
    else:
        require(label, "Mode 1", mode1, "foreground command", "never as a background job", "180 seconds")
    require(label, "Mode 1", mode1, "Never echo, log, or print the token")

    mode2 = section(label, text, "Step 5: Mode 2 - New loopai Thread")
    lookup = bash_block(label, mode2, "providerInstances")
    workspace = bash_block(label, mode2, "git-common-dir")
    require(label, "Mode 2", mode2, prefix + "t3_thread_launch", '"title": "loopai: <plan stem>"',
            '"modelSelection": {"instanceId": "<id>", "model": "<MODEL>"}', "`models[0].id`",
            '"workspaceStrategy"', '{"type": "root"}',
            '{"type": "existing_worktree", "worktreePath": "<path>"}',
            "Omit `runtimeMode` and `interactionMode`", "`interactionMode` is `plan`", "whitespace",
            prefix + "t3_thread_list", "before retrying", "empty `constraints`",
            "never edit `settings.json`", "Add provider", "ACP Registry", "`Local command`",
            "command -v loopai-acp", "the argument `--acp`", "Arguments editor",
            "the default is fine", "acpRegistry_local_acp")

    report = section(label, text, "Step 6: Report")
    blocks = re.findall(r"```text\n(.*?)```", report, re.S)
    assert len(blocks) == 2, f"{label}: Mode 1 and Mode 2 report blocks required, found {len(blocks)}"
    named = dict(zip(("Mode 1", "Mode 2"), blocks))
    for mode_name, phrase in report_phrases:
        assert phrase in named[mode_name], f"{label}: missing report phrase: {phrase}"

    for key, value in (("environment check", environment), ("Mode 1 launch", launch), ("settings lookup", lookup),
                       ("workspace", workspace), ("report", blocks)):
        if key in shared:
            assert shared[key] == value, f"{label}: {key} differs between Claude and Codex"
        shared[key] = value


def run_bash(script, cwd=None, env=None):
    path = work / "snippet.sh"
    path.write_text(script, encoding="utf-8")
    full_env = dict(os.environ, **(env or {}))
    result = subprocess.run(["bash", str(path)], cwd=cwd, env=full_env, capture_output=True, text=True)
    assert result.returncode == 0, f"snippet failed: {result.stderr}"
    return result.stdout.strip()


def acp(path, args=None, source="local", **extra):
    config = {"source": source, "enabled": True, "agentId": "", "commandPath": path,
              "commandArgs": [] if args is None else args, "authMethodId": None, "customModels": []}
    config.update(extra.pop("config", {}))
    return dict({"driver": "acpRegistry", "config": config}, **extra)


# the environment check reports a sandbox exactly when loopai's detectSandbox refuses --detach
sandbox_vars = ("CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", "SANDBOX_RUNTIME")
sandbox_cases = [
    ("no sandbox", {}, "sandbox="),
    ("SANDBOX_RUNTIME=1", {"SANDBOX_RUNTIME": "1"}, "sandbox=SANDBOX_RUNTIME=1"),
    ("SANDBOX_RUNTIME=0", {"SANDBOX_RUNTIME": "0"}, "sandbox="),
    ("CODEX_SANDBOX set", {"CODEX_SANDBOX": "seatbelt"}, "sandbox=CODEX_SANDBOX=seatbelt"),
    ("CODEX_SANDBOX_NETWORK_DISABLED=1", {"CODEX_SANDBOX_NETWORK_DISABLED": "1"},
     "sandbox=CODEX_SANDBOX_NETWORK_DISABLED=1"),
    ("CODEX_SANDBOX_NETWORK_DISABLED=0", {"CODEX_SANDBOX_NETWORK_DISABLED": "0"}, "sandbox="),
    ("all three", {"CODEX_SANDBOX": "seatbelt", "CODEX_SANDBOX_NETWORK_DISABLED": "1", "SANDBOX_RUNTIME": "1"},
     "sandbox=CODEX_SANDBOX=seatbelt CODEX_SANDBOX_NETWORK_DISABLED=1 SANDBOX_RUNTIME=1"),
]
for name, values, expected in sandbox_cases:
    env = dict({var: "" for var in sandbox_vars}, **values)
    lines = run_bash(shared["environment check"], env=env).splitlines()
    got = [line for line in lines if line.startswith("sandbox=")]
    assert got == [expected], f"environment check, {name}: got {got!r}, want {expected!r}"

settings_cases = [
    ("missing file", None, "instance=none"),
    ("invalid json", "{not json", "instance=unreadable (JSONDecodeError)"),
    ("top-level list", [], "instance=none"),
    ("no instances", {"providers": {}}, "instance=none"),
    ("loopai-acp", {"providerInstances": {"acpRegistry_local_acp": acp("/Users/me/.local/bin/loopai-acp")}},
     "instance=acpRegistry_local_acp"),
    ("loopai with --acp", {"providerInstances": {"z": acp("/opt/loopai", ["--acp"])}}, "instance=z"),
    ("loopai without --acp", {"providerInstances": {"z": acp("/opt/loopai", ["--serve"])}}, "instance=none"),
    ("loopai.exe with a string argument", {"providerInstances": {"z": acp("C:\\bin\\loopai.exe", "--acp")}},
     "instance=none"),
    ("registry source", {"providerInstances": {"r": acp("/opt/loopai-acp", source="registry")}}, "instance=none"),
    ("other driver", {"providerInstances": {"g": {"driver": "grok", "config": {"binaryPath": "/opt/loopai-acp"}}}},
     "instance=none"),
    ("disabled instance and config", {"providerInstances": {
        "a": acp("/opt/loopai-acp", enabled=False),
        "b": acp("/opt/loopai-acp", config={"enabled": False}),
        "c": acp("/opt/loopai-acp"),
    }}, "instance=c"),
    ("unknown keys and value types", {"providerInstances": {
        "a": "string", "b": {"config": 5}, "c": {"driver": "acpRegistry", "config": {"source": "local", "commandPath": 7}},
        "d": acp("C:\\bin\\loopai-acp.exe", "--acp", accentColor=["x"], config={"unknown": [1]}),
    }, "theme": 3}, "instance=d"),
    ("sorted key order", {"providerInstances": {
        "loopai": acp("/opt/loopai-acp"), "acpRegistry_local_acp": acp("/opt/loopai", ["--acp"]),
    }}, "instance=acpRegistry_local_acp"),
]
for name, content, expected in settings_cases:
    home = work / "t3" / re.sub(r"\W+", "-", name)
    (home / "userdata").mkdir(parents=True)
    if content is not None:
        raw = content if isinstance(content, str) else json.dumps(content)
        (home / "userdata" / "settings.json").write_text(raw, encoding="utf-8")
    got = run_bash(shared["settings lookup"], env={"T3CODE_HOME": str(home)})
    assert got == expected, f"settings lookup, {name}: got {got!r}, want {expected!r}"

repo = work / "repo"
repo.mkdir()
git = ["git", "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"]
subprocess.run(git + ["init", "-q"], cwd=repo, check=True)
subprocess.run(git + ["commit", "-q", "--allow-empty", "-m", "init"], cwd=repo, check=True)
linked = work / "linked"
subprocess.run(git + ["worktree", "add", "-q", "-b", "feature", str(linked)], cwd=repo, check=True)
got = run_bash(shared["workspace"], cwd=repo)
assert got == "workspace=root", f"workspace in the main checkout: got {got!r}"
top = subprocess.run(["git", "rev-parse", "--show-toplevel"], cwd=linked, capture_output=True, text=True,
                     check=True).stdout.strip()
got = run_bash(shared["workspace"], cwd=linked)
assert got == f"workspace=existing_worktree {top}", f"workspace in a linked worktree: got {got!r}"
PY
}

check_skills "$repo_root"

# the checker must reject broken copies of the skills.
fixture="$tmp/fixture"
make_fixture() {
	rm -rf "$fixture"
	for variant in claude codex; do
		mkdir -p "$fixture/assets/$variant/skills/loopai-t3"
		cp "$repo_root/assets/$variant/skills/loopai-t3/SKILL.md" "$fixture/assets/$variant/skills/loopai-t3/SKILL.md"
	done
}

expect_failure() {
	local expected="$1"
	local output
	if output="$(check_skills "$fixture" 2>&1)"; then
		printf 'expected loopai-t3 check to fail with: %s\n' "$expected" >&2
		exit 1
	fi
	if [[ "$output" != *"$expected"* ]]; then
		printf 'expected failure containing %q, got: %s\n' "$expected" "$output" >&2
		exit 1
	fi
}

# edit_fixture replaces the first occurrence of $2 with $3 in the $1 skill copy, failing when absent.
edit_fixture() {
	python3 - "$fixture/assets/$1/skills/loopai-t3/SKILL.md" "$2" "$3" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
text = path.read_text(encoding="utf-8")
assert sys.argv[2] in text, f"fixture edit target not found: {sys.argv[2]}"
path.write_text(text.replace(sys.argv[2], sys.argv[3], 1), encoding="utf-8")
PY
}

make_fixture
check_skills "$fixture"

printf '%s\n' 'loopai --t3-launch docs/plans/x.md' >>"$fixture/assets/codex/skills/loopai-t3/SKILL.md"
expect_failure "codex: removed launcher spelling: --t3-launch"

make_fixture
edit_fixture claude "it resumes the run with a fresh token." "it resumes the run."
expect_failure "claude: missing report phrase: fresh token"

make_fixture
edit_fixture codex "type \`resume\` in that thread" "send resume there"
expect_failure "codex: missing report phrase: type \`resume\` in that thread"

make_fixture
edit_fixture codex "The thread shows Working until the run ends." "The thread shows Working while it runs."
expect_failure "codex: report differs between Claude and Codex"

make_fixture
edit_fixture claude " --ttl 3d --label loopai" " --label loopai"
expect_failure "claude: Mode 1 launch missing"

make_fixture
edit_fixture claude '"Cancel": stop without launching.' 'Stop: do not launch.'
expect_failure "claude: the mode question must offer both modes, then Cancel"

make_fixture
edit_fixture claude 'base in ("loopai-acp", "loopai-acp.exe")' 'base in ("loopai-acp.exe",)'
edit_fixture codex 'base in ("loopai-acp", "loopai-acp.exe")' 'base in ("loopai-acp.exe",)'
expect_failure "settings lookup, loopai-acp"

make_fixture
edit_fixture claude 'if [ "${SANDBOX_RUNTIME:-}" = 1 ]' 'if [ -n "${SANDBOX_RUNTIME:-}" ]'
edit_fixture codex 'if [ "${SANDBOX_RUNTIME:-}" = 1 ]' 'if [ -n "${SANDBOX_RUNTIME:-}" ]'
expect_failure "environment check, SANDBOX_RUNTIME=0"

make_fixture
edit_fixture codex 'if [ -n "${CODEX_SANDBOX:-}" ]' 'if [ "${CODEX_SANDBOX:-}" = 1 ]'
expect_failure "codex: environment check differs between Claude and Codex"

echo "loopai-t3 skill tests passed"
