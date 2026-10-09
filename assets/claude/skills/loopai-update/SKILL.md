---
description: Smart-merge updated loopai defaults into customized prompts/agents
allowed-tools: [Bash, Read, Write, Glob, AskUserQuestion]
---

# loopai-update - Smart Prompt Merging

**SCOPE**: Update the loopai installation from its source checkout, then compare current embedded defaults with user's installed config, and intelligently merge updates into customized files. Preserves user intent while incorporating structural changes.

## Skill Contract Check

Run this first and read the printed line:

```bash
expected=1
if ! command -v loopai >/dev/null 2>&1; then
  actual=missing
else
  actual=$(loopai --skill-contract 2>/dev/null) || actual=0
  case $actual in ''|*[!0-9]*) actual=0 ;; esac
fi
echo "skill contract: skill=$expected binary=$actual"
```

- `binary=missing`: continue with this skill's existing handling of a missing CLI.
- `binary` lower than `skill`: tell the user "the installed loopai binary is older than these skills; run `/loopai:loopai-update`", then continue.
- `binary` higher than `skill`: tell the user "these skills are older than the installed loopai binary; run `/loopai:loopai-update`, then `/reload-plugins` or restart the session", then continue.
- equal: continue silently.

Never stop on a mismatch.

This skill is the update path itself, so do not tell the user to run it: instead of warning, run the snippet above again after Step 0, because `make install` may have replaced the binary, and report that second `skill contract:` line in the final summary.

## Step 0: Update the Installation

`make install` in the loopai checkout rebuilds and installs the binary and its ACP launcher, updates the Claude Code plugins, reinstalls the Codex skills, and verifies that the binary speaks the skill contract. Run it before merging prompts, so the defaults below come from the updated binary.

Locate the checkout: `$LOOPAI_SRC` first, then the `loopai` marketplace when Claude Code reads it from a local directory:

```bash
src="${LOOPAI_SRC:-}"
marketplaces="$HOME/.claude/plugins/known_marketplaces.json"
if [ -z "$src" ] && [ -f "$marketplaces" ] && command -v jq >/dev/null 2>&1; then
  src=$(jq -r '.loopai // empty | select(.source.source == "directory") | .source.path // empty' "$marketplaces")
fi
if [ -n "$src" ] && [ -f "$src/scripts/install.sh" ]; then
  printf 'checkout: %s\n' "$src"
else
  printf 'checkout: not found\n'
fi
```

**If no checkout was found**, use AskUserQuestion:
- header: "Checkout"
- question: "Where is the loopai source checkout?"
- options:
  - label: "Enter a path"
    description: "Use an existing checkout; it must contain scripts/install.sh"
  - label: "Clone it"
    description: "Clone the repository and install from the new checkout"
  - label: "Skip"
    description: "Leave the installation as it is and only merge prompts"

For "Clone it", clone into a directory the user names and use it as the checkout:

```bash
git clone https://github.com/echeg/loopai <dir>
```

**With a checkout**, show its state, fetching first so the ahead/behind counts are current (a failed fetch is not fatal):

```bash
git -C <checkout> fetch --quiet || echo "fetch failed; upstream state may be stale"
git -C <checkout> status -sb
git -C <checkout> rev-list --count HEAD..@{u} 2>/dev/null || echo "no upstream"
```

Only when the count is above zero, use AskUserQuestion:
- header: "Pull"
- question: "The checkout is N commits behind its upstream. Fast-forward it before installing?"
- options:
  - label: "Pull"
    description: "Run git pull --ff-only, then install"
  - label: "Install as is"
    description: "Install the current checkout without pulling"

Never pull without that answer, and never pull anything but `--ff-only`. If the pull fails, report its output and install the checkout as it is.

Then run the installer:

```bash
make -C <checkout> install </dev/null
```

Summarize its output: every `✓` line in one short list, every `!` line verbatim, since each one names something the user has to do (a standalone copy to remove, a `PATH` entry that shadows the installed binary, a plugin to install). Keep its closing reminder about `/reload-plugins`.

Choose the binary Step 1 runs. When the output has the line `installed loopai and loopai-acp into <dir>`, the new binary is `<dir>/loopai` (on Windows the line names `loopai.exe` and the binary is `<dir>/loopai.exe`): Step 1 runs that absolute path, never a bare `loopai`, because a `PATH` entry that shadows it, or an install directory missing from `PATH`, would otherwise extract the old binary's defaults or none at all.

If it exits non-zero without that line (the build failed), report the failing line, continue to Step 1 with `loopai` from `PATH` only when `command -v loopai` succeeds, and tell the user the merge compares against the binary that is still installed. If no loopai binary exists at all, stop here. A non-zero exit after that line (a contract mismatch) still installed the new binary: report the failing line and use `<dir>/loopai`.

For "Skip", continue to Step 1 with `loopai` from `PATH` only when `command -v loopai` succeeds; otherwise stop and tell the user to install it from a checkout with `make install`.

## Step 1: Extract Current Defaults

Create temp directory and dump embedded defaults:

```bash
DUMP_DIR=$(mktemp -d /tmp/loopai-defaults-XXXX)
LOOPAI_BIN=<binary chosen in Step 0>  # <dir>/loopai after an install, otherwise loopai
"$LOOPAI_BIN" --dump-defaults "$DUMP_DIR"
echo "$DUMP_DIR"
```

Save the dump directory path for later use.

## Step 2: Determine Config Directory

Resolve the user's config directory:

```bash
# check environment variable first
echo "${LOOPAI_CONFIG_DIR:-}"
```

If `LOOPAI_CONFIG_DIR` is empty, use default:
- **macOS/Linux**: `~/.config/loopai/`

Verify the directory exists:
```bash
ls -la <config-dir>/
```

If it doesn't exist, inform user that loopai hasn't been configured yet and there's nothing to update.

## How loopai Config Files Work

loopai installs config, prompt, and agent files with all content **commented out** (every line prefixed with `# `). At runtime, `stripComments()` removes these lines, finds nothing, and falls back to **embedded defaults** compiled into the binary. These all-commented files are functionally identical to missing files — they are do-nothing placeholders.

When loopai is updated, new embedded defaults take effect **automatically** for every file that hasn't been customized. No file changes are needed.

A file is **customized** only if it contains at least one uncommented, non-empty line that was intentionally modified by the user. The `--dump-defaults` command produces the raw (uncommented) embedded content for comparison.

## Step 3: Compare Files

For each file in the defaults dump (`config`, `prompts/*.txt`, `agents/*.txt`), compare with the corresponding file in the user's config directory.

**Algorithm to detect customized files**: a file is customized if it contains at least one non-empty line that does NOT start with `#`. Files that are missing, empty, or contain only comment lines (`# ...`) and whitespace are do-nothing defaults.

**Classify each file into one of these categories:**

### Skip (do-nothing default)
- File is missing in user's config, OR
- File is empty, OR
- File contains only comments and whitespace (every non-empty line starts with `#`)
- **Action**: no action needed — embedded defaults handle it automatically

Note: files that exist only in the dump directory (no corresponding user file) are also do-nothing — do NOT offer to install them. Files that exist only in the user's config directory (no corresponding dump file) are user-created custom files — ignore them entirely.

### Skip (unchanged)
- File has uncommented content that matches the raw dump default
- **Action**: no action needed — user's file already matches current defaults

**How to compare**: strip all `#`-prefixed lines from BOTH the user's file and the dump file, then compare the remaining non-comment content. This handles the config file where the dump has descriptive comment lines mixed with value lines — only the actual values matter for comparison.

### Smart merge needed
- File has uncommented content that differs from the raw dump default (after stripping `#`-prefixed lines from both sides)
- **Action**: needs Claude to semantically analyze and propose merge

## Step 4: Present Summary

Show the user a summary with two groups:

```
loopai config update summary:

No changes needed (N files):
  prompts/task.txt, prompts/review_first.txt, agents/quality.txt, prompts/codex.txt, ...

Smart merge needed (N files):
  prompts/review_second.txt, agents/implementation.txt
```

If nothing needs merging, report "all config files are up to date — no changes needed" and skip to cleanup.

Otherwise, use AskUserQuestion to confirm proceeding:
- header: "Proceed"
- question: "Review smart merges? Each customized file will be reviewed one by one."
- options:
  - label: "Yes, proceed"
    description: "Review and merge customized files one at a time"
  - label: "Skip, just show details"
    description: "Show what changed without modifying anything"

If user selects "Skip, just show details": for each file needing smart merge, show the diff between the user's file and the new default, then skip to Step 6 (Cleanup) without modifying any files.

## Step 5: Process Smart Merges

For each customized file that needs merging:

1. **Read both versions** - the new default and the user's current version
2. **Analyze the differences semantically**:
   - What did the user customize? (added content, changed wording, different instructions)
   - What changed in the new default? (structural changes, new template variables, new sections, removed sections)
3. **Propose a merged version** that:
   - Preserves user additions not present in defaults
   - Applies structural/pattern changes from new defaults
   - Updates template variable references (e.g., new `{{VARIABLE}}` usage)
   - Preserves user's tone and style choices
   - Flags direct conflicts where both changed the same thing
   - In `config`, translates keys that were removed instead of preserving or deleting them: loopai stops at startup on each, and deleting one silently changes which provider or model a run uses. Name every translation in the summary:
     - `executor` was removed: `executor = codex` becomes a `codex:` prefix on `task_model` (`task_model = codex:` when it is unset) and on every unprefixed `plan_model` or `review_model`; `executor = claude` needs only the `claude:` prefixes below
     - `codex_model` and `codex_reasoning_effort` were removed: they filled any empty model or effort segment of a codex spec, so write them into every `codex:` phase spec and `external_reviewers` entry that leaves that segment empty; when codex is only the automatic reviewer, ask whether to pin it as an explicit `external_reviewers = codex:<model>[:<effort>]` entry
     - `external_review_tool` and `external_review_model` were removed: `none` becomes an empty `external_reviewers =`, and `claude`, `codex`, or `custom` becomes `external_reviewers = <tool>[:<model>[:<effort>]]` from the old model value (`custom` carries no model); `auto` or no tool is dropped when the old model value is empty, because the automatic reviewer is the default, but a non-empty one configured that automatic reviewer, which now always runs at its default model, so ask whether to pin it as `external_reviewers = <provider>:<model>[:<effort>]` from the old model value, `<provider>` being the one other than the migrated `task_model`'s (`codex` when `task_model` is unset); a pinned entry makes a missing reviewer binary a startup error and ignores `codex_enabled`. Under `codex_enabled = false` ask too, because `--external-only` bypasses that gate and was the one mode where the old model value reached the reviewer: explain that pinning also turns external review on for every other run, while dropping it keeps those runs unreviewed and leaves `--external-only` at the default model unless that command passes `--external-reviewers=<provider>:<model>[:<effort>]`
   - Rewrites a `plan_model`, `task_model`, or `review_model` value without a provider prefix as `claude:<value>` or `codex:<value>`: loopai stops at startup on it too. Use the provider the `executor` key that was removed named, if the file had one; under a wrapper `claude_command` use `claude:<value>`, since that wrapper ran every unprefixed spec; otherwise take it from the model name (`opus`, `sonnet`, `haiku`, `fable` are claude; `gpt-*` and `o3`-style names are codex). Ask when it is not obvious, and name every such change in the summary
4. **Show the user**:
   - Brief summary of what changed in defaults
   - Brief summary of what user customized
   - The proposed merged version
5. **Use AskUserQuestion** for each file:
   - header: "Merge"
   - question: "How to handle <filename>?"
   - options:
     - label: "Accept merge"
       description: "Use the proposed merged version"
     - label: "Keep mine"
       description: "Keep your current version unchanged"
     - label: "Use new default"
       description: "Replace with new default (discard customizations)"

6. Apply the user's choice

## Step 6: Cleanup

Remove the temp directory:

```bash
rm -rf <dump-dir>
```

Report final summary:
```
Update complete:
  Installation: <make install result, or "skipped">
  Skill contract: <the skill contract: line from after Step 0>
  Skipped: N files (no changes needed)
  Smart-merged: N files (M accepted, K kept)
```

## Merge Principles

When proposing smart merges, follow these rules:

- **Preserve user additions**: content the user added that doesn't exist in defaults should be kept
- **Apply structural changes**: if defaults restructured prompts (e.g., changed from sequential to parallel agents), apply the new structure while keeping user's custom content
- **Update template variables**: if new `{{VARIABLE}}` references were added to defaults, include them in the merge
- **Preserve user tone/style**: if user rewrote instructions in a different style, keep their style while incorporating new functionality
- **Flag conflicts clearly**: if both user and defaults changed the same section differently, present both versions and let the user choose
- **Don't lose information**: when in doubt, keep both versions with clear markers

## Constraints

- This command is ONLY for updating the loopai installation and its configuration files
- Do NOT modify any project source code
- Do NOT run loopai execution or review
- Do NOT touch files outside the config directory, except through Step 0's `make install` and the pull the user approved
- Always clean up the temp directory when done
