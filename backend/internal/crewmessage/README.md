# `backend/internal/crewmessage`

The Go half of the crew-message contract — ADR-081 ([#3727](https://github.com/kuhlman-labs/fishhawk/issues/3727)) rules 1–3, filed as E77.1 ([#3735](https://github.com/kuhlman-labs/fishhawk/issues/3735)).

Canonical schema and field reference: [`docs/spec/crew-message-v1.md`](../../../docs/spec/crew-message-v1.md). This file is the behavioural contract of the package.

## Boundary

Not a plan `ArtifactKind`. `backend/internal/plan` is untouched: `plan-standard-v1` stays frozen, and a crew message never routes through the plan validator. The package embeds its own mirror of the canonical schema (`schemas/crew-message-v1.schema.json`, written by `scripts/sync-schemas`' `crew-message-*` case arm — one mirror, backend only) and compiles it once at package init. A malformed embedded schema **panics at process start** rather than serving a wrong verdict on the first call.

## Two layers

The split is the load-bearing design decision, so read it before changing either side.

**The schema layer** carries what JSON Schema can express declaratively: the closed `crew-role` enum (rule 3), `additionalProperties: false` on every object-typed subschema plus the absence of the authority vocabulary (rule 2), exactly-one-anchor, the closed `type` enum, and the per-type payload arms.

**The Go layer** carries the two DIRECTIONAL rules JSON Schema cannot express, each with its own sentinel so a caller asserts error *identity* rather than substring-matching a message:

| Rule | Sentinel | Predicate |
|---|---|---|
| No message is addressed to a role an implement stage executes (`docs/ARCHITECTURE.md` §6 invariant #8) | `ErrRecipientNotAddressable` | `CanReceive` |
| `response_required` is legal only on `consult` and `escalation` (ADR-081 D1) | `ErrResponseNotAnswerable` | `answerableTypes` |
| No object repeats a member name, at any depth | `ErrDuplicateMember` | `rejectDuplicateMembers` |

Both directional rules read the value decoded inside `Parse`, never a second independent decode of the raw bytes, so the schema layer and the Go layer agree on one interpretation of the document.

**The duplicate-member rule is what makes that last sentence true.** `Parse` validates a generic decode and then decodes the original bytes into `Message`, and the two decoders disagree on exactly one construct: a repeated member name. `map[string]any` **replaces** the earlier value; a struct field that is itself a struct is **merged** into twice. So

```json
{"anchor":{"run_id":"…"},"anchor":{"issue_ref":"…"}}
```

presented ONE anchor to the schema layer — the `oneOf` passed — and TWO to the decoded `Message`, letting exactly-one-anchor escape into the typed value. Observed, not reasoned: before the guard, `Parse` on that document returned `Anchor{RunID:"…", IssueRef:"…"}` with a nil error. `rejectDuplicateMembers` runs ahead of *both* decodes, as a token walk, because every decode target this package has already collapses the duplicate before it can be seen.

`check-jsonschema` **accepts** duplicates (Python's `json` is last-wins), so this is a place the Go layer is deliberately the stricter of the two. The paired accept arm matters: the same member name in two *sibling* objects is legal and stays accepted — every `evidence` element carries `kind` and `ref`.

### Format assertion

The compiler is built with `Compiler.AssertFormat()`. Under Draft 2020-12 `format` is an **annotation** by default, so without it the Go layer accepted an `anchor.run_id` of `"not-a-uuid"` and a `deadline` of `"not-a-date"` that `check-jsonschema` — which asserts formats — rejects against the canonical copy. Both were observed. That divergence ran the wrong way: this package is documented as the *stricter* of the two layers, so it must not be the weaker one on the two format-bearing properties. No sibling validator in the repo asserts formats, so this is a deliberate departure from the local idiom rather than a copy of it.

`CanReceive` is the **single source of truth for addressability**. It is defined as the complement of `implementStageRoles`, kept as its own declaration so a future implement-executed role is added in exactly one place, and it is fail-closed for a role outside `AllRoles`.

### Schema-only validation does NOT enforce invariant #8

`AllRoles` — and the schema enum it mirrors — **includes `RoleImplementer`**. So the schema *alone* accepts `recipient_role: implementer`: a bare `check-jsonschema` run, or any non-Go consumer of the schema, does not enforce the no-implement-recipient rule. **Consumers must call `Validate` or `Parse`.**

Why the enum is wide on purpose:

- The implementer is a real crew role and a legal `sender_role`. Invariant #8 forbids *addressing* one — a directional rule about the recipient, not a fact about the vocabulary.
- Narrowing the enum would enforce the rule twice and **mask** the Go rule: with the Go check deleted, the fixture would still be rejected by the enum, `TestValidate_RejectsImplementRecipient` would stay green, and the rule would be untested dead code. Keeping the enum wide leaves `CanReceive` the sole gate in that fixture's path, which is what makes the test a genuine counterfactual.
- The test asserts the schema **accepts** the document as an explicit precondition. If the enum is ever narrowed, the masking is reported by a failing precondition rather than silently tolerated.

A later E77 child that ships this schema to a **non-backend process** must re-evaluate the split: with no Go validator in that process, invariant #8 would go unenforced there, and the answer may then be a second, narrower recipient enum rather than this one.

## API

| Symbol | Contract |
|---|---|
| `Validate(data []byte) error` | schema-validate, then apply both semantic rules. Returns `*ParseError`, `*SchemaError`, `ErrRecipientNotAddressable` or `ErrResponseNotAnswerable` |
| `Parse(data []byte) (*Message, error)` | `Validate` plus a typed decode (`decodeStrict`) with `DisallowUnknownFields`, so the typed path can never silently accept a field the schema rejects |
| `CanReceive(Role) bool` | addressability; see above |
| `AllRoles` | the full vocabulary, in schema order |
| `EmbeddedSchema() []byte` | the shipped mirror's raw bytes, for the drift and shipped-document tests |
| `WithSenderRole(raw []byte, role Role) ([]byte, error)` | E77.3 / #3737 admission gate for the SERVER-DERIVED sender role (ADR-081 rule 3). Returns `ErrDuplicateMember`, `ErrUnknownRole` (the role is not in `AllRoles` — a caller bug), `*ParseError` (empty, malformed, or not a JSON object — a top-level `null` included) or `ErrSenderRoleNotSettable` (the document names a DIFFERENT `sender_role`; presence is decided on the raw member, so `null`, `""` and a non-string are present-and-different). A matching `sender_role` returns the caller's bytes UNCHANGED; only an ABSENT one is injected and re-marshalled. NOT a validator: `Mailbox.Send`'s `Parse` re-runs every rule over the returned bytes |

**`WithSenderRole` ordering is load-bearing.** The duplicate-member token walk runs over the CALLER'S RAW BYTES before anything else, because the injection path decodes the root into `map[string]json.RawMessage` and re-marshals it, and a map is last-wins: two root `anchor` members of different kinds would be COLLAPSED into one, and the re-marshalled document would reach `Parse` with the duplicate laundered away — exactly the escape `ErrDuplicateMember` refuses (see "Two layers"). Carrying the root's values as `json.RawMessage` means a duplicate NESTED below the root survives the re-marshal (only the root is collapsed), but the walk refuses it at admission anyway. The matching path returns the raw bytes rather than a re-marshal, so what `Parse` validates is what the caller sent. The caller's `sender_role` value is deliberately not echoed in the error: it is untrusted, unbounded input on its way to an HTTP response.

`*SchemaError` carries the failing instance location plus the full causal tree, so a test can assert a rejection happened for the *right* reason rather than accepting any non-nil error. The schema is registered with the compiler under its own absolute `$id`, not a bare filename — a relative resource name resolves against the process cwd and would leak an absolute host path into every error message.

`Payload` is a flat union of the five per-type shapes. The schema narrows `payload` to exactly one closed `$def` per type, so a decoded `Payload` carries only its own type's fields; the fixture tests assert field-by-field fidelity, which is what catches a wrong json tag.

## Controls and their counterfactual mechanisms

One behavioural test per control. For the Go controls, mutate the rule **body** (return `nil` early) — deleting the function only breaks compilation and proves nothing. For the schema controls, mutate **both** the canonical copy and the embedded mirror, or `TestEmbeddedSchemaMatchesCanonical` reddens first and masks the arm under test; restore and re-run `scripts/sync-schemas` afterwards.

| # | Control | Layer | Test | Mechanism |
|---|---|---|---|---|
| 1 | no implement recipient | Go, `ErrRecipientNotAddressable` | `TestValidate_RejectsImplementRecipient` | the fixture is a notice well-formed in every other field whose `recipient_role` is `implementer`; the enum admits it, so the Go rule is the only gate |
| 2 | addresses are roles | schema, `crew-role` enum | `TestValidate_SchemaLayerRejections/model_id_{recipient,sender}` | replace the enum with `type: string` and `"claude-opus-5"` validates |
| 3 | no authority field | schema, `additionalProperties: false` | `.../scope_authority_field`, `.../authority_field_inside_the_payload` | every other field is valid, so the unknown-property rejection is the sole failure cause. The payload case also pins that each `$def` closes itself rather than relying on the root |
| 4 | exactly one anchor | schema, `anchor` `oneOf` | `.../two_anchors`, `.../no_anchor` | each anchor value is individually valid, so the `oneOf` is the only thing failing |
| 5 | escalation calibration | schema, the escalation `if`/`then` arm | `.../escalation_missing_recommended_default`, `.../escalation_missing_tradeoffs` | each case leaves the OTHER field present, proving the payload is otherwise well-formed, so only the `then`-clause `required` entry rejects it. Making either field optional turns one case red |
| 6 | closed type set | schema, `type` enum | `.../unknown_type` | with the enum deleted no `if`/`then` arm matches, payload goes unconstrained, and the document validates |
| 7 | `response_required` answerability | Go, `ErrResponseNotAnswerable` | `TestValidate_RejectsResponseRequiredOnNotice` | the schema declares `response_required` as a plain boolean on every type, so the document is schema-valid and the Go rule is the only gate. `TestValidate_ResponseRequiredLegalOnAnswerableTypes` is the paired accept arm, so a rule that rejected it on EVERY type would not pass |
| 8 | format assertion | schema, `Compiler.AssertFormat` | `.../malformed_run_id_uuid`, `.../malformed_deadline_date-time` | every other field is valid, so the `format` keyword is the sole failure cause; drop `AssertFormat` and both cases redden (observed) |
| 9 | no duplicate members | Go, `ErrDuplicateMember` | `TestParse_RejectsDuplicateMembers` | the fixtures are RAW BYTES — a `map[string]any` cannot hold a duplicate member, so `mutateFixture` structurally cannot reach this boundary. Each case asserts as a precondition that the schema ACCEPTS the collapsed document, so the Go rule is the sole gate. `TestParse_AcceptsRepeatedNameInSiblingObjects` is the paired accept arm |
| 10 | unknown-field guard | Go, `DisallowUnknownFields` | `TestDecodeStrict_RejectsUnknownField` | the guard lives in the extracted `decodeStrict`, which is the only site of the call. It is tested *there* because no document can both pass the schema (`additionalProperties: false` everywhere) and carry a field `Message` does not know — a test driving `Parse` can never reach the branch, which is what made the earlier version of this test vacuous |

| 11 | sender role not settable | Go, `ErrSenderRoleNotSettable` | `TestWithSenderRole_RefusesMismatchedSenderRole` | the `reviewer` case is asserted `Parse`-valid as a precondition, so the comparison is the only gate; making it permissive returns the caller's role for every case (observed) |
| 12 | duplicate walk BEFORE injection | Go, `rejectDuplicateMembers` in `WithSenderRole` | `TestWithSenderRole_RejectsDuplicateMembersBeforeInjection` | the fixtures omit `sender_role` so the injection path is taken, and each asserts as a precondition that the COLLAPSED document `Parse`s cleanly; with the walk removed the helper returns the collapsed bytes with a nil error (observed) |
| 13 | matching role is not rewritten | Go, the raw-return arm | `TestWithSenderRole_ReturnsRawBytesUnchangedWhenMatching` | the fixture is indented, so a re-marshal is a byte difference (observed) |
| 14 | root must be an object | Go, the nil-map check | `TestWithSenderRole_RejectsNonObjectDocuments` | a top-level `null` decodes into a NIL map without error; without the check the injection panics (observed) |
| 15 | role vocabulary | Go, `ErrUnknownRole` | `TestWithSenderRole_RefusesUnknownRole` | `engineer` is not a crew role; permissive, it is injected (observed) |

Controls 1, 7 and 9 assert the schema **accepts** the mutated document as an explicit precondition, so a future schema tightening that masks the Go rule fails loudly instead of leaving the test quietly non-counterfactual.

Beyond the table: `TestSchema_ClosedAtEveryLevel` and `TestSchema_DeclaresNoAuthorityVocabulary` walk the **shipped** document, so a comment-only or no-op touch of the schema cannot satisfy them; `TestEmbeddedSchemaMatchesCanonical` fails mirror drift in-loop, ahead of CI's schema-sync gate; and `TestSchemaRoleEnumMatchesAllRoles` pins the Go vocabulary against the schema enum in both directions.

`TestSchema_DeclaresNoAuthorityVocabulary` has **no exemption list**, deliberately. A legitimate need for one of those names is a decision to take explicitly, not a carve-out that quietly weakens the test pinning rule 2.

## Verification

Per-package during iteration, one full gate at the end:

```sh
scripts/test single ./backend/internal/crewmessage/   # the WHOLE package
(cd backend && go test -race ./internal/crewmessage/ ./internal/server/)
scripts/test verify        # once, on the committed tree
```

## Store: chain-authoritative mailbox (E77.2 / #3736)

ADR-081 D2/D3 and rule 7. Files: `state.go` (the pure state machine), `store.go` (the derived table), `mailbox.go` (the chain-writing domain layer), `rebuild.go` (replay).

**Chain first.** Every send and every disposition is an audit entry FIRST — `AppendChainedTx` for a `run_id` anchor, `AppendGlobalChainedTx` (the account partition, `SendParams.AccountID`) for an `issue_ref` / `decision_record_id` anchor — and the `crew_messages` row (migration 0090) is projected SECOND. The table is a derived INDEX: no append-only trigger, truncatable, rebuildable, and never evidence. Its primary key is the `crew_message_sent` entry's chain sequence, so "one row per message" is structural. A projection that fails after the entry committed returns `*ProjectionError` carrying the built row: the operation HAPPENED — do not retry it (that records it twice); `Rebuild` closes the index gap.

| Category | Written by | Payload |
|---|---|---|
| `crew_message_sent` | `Send` | `message` (the raw crew-message-v1 document), `thread_root_sequence` (absent on a thread root); the entry's `stage_id` column carries `SendParams.StageID` when set (run anchor only) |
| `crew_message_disposed` | `Dispose` | `sent_sequence`, `thread_root_sequence`, `disposition`, `round`, `reason` (the prose lives ONLY here; the row's `reason_sequence` points at this entry) |
| `crew_message_escalated` | `Dispose`, on the refused (bound+1)th rejection | `sent_sequence`, `thread_root_sequence`, `round_bound`, `rejections` — once per thread |

All three are INTERNAL, not `issuecomment` activity categories (`docs/issue-comment-surfaces.md`).

**State machine.** `open -> accepted | rejected | expired`; `open` is the only non-terminal state and every transition is one-way. An invalid transition is refused, never coerced.

**Send** validates through `Parse` — so invariant #8, the `response_required` rule and the duplicate-member refusal gate every stored message; there is no second, weaker path — and, for a reply, validates thread membership BEFORE appending: the named `ThreadRootSequence` must be a `crew_message_sent` entry (`ErrThreadRootNotFound`), in the reply's account (`ErrThreadCrossAccount`; for a run anchor the account follows from the anchor check, since one run has one account), itself a root (`ErrThreadRootNotRoot` — a reply names its thread's ROOT), on the same anchor (`ErrThreadAnchorMismatch`).

**Dispose** order is load-bearing, and every refusal appends NOTHING: `ValidDisposition` (`ErrInvalidDisposition`) → resolve the sent entry from the CHAIN (`ErrMessageNotFound`; an entry of another category at that sequence is not found) → one READ COMMITTED transaction taking `pg_advisory_xact_lock(thread_root_sequence)` FIRST, then: the chain already records a disposition for the message → `ErrAlreadyDisposed` (refuse-before-append; this, not the table's pending-only UPDATE, is what makes a concurrent disposition exactly-one on the chain) → the round bound → append. The thread lock is always taken before the chain helper's own run-row / partition lock, so they cannot invert.

**Round bound, per THREAD.** A rejection when the thread already holds `roundBound` rejections (`DefaultRoundBound` = 3, `NewMailbox`'s argument) is refused with `ErrRoundBoundExhausted` and one `crew_message_escalated` entry is recorded instead (only the thread's first; a later refused rejection does not re-escalate). Acceptances and expiries are not bounded. The count is read from the CHAIN, never the derived table, so a truncate cannot reset it. **Deliberate deviation from the issue's rule-4 wording:** `audit.AppendChainedUnderBudget` counts a RUN's entries of one category and takes a non-pointer `RunID`, so it would share one budget across every thread on a run and cannot serve the run-less anchors. The bound mirrors its ordering discipline (lock, then count — READ COMMITTED's per-statement snapshot sees a racing committed append) rather than its scope.

**The `round` column** is the 1-based rejection round a `rejected` disposition closed: the thread's Nth recorded rejection carries `round = N` (so the refused (bound+1)th never gets one). It is `0` while `open` and for an `accepted` or `expired` disposition. It is carried on the disposition entry's payload, so a rebuild reproduces it exactly.

**Monotonic projection.** Every projection is the guarded upsert keyed on `last_applied_sequence` (`ON CONFLICT … WHERE crew_messages.last_applied_sequence < EXCLUDED.last_applied_sequence`): a send projection delayed past a disposition's is a no-op, never a regression to `open`. A disposition projects through `projectDisposition`: the pending-only `Transition`; the whole terminal row via the guarded upsert when the send projection has not landed; and nothing when the row is already terminal — the FIRST disposition in chain order wins.

**Rebuild** replays the chain in ascending sequence order through the SAME builders (`projectSentRow`, `applyDisposed`) and the SAME projections (`Store.Upsert`, `projectDisposition`) the live path uses, so rebuild identity is structural. `RebuildOptions.Truncate` truncates inside the replay's transaction, so a failed rebuild rolls back instead of leaving the index half-empty. Undecodable entries are counted (`RowsSkippedUndecodable`) and skipped with a WARN, never guessed at; a disposition with no replayed send is `DispositionsSkippedOrphan`; a later disposition of an already-terminal message is `DispositionsSkippedSuperseded` (also counted when a no-truncate replay meets a row it already applied).

### Store controls and their counterfactual mechanisms

Each was run under an actual mutation of the control's BODY or SQL, observed RED, and restored.

| Control | Test | Mechanism |
|---|---|---|
| refuse-before-append | `TestMailbox_Dispose_ConcurrentExactlyOneWins`, `…_SecondDispositionRefused` | the message is already disposed, so the chain check is the only gate; deleted, a second disposition is appended (2 entries); moved after the append it sees its own entry and refuses all 8 |
| first disposition wins | `TestRebuild_FirstDispositionWins` | a hand-seeded chain carries two dispositions; applying the second over a terminal row makes the row `rejected` |
| thread membership (×4) | `TestMailbox_Send_ThreadMembership` | the other-category root carries a VALID sent payload, so only the category check rejects it; each named check deleted appends a reply |
| disposition value | `TestMailbox_Dispose_UnknownDispositionRefusedBeforeAppend` | `"maybe"` is the only defect; the chain entry count is read back |
| sent-entry category | `TestMailbox_Dispose_UnknownSentSequenceRefusedBeforeAppend` | the sequence EXISTS with a valid sent payload under another category, so an existence check or a decode would pass it |
| round bound | `TestMailbox_Reject_RoundBoundRefusesAndEscalates`, `…_BoundIsPerThreadNotPerRun` | `roundBound` is a field (bound 2 / 1); the thread already holds bound rejections |
| lock then count | `TestMailbox_Reject_ConcurrentRoundsRespectBound` | bound+4 racing rejections on one thread, run and run-less anchors; without the lock 4 escalations land |
| count from the chain | `TestMailbox_Reject_BoundCountedFromChainNotTable` | truncate an exhausted thread's rows; a table count resets to 0 |
| escalate once | `TestMailbox_Reject_RoundBoundRefusesAndEscalates` | two refused attempts; deleted, two escalations |
| Send validates | `TestMailbox_Send_RejectsImplementRecipient` | an implementer recipient is schema-valid; a bare decode stores it |
| monotonic guard | `TestMailbox_Send_DelayedProjectionCannotRegressTerminalRow` | the unexported `afterChainAppend` seam (nil in production) runs a full `Dispose` between Send's append and its projection |
| truncate in the replay tx | `TestRebuild_FaultInjection` | a fault after the truncate must leave the prior index intact |

`TestMailbox_FaultInjection`, `TestMailbox_ProjectionAfterCommitFailure` and `TestRebuild_FaultInjection` reach every error return (`faultDB` for pool calls, `faultTx` for calls inside the mailbox's own transaction, injected chain writers for the append) and assert a pre-commit fault changes neither the chain nor the index.

## Delivery surface (E77.3 / #3737)

The agent-facing REST surface, the `write:messages` run-token scope and the operator MCP tools HAVE landed, OUTSIDE this package: the five `/v0/crew-messages` handlers live in `backend/internal/server/crewmessage.go` (contract: `backend/internal/server/README.md`), the stage-typed scope is minted in `backend/internal/server/mcptoken.go` for plan and review stages ONLY, and the operator wrappers live in `backend/internal/mcpserver/crew_message.go`. This package contributes `WithSenderRole` (above) — the server derives the sender role from the caller's identity and never trusts the body's — and `Mailbox`, which the server wires from `backend/cmd/fishhawkd/serve.go`.

**Two of the three audit categories stay INTERNAL; `crew_message_escalated` acquired an issue-comment surface in E77.6 (#3740).** The E77.2 note that a server writer must revisit their surface decision is discharged here, and E77.6 revisited it. `crew_message_sent` and `crew_message_disposed` remain INTERNAL and are NOT `issuecomment` `activityCategories`, because a crew message is advice between crew roles, not run activity, and its TEXT must reach readers only through `prompt.RenderCrewMessages`' envelope — an issue-thread activity line would be a second, un-enveloped render.

That argument is about the TEXT, and it is why the E77.6 surfaces carry none. `crew_message_escalated` IS now an `activityCategories` member and renders the fixed line "Crew disagreement escalated to the captain" — no payload prose, no message text, just the governance fact that a disagreement needs the captain's ruling, which is run activity in exactly the sense a scope-amendment decision is. `crew_message_sent` additionally yields a page-class PING (never a timeline line) for a root `escalation` only, so the captain learns a disagreement was raised without every consult, finding and notice posting a comment. `docs/issue-comment-surfaces.md` records both, including the asymmetry and the run-less-anchor residual.

**The escalation page + the binding-only-once-answered contract (E77.6 / #3740).** A crew disagreement pages the captain on ANY run at ANY autonomy tier, `low` included: `server.activePageEvent` refuses the gate under the server-package `PageEventCrewEscalation` token, deliberately NOT a `spec.PageEvent*` constant, so no `page_human_on` declaration can carry it and `expandTier(TierLow)`'s empty page list cannot drop it. The page fires while the thread's ROOT holds no terminal disposition — the rejections that EXHAUSTED the bound are rounds of the disagreement, not rulings on it, so they never mark it settled. And the captain's ANSWER is the only thing an escalation ever turns into binding prompt text: `server.resolveDecidedCrewEscalations` folds a root escalation into `prompt.Trigger.CrewEscalationRulings` only once its row is `accepted` or `rejected`, carrying the contract-closed disposition and the operator-authored reason from the `crew_message_disposed` entry and NEVER the agent-authored `summary` / `recommended_default` / `tradeoffs`. An open escalation binds nothing. Contracts: `backend/internal/server/README.md`, `backend/internal/prompt/README.md`, `docs/METHODOLOGY.md`.

**`SendParams.StageID` (E77.5 / #3739).** An optional stage id stamped onto the `crew_message_sent` chain entry's nullable `stage_id` column for a RUN anchor (a run-less anchor's global partition has no stage column, so it is ignored there). It is what makes the server's per-stage consult budget countable FROM THE CHAIN — the same count-from-the-chain discipline the round bound uses. The derived row gains no column, so there is no migration and `Rebuild` is unaffected; `stage_id` is already a chain hash input, so a stamped entry re-verifies (`TestMailbox_Send_StampsStageID`, counterfactual: dropping the threading in `appendEntry` leaves the entry's `stage_id` NULL).

**The fishhawkd-invoked responder HAS landed (E77.5 / #3739)**, outside this package: `backend/internal/server/crew_consult.go` dispatches a run-bound `response_required` consult to a CLOSED role → responder registry, answers through `RespondToCrewMessage` or disposes it `expired` at its deadline, and bounds it per stage and per consult (contract: `backend/internal/server/README.md`, "Synchronous consult"). The registry ships EMPTY — E77.8 registers the first responder. The consult channel is rendered in the PLAN prompt only: in-process review stages hold no run token and cannot reach the endpoint, so the review-stage half is deferred to a follow-up. **Still not shipped:** the E77.7 general delivery work under ADR-081 rules 4–8. The `issuecomment` surface is no longer missing — E77.6 (#3740) shipped the escalation half of it (above); the consult / finding / notice / work_request kinds still have none, deliberately.

**The `CREW MESSAGE` quarantine envelope and the `agenteval` crew-message channel HAVE since landed (E77.4 / #3738).** `prompt.writeUntrustedCrewMessages` is the ONLY form in which a message's `MessageText` may reach an agent — a column-0 `<<<BEGIN/END UNTRUSTED CREW MESSAGE>>>` envelope with an ignore-and-report framing paragraph, the text through `sanitizeUntrustedComment`, Fishhawk-rendered attribution outside the envelope, and per-message/block caps — rendered by the plan, plan-review and implement-review prompts ONLY, never the implement path. That is ADR-081 rule 5's obligation, which binds the containment work to the same change that ships a delivery surface: the render landed FIRST and deliberately, so the surface that E77.5 / E77.7 builds has a contained form to deliver INTO rather than needing to ship both at once. Two delivery paths now populate `prompt.Trigger.CrewMessages`: E77.5's `resolveAnsweredCrewConsults` (a plan stage's OWN answered consults, folded into a resumed or retried attempt) and nothing else — E77.7 owns general delivery — so beyond that fold the channel is proven structurally and exercised by the `agenteval` injection corpus. E77.6's captain's-ruling channel is deliberately NOT this one: `prompt.Trigger.CrewEscalationRulings` is TRUSTED operator-authored text outside every envelope, carrying only the contract-closed disposition and the captain's reason. Contract: `backend/internal/prompt/README.md`; evidence: `docs/compliance/prompt-injection-evidence.md`. The delivery path maps a `crewmessage.Message` onto the plain-data `prompt.CrewMessage` on the server side (mirroring `repodoc.ToPromptDocument`) — `prompt` deliberately does NOT import this package. Since E77.3 the server package is this package's production consumer (`Mailbox`, `Store` and `WithSenderRole`); the REST read path maps a stored message onto `prompt.CrewMessage` and returns ONLY `prompt.RenderCrewMessages`' output, never the raw text.
