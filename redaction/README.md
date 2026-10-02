# redaction

Shared secret-redaction module (#1106) — a single definition imported by BOTH the runner and the backend, replacing the former byte-identical `runner/internal/redaction` + `backend/internal/redaction` hand-copies that could silently drift.

## API

`RedactDefault(bytes)` applies the closed pattern set (GitHub PAT, OpenAI / Anthropic / AWS keys, Authorization Bearer headers, JSON `password`/`token`/`api_key` fields) to bytes and returns the redacted form + per-pattern hit counts.

## Consumers

Three consumers:

- **Runner — trace bytes.** `runner/cmd/fishhawk-runner/redact.go::redactEvents` walks `agent.Result.Events`, applies `RedactDefault` to each non-empty payload, and returns a fresh slice (it does **not** mutate the input — the raw bundle reads from the original events and must stay verbatim); `redactString` does the same for the manifest's `agent_failure_reason`.
- **Backend — operator free text at the product-report egress boundary.** `backend/internal/server/product_report.go` (#1006 slice 3) — `description` is scrubbed before crossing only when `include_free_text` consent is set.
- **Backend — the diff secrets check (E80.3 / #3760).** `backend/internal/diffsecrets` matches `DefaultPatterns` against the ADDED lines of every implement-review round's diff and reports only path, line and pattern `Name` (never the match), which `backend/internal/server/diff_secrets.go` raises as a human-only `server_check` concern; the same server file passes the review prompt's diff through `RedactDefault` (`redactReviewPatch`) so the matched value never reaches a model reviewer. A pattern `Name` is now ALSO part of that check's persisted de-duplication key (`diff_secrets|<name>|<path>`) and of the concern note, so renaming a pattern re-raises every concern a human already waived under the old name — one more reason the `Name` field is "stable across versions". A `Name` must never contain `|` (`diffsecrets.TestCheckKey_Injective` pins it).

## Two bundles per stage

The runner packs **two** bundles per stage from the (raw, redacted) event lists and ships both via `uploadTrace` — raw first so the audit row preferred by compliance writes earliest, redacted second so the SPA's transcript surface (#218) has something to read.

The runner emits a `trace_redacted` log line listing per-pattern hit counts (no secret bytes) so operators can see "the redactor caught N tokens this run".
