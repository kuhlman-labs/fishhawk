package account

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// fakeGitLabInstallationQueries serves seeded rows keyed by project_path with
// real lookup semantics (an unseeded path returns zero rows), records the
// path / ref it was queried with, counts calls, and can be programmed to fail.
type fakeGitLabInstallationQueries struct {
	byPath   map[string][]accountdb.Installation
	byRef    map[string]accountdb.Installation
	listErr  error
	getErr   error
	calls    int
	lastPath string
	lastRef  string
	lastProv string
}

func (f *fakeGitLabInstallationQueries) ListGitLabInstallationsByProjectPath(_ context.Context, projectPath *string) ([]accountdb.Installation, error) {
	f.calls++
	if projectPath != nil {
		f.lastPath = *projectPath
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byPath[f.lastPath], nil
}

func (f *fakeGitLabInstallationQueries) GetInstallationByRef(_ context.Context, arg accountdb.GetInstallationByRefParams) (accountdb.Installation, error) {
	f.calls++
	f.lastRef = arg.InstallationRef
	f.lastProv = arg.Provider
	if f.getErr != nil {
		return accountdb.Installation{}, f.getErr
	}
	row, ok := f.byRef[arg.InstallationRef]
	if !ok {
		return accountdb.Installation{}, pgx.ErrNoRows
	}
	return row, nil
}

func instRow(ref, path string, baseURL *string) accountdb.Installation {
	p := path
	return accountdb.Installation{
		ID: uuid.New(), AccountID: uuid.New(), Provider: "gitlab",
		InstallationRef: ref, ProjectPath: &p, ForgeBaseUrl: baseURL,
	}
}

func strp(s string) *string { return &s }

func TestResolveGitLabProject_ExactMatchResolves(t *testing.T) {
	f := &fakeGitLabInstallationQueries{byPath: map[string][]accountdb.Installation{
		"acme/platform/api": {instRow("gitlab:4242", "acme/platform/api", strp("https://gitlab.example.com"))},
	}}
	r := NewGitLabProjectResolver(f)
	got, found, err := r.ResolveGitLabProject(context.Background(), "  acme/platform/api ")
	if err != nil {
		t.Fatalf("ResolveGitLabProject: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if got.InstallationRef != "gitlab:4242" || got.ProjectPath != "acme/platform/api" || got.ForgeBaseURL != "https://gitlab.example.com" {
		t.Errorf("got = %+v", got)
	}
	if f.lastPath != "acme/platform/api" {
		t.Errorf("queried path = %q, want the TRIMMED path", f.lastPath)
	}
}

// Case-sensitive on purpose: GitLab paths are, and the run-creation
// authorization gate binds the exact path. A differently-cased query must NOT
// resolve the registered row.
func TestResolveGitLabProject_CaseMismatchNotFound(t *testing.T) {
	f := &fakeGitLabInstallationQueries{byPath: map[string][]accountdb.Installation{
		"acme/api": {instRow("gitlab:1", "acme/api", nil)},
	}}
	r := NewGitLabProjectResolver(f)
	_, found, err := r.ResolveGitLabProject(context.Background(), "Acme/API")
	if err != nil {
		t.Fatalf("ResolveGitLabProject: %v", err)
	}
	if found {
		t.Error("found = true for a case-mismatched path; want false")
	}
}

// Two rows bound to one path is AMBIGUOUS → found=false, never an arbitrary
// first row. Counterfactual vehicle for the `len(rows) != 1` guard.
func TestResolveGitLabProject_AmbiguousTwoRowsNotFound(t *testing.T) {
	f := &fakeGitLabInstallationQueries{byPath: map[string][]accountdb.Installation{
		"acme/api": {instRow("gitlab:1", "acme/api", nil), instRow("gitlab:2", "acme/api", nil)},
	}}
	r := NewGitLabProjectResolver(f)
	got, found, err := r.ResolveGitLabProject(context.Background(), "acme/api")
	if err != nil {
		t.Fatalf("ResolveGitLabProject: %v", err)
	}
	if found {
		t.Errorf("found = true with two rows bound to one path (resolved %q); want ambiguous → false", got.InstallationRef)
	}
}

func TestResolveGitLabProject_ZeroRowsNotFound(t *testing.T) {
	f := &fakeGitLabInstallationQueries{byPath: map[string][]accountdb.Installation{}}
	r := NewGitLabProjectResolver(f)
	_, found, err := r.ResolveGitLabProject(context.Background(), "nobody/here")
	if err != nil {
		t.Fatalf("ResolveGitLabProject: %v", err)
	}
	if found {
		t.Error("found = true for an unregistered path")
	}
	if f.calls != 1 {
		t.Errorf("calls = %d, want 1", f.calls)
	}
}

func TestResolveGitLabProject_EmptyPathNoQuery(t *testing.T) {
	f := &fakeGitLabInstallationQueries{}
	r := NewGitLabProjectResolver(f)
	for _, in := range []string{"", "   "} {
		_, found, err := r.ResolveGitLabProject(context.Background(), in)
		if err != nil || found {
			t.Errorf("ResolveGitLabProject(%q) = (found=%v, err=%v), want (false, nil)", in, found, err)
		}
	}
	if f.calls != 0 {
		t.Errorf("calls = %d, want 0 for a malformed path", f.calls)
	}
}

func TestResolveGitLabProject_QueryErrorPropagates(t *testing.T) {
	boom := errors.New("connection reset")
	f := &fakeGitLabInstallationQueries{listErr: boom}
	r := NewGitLabProjectResolver(f)
	_, found, err := r.ResolveGitLabProject(context.Background(), "acme/api")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the query error propagated", err)
	}
	if found {
		t.Error("found = true alongside an error")
	}
}

func TestResolveGitLabProject_NilResolverOrQueries(t *testing.T) {
	var nilR *GitLabProjectResolver
	if _, found, err := nilR.ResolveGitLabProject(context.Background(), "a/b"); found || err != nil {
		t.Errorf("nil resolver = (found=%v, err=%v), want (false, nil)", found, err)
	}
	r := NewGitLabProjectResolver(nil)
	if _, found, err := r.ResolveGitLabProject(context.Background(), "a/b"); found || err != nil {
		t.Errorf("nil queries = (found=%v, err=%v), want (false, nil)", found, err)
	}
	if _, found, err := r.ResolveGitLabInstallationByRef(context.Background(), "gitlab:1"); found || err != nil {
		t.Errorf("nil queries ByRef = (found=%v, err=%v), want (false, nil)", found, err)
	}
}

func TestResolveGitLabInstallationByRef_Found(t *testing.T) {
	f := &fakeGitLabInstallationQueries{byRef: map[string]accountdb.Installation{
		"gitlab:7": instRow("gitlab:7", "acme/api", strp(" https://gitlab.example.com ")),
	}}
	r := NewGitLabProjectResolver(f)
	got, found, err := r.ResolveGitLabInstallationByRef(context.Background(), " gitlab:7 ")
	if err != nil || !found {
		t.Fatalf("ByRef = (found=%v, err=%v), want found", found, err)
	}
	if got.ForgeBaseURL != "https://gitlab.example.com" || got.ProjectPath != "acme/api" || got.InstallationRef != "gitlab:7" {
		t.Errorf("got = %+v", got)
	}
	if f.lastProv != "gitlab" || f.lastRef != "gitlab:7" {
		t.Errorf("queried (provider=%q, ref=%q), want (gitlab, gitlab:7)", f.lastProv, f.lastRef)
	}
}

func TestResolveGitLabInstallationByRef_NotFoundAndError(t *testing.T) {
	f := &fakeGitLabInstallationQueries{byRef: map[string]accountdb.Installation{}}
	r := NewGitLabProjectResolver(f)
	if _, found, err := r.ResolveGitLabInstallationByRef(context.Background(), "gitlab:404"); found || err != nil {
		t.Errorf("unregistered ref = (found=%v, err=%v), want (false, nil) — ErrNoRows maps to not-found", found, err)
	}
	if _, found, err := r.ResolveGitLabInstallationByRef(context.Background(), ""); found || err != nil || f.calls != 1 {
		t.Errorf("empty ref = (found=%v, err=%v, calls=%d), want (false, nil, 1)", found, err, f.calls)
	}
	boom := errors.New("timeout")
	f.getErr = boom
	if _, _, err := r.ResolveGitLabInstallationByRef(context.Background(), "gitlab:7"); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the query error propagated", err)
	}
}

// forge_base_url is a nullable column: NULL, empty/whitespace and a value are
// three states, and the consumer slice collapses the first two to "" so the
// caller's deployment-default fallback fires for both.
func TestGitLabInstallationFromRow_ForgeBaseURLThreeStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		col  *string
		want string
	}{
		{"null", nil, ""},
		{"empty", strp(""), ""},
		{"whitespace", strp("   "), ""},
		{"value", strp("https://gitlab.example.com"), "https://gitlab.example.com"},
	} {
		got := gitlabInstallationFromRow(instRow("gitlab:1", "a/b", tc.col))
		if got.ForgeBaseURL != tc.want {
			t.Errorf("%s: ForgeBaseURL = %q, want %q", tc.name, got.ForgeBaseURL, tc.want)
		}
	}
	// NULL project_path (a pre-0078 row) projects to "" rather than panicking.
	if got := gitlabInstallationFromRow(accountdb.Installation{InstallationRef: "gitlab:9"}); got.ProjectPath != "" {
		t.Errorf("nil project_path projected to %q", got.ProjectPath)
	}
}

// TestResolveGitLabProject_Postgres drives the hand-written sqlc query against
// the real column set (sqlc is not regenerated locally), so scan-order drift
// between ListGitLabInstallationsByProjectPath and the Installation model is
// caught here. It also proves the query is provider-scoped: a GITHUB row bound
// to the same project_path (an operator error the schema does not forbid) is
// never returned.
func TestResolveGitLabProject_Postgres(t *testing.T) {
	pool := pgtest.NewPool(t)
	q := accountdb.New(pool)
	ctx := context.Background()

	for _, provider := range []string{"gitlab", "github"} {
		if _, err := CreateAccount(ctx, q, CreateAccountRequest{Provider: provider, AccountKey: "acme"}); err != nil {
			t.Fatalf("create %s account: %v", provider, err)
		}
	}
	if _, err := RegisterInstallation(ctx, q, RegisterInstallationRequest{
		Provider: "gitlab", AccountKey: "acme", InstallationRef: "gitlab:4242",
		ForgeBaseURL: "https://gitlab.example.com", ProjectPath: "acme/platform/api",
	}); err != nil {
		t.Fatalf("register gitlab installation: %v", err)
	}
	// A github row with the same project_path is written directly (the
	// registry ignores ProjectPath for github) to prove the provider filter.
	if _, err := pool.Exec(ctx,
		`INSERT INTO installations (id, account_id, provider, installation_ref, project_path)
		 SELECT $1, id, 'github', '77', $2 FROM accounts WHERE provider = 'github' AND account_key = 'acme'`,
		uuid.New(), "acme/platform/api"); err != nil {
		t.Fatalf("insert github row: %v", err)
	}

	r := NewGitLabProjectResolver(q)
	got, found, err := r.ResolveGitLabProject(ctx, "acme/platform/api")
	if err != nil {
		t.Fatalf("ResolveGitLabProject: %v", err)
	}
	if !found {
		t.Fatal("found = false; want the registered gitlab row (and the github row filtered out, else this is ambiguous)")
	}
	if got.InstallationRef != "gitlab:4242" || got.ForgeBaseURL != "https://gitlab.example.com" || got.ProjectPath != "acme/platform/api" {
		t.Errorf("got = %+v", got)
	}
	if _, found, err := r.ResolveGitLabProject(ctx, "acme/platform/API"); err != nil || found {
		t.Errorf("case-mismatched path = (found=%v, err=%v), want (false, nil)", found, err)
	}
	back, found, err := r.ResolveGitLabInstallationByRef(ctx, "gitlab:4242")
	if err != nil || !found || back.ForgeBaseURL != "https://gitlab.example.com" {
		t.Errorf("ByRef = (%+v, found=%v, err=%v)", back, found, err)
	}
	if _, found, err := r.ResolveGitLabInstallationByRef(ctx, "gitlab:0"); err != nil || found {
		t.Errorf("ByRef unregistered = (found=%v, err=%v), want (false, nil)", found, err)
	}
}
