package refinement

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ---- in-package fakes -----------------------------------------------------

// memFilingRepo is an in-memory Repository serving ONLY the filing methods the
// executor exercises (it embeds the interface so the draft/decision methods
// exist but panic if called — they never are). WithFilingLock runs fn inline;
// the unit tests are single-threaded, so the real advisory serialization is
// exercised by the server-package pgtest concurrency test instead.
type memFilingRepo struct {
	Repository
	session *FilingSession
	items   map[int]*FiledItem
}

func (m *memFilingRepo) WithFilingLock(ctx context.Context, _ uuid.UUID, fn func(context.Context) error) error {
	return fn(ctx)
}

func (m *memFilingRepo) GetFilingSession(_ context.Context, _ uuid.UUID) (*FilingSession, error) {
	if m.session == nil {
		return nil, ErrNotFound
	}
	return m.session, nil
}

func (m *memFilingRepo) CreateFilingSession(_ context.Context, p FilingSessionParams) (*FilingSession, error) {
	if m.session != nil {
		return nil, errors.New("filing session already exists")
	}
	m.session = &FilingSession{DraftID: p.DraftID, SessionID: p.SessionID, Repo: p.Repo, CreatedAt: time.Unix(0, 0)}
	return m.session, nil
}

func (m *memFilingRepo) CompleteFilingSession(_ context.Context, _ uuid.UUID) error {
	if m.session != nil && m.session.CompletedAt == nil {
		t := time.Unix(1, 0)
		m.session.CompletedAt = &t
	}
	return nil
}

func (m *memFilingRepo) RecordFiledItem(_ context.Context, p FiledItemParams) (*FiledItem, error) {
	if m.items == nil {
		m.items = map[int]*FiledItem{}
	}
	if _, ok := m.items[p.Ordinal]; ok {
		return nil, fmt.Errorf("duplicate record for ordinal %d (unique violation)", p.Ordinal)
	}
	it := &FiledItem{DraftID: p.DraftID, Ordinal: p.Ordinal, IssueNumber: p.IssueNumber, IssueURL: p.IssueURL}
	m.items[p.Ordinal] = it
	return it, nil
}

func (m *memFilingRepo) ListFiledItems(_ context.Context, _ uuid.UUID) ([]*FiledItem, error) {
	ords := make([]int, 0, len(m.items))
	for o := range m.items {
		ords = append(ords, o)
	}
	sort.Ints(ords)
	out := make([]*FiledItem, 0, len(ords))
	for _, o := range ords {
		out = append(out, m.items[o])
	}
	return out, nil
}

// scriptedFiler is a FileItem fake: it logs every request, hands out sequential
// issue numbers starting at start, and (when failAt>0) fails the failAt-th call.
type scriptedFiler struct {
	calls  []workmgmt.FilingRequest
	next   int
	failAt int
}

func newScriptedFiler(start, failAt int) *scriptedFiler {
	return &scriptedFiler{next: start, failAt: failAt}
}

func (s *scriptedFiler) file(_ context.Context, req workmgmt.FilingRequest) (int, string, error) {
	s.calls = append(s.calls, req)
	if s.failAt != 0 && len(s.calls) == s.failAt {
		return 0, "", errors.New("provider boom")
	}
	n := s.next
	s.next++
	return n, fmt.Sprintf("https://x/issues/%d", n), nil
}

func storedDraft(d EpicDraft) *StoredDraft {
	return &StoredDraft{ID: uuid.New(), SessionID: uuid.New(), Draft: d}
}

// ---- FilingOrder ----------------------------------------------------------

func TestFilingOrder_BackwardChain(t *testing.T) {
	// child2 depends on child1: wave0=[1], wave1=[2] -> order [1,2].
	d := EpicDraft{
		Epic: EpicSpec{Summary: "e", Scope: "s"},
		Children: []ChildDraft{
			validChild("one"),
			func() ChildDraft { c := validChild("two"); c.DependsOn = []int{1}; return c }(),
		},
	}
	order, err := FilingOrder(d)
	if err != nil {
		t.Fatalf("FilingOrder: %v", err)
	}
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("order = %v, want [1 2]", order)
	}
}

func TestFilingOrder_ForwardEdge(t *testing.T) {
	// child1 depends on child5 (a forward sibling edge, legal — Validate only
	// rejects dangling/cycle). Wave 0 must place 5 (and other independents)
	// BEFORE 1, which plain 1..N ordinal order cannot.
	children := make([]ChildDraft, 5)
	for i := range children {
		children[i] = validChild(fmt.Sprintf("c%d", i+1))
	}
	children[0].DependsOn = []int{5} // child 1 depends on child 5
	d := EpicDraft{Epic: EpicSpec{Summary: "e", Scope: "s"}, Children: children}

	order, err := FilingOrder(d)
	if err != nil {
		t.Fatalf("FilingOrder: %v", err)
	}
	pos := map[int]int{}
	for i, o := range order {
		pos[o] = i
	}
	if pos[5] >= pos[1] {
		t.Errorf("forward edge not honored: child 5 at %d must precede child 1 at %d (order %v)", pos[5], pos[1], order)
	}
	if len(order) != 5 {
		t.Errorf("order has %d ordinals, want 5", len(order))
	}
}

// ---- ExecuteFiling: happy path --------------------------------------------

func TestExecuteFiling_HappyPath_EpicThenChildrenWithResolvedDeps(t *testing.T) {
	// child2 depends on child1; wave order files child1 before child2, so
	// child2's depends_on ref carries child1's REAL number.
	d := EpicDraft{
		Epic: EpicSpec{Summary: "e", Scope: "s"},
		Children: []ChildDraft{
			validChild("one"),
			func() ChildDraft { c := validChild("two"); c.DependsOn = []int{1}; return c }(),
		},
	}
	repo := &memFilingRepo{}
	filer := newScriptedFiler(100, 0)
	draft := storedDraft(d)

	outcome, err := ExecuteFiling(context.Background(), draft, "kuhlman-labs/fishhawk", repo, filer.file)
	if err != nil {
		t.Fatalf("ExecuteFiling: %v", err)
	}
	// Epic filed first (number 100), children next (101, 102).
	if outcome.Epic.IssueNumber != 100 {
		t.Errorf("epic number = %d, want 100", outcome.Epic.IssueNumber)
	}
	if len(outcome.Children) != 2 || outcome.Children[0].IssueNumber != 101 || outcome.Children[1].IssueNumber != 102 {
		t.Errorf("children = %+v, want ordinals 1,2 -> 101,102", outcome.Children)
	}
	if outcome.Resumed || outcome.AlreadyCompleted {
		t.Errorf("fresh fill: Resumed=%v AlreadyCompleted=%v, want both false", outcome.Resumed, outcome.AlreadyCompleted)
	}
	// 3 File calls: epic, child1, child2.
	if len(filer.calls) != 3 {
		t.Fatalf("File calls = %d, want 3", len(filer.calls))
	}
	// Epic call is the epic type with empty ExistingNumbers (discovery runs).
	if filer.calls[0].Type != "epic" || len(filer.calls[0].ExistingNumbers) != 0 {
		t.Errorf("epic call = %+v, want type=epic and empty ExistingNumbers", filer.calls[0])
	}
	// child2 (the last call) carries the epic number title var (100), the `#100`
	// parent ref, and the resolved `#101` depends_on ref.
	c2 := filer.calls[2]
	if c2.TitleVars["epic"] != "100" || c2.TitleVars["n"] != "2" {
		t.Errorf("child2 title vars = %v, want epic=100 n=2", c2.TitleVars)
	}
	if c2.Relations.ParentEpic != "#100" {
		t.Errorf("child2 parent epic = %q, want #100", c2.Relations.ParentEpic)
	}
	if len(c2.Relations.DependsOn) != 1 || c2.Relations.DependsOn[0] != "#101" {
		t.Errorf("child2 depends_on = %v, want [#101] (child1's real number)", c2.Relations.DependsOn)
	}
	// Records: epic + 2 children.
	if len(repo.items) != 3 {
		t.Errorf("recorded items = %d, want 3", len(repo.items))
	}
}

// ---- ExecuteFiling: partial failure + resume ------------------------------

func TestExecuteFiling_PartialFailure_ThenResumesExactly(t *testing.T) {
	// 6 independent children. Fail on the 5th File call (epic, c1, c2, c3 ok;
	// c4 fails). Recorded = epic + children 1-3; re-invoke files EXACTLY 4-6.
	children := make([]ChildDraft, 6)
	for i := range children {
		children[i] = validChild(fmt.Sprintf("c%d", i+1))
	}
	d := EpicDraft{Epic: EpicSpec{Summary: "e", Scope: "s"}, Children: children}
	repo := &memFilingRepo{}
	draft := storedDraft(d)

	filer1 := newScriptedFiler(200, 5) // fail on the 5th call
	_, err := ExecuteFiling(context.Background(), draft, "o/r", repo, filer1.file)
	var partial *FilingPartialError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want *FilingPartialError", err)
	}
	if partial.FailedOrdinal != 4 {
		t.Errorf("FailedOrdinal = %d, want 4", partial.FailedOrdinal)
	}
	// Recorded ordinals: 0 (epic), 1, 2, 3.
	if len(repo.items) != 4 {
		t.Fatalf("recorded = %d, want 4 (epic + children 1-3)", len(repo.items))
	}
	for _, o := range []int{0, 1, 2, 3} {
		if _, ok := repo.items[o]; !ok {
			t.Errorf("ordinal %d not recorded", o)
		}
	}

	// Re-invoke: files EXACTLY ordinals 4,5,6 (3 calls), no duplicates.
	filer2 := newScriptedFiler(300, 0)
	outcome, err := ExecuteFiling(context.Background(), draft, "o/r", repo, filer2.file)
	if err != nil {
		t.Fatalf("resume ExecuteFiling: %v", err)
	}
	if len(filer2.calls) != 3 {
		t.Fatalf("resume File calls = %d, want 3 (ordinals 4,5,6)", len(filer2.calls))
	}
	gotN := map[string]bool{}
	for _, c := range filer2.calls {
		gotN[c.TitleVars["n"]] = true
	}
	for _, n := range []string{"4", "5", "6"} {
		if !gotN[n] {
			t.Errorf("resume did not file child n=%s (filed %v)", n, gotN)
		}
	}
	if !outcome.Resumed {
		t.Error("resume outcome.Resumed = false, want true")
	}
	if len(repo.items) != 7 {
		t.Errorf("final recorded = %d, want 7 (epic + 6 children, zero duplicates)", len(repo.items))
	}
}

// TestExecuteFiling_RecordAfterFileOrdering: a File success is recorded even
// when the NEXT File fails — the done-means resume anchor.
func TestExecuteFiling_RecordAfterFileOrdering(t *testing.T) {
	d := EpicDraft{
		Epic:     EpicSpec{Summary: "e", Scope: "s"},
		Children: []ChildDraft{validChild("one"), validChild("two")},
	}
	repo := &memFilingRepo{}
	// epic (call1) ok, child1 (call2) ok, child2 (call3) fails.
	filer := newScriptedFiler(10, 3)
	_, err := ExecuteFiling(context.Background(), storedDraft(d), "o/r", repo, filer.file)
	var partial *FilingPartialError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want *FilingPartialError", err)
	}
	// child1 (ordinal 1) was recorded despite child2 failing right after.
	if _, ok := repo.items[1]; !ok {
		t.Error("ordinal 1 not recorded although its File succeeded before the next failed")
	}
	if _, ok := repo.items[2]; ok {
		t.Error("ordinal 2 recorded although its File failed")
	}
}

// ---- ExecuteFiling: repo mismatch -----------------------------------------

func TestExecuteFiling_RepoMismatch_NoFileCalls(t *testing.T) {
	d := EpicDraft{Epic: EpicSpec{Summary: "e", Scope: "s"}, Children: []ChildDraft{validChild("one")}}
	draft := storedDraft(d)
	repo := &memFilingRepo{session: &FilingSession{DraftID: draft.ID, SessionID: draft.SessionID, Repo: "a/b"}}
	filer := newScriptedFiler(1, 0)

	_, err := ExecuteFiling(context.Background(), draft, "c/d", repo, filer.file)
	if !errors.Is(err, ErrFilingRepoMismatch) {
		t.Fatalf("err = %v, want ErrFilingRepoMismatch", err)
	}
	if len(filer.calls) != 0 {
		t.Errorf("File calls = %d, want 0 on repo mismatch", len(filer.calls))
	}
}

// ---- ExecuteFiling: completed session -------------------------------------

func TestExecuteFiling_CompletedSession_NoWritesNoFiles(t *testing.T) {
	d := EpicDraft{Epic: EpicSpec{Summary: "e", Scope: "s"}, Children: []ChildDraft{validChild("one")}}
	draft := storedDraft(d)
	done := time.Unix(5, 0)
	repo := &memFilingRepo{
		session: &FilingSession{DraftID: draft.ID, SessionID: draft.SessionID, Repo: "o/r", CompletedAt: &done},
		items: map[int]*FiledItem{
			0: {DraftID: draft.ID, Ordinal: 0, IssueNumber: 50, IssueURL: "u0"},
			1: {DraftID: draft.ID, Ordinal: 1, IssueNumber: 51, IssueURL: "u1"},
		},
	}
	filer := newScriptedFiler(1, 0)

	outcome, err := ExecuteFiling(context.Background(), draft, "o/r", repo, filer.file)
	if err != nil {
		t.Fatalf("ExecuteFiling: %v", err)
	}
	if !outcome.AlreadyCompleted {
		t.Error("AlreadyCompleted = false, want true on a completed session")
	}
	if len(filer.calls) != 0 {
		t.Errorf("File calls = %d, want 0 on a completed session", len(filer.calls))
	}
	if outcome.Epic.IssueNumber != 50 || len(outcome.Children) != 1 || outcome.Children[0].IssueNumber != 51 {
		t.Errorf("replayed outcome = %+v, want epic 50 + child 51", outcome)
	}
}

// ---- VerifyFiledEpic ------------------------------------------------------

type fakeEpicQuerier struct {
	res *workmgmt.EpicChildrenResult
	err error
}

func (f fakeEpicQuerier) EpicChildren(_ context.Context, _ workmgmt.EpicChildrenRequest) (*workmgmt.EpicChildrenResult, error) {
	return f.res, f.err
}

func TestVerifyFiledEpic_HappyRoundTrip(t *testing.T) {
	filed := map[int]int{0: 100, 1: 101, 2: 102}
	q := fakeEpicQuerier{res: &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 101}, {Number: 102}},
		Edges:    []workmgmt.DependsEdge{{From: 102, To: 101}},
	}}
	if err := VerifyFiledEpic(context.Background(), q, workmgmt.Target{}, 100, filed); err != nil {
		t.Errorf("VerifyFiledEpic: %v", err)
	}
}

func TestVerifyFiledEpic_ChildSetMismatch(t *testing.T) {
	filed := map[int]int{0: 100, 1: 101, 2: 102}
	// EpicChildren reports only one of the two recorded children.
	q := fakeEpicQuerier{res: &workmgmt.EpicChildrenResult{Children: []workmgmt.EpicChild{{Number: 101}}}}
	if err := VerifyFiledEpic(context.Background(), q, workmgmt.Target{}, 100, filed); err == nil {
		t.Error("VerifyFiledEpic accepted a child-set mismatch, want error")
	}
}

func TestVerifyFiledEpic_AssembleFailsClosedOnDroppedEdge(t *testing.T) {
	filed := map[int]int{0: 100, 1: 101, 2: 102}
	// A dropped edge (a depends_on target that is not a fellow child) makes
	// campaign.Assemble fail closed.
	q := fakeEpicQuerier{res: &workmgmt.EpicChildrenResult{
		Children:     []workmgmt.EpicChild{{Number: 101}, {Number: 102}},
		DroppedEdges: []workmgmt.DependsEdge{{From: 101, To: 9999}},
	}}
	if err := VerifyFiledEpic(context.Background(), q, workmgmt.Target{}, 100, filed); err == nil {
		t.Error("VerifyFiledEpic accepted a dropped-edge result, want error")
	}
}

func TestVerifyFiledEpic_QueryError(t *testing.T) {
	q := fakeEpicQuerier{err: errors.New("boom")}
	if err := VerifyFiledEpic(context.Background(), q, workmgmt.Target{}, 100, map[int]int{0: 100}); err == nil {
		t.Error("VerifyFiledEpic swallowed a query error, want error")
	}
}
