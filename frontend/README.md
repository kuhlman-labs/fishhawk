# Fishhawk Web UI

The browser surface for plan review, approval, audit log search, and
run visualization. Vite + React 19 + React Router 7 + TypeScript,
styled with Tailwind CSS v4 + shadcn/ui (per
[ADR-004](https://github.com/kuhlman-labs/fishhawk/issues/68)).

This directory is its own pnpm package, decoupled from the Go modules
above it. CI's TS lane is path-filtered to `frontend/**` so backend-only
changes don't pay the install/test cost.

## Layout

- `src/main.tsx` — entry; mounts `<App />` inside `<BrowserRouter>`.
- `src/App.tsx` — route table.
- `src/routes/` — one file per route (`root` is the app shell;
  `login` is rendered outside the shell; `attention` is the index
  route — the "Needs You" queue; `runs` lists workflow runs at `/runs`,
  `run-detail` drills into one, `stage-detail` renders the plan;
  `repo-dashboard` is the per-repo dashboard at `/repos/:owner/:name`;
  `audit` is still a stub; `not-found` catches the rest).
- `src/repo/` — the repo dashboard's Overview panels (in-flight,
  throughput, economics, health, posture) plus the Record tab
  (`record-tab.tsx`) and its pure export fold (`chain-verification.ts`).
- `src/auth/` — auth context, provider, `RequireAuth` gate, hook.
  The provider fetches `/v0/auth/me`; routes inside `<Root />` are
  gated behind it.
- `src/api/` — typed wrappers around the v0 REST surface
  (`client.ts`), TS mirrors of the OpenAPI schemas (`types.ts`,
  `plan.ts`), and a small `useAsync` hook for component-level
  data loading.
- `src/run-narrative/` — the run-detail evidence narrative's data layer
  (#1715): pure derivations (`narrative.ts`) and the composed loader
  (`use-run-narrative.ts`). See "Run-detail evidence narrative" below.
- `src/attention/` — the attention-queue item card
  (`attention-item.tsx`), one per-kind context renderer, plus the
  decision write surface (`decision-verbs.ts`, `decision-form.tsx`,
  `decision-panel.tsx`, `plan-gate-decision.tsx`) wired in E40.2.
- `src/plan/` — the plan-document renderer (`plan-document.tsx`)
  and its section primitives (`sections.tsx`). Each `standard_v1`
  field is its own section so the side nav anchors line up
  one-to-one.
- `src/components/ui/` — shadcn-copied primitives (currently just
  `Button`; add more on demand, never as a library dep).
- `src/lib/cn.ts` — `clsx + tailwind-merge` class helper.
- `src/index.css` — Tailwind v4 entry + `@theme` token overrides.

## Develop

Requires Node 22+ and pnpm 10+.

```sh
pnpm install
pnpm dev          # http://localhost:5173 → proxies /v0 to localhost:8080
pnpm typecheck
pnpm lint
pnpm test         # vitest, jsdom
pnpm build        # tsc -b + vite build → dist/
```

For backend talkback, run `fishhawkd serve` in another terminal; the
Vite dev server proxies `/v0/*` to `http://localhost:8080` so the
session cookie set by `/v0/auth/github/callback` is same-origin from
the browser's perspective. Override the proxy target by editing
`vite.config.ts` if the backend runs elsewhere.

### Running from a run worktree (#3030)

A Fishhawk run executes in a git worktree nested under `.git/` — e.g.
`<repo>/.git/fishhawk-worktrees/run-<id>/frontend`. Out of the box the
frontend toolchain (Vite/Vitest) is UNRUNNABLE there: Vite's
`server.fs.deny` default includes `**/.git/**`, and because the served
root (`server.fs.allow[0]`) is the frontend directory whose absolute path
contains a `.git` segment, that rule denies EVERY module under the root.
The refusal surfaces as a misleading message — in this toolchain
(vite 8.2.2 / vitest 4.1.11) the vitest `setupFiles` refusal renders as
`Error: Cannot find module '/src/test-setup.ts'` and component imports as
`Failed to load url … Does the file exist?`. The file exists and the
resolved config root is the FULL nested path (the `.git` segment is NOT
stripped), so the originating issue's proposed fix — making `setupFiles`
an absolute path — does not work and should not be re-tried; the path was
never the problem.

`vite-fs-deny.ts` drops exactly the `**/.git/**` rule, and ONLY when the
config directory has a `.git` path SEGMENT (`isNestedUnderGitDir`, a
segment match — `.github`, `my.git` and `.gitignore-dir` do not trigger
it). In a normal checkout `resolveFsDeny` returns `undefined`, so Vite
applies its stock defaults verbatim and the dev server's posture is
unchanged. The remaining rules are read from Vite's own resolved defaults
via `resolveConfig({ configFile: false }, 'serve')` — never hardcoded — so
a future Vite upgrade that widens the list is inherited automatically and a
rename of the git rule fails loudly rather than silently dropping
protection. Verified `pnpm test` green — 30 files / 288 tests — from both
a `.git`-nested worktree and a normal checkout.

SECURITY RESIDUAL (deliberate, narrow): this narrows a default-deny rule.
The reachable surface is dev-server file reads within `server.fs.allow`. A
git worktree carries a `.git` FILE not directory, so no worktree-local
repository metadata was being protected — but the rule ALSO denied any
DESCENDANT `.git` directory inside the served root (a nested repo, a
vendored checkout, or one cloned under the tree later), and within a
`.git`-nested checkout those are no longer denied by this rule. The
carve-out fires only when the checkout root itself sits under a `.git`
segment, and never in a normal checkout.

## What's stubbed

The plan-review vertical slice (E7.1 → E7.2 → E7.3) is in. Still
to come:

- **Audit search** under `/audit` is still a placeholder. The
  per-run audit list at `/runs/:id#audit` is wired (E7.4) but the
  global search across runs is later in E7.
- **Regenerate** in the plan-review header
  ([#146](https://github.com/kuhlman-labs/fishhawk/issues/146)) — renders disabled until E8.3 wires re-execution.

## Attention queue — the home page (E40.1 / #1713)

`/` renders `src/routes/attention.tsx` ("Needs You"), a ranked list from
`GET /v0/attention` (`api.listAttention`) of every decision parked on a
human across runs and campaigns. It was read-only in E40.1; E40.2 wired the
inline decision write surface (below), so cards are now actionable. The runs
list is unchanged at `/runs`; the shell's first nav entry points at `/`.

- **Six item kinds**, rendered by `src/attention/attention-item.tsx`:
  `plan_gate`, `scope_amendment`, `acceptance_disposition`,
  `split_verdict`, `paged_concern`, `attend_human_led_campaign`. The
  per-kind context renderer is an exhaustive `switch` over the
  `AttentionItemKind` union, so a new kind fails typecheck instead of
  rendering a blank card (an unknown kind arriving at runtime renders a
  labelled "Unknown item" fallback).
- **Links are server-emitted.** Each card links to the item's
  `detail_path` (`/runs/…` or `/campaigns/…`), never a client-derived
  path. The card also carries an inline decision affordance (see the E40.2
  section below); DISPATCH / re-execution affordances stay excluded, and
  the link target is still never re-derived here.
- **Completeness is never silent.** `truncated: true` or a non-empty
  `degraded[]` renders a "This list is incomplete." banner listing each
  reason, INCLUDING when `items` is empty — in that case the "Nothing
  needs you right now." all-clear is NOT shown.
- **One wire fixture.** `src/attention/attention-item.test.tsx` and
  `src/routes/attention.test.tsx` read the backend's end-to-end golden
  `testdata/wire/attention_list.json` (via `node:fs`, relative to the
  test file) and render it through `api.listAttention` and the page, so a
  field rename on either side of the wire fails a test. Do not
  hand-author a second copy of that payload.

## Decision write surface (E40.2 / #1717)

Each queue card with a decision endpoint gains a `Decide` disclosure
(`src/attention/attention-item.tsx`) that expands an inline per-kind panel
(`src/attention/decision-panel.tsx`). No new backend endpoint and no OpenAPI
change — every write rides an EXISTING route through the shared `api` client
(CSRF auto-attach covers all four POSTs):

| Kind                             | Verbs              | Endpoint                                                       |
| -------------------------------- | ------------------ | -------------------------------------------------------------- |
| `plan_gate`                      | Approve / Reject   | `POST /v0/stages/{id}/approvals` (reuses `ApprovalPanel`)      |
| `scope_amendment`                | Approve / Deny     | `POST /v0/runs/{run}/scope-amendments/{id}/decision`           |
| `split_verdict`, `paged_concern` | Waive / Defer      | `POST /v0/concerns/{id}/waive`, `POST /v0/concerns/{id}/defer` |
| `acceptance_disposition`         | Record arbitration | `POST /v0/runs/{run}/acceptance-arbitration`                   |
| `attend_human_led_campaign`      | —                  | no decision endpoint; no submit control                        |

- **`DECISION_VERBS` (`decision-verbs.ts`) is the single source** of which
  verbs each kind offers; the panels render their buttons from it, and
  `isDrivePlaneVerb` proves no verb — and no rendered button/link — is a
  re-execution ("drive-plane") affordance. Adding a drive-plane verb goes red.
- **Remove only on a confirmed write.** A 2xx removes the resolved item from
  the live list via route-local state (keyed on `item.id`) — no refetch, no
  reload. Any non-2xx leaves the item and renders the error envelope inline.
  For `plan_gate` the item is resolved from `ApprovalPanel`'s `onSubmitted`
  (fired only after the POST resolves), NOT its optimistic `onUpdate`.
- **Fail closed on a missing id.** `run_id` / `stage_id` / `concern_id` /
  `amendment_id` are optional on the wire, so a panel whose required ids are
  absent renders a named refusal and no submit button rather than building a
  `/runs/undefined/...` URL.
- **`ApprovalPanel` gained two optional props** — `onSubmitted` and
  `showRegenerate` (default `true`) — both preserving every existing call
  site. The queue passes `showRegenerate={false}` because Regenerate is a
  drive-plane verb.

## Plan review surface

`src/plan/plan-document.tsx` renders a `standard_v1` plan as a structured
document with side-nav anchors per Brand Foundations §6 ("plans are
documents, not chat"). Section primitives in `src/plan/sections.tsx` —
Ticket, Generated by, Summary, Scope, Approach, Verification, Risks
(optional).

Routes: `/runs` (`src/routes/runs.tsx`; the index `/` is the attention
queue) → `/runs/:id`
(`src/routes/run-detail.tsx`) → `/runs/:id/stages/:sid`
(`src/routes/stage-detail.tsx`). The last dispatches on `stage.type`:

- `plan` — hands the most-recent `kind=plan, schema_version=standard_v1`
  artifact's `content` to `<PlanDocument>`.
- `implement` — hands the most-recent `kind=pull_request` artifact to
  `<PullRequestDocument>` (#205).
- `review` — loads the _implement_ stage's `pull_request` artifact and
  hands it to `<ReviewDocument>` (#213).
- Other types still render a "lands later in E7" placeholder.

Older / unknown plan schema versions render a labelled warning rather
than guessing. The shared `<StageStateBadge>`
(`src/components/stage-state-badge.tsx`) is reused on both the
run-detail stage list and the stage-detail header so the visual language
for stage state stays consistent.

## Repo dashboard (E40.3 / #1714)

`/repos/:owner/:name` renders `src/routes/repo-dashboard.tsx`, reached from
the repo cell of the `/runs` list (the workflow cell links to the run). Two
tabs, selected by the `?tab=` search param: **Overview** (the default) and
**Record** (`?tab=record`, below). Overview renders five panels, in order,
each with its OWN fetch and its own loading/error surface — one failing read
renders one panel-scoped alert, never a blank page:

- **In flight** (`src/repo/in-flight-panel.tsx`) — composed from EXISTING
  surfaces, no backend of its own: `GET /v0/runs?repo=` (pending/running
  runs, current stage from `/v0/runs/{id}/stages`) and
  `GET /v0/campaigns?repo=` (active campaigns, wave progress derived from
  the items' `depends_on` DAG, the readiness rollup, and each blocked item
  with its unresolved blockers from `/v0/campaigns/{id}/status`). A failed
  per-run or per-campaign read degrades only that row; a list read with a
  further page renders a "Partial data" note.
- **Throughput / Economics / Health** (`throughput-panel.tsx`,
  `economics-panel.tsx`, `health-panel.tsx`) — `GET
/v0/repos/{owner}/{name}/{throughput,economics,health}`; `truncated: true`
  renders a "Partial data" note; the wait-on-human sub-panel renders only
  when the key is present.
- **Posture** (`posture-panel.tsx`) — `GET /v0/repos/{owner}/{name}/posture`:
  stages, gates + approvers, reviewers, autonomy and budgets from the
  newest run's cached spec. The drift warning fires on
  `schema_supported: false` or `spec_valid: false` (naming the declared
  version and `spec_error`) — NOT on a hash comparison with `/healthz`,
  which serves the same binary's hashes and so could never disagree.
- **One wire fixture per endpoint.** The panel and route tests read the
  backend's goldens `testdata/wire/repodash_*.json` via `node:fs` and serve
  them through the real client, so a field rename on either side fails a
  test. Do not hand-author a second copy.

### Record tab (E40.6 / #1718)

`?tab=record` selects it; anything else — including the absent param —
selects Overview, so every existing deep link is unchanged. Each branch
renders only its own panels, so the Record tab never fires the four
rollup fetches and Overview never fetches the export. Three sections:

- **Chain verification** — one page of `GET /v0/audit/export?repo=…&limit=25`
  folded through `src/repo/chain-verification.ts`, a PURE, fetch-free,
  React-free module. What it checks: per-run chain STRUCTURE (genesis,
  `prev_hash` linkage, strict sequence monotonicity) and SIGNING-KEY
  COVERAGE (a run with entries carries a key whose window covers
  `exported_at`; `issued_at <= exported_at <= expires_at`), mirroring the
  external verifier's `chain_broken` / `first_entry_has_prev_hash` /
  `sequence_not_monotonic` / `missing_signing_key` kinds plus its own
  `signing_key_expired` / `signing_key_not_yet_valid`.

  What it does NOT check, and why the badge says so in its own wording:
  it does **not** recompute entry hashes (the verifier's `hash_mismatch`)
  — that needs byte-exact reproduction of Go's `encoding/json` over the
  canonical `HashInputs`, and a third implementation with no shared
  `(input, expected-hash)` fixture would drift silently; it is also
  outside ADR-008's trust model, whose point is that verification does
  not run code the backend served. It does **not** verify Ed25519 bundle
  signatures — structurally impossible here, because the export is
  POINTER-ONLY (ADR-054: trace bundle bytes are never inlined), so there
  is nothing to verify a signature over. The badge therefore says
  "signing-key coverage", never "signatures verified", and points at the
  offline `fishhawk-verify` CLI for cryptographic proof.

  **Fail-closed shape guard.** A body that is not Export v1 — wrong
  `schema`, non-object `runs`, a run without an `audit_entries` array, an
  entry with a non-string `entry_hash` / non-numeric `sequence` /
  non-string-non-null `prev_hash`, an unparseable `exported_at`, or a
  present `signing_key` missing a string `public_key` or a parseable
  `issued_at`/`expires_at` — yields `unverifiable`, never `pass`. That
  guard is what stands in for a shared backend/SPA wire golden, which the
  export's live `exported_at` and random run UUIDs make awkward to pin.
  Partiality rides the `X-Fishhawk-Export-Complete` RESPONSE header (the
  verifier strict-decodes the three-field body, so no marker can live in
  it); the client treats only the exact string `true` as complete, so a
  missing header renders the "Partial data" note.

  No anchored-checkpoint field is rendered: ADR-056 / #1699 has not
  landed and nothing in the tree exposes one. An absence test on the
  badge's rendered text pins that, and must be updated deliberately when
  ADR-056 ships.

- **Export** — a one-click Export v1 download of the same endpoint via a
  Blob + object URL. A **403** gets a dedicated re-auth panel naming the
  required scope from `details.required_scope` (falling back to the
  literal `read:audit-export`) and telling the operator to re-authenticate
  with a token that carries it; every other status renders the generic
  inline error. A cookie-session operator normally never sees the 403 —
  `requireWriteScope` enforces the scope only for token identities — so
  the branch matters for bearer-token contexts.
- **Release evidence** — a visibly disabled placeholder, always rendered,
  wired when E33 (#1583) ships.

## Campaign detail + `operator_agent` override display (E25.12 / #1451; web UI #1467)

`src/routes/campaign-detail.tsx` renders the GET
`/v0/campaigns/{id}/status` rollup: header, pending-decision, rollup
buckets, dependency graph, per-issue run grid.

When the campaign carries a campaign-level `operator_agent` override it
surfaces a **Delegation override** block listing the set `may_*` knobs
plus `must_page_human`, captioned that it governs EVERY issue-run
wholesale — it replaces, never merges with, each run's per-workflow
contract. Absent the override the block is omitted (the
inherit-the-workflow-contract default).

The field is read off the status payload via a narrow local
`OperatorAgentOverride` shape + `operatorAgentOverrideOf`
(`src/api/client.ts` returns `res.json()` verbatim so the wire field
survives) rather than the shared `Campaign` type. Adding
`operator_agent` to `src/api/types.ts` is the canonical home but fell
outside the #1467 slice's scope (scope-amendment not granted in time),
so the typed-field promotion is a follow-up.

## Implement-stage session view (#205, #215, #218)

`src/implement/session-document.tsx` is the implement-stage page. It
replaced the original PR-card-as-page (#205) — which duplicated the
review surface (#213) — with the agent's record of work. Four sections:

- A foldable **prompt**, fetched via `api.getStagePromptRender` →
  `GET /v0/stages/{id}/prompt-render`, gated on
  `stage.state !== 'pending' && !== 'cancelled'` so we don't render
  "no prompt yet" before the runner has fetched it.
- A **stage-scoped audit feed** (uses the `?stage_id=` filter on
  `/v0/runs/{id}/audit`).
- A **transcript** — the agent's actual turns (see "Trace transcript
  surface" below).
- A small **PR-link footer** ("View PR #N on GitHub" — the full PR card
  lives on the review page).

Domain-aware audit rendering lives in `src/implement/stage-event.ts`:
`policy_evaluated`, `pull_request_opened`, `installation_token_issued`,
`stage_failed` etc. get bespoke labels with payload-derived details;
unknown categories fall through to the raw name + a JSON peek so new
event types don't disappear silently. `<ApprovalPanel>` reuses only when
`state === 'awaiting_approval'` (forward-looking gate, same as the
implement-stage card-view did before).

## Trace transcript surface (#218)

`<TranscriptSection>` (`src/implement/transcript-section.tsx`) on the
implement page renders the agent's actual turns from the runner's trace
bundle — one row per stream-json record (system.init, assistant text,
tool_use / tool_result, result).

Backend: `GET /v0/stages/{id}/trace`
(`backend/internal/server/trace.go::handleGetStageTrace`) walks the
run's `trace_uploaded` audit entries to find the most-recent
**redacted** variant for the stage, then streams its gzipped JSONL bytes
from `tracestore` with `Content-Encoding: gzip` so browsers
auto-decompress. `X-Fishhawk-Content-Hash` carries the bundle sha256 for
cross-reference with the audit log. Raw variant access is intentionally
out of scope — it lives behind S3 Object Lock and needs a separate
auth-tier decision.

Frontend: `parseBundle` (`src/implement/transcript-bundle.ts`) is a
streaming line-splitter that tolerates CRLF, chunk boundaries, and blank
lines, and surfaces malformed records as `unparsed` rather than
dropping. `describeEvent` (`src/implement/transcript-event.ts`) is the
Claude-Code-aware shape adapter — when a second agent runtime lands,
that file is where polymorphism goes. Tool-use / tool-result blocks
default-collapsed; assistant text renders as preformatted (markdown is a
v0.x graduation). Each event row gets a stable `#turn-<n>` id for
deep-linking.

## Review-stage detail surface (#213, #228, #256)

`src/review/review-document.tsx` is the review gate's decision surface.
Review stages don't produce their own artifact; the page reuses the
implement stage's `pull_request` artifact as input.
`src/routes/stage-detail.tsx::ReviewArtifact` is the loader: it lists
the run's stages, picks the implement stage, lists its artifacts,
fetches the most-recent `pull_request` via `api.getArtifact`, then hands
off to `<ReviewDocument>`.

Composition: `<PullRequestSummary>` (shared with the implement page) and
`<RequiredChecksPanel>` (`src/components/required-checks-panel.tsx`,
renamed from `<BlockingChecksPanel>` in #256) listing the run's required
checks with **live state** from `GET /v0/stages/{id}/checks` (#228). The
endpoint returns `declared` (context names) + `sources` (which surfaces
contributed: `branch_protection` and/or `ruleset:<id>`) + `items`
(observed state per name); declared-but-not-observed checks fall back to
`not_tracked`. The panel renders a "Required by branch protection"
attribution sub-label from `sources`.

**Informational only as of #253 / ADR-017**: the approve button no
longer waits on check state; GitHub branch protection is the merge gate,
including for `fishhawk_audit_complete` (published as a Check Run per
#231). `<ApprovalPanel>` is suppressed for check-only review gates
(e.g. `routine_change.workflows.yaml`) — there's no human action; the
gate clears automatically when the checks pass.

## Run-detail evidence narrative — data layer (E40.4 / #1715)

`src/run-narrative/` holds the data layer behind the gate-centric run-detail
narrative. It adds NO backend surface: every section reads an endpoint that
already ships. `narrative.ts` is pure (no fetch, no React) — every decoder
takes an `unknown` payload and returns null on a shape it cannot read, never
throwing. `use-run-narrative.ts` is the composed loader.

| Section           | Reads                                                                                                                                             | Derivation                                                                    |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| plan              | `listRunStages` → newest `plan` stage → `listStageArtifacts` → `getArtifact`                                                                      | `derivePlan` (standard_v1 only)                                               |
| advisory verdicts | audit `plan_reviewed` + `implement_reviewed`; `getRunGateView`                                                                                    | `decodeReviewVerdicts` → `joinConcernLifecycle`                               |
| diff summary      | plan `scope.files`; newest decodable audit `policy_evaluated`                                                                                     | `deriveScopeDivergence`                                                       |
| acceptance        | newest audit `acceptance_outcome_recorded` → `getArtifact(artifact_id)`; plan `verification.acceptance_criteria`                                  | `latestAcceptanceOutcome`, `decodeAcceptanceArtifact`, `deriveAcceptanceRows` |
| approvals         | audit `approval_submitted`                                                                                                                        | `deriveApprovals`                                                             |
| merge             | audit `pr_merged`, `merge_verdict_recorded`, `merge_observation_recorded`, `pr_closed_without_merge`                                              | `deriveMerge` (+ `unavailable[]`)                                             |
| gate timeline     | `listRunStages`; audit `approval_submitted`, `acceptance_outcome_recorded`, `approval_predicate_rejected`, `run_auto_advanced`, `run_auto_driven` | `classifyStage`, `classifyTransition`, `deriveGateTimeline`                   |

- **Degrade one section, never the page.** Every read runs under
  `Promise.allSettled`; a rejected, unconfigured (503 `gate_view_unconfigured`)
  or undecodable read becomes a `DegradeNote {section, kind, read, message}`
  on the ONE section that depends on it (`kind`: `read_failed` / `absent` /
  `unreadable` / `incomplete`). `SectionState.model === null` means the whole
  section is unavailable; a non-null model with notes is a partial degrade
  (e.g. acceptance keeps the recorded verdict + tallies when the artifact
  read fails; verdicts keep every concern labelled `unavailable` when the
  gate view fails). The only page-level failure is `getRun`.
- **Unavailable merge evidence is INDETERMINATE, not "Not merged".** The
  merge section survives a partial read failure (`readable > 0`), but the
  failed read could itself hold the merge event — so
  `MergeSectionModel.unavailable[]` names every merge category whose read
  rejected, and `<MergeBlock>` renders state `indeterminate` whenever no
  terminal outcome was decoded and that list is non-empty. A terminal
  outcome (`pr_merged` / `merge_observation_recorded` /
  `pr_closed_without_merge`) is positive evidence and still stands on its
  own; indeterminacy outranks the `verdict_only` state, which is not an
  observed merge.
- **Bounded reads.** One `listRunAudit` per category with the `category`
  filter and `limit=500`, paginating until `next_cursor` is null or
  `AUDIT_MAX_PAGES` (10), which records an `incomplete` note.
- **Staged scope comes from `policy_evaluated`, not the PR artifact.** The
  `pull_request` artifact carries only `files_changed_count`; the real
  changed-file list is `EvaluationPayload.diff[]`. A rename (`status: "R"`)
  stages its `old_path` too, because standard_v1 has no rename operation —
  a planned rename is declared as delete-old + create-new, and without the
  fold the deleted half reads as a false "declared, not staged" divergence.
- **Per-criterion acceptance rows come from the acceptance ARTIFACT.**
  `acceptance_outcome_recorded` carries only `artifact_id` / `content_hash`,
  the verdict and the tallies. The row inventory is the PLAN's
  `verification.acceptance_criteria`: every plan criterion gets a row, and one
  the artifact did not record renders `pending`; an artifact criterion the
  plan does not declare is appended with `inPlan: false`. The acceptance
  stage is found by the outcome entry's `stage_id`, not `stage.type`
  (`StageType` deliberately stays the OpenAPI enum, which lacks `acceptance`).
- **One decoding contract for acceptance criteria** (`src/api/acceptance.ts`):
  `normalizeAcceptanceCriteria` folds the historical object-keyed-by-id
  variant into the flat array BEFORE `isAcceptanceArtifact` decides, so the
  guard rejects only content neither shape decodes. Read bodies through
  `decodeAcceptanceArtifact`, which returns the normalized array.
- **Concern lifecycle join.** Reviewer verdict payloads carry no concern id,
  so `joinConcernLifecycle` matches open rows on (stage_kind, reviewer_model,
  origin_review_sequence, category, note) and settled rows — which carry no
  origin sequence — on the same key minus the sequence, each row joining at
  most once. An unjoined concern is `unmatched`, never dropped.
- **Gate vs auto-advance.** `classifyTransition`: `approval_submitted`,
  `acceptance_outcome_recorded`, `approval_predicate_rejected` → `gate`;
  `run_auto_advanced`, `run_auto_driven` → `auto_advance`. `classifyStage`
  makes a stage parked in `awaiting_approval` / `awaiting_deploy_approval` a
  `gate` from its STATE, before any audit entry exists. The loader reads every
  category either classifier handles.
- **Evidence links resolve to the entry.** `evidenceHref` renders an
  audit-backed claim as `/runs/:runId?entry=<sequence>#entry-<sequence>` and
  an artifact-backed one as the stage page. `loadAuditEntry` /
  `useAuditEntry` resolve `?entry=N` independently of list pagination via
  `listRunAudit({sinceSequence: N-1, limit: 1})`, accepting the result only
  when its sequence IS N (null → the "entry not found" state). A claim whose
  backing record names only a SEQUENCE — a gate-view fix-up or resolution
  row, which carries neither entry hash nor category — uses
  `sequenceEvidenceRef`, so each history claim resolves ITS OWN later entry
  rather than the review entry that raised the concern; `EvidenceLink`
  labels such a ref `audit entry #N` and the panel reads the hash and
  category back.
- **External hrefs are scheme-allowlisted.** A url decoded from untrusted
  agent output — the plan artifact's `ticket_reference.url`, a `pr_url` off
  an audit payload — passes through `safeExternalHref`, which renders only
  absolute `http:`/`https:` urls as links (React escapes link TEXT but not a
  link's SCHEME, so a `javascript:` value would otherwise be a clickable
  script link). Anything else renders as inert text, so the operator still
  sees the recorded value.

## Threaded runs (#216)

Two columns on `runs` (migration 0016) thread follow-up runs to their
predecessors: `parent_run_id UUID REFERENCES runs(id)` and
`pull_request_url TEXT`, both nullable, both partial-indexed
(`WHERE NOT NULL`).

Backend plumbing: the dispatcher's `findParentRunID`
(`backend/internal/webhook/dispatcher.go`) reads the most-recent
non-terminal run for `(repo, trigger_ref)` before creating a new one and
threads the new run as a follow-up — turning repeated `/fishhawk run`
invocations on the same issue into a chain instead of disconnected runs.
The PR-upload handler (`backend/internal/server/pullrequest.go`)
backfills `pull_request_url` after the artifact lands so the
threaded-runs view can group by PR with a single equality query rather
than a recursive parent walk. New `run.Repository.SetRunPullRequestURL`

- `ListRunsFilter.PullRequestURL` / `TriggerRef`.
  `GET /v0/runs?pull_request_url=…` and `?trigger_ref=…` are equality
  filters powering the SPA's grouping.

SPA: `<RelatedRunsSection>` (`src/runs/related-runs.tsx`) on the
run-detail page renders sibling runs grouped by PR (preferred when set)
or trigger_ref (fallback); `<FollowUpLink>` in the run-detail header
points at `parent_run_id` when set.

Decision per the #216 body: option (a) `parent_run_id` as the primitive

- (b) `pull_request_url` denorm, as a hybrid. Out of scope: CI-failure /
  PR-comment triggers (#216 calls these out as their own issues).

## Policy section on the implement page (#233)

`src/implement/policy-section.tsx` surfaces the most-recent
`policy_evaluated` audit entry for the implement stage rather than
burying it as a one-line row in the activity feed. Three states:

- **pending** — no entry yet; a single line.
- **pass** — green header + diff summary + foldable applied-constraints
  list, default-open so reviewers see _what was checked_, not just that
  something passed.
- **fail** — `Policy violations (N)` header + violations grouped by
  constraint name with per-violation files; applied constraints fold
  away by default but stay accessible.

Reads via the existing `?stage_id=&category=policy_evaluated&limit=1`
filter on `/v0/runs/{id}/audit` — no new endpoint. Backend touch:
`policy.Constraints` ships JSON tags so the wire shape is snake_case
(`forbidden_paths`, `allowed_paths`, `max_files_changed`,
`required_outcomes`, `ci_green`) — consistent with the rest of the
audit-payload schema.

`<FailureBanner>` adds a "View violations" anchor link to `#policy` for
category-B failures so a reviewer can jump straight from the banner to
the structured diagnostic. Out of scope: plan-stage policy evaluation
(today only the implement-stage diff is evaluated), per-rule severity
tiers, constraint editing UI.

## Post-sign-in redirect intent

`<RequireAuth>` (`src/auth/require-auth.tsx`) captures
`location.pathname + location.search` and forwards it to `/login` as
`?next=…` when an unauthenticated visitor hits a deep link. The Login
route (`src/routes/login.tsx`) reads `next` and appends it to
`/v0/auth/github/login?next=…`.

The backend stashes the value in a short-lived `fishhawk_oauth_next`
cookie at login (`server.handleGitHubLogin`) — only after
`isSafeRelativeRedirect` passes, dropping anything that looks like an
absolute URL, scheme-relative URL, or `/\…` Windows-path fragment. The
callback (`server.handleGitHubCallback`) reads the cookie, re-validates
(defense in depth), uses it as the redirect target, and clears the
cookie. Tampered or malformed values fall back to the
operator-configured default. Constants in
`backend/internal/auth/auth.go`.

## See also

- `docs/MVP_SPEC.md` §5.1.3 (Web UI scope)
- `docs/BRAND_FOUNDATIONS.md` §6 (UI principles: density, restraint,
  audit log as a first-class surface)
- `docs/api/v0.openapi.yaml` (the REST contract this UI consumes)
