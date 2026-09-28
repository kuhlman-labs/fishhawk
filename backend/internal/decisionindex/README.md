# decisionindex

`decision_index` (migration 0088) is a **derived, rebuildable projection** of the decision-bearing entries on the audit chain — E75.2 / #3730, implementing ADR-082 (#3728) decision (a)2 and binding rules 1, 3 and 4.

## Contract

- **The chain is the sole authority.** The index is never a source of truth. Each row cites its source entry by `source_sequence` (the primary key) and `source_entry_hash` (tamper detection). The recorded reason is a POINTER — `reason_sequence` + `reason_key` (the payload key holding it) — and the prose is never copied (ADR-082 rule 1).
- **Rebuildable, so no append-only trigger.** The table can be truncated and reconstructed from the chain at any time with `fishhawkd decision-index backfill --rebuild`.
- **Rebuild is byte-identical, structurally.** `audit_entries.sequence` is a table-wide BIGSERIAL, so it is a sound primary key: exactly one row per decision-bearing entry. Every column is a pure function of the entry plus its joined `RowContext`, and there is deliberately no `indexed_at` or other operational metadata the chain does not carry (#3730 approval condition 4). The backfill and the live writer share ONE `Extract` (pure, no DB, no clock) and ONE context query (`resolveContexts`), so they produce the same row for the same entry. Pinned by `TestBackfill_RebuildIsByteIdentical` (backfill vs backfill) and `TestIndexingRepository_IndexesLikeBackfill` (live writer vs backfill), both comparing complete rows.
- **Tenant isolation.** Forced RLS keyed on `account_id`, and the `decision_index_tenant_isolation` predicate is byte-identical to 0057's `audit_entries` policy (NULL-account rows stay visible — the #1829 window). `TestDecisionIndex_RLSIsolation` runs under a NOBYPASSRLS probe role, because the admin test role is a superuser and bypasses even FORCE.

## The nine decision classes

`decisionBearing` in `decisionindex.go` is the single source of truth, read by the extractor, the backfill, the gap check and the writer. `TestDecisionBearingCategories_AllRegistered` pins every key against `audit.KnownCategories`, so renaming a category fails a test instead of silently removing it from the index.

| Audit category | `decision_class` | Reaches the chain through |
|---|---|---|
| `approval_submitted` | `plan_approval` | `AppendChained` |
| `concern_waived` | `concern_waive` | `AppendChained` |
| `concern_deferred` | `concern_defer` | `AppendChained` |
| `concern_addressed_by_condition` | `concern_addressed_by_condition` | `AppendChained` |
| `scope_amendment_decided` | `scope_amendment` | `AppendChained` |
| `acceptance_triage_arbitrated` | `acceptance_arbitration` | **`audit.AnchoredChainAppender` only** |
| `merge_verdict_recorded` | `merge_verdict` | `AppendChained` / `audit.DedupedChainAppender` |
| `clarification_answered` | `clarification` | `AppendChained` |
| `grooming_disposition_recorded` | `grooming_disposition` | **`audit.GroomingWindowAppender` only** |

Every class is run-scoped. An entry with no run ID, or whose run row no longer exists, is never guessed at. `Extract` returns `ErrNoRun`, the resolver returns `ErrRunMissing`, the backfill counts the entry as `rows_skipped_no_run`, and the gap check reports it as orphaned.

## Columns worth knowing

- `outcome`: for concern_waive, concern_defer, concern_addressed_by_condition and clarification, the category itself implies the outcome. Every other class reads the payload ladder `decision` → `verdict` → `outcome`.
- `reject_class`: approval_submitted's E75.1 (#3729) `reject_class` (scope/approach/verification/other). It is empty for an approval, for a rejection recorded without a class, and for an entry that predates E75.1.
- `concern_category_raw` / `concern_category` / `concern_category_unmapped` / `severity`: taken from the payload's `category` / `severity` when those keys are present (E75.1 added them to concern_addressed_by_condition). Otherwise they are joined from the `review_concerns` row that the payload's `concern_id` names.
- `touched_paths`: the `scope.files[].path` values from the run's LATEST plan artifact, sorted and de-duplicated. The value is empty when the run has no plan, or when the plan predates the `scope.files` object shape.
- `escalation_keys`: the `fired_keys` of the LATEST `escalation_fired` entry with the SAME stage and a LOWER sequence than the decision, sorted and de-duplicated (#3730 approval condition 3). An escalation fired on a different stage never contributes. The value is empty when no such entry exists.
- `doctrine_version`: `runs.workflow_sha` today. ADR-082 rule 4's charter revision will replace the column's VALUE, not the column, once E71.2 (#3242) binds it at admission.

## Concern-category normalization

`NormalizeConcernCategory` trims the value, lowercases it, and collapses whitespace and underscores to hyphens. It then looks the result up in `concernCategoryAliases`. The correctness family is seeded verbatim from `server/defer_concern.go`'s `deferDefectCategories`. The other families (testing, performance, documentation, maintainability, scope) are the common reviewer synonyms.

**An unmapped value is indexed as ITSELF**, with `concern_category_unmapped = true`. It is never dropped and never coerced. The backfill reports the sorted, distinct set as `unmapped_categories`, which tells you which aliases to add next.

## The live writer (`writer.go`)

`NewIndexingRepository(inner, store, resolver, logger)` wraps the concrete audit repository at the SINGLE construction point, `newAuditRepository` in `backend/cmd/fishhawkd/serve.go`.

- **Best-effort, never failing a decision.** The decorator delegates every method. After a SUCCESSFUL append of a decision-bearing entry, it resolves the context, extracts the row, and upserts it. A failure at any of those three steps is logged at WARN (naming the sequence, category and failing step) and swallowed. The caller gets back exactly the entry and `nil` error the inner repository returned. An inner-repository error is returned unchanged, and nothing is indexed. Tests: `TestIndexingRepository_{IndexFailure,ResolveFailure,ExtractFailure}DoesNotFailAppend` (each reads the committed audit row back) and `TestIndexingRepository_InnerErrorPropagatesUnchanged`.
- **Detached from caller cancellation.** The index write runs on `context.WithoutCancel(ctx)`, bounded by `IndexTimeout` (5s). A client that disconnects after its decision committed therefore does not also lose the row. Test: `TestIndexingRepository_IndexSurvivesCallerCancellation`.
- **Capability-forwarding obligation.** `server/*.go` type-asserts four OPTIONAL capabilities off `cfg.AuditRepo`: `AnchoredChainAppender`, `DedupedChainAppender`, `GroomingWindowAppender` and `RetryBudgetAppender`. If a decorator carried only `audit.Repository`, each assertion would return `ok=false`. That would silently disable acceptance arbitration's atomic path and grooming capture, and it would drop two of the nine decision classes, which reach the chain only through those capabilities. The decorator therefore embeds `FullRepository` (Repository plus all four), overrides every append method so the result is indexed, and `NewIndexingRepository` **fails closed** (`ErrMissingCapability`) on an inner repository that lacks one. A decorator that claimed a capability the inner could not honour would defeat the server's non-atomic fallback. `AppendChainedGroomingWindowClose` indexes only the watermark, which is not decision-bearing. The consumed dispositions were indexed when they were appended.
- **Drift guard by source scan.** `TestIndexingRepository_ForwardsEveryOptionalCapability` parses `backend/internal/audit/*.go` (non-test files) with `go/parser` and collects every exported interface whose name ends in `Appender`. A name it does not recognise lands in the switch's default arm, and the test goes RED. **Adding a fifth `*Appender` to package audit therefore obliges this package:** add the interface to `FullRepository`, add an overriding method that calls `r.index`, and list the new interface in the test's switch. Reflection cannot enumerate a package's declared interfaces, which is why the guard scans source (#3730 approval condition 5). `serve_test.go` keeps the four explicit assertions against the wired value.
- **Coverage residual.** Only appends made through the wired repository are indexed. An append through an `audit.Repository` built anywhere else (tests, one-off commands) is not. That is acceptable because the chain is the authority: `check` names the shortfall and `backfill` closes it.

## Gaps and orphaned entries (`fishhawkd decision-index check`)

`Store.Gaps` finds every decision-bearing audit entry that has no index row and splits the results into two groups:

- **True gaps**: the run row still exists, so a backfill can close the gap. `check` exits 1 when any true gap exists, so a cron job can alert on it.
- **Orphaned entries**: the run row no longer exists, or the entry carries no run ID. These are reported as `orphaned_entries` (a count plus sequences) and **never fail `check`** (#3730 approval condition 6). `decision_index.run_id` references `runs`, so no backfill can ever index such an entry. Counting it as a gap would make `check` fail forever on a state the operator cannot repair.

## Operator commands

```sh
fishhawkd decision-index backfill [--db <url>] [--rebuild] [--dry-run] [--page-size N]
fishhawkd decision-index check    [--db <url>] [--limit N]
```

`--db` falls back to `FISHHAWKD_DATABASE_URL`. `backfill` without `--rebuild` is a converging top-up: the upsert is keyed on `source_sequence`, so re-running it converges instead of erroring. `--rebuild` truncates first. `--dry-run` extracts and counts but writes nothing. The backfill pages `audit_entries` by keyset with a bounded LIMIT and never uses `audit.Repository.ListAll`, which has no LIMIT. It resolves each page's context with one batched join. A decision-bearing entry whose payload is not a JSON object fails the run loudly and names its sequence.

## Rollback

The change is purely additive. Dropping the wrap in `newAuditRepository` disables live indexing and leaves backfill available. `fishhawkd migrate down` reverts 0088, pinned by `TestMigrateDown_DecisionIndexReversal`. Because the index holds no fact the chain lacks, dropping the table loses nothing.
