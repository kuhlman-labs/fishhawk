package captain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

func makeAccount(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`,
		id, "acct-"+id.String()[:8]); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return id
}

func applyParams(acct *uuid.UUID, repo, actor string) ApplyParams {
	return ApplyParams{AccountID: acct, Repo: repo, Actor: actor, ActorKind: audit.ActorUser, Timestamp: time.Now().UTC()}
}

func claimFn(repo, actor string) func(State) (Event, error) {
	return func(s State) (Event, error) {
		p := Params{Repo: repo, Actor: actor, Predicate: PredicateTrivial, PredicateBasis: "trivial:any-non-agent-token-holder"}
		return Claim(s, p)
	}
}

func mustApply(t *testing.T, st *Store, p ApplyParams, decide func(State) (Event, error)) *Applied {
	t.Helper()
	a, err := st.Apply(context.Background(), p, decide)
	if err != nil {
		t.Fatalf("Apply(%s by %s): %v", p.Repo, p.Actor, err)
	}
	return a
}

// barrier is the approval-condition-2 seam driver: every caller that reaches
// the store's afterRead hook parks until all n have arrived, or until timeout.
// With the read correctly INSIDE the advisory lock only one caller is ever in
// the hook (the rest are blocked on the lock), so each holder is released by
// the timeout and maxIn stays 1. With the read moved OUTSIDE the lock, all n
// arrive holding the same stale state and are released together.
type barrier struct {
	n       int
	timeout time.Duration
	release chan struct{}

	mu      sync.Mutex
	arrived int
	in      int
	maxIn   int
}

func newBarrier(n int, timeout time.Duration) *barrier {
	return &barrier{n: n, timeout: timeout, release: make(chan struct{})}
}

func (b *barrier) hook() {
	b.mu.Lock()
	b.arrived++
	b.in++
	if b.in > b.maxIn {
		b.maxIn = b.in
	}
	if b.arrived == b.n {
		close(b.release)
	}
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-time.After(b.timeout):
	}
	b.mu.Lock()
	b.in--
	b.mu.Unlock()
}

func countCategory(entries []ChainEntry, category string) int {
	n := 0
	for _, e := range entries {
		if e.Category == category {
			n++
		}
	}
	return n
}

func TestStore_Entries_ScopedToRepoAndAccount(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := NewStore(pool)
	ctx := context.Background()
	acctA, acctB := makeAccount(t, pool), makeAccount(t, pool)
	const repo1, repo2 = "org/one", "org/two"

	mustApply(t, st, applyParams(&acctA, repo1, "github:alice"), claimFn(repo1, "github:alice"))
	mustApply(t, st, applyParams(&acctB, repo1, "github:bob"), claimFn(repo1, "github:bob"))
	mustApply(t, st, applyParams(&acctA, repo2, "github:carol"), claimFn(repo2, "github:carol"))
	mustApply(t, st, applyParams(nil, repo1, "github:dave"), claimFn(repo1, "github:dave"))
	mustApply(t, st, applyParams(&acctA, repo1, "github:alice"), func(s State) (Event, error) {
		return Relinquish(s, Params{Repo: repo1, Actor: "github:alice"})
	})
	// A non-captain global entry naming the same repo must not be read.
	if _, err := audit.NewPostgresRepository(pool).AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(), Category: "digest_marked_read",
		Payload: json.RawMessage(`{"repo":"org/one","subject":"github:alice"}`), AccountID: &acctA,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.Entries(ctx, &acctA, repo1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Category != CategoryClaimed || got[1].Category != CategoryRelinquished || got[0].Sequence >= got[1].Sequence {
		t.Fatalf("acctA/repo1 entries = %+v, want [claimed, relinquished] ascending", got)
	}
	for _, e := range got {
		var p payload
		_ = json.Unmarshal(e.Payload, &p)
		if p.Subject != "github:alice" {
			t.Errorf("acctA/repo1 read a foreign entry: %s", e.Payload)
		}
	}
	snapB, err := st.Read(ctx, &acctB, repo1)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapB.Entries) != 1 || snapB.State.Current == nil || snapB.State.Current.Subject != "github:bob" {
		t.Errorf("acctB/repo1 = %+v, want only bob's claim", snapB)
	}
	untenanted, err := st.Read(ctx, nil, repo1)
	if err != nil {
		t.Fatal(err)
	}
	if len(untenanted.Entries) != 1 || untenanted.State.Current == nil || untenanted.State.Current.Subject != "github:dave" {
		t.Errorf("untenanted/repo1 = %+v, want only dave's claim", untenanted)
	}
	a2, err := st.Read(ctx, &acctA, repo2)
	if err != nil {
		t.Fatal(err)
	}
	if a2.State.Current == nil || a2.State.Current.Subject != "github:carol" {
		t.Errorf("acctA/repo2 = %+v, want carol", a2.State.Current)
	}
}

// TestStore_ConcurrentClaimsAtVacantSeat_ExactlyOneWins is C0's vehicle. The
// barrier forces every claimant to have READ before any appends when the read
// escapes the lock, so the mutation reddens every run, not by luck.
func TestStore_ConcurrentClaimsAtVacantSeat_ExactlyOneWins(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := NewStore(pool)
	acct := makeAccount(t, pool)
	const repo = "org/race"
	const n = 8
	b := newBarrier(n, 250*time.Millisecond)
	st.afterRead = b.hook

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			actor := fmt.Sprintf("github:claimant%d", i)
			_, errs[i] = st.Apply(context.Background(), applyParams(&acct, repo, actor), claimFn(repo, actor))
		}()
	}
	wg.Wait()

	wins, exists := 0, 0
	winner := ""
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
			winner = fmt.Sprintf("github:claimant%d", i)
		case errors.Is(err, ErrCaptainExists):
			exists++
		default:
			t.Errorf("claimant %d: unexpected error %v", i, err)
		}
	}
	st.afterRead = nil
	entries, err := st.Entries(context.Background(), &acct, repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := countCategory(entries, CategoryClaimed); got != 1 {
		t.Fatalf("committed chain holds %d captain_claimed entries, want exactly 1 (winners=%d)", got, wins)
	}
	if wins != 1 || exists != n-1 {
		t.Errorf("wins=%d ErrCaptainExists=%d, want 1 and %d", wins, exists, n-1)
	}
	if cur := Derive(repo, entries).Current; cur == nil || cur.Subject != winner {
		t.Errorf("derived captain = %+v, want the winner %s", cur, winner)
	}
	if b.maxIn != 1 {
		t.Errorf("max concurrent callers past the read = %d, want 1 (the read must be under the lock)", b.maxIn)
	}
}

// TestStore_AcceptRacesWithdraw_NeverAssignsAWithdrawnOffer: the two legal
// interleavings are withdraw-then-accept (accept refused ErrNoOffer) and
// accept-then-withdraw (withdraw refused ErrNoOffer); no third outcome — in
// particular never a captain_assigned AND a captain_handover_withdrawn both
// naming the same offer.
func TestStore_AcceptRacesWithdraw_NeverAssignsAWithdrawnOffer(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := NewStore(pool)
	acct := makeAccount(t, pool)
	const repo = "org/handover"

	mustApply(t, st, applyParams(&acct, repo, "github:alice"), claimFn(repo, "github:alice"))
	offered := mustApply(t, st, applyParams(&acct, repo, "github:alice"), func(s State) (Event, error) {
		return Offer(s, Params{Repo: repo, Actor: "github:alice", Successor: "github:carol"})
	})
	offerHash := offered.Entry.EntryHash
	if offered.State.PendingOffer == nil || offered.State.PendingOffer.EntryHash != offerHash {
		t.Fatalf("seed offer not pending: %+v", offered.State)
	}

	b := newBarrier(2, 250*time.Millisecond)
	st.afterRead = b.hook
	var wg sync.WaitGroup
	var acceptErr, withdrawErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, acceptErr = st.Apply(context.Background(), applyParams(&acct, repo, "github:carol"), func(s State) (Event, error) {
			return Accept(s, Params{Repo: repo, Actor: "github:carol"})
		})
	}()
	go func() {
		defer wg.Done()
		_, withdrawErr = st.Apply(context.Background(), applyParams(&acct, repo, "github:alice"), func(s State) (Event, error) {
			return Withdraw(s, Params{Repo: repo, Actor: "github:alice"})
		})
	}()
	wg.Wait()
	st.afterRead = nil

	entries, err := st.Entries(context.Background(), &acct, repo)
	if err != nil {
		t.Fatal(err)
	}
	var assigned, withdrawn int
	for _, e := range entries {
		var p payload
		if err := json.Unmarshal(e.Payload, &p); err != nil || p.OfferEntryHash != offerHash {
			continue
		}
		switch e.Category {
		case CategoryAssigned:
			assigned++
		case CategoryHandoverWithdrawn:
			withdrawn++
		}
	}
	if assigned+withdrawn != 1 {
		t.Fatalf("committed chain resolves offer %s %d times (assigned=%d withdrawn=%d), want exactly once", offerHash, assigned+withdrawn, assigned, withdrawn)
	}
	switch {
	case acceptErr == nil && errors.Is(withdrawErr, ErrNoOffer):
		if cur := Derive(repo, entries).Current; cur == nil || cur.Subject != "github:carol" {
			t.Errorf("accept won but captain = %+v", cur)
		}
	case withdrawErr == nil && errors.Is(acceptErr, ErrNoOffer):
		if cur := Derive(repo, entries).Current; cur == nil || cur.Subject != "github:alice" {
			t.Errorf("withdraw won but captain = %+v", cur)
		}
	default:
		t.Errorf("third outcome: accept=%v withdraw=%v", acceptErr, withdrawErr)
	}
	if b.maxIn != 1 {
		t.Errorf("max concurrent callers past the read = %d, want 1", b.maxIn)
	}
}

// TestStore_DecideErrorAppendsNothing: a decide that returns a VALID event
// together with an error must append nothing — the store honours the error,
// the transaction rolls back, and the partition is byte-unchanged.
func TestStore_DecideErrorAppendsNothing(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := NewStore(pool)
	acct := makeAccount(t, pool)
	const repo = "org/rollback"
	ctx := context.Background()
	mustApply(t, st, applyParams(&acct, repo, "github:alice"), claimFn(repo, "github:alice"))

	auditRepo := audit.NewPostgresRepository(pool)
	before, err := auditRepo.ListGlobalByAccount(ctx, &acct)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("refused by decide")
	_, err = st.Apply(ctx, applyParams(&acct, repo, "github:alice"), func(s State) (Event, error) {
		ev, _ := Relinquish(s, Params{Repo: repo, Actor: "github:alice"})
		return ev, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Apply err = %v, want the decide error", err)
	}
	after, err := auditRepo.ListGlobalByAccount(ctx, &acct)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("decide error changed the partition: %d -> %d entries", len(before), len(after))
	}
}

// Malformed events from decide are refused inside the transaction and append
// nothing.
func TestStore_MalformedEventAppendsNothing(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := NewStore(pool)
	acct := makeAccount(t, pool)
	const repo = "org/malformed"
	ctx := context.Background()

	_, err := st.Apply(ctx, applyParams(&acct, repo, "github:alice"), func(State) (Event, error) {
		return Event{Kind: CategoryClaimed, Repo: "org/elsewhere", Subject: "github:alice"}, nil
	})
	if !errors.Is(err, ErrEventRepoMismatch) {
		t.Errorf("repo-mismatched event: err = %v, want ErrEventRepoMismatch", err)
	}
	_, err = st.Apply(ctx, applyParams(&acct, repo, "github:alice"), func(State) (Event, error) {
		return Event{Kind: "digest_marked_read", Repo: repo, Subject: "github:alice"}, nil
	})
	if err == nil {
		t.Error("non-captain event kind was appended")
	}
	got, err := audit.NewPostgresRepository(pool).ListGlobalByAccount(ctx, &acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("malformed events appended %d entries, want 0", len(got))
	}
}

func TestStore_ApplyRefusesMissingInputs(t *testing.T) {
	st := NewStore(nil)
	ctx := context.Background()
	if _, err := st.Apply(ctx, ApplyParams{}, claimFn("r", "github:a")); !errors.Is(err, ErrRepoRequired) {
		t.Errorf("no repo: err = %v, want ErrRepoRequired", err)
	}
	if _, err := st.Apply(ctx, ApplyParams{Repo: "r"}, nil); !errors.Is(err, ErrDecideRequired) {
		t.Errorf("nil decide: err = %v, want ErrDecideRequired", err)
	}
	if _, err := st.Read(ctx, nil, ""); !errors.Is(err, ErrRepoRequired) {
		t.Errorf("read no repo: err = %v, want ErrRepoRequired", err)
	}
}

// TestStore_FullLifecycle drives claim -> offer -> accept -> relinquish ->
// claim through Apply and checks Applied.State against the lock-free Read at
// each step, plus the actor stamp on the persisted entry.
func TestStore_FullLifecycle(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := NewStore(pool)
	acct := makeAccount(t, pool)
	const repo = "org/lifecycle"
	ctx := context.Background()

	check := func(a *Applied, wantCaptain string) {
		t.Helper()
		snap, err := st.Read(ctx, &acct, repo)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(snap.State, a.State) {
			t.Errorf("Applied.State %+v != Read state %+v", a.State, snap.State)
		}
		got := ""
		if a.State.Current != nil {
			got = a.State.Current.Subject
		}
		if got != wantCaptain {
			t.Errorf("captain = %q, want %q", got, wantCaptain)
		}
	}

	a := mustApply(t, st, applyParams(&acct, repo, "github:alice"), claimFn(repo, "github:alice"))
	if a.Entry.ActorSubject == nil || *a.Entry.ActorSubject != "github:alice" || a.Entry.ActorKind == nil || *a.Entry.ActorKind != audit.ActorUser || a.Entry.RunID != nil {
		t.Errorf("entry actor stamp = %+v", a.Entry)
	}
	check(a, "github:alice")
	a = mustApply(t, st, applyParams(&acct, repo, "github:alice"), func(s State) (Event, error) {
		return Offer(s, Params{Repo: repo, Actor: "github:alice", Successor: "brett@local-mcp"})
	})
	check(a, "github:alice")
	if a.State.PendingOffer == nil || a.State.PendingOffer.SuccessorIdentityVerified {
		t.Errorf("pending offer = %+v, want static successor unverified", a.State.PendingOffer)
	}
	a = mustApply(t, st, applyParams(&acct, repo, "brett@local-mcp"), func(s State) (Event, error) {
		return Accept(s, Params{Repo: repo, Actor: "brett@local-mcp"})
	})
	check(a, "brett@local-mcp")
	if a.State.Current.IdentityVerified || a.State.Current.Basis != BasisAssigned || a.State.Current.ClaimVerified != nil {
		t.Errorf("assigned static captain = %+v", a.State.Current)
	}
	a = mustApply(t, st, applyParams(&acct, repo, "brett@local-mcp"), func(s State) (Event, error) {
		return Relinquish(s, Params{Repo: repo, Actor: "brett@local-mcp"})
	})
	check(a, "")
	a = mustApply(t, st, applyParams(&acct, repo, "github:bob"), claimFn(repo, "github:bob"))
	check(a, "github:bob")
	var p payload
	if err := json.Unmarshal(a.Entry.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.PreviousCaptain != "brett@local-mcp" || !p.PagePending {
		t.Errorf("persisted claim payload = %+v, want previous_captain brett@local-mcp and page_pending", p)
	}
}

func TestCaptainLockKey_DomainSeparated(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	keys := map[int64]string{}
	for name, k := range map[string]int64{
		"a/r1":          captainLockKey(&a, "r1"),
		"a/r2":          captainLockKey(&a, "r2"),
		"b/r1":          captainLockKey(&b, "r1"),
		"untenanted/r1": captainLockKey(nil, "r1"),
	} {
		if other, dup := keys[k]; dup {
			t.Errorf("%s and %s share lock key %d", name, other, k)
		}
		keys[k] = name
	}
	a2 := a
	if captainLockKey(&a, "r1") != captainLockKey(&a2, "r1") {
		t.Error("lock key is not deterministic")
	}
}
