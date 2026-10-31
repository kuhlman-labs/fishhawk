# Agent context maintenance

Contract for repository-owned prompt entrypoints and task references (#4886).
Read when changing instructions, skills or their loading/checking behavior.
This directory is linked, not imported wholesale into every prompt.

## Routing and ownership

| Need | Read |
|---|---|
| PR/tracker lifecycle | [contributing](contributing.md) |
| Docs/site/generated sources | [documentation](documentation.md) |
| Schema/API/auth/audit/module/review-prompt scope | [change checklists](change-checklists.md) |
| Gate invocation and interpretation | [verification](verification.md), verify-gate skill |
| Fixtures, concurrency, wire parity | [testing](testing.md) |
| Deploy/restart/cache/host failures | [operations](operations.md), relevant operational skill |

Root AGENTS.md contains universal decisions and short routing, not incident
history. Package READMEs own implementation contracts; scripts own executable
checks; skills own procedures. Add a root rule only when it changes decisions
across tasks and cannot be discovered reliably at the owning surface. Route
conditional obligations before planning scope, not only after a gate fails.

Skills are operator-authored under `.agents/skills/<name>/SKILL.md`. Each has a
`.claude/skills/<name>` symlink to `../../.agents/skills/<name>`. Keep name and
single-line description frontmatter, and preserve invocation policies. The
stop/delete skills retain `disable-model-invocation: true`; their bodies and
deploy-local still run `scripts/is-run-agent` before acting. Continue only on
`operator` + exit 0, including failing closed if the guard is absent. A new run
agent path belongs in that shared detector and its harness, never a skill-local
copy. Keep `.claude/settings.json` permissions-only; hooks/env would reach runner
agents and require separate review.

Instruction-path protections remain in the workflow and built-in agentpaths
constraint. Implement agents cannot author skills/config; feature_change's exact
root AGENTS.md exception does not relax other paths or letter-case variants.
Do not change those protections as a side effect of documentation cleanup.

When a cited contract, pin or scripts/dev/test behavior changes, search the
skills for that term and update affected procedures in the same operator PR.
A run unable to write them files an operator follow-up. Prefer reading pins from
canonical files/checkers over duplicating version literals. The separate
repeated-fact drift-check proposal is #3943.

## Loading inventory

| Layer | Loading behavior / maintenance |
|---|---|
| Codex repository instructions | Root-to-cwd instruction discovery, with overrides taking precedence and a default 32 KiB project budget. This repo's root budget is deliberately smaller. |
| Claude repository instructions | CLAUDE.md imports AGENTS.md. Imported text loads together; moving prose behind more imports does not save context. Keep task documents as ordinary links. |
| Skills | Names/descriptions support discovery; selected skill bodies/references load later. Budget discovery separately from bodies. |
| Personal memory/config/plugins | Machine-local, outside repository control. Audit separately; do not commit private memories or alter settings during routine repo maintenance. |
| Fishhawk stage prompts | Per-stage prompt builder plus selected declarations/personas/ADRs. Repository instructions are excluded from reviewer exports; reviewer requirements must use declared review surfaces. |

Sources checked 2026-10-31: [Codex instructions](https://learn.chatgpt.com/docs/agent-configuration/agents-md),
[skills](https://learn.chatgpt.com/docs/build-skills),
[Claude memory/imports](https://code.claude.com/docs/en/memory).
Loading can vary by installed client/version; verify the effective session, not
only disk contents. Runtime/app instructions, tool schemas, memory and history
are additional context; repository file sizes are not total prompt tokens.

Fishhawk's [repodoc](../../backend/internal/repodoc/README.md) records injection
identity, byte caps and truncation. [Decision-record selection](../../backend/internal/decisionrecord/README.md)
budgets a compact index separately from selected records. Preserve these
mechanisms and reviewer isolation; a repository-doc refactor does not authorize
changing review prompts or reducing their evaluation obligations.

## Budgets and check scope

`scripts/check-agent-context` uses Python's standard library and runs in verify,
including `--no-tests`. Limits are UTF-8 bytes, not tokens or line counts:

| Surface | Limit |
|---|---:|
| Root AGENTS.md | 8,192 |
| CLAUDE.md wrapper | 1,024 |
| Optional root AGENTS.override.md / CLAUDE.local.md | 8,192 each |
| Each local skill description / all local descriptions | 256 / 2,048 |
| Each selected SKILL.md body including metadata | 12,288 |
| Each docs/agents task guide | 16,384 |

The checker requires the root files and a nonempty skill/guide set; validates
skill names and the exact, live, two-way mirror set; and checks inline Markdown
local file links in those files. It ignores code fences, inline code, comments,
remote URLs and fragments. It does not validate heading anchors, backtick paths,
reference-style links, arbitrary Markdown, or every package README. Use ordinary
inline links for maintained routing. Optional root override/local files receive the same 8 KiB check and link
validation. These root limits total less than 32 KiB; they do not claim to
measure the full root-to-cwd chain or every nested/client-specific source.

`CLAUDE.md` must import AGENTS.md once and no other file. The checker does not
change the client cap. Run `scripts/test-agent-context` for isolated negative
fixtures. When adding a gate input, update the basedelta leg table and both the
live and example verdict-transfer declarations. Link targets are dependencies
even if only their existence is checked; the declaration covers the enumerated
guide and README targets so deleting one cannot reuse an old green. Unlinked
documentation retains its prior classification; add a new route target to the
leg inputs and both declarations when introducing it.

## Memory and temporary state

During an explicitly requested machine-local audit, keep durable preferences
separate from campaign handovers and transient environment facts. Date temporary
facts and give them an expiry or a condition to re-check. Index a topic once
rather than every incident. Promote durable repository facts into their owning
tracked documentation and replace the memory copy with a pointer.

Remembered authorization must retain its source, action scope and duration; do
not infer fresh authority from a terse index label. Validate against current
instructions and product governance. Keep expired quota/rollout notes out of
startup context; retain useful history in an archive/topic when appropriate.
Moving a checkout can leave a second memory tree: inspect actual client loading
before consolidating, and do not bulk-delete it as repository cleanup.

## Validation scenarios

Exercise routing with fresh task context; do not make every task read this file.
These are review scenarios, not permission to modify a live stack:

| Scenario | Expected observable behavior |
|---|---|
| Small backend fix | Reads the owning package, uses focused tests during iteration, retains DCO/docs/full-final-verify obligations |
| Schema field or major | Finds classification/soak rules, both mirrors, generated reference and advertising obligations before scope approval |
| Frontend test change | Chooses frontend-check; finds cookie and `.git` worktree fixture rules without loading deployment history |
| Toolchain mismatch or verify lock refusal | Fixes environment or narrows testing; never edits unrelated code or loops on refused verify |
| Local restart | Uses the correct skill and run-agent/restart-blocker guards; distinguishes disk rebuild from live GitSHA |
| Review-prompt change | Finds catch-rate and injection/quality evidence requirements; does not treat offline containment as behavioral resistance |

Record root bytes, description bytes, files retrieved, missed obligations and
unnecessary reads. Compare operator, implement, review/persona and fixup prompts
separately, distinguishing cached input from uncached tokens. Do not claim a
latency or quality improvement from byte reduction alone. The migration measured
87,175 root bytes before consolidation; its 32,768-byte Codex prefix ended in the
audit-category checklist, omitting Git flow and every trap.

## Migration map

This map preserves discovery of the former AGENTS.md sections without retaining
an obsolete second copy. Issue numbers retain access to incident history.

| Former section | Maintained destination |
|---|---|
| Repo state / canonical references | Root orientation/routes; current checkout, architecture and ADR index replace historical absence/count claims |
| Documentation surfaces / voice / naming | Root essentials + documentation guide; brand foundations; this file's skill ownership rules |
| Build, test, lint | Verification guide + scripts/README; source-of-truth flags/order stay beside scripts |
| Schema / auth / audit-category checklists | Change checklists (scope and rollout obligations retained) |
| Rebuild matrix | scripts/README § Rebuild matrix and activation; operations guide |
| Adding a Go module | new-go-module skill + change checklists |
| Git flow / tracker | Root essentials + contributing guide; live work-management conventions replace original issue-number ranges |

Every former trap has a route below (numbers are original list order):

| Trap(s) | Destination |
|---|---|
| 1 Parallel with Setenv/Chdir | Testing: Database and environment fixtures |
| 2 bash 3.2; 3 pipefail grep; 4 TARGETARCH | Operations: Shell and image authoring; scripts/README |
| 5 proxy CA; 6 locked-screen Docker helpers | Operations: Host and container diagnostics; deployment/gateiso docs |
| 7 verify lock | Root + verification + verify-gate |
| 8 go.work/go.work.sum | Change checklists + new-go-module |
| 9–11 Project Status, user owner, pagination | Contributing: Tracker; board-capability matrix |
| 12 HTTPS cookies; 13 Vite worktree deny | Testing: Frontend fixtures; frontend README |
| 14 signing env; 15 URL push refs; 16 detached maintenance | Testing: Git fixtures; runner README |
| 17 scaled durations; 18 DB clock; 19 fake mutex; 20 sink ordering; 21 spawn latency | Testing: Timing and concurrency |
| 22 trimpath fixture anchors; 23 container wait strategy | Testing: Database and environment fixtures |
| 24 newer Go | Verification: Toolchain; diagnose-red |
| 25 vanished image/sentinel; 26 k8s stale image; 27 digest types | Operations: Host and container diagnostics; Kubernetes documentation |
| 28 runner bootstrap | Operations: Stack lifecycle; runner README |
| 29 prompt injection/quality eval | Change checklists: Review prompts and evaluation |
| 30 wire parity | Testing: Cross-module wire types |
| 31 restart guards; 32 host overload; 33 cache growth | Root essentials + operations + operational skills; scripts/README |
| 34 workflow-run requirement | Root Working rules |
