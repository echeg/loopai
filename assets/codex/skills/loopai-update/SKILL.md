---
name: loopai-update
description: Smart-merge updated loopai embedded defaults into customized prompt and agent files in the user's loopai config directory. Use when the user asks to update loopai config, merge new defaults, or refresh customized prompts.
metadata:
  short-description: Merge new loopai defaults into custom prompts
---

# loopai-update - Smart Prompt Merging

**SCOPE**: compare the current embedded defaults with the user's installed config and merge updates into the files they customized. Do not modify project source code, do not run loopai, and do not touch anything outside the config directory.

## Step 0: Verify the CLI

```bash
command -v loopai
```

If missing, install from source and stop until it succeeds:

```bash
git clone https://github.com/echeg/loopai && cd loopai && make build
install -d ~/.local/bin && install -m 0755 .bin/loopai ~/.local/bin/loopai
```

## Step 1: Extract the Current Defaults

```bash
DUMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/loopai-defaults-XXXXXX")
loopai --dump-defaults "$DUMP_DIR"
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
     - `external_review_tool` and `external_review_model` were removed: `none` becomes an empty `external_reviewers =`, `auto` or no tool is dropped because the automatic reviewer is the default, and `claude`, `codex`, or `custom` becomes `external_reviewers = <tool>[:<model>[:<effort>]]` from the old model value (`custom` carries no model).
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
  Skipped:      N files (no changes needed)
  Smart-merged: N files (M accepted, K kept)
```

## Merge Principles

- Keep content the user added that has no counterpart in the defaults.
- Apply structural changes from the defaults while preserving the user's custom content inside the new shape.
- Carry over new `{{VARIABLE}}` references; a prompt missing one silently loses that data.
- Preserve the user's wording and tone when they rewrote an instruction.
- Present both versions and let the user choose when the two sides conflict directly.
- When in doubt, keep both with clear markers rather than dropping information.
