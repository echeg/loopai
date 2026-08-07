---
name: loopai-brainstorm
description: "Design a feature through collaborative discovery and turn the approved design into a loopai implementation plan. Use when the user wants to explore or design work that should become a loopai plan; unlike superpowers brainstorming, this hands off directly to loopai-plan and creates no spec file."
---

<!-- adapted from obra/superpowers v6.2.0 (MIT); see ../../../../THIRD_PARTY_NOTICES.md -->

# Brainstorming Ideas Into Loopai Plans

Turn an idea into an approved design through natural dialogue, then pass that design directly to `loopai-plan`.

<HARD-GATE>
Do not invoke an implementation skill, write code, scaffold a project, or take any implementation action until you have presented a design and the user has approved it. The only skill invoked after brainstorming is `loopai-plan`.
</HARD-GATE>

## Checklist

Complete these activities in order:

1. Explore project context by checking relevant files, documentation, and recent commits.
2. Assess whether the request fits one implementation plan; decompose oversized work before refining it.
3. Ask clarifying questions one at a time to understand purpose, constraints, and success criteria.
4. Propose two or three approaches, lead with a recommendation, and explain the trade-offs.
5. Present the design in sections appropriate to its complexity and get approval after each section.

## Understand the Idea

- Inspect the current project before proposing changes. Follow its established patterns and review relevant files, documentation, tests, and recent commits.
- Assess scope before detailed questioning. If the request contains independent subsystems, help the user split it into ordered, coherent projects, then brainstorm only the first project through this flow.
- Ask exactly one question per message. Break a broad topic into multiple turns.
- Prefer multiple-choice questions when they make the decision easier, while allowing open-ended answers when needed.
- Establish the goal, scope, constraints, success criteria, and important non-goals.
- Improve affected code boundaries when that directly serves the goal, but do not introduce unrelated refactoring.

## Explore Approaches

- Propose two or three meaningfully different approaches with their trade-offs.
- Lead with the recommended approach and explain why it best fits the discovered context.
- Apply YAGNI ruthlessly: remove features, abstractions, and extensibility that are not needed for the stated success criteria.
- Ask the user which direction they approve before presenting the detailed design.

## Present the Design

- Present the design section by section, scaled to complexity: a few sentences for straightforward work and up to roughly 200-300 words for nuanced areas.
- Ask after each section whether it is correct so far. Revise or clarify before continuing when the user raises concerns.
- Cover the relevant architecture, components, interfaces, data flow, error handling, and testing strategy.
- Prefer small units with one clear purpose and explicit dependencies. Each unit should be understandable and testable without reading its internals.
- Keep the design focused on the approved goal and avoid speculative additions.

## Hand Off to Loopai Plan

After every design section is approved:

1. Summarize the approved design decisions as structured context for `loopai-plan`:
   - context and goals
   - chosen approach and why
   - rejected alternatives and their trade-offs
   - verified project facts and constraints
   - success criteria, non-goals, and testing expectations
2. Invoke the `loopai-plan` skill and instruct it to record that context in the plan's `## Decisions` section.
3. Let `loopai-plan` ask only for information still missing from the approved design.

Do not create a design spec or any file under `docs/superpowers/specs/`. The loopai plan is the durable output of this workflow. A slim architecture decision record is allowed only as a rare exception when the user explicitly requests one for a long-lived architectural decision; it does not replace the `loopai-plan` handoff.
