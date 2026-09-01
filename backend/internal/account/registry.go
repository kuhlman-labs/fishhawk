package account

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// The operator registry surface (E45.33 / #2923). It is the supported write
// path for the ADR-057 tenancy rows that GitLab run creation is gated on:
// gitLabProjectRegistry.AuthorizedGitLabProject (backend/cmd/fishhawkd/serve.go)
// admits a GitLab trigger only when an installations row resolves to an accounts
// row whose account_key matches the project path's namespace segment — yet
// nothing shipped creates either row, so operators were reduced to hand-written
// SQL (docs/deploy/gitlab.md). CreateAccount / RegisterInstallation back the
// `fishhawkd account create|register` subcommands; the CLI files stay thin
// flag-parsing shells over this domain surface so the logic is unit-testable
// against a fake.
//
// Every input validation failure wraps ErrValidation and every account-does-not-
// exist refusal wraps ErrAccountNotFound, so the CLI can switch on error KIND
// (errors.Is) rather than string-matching a message that could be reworded.

// Default granularities for the operator registry, applied per provider when
// --granularity is omitted. A GitHub account is an organization; a GitLab
// namespace is a group.
const (
	DefaultGranularityGitHub = "organization"
	DefaultGranularityGitLab = "group"
)

// ErrValidation is wrapped by every operator-registry input validation failure
// (bad provider, empty account key, out-of-set granularity, malformed
// installation ref, non-https base URL). The CLI maps errors.Is(err,
// ErrValidation) to its usage exit code. Distinguishing it by KIND — not by
// message text — keeps the exit-code split from breaking silently the first
// time someone improves the wording.
var ErrValidation = errors.New("account: invalid registration input")

// ErrAccountNotFound is wrapped by RegisterInstallation's fail-closed refusal
// when the named account does not exist. It is a DISTINCT control from the
// composite (account_id, provider) foreign key: both reject the write, but only
// this produces an operator-actionable message naming the remedy, and the CLI
// maps it to its failure (not usage) exit code. A registration path that
// conjured the missing account instead would hollow out the whole gate — a
// GitLab payload is authenticated by a shared token with no signature over the
// body, so the project it names is untrusted and authorization must be an
// operator decision recorded server-side.
var ErrAccountNotFound = errors.New("account: named account does not exist")

// RegistryQueries is the query surface the operator registry needs.
// *accountdb.Queries (accountdb.New(pool)) satisfies it; tests inject a fake for
// the branches that need no database.
type RegistryQueries interface {
	GetAccountByKey(ctx context.Context, arg accountdb.GetAccountByKeyParams) (accountdb.Account, error)
	UpsertAccount(ctx context.Context, arg accountdb.UpsertAccountParams) (accountdb.Account, error)
	UpsertInstallation(ctx context.Context, arg accountdb.UpsertInstallationParams) (accountdb.Installation, error)
	ListAccounts(ctx context.Context) ([]accountdb.Account, error)
	ListInstallations(ctx context.Context) ([]accountdb.ListInstallationsRow, error)
}

var _ RegistryQueries = (*accountdb.Queries)(nil)

// CreateAccountRequest is the input to CreateAccount.
type CreateAccountRequest struct {
	Provider    string
	AccountKey  string
	DisplayName string
	Granularity string
}

// resolveDefaults trims every field and fills the per-provider default
// granularity when Granularity is empty. An unknown provider leaves Granularity
// empty so Validate's provider check fires first and names the offending value.
func (r CreateAccountRequest) resolveDefaults() CreateAccountRequest {
	out := CreateAccountRequest{
		Provider:    strings.TrimSpace(r.Provider),
		AccountKey:  strings.TrimSpace(r.AccountKey),
		DisplayName: strings.TrimSpace(r.DisplayName),
		Granularity: strings.TrimSpace(r.Granularity),
	}
	if out.Granularity == "" {
		switch out.Provider {
		case "github":
			out.Granularity = DefaultGranularityGitHub
		case "gitlab":
			out.Granularity = DefaultGranularityGitLab
		}
	}
	return out
}

// Validate checks a RESOLVED request against the accounts_provider_check /
// accounts_granularity_check CHECK constraints, each failure wrapping
// ErrValidation and naming the offending value.
func (r CreateAccountRequest) Validate() error {
	if r.AccountKey == "" {
		return fmt.Errorf("account: account key must be non-empty (--account-key): %w", ErrValidation)
	}
	if !slices.Contains(accountProviders, r.Provider) {
		return fmt.Errorf("account: provider %q is not one of %s (--provider): %w",
			r.Provider, strings.Join(accountProviders, ", "), ErrValidation)
	}
	if !slices.Contains(accountGranularities, r.Granularity) {
		return fmt.Errorf("account: granularity %q is not one of %s (--granularity): %w",
			r.Granularity, strings.Join(accountGranularities, ", "), ErrValidation)
	}
	return nil
}

// CreateAccount resolves defaults, validates, and upserts one accounts row. It
// deliberately does NOT write auto_join_role: that column is the single-tenant
// profile's concept (UpsertSingleTenantAccount is its only writer), and a
// multi-tenant account admits members via invited grants, not an auto-join
// policy. The upsert is idempotent on (provider, account_key).
func CreateAccount(ctx context.Context, q RegistryQueries, req CreateAccountRequest) (accountdb.Account, error) {
	resolved := req.resolveDefaults()
	if err := resolved.Validate(); err != nil {
		return accountdb.Account{}, err
	}

	var displayName *string
	if resolved.DisplayName != "" {
		name := resolved.DisplayName
		displayName = &name
	}
	return q.UpsertAccount(ctx, accountdb.UpsertAccountParams{
		ID:          uuid.New(),
		Provider:    resolved.Provider,
		AccountKey:  resolved.AccountKey,
		DisplayName: displayName,
		Granularity: resolved.Granularity,
		HomeRegion:  nil,
	})
}

// ValidateInstallationRef checks that ref matches the shape the shipped forge
// code parses for provider, wrapping ErrValidation on any mismatch:
//
//   - gitlab: "gitlab:" + a base-10 POSITIVE integer — the exact form
//     forge/gitlab's projectIDFromScope parses and runs.installation_ref carries.
//   - github: a BARE base-10 positive integer with NO "github:" prefix —
//     forge.FromGitHubInstallationID's canonical form (docs/deploy/gitlab.md's
//     "installation_ref format" section).
func ValidateInstallationRef(provider, ref string) error {
	ref = strings.TrimSpace(ref)
	switch provider {
	case "gitlab":
		rest, ok := strings.CutPrefix(ref, "gitlab:")
		if !ok {
			return fmt.Errorf("account: gitlab installation ref %q must be of the form gitlab:<project-id>: %w", ref, ErrValidation)
		}
		id, err := strconv.Atoi(rest)
		if err != nil {
			return fmt.Errorf("account: gitlab installation ref %q carries a non-numeric project id: %w", ref, ErrValidation)
		}
		if id <= 0 {
			return fmt.Errorf("account: gitlab installation ref %q carries a non-positive project id %d: %w", ref, id, ErrValidation)
		}
		return nil
	case "github":
		if strings.HasPrefix(ref, "github:") {
			return fmt.Errorf("account: github installation ref %q must be a BARE numeric installation id, not %q-prefixed: %w", ref, "github:", ErrValidation)
		}
		id, err := strconv.Atoi(ref)
		if err != nil {
			return fmt.Errorf("account: github installation ref %q must be a base-10 installation id: %w", ref, ErrValidation)
		}
		if id <= 0 {
			return fmt.Errorf("account: github installation ref %q must be a positive installation id, got %d: %w", ref, id, ErrValidation)
		}
		return nil
	default:
		return fmt.Errorf("account: provider %q is not one of %s (--provider): %w",
			provider, strings.Join(accountProviders, ", "), ErrValidation)
	}
}

// RegisterInstallationRequest is the input to RegisterInstallation.
type RegisterInstallationRequest struct {
	Provider        string
	AccountKey      string
	InstallationRef string
	ForgeBaseURL    string
	OAuthBaseURL    string
	// ProjectPath is the GitLab path_with_namespace this installation is
	// authorized to act on. REQUIRED for provider 'gitlab' (E45.26 / #2877)
	// and ignored for 'github', whose payload identity arrives HMAC-signed
	// and is resolved through the installation id instead.
	ProjectPath string
}

// ValidateGitLabProjectPath checks a GitLab path_with_namespace against the
// shape the run-creation authorization gate binds, wrapping ErrValidation on
// any mismatch. accountKey is the ALREADY-TRIMMED account_key the path must
// live under.
//
// The split is on the FIRST separator only. GitLab groups NEST
// ('acme/platform/widgets'), so a validator demanding exactly two segments
// would make every nested-group project unregisterable — and the authorizer's
// own namespace derivation is `strings.Cut(path, "/")`, the same
// owner-segment convention account.Resolver uses, so splitting any other way
// here would validate a shape the gate then rejects.
//
// Comparison is EXACT and case-SENSITIVE, matching the authorizer. GitLab
// canonicalises project path case, so a case difference means the payload does
// not name the recorded project.
func ValidateGitLabProjectPath(accountKey, projectPath string) error {
	path := strings.TrimSpace(projectPath)
	if path == "" {
		return fmt.Errorf("account: gitlab registrations must record the project path (--project-path <namespace>/<project>): %w", ErrValidation)
	}
	namespace, project, ok := strings.Cut(path, "/")
	if !ok || namespace == "" || project == "" {
		return fmt.Errorf(
			"account: gitlab project path %q must be of the form <namespace>/<project> (nested groups allowed, e.g. acme/platform/widgets) (--project-path): %w",
			path, ErrValidation)
	}
	if namespace != accountKey {
		return fmt.Errorf(
			"account: gitlab project path %q lives under namespace %q but the installation is owned by account_key %q; the namespace segment must equal the account key (--project-path): %w",
			path, namespace, accountKey, ErrValidation)
	}
	return nil
}

// RegisterInstallation validates its input, resolves the owning account by
// (provider, account_key), and upserts one installations row pointing at it.
//
// It FAILS CLOSED when the named account does not exist (ErrAccountNotFound),
// naming the `fishhawkd account create` remedy, rather than materializing the
// account — this is the load-bearing control (see ErrAccountNotFound). Every
// input validation failure wraps ErrValidation. The upsert is idempotent on
// (provider, installation_ref).
//
// For provider 'gitlab' a --project-path is REQUIRED and must live under the
// resolved account_key (ValidateGitLabProjectPath): since E45.26 / #2877 the
// run-creation authorization gate binds the payload's path EXACTLY against this
// value, and a row carrying none is UNBOUND and refuses every trigger. Refusing
// to create one is what keeps the fail-closed refusal an upgrade artifact
// rather than something the supported write path can newly produce.
func RegisterInstallation(ctx context.Context, q RegistryQueries, req RegisterInstallationRequest) (accountdb.Installation, error) {
	provider := strings.TrimSpace(req.Provider)
	accountKey := strings.TrimSpace(req.AccountKey)
	ref := strings.TrimSpace(req.InstallationRef)
	forgeBaseURL := strings.TrimSpace(req.ForgeBaseURL)
	oauthBaseURL := strings.TrimSpace(req.OAuthBaseURL)
	projectPath := strings.TrimSpace(req.ProjectPath)

	if !slices.Contains(accountProviders, provider) {
		return accountdb.Installation{}, fmt.Errorf("account: provider %q is not one of %s (--provider): %w",
			provider, strings.Join(accountProviders, ", "), ErrValidation)
	}
	if accountKey == "" {
		return accountdb.Installation{}, fmt.Errorf("account: account key must be non-empty (--account-key): %w", ErrValidation)
	}
	if err := ValidateInstallationRef(provider, ref); err != nil {
		return accountdb.Installation{}, err
	}
	if forgeBaseURL != "" {
		if err := ValidateResolvedBaseURL(forgeBaseURL); err != nil {
			return accountdb.Installation{}, fmt.Errorf("%w (--forge-base-url): %w", err, ErrValidation)
		}
	}
	if oauthBaseURL != "" {
		if err := ValidateResolvedBaseURL(oauthBaseURL); err != nil {
			return accountdb.Installation{}, fmt.Errorf("%w (--oauth-base-url): %w", err, ErrValidation)
		}
	}

	acct, err := q.GetAccountByKey(ctx, accountdb.GetAccountByKeyParams{Provider: provider, AccountKey: accountKey})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return accountdb.Installation{}, fmt.Errorf(
				"account: no %s account with account_key %q; create it first with: fishhawkd account create --provider %s --account-key %s: %w",
				provider, accountKey, provider, accountKey, ErrAccountNotFound)
		}
		return accountdb.Installation{}, err
	}

	// The exact-path binding the GitLab authorization gate compares against
	// (E45.26 / #2877). Validated AFTER the account lookup and against the same
	// trimmed accountKey, so the namespace-consistency message names the key
	// that was actually resolved. github is unaffected: its identity is
	// HMAC-signed and resolved through the installation id, so no project path
	// applies and any supplied one is ignored rather than persisted.
	var projectPathPtr *string
	if provider == "gitlab" {
		if err := ValidateGitLabProjectPath(accountKey, projectPath); err != nil {
			return accountdb.Installation{}, err
		}
		projectPathPtr = &projectPath
	}

	var forgePtr, oauthPtr *string
	if forgeBaseURL != "" {
		forgePtr = &forgeBaseURL
	}
	if oauthBaseURL != "" {
		oauthPtr = &oauthBaseURL
	}
	return q.UpsertInstallation(ctx, accountdb.UpsertInstallationParams{
		ID:              uuid.New(),
		AccountID:       acct.ID,
		Provider:        provider,
		InstallationRef: ref,
		ForgeBaseUrl:    forgePtr,
		OauthBaseUrl:    oauthPtr,
		ProjectPath:     projectPathPtr,
	})
}
