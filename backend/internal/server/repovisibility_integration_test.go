package server

// repovisibility_integration_test.go is the CROSS-BOUNDARY test for repo-scoped
// in-workspace visibility (ADR-057 Amendment A2, E44.10 / #2071).
//
// Why it exists as its own file: scope.files for this change spans persistence
// (migration 0059 + repoacl/db), a domain package (repoacl), the server authz
// seam (repovisibility.go), and five HTTP handlers. Every layer's unit tests
// can pass while the seam between them fails open — a mirror that answers
// correctly but is never consulted, a filter resolved but never applied, a
// subject keyed one way when written and another when read. So this drives
// REAL HTTP requests through the REAL handlers against a REAL Postgres-backed
// mirror, a REAL account-role store, and a fake forge.
//
// It also pins the honest boundary of the change: migration 0057's RLS
// policies are inert under the superuser test role, so the filtering asserted
// here is entirely the handler-side filter's doing. That is the point — it
// passes with RLS bypassed and would fail if the handler filter regressed.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	authpkg "github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repoacl"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const (
	visibleRepo = "acme/visible"
	hiddenRepo  = "acme/hidden"
)

// fakeForge is the identity.IdentityProvider slice the mirror consumes. alice
// holds read on acme/visible only; everyone else holds nothing. Calls are
// counted so the cross-forge case can assert ZERO forge traffic.
type fakeForge struct {
	calls int
}

func (f *fakeForge) PermissionLevel(_ context.Context, repo, subject string) (identity.Permission, error) {
	f.calls++
	if subject == "alice" && repo == visibleRepo {
		return identity.PermissionRead, nil
	}
	return identity.PermissionNone, nil
}

// visibilityFixture is the fully-wired stack plus the ids the assertions need.
type visibilityFixture struct {
	srv       *Server
	forge     *fakeForge
	accountID uuid.UUID
	visRun    uuid.UUID
	hidRun    uuid.UUID
	visCamp   uuid.UUID
	hidCamp   uuid.UUID
	hidSess   uuid.UUID
	// alice / bob are REAL session cookies minted through the real auth
	// repository, so requests traverse the real bearerAuth middleware rather
	// than an injected context identity — the middleware is part of the seam
	// under test.
	alice *http.Cookie
	bob   *http.Cookie
}

func newVisibilityFixture(t *testing.T) *visibilityFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)

	accountID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (id, account_key, provider) VALUES ($1, 'acme', 'github')`,
		accountID); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	// alice is a plain member (filtered); bob is a workspace admin (bypass).
	for _, m := range []struct{ ref, role string }{{"alice", "member"}, {"bob", "admin"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO account_members (id, account_id, provider, member_ref, origin, role)
			 VALUES ($1, $2, 'github', $3, 'invited', $4)`,
			uuid.New(), accountID, m.ref, m.role); err != nil {
			t.Fatalf("insert member %s: %v", m.ref, err)
		}
	}

	runRepo := runpkg.NewPostgresRepository(pool)
	campRepo := campaign.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	refineRepo := refinement.NewPostgresRepository(pool)

	newRun := func(repo string) uuid.UUID {
		t.Helper()
		r, err := runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo:          repo,
			WorkflowID:    "feature_change",
			WorkflowSHA:   "deadbeef",
			TriggerSource: runpkg.TriggerCLI,
		})
		if err != nil {
			t.Fatalf("create run %s: %v", repo, err)
		}
		return r.ID
	}
	visRun, hidRun := newRun(visibleRepo), newRun(hiddenRepo)

	newCampaign := func(repo, epic string) uuid.UUID {
		t.Helper()
		c, err := campRepo.CreateCampaign(ctx, campaign.CreateCampaignParams{
			Repo: repo, EpicRef: epic, PausePolicy: campaign.PausePolicyPauseCampaign,
		})
		if err != nil {
			t.Fatalf("create campaign %s: %v", repo, err)
		}
		return c.ID
	}
	visCamp, hidCamp := newCampaign(visibleRepo, "#1"), newCampaign(hiddenRepo, "#2")

	// One audit entry per run, plus one GLOBAL-CHAIN entry (no run) — the
	// admin-only case.
	for _, id := range []uuid.UUID{visRun, hidRun} {
		if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: id, Category: "run_created", Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("append run audit: %v", err)
		}
	}
	if _, err := auditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Category: "installation_token_issued", Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("append global audit: %v", err)
	}

	// A refinement session filed against the HIDDEN repo (binding condition 5:
	// refinement gates on its own field, not the run-scoped middleware path).
	hidSess := uuid.New()
	draft, err := refineRepo.CreateDraft(ctx, refinement.CreateParams{
		SessionID: hidSess,
		Brief:     "stand up X",
		Draft:     refinementValidDraft(),
		Model:     "test",
		Origin:    refinement.OriginBrief,
	})
	if err != nil {
		t.Fatalf("create refinement draft: %v", err)
	}
	if _, err := refineRepo.CreateFilingSession(ctx, refinement.FilingSessionParams{
		DraftID: draft.ID, SessionID: hidSess, Repo: hiddenRepo,
	}); err != nil {
		t.Fatalf("create filing session: %v", err)
	}

	forge := &fakeForge{}
	authRepo := authpkg.NewPostgresRepository(pool)
	srv := New(Config{
		Addr:           "127.0.0.1:0",
		AuthRepo:       authRepo,
		RunRepo:        runRepo,
		CampaignRepo:   campRepo,
		AuditRepo:      auditRepo,
		RefinementRepo: refineRepo,
		AccountRoles:   account.NewStore(accountdb.New(pool)),
		RepoProviders:  account.NewResolver(accountdb.New(pool)),
		// The REAL mirror over the REAL Postgres store — this is the seam the
		// per-layer units cannot cover.
		RepoVisibility: repoacl.NewMirror(repoacl.NewPostgresStore(pool), forge, repoacl.DefaultTTL, nil),
	})

	// Mint one real session per member. The middleware derives
	// Subject="github:<login>" and AccountID from the session row, which is
	// exactly what repoFilterFor keys on.
	signIn := func(githubID int64, login string) *http.Cookie {
		t.Helper()
		_, sess, err := authRepo.SignIn(ctx, "github",
			authpkg.GitHubProfile{ID: githubID, Login: login, Name: login}, accountID)
		if err != nil {
			t.Fatalf("sign in %s: %v", login, err)
		}
		return &http.Cookie{Name: authpkg.SessionCookieName, Value: sess.PlainText}
	}

	return &visibilityFixture{
		srv: srv, forge: forge, accountID: accountID,
		visRun: visRun, hidRun: hidRun,
		visCamp: visCamp, hidCamp: hidCamp, hidSess: hidSess,
		alice: signIn(1, "alice"), bob: signIn(2, "bob"),
	}
}

// get drives one GET through the REAL router + middleware chain carrying a
// real session cookie. The router owns path values, so nothing is injected.
func (f *visibilityFixture) get(t *testing.T, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	return w
}

// getAs is the ONE case a real cookie cannot express: bearerAuth derives every
// cookie session's subject as "github:<login>" (there is no GitLab browser
// sign-in flow yet — seam-first delivery, E44.8), so a cross-forge caller has
// to be injected and the handler called directly. Everything else above goes
// through the full chain.
func (f *visibilityFixture) getAs(t *testing.T, target string, id Identity, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := withIdentity(httptest.NewRequest(http.MethodGet, target, nil), id)
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

// decodeItems asserts the response IS a successful list page before reading it.
// Without the status check an empty-items assertion is vacuous: a 403/500/503
// error envelope carries no "items" key, so it decodes to a nil slice and every
// "want none" case passes on a failed request (as does a zero-forge-call
// assertion, since a request that errors early never reaches the forge).
func decodeItems(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a filtered list page must succeed):\n%s", w.Code, w.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return body.Items
}

func repoField(items []map[string]any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if r, ok := it["repo"].(string); ok {
			out = append(out, r)
		}
	}
	return out
}

// TestRepoVisibility_Integration_MemberSeesOnlyGrantedRepo is the primary
// cross-layer assertion: one cookie session, real handlers, real mirror, and a
// forge that grants read on exactly one of two repos.
func TestRepoVisibility_Integration_MemberSeesOnlyGrantedRepo(t *testing.T) {
	f := newVisibilityFixture(t)
	alice := f.alice

	t.Run("runs list", func(t *testing.T) {
		items := decodeItems(t, f.get(t, "/v0/runs", alice))
		if got := repoField(items); len(got) != 1 || got[0] != visibleRepo {
			t.Errorf("runs = %v, want only [%s]", got, visibleRepo)
		}
	})

	t.Run("campaigns list", func(t *testing.T) {
		items := decodeItems(t, f.get(t, "/v0/campaigns", alice))
		if got := repoField(items); len(got) != 1 || got[0] != visibleRepo {
			t.Errorf("campaigns = %v, want only [%s]", got, visibleRepo)
		}
	})

	t.Run("audit feed drops the hidden run AND the global chain", func(t *testing.T) {
		items := decodeItems(t, f.get(t, "/v0/audit", alice))
		if len(items) != 1 {
			t.Fatalf("audit items = %d, want 1 (visible run only)", len(items))
		}
		if items[0]["run_id"] != f.visRun.String() {
			t.Errorf("audit run_id = %v, want the visible run %s", items[0]["run_id"], f.visRun)
		}
	})

	// Binding condition (3): POINT reads 403 repo_forbidden, they do not merely
	// filter away.
	t.Run("point reads 403 repo_forbidden", func(t *testing.T) {
		cases := []struct {
			name, target string
		}{
			{"run", "/v0/runs/" + f.hidRun.String()},
			{"campaign", "/v0/campaigns/" + f.hidCamp.String()},
			{"refinement session", "/v0/refinement/sessions/" + f.hidSess.String()},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				w := f.get(t, tc.target, alice)
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
				}
				var body struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &body)
				if body.Error.Code != "repo_forbidden" {
					t.Errorf("error code = %q, want repo_forbidden (body: %s)", body.Error.Code, w.Body.String())
				}
			})
		}
	})

	t.Run("visible point reads still 200", func(t *testing.T) {
		w := f.get(t, "/v0/runs/"+f.visRun.String(), alice)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 on the granted repo:\n%s", w.Code, w.Body.String())
		}
	})
}

// TestRepoVisibility_Integration_AdminSeesEverything is the mode (f) bypass
// across ALL the surfaces — lists, the audit feed's global chain, point reads,
// and refinement (binding condition 3's admin criterion).
func TestRepoVisibility_Integration_AdminSeesEverything(t *testing.T) {
	f := newVisibilityFixture(t)
	bob := f.bob

	if got := repoField(decodeItems(t, f.get(t, "/v0/runs", bob))); len(got) != 2 {
		t.Errorf("admin runs = %v, want both repos", got)
	}
	if got := repoField(decodeItems(t, f.get(t, "/v0/campaigns", bob))); len(got) != 2 {
		t.Errorf("admin campaigns = %v, want both repos", got)
	}
	if got := decodeItems(t, f.get(t, "/v0/audit", bob)); len(got) != 3 {
		t.Errorf("admin audit items = %d, want 3 (both runs + the global-chain entry)", len(got))
	}
	if w := f.get(t, "/v0/runs/"+f.hidRun.String(), bob); w.Code != http.StatusOK {
		t.Errorf("admin run point read = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	// Binding condition (5): refinement is the one surface the shared
	// middleware seam does NOT cover, so assert the admin bypass reaches it.
	if w := f.get(t, "/v0/refinement/sessions/"+f.hidSess.String(), bob); w.Code != http.StatusOK {
		t.Errorf("admin refinement read = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if f.forge.calls != 0 {
		t.Errorf("admin bypass made %d forge calls, want 0", f.forge.calls)
	}
}

// TestRepoVisibility_Integration_CrossForgeDefaultDeny is mode (e) end to end: a
// session whose subject provider differs from the rows' forge sees NOTHING, and
// the forge is never asked — a GitHub-only login must not spend rate limit
// asking GitHub about a GitLab row, nor vice versa.
func TestRepoVisibility_Integration_CrossForgeDefaultDeny(t *testing.T) {
	f := newVisibilityFixture(t)
	// A gitlab-provider session. Its account_members row is keyed on the
	// github provider, so it resolves to no role — a filtered caller. The rows
	// belong to a github-provider account, so every one is cross-forge.
	gl := Identity{
		Subject:   "gitlab:alice",
		UserID:    uuid.NewString(),
		SessionID: uuid.NewString(),
		AccountID: f.accountID.String(),
	}

	if got := repoField(decodeItems(t, f.getAs(t, "/v0/runs", gl, f.srv.handleListRuns))); len(got) != 0 {
		t.Errorf("cross-forge runs = %v, want none", got)
	}
	if got := repoField(decodeItems(t, f.getAs(t, "/v0/campaigns", gl, f.srv.handleListCampaigns))); len(got) != 0 {
		t.Errorf("cross-forge campaigns = %v, want none", got)
	}
	if got := decodeItems(t, f.getAs(t, "/v0/audit", gl, f.srv.handleListGlobalAudit)); len(got) != 0 {
		t.Errorf("cross-forge audit items = %d, want 0", len(got))
	}
	if f.forge.calls != 0 {
		t.Errorf("cross-forge deny made %d forge calls, want 0", f.forge.calls)
	}
}

// TestRepoVisibility_Integration_MirrorMemoizesAcrossRequests proves the
// persistence layer is actually load-bearing: the SECOND request for the same
// (subject, repo) is served from the mirrored row rather than re-asking the
// forge. A mirror that wrote nothing, or keyed writes and reads differently,
// fails here while every in-memory unit test still passes.
func TestRepoVisibility_Integration_MirrorMemoizesAcrossRequests(t *testing.T) {
	f := newVisibilityFixture(t)
	alice := f.alice

	f.get(t, "/v0/runs", alice)
	after := f.forge.calls
	if after == 0 {
		t.Fatal("first request made no forge call; the mirror never resolved anything")
	}
	f.get(t, "/v0/runs", alice)
	if f.forge.calls != after {
		t.Errorf("forge calls grew %d → %d across requests; the mirrored rows are not being read back",
			after, f.forge.calls)
	}
}
