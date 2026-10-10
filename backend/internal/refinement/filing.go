package refinement

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// The E34.3 filing executor (ADR-052 filing half, #1594) turns an approved,
// hash-pinned refinement draft into real tracker items over the EXISTING
// provider pipeline: the epic first, then the children in dependency (wave)
// order, each routed through the same FileItem seam the server wires to
// applyAndFileWorkItem. Parent links, sub-issue attachment, and depends_on
// markers come free from the provider's File; sibling depends_on ordinals
// resolve to filed #numbers as filing proceeds.
//
// Idempotent partial-failure recovery: a per-draft filing session row pins the
// target repo, and one durable row per filed item (ordinal -> issue number,
// unique(draft_id, ordinal)) is recorded WRITE-AHEAD (#4153): the provider
// fires the request's OnCreated hook immediately after the forge create and
// BEFORE its board placement / sub-issue linking, and the hook records the row
// on a cancellation-detached, bounded context (filedItemRecordTimeout). A
// filing cancelled or budget-expired mid-enrichment therefore leaves the
// created issue durable, and a re-invoke resumes at the first unfiled ordinal
// and never re-files a recorded one. A provider that never fires the hook
// (jira) is recorded once File returns, on the same detached context.
//
// Every filed body carries a deterministic hidden idempotency key
// (ItemIdempotencyKey over draft id + ordinal, the #2064 primitive). On resume
// the executor reads the recorded epic's children ONCE and ADOPTS an
// unrecorded ordinal whose key is already on a linked child (record, no
// create); and after every child is recorded it runs a LINK PASS that links
// each recorded child absent from the epic's children through the optional
// workmgmt.EpicLinker capability. The link step is idempotent (an already
// linked child is never re-linked) and resumable per child.
//
// AT-LEAST-ONCE SEAM (documented residual, narrowed by #4153): the external
// provider create is not transactional with Postgres, so a create whose
// RESPONSE was lost — the forge created the issue but the client saw an error,
// so no hook fired and nothing was recorded — re-files that ONE item on resume.
// For a child the duplicate is avoided anyway when the lost create still got
// linked (the resume adopts it by key); the residual is the epic, or a child
// that also never got linked. UNIQUE(draft_id, ordinal) prevents
// double-RECORDING, and GitHub's REST issue-create API offers no idempotency
// key to close the rest. Concurrent duplication is a DIFFERENT hazard, closed
// hard by the per-draft WithFilingLock mutual exclusion.

// ErrFilingRepoMismatch is returned when a re-invoke names a different target
// repo than the filing session pinned at first invoke. Filing fails closed
// rather than filing the same draft into two repos.
var ErrFilingRepoMismatch = errors.New("refinement: filing session pins a different target repo")

// FileItemFunc files one fully-built FilingRequest and returns the created
// issue number + url. The server wires applyAndFileWorkItem into it, so draft
// children ride exactly the hand-filed conventions/provider pipeline (label
// completeness #1616, epic-number discovery #1269, board placement, sub-issue
// linking). Kept a narrow func seam so the executor is unit-testable against a
// scripted fake with no provider wiring.
type FileItemFunc func(ctx context.Context, req workmgmt.FilingRequest) (number int, url string, err error)

// FinalizeFunc runs the session-closing side effects (round-trip verification,
// the refinement_filing_completed audit append, and the completed_at flip) for
// a fresh/resumed FULL fill, INSIDE the per-draft advisory lock and just before
// it releases. Because it is serialized with filing under the same lock, a
// concurrent second invocation that was blocked on the lock enters only after
// this returns and CompleteFilingSession has flipped completed_at — so it
// observes AlreadyCompleted=true and appends NO second completion audit. It is
// NOT called for an already-completed replay (which performs no writes).
// Returning an error aborts before completion is durable (the lock releases, the
// items stay durable, a re-invoke retries) and propagates out of ExecuteFiling
// unchanged. A nil FinalizeFunc skips finalization (the executor's unit tests,
// which assert filing/recording only).
type FinalizeFunc func(ctx context.Context, outcome *FilingOutcome) error

// FiledResult is one filed item's ordinal -> (number, url) mapping in a
// FilingOutcome (ordinal 0 is the epic, 1..N the children).
type FiledResult struct {
	Ordinal     int
	IssueNumber int
	IssueURL    string
}

// FilingOutcome is what ExecuteFiling returns on a fully-filed draft: the epic,
// the children in draft-ordinal order, and whether this invocation resumed a
// partially-filed session (Resumed) or was a no-op replay of an
// already-completed one (AlreadyCompleted, no writes performed).
type FilingOutcome struct {
	Epic             FiledResult
	Children         []FiledResult
	Resumed          bool
	AlreadyCompleted bool
}

// FiledMap returns the ordinal -> issue-number map (including ordinal 0 for the
// epic), the input VerifyFiledEpic asserts against.
func (o *FilingOutcome) FiledMap() map[int]int {
	m := make(map[int]int, len(o.Children)+1)
	m[0] = o.Epic.IssueNumber
	for _, c := range o.Children {
		m[c.Ordinal] = c.IssueNumber
	}
	return m
}

// The FilingPartialError steps: which part of the filing sequence stopped.
const (
	// FilingStepCreate: an item's create (or the per-item context check ahead
	// of it) failed. FailedOrdinal names the item; when its create landed
	// before the failure (the write-ahead hook recorded it), it is ALREADY in
	// Filed and a re-invoke does not re-create it.
	FilingStepCreate = "create"
	// FilingStepReconcile: the resume could not read the recorded epic's
	// children, so it stopped before creating anything rather than proceed
	// blind (the duplicate path). FailedOrdinal names the first unrecorded
	// child it was about to reconcile.
	FilingStepReconcile = "reconcile"
	// FilingStepLink: the link pass failed. FailedOrdinal names the recorded
	// child whose link failed, or 0 (the epic) when the epic's children could
	// not be read for the pass. Every item is filed; finalize did not run.
	FilingStepLink = "link"
)

// FilingPartialError carries a mid-sequence failure: what filed so far
// (durably recorded, so a re-invoke files exactly the remaining ordinals), the
// ordinal the sequence stopped at, and the Step that stopped (FilingStepCreate,
// FilingStepReconcile or FilingStepLink). It wraps the underlying error.
type FilingPartialError struct {
	Filed         []FiledResult
	FailedOrdinal int
	Step          string
	Err           error
}

func (e *FilingPartialError) Error() string {
	return fmt.Sprintf("refinement: filing failed at ordinal %d (step %s) after filing %d item(s): %v",
		e.FailedOrdinal, e.Step, len(e.Filed), e.Err)
}

func (e *FilingPartialError) Unwrap() error { return e.Err }

// FilingOrder flattens the draft's wave DAG into a deterministic filing order:
// wave by wave, ascending ordinal within a wave. Filing in wave (topological)
// order — not raw 1..N — guarantees every depends_on target is already filed
// (its real #number known) before any dependent files, including a legal
// forward sibling edge (child 2 depends on child 5) that plain ordinal order
// cannot resolve. A dangling/cyclic graph surfaces the wrapped
// campaign.ErrDanglingDependency / campaign.ErrCycle via Waves().
func FilingOrder(draft EpicDraft) ([]int, error) {
	waves, err := draft.Waves()
	if err != nil {
		return nil, err
	}
	order := make([]int, 0, len(draft.Children))
	for _, wave := range waves {
		w := append([]int(nil), wave...)
		sort.Ints(w)
		order = append(order, w...)
	}
	return order, nil
}

// filedItemRecordTimeout bounds each write-ahead ledger insert. The insert runs
// on a context DETACHED from the filing's cancellation (context.WithoutCancel),
// because the filing context is exactly what an interrupted or budget-expired
// filing has just cancelled — recording on it would lose the row for an issue
// the forge already created. A package var so tests can shrink it.
var filedItemRecordTimeout = 10 * time.Second

// ItemIdempotencyKey is the deterministic idempotency key stamped into the body
// of the draft item at ordinal (0 the epic, 1..N the children): the #2064
// workmgmt.MintIdempotencyKey over the draft id and the ordinal. It is a pure
// function, so a resume in another process re-derives the byte-identical key
// and can adopt a child it created but never recorded.
func ItemIdempotencyKey(draftID uuid.UUID, ordinal int) string {
	return workmgmt.MintIdempotencyKey("refinement_item", draftID.String(), strconv.Itoa(ordinal))
}

// ChildrenFunc lists the children currently linked under the filed epic
// (in production the provider's EpicChildrenQuerier). Body and URL are read:
// Body for the idempotency-key adoption, Number for the link pass.
type ChildrenFunc func(ctx context.Context, epicNumber int) ([]workmgmt.EpicChild, error)

// LinkFunc links the already-filed childNumber under epicNumber (in production
// the provider's EpicLinker).
type LinkFunc func(ctx context.Context, epicNumber, childNumber int) error

// FilingDeps are ExecuteFilingWith's seams. File is required. Finalize is
// optional (nil skips finalization). Children and Link are optional
// capabilities: a nil Children skips resume adoption AND the link pass; a nil
// Link skips the link pass.
type FilingDeps struct {
	File     FileItemFunc
	Finalize FinalizeFunc
	Children ChildrenFunc
	Link     LinkFunc
}

// ExecuteFiling is ExecuteFilingWith with only the File and Finalize seams —
// no resume adoption and no link pass. Kept source-compatible for callers that
// have no epic-children / link capability to offer.
func ExecuteFiling(ctx context.Context, draft *StoredDraft, repo string, r Repository, file FileItemFunc, finalize FinalizeFunc) (*FilingOutcome, error) {
	return ExecuteFilingWith(ctx, draft, repo, r, FilingDeps{File: file, Finalize: finalize})
}

// ExecuteFilingWith files the approved draft into repo, resuming any partial
// prior filing and never duplicating a recorded item. It holds a per-draft
// advisory lock (via r.WithFilingLock) for its whole body so two concurrent
// invocations for the same draft cannot both observe an ordinal as unfiled —
// the second blocks until the first releases, then observes the first's
// recorded progress and files nothing new.
//
// It ensures/verifies the filing session (a different pinned repo returns
// ErrFilingRepoMismatch; an already-completed session replays the recorded
// result with AlreadyCompleted=true and performs NO writes), loads recorded
// items, files the epic first if ordinal 0 is unrecorded, then each unrecorded
// child in FilingOrder — resolving depends_on ordinals through the running
// filed map. Each item is recorded WRITE-AHEAD from the provider's OnCreated
// hook (fallback: right after File returns) and carries ItemIdempotencyKey.
// The context is checked before every item, so an expired budget stops before
// the next create rather than dialing the forge on a dead context.
//
// On a resume (the epic was recorded at entry) with deps.Children set, an
// unrecorded child whose key is already on one of the epic's children is
// adopted instead of created; a Children error fails closed (Step reconcile,
// nothing created). With Children and Link both set, a link pass then links
// every recorded child absent from the epic's children (Step link on failure,
// finalize not run). Any failure returns a *FilingPartialError (or the
// underlying error for an infrastructure failure); the recorded rows persist.
//
// On a fresh/resumed FULL fill it invokes deps.Finalize (the session-closing
// side effects) BEFORE releasing the lock, so verification, the completion
// audit, and the completed_at flip are serialized with filing against the same
// draft — a concurrent second caller cannot slip between "all items recorded"
// and "completed_at set" and append a duplicate completion audit. Finalize is
// not called for an already-completed replay; a nil Finalize skips it.
func ExecuteFilingWith(ctx context.Context, draft *StoredDraft, repo string, r Repository, deps FilingDeps) (*FilingOutcome, error) {
	var outcome *FilingOutcome
	if err := r.WithFilingLock(ctx, draft.ID, func(ctx context.Context) error {
		o, err := executeFilingLocked(ctx, draft, repo, r, deps)
		if err != nil {
			return err
		}
		outcome = o
		return nil
	}); err != nil {
		return nil, err
	}
	return outcome, nil
}

// filingRun is one locked executor pass: the draft, the ledger, the seams, and
// the running ordinal -> (number, url) map every step reads and extends.
type filingRun struct {
	draft    *StoredDraft
	r        Repository
	deps     FilingDeps
	filed    map[int]int
	filedURL map[int]string
}

func (f *filingRun) partial(ord int, step string, err error) *FilingPartialError {
	return &FilingPartialError{Filed: sortedFiled(f.filed, f.filedURL), FailedOrdinal: ord, Step: step, Err: err}
}

// record writes one ledger row on a context detached from ctx's cancellation
// and bounded by filedItemRecordTimeout, then extends the running map.
func (f *filingRun) record(ctx context.Context, ord, num int, url string) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), filedItemRecordTimeout)
	defer cancel()
	if _, err := f.r.RecordFiledItem(rctx, FiledItemParams{
		DraftID: f.draft.ID, Ordinal: ord, IssueNumber: num, IssueURL: url,
	}); err != nil {
		return err
	}
	f.filed[ord] = num
	f.filedURL[ord] = url
	return nil
}

// fileItem files one item with its idempotency key stamped and the write-ahead
// hook installed. The hook records the created issue the moment the provider
// reports it, so it is durable even when File then fails (an interrupted
// link/board step). When the hook did not record — the provider never fired it,
// or its insert failed — the item is recorded once more after a successful
// File; a second failure returns an error naming the created issue (its body
// carries its key, so a resume adopts it once it is linked).
func (f *filingRun) fileItem(ctx context.Context, ord int, req workmgmt.FilingRequest) error {
	req.IdempotencyKey = ItemIdempotencyKey(f.draft.ID, ord)
	var (
		fired             bool
		recorded          bool
		hookNum           int
		hookRecordFailure error
	)
	req.OnCreated = func(hctx context.Context, item workmgmt.CreatedItem) {
		if fired {
			return // the contract is exactly once; never double-record
		}
		fired = true
		hookNum = item.Number
		if err := f.record(hctx, ord, item.Number, item.URL); err != nil {
			hookRecordFailure = err
			return
		}
		recorded = true
	}
	num, url, ferr := f.deps.File(ctx, req)
	if ferr != nil {
		if hookRecordFailure != nil {
			ferr = fmt.Errorf("%w (issue #%d was created but its ledger row failed: %v)", ferr, hookNum, hookRecordFailure)
		}
		return f.partial(ord, FilingStepCreate, ferr)
	}
	if recorded {
		return nil
	}
	if err := f.record(ctx, ord, num, url); err != nil {
		return fmt.Errorf("refinement: issue #%d (%s) was created for ordinal %d but could not be recorded: %w", num, url, ord, err)
	}
	return nil
}

func executeFilingLocked(ctx context.Context, draft *StoredDraft, repo string, r Repository, deps FilingDeps) (*FilingOutcome, error) {
	// (1) Ensure the filing session, pinning the target repo at first invoke.
	sess, err := r.GetFilingSession(ctx, draft.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		if sess, err = r.CreateFilingSession(ctx, FilingSessionParams{
			DraftID:   draft.ID,
			SessionID: draft.SessionID,
			Repo:      repo,
		}); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if sess.Repo != repo {
			return nil, fmt.Errorf("%w: session pins %q, requested %q", ErrFilingRepoMismatch, sess.Repo, repo)
		}
	}

	// (2) Load recorded items into the running ordinal -> (number, url) map.
	recorded, err := r.ListFiledItems(ctx, draft.ID)
	if err != nil {
		return nil, err
	}
	f := &filingRun{
		draft:    draft,
		r:        r,
		deps:     deps,
		filed:    make(map[int]int, len(recorded)),
		filedURL: make(map[int]string, len(recorded)),
	}
	for _, it := range recorded {
		f.filed[it.Ordinal] = it.IssueNumber
		f.filedURL[it.Ordinal] = it.IssueURL
	}
	resumed := len(recorded) > 0

	// A completed session replays the recorded result with NO writes.
	if sess.CompletedAt != nil {
		return buildOutcome(draft, f.filed, f.filedURL, resumed, true), nil
	}

	// (3) File the epic (ordinal 0) first if unrecorded. Only an epic recorded
	// at ENTRY can have children from an earlier invocation to adopt.
	_, epicRecordedAtEntry := f.filed[0]
	if !epicRecordedAtEntry {
		if err := ctx.Err(); err != nil {
			return nil, f.partial(0, FilingStepCreate, err)
		}
		if err := f.fileItem(ctx, 0, FilingRequestForEpic(draft.Draft.Epic, nil)); err != nil {
			return nil, err
		}
	}
	// The epic's ISSUE number is ONLY the children's `#N` parent-epic RELATION
	// ref (and their depends_on refs). It is DELIBERATELY not the {epic} title
	// var: the {epic} placeholder must carry the epic's DISCOVERED ordinal (the
	// digits in its own [E<n>] title), which the server-side deriveEpicTitleVar
	// derives downstream from the parent-epic relation — so the executor passes
	// "" for the title var and lets that derivation run (#1644).
	epicNum := f.filed[0]
	epicRef := "#" + strconv.Itoa(epicNum)

	// (4) File each unrecorded child in wave order.
	order, err := FilingOrder(draft.Draft)
	if err != nil {
		return nil, err
	}
	var (
		existing        []workmgmt.EpicChild
		existingFetched bool
	)
	for _, ord := range order {
		if _, ok := f.filed[ord]; ok {
			continue
		}
		// Stop before the next create on an expired budget / cancelled filing
		// rather than dialing the forge on a dead context.
		if err := ctx.Err(); err != nil {
			return nil, f.partial(ord, FilingStepCreate, err)
		}
		// Resume reconcile: read the recorded epic's children once, lazily,
		// and adopt an unrecorded child already filed (and linked) under it.
		// A read error fails CLOSED — proceeding blind is the duplicate path.
		if epicRecordedAtEntry && deps.Children != nil {
			if !existingFetched {
				kids, cerr := deps.Children(ctx, epicNum)
				if cerr != nil {
					return nil, f.partial(ord, FilingStepReconcile, fmt.Errorf("read epic #%d children: %w", epicNum, cerr))
				}
				existing, existingFetched = kids, true
			}
			if c, ok := childWithKey(existing, ItemIdempotencyKey(draft.ID, ord)); ok {
				if err := f.record(ctx, ord, c.Number, c.URL); err != nil {
					return nil, fmt.Errorf("refinement: adopt existing issue #%d for ordinal %d: %w", c.Number, ord, err)
				}
				continue
			}
		}
		child := draft.Draft.Children[ord-1]
		depRefs := make([]string, 0, len(child.DependsOn))
		for _, dep := range child.DependsOn {
			depNum, ok := f.filed[dep]
			if !ok {
				// Wave order guarantees deps are filed first; a miss is an
				// internal invariant break, not user input — fail closed.
				return nil, fmt.Errorf("refinement: child ordinal %d depends on unfiled ordinal %d", ord, dep)
			}
			depRefs = append(depRefs, "#"+strconv.Itoa(depNum))
		}
		if err := f.fileItem(ctx, ord, FilingRequestForChild(child, ord, "", epicRef, depRefs)); err != nil {
			return nil, err
		}
	}

	// (5) Link pass: link every recorded child absent from the epic's children
	// (a create that landed but whose best-effort link did not, or was cut off).
	// Children already linked are never re-linked, which keeps the step
	// idempotent. A failure stops before finalize; a re-invoke re-runs it.
	if deps.Children != nil && deps.Link != nil {
		if err := f.linkPass(ctx, len(draft.Draft.Children)); err != nil {
			return nil, err
		}
	}

	// (6) A fresh/resumed full fill: run the session-closing side effects (verify
	// + completion audit + completed_at flip) while STILL under the lock, so a
	// concurrent second caller observes completed_at set on entry and never
	// appends a duplicate completion audit.
	outcome := buildOutcome(draft, f.filed, f.filedURL, resumed, false)
	if deps.Finalize != nil {
		if err := deps.Finalize(ctx, outcome); err != nil {
			return nil, err
		}
	}
	return outcome, nil
}

// linkPass re-reads the epic's children and links each recorded child (ordinals
// 1..n, ascending) that is absent from that set.
func (f *filingRun) linkPass(ctx context.Context, n int) error {
	epicNum := f.filed[0]
	kids, err := f.deps.Children(ctx, epicNum)
	if err != nil {
		return f.partial(0, FilingStepLink, fmt.Errorf("read epic #%d children: %w", epicNum, err))
	}
	linked := make(map[int]bool, len(kids))
	for _, c := range kids {
		linked[c.Number] = true
	}
	for ord := 1; ord <= n; ord++ {
		num := f.filed[ord]
		if linked[num] {
			continue
		}
		if err := f.deps.Link(ctx, epicNum, num); err != nil {
			return f.partial(ord, FilingStepLink, fmt.Errorf("link #%d under epic #%d: %w", num, epicNum, err))
		}
	}
	return nil
}

// childWithKey returns the child whose body carries key as a whole-line hidden
// marker (workmgmt.BodyHasIdempotencyKey).
func childWithKey(children []workmgmt.EpicChild, key string) (workmgmt.EpicChild, bool) {
	for _, c := range children {
		if workmgmt.BodyHasIdempotencyKey(c.Body, key) {
			return c, true
		}
	}
	return workmgmt.EpicChild{}, false
}

// buildOutcome assembles the FilingOutcome from the running filed map, with the
// children in draft-ordinal order (1..N) for a stable response.
func buildOutcome(draft *StoredDraft, filed map[int]int, filedURL map[int]string, resumed, completed bool) *FilingOutcome {
	o := &FilingOutcome{
		Epic:             FiledResult{Ordinal: 0, IssueNumber: filed[0], IssueURL: filedURL[0]},
		Resumed:          resumed,
		AlreadyCompleted: completed,
		Children:         make([]FiledResult, 0, len(draft.Draft.Children)),
	}
	for i := range draft.Draft.Children {
		ord := i + 1
		o.Children = append(o.Children, FiledResult{Ordinal: ord, IssueNumber: filed[ord], IssueURL: filedURL[ord]})
	}
	return o
}

// sortedFiled renders the running filed map as an ordinal-ascending slice, for
// the FilingPartialError's filed-so-far report.
func sortedFiled(filed map[int]int, filedURL map[int]string) []FiledResult {
	ords := make([]int, 0, len(filed))
	for o := range filed {
		ords = append(ords, o)
	}
	sort.Ints(ords)
	out := make([]FiledResult, 0, len(ords))
	for _, o := range ords {
		out = append(out, FiledResult{Ordinal: o, IssueNumber: filed[o], IssueURL: filedURL[o]})
	}
	return out
}

// VerifyFiledEpic asserts the filed epic round-trips: it queries the provider's
// EpicChildren for the filed epic, checks the returned child-number set matches
// the recorded child numbers (ordinals 1..N in filed), then runs
// campaign.Assemble over the result. Assemble already fails closed on
// DroppedEdges (a dangling depends_on), a dangling non-child edge, and a cycle,
// so the zero-DroppedEdges round-trip assertion reuses the exact filing-time
// semantics. A mismatch or an Assemble failure returns an error; the caller
// surfaces it as a verification failure (the filed items are durable, so a
// re-invoke skips straight back to re-verification).
func VerifyFiledEpic(ctx context.Context, querier workmgmt.EpicChildrenQuerier, target workmgmt.Target, epicNumber int, filed map[int]int) error {
	res, err := querier.EpicChildren(ctx, workmgmt.EpicChildrenRequest{
		Target: target,
		Epic:   "#" + strconv.Itoa(epicNumber),
	})
	if err != nil {
		return fmt.Errorf("refinement: epic #%d children round-trip: %w", epicNumber, err)
	}
	want := make(map[int]bool)
	for ord, num := range filed {
		if ord == 0 {
			continue
		}
		want[num] = true
	}
	got := make(map[int]bool, len(res.Children))
	for _, c := range res.Children {
		got[c.Number] = true
	}
	if len(want) != len(got) {
		return fmt.Errorf("refinement: filed epic #%d reports %d children, recorded %d", epicNumber, len(got), len(want))
	}
	for n := range want {
		if !got[n] {
			return fmt.Errorf("refinement: recorded child #%d is missing from filed epic #%d children", n, epicNumber)
		}
	}
	if _, err := campaign.Assemble("issue:"+strconv.Itoa(epicNumber), res); err != nil {
		return fmt.Errorf("refinement: filed epic #%d failed campaign assembly: %w", epicNumber, err)
	}
	return nil
}
