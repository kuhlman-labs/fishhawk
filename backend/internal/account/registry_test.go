package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
)

// fakeRegistryQueries is an in-memory RegistryQueries for the branches that need
// no database. getAccountErr forces GetAccountByKey to return a specific error
// (e.g. pgx.ErrNoRows) so the unknown-account refusal is reachable without a
// live composite FK, and upsertInstallationCalls records whether the write was
// ever reached.
type fakeRegistryQueries struct {
	getAccountErr           error
	account                 accountdb.Account
	upsertInstallationCalls int
	upsertAccountCalls      int
}

func (f *fakeRegistryQueries) GetAccountByKey(_ context.Context, _ accountdb.GetAccountByKeyParams) (accountdb.Account, error) {
	if f.getAccountErr != nil {
		return accountdb.Account{}, f.getAccountErr
	}
	return f.account, nil
}

func (f *fakeRegistryQueries) UpsertAccount(_ context.Context, arg accountdb.UpsertAccountParams) (accountdb.Account, error) {
	f.upsertAccountCalls++
	return accountdb.Account{
		ID:          arg.ID,
		Provider:    arg.Provider,
		AccountKey:  arg.AccountKey,
		DisplayName: arg.DisplayName,
		Granularity: arg.Granularity,
	}, nil
}

func (f *fakeRegistryQueries) UpsertInstallation(_ context.Context, arg accountdb.UpsertInstallationParams) (accountdb.Installation, error) {
	f.upsertInstallationCalls++
	return accountdb.Installation{
		ID:              arg.ID,
		AccountID:       arg.AccountID,
		Provider:        arg.Provider,
		InstallationRef: arg.InstallationRef,
		ForgeBaseUrl:    arg.ForgeBaseUrl,
		OauthBaseUrl:    arg.OauthBaseUrl,
	}, nil
}

func (f *fakeRegistryQueries) ListAccounts(_ context.Context) ([]accountdb.Account, error) {
	return nil, nil
}

func (f *fakeRegistryQueries) ListInstallations(_ context.Context) ([]accountdb.ListInstallationsRow, error) {
	return nil, nil
}

// TestCreateAccountRequestValidate covers one behavioral case per named failure
// mode: each must return a non-nil error that wraps ErrValidation AND names the
// offending value.
func TestCreateAccountRequestValidate(t *testing.T) {
	cases := []struct {
		name    string
		req     CreateAccountRequest
		wantSub string // substring the message must name
	}{
		{"empty account key", CreateAccountRequest{Provider: "github", Granularity: "organization"}, "account key must be non-empty"},
		{"unknown provider", CreateAccountRequest{Provider: "bitbucket", AccountKey: "acme", Granularity: "organization"}, `"bitbucket"`},
		{"granularity org", CreateAccountRequest{Provider: "gitlab", AccountKey: "acme", Granularity: "org"}, `"org"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.resolveDefaults().Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error wrapping ErrValidation")
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("error %v does not wrap ErrValidation", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not name %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestCreateAccountRequestResolveDefaults pins the per-provider granularity
// default and that an explicit override wins.
func TestCreateAccountRequestResolveDefaults(t *testing.T) {
	cases := []struct {
		name     string
		req      CreateAccountRequest
		wantGran string
	}{
		{"github defaults organization", CreateAccountRequest{Provider: "github", AccountKey: "acme"}, "organization"},
		{"gitlab defaults group", CreateAccountRequest{Provider: "gitlab", AccountKey: "acme"}, "group"},
		{"explicit override wins", CreateAccountRequest{Provider: "github", AccountKey: "acme", Granularity: "enterprise"}, "enterprise"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.resolveDefaults().Granularity; got != tc.wantGran {
				t.Errorf("resolved granularity = %q, want %q", got, tc.wantGran)
			}
		})
	}
}

// TestValidateInstallationRef covers one case per branch, each asserting a
// non-nil error wrapping ErrValidation and naming the offending value.
func TestValidateInstallationRef(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		ref      string
		wantSub  string
	}{
		{"gitlab missing prefix", "gitlab", "4242", `"4242"`},
		{"gitlab non-numeric", "gitlab", "gitlab:abc", `"gitlab:abc"`},
		{"gitlab zero", "gitlab", "gitlab:0", `"gitlab:0"`},
		{"gitlab negative", "gitlab", "gitlab:-1", `"gitlab:-1"`},
		{"github wrong prefix", "github", "github:42", `"github:42"`},
		{"github non-numeric", "github", "abc", `"abc"`},
		{"unknown provider", "bitbucket", "42", `"bitbucket"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInstallationRef(tc.provider, tc.ref)
			if err == nil {
				t.Fatalf("ValidateInstallationRef(%q, %q) = nil, want error", tc.provider, tc.ref)
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("error %v does not wrap ErrValidation", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not name %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestValidateInstallationRef_Accepts pins the well-formed forms so the
// validator is not merely rejecting everything.
func TestValidateInstallationRef_Accepts(t *testing.T) {
	if err := ValidateInstallationRef("gitlab", "gitlab:4242"); err != nil {
		t.Errorf("gitlab:4242 rejected: %v", err)
	}
	if err := ValidateInstallationRef("github", "42"); err != nil {
		t.Errorf("bare github id 42 rejected: %v", err)
	}
}

// TestRegisterInstallation_BadBaseURL covers every base-URL rejection branch —
// a non-https SCHEME and a syntactically MALFORMED url (the url.Parse failure
// path), on BOTH the forge and the oauth override fields. The oauth field is
// validated by a SEPARATE block in RegisterInstallation, so it gets its own
// cases; each must wrap ErrValidation and never reach the write.
func TestRegisterInstallation_BadBaseURL(t *testing.T) {
	// "https://\x7f" is a DEL control byte in the host — url.Parse rejects it,
	// exercising ValidateResolvedBaseURL's malformed (parse-error) branch rather
	// than the scheme branch a plain http:// value hits.
	const malformedURL = "https://\x7f"
	cases := []struct {
		name    string
		req     RegisterInstallationRequest
		wantSub string
	}{
		{"forge non-https scheme",
			RegisterInstallationRequest{Provider: "gitlab", AccountKey: "acme", InstallationRef: "gitlab:4242", ForgeBaseURL: "http://insecure.example"},
			"https"},
		{"forge malformed url",
			RegisterInstallationRequest{Provider: "gitlab", AccountKey: "acme", InstallationRef: "gitlab:4242", ForgeBaseURL: malformedURL},
			"malformed"},
		{"oauth non-https scheme",
			RegisterInstallationRequest{Provider: "gitlab", AccountKey: "acme", InstallationRef: "gitlab:4242", OAuthBaseURL: "http://insecure.example"},
			"https"},
		{"oauth malformed url",
			RegisterInstallationRequest{Provider: "gitlab", AccountKey: "acme", InstallationRef: "gitlab:4242", OAuthBaseURL: malformedURL},
			"malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeRegistryQueries{}
			_, err := RegisterInstallation(context.Background(), fake, tc.req)
			if err == nil {
				t.Fatalf("RegisterInstallation with bad base url = nil error, want validation error")
			}
			if !errors.Is(err, ErrValidation) {
				t.Errorf("error %v does not wrap ErrValidation", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not name %q", err.Error(), tc.wantSub)
			}
			if fake.upsertInstallationCalls != 0 {
				t.Errorf("UpsertInstallation was called %d times on a validation failure, want 0", fake.upsertInstallationCalls)
			}
		})
	}
}

// TestRegisterInstallation_UnknownAccountRefuses is the unit-level twin of the
// pgtest counterfactual: when GetAccountByKey reports no row, RegisterInstallation
// must refuse with an error wrapping ErrAccountNotFound that names the
// `fishhawkd account create` remedy, and must NEVER reach the installation write.
func TestRegisterInstallation_UnknownAccountRefuses(t *testing.T) {
	fake := &fakeRegistryQueries{getAccountErr: pgx.ErrNoRows}
	_, err := RegisterInstallation(context.Background(), fake, RegisterInstallationRequest{
		Provider:        "gitlab",
		AccountKey:      "acme",
		InstallationRef: "gitlab:4242",
	})
	if err == nil {
		t.Fatalf("RegisterInstallation for unknown account = nil error, want refusal")
	}
	if !errors.Is(err, ErrAccountNotFound) {
		t.Errorf("error %v does not wrap ErrAccountNotFound", err)
	}
	if errors.Is(err, ErrValidation) {
		t.Errorf("unknown-account refusal must NOT be a validation error (drives a different CLI exit code): %v", err)
	}
	if !strings.Contains(err.Error(), "fishhawkd account create") {
		t.Errorf("error %q does not name the `fishhawkd account create` remedy", err.Error())
	}
	// The acceptance criterion requires the error to name the MISSING account
	// key, not merely a generic remedy — the quoted `"acme"` appears only in the
	// account_key %q clause (the remedy command spells it unquoted), so this
	// stays red if the message regresses to a generic form.
	if !strings.Contains(err.Error(), `"acme"`) {
		t.Errorf("error %q does not name the missing account_key \"acme\"", err.Error())
	}
	if fake.upsertInstallationCalls != 0 {
		t.Errorf("UpsertInstallation was called %d times on an unknown account, want 0", fake.upsertInstallationCalls)
	}
}

// TestRegisterInstallation_PropagatesDBFault asserts a non-ErrNoRows fault from
// GetAccountByKey is propagated (not swallowed into a refusal), so the CLI maps
// it to a failure exit rather than usage.
func TestRegisterInstallation_PropagatesDBFault(t *testing.T) {
	boom := errors.New("connection reset")
	fake := &fakeRegistryQueries{getAccountErr: boom}
	_, err := RegisterInstallation(context.Background(), fake, RegisterInstallationRequest{
		Provider:        "gitlab",
		AccountKey:      "acme",
		InstallationRef: "gitlab:4242",
	})
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the propagated DB fault", err)
	}
	if errors.Is(err, ErrValidation) || errors.Is(err, ErrAccountNotFound) {
		t.Errorf("a raw DB fault must not be classified as validation/not-found: %v", err)
	}
}

// TestRegisterInstallation_HappyPath asserts the write is reached with the
// resolved account id when the account exists.
func TestRegisterInstallation_HappyPath(t *testing.T) {
	acctID := uuid.New()
	fake := &fakeRegistryQueries{account: accountdb.Account{ID: acctID, Provider: "gitlab", AccountKey: "acme", Granularity: "group"}}
	inst, err := RegisterInstallation(context.Background(), fake, RegisterInstallationRequest{
		Provider:        "gitlab",
		AccountKey:      "acme",
		InstallationRef: "gitlab:4242",
	})
	if err != nil {
		t.Fatalf("RegisterInstallation happy path: %v", err)
	}
	if fake.upsertInstallationCalls != 1 {
		t.Errorf("UpsertInstallation called %d times, want 1", fake.upsertInstallationCalls)
	}
	if inst.AccountID != acctID {
		t.Errorf("installation account id = %s, want the resolved account %s", inst.AccountID, acctID)
	}
}
