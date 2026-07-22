package server

// Per-account run-less audit chain caller wiring (ADR-057 / #1828): server
// handlers stamp the caller Identity's account onto every AppendGlobalChained
// write, and the boardsync campaign hook stamps the campaign entity's account
// (a background caller with no request Identity). The end-to-end case drives
// a real handler over the Postgres-backed audit repository (pgtest) and
// asserts the PERSISTED rows carry account_id and chain within their account
// partition — handler → domain → persistence.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// TestTokenHandler_EndToEnd_AuditChainsWithinAccount is the cross-boundary
// assertion for the server-handler caller family: POST /v0/tokens requests
// carrying different Identity.AccountID bindings produce persisted
// api_token_issued rows whose account_id matches the caller's account and
// whose prev_hash links WITHIN that account's run-less partition — an
// interleaved append from another account (or the untenanted partition)
// never enters the chain.
func TestTokenHandler_EndToEnd_AuditChainsWithinAccount(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	auditRepo := audit.NewPostgresRepository(pool)
	tokenRepo := apitoken.NewPostgresRepository(pool)
	s := New(Config{AuditRepo: auditRepo, APITokenRepo: tokenRepo})

	acctA := uuid.New()
	acctB := uuid.New()
	for _, a := range []struct {
		id  uuid.UUID
		key string
	}{{acctA, "acct-a"}, {acctB, "acct-b"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, a.id, a.key); err != nil {
			t.Fatalf("insert account %s: %v", a.key, err)
		}
	}

	mint := func(id Identity) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v0/tokens",
			strings.NewReader(`{"scopes":["runs:write"]}`))
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
		w := httptest.NewRecorder()
		s.handleCreateToken(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("mint status = %d, want 201 (body=%s)", w.Code, w.Body.String())
		}
	}

	// Interleave appends across two accounts and the untenanted partition so
	// intra-account linkage is meaningful (A's second entry must skip B's).
	mint(Identity{Subject: "github:alice", AccountID: acctA.String()})
	mint(Identity{Subject: "github:bob", AccountID: acctB.String()})
	mint(Identity{Subject: "github:alice", AccountID: acctA.String()})
	mint(Identity{Subject: "github:carol"}) // no account binding → untenanted

	aEntries, err := auditRepo.ListGlobalByAccount(ctx, &acctA)
	if err != nil {
		t.Fatalf("ListGlobalByAccount(A): %v", err)
	}
	if len(aEntries) != 2 {
		t.Fatalf("account A partition: %d entries, want 2", len(aEntries))
	}
	for i, e := range aEntries {
		if e.AccountID == nil || *e.AccountID != acctA {
			t.Fatalf("A entry %d: persisted account_id = %v, want %s", i, e.AccountID, acctA)
		}
		if e.Category != "api_token_issued" {
			t.Fatalf("A entry %d: category = %q, want api_token_issued", i, e.Category)
		}
	}
	if aEntries[0].PrevHash != nil {
		t.Errorf("A genesis prev_hash = %q, want nil", *aEntries[0].PrevHash)
	}
	if aEntries[1].PrevHash == nil || *aEntries[1].PrevHash != aEntries[0].EntryHash {
		t.Errorf("A second entry must chain onto A's genesis (prev=%v, want %s)",
			aEntries[1].PrevHash, aEntries[0].EntryHash)
	}

	bEntries, err := auditRepo.ListGlobalByAccount(ctx, &acctB)
	if err != nil {
		t.Fatalf("ListGlobalByAccount(B): %v", err)
	}
	if len(bEntries) != 1 {
		t.Fatalf("account B partition: %d entries, want 1", len(bEntries))
	}
	if bEntries[0].AccountID == nil || *bEntries[0].AccountID != acctB {
		t.Fatalf("B entry: persisted account_id = %v, want %s", bEntries[0].AccountID, acctB)
	}
	if bEntries[0].PrevHash != nil {
		t.Errorf("B entry is its own genesis; prev_hash = %q, want nil", *bEntries[0].PrevHash)
	}

	unt, err := auditRepo.ListGlobalByAccount(ctx, nil)
	if err != nil {
		t.Fatalf("ListGlobalByAccount(nil): %v", err)
	}
	if len(unt) != 1 {
		t.Fatalf("untenanted partition: %d entries, want 1", len(unt))
	}
	if unt[0].AccountID != nil {
		t.Fatalf("untenanted entry account_id = %s, want nil", *unt[0].AccountID)
	}
	if unt[0].PrevHash != nil {
		t.Errorf("untenanted genesis prev_hash = %q, want nil", *unt[0].PrevHash)
	}
}

// acctGetterCampaignRepo is a campaign repo fake with a configurable
// campaign.AccountGetter capability.
type acctGetterCampaignRepo struct {
	campaign.BaseFake
	acct string
	err  error
}

func (r acctGetterCampaignRepo) GetCampaignAccountID(context.Context, uuid.UUID) (string, error) {
	return r.acct, r.err
}

// TestAuditCampaignBoardTransition_StampsCampaignAccount covers the boardsync
// caller family: the campaign-scoped work_item_transitioned entry carries the
// CAMPAIGN entity's account (resolved via the AccountGetter capability — a
// background caller has no request Identity).
func TestAuditCampaignBoardTransition_StampsCampaignAccount(t *testing.T) {
	acct := uuid.New()
	rec := &campaignAuditRecorder{}
	s := New(Config{
		AuditRepo:    rec,
		CampaignRepo: acctGetterCampaignRepo{acct: acct.String()},
	})
	c := &campaign.Campaign{ID: uuid.New(), Repo: "acme/widgets"}

	s.auditCampaignBoardTransition(context.Background(), c, lifecycleCampaignStarted, 7,
		"In Progress", &workmgmt.TransitionResult{Moved: true, From: "Backlog", To: "In Progress"})

	if len(rec.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(rec.entries))
	}
	got := rec.entries[0]
	if got.Category != categoryWorkItemTransitioned {
		t.Fatalf("category = %q, want %q", got.Category, categoryWorkItemTransitioned)
	}
	if got.AccountID == nil || *got.AccountID != acct {
		t.Fatalf("AccountID = %v, want %s", got.AccountID, acct)
	}
}

// TestCampaignAccountIDForAudit_Branches pins every defensive degrade of the
// boardsync account resolution: each falls back to nil (the untenanted NULL
// partition) rather than failing the audit write it feeds.
func TestCampaignAccountIDForAudit_Branches(t *testing.T) {
	acct := uuid.New()
	cases := []struct {
		name string
		repo campaign.Repository
		want *uuid.UUID
	}{
		{
			name: "repo without the AccountGetter capability",
			// Interface-embedding strips the concrete BaseFake's optional
			// capability methods, so the type assertion fails.
			repo: struct{ campaign.Repository }{campaign.BaseFake{}},
			want: nil,
		},
		{
			name: "lookup error degrades to untenanted with a warn",
			repo: acctGetterCampaignRepo{err: errors.New("db down")},
			want: nil,
		},
		{
			name: "untenanted campaign (empty account)",
			repo: acctGetterCampaignRepo{acct: ""},
			want: nil,
		},
		{
			name: "unparsable account value",
			repo: acctGetterCampaignRepo{acct: "not-a-uuid"},
			want: nil,
		},
		{
			name: "tenanted campaign resolves its account",
			repo: acctGetterCampaignRepo{acct: acct.String()},
			want: &acct,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{CampaignRepo: tc.repo})
			got := s.campaignAccountIDForAudit(context.Background(), uuid.New())
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %s, want nil", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("got %v, want %s", got, *tc.want)
			}
		})
	}
}

// TestIdentityAccountID_Branches pins the Identity → partition mapping every
// server-handler call site shares: no identity, an empty binding, and an
// unparsable value all degrade to nil (untenanted); a bound identity yields
// its account UUID.
func TestIdentityAccountID_Branches(t *testing.T) {
	acct := uuid.New()
	cases := []struct {
		name string
		ctx  context.Context
		want *uuid.UUID
	}{
		{"no identity in context", context.Background(), nil},
		{"identity without account binding",
			context.WithValue(context.Background(), ctxKeyIdentity,
				Identity{Subject: "github:alice"}), nil},
		{"unparsable account value",
			context.WithValue(context.Background(), ctxKeyIdentity,
				Identity{Subject: "github:alice", AccountID: "not-a-uuid"}), nil},
		{"bound identity resolves its account",
			context.WithValue(context.Background(), ctxKeyIdentity,
				Identity{Subject: "github:alice", AccountID: acct.String()}), &acct},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := identityAccountID(tc.ctx)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %s, want nil", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("got %v, want %s", got, *tc.want)
			}
		})
	}
}
