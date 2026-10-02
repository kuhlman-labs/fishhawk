---
id: ADR-003
title: "Object storage choice + dev-loop strategy"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/67
supersedes: []
superseded_by: []
applies_to: ["docker-compose.yml"]
---

# ADR-003: Object storage choice + dev-loop strategy

## Context

Trace bundles (full prompt/tool-call history per workflow run) are large and append-only. They live in S3-compatible object storage per MVP_SPEC §4.4. Choice depends on the cloud target (ADR-001) but also on dev-loop ergonomics: do we mandate a real cloud bucket for local dev, or do we run MinIO?

## Options

- **AWS S3 (or equivalent)** — production storage; for dev, MinIO container in docker-compose.
- **GCS** — if cloud is GCP per ADR-001.
- **Cloudflare R2** — egress-free; useful if traffic patterns warrant.

For dev:

- **MinIO in docker-compose** — closest to prod API surface, easy local setup.
- **Real cloud bucket per developer** — costs money, slower local loop.
- **Filesystem-backed shim** — fast but doesn't catch S3-quirks.

## Recommendation

Resolve with ADR-001 first. Use MinIO for local dev regardless of prod choice.

## Decision

**Recorded 2026-04-30: AWS S3 (production), MinIO via docker-compose (local dev).**

Layout decisions:

- **Bucket-per-environment** (e.g., `fishhawk-traces-prod`, `fishhawk-traces-staging`).
- **Key prefix scheme**: `{run_id}/redacted/{sha256}.jsonl.gz` and `{run_id}/raw/{sha256}.jsonl.gz`. Run-ID prefix gives natural lifecycle policies per run; the redacted/raw split mirrors the access-control split from MVP_SPEC §4.4.
- **Bucket policy**: deny `s3:DeleteObject` to all principals except a dedicated lifecycle-management role. Append-only at the storage layer reinforces the audit append-only invariant.
- **Object Lock** (Compliance Mode) for the `raw/` prefix on customer-facing tier (post-v0, gated by retention SLA decision).
- **Local dev**: `docker-compose.yml` runs MinIO on port 9000 with the same key layout. Dev backend uses `aws-sdk-go-v2` with `BaseEndpoint` pointed at MinIO.

## Consequences

**Easier**
- Direct path to S3-Object-Lock for compliance-grade customer tiers in v1+.
- The same SDK call works against MinIO and S3, so dev/prod behavioral drift is minimized.
- Lifecycle policies can move old traces to S3 Glacier without code changes.

**Harder**
- Two storage tiers (redacted + raw) means twice the writes. Acceptable; trace bundles are I/O-light per request.
- MinIO has occasional API gaps vs. S3 (rare, but they exist). Pin a specific MinIO version and run integration tests against both targets in CI as the audit log lands (E2.2 / #23).

**Other decisions this constrains**
- ADR-007 (trace wire format) → JSON Lines + gzip; `.jsonl.gz` keys reflect that.
- E2.2 (#23) implements the bucket layout described above.

## Spec reference

`docs/MVP_SPEC.md` §4.4 (storage)

## Target deadline

Day 5 — **met**.

---
Parent epic: #15
