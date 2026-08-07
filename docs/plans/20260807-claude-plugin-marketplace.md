# Claude Plugin Marketplace for loopai

## Overview

Make the loopai repository a self-hosted Claude Code plugin marketplace that replaces the upstream `ralphex` plugin, and add a custom `loopai-brainstorm` skill that feeds directly into `loopai-plan`.

Today the user installs umputun's `ralphex` plugin whose skills reference the upstream CLI (`which ralphex`, brew install) and whose plan-creation flow competes with the superpowers `brainstorming` → `writing-plans` pipeline (producing duplicate plan formats and spec files, as observed in real projects). The fork already ships rebranded skills in `assets/claude/skills/` (`loopai`, `loopai-plan`, `loopai-adopt`, `loopai-update`) but has no `.claude-plugin/` manifests — upstream packaging infrastructure was intentionally dropped during the fork rebrand.

This plan adds:

- `.claude-plugin/marketplace.json` and `.claude-plugin/plugin.json` so `claude plugin marketplace add echeg/loopai` installs the `loopai` plugin straight from this repository (same single-repo pattern upstream uses: marketplace declares one plugin with `source: "./"`, plugin points `skills` at `./assets/claude/skills/`)
- a new `loopai-brainstorm` skill adapted from superpowers `brainstorming` v6.2.0 (MIT, attributed): one-question-at-a-time design dialogue, 2-3 approaches with recommendation, section-by-section design approval — but the terminal state is invoking the `loopai-plan` skill, and approved design decisions are captured in a `## Decisions` section of the resulting plan file; no spec files and no `writing-plans` handoff
- a `## Decisions` section in the `loopai-plan` plan template so the brainstorm output has a defined landing place
- documentation and migration notes (install own marketplace, remove the upstream `ralphex` plugin)

Deliberately out of scope: superpowers stays installed and untouched — no other skills are copied (teammates may have superpowers; local removal would not help them, and the orthogonal skills do not conflict with loopai).

## Context (from discovery)

- Files/components involved:
  - `.claude-plugin/marketplace.json`, `.claude-plugin/plugin.json` — new; upstream reference shapes captured from the installed plugin cache: marketplace `{name, owner, plugins:[{name, source:"./", ...}]}`, plugin `{name, version, ..., skills:"./assets/claude/skills/"}`
  - `assets/claude/skills/` — existing four loopai skills already rebranded (loopai CLI, source-build install from `echeg/loopai`)
  - `assets/claude/loopai*.md` — top-level symlinks to `skills/<name>/SKILL.md`; `make check-symlinks` enforces alignment of command name, directory name, and link target
  - superpowers `brainstorming` skill source: `~/.claude/plugins/cache/claude-plugins-official/superpowers/6.2.0/skills/brainstorming/SKILL.md` (MIT, Jesse Vincent) — adaptation source, read at implementation time
- Related patterns found:
  - single-repo marketplace (`source: "./"`) proven by upstream umputun/ralphex 0.20.0
  - fork policy: user-visible surfaces are `loopai`-branded; Go module path and `<<<RALPHEX:...>>>` signals untouched — plugin manifests are user-visible surface, so `loopai` naming is required
  - marketplace name must be `loopai` (distinct from `ralphex`) so both marketplaces can coexist during migration
- Dependencies identified: none new; validation tooling should reuse existing make targets/scripts style

## Development Approach

- **Testing approach**: TDD where testable (asset validation first, then assets); skill prose itself is validated by structural checks, not unit tests
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated checks** for the assets it adds
  - manifest JSON validity and required fields are covered by an automated check wired into `make test`
  - symlink alignment for the new skill is covered by `make check-symlinks`
  - checks cover both success and failure scenarios where scriptable
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- Maintain fork policy: no changes to the Go module path or protocol signals; `CHANGELOG.md` untouched
- Preserve attribution: adapted skill carries "adapted from obra/superpowers v6.2.0 (MIT)" in its header comment
- Keep the existing four skills' content unchanged except where the brainstorm handoff requires an explicit hook

## Testing Strategy

- **Unit tests**: not applicable to prose skills; automated structural checks instead (JSON validity, required manifest fields, symlink alignment, skill frontmatter presence)
- **E2E tests**: not applicable (no Go code changes expected); if any Go/Makefile helper is added, it gets a standard test
- Manual verification of the actual plugin install flow is in Post-Completion (requires the pushed repo)

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## Validation Commands

- `make test`
- `make lint`

## Implementation Steps

### Task 1: Add manifest validation check

- [x] add a `check-plugin` step (script under `scripts/` + make target, matching existing asset-check style) that fails when `.claude-plugin/marketplace.json` or `.claude-plugin/plugin.json` is missing, is invalid JSON, lacks required fields (marketplace: name, plugins with name+source; plugin: name, version, skills path), names anything `ralphex`, or points `skills` at a nonexistent directory
- [x] wire the check into `make test` alongside `check-symlinks`
- [x] verify the check fails before the manifests exist (red), including on a hand-broken fixture invocation if the script supports a path argument
- [x] run `make test` - expected red on missing manifests; proceed to task 2 to turn it green

### Task 2: Add plugin and marketplace manifests

- [x] create `.claude-plugin/marketplace.json`: marketplace `loopai`, owner echeg, single plugin `{name: "loopai", source: "./"}` with description and version
- [x] create `.claude-plugin/plugin.json`: name `loopai`, version `0.1.0`, description, author, repository `https://github.com/echeg/loopai`, license MIT, `"skills": "./assets/claude/skills/"`
- [x] run the task 1 check - now green
- [x] run `make test` - must pass before task 3

### Task 3: Create loopai-brainstorm skill

- [x] read the superpowers `brainstorming` v6.2.0 skill from the plugin cache and adapt it into `assets/claude/skills/loopai-brainstorm/SKILL.md` with an "adapted from obra/superpowers v6.2.0 (MIT)" attribution note
- [x] keep: project-context exploration, one-question-at-a-time dialogue, 2-3 approaches with recommendation, YAGNI, section-by-section design presentation with approval
- [x] replace the ending: instead of writing a spec to `docs/superpowers/specs/` and invoking `writing-plans`, the terminal state is invoking the `loopai-plan` skill, passing approved design decisions to be recorded in the plan's `## Decisions` section; spec files are explicitly not created (a slim ADR is allowed only as a rare exception for long-lived architectural decisions, on user request)
- [x] add frontmatter description that distinguishes it from superpowers `brainstorming` (trigger phrasing oriented at "design a feature that will become a loopai plan") since both skills may be installed side by side
- [x] add the top-level symlink `assets/claude/loopai-brainstorm.md` → `skills/loopai-brainstorm/SKILL.md`
- [x] run `make check-symlinks` and `make test` - must pass before task 4

### Task 4: Decisions section in loopai-plan

- [x] update `assets/claude/skills/loopai-plan/SKILL.md` plan template with an optional `## Decisions` section (context, chosen approach, rejected alternatives, verified facts) placed after Overview, and a note that `loopai-brainstorm` hands its approved design into this section
- [x] mention in the loopai-plan flow that when invoked from `loopai-brainstorm`, the questions already answered during brainstorming (goal, scope, approach) are not re-asked — only missing ones (e.g. TDD preference, title)
- [x] run `make check-symlinks` and `make test` - must pass before task 5

### Task 5: Verify acceptance criteria

- [x] verify all requirements from Overview: manifests valid and loopai-named, skill set is exactly the four existing plus loopai-brainstorm, brainstorm terminal state is loopai-plan, no spec-file machinery remains in the new skill
- [x] verify edge cases: `make test` from a clean checkout, symlink check on a case-sensitive path, manifest check failure modes (broken JSON, ralphex naming, missing skills dir)
- [x] run full test suite via `make test`
- [x] run `make lint` - all issues must be fixed

### Task 6: Update documentation

- [x] update `README.md`: "Claude Code plugin" section — install via `claude plugin marketplace add echeg/loopai`, list of provided skills, migration note (remove the upstream `ralphex` plugin/marketplace; superpowers can stay installed — only its plan-writing flow is superseded, and a per-project CLAUDE.md directive can point plan creation at loopai-plan for teammates who keep superpowers)
- [x] update `llms.txt`: plugin installation and the loopai-brainstorm → loopai-plan pipeline
- [x] update `CLAUDE.md`: project structure entry for `.claude-plugin/`, the version-bump discipline (bump plugin.json version whenever skills change, otherwise installed copies do not update), and the skill/symlink conventions extended to the new skill
- [x] run `make test` and `make lint` - final green

## Technical Details

- Manifest shapes mirror upstream ralphex 0.20.0 (verified from the local plugin cache): marketplace declares the repo itself as the single plugin source (`"source": "./"`); plugin manifest points `skills` at `./assets/claude/skills/`
- Marketplace and plugin are both named `loopai`; installed skills surface as `loopai:<skill>` — distinct from `ralphex:*` and `superpowers:*`, so coexistence during migration is safe
- `loopai-brainstorm` replaces only the plan-pipeline part of superpowers: superpowers stays installed for its orthogonal skills (debugging, TDD, code review); no superpowers files are copied except the adapted brainstorming text
- Version discipline: plugin.json `version` bumps on every skill change (start 0.1.0); the marketplace entry version mirrors it
- Attribution: "adapted from obra/superpowers v6.2.0 (MIT)" comment in the adapted skill records the sync baseline for future manual diffs against upstream
- Fork policy intact: no Go code, module path, or `<<<RALPHEX:...>>>` signal changes; `CHANGELOG.md` untouched

## Post-Completion

**Manual verification** (requires pushed repo):

- `claude plugin marketplace add echeg/loopai` on this machine; install the `loopai` plugin; confirm `loopai:loopai-plan` and `loopai:loopai-brainstorm` appear in the skills list
- remove the upstream plugin (`claude plugin uninstall ralphex`, `claude plugin marketplace remove ralphex`) and confirm `/loopai-plan`-style commands come from the own plugin
- run one real feature through `loopai-brainstorm` → `loopai-plan` → `loopai --worktree` and confirm the plan contains the `## Decisions` section and no spec file was created
- with superpowers still enabled, confirm the two brainstorm skills coexist and the CLAUDE.md directive routes plan creation to loopai-plan

**External system updates**:

- add the plan-creation directive to CLAUDE.md of active projects (e.g. IdleMerge): plans are created via `loopai:loopai-plan`/`loopai:loopai-brainstorm`; superpowers `writing-plans` must not produce plan files
- teammates install the marketplace with one command; no changes required to their superpowers setup
