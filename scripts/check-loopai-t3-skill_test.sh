#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 - "$repo_root" <<'PY'
from pathlib import Path
import re
import sys

root = Path(sys.argv[1])
reports = []
for variant in ("claude", "codex"):
    path = root / "assets" / variant / "skills/loopai-t3/SKILL.md"
    text = path.read_text(encoding="utf-8")

    def section(title):
        match = re.search(r"^## " + re.escape(title) + r"\n(.*?)(?=^## |\Z)", text, re.M | re.S)
        assert match, f"{variant}: missing {title}"
        return match[1]

    scope = next(line for line in text.splitlines() if line.startswith("**SCOPE**"))
    assert "provider session" in scope and "terminal" in scope, f"{variant}: scope must cover both modes"
    launch = section("Step 3: Launch")
    assert 'loopai --t3-launch $FLAGS "$PLAN"' in launch, f"{variant}: skill must retain automatic launch"
    for expected in ("`auto`", "userdata/settings.json", "providerInstances", "`grok`",
                     "`loopai-acp`", "`loopai-acp.exe`", "`config.enabled` is not false",
                     "terminal fallback", "opens no terminal", "does not wait for the run"):
        assert expected in launch, f"{variant}: missing launch contract: {expected}"

    token = section("Step 2: Resolve the Token")
    assert "no token or environment is placed in the thread" in token, f"{variant}: agent token boundary"
    assert "In terminal mode the token lives" in token, f"{variant}: terminal token lifetime"

    report = section("Step 4: Report")
    assert 'Use the `mode:` line from Step 3' in report, f"{variant}: use actual mode output"
    blocks = re.findall(r"```\n(.*?)\n```", report, re.S)
    assert len(blocks) == 3, f"{variant}: common, agent and terminal report blocks required"
    common, agent, terminal = blocks
    assert "Mode:" in common, f"{variant}: common report must identify mode"
    for expected in ("Working state", "plan\nsteps", "streamed reasoning", "final message",
                     "stop button cancels the run", "loopai does not update"):
        assert expected in agent, f"{variant}: missing agent report behavior: {expected}"
    assert "terminal" not in agent and "token" not in common + agent and "thread title follows" not in common, f"{variant}: agent report leaks terminal assumptions"
    assert 'thread title shows' in terminal and '"loopai" terminal' in terminal, f"{variant}: terminal progress guidance"
    reports.append(blocks)

    pitfalls = section("Pitfalls")
    assert "`--t3-launch=terminal`" in pitfalls and "ignores provider settings" in pitfalls, f"{variant}: terminal escape hatch"
    closeout = section("Close-out (tell the user, do not run)")
    assert "apply only to terminal mode" in closeout, f"{variant}: T3 reporting must be mode-specific"

assert reports[0] == reports[1], "Claude and Codex must report the same mode behaviors"
print("loopai-t3 skill tests passed")
PY
