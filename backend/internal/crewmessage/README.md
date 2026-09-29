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

## Deliberately not shipped here

No `crew_messages` table or migration, no REST endpoint, no `write:messages` token scope, no `prompt`-package `CREW MESSAGE` envelope, no audit category, no `issuecomment` surface, no MCP tool, and no `agenteval` crew-message channel or seventh attack class. Those are later E77 children under ADR-081 rules 4–8; rule 5 binds the containment work to the same change that ships a delivery surface, so none of it can be skipped by shipping the transport first. The package's only consumer today is its own test suite — an exported-but-uncalled library package draws no `golangci-lint` finding, since `unused` reports unexported symbols only.
