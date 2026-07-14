# backend/internal/sla

Approval SLA timeout ticker: fails gated stages whose approval deadline elapsed.

## Parse and Ticker

`Parse` maps `<n>_<unit>` → `time.Duration`; `business_hours` is aliased to wall-clock hours in v0.

`Ticker` is a background goroutine that lists gated stages — `awaiting_approval` AND, since ADR-038 / #1390, the deploy pre-execution gate's `awaiting_deploy_approval` — with non-null `gate_sla`, fails-D + chains an `approval_sla_elapsed` audit entry once the deadline passes.

- The candidate query (`run.Repository.ListStagesAwaitingApproval`) is `state IN ('awaiting_approval','awaiting_deploy_approval') AND gate_sla IS NOT NULL`.
- Its OTHER consumer, the reaction poller, is unaffected by the deploy broadening because it skips any stage whose type ≠ `plan`.
- So a deploy whose operator never decides within `gate_sla` times out to failed-D exactly as a generic gate does (`awaiting_deploy_approval → failed` is an already-legal transition).

## Configuration

Off by default; enable with `--enable-sla-timer` (or `FISHHAWKD_ENABLE_SLA_TIMER=true`). Scan interval via `--sla-interval`, default 60s.

The dispatcher persists the gate's SLA string to `stages.gate_sla` at create time.
