package account

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// fakePinQueries records calls and returns programmed results. It exists for
// the branches that refuse BEFORE any query runs — those must be provably
// query-free, which a real database cannot show.
type fakePinQueries struct {
	pinCalls int
	getCalls int
	pinErr   error
	row      accountdb.Account
	getErr   error
}

func (f *fakePinQueries) PinAccountHomeRegion(_ context.Context, _ accountdb.PinAccountHomeRegionParams) (accountdb.Account, error) {
	f.pinCalls++
	return f.row, f.pinErr
}

func (f *fakePinQueries) GetAccountByKey(_ context.Context, _ accountdb.GetAccountByKeyParams) (accountdb.Account, error) {
	f.getCalls++
	return f.row, f.getErr
}

// TestPin_DisabledWhenCellRegionUnset is the ADR-062 A2.4 fail-closed branch:
// a cell with no configured region refuses every pin and issues NO query.
func TestPin_DisabledWhenCellRegionUnset(t *testing.T) {
	q := &fakePinQueries{}
	p := NewRegionPinner(q, "")

	err := p.Pin(context.Background(), "github", "acme", "us")
	if !errors.Is(err, ErrRegionDisabled) {
		t.Fatalf("Pin with unset cell region = %v, want ErrRegionDisabled", err)
	}
	if q.pinCalls != 0 {
		t.Fatalf("disabled pinner issued %d queries, want 0", q.pinCalls)
	}
	if p.Enabled() {
		t.Fatal("Enabled() = true for an unset cell region")
	}
}

// A nil query surface is the same posture: disabled, not permissive.
func TestPin_DisabledWhenQueriesNil(t *testing.T) {
	p := NewRegionPinner(nil, "us")
	if err := p.Pin(context.Background(), "github", "acme", "us"); !errors.Is(err, ErrRegionDisabled) {
		t.Fatalf("Pin with nil queries = %v, want ErrRegionDisabled", err)
	}
	if got := p.CellRegion(); got != "us" {
		t.Fatalf("CellRegion() = %q, want %q", got, "us")
	}
}

// A nil *RegionPinner must refuse rather than panic — the disabled-cell
// wiring hands one straight through.
func TestPin_NilReceiverRefuses(t *testing.T) {
	var p *RegionPinner
	if err := p.Pin(context.Background(), "github", "acme", "us"); !errors.Is(err, ErrRegionDisabled) {
		t.Fatalf("nil pinner Pin = %v, want ErrRegionDisabled", err)
	}
	if p.Enabled() || p.CellRegion() != "" {
		t.Fatal("nil pinner reported itself enabled")
	}
}

// The residency self-check: a signature is not authority to record another
// region's account here.
func TestPin_RefusesForeignRegion(t *testing.T) {
	q := &fakePinQueries{}
	p := NewRegionPinner(q, "us")

	err := p.Pin(context.Background(), "github", "acme", "eu")
	if !errors.Is(err, ErrRegionMismatch) {
		t.Fatalf("Pin of a foreign region = %v, want ErrRegionMismatch", err)
	}
	if q.pinCalls != 0 {
		t.Fatalf("mismatched pin issued %d queries, want 0", q.pinCalls)
	}
}

func TestPin_RefusesEmptyIdentity(t *testing.T) {
	q := &fakePinQueries{}
	p := NewRegionPinner(q, "us")

	for _, tc := range []struct{ name, provider, key string }{
		{"empty provider", "", "acme"},
		{"empty account key", "github", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := p.Pin(context.Background(), tc.provider, tc.key, "us"); !errors.Is(err, ErrInvalidPin) {
				t.Fatalf("Pin(%q,%q) = %v, want ErrInvalidPin", tc.provider, tc.key, err)
			}
		})
	}
	if q.pinCalls != 0 {
		t.Fatalf("invalid pin issued %d queries, want 0", q.pinCalls)
	}
}

// A non-ErrNoRows database fault propagates: an unclassifiable failure must
// never read as a successful pin.
func TestPin_PropagatesQueryError(t *testing.T) {
	boom := errors.New("connection reset")
	p := NewRegionPinner(&fakePinQueries{pinErr: boom}, "us")

	err := p.Pin(context.Background(), "github", "acme", "us")
	if !errors.Is(err, boom) {
		t.Fatalf("Pin = %v, want the underlying query error", err)
	}
}

// A read fault while classifying a zero-row miss is likewise propagated
// rather than collapsing into one of the typed refusals.
func TestPin_PropagatesClassifyError(t *testing.T) {
	boom := errors.New("read timeout")
	p := NewRegionPinner(&fakePinQueries{pinErr: pgx.ErrNoRows, getErr: boom}, "us")

	err := p.Pin(context.Background(), "github", "acme", "us")
	switch {
	case !errors.Is(err, boom):
		t.Fatalf("Pin = %v, want the classify read error", err)
	case errors.Is(err, ErrUnknownAccount) || errors.Is(err, ErrAlreadyPinned):
		t.Fatalf("Pin = %v, want an unclassified error rather than a typed refusal", err)
	}
}

// --- real-Postgres behavior -------------------------------------------------

// seedAccount inserts an accounts row and returns its (provider, key).
func seedAccount(t *testing.T, pool *pgxpool.Pool, provider, key string, region *string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO accounts (id, provider, account_key, granularity, home_region)
		 VALUES ($1, $2, $3, 'organization', $4)`,
		uuid.New(), provider, key, region)
	if err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

func readRegion(t *testing.T, pool *pgxpool.Pool, provider, key string) *string {
	t.Helper()
	var region *string
	err := pool.QueryRow(context.Background(),
		`SELECT home_region FROM accounts WHERE provider = $1 AND account_key = $2`,
		provider, key).Scan(&region)
	if err != nil {
		t.Fatalf("read home_region: %v", err)
	}
	return region
}

func TestPin_StampsUnpinnedAccount(t *testing.T) {
	pool := pgtest.NewPool(t)
	seedAccount(t, pool, "github", "acme", nil)
	p := NewRegionPinner(accountdb.New(pool), "us")

	if err := p.Pin(context.Background(), "github", "acme", "us"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	got := readRegion(t, pool, "github", "acme")
	if got == nil || *got != "us" {
		t.Fatalf("home_region = %v, want \"us\"", got)
	}

	// Idempotence is what makes a replayed handoff harmless (ADR-062 A2.3,
	// approval condition 3): the same pin again is a no-op, not an error.
	if err := p.Pin(context.Background(), "github", "acme", "us"); err != nil {
		t.Fatalf("re-Pin of the same region: %v, want nil", err)
	}
	if got := readRegion(t, pool, "github", "acme"); got == nil || *got != "us" {
		t.Fatalf("home_region after replay = %v, want \"us\"", got)
	}
}

// The UPDATE-only guarantee (approval condition 4): a handoff for an account
// this cell has never heard of is REFUSED, never silently created.
func TestPin_RefusesUnknownAccountAndCreatesNothing(t *testing.T) {
	pool := pgtest.NewPool(t)
	p := NewRegionPinner(accountdb.New(pool), "us")

	err := p.Pin(context.Background(), "github", "ghost", "us")
	if !errors.Is(err, ErrUnknownAccount) {
		t.Fatalf("Pin of an unknown account = %v, want ErrUnknownAccount", err)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM accounts WHERE provider = 'github' AND account_key = 'ghost'`).Scan(&n); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if n != 0 {
		t.Fatalf("refused pin created %d account rows, want 0", n)
	}
}

// First-write-wins, sequentially: an account already homed elsewhere is not
// re-homed by a later handoff.
func TestPin_RefusesAlreadyPinnedElsewhere(t *testing.T) {
	pool := pgtest.NewPool(t)
	eu := "eu"
	seedAccount(t, pool, "github", "acme", &eu)
	p := NewRegionPinner(accountdb.New(pool), "us")

	err := p.Pin(context.Background(), "github", "acme", "us")
	if !errors.Is(err, ErrAlreadyPinned) {
		t.Fatalf("Pin of an account homed elsewhere = %v, want ErrAlreadyPinned", err)
	}
	if got := readRegion(t, pool, "github", "acme"); got == nil || *got != "eu" {
		t.Fatalf("home_region = %v, want it unchanged at \"eu\"", got)
	}
}

// The concurrency property the plan calls out (ADR-062 A2.3): two pinners in
// different regions race for the same unpinned account. Exactly one wins, and
// the persisted value never moves afterwards. A sequential or fake-backed test
// does not discharge this — the guarantee lives in the WHERE clause and the
// row lock Postgres takes on it.
func TestPin_ConcurrentPinsFirstWriteWins(t *testing.T) {
	pool := pgtest.NewPool(t)
	seedAccount(t, pool, "github", "acme", nil)

	regions := []string{"us", "eu", "ap", "sa"}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs = map[string]error{}
	)
	start := make(chan struct{})
	for _, region := range regions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := NewRegionPinner(accountdb.New(pool), region)
			<-start
			err := p.Pin(context.Background(), "github", "acme", region)
			mu.Lock()
			errs[region] = err
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	var winners []string
	for _, region := range regions {
		switch err := errs[region]; {
		case err == nil:
			winners = append(winners, region)
		case errors.Is(err, ErrAlreadyPinned):
			// The expected loss.
		default:
			t.Fatalf("pin from %q = %v, want nil or ErrAlreadyPinned", region, err)
		}
	}
	if len(winners) != 1 {
		t.Fatalf("winners = %v, want exactly one", winners)
	}

	stored := readRegion(t, pool, "github", "acme")
	if stored == nil || *stored != winners[0] {
		t.Fatalf("home_region = %v, want the winning region %q", stored, winners[0])
	}

	// And it stays put: a later pin from a losing region is still refused.
	loser := "eu"
	if winners[0] == "eu" {
		loser = "us"
	}
	p := NewRegionPinner(accountdb.New(pool), loser)
	if err := p.Pin(context.Background(), "github", "acme", loser); !errors.Is(err, ErrAlreadyPinned) {
		t.Fatalf("post-race pin from %q = %v, want ErrAlreadyPinned", loser, err)
	}
	if got := readRegion(t, pool, "github", "acme"); got == nil || *got != winners[0] {
		t.Fatalf("home_region = %v, want it stable at %q", got, winners[0])
	}
}
