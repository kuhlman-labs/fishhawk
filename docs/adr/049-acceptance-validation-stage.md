---
id: ADR-049
title: "Acceptance-validation stage: agent-driven behavioral validation against intent before release"
status: accepted
date: 2026-07-01
issue: https://github.com/kuhlman-labs/fishhawk/issues/1519
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-049: Acceptance-validation stage: agent-driven behavioral validation against intent before release

## Context

In human-led development, a UAT/staging environment is where a person validates a change end-to-end against **intent** — "does this do what we actually asked?" — before it ships. It catches the defect class unit tests structurally cannot: seam/integration failures, and the gap between "the code the agent wrote passes the tests the agent wrote" and "the feature behaves as the requester meant."

Fishhawk's gate chain has **no equivalent**. What exists, and why each falls short:

- **Committed-tree verify-fix loop** (#651, `executor.verify.command`) — runs `go vet` / `go test` / a configured command against the *isolated committed source tree* on the runner. Execution-grounded, but it validates **source**, never a **running instance**, and the tests it runs are largely the ones the implement agent authored — self-referential.
- **Advisory review agents** (ADR-027) — independent agents (heterogeneous, e.g. opus-4-8 + gpt-5.5) that judge the **plan and diff** against intent and emit structured verdicts with no write authority. Independent and intent-grounded, but they read *artifacts*, never *runtime behavior*.
- **Delegating deploy stage** (ADR-038, `workflow-v1`) — gates deploy *intent* **pre-execution** and records a signed `deployment` *outcome*. It never independently validates *behavior after* execution; Fishhawk holds no credentials and provisions nothing.
- **`fishhawk_verify_run`** — verifies audit-chain integrity, not behavior.

The missing capability is precise: **execution-grounded, intent-grounded, independent validation of a running instance, feeding a gate, before release.** This ADR decides whether and how to add it.

Two invariants tightly constrain the solution space and must be honored, not worked around:

- **Invariant #1 — customer source code and runtime never reach Fishhawk's backend.** The backend sees traces, plans, metadata; never source, never a live app. This rules out any design where Fishhawk observes the running system directly.
- **Rule-of-Two posture (ARCHITECTURE §6).** `review` is "n/a — advisory only, no state mutation / external write." An agent that drives a running app touches network + untrusted runtime output and possibly credentials, so its posture must be placed explicitly on this table, not assumed.

And a scope guard: **MVP_SPEC §9** keeps the stage set a small closed set and puts "deploy *mechanics*" / "a CI/CD platform" / environment provisioning explicitly **out of scope** ("we do not compete with Argo / Spinnaker / GitHub Environments"). ADR-038 added *exactly one* stage type under that guard by staying delegating. Any acceptance design that has Fishhawk provision environments re-opens exactly the scope §9 closed.

## Options

### Option A — No new stage; extend `verify.command` to hit a URL
Let the existing committed-tree verify loop run against a running instance (the customer's `verify.command` curls a preview URL / runs Playwright).
- **Pros:** zero new stage type; reuses #651 entirely; ships today via spec config.
- **Cons:** no *independent* validator (the implement agent still authors the check → self-referential, the exact circularity we're trying to break); no intent-grounding (a shell command has no notion of "what was asked"); no structured evidence bundle or verdict; conflates "compile/test the source" with "validate a deployment." Fails the two properties that make UAT valuable.

### Option B — New delegating `acceptance` stage: runner-hosted advisory agent + evidence bundle → gate  *(recommended)*
A new closed-set stage type. An **independent acceptance agent** runs **on the customer's runner** (same place plan/implement/review agents already run — so no backend-boundary crossing), reads **intent** (issue + spec + approved plan's `verification` section), exercises a **running instance whose provisioning is the customer's responsibility** (a prior deploy-to-preview stage, or the customer's existing PR-preview infra), and emits a **signed evidence bundle** (structured verdict + trace + optional artifacts) that feeds a gate. Advisory posture (mirrors `review`), delegating provisioning (mirrors `deploy`).
- **Pros:** independent validator (different agent, ideally a different model, reading intent not the diff — breaks circularity); execution-grounded against a real running instance; honors Invariant #1 (agent runs customer-side; only redacted trace/verdict/evidence-metadata reach the backend, exactly like today's trace bundles); honors the closed-set guard by staying delegating like ADR-038; reuses three existing mechanisms wholesale (trace-bundle signing/ship, `deployment`-style artifact + audit-kind + living-anchor surface, ADR-040 condition-gated gate).
- **Cons:** a second closed-set expansion (must be justified against §9); the acceptance agent needs network + possibly test credentials → new entry on the Rule-of-Two table; evidence blobs (screenshots/recordings) can contain customer data → redaction/residency question; depends on the customer having *some* running instance to point at.

### Option C — Fishhawk-provisioned ephemeral preview environments
Fishhawk spins up an isolated production-like environment per run and observes it.
- **Pros:** the most literal UAT analogue; strongest data realism.
- **Cons:** **violates Invariant #1** (Fishhawk would touch customer runtime) and re-opens the exact "deploy mechanics / CI-CD platform / environment provisioning" scope MVP_SPEC §9 closes. Rejected on invariant + scope grounds, independent of effort.

### Option D — Reuse the `review` stage with a runtime-capable reviewer
Give a review agent network access to a running instance instead of adding a stage.
- **Pros:** no new stage type; inherits review authority/settling machinery.
- **Cons:** conflates artifact-review (no side effects, no network) with runtime-validation (network, untrusted output, latency, flake) under one Rule-of-Two row, muddying the cleanest posture in the system; makes the `review` gate depend on a running instance existing; harder to position independently in the lifecycle. Rejected — the posture blurring is not worth the saved stage type.

## Recommendation

Adopt **Option B**, with these sub-decisions:

1. **New stage type `acceptance`**, added the same way `deploy` was: constant in `backend/internal/run/run.go` and `backend/internal/spec/spec.go`, enum in the workflow JSON schema (additive `workflow-v1.x` if it can be optional; a `workflow-v2` bump only if it must be required), and type↔member binding rules in `backend/internal/spec/validate.go`.

2. **Delegating provisioning.** Fishhawk never provisions or tears down an environment. The stage validates against a running instance the *customer* stood up — typically a preceding `deploy` stage targeting a non-prod environment (`staging`/`preview`), or the customer's existing PR-preview infrastructure surfaced to the runner as a URL input. This keeps us inside MVP_SPEC §9.

3. **Runner-hosted, advisory agent.** The acceptance agent is a runner subprocess (customer CI), like every other Fishhawk agent — so runtime observation happens customer-side and Invariant #1 holds. Its Rule-of-Two posture is a **new row**: advisory (no repo/state write authority, like `review`) but with an explicit, spec-declared **network egress allowance** to the target instance and a default-deny for everything else. Test credentials, if any, are the customer's, injected runner-side, never seen by the backend.

4. **Intent-grounded, not diff-grounded.** The agent's inputs are the issue, the workflow spec, and the **approved plan's `verification` section** (MVP_SPEC §4.3 already carries "test strategy / rollback plan"). Acceptance criteria are authored/frozen at **plan time** and reviewed at the plan gate — so the criteria are fixed before implementation and cannot be back-fitted to whatever was built. The agent is deliberately **not** given the diff, to preserve independence.

5. **Evidence bundle, reusing existing infra.** The stage produces a signed bundle exactly like today's trace bundles (per-run ephemeral key, redaction pipeline, S3 content-addressed). It persists an `acceptance` artifact in the Postgres `artifacts` table (`data_jsonb` = structured verdict + criterion-by-criterion results + `content_hash` refs to any large blobs), emits `acceptance_*` audit kinds, and renders **data-drivenly on the living anchor** — mirroring the `deployment` / `deployment_outcome_recorded` pattern verbatim.

6. **Gate semantics via ADR-040 condition-gating.** An acceptance **pass** may satisfy a named delegation condition feeding the downstream gate (e.g. `may_approve_deploy: acceptance_passed`, analogous to `gates_resolved_ci_green`), so a clean pass need not page a human. An acceptance **reject or ambiguous/degraded verdict is a non-delegable `must_page_human` event** — the human reviews the evidence bundle and arbitrates, consistent with the existing rule that deploy rejection always pages.

7. **Placement: pre-deploy (post-merge) for v1**, spec-positionable. The canonical flow becomes `review → merge → deploy(staging) → acceptance(vs staging) → [human gate] → deploy(prod)`. A pre-merge preview-env variant (validate a PR preview before merge) is a documented option the spec can express later; v1 anchors on pre-deploy because it reuses the deploy stage's provisioning and pre-execution-gate machinery directly.

The through-line: **acceptance validation is what turns the deploy gate from intent-based into evidence-based.** Today deploy approval is "approve the intent, pre-execution." This inserts observed behavioral evidence, judged independently against frozen intent, before the human commits to release.

## Decision

**Accepted (2026-07-01).** Adopt Option B — a new delegating, advisory, runner-hosted `acceptance` stage. Ratified and decomposed into epic **E31 (#1528)**, driven as an ADR-047 campaign over its child issues.

The Recommendation stands, with the following refinements settled during ratification. Where they differ from the Recommendation above, **these govern**:

1. **Placement flip — pre-merge is the default (supersedes Recommendation #7).** The stage validates an **ephemeral preview built from the PR ref** and gates the **merge**, run on the **final pass only**. Rationale: a pre-merge acceptance failure collapses into the existing fixup/retry/waive loop and never contaminates trunk, whereas post-merge failure forces revert/roll-forward on a shared branch and risks irreversible staging side effects. The pre-deploy/staging placement is retained as a documented, spec-positionable **"release-acceptance" variant** for changes that require staging realism — with the understood harder failure contract.

2. **Failure routing is a triage, not auto-fixup.** The verdict distinguishes `error` (objective — crash/500/exception → class-1 bug) from `assertion_fail` (behaved-but-unexpected → needs triage). Routes: class-1 → `fixup` (behavioral evidence as the concern payload), class-2 (flake/env) → `retry`, class-3 (bad/ambiguous criterion) → revise-criteria or waive, class-4 (works-as-planned but disputed) → arbitrate/replan. Bounded to 1–2 acceptance re-runs (each rebuilds the preview); a code change re-reviews before re-accept; non-convergence pages the human.

3. **Acceptance criteria become structured and provenance-tagged.** `verification.acceptance_criteria` is a typed list (`id`, `statement`, `source` = explicit|inferred, `source_ref`, `rationale` [required when inferred], `blocking`, optional `verify_hint`/`preconditions`), plus a sibling `verification.out_of_scope`. Additive within `standard_v1` (optional field, `x-intended-required`). Each criterion's `id` is the join key across plan → execution → evidence bundle → triage → feedback. Runtime `expected`/`observed` live in the evidence bundle, not the plan.

4. **Plan-gate enforcement is two-tier.** A deterministic `plan_acceptance_precheck` hard-gates the objective rules (presence-or-justified-absence via `out_of_scope`, provenance completeness, id integrity); the plan reviewers judge the semantic rules (coverage, warrant-of-inferred, testability, independence, falsifiability) under existing ADR-027 authority. A class-3 triage outcome = a plan-review miss, fed back to the agent-eval corpus.

5. **Evidence-blob residency** defaults to customer-side (only hashes + structured verdict cross; verdict + small textual evidence may ride the redacted trace bundle). Final call deferred to a child issue.

Named approver: repository maintainer (human).

## Consequences

**Positive**
- Closes the one structural gap between "tests the agent wrote pass" and "the feature does what was asked," with an *independent, intent-grounded, execution-grounded* check.
- Zero new invariant violations: agent runs customer-side, backend still sees only redacted evidence.
- Makes the deploy gate evidence-based; strengthens the audit story (a signed, replayable record of *what behavior was observed* at release time).
- Reuses trace-bundle, artifact/audit-kind/anchor, and condition-gating machinery — low net-new surface for the value delivered.

**Negative / risks**
- **Second closed-set expansion.** Must be justified against MVP_SPEC §9 each review; the delegating posture is the justification (same as ADR-038).
- **The oracle problem moves onto the critical path.** If the plan's `verification` criteria are vague, the acceptance agent guesses — reproducing the builder's failure mode one layer over. Mitigation: criteria are a reviewed plan-gate artifact; a plan with empty/weak acceptance criteria for a behavioral change should be a plan-review concern.
- **Validation can hallucinate a pass.** An agent verdict is not evidence. Mitigation: the bundle must carry *observable artifacts* (HTTP traces, DB state assertions, screenshots) that a human or a deterministic post-check can confirm — verdict + evidence, never verdict alone.
- **Correlated blind spots.** Same model building and validating shares blind spots. Mitigation: default the acceptance agent to a *different provider/model* than implement, reusing the heterogeneous-reviewer config (ADR-027 / #955).
- **Evidence residency/redaction.** Screenshots/recordings of a running app can contain customer data or rendered source. Open question: do blobs cross into Fishhawk's S3 (redaction pipeline applies, same as traces) or stay customer-side with only hashes + verdict crossing? Leaning customer-side-by-default with an opt-in; to be settled in the epic.
- **Cost.** A full extra agent pass + a live environment per run is real spend. Mitigation: gate by change class (config tweaks skip it; behavioral/cross-boundary changes require it), expressed in the spec.
- **New Rule-of-Two row.** An advisory-but-network-capable agent is a genuinely new posture; the egress allowance must be spec-declared and default-deny, and this row needs explicit security review (autonomy:low-adjacent).

**Follow-ups (filed under epic E31 / #1528)**
- Epic: `acceptance` stage type + schema + validator bindings (mirror ADR-038 mechanics).
- Runner: acceptance-agent executor, network-egress posture, evidence-bundle capture.
- Backend: `acceptance` artifact, `acceptance_*` audit kinds, living-anchor surface, condition-gate wiring (`acceptance_passed`).
- Spec/docs: `verification`-section → acceptance-criteria contract; plan-review concern for weak criteria; workflow-v1.x grammar + `docs/spec/` + embedded-copy sync.
- Decision: evidence-blob residency (customer-side vs redacted-to-S3).
