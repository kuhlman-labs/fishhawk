package account

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// The single-tenant profile is a DEPLOYMENT-CONFIG change, so the create /
// idempotence / update cases run against a REAL migrated database through the
// production accountdb.Queries: a comment-only or no-op bootstrap fails them
// where a presence gate would pass. The fail-closed branches that need no
// database (validation, partial configuration, write error) use a stub.

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubSingleTenantQueries records the upsert params and can fail the write.
type stubSingleTenantQueries struct {
	calls []accountdb.UpsertSingleTenantAccountParams
	err   error
}

func (s *stubSingleTenantQueries) UpsertSingleTenantAccount(_ context.Context, arg accountdb.UpsertSingleTenantAccountParams) (accountdb.Account, error) {
	s.calls = append(s.calls, arg)
	if s.err != nil {
		return accountdb.Account{}, s.err
	}
	return accountdb.Account{ID: arg.ID, Provider: arg.Provider, AccountKey: arg.AccountKey}, nil
}

// TestSingleTenantConfig_Validate walks every fail-closed validation branch,
// each with its own assertion (per-failure-mode rule). Validate operates on a
// RESOLVED config, which is why the empty-provider/granularity/role cases are
// stated directly here rather than through the flag path.
func TestSingleTenantConfig_Validate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     SingleTenantConfig
		wantErr string
	}{
		{
			name:    "empty account key",
			cfg:     SingleTenantConfig{Provider: "github", Granularity: "enterprise", AutoJoinRole: "member"},
			wantErr: "--single-tenant-account-key",
		},
		{
			name:    "granularity outside the CHECK constraint",
			cfg:     SingleTenantConfig{Provider: "github", AccountKey: "acme", Granularity: "team", AutoJoinRole: "member"},
			wantErr: "--single-tenant-granularity",
		},
		{
			name:    "provider outside the CHECK constraint",
			cfg:     SingleTenantConfig{Provider: "bitbucket", AccountKey: "acme", Granularity: "enterprise", AutoJoinRole: "member"},
			wantErr: "--single-tenant-provider",
		},
		{
			name:    "empty provider",
			cfg:     SingleTenantConfig{AccountKey: "acme", Granularity: "enterprise", AutoJoinRole: "member"},
			wantErr: "--single-tenant-provider",
		},
		{
			name:    "empty granularity",
			cfg:     SingleTenantConfig{Provider: "github", AccountKey: "acme", AutoJoinRole: "member"},
			wantErr: "--single-tenant-granularity",
		},
		{
			name:    "empty auto-join role admits nobody",
			cfg:     SingleTenantConfig{Provider: "github", AccountKey: "acme", Granularity: "enterprise"},
			wantErr: "--single-tenant-auto-join-role",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error naming %s", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name %s", err, tc.wantErr)
			}
		})
	}

	valid := SingleTenantConfig{Provider: "gitlab", AccountKey: "acme/platform", Granularity: "group", AutoJoinRole: "member"}
	if err := valid.Validate(); err != nil {
		t.Errorf("Validate() on a valid profile = %v, want nil", err)
	}
}

// (a) Nothing set: the bootstrap is skipped and NOTHING is written — the
// hosted multi-tenant posture, unchanged.
func TestEnsureSingleTenantAccount_UnconfiguredSkips(t *testing.T) {
	q := &stubSingleTenantQueries{}
	id, bootstrapped, err := EnsureSingleTenantAccount(context.Background(), q, SingleTenantConfig{}, testLogger())
	if err != nil {
		t.Fatalf("EnsureSingleTenantAccount: %v", err)
	}
	if bootstrapped {
		t.Errorf("bootstrapped = true, want false for an unconfigured profile")
	}
	if id != uuid.Nil {
		t.Errorf("id = %s, want uuid.Nil", id)
	}
	if len(q.calls) != 0 {
		t.Errorf("upsert called %d times, want 0 — an unconfigured deployment must not write", len(q.calls))
	}
}

// (b) Key set, everything else omitted: the internal defaults fill in.
func TestEnsureSingleTenantAccount_AppliesInternalDefaults(t *testing.T) {
	q := &stubSingleTenantQueries{}
	_, bootstrapped, err := EnsureSingleTenantAccount(context.Background(), q,
		SingleTenantConfig{AccountKey: "acme-corp"}, testLogger())
	if err != nil {
		t.Fatalf("EnsureSingleTenantAccount: %v", err)
	}
	if !bootstrapped {
		t.Fatalf("bootstrapped = false, want true — the account key alone enables the profile")
	}
	if len(q.calls) != 1 {
		t.Fatalf("upsert called %d times, want 1", len(q.calls))
	}
	got := q.calls[0]
	if got.Provider != DefaultSingleTenantProvider {
		t.Errorf("provider = %q, want the internal default %q", got.Provider, DefaultSingleTenantProvider)
	}
	if got.Granularity != DefaultSingleTenantGranularity {
		t.Errorf("granularity = %q, want the internal default %q", got.Granularity, DefaultSingleTenantGranularity)
	}
	if got.AutoJoinRole == nil || *got.AutoJoinRole != DefaultSingleTenantAutoJoinRole {
		t.Errorf("auto_join_role = %v, want the internal default %q", got.AutoJoinRole, DefaultSingleTenantAutoJoinRole)
	}
	if got.DisplayName != nil {
		t.Errorf("display_name = %v, want NULL when unset", *got.DisplayName)
	}
}

// (c) A single-tenant field set with the account key EMPTY is a hard error
// naming the missing key — never a silent fall-through to hosted mode, which
// would boot a deployment with no admitting account.
func TestEnsureSingleTenantAccount_PartialConfigurationErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  SingleTenantConfig
	}{
		{"granularity without a key", SingleTenantConfig{Granularity: "organization"}},
		{"auto-join role without a key", SingleTenantConfig{AutoJoinRole: "admin"}},
		{"provider without a key", SingleTenantConfig{Provider: "gitlab"}},
		{"display name without a key", SingleTenantConfig{DisplayName: "Acme"}},
		{"whitespace-only key with another field set", SingleTenantConfig{AccountKey: "   ", Granularity: "group"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &stubSingleTenantQueries{}
			_, bootstrapped, err := EnsureSingleTenantAccount(context.Background(), q, tc.cfg, testLogger())
			if !errors.Is(err, ErrSingleTenantMissingAccountKey) {
				t.Fatalf("error = %v, want ErrSingleTenantMissingAccountKey", err)
			}
			if bootstrapped {
				t.Errorf("bootstrapped = true, want false")
			}
			if len(q.calls) != 0 {
				t.Errorf("upsert called %d times, want 0", len(q.calls))
			}
		})
	}
}

// An invalid granularity is refused BEFORE the write, so the operator sees a
// message naming the flag rather than a raw SQLSTATE 23514 at boot.
func TestEnsureSingleTenantAccount_InvalidGranularityErrorsBeforeWrite(t *testing.T) {
	q := &stubSingleTenantQueries{}
	_, _, err := EnsureSingleTenantAccount(context.Background(), q,
		SingleTenantConfig{AccountKey: "acme", Granularity: "team"}, testLogger())
	if err == nil {
		t.Fatal("error = nil, want an invalid-granularity error")
	}
	if len(q.calls) != 0 {
		t.Errorf("upsert called %d times, want 0 — validation must precede the write", len(q.calls))
	}
}

// A configured profile with NO database is a startup error, not a silent skip.
func TestEnsureSingleTenantAccount_ConfiguredWithoutQueriesErrors(t *testing.T) {
	_, bootstrapped, err := EnsureSingleTenantAccount(context.Background(), nil,
		SingleTenantConfig{AccountKey: "acme"}, testLogger())
	if err == nil {
		t.Fatal("error = nil, want a no-database error")
	}
	if bootstrapped {
		t.Errorf("bootstrapped = true, want false")
	}
}

// A DB write error propagates rather than returning a zero id and a healthy
// boot.
func TestEnsureSingleTenantAccount_WriteErrorPropagates(t *testing.T) {
	q := &stubSingleTenantQueries{err: errors.New("connection refused")}
	id, bootstrapped, err := EnsureSingleTenantAccount(context.Background(), q,
		SingleTenantConfig{AccountKey: "acme"}, testLogger())
	if err == nil {
		t.Fatal("error = nil, want the write error propagated")
	}
	if bootstrapped || id != uuid.Nil {
		t.Errorf("got (%s, %v), want (uuid.Nil, false) on a write failure", id, bootstrapped)
	}
}

// Behavioral, against a real database: the row is created with a NON-NULL
// auto_join_role and the granularity actually requested; a second boot returns
// the SAME id with no duplicate row; and a changed role/granularity is updated
// in place.
func TestEnsureSingleTenantAccount_CreatesAndIsIdempotent(t *testing.T) {
	pool := pgtest.NewPool(t)
	q := accountdb.New(pool)
	ctx := context.Background()

	id, bootstrapped, err := EnsureSingleTenantAccount(ctx, q, SingleTenantConfig{
		AccountKey:   "acme-corp",
		Granularity:  "organization",
		AutoJoinRole: "member",
		DisplayName:  "Acme Corp",
	}, testLogger())
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if !bootstrapped || id == uuid.Nil {
		t.Fatalf("first boot returned (%s, %v), want a real id and true", id, bootstrapped)
	}

	var granularity string
	var role, displayName *string
	if err := pool.QueryRow(ctx,
		`SELECT granularity, auto_join_role, display_name FROM accounts WHERE id = $1`, id,
	).Scan(&granularity, &role, &displayName); err != nil {
		t.Fatalf("read bootstrapped account: %v", err)
	}
	if granularity != "organization" {
		t.Errorf("granularity = %q, want the requested organization", granularity)
	}
	if role == nil || *role != "member" {
		t.Fatalf("auto_join_role = %v, want NON-NULL 'member' (a NULL role admits nobody)", role)
	}
	if displayName == nil || *displayName != "Acme Corp" {
		t.Errorf("display_name = %v, want 'Acme Corp'", displayName)
	}

	// Second boot: same identity, no duplicate, role + granularity converged
	// on the new configuration.
	id2, _, err := EnsureSingleTenantAccount(ctx, q, SingleTenantConfig{
		AccountKey:   "acme-corp",
		Granularity:  "enterprise",
		AutoJoinRole: "admin",
		DisplayName:  "Acme Corp",
	}, testLogger())
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	if id2 != id {
		t.Errorf("second boot id = %s, want the same account id %s (the upsert must not mint a second account)", id2, id)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM accounts WHERE provider = 'github' AND account_key = 'acme-corp'`,
	).Scan(&rows); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if rows != 1 {
		t.Errorf("accounts rows = %d, want exactly 1", rows)
	}

	if err := pool.QueryRow(ctx,
		`SELECT granularity, auto_join_role FROM accounts WHERE id = $1`, id,
	).Scan(&granularity, &role); err != nil {
		t.Fatalf("re-read bootstrapped account: %v", err)
	}
	if granularity != "enterprise" {
		t.Errorf("granularity = %q, want the updated enterprise", granularity)
	}
	if role == nil || *role != "admin" {
		t.Errorf("auto_join_role = %v, want the updated 'admin'", role)
	}
}

// The bootstrap must never clear a region pin: PinAccountHomeRegion owns
// home_region (first-write-wins), and this upsert re-runs on every restart.
func TestEnsureSingleTenantAccount_LeavesHomeRegionAlone(t *testing.T) {
	pool := pgtest.NewPool(t)
	q := accountdb.New(pool)
	ctx := context.Background()
	cfg := SingleTenantConfig{AccountKey: "acme-corp", Granularity: "enterprise", AutoJoinRole: "member"}

	id, _, err := EnsureSingleTenantAccount(ctx, q, cfg, testLogger())
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if err := NewRegionPinner(q, "eu").Pin(ctx, "github", "acme-corp", "eu"); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	if _, _, err := EnsureSingleTenantAccount(ctx, q, cfg, testLogger()); err != nil {
		t.Fatalf("restart boot: %v", err)
	}

	var region *string
	if err := pool.QueryRow(ctx, `SELECT home_region FROM accounts WHERE id = $1`, id).Scan(&region); err != nil {
		t.Fatalf("read home_region: %v", err)
	}
	if region == nil || *region != "eu" {
		t.Errorf("home_region = %v, want the pin 'eu' preserved across the restart bootstrap", region)
	}
}
