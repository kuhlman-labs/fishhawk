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

`*SchemaError` carries the failing instance location plus the full causal tree, so a test can assert a rejection happened for the *right* reason rather than accepting any non-nil error. The schema is registered with the compiler under its own absolute `$id`, not a bare filename — a relative resource name resolves against the process cwd and would leak an absolute host path into every error message.

`Payload` is a flat union of the five per-type shapes. The schema narrows `payload` to exactly one closed `$def` per type, so a decoded `Payload` carries only its own type's fields; the fixture tests assert field-by-field fidelity, which is what catches a wrong json tag.

## Controls and their counterfactual mechanisms

Seven controls, one behavioural test each. For the two Go controls, mutate the rule **body** (return `nil` early) — deleting the function only breaks compilation and proves nothing. For the schema controls, mutate **both** the canonical copy and the embedded mirror, or `TestEmbeddedSchemaMatchesCanonical` reddens first and masks the arm under test; restore and re-run `scripts/sync-schemas` afterwards.

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

Controls 1, 7 and 9 assert the schema **accepts** the mutated document as an explicit precondition, so a future schema tightening that masks the Go rule fails loudly instead of leaving the test quietly non-counterfactual.

Beyond the seven: `TestSchema_ClosedAtEveryLevel` and `TestSchema_DeclaresNoAuthorityVocabulary` walk the **shipped** document, so a comment-only or no-op touch of the schema cannot satisfy them; `TestEmbeddedSchemaMatchesCanonical` fails mirror drift in-loop, ahead of CI's schema-sync gate; and `TestSchemaRoleEnumMatchesAllRoles` pins the Go vocabulary against the schema enum in both directions.

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
| `crew_message_sent` | `Send` | `message` (the raw crew-message-v1 document), `thread_root_sequence` (absent on a thread root) |
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

## Deliberately not shipped here

No REST endpoint, no `write:messages` token scope, no `issuecomment` surface, and no MCP tool — the delivery surface is E77.5 / E77.7 under ADR-081 rules 4–8. The `crew_messages` table (0090), the three audit categories and the chain-authoritative store above HAVE landed (E77.2 / #3736); when a server writer ships, it must revisit the categories' INTERNAL surface decision in the same change.

**The `CREW MESSAGE` quarantine envelope and the `agenteval` crew-message channel HAVE since landed (E77.4 / #3738).** `prompt.writeUntrustedCrewMessages` is the ONLY form in which a message's `MessageText` may reach an agent — a column-0 `<<<BEGIN/END UNTRUSTED CREW MESSAGE>>>` envelope with an ignore-and-report framing paragraph, the text through `sanitizeUntrustedComment`, Fishhawk-rendered attribution outside the envelope, and per-message/block caps — rendered by the plan, plan-review and implement-review prompts ONLY, never the implement path. That is ADR-081 rule 5's obligation, which binds the containment work to the same change that ships a delivery surface: the render landed FIRST and deliberately, so the surface that E77.5 / E77.7 builds has a contained form to deliver INTO rather than needing to ship both at once. Nothing populates `prompt.Trigger.CrewMessages` yet, so the channel is today proven structurally and exercised only by the `agenteval` injection corpus. Contract: `backend/internal/prompt/README.md`; evidence: `docs/compliance/prompt-injection-evidence.md`. The delivery path maps a `crewmessage.Message` onto the plain-data `prompt.CrewMessage` on the server side (mirroring `repodoc.ToPromptDocument`) — `prompt` deliberately does NOT import this package. The package's only consumer today is its own test suite (the store included — no server package calls `Mailbox` yet) — an exported-but-uncalled library package draws no `golangci-lint` finding, since `unused` reports unexported symbols only.
