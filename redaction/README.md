# redaction

Shared secret-redaction module (#1106) — a single definition imported by BOTH the runner and the backend, replacing the former byte-identical `runner/internal/redaction` + `backend/internal/redaction` hand-copies that could silently drift.

## API

`RedactDefault(bytes)` applies the closed pattern set (GitHub PAT, OpenAI / Anthropic / AWS keys, Authorization Bearer headers, JSON `password`/`token`/`api_key` fields) to bytes and returns the redacted form + per-pattern hit counts.

## Consumers

Two consumers:

- **Runner — trace bytes.** `runner/cmd/fishhawk-runner/redact.go::redactEvents` walks `agent.Result.Events`, applies `RedactDefault` to each non-empty payload, and returns a fresh slice (it does **not** mutate the input — the raw bundle reads from the original events and must stay verbatim); `redactString` does the same for the manifest's `agent_failure_reason`.
- **Backend — operator free text at the product-report egress boundary.** `backend/internal/server/product_report.go` (#1006 slice 3) — `description` is scrubbed before crossing only when `include_free_text` consent is set.

## Two bundles per stage

The runner packs **two** bundles per stage from the (raw, redacted) event lists and ships both via `uploadTrace` — raw first so the audit row preferred by compliance writes earliest, redacted second so the SPA's transcript surface (#218) has something to read.

The runner emits a `trace_redacted` log line listing per-pattern hit counts (no secret bytes) so operators can see "the redactor caught N tokens this run".
