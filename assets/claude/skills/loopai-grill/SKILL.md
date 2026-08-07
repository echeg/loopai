---
name: loopai-grill
description: "Adversarially review or compare loopai plans. Use when the user asks to grill plan, прогрилить план, compare plans, or run a plan-off."
argument-hint: 'plan file or: compare <description>'
allowed-tools: [Bash, Read, Write, Edit, Glob, Grep, Agent, AskUserQuestion]
---

# Grill and Compare Loopai Plans

Improve a loopai implementation plan by combining independent Claude and Codex analysis. Operate only on plans outside `docs/plans/completed/` and never read from or write to `.loopai/`.

## Route the Request

Interpret the arguments before doing any critique or generation:

1. If the first argument is exactly `compare`, use plan-off mode. Everything after `compare` is the input. If it is empty or whitespace, stop and ask the user for a feature description or source plan path.
2. If any other argument is present, treat the complete argument as a plan path and use grill mode. Require the path to exist, to be a regular Markdown file under `docs/plans/`, and not to be under `docs/plans/completed/`. Paths are case-sensitive: do not guess a differently cased path. Report an invalid path and stop without editing anything.
3. With no arguments, use Glob to find `docs/plans/*.md`; do not search `docs/plans/completed/`. Sort candidates by modification time, present the newest candidate, and use AskUserQuestion to confirm it before continuing. If no candidate exists, explain that no active plan was found and stop.

Before invoking Codex, check for it with `command -v codex`. Never substitute `loopai --codex --plan`; this workflow needs non-interactive output that remains under this skill's control.

## Grill Mode

Read the complete selected plan. Critique it against the actual repository, not in isolation.

### Run Independent Critics

Launch these four critics concurrently. Give every critic the full plan content and tell it to return concise findings with evidence, impact, and a proposed plan change.

Use three Agent calls with distinct lenses:

- Feasibility: inspect the actual codebase and challenge paths, APIs, dependencies, ordering, compatibility assumptions, and whether each task is implementable as written.
- YAGNI and scope: identify speculative work, duplication, unnecessary abstractions, scope creep, and simpler ways to meet the Overview.
- Testability and granularity: check that tasks are cohesive, ordered, independently verifiable, sized for one loop iteration, and include concrete success and failure tests.

Run the agents in parallel. Tell each agent not to edit files.

In parallel with them, invoke `codex exec` non-interactively. Pass the plan through a temporary file outside the repository plan tree and use a prompt equivalent to:

> Critique this implementation plan adversarially. Find missing tasks, hidden dependencies, underestimated work, invalid repository assumptions, and weak tasks you can refute. Check the actual codebase before asserting a finding. Return concise findings with evidence, impact, and an exact proposed plan change. Do not edit files.

Create temporary files with `mktemp` under `${TMPDIR:-/tmp}`, record their literal paths because shell variables do not persist between tool calls, and remove them after their contents have been read. Quote paths and arguments. Capture Codex stdout and stderr separately so failures are visible.

### Degrade Explicitly

- If `codex` is absent, warn the user that cross-model critique is unavailable and continue with the three Claude critics.
- If `codex exec` exits unsuccessfully, times out, or produces unusable output, report the failure and its actionable error text. Continue with Claude findings only; never claim the Codex lens ran successfully and never silently discard the failure.
- A Claude critic failure does not invalidate successful critics. Report the failed lens and continue with the remaining results.

### Verify and Apply Findings

1. Merge semantically duplicate findings, keeping the clearest evidence and strongest proposed change.
2. Verify every surviving finding with a quick Read, Glob, or Grep check against the repository. Drop findings that are false, already addressed by the plan, purely stylistic, or unsupported by repository evidence.
3. Present the verified findings with short evidence and impact summaries using AskUserQuestion with `multiSelect: true`. Include no unverified finding. If the tool limits the number of choices, present findings in batches.
4. If the user selects none, leave the plan unchanged and summarize the completed review.
5. Apply only selected findings with Edit. Preserve the plan's intent and existing checked state. Prefer tightening existing tasks; add the smallest necessary task or checkbox when the work is genuinely missing.
6. Re-read the edited plan and confirm its task numbering, checkbox syntax, dependencies, and testing requirements remain coherent.

## Plan-Off Mode

Plan-off requires Codex. If `command -v codex` fails, stop before generating either plan and state that the `codex` binary is required for plan-off mode.

### Establish Shared Input

Treat the text after `compare` as either a source plan path or a feature description:

- For an existing Markdown plan under `docs/plans/` but outside `completed/`, read its complete `## Overview` as the requirements. Include any explicit constraints elsewhere in the plan that are necessary to interpret that Overview.
- Otherwise use the text verbatim as the requirements. A path-like input that does not exist or has the wrong case is an error, not a feature description; report it and stop.

Read the installed `loopai-plan` skill and extract its complete `Plan Structure` template and task rules. If the installed skill cannot be found, use `assets/claude/skills/loopai-plan/SKILL.md` from the current repository. Both candidates must receive the same requirements and the same template.

Create a unique temporary directory outside `docs/plans/` for candidate and judging output. Scratch drafts must never be discoverable by loopai. Remove the directory when the comparison is complete or fails.

### Generate Independent Candidates

Generate the candidates independently, without showing either model the other's draft:

1. Plan A: author with Claude after inspecting the repository. Follow the shared loopai template exactly, use repository-backed file references, make each Task section executable in one iteration, and give every implementation task explicit tests.
2. Plan B: run `codex exec` with the same requirements, repository root, and full template. Require Markdown only and prohibit file edits. Save the returned draft in the temporary directory.

If `codex exec` fails, report the command failure and actionable stderr, delete scratch files, and stop. Plan-off must not degrade to a one-model comparison.

Validate both drafts before judging: each must contain Overview, Context, Development Approach, Testing Strategy, Implementation Steps, numbered `### Task N:` sections with checkboxes, and Post-Completion. Repair only mechanical formatting in Claude's own draft. If the Codex draft is structurally invalid, make one explicit retry describing the validation errors; if it still fails, report the error and stop.

### Cross-Judge Symmetrically

Have Claude and Codex each score both complete drafts without knowing which model authored which one. Label them only Candidate A and Candidate B. Use exactly these criteria, each scored from 1 to 10:

- Completeness: covers all requirements and necessary integration work.
- Risk coverage: identifies dependencies, edge cases, failure modes, and compatibility concerns.
- Minimalism: avoids speculative scope and uses the simplest sufficient design.
- Testability: tasks are well-sized, ordered, specific, and verifiable with explicit tests.

Each judge must justify every score with concrete repository or draft evidence, identify the stronger candidate, and name useful ideas unique to the weaker candidate. Run the Claude judgment independently and run `codex exec` over both candidates with the identical rubric. A Codex judging failure is fatal: report it and stop rather than silently using one judge.

Summarize both judges' scores in one table with a row per judge and candidate, four criterion columns, a total, and a short verdict. Determine the winner by combined total across both judges. If totals tie, prefer the candidate with the higher combined completeness score, then risk coverage, then testability, then minimalism. If still tied, Claude selects the candidate with the more repository-specific evidence and explains the tie-break.

### Synthesize One Final Plan

1. Start from the winning candidate; do not rewrite it merely to blend prose styles.
2. Incorporate only the losing candidate's ideas that at least one judge identified as useful and that a quick repository check verifies. Keep the winner's simpler approach when ideas conflict without a demonstrated requirement.
3. Ensure the final result follows the loopai template, contains all requirements, has coherent ordering and numbering, and includes explicit success and error tests for every code-changing task.
4. Derive a concise lowercase hyphenated slug, then use AskUserQuestion to confirm the proposed `docs/plans/YYYYMMDD-<slug>.md` path. Use the local calendar date. Never overwrite an existing file without asking the user for a different slug.
5. Write exactly one final plan under `docs/plans/`. Do not write either candidate there.
6. Remove all scratch drafts and judging files, then report the final path and the score table.

Never edit an existing source plan during plan-off mode. Never create or modify anything under `.loopai/`.
