package account_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// fakeRegionQuerier is an in-memory accounts table keyed by (provider,
// account_key), mirroring UpsertAccount's ON CONFLICT semantics.
type fakeRegionQuerier struct {
	rows      map[string]accountdb.Account
	getErr    error
	upsertErr error
	upserts   []accountdb.UpsertAccountParams
}

func newFakeRegionQuerier() *fakeRegionQuerier {
	return &fakeRegionQuerier{rows: map[string]accountdb.Account{}}
}

func (f *fakeRegionQuerier) key(provider, accountKey string) string {
	return provider + "\x00" + accountKey
}

func (f *fakeRegionQuerier) seed(provider, accountKey string, region *string) accountdb.Account {
	name := "Seeded"
	a := accountdb.Account{
		ID:          uuid.New(),
		Provider:    provider,
		AccountKey:  accountKey,
		DisplayName: &name,
		Granularity: "organization",
		HomeRegion:  region,
	}
	f.rows[f.key(provider, accountKey)] = a
	return a
}

func (f *fakeRegionQuerier) GetAccountByKey(_ context.Context, arg accountdb.GetAccountByKeyParams) (accountdb.Account, error) {
	if f.getErr != nil {
		return accountdb.Account{}, f.getErr
	}
	a, ok := f.rows[f.key(arg.Provider, arg.AccountKey)]
	if !ok {
		return accountdb.Account{}, pgx.ErrNoRows
	}
	return a, nil
}

func (f *fakeRegionQuerier) UpsertAccount(_ context.Context, arg accountdb.UpsertAccountParams) (accountdb.Account, error) {
	f.upserts = append(f.upserts, arg)
	if f.upsertErr != nil {
		return accountdb.Account{}, f.upsertErr
	}
	a := accountdb.Account{
		ID:          arg.ID,
		Provider:    arg.Provider,
		AccountKey:  arg.AccountKey,
		DisplayName: arg.DisplayName,
		Granularity: arg.Granularity,
		HomeRegion:  arg.HomeRegion,
	}
	f.rows[f.key(arg.Provider, arg.AccountKey)] = a
	return a, nil
}

func ptr(s string) *string { return &s }

func TestIsSupportedRegion(t *testing.T) {
	for _, r := range []string{"us", "eu", "au", "EU", "  us  "} {
		if !account.IsSupportedRegion(r) {
			t.Errorf("IsSupportedRegion(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"", "  ", "uk", "us-east-1", "europe"} {
		if account.IsSupportedRegion(r) {
			t.Errorf("IsSupportedRegion(%q) = true, want false", r)
		}
	}
}

// Happy path: an account with no row yet gets created with home_region set.
func TestPinCreatesAccountWithRegion(t *testing.T) {
	q := newFakeRegionQuerier()
	p := account.NewRegionPinner(q, "eu")

	got, err := p.Pin(context.Background(), account.PinParams{
		Provider: "github", AccountKey: "acme", DisplayName: "Acme", Region: "eu",
	})
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if got.HomeRegion == nil || *got.HomeRegion != "eu" {
		t.Fatalf("home_region: got %v want eu", got.HomeRegion)
	}
	if len(q.upserts) != 1 {
		t.Fatalf("upserts: got %d want 1", len(q.upserts))
	}
	if q.upserts[0].Granularity != "enterprise" {
		t.Errorf("granularity: got %q", q.upserts[0].Granularity)
	}
	if q.upserts[0].DisplayName == nil || *q.upserts[0].DisplayName != "Acme" {
		t.Errorf("display_name: got %v", q.upserts[0].DisplayName)
	}
}

// Replay bound, NULL branch: an existing row with home_region NULL accepts the
// pin, and the pin does not clobber display_name / granularity.
func TestPinStampsNullRegionWithoutClobbering(t *testing.T) {
	q := newFakeRegionQuerier()
	seeded := q.seed("github", "acme", nil)
	p := account.NewRegionPinner(q, "eu")

	got, err := p.Pin(context.Background(), account.PinParams{
		Provider: "github", AccountKey: "acme", DisplayName: "Ignored", Region: "EU",
	})
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if got.ID != seeded.ID {
		t.Errorf("id: got %s want %s (must update in place, not create a second row)", got.ID, seeded.ID)
	}
	if got.HomeRegion == nil || *got.HomeRegion != "eu" {
		t.Fatalf("home_region: got %v want eu", got.HomeRegion)
	}
	if got.Granularity != "organization" {
		t.Errorf("granularity clobbered: got %q want organization", got.Granularity)
	}
	if got.DisplayName == nil || *got.DisplayName != "Seeded" {
		t.Errorf("display_name clobbered: got %v want Seeded", got.DisplayName)
	}
}

// Replay bound, equal branch: replaying the SAME pin is idempotent.
func TestPinIsIdempotentForTheSameRegion(t *testing.T) {
	q := newFakeRegionQuerier()
	q.seed("github", "acme", ptr("eu"))
	p := account.NewRegionPinner(q, "eu")

	for i := range 2 {
		got, err := p.Pin(context.Background(), account.PinParams{Provider: "github", AccountKey: "acme", Region: "eu"})
		if err != nil {
			t.Fatalf("Pin #%d: %v", i, err)
		}
		if got.HomeRegion == nil || *got.HomeRegion != "eu" {
			t.Fatalf("Pin #%d home_region: got %v", i, got.HomeRegion)
		}
	}
}

// Replay bound, conflict branch: a pin can never MOVE an account's region.
func TestPinRejectsRegionMove(t *testing.T) {
	q := newFakeRegionQuerier()
	q.seed("github", "acme", ptr("eu"))
	// A cell with no region tag, so the residency self-check cannot be the
	// thing doing the rejecting here — the replay bound must be.
	p := account.NewRegionPinner(q, "")

	_, err := p.Pin(context.Background(), account.PinParams{Provider: "github", AccountKey: "acme", Region: "us"})
	if !errors.Is(err, account.ErrRegionConflict) {
		t.Fatalf("got %v, want ErrRegionConflict", err)
	}
	if len(q.upserts) != 0 {
		t.Fatalf("a rejected pin must issue no write; got %d upserts", len(q.upserts))
	}
}

// Residency invariant: a valid EU pin reaching a US cell fails closed.
func TestPinRejectsForeignRegion(t *testing.T) {
	q := newFakeRegionQuerier()
	p := account.NewRegionPinner(q, "us")

	_, err := p.Pin(context.Background(), account.PinParams{Provider: "github", AccountKey: "acme", Region: "eu"})
	if !errors.Is(err, account.ErrRegionForeign) {
		t.Fatalf("got %v, want ErrRegionForeign", err)
	}
	if len(q.upserts) != 0 {
		t.Fatalf("a foreign pin must issue no write; got %d upserts", len(q.upserts))
	}
}

// An untagged cell (single-region deployment) skips the residency check.
func TestPinAcceptsAnyRegionOnUntaggedCell(t *testing.T) {
	q := newFakeRegionQuerier()
	p := account.NewRegionPinner(q, "")
	if _, err := p.Pin(context.Background(), account.PinParams{Provider: "github", AccountKey: "acme", Region: "au"}); err != nil {
		t.Fatalf("Pin: %v", err)
	}
}

func TestPinRejectsUnsupportedAndIncompleteInput(t *testing.T) {
	tests := []struct {
		name string
		in   account.PinParams
	}{
		{"empty_region", account.PinParams{Provider: "github", AccountKey: "acme", Region: ""}},
		{"unknown_region", account.PinParams{Provider: "github", AccountKey: "acme", Region: "uk"}},
		{"blank_provider", account.PinParams{Provider: " ", AccountKey: "acme", Region: "eu"}},
		{"blank_account_key", account.PinParams{Provider: "github", AccountKey: "", Region: "eu"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := newFakeRegionQuerier()
			p := account.NewRegionPinner(q, "eu")
			if _, err := p.Pin(context.Background(), tc.in); !errors.Is(err, account.ErrRegionUnsupported) {
				t.Fatalf("got %v, want ErrRegionUnsupported", err)
			}
			if len(q.upserts) != 0 {
				t.Fatalf("must issue no write; got %d upserts", len(q.upserts))
			}
		})
	}
}

// A real DB read fault propagates rather than creating a duplicate row.
func TestPinPropagatesLookupError(t *testing.T) {
	q := newFakeRegionQuerier()
	q.getErr = errors.New("connection reset")
	p := account.NewRegionPinner(q, "eu")

	_, err := p.Pin(context.Background(), account.PinParams{Provider: "github", AccountKey: "acme", Region: "eu"})
	if err == nil || !errors.Is(err, q.getErr) {
		t.Fatalf("got %v, want the underlying lookup error", err)
	}
	if len(q.upserts) != 0 {
		t.Fatalf("a failed lookup must not fall through to a write; got %d upserts", len(q.upserts))
	}
}

// The no-database posture reports unavailable rather than panicking.
func TestPinWithoutStoreIsUnavailable(t *testing.T) {
	for name, p := range map[string]*account.RegionPinner{
		"nil_pinner":   nil,
		"nil_querier":  account.NewRegionPinner(nil, "eu"),
		"typed_nil_ok": account.NewRegionPinner(nil, ""),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := p.Pin(context.Background(), account.PinParams{Provider: "github", AccountKey: "acme", Region: "eu"}); !errors.Is(err, account.ErrRegionUnavailable) {
				t.Fatalf("got %v, want ErrRegionUnavailable", err)
			}
		})
	}
}

func TestHomeRegionAccessor(t *testing.T) {
	if got := account.NewRegionPinner(newFakeRegionQuerier(), " EU ").HomeRegion(); got != "eu" {
		t.Fatalf("HomeRegion: got %q want eu", got)
	}
	var nilPinner *account.RegionPinner
	if got := nilPinner.HomeRegion(); got != "" {
		t.Fatalf("nil HomeRegion: got %q want empty", got)
	}
}
