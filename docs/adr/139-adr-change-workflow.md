---
id: ADR-139
title: "Direction C adopted: the adr_change workflow (amends ADR-113)"
status: accepted
date: 2026-11-04
issue: https://github.com/kuhlman-labs/fishhawk/issues/5007
supersedes: []
superseded_by: []
amends: ["ADR-084", "ADR-113"]
applies_to: []
---

# ADR-139: Direction C adopted: the adr_change workflow (amends ADR-113)

**Status: accepted (ratified 2026-11-04).**

## Context

Prepared at the Captain's request on 2026-11-04.

ADR-113 leaves this open: "**Direction C — The full `adr_change` workflow,** if B′ shows a remaining gap." An experiment mimicked it by hand on ten records (ADR-127 to ADR-134, ADR-136, ADR-137); its report (`experiment-report.md`, by §) and departure list (`_refs/fidelity-departures.md`, "dep. N") are the evidence. Repository citations are to a snapshot of forge main at `b07ad91b`.

**The gap.** B′ binds final text to a forge identity, but "Hand-run ADR reviews are outside the product" (ADR-113 D1). The numbers:
- **Ceiling.** None of the nine filed records cleared the ready rule within three reviewed revisions: seven received further reviewed revisions, while ADR-133 and ADR-136 proceeded through unreviewed errata. Three records used the earlier six-revision budget (dep. 14); 14 mediums remained at the respective ceilings, four from operator rulings and one from an operator-accepted replacement (§1, §3).
- **Form.** The metadata line and draft-status prose failed on 9 of 9 filed drafts, own H1 on 5, prose-only relation on 7; first-round rejects (R2 ten, R1 seven) mostly rest on that (§2, §3).
- **Unseen text.** Seven records' final text includes wording no reviewer saw (`erratum_without_confirming_round` in their audit logs; dep. 21).
- **Security trigger.** On the ten final metadata lines, neither `applies_to` nor the amended records' matches `.fishhawk/workflows.yaml:44-64`; any-`amends` fires on eight, missing ADR-127 and ADR-134 (the latter the source of the static-token finding) (own check against `docs/adr/index.json`; §3); plan scope cannot fire (`backend/internal/spec/review_conventions.go:330-332`); a keyword scan fired on all ten.
- **Siblings.** The ADR-073 §4 amendment, handed between two drafts that both knew of it, was found by reviewers reading siblings. A status change made three statements in ADR-131 and ADR-132 wrong and ADR-134's Related line stale; two records needed a final pass for one number (§3).
- **Scoped rounds.** They cleared ADR-128, 131 and 132 and left one medium on ADR-130 and 134, at roughly two-thirds of a full round's tokens as the mimicry counted them (§4 proposal 2; not comparable with product `cost_recorded`, dep. 10).
- **Drafter returns.** 18 in the audit logs (own count; the report gives none); without that channel "its ceiling would bite harder" (dep. 6).
- **Cost.** 34,356k tokens as a floor, about 13 hours, about ten Captain decision points on the final day (§2, §5).

**What exists.** `adr_change` appears only in `docs/adr/113-forge-verified-adr-ratification.md`, at lines 18, 39 and 250; this snapshot contains no implementation. `ratification-challenge` and `adr_metadata_line_invalid` appear nowhere under `backend/`, `cli/`, `scripts/` or `docs/api/`, so ADR-113 D2 and D3 are unbuilt. Records stop at ADR-126 (`docs/adr/README.md:549`); reviewers relied on the operator for the rest (§3).

## Options

- **A. B′ and the README drafting conventions, no workflow.** The conventions are being landed in `docs/adr/README.md`, not there at `b07ad91b`, and may carry much of the benefit: ADR-137, under the form rules, alone cleared the ready rule in one run of three revisions, with errata after (§1, §3). Still broken: reviewers "unmeasured on ADR drafts" (dep. 1), no security convention on a draft, no audit mark on unseen text, controls on out-of-tree tooling, nothing for other repositories (ADR-110).
- **B. A workflow carrying the ranked decisions, the rest as directions (recommended).** Cost: a render, arms and corpus, pre-flight, erratum and scoped-round paths, the preconditions. It saves reviewer tokens only where pre-flight saves a round.
- **C. All fourteen candidates decided now,** committing the build to remedies that never ran, at the cost of rework.

## Recommendation

Every human act in this record (a ceiling ruling, an erratum, ordering a scoped round, a waiver) is by a non-agent approver admitted by the `approvals` block at `types.adr.ratify.approvals` (ADR-113 D2) and is never covered by a standing order. An act recorded at a stage gate also satisfies that gate's full effective approvals predicate; every act named here, at a gate or not, is admitted only on a forge-verified credential of kind `session`, `oauth` or `oauth_as`, derived per ADR-123 D1; ratification itself keeps ADR-113 D2(a) and (b), and a security disposition keeps ADR-126 D5's gate.

**Option B,** because Direction C is what ADR-113 named and A cannot supply a measured reviewer, not because B is shown better. This decides requirements, not schemas, stage names or APIs; Direction C's other lines stand, including its grooming shape (`docs/spec/workflow-v2.md:487`), whose human `confirm` is spelled with the `review` stage type; Direction C's 'no `review`-type stage' means no agent review stage.

**D1. A review render for ADR drafts, with a catch-rate arm.** Drafts get their own render; plan review's does not change. Inputs: the draft, the records named in `amends` and `supersedes` with their transitive amendments and successors, and operator-selected sibling context (beyond Direction C, on one case), under the existing index, standing-record, attribution and truncation rules. Other cited records stay available for grounding; the arm records what is injected. The draft and each sibling are tracker bodies, entering only inside the server's quarantine envelope, and the injection containment gate covers the render before enablement. The operator may name siblings at run start and change them by a recorded context update; each round records the sibling identities and versions it reviewed. Every prompt has a calibrated, pinned `standing` arm (ADR-118 D1) before enablement (ADR-068 Decision 3 as amended by ADR-118 for the standard reviewers, ADR-084 rule 7 for a persona), and a portability arm (ADR-110 rule 3(b)) before enablement beyond this repository: without one nothing is enabled, and `catchRateArmTable` has no ADR arm (`backend/internal/agenteval/README.md:1009`). ADR-118 D1's `not_measured` rule for a non-Anthropic model applies.

**D2. Pre-flight before every round.** Candidate-local checks run before every review round and before an erratum is presented for ratification. Product refusals are what ADR-113 D2 and D3 would refuse: rendering and metadata failures. Pattern flags (own H1, draft-status prose, process sections, prose-only relations, sibling status) are declared configuration (ADR-110), with a non-Fishhawk fixture; the product requires a written disposition on each declared flag it raises. A repository may run further checks as its own tooling, outside the product.

**D3. Ready rule, ceiling, scope ruling.** Ready is advisory, on the current draft's findings and dispositions: no open high or medium and a written disposition on each remaining low. Failed, skipped or missing reviews are incomplete coverage, never a clean round. Reviewers need not agree; the approver keeps ratification authority, and accepting outstanding findings or incomplete coverage is recorded. Dispositions and rulings record their author. A high and a medium both close only by the reviewer that raised it confirming, in a later round, that the finding no longer applies to the current draft, or by the approver's recorded waiver, with its reason, or acceptance naming an erratum (D4). The challenge presents review coverage and findings with their dispositions, authors and reasons, including waived highs and mediums and each finding closed by resolution, with its closing round, beside the Status and Decision lines, using D5's permitted security projections; this amends ADR-113 D2's display.

On the ADR draft stage only, runs get a budget of three revisions, not the plan-stage default of one (`backend/internal/server/revise.go:39-48`); a draft not ready at the third goes to the approver with its open findings and options, including a scope cut, in place of other plan stages' "hard stop" (`revise.go:46`). A grant beyond the third revision starts a fresh run carrying the draft, findings and rulings; no grant raises a single ADR draft stage's ceiling above three. Three is a cost default, not an evidenced optimum, and the first three product runs revisit it.

**D4. A governed erratum.** An erratum is a human-approved change to an unratified candidate after its last review round, full or scoped; a later change lands only this way, recorded, never under a standing order. It changes wording or applies a reviewer-supplied fix and decides nothing new; a finding it fixes stays open until the approver's recorded acceptance names the erratum, and the challenge shows it beside the erratum diff; it never changes the record-metadata line, and whether a change decides something new is the approver's recorded judgement; any other change takes a confirming round (D7). The audit records its source and last-reviewed and final hashes and marks the changed text unseen. The approver sees the candidate diff, which amends ADR-113 D2's challenge display; ADR-113 D2 still authorizes the exact final body, its Status and Decision rendering identified separately. Changes after ratification follow ADR-113 D6, not this path.

**D5. Security conventions on every ADR round.** Not through `amends` and `applies_to`, and not the security persona, a direction. Every round is what ran: a conservative policy, not a measured gain over another trigger. It takes a second declaration: `applies_to` sits on the declaration, not the stage (`review_conventions.go:364-372`), and a convention with none "attaches unconditionally" (`:333`). Enablement requires ADR-126's confidentiality controls on ADR-review ingest and every downstream projection, including inherited findings, rulings and erratum records: on an ADR round, a concern deriving from the security convention (`repo_convention` naming it, ADR-068) is classified `security` for ADR-126 D3 at ingest, security payloads are reduced before audit append, and prompts and audit records carry only permitted references and metadata.

**D6. Sibling status stays out of hashed bodies.** A body names other records by id and carries no mutable status annotation; substantive prerequisite conditions stay in the decision. A sibling named in a body has its number before the last round naming it. Later corrections follow ADR-113 D6.

**D7. A scoped confirming round.** The approver may order a round on a diff, after a ceiling ruling or an erratum, or for a change an erratum can't carry (D4); errata and scoped rounds complete before the first ratification challenge, since a base-body change discards a pending ratification (ADR-113 D2). Outstanding findings and dispositions carry, coverage is reported as scoped, not as a full round, and any scoped prompt falls under D1's arm.

**D8. A drafter return channel.** A drafter may return a conflict or missing fact for operator resolution before publishing a revised candidate, because "Operator rulings are a defect source" (§3). The return does not itself consume another revision or authorize the drafter to override the ruling. How many returns and drafter executions one pass admits is a declared build default this record does not set; an additional grant for that pass is the approver's recorded act, with its author, and never raises the ceiling of three.

**D9. The record change comes out of apply.** After ADR-113 D2's apply succeeds, ADR-113 D3's writer produces the record change, which lands by that decision's route: "governance text never lands unattended". The approver's `confirm` stays open until the record lands and closes only on a verified landing; nothing defers it. Landing stales the architect arm until ADR-113 D4 ships, a precondition of routine use.

**Preconditions.** (a) ADR-113 D2 and D3. (b) ADR-094 D6, unbuilt (`backend/internal/server/docs/endpoints.md:305`), which already admits "A plan or implement review invocation", so nothing beyond it is decided. (c) The ADR-069 amendment, below. (d) Candidate binding, owned by the build contract: the workflow must bind the reviewed candidate revision to ADR-113 D2's pending ratification, assign an owner for staging it on the tracker, verify that the challenge binds it, invalidate the binding on edits, and advance the draft gate only after the matching ratification is applied. Generic stage approval cannot substitute for it. (e) For routine use, ADR-113 D4. (f) ADR-126 D3's ingest-time confidentiality, built, before any ADR round runs.

**Directions (not decided).**
- **Asked-for elements (§3).** *Promote on:* hand-kept lists, counting drops caught against drops reviewers still found.
- **Omission findings (§3, dep. 3).** *Promote on:* arm cases showing severity misstates omissions under the product's ingest.
- **Live sibling status.** Tracker-backed context waits on a source-resolution contract. Live status is presentation context, not an amendment to the hashed record. *Promote on:* that contract.
- **A limit on what a scoped round may raise.** *Promote on:* catches and misses against a recorded full-review baseline under actual persona caps, including unchanged serious defects and defects caused outside the edited lines.
- **Citation and size checks.** Neither ran. *Promote on:* a dry run catching what reviewers caught; a scope cut an advisory prompted.
- **Issues claiming to change a decided record.** One incident (§3). *Promote on:* a second in a scan of open issues.
- **Build tracking.** One instance (ADR-125). *Promote on:* a second.
- **Quota before a round.** One lost attempt, one early resume (§2, §4). *Promote on:* repeated loss under ADR-094 D6, plus a provider surface to read.
- **Security persona on ADR rounds (D5).** *Promote on:* an arm for it (ADR-084 rule 7).
- **Suggested siblings or a tree read for undeclared amendments.** *Promote on:* a product run where one reached ratification.

**What the evidence does not show.** ADR-137 is one record, written last, with every lesson in its rules. One family of proposals; rulings visible to reviewers (dep. 7); a freeze shaping severity (dep. 9); the counterfactual unknown (§5). Nothing here shows B beats A.

**What this amends.** ADR-113: Direction C becomes this decision; D1's "Hand-run ADR reviews are outside the product" no longer holds for a run under it; D3's writer is invoked after apply (D9) and still lands by D3's route; and its D2 challenge display gains review coverage and dispositioned findings (D3) and the erratum diff (D4); the rest of its D1 to D6 stands. ADR-084, narrowly: rule 4 selects "by `spec.Predicate` over plan scope ∪ diff", a draft has neither, so D1 adds stated relations and named siblings on ADR-draft rounds only; ADR-120 D2's topic matching cannot fire on a draft and does not apply, while its standing list is shown as excerpts as on plan rounds; the within-budget order of relation-selected records and named siblings is the build's, under ADR-120's whole-then-excerpt rule. ADR-120 composes, unchanged, so it is in Related, not `amends`. ADR-094 D6, ADR-118, ADR-121 and ADR-126 are applied, not changed. `applies_to` is empty: paths here are citations. Direction C's "ADR-069 amendment for type eligibility" is owned by a separate ADR amending ADR-069, filed before any build under this decision starts and a precondition (c) of that build, because ADR-069 makes ADR issues ineligible for workflow runs by type and this record does not amend that.

## Decision

**Accepted 2026-11-04** by the Captain (1 of 1; manual ratification path, the ADR-113 D2 verb is not yet built; approver and hashes in the ratification comment). **Option B**, i.e. D1–D9 as stated in the Recommendation, decided now as requirements for the `adr_change` workflow, with the rest as named directions. It amends ADR-113 and ADR-084 only as "What this amends" states. This record builds nothing by itself and changes no runtime permission.

## Consequences

- **Scope.** This builds nothing by itself and changes no runtime permission.
- **Costs:** the render, corpus and arms (ADR-118's comparable arm: "several hundred to over a thousand dollars per arm-recording"); tier-I review changes needing pre-registration (ADR-118 D5, ADR-121 D1).
- **Residuals:** arms run with "no checkout, no tools" (ADR-118 D1), so selection by stated relations can't surface an undeclared amendment, the architect's best yield; a non-Anthropic reviewer stays `not_measured`; an erratum puts wording no reviewer saw into the ratified body, and the displayed diff and the approver's reading are the only control on its substance; one operator, no control, mimicry-shaped severities; § citations hold only while the out-of-tree report and logs are kept.
- **Record metadata.** amends: ["ADR-084", "ADR-113"]; supersedes: []; applies_to: []
- **Related:** ADR-068, ADR-069, ADR-073, ADR-084, ADR-094, ADR-110, ADR-113, ADR-118, ADR-120, ADR-121, ADR-123, ADR-125, ADR-126, #4242, #4628.

### Validation

- **Conformance (D3, D4, D9, precondition (d)):** tests must cover candidate-to-ratification hash binding, changed-candidate invalidation, ceiling grants and fresh-run inheritance, erratum authorization, retry after partial failure, and confirmation only after verified landing. A non-Fishhawk fixture must exercise production resolution and missing-declaration behavior.
- **D1:** no recorded arm, no enablement; a sibling probe lands inside the envelope.
- **D2:** on committed fixtures built from the nine filed first drafts, the product check refuses each on its missing record-metadata line (§3's table), and the declared pattern flags reproduce §3's own-H1, draft-status and prose-only-relation hits for written disposition.
- **D3:** a draft with an open high or medium, an undispositioned low or an incomplete review is not ready; a waived high or medium records its approver and reason; a round with a failed reviewer is reported as incomplete coverage; the challenge lists review coverage and each waived or resolved high and medium with its author, reason and closing round, beside the Status and Decision lines; an agent subject or a standing order cannot record a ruling, waiver, erratum, scoped-round order or grant.
- **D4:** errata retain their source, last-reviewed and final hashes, unseen-text marker and displayed diff; an erratum that changes the record-metadata line is refused; a finding an erratum fixes stays open until the approver's acceptance names the erratum.
- **D5:** `amends: []` and `applies_to: []` still get the convention; no audit row holds a security payload.
- **D6:** a committed fixture carrying ADR-134's final Related line (a sibling named unratified after its ratification) is flagged.
- **D7:** a scoped round reports scoped coverage.
- **D8:** a return leaves the revision count unchanged; repeated drafter returns neither reset nor exceed the declared build default without a recorded additional grant.
- **Overall:** no comparative effectiveness target is set. The first three product ADR runs report revisions, ceiling findings, unseen text and token usage descriptively, with `cost_recorded` as a fresh baseline (§2's tokens are not comparable, dep. 10), and revisit D3's three; comparisons with §2 name differences in revision accounting, reviewer coverage, severity handling and token/cache measurement and establish no effectiveness gain.
