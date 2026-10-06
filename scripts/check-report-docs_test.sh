#!/usr/bin/env bash

set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "$repo_root" <<'PY'
import re
import sys
import unittest
from pathlib import Path

root = Path(sys.argv[1])
docs = {name: (root / name).read_text(encoding="utf-8") for name in ("README.md", "llms.txt", "CLAUDE.md")}
inventory = {
    host: {path.parent.name for path in (root / f"assets/{host}/skills").glob("*/SKILL.md")}
    for host in ("claude", "codex")
}
prompt = (root / "pkg/config/defaults/prompts/report.txt").read_text(encoding="utf-8")
sections = [title or heading for title, heading in re.findall(r"^(?:# (Report: <plan title>)|## ([^\n]+))$", prompt, re.M)]


def skill_list(text, start, end, prefix=""):
    block = text.split(start, 1)[1].split(end, 1)[0]
    return set(re.findall(r"`" + re.escape(prefix) + r"(loopai(?:-[a-z0-9]+)?)`", block))


def check_inventories(contents):
    readme = contents["README.md"]
    counts = {"eight": 8, "nine": 9, "ten": 10}
    claude_count = re.search(r"The plugin provides (\w+) skills:", readme).group(1).lower()
    codex_count = re.search(r"(\w+) skills are installed:", readme).group(1).lower()
    assert counts[claude_count] == len(inventory["claude"]), "README Claude count is stale"
    assert counts[codex_count] == len(inventory["codex"]), "README Codex count is stale"
    assert skill_list(readme, "The plugin provides", "Use `/loopai:loopai-merge", "loopai:") == inventory["claude"]
    assert skill_list(readme, "skills are installed:", "Invoke them as") == inventory["codex"]
    assert skill_list(contents["llms.txt"], "The plugin provides", "The CLI remains", "loopai:") == inventory["claude"]
    assert skill_list(contents["llms.txt"], "It installs", "`loopai-grill` stays", "$") == inventory["codex"]
    assert skill_list(contents["CLAUDE.md"], "The current set is", "; every added skill") == inventory["claude"]


def check_reports(contents):
    for name, text in contents.items():
        # Compare each documented complete heading list with the canonical prompt,
        # allowing line wrapping inside a heading but preserving its order.
        match = re.search(r"`# Report: <plan title>`(.*?)`Validation`", text, re.S)
        assert match, f"{name}: missing report contract"
        actual = ["Report: <plan title>"] + [" ".join(h.split()) for h in re.findall(r"`([^`]+)`", match.group(1))] + ["Validation"]
        assert actual == sections, f"{name}: stale report section order: {actual}"
        assert "eleven" in text and "ten level-two headings" in text
        assert not re.search(r"nine-section|these nine\s+sections", text)
        assert "Door: one-way | two-way" in text and "Blast radius" in text
        assert "refs/heads/<branch>" in text and "in-memory report" in text
        assert "65,536" in text and "legacy" in text and "<details>" in text
        assert re.search(r"(?:drops|drop) both `<details>`\s+blocks.*?then.*?legacy", text, re.S), f"{name}: missing body degradation order"
        assert "read-only" in text and "only writes" in text and "path:line" in text
    assert "/loopai:loopai-retro [plan stem | progress log path | --last N]" in contents["README.md"]
    assert "$loopai-retro [plan stem | progress log path | --last N]" in contents["README.md"]
    assert "manually" in contents["README.md"] and "explicitly" in contents["README.md"]
    assert "manually" in contents["CLAUDE.md"] and "explicitly" in contents["CLAUDE.md"]
    assert "Manually invoke" in contents["llms.txt"] and "explicit `$loopai-retro`" in contents["llms.txt"]
    for symbol in ("closeoutTarget.report", "Runner.Report()", "locateCompletionReport", "maxPRPlanSize", "maxPRBodyRunes"):
        assert symbol in contents["CLAUDE.md"], f"missing developer pointer: {symbol}"


class ReportDocumentationTests(unittest.TestCase):
    def test_documented_inventories_match_assets(self):
        check_inventories(docs)

    def test_documented_report_contract_matches_prompt(self):
        check_reports(docs)

    def test_stale_skill_count_is_rejected(self):
        stale = dict(docs, **{"README.md": docs["README.md"].replace("provides ten skills", "provides eight skills")})
        with self.assertRaisesRegex(AssertionError, "count is stale"):
            check_inventories(stale)

    def test_missing_retro_skill_is_rejected(self):
        stale = dict(docs, **{"llms.txt": docs["llms.txt"].replace("`$loopai-retro`", "`$missing`")})
        with self.assertRaises(AssertionError):
            check_inventories(stale)

    def test_stale_report_order_is_rejected(self):
        stale = dict(docs, **{"CLAUDE.md": docs["CLAUDE.md"].replace("`Evidence`, `Risk`", "`Risk`, `Evidence`")})
        with self.assertRaisesRegex(AssertionError, "stale report section order"):
            check_reports(stale)


unittest.main(argv=[sys.argv[0]])
PY
