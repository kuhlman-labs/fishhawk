# repodoc — repo-authored document injection

`backend/internal/repodoc` is the consumer-agnostic mechanism for injecting a
repo-authored document into an agent prompt (E55.1 / #2242).

Two consumers will attach to it:

| Consumer | Declared path | Declaration site |
|---|---|---|
| E55 review conventions | `.fishhawk/review-conventions.md` (repo's choice) | `review_conventions[]` in `.fishhawk/workflows.yaml` |
| #2234 product charter | `.fishhawk/charter.md` | `charter.path` in `.fishhawk/work-management.yaml` |

Neither declaration site ships in this slice. The package has **no production
caller today**: the server seam (`Config.DocumentDeclarations` /
`DocumentResolver` / `DocumentScope`) is nil, so
`Server.resolveInjectedDocuments` returns `(nil, nil)` and every served prompt
is byte-identical to the pre-#2242 render.

## Why server-side injection at all

The alternative — telling the agent "read `.fishhawk/charter.md`" — is not a
control. The agent's working tree is writable by the agent itself and by the
change under review, so a pointer can be satisfied from a file the adversary
controls. This mechanism reads the document **server-side**, from the run's
**base ref pinned to a commit SHA**, and renders the bytes into the prompt as
quoted data. `repodoc` imports no file-read API at all — there is no code path
by which a working-tree file can become the injected document.

## Resolution order (`Resolver.Resolve`)

Every step fails closed.

| Step | Behavior | Failure |
|---|---|---|
| a | Validate the declared path: non-empty, repo-relative, no leading `/`, no `\`, no `.` / `..` / empty segment, canonical under `path.Clean` | `ErrInvalidPath` |
| b | Refuse an **empty** base ref | `ErrUnpinnedBaseRef` |
| c | Pin the ref: a 40-hex ref is used verbatim; anything else resolves via `GetBranchSHA` | `ErrUnpinnedBaseRef` (missing branch) or the wrapped transport error |
| d | `FetchFile` **at the pinned commit SHA** | wrapped transport error |
| e | `forge.ErrNotFound` → declared-but-absent | `ErrMissingDocument` |

Every failure is wrapped in a `*ResolveError` carrying the path **and** the
declaration site, so an operator reading the error knows which knob produced
it.

### Why an empty base ref is refused rather than defaulted

`forge.FileFetcher` documents an empty `ref` as *the repo's default branch* —
the GitHub implementation omits the ref parameter, the GitLab implementation
substitutes `HEAD`. A default-branch read is a **mutable** read: it is exactly
the unpinned read the security property forbids. So an empty ref is a refusal,
never a fallback (`TestResolve_EmptyBaseRef_Refused`).

The same reasoning covers `GetBranchSHA`'s missing-branch shape. It reports a
missing branch as `("", false, nil)` — **not** an error. Treating `found=false`
as a fall-through would produce an empty ref and hence a default-branch read,
so it is turned into a fail-closed error
(`TestResolve_BaseBranchNotFound_FailsClosed`).

`forge.FileContent.SHA` is the forge's **blob** id (GitHub blob SHA, GitLab
`blob_id`), not a commit SHA. The attributed commit therefore comes from the
pinned commit resolution, never from `FileContent.SHA`.

## Where the base ref comes from (the #2234 hand-off)

**There is no per-run base-ref source reachable at prompt-serve time today.**
This was verified for this slice, and it is recorded here so #2234 does not
discover it late:

- `run.Run` carries no base-branch or base-commit field, and no migration adds
  one.
- The base branch exists only as **runner argv**: it is an MCP tool input
  (`base_branch` on `fishhawk_run_stage` / `dispatch_stage` / `drive_run` /
  `run_children`, defaulting to `"main"`), composed into `--base-branch` /
  `--check-base-ref` by `backend/internal/mcpserver/dispatch_stage.go`. For a
  dependent fan-out child the server is the authority
  (`Server.resolveDependentChildBase`, `backend/internal/server/host_dispatch.go`),
  but that value is returned in the host-dispatch marker response — it is not
  persisted on the run.
- `GET /v0/stages/{id}/prompt` therefore cannot recover the run's base ref from
  storage.

So `Config.DocumentDeclarations` returns the base ref **as its second return
value**: the mechanism takes it as a caller-supplied parameter rather than
guessing. **#2234 must create the per-run source** before it can supply a real
one. The concrete options, in preference order:

1. **Persist the base ref on the run row** at run-create / dispatch time (a
   `base_branch` column plus a migration), which makes it available to every
   later read including this one. This is the option that also fixes the
   adjacent gap that the base ref is not auditable today.
2. **Record it as a run-scoped audit entry** at dispatch and read it back with
   `ListForRunByCategory` — cheaper (no migration), but a run whose dispatch
   predates the entry has no base ref, so the seam must fail closed rather than
   default.

Whichever is chosen, the value handed to the seam MUST be the run's **base**
branch or a commit SHA — never the run's own branch, which the agent can write.
A pinned-SHA preference is verified here by fake seam
(`TestResolve_ReadsFromPinnedBaseRef`, `TestGetStagePrompt_InjectedDocument_EndToEnd`);
the provenance of that SHA is #2234's to establish.

## Content-hash byte domain

**One domain, everywhere:** `Document.ContentHash` is `sha256:<hex>` over the
**RESOLVED bytes exactly as fetched** — *pre*-truncation, *pre*-neutralization.

Attribution answers *"which revision of this document constrained the agent"*,
and that question must have the same answer whether or not the document
happened to exceed the cap or to contain a forged delimiter line. What was
actually **shown** is described by the sibling fields — `OriginalBytes`,
`RenderedBytes`, `DroppedBytes`, `CapBytes`, `Truncated` — not by the hash.
`TestResolve_ContentHashCoversResolvedBytesPreTruncation` asserts the recorded
hash against an explicitly constructed expected byte sequence and asserts it is
**not** the hash of the truncated content.

## Size cap and loud truncation

`DefaultMaxBytes` is 32 KiB, overridable per `Resolver` (`MaxBytes`). It is a
judgment call bounded by the #606 added-prompt-cost precedent, not a measured
limit: a governance document is per-repo stable, so it rides the cache-stable
prefix and is paid for once per cached prefix rather than per turn.

An over-cap document is cut rune-safely (`strings.ToValidUTF8` drops the
trailing partial rune, the same idiom as `prompt.CapTextWithRetrieval`) and
carries a marker naming bytes shown, original bytes, dropped bytes, the cap,
the resolved path and the pinned commit, plus an explicit statement that the
visible text is INCOMPLETE. Truncation is **never silent at any layer**: the
marker is in the prompt, `Truncated` is on the Document, the rendered block
discloses it, and a `document_truncated` audit entry accompanies the injection.

An at-cap document renders verbatim (the `>` not `>=` boundary).

## Framing integrity

`Render(doc, framing)` is pure and deterministic. It emits the consumer's
heading, preamble and trust note, a fixed **data-not-instructions** clause, a
source line naming path + commit + content hash, and the document body between
fixed `BEGIN`/`END` delimiters.

Any body line that **is** a delimiter (whitespace-trimmed) is replaced by a
neutralization note. Without that, committed content could close the boundary
and speak as framing — the document would stop being quoted data and start
being prompt. The delimiters carry no interpolated path or commit precisely so
that "is this the closing delimiter?" is a byte-exact question with one answer.

## Audit categories

| Category | When | Payload keys |
|---|---|---|
| `document_injected` | every injection | `declaration_site`, `path`, `commit`, `content_hash`, `original_bytes`, `rendered_bytes`, `truncated` |
| `document_truncated` | in **addition**, when the document was cut | `path`, `commit`, `content_hash`, `cap_bytes`, `dropped_bytes` |

Both are registered in `audit.KnownCategories`, so `fishhawk_await_audit` and
`GET /v0/runs/{id}/audit` accept them without `allow_unknown`. `cap_bytes` is
the **configured** cap carried on the Document — it cannot be reconstructed
from `OriginalBytes` and `RenderedBytes` once a rune-safe cut and a marker have
been applied.

`Attribute` **fails closed**: an append error is returned and the caller must
not inject. `Server.resolveInjectedDocuments` returns **no documents at all**
on an attribution failure — an un-attributed injection is exactly what the
attribution property forbids, so "log and proceed" is a defect, not a degrade.

### Attribution is PER SERVE — intended, not a bug

Attribution runs in the stage-prompt handler, so **every fetch of a stage
prompt appends fresh entries**. A retry, a re-dispatch, or an operator
inspecting a prompt each accumulate another `document_injected` entry for the
same document revision.

That is deliberate and it is the safer direction. The guarantee is *every
injection is attributed*; de-duplicating by content hash would trade it for
*some injections are attributed*, and the entries that would be dropped are
exactly the ones covering re-dispatches — the case where knowing what the agent
was shown matters most. A reader wanting the distinct set groups by
`content_hash`; do **not** dedupe at write time.

Note that only the **signed** `/prompt` endpoint injects and attributes.
`/prompt-render` (the unsigned preview) does not, so a preview never writes to
the audit trail. Once a declaration site ships, a preview and a dispatched
prompt will differ by the injected block; that is the trade for not letting a
read-only preview mutate the run record.

## Fail-closed matrix

| Mode | Behavior | Test |
|---|---|---|
| M1 empty base ref | refuse before any fetch | `TestResolve_EmptyBaseRef_Refused` |
| M2 base branch not found | error, never an empty-ref fetch | `TestResolve_BaseBranchNotFound_FailsClosed` |
| M3 ref-resolution transport error | wrapped, no degrade | `TestResolve_RefResolutionError_FailsClosed` |
| M4 invalid declared path | refuse before any fetch | `TestResolve_InvalidPath_FailsClosed` |
| M5 declared document absent | `ErrMissingDocument`, never an empty document | `TestResolve_MissingDocument_FailsClosed` |
| M6 fetch transport error | wrapped, not reported as missing | `TestResolve_FetchTransportError_FailsClosed` |
| M7 over-cap document | loud truncation + paired audit entry | `TestResolve_OverCap_TruncatesLoudly` |
| M8 audit append failure | error; document NOT injected | `TestAttribute_AppendFailure_FailsClosed`, `TestGetStagePrompt_AttributionFailure_DocumentNotInjected` |
| M9 forged END delimiter | neutralized with a visible note | `TestRender_ForgedEndDelimiter_Neutralized` |
| no fetcher / no commit resolver | refuse | `TestResolve_NoFetcherConfigured_FailsClosed`, `TestResolve_NoCommitResolverForBranchRef_FailsClosed` |
| malformed run repo | refuse before any fetch | `TestResolveInjectedDocuments_MalformedRepo_FailsClosed` |
| credential-scope resolution failure | refuse before any fetch | `TestResolveInjectedDocuments_ScopeResolutionError_FailsClosed` |
| declaration seam error | prompt request fails 500 | `TestGetStagePrompt_DeclarationSeamError_FailsClosed` |

## Consumer contract

A consumer supplies exactly two things: a `Declaration` (path + declaration
site, plus the `Framing` it wants) and the base ref to pin against. Everything
else is shared. `Declaration.Framing` rides on the declaration purely so one
seam value carries both halves of a consumer's contribution; `Resolve` ignores
it entirely — only `Render` reads it.

`TestTwoShapedConsumers_OneImplementation` drives a conventions-shaped and a
charter-shaped caller through the same `Resolver`/`Render`/`Attribute` and
asserts their outputs differ only in the caller-supplied path, framing and
body. `TestRepodocCarriesNoConsumerVocabulary` scans the package's non-test
source (comments stripped) for consumer words, so the mechanism cannot quietly
learn about conventions or charters.

## Prompt placement

`prompt.Trigger.InjectedDocuments` renders at the **head of the cache-stable
prefix** of `buildPlan`, `buildPlanReview`, `buildImplement` and
`buildImplementReview` — far ahead of `PlanReviewSplitMarker` /
`ImplementReviewSplitMarker` — so a per-repo-stable document costs nothing
incremental across a stage's fix-up re-review rounds. An empty slice renders
nothing at all. See `backend/internal/prompt/README.md`.

The slim fix-up prompt (`buildImplementFixup`) does **not** render injected
documents: it forks before the writer, and a fix-up pass is a targeted patch
against concerns already judged under the document. If a consumer needs the
document on that path, wire the writer into `buildImplementFixup` too.
