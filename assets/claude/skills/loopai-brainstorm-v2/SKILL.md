---
name: loopai-brainstorm-v2
description: "Design a feature into a loopai implementation plan with few questions. Use when the user wants to design work that should become a loopai plan and usually accepts the recommended option, so the dialogue should ask only about direction while a grill round checks the rest; like loopai-brainstorm it hands off to loopai-plan and creates no spec file."
---

<!-- adapted from obra/superpowers v6.2.0 (MIT); see ../../../../THIRD_PARTY_NOTICES.md -->

# Brainstorming Ideas Into Loopai Plans, Few Questions

Turn an idea into an approved design, then pass that design directly to `loopai-plan`. Ask the user only what the repository cannot answer; decide the rest from the code and let a grill round check those decisions.

<HARD-GATE>
Do not invoke an implementation skill, write code, scaffold a project, or take any implementation action until you have presented a design and the user has approved it. The only skill invoked after brainstorming is `loopai-plan`.
</HARD-GATE>

## Skill Contract Check

Run this first and read the printed line:

```bash
expected=4
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

## Checklist

Complete these activities in order:

1. Explore project context by checking relevant files, documentation, and recent commits.
2. Assess whether the request fits one implementation plan; decompose oversized work before refining it.
3. Verify every open point against the code and classify it as a direction question or a mechanism fork.
4. Ask the direction questions one at a time, only those among purpose, scope, constraints, order of work, success criteria, and non-goals that the repository cannot answer; state the rest in the design.
5. Decide the mechanism forks yourself and show them in one decision table with one question.
6. Present the whole design in one message and get one approval.
7. Hand off to `loopai-plan` with a grill round pre-selected.

## Two Classes of Questions

Every open point is either a direction question or a mechanism fork. Classify it by one observable test: **can the repository answer it?**

- **Direction question: ask it.** The answer lives outside the repository: product intent, who the users are, whether live data or a running deployment exists, what to leave out, the order of several projects, acceptable risk. Ask these one per message, as before. They are the questions whose answers change the plan.
- **Mechanism fork: decide it.** The goal is agreed and the options are implementations of it, and the repository supplies the evidence: an existing pattern, a comment stating a principle, a similar earlier feature, a backlog entry, a test. Decide it, record the evidence, and do not ask.
- **Promotion.** A mechanism fork becomes a direction question when, after verification, no evidence favors one option. Mark a decided fork that is hard to reverse (schema, public API, data migration, external contract) with ⚠️ in the decision table so the user sees it at a glance; the marker does not by itself make it a question. A decided fork whose evidence touches user intent stays in the table; when the user needs to see it before answering a direction question, put it in that question's recommendation text rather than asking it on its own.

## Understand the Idea

- Inspect the current project before proposing changes. Follow its established patterns and review relevant files, documentation, tests, and recent commits.
- Assess scope before detailed questioning. If the request contains independent subsystems, propose an ordered split as one question: the user may take the first project, several, or all of them in one plan.
- Ask exactly one direction question per message. Prefer multiple-choice options with a recommendation, while allowing open-ended answers.
- Establish the goal, scope, constraints, success criteria, and important non-goals.
- Improve affected code boundaries when that directly serves the goal, but do not introduce unrelated refactoring.
- File adjacent problems noticed during exploration to the backlog instead of growing the design: create `docs/backlog/<kebab-slug>.md` (or the project's configured `backlog_dir`) with a `# <short problem title>` heading, then `- found: <YYYY-MM-DD>, plan: <plan-name>, phase: planning`, `- severity: minor|major`, `- area: <primary file or package>`, then a short description with file:line references and a suggested fix direction. List existing backlog files first and update a similar entry rather than duplicating it. Do not fix the problem now and do not pull it into scope. Commit that entry on its own right away with `git add <entry>` then `git commit -m "docs: add backlog entry" -- <entry>`, so anything the user had already staged stays out of this commit: loopai tolerates exactly one uncommitted path, the plan file, so an untracked backlog entry makes a later branch or worktree creation fail with `worktree has uncommitted changes`.

## Verify Before Deciding

Verification comes before the direction questions, not after them: a question written before the check may ask about something the code already decides. Before a fork is written down, in either class, check the base branch for an existing answer: grep for the concept and its likely names, read the module the change would touch, and scan recent commits on that area. Three outcomes:

- The code already solves it: record a verified fact with `file:line`, no fork.
- The code states a principle or has a pattern that favors one option: a mechanism fork, decided by that evidence.
- Nothing in the code favors an option: a direction question.

A fork written from memory of the branch or the earlier conversation, without this check, is the wrong shape: it asks the user to choose between an option and something that already exists.

## Decide Mechanism Forks

After the direction questions are answered, collect every mechanism fork into one message with one table:

| Decision | Chosen | Alternatives not taken | Evidence |
|---|---|---|---|
| short name of the fork | the option and one line of why | one line each | `file:line` or commit |

Apply YAGNI ruthlessly: remove features, abstractions, and extensibility that are not needed for the stated success criteria. Then ask one question with AskUserQuestion — "Accept these decisions?" — with the options "Accept all" (Recommended) and "Review one by one" (present each fork as a multiple-choice question in turn, as `loopai-brainstorm` does). The user can also name specific decisions to change through "Other"; revise those and show the table again.

## Present the Design

- Present the complete design in one message, divided into sections scaled to complexity: a few sentences for straightforward work and up to roughly 200-300 words for a nuanced area. There is no approval between sections.
- Cover the relevant architecture, components, interfaces, data flow, error handling, and testing strategy.
- Prefer small units with one clear purpose and explicit dependencies. Each unit should be understandable and testable without reading its internals.
- Keep the design focused on the approved goal and avoid speculative additions.
- End with one question using AskUserQuestion — "Approve this design?" — with the options "Approve" (Recommended) and "Revise": for "Revise" and for a comment through "Other", change the named sections and present the whole design again.

## Hand Off to Loopai Plan

After the design is approved:

1. Summarize the approved design as structured context for `loopai-plan`:
   - context and goals
   - chosen approach and why
   - rejected alternatives and their trade-offs
   - verified project facts and constraints
   - success criteria, non-goals, and testing expectations
   - the decision table
2. Invoke the `loopai-plan` skill with that summary and the instructions below as its argument text, so they travel with the invocation rather than resting on earlier conversation, and instruct it to record that context in the plan's `## Decisions` section and to add a `## Decision Log` section (no checkboxes there) with:
   - one **accepted** entry per decided mechanism fork, naming what was chosen and its evidence;
   - one **rejected** entry only for a point the user turned down in a direction question, with a reason the user actually stated; an option they merely did not pick carries no reason and is not logged.
   The alternatives of a decided fork are deliberately not logged as rejected: the grill critics skip a recorded rejection, and these alternatives were never judged by the user, so they must stay open to critique.
3. Instruct `loopai-plan` to ask nothing already answered and to run the grill round before its first launch question, as if the user had chosen "Grill the plan first". That round is the review the user no longer does by hand.

Do not create a design spec or any file under `docs/superpowers/specs/`. The loopai plan is the durable output of this workflow. A slim architecture decision record is allowed only as a rare exception when the user explicitly requests one for a long-lived architectural decision; it does not replace the `loopai-plan` handoff.
