package refinement

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

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
// unique(draft_id, ordinal)) is recorded IMMEDIATELY after each provider File.
// A re-invoke after a mid-sequence failure resumes at the first unfiled ordinal
// and never re-files a recorded one.
//
// AT-LEAST-ONCE SEAM (documented residual): the external provider create is not
// transactional with Postgres, so a crash in the window AFTER File succeeds but
// BEFORE RecordFiledItem commits re-files that ONE item on resume. The window is
// minimized (record immediately, one item at a time) and UNIQUE(draft_id,
// ordinal) prevents double-RECORDING, but GitHub's REST issue-create API offers
// no idempotency key to close it. Concurrent duplication is a DIFFERENT hazard,
// closed hard by the per-draft WithFilingLock mutual exclusion.

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

// FilingPartialError carries a mid-sequence FileItem failure: what filed so far
// (durably recorded, so a re-invoke files exactly the remaining ordinals) and
// the ordinal whose File failed. It wraps the underlying provider error.
type FilingPartialError struct {
	Filed         []FiledResult
	FailedOrdinal int
	Err           error
}

func (e *FilingPartialError) Error() string {
	return fmt.Sprintf("refinement: filing failed at ordinal %d after filing %d item(s): %v",
		e.FailedOrdinal, len(e.Filed), e.Err)
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

// ExecuteFiling files the approved draft into repo, resuming any partial prior
// filing and never duplicating a recorded item. It holds a per-draft advisory
// lock (via r.WithFilingLock) for its whole body so two concurrent invocations
// for the same draft cannot both observe an ordinal as unfiled — the second
// blocks until the first releases, then observes the first's recorded progress
// and files nothing new.
//
// It ensures/verifies the filing session (a different pinned repo returns
// ErrFilingRepoMismatch; an already-completed session replays the recorded
// result with AlreadyCompleted=true and performs NO writes), loads recorded
// items, files the epic first if ordinal 0 is unrecorded, then each unrecorded
// child in FilingOrder — resolving depends_on ordinals through the running
// filed map — recording each success IMMEDIATELY after File returns. A FileItem
// failure returns a *FilingPartialError; the recorded rows persist.
func ExecuteFiling(ctx context.Context, draft *StoredDraft, repo string, r Repository, file FileItemFunc) (*FilingOutcome, error) {
	var outcome *FilingOutcome
	if err := r.WithFilingLock(ctx, draft.ID, func(ctx context.Context) error {
		o, err := executeFilingLocked(ctx, draft, repo, r, file)
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

func executeFilingLocked(ctx context.Context, draft *StoredDraft, repo string, r Repository, file FileItemFunc) (*FilingOutcome, error) {
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
	filed := make(map[int]int, len(recorded))
	filedURL := make(map[int]string, len(recorded))
	for _, it := range recorded {
		filed[it.Ordinal] = it.IssueNumber
		filedURL[it.Ordinal] = it.IssueURL
	}
	resumed := len(recorded) > 0

	// A completed session replays the recorded result with NO writes.
	if sess.CompletedAt != nil {
		return buildOutcome(draft, filed, filedURL, resumed, true), nil
	}

	// (3) File the epic (ordinal 0) first if unrecorded.
	if _, ok := filed[0]; !ok {
		req := FilingRequestForEpic(draft.Draft.Epic, nil)
		num, url, ferr := file(ctx, req)
		if ferr != nil {
			return nil, &FilingPartialError{Filed: sortedFiled(filed, filedURL), FailedOrdinal: 0, Err: ferr}
		}
		if _, rerr := r.RecordFiledItem(ctx, FiledItemParams{
			DraftID: draft.ID, Ordinal: 0, IssueNumber: num, IssueURL: url,
		}); rerr != nil {
			return nil, rerr
		}
		filed[0] = num
		filedURL[0] = url
	}
	// The epic number is the child {epic} title var AND their `#N` parent ref.
	epicNumberStr := strconv.Itoa(filed[0])
	epicRef := "#" + epicNumberStr

	// (4) File each unrecorded child in wave order.
	order, err := FilingOrder(draft.Draft)
	if err != nil {
		return nil, err
	}
	for _, ord := range order {
		if _, ok := filed[ord]; ok {
			continue
		}
		child := draft.Draft.Children[ord-1]
		deps := make([]string, 0, len(child.DependsOn))
		for _, dep := range child.DependsOn {
			depNum, ok := filed[dep]
			if !ok {
				// Wave order guarantees deps are filed first; a miss is an
				// internal invariant break, not user input — fail closed.
				return nil, fmt.Errorf("refinement: child ordinal %d depends on unfiled ordinal %d", ord, dep)
			}
			deps = append(deps, "#"+strconv.Itoa(depNum))
		}
		req := FilingRequestForChild(child, ord, epicNumberStr, epicRef, deps)
		num, url, ferr := file(ctx, req)
		if ferr != nil {
			return nil, &FilingPartialError{Filed: sortedFiled(filed, filedURL), FailedOrdinal: ord, Err: ferr}
		}
		if _, rerr := r.RecordFiledItem(ctx, FiledItemParams{
			DraftID: draft.ID, Ordinal: ord, IssueNumber: num, IssueURL: url,
		}); rerr != nil {
			return nil, rerr
		}
		filed[ord] = num
		filedURL[ord] = url
	}

	return buildOutcome(draft, filed, filedURL, resumed, false), nil
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
