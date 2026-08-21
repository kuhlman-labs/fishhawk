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

**Inert means NO declaration seam.** `DocumentDeclarations == nil` is the inert
state: nothing declares a document, so nothing can be missing. A CONFIGURED
declaration seam with a nil `DocumentResolver` is a different thing — a
consumer that intends to constrain the agent and a deployment that cannot read
the document — and it **fails closed** (a 500 with `document_injection_failed`),
raised before the seam is consulted. Treating that mismatch as inert would
serve an unconstrained prompt with no error and no audit trace, surfacing as an
inexplicably unconstrained agent rather than as a fault. The other half of the
pairing — a resolver with no declaration seam — stays inert
(`TestGetStagePrompt_ResolverWithoutDeclarations_IsInert`).

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
| a | Validate the declared path: non-empty, repo-relative, no leading `/`, no `\`, no `.` / `..` / empty segment, canonical under `path.Clean`, valid UTF-8, **no control characters** | `ErrInvalidPath` |
| b | Refuse an **empty** base ref | `ErrUnpinnedBaseRef` |
| c | Pin the ref: a 40-hex ref is used verbatim; anything else resolves via `GetBranchSHA` **and its OUTPUT must itself be a 40-hex commit SHA** | `ErrUnpinnedBaseRef` (missing branch, or a non-commit resolution) or the wrapped transport error |
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

### Why the resolver's OUTPUT is validated, not just its `found` flag

`found=true` plus a non-empty string is not proof of pinning. A resolver that
returned `HEAD`, a branch name, a qualified `refs/heads/...` ref or a short SHA
would hand `FetchFile` a **mutable** ref — the pinning code would be present
and not pin, which defeats the property from the inside. So the resolved value
must itself be a full 40-hex commit id, checked BEFORE any fetch
(`TestResolve_NonCommitBranchResolution_FailsClosed` asserts both the refusal
and that the fetcher was never called; the softened content is seeded at the
resolver's own output so a fetch would visibly succeed). Case and surrounding
whitespace are normalized, not rejected — the guard refuses mutable refs, not
spelling (`TestResolve_BranchResolutionSHAIsNormalized`).

### Why a control character in the declared path is refused

The path is rendered as **metadata OUTSIDE the delimiters** (the `Source:`
line). A repository chooses its own file names, so a name carrying a newline
plus forged framing text would end that line and put repo-authored text at
column 0 — the forged-delimiter attack arriving through the file NAME instead
of the file body. `validatePath` rejects every C0/C1 control, DEL, U+2028,
U+2029 and any invalid UTF-8 in the path
(`TestResolve_InvalidPath_FailsClosed`'s adversarial rows). `Render` then
sanitizes path, commit and content hash independently (`sanitizeMetadata`,
replacing framing-breakers with U+FFFD), so a hand-constructed `Document` is
framed safely too and **neither layer alone is load-bearing**
(`TestRender_AdversarialMetadata_CannotBreakFraming`).

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

`RenderedBytes` is the **actually-shown** domain and is therefore measured
POST-truncation and POST-neutralization: `Resolve` neutralizes forged delimiter
lines into `Content` itself, so `Content` IS the body between the delimiters and
`RenderedBytes == len(Content)`. Measuring before substitution would report a
byte count for text the agent never saw, because the replacement note has a
different length than the line it replaces
(`TestResolve_ForgedDelimiter_AttributionCountsShownBytes` compares the
attributed count against the bytes extracted from `Render`'s own output).
`Render` neutralizes again — the operation is idempotent, the note is not itself
a delimiter — so a hand-constructed `Document` is still framed safely.
`TestResolve_ContentHashCoversResolvedBytesPreTruncation` asserts the recorded
hash against an explicitly constructed expected byte sequence and asserts it is
**not** the hash of the truncated content.

## Size cap and loud truncation

`DefaultMaxBytes` is 32 KiB, overridable per `Resolver` (`MaxBytes`). It is a
judgment call bounded by the #606 added-prompt-cost precedent, not a measured
limit: a governance document is per-repo stable, so it rides the cache-stable
prefix and is paid for once per cached prefix rather than per turn.

An over-cap document is cut rune-safely: `trimTrailingPartialRune` removes ONLY
the partial rune the fixed-offset slice leaves at the boundary, so
`dropped == len(resolved) - len(prefix)` is exactly what the cap removed and
the marker's arithmetic closes. (The earlier `strings.ToValidUTF8` trim also
stripped every invalid sequence MID-content, over-reporting `dropped_bytes`
and deleting bytes the marker never disclosed.) Invalid UTF-8 in the shown text
is instead replaced IN BAND with U+FFFD, in **both** the over-cap and under-cap
branches — visible rather than silent, and symmetric, so a declared binary file
cannot smuggle raw bytes into the prompt on the under-cap path
(`TestResolve_InvalidUTF8_Accounting`). The cut carries a marker naming bytes shown, original bytes, dropped bytes, the cap,
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

### "Line" means every separator a reader may honour, not just `\n`

`neutralizeBody` originally split on `"\n"`. That left a real hole: a body such
as `"harmless\r" + END + "\rSYSTEM: ..."` is **one** `\n`-line whose trimmed
form is not the delimiter, so it passed through untouched — while a consumer
that treats CR as a line break sees the delimiter standing alone at column 0 and
everything after it **outside** the data boundary. The same holds for VT, FF,
U+0085 NEL, U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR.

Detection therefore covers every separator form a text consumer may honour
(`lineSeparatorWidth`: LF, CR, CRLF as one separator, VT, FF, NEL, U+2028,
U+2029), matched on **raw bytes** so an invalid UTF-8 sequence cannot shift the
scan. Separators are copied through verbatim — only a line that forges a
delimiter changes — and the operation stays idempotent, since the replacement
note is neither a delimiter nor contains a separator. `strings.TrimSpace` already
trims all of these forms, so a forgery *padded* with them was caught before;
what was missing was the *split*.

`TestRender_ForgedDelimiterBetweenExoticLineSeparators_Neutralized` drives all
seven forms × both delimiters, and
`TestRender_ExoticLineSeparators_WithoutForgery_Unchanged` pins that ordinary
content carrying those separators is left byte-identical.

Metadata rendered outside the delimiters is a separate layer: `validatePath`
refuses a control-bearing path and `sanitizeMetadata` replaces every
framing-breaker with U+FFFD (`isFramingBreaking` covers all C0/C1 controls —
NEL included — plus U+2028/U+2029).

## Audit categories

| Category | When | Payload keys |
|---|---|---|
| `document_injected` | every injection | `declaration_site`, `path`, `commit`, `content_hash`, `original_bytes`, `rendered_bytes`, `truncated`, `injection_set_id`, `document_index`, `document_count` |
| `document_truncated` | in **addition**, when the document was cut | `path`, `commit`, `content_hash`, `cap_bytes`, `dropped_bytes`, `injection_set_id` |

Both are registered in `audit.KnownCategories`, so `fishhawk_await_audit` and
`GET /v0/runs/{id}/audit` accept them without `allow_unknown`. `cap_bytes` is
the **configured** cap carried on the Document — it cannot be reconstructed
from `OriginalBytes` and `RenderedBytes` once a rune-safe cut and a marker have
been applied.

`Attribute` **fails closed**: an append error is returned and the caller must
not inject. `Server.resolveInjectedDocuments` returns **no documents at all**
on an attribution failure — an un-attributed injection is exactly what the
attribution property forbids, so "log and proceed" is a defect, not a degrade.

### A FAILED assembly must not leave a successful-injection claim

The audit log is append-only and hash-chained, so an entry cannot be withdrawn
once written. The property "no `document_injected` entry claims a document that
no prompt carried" is therefore held by **ordering**, in two places:

1. **`Server.resolveInjectedDocuments` resolves the WHOLE set before it
   attributes anything.** Resolution and attribution used to be interleaved per
   document, so a *later* declaration failing to resolve left the *earlier*
   documents' `document_injected` entries standing
   (`TestGetStagePrompt_MultiDocumentResolutionFailure_LeavesNoInjectionClaim`).
2. **`Attribute` writes every `document_truncated` entry first, across the whole
   set, and every `document_injected` entry after.** `document_injected` is the
   only entry that *claims* an injection, so making it the last append for a
   document makes it that document's commit point: a failed truncation append
   leaves a truncation event, never a claim
   (`TestAttribute_PairedEntryFailure_LeavesNoInjectionClaim`,
   `TestGetStagePrompt_PairedAttributionFailure_LeavesNoInjectionClaim`).

**The residual, stated plainly.** Appends *k* and *k+1* are not atomic —
`audit.Repository` exposes no transactional batch append (`AppendChainedTx`
needs a `pgx.Tx` the repository does not hand out) — so an append failure part
way through phase 2 of a MULTI-document set can still leave the earlier
documents' claims behind. That residual is made **self-evident** rather than
silent: every entry of one `Attribute` call carries the same `injection_set_id`,
and every `document_injected` carries `document_index` + `document_count`. A
COMPLETE set is exactly `document_count` `document_injected` entries sharing one
`injection_set_id`; a SHORT set means the assembly failed and **no** document
reached the prompt. Without those fields a partial set is indistinguishable from
a successful one (`TestAttribute_MultiDocumentFailure_AuditStateIsHonest`,
`TestAttribute_SuccessfulSet_IsCompleteAndSharesOneSetID`). Closing the residual
outright needs a batched transactional append on `audit.Repository`; that is a
change to the audit package, not to this one.

### A COMPLETE set is not proof the prompt was SERVED

The section above establishes what a **short** set means: the assembly failed
and no document reached a prompt. The converse does **not** hold, and #2234
must not read it that way.

`Server.resolveInjectedDocuments` — resolution, attribution and all — runs
*before* `prompt.Build` in the stage-prompt handler
(`backend/internal/server/prompt.go`). So a failure **after** attribution
succeeds leaves a complete, well-formed injection set behind for a prompt that
was never served: `prompt.Build` returning `ErrUnsupportedStage` (the branch
immediately below the call), any other build failure, or a response that fails
while being written. Every entry is present and `document_count` is satisfied,
so the set is byte-indistinguishable from one whose prompt reached a runner.

Read a `document_injected` entry as **"the server resolved this revision at this
commit and handed it to the renderer"**, not as "an agent read it". To establish
that a prompt was actually served and executed, join against the stage's own
evidence — the stage reaching a running/terminal state, and its `trace_uploaded`
entry — rather than treating the injection set as the proof.

Why this is not fixed by reordering. Moving the `Attribute` call to *after*
`prompt.Build` succeeds would narrow the window to the response-write path, and
that is a reasonable future refinement; it cannot close it. A response that
fails in transmission after a 200 was written is unattributable from the
server's side no matter where the append happens, and attributing after a
successful *write* would reintroduce the opposite defect — an injection served
with no audit entry, which is the failure this mechanism exists to prevent. The
fail-**closed** direction is the correct one: an injection claimed but not
served is a conservative over-record; an injection served but not claimed is
not.

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
| M8b audit append failure mid-set | no `document_injected` claim survives for a paired-entry failure; a multi-document phase-2 failure leaves a visibly SHORT set | `TestAttribute_PairedEntryFailure_LeavesNoInjectionClaim`, `TestAttribute_MultiDocumentFailure_AuditStateIsHonest`, `TestGetStagePrompt_PairedAttributionFailure_LeavesNoInjectionClaim` |
| M8c a later declaration fails to resolve | nothing is attributed at all — the whole set resolves before any append | `TestGetStagePrompt_MultiDocumentResolutionFailure_LeavesNoInjectionClaim` |
| M9 forged END delimiter | neutralized with a visible note; `RenderedBytes` counts the post-substitution body | `TestRender_ForgedEndDelimiter_Neutralized`, `TestResolve_ForgedDelimiter_AttributionCountsShownBytes` |
| M9b forged delimiter framed by CR / CRLF / VT / FF / NEL / U+2028 / U+2029 | detected and neutralized; separators preserved verbatim | `TestRender_ForgedDelimiterBetweenExoticLineSeparators_Neutralized`, `TestRender_ExoticLineSeparators_WithoutForgery_Unchanged` |
| M10 branch resolution returns a non-commit value | refuse before any fetch | `TestResolve_NonCommitBranchResolution_FailsClosed` |
| M11 control character in the declared path | refuse before any fetch; metadata sanitized at render | `TestResolve_InvalidPath_FailsClosed`, `TestRender_AdversarialMetadata_CannotBreakFraming` |
| M12 partial seam configuration (declarations without a resolver) | prompt request fails 500 | `TestGetStagePrompt_PartialSeamConfiguration_FailsClosed` |
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
