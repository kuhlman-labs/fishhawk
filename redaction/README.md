# redaction

Shared secret-redaction module (#1106) — a single definition imported by BOTH the runner and the backend, replacing the former byte-identical `runner/internal/redaction` + `backend/internal/redaction` hand-copies that could silently drift.

## API

`RedactDefault(bytes)` applies the closed pattern set `DefaultPatterns` (GitHub classic and fine-grained PATs, App installation `ghs_`, OAuth `gho_`, user-to-server `ghu_` and refresh `ghr_` tokens; GitLab `glpat-` PATs; OpenAI / Anthropic / AWS / npm keys; Authorization Bearer headers; JSON `password`/`token`/`api_key` fields) to bytes and returns the redacted form + per-pattern hit counts.

The `gho_`/`ghu_`/`ghr_` patterns (E72.41 / #3793) use the alnum body with a `{36,}` floor, not ghp_'s exact `{36}`: GitHub asks callers to treat tokens as opaque and documents refresh tokens longer than 40 chars, so a fixed length would leave a tail. `glpat-[A-Za-z0-9_-]{20,}` stops at the `.` of a routable GitLab PAT's version+CRC suffix, so that short non-secret suffix survives.

## Known-value redaction (E72.41 / #3793)

Patterns redact by shape; `KnownValues` redacts by value, for credentials a caller bound into an agent (ADR-086 decision 2) whose shape no pattern knows.

- **API.** `NewKnownValues(bindings ...KnownValue) (*KnownValues, []string)` compiles `{Name, Value}` bindings and returns the NAMES (never values) of the ones it dropped. `(*KnownValues).Redact(bytes)` replaces known values only; `(*KnownValues).Len()` counts retained values; `RedactDefaultKnown(bytes, kv)` runs `kv.Redact` THEN `RedactDefault` and merges the hits. A nil `*KnownValues` and an empty set are no-ops (input returned unchanged, nil hits), so `RedactDefaultKnown(b, nil)` is exactly `RedactDefault(b)` and callers thread the set unconditionally.
- **Floor.** A value shorter than `MinKnownValueBytes` (8), or empty, is NOT redacted — replacing a short string everywhere would shred unrelated evidence — and its binding name is returned for the caller to log. A real credential that short leaks.
- **Needle forms** per value: raw; JSON-string-escaped with HTML escaping on and off; `url.QueryEscape`, `url.PathEscape` and the userinfo-password escaping; base64 of the value alone (Std and URL alphabets, padded and raw); and the base64 CORES at byte shifts 0/1/2 (encode `shift` zero bytes + value, drop the 0/2/3 leading chars that mix in prefix bits and, when the length is not a whole number of 3-byte groups, the final char), which match the value embedded anywhere in a longer base64 string — the technique of the GitHub Actions runner's secret masker. A few boundary characters of an embedded value keep partial bits of its first/last byte; that is not the value.
- **Basic pass.** Before the needles, every `Basic <base64>` credential (case-insensitive) is decoded (Std, URL, raw variants); when the decoded bytes contain a known value, the WHOLE token, username included, becomes that binding's marker (the longest contained value wins). The needle pass alone would leave username-derived and boundary characters.
- **Ordering.** All needles of all values are de-duplicated and replaced longest first, so a value that is a substring of another, or a short encoding of a longer value, cannot pre-empt it and leave a tail. `RedactDefaultKnown` runs known values before patterns so a value beginning with a pattern-shaped prefix is replaced whole.
- **Marker grammar.** `[REDACTED:credential:<NAME>]` when the binding name matches `^[A-Za-z_][A-Za-z0-9_]{0,63}$`, else the generic `[REDACTED:credential]` — a malformed name still redacts and can never inject `"`, `]` or a newline. `Hit.Pattern` is `credential:<NAME>` (or `credential`), never the value; no pattern `Name` contains `:`, so the two hit namespaces cannot collide.
- **Structure.** Replacing a value inside a JSON string leaves valid JSON. A value inside a grammar-constrained field (a URL's userinfo, a request path) becomes a `[`-bearing marker the field's grammar refuses — `TestKnownValues_TargetURLUserinfo` pins that the redacted `https://user:[REDACTED:credential:NAME]@host` no longer parses — so a consumer that re-validates after redaction must drop or refuse such a payload rather than ship it. A value occurring outside a JSON string can make the payload invalid; pathological for an 8-byte-plus credential.
- **Consumer.** The runner's acceptance stage (E72.41 / #3793) threads one set through every acceptance evidence surface — the shipped verdict, the shipped transcript, and both trace bundles; see `runner/README.md`. The values come from the credential-binding slice (#3795); until it lands the set is empty and every surface is byte-identical to `RedactDefault`.

## Consumers

Three `RedactDefault` consumers (the runner's acceptance stage additionally uses `RedactDefaultKnown`, above):

- **Runner — trace bytes.** `runner/cmd/fishhawk-runner/redact.go::redactEvents` walks `agent.Result.Events`, applies `RedactDefault` to each non-empty payload, and returns a fresh slice (it does **not** mutate the input — the raw bundle reads from the original events and must stay verbatim, with one exception: an acceptance stage scrubs the values bound into its agent from BOTH variants, raw included — E72.41 / #3793); `redactString` does the same for the manifest's `agent_failure_reason`.
- **Backend — operator free text at the product-report egress boundary.** `backend/internal/server/product_report.go` (#1006 slice 3) — `description` is scrubbed before crossing only when `include_free_text` consent is set.
- **Backend — the diff secrets check (E80.3 / #3760).** `backend/internal/diffsecrets` matches `DefaultPatterns` against the ADDED lines of every implement-review round's diff and reports only path, line and pattern `Name` (never the match), which `backend/internal/server/diff_secrets.go` raises as a human-only `server_check` concern; the same server file passes the review prompt's diff through `RedactDefault` (`redactReviewPatch`) so the matched value never reaches a model reviewer. A pattern `Name` is now ALSO part of that check's persisted de-duplication key (`diff_secrets|<name>|<path>`) and of the concern note, so renaming a pattern re-raises every concern a human already waived under the old name — one more reason the `Name` field is "stable across versions". A `Name` must never contain `|` (`diffsecrets.TestCheckKey_Injective` pins it).

## Two bundles per stage

The runner packs **two** bundles per stage from the (raw, redacted) event lists and ships both via `uploadTrace` — raw first so the audit row preferred by compliance writes earliest, redacted second so the SPA's transcript surface (#218) has something to read.

The runner emits a `trace_redacted` log line listing per-pattern hit counts (no secret bytes) so operators can see "the redactor caught N tokens this run".
