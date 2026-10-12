# personaprompt — the pure half of a reviewer persona's prompt and ingest controls

`backend/internal/personaprompt` renders ONE reviewer persona's own review
prompt and runs the persona-only concern-ingest controls (ADR-084 D1(b), D3,
D4(b); E55.8 #3753, E55.10 #3755, E78.5 #3756). It was extracted from
`backend/internal/server` (#4313) so the review loop and the agenteval persona
catch arm render and score through ONE builder: `agenteval` cannot import
`server` (server imports agenteval), so the shared code lives here.

The package is PURE: no forge read, no persistence, no logging. The caller
resolves the remit (`repodoc.Resolve`) and the decision record
(`decisionrecord.Assemble`), attributes the injection (`repodoc.AttributeSet`),
and logs the control results it gets back. In the review loop that caller is
`backend/internal/server/reviewer_persona.go` (`buildPersonaPrompt`) and
`persona_concern_ingest.go` (`applyPersonaConcernControls`); the long-form loop
contract (degrade details, accounting, authority, escalation gating) stays in
`backend/internal/server/README.md` § "Reviewer personas".

## Entry points

| Function | Does |
|---|---|
| `Framing(name)` | The remit block's `repodoc.Framing`: heading `Reviewer persona remit: <name>`, a preamble naming the persona, and the ADR-068 trust note (the remit ADDS a lens; the #3915 narrowed quote instruction). The caller passes the same value to `repodoc.Resolve`. |
| `Build(Input)` | Renders the persona prompt and returns `Result{Prompt, Trigger, TreeAccess, QuoteDocs, Attribution}` (below). |
| `QuoteDocuments(trig)` | Every document a prompt built from `trig` renders, in render order: `InjectedDocuments`, then each review convention's document. |
| `QuotedDocuments(docs)` | Maps injected documents to the `planreview.QuotedDocument` set quotes are verified against. `Text` is `repodoc.InjectedContent` — the delimiter-bracketed body the reviewer saw, never the framing; a withheld block contributes empty `Text` (`document_text_unavailable`). |
| `ChangePaths(plan, diff)` | The decision-record selection paths: `plan` UNION `diff`, de-duplicated, sorted; nil when both are empty. |
| `ApplyConcernControls(v, quoteDocs, severityCap)` | `planreview.VerifyQuotedPassages` against `QuotedDocuments(quoteDocs)`, THEN `planreview.ClampPersonaSeverities` against `severityCap` (`""` = uncapped), in place. Returns both results and the merged `VerdictClampedFrom` (quote first, then clamp). |

## `Build`

`Input{Kind, Trigger, StandardInjected, PersonaName, Remit, DecisionRecord, RoundGrounded, ReviewerCanGround}`:

1. **Value copy.** `Trigger` is copied; nothing Build writes reaches the
   caller's Trigger, so the standard prompt (built earlier from it) stays
   byte-identical.
2. **Grounding.** `TreeAccess = RoundGrounded && ReviewerCanGround`. Without
   it the tree fields (`ReviewTreeCommit`, the two skipped counts) are cleared
   so the diff-only clause renders; a GROUNDED round whose persona reviewer
   cannot ground is stamped `prompt.ReviewUngroundedReviewerCannotGround`
   (#4066), and an ungrounded round's reason carries over.
3. **Documents.** `InjectedDocuments = slices.Clone(StandardInjected)` + the
   remit (`repodoc.ToPromptDocument(Remit, Framing(PersonaName))`) + — for a
   decision_record persona — `DecisionRecord.PromptDocuments(TreeAccess)` (the
   index, then each selected record).
4. **Render.** `prompt.Build(Kind, trigger)`. An error returns a ZERO
   `Result`.

`Result.Attribution` is the ONE `repodoc.InjectionSet` to attribute: the remit,
then the selection's documents and its selection-level truncation (when a match
was dropped). Build attributes nothing — the caller attributes only after Build
succeeded (build before attribute), so no injection claim exists for a prompt
that never built.

## Invariants

- **The standard injected set is never aliased.** It is CLONED before the
  remit is appended, so a `StandardInjected` with spare capacity is never
  written and a second `Build` from the same slice cannot overwrite the first
  result's remit slot (`TestBuild_DoesNotAliasStandardInjected`).
- **The attribution set equals the injected set** past the standard documents:
  remit, index, records, in render order
  (`TestBuild_AppendsRemitThenRecordAfterStandard`, `TestBuild_AttributionSet`).
- **Quote verification runs before the cap**, so an unverified high under a
  medium cap is demoted to low once and the cap records no clamp
  (`TestApplyConcernControls_QuoteBeforeCap`).
- `ApplyConcernControls` neither scrubs reviewer-supplied ingest markers
  (`planreview.ClearPersonaIngestMarkers`) nor stamps `reviewer_role`: the
  caller does both, on every verdict, before and after it.

## Tests

`personaprompt_test.go` pins the contract directly. The UNEDITED server tests
`reviewer_persona_test.go`, `decision_record_persona_test.go`,
`persona_hardening_test.go` and `persona_concern_ingest_test.go` are the
cross-boundary byte-identity net (server → personaprompt →
repodoc/decisionrecord → prompt): the extraction kept the server wrappers
`personaRemitFraming`, `reviewChangePaths` and `personaPromptDocuments` with
their exact signatures so those tests compile and pass untouched.
