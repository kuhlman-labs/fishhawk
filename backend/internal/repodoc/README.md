# repodoc — repo-authored document injection

`backend/internal/repodoc` is the consumer-agnostic mechanism for injecting a
repo-authored document into an agent prompt (E55.1 / #2242).

Two consumers will attach to it:

| Consumer | Declared path | Declaration site |
|---|---|---|
| E55 review conventions (E55.3 / #2244) | each entry's `path` (repo's choice) | `review_conventions.<name> in .fishhawk/workflows.yaml` |
| #2234 product charter | `.fishhawk/charter.md` | `charter.path` in `.fishhawk/work-management.yaml` |

Neither declaration site ships in this slice. The package has **no production
caller today**: the server seam (`Config.DocumentDeclarations` /
`DocumentResolver` / `DocumentScope`) is nil, so
`Server.resolveInjectedDocuments` returns `(nil, nil)` and every served prompt
is byte-identical to the pre-#2242 render.

**E55 review conventions attach via `BaseSourceRunAdmission`, not the seam.**
`Server.resolveReviewDocuments` (`backend/internal/server/document_injection.go`)
selects the reviewed stage's `reviewers.conventions` from the run's
workflow-spec snapshot and appends one run-admission `Declaration` per
selected convention AFTER the seam's, so conventions ride the same base-source
partition and the same resolve-everything-before-any-attribution ordering. They
are resolved ONLY on the in-process review paths (plan review, implement
review) and returned as `prompt.ReviewConvention`s — never as injected
documents and never on an author prompt. A missing REQUIRED convention fails
the set (`review_convention_missing`) before any audit entry; a missing
OPTIONAL one (`required: false`) is withheld with reason
`optional_document_missing` (`WithheldReasonOptionalDocumentMissing`) on the
existing `document_injection_degraded` category — no new audit category — and
renders nothing. A selected convention with a nil `DocumentResolver` fails
closed even with no declaration seam. The review resolve phase (seam call,
credential scope, every `Resolve`) is bounded by the server's
`reviewDocumentResolveTimeout`; the audit appends run on the caller's context.
Server-side contract: `backend/internal/server/README.md`.

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
| b2 | Refuse an unknown `Declaration.Base`; for a `BaseSourceRunAdmission` declaration, refuse any ref that is not already a 40-hex commit — **before** step c, so `GetBranchSHA` is never consulted for it | `ErrUnknownBaseSource` / `ErrUnpinnedBaseRef` |
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

## Where the base ref comes from

Two sources, selected PER DECLARATION by `Declaration.Base` (a `BaseSource`):

| `Declaration.Base` | Resolved against | `base_source` on `document_injected` | Consumer today |
|---|---|---|---|
| `BaseSourceDeclarationSeam` (zero value) | the ref `Config.DocumentDeclarations` returns as its second value (a branch pinned at serve time, or a commit) | `declaration_seam` | the #2234 charter (default-branch head at serve time, below) |
| `BaseSourceRunAdmission` | `runs.document_base_commit` — the commit recorded on the run at admission, and nothing else | `run_admission` | none yet; E55.2 attaches here (the review prompts honour it since #2797) |

Before E55.7 there was no per-run base source reachable at prompt-serve time:
`run.Run` carried no base field, and the base branch existed only as runner
argv (the `base_branch` MCP dispatch input, composed into `--base-branch` by
`backend/internal/mcpserver/dispatch_stage.go`). That is why
`Config.DocumentDeclarations` returns a base ref at all, and it still does for
seam-sourced declarations.

### Run-admission base source (E55.7 / #3746)

- **Recording.** At every ROOT run-mint seam — `POST /v0/runs` and campaign item
  start (both via `CreateRunForTrigger`), and the GitHub and GitLab webhook
  dispatchers — the server pins the repository's default-branch head through the
  existing document seam (`DocumentScope` → `DocumentBaseRef` →
  `Resolver.PinCommit`) and stamps it on `runs.document_base_commit` (nullable;
  a DB CHECK admits only NULL or a lowercase 40-hex commit). Capture is bounded
  and **never blocks admission**: every capture failure records NULL.
- **Inheritance.** Children (operator recovery, CI retry, decomposition) inherit
  the column verbatim through `run.ChildParamsFrom`, and a fix-up re-serves from
  the same row, so retries and fix-ups resolve against the admission commit —
  never a newer head and never the run's own (agent-writable) branch.
- **Resolution.** The server hands a run-admission declaration
  `Request.BaseRef = *runRow.DocumentBaseCommit`. `Resolve` refuses any ref for
  it that is not already a 40-hex commit **before** `pinCommit`, so
  `GetBranchSHA` is never consulted and a branch name can never become the read
  point, whatever a future caller passes
  (`TestResolve_RunAdmission_RefusesBranchRef`, whose fake commit resolver maps
  every ref to a valid commit so the guard is the only thing in the path;
  `TestResolve_RunAdmission_ReadsAtRecordedCommit`). An unknown `Base` is
  refused rather than read as the zero value, which would silently resolve at
  the seam's ref (`TestResolve_UnknownBaseSource_Refused`).
- **Withholding, not fallback.** A run with NULL `document_base_commit` (a legacy
  row, a deployment with no document seam, or a degraded capture) has its
  run-admission declarations **WITHHELD**: none is resolved or fetched. Falling
  back to the default branch would be exactly the mutable read this source
  rules out. The prompt carries `WithheldNotice` — heading `Declared repository
  documents withheld`, reason `run_base_commit_unrecorded`
  (`WithheldReasonRunBaseUnrecorded`), and each withheld path and declaration
  site — on BOTH the served and preview paths, so the two stay byte-identical,
  and on the in-process review prompts (#2797). The notice states only what the
  server knows: the documents are DECLARED and were NOT fetched or read, so it
  says nothing about their content or existence at any commit (the earlier "not
  absent and were not empty" sentence claimed what the server never checked —
  #2797, carried from #3746). The notice is system-authored; every interpolated value passes
  `sanitizeMetadata`, so a repo-chosen file name carrying a newline cannot start
  a forged heading (`TestWithheldNotice_AdversarialMetadata_CannotStartALine`).
  Its `Path` / `Commit` / `ContentHash` are empty, so it can never satisfy a
  consumer's injected-document identity check.
- **Attribution of a withholding.** The served path (only) calls
  `RecordWithheld`, which writes ONE `document_injection_degraded` entry and
  **fails closed** exactly like `Attribute` — an append error, a nil appender,
  or an empty reason is returned and the caller must not serve the prompt
  (`TestRecordWithheld_FailsClosed`). The preview writes nothing, matching the
  attribution domain below.

`Resolver.PinCommit(ctx, repo, scope, ref)` is the exported admission-time pin:
the same rules as `Resolve`'s step c (a 40-hex ref verbatim and lowercased;
anything else through `GetBranchSHA`, whose OUTPUT must itself be a full commit
SHA), plus an explicit refusal of an empty ref (`TestPinCommit`).

**Residuals, stated plainly.**

1. **The admission commit is the DEFAULT-BRANCH head at run creation**, not the
   run's own base branch: at admission the server cannot know the
   `--base-branch` a later `dispatch_stage` passes. This repo bases every run on
   the default branch, so the two coincide here; a run dispatched against a
   non-default base reads documents from the default-branch head captured at
   admission. That is a fidelity residual, not a security one — documents never
   follow the run's own branch. Capturing the run's own non-default base branch
   is tracked as **#3902**.
2. **The webhook dispatcher's capture hook is wired in `cmd/fishhawkd/serve.go`
   after `server.New` with no direct automated test** (the dispatcher's own stamp
   is unit-tested with a fake hook). Deleting that wiring leaves webhook-minted
   runs NULL, which degrades safely — withheld with the notice, never a mutable
   read. Tracked as **#3902**.
3. **The charter keeps its serve-time default-branch semantics** (below) until
   E71.2 / #3242, which should switch its declaration to
   `BaseSourceRunAdmission` and reuse `runs.document_base_commit` rather than add
   a second column.
4. A deployment with no attributable forge credential for a repo fails
   `DocumentScope` at admission, records NULL, and withholds; the notice names
   the cause.

### What #2234 actually did (the charter consumer)

The charter consumer (`backend/internal/server/charter_injection.go`) took
**neither** option below. It supplies the **repository's DEFAULT BRANCH**,
resolved explicitly through `forge.Forge.GetRepository` by a
`Config.DocumentBaseRef` adapter wired in `cmd/fishhawkd`, and lets `Resolve`
pin it to a commit.

State the guarantee as it is, not as "resolved from the run's base ref":

- **Resolution pins to a specific commit BEFORE any fetch, never a mutable
  ref.** `pinCommit` accepts only a full 40-hex commit id and refuses an empty
  ref outright, so the two-step default-branch-then-pin path cannot degrade
  into a mutable read.
- **The resolved commit and content hash are recorded** (`document_injected`),
  so a ranking is attributable to an exact document revision.
- **That commit is the default-branch head AT PROMPT-SERVE TIME**, which for a
  non-diff workflow is the defined base: a backlog-grooming run produces no
  diff and owns no branch, so there is no per-run base to resolve and the trunk
  IS the base.

The honest consequence: **a document amendment landing between run creation and
prompt serve changes which revision constrains that run.** For grooming that is
wanted (the groomer should rank against the current charter), and the recorded
commit + content hash make which revision applied decidable after the fact.

**This shortcut is NOT reusable by E55's review-conventions consumer.** That
consumer attaches to CODE-CHANGE runs, where a serve-time head would let a
document amendment landing mid-run change which revision constrains a retry or
fix-up. It declares `BaseSourceRunAdmission` instead (above). Of the two options
this section used to list — persist the base on the run row, or record it as a
dispatch audit entry — E55.7 took the first, as a **commit** rather than a
branch name: a persisted branch name would still be a mutable read point.

Whatever the source, the value handed to `Resolve` MUST be the run's **base**
or a commit SHA — never the run's own branch, which the agent can write.
A pinned-SHA preference is verified here by fake seam
(`TestResolve_ReadsFromPinnedBaseRef`, `TestGetStagePrompt_InjectedDocument_EndToEnd`,
and the charter consumer's `TestGroomingPrompt_PinnedCommitBeatsBranchTip`).

### `Request.Repo` does not identify a repository across forges (#2234)

`forge.RepoRef` carries owner/name and nothing else, and this package resolves through
whatever `Fetcher` / `Commits` the wiring handed it. So on a deployment with more than one
file-capable forge, `acme/widgets` on one forge and `acme/widgets` on the other are
indistinguishable HERE, and a document read routed to the wrong forge returns a real,
well-formed document from the WRONG repository — worse than a refusal, because nothing in
the resolve/hash/attribute path can tell.

Selecting one forge is therefore not the control. The control is
`documentForgeOwnershipGuard` in `cmd/fishhawkd`, which refuses a repo whose
`accounts.provider` is not the selected forge before any forge call. E55's
review-conventions consumer inherits this hazard unchanged — it is a property of the
identifier, not of the charter.

### Preview convergence (E54.12 / #2804)

`GET /v0/stages/{id}/prompt-render` — the unsigned preview surface — now
resolves and injects documents through the SAME resolve/render core as the
signed `/prompt` path (`server.resolveDeclaredDocuments`), and every consumer's
fail-closed refusal runs there too. A preview and a served prompt for the same
stage therefore carry identical bytes and refuse identically.

The one divergence is ATTRIBUTION, and it is deliberate: the preview wrapper
passes `attribute=false`, so neither `Attribute` nor `RecordWithheld` is called
and no `document_injected` / `document_truncated` / `document_injection_degraded`
entry is written. See the
attribution-domain note below.

## Content-hash byte domain

**One domain, everywhere:** `Document.ContentHash` is `sha256:<hex>` over the
**RESOLVED bytes exactly as fetched** — *pre*-truncation, *pre*-neutralization.

Attribution answers *"which revision of this document constrained the agent"*,
and that question must have the same answer whether or not the document
happened to exceed the cap or to contain a forged delimiter line. What was
actually **shown** is described by the sibling fields — `OriginalBytes`,
`RenderedBytes`, `DroppedBytes`, `CapBytes`, `Truncated` — not by the hash.

**The claim is scoped to a SERVE, not to a render.** `Attribute` is called only
on the SIGNED `/prompt` path; the `/prompt-render` preview resolves and renders
the identical bytes but writes nothing (E54.12 / #2804). So the audit log
answers *"which revision constrained the run"* and NEVER *"which revision an
operator previewed"* — a preview leaves no trace, by design and as a stated
residual, because a preview constrains no agent and an unsigned read-access GET
must not append claims to an append-only log.

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
| `document_injected` | every injection | `declaration_site`, `path`, `commit`, `content_hash`, `original_bytes`, `rendered_bytes`, `truncated`, `base_source`, `injection_set_id`, `document_index`, `document_count` |
| `document_truncated` | in **addition**, when the document was cut | `path`, `commit`, `content_hash`, `cap_bytes`, `dropped_bytes`, `injection_set_id` |
| `document_injection_degraded` | once per SERVED prompt PER REASON: run-admission declarations withheld for an unrecorded base (E55.7 / #3746), and optional review conventions resolved and found absent (E55.3 / #2244); both precede any `document_injected` claim | `reason` (`run_base_commit_unrecorded` \| `optional_document_missing`), `paths`, `declaration_sites`, `document_count` |

`base_source` on `document_injected` is `run_admission` or `declaration_seam`
(the zero `BaseSource`, named explicitly so an entry never reads as
unrecorded). All three are registered in `audit.KnownCategories`, so `fishhawk_await_audit` and
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

The in-process review builds (#2797) attribute the same way, immediately before
their own `prompt.Build`, so the same over-record direction holds there: a
review-prompt build failure after attribution leaves a complete set for a
prompt no reviewer read. The #797 duplicate-dispatch skip in
`runImplementReviewsForTree` does NOT add a case — the guard runs BEFORE the
resolution, so a skipped duplicate resolves and attributes nothing. Review-build
entries share `stage_id` with the stage-prompt serves of the same stage and
carry no prompt-surface key, so the audit log alone cannot say whether a set
reached the author or a reviewer; the production-path tests in
`backend/internal/server` are the evidence that reviewers receive the
document.

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
same document revision. The same holds per review BUILD (#2797): each review
round appends one set — shared by every reviewer of that round, which all read
one prompt — stamped with the REVIEWED stage's id.

That is deliberate and it is the safer direction. The guarantee is *every
injection is attributed*; de-duplicating by content hash would trade it for
*some injections are attributed*, and the entries that would be dropped are
exactly the ones covering re-dispatches — the case where knowing what the agent
was shown matters most. A reader wanting the distinct set groups by
`content_hash`; do **not** dedupe at write time.

Note that only the **signed** `/prompt` endpoint attributes. `/prompt-render`
(the unsigned preview) injects the identical bytes since E54.12 / #2804 —
including any `WithheldNotice` — but writes NO `document_injected`,
`document_truncated` or `document_injection_degraded` entry, so a preview never
writes to the audit trail.

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
| M13 run-admission declaration handed a non-commit ref (a branch, `HEAD`, a short SHA) | refuse before any branch resolution or fetch | `TestResolve_RunAdmission_RefusesBranchRef` |
| M14 unknown `Declaration.Base` | refuse before any fetch, never read as the zero value | `TestResolve_UnknownBaseSource_Refused` |
| M15 `PinCommit` empty ref / missing branch / non-commit output / no resolver / transport error | refuse (or wrap), never a default-branch pin | `TestPinCommit` |
| M16 run recorded no admission commit | run-admission declarations withheld, never fetched; notice rendered; one `document_injection_degraded` on the served path | `TestRecordWithheld_WritesOneDegradedEntry`, `TestWithheldNotice_NamesReasonPathsAndSites`, and the serve cases in `backend/internal/server/document_injection_test.go` |
| M17 `document_injection_degraded` append fails / nil appender / empty reason | error; caller must not serve | `TestRecordWithheld_FailsClosed` |
| M18 withheld path or site forges a heading through a line separator | sanitized to U+FFFD, cannot start a line | `TestWithheldNotice_AdversarialMetadata_CannotStartALine` |
| M19 review-prompt resolution failure (#2797) | no reviewer runs, no `*_review_started`; `*_review_failed` reason `document_injection_failed: …` names path + site | `TestShipPlan_PlanReview_DocumentResolutionFailure_AdvisorySkipsReviewers`, `TestRunImplementReviews_DocumentResolutionFailure_FailsClosed`, `TestRunSupplementalReinvokeReview_DocumentResolutionFailure_FailsClosed` (in `backend/internal/server`) |
| M20 review-prompt attribution failure | no reviewer runs; no `document_injected` claim | `TestRunImplementReviews_AttributionFailure_NoReviewerRuns` |
| M21 review-prompt partial seam (declarations, no resolver) | no reviewer runs; `*_review_failed` names the misconfiguration | `TestRunPlanReviews_PartialSeamConfiguration_FailsClosed` |
| M22 reviewed stage unloadable / nil run or stage with the seam configured | no reviewer runs (never read as "nothing declared") | `TestRunPlanReviews_ReviewedStageUnloadable_FailsClosed`, `TestResolveReviewInjectedDocuments_NilInputsFailClosed` |
| M23 review-prompt failure: gating vs advisory | gating fails the stage category-B (plan: `plan_review_document_injection_failed`; implement: the existing `implement_review_rejected` branch, discriminated by the failed entry); advisory leaves the human gate authoritative | `TestRunPlanReviews_DocumentResolutionFailure_GatingFailsStage`, the gating/advisory rows of M19's implement tests |
| M24 run-admission withholding on a review prompt | notice rendered, nothing fetched, one `document_injection_degraded` for the reviewed stage | `TestRunImplementReviews_RunAdmissionNilCommit_CarriesWithheldNotice`, `TestResolveReviewDocuments_NoAdmissionCommit_Withheld` |
| M25 required review convention absent at the admission commit (E55.3 / #2244) | `review_convention_missing` names name, path, site, commit; no `document_injected` claim even for an already-resolved seam document | `TestResolveReviewDocuments_RequiredConventionMissing_NoClaim` |
| M26 optional review convention absent | withheld, `document_injection_degraded` reason `optional_document_missing` written before the injection claim; nothing rendered | `TestRecordWithheld_OptionalDocumentMissingReason`, `TestResolveReviewDocuments_ConventionResolvedReviewOnly` |
| M27 convention selected, no `DocumentResolver` (even with no seam) | fails closed, no audit entry | `TestResolveReviewDocuments_SelectionWithNilResolver_FailsClosed` |
| M28 convention selection error (unparseable spec snapshot) | fails closed, nothing fetched | `TestResolveReviewDocuments_SelectionError_FailsClosed`, `TestSelectReviewConventions_Degrades` |
| M29 review resolve phase exceeds `reviewDocumentResolveTimeout` | error, no `document_injected` | `TestResolveReviewDocuments_ResolvePhaseBounded`, `TestResolveReviewDocuments_ResolveDeadlineExceeded_FailsClosed` |
| M30 no seam, no selected convention, reviewed stage unloadable | inert: zero value, no stage read, no forge call, no audit row | `TestResolveReviewDocuments_NilSeamNoSelection_UnloadableStage_Inert` |

## Consumer contract

A consumer supplies exactly two things: a `Declaration` (path + declaration
site, plus the `Framing` it wants, plus its `Base` source) and — for a
seam-sourced declaration — the base ref to pin against; a
`BaseSourceRunAdmission` declaration is pinned to the run's recorded commit
instead. Everything
else is shared. `Declaration.Framing` rides on the declaration purely so one
seam value carries both halves of a consumer's contribution; `Resolve` ignores
it entirely — only `Render` reads it.

`TestTwoShapedConsumers_OneImplementation` drives a conventions-shaped and a
charter-shaped caller through the same `Resolver`/`Render`/`Attribute` and
asserts their outputs differ only in the caller-supplied path, framing and
body. `TestRepodocCarriesNoConsumerVocabulary` scans the package's non-test
source (comments stripped) for consumer words, so the mechanism cannot quietly
learn about conventions or charters.

## Recovering the shown text (`InjectedContent`, E55.10 / #3755)

`InjectedContent(prompt.InjectedDocument) (string, bool)` returns the
neutralized document text between the BEGIN/END delimiter lines a
`ToPromptDocument` block rendered — exactly the bytes the agent was shown — and
`("", false)` when the body carries no delimiter pair (a `WithheldNotice`, a
hand-built body, a body with text after its END line). The server verifies a
reviewer persona's `quoted_passage` against this text, never against the
framing (heading, preamble, trust note, data clause, Source line). The bracket
is unambiguous because `neutralizeBody` guarantees no delimiter LINE survives
inside the content: the first newline-bounded BEGIN is the framing's own and the
last END line is the closing one. The result is the SHOWN text, not the fetched
bytes the content hash covers: a forged-delimiter line reads as the
neutralization note and a truncated document includes its marker (pinned by
`TestInjectedContent_*`).

## Prompt placement

`prompt.Trigger.InjectedDocuments` renders at the **head of the cache-stable
prefix** of `buildPlan`, `buildPlanReview`, `buildImplement` and
`buildImplementReview` — far ahead of `PlanReviewSplitMarker` /
`ImplementReviewSplitMarker` — so a per-repo-stable document costs nothing
incremental across a stage's fix-up re-review rounds. An empty slice renders
nothing at all. See `backend/internal/prompt/README.md`.

The two REVIEW renders are populated by the three in-process review build
sites in `backend/internal/server` (#2797): `runPlanReviews` (`plan_review`),
`runImplementReviewsForTree` (`implement_review` — trace-time review, fix-up
re-review backstop, decomposed-parent consolidated review) and
`runSupplementalReinvokeReview`. Each carries the documents declared for the
stage it REVIEWS — the reviewer is constrained by what constrained the author —
and fails closed (no reviewer runs) when they cannot be resolved or attributed.

The slim fix-up prompt (`buildImplementFixup`) does **not** render injected
documents: it forks before the writer, and a fix-up pass is a targeted patch
against concerns already judged under the document. If a consumer needs the
document on that path, wire the writer into `buildImplementFixup` too.
