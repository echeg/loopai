# loopai

Autonomous plan execution with Claude Code or Codex. See `llms.txt` for compact usage guidance and `README.md` for user documentation.

## Fork policy

The public CLI, configuration directories, progress paths, notifications, and other user-visible strings use `loopai`.

Three upstream-compatible internals intentionally retain the old name:

- `module github.com/umputun/ralphex` in `go.mod`
- Go import paths and internal package identifiers derived from that module
- protocol signals matching `<<<RALPHEX:...>>>`

Do not rename these. Keeping the module path avoids import-line conflicts during upstream merges. Keeping the signals preserves compatibility with embedded/custom prompts and historical progress logs. These are deliberate boundaries, not rebrand omissions.

The fork does not contain upstream packaging/release infrastructure or the upstream documentation website. Distribution is by source build and the repository's Claude Code plugin marketplace.

## Build commands

```bash
make build      # build .bin/loopai
make test       # asset checks, race-enabled unit tests with coverage, provider-wrapper suites
make check-symlinks # validate the six Claude skill assets and links
make test-symlinks  # regression tests for Claude skill asset validation
make check-plugin   # validate Claude plugin and marketplace manifests
make test-plugin    # regression tests for manifest validation
make test-grill-skill # validate loopai-grill metadata and workflow contracts
make test-wrappers # all retained provider-wrapper and wrapper-doc suites
make lint       # golangci-lint
make fmt        # gofmt and goimports
make race       # focused race run
make e2e        # dashboard browser tests
```

Go 1.26 or newer is required.

Dependencies behind the `e2e` build tag must be updated explicitly:

```bash
go get -u ./...
go get -u -tags=e2e github.com/playwright-community/playwright-go
go mod tidy
go mod vendor
```

## Project structure

```text
cmd/loopai/          main package, CLI parsing, startup wiring
internal/validation/ shared validation-command matching without package cycles
pkg/cmux/            best-effort cmux status integration
pkg/config/          configuration loading and embedded defaults
pkg/executor/        Claude-compatible and Codex process execution
pkg/git/             Git CLI operations and worktree management
pkg/input/           interactive input, fzf fallback, draft review
pkg/notify/          Telegram, email, Slack, webhook, custom notifications
pkg/plan/            plan discovery, parsing, and mutation
pkg/processor/       pipeline coordinator, prompts, executor policy
pkg/processor/phase/ task, review, external, finalize, and planning phases
pkg/progress/        timestamped progress logging
pkg/status/          shared phases, sections, and protocol signals
pkg/web/             dashboard, SSE, templates, static assets
e2e/                 Playwright dashboard tests
scripts/             provider wrappers and internal test helpers
scripts/copilot-as-claude/ # GitHub Copilot CLI wrapper for Claude-compatible output
scripts/pi-as-claude/ # pi wrapper for Claude-compatible output
assets/claude/skills/ canonical Claude Code plugin skill sources
assets/claude/loopai*.md legacy standalone-command compatibility symlinks
.claude-plugin/      Claude Code plugin and marketplace manifests
docs/                focused operational documentation and plans
```

The top-level `assets/claude/loopai*.md` files are symlinks to the matching
`assets/claude/skills/loopai*/SKILL.md` sources. Keep the command name,
directory name, and link target aligned; `make check-symlinks` rejects broken,
missing, incorrect, and orphan links, requires skill descriptions, and verifies
the exact skill inventory. The current set is `loopai`, `loopai-plan`,
`loopai-brainstorm`, `loopai-adopt`, `loopai-update`, and `loopai-grill`; every
added skill needs the matching top-level symlink. When adding or removing a
skill, update `expected_skills` in `scripts/check-symlinks.sh` and the valid
fixture inventory in `scripts/check-symlinks_test.sh`, then bump both manifest
versions.

Standalone installation must copy the complete directories under
`assets/claude/skills/`, not dereference only the top-level Markdown symlinks;
`loopai-grill` depends on bundled scripts addressed through
`${CLAUDE_SKILL_DIR}` and requires Python 3 in a POSIX environment (Linux,
macOS, or Windows via WSL) for its path helper.

`.claude-plugin/marketplace.json` exposes this repository as the `loopai`
marketplace, and `.claude-plugin/plugin.json` points Claude Code at the skill
directory. Whenever any skill changes, bump the version in both manifests so
installed copies receive the update. Keep the marketplace entry version equal
to the plugin version.

`loopai-grill` has two safety-sensitive routes. Grill mode critiques an active
plan and applies only user-selected verified findings; plan-off creates one new
plan and never edits its source. Both reject completed, symlinked, nested, and
`.loopai/` plans. The skill pre-approves no Claude tools. Its bundled path
helper rejects symlinked plan and `.loopai` roots, rejects hard-linked plans,
validates outside-repository scratch directories, identity-and-content-guards
active-plan replacements without overwriting concurrent writers, retains and
reports each displaced inode in Git-private non-stageable storage so late
pre-opened-descriptor writes remain recoverable, and performs
locked atomic no-clobber final creation. Plan and draft reads are capped at
8 MiB. Its Claude and Codex wrappers snapshot
only tracked and non-ignored untracked single-link regular files through
descriptor-anchored no-follow reads while excluding `.git/`, `.loopai/`,
recovery paths, and their case aliases, reject files over 64 MiB and snapshots
over 512 MiB, confine model reads to isolated temporary directories, and reject
in-worktree alternate Git directories. The Claude wrapper exposes only
read-only repository tools and disables user/project customizations; the Codex
wrapper requires strict-config and permission-profile support, disables
user/project config, rules, MCP and external tools, strips
credential-like shell variables, and starts an ephemeral session without
approval escalation. Candidate and judging scratch files live outside the
repository and are removed on every exit. Grill mode reports Codex failure
and degrades to Claude-only; plan-off requires Codex and fails closed. Grill
mode's apply path also records the round in the draft's `## Decision Log` —
accepted findings with what changed, findings the user declined with a stated
reason as rejected, and findings merely not selected as deferred — before
`replace-active` publishes it, since the skill never edits the plan at
its repository path and a post-publication write would need a second guarded
replacement. The section is created immediately before `## Implementation
Steps` when absent, existing entries are preserved, a finding already recorded
as deferred has that entry's date updated in place instead of a second
near-identical line appended, and it must never carry a checkbox. `ParsePlan` ignores a checkbox above `## Implementation Steps`,
since the H2 closes the current task, but `FileHasUncompletedCheckbox` — the
fallback used when a plan has no task headings — counts it, so such a plan
never reads as complete; the executor also reads the section as text and can
treat the line as outstanding work. Every critic and the verification pass are told
to read the log and not re-raise a recorded rejection absent contradicting
evidence, while a deferred entry may be raised again: `AskUserQuestion` with
`multiSelect` captures no reason, so recording an unexplained non-selection as
rejected would silently suppress the finding in every later round. The log is
written only when the user selects at least one finding;
a round where nothing is selected leaves the plan untouched, so its rejections
go unrecorded rather than loopai writing to a plan the user asked to leave
alone. When these contracts change, update and run `scripts/check-grill-skill_test.sh`.

## Configuration

- Global directory: `~/.config/loopai/`
- Local directory: `.loopai/`
- Global config: `~/.config/loopai/config`
- Local config: `.loopai/config`
- Global prompts/agents: `~/.config/loopai/{prompts,agents}/`
- Local prompts/agents: `.loopai/{prompts,agents}/`
- Progress: `.loopai/progress/`
- Worktrees: `.loopai/worktrees/`
- Override: `--config-dir` or `LOOPAI_CONFIG_DIR`

Local files override global files, which override embedded defaults. Embedded defaults remain the per-file fallback, so deleting an installed prompt or agent does not disable it. Remove its template reference to disable an agent.

`backlog_dir` (default `docs/backlog`) names the directory where agents file
out-of-scope findings, one markdown file per entry. It has no Go consumer: loopai
never reads, validates, or creates it, and the path only reaches prompts through
the `{{BACKLOG_DIR}}` placeholder — do not add containment or creation logic. It is
deliberately a committed repository path and not `.loopai/`, which is gitignored and
whose worktrees are removed after a run, so an entry written there would be lost
before `--merge`. Capture is instructed on the four paths that can write: task,
internal review, external-review evaluation, and plan creation. The three external
*review* prompts deliberately omit it, since external reviewers are read-only and
their findings reach the backlog through the primary evaluator. Each path states its
own commit rule, and they are not interchangeable. Task and internal review commit the
entry in phase, and all three prompts stage it with `git add` first, since it is untracked and
no commit picks it up on its own. `task.txt` is the only capture path that sweeps with
`git add -A`, and only because `modeRequiresBranch` is true for the two modes that run it, so
the tree it commits is either a fresh worktree or a checkout `CreateBranchForPlan` already
proved clean. `review_first.txt` and `review_second.txt` deliberately do not sweep and say so
inline: `ModeReview` and `ModeCodexOnly` create no branch and no worktree, so `--review`,
`--external-only`, and `--codex-only` commit in the user's own checkout, where a dirty tree is
allowed and never gated, and a sweep there would commit their unrelated work in progress. Those
two prompts stage the files the model itself created or modified, and their entry-only
`docs: add backlog entry` commit uses the pathspec form
`git commit -m "docs: add backlog entry" -- <entry>` for the same reason `make_plan.txt` does.
Do not restore a bare `git commit -m` at either site: with the entry staged it succeeds and
commits only the entry. The three evaluation prompts must leave it uncommitted: after the first
round `getDiffInstruction` shows the external reviewer only the uncommitted diff, so a
mid-loop commit hides the accumulated fixes, the next round reports clean, and
`EXTERNAL_REVIEW_DONE` fires with the fixes unverified — the entry is swept into the
final commit instead. Those three prompts do say to `git add` the entry alone: the final
sweep is described as reviewing `git diff`, which never shows a new untracked file, and
staging keeps the entry out of the unstaged diff the reviewer is shown while guaranteeing
the commit picks it up. Because that leaves the index deliberately non-empty, the same three
prompts spell the final commit as `git diff HEAD` plus an explicit `git add <paths>` over every
file the model changed: a bare `git commit -m` used to fail loudly with `no changes added to
commit`, and with the entry staged it would instead succeed, commit only the entry, and drop
every accumulated fix right before `EXTERNAL_REVIEW_DONE`. That explicit stage is deliberately
not `git add -A`, for the same reason the internal-review prompts avoid one: these prompts also
run under `--external-only` and `--codex-only`, which create no worktree. Staging does not
weaken the stalemate reset either, since `diffFingerprint` runs `git diff HEAD`. Plan creation runs in the source checkout before
the branch and worktree exist, so it stages that one file and commits it through the
pathspec form `git commit -m "docs: add backlog entry" -- <entry>`; an uncommitted
entry there never reaches the branch, and an untracked one fails branch and worktree
creation outright, because `hasChangesOtherThan` counts untracked files and exempts only
the plan file. The pathspec is what keeps that commit honest: this is the user's own
checkout rather than a loopai worktree, so a bare `git commit -m` would sweep whatever they
had already staged into a commit labelled as a backlog entry, on whatever branch is checked
out — normally `main` or `master`, and the same happens on a session the user later cancels
at draft review. The `loopai-plan` and `loopai-brainstorm` skills file entries too and carry
that same commit rule for the same reason: they run before loopai does, and loopai commits
nothing but the plan. Filing is dismissal-equivalent only for the completion signal:
the write is a real repository change, so the round that makes it resets
`review_patience` stalemate detection. A pre-existing linter error or failing test is
never out of scope on any path that can fix — all six such capture blocks say so
explicitly, because the category is otherwise an escape hatch from the pre-existing-issues
rule that sits beside it in the same prompts, and `task.txt` and `external_claude_eval.txt`
carry no such rule of their own. `make_plan.txt` is the deliberate exception: plan creation
fixes nothing, so filing is the only thing it can do with such a finding. The review and
evaluation prompts additionally bound the category unconditionally: a defect in code the branch
itself wrote is never out of scope, whether or not `{{PLAN_FILE}}` names a plan. Filing is
dismissal-equivalent for the signal, so without that bound "outside this plan's scope" is a new
way to end a review green with a defect this branch introduced. The bound must not be written as
a consequence of the no-plan fallback alone: under `--review`, `--external-only`, and
`--codex-only` there is additionally no plan for any finding to be out of scope of, but the
dangerous case is the ordinary one where a plan is present.

Agent files may carry YAML frontmatter parsed into `config.Options` (`model`,
`agent`, `description`). A non-empty `description` marks the file as a *dynamic
agent*: it is offered to the internal review phase through the
`{{agents:dynamic}}` catalog instead of requiring a hard-wired
`{{agent:<name>}}` reference. The five embedded agents (quality,
implementation, testing, simplification, documentation) carry no description and
always run as the base set. `loopai --gen-agents` writes starter dynamic agents
into `.loopai/agents/`. When `Options.Validate` warns, `buildAgent` drops the
execution overrides but preserves `Description`: catalog membership must not
depend on an unrelated `model` typo. `config.ParseAgentOptions` applies the same
CRLF/whitespace/leading-comment normalization the loader does, so any second
reader of an agent file agrees with the review phase about its description.
`parseOptions` collapses the description to a single space-separated line: a YAML
block or folded scalar parses fine but yields embedded newlines, and the catalog
renders the description as one Markdown list item followed by an indented
invocation snippet, so continuation lines would land unindented between the two.

`external_reviewers` configures an ordered comma-separated reviewer chain using
`provider[:model[:effort]]` entries. It takes precedence over the legacy
`external_review_tool` and `external_review_model` keys. `custom` entries use
`custom_review_script` and cannot specify a model. An explicitly empty value
clears an inherited chain and disables external review.

`loopai --init` creates project-local commented defaults. `loopai --reset` restores global defaults interactively. `loopai --dump-defaults <dir>` extracts embedded defaults for inspection.

This is a clean configuration break: never add automatic reads or migrations from `~/.config/ralphex/` or `.ralphex/`.

Tests must redirect HOME or config paths to `t.TempDir()` and must never touch either real user configuration directory.

## Architecture and key patterns

- `cmd/loopai/main.go` parses flags, resolves executors, constructs the processor, and owns process-level cleanup.
- `pkg/processor/processor.go` coordinates task and review phases.
- `pkg/processor/phase` contains the individual phase engines.
- `pkg/executor` owns child-process invocation and stream parsing.
- `pkg/status` contains the shared execution model and stable protocol signals.
- `pkg/config/defaults` contains embedded config, prompts, and agents.

Task plans use `### Task N:` or `### Iteration N:` headings and Markdown checkboxes. The task phase handles only the first incomplete section per executor iteration.

`pkg/processor/prompts.go` expands `{{BACKLOG_DIR}}` alongside `{{PLANS_DIR}}` in
`replaceBaseVariables`, the choke point every builder funnels through, so all twelve
prompt paths get it from one line plus `getBacklogDir`, which mirrors `getPlansDir`
and falls back to `docs/backlog` when `BacklogDir` is unset. A prompt file's header
comment lists the variables that prompt actually uses, not every variable expanded in
it, so adding the placeholder to a prompt and its header happen together.

`pkg/processor/prompts.go` expands two agent placeholders with the same
per-executor invocation snippet builder (Task tool for Claude, `spawn_agent` for
Codex): `{{agent:<name>}}` inlines one named agent, and `{{agents:dynamic}}`
renders the dynamic-agent catalog sorted by name, or
`(no project-specific agents configured)` when the project defines none. Only
the embedded `review_first.txt` uses the catalog; `review_second.txt` and the
embedded external-reviewer prompts do not, though both placeholders are expanded
on the external path too so customized prompts behave alike. The catalog pass runs
after agent-reference expansion, so `agentBodyText` strips `{{agents:dynamic}}`
from inlined agent bodies — otherwise raw catalog text lands inside an
already-escaped codex `task='...'` literal. The catalog also skips agents the same
prompt already inlines through `{{agent:<name>}}` — `agentRefNames` collects those
names before expansion — so a user copy of a base agent that carries a description
is listed and launched once, not twice. Which catalog entries actually run is decided by the model from
the descriptions — deliberately not by path or glob triggers in code — and the
prompt bounds the selection to roughly 0-3 agents launched in the same parallel
message as the base five. `FirstReviewPrompt` calls `warnMissingDynamicCatalog`,
which warns once per run when the project has dynamic agents but the effective
`review_first.txt` carries no placeholder: a prompt copy installed by an earlier
`--init` predates the catalog and would otherwise drop every dynamic agent
without a trace. The check belongs there and not in `expandDynamicAgentCatalog`,
which also runs for `review_second.txt` and the external prompts that
deliberately omit the placeholder.

`--gen-agents` is a standalone mode backed by `processor.ModeGenAgents` and
`phase.GenAgentsPhase` rather than ad-hoc executor wiring, so retry/limit policy
and section timing match the other phases. It runs one session with
`gen_agents.txt`, logs to `.loopai/progress/progress-gen-agents.txt`, then
reports the agent files present with their descriptions using
`config.ParseAgentOptions`; an unreadable file is reported inline rather than
aborting the listing. `describeAgentFile` reports what the loader will do with
each file rather than its description alone, so it gates emptiness on
`config.AgentFileHasBody` — the same comment-stripping check the loader uses.
A file with no body is ignored (replaced by the embedded default only for a
reserved name, dropped entirely otherwise), and frontmatter that fails to parse —
an unquoted `description` containing `: ` is the likely cause — is
indistinguishable from having none, so both are flagged instead of being listed
as working agents. An unquoted ` #` does not break parsing; YAML reads it as a
comment and truncates the description, so `gen_agents.txt` requires a
double-quoted description for both reasons. That distinction comes from
`config.AgentFrontmatterUnparsable` and not from a `---` prefix on the parsed body:
a working agent may open its body with a markdown rule, and a broken block written
below a `# ...` header never reaches the parsed form at all, so the prefix check
misdiagnoses both. It requires a *complete* `---` block — opening delimiter plus a
closing one on its own line — before calling anything unparsable, because a file with
no frontmatter whose body simply starts with a markdown rule matches the opening
delimiter alone and its real gap is a missing `description`, not YAML quoting.
`parseOptionsWithCommentRetry` accepts its comment-stripped retry
whenever that parse consumed a frontmatter block, not only when the block carried a
recognized key: a well-formed block holding only foreign keys must read the same
behind a comment header as it does without one, otherwise its raw `---` lines stay in
the agent body and valid YAML gets reported as broken. The report also runs on the failure path — a session that writes
files and then fails, times out, or is interrupted leaves them in the next run's
catalog, and the reserved-name warning would otherwise be lost with the error.
Overwriting a reserved base name warns instead of failing, because the user
reviews the generated files with `git diff` before committing them, and the
warning fires only for a file that actually has a body: a plain `--init` fills
`.loopai/agents/` with all-commented copies of the five base agents that override
nothing. Non-`.txt` files in the directory are reported as ignored rather than
skipped silently: the agent loader reads `.txt` only, so a session that disregards
the prompt and writes `.md` would otherwise leave files `git status` shows and
nothing ever loads. `clearStaleCmuxStatus` excludes the mode alongside the other
config utilities — it executes no plan and constructs no reporter, so clearing
would drop the previous run's completion pill with nothing to replace it. The
session resolves `task_model` only — `plan_model` does not
apply and no review model is printed. It is routed before branch and worktree
setup but still opens the repository to run `EnsureLocalGitignore`, since it tells
the user to inspect `git status` afterwards. `validateGenAgentsFlags` must reject
`--serve`/`--watch` alongside the other standalone modes: watch-only routing is
decided before the `ModeGenAgents` branch. Like `ModeTasksOnly`, the mode
short-circuits `resolveExternalReviewSelection`: it runs no review phase, so a
configured `external_reviewers` binary missing from `PATH` must not fail it. It is
also routed before `notify.New`, which validates channels eagerly: the mode sends
no notification, so a half-filled `notify_slack_*`/`notify_email_*` block must not
be what stops it. Notification setup covers only the plan-executing paths — the
close-out commands and watch-only mode return before it for the same reason.

The primary executor owns all repository writes. External reviewers produce findings only; the primary evaluates and fixes them using `review_model`, falling back to `task_model`. Reviewer chains run in order, and each reviewer loops until clean, its independent iteration cap, or its independent stalemate threshold before the next reviewer starts. Post-external review and finalize run once after the complete chain.

Claude is the default primary. `--codex` switches planning, tasks, internal reviews, evaluation, and finalize to Codex. `external_reviewers` entries require explicit `claude`, `codex`, or `custom` providers; duplicate providers with different models are supported. In the legacy path, `external_review_tool = auto` selects the other provider when installed. Missing automatic reviewers are skipped with a warning; missing explicit reviewers are errors.

Codex invocations use additive `-c` overrides so user `~/.codex/config.toml` settings remain available. loopai never writes to `~/.codex/`. `--pass-claude-md` lets Codex discover project `CLAUDE.md`; it does not install or link user-level files.

`--codex-args` and the `codex_args` config key extend that additive contract: their value is tokenized by the shared `splitArgs`, which separates tokens on any unquoted, unescaped ASCII whitespace rather than the space alone — a tab or newline surviving the ini loader would otherwise fold into the neighboring token and turn the remainder into the bare positional `codex exec` takes as its prompt. The set is deliberately ASCII and not `unicode.IsSpace`, since no shell splits on U+00A0 and a non-breaking or thin space pasted from rendered documentation into an unquoted value would otherwise produce that same stray positional — and applies POSIX-shell backslash rules (literal inside single quotes, escaping only `"` and `\` inside double quotes, escaping the next rune when unquoted) so literal backslashes in Windows paths survive while the documented `[\"CLAUDE.md\"]` quote-escaping recipe still works, and appended to every codex invocation loopai composes, at both construction sites in `pkg/processor/executor_factory.go` — first-class `--codex` phases and external codex review under a Claude primary, which share `newBaseCodexExecutor`. The extras go *after* loopai's own `-c` overrides and sandbox flags, because codex resolves repeated `-c` keys last-occurrence-wins (verified against codex-cli 0.147.0), so a user value deliberately overrides the matching loopai one. This is the intentional asymmetry with `claude_args`, which replaces the claude command's argument list rather than extending it: loopai owns the `codex exec` invocation shape. An explicit empty `--codex-args=` clears an inherited config value through the `o.codexArgsSet` guard in `applyCLIOverrides` alone; there is deliberately no `Config.CodexArgsSet` mirroring `ClaudeArgsSet`, because that flag exists only so an empty `claude_args` means "no arguments" instead of "use the defaults", and empty codex extras already append nothing — the invocation stays byte-identical to a build without the flag. Four consequences of appending rather than merging are documented for users and deliberately not policed in code, since extras are trusted input like `codex_command`: `splitArgs` consumes unescaped quotes, so a `-c` value whose TOML type depends on them must escape them or codex fails to load its config and every phase dies at startup; last-occurrence-wins covers `-c key=value` only, so repeating any flag loopai already emits, valued or bare, is a fatal codex parse error rather than an override — `--sandbox` on every path, and `--dangerously-bypass-approvals-and-sandbox` on the first-class `--codex` path, where `codex.go` emits it whenever the effective sandbox is `danger-full-access`, which is the `--codex` default; the extras land after the `exec` subcommand, so an option only the top-level `codex` command accepts (`--search`) is a fatal unexpected-argument error rather than a pass-through, while the global options that matter here (`-c`, `--model`, `--sandbox`, `--cd`, `--profile`) are accepted by `exec` too; and a bare positional token becomes `codex exec`'s prompt, demoting the one loopai sends on stdin to the trailing `<stdin>` block codex appends when both are present. Because the extras are appended on the shared base, they also reach the external reviewer whose `ForceReadOnly` pin exists so it cannot write — that pin holds against `codex_sandbox` but not against `--dangerously-bypass-approvals-and-sandbox` in `codex_args`, which codex accepts alongside `--sandbox read-only` precisely because loopai emits no bypass flag on the reviewer path. The CLI spelling is `--codex-args=<value>`: the values that matter start with `-`, so the detached form is rejected by go-flags before the run starts, and the documented examples use the attached form.

Alternative Claude-compatible providers live under `scripts/`. `scripts/copilot-as-claude/copilot-as-claude.sh` wraps GitHub Copilot CLI and uses native autopilot mode; plan creation deliberately uses `--autopilot --allow-all` without `--no-ask-user`. `scripts/pi-as-claude/pi-as-claude.sh` translates pi JSONL output and maps loopai model/effort settings to pi provider options. Detailed setup and wrapper behavior live in `docs/custom-providers.md`.

Worktrees live under `.loopai/worktrees/<branch>`. A new worktree branch is cut from the source checkout's current HEAD, including a non-default branch or detached HEAD. When an existing plan branch does not already contain that source HEAD, loopai reuses the branch and automatically merges the source HEAD into it in the fresh worktree. Git 2.38+ predicts conflicts during preflight before source mutation; older Git skips prediction, and prediction races remain possible, so the real merge is always the backstop and removes the fresh worktree on failure. `--base-ref` remains the review/template diff base and does not control worktree creation. With `-c`/`--commit`, loopai stages all non-ignored changes and commits them in the source checkout, advancing its branch or detached HEAD, before creating a fresh worktree; branch reuse merges that new source commit through the same synchronization path. Every worktree run holds a non-blocking OS advisory lock at `$(git -C <worktree> rev-parse --git-dir)/loopai-run.lock` for its lifetime. The lock, not its diagnostic pid and start time, is the liveness authority and the OS releases it on process death. Worktree preparation holds the shared repository lock while classifying or creating the expected path and acquiring its run lock. A durable per-target preparation marker in shared Git metadata distinguishes a crash during fresh initialization from an interrupted executable run; the next invocation removes and recreates a marked partial checkout instead of resuming it. A held run lock produces a precise busy error before mutable resume validation; an acquired lock routes through existing-worktree validation and automatically resumes from the first incomplete task without source synchronization or auto-commit. Any supplied `-c`/`--commit` is ignored with a warning on this path. Teardown takes the same shared lock before releasing the run lock and removing the worktree, preventing another process from acquiring ownership in the Windows-required release-before-delete interval. The lock file is advisory: deleting its directory entry while held does not release the open file lock and can allow a replacement file to be locked independently, so users must not delete it manually. The former `--resume-worktree` flag was removed and is an unknown option. Shared lock waits and run-lock Git-directory lookup honor cancellation and the configured `vcs_command`. The progress logger is created before changing into a worktree, so logs remain in the main checkout at `.loopai/progress/`.

cmux reporting is best-effort and must never affect execution. The status key and notification title are `loopai`. All calls go through the public `cmux` CLI and failures are ignored. After a completed run, `Reporter.Finish` intentionally leaves the final success or failure pill in place: `Stop` still clears the spinner and progress, but does not clear that pill. Abort paths do not call `Finish`, so `Stop` performs the full cleanup. A later run overwrites the pill, while `--clear`, a successful `--merge`, or a successful `--pr` removes it explicitly.

Auto-mode status ownership begins only after a successful local `starting` reservation or normal reporter startup. Validation and mandatory hand-off failures never clear a pre-existing pill that may belong to the active run. The reservation is installed before a combined `--reset` can block for input, including ambiguous bare-`--serve` invocations; if loaded configuration proves the run is dashboard-only, that temporary reservation is cleared. Interactive plan creation quiesces its reporter but retains the non-final pill through branch/worktree setup, then releases it only after the execution reporter starts. At completion, the reporter is quiesced before plan archival, progress-log closure, cwd restoration, and any worktree removal required before final status; `done` or `failed` is published afterward as the final sidebar command and marks the execution inactive for auto-mode routing. A successful `--worktree --serve` run is the deliberate exception to immediate worktree removal: its final pill is published after cwd restoration and log closure, while the plan-bearing worktree remains until the idle dashboard exits. A failed final status write does not become persistent: normal stop cleanup clears the preceding active pill.

`--cmux-workspace[=always|auto]` is part of that best-effort contract. The bare flag and `=always` retain unconditional hand-off. With `=auto` (the optional value must be attached with `=`), `cmux.WorkspaceBusy` runs `cmux list-status` and examines only the `loopai` pill: no pill, or pill text beginning with the final prefixes `done` or `failed`, means free; any other text means busy and triggers hand-off. The final prefixes are shared with `Reporter.Finish`, so producer and query cannot drift. A local auto run whose execution intent is known before config immediately uses `Reporter.Reserve` to install a non-final `starting` pill before config, dependency, or worktree setup; the run-level `cmuxStop` holder clears it on every startup failure and transfers cleanup ownership when the normal reporter starts. A missing cmux environment or binary and any list-status failure make auto mode continue locally without warning unless debug logging is enabled; reservation is attempted independently so a transient query failure does not reopen the startup window when status writes still work. Detection remains intentionally best-effort: a force-killed run can leave a stale phase pill and cause one extra workspace, while simultaneous auto starts can both observe free before either reservation lands. The output query stays separate from discard-only reporter commands, and its `cmd.Output` path must retain `WaitDelay` so a descendant inheriting stdout cannot hold the pipe past the query deadline. `cmux.SpawnWorkspace` is the one cmux call that propagates its error instead of swallowing it, because the caller has to choose between exiting after hand-off and running locally; it returns `cmux.ErrNotInCmux` when `CMUX_WORKSPACE_ID` is unset or the `cmux` binary is absent. `prepareRunBeforeConfig` routes `handOffToCmuxWorkspace` before taking a local reservation and before `handleEarlyFlags`, so hand-off happens before config loading and executor resolution while a combined `--reset` can reserve before blocking for input. It covers plan execution and interactive `--plan` creation alike, but never `isStandaloneCommand` invocations (`--clear`, close-out, `--init`, `--dump-defaults`, reset-only), which own no sidebar state; that predicate is shared with `clearStaleCmuxStatus`. The workspace name comes from `cmuxWorkspaceName`: a `--branch` override wins, then `plan.ExtractBranchName`, then `loopai`, so cards line up with `.loopai/worktrees/<branch>`. The relaunch argv comes from `cmuxHandOffArgv`: `os.Executable()` plus `stripCmuxWorkspaceArg(os.Args[1:])`, which removes the bare and `=value` forms and is the recursion guard, prefixed with `env` and the currently-set variables in `cmuxEnvOptions` when there are any. That prefix exists because the new workspace runs a shell of cmux's own, which inherits cmux's environment and not loopai's, so an option provided through the environment would silently revert to its default after hand-off; `env` is used instead of shell assignment prefixes because the target shell is unknown. `cmuxEnvOptions` must list every `env:` tag in `opts`, and `TestCmuxEnvOptionsCoversOptionTags` enforces that by reflection. Only those options cross; the rest of the environment does not, and `warnAPIKeyNotCarried` reports the one case where that silently changes the run's outcome rather than failing it: the pass-through request travels in argv or config while `ANTHROPIC_API_KEY` does not, so a key exported only in the originating terminal leaves the handed-off run falling back to OAuth or the keychain and billing an account the user did not pick. The key is deliberately not forwarded, because the command reaches the new workspace as text typed into its shell. The warning is emitted only after a successful spawn and only when the variable is actually set here, since otherwise there is nothing to preserve in this terminal either. That test comes first so the quiet case costs no config read, because `preserveAPIKeyRequested` answers the request from both of its sources: `--preserve-anthropic-api-key` and the `preserve_anthropic_api_key` config key, which `applyCLIOverrides` ORs together. Reading argv alone would stay silent for exactly the users who set it once and never type it again. Config is read through `config.LoadReadOnly` as in `handOffAllowedOutsideRepo`, and an unreadable config leaves the flag as the only answer since the child would fail to load it too. Every argv element is POSIX single-quote escaped into the `--command` string, since cmux sends it to the new workspace's shell as text. `cmux new-workspace` is bounded by `spawnTimeout` rather than the 2s `execTimeout`: it starts a terminal instead of updating a label, and a premature kill is ambiguous rather than cosmetic, because cmux may already have created the workspace while the caller reads the error as a failure and runs the plan locally too. That deadline is a mitigation, not a fix, so the ambiguity is also reported: `spawnWorkspace` wraps a `context.DeadlineExceeded` outcome in `cmux.ErrSpawnAmbiguous`, and `handOffSpawnFailure` maps it to a stop with an error. A positive auto busy verdict likewise makes every later refusal or spawn failure stop: falling back into a workspace known to be occupied would defeat isolation. Unconditional mode retains the local fallback for clean failures. Because that error is the one cmux failure a user sees, `SpawnWorkspace` uses `spawnRunner` instead of the output-discarding `execRunner`: it captures the child's stderr into a temp file and folds a bounded excerpt into the wrapped error, so a refusal arrives with cmux's own reason rather than a bare exit status. The spawn sets `CMUX_QUIET=1` in the child environment for that excerpt's sake, because `new-workspace` is a legacy alias for `cmux workspace create` and cmux prints a ~150-character deprecation hint ahead of everything else on every call, which on its own is most of `stderrDetailLimit` and would truncate the reason away; the variable silences advisory output only, so cmux's own `Error:` line still arrives, and the rest of the inherited environment is kept since the client needs it to find the socket. The capture is an `*os.File` and not an `io.Writer`, which is what keeps `execRunner`'s pipe-and-copy-goroutine hazard from applying — `os/exec` hands the descriptor to the child directly. Isolation is only achievable at the workspace boundary: `cmux set-progress` has no key and the pill key is the fixed constant `loopai`. In unconditional mode, any other failure, including an unresolvable working directory, prints a warning and continues the run in the current terminal. `executableHandOffRefusal` covers the relaunch binary itself: resolution failing is one half, but `os.Executable` also succeeds while naming a path the new workspace's shell cannot run, since Linux reads `/proc/self/exe` and keeps naming an unlinked binary with a ` (deleted)` suffix. Presence is all it can test, so a binary that disappears afterwards is out of reach: `go run` unlinks its temporary binary only once the successful hand-off has exited 0, so that invocation still hands off and fails in the new workspace, and the flag has to be smoke-tested from a built binary. A non-empty `o.PlanFile` the child could not read is refused before spawning, for the same reason: the child would otherwise be the first to notice, long after the workspace was created and focused, leaving this terminal claiming success with exit 0 and the user an orphan card to close by hand. `planFileHandOffRefusal` resolves the path against the same working directory `plan.Selector` does and goes past its bare existence test, because `os.Stat` also succeeds for a directory, so `--cmux-workspace docs/plans` with the filename forgotten would hand off and only fail in the child at `plan.ParsePlanFile`, in full mode after branch creation. A non-regular path and an unreadable file are both refused; neither can be read by the child either, so no hand-off that would have worked is refused, and a symlink to a readable regular file still passes. The regular-file test runs before the open because opening a fifo blocks until a writer appears. An empty plan file, as under `--plan` or `--review`, skips the whole check. A working directory that is not a repository root is refused before spawning for that same reason, since running from a subdirectory is the likelier mistake and the child's `must run from repository root` would arrive after the card exists. The guard is `fileExists(".git")`, the marker the run itself checks for, so hand-off stays config-independent wherever that marker exists; only its absence consults `config.LoadReadOnly`, through `handOffAllowedOutsideRepo`, to tell the two invocations that legitimately have no marker apart from a subdirectory: a non-git `vcs_command`, and a watch-only `--serve`, which never reaches the repository-root check and runs from any directory. That second case is decided by `isWatchOnlyMode` and not the config-free `mayBeWatchOnlyMode`, because a bare `--serve` with no `--watch` and no `watch_dirs` is a normal run that does reach the check; exempting it would create and focus the orphan card the guard exists to prevent. An unreadable config is neither, because the child would fail to load it too. A successful hand-off leaves the originating workspace's completion pill alone, since the run reports into another card: `prepareStaleCmuxStatus` defers the stale-pill clear for `--cmux-workspace` exactly as it does for a possibly-watch-only `--serve`, and `handOffSucceeded` derives the preserve verdict from `prepareRunBeforeConfig` stopping after a hand-off with no error. Under `--cmux-workspace` the other early flags reach an early exit only after a failed hand-off, and with an error except for the standalone commands whose clear is a no-op anyway.

The immediate pre-config auto reservation described above has one exception: an invocation with `--serve` and no plan or description must wait for loaded `watch_dirs` to distinguish a watch-only dashboard from a normal execution run. It reserves after config only when execution will follow. Combined `--reset --serve` still reserves before its prompt, then releases that temporary ownership if config confirms watch-only mode. On successful worktree execution with `--serve`, finalization restores cwd and closes the progress log but leaves the worktree for the dashboard's `/api/plan`; the normal `runWithWorktree` defer removes it when the dashboard exits.

Standalone close-out routing happens before executor and notification dependencies are constructed. `--clear` exits before config loading; `--merge` and `--pr` load config for `vcs_command` and colors, then open Git directly. Base resolution accepts an explicit local branch or local `main`/`master`. Both commands take an optional positional argument naming the feature, so they can run from the root of the primary checkout or any other registered worktree instead of the feature checkout; `runCloseoutCommand` still requires a checkout root, not an arbitrary subdirectory. Without the argument the feature is the current branch and behavior is unchanged, including the invoking checkout's own clean-tree requirement; with it that top-level `IsDirtyAll` check is skipped and cleanliness is enforced per worktree inside `prepareMergeWorktrees`. `resolveFeatureBranch` resolves the identifier deterministically: an exact local branch first, then a plan file in the plans directory and its `completed/` subdirectory. For a located plan, the branch recorded for it in `.loopai/progress/` wins over the name derived from the filename, because a `--branch` override makes the filename imply a branch the run never created and an unrelated branch carrying that derived name would otherwise be merged and deleted; the newest record wins and an unrecorded plan falls back to derivation. Only records whose `Mode` creates a branch are consulted: `--review`, `--codex-only`, and plan creation write their own record for the same plan, under a distinct progress filename and with a later mtime, but their `Branch` header names whatever was checked out at the time, so `recordedBranchIsFeature` skips them rather than resolving the close-out to an unrelated branch. That check is a denylist, so an absent or future `Mode` still supplies the branch, since dropping a valid association silently falls back to the derivation it exists to override. `findRecordedPRPlan` keeps consulting every mode: its branch-to-plan direction is correct for review runs too. Either way the branch must exist locally. `readProgressAssociations` is the shared scan behind both this plan-to-branch lookup and `findRecordedPRPlan`'s branch-to-plan lookup. The plan-to-branch lookup scans every root `progressRecordRoots` returns, which is every registered worktree: the primary first, the invoking checkout second, then the rest, deduplicated through `sameProgressRoot`. The progress logger resolves `.loopai/progress` against the working directory before loopai changes into a worktree, so a run started from the primary records there even though it executed in a linked worktree, while a run started inside any linked worktree records in that worktree. Restricting the scan to the primary and the invoking checkout misses a run started in a third worktree, and a miss falls back to the filename derivation the record exists to override. The newest matching record across all roots wins. The branch-to-plan lookup behind PR metadata stays anchored at the invoking checkout, since its result feeds `readPRPlan`'s containment check and a record naming a plan in another worktree would turn a stats-only body into a hard failure. `recordedBranchForPlan` matches a record to a located plan by case-folded filename via `planAssociationKey`, not by absolute path, so the association survives a case-insensitive filesystem returning the caller's spelling, a record naming the `completed/` copy of a plan still present in the active directory, and a lookup running in a different worktree than the record. It matches the path the record names rather than that path's repo-contained resolution, so an out-of-tree `plans_dir` or a checkout moved since the run still supplies the branch; resolution inside the repository is required only by the PR-metadata consumer, which skips associations whose plan resolves nowhere. Missing the association is the dangerous direction, since it silently falls back to the filename derivation the record exists to override. `recordedPlanInRepo` re-anchors a recorded path at the root when it resolves inside the repository only through symlinks, which a purely lexical containment test rejects wherever the checkout sits behind one; it still refuses symlinked plans and paths genuinely outside the repository. Plan lookup, for both resolution and PR metadata, anchors `cfg.PlansDir` at the invoking checkout's root through `plansDirPath`, so a plan present only inside the unmerged feature worktree is invisible from the primary checkout. Filename derivation goes through `git.Service.EffectiveBranchName`, which resolves the plan's real on-disk filename case, so the derived name matches the one worktree creation produced on case-insensitive filesystems. A feature equal to the base is an error. So is a surplus positional: `main` records `args[1:]` through `applyPositionalArgs` and `validateCloseoutFlags` rejects them, because `--merge <base> <feature>` is the `--merge=<base> <feature>` form with the `=` forgotten, go-flags hands both tokens back as positionals, and silently taking the first would merge and delete the intended base branch. Merge uses the registered base worktree, falling back to the primary worktree when the base branch is checked out nowhere, requires clean feature and merge worktrees, overrides branch-level squash/no-commit defaults, verifies the feature commit became an ancestor of the base, never force-removes a close-out worktree, aborts conflicts as `git.ErrMergeConflict`, and deletes the feature branch only after verified cleanup. `prepareMergeWorktrees` returns a `mergeTargets` value covering three cases: the feature is the current checkout, it lives in another registered worktree, or it is checked out nowhere and the merge runs in the base worktree with worktree cleanup skipped entirely. A registered worktree whose directory was deleted by hand fails with prune guidance instead of a raw chdir error, since Git still treats its branch as checked out until `git worktree prune`. Feature-worktree cleanliness is validated before the merge, not only before cleanup, and `git.Service.BranchHash` reads the feature head without a checkout. Ignored files do not make a worktree dirty and are deleted when the linked worktree is removed, so callers must preserve any local-only ignored files before `--merge`. The success line names the removed worktree, resolved through `mergeTargets.removableWorktree`, because with an explicit feature the removal target comes from the worktree list rather than being the caller's own directory. PR creation requires every effective `origin` push URL to identify the same GitHub repository used by `gh`, pushes committed state, and derives metadata from an exact associated plan (including the active-plan fallback used by worktree runs). With an explicit feature it needs no checkout: `git.Service.BranchDiffStats` measures the branch tip against the base through `refs/heads/<branch>` so a same-named tag cannot shadow it, and `gh pr create --head <branch>` targets the resolved branch. Plan discovery never returns a path outside the repository root, so an out-of-tree `plans_dir` or recorded plan degrades to a stats-only PR body instead of failing `readPRPlan`'s containment check. Tests use local bare remotes, Git push stubs, and `PATH`-injected `gh` stubs.

`cmd/loopai` resolves effective plan, task, review, and external-review models
and passes them to `cmux.Reporter`. Phase labels come from `status.PhaseHolder`;
review iteration labels come from `Reporter.WrapLogger`, which observes
structured `PrintSection` calls while forwarding the complete logger interface.
Keep `Reporter.WrapLogger` in the logger chain after dashboard setup. The
`progress.SectionTimer` sits below that cmux wrapper and above the dashboard
broadcast logger, preserving cmux's outermost rate-limit interfaces while timing
the structured sections. `progress.ValidationTimer` receives that wrapped runner
logger, filters executor command-timing events against the plan's `## Validation
Commands`, and writes per-run and aggregate `validation:` lines through the same
chain. Its aggregate sums command durations, so concurrent commands can overlap
and the total can exceed section or run wall-clock time. Use
`runWithSectionTiming` for both the main runner and interactive plan-creation
paths; it calls `SectionTimer.FinishRun` immediately after `Runner.Run` returns.
The main execution path then calls `ValidationTimer.FinishRun` before handling
the run error. Do not defer either finish because dashboard shutdown can close
the underlying log first. Plan-creation mode has no validation timer. A nil
reporter must return the section timer unchanged.

Claude command timing pairs foreground Bash `tool_use` and `tool_result` events
by tool-use ID and measures their arrival times; background Bash calls are
omitted because their first result is not process completion. Codex accepts both
legacy `exec_command` function calls and current custom `exec` rollout records,
follows yielded sessions through continuation/wait events, and tails child-agent
rollouts, including child rollouts stored on the next calendar day. For output-only
custom records, it uses valid native timestamps to prove
that a command returned before its configured yield threshold; calls at or beyond
the threshold attach to a later session continuation only when the association is
unique. Ambiguous batches are omitted rather than paired by order. It otherwise
prefers valid native event timestamps and falls back to arrival times; the fallback
is approximate because the final drain can deliver buffered events late. Executors
report completed shell commands; `ValidationTimer` alone performs classification
and logs the canonical configured label rather than raw provider arguments.
Unpaired commands and providers that omit tool events produce no timing lines.
Current custom `exec` records expose nested commands only as model-generated
JavaScript, so their parser is deliberately heuristic and best-effort rather
than a complete JavaScript grammar. Unknown or ambiguous source shapes are
omitted.

## Code style

- Use idiomatic Go and keep packages focused.
- Comments are lowercase except exported godoc.
- Wrap errors with context using `fmt.Errorf("context: %w", err)`.
- Define small interfaces at the consumer.
- Prefer existing dependencies; discuss new dependencies first.
- Use table-driven tests with `testify`.
- Test filesystem operations with `t.TempDir()`.
- Put tests in one corresponding `_test.go` file per source file.
- Generated mocks use `moq`; do not hand-edit them.
- Keep behavior cross-platform and use `filepath` for paths.

## Testing

After any code change:

```bash
make test
make lint
```

The full suite is required because configuration and progress paths cross package boundaries.
`make test` first validates Claude skill assets and plugin manifests, runs their
regression suites and shell-completion checks, then runs the race-enabled Go
suite with coverage and every retained provider-wrapper and wrapper-documentation
shell suite. The asset and manifest checks require Bash and `jq`; the focused
`test-grill-skill` suite checks the grill skill's metadata and operational
contracts. CI runs the same focused asset, manifest, grill-skill, completion,
and wrapper checks.

Dashboard e2e:

```bash
make e2e-setup
make e2e
make e2e-ui
```

Cross-compile when changing platform-sensitive code:

```bash
GOOS=windows GOARCH=amd64 go build ./...
```

Before tests involving configuration, snapshot or otherwise verify that real `~/.config/loopai/` and legacy `~/.config/ralphex/` content is unchanged.

## Development workflow

1. Read the relevant source, tests, embedded defaults, and plan.
2. Make the smallest cohesive change.
3. Add or update tests with the implementation.
4. Run focused tests while iterating.
5. Run `make test` and `make lint`.
6. Update user and developer documentation when behavior changes.
7. Commit a focused change with a meaningful message.

Do not edit `CHANGELOG.md` during normal development; it belongs to the release process.

Do not weaken tests to make a change pass. Do not skip validation between plan tasks. Preserve unrelated user changes in a dirty worktree.

## End-to-end smoke test

Prepare a toy repository and binary:

```bash
make e2e-prep
cd /tmp/loopai-test
.bin/loopai docs/plans/fix-issues.md
tail -f .loopai/progress/progress-fix-issues.txt
```

Review an existing branch:

```bash
make e2e-review
cd /tmp/loopai-review-test
.bin/loopai --review
tail -f .loopai/progress/progress-review.txt
```

Smoke-test Claude findings with Codex evaluation and fixes:

```bash
make e2e-codex
cd /tmp/loopai-review-test
.bin/loopai --codex --external-only
tail -f .loopai/progress/progress-codex.txt
```

## Pull-request checklist

- New behavior has tests, including error cases.
- `make test` passes.
- `make lint` reports no issues.
- Documentation and embedded config comments match behavior.
- Any Claude skill change bumps both manifest versions to the same value.
- No test touched real user configuration.
- The module path and `<<<RALPHEX:...>>>` signals remain unchanged.
- `CHANGELOG.md` is untouched unless release work explicitly requires it.
