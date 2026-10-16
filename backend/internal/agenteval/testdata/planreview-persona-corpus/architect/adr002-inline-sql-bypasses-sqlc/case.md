# Case: adr002-inline-sql-bypasses-sqlc

**Provenance: SYNTHETIC.** This case is a hand-authored architect persona
case (E55.26 / #4453), NOT a captured production review. The issue, its
number and the plan are fabricated; nothing here is a real run. The governing
record IS real: the loader checks it against the real `docs/adr/index.json`
(status and `superseded_by`), so a premise that stops holding fails verify.

Persona: architect.

ADR sourcing: ADR-002 was chosen by reading its body, as #4453's plan step 8
requires, and the case anchors on a BODY-level rule. It was NOT substituted:
the approval's condition 2 tripwires
(`TestPersonaCatchCorpus_MatchedGoverningRecordBodiesReachThePersona` and
`TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes`) passed on
this case as drafted, before the next case was written. The plan's fallback
conflict (hand-editing the generated `db/*.go`) was not needed.

## What it represents

The issue asks for combinable status, repository and label filters on the runs
list. The plan's approach step 2 adds `ListRunsFiltered` to the run store,
which "appends a WHERE condition for each filter the caller set and runs the
assembled SQL string through the pgxpool directly, because a filter set that
varies per request does not fit a static generated query". The step binds
every value as a parameter, so it is not an injection defect. Step 1 adds an
ordinary sqlc query beside it, which is why `queries.sql` is in scope.

## The planted defect

- Kind: `accepted_conflict`, expect `catch`, anchored at approach step 2.
- The defect: a backend query that lives outside `queries.sql` and outside
  sqlc. ADR-002's Decision says "Queries live in
  `backend/internal/<feature>/queries.sql`. `sqlc generate` writes typed Go
  into `backend/internal/<feature>/db/`", and its Consequences say "Every
  database query is visible in `*.sql` files". sqlc expresses optional
  filters with nullable parameters (`sqlc.narg`), so the stated reason does
  not force the bypass.
- Why it is a BODY-level conflict: the index title is "Postgres access layer
  (sqlc + pgx vs. ORM)". A reviewer reading only the title sees a pgx call
  and no ORM, which reads as conforming. The rule that EVERY query lives in a
  `.sql` file is in the body.
- The case never names ADR-002 (asserted by
  `TestPersonaCatchCorpus_GoverningPremisesHold`), and no catch probe occurs
  in the issue or plan (loader mode m).

## Remit item

Item 1, conflict with an accepted decision.

## Governing records

- ADR-002 — accepted, not superseded,
  `applies_to: [backend/internal/*/queries.sql, backend/internal/*/db/**, backend/sqlc.yaml]`.

## What the persona is shown

The persona prompt is rendered through the production builder
(`personaprompt.Build`) over the real `docs/review/architect-remit.md` and the
ADR-084 D4(b) selection from the real `docs/adr/index.json`. Observed at
authoring (2026-10-15): the scope's `backend/internal/run/queries.sql` and
`backend/internal/run/db/queries.sql.go` match ADR-002 and nothing else;
ADR-002 is included in full; the index rendered 32,432 bytes, untruncated,
under its 32,768-byte cap. The persona can quote the Decision text.

The clean twin in spirit is `clean-sqlc-query-cli`, whose plan adds a sqlc
query the ordinary way under the same record.

## Scoring

Catch rule v5 under the architect's `decision_record` category predicate,
after the production persona-ingest controls. Every probe names this
defect: `adr-002`, `narg`, the `bypass(es|ing) sqlc` and `outside (of) sqlc`
spellings, `instead of sqlc`, `not a sqlc query`, `outside queries.sql`,
`not in queries.sql`, and the two ADR-002 body phrases `queries live in` and
`every database query is visible`. There is no `sql`, `inline` or `dynamic`
probe, because an injection note anchored at step 2 would carry those words;
the `security`-category non-catching example pins that such a note does not
score. Attribution by `step 2`, `ListRunsFiltered`, `pgxpool directly` or
`assembled SQL`. `persona_case.json` carries the hand-written examples the
loader classifies to prove the rule discriminates. Difficulty is not
decidable offline: it waits on the operator's Step 1d calibration
(`docs/compliance/planreview-catchrate-evidence.md`).

**Result (Step 1d, head `18a3ad31`, 30 trials).** Scored 30/30 on the
architect arm. Kept as a documented CEILING by Captain ruling 2026-10-16
(honest rate about 30/30); the combined batch re-recording rescores it. Step
1d "Results" in `docs/compliance/planreview-catchrate-evidence.md` is the
record.
