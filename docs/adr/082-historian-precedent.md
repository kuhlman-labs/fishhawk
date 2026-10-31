---
id: ADR-082
title: "Historian: a derived decision index over the audit chain, deterministic precedent at the gate for the captain, and a thresholded divergence question"
status: accepted
date: 2026-09-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/3728
supersedes: []
superseded_by: []
amended_by: ["ADR-125"]
applies_to: []
---

# ADR-082: Historian: a derived decision index over the audit chain, deterministic precedent at the gate for the captain, and a thresholded divergence question

## Context

ADR-080 #3693 makes change of command an alpha theme (charter §2, T11): *precedent, decisions, and open work are readable by a captain who did not create them; a divergence from precedent is surfaced at the gate and resolved either as a one-off or as a doctrine change.* E75 #3696 carries the read path; E76 #3697 (handover brief) and E77 #3698 / ADR-081 #3727 (historian as a consult target) depend on it. This ADR settles the historian's shape.

**What the chain holds.** `audit_entries` (`backend/internal/postgres/migrations/0002_audit_log_schema.up.sql`; `account_id` + forced RLS since 0055/0057; append-only triggers; no payload/jsonb indexes; no retention). In this repository's dev database: 103,572 rows across 1,845 runs, of which only **~6k are decision-bearing**:

| Category | Rows | "Same kind" keys available | Missing |
|---|---|---|---|
| `approval_submitted` (plan) | 1,290 approve / 96 reject | decision, comment/rejection_comment, scope add/remove, claims, `delegated` | stage type needs a join; no structured rejection class; touched paths only in the plan artifact |
| `concern_waived` / `concern_deferred` | 1,726 / 303 | severity, category, stage_kind, reason, `delegated` | category is reviewer free text (95 distinct in waive/defer; 186 in `review_concerns`); no path |
| `concern_addressed_by_condition` | 1,586 | concern_id, approval_sequence | no category/severity (join via `review_concerns`) |
| `scope_amendment_decided` | 235 approve / 3 deny | decision, reason (join to request for paths) | — |
| `merge_verdict_recorded` | 549 | verdict prose | no negative outcome |
| `escalation_fired` | 276 | glob summary, fingerprint | rule id is positional; changes when `workflows.yaml` is edited |

**64 of 1,726 waive reasons already cite precedent by hand** ("previously", "as before", "same as"): the captain is doing this retrieval from memory.

**Read paths that set precedent for this:** `GET /v0/calibration` / `fishhawk_runtime_calibration` (`server/calibration.go`: scans `runtime_observed`, computes bands, memoized for the prompt path); `GET /v0/acceptance-triage/stats`; the gate view (`server/gateview.go`, with visible `HistoryIncomplete`/`HistoryGaps` degradation); the within-run settled-concerns ledger already shown to reviewers (`prompt/prompt.go` ~6203, `concern_relitigation_suppressed`); `releaseevidence`; `corpusdistill` (joins dispositions to reviews, fails loudly on ambiguity); `intakegroom/duplicate.go` (a deterministic Jaccard scorer that reports the shared tokens). `review_concerns` (0030) is a derived index over audit payloads: the precedent for a rebuildable side table. `audit.Repository.ListAll` has no LIMIT and pages in memory, so live scans do not scale.

**Where precedent would surface:** `GetRunStatusOutput` next to `next_actions` (bounded per ADR-077 / `mcpserver/compact.go`), the gate view, `drive_run`'s decision-required stop, the issue anchor's size-capped degradation ladder (`issuecomment/anchor_template.go`), and E40.2 #1717's decision write surface.

## Options

**(a) Retrieval.** 1: live SQL over `audit_entries`: nothing new stored, but the keys span audit, `runs`, `stages`, `artifacts` and `review_concerns`, with no payload indexes. 2: **a derived `decision_index` table** written after each decision-bearing append, rebuildable from the chain by backfill, carrying `account_id` under RLS, each row citing its source `sequence` and `entry_hash`. 3: an offline batch or materialized view: stale at gate time.

**(b) Similarity.** 1: exact tuple match: too sparse given free-text categories and positional escalation ids. 2: **hard filter (repo, decision class, stage kind), then a deterministic weighted score** (path-prefix Jaccard, normalized concern category, severity, escalation fingerprint overlap), reporting which keys matched. 3: embeddings: no infrastructure exists (no pgvector), and it breaks deterministic-first.

**(c) Where it is computed.** 1: on demand only: divergence could not prove what the captain was shown. 2: at gate open, recorded as `precedent_surfaced` (cited sequences, scores, index version; deduplicated by fingerprint like `escalation_fired`). 3: **both**: recorded at gate open, plus an on-demand query for digests, handover briefs and consults.

**(d) Divergence.** 1: flag any decision contrary to the top precedent: it would nag on every rejection, since 93% of plans are approved and 235/3 amendments. 2: **threshold**: at least N prior **human** (non-`delegated`) decisions with the same key, at least X% agreement, within a recency window, under the same doctrine version, otherwise "no clear precedent". 3: an allow-list of decision classes.

**(e) Who sees precedent.** 1: the captain only. 2: reviewers and planner too: anchoring would undercut adversarial review (`agenteval/adversarialretention.go`), and cross-run reviewer notes are agent prose derived from possibly attacker-influenced input. 3: **the captain now; agents later only through ADR-081 consults, structured fields only, inside the crew-message envelope.**

## Recommendation

**(a) 2, (b) 2, (c) 3, (d) 2 combined with 3, (e) 3.** Binding rules:

1. **The chain stays the authority.** `decision_index` is a derived, rebuildable projection. A row never holds a fact the chain lacks, and every precedent shown cites chain entries by `sequence` and `entry_hash`. Index-write failure degrades visibly (the gate view's `HistoryGaps` pattern) and never blocks a decision.
2. **Deterministic first.** Retrieval and ranking are deterministic and explain their matched keys. Any model summarization is advisory, labelled as such, and cites entries. Nothing auto-decides: precedent never changes a gate's state or its available actions.
3. **Scope: one repository.** The hard filter includes repo. Cross-repository precedent is out (N7), even within one account.
4. **Normalize once, at index time.** A concern-category normalization map (seeded from `defer_concern.go`'s category-to-issue-type mapping). A stable escalation key from the declaration's fingerprint, not its index. An optional structured rejection class on plan rejections. Repo, workflow and doctrine version (`runs.workflow_sha`, and the charter revision once E71.2 #3242 binds it at admission) on every index row.
5. **Divergence asks once and never nags.** Only when the threshold holds, starting with waive/defer and plan reject. The question is optional. "One-off" records the answer and stays silent for that gate. "Doctrine change" files an issue (`autonomy:low`, through the work-items pipeline) proposing the standing-order change. It never edits the charter or the spec: `.fishhawk/**` stays human-authored and forbidden to agents.
6. **Captain-facing only in alpha.** The precedent block goes to the gate hand-off, `get_run_status`, the gate view and the E40 decision surface, bounded and elidable like other prose. It does not go to reviewer or planner prompts. The within-run settled-concerns ledger is unchanged.
7. **The digest is on-demand.** "Since you last looked" needs a per-captain read watermark, which does not exist yet. It is computed on request from the index, not pushed. E76's handover brief composes the same queries.
8. **Registered and surfaced.** `precedent_surfaced`, `precedent_divergence` and any index lifecycle categories go into `audit/categories.go`. Operator-visible ones go through `issuecomment` or are recorded as internal in `docs/issue-comment-surfaces.md`.

## Decision

**Accepted (2026-09-26). Ratified by Brett (repository maintainer) on the explicit instruction "ratify both ADRs" after reviewing this proposal.** The agent records the maintainer's decision; it is not self-approval.

Adopt the Recommendation as the binding design: (a) a derived, rebuildable `decision_index` with the chain as authority; (b) a hard filter on repository, decision class and stage kind, then a deterministic weighted score that explains its matched keys, with no embeddings; (c) precedent recorded at gate open as `precedent_surfaced` plus an on-demand query; (d) a thresholded divergence question on an allow-listed set of decision classes, shipped disabled until tuned; (e) captain-facing only, with agents reaching precedent solely through ADR-081 #3727 consults. Rules 1–8 bind the implementation, in particular: nothing auto-decides, and a doctrine change files an issue and never edits standing orders.

**Scope of this ratification:** the architecture above. It changes no gate behaviour until the implementation lands under E75 #3696.

## Consequences

- New: the `decision_index` table plus backfill command (a migration with RLS), the index writer hooked after the decision-bearing appends, a precedent query endpoint plus MCP tool, a `precedent_surfaced` / `precedent_divergence` pair, and a read watermark for digests.
- Recording gaps closed along the way: a structured rejection class, a stable escalation key, and category/severity reachable for `concern_addressed_by_condition` (via `review_concerns`). Each is additive.
- Naming, to avoid collisions: E76's output stays the "handover brief" (distinct from E71's "release brief"). E78's "new-precedent flag" should be renamed "new architectural decision flag", so "precedent" means one thing.
- Thresholds (N, X%, window) start conservative and are tuned against this repository's history before the divergence question is enabled. The first slice ships retrieval and the recorded block with divergence off.
- Out of scope: embeddings, cross-repository precedent, showing precedent to agents outside ADR-081 consults, and any change to delegation (E82).
- Reversal: stop writing the index and drop the surfaces. The chain is untouched, and the index can be rebuilt later.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- #3696 [E75] Historian: read the ship's log back — precedent at the gate, digests, handover briefs — low confidence (0.24); shared: gate, historian, precedent

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"epic_suggestion":{"number":3696,"url":"https://github.com/kuhlman-labs/fishhawk/issues/3696","title":"[E75] Historian: read the ship's log back — precedent at the gate, digests, handover briefs","score":0.24285714285714285,"confidence":"low","basis":"gate, historian, precedent"},"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2886} -->
