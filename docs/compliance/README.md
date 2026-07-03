# Fishhawk dogfood compliance export

This directory publishes the audit log of Fishhawk's own development as a
public artifact, satisfying the [`docs/MVP_SPEC.md`](../MVP_SPEC.md) §12 build
commitment ("The audit log of Fishhawk's own development is published as a
public artifact", line 475) and demonstrating the §13 done criterion "First
compliance report export works end-to-end".

You do not have to trust this repository, the backend that produced the
export, or these instructions. The point of the artifact is that you can
verify its integrity yourself, offline, with a standalone binary. The reader
verifies; we do not assert.

## What is here

| File | What it is |
|---|---|
| [`fishhawk-dev-audit-export.json`](fishhawk-dev-audit-export.json) | The machine-verifiable `Export v1` audit log: every run in the window, each carrying its run-scoped public signing key and full chained audit trail. This is the file `fishhawk-verify` consumes. |
| [`fishhawk-dev-agent-changes.md`](fishhawk-dev-agent-changes.md) | The human-readable agent-changes report generated from the same audit data: per change, who approved it, what reviewed it, and what validated it. |

Both files are **redacted-tier** evidence per ADR-054 (see the caveat below).

## Provenance

- **Source:** Fishhawk's own development audit log, served by the local
  `fishhawkd` control plane over the E31 acceptance-campaign window.
- **Window (fixed, closed):** `2026-06-25T00:00:00Z` .. `2026-07-03T00:00:00Z`.
  The upper bound is in the past, so the window's contents are frozen — the
  export excludes today's in-flight runs (including the run that produced this
  artifact).
- **Producing `fishhawkd` `git_sha`:** `52bac1e` (from `GET /healthz`).
- **Generated:** 2026-07-03 (export `exported_at` and report `Generated at`
  are generation-time stamps).
- **Totals:** 134 runs in range, 78 agent-authored changes, 0 human-led
  changes, 56 runs without a change.
- **Export `sha256`:**
  `fb7dce02920471c65163ab59fe21eb1ef10439a739d358dcda79dd8790949bc2`

Verify the export hash yourself:

```sh
shasum -a 256 fishhawk-dev-audit-export.json
# fb7dce02920471c65163ab59fe21eb1ef10439a739d358dcda79dd8790949bc2  fishhawk-dev-audit-export.json
```

The `exported_at` / `Generated at` stamps mean regeneration is **not**
byte-identical; this `sha256` plus the exact commands below are the provenance
record, not a byte-reproducibility claim. The entry content for the closed
window IS stable — the audit chain is append-only and the window is in the
past.

### Exact generation commands

The export (the CLI follows the `X-Fishhawk-Export-Complete` /
`X-Fishhawk-Export-Next-Cursor` continuation headers and assembles one
complete file — a single raw-endpoint `curl` would be silently partial):

```sh
cd cli && go run ./cmd/fishhawk export \
  --backend-url http://localhost:8080 \
  --from 2026-06-25T00:00:00Z --to 2026-07-03T00:00:00Z \
  --out ../docs/compliance/fishhawk-dev-audit-export.json
```

The human-readable report (`limit=200` is the server maximum; the window's
134 runs fit one page):

```sh
curl -fsS 'http://localhost:8080/v0/reports/agent-changes.md?from=2026-06-25T00:00:00Z&to=2026-07-03T00:00:00Z&limit=200' \
  -o docs/compliance/fishhawk-dev-agent-changes.md
```

## Verify the export

Obtain `fishhawk-verify` — either install it directly (needs the public
commit; see the note below), or build it from a checkout of the `verifier`
module:

```sh
# Option A: install (requires the merged commit to be publicly fetchable)
go install github.com/kuhlman-labs/fishhawk/verifier/cmd/fishhawk-verify@latest

# Option B: build from a repo checkout
cd verifier && go build -o /tmp/fishhawk-verify ./cmd/fishhawk-verify
```

Then run it against the export:

```sh
fishhawk-verify --export fishhawk-dev-audit-export.json
```

Expected output and exit codes:

```
PASS — verified N run(s), M audit entries; no issues found.
```

| Exit code | Meaning |
|---|---|
| `0` | Every chain in the export verified |
| `1` | One or more integrity issues found (chain break, hash mismatch, non-monotonic sequence) |
| `2` | Usage error (missing flag, unreadable file, malformed JSON) |

No backend access, no network, and no other files are needed — the binary
plus this one JSON file is the complete input. This artifact was verified in
exactly that shape (a fresh directory containing only the built binary and the
export) before publication.

## What verification proves — and what it does not

`fishhawk-verify` re-implements the canonical hash algorithm rather than
importing the backend's, so a tampered backend cannot ship a matching tampered
verifier. For every run it checks:

- **Hash match** — the recomputed `entry_hash` for each entry equals the stored
  value.
- **Chain link** — `entries[i].prev_hash` equals `entries[i-1].entry_hash`.
- **Sequence monotonicity** — `entries[i].sequence > entries[i-1].sequence`.
- **Genesis shape** — the first entry's `prev_hash` is `null`.

Each run's public signing key travels inside the export, so the verification
is self-contained. What it does **not** yet do: the CLI does not wire up
trace-bundle signature checking. That capability exists as a library
(`audit.VerifyBundleSignature`) but is not invoked by the command — see
[`../../verifier/README.md`](../../verifier/README.md) for the authoritative
checks list. Do not read a `PASS` as a bundle-signature assertion.

## Redaction caveat

This export is **redacted-tier** evidence per ADR-054. Producer-side
raw-pointer redaction is a deferred sibling change; the export therefore
carries full audit payloads. Before publication both committed artifacts were
scanned for credential/secret patterns
(`fhk_`/`fhm_`/`ghp_`/`gho_`/`github_pat_`/`AKIA…`/PEM key headers) and local
filesystem paths (`/Users/`) with zero hits required — post-hoc redaction is
not an option because mutating any entry would break the recomputed hash chain
that verification depends on.

## Regenerating over a different window

Change the `--from` / `--to` (and the report's `from` / `to`) bounds to any
closed RFC 3339 range and re-run both commands above. Record the new `sha256`
and window here as provenance; a moving `--to` (one not yet in the past) would
produce a non-stable artifact and is not recommended for a published export.

## See also

- [`docs/MVP_SPEC.md`](../MVP_SPEC.md) §12 (build commitment), §13 (done
  criteria), §4.4 (audit-log integrity properties).
- [`docs/api/v0.md`](../api/v0.md) — the export / report endpoints and the
  end-to-end external-verification procedure.
- [`verifier/README.md`](../../verifier/README.md) — the verifier's checks and
  the re-implementation rationale (ADR-008).
- [`cli/README.md`](../../cli/README.md) — the `fishhawk export` producer verb.
