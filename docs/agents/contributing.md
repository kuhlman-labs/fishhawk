# Contribution and tracker procedure

Read for branch/PR preparation or tracker updates. Root AGENTS.md holds the
universal DCO, branch and verification rules.

## Change lifecycle

1. Pick a child issue from Project #7; resolve a blocking ADR before implementing.
2. Branch from `origin/main`, named for the issue and change (e.g.
   `e97.28-agent-context`). Use a separate checkout when other work is present.
3. Commit with `git commit -s`, imperative title, no conventional-commits prefix.
   Preserve real newlines in multiline bodies (a quoted heredoc/file is suitable).
4. Update the relevant documentation in the same PR:
   - New package, route, env var or flag: an architecture §10 pointer plus the
     owning package contract; operator inputs also belong in the component README.
   - Schema: canonical reference and embedded copies; API: OpenAPI and companion;
     generated site references: regenerate. See [checklists](change-checklists.md).
   - Added/removed/renamed issue-comment surface: update
     [issue-comment surfaces](../issue-comment-surfaces.md).
   - Voice/naming: [brand foundations](../BRAND_FOUNDATIONS.md); autonomy:
     [methodology](../METHODOLOGY.md).
   - New lesson: owning package/guide/skill/check. Add a root instruction only
     when it applies across tasks. Skill edits are operator-authored.
   - Ratified ADR: use `record-adr`; transcribe the tracker body, update the
     records table and supersession/amendment links, regenerate index.json in the
     ratification change or immediately after it. Follow [ADR upkeep](../adr/README.md).
5. File deferred work before opening the PR: TODOs, follow-up promises and known
   operability gaps need issues linked from `## Notes`. Prefer `fishhawk_file_issue`
   for repository conventions, numbering, parent links and board placement.
   For product friction during a run, use `fishhawk_report_product_issue` with
   the run ID so the diagnostic bundle is redacted and deduplicated.
6. PR body: `## Summary`, `## Test plan`, optional `## Notes`, `Closes #<issue>`.
   Use `verify-gate` before push/open; add `frontend-check` for frontend changes.
7. After merge, inspect parent sub-issue links and tracker Status (not a body
   checklist), update siblings if scope shifted, and record/close resolved ADRs.
   Use `sync-main` for local post-merge work; it guards live runs/decompositions.

## Tracker

The live conventions are `.fishhawk/work-management.yaml`. Every implementation
issue needs `area:*`, `autonomy:*`, `phase:*`, `type:*`; parents use `epic` or
`adr`. Set Status to Backlog on creation. The board fields include Component,
Phase, Autonomy, Estimate, Priority and Target day. Historical phase day 1 is
2026-04-30; do not infer today's backlog from the original issue-number ranges.

`fishhawk_file_issue` allocates numbered titles server-side. Use `existing_numbers`
only to override/hint discovery; seed `[0]` only when starting at 1 without
provider discovery. A real discovery failure must not allocate a guessed number.
Check `boarded` / `epic_linked`; a created issue can still need board repair.
Manual board fallback: `gh project item-add 7 --owner kuhlman-labs --url <issue-url>`.

Project #7 is user-owned (`user(login:"kuhlman-labs")`, not `organization`).
An App installation token cannot access user-owned Projects v2; see
[board capability matrix](../board-capability-matrix.md) for the required
project-scoped PAT/UAT and degradation reasons. Avoid board enumeration for
issue membership: board `items(first:N)` caps at 100; query the issue's
`projectItems(first:10)` and filter by project ID. A full page without the target
is undecidable, not proof of absence. Board Status options are Backlog, Up Next,
In Progress, In Review, Blocked, Done; the historical setup used GraphQL
`updateProjectV2Field.singleSelectOptions`.
