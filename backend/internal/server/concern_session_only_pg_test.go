package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// TestServerCheckClearSession_PostgresCrossBoundary is the ADR-143 D1
// (#5228) cross-boundary test: the credential kind is derived from REAL
// persisted state, not a fake. Real api_tokens rows (a static Issue, whose
// auth_method column defaults to 'static', and an IssueOAuth device-flow
// row), a real auth session minted through auth.NewPostgresRepository SignIn,
// and the real concern and chained audit repositories all sit behind
// s.Handler(), so bearerAuth, the cookie and CSRF middleware, the waive
// handler, the concern store and the audit chain are all production code.
//
// The static and oauth bearers (subject github:alice, non-agent, no delegated
// flag) are refused 403 concern_requires_session and the COMMITTED row, read
// back from Postgres, stays raised with no concern_waived row in the chain.
// The session (the same login) is admitted and the row reads back waived with
// exactly one concern_waived row.
//
// COUNTERFACTUAL (hand-run, #5228 slice 1; restored byte-identically): the D1
// condition made permissive (refuseServerCheckClearOffSession's
// `if kind == identity.CredentialSession {` → `... || kind != "" {`) turned
// the static arm RED — 200, the Postgres row read back waived and one
// concern_waived row was appended. The oauth and session arms then ran
// against the already-waived row and went RED too (422 concern_waive_conflict,
// with waive.go's documented intent-first concern_waived entry).
func TestServerCheckClearSession_PostgresCrossBoundary(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	concernRepo := concern.NewPostgresRepository(pool)
	tokenRepo := apitoken.NewPostgresRepository(pool)
	authRepo := auth.NewPostgresRepository(pool)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		AuditRepo:    auditRepo,
		ConcernRepo:  concernRepo,
		APITokenRepo: tokenRepo,
		AuthRepo:     authRepo,
	})

	// A session bound to a real workspace account (the write tier refuses an
	// unbound one); the run is untenanted, so ownership admits every
	// credential and D1 is the only refusal in the path.
	accountID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, provider, account_key) VALUES ($1, $2, $3)`,
		accountID, "github", "acme-"+accountID.String()[:8]); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	_, sess, err := authRepo.SignIn(ctx, "github", auth.GitHubProfile{ID: 4242, Login: "alice", Name: "Alice"}, accountID)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	csrfTok, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "cafef00d",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, r.ID, run.StateRunning); err != nil {
		t.Fatalf("run -> running: %v", err)
	}
	impl, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	rows, err := concernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID: r.ID, StageID: impl.ID, StageKind: concern.StageKindImplement,
		Provenance: concern.ProvenanceServerCheck, OriginReviewSequence: 9,
		Concerns: []concern.RaisedConcern{{
			Severity: "high", Category: "security",
			Note:     "credential-shaped addition (github-pat-classic) at config/settings.go:2",
			CheckKey: serverCheckKey,
		}},
	})
	if err != nil {
		t.Fatalf("seed server_check concern: %v", err)
	}
	row := rows[0]
	if !row.IsServerCheck() {
		t.Fatalf("seeded row provenance = %q, want server_check", row.Provenance)
	}

	staticTok, err := tokenRepo.Issue(ctx, "github:alice", clearScopes)
	if err != nil {
		t.Fatalf("issue static token: %v", err)
	}
	oauthIssuer, ok := tokenRepo.(apitoken.OAuthIssuer)
	if !ok {
		t.Fatalf("the Postgres api-token repository is not an apitoken.OAuthIssuer")
	}
	oauthTok, err := oauthIssuer.IssueOAuth(ctx, "github:alice", clearScopes, "github")
	if err != nil {
		t.Fatalf("issue oauth token: %v", err)
	}

	path := "/v0/concerns/" + row.ID.String() + "/waive"
	body := jsonBody(map[string]any{"reason": "known test fixture", "judgement": "no_defect"})
	post := func(bearer string, session bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if session {
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText})
			req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrfTok})
			req.Header.Set(CSRFHeaderName, csrfTok)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}
	// committed reads the row and the chain back from Postgres.
	committed := func(t *testing.T, want concern.State, wantWaived int) {
		t.Helper()
		got, err := concernRepo.GetByIDs(ctx, []uuid.UUID{row.ID})
		if err != nil {
			t.Fatalf("read concern back: %v", err)
		}
		if got[0].State != want {
			t.Errorf("committed concern state = %q, want %q", got[0].State, want)
		}
		entries, err := auditRepo.ListForRunByCategory(ctx, r.ID, CategoryConcernWaived)
		if err != nil {
			t.Fatalf("read audit chain: %v", err)
		}
		if len(entries) != wantWaived {
			t.Errorf("committed concern_waived rows = %d, want %d", len(entries), wantWaived)
		}
	}

	for _, tc := range []struct {
		kind, bearer string
	}{
		{identity.CredentialStatic, staticTok.PlainText},
		{identity.CredentialDeviceFlow, oauthTok.PlainText},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			w := post(tc.bearer, false)
			assertRequiresSession(t, w, row, clearVerbWaive, tc.kind)
			committed(t, concern.StateRaised, 0)
		})
	}

	t.Run(identity.CredentialSession, func(t *testing.T) {
		w := post("", true)
		if w.Code != http.StatusOK {
			t.Fatalf("session waive = %d, want 200:\n%s", w.Code, w.Body.String())
		}
		committed(t, concern.StateWaived, 1)
	})
}
