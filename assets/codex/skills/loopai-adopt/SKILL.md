---
name: loopai-adopt
description: "Convert a plan from another format (OpenSpec, spec-kit, GitHub or GitLab issue with a checklist, generic task list, free-form markdown, loopai backlog entry) into a loopai-format plan in docs/plans/. Use for adopt plan, convert plan to loopai, import plan as loopai."
metadata:
  short-description: Convert a foreign plan into loopai format
---

# loopai-adopt - Convert Plans Into loopai Format

**SCOPE**: read a source plan in another format and write a new loopai-format plan at `docs/plans/YYYYMMDD-<slug>.md`. The source is never modified. An existing target is never silently overwritten. Do not change code or run tests. The output is the prepared plan and its own Git commit.

Supported source shapes:

- **OpenSpec change** - a directory with `proposal.md`, `tasks.md`, and optional `specs/**/spec.md`
- **spec-kit spec** - a directory or file separating spec, plan, and tasks
- **GitHub or GitLab issue** - a URL, `#N`, or `owner/repo#N` whose body carries a checklist
- **Generic task list** - structured markdown or text with headings and bullet items
- **Free-form markdown** - prose with no fixed structure
- **loopai backlog entry** - a single-finding file under `backlog_dir` (`docs/backlog/` by default) carrying `found`/`plan`/`phase`, `severity`, and `area`; convert it as free-form and do not treat its small size as a scope ambiguity

## Step 0: Optional CLI Check

Informational only. A missing loopai must not block the conversion - do not prompt, wait, or exit.

```bash
command -v loopai
```

On failure, mention once that loopai is needed later to execute the plan, show the source install (`git clone https://github.com/echeg/loopai && cd loopai && make build && install -m 0755 .bin/loopai ~/.local/bin/loopai`), and continue immediately. On success say nothing.

## Step 1: Resolve the Source

Inspect the request and pick exactly one source, in this order:

1. **Full URL** - GitHub issue or PR via `gh issue view <url> --json title,body,labels` (or `gh pr view`); GitLab via `glab issue view <url>` (or `glab mr view`); any other URL only with `curl -fsSL` when it points at raw markdown, otherwise ask the user to paste the body.
2. **Bare `#N`** - detect the host from `git remote get-url origin` and use `gh issue view N --json title,body` or `glab issue view N`, falling back to the PR/MR view. When the remote lookup fails, ask which host it is, or for a qualified `owner/repo#N`, before re-resolving.
3. **Qualified `owner/repo#N`** - `gh issue view N --repo owner/repo` or `glab issue view N --repo group/project`.
4. **Existing path** - probe the literal argument with `test -e`. A file is read directly. For a directory, `ls -la` it: `proposal.md` plus `tasks.md` means OpenSpec; a single `*.md` is the source; otherwise ask which file inside is the plan.
5. **Bare name** - only when every check above failed. Search with `rg --files -g '*<name>*.md'` and similar. One match wins; several mean asking the user to pick; none means asking what they meant.
6. **No argument** - ask where the source plan is: pasted text, a file path, or an issue reference.

Record the source kind, its full content, and an identifier for the slug.

## Step 2: Detect the Format

Classify as OpenSpec, spec-kit, issue-with-checklist, generic task list, or free-form, using the signals in the list above. When signals conflict - a directory holding both a `proposal.md` and a clearly spec-kit `plan.md` - ask which format to use before drafting.

## Step 3: Confidence Guard - Ask Before Drafting

Scan for anything you cannot map confidently and ask **before** writing a draft. Never embed `???`, `TBD`, or `[FIXME]` in the output.

Typical uncertainties: which headings become Tasks rather than Overview or Technical Details; how to split a long flat list; a vague item such as "clean up the auth module"; a source mixing feature, refactor, and bug fix that may need several plans; a non-English source where prose translation is a choice (the structural `Task` keyword stays English regardless); a source over 1000 lines or under 10.

Offer concrete options as a numbered list. When the question is genuinely open-ended, list the possibilities and ask for a number. Ask, then draft - never the reverse.

## Step 4: Convert

Every converted plan must satisfy loopai's format rules:

- The file opens with a `# <Plan Title>` H1.
- Sections in order: `## Overview`, `## Context`, `## Development Approach`, `## Testing Strategy`, `## Progress Tracking`, optional `## Technical Details`, `## Implementation Steps`, optional `## Post-Completion`.
- Task headings use the structural form `### Task <N>: <title>`. The keyword is **always English** even when the rest of the plan is not: loopai's parser recognizes only `Task` and `Iteration`, so localized variants are never detected.
- Checkboxes appear **only inside Task sections**. One in Overview, Context, or success criteria makes the executor spawn extra iterations.
- Every Task ends with a write-tests checkbox and a run-project-tests checkbox, phrased generically since the target project may be in any language.
- The last Task is always `### Task <last>: Verify acceptance criteria`, re-running the suite and linter and confirming the Overview requirements.

Per-format mapping:

- **OpenSpec** - "Why" becomes Overview; "What Changes" becomes Context; `specs/**/spec.md` deltas become Technical Details; each top-level group in `tasks.md` becomes a Task with its sub-bullets as checkboxes.
- **spec-kit** - Specification becomes Overview and Context; the implementation plan becomes Technical Details; each Tasks phase becomes one Task.
- **Issue with checklist** - the title becomes the plan title with trailing punctuation dropped; prose above the first checklist becomes Overview; labels and repo become Context ("Adopted from issue #312"); H3 groupings become Tasks, and an ungrouped list is split into Tasks of five to seven items with synthetic titles. Preserve `- [x]` state.
- **Generic task list** - infer the heading and item styles, promote top-level groupings to `### Task N:`, normalize items to `- [ ]`, and preserve any done markers. Ask before drafting when the grouping is unclear.
- **Free-form** - infer intent, turn the opening paragraphs into Overview and the background into Context, then decompose the body into three to seven Tasks of three to six concrete checkboxes that map to phrases actually present in the source. Do not invent steps.

In every case, add the test checkboxes and the final verification Task even when the source has none.

## Step 5: Review the Draft

Write the draft to a temp file first. Each shell call runs in its own process, so shell variables do not survive between calls: capture the literal path that `mktemp` prints and substitute that exact string everywhere after.

A template ending in `XXXXXX` is portable; a suffix after it is treated literally by BSD `mktemp`, so generate first and rename:

```bash
TMP=$(mktemp "${TMPDIR:-/tmp}/loopai-adopt-XXXXXX") && mv "$TMP" "$TMP.md" && printf '%s\n' "$TMP.md"
```

Do not install an `EXIT` trap - the shell exits between calls and it would fire immediately. Clean up explicitly with `rm -f <draft-path>` after writing the target and on every cancel path.

When `command -v revdiff` succeeds, review the draft with it and read its stdout: empty means silent approval, non-empty means annotations to apply before re-running. Otherwise fall back to in-chat review - print the draft and ask to accept, revise (treating the next message as feedback), or reject - looping until accepted.

## Step 6: Write the Target

Build the filename from today's date as `YYYYMMDD` and a slug derived from the title: lowercase ASCII, words joined by `-`, articles and trailing punctuation dropped, about 50 characters at most.

Confirm the slug with the user before writing. If the target exists, offer to bump the suffix (`-v2`, `-v3`, checking both `docs/plans/` and `docs/plans/completed/` until clear), to pick a new slug, or to cancel. Never overwrite silently.

Sanity-check before writing: the draft must contain at least one `### Task <N>: <title>` line and at least one `- [ ]` checkbox under a Task. If either fails, return to Step 4 rather than writing the file.

```bash
mkdir -p docs/plans
```

Write the plan, remove the temp file, follow **Commit the Prepared Plan** below, and report:

```
Adopted plan: docs/plans/<final-name>.md

Source: <kind and identifier>
Tasks:  <N>

Next: run `loopai docs/plans/<final-name>.md` to execute.
```

## Commit the Prepared Plan

After writing the finished plan or applying agreed revisions, commit that plan on the current branch before offering execution or returning the final result. Do this even when execution is postponed, unless the user explicitly asked to leave it uncommitted. Commit once per finished preparation/revision round, not during drafting. This keeps other prepared plans from blocking loopai's clean-checkout checks.

From the repository root, set `PLAN_PATH` to the exact repository-relative output path and inspect its diff (read the file too when it is new). If that path has no changes, skip the commit. Otherwise run:

```bash
git --literal-pathspecs add -- "$PLAN_PATH"
git --literal-pathspecs commit --only -m "docs: save plan $(basename "$PLAN_PATH" .md)" -- "$PLAN_PATH"
```

The explicit file path and `--only` keep unrelated staged changes out of the commit. Never stage the whole plans directory, other plans, or source code; never push or stash as part of this step. Verify that this plan is clean afterward and report the commit hash with its path. If Git is unavailable, the destination is outside a repository, or the commit fails, keep the saved plan, explain why it remains uncommitted, and do not launch execution automatically. Do not bypass hooks or change Git configuration to force a commit.

## Edge Cases

A nonexistent path, an ambiguous bare name, a failed URL fetch, an unrecognizable directory, or conflicting format signals all mean asking the user rather than guessing. A source with no task-like content means asking whether to infer Tasks from prose or cancel. A source over 1000 lines gets a warning and a choice to proceed, summarize, or split; under 10 lines, a warning that the result will be sparse. Re-running on the same source uses today's date and never touches plans already in `completed/`.

Missing `gh`, `glab`, or `revdiff` each degrade to asking the user to paste content or to the in-chat review loop - none of them is fatal.

## Constraints

- Never modify the source plan or directory.
- Never write into `docs/plans/` without a user-confirmed slug, and never overwrite an existing target.
- Never embed placeholder markers - ask before drafting instead.
- Never assume the target project's language; test checkboxes stay generic.
- Never cite loopai's own source files in the converted plan.
- Do not run tests or linters or push. Commit only the prepared output plan as described above.
