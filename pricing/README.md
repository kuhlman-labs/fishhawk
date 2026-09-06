# pricing

Model-pricing tables and the community-dataset drift alarm.

## Long-context surcharge (documented, not implemented)

GPT-6 Astra bills any request whose input exceeds **272K tokens** at 2x
the input/cache rates and 1.5x the output rate, applied to the full
request. This registry prices **every** request at the base rate, so a
hypothetical over-threshold request would be under-priced by those
multipliers. Observed evidence (not a universal claim): the only Astra
call sites in the shipped workflow are the two reviewer stages; the
observed plan-review request on run 12aaac69 was 26,853 input tokens;
implement reviews carry a diff and are larger, but have not been
measured. Implementing the surcharge correctly needs a
per-provider-request usage split that the codex/claudecode/anthropic
adapters do not yet report — an invocation-wide aggregate spanning
several turns cannot decide the per-request threshold.

## Price drift-check (#1335, ADR-044 decision 1)

`pricing/drift.go` — `CheckDrift(datasetJSON, sourceSHA, generatedAtUTC) (DriftReport, error)`
compares the `familyRates` table against the community **LiteLLM**
`model_prices_and_context_window.json` dataset and reports per-field
drift (input / output / cache-read / cache-write).

Report semantics:

- **Severity-banded**: ignore <2%, warn >2%, **high >10%** — the high
  band is the daily job's open-an-issue trigger.
- **Directionality**: `ours_lower` = under-billing risk; `ours_higher` =
  over-reporting.
- **Provenance**: the pinned LiteLLM SHA + generation stamp travel in
  the report.

`CheckDrift` is pure and deterministic — it takes dataset bytes and
never reads the clock or the network.

### The family→reference map

`familyToLiteLLM` is the **operator-maintained** family → reference-id
map (e.g. `claude-opus` → `claude-opus-4-7`).
`TestDriftReferenceMapMatchesFamilies` pins it to `familyRates` so a
family add/rename can't silently drop coverage. A missing reference
reports `no_reference` — a provenance gap, not a false drift. The alarm
compares **base** per-token rates only — it has no notion of a
long-context surcharge tier, so a vendor changing only those multipliers
is invisible to it.

### Alarm, not authority

Per ADR-044 the LiteLLM dataset is an **alarm, not authority**: the
drift check **WARNS, never fails a normal build**. This is distinct from
the internal completeness invariant `TestCost_PricesLiveModelIDs`
(every live model id priced), which stays a hard CI FAIL.

### Network/clock shell

`pricing/cmd/price-drift` is the impure wrapper: it fetches the dataset
at the **pinned** `litellmPinnedSHA` (an immutable commit, per the
AGENTS.md pin-tools rule — bump deliberately), renders the report
markdown to stdout, and emits `high_severity` / `has_findings` to
`GITHUB_OUTPUT` for the daily scheduled job (the `.github/workflows`
cron is human-led).

The `/v1/models` availability-poll half of #1335 is the already-shipped
`modeloracle.Cached` (#1341) — see the model-id-validity entry in
`docs/ARCHITECTURE.md` §10.
