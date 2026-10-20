package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Cross-boundary tenancy tests for POST /v0/tokens (#4506). Each drives the
// REAL apitoken repository on pgtest Postgres: request -> bearerAuth ->
// handleCreateToken -> api_tokens row -> bearerAuth (the minted token) ->
// the account-ownership middleware and the run list's account scoping.
// Every case reads the COMMITTED api_tokens.account_id by raw SQL after the
// mint returned, not the response alone.

// tenancyMintFixture wires a Server over the real token repository with
// accounts A and B and three runs: runA (account A), runB (account B) and
// runU (untenanted).
type tenancyMintFixture struct {
	s                *Server
	pool             *pgxpool.Pool
	repo             apitoken.Repository
	acctA, acctB     uuid.UUID
	runA, runB, runU uuid.UUID
}

func newTenancyMintFixture(t *testing.T) *tenancyMintFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	f := &tenancyMintFixture{
		pool:  pool,
		repo:  apitoken.NewPostgresRepository(pool),
		acctA: uuid.New(),
		acctB: uuid.New(),
		runA:  uuid.New(),
		runB:  uuid.New(),
		runU:  uuid.New(),
	}
	for _, a := range []struct {
		id  uuid.UUID
		key string
	}{{f.acctA, "tenancy-a"}, {f.acctB, "tenancy-b"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, a.id, a.key); err != nil {
			t.Fatalf("insert account %s: %v", a.key, err)
		}
	}
	runs := newFakeRepo()
	now := time.Now().UTC()
	for i, ru := range []*run.Run{
		{ID: f.runA, AccountID: f.acctA.String()},
		{ID: f.runB, AccountID: f.acctB.String()},
		{ID: f.runU},
	} {
		ru.Repo = "acme/app"
		ru.WorkflowID = "feature_change"
		ru.State = run.StatePending
		ru.CreatedAt = now.Add(-time.Duration(i) * time.Minute)
		runs.runs[ru.ID] = ru
	}
	f.s = New(Config{
		Addr:                  "127.0.0.1:0",
		APITokenRepo:          f.repo,
		RunRepo:               runs,
		OperatorDefaultScopes: []string{"read:runs"},
	})
	return f
}

// committedAccountID reads the minted row's account_id back by raw SQL:
// nil for NULL (untenanted).
func (f *tenancyMintFixture) committedAccountID(t *testing.T, tokenID string) *uuid.UUID {
	t.Helper()
	var acct *uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`SELECT account_id FROM api_tokens WHERE id = $1`, tokenID).Scan(&acct); err != nil {
		t.Fatalf("read committed account_id: %v", err)
	}
	return acct
}

// listRunIDs drives GET /v0/runs through the full handler as bearer.
func (f *tenancyMintFixture) listRunIDs(t *testing.T, bearer string) []uuid.UUID {
	t.Helper()
	w := tokenRequest(t, f.s, http.MethodGet, "/v0/runs", bearer, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v0/runs = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp struct {
		Items []struct {
			ID uuid.UUID `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode run list: %v\n%s", err, w.Body.String())
	}
	ids := make([]uuid.UUID, 0, len(resp.Items))
	for _, it := range resp.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

// mintedToken decodes a 201 mint response, failing the test on any other status.
func mintedToken(t *testing.T, w *httptest.ResponseRecorder) apiTokenCreatedResponse {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("mint = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var resp apiTokenCreatedResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	return resp
}

// requireCommittedAccount asserts the committed row's account_id equals
// want (nil = NULL), without stopping the test so later assertions report.
func (f *tenancyMintFixture) requireCommittedAccount(t *testing.T, tokenID string, want *uuid.UUID) {
	t.Helper()
	got := f.committedAccountID(t, tokenID)
	switch {
	case want == nil && got != nil:
		t.Errorf("committed account_id = %s, want NULL (untenanted)", *got)
	case want != nil && (got == nil || *got != *want):
		t.Errorf("committed account_id = %v, want %s", got, *want)
	}
}

// TestCreateToken_TenantedBearerMintStampsAccount: a tenanted fhk_ bearer
// (seeded BY CONSTRUCTION with a raw UPDATE, not through the control) mints
// a token that is committed with its account and stays inside it: its own
// account's run is readable, another account's run is refused, and the run
// list excludes the other account's run. With the account stamp dropped the
// committed row is NULL, the own-account read is refused and the list
// includes runB.
func TestCreateToken_TenantedBearerMintStampsAccount(t *testing.T) {
	f := newTenancyMintFixture(t)
	ctx := context.Background()
	seed, err := f.repo.Issue(ctx, "github:member-a", []string{"read:runs"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE api_tokens SET account_id = $1 WHERE id = $2`,
		f.acctA, seed.ID); err != nil {
		t.Fatalf("bind seed account: %v", err)
	}

	minted := mintedToken(t, tokenRequest(t, f.s, http.MethodPost, "/v0/tokens", seed.PlainText,
		map[string]any{"scopes": []string{"read:runs"}}))
	f.requireCommittedAccount(t, minted.ID, &f.acctA)

	if w := tokenRequest(t, f.s, http.MethodGet, "/v0/runs/"+f.runB.String(), minted.Token, nil); w.Code != http.StatusForbidden {
		t.Errorf("GET other account's run = %d, want 403 account_forbidden:\n%s", w.Code, w.Body.String())
	} else {
		var env errorEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Error.Code != "account_forbidden" {
			t.Errorf("GET other account's run code = %q, want account_forbidden", env.Error.Code)
		}
	}
	if w := tokenRequest(t, f.s, http.MethodGet, "/v0/runs/"+f.runA.String(), minted.Token, nil); w.Code != http.StatusOK {
		t.Errorf("GET own account's run = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	ids := f.listRunIDs(t, minted.Token)
	if !slices.Contains(ids, f.runA) || !slices.Contains(ids, f.runU) {
		t.Errorf("run list = %v, want it to contain runA %s and runU %s", ids, f.runA, f.runU)
	}
	if slices.Contains(ids, f.runB) {
		t.Errorf("run list = %v, want it to exclude another account's runB %s", ids, f.runB)
	}
}

// TestCreateToken_TenantedCookieMintStampsAccount: a cookie session resolved
// to account A mints a token committed with account A, whose run list
// excludes account B's run.
func TestCreateToken_TenantedCookieMintStampsAccount(t *testing.T) {
	f := newTenancyMintFixture(t)
	minted := mintedToken(t, mintAs(t, f.s, Identity{
		Subject:   "github:member-a",
		UserID:    uuid.NewString(),
		SessionID: "sess-a",
		AccountID: f.acctA.String(),
	}, []string{"read:runs"}))
	f.requireCommittedAccount(t, minted.ID, &f.acctA)
	if ids := f.listRunIDs(t, minted.Token); slices.Contains(ids, f.runB) {
		t.Errorf("run list = %v, want it to exclude another account's runB %s", ids, f.runB)
	}
}

// TestCreateToken_UntenantedMintStaysUntenanted: an untenanted caller (an
// operator bearer, or an accountless cookie session) still mints an
// untenanted token whose run list keeps the operator view (runB included).
// With the stamp made unconditional the untenanted mint reaches
// IssueForAccount with an empty account id and is refused (500).
func TestCreateToken_UntenantedMintStaysUntenanted(t *testing.T) {
	assertUntenanted := func(t *testing.T, f *tenancyMintFixture, w *httptest.ResponseRecorder) {
		t.Helper()
		minted := mintedToken(t, w)
		f.requireCommittedAccount(t, minted.ID, nil)
		if ids := f.listRunIDs(t, minted.Token); !slices.Contains(ids, f.runB) {
			t.Errorf("run list = %v, want the operator view including runB %s", ids, f.runB)
		}
	}

	t.Run("bearer", func(t *testing.T) {
		f := newTenancyMintFixture(t)
		seed, err := f.repo.Issue(context.Background(), "github:operator", []string{"read:runs"})
		if err != nil {
			t.Fatal(err)
		}
		assertUntenanted(t, f, tokenRequest(t, f.s, http.MethodPost, "/v0/tokens", seed.PlainText,
			map[string]any{"scopes": []string{"read:runs"}}))
	})

	t.Run("cookie", func(t *testing.T) {
		f := newTenancyMintFixture(t)
		assertUntenanted(t, f, mintAs(t, f.s, Identity{
			Subject:   "github:operator",
			UserID:    uuid.NewString(),
			SessionID: "sess-operator",
		}, []string{"read:runs"}))
	})
}
