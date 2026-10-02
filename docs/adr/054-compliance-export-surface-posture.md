---
id: ADR-054
title: "Compliance export surface posture (auth scope, redaction, filtering semantics)"
status: accepted
date: 2026-07-02
issue: https://github.com/kuhlman-labs/fishhawk/issues/1582
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-054: Compliance export surface posture (auth scope, redaction, filtering semantics)

## Context

ADR-008 (#72) decided offline verification: (run_id, public_key) + the canonical hash algorithm suffice to recompute the chain. The verifier module fully implements the consumer side — verifier/internal/audit/export.go defines Export schema v1 (runs map of signing_key + audit_entries), verify.go recomputes hashes/chains/signatures with typed issue kinds — but NO backend code produces that shape: there is no export endpoint, and the audit-repo methods annotated &#39;used by the compliance export&#39; have no export caller. E9 (#9) is the epic; its v0 scope (JSON export with trace pointers, CSV, date/repo/approver filters, external verification path, canned agent-changes report) maps directly onto this gap. What ADR-008 did NOT decide — and what this ADR settles — is the export SURFACE posture: authorization, redaction, filtering semantics, and size behavior.

## Options

**Auth — Option A: dedicated `read:audit-export` scope (RECOMMENDED).** Bulk evidence export is a different risk class than the per-run read the UI uses: it is the exfiltration-shaped operation, and compliance officers are a distinct principal from operators. A dedicated scope makes export-capable tokens enumerable and revocable independently. **Option B: reuse `read:audit`.** Simpler, but every UI-read token silently becomes a bulk-export token.

**Redaction — Option A: redacted-only by default; raw trace POINTERS included but raw-bundle access stays governed by the existing compliance-gated raw-variant path (RECOMMENDED).** The export carries entry payloads (already redacted at write time where applicable) and content-hash pointers; it never inlines trace bundles. **Option B: full raw inline export.** Breaks the redaction boundary established for traces and bloats exports unboundedly.

**Filtering — audit_entries has no repo column, so repo/date filtering joins through runs (the calibration.go precedent). Option A (RECOMMENDED): filters resolved server-side (date-range, repo, run-set), export assembled per-run to match the verifier&#39;s runs-map shape exactly; global-chain entries included under their own partition. Option B: client-side filtering of a full dump — rejected at any real volume.

**Size — Option A (RECOMMENDED): bounded export with explicit continuation (the export is a compliance artifact; silent truncation is disqualifying — a partial export must SAY it is partial and be resumable).**

## Recommendation

Dedicated scope, redacted-with-pointers default, server-side runs-join filtering assembled to the verifier&#39;s exact v1 shape, bounded-with-continuation size posture. The verifier&#39;s export.go is the BINDING wire contract — the producer must round-trip against fishhawk-verify in CI (the MVP §13 external-verification done criterion becomes an integration test). Implementation: six children under the existing E9 (#9), filed alongside this ADR.

## Decision

**ACCEPTED 2026-07-02** (founder-delegated ratification; selected as the highest-value open ADR — the compliance export is the design-partner wedge and its wire contract already exists in the verifier). All four recommended postures are adopted:

1. **Auth:** dedicated `read:audit-export` scope (Option A). Export-capable tokens are enumerable and revocable independently of UI-read tokens. Rollout follows the AGENTS.md auth-change checklist (impact inventory + migration path in the E9.5/#1608 PR body).
2. **Redaction:** redacted-only default with raw trace POINTERS; raw-bundle access stays behind the existing compliance-gated raw-variant path (Option A). Exports never inline trace bundles.
3. **Filtering:** server-side runs-join filtering (date-range, repo, run-set), export assembled per-run to the verifier's exact v1 runs-map shape; global-chain (run-less) entries explicitly partitioned, never silently dropped (Option A).
4. **Size:** bounded export with explicit continuation — a partial export declares itself partial and is resumable; silent truncation is disqualifying (Option A).

`verifier/internal/audit/export.go` is the BINDING wire contract; the producer must round-trip against `fishhawk-verify` in CI (E9.4/#1607). Unblocks the E9 (#9) campaign: #1604 → {#1605, #1606, #1607, #1608} → #1609. #1608 (auth scope/redaction enforcement) is autonomy:low — founder-led merge.

## Consequences

Positive: E9 becomes runnable with the architecture questions pre-answered; the export round-trip test turns the product&#39;s central compliance promise into CI; the dogfood export of Fishhawk&#39;s own development history (MVP line 475) becomes shippable. Negative/risks: a new scope triggers the AGENTS.md auth-change checklist (token impact inventory); export assembly at volume needs the pagination posture honored from day one; the runs-join filter must not silently drop global-chain (run-less) entries — partition them explicitly.
