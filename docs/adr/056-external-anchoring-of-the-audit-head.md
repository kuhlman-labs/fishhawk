---
id: ADR-056
title: "External anchoring of the audit head: tamper-evidence beyond the exported chain"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/1699
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-056: External anchoring of the audit head: tamper-evidence beyond the exported chain

## Context

The audit chain is hash-linked, verdicts are ed25519-signed, E9 ships an Export v1 wire contract, and #27 (E2.6) delivered an external verifier: a third party can verify an export's internal consistency without trusting the hosted backend. What no artifact currently proves is that the chain wasn't **rewritten before export** — a server that controls the database and the signing keys can fabricate a self-consistent history. "Keeper of the record" needs an external commitment: a periodic anchor of the audit head hash somewhere Fishhawk cannot silently rewrite, so any later tampering is detectable against the anchored heads.

## Options

**A. Git commit trailers.** Stamp the current audit head hash (per repo) into a trailer on commits Fishhawk already makes (merge commits / status artifacts). Cheap, forge-agnostic, colocated with the code history it attests. Granularity is tied to merge cadence; anchors are only as durable as the git history (force-push on a protected main is the threat boundary, which is acceptable and documentable).

**B. Signed checkpoint file in-repo.** A `.fishhawk/checkpoints` append-only file updated on a cadence (per merge or daily), each entry `{seq, head_hash, ts}` signed with the runner/backend key. Verifiable offline together with the export; same durability boundary as A but independent of commit message plumbing and easier for the verifier to consume.

**C. Public transparency log (e.g. Sigstore Rekor).** Strongest guarantee (external, append-only, third-party-operated), forge-independent. Adds an external service dependency + privacy consideration (head hashes leak activity cadence, not content). Could be optional/enterprise.

**D. Forge release/artifact anchoring.** Attach checkpoint to GitHub Releases / GitLab equivalents on release cadence. Coarse granularity; ties anchoring to the release workflow (E33 synergy).

## Recommendation

B as the default (in-repo signed checkpoints, per-merge cadence, consumed by the existing external verifier so `verify` can check both internal consistency AND anchored heads), with C as an opt-in flag for deployments that want a third-party log. A falls out of B nearly for free if desired. Revisit D when E33 release governance lands (a release's evidence bundle should embed the checkpoint covering its range).

## Decision

**Accepted (2026-07-26).** Adopt **Option B as the default** — in-repo signed checkpoints, consumed by the existing external verifier (#27) so `verify` checks internal consistency **and** anchored heads — with **Option C (Sigstore Rekor) as an opt-in** for deployments wanting a third-party log. Three forks were settled; where they refine the Recommendation, **these govern**.

### 0. Finding: the per-account chain landed after this ADR was filed, and it changes Option B's shape

This ADR was filed 2026-07-08. **ADR-057 (#1823) decided two days later that the audit chain is per-account**, and **E44.4 (#1828) shipped it** — `AccountID *uuid.UUID` now selects the chain partition (`backend/internal/audit/chain.go:141`).

Option B as written assumed a repo-shaped chain: an in-repo `.fishhawk/checkpoints` file. A per-account chain spans many repos, so there is no per-repo chain for a per-repo file to anchor. That mismatch had to be resolved before this ADR could be implemented, and is fork 2 below.

### 1. Mechanism: B default, C opt-in

Unchanged from the Recommendation. In-repo signed checkpoints (`{seq, head_hash, ts}`, signed with the backend key, append-only) ship as the default and are verified offline alongside the export. Rekor stays **opt-in**: it would be the product's first third-party service dependency, nobody is asking for it yet, and head hashes leaving the tenant boundary collides with ADR-057's hard data-residency requirement. When enabled it must degrade cleanly — **an anchor miss warns, never blocks**.

Option A (git commit trailers) falls out of B nearly for free and may be added; Option D (release-artifact anchoring) is revisited when E33 release governance is exercised.

### 2. Scoping: replicate the ACCOUNT's checkpoints into every repo

Each repo in an account carries the same account-chain checkpoints.

**The redundancy is the security property, not overhead.** Forging history requires rewriting the checkpoint file in *every* repo of the account — force-pushing N protected branches rather than one. That is a materially stronger threat boundary than a single designated anchor repo, and it is the reason to prefer replication over the alternatives.

Rejected: a **designated anchor repo** (one force-push rewrites the anchor, and the designated repo must outlive every other repo in the account) and a **per-repo filtered view** (a checkpoint that commits only to entries touching that repo no longer commits to the chain, leaving entries between checkpoints uncovered — it would weaken the very thing a checkpoint proves).

**Accepted cost, to be documented rather than glossed:** a repo's collaborators can see head hashes covering account-wide activity. That reveals **cadence, not content** — how often the account's chain advanced, never what changed or in which repo. Deployments for which that is unacceptable are the case for the Rekor opt-in or a designated-repo variant; note it in the docs rather than discovering it in a customer conversation.

### 3. Timing: ratify now, build after E44 settles

The design and scoping are decided; implementation sequences **after E44 (#1824)**, which has 8 children still open. The per-account chain has shipped, but anchoring sits downstream of tenancy decisions those children are still making, and building against a partitioning that may still move would mean anchoring twice.

### The trust-model claim, stated precisely

The documentation must say what this does and does not provide: **tamper-evidence from the last anchored checkpoint, not tamper-proofness.** An attacker who controls the database, the signing keys, *and* can force-push every repo in the account can still forge a self-consistent history; what anchoring removes is the ability to do so **silently**. This is the same discipline as the patch-coverage gate's "tamper-evident, not tamper-proof" framing — and overstating it would be exactly the trust-as-a-marketing-claim posture `BRAND_FOUNDATIONS.md` §5 bans.

Named approver: repository maintainer (human).

## Consequences

- The external verifier (#27) gains an anchored-head check; docs state the trust model precisely: tamper-evidence from the last anchored checkpoint, not tamper-proofness.
- Small write path on merge (checkpoint append + sign); no schema change to the audit chain itself.
- Rekor opt-in introduces the product's first third-party service dependency — must degrade cleanly (anchor-miss = warn, not block).

## Relations

Related: #27 (E2.6 external verifier), E9 #9 (export surface), ADR-054 #1582 (export posture), E33/ADR-051 #1579 (release evidence should embed checkpoints). Follow-up from the 2026-07-07 product review.
