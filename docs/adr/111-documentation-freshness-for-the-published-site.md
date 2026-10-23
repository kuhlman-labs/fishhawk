---
id: ADR-111
title: "Documentation freshness for the published site: a PR-time site job, a reference-integrity check, a stated site obligation, and a narrow definition of up to date"
status: accepted
date: 2026-10-21
issue: https://github.com/kuhlman-labs/fishhawk/issues/4611
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-111: Documentation freshness for the published site: a PR-time site job, a reference-integrity check, a stated site obligation, and a narrow definition of up to date

**Status: accepted (ratified by the Captain 2026-10-21).**
- **Review history:** v1 was reviewed by fable (approve_with_concerns, 20 findings); v2 by Codex (reject, 13 findings). v3 narrows the decision to what is fully specified and moves the under-specified parts to their own design issues. A Codex confirmation pass on v3 returned approve_with_concerns: 12 of 13 v2 findings resolved or acceptably moved, and 3 bounded mediums, folded in below.
- **Freeze:** if accepted, this record is tracker-only until the 2026-11-01 catch-rate re-pin, because `docs/adr/` is frozen; the in-repo record then follows the upkeep rule.

## Context

Fishhawk ships a human-facing documentation site (`site/`, Astro + Starlight, E12.1 #2261), published to GitHub Pages at https://kuhlman-labs.github.io/fishhawk/ by `.github/workflows/docs.yml` (installed 2026-10-21, PR #4603). The site is unversioned and tracks `main` (#2261).

**Already enforced:**
- The four Reference pages carry GENERATED regions that `cli/internal/docgen` renders byte-exact from canonical sources (E12.4 #2264), with drift tests (`cli/internal/docgen/drift_test.go`, `scripts/test-site-reference`).
- The plan gate's `generated_surface` rows and generated-surface gate (#3437) pair each canonical source with its derivatives. It retries once, then admits with advisory evidence; exemptions are keyed `(generator, derived path)`.
- `scripts/check-site-voice` and `scripts/check-site-ia` run in `scripts/test verify`.
- `docs.yml` deploys on push to `main` touching `site/**`.

**Gaps:**
1. **No PR-time site check.** `ci.yml`'s Go filter excludes `site/**` and ordinary `docs/spec/**` changes, and `docs.yml` runs only after merge. A docs-only PR can merge with a broken build, a dead internal link, or a hand-edited generated region the drift test would have failed.
2. **Reference integrity.** Hand-written pages can name CLI flags, MCP tools, routes or workflow-spec shapes that no longer exist, or show obsolete shapes (`concepts/constraint.md` still shows list-form constraints). Nothing checks this.
3. **No stated obligation.** AGENTS.md Git flow step 4 lists `docs/` surfaces but never `site/`.
4. **Narrative pages** (`concepts/*`, `operating/*`, `start/*`) have no machine link to the code they describe.
5. **Releases do not reach the site.**

Gaps 1–3 have well-understood, cheap fixes and are decided here, within the stated limits of D1: unannotated detached flags and fragment examples are out of its coverage. Gaps 4–5 need product design that two independent reviews showed is not yet specified, so they are decided only as DIRECTIONS, each with its own design issue.

## Options

- **A.** Status quo plus convention.
- **B.** Generate everything. Explanation is not derivable data.
- **C.** Decide the cheap, fully specified controls now, and route the product-level designs through their own issues (recommended).

## Recommendation (decided now)

Under ADR-110 (#4605), D1–D3 are this repository's own tooling and CI (rule 5). No product code changes.

**D1 — Generated regions, plus a reference-integrity check.**
- **Generated regions.** E12.4's byte-exact contract is unchanged. New generated regions are added only where a canonical, importable source exists. The MCP tool list and the audit-category table need a committed inventory written by a `backend/cmd` emitter, because cli cannot import backend; the emitter is named in its implementing issue.
- **Reference-integrity check:** `scripts/check-site-references` plus a Go test where an inventory lives. It runs in `scripts/test verify` over hand-written pages under `site/src/content/docs`, skipping GENERATED regions. It is MARKDOWN-AWARE: it parses code spans and fenced blocks, never raw text.
  - **CLI:** a `fishhawk <command> … --flag` example is validated against THAT command's flag set in `cmdinfo`, never a global flag list. It covers fenced shell examples, not only inline spans. A detached flag in prose ("`fishhawk run start` takes `--runner-kind`") is bound to its command by a `<!-- ref: command="run start" -->` annotation. An unannotated detached flag is OUT of coverage, and the check reports the count of unbound flags, so the gap stays visible.
  - **MCP tools:** `fishhawk_*` tokens are checked against the tool registry's committed inventory. A non-tool name that shares the prefix (for example the `fishhawk_audit_complete` Check Run) is annotated, not suppressed.
  - **Routes:** `/v0/…` paths are normalized, so a placeholder like `<run-id>` or `{run_id}` matches an OpenAPI parameter template, and are then checked against the OpenAPI document.
  - **Workflow examples:** a fenced `yaml` block annotated as a complete workflow document runs through `fishhawk validate`. Fragments and illustrative blocks (ellipses, stage-level snippets) are annotated as such and are not validated as documents.
  - **Annotations.** Typed annotations in an HTML comment before the block (`<!-- ref: example | fragment | historical | negative -->`) and a reviewed allow-list file with one reason per entry are the only exception paths.
  - **Honesty.** This is a reference-integrity check, not proof of accuracy: it catches removed or renamed identifiers and invalid complete examples, not wrong prose. An example annotated as a fragment or historical is NOT validated, so an obsolete fragment (e.g. `concepts/constraint.md`'s list-form constraints) stays a review item. The fix is to rewrite it as a complete, validated example.
  - **Inventory binding.** Every committed inventory the check reads (the MCP tool list, audit categories) is pinned to its producer by a Go test in the producer's module. An inventory-only edit cannot validate references against a stale or wrong inventory.

**D2 — The PR-time site job, in `.github/workflows/ci.yml` (operator-authored).**
- **Trigger.** A `site` filter in the existing `detect` job (dorny/paths-filter) covers:
  - `site/**`, `docs/spec/**`, `docs/api/**`;
  - `cli/internal/docgen/**`, `cli/internal/cmdinfo/**`;
  - `scripts/gen-site-reference`, `scripts/check-site-*`, `scripts/test-site-*`;
  - the D1 inventories and allow-lists;
  - the workflow validator the examples run through (`cli/internal/spec/**`, `cli/cmd/fishhawk/validate.go`, and the schemas under `docs/spec/`, already covered);
  - each inventory's producer package;
  - `ci.yml` itself.
- **The `site` job** runs when that filter matches:
  1. the focused checks:
     - the docgen drift test (`go test ./internal/docgen/` in `cli`);
     - the inventory producer-binding tests (D1);
     - `scripts/check-site-ia` and `scripts/check-site-voice`;
     - the D1 reference-integrity check, including `fishhawk validate` over complete examples built from this PR's CLI.

     CI's Go legs do not run `scripts/test verify`, so the site job runs these directly rather than relying on verify.
  2. `pnpm install --frozen-lockfile`, `pnpm check`, `pnpm build`;
  3. an internal link check over `site/dist`. It uses PINNED lychee with its configuration committed:
     - the `/fishhawk/` base path mapped to `site/dist`;
     - directory URLs resolved to `index.html`;
     - same-origin absolute URLs treated as internal;
     - fragment (`#anchor`) validation on;
     - a broken-page fixture and a broken-anchor fixture pinned by a test.
- **Required check.** The job is added to `CI Pass`'s `needs`. `CI Pass` runs `if: always()` and treats `skipped` as pass, so there is no new required check, and no path-filtered required check can block a merge.
- **External links:** a scheduled, non-required workflow (cron plus `workflow_dispatch`) that runs regardless of path filters, kept OUT of the `CI Pass` rollup, reporting through its run summary.
- **`docs.yml`** stays deploy-only.

**D3 — The stated obligation.** AGENTS.md Git flow step 4 gains a `site/` row: a user-visible behaviour change updates the site page that describes it, in the same change, or the PR Notes say why not. § "Documentation surfaces" gains a pointer to D1 and D2. Root `AGENTS.md` is writable to `feature_change` runs.

**D4 — What "up to date" means, stated narrowly.** At any `main` commit:
- every generated region equals its source, and every annotated reference on a hand-written page resolves (D1; enforced in verify and, for docs-only PRs, in D2's job);
- the site builds with no dead internal link or anchor (D2; enforced at PR through `CI Pass` for changes matching the `site` filter).

The PUBLISHED site is the last SUCCESSFULLY deployed SHA, which the Pages deployment records; it may lag `main` after a failed deploy. Narrative accuracy is NOT machine-enforced: D3 makes it an obligation, and reviewers check it. External links are checked on a schedule and never block. Operator-authored PRs that bypass the plan gate are still covered by D1 and D2, which run in CI.

## Directions (not decided here; each needs its own design issue before implementation)

**Direction A — A declared coupled-documentation map (product feature, ADR-110).** It closes gap 4 and ADR-110 debt #4608 rows 2–3. Requirements both reviews established, which its design must meet:
- it preserves the generated-surface gate exactly: one retry, then advisory admission, and `(generator, derived path)` exemptions, or it decides any change explicitly;
- a failure-policy table that satisfies ADR-110 rule 4, with requiredness knowable before a `from:` document is loaded;
- the existing doublestar matcher and its validation;
- create, modify, delete and rename semantics;
- per-sub-plan satisfaction under decomposition;
- cap accounting for regenerated-unchanged outputs;
- an exhaustive migration table covering all 14 `surfacePatterns` and every `generated_surface` row, with the schema-mirror patterns ADR-067 requires retained;
- an optional reason code beside the existing free-text exemption reason (never replacing it);
- plan-prompt rendering;
- a non-Fishhawk fixture through the production resolver and consumer;
- an initial NARRATIVE map for `start/`, `concepts/` and `operating/`.

**Direction B — A release changelog page.** It closes gap 5. Constraints:
- `fishhawk release cut` keeps its contract: it records the DECISION and references the persisted `release_notes` artifact and its hash. Export is a separate step, a Fishhawk-repository script or an explicit portable export command with a caller-supplied destination.
- A reviewed public projection is generated outside Astro and committed under `site/`. Whether that needs an amendment to the "`docs/` is never a publish source" boundary is decided there.
- The sequence must work with immutable releases (ADR-051's 2026-10-16 amendment): when the link enters the Release body BEFORE publication; binding to the cut artifact, its hash and the tag commit; and cut, pending, failed and published states.
- It depends on #4521. A changelog is content, not a site version (#2261), and #2261's revisit trigger (supported releases) is unchanged.

**Direction C — Churn detection.** It follows Direction A. It requires durable cross-scan dispositions keyed by repository, finding and evidence revision (beyond #4565), defined meaningful-evidence changes tested against window rollover, pinned scan head and time, a definition of "substantive", and truncated history reported as a degrade. Deferred until D1–D3 and Direction A show a remaining gap.

## Decision

**Accepted 2026-10-21** by the Captain (repository maintainer): **Option C**, i.e. D1–D4 as stated in the Recommendation, decided now. Directions A–C are NOT decided by this ADR. Each needs its own design issue, meeting the requirements stated above, before implementation. The implementation issues are filed with this ratification.

## Consequences

- **Gains.**
  - A docs-only PR can no longer merge a broken site, a stale generated region, or a reference to a removed identifier.
  - The obligation to update the site is written down.
  - The remaining product designs get their own reviewed decisions instead of being ratified under-specified.
- **Costs.** A `site` CI job on doc-touching PRs; maintaining the annotations and allow-list; the emitter inventories for D1's new generated regions.
- **Implementation (after ratification):**
  1. D2's `site` job and the scheduled external-link workflow (operator);
  2. D1's reference-integrity check and its fixtures;
  3. D3's AGENTS.md edits;
  4. D1's new generated regions;
  5. a design issue each for Directions A and B, under E12; Direction C waits.
- **Related:** ADR-051, ADR-067, ADR-110 (#4605), #2261, E12.4 #2264, #3437, #4521, #4565, #4608.
