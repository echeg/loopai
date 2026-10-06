#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# the contract checks run in python; each skill's history snippet is extracted into
# $tmp/snippets so the shell half can execute it against fixture history files.
mkdir -p "$tmp/snippets"
python3 - "$repo_root" "$tmp/snippets" <<'PY'
from pathlib import Path
import re
import sys
import textwrap

root = Path(sys.argv[1])
out = Path(sys.argv[2])
charset = "`^[A-Za-z0-9._:,+-]+$`"
skills = [("claude", name) for name in ("loopai-plan", "loopai-t3", "loopai-orca")]

history_parts = {}
for variant, name in skills:
    label = f"{variant}/{name}"
    text = (root / "assets" / variant / "skills" / name / "SKILL.md").read_text(encoding="utf-8")

    for expected in ("${LOOPAI_CONFIG_DIR:-$HOME/.config/loopai}/launch-history",
                     "loopai itself writes", "flag strings only, never tokens or paths",
                     "never write to it", "skipped, never offered", charset,
                     "newest first and the newest marked \"(Recommended)\"",
                     "at most four options", "\"Cancel\"", "\"Other\" is the manual-entry path"):
        assert expected in text, f"{label}: missing history contract: {expected}"

    blocks = [b for b in re.findall(r"```bash\n(.*?)```", text, re.S) if "launch-history" in b]
    assert len(blocks) == 1, f"{label}: expected exactly one history snippet, found {len(blocks)}"
    snippet = textwrap.dedent(blocks[0])
    assert "HISTORY=$(awk" in snippet, f"{label}: snippet must assign HISTORY"
    history_parts[label] = snippet[:snippet.index("' \"$HIST\" 2>/dev/null)") ]
    (out / f"{variant}-{name}.sh").write_text(snippet, encoding="utf-8", newline="\n")

    if name == "loopai-plan":
        step = re.search(r"^## Step 3: Offer to Start\n(.*?)(?=^## )", text, re.M | re.S)
        assert step, f"{label}: missing Step 3"
        step = step[1]
        assert "LAST_LAUNCHER=$(awk" in snippet, f"{label}: snippet must assign LAST_LAUNCHER"
        assert "\"(Recommended)\" goes to the option for `LAST_LAUNCHER`" in step, f"{label}: recommendation must follow the last launcher"
        assert "otherwise, including when `LAST_LAUNCHER` is empty, it goes to the first launcher offered" in step, f"{label}: recommendation fallback"
        order = [step.find(option) for option in ("\"Run in Orca now\"", "\"Run in T3 Code now\"",
                                                  "\"Start implementation here\"", "\"Not now\"")]
        assert -1 not in order and order == sorted(order), f"{label}: launcher options must stay Orca, T3 Code, here, not now"
        assert "(Recommended)\", only with" not in step, f"{label}: Orca must not be recommended unconditionally"
        assert "\"From .loopai/config: <FLAGS>\"" in step, f"{label}: config flags option"
        assert "`HISTORY` empty and `FLAGS` non-empty: do not ask" in step, f"{label}: config-only launch must not ask"
        assert "`HISTORY` and `FLAGS` both empty" in step, f"{label}: no-history question"
        assert "those skills must not ask again" in step, f"{label}: launcher skills must not re-ask"
    else:
        assert "LAST_LAUNCHER" not in text, f"{label}: only loopai-plan recommends a launcher"
        assert "Skip this entirely when `FLAGS` is non-empty" in text, f"{label}: explicit flags skip history"
        assert "when the `loopai-plan` skill invoked this skill" in text, f"{label}: loopai-plan choice must not be re-asked"
        assert "then launch with the empty `FLAGS` as before, without asking" in text, f"{label}: missing history fallback"

first = next(iter(history_parts.values()))
for label, part in history_parts.items():
    assert part == first, f"{label}: history snippet differs from {next(iter(history_parts))}"
PY

# fixture: newest first, with every kind of line the snippet must skip.
mkdir -p "$tmp/cfg" "$tmp/cli-only" "$tmp/missing"
printf '%s\n' \
  $'2026-10-06T12:00:00Z\tcli\t--task-model codex:gpt-6:high' \
  '' \
  $'2026-10-06T11:50:00Z\tbogus\t--task-model claude:opus' \
  $'2026-10-06T11:40:00Z\tt3\t--task-model opus:high' \
  $'2026-10-06T11:30:00Z\torca\t--task-model claude:opus:high --external-reviewers codex:gpt-6:high,claude:fable' \
  $'2026-10-06T11:20:00Z\tt3\t' \
  'malformed' \
  $'2026-10-06T11:10:00Z\tcli\t--task-model codex:gpt-6:high' \
  $'2026-10-06T11:00:00Z\tt3\t--task-model claude:x;rm' \
  $'2026-10-06T10:50:00Z\tcli\t--review-model claude:sonnet --review-model claude:haiku' \
  $'2026-10-06T10:40:00Z\tcli\t--external-reviewers' \
  $'2026-10-06T10:30:00Z\tcli\t--worktree x' \
  $'2026-10-06T10:20:00Z\tcli\t--review-model claude:sonnet' \
  $'2026-10-06T10:10:00Z\tcli\t--review-model claude:haiku' \
  > "$tmp/cfg/launch-history"
printf '%s\n' $'2026-10-06T12:00:00Z\tcli\t--task-model codex:gpt-6:high' > "$tmp/cli-only/launch-history"

expected_history=$'--task-model codex:gpt-6:high\n--task-model claude:opus:high --external-reviewers codex:gpt-6:high,claude:fable\n--review-model claude:sonnet'

# the trailing "." keeps command substitution from stripping an empty LAST_LAUNCHER's newline.
run_snippet() {
  LOOPAI_CONFIG_DIR="$2" bash -c '. "$1"; printf "%s\n--\n%s." "$HISTORY" "${LAST_LAUNCHER-unset}"' _ "$1"
}

for snippet in "$tmp"/snippets/*.sh; do
  name="$(basename "$snippet" .sh)"
  last_full="unset" last_cli="unset" last_missing="unset"
  if [[ "$name" == *-loopai-plan ]]; then
    last_full="t3" last_cli="" last_missing=""
  fi
  got="$(run_snippet "$snippet" "$tmp/cfg")"
  [[ "$got" == "$expected_history"$'\n--\n'"$last_full." ]] || { printf '%s: unexpected result for full fixture:\n%s\n' "$name" "$got" >&2; exit 1; }
  got="$(run_snippet "$snippet" "$tmp/cli-only")"
  [[ "$got" == $'--task-model codex:gpt-6:high\n--\n'"$last_cli." ]] || { printf '%s: unexpected result for cli-only fixture:\n%s\n' "$name" "$got" >&2; exit 1; }
  got="$(run_snippet "$snippet" "$tmp/missing")"
  [[ "$got" == $'\n--\n'"$last_missing." ]] || { printf '%s: missing file must yield no history:\n%s\n' "$name" "$got" >&2; exit 1; }
done

echo "launch-history skill tests passed"
