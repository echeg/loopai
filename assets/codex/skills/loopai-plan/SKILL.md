---
name: loopai-plan
description: Create a structured loopai implementation plan in docs/plans/ through interactive context gathering. Use when the user asks to plan a feature, bug fix, refactor, or migration, or explicitly requests a loopai plan.
metadata:
  short-description: Create a loopai plan
---

# loopai-plan - Create an Implementation Plan

Produce a plan, not an implementation. The output is one file at `docs/plans/YYYYMMDD-<slug>.md`.

## Prerequisites

```bash
command -v loopai
```

Missing loopai does not block planning. Mention the source install once and continue:

```bash
git clone https://github.com/echeg/loopai && cd loopai && make build
install -d ~/.local/bin && install -m 0755 .bin/loopai ~/.local/bin/loopai
```

## Step 0: Read Intent, Then the Repository

Classify the request first - feature, bug fix, refactor, migration, or unclear - then gather context yourself with `rg`, `ls`, `git log`, and `git status`. Read repository instructions (`AGENTS.md`, `CLAUDE.md`, `README.md`) before proposing anything.

What to look for, by intent:

- **Feature**: existing code with the same shape, project structure, affected components and their dependencies.
- **Bug fix**: failing tests, error output, stack traces, and recent commits touching the suspect area.
- **Refactor or migration**: every affected file, the test coverage protecting it, and the integration points.
- **Unclear**: `git status`, recently modified files, and the primary language and framework.

Summarize what you found: work in progress, files involved, apparent goal, patterns worth following.

**File adjacent problems to the backlog instead of widening the plan.** A real issue outside this plan's goal goes to `docs/backlog/<kebab-slug>.md` (or the project's configured `backlog_dir`): a `# <short title>` heading, then `- found: <YYYY-MM-DD>, plan: <plan-name>, phase: planning`, `- severity: minor|major`, `- area: <primary file or package>`, then a short description with file:line references and a fix direction. List the existing backlog files first and update a similar entry rather than duplicating it. Do not fix it now. Commit the entry on its own immediately:

```bash
git add <entry> && git commit -m "docs: add backlog entry" -- <entry>
```

The pathspec keeps anything the user had already staged out of that commit. Commit it before loopai runs: loopai tolerates exactly one uncommitted path, the plan file, so an untracked backlog entry makes branch and worktree creation fail with `worktree has uncommitted changes`.

## Step 1: Ask Focused Questions

Show the discovered context, then ask **one question per message**, waiting for each answer. Offer concrete options as a numbered list with a recommendation, and accept a number in reply.

1. **Goal** - what is the main outcome?
2. **Scope** - which components and files are involved?
3. **Constraints** - requirements or limitations that shape the design?
4. **Testing approach** - TDD (tests first) or regular (code first, then tests)?
5. **Title** - a short descriptive name for the plan.

When the request arrives from `$loopai-brainstorm`, its approved design is authoritative: preserve the goal, scope, constraints, chosen approach, and rejected alternatives, and ask only for what is genuinely missing.

## Step 1.5: Explore Approaches

Propose two or three meaningfully different approaches with trade-offs, lead with a recommendation and its reasoning, and ask which direction to take before drafting.

Skip this step when the approach is obvious, the user already specified it, or it is a bug fix with one clear solution. Skip it when invoked from `$loopai-brainstorm` and carry that dialogue's approved and rejected approaches into `## Decisions` instead.

## Step 2: Write the Plan File

Check `docs/plans/` for collisions, then write `docs/plans/YYYYMMDD-<slug>.md`.

### Required structure

```markdown
# <Plan Title>

## Overview
- what is being built and the problem it solves
- how it fits the existing system

## Decisions
<!-- Optional. Record an approved design from $loopai-brainstorm here. -->
- **Context**: why a decision was needed
- **Chosen approach**: the approved direction and its rationale
- **Rejected alternatives**: what else was considered and why it lost
- **Verified facts**: what discovery established that constrains the work

## Context (from discovery)
- files and components involved
- patterns found in the codebase
- dependencies identified

## Development Approach
- **Testing approach**: TDD or regular, from the answer above
- complete each task fully before starting the next
- **every task MUST include new or updated tests** for the code it changes -
  success and error paths, listed as their own checklist items
- **all tests must pass before the next task starts**
- update this plan file when scope changes during implementation

## Testing Strategy
- unit tests are required for every code-changing task
- when the project has UI e2e tests, changes to UI or its backing endpoints
  update those tests in the same task, held to the same standard

## Progress Tracking
- mark finished items `[x]` immediately
- prefix newly discovered tasks with a plus sign, blockers with a warning sign
- keep the plan in sync with the work actually done

## Decision Log
<!-- Omit entirely from a first-time plan. Add it only after the plan was revised
     following critique or draft review, then one list item per point:
     - <YYYY-MM-DD> <source>: **accepted** - <what changed and where>
     - <YYYY-MM-DD> <source>: **rejected** - "<the point>" - <one-line reasoning>
     NEVER put checkboxes in this section. -->

## Implementation Steps

### Task 1: <specific name saying what the task accomplishes>
- [ ] <concrete action naming the file>
- [ ] <concrete action naming the file>
- [ ] write tests for the new behavior (success cases)
- [ ] write tests for error and edge cases
- [ ] run project tests - must pass before task 2

### Task N-1: Verify acceptance criteria
- [ ] verify every requirement from Overview is implemented
- [ ] verify edge cases are handled
- [ ] run the full test suite
- [ ] run e2e tests when the project has them
- [ ] run the project linter - every issue must be fixed

### Task N: <Final> Update documentation
- [ ] update README and project docs when behavior changed

## Technical Details
- data structures, parameters, formats, processing flow

## Post-Completion
*Manual or external work - no checkboxes, informational only*
- manual verification, load or security review
- consuming projects, deployment config, third-party checks
```

### Format rules loopai enforces

- Task headings must use the structural English keyword: `### Task N: <title>` or `### Iteration N: <title>`. Localized keywords are not parsed, even when the rest of the plan is in another language.
- **Checkboxes belong only inside Task sections.** A checkbox in Overview, Context, Decisions, Decision Log, or success criteria causes extra executor iterations. `FileHasUncompletedCheckbox` counts a stray checkbox above `## Implementation Steps`, so such a plan never reads as complete.
- One task is one logical unit - a function, an endpoint, a component - with roughly five checkboxes.
- Each task ends with its own test checkboxes and a "run project tests" checkbox, phrased generically since the project may be in any language.
- loopai moves the finished plan to `docs/plans/completed/` itself.

## Commit the Prepared Plan

After writing the finished plan or applying agreed revisions, commit that plan on the current branch before offering execution or returning the final result. Do this even when execution is postponed, unless the user explicitly asked to leave it uncommitted. Commit once per finished preparation/revision round, not during drafting. This keeps other prepared plans from blocking loopai's clean-checkout checks.

From the repository root, set `PLAN_PATH` to the exact repository-relative output path and inspect its diff (read the file too when it is new). If that path has no changes, skip the commit. Otherwise run:

```bash
git --literal-pathspecs add -- "$PLAN_PATH"
git --literal-pathspecs commit --only -m "docs: save plan $(basename "$PLAN_PATH" .md)" -- "$PLAN_PATH"
```

The explicit file path and `--only` keep unrelated staged changes out of the commit. Never stage the whole plans directory, other plans, or source code; never push or stash as part of this step. Verify that this plan is clean afterward and report the commit hash with its path. If Git is unavailable, the destination is outside a repository, or the commit fails, keep the saved plan, explain why it remains uncommitted, and do not launch execution automatically. Do not bypass hooks or change Git configuration to force a commit.

## Step 3: Offer to Start

Detect Orca before speaking: `ORCA=${ORCA_CLI_COMMAND:-orca}`, and on Linux outside an Orca terminal use `orca-ide` instead of bare `orca`, which is the GNOME screen reader there. Orca is available when `command -v "$ORCA"` succeeds. Do not call `orca status`; `$loopai-orca` runs its own preflight. T3 Code is available when `${T3CODE_HOME:-$HOME/.t3}/userdata/server-runtime.json` exists; do not contact the server, since `$loopai-t3` runs its own preflight.

Derive `FLAGS` from the effective configuration - the first uncommented `key = value` for `executor`, `task_model`, `review_model`, and `external_reviewers`, taken from `<repo root>/.loopai/config` and falling back to `${LOOPAI_CONFIG_DIR:-$HOME/.config/loopai}/config`:

```bash
cfgval() {
  for f in "$(git rev-parse --show-toplevel)/.loopai/config" "${LOOPAI_CONFIG_DIR:-$HOME/.config/loopai}/config"; do
    v=$(grep -E "^[[:space:]]*$1[[:space:]]*=" "$f" 2>/dev/null | head -1 | sed -E 's/^[^=]*=[[:space:]]*//; s/[[:space:]]+$//')
    [ -n "$v" ] && { printf '%s\n' "$v"; return; }
  done
}
```

Map `executor = codex` to `--codex`, and the other three keys to `--task-model <v>`, `--review-model <v>`, `--external-reviewers <v>`. Skip a key with no value, and skip a value that does not match `^[A-Za-z0-9._:,+-]+$` - `$loopai-orca` and `$loopai-t3` reject it - naming the skipped key. Join with single spaces.

With Orca available, report the plan path and the launch line `$loopai-orca <plan> <FLAGS>`; with T3 Code available, also the line `$loopai-t3 <plan> <FLAGS>`. When `FLAGS` came out empty, add one line saying no `executor`/`task_model`/`review_model`/`external_reviewers` is set, so the run would use loopai defaults.

Then ask how to proceed: run it in Orca now, run it in T3 Code now (each only when available), start implementing here from task 1, or stop. Without Orca or T3 Code, offer the plain `loopai <plan>` run instead.

## Key Principles

- One question per message; multiple choice beats open-ended.
- YAGNI ruthlessly - cut features, abstractions, and extensibility the success criteria do not need.
- Have an opinion: lead with a recommendation and its reasoning, then let the user decide.
- When code would repeat, ask whether the user prefers duplication (simpler, uncoupled) or abstraction (DRY, more indirection) rather than deciding silently.
