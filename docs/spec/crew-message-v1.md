# Crew message `crew-message-v1`

The typed, addressed message one crew role sends another. Implements [ADR-081](https://github.com/kuhlman-labs/fishhawk/issues/3727) rules 1–3; filed as [E77.1 / #3735](https://github.com/kuhlman-labs/fishhawk/issues/3735).

- Canonical schema: [`crew-message-v1.schema.json`](crew-message-v1.schema.json)
- Go validator: `backend/internal/crewmessage` (`Validate`, `Parse`) — long-form contract in [`backend/internal/crewmessage/README.md`](../../backend/internal/crewmessage/README.md)
- Valid fixtures, one per type: `backend/internal/crewmessage/testdata/valid/`

## Not a plan artifact

A crew message is **not** a plan `ArtifactKind`. `backend/internal/plan` is untouched and routes nothing here: `plan-standard-v1` stays frozen, and this contract carries its own schema, its own embedded mirror and its own validator so it can move on its own major. A message is not a concern either — a `finding` never enters the merge gate.

## Mirrors

`scripts/sync-schemas`' `crew-message-*.schema.json` case arm routes the canonical copy to **exactly one** mirror:

| Mirror | Why |
|---|---|
| `backend/internal/crewmessage/schemas/crew-message-v1.schema.json` | the embedded copy `Validate` compiles at package init |

There is deliberately **no runner and no CLI mirror**: the runner neither writes nor reads a crew message, and `fishhawk validate` has nothing to check one against, so a second mirror would be dead weight CI's schema-sync gate then has to police forever. `TestEmbeddedSchemaMatchesCanonical` byte-compares the mirror in-loop; the two `backend/internal/server` plan-gate sweeps (`surface_sweep.go`'s `crew-message schema requires every mirror`, `test_sweep.go`'s `generated_surface` row) flag a plan that scopes the canonical copy without the mirror. A later E77 child that ships the schema to a non-backend process adds its mirror to the case arm **and** to both sweep registries in the same change.

## Root fields

| Field | Type | Required | Notes |
|---|---|---|---|
| `schema_version` | string, const `crew-message-v1` | yes | contract discriminator; a future major is a new file, never a break in place |
| `type` | enum | yes | `consult`, `finding`, `work_request`, `notice`, `escalation` — selects the payload shape |
| `sender_role` | crew role | yes | on the wire the server **derives** this from the caller's identity (a run token's executing stage, or `captain` for an operator) and never takes it from a request body: a body may omit it or repeat the derived role, and any other value is refused `sender_role_not_settable` (rule 3) |
| `recipient_role` | crew role | yes | the addressee; see [Invariants](#invariants) — the schema alone does **not** enforce invariant #8 |
| `anchor` | object | yes | exactly one of `run_id` (uuid), `issue_ref`, `decision_record_id` |
| `payload` | object | yes | narrowed to one closed per-type shape by the `allOf` `if`/`then` arms |
| `evidence` | array of `{kind, ref}` | no | `kind` ∈ `audit_entry`, `run`, `stage`, `issue`, `decision_record`, `url` — **no** file/path member, by design |
| `response_required` | boolean (default `false`) | no | legal only on `consult` and `escalation`; enforced in Go, not in the schema |
| `deadline` | RFC 3339 date-time | no | after it, the sender stops waiting; an unanswered consult times out to "no answer" and never blocks the stage |

The crew-role enum is closed: `captain`, `planner`, `architect`, `historian`, `security`, `reviewer`, `implementer`. Adding an eighth role is an additive change within this major (AGENTS.md schema-change checklist step 1) and must be added to `crewmessage.AllRoles` in the same change — `TestSchemaRoleEnumMatchesAllRoles` pins the two sides against each other in both directions.

## Payloads

Every payload object closes with `additionalProperties: false` in its **own** schema object. Draft 2020-12 evaluates `additionalProperties` against the `properties` declared in the same object only, so a `$ref`'d subschema contributes nothing to its parent's allowed set — each payload must close itself, and each does.

| Type | Required | Optional | Semantics |
|---|---|---|---|
| `consult` | `question` | `what_i_can_infer`, `context` | a question answered synchronously in-process (ADR-081 D1 option 1); first use is planner → historian |
| `finding` | `summary` | `detail`, `severity` (`low`/`medium`/`high`) | an observation. **Not** a concern: it enters no gate. `severity` is advisory |
| `work_request` | `title`, `summary` | `rationale` | files an item through the existing work-item path, run-scoped; **never** dispatches a run |
| `notice` | `summary` | `detail` | information, delivered asynchronously, no answer expected |
| `escalation` | `summary`, `recommended_default`, `tradeoffs` | — | a disagreement surfaced to the captain; becomes binding only once the captain answers |

`recommended_default` and `tradeoffs` are declared as optional properties on the `escalation-payload` `$def` and made **required by the escalation `if`/`then` arm**. That mirrors the `clarification-request-v1` calibration rule: the captain must be able to accept the default at a glance. Both are pinned by their own rejection case (a payload missing `recommended_default` with `tradeoffs` present, and the mirror image), so making either optional turns a test red.

## Invariants

Two layers carry the invariants, and **which layer** matters.

| Rule | Layer | Mechanism |
|---|---|---|
| Rule 2 — a message cannot express scope paths, constraints, delegation or a gate outcome | **schema** | `additionalProperties: false` on every object-typed subschema, plus the deliberate absence of the authority vocabulary (`scope`, `files`, `constraints`, `forbidden_paths`, `max_files_changed`, `delegation`, `delegated`, `autonomy`, `approval`, `approved`, `gate`, `verdict`, `outcome`) from every property name. Pinned by `TestSchema_ClosedAtEveryLevel` and `TestSchema_DeclaresNoAuthorityVocabulary`, which walk the **shipped** document |
| Rule 3 — addresses are roles, never model instances | **schema** | the closed `crew-role` enum. `"claude-opus-5"` is not a role, so it is structurally unaddressable |
| Exactly one anchor | **schema** | the `anchor` `oneOf` over three single-member arms: zero anchors match no arm, two match two |
| ARCHITECTURE §6 invariant #8 — no message is delivered to an implement stage | **Go only** | `crewmessage.CanReceive`, returning `ErrRecipientNotAddressable` |
| ADR-081 D1 — `response_required` only on the synchronously-answered types | **Go only** | `ErrResponseNotAnswerable` |
| No object repeats a member name | **Go only** | `ErrDuplicateMember`. JSON permits a repeated member and every decoder accepts one, but they disagree on its meaning — a generic decode REPLACES, a typed decode into a struct field MERGES — so `{"anchor":{"run_id":…},"anchor":{"issue_ref":…}}` presents ONE anchor to the schema `oneOf` and TWO to the decoded `Message`. Refusing duplicates up front is what keeps exactly-one-anchor true of the typed value |
| `anchor.run_id` is a uuid, `deadline` an RFC 3339 date-time | **both** | `format` is an annotation by default under Draft 2020-12, so the Go compiler is built with `AssertFormat` — without it the Go layer would ACCEPT values `check-jsonschema` rejects |

### Schema-only validation is NOT sufficient

The `crew-role` enum **includes `implementer`**, so this schema *on its own* accepts `recipient_role: implementer`. A bare `check-jsonschema --schemafile` run — or any consumer that validates against the schema without calling Go — does **not** enforce the no-implement-recipient rule. Invariant #8 lives in `crewmessage.Validate` and only there. **Every consumer must call the Go `Validate` (or `Parse`, which is `Validate` plus a typed decode).**

Invariant #8 is not the only Go-only rule. The `response_required` answerability rule and the duplicate-member refusal are also enforced nowhere but in Go, and they cut the other way from a normal schema/validator split: a document `check-jsonschema` **accepts** can be refused by `Validate`. On duplicate members the Go layer is deliberately the stricter of the two — Python's `json` is last-wins, so `check-jsonschema` silently collapses a repeated member and never sees the divergence the Go decoders would. Formats are the one place the two were out of step in the *other* direction, which is why the compiler is built with `AssertFormat`: a garbage `run_id` or `deadline` must not pass the layer documented as the stricter one.

That split is deliberate, not an oversight:

- The implementer **is** a real crew role and a legal `sender_role`. What invariant #8 forbids is *addressing* one, which is a directional rule about the recipient, not a fact about the vocabulary.
- Excluding `implementer` from the enum would enforce the rule twice, but the schema layer would then **mask** the Go rule: deleting the Go check would leave the fixture still rejected by the enum, the test would stay green, and the rule would be untested dead code. Keeping the enum wide leaves `CanReceive` the sole gate in its fixture's path, so `TestValidate_RejectsImplementRecipient` is a genuine counterfactual — and that test asserts the schema accepts the document as an explicit precondition, so if the enum ever narrows, the masking is reported rather than silently tolerated.

A later E77 child that ships this schema to a **non-backend process** must re-evaluate this: without a Go validator in that process, invariant #8 would go unenforced there, and the right answer may then be a second, narrower recipient enum rather than this one.

## Example

A planner asking the historian whether a decision has been settled before — ADR-081 rule 8's first use, and the lowest-risk exercise of the channel (the historian's answer is a deterministic precedent query, so it needs no model call):

```json
{
  "schema_version": "crew-message-v1",
  "type": "consult",
  "sender_role": "planner",
  "recipient_role": "historian",
  "anchor": { "run_id": "6f1d2c3a-4b5e-4f70-8a91-2c3d4e5f6a7b" },
  "payload": {
    "question": "Has a closed type set for crew messages been settled before, and if so where?",
    "what_i_can_infer": "ADR-081 names five types; I cannot tell whether an earlier decision narrowed or widened that set.",
    "context": "Planning E77.1, the crew-message contract child."
  },
  "evidence": [
    { "kind": "decision_record", "ref": "ADR-081" },
    { "kind": "issue", "ref": "kuhlman-labs/fishhawk#3727" }
  ],
  "response_required": true,
  "deadline": "2026-09-29T17:30:00Z"
}
```

## Validating locally

```sh
check-jsonschema --check-metaschema docs/spec/crew-message-v1.schema.json

check-jsonschema --schemafile docs/spec/crew-message-v1.schema.json \
    backend/internal/crewmessage/testdata/valid/consult.json \
    backend/internal/crewmessage/testdata/valid/finding.json \
    backend/internal/crewmessage/testdata/valid/work_request.json \
    backend/internal/crewmessage/testdata/valid/notice.json \
    backend/internal/crewmessage/testdata/valid/escalation.json
```

Both commands check the **schema layer only**. Neither enforces invariant #8, the `response_required` rule or the duplicate-member refusal — for those, run the Go validator. Run the whole package, not a `-run TestValidate` subset: the duplicate-member and unknown-field controls are named `TestParse_*` and `TestDecodeStrict_*` and a `TestValidate` filter skips them.

```sh
scripts/test single ./backend/internal/crewmessage/   # the WHOLE package
```

## Deliberately not in this contract

E77.1 shipped the contract and nothing that carries it. Later E77 children have since added: E77.4 / #3738 the `prompt`-package `CREW MESSAGE` quarantine envelope and the `agenteval` crew-message channel; E77.2 / #3736 the chain-authoritative store — the `crew_messages` derived table (migration 0090, rebuildable from the chain) and the `crew_message_sent` / `crew_message_disposed` / `crew_message_escalated` audit categories (contract: `backend/internal/crewmessage/README.md`). E77.3 / #3737 added the REST surface — `POST/GET /v0/crew-messages`, `GET /v0/crew-messages/{sequence}` (returning ONLY the `prompt.RenderCrewMessages` envelope), a refusal-ladder `respond` and the captain's `escalation-decision` — plus the stage-typed `write:messages` run-token scope (plan and review stages only) and a SERVER-DERIVED `sender_role`: a document may omit it or name the caller's derived role, and any other value is refused `sender_role_not_settable` (contract: `docs/api/v0.md` "Crew messages", `backend/internal/server/README.md`). E77.5 / #3739 added the synchronous consult round trip — a run-bound `response_required` consult is bounded per stage and per consult and answered by an in-process responder from a closed registry that ships empty until E77.8, with the consult channel rendered in the plan prompt only (contract: `backend/internal/server/README.md` "Synchronous consult") — and the rest of the delivery work (E77.7) remains under ADR-081 rules 4–8, which rule 5 binds to the containment work already landed.
