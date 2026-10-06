#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "$repo_root" <<'PY'
import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
topics = (
    "Summary", "Change scope", "Evidence", "Risk", "Merge danger",
    "Migrations and operational steps", "Plan deviation", "Backlog", "External review",
)
for provider, narration_heading, gate_heading in (
    ("claude", "Narrate the Completion Report", "Confirmation Gate"),
    ("codex", "Resolve the plan and report", "Confirm and execute"),
):
    path = root / "assets" / provider / "skills" / "loopai-merge" / "SKILL.md"
    text = path.read_text(encoding="utf-8")

    def section(heading):
        match = re.search(r"^## " + re.escape(heading) + r"\n(.*?)(?=^## |\Z)", text, re.M | re.S)
        assert match, f"{path}: missing {heading} section"
        return match.group(1)

    narration = section(narration_heading).lower()
    offset = 0
    for topic in topics:
        position = narration.find(topic.lower(), offset)
        assert position >= 0, f"{path}: missing or out-of-order narration topic {topic}"
        offset = position + len(topic)
    assert "older reports lack evidence and merge danger" in narration, f"{path}: missing old-report compatibility"
    assert "report those sections as absent, not inferred" in narration, f"{path}: missing no-inference rule"

    gate = section(gate_heading)
    reminder = gate.find("Immediately above the `Merge into <base>?` question")
    question = re.search(r"(?:ask exactly `|ask \u201c)Merge into <base>\?", gate)
    assert question and 0 <= reminder < question.start(), f"{path}: danger reminder must precede the question"
    assert "in one line: `Door: <reported value>; Blast radius: <reported value>`" in gate, f"{path}: missing danger values"
    assert "Include only values present in the report" in gate, f"{path}: missing partial-danger handling"
    assert "omit the line when neither is present" in gate, f"{path}: missing absent-danger handling"
    assert "never infer a missing value" in gate, f"{path}: danger values must not be inferred"
    assert "the pull request body is built from the report, with the legacy body as fallback" in gate, f"{path}: missing report-backed PR explanation"
    if provider == "claude":
        options = re.findall(r"^- `Open PR`.*$", gate, re.M)
        assert len(options) == 2, f"{path}: expected Open PR options with and without conflicts"
        assert all("body is built from the report" in option for option in options), f"{path}: both PR options must explain the body"
    else:
        assert "For the Open PR option in either case" in gate, f"{path}: PR explanation must cover both conflict outcomes"
        assert "AskUserQuestion" not in text, f"{path}: Codex must use prose confirmation"

print("check-merge-skill tests passed")
PY
