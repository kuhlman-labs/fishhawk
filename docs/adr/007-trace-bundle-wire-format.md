---
id: ADR-007
title: "Trace bundle wire format"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/71
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-007: Trace bundle wire format

## Context

The runner produces trace bundles (full prompt/tool-call history) that are signed and shipped to the backend. The wire format affects deduplication, redaction tooling, and how easily an external party can verify entries (MVP_SPEC §13 done criterion).

## Options

- **JSON Lines (NDJSON) + gzip** — human-readable, dedupes well via content addressing, easy for external verifiers to parse. Larger than binary formats.
- **CBOR** — compact binary, schema-flexible, easy to canonicalize for signatures. Less tooling.
- **Protobuf** — typed schema, smallest payload, fastest parse. Requires schema versioning discipline; less inspection-friendly without tooling.

## Recommendation

JSON Lines + gzip for v0. Inspection-friendly is a feature for an audit product. Revisit if storage costs balloon.

## Decision

**Recorded 2026-04-30: JSON Lines + gzip (`*.jsonl.gz`).**

Bundle layout:

- One JSON object per line, UTF-8 encoded.
- Each line is one trace event. Required envelope fields: `seq` (monotonic per-bundle), `ts` (RFC 3339 nanosecond), `kind` (event taxonomy: `prompt`, `tool_call`, `tool_result`, `model_response`, `error`, `policy_event`, `gate_event`), `data` (kind-specific payload).
- First line is a manifest event (`kind: "manifest"`) carrying schema version, run ID, stage ID, agent identity, model identity.
- Last line is a `kind: "trailer"` event with `event_count` and a content hash of all preceding lines, used to detect truncation before signature verification.
- gzip with default compression level (level 6); decompresses streaming.

Schema versioning: the manifest line carries `bundle_schema: "v1"`. v2+ stays additive within v1; breaking changes bump the major.

## Consequences

**Easier**
- A compliance officer can `gunzip | jq` a trace bundle and read it.
- The external verifier (MVP_SPEC §13) can run on any platform with `jq` — no protobuf compiler in the trust path.
- Streaming write/read fits the runner's incremental capture model.
- Content-addressing trivially dedupes (sha256 of compressed bytes is the storage key).

**Harder**
- Larger payloads than CBOR/protobuf. Acceptable for v0; trace volumes per run are bounded by token-budget caps.
- Each event line is independently parseable, so a corrupt middle line doesn't lose the whole bundle — but it does mean the trailer hash check is the integrity floor.

**Other decisions this constrains**
- ADR-008 (signing): we sign the **raw compressed bytes** of the bundle — canonicalizing JSON would otherwise be a separate decision surface.
- E2.2 (#23) storage layout puts `.jsonl.gz` files in S3 keyed by content sha256.
- E5.3 (#30) trace capture in the runner uses a streaming JSON Lines writer wrapped in a gzip writer.

## Spec reference

`docs/MVP_SPEC.md` §4.4, §5.3

## Target deadline

Day 7 — **met**.

---
Parent epic: #15
