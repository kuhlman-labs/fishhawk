package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// Single-tenant deployment profile (ADR-057 Mode 1, E44.9 / #1833).
//
// The multi-tenant core (E44.1–E44.8) admits a sign-in only against an
// EXISTING accounts row: auth.MembershipResolver returns an empty set — a
// denial — when no account matches. Nothing in the product creates that first
// row, so a fresh self-hosted install has no admitting account and no way to
// make one short of hand-written SQL.
//
// The single-tenant profile closes that gap by short-circuiting tenancy to ONE
// implicit tenant: a boot-time, idempotent upsert of a single account carrying
// an auto_join_role, so every member of the customer's enterprise / org /
// group auto-joins it through the existing login gate. No new admission logic
// — the same E44.3 / E44.8 walk, with exactly one account to match.
//
// ENABLEMENT IS THE ACCOUNT KEY, AND ONLY THE ACCOUNT KEY. Every
// FISHHAWKD_SINGLE_TENANT_* flag defaults to EMPTY; the github / enterprise /
// member defaults below are applied INTERNALLY, after enablement. That makes
// the three deployment states unambiguous:
//
//   - nothing set                       → bootstrap skipped, hosted
//     multi-tenant behavior unchanged.
//   - account key set                   → bootstrap runs, omitted fields
//     filled from the internal defaults.
//   - another field set, key EMPTY      → startup ERROR naming the missing
//     --single-tenant-account-key.
//
// The third state is the one that matters: silently treating a
// half-configured profile as "hosted" boots a deployment with no admitting
// account, in which nobody can sign in and nothing says why.

// Internal defaults for an ENABLED single-tenant profile. These are applied by
// resolveDefaults, never as flag defaults — see the enablement note above.
const (
	// DefaultSingleTenantProvider matches accounts.provider's own column
	// default (migration 0052).
	DefaultSingleTenantProvider = "github"
	// DefaultSingleTenantGranularity is the GitHub Enterprise posture a
	// self-hosted (GHES / EMU) install almost always wants.
	DefaultSingleTenantGranularity = "enterprise"
	// DefaultSingleTenantAutoJoinRole is the least-privileged role the
	// login gate can mint a grant with.
	DefaultSingleTenantAutoJoinRole = "member"
)

// ErrSingleTenantMissingAccountKey reports a partially-configured profile:
// at least one single-tenant field is set while the account key — the sole
// enablement signal — is empty. Fail closed rather than degrading to hosted
// mode, which would boot a deployment nobody can sign in to.
var ErrSingleTenantMissingAccountKey = errors.New(
	"account: single-tenant profile is partially configured: set --single-tenant-account-key (FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY) or unset every other --single-tenant-* flag")

// singleTenantGranularities / singleTenantProviders mirror the
// accounts_granularity_check / accounts_provider_check CHECK constraints
// (migrations 0052 / 0055). Validating in Go turns a raw SQLSTATE 23514 at
// boot into a message naming the flag and the accepted values.
var (
	singleTenantGranularities = []string{"enterprise", "organization", "group"}
	singleTenantProviders     = []string{"github", "gitlab"}
)

// SingleTenantConfig is the deployment configuration of the profile, as
// resolved from the FISHHAWKD_SINGLE_TENANT_* flags/env.
type SingleTenantConfig struct {
	// Provider is the forge discriminator ("github" | "gitlab").
	Provider string
	// AccountKey is the forge-neutral natural key (enterprise slug, org
	// login, GitLab group path) AND the profile's enablement signal.
	AccountKey string
	// DisplayName is cosmetic; empty stores NULL.
	DisplayName string
	// Granularity is the tier the account_key names ("enterprise" |
	// "organization" | "group").
	Granularity string
	// AutoJoinRole is the role auto-joined members are granted. It must be
	// non-empty: ListAutoJoinAccountsByKeys selects only accounts whose
	// auto_join_role IS NOT NULL, so a NULL role is invisible to the login
	// gate and the account would admit nobody.
	AutoJoinRole string
}

// Enabled reports whether the profile is configured. The account key alone is
// the signal.
func (c SingleTenantConfig) Enabled() bool {
	return strings.TrimSpace(c.AccountKey) != ""
}

// configured reports whether ANY single-tenant field carries a value — the
// input to the partial-configuration guard.
func (c SingleTenantConfig) configured() bool {
	return strings.TrimSpace(c.Provider) != "" ||
		strings.TrimSpace(c.AccountKey) != "" ||
		strings.TrimSpace(c.DisplayName) != "" ||
		strings.TrimSpace(c.Granularity) != "" ||
		strings.TrimSpace(c.AutoJoinRole) != ""
}

// resolveDefaults trims every field and fills the internal defaults for an
// enabled profile. Applied only AFTER enablement, so an unset deployment never
// acquires a populated config it did not ask for.
func (c SingleTenantConfig) resolveDefaults() SingleTenantConfig {
	out := SingleTenantConfig{
		Provider:     strings.TrimSpace(c.Provider),
		AccountKey:   strings.TrimSpace(c.AccountKey),
		DisplayName:  strings.TrimSpace(c.DisplayName),
		Granularity:  strings.TrimSpace(c.Granularity),
		AutoJoinRole: strings.TrimSpace(c.AutoJoinRole),
	}
	if out.Provider == "" {
		out.Provider = DefaultSingleTenantProvider
	}
	if out.Granularity == "" {
		out.Granularity = DefaultSingleTenantGranularity
	}
	if out.AutoJoinRole == "" {
		out.AutoJoinRole = DefaultSingleTenantAutoJoinRole
	}
	return out
}

// Validate checks a RESOLVED profile (post-resolveDefaults) against the
// database's own CHECK constraints and the login gate's requirements. It is
// exported so a caller constructing the struct directly — not only
// EnsureSingleTenantAccount — gets the same fail-closed guarantees; through
// the flag path the empty-provider / empty-granularity / empty-role branches
// are unreachable because resolveDefaults has filled them.
func (c SingleTenantConfig) Validate() error {
	if c.AccountKey == "" {
		return ErrSingleTenantMissingAccountKey
	}
	if !slices.Contains(singleTenantProviders, c.Provider) {
		return fmt.Errorf("account: single-tenant provider %q is not one of %s (--single-tenant-provider)",
			c.Provider, strings.Join(singleTenantProviders, ", "))
	}
	if !slices.Contains(singleTenantGranularities, c.Granularity) {
		return fmt.Errorf("account: single-tenant granularity %q is not one of %s (--single-tenant-granularity)",
			c.Granularity, strings.Join(singleTenantGranularities, ", "))
	}
	if c.AutoJoinRole == "" {
		// A NULL auto_join_role is invisible to ListAutoJoinAccountsByKeys,
		// so the bootstrapped account would admit nobody — the exact failure
		// this profile exists to prevent.
		return errors.New("account: single-tenant auto-join role must be non-empty (--single-tenant-auto-join-role); an account with no auto-join role admits nobody")
	}
	return nil
}

// SingleTenantQueries is the single query surface the bootstrap needs.
// *accountdb.Queries (accountdb.New(pool)) satisfies it; tests inject a stub
// for the branches that need no database.
type SingleTenantQueries interface {
	UpsertSingleTenantAccount(ctx context.Context, arg accountdb.UpsertSingleTenantAccountParams) (accountdb.Account, error)
}

var _ SingleTenantQueries = (*accountdb.Queries)(nil)

// EnsureSingleTenantAccount performs the boot-time bootstrap.
//
// Returns (id, true, nil) when the profile is enabled and the account is in
// place, (uuid.Nil, false, nil) when the profile is unconfigured (the hosted
// posture — no write, no log), and an error on any fail-closed branch:
// a partially-configured profile, an invalid provider/granularity/role, or a
// DB write failure. Every error is a startup abort at the call site: booting
// into a deployment with no admitting account is worse than not booting.
//
// The write is idempotent — ON CONFLICT (provider, account_key) DO UPDATE — so
// every restart converges the row on the configured profile without minting a
// second account, and home_region is left untouched for PinAccountHomeRegion.
func EnsureSingleTenantAccount(ctx context.Context, q SingleTenantQueries, cfg SingleTenantConfig, logger *slog.Logger) (uuid.UUID, bool, error) {
	if !cfg.Enabled() {
		if cfg.configured() {
			return uuid.Nil, false, ErrSingleTenantMissingAccountKey
		}
		return uuid.Nil, false, nil
	}

	resolved := cfg.resolveDefaults()
	if err := resolved.Validate(); err != nil {
		return uuid.Nil, false, err
	}
	if q == nil {
		return uuid.Nil, false, errors.New("account: single-tenant profile is configured but no database is available (set FISHHAWKD_DATABASE_URL)")
	}

	var displayName *string
	if resolved.DisplayName != "" {
		name := resolved.DisplayName
		displayName = &name
	}
	role := resolved.AutoJoinRole

	row, err := q.UpsertSingleTenantAccount(ctx, accountdb.UpsertSingleTenantAccountParams{
		ID:           uuid.New(),
		Provider:     resolved.Provider,
		AccountKey:   resolved.AccountKey,
		DisplayName:  displayName,
		Granularity:  resolved.Granularity,
		AutoJoinRole: &role,
	})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("account: single-tenant bootstrap upsert: %w", err)
	}

	if logger != nil {
		logger.Info("single-tenant profile bootstrapped; every member of this account auto-joins at sign-in",
			slog.String("account_id", row.ID.String()),
			slog.String("provider", resolved.Provider),
			slog.String("account_key", resolved.AccountKey),
			slog.String("granularity", resolved.Granularity),
			slog.String("auto_join_role", resolved.AutoJoinRole),
			slog.String("ref", "#1833"))
	}
	return row.ID, true, nil
}
