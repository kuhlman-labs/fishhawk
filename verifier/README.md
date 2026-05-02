# fishhawk-verify

Standalone CLI that verifies Fishhawk audit log exports without trusting the backend that produced them. Per [ADR-008 (#72)](https://github.com/kuhlman-labs/fishhawk/issues/72), the `(run_id, public_key)` pair plus a copy of the canonical hash algorithm is sufficient to confirm the chain offline. This binary is the canonical "external party" in that story.

This directory is its own Go module (`github.com/kuhlman-labs/fishhawk/verifier`) so it can be released independently of the backend; customers and auditors can build it from this directory without pulling the rest of the repo.

## Why a re-implementation, not an import?

The verifier deliberately re-implements `ComputeEntryHash` and the bundle signature check rather than importing `backend/internal/audit` and `backend/internal/signing`. If a compromised backend could ship a tampered hash function alongside a tampered audit log, "external verification" would mean nothing. The re-implementation lives in `internal/audit/chain.go` and is pinned to the backend's algorithm by a **canonical fixture test**: both packages assert the same `(input, expected hash)` pair. Update one without the other and CI fails on both sides.

## Usage

    fishhawk-verify --export <path>

Exits:

| Code | Meaning |
|---|---|
| 0 | Every chain in the export verified |
| 1 | One or more integrity issues found (chain breaks, hash mismatches, etc.) |
| 2 | Usage error (missing flag, bad file, malformed JSON) |

Output is human-readable on `stdout`; one issue per line, tab-separated fields suitable for piping to `awk`.

## Export format (v1)

The verifier consumes JSON of the shape:

```jsonc
{
  "schema": "v1",
  "exported_at": "2026-05-02T10:00:00Z",
  "runs": {
    "<run-uuid>": {
      "signing_key": {
        "public_key": "<base64>",
        "issued_at":  "...",
        "expires_at": "..."
      },
      "audit_entries": [
        {
          "id": "<uuid>", "sequence": 1, "run_id": "<uuid>", "stage_id": null,
          "ts": "...", "category": "...", "actor_kind": null, "actor_subject": null,
          "payload": { /* event-specific */ },
          "prev_hash": null, "entry_hash": "<hex>"
        }
      ]
    }
  }
}
```

The compliance-export path on the backend (E9, not yet built) will produce this shape directly. For now operators can hand-craft the file from a database dump.

## What the verifier checks

For each run:

- **Hash match** — recomputed `entry_hash` for each entry must equal the stored value.
- **Chain link** — `entries[i].prev_hash` must equal `entries[i-1].entry_hash` (and `nil` for the first entry).
- **Sequence monotonicity** — `entries[i].sequence > entries[i-1].sequence`.
- **First-entry shape** — first entry's `prev_hash` must be `nil`.

Trace bundle signatures are checkable via the `audit.VerifyBundleSignature` function but the CLI doesn't yet wire them up. That lands when the bundle-signature persistence story (currently scoped to E5.6 and E2 trace-bundle metadata) is finalized.

## Build and test

From the repo root:

    go build ./verifier/...
    go test -race ./verifier/...

Or from this directory directly:

    go build ./...
    go test ./...

Verifier tests are pure unit tests — no Docker, no Postgres. The library is small enough to verify thoroughly without container scaffolding.

## Local invocation

    go run ./cmd/fishhawk-verify --export path/to/export.json

## See also

- `docs/MVP_SPEC.md` §4.4 — audit log integrity properties.
- `docs/MVP_SPEC.md` §13 — done criterion: "any external party can take an exported log + signing key chain and verify entries."
- `docs/ARCHITECTURE.md` §6 — the load-bearing invariants this tool enforces.
- `backend/internal/audit/chain.go` — the canonical implementation; this package's `chain.go` is a paired re-implementation.
