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
	// lastInstallation is the params of the most recent UpsertInstallation.
	// The COMMITTED-STATE read: a validation that fired and was then rolled
	// back would return a byte-identical error, so every refusal case asserts
	// the write was never reached rather than only the error identity.
	lastInstallation accountdb.UpsertInstallationParams
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
	f.lastInstallation = arg
	return accountdb.Installation{
		ID:              arg.ID,
		AccountID:       arg.AccountID,
		Provider:        arg.Provider,
		InstallationRef: arg.InstallationRef,
		ForgeBaseUrl:    arg.ForgeBaseUrl,
		OauthBaseUrl:    arg.OauthBaseUrl,
		ProjectPath:     arg.ProjectPath,
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
		// The rejection must enumerate 'user' so an operator sees the new tier
		// (E44.35 / #2925) as an accepted value.
		{"rejection enumerates user tier", CreateAccountRequest{Provider: "gitlab", AccountKey: "acme", Granularity: "org"}, "user"},
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

// TestCreateAccountRequestValidate_AcceptsUserGranularity pins that the
// personal-namespace tier (E44.35 / #2925) validates clean — the positive
// twin of the enumeration case above. A registry surface that dropped 'user'
// from accountGranularities would reject this.
func TestCreateAccountRequestValidate_AcceptsUserGranularity(t *testing.T) {
	req := CreateAccountRequest{Provider: "github", AccountKey: "octocat", Granularity: "user"}
	if err := req.resolveDefaults().Validate(); err != nil {
		t.Errorf("Validate() on a user-granularity request = %v, want nil", err)
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
		ProjectPath:     "acme/widgets",
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

// TestRegisterInstallation_GitLabProjectPath covers every named branch of the
// E45.26 / #2877 write-side binding: --project-path is REQUIRED for a gitlab
// registration, must be a <namespace>/<project> path (nested groups included),
// and its namespace segment must equal the resolved account_key.
//
// COMMITTED STATE, not error identity (counterfactual trap (a)): each refusal
// asserts UpsertInstallation was NEVER reached. A validation that fired and was
// then rolled back would return a byte-identical error, so the error alone
// cannot discriminate.
func TestRegisterInstallation_GitLabProjectPath(t *testing.T) {
	acctID := uuid.New()
	cases := []struct {
		name        string
		provider    string
		projectPath string
		wantWrite   bool
		// wantSub is a substring the refusal message must carry, so a
		// regression to a generic message reddens.
		wantSub string
	}{
		{
			name: "missing_project_path_refuses", provider: "gitlab", projectPath: "",
			wantSub: "--project-path",
		},
		{
			name: "whitespace_project_path_refuses", provider: "gitlab", projectPath: "   ",
			wantSub: "--project-path",
		},
		{
			// No separator at all: a bare project name names no namespace, so
			// the gate's own strings.Cut derivation would refuse it later.
			name: "namespaceless_path_refuses", provider: "gitlab", projectPath: "widgets",
			wantSub: "<namespace>/<project>",
		},
		{
			name: "empty_namespace_segment_refuses", provider: "gitlab", projectPath: "/widgets",
			wantSub: "<namespace>/<project>",
		},
		{
			name: "empty_project_segment_refuses", provider: "gitlab", projectPath: "acme/",
			wantSub: "<namespace>/<project>",
		},
		{
			// The namespace-consistency check: a path outside the owning
			// account's namespace is the mis-registration the authorizer's
			// retained tenancy invariant exists to catch. Refuse it at the
			// write instead of creating a row the gate will always reject.
			name: "namespace_mismatch_refuses", provider: "gitlab", projectPath: "other/widgets",
			wantSub: `"acme"`,
		},
		{
			name: "gitlab_happy_path_writes", provider: "gitlab", projectPath: "acme/widgets",
			wantWrite: true,
		},
		{
			// BINDING CONDITION 4: GitLab groups NEST. A validator splitting on
			// every "/" and demanding exactly two segments would make every
			// nested-group project unregisterable. The split is on the FIRST
			// separator only, so the namespace is "acme" and the project is
			// "platform/widgets".
			name: "nested_group_writes", provider: "gitlab", projectPath: "acme/platform/widgets",
			wantWrite: true,
		},
		{
			name: "deeply_nested_group_writes", provider: "gitlab", projectPath: "acme/platform/infra/widgets",
			wantWrite: true,
		},
		{
			// The rule is gitlab-ONLY. A github installation's identity arrives
			// inside an HMAC-signed payload and resolves through the
			// installation id, so it records no project path — and a required
			// flag there would be a gratuitous contract break.
			name: "github_without_project_path_writes", provider: "github", projectPath: "",
			wantWrite: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := "gitlab:4242"
			if tc.provider == "github" {
				ref = "4242"
			}
			fake := &fakeRegistryQueries{account: accountdb.Account{
				ID: acctID, Provider: tc.provider, AccountKey: "acme", Granularity: "group",
			}}
			inst, err := RegisterInstallation(context.Background(), fake, RegisterInstallationRequest{
				Provider:        tc.provider,
				AccountKey:      "acme",
				InstallationRef: ref,
				ProjectPath:     tc.projectPath,
			})

			if !tc.wantWrite {
				if err == nil {
					t.Fatal("err = nil, want a validation refusal")
				}
				if !errors.Is(err, ErrValidation) {
					t.Errorf("err = %v, want ErrValidation", err)
				}
				if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("err %q does not name %q", err.Error(), tc.wantSub)
				}
				// The committed-state assertion.
				if fake.upsertInstallationCalls != 0 {
					t.Errorf("UpsertInstallation called %d times after a refusal, want 0 (no row may be written)",
						fake.upsertInstallationCalls)
				}
				return
			}

			if err != nil {
				t.Fatalf("RegisterInstallation: %v", err)
			}
			if fake.upsertInstallationCalls != 1 {
				t.Fatalf("UpsertInstallation called %d times, want 1", fake.upsertInstallationCalls)
			}
			got := fake.lastInstallation.ProjectPath
			if tc.provider == "github" {
				// github persists NO project path: writing one would record a
				// binding nothing evaluates.
				if got != nil {
					t.Errorf("github installation project_path = %q, want NULL", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("gitlab installation project_path = NULL, want the recorded path")
			}
			if *got != tc.projectPath {
				t.Errorf("persisted project_path = %q, want %q (exact, untransformed)", *got, tc.projectPath)
			}
			if inst.ProjectPath == nil || *inst.ProjectPath != tc.projectPath {
				t.Errorf("returned installation project_path = %v, want %q", inst.ProjectPath, tc.projectPath)
			}
		})
	}
}

// TestRegisterInstallation_TrimsProjectPath pins that a path is stored trimmed:
// the authorizer compares the recorded value against a payload path byte for
// byte, so a stray flag-quoting space would silently produce a row that never
// admits anything.
func TestRegisterInstallation_TrimsProjectPath(t *testing.T) {
	fake := &fakeRegistryQueries{account: accountdb.Account{
		ID: uuid.New(), Provider: "gitlab", AccountKey: "acme", Granularity: "group",
	}}
	if _, err := RegisterInstallation(context.Background(), fake, RegisterInstallationRequest{
		Provider:        "gitlab",
		AccountKey:      "acme",
		InstallationRef: "gitlab:4242",
		ProjectPath:     "  acme/widgets  ",
	}); err != nil {
		t.Fatalf("RegisterInstallation: %v", err)
	}
	got := fake.lastInstallation.ProjectPath
	if got == nil || *got != "acme/widgets" {
		t.Errorf("persisted project_path = %v, want %q (trimmed)", got, "acme/widgets")
	}
}
