# pricing

Model-pricing tables and the community-dataset drift alarm.

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
reports `no_reference` — a provenance gap, not a false drift.

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
