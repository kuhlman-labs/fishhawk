---
id: ADR-084
title: "Reviewer personas: a separate reviewer invocation with a declared remit, attached per stage or by an escalation, advisory with ingest-time checks, plus deterministic security checks as concern sources"
status: accepted
date: 2026-09-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/3752
supersedes: []
superseded_by: []
amended_by: ["ADR-118", "ADR-120", "ADR-139", "ADR-143", "ADR-147"]
applies_to: []
---

# ADR-084: Reviewer personas: a separate reviewer invocation with a declared remit, attached per stage or by an escalation, advisory with ingest-time checks, plus deterministic security checks as concern sources

## Context

ADR-080 #3693 says new crew roles are "reviewer personas", not new stage types (N6). Two alpha epics need one: the **architect** (E78 #3699), a plan-stage reviewer whose remit is the in-repo decision record, and the **security officer** (E80 #3701), a threat-model reviewer on declared sensitive paths. Both epics say "via E55's grammar". ADR-068 #2211 (accepted 2026-07-26) governs that grammar, but it decides something narrower: **workflow-declared conventions injected as supplemental criteria into the existing reviewers' prompt**, with base-ref resolution, subordinate framing, `severity_cap` and attribution. It does not decide a separate reviewer, per-reviewer prompts, attachment by escalation, or choosing records from a directory.

**Verified in code (2026-09-27):**

- **One prompt for every reviewer.** `runPlanReviews` (`server/plan.go` ~1458) builds one `promptText`, and `runPlanReviewLoop` (~2133) sends it to every `reviewerInvocation` (~77), which has no per-reviewer prompt or remit field. The implement side is the same (`runImplementReviewInvocations`, `server/trace.go` ~5111). `spec.AgentReviewer` carries provider, model, effort and version only. Authority is per stage (this repository: two agents plus `human: 1`, so advisory).
- **E55 status.** E55.1 #2242 (the `repodoc` resolver, 32 KiB cap, `FetchFile`-only access with no directory listing) is done. E55.2 grammar #2243, E55.3 rendering #2244, E55.4 hard eval gate #2245 and E55.6 in-process wiring #2797 are open and `milestone:beta`. The in-process review builds never set injected documents, so a declared document reaches no reviewer today. The per-run base ref for code-change runs was unowned until E55.7 #3746.
- **Size.** The ADR corpus is about 606 KB against a 32 KiB injection cap. Whole-record injection is impossible, so selection needs a machine-readable index (E78.1 #3722, amended).
- **Escalations** carry only `approvals` and `max_autonomy` (`escalation_requirements`). They match the approved plan's scope union (`server/escalation_gate.go`), whereas review happens on the diff.
- **Security today.** The implement review prompt has a self-gating security lens (`prompt.go` ~5823); the plan review has none. `securityscan` consumes CodeQL findings (dormant on stock installs; E80.1 #3749). There is no diff secret scanning or permission-drift check. `redaction/`'s pattern set is reusable.
- **E55.4 itself names "a separate reviewer pass"** as the fallback if conventions dilute the standard reviewers' catch rate.

## Options

**D1 — persona shape.** (a) A remit document injected into the existing reviewers (ADR-068 as-is). (b) **A separate reviewer invocation** with its own prompt: a persona is a model configuration plus a remit document plus the stage's normal inputs. (c) A separate pass after review completes.

**D2 — attachment.** (a) Static, declared on a stage's reviewers. (b) Attached by an escalation (`require.reviewers: [<persona>]` on `escalation_requirements`), so the sensitive-path globs are declared once. (c) Both.

**D3 — authority.** (a) Inherit the stage's authority (advisory here), with no per-reviewer authority. (b) Binding at a severity.

**D4 — document selection.** (a) The whole index only. (b) **The index plus the full records whose `applies_to` matches** the plan scope ∪ diff, up to the cap, with the rest reachable through read-only grounding (ADR-078). (c) A model preselects records.

**D5 — secrets and permission drift.** (a) **Deterministic server-side checks** emitting server-synthesized concerns. (b) A prompt lens.

**D6 — where personas run.** (a) In-process in fishhawkd, like today's reviews. (b) A new stage.

## Recommendation

**D1 (b), D2 (c), D3 (a), D4 (b), D5 (a), D6 (a).** Binding rules:

1. **A persona is a reviewer, not a stage.** A named persona declares a model configuration and a remit document. It runs as its own `reviewerInvocation` with a prompt built for it: the stage's normal review inputs plus its remit and selected documents, framed per ADR-068 (subordinate, base-ref resolved via E55.7 #3746, attributed). **The standard reviewers' prompts do not change**, which protects E55.4's catch-rate gate.
2. **Attachment.** A persona is attached statically to a stage's reviewers (the architect on the plan stage) or by an escalation's `require.reviewers` (the security officer on sensitive paths). Escalation attachment matches the **union of the approved plan's scope and the diff's files**, so scope drift into a sensitive path still attaches the persona. Both are additive, optional `workflow-v2` grammar, validated in backend and CLI and mirrored per the schema checklist.
3. **Advisory, checked at ingest.** A persona inherits its stage's authority; there is no per-reviewer authority. Its concerns enter the existing concern state machine with a `reviewer_role` attribution, clamped by the remit's `severity_cap`. **Citation check:** a concern that quotes a remit or decision-record text is verified at ingest against the text the server actually injected. A quote not found there is demoted and marked, never trusted.
4. **Selection by index.** Documents are selected through a machine-readable index (`docs/adr/index.json` for the decision record) by `spec.Predicate` over plan scope ∪ diff, up to the injection cap, with loud truncation and read-only grounding for the rest. There is no model preselection.
5. **Deterministic checks are concern sources, and they attach the persona.** Diff secret detection (the `redaction` patterns over added lines, reporting location and pattern class but **never** the matched bytes) and permission-drift detection (widening of a declared list of permission surfaces: workflow `permissions:` blocks, App `default_permissions`, token scope grants, reviewer environment allow-lists, and `forbidden_paths` / `escalations` / `autonomy` in the spec) emit **server-synthesized concerns**. These use a distinct provenance value, the same state machine, and are never waivable by an agent. A hit also attaches the security persona to that review.
6. **Where they run.** In-process in fishhawkd (N6). When runner-hosted reviewers land (E63 #2307, ADR-074), personas move with them. That is a stated obligation on E63, not a fork.
7. **Every persona prompt passes the E55.4 hard eval gate** before it is enabled on this repository, including a planreview-miss-corpus arm for its own remit (ADR conflicts for the architect, sensitive-path defects for security).

## Decision

**Accepted (2026-09-27). Ratified by Brett (repository maintainer) on the explicit instruction "ratify both ADRs" after reviewing this proposal.** The agent records the maintainer's decision; it is not self-approval.

Adopt the Recommendation as binding: D1 (b) a persona is a separate reviewer invocation with its own prompt, leaving the standard reviewers' prompts unchanged; D2 (c) static or escalation-attached via `require.reviewers`, matched on plan scope ∪ diff; D3 (a) advisory, inheriting stage authority, with `severity_cap` and ingest-time quote verification; D4 (b) index-driven selection with read-only grounding for the rest; D5 (a) deterministic secrets and permission-drift checks as server-synthesized, non-agent-waivable concern sources that also attach the security persona; D6 (a) in-process, moving with E63 when it lands. Rules 1–7 bind the implementation, including passing the E55.4 hard eval gate before any persona is enabled.

**Scope of this ratification:** the architecture above. The E55 children it depends on move to `milestone:alpha` by the same instruction.

## Consequences

- **Critical path moves into alpha.** Both personas depend on E55.2 #2243, E55.3 #2244, E55.4 #2245 and E55.6 #2797 (all currently `milestone:beta`) plus E55.7 #3746. E55 itself was moved to alpha by ADR-080; its children should follow.
- **Children once ratified:**
  - Shared: a per-invocation persona prompt and the persona grammar; escalation `require.reviewers`; ingest-time quote verification and `reviewer_role` attribution.
  - E78: decision-record selection by index; the plan-fit persona; the architect consult responder (deterministic index lookup first, per ADR-081 rule 8).
  - E80: the diff secrets check; the permission-drift check; the security persona; linking advisories to in-flight runs as E77 findings.
  - Capstone: human-authored remit documents and their declaration on `feature_change`.
- **Cost** lands only where a persona is attached: statically on plan review for the architect, and on escalated paths for security.
- **Out of scope:** binding reviewer authority, new stage types, model preselection of documents, and replacing SAST/SCA products.
- **Reversal:** remove the persona declarations. The standard reviewers were never changed, so there is nothing to restore.

## Amendment A1: server-check concerns are refused on agent-drivable channels (ratified by the Captain 2026-11-12)

**Context.** Rule 5 says a server-synthesized concern is never waivable by an agent. The guard that enforces it (E80.3, #3760) decides "agent" from the credential: it refuses an operator-agent, run-bound or first-officer-seat subject, and a request that declares itself delegated. A credential's subject does not say who is using it. An MCP server configured with a human-bound credential and driven by an agent session presents a human subject, so the guard admits it. This was found on this repository's own deployment on 2026-11-12 (#5228).

**Options.**
- **A.** Leave the guard as it is and rely on each deployment to give its agent sessions an operator-agent credential.
- **B.** Enforce rule 5 by channel as well as by identity.
- **C.** Add a recorded grant under which an agent may clear such a concern. That weakens rule 5 and is not taken.

**Decision.** Option B. A request to clear a server-synthesized concern is refused when it arrives over an agent-drivable channel (any MCP transport), regardless of the subject its credential carries; a human clears it through the Web UI session or the CLI. The existing subject and delegated tests stay. Rule 5 is otherwise unchanged, and no agent path is added.

**Consequences.** Implemented in #5228 for the waive, bulk-waive and defer verbs. A human keeps a cheap path through propose-and-confirm (#5229). This amendment does not decide whether the same test applies to other human-only actions, what makes a CLI call human, or whether a deployment that gives an agent session a human-bound credential is supported; ADR-143 (#5231) proposes those. No reviewer saw this text before ratification.

## Amendment A2: reviewed allowlist and in-scope fixture markers suppress diff-secret hits (ratified by the Captain, 2026-11-17 (issue #5230 comment))

Ratification: Captain, 2026-11-17 (issue #5230 comment); final text confirmed by the Captain at merge.

**Context.** Rule 5's diff secrets check (E80.3, #3760) raises every credential-shaped added line as a high concern that only a human can clear. Test fixtures and documented placeholders produce most of these, so humans spend waives on known non-secrets (#5230). A suppression arm must not create a path by which a change hides its own credential, and must not render the bytes it suppresses.

**Options.**
- **A.** Keep raising every hit and rely on human waives.
- **B.** A reviewed allowlist file plus a per-line fixture marker honoured only inside a declared marker scope, read only at the run's admission commit, with the file itself a permission-drift surface.
- **C.** Let an agent waive a hit it judges to be a fixture. That contradicts rule 5 and is not taken.

**Decision.** Option B, under seven points.
1. **Suppression arm.** `.fishhawk/diff-secrets-allowlist.yaml` (version 1) declares `entries` (path globs × pattern classes × reason) and `markers` (marker scopes, the same shape). An added line carrying `fishhawk:allow-secret` is suppressed only when it lies inside a declared marker scope for that pattern class. The marker is per line: a token on another line, a context line or a removed line marks nothing.
2. **Trust boundary.** The server reads the file only at the run's admission commit (`run.DocumentBaseCommit`), never at a head or pass base, and fails closed: every read, configuration or parse failure suppresses nothing. The file is the high-severity `diff-secrets-allowlist` permission-drift surface, keyed per rule by validated id plus fingerprint, so a widening raises a human-only concern.
3. **Never the bytes.** No allowlist-derived string is rendered verbatim. A rule is attributed by its validated id and a fingerprint (truncated SHA-256 of list, glob and pattern); globs are never rendered. A rule whose id, glob or reason matches the redaction pattern set is refused (`redaction_hit`). A hit whose path is unresolved, redacted, or the allowlist file itself is never suppressed.
4. **Reporting.** Entry-suppressed hits are counted and attributed only. Marker-suppressed hits are listed by location, and each distinct hit raises a low, non-blocking, human-only `server_check` concern (category `diff_secrets_marker_suppressed`), de-duplicated per hit. This answers the self-marking path: adding a marker line is a suppression event a human sees.
5. **Persona.** An entry-suppressed hit does NOT attach the security persona. A marker-suppressed hit DOES; the raise path's return seam carries it for #3762.
6. **No agent settlement.** Suppressed hits and marker concerns are never waivable or settable by an agent. No verb records or clears a suppression; a marker concern clears only per A1 and ADR-143; and the run cannot edit the allowlist in effect, because it is read at the admission commit.
7. **Staleness.** A long run, or a decomposed child that inherits `DocumentBaseCommit`, keeps the allowlist as admitted. A widening never takes effect inside the run that makes it, and a narrowing takes effect only for a fresh run, which is fail-safe.

**Consequences.** Implemented by #5230 in two slices: the format, the drift surface (`SurfacesVersion` 2), the shared summary wire types and the PR status section first; the server read, the raise path, the gate-view block and the conflict-resolution counts second. The audit category stays `diff_secrets_detected`; `diff_secrets_marker_suppressed` is a concern category, not an audit category. A low marker concern does not block the human merge verb, but like every open concern it holds delegated settlement until a human dispositions it. Releasing delegated settlement for it would be a separate decision.
