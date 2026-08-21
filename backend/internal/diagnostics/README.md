# backend/internal/diagnostics

Product-facts diagnostic bundle backing `GET /v0/runs/{id}/diagnostics` (#1006).

## Collector

`bundle.go` — pure `CollectWithWedge(run, stages, auditEntries, versions, wedge) DiagnosticBundle`, no I/O, with `Collect(run, stages, auditEntries, versions)` as the no-wedge delegating wrapper (it passes `nil`).

Carries STRUCTURED product facts only: run id, ordered stage states, the failing stage's category + audit surface + failure detail class, audit sequence `[min,max]`, this binary's fishhawkd version + git SHA from `internal/version` + the required min runner version, workflow spec hash, runner kind.

By construction NO diffs, paths, prompts, free text, or audit payload bodies — the failing stage's free-text `FailureReason` is excluded (a `bundle_test.go` assertion).

## Wedge context (#1737)

`wedge_context` (`omitempty`) names WHY a stuck run is stuck, so an auto-drafted product report approaches a hand-authored one. Four structured fields: `blocking_checks` (red required-check CONTEXT NAMES), `campaign_item_state` + `blocked_dependents`, and `integrate_wave_error` (the fan-in marker).

**A nil `*WedgeFacts` suppresses the block entirely**, which is what keeps `Collect` byte-identical to its pre-#1737 self for every un-migrated caller — no new key appears, not even on a run whose audit chain carries a fan-in conflict (`TestCollect_NoWedgeArgument_BundleUnchanged`). A non-nil (even zero-valued) `WedgeFacts` opts in; the block is still OMITTED when the assembly finds nothing, so a healthy run stays silent.

The redaction contract holds by construction, on the same idiom as `FailureDetailClass`: `campaign_item_state` is normalized through the package-local closed `campaignItemStates` table, so an unrecognized (possibly free-text-bearing) value is DROPPED rather than echoed; `integrate_wave_error` is derived from the audit **category** set and returns the package's OWN literal, never an entry payload; `blocking_checks` are branch-protection configuration identifiers. The drive `Advance.Event` string and a stage's `FailureReason` are never read (`TestWedgeContext_NeverCarriesFreeText`).

`WedgeFacts` is CALLER-INJECTED — same idiom as `VersionFacts` — because the facts need repository reads this pure package will not do. `backend/internal/server/diagnostics.go::collectWedgeFacts` assembles them best-effort: an unwired `StageCheckRepo`/`CampaignRepo`, a nil `RequiredChecksSnapshot` (every local run — #2497), a run with no review stage, and either campaign read erroring all yield fewer facts and never a 500.

## Failure-detail classifier

`detailclass.go` — pure `ClassifyFailureDetail(reason) string` reduces the failing stage's free-text `FailureReason` (wrapped git stderr) to a CLOSED enum via an ordered marker table: `auth-401`, `bad-object-ref`, `target-unreachable`, or `""` (unclassified). It never returns any part of its input — only a table-owned enum literal — so `FailingStage.FailureDetailClass` is redaction-safe by construction. Ordering is load-bearing: git prefixes both auth and network failures with `fatal: unable to access '<url>':`, so `unable to access` is NOT a marker and `auth-401` is checked before `target-unreachable`.

The class is a fourth fingerprint component (`fingerprint.go`), included ONLY when non-empty, so it splits distinct root causes that share a failing surface (#1962) while keeping every unclassified failure's pre-change 3-component fingerprint.

## Read handler and consumers

Read handler: `backend/internal/server/diagnostics.go::handleGetRunDiagnostics` loads run + stages + audit (`GetRun` / `ListStagesForRun` / `ListForRun`) and returns the bundle; pure read, no egress.

Backs the `fishhawk diagnose <run-id>` CLI verb (`cli/cmd/fishhawk/diagnose.go`). Foundation of the product-feedback feature; the deduped egress path + operator surfaces ride on top in sibling slices.
