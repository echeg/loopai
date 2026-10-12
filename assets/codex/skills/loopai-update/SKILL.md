---
name: loopai-update
description: Smart-merge updated loopai embedded defaults into customized prompt and agent files in the user's loopai config directory. Use when the user asks to update loopai config, merge new defaults, or refresh customized prompts.
metadata:
  short-description: Merge new loopai defaults into custom prompts
---

# loopai-update - Smart Prompt Merging

**SCOPE**: update the loopai installation from its source checkout, then compare the current embedded defaults with the user's installed config and merge updates into the files they customized. Do not modify project source code, do not run loopai, and do not touch anything outside the config directory except through Step 0's `make install` and the pull the user approved.

## Skill Contract Check

Run this first and read the printed line:

```bash
expected=2
if ! command -v loopai >/dev/null 2>&1; then
  actual=missing
else
  actual=$(loopai --skill-contract 2>/dev/null) || actual=0
  case $actual in ''|*[!0-9]*) actual=0 ;; esac
fi
echo "skill contract: skill=$expected binary=$actual"
```

- `binary=missing`: continue with this skill's existing handling of a missing CLI.
- `binary` lower than `skill`: tell the user "the installed loopai binary is older than these skills; run `$loopai-update`", then continue.
- `binary` higher than `skill`: tell the user "these skills are older than the installed loopai binary; run `$loopai-update`, then restart the Codex session", then continue.
- equal: continue silently.

Never stop on a mismatch.

This skill is the update path itself, so do not tell the user to run it: instead of warning, run the snippet above again after Step 0, because `make install` may have replaced the binary, and report that second `skill contract:` line in the final summary.

## Step 0: Update the Installation

`make install` in the loopai checkout rebuilds and installs the binary and its ACP launcher, updates the Claude Code plugins when `claude` is installed, reinstalls these Codex skills, and verifies that the binary speaks the skill contract. Run it before merging prompts, so the defaults below come from the updated binary.

Locate the checkout: `$LOOPAI_SRC` first, then the `loopai` Claude Code marketplace when it is read from a local directory:

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

If no checkout was found, ask the user in plain text whether to use an existing checkout (a path that contains `scripts/install.sh`), clone a fresh one into a directory they name, or skip the installation update and only merge prompts. Wait for the answer. To clone:

```bash
git clone https://github.com/echeg/loopai <dir>
```

With a checkout, show its state, fetching first so the ahead/behind counts are current (a failed fetch is not fatal):

```bash
git -C <checkout> fetch --quiet || echo "fetch failed; upstream state may be stale"
git -C <checkout> status -sb
git -C <checkout> rev-list --count HEAD..@{u} 2>/dev/null || echo "no upstream"
```

Only when the count is above zero, ask whether to fast-forward the checkout with `git pull --ff-only` before installing, or install it as it is. Wait for the answer. Never pull without it, and never pull anything but `--ff-only`. If the pull fails, report its output and install the checkout as it is.

Then run the installer:

```bash
make -C <checkout> install </dev/null
```

Summarize its output: every `✓` line in one short list, every `!` line verbatim, since each one names something the user has to do (a standalone copy to remove, a `PATH` entry that shadows the installed binary, a plugin to install). Tell the user to restart open Codex sessions so they load the reinstalled skills.

Choose the binary Step 1 runs. When the output has the line `installed loopai and loopai-acp into <dir>`, the new binary is `<dir>/loopai` (on Windows the line names `loopai.exe` and the binary is `<dir>/loopai.exe`): Step 1 runs that absolute path, never a bare `loopai`, because a `PATH` entry that shadows it, or an install directory missing from `PATH`, would otherwise extract the old binary's defaults or none at all.

If it exits non-zero without that line (the build failed), report the failing line, continue to Step 1 with `loopai` from `PATH` only when `command -v loopai` succeeds, and tell the user the merge compares against the binary that is still installed. If no loopai binary exists at all, stop here. A non-zero exit after that line (a contract mismatch) still installed the new binary: report the failing line and use `<dir>/loopai`.

When the user skipped the update, continue to Step 1 with `loopai` from `PATH` only when `command -v loopai` succeeds; otherwise stop and tell them to install it from a checkout with `make install`.

## Step 1: Extract the Current Defaults

```bash
DUMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/loopai-defaults-XXXXXX")
LOOPAI_BIN=<binary chosen in Step 0>  # <dir>/loopai after an install, otherwise loopai
"$LOOPAI_BIN" --dump-defaults "$DUMP_DIR"
printf '%s\n' "$DUMP_DIR"
```

Keep the printed path; every later step needs it.

## Step 2: Locate the Config Directory

```bash
printf '%s\n' "${LOOPAI_CONFIG_DIR:-$HOME/.config/loopai}"
ls -la "${LOOPAI_CONFIG_DIR:-$HOME/.config/loopai}"
```

If the directory does not exist, loopai was never configured locally and there is nothing to update. Say so and stop.

## How loopai Config Files Work

loopai installs config, prompt, and agent files with every line commented out. At runtime `stripComments()` removes those lines, finds nothing, and falls back to the **embedded defaults** compiled into the binary. An all-commented file is functionally identical to a missing one.

So a loopai upgrade already takes effect automatically for every file the user never customized. Nothing needs to change for those.

A file is **customized** only when it holds at least one non-empty line that does not start with `#`. `--dump-defaults` writes the raw, uncommented embedded content for comparison.

## Step 3: Classify Every File

For each file in the dump (`config`, `prompts/*.txt`, `agents/*.txt`), compare it with the same-named file in the config directory. Compare **after** stripping `#`-prefixed lines from both sides - the shipped config mixes descriptive comments with values, and only the values matter.

- **Skip, do-nothing default** - the user file is missing, empty, or entirely comments and whitespace. Embedded defaults already cover it. A file present only in the dump is also do-nothing: do not offer to install it.
- **Skip, unchanged** - the stripped user content equals the stripped default.
- **Needs a smart merge** - the stripped user content differs from the stripped default.
- **Ignore** - a file present only in the config directory is user-created; leave it alone.

## Step 4: Present the Summary

```
loopai config update summary:

No changes needed (N files):
  prompts/task.txt, prompts/review_first.txt, agents/quality.txt, ...

Smart merge needed (N files):
  prompts/review_second.txt, agents/implementation.txt
```

When nothing needs merging, report that everything is current and go straight to cleanup.

Otherwise ask whether to review the merges one file at a time, or only show the diffs without changing anything. On the show-only choice, print each diff and skip to Step 6.

## Step 5: Merge, One File at a Time

For each customized file:

1. Read both versions - the new default and the user's file.
2. Work out what each side changed: what the user added or rewrote, and what moved structurally in the default (new sections, new `{{VARIABLE}}` references, removed guidance).
3. Propose a merged version that keeps the user's additions and tone, applies the default's structural changes, picks up new template variables, and flags any place both sides changed the same thing.
   - In `config`, translate keys that were removed instead of keeping or deleting them: loopai stops at startup on each, and deleting one silently changes which provider or model a run uses. Name every translation in the summary.
     - `executor` was removed: `executor = codex` becomes a `codex:` prefix on `task_model` (`task_model = codex:` when it is unset) and on every unprefixed `plan_model` or `review_model`; `executor = claude` needs only the `claude:` prefixes below.
     - `codex_model` and `codex_reasoning_effort` were removed: they filled any empty model or effort segment of a codex spec, so write them into every `codex:` phase spec and `external_reviewers` entry that leaves that segment empty; when codex is only the automatic reviewer, ask whether to pin it as an explicit `external_reviewers = codex:<model>[:<effort>]` entry.
     - `external_review_tool` and `external_review_model` were removed: `none` becomes an empty `external_reviewers =`, and `claude`, `codex`, or `custom` becomes `external_reviewers = <tool>[:<model>[:<effort>]]` from the old model value (`custom` carries no model); `auto` or no tool is dropped when the old model value is empty, because the automatic reviewer is the default, but a non-empty one configured that automatic reviewer, which now always runs at its default model, so ask whether to pin it as `external_reviewers = <provider>:<model>[:<effort>]` from the old model value, `<provider>` being the one other than the migrated `task_model`'s (`codex` when `task_model` is unset); a pinned entry makes a missing reviewer binary a startup error and ignores `codex_enabled`. Under `codex_enabled = false` ask too, because `--external-only` bypasses that gate and was the one mode where the old model value reached the reviewer: explain that pinning also turns external review on for every other run, while dropping it keeps those runs unreviewed and leaves `--external-only` at the default model unless that command passes `--external-reviewers=<provider>:<model>[:<effort>]`.
   - Rewrite a `plan_model`, `task_model`, or `review_model` value without a provider prefix as `claude:<value>` or `codex:<value>`: loopai stops at startup on it too. Use the provider the `executor` key that was removed named, if the file had one; under a wrapper `claude_command` use `claude:<value>`, since that wrapper ran every unprefixed spec; otherwise take it from the model name (`opus`, `sonnet`, `haiku`, `fable` are claude; `gpt-*` and `o3`-style names are codex). Ask when it is not obvious, and name every such change in the summary.
4. Show a short summary of each side's changes plus the proposed result.
5. Ask how to handle the file: accept the merge, keep the current version, or take the new default and discard the customization.
6. Apply the answer.

## Step 6: Clean Up

```bash
rm -rf "$DUMP_DIR"
```

```
Update complete:
  Installation:   <make install result, or "skipped">
  Skill contract: <the skill contract: line from after Step 0>
  Skipped:        N files (no changes needed)
  Smart-merged:   N files (M accepted, K kept)
```

## Merge Principles

- Keep content the user added that has no counterpart in the defaults.
- Apply structural changes from the defaults while preserving the user's custom content inside the new shape.
- Carry over new `{{VARIABLE}}` references; a prompt missing one silently loses that data.
- Preserve the user's wording and tone when they rewrote an instruction.
- Present both versions and let the user choose when the two sides conflict directly.
- When in doubt, keep both with clear markers rather than dropping information.
