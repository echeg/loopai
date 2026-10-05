#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "$repo_root" <<'PY'
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path

root = Path(sys.argv[1])
def check_skill(host):
    path = root / f"assets/{host}/skills/loopai-retro/SKILL.md"
    text = path.read_text(encoding="utf-8")
    frontmatter = text.split("---", 2)[1]
    assert re.search(r"^name: loopai-retro$", frontmatter, re.M)
    if host == "claude":
        assert re.search(r"^disable-model-invocation: true$", frontmatter, re.M), "retro must be manually invoked"
        tools = re.search(r"^allowed-tools: \[(.*?)\]$", frontmatter, re.M)
        assert tools and set(tools.group(1).split(", ")) == {"Bash", "Read", "Glob", "Grep", "AskUserQuestion"}
        assert "[plan stem | progress log path | --last N]" in frontmatter
        for trigger in ("loopai-retro", "retro", "retrospective", "\u0440\u0435\u0442\u0440\u043e"):
            assert trigger in frontmatter, f"missing trigger: {trigger}"
    else:
        assert "explicit `$loopai-retro` request" in text, "retro must be manually invoked"
        for token in ("AskUserQuestion", "allowed-tools", "/loopai:", "Task tool"):
            assert token not in text, f"unported host token: {token}"
        interface = (path.parent / "agents/openai.yaml").read_text(encoding="utf-8")
        for key in ("display_name", "short_description", "default_prompt"):
            assert re.search(r'^  ' + key + r': "[^"\n]+"$', interface, re.M), f"missing UI key: {key}"
        assert "$loopai-retro" in interface
        assert re.search(r"^policy:\n  allow_implicit_invocation: false$", interface, re.M), "retro must not activate implicitly"


    def section(heading):
        match = re.search(r"^## " + re.escape(heading) + r"\n(.*?)(?=^## |\Z)", text, re.M | re.S)
        assert match, f"missing section: {heading}"
        return match.group(1)


    inputs = section("Select Inputs")
    for artifact in (
        "progress-<stem>.txt", "history/<stem>/archive-*.txt", "progress-<stem>.run.json",
        "docs/plans/completed/<stem>.report.md", "docs/backlog/*.md", "CLAUDE.md", "AGENTS.md",
        ".loopai/config", ".loopai/prompts/", ".loopai/agents/", "Makefile",
        ".github/workflows/", ".pre-commit-config.yaml",
    ):
        assert artifact in inputs, f"missing input: {artifact}"
    for rule in ("that log alone", "positive integer N, default 5", "top-level", "by modification time", "Do not recurse", "Missing `.run.json` is normal", "With no selected run artifact"):
        assert rule in inputs, f"missing selection boundary: {rule}"

    bounded = section("Read Bounded Evidence")
    for rule in ("Never read a progress log whole", "80 lines per window", "1,500 log lines total", "coverage limit", "Report sidecars and backlog entries may be read whole", "actual file line numbers"):
        assert rule in bounded, f"missing reading boundary: {rule}"
    commands = re.search(r"```bash\n(.*?)\n```", bounded, re.S).group(1).splitlines()
    assert len(commands) == 2
    markers = ("--- task iteration 1 ---", "validation:", "Completed:", "Failed:", "QUESTION:", "DRAFT REVIEW:", "TASK_FAILED", "stalemate", "limit", "retry", "warning:")
    # Execute the skill's examples: every structural marker is found, unrelated lines
    # stay out, hit output is capped, and bounded excerpts preserve source line numbers.
    with tempfile.TemporaryDirectory() as directory:
        log = Path(directory) / "log with spaces.txt"
        lines = ["unrelated output"] * 400
        for index, marker in enumerate(markers, start=121):
            lines[index - 1] = marker
        log.write_text("\n".join(lines) + "\n", encoding="utf-8")
        env = dict(os.environ, LOG=str(log))
        hits = subprocess.check_output(["bash", "-c", commands[0]], env=env, text=True).splitlines()
        assert len(hits) == len(markers)
        assert [int(hit.split(":", 1)[0]) for hit in hits] == list(range(121, 121 + len(markers)))
        window = subprocess.check_output(["bash", "-c", commands[1]], env=env, text=True).splitlines()
        assert len(window) == 61
        assert int(window[0].split()[0]) == 120 and int(window[-1].split()[0]) == 180
        log.write_text("validation: repeated\n" * 400, encoding="utf-8")
        hits = subprocess.check_output(["bash", "-c", commands[0]], env=env, text=True).splitlines()
        assert len(hits) == 200
        log.write_text("unrelated output\n" * 400, encoding="utf-8")
        assert not subprocess.check_output(["bash", "-c", commands[0]], env=env, text=True)

    categories = section("Identify Candidates")
    for category in (
        "Navigation pointers", "Automated checks", "Coding standards", "Bloated steering files",
        "No-op instructions", "Tool economy", "Information access", "Repeated reviewer finding",
        "Capped or stalled review", "Failed task retries", "Validation cost", "Avoidable human waits",
    ):
        assert re.search(r"^\| " + re.escape(category) + r" \| .+ \| .+ \|$", categories, re.M), f"missing category trigger/destination: {category}"
    assert "Do not count a canonical log and its archive as separate runs" in categories
    assert "cite both the observed failure and the relevant check definition or wiring" in categories

    output = section("Rank and Present")
    for requirement in ("**major**", "**minor**", "path:line", "short quotation", "proposed change", "where it belongs", "existing backlog entry", "stop without a selection question or writes"):
        assert requirement in output, f"missing output requirement: {requirement}"
    filing = section("File Only Selected Backlog Entries")
    for requirement in ("before writing", "nothing is selected writes nothing", "list existing files", "missing directory as empty", "Update a similar entry", "phase: retro", "severity: minor|major", "area:"):
        assert requirement in filing, f"missing filing requirement: {requirement}"
    if host == "claude":
        assert "AskUserQuestion" in filing and "multiSelect: true" in filing
    else:
        assert 'ask in prose:' in filing
        assert 'Wait for explicit selections before writing' in filing
        assert 'Silence is not a selection' in filing
    assert 'git add -- "$ENTRY"' in filing
    assert 'git commit -m "docs: add backlog entry" -- "$ENTRY"' in filing
    constraints = section("Constraints")
    for requirement in ("Read-only except the selected backlog entries", "Never edit prompts, agents, config, steering files, plans, or reports", "Never run loopai", "Never present a candidate without `path:line` evidence", "nothing is selected writes nothing"):
        assert requirement in constraints, f"missing constraint: {requirement}"


for host in ("claude", "codex"):
    check_skill(host)

print("check-retro-skill tests passed")
PY
