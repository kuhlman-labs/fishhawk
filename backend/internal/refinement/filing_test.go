package refinement

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	// ctxRespecting makes RecordFiledItem fail with ctx.Err() on a done ctx,
	// as the real Postgres insert does; the default stays ctx-agnostic so the
	// pre-#4153 tests are unaffected.
	ctxRespecting bool
	// failRecord makes RecordFiledItem fail for the named ordinals.
	failRecord map[int]error
	// recordCalls counts RecordFiledItem calls per ordinal (successful or not).
	recordCalls map[int]int
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

func (m *memFilingRepo) RecordFiledItem(ctx context.Context, p FiledItemParams) (*FiledItem, error) {
	if m.recordCalls == nil {
		m.recordCalls = map[int]int{}
	}
	m.recordCalls[p.Ordinal]++
	if m.ctxRespecting && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := m.failRecord[p.Ordinal]; err != nil {
		return nil, err
	}
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

	outcome, err := ExecuteFiling(context.Background(), draft, "kuhlman-labs/fishhawk", repo, filer.file, nil)
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
	// child2 (the last call) carries n=2 but NO {epic} title var: the executor
	// leaves it unset so the server-side deriveEpicTitleVar derives the epic's
	// DISCOVERED ordinal downstream (#1644); the epic issue number is only the
	// `#100` parent ref, never the title var. The scripted fake does NOT run
	// deriveEpicTitleVar (that is the server layer), so the "epic" key is absent.
	c2 := filer.calls[2]
	if _, ok := c2.TitleVars["epic"]; ok {
		t.Errorf("child2 title vars = %v, want NO epic key (derived downstream, not injected)", c2.TitleVars)
	}
	if c2.TitleVars["n"] != "2" {
		t.Errorf("child2 title vars = %v, want n=2", c2.TitleVars)
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
	_, err := ExecuteFiling(context.Background(), draft, "o/r", repo, filer1.file, nil)
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
	outcome, err := ExecuteFiling(context.Background(), draft, "o/r", repo, filer2.file, nil)
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

// TestExecuteFiling_RecordOrdering_WriteAheadBeforePostCreate: each item is
// recorded from the provider's OnCreated hook, so the ledger row is ALREADY
// durable when the provider runs its post-create step (#4153) — and a File
// success is still recorded even when the NEXT File fails (the resume anchor).
func TestExecuteFiling_RecordOrdering_WriteAheadBeforePostCreate(t *testing.T) {
	d := EpicDraft{
		Epic:     EpicSpec{Summary: "e", Scope: "s"},
		Children: []ChildDraft{validChild("one"), validChild("two")},
	}
	draft := storedDraft(d)
	repo := &memFilingRepo{}
	tr := newTrackerFiler(10)
	var observed []string
	tr.afterCreate = func(_ context.Context, n int, req workmgmt.FilingRequest) {
		ord := tr.ordinalOf(draft, req)
		if it, ok := repo.items[ord]; ok && it.IssueNumber == n {
			observed = append(observed, fmt.Sprintf("ord%d:recorded-before-post-create", ord))
		} else {
			observed = append(observed, fmt.Sprintf("ord%d:NOT-recorded-before-post-create", ord))
		}
	}
	tr.failLinkFor = map[string]error{ItemIdempotencyKey(draft.ID, 2): errors.New("link boom")}

	_, err := ExecuteFiling(context.Background(), draft, "o/r", repo, tr.file, nil)
	var partial *FilingPartialError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want *FilingPartialError", err)
	}
	want := []string{"ord0:recorded-before-post-create", "ord1:recorded-before-post-create", "ord2:recorded-before-post-create"}
	if fmt.Sprint(observed) != fmt.Sprint(want) {
		t.Errorf("post-create observations = %v, want %v (the hook must record before board/link)", observed, want)
	}
	// child1 (ordinal 1) was recorded despite child2 failing right after; child2
	// is ALSO recorded because its create landed before its link failed.
	if _, ok := repo.items[1]; !ok {
		t.Error("ordinal 1 not recorded although its File succeeded before the next failed")
	}
	if _, ok := repo.items[2]; !ok {
		t.Error("ordinal 2 not recorded although its create landed before its link step failed")
	}
	if partial.FailedOrdinal != 2 || partial.Step != FilingStepCreate {
		t.Errorf("partial = ordinal %d step %q, want ordinal 2 step create", partial.FailedOrdinal, partial.Step)
	}
	if len(partial.Filed) != 3 {
		t.Errorf("partial.Filed = %+v, want epic + both children (the interrupted child is durable)", partial.Filed)
	}
}

// ---- ExecuteFiling: repo mismatch -----------------------------------------

func TestExecuteFiling_RepoMismatch_NoFileCalls(t *testing.T) {
	d := EpicDraft{Epic: EpicSpec{Summary: "e", Scope: "s"}, Children: []ChildDraft{validChild("one")}}
	draft := storedDraft(d)
	repo := &memFilingRepo{session: &FilingSession{DraftID: draft.ID, SessionID: draft.SessionID, Repo: "a/b"}}
	filer := newScriptedFiler(1, 0)

	_, err := ExecuteFiling(context.Background(), draft, "c/d", repo, filer.file, nil)
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

	outcome, err := ExecuteFiling(context.Background(), draft, "o/r", repo, filer.file, nil)
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

// ---- #4153: write-ahead ledger, idempotency keys, adoption, link pass -------

// trackerIssue is one issue in trackerFiler's in-memory forge.
type trackerIssue struct {
	number   int
	url      string
	body     string
	linkedTo int // the epic number it is linked under, 0 when unlinked
}

// trackerFiler is a provider-emulating FileItem fake: per FilingRequest it
// (a) creates an issue in an in-memory tracker, stamping the request's
// idempotency key into the body as Apply would and counting creates per key,
// (b) fires the request's OnCreated hook through the real
// ProviderRequest.NotifyCreated (unless skipHook), (c) runs afterCreate, then
// (d) runs a 'link' step under the parent epic that fails with ctx.Err() on a
// cancelled ctx, with failLinkFor's error for a scripted key, and silently
// skips the link (success, unlinked — github's best-effort posture) for a
// skipLinkFor key. Children and Link back FilingDeps over the same tracker.
type trackerFiler struct {
	next        int
	issues      map[int]*trackerIssue
	creates     map[string]int
	calls       []workmgmt.FilingRequest
	linkCalls   []int
	skipHook    bool
	hookTwice   bool
	beforeHook  func(ctx context.Context, n int, req workmgmt.FilingRequest)
	afterCreate func(ctx context.Context, n int, req workmgmt.FilingRequest)
	failLinkFor map[string]error
	skipLinkFor map[string]bool
	childrenErr error
	linkErr     error
}

func newTrackerFiler(start int) *trackerFiler {
	return &trackerFiler{next: start, issues: map[int]*trackerIssue{}, creates: map[string]int{}}
}

func (tr *trackerFiler) file(ctx context.Context, req workmgmt.FilingRequest) (int, string, error) {
	tr.calls = append(tr.calls, req)
	n := tr.next
	tr.next++
	url := fmt.Sprintf("https://x/issues/%d", n)
	tr.issues[n] = &trackerIssue{number: n, url: url, body: workmgmt.StampIdempotencyKey(req.Body, req.IdempotencyKey)}
	tr.creates[req.IdempotencyKey]++
	if tr.beforeHook != nil {
		tr.beforeHook(ctx, n, req)
	}
	if !tr.skipHook {
		pr := workmgmt.ProviderRequest{OnCreated: req.OnCreated}
		pr.NotifyCreated(ctx, &workmgmt.CreatedItem{Number: n, URL: url})
		if tr.hookTwice {
			pr.NotifyCreated(ctx, &workmgmt.CreatedItem{Number: n, URL: url})
		}
	}
	if tr.afterCreate != nil {
		tr.afterCreate(ctx, n, req)
	}
	if epic := req.Relations.ParentEpic; epic != "" {
		if err := ctx.Err(); err != nil {
			return 0, "", fmt.Errorf("link step: %w", err)
		}
		if err := tr.failLinkFor[req.IdempotencyKey]; err != nil {
			return 0, "", err
		}
		if !tr.skipLinkFor[req.IdempotencyKey] {
			num, err := workmgmt.ParseIssueRef(epic)
			if err != nil {
				return 0, "", err
			}
			tr.issues[n].linkedTo = num
		}
	}
	return n, url, nil
}

func (tr *trackerFiler) children(_ context.Context, epic int) ([]workmgmt.EpicChild, error) {
	if tr.childrenErr != nil {
		return nil, tr.childrenErr
	}
	var out []workmgmt.EpicChild
	for _, is := range tr.issues {
		if is.linkedTo == epic {
			out = append(out, workmgmt.EpicChild{Number: is.number, Body: is.body, URL: is.url})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (tr *trackerFiler) link(_ context.Context, epic, child int) error {
	tr.linkCalls = append(tr.linkCalls, child)
	if tr.linkErr != nil {
		return tr.linkErr
	}
	is, ok := tr.issues[child]
	if !ok {
		return fmt.Errorf("no issue #%d", child)
	}
	is.linkedTo = epic
	return nil
}

func (tr *trackerFiler) deps() FilingDeps {
	return FilingDeps{File: tr.file, Children: tr.children, Link: tr.link}
}

// numberForKey returns the number of the tracker issue whose body carries key.
func (tr *trackerFiler) numberForKey(key string) int {
	for n, is := range tr.issues {
		if workmgmt.BodyHasIdempotencyKey(is.body, key) {
			return n
		}
	}
	return 0
}

// ordinalOf maps a request back to its draft ordinal through its key.
func (tr *trackerFiler) ordinalOf(draft *StoredDraft, req workmgmt.FilingRequest) int {
	for ord := 0; ord <= len(draft.Draft.Children); ord++ {
		if ItemIdempotencyKey(draft.ID, ord) == req.IdempotencyKey {
			return ord
		}
	}
	return -1
}

// independentDraft returns a draft of n children; child 2 depends on child 1
// and child 4 on child 3 when present, so Depends on refs are exercised.
func independentDraft(n int) EpicDraft {
	children := make([]ChildDraft, n)
	for i := range children {
		children[i] = validChild(fmt.Sprintf("c%d", i+1))
	}
	if n >= 2 {
		children[1].DependsOn = []int{1}
	}
	if n >= 4 {
		children[3].DependsOn = []int{3}
	}
	return EpicDraft{Epic: EpicSpec{Summary: "e", Scope: "s"}, Children: children}
}

func assertEveryKeyCreatedOnce(t *testing.T, tr *trackerFiler, draft *StoredDraft) {
	t.Helper()
	for ord := 0; ord <= len(draft.Draft.Children); ord++ {
		if got := tr.creates[ItemIdempotencyKey(draft.ID, ord)]; got != 1 {
			t.Errorf("ordinal %d created %d time(s), want exactly 1 (a duplicate is the #4153 defect)", ord, got)
		}
	}
}

func assertEveryChildLinked(t *testing.T, tr *trackerFiler, repo *memFilingRepo, draft *StoredDraft) {
	t.Helper()
	epic := repo.items[0].IssueNumber
	for ord := 1; ord <= len(draft.Draft.Children); ord++ {
		it, ok := repo.items[ord]
		if !ok {
			t.Errorf("ordinal %d not recorded", ord)
			continue
		}
		if got := tr.issues[it.IssueNumber].linkedTo; got != epic {
			t.Errorf("ordinal %d (#%d) linked under %d, want epic #%d", ord, it.IssueNumber, got, epic)
		}
	}
}

// TestExecuteFiling_WriteAhead_CancelAfterCreate_ResumeNoDuplicates (#4153
// acceptance): the filing is cancelled right after child 3's create+hook, so
// its link step fails and file() returns an error for child 3 — a record
// placed after file() returns never runs. Only the write-ahead hook leaves it
// durable; the resume finishes it (the link pass) instead of re-creating it.
func TestExecuteFiling_WriteAhead_CancelAfterCreate_ResumeNoDuplicates(t *testing.T) {
	draft := storedDraft(independentDraft(4))
	repo := &memFilingRepo{}
	tr := newTrackerFiler(100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.afterCreate = func(_ context.Context, _ int, req workmgmt.FilingRequest) {
		if req.IdempotencyKey == ItemIdempotencyKey(draft.ID, 3) {
			cancel()
		}
	}

	_, err := ExecuteFilingWith(ctx, draft, "o/r", repo, tr.deps())
	var partial *FilingPartialError
	if !errors.As(err, &partial) {
		t.Fatalf("first filing err = %v, want *FilingPartialError", err)
	}
	if partial.FailedOrdinal != 3 || partial.Step != FilingStepCreate {
		t.Fatalf("partial = ordinal %d step %q, want ordinal 3 step create", partial.FailedOrdinal, partial.Step)
	}
	first3, ok := repo.items[3]
	if !ok {
		t.Fatal("ordinal 3 not recorded after its create landed — the write-ahead hook did not record it")
	}

	tr.afterCreate = nil
	outcome, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertEveryKeyCreatedOnce(t, tr, draft)
	if repo.items[3].IssueNumber != first3.IssueNumber || outcome.Children[2].IssueNumber != first3.IssueNumber {
		t.Errorf("ordinal 3 = #%d (outcome #%d), want its first number #%d", repo.items[3].IssueNumber, outcome.Children[2].IssueNumber, first3.IssueNumber)
	}
	if len(tr.linkCalls) != 1 || tr.linkCalls[0] != first3.IssueNumber {
		t.Errorf("link calls = %v, want exactly [%d] (the interrupted child)", tr.linkCalls, first3.IssueNumber)
	}
	assertEveryChildLinked(t, tr, repo, draft)
	last := tr.calls[len(tr.calls)-1]
	if want := fmt.Sprintf("#%d", first3.IssueNumber); len(last.Relations.DependsOn) != 1 || last.Relations.DependsOn[0] != want {
		t.Errorf("child 4 depends_on = %v, want [%s] (the recorded number of child 3)", last.Relations.DependsOn, want)
	}
}

// TestExecuteFiling_WriteAhead_RecordSurvivesCancelledContext: the filing ctx
// is cancelled BEFORE the hook fires and the repo honours ctx like Postgres
// does, so the hook's insert must run on a cancellation-detached context.
func TestExecuteFiling_WriteAhead_RecordSurvivesCancelledContext(t *testing.T) {
	draft := storedDraft(independentDraft(3))
	repo := &memFilingRepo{ctxRespecting: true}
	tr := newTrackerFiler(200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key2 := ItemIdempotencyKey(draft.ID, 2)
	tr.beforeHook = func(_ context.Context, _ int, req workmgmt.FilingRequest) {
		if req.IdempotencyKey == key2 {
			cancel()
		}
	}

	_, err := ExecuteFilingWith(ctx, draft, "o/r", repo, tr.deps())
	var partial *FilingPartialError
	if !errors.As(err, &partial) || partial.FailedOrdinal != 2 {
		t.Fatalf("first filing err = %v, want *FilingPartialError at ordinal 2", err)
	}
	if _, ok := repo.items[2]; !ok {
		t.Fatal("ordinal 2 not recorded: the hook's insert ran on the cancelled filing context")
	}

	tr.beforeHook = nil
	if _, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := tr.creates[key2]; got != 1 {
		t.Errorf("ordinal 2 created %d times, want 1", got)
	}
	assertEveryChildLinked(t, tr, repo, draft)
}

// TestExecuteFiling_ResumeLinksCreatedButUnlinkedChild: child 2's create lands
// and is recorded but its link fails; the resume's link pass links it exactly
// once and never re-links an already-linked child.
func TestExecuteFiling_ResumeLinksCreatedButUnlinkedChild(t *testing.T) {
	draft := storedDraft(independentDraft(3))
	repo := &memFilingRepo{}
	tr := newTrackerFiler(300)
	tr.failLinkFor = map[string]error{ItemIdempotencyKey(draft.ID, 2): errors.New("sub-issue link refused")}

	if _, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps()); err == nil {
		t.Fatal("first filing succeeded, want the scripted link failure")
	}
	child2 := repo.items[2].IssueNumber

	tr.failLinkFor = nil
	if _, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(tr.linkCalls) != 1 || tr.linkCalls[0] != child2 {
		t.Errorf("link calls = %v, want exactly [%d]", tr.linkCalls, child2)
	}
	assertEveryKeyCreatedOnce(t, tr, draft)
	assertEveryChildLinked(t, tr, repo, draft)
}

// seededResume returns a draft whose epic (#500) is already recorded, so the
// next invocation is a resume.
func seededResume(t *testing.T, n int) (*StoredDraft, *memFilingRepo, *trackerFiler) {
	t.Helper()
	draft := storedDraft(independentDraft(n))
	repo := &memFilingRepo{
		session: &FilingSession{DraftID: draft.ID, SessionID: draft.SessionID, Repo: "o/r"},
		items:   map[int]*FiledItem{0: {DraftID: draft.ID, Ordinal: 0, IssueNumber: 500, IssueURL: "u500"}},
	}
	tr := newTrackerFiler(700)
	tr.issues[500] = &trackerIssue{number: 500, url: "u500"}
	return draft, repo, tr
}

// TestExecuteFiling_ResumeAdoptsLinkedUnrecordedChildByKey: an issue linked
// under the recorded epic carries ordinal 2's key but has no ledger row (its
// create response, or its record, was lost). The resume adopts it.
func TestExecuteFiling_ResumeAdoptsLinkedUnrecordedChildByKey(t *testing.T) {
	draft, repo, tr := seededResume(t, 3)
	key2 := ItemIdempotencyKey(draft.ID, 2)
	tr.issues[600] = &trackerIssue{number: 600, url: "u600", linkedTo: 500, body: workmgmt.StampIdempotencyKey("body", key2)}

	outcome, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := tr.creates[key2]; got != 0 {
		t.Errorf("ordinal 2 created %d time(s), want 0 (adopted, not duplicated)", got)
	}
	if it := repo.items[2]; it == nil || it.IssueNumber != 600 || it.IssueURL != "u600" {
		t.Errorf("ordinal 2 ledger row = %+v, want the adopted #600 / u600", it)
	}
	if outcome.Children[1].IssueNumber != 600 {
		t.Errorf("outcome child 2 = #%d, want #600", outcome.Children[1].IssueNumber)
	}
	for _, c := range tr.linkCalls {
		if c == 600 {
			t.Error("the adopted, already-linked child was re-linked")
		}
	}
	if got := tr.creates[ItemIdempotencyKey(draft.ID, 1)] + tr.creates[ItemIdempotencyKey(draft.ID, 3)]; got != 2 {
		t.Errorf("children 1 and 3 created %d time(s) in total, want 2", got)
	}
}

// TestExecuteFiling_ResumeReconcileQueryErrorFailsClosed: a resume that cannot
// read the epic's children stops before creating anything.
func TestExecuteFiling_ResumeReconcileQueryErrorFailsClosed(t *testing.T) {
	draft, repo, tr := seededResume(t, 2)
	tr.childrenErr = errors.New("graphql down")

	_, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps())
	var partial *FilingPartialError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want *FilingPartialError", err)
	}
	if partial.Step != FilingStepReconcile || partial.FailedOrdinal != 1 {
		t.Errorf("partial = ordinal %d step %q, want ordinal 1 step reconcile", partial.FailedOrdinal, partial.Step)
	}
	if len(tr.calls) != 0 {
		t.Errorf("file() called %d time(s), want 0 — a blind resume is the duplicate path", len(tr.calls))
	}
}

// TestExecuteFiling_StopsBeforeNextItemOnExpiredContext: a ctx-ignoring filer
// keeps working after the budget expires; the executor's per-item check stops
// before the next create.
func TestExecuteFiling_StopsBeforeNextItemOnExpiredContext(t *testing.T) {
	d := EpicDraft{Epic: EpicSpec{Summary: "e", Scope: "s"}, Children: []ChildDraft{validChild("a"), validChild("b"), validChild("c")}}
	draft := storedDraft(d)
	repo := &memFilingRepo{}
	inner := newScriptedFiler(10, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	file := func(c context.Context, req workmgmt.FilingRequest) (int, string, error) {
		n, u, err := inner.file(c, req)
		if req.TitleVars["n"] == "1" {
			cancel()
		}
		return n, u, err
	}

	_, err := ExecuteFilingWith(ctx, draft, "o/r", repo, FilingDeps{File: file})
	var partial *FilingPartialError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want *FilingPartialError", err)
	}
	if partial.FailedOrdinal != 2 || partial.Step != FilingStepCreate || !errors.Is(err, context.Canceled) {
		t.Errorf("partial = ordinal %d step %q err %v, want ordinal 2 step create wrapping context.Canceled", partial.FailedOrdinal, partial.Step, partial.Err)
	}
	if len(inner.calls) != 2 {
		t.Errorf("file() calls = %d, want 2 (epic + child 1; nothing after the context expired)", len(inner.calls))
	}
	if _, ok := repo.items[1]; !ok {
		t.Error("child 1 not recorded although its File returned before the expiry was observed")
	}
}

// TestExecuteFiling_CancelledBeforeEpic_NoFileCalls: a dead context never
// dials the forge for the epic.
func TestExecuteFiling_CancelledBeforeEpic_NoFileCalls(t *testing.T) {
	draft := storedDraft(independentDraft(1))
	repo := &memFilingRepo{}
	tr := newTrackerFiler(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ExecuteFilingWith(ctx, draft, "o/r", repo, tr.deps())
	var partial *FilingPartialError
	if !errors.As(err, &partial) || partial.FailedOrdinal != 0 || partial.Step != FilingStepCreate {
		t.Fatalf("err = %v, want *FilingPartialError at ordinal 0 step create", err)
	}
	if len(tr.calls) != 0 {
		t.Errorf("file() calls = %d, want 0 on a dead context", len(tr.calls))
	}
}

// TestExecuteFiling_LinkFailureReturnsPartialAndSkipsFinalize: child 2 lands
// unlinked (best-effort link skipped) and the link pass's Link fails.
func TestExecuteFiling_LinkFailureReturnsPartialAndSkipsFinalize(t *testing.T) {
	draft := storedDraft(independentDraft(3))
	repo := &memFilingRepo{}
	tr := newTrackerFiler(40)
	tr.skipLinkFor = map[string]bool{ItemIdempotencyKey(draft.ID, 2): true}
	tr.linkErr = errors.New("sub-issue refused")
	deps := tr.deps()
	finalized := false
	deps.Finalize = func(context.Context, *FilingOutcome) error { finalized = true; return nil }

	_, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, deps)
	var partial *FilingPartialError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want *FilingPartialError", err)
	}
	if partial.Step != FilingStepLink || partial.FailedOrdinal != 2 {
		t.Errorf("partial = ordinal %d step %q, want ordinal 2 step link", partial.FailedOrdinal, partial.Step)
	}
	if len(partial.Filed) != 4 {
		t.Errorf("partial.Filed = %d items, want 4 (everything filed before the link pass)", len(partial.Filed))
	}
	if finalized {
		t.Error("finalize ran although the link pass failed")
	}
}

// TestExecuteFiling_LinkPassChildrenErrorReturnsPartial: the link pass cannot
// read the epic's children (on a fresh fill the pass is the first read).
func TestExecuteFiling_LinkPassChildrenErrorReturnsPartial(t *testing.T) {
	draft := storedDraft(independentDraft(2))
	repo := &memFilingRepo{}
	tr := newTrackerFiler(60)
	tr.childrenErr = errors.New("graphql down")
	deps := tr.deps()
	finalized := false
	deps.Finalize = func(context.Context, *FilingOutcome) error { finalized = true; return nil }

	_, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, deps)
	var partial *FilingPartialError
	if !errors.As(err, &partial) || partial.Step != FilingStepLink || partial.FailedOrdinal != 0 {
		t.Fatalf("err = %v, want *FilingPartialError at ordinal 0 step link", err)
	}
	if finalized {
		t.Error("finalize ran although the link pass could not read the epic's children")
	}
	if len(repo.items) != 3 {
		t.Errorf("recorded = %d, want 3 (every item filed before the pass)", len(repo.items))
	}
}

// TestExecuteFiling_RecordFailureNamesCreatedIssue_ResumeAdopts: the ledger
// refuses ordinal 2 on both the hook and the fallback; the error names the
// created issue, and a resume against a healthy ledger adopts it by key.
func TestExecuteFiling_RecordFailureNamesCreatedIssue_ResumeAdopts(t *testing.T) {
	draft := storedDraft(independentDraft(3))
	repo := &memFilingRepo{failRecord: map[int]error{2: errors.New("db down")}}
	tr := newTrackerFiler(80)

	_, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps())
	if err == nil {
		t.Fatal("filing succeeded although ordinal 2 could not be recorded")
	}
	// Wave order files epic #80, child1 #81, child3 #82, then child2 #83.
	created2 := tr.numberForKey(ItemIdempotencyKey(draft.ID, 2))
	if created2 == 0 || !strings.Contains(err.Error(), fmt.Sprintf("issue #%d ", created2)) {
		t.Errorf("err = %v, want it to name the created issue #%d", err, created2)
	}
	if repo.recordCalls[2] != 2 {
		t.Errorf("ordinal 2 record attempts = %d, want 2 (hook + fallback)", repo.recordCalls[2])
	}

	repo.failRecord = nil
	if _, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps()); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := tr.creates[ItemIdempotencyKey(draft.ID, 2)]; got != 1 {
		t.Errorf("ordinal 2 created %d times, want 1 (adopted on resume)", got)
	}
	if repo.items[2].IssueNumber != created2 {
		t.Errorf("ordinal 2 = #%d, want the adopted #%d", repo.items[2].IssueNumber, created2)
	}
}

// TestExecuteFiling_FileErrorAfterHookRecordFailure_NamesCreatedIssue: the
// create landed, the hook's insert failed and then File failed too — the
// partial error names the created-but-unrecorded issue.
func TestExecuteFiling_FileErrorAfterHookRecordFailure_NamesCreatedIssue(t *testing.T) {
	draft := storedDraft(independentDraft(2))
	repo := &memFilingRepo{failRecord: map[int]error{1: errors.New("db down")}}
	tr := newTrackerFiler(90)
	tr.failLinkFor = map[string]error{ItemIdempotencyKey(draft.ID, 1): errors.New("link boom")}

	_, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps())
	var partial *FilingPartialError
	if !errors.As(err, &partial) || partial.FailedOrdinal != 1 {
		t.Fatalf("err = %v, want *FilingPartialError at ordinal 1", err)
	}
	if !strings.Contains(err.Error(), "issue #91 was created") || !strings.Contains(err.Error(), "link boom") {
		t.Errorf("err = %v, want it to wrap the provider error AND name the created issue #91", err)
	}
}

// TestExecuteFiling_FallbackRecordsWhenProviderSkipsHook: a provider that never
// fires OnCreated (jira-shaped) still gets every item recorded.
func TestExecuteFiling_FallbackRecordsWhenProviderSkipsHook(t *testing.T) {
	draft := storedDraft(independentDraft(2))
	repo := &memFilingRepo{}
	tr := newTrackerFiler(20)
	tr.skipHook = true
	if _, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps()); err != nil {
		t.Fatalf("ExecuteFilingWith: %v", err)
	}
	for ord, want := range map[int]int{0: 20, 1: 21, 2: 22} {
		if it := repo.items[ord]; it == nil || it.IssueNumber != want {
			t.Errorf("ordinal %d = %+v, want #%d recorded after File returned", ord, it, want)
		}
	}
}

// TestExecuteFiling_HookFiredTwiceRecordsOnce: the hook is exactly-once on the
// executor side even if a provider fires it twice.
func TestExecuteFiling_HookFiredTwiceRecordsOnce(t *testing.T) {
	draft := storedDraft(independentDraft(1))
	repo := &memFilingRepo{}
	tr := newTrackerFiler(30)
	tr.hookTwice = true
	if _, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps()); err != nil {
		t.Fatalf("ExecuteFilingWith: %v", err)
	}
	for ord := 0; ord <= 1; ord++ {
		if repo.recordCalls[ord] != 1 {
			t.Errorf("ordinal %d record attempts = %d, want 1", ord, repo.recordCalls[ord])
		}
	}
}

// TestExecuteFiling_StampsDeterministicItemKeys: every FilingRequest carries
// ItemIdempotencyKey(draft.ID, ordinal); keys are distinct per ordinal and
// stable across invocations.
func TestExecuteFiling_StampsDeterministicItemKeys(t *testing.T) {
	draft := storedDraft(independentDraft(3))
	run := func() []string {
		tr := newTrackerFiler(1)
		if _, err := ExecuteFilingWith(context.Background(), draft, "o/r", &memFilingRepo{}, FilingDeps{File: tr.file}); err != nil {
			t.Fatalf("ExecuteFilingWith: %v", err)
		}
		keys := make([]string, 0, len(tr.calls))
		for _, c := range tr.calls {
			keys = append(keys, c.IdempotencyKey)
		}
		return keys
	}
	first, second := run(), run()
	order, _ := FilingOrder(draft.Draft)
	want := []string{ItemIdempotencyKey(draft.ID, 0)}
	for _, ord := range order {
		want = append(want, ItemIdempotencyKey(draft.ID, ord))
	}
	if fmt.Sprint(first) != fmt.Sprint(want) {
		t.Errorf("keys = %v, want %v", first, want)
	}
	if fmt.Sprint(second) != fmt.Sprint(first) {
		t.Errorf("keys differ across invocations: %v vs %v", first, second)
	}
	seen := map[string]bool{}
	for _, k := range first {
		if k == "" || seen[k] {
			t.Errorf("key %q empty or duplicated across ordinals", k)
		}
		seen[k] = true
	}
	if ItemIdempotencyKey(draft.ID, 1) == ItemIdempotencyKey(uuid.New(), 1) {
		t.Error("the key does not depend on the draft id")
	}
}

// TestExecuteFiling_AdoptionRecordFailureNamesIssue: the ledger refuses the
// adoption row; the error names the adoptable issue and nothing is created.
func TestExecuteFiling_AdoptionRecordFailureNamesIssue(t *testing.T) {
	draft, repo, tr := seededResume(t, 1)
	key1 := ItemIdempotencyKey(draft.ID, 1)
	tr.issues[600] = &trackerIssue{number: 600, url: "u600", linkedTo: 500, body: workmgmt.StampIdempotencyKey("b", key1)}
	repo.failRecord = map[int]error{1: errors.New("db down")}

	_, err := ExecuteFilingWith(context.Background(), draft, "o/r", repo, tr.deps())
	if err == nil || !strings.Contains(err.Error(), "adopt existing issue #600 for ordinal 1") {
		t.Fatalf("err = %v, want it to name the adoptable issue #600", err)
	}
	if len(tr.calls) != 0 {
		t.Errorf("file() called %d time(s), want 0 — a failed adoption must not fall through to a create", len(tr.calls))
	}
}
