---
id: ADR-008
title: "Signing scheme (Ed25519, canonicalization, issuance protocol)"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/72
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-008: Signing scheme (Ed25519, canonicalization, issuance protocol)

## Context

Per MVP_SPEC §5.3, each run gets a per-run ephemeral signing key. The backend issues at job start; the runner uses it to sign the trace bundle; the backend verifies on receipt. Decision: which signing scheme, key encoding, message canonicalization, and issuance protocol?

## Options

- **Ed25519** — modern, fast, small keys/signatures, IETF-blessed. Default for new systems.
- **ECDSA P-256** — broader compatibility with HSMs; more failure modes (deterministic-k vs. random-k).
- **RSA** — broadest compatibility; larger keys/signatures; slower.

For canonicalization:

- **Sign hash of canonicalized JSON** — works with JSON Lines bundle (ADR-007). Need a canonicalization rule (sorted keys, no whitespace).
- **Sign hash of raw bundle bytes** — simpler; commits to the exact byte sequence.

## Recommendation

Ed25519 + sign hash of raw bundle bytes (sha256). Simplest and minimizes canonicalization surface.

## Decision

**Recorded 2026-04-30: Ed25519 over sha256 of raw bundle bytes.**

Issuance protocol:

1. Runner action starts. Calls backend's `POST /v0/runs/{run_id}/signing-key` with the GitHub OIDC token (verified by the backend against GitHub's JWKS).
2. Backend mints a fresh Ed25519 keypair, scoped to that `run_id`, with a 30-minute TTL. Returns the **private key** to the runner over TLS in the response body and stores the **public key** server-side keyed by `run_id`.
3. The private key is never persisted by the runner — it lives in process memory until trace upload, then is overwritten and the process exits.
4. The runner signs `sha256(raw bundle bytes)` and ships `(bundle, signature, run_id)` to `POST /v0/runs/{run_id}/trace`.
5. Backend looks up the public key by `run_id`, verifies, then either accepts the bundle (storing it) or marks the run as a category-C failure with reason `signature_invalid`.

Signing details:

- Algorithm: pure Ed25519 (`ed25519.Sign` from `crypto/ed25519` — no pre-hashing).
- Message: literally `sha256(raw_compressed_bundle)` — 32 bytes. Ed25519 hashes internally; pre-hashing adds a layer of canonicalization-free commitment.
- Signature encoding: 64 bytes raw, hex-encoded in the JSON envelope.

Key chain root:

- Backend's `signing_keys` table stores `(run_id, public_key, issued_at, expires_at)`.
- An entry is immutable once written. A mutation attempt is itself a security event.
- The external verification tool (E2.6 / #27) reads the `(run_id, public_key)` pair from an audit-log export and verifies the corresponding bundle against the public key. No backend trust required.

## Consequences

**Easier**
- Smallest possible attack surface: no JSON canonicalization (a notorious source of signature bypass bugs).
- Standard library `crypto/ed25519` — no third-party crypto dependency.
- External verification needs only sha256, Ed25519, and a public key. Implementable in any language without trusting Fishhawk.
- Per-run keys mean a compromised runner only forges traces for *that one run*. The blast radius is contained.

**Harder**
- The GitHub OIDC verification path on the backend has to be implemented carefully. Mitigation: we'll vendor a small, audited OIDC verifier rather than rolling our own (E4 /#4 territory).
- Key TTLs of 30 minutes mean a slow runner that exceeds the TTL fails category-C. Acceptable; the runner SLA in the spec is well under 30 minutes.

**Other decisions this constrains**
- E2.3 (#24) implements the issuance + verification primitives as described.
- E2.6 (#27) implements the external verifier consuming `(run_id, public_key, bundle, signature)` tuples.
- E5.6 (#32) implements the runner-side signing call.

## Spec reference

`docs/MVP_SPEC.md` §5.3

## Target deadline

Day 7 — **met**.

---
Parent epic: #15
