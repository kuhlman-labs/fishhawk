package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// captainPG drives the REAL captain handlers against a pgtest database: the
// production captain.Store, the Postgres run repository the claim's
// predicate reads, and a fake identity provider for the forge predicate.
type captainPG struct {
	pool  *pgxpool.Pool
	audit audit.Repository
	srv   *Server
}

func newCaptainPG(t *testing.T, idp *fakeIdentityProvider) *captainPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	cfg := Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      run.NewPostgresRepository(pool),
		CaptainStore: captain.NewStore(pool),
	}
	if idp != nil {
		cfg.IdentityProvider = idp
	}
	return &captainPG{pool: pool, audit: audit.NewPostgresRepository(pool), srv: New(cfg)}
}

// seedRunSpec seeds one run for repo carrying specBytes (nil = no cached
// spec) as its cached WorkflowSpec.
func (f *captainPG) seedRunSpec(t *testing.T, repo string, specBytes []byte) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, workflow_spec)
		VALUES ($1, $2, 'feature_change', 'sha', 'cli', 'succeeded', 'local', $3)`, uuid.New(), repo, specBytes); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

// seedEntry appends a captain entry BY CONSTRUCTION (straight onto the
// global chain, bypassing every transition control) and returns it.
func (f *captainPG) seedEntry(t *testing.T, category string, payload map[string]any) *audit.Entry {
	t.Helper()
	raw, _ := json.Marshal(payload)
	kind := audit.ActorUser
	subject, _ := payload["subject"].(string)
	e, err := f.audit.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(), Category: category, ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	})
	if err != nil {
		t.Fatalf("seed %s: %v", category, err)
	}
	return e
}

func (f *captainPG) seedCaptain(t *testing.T, repo, subject string) {
	f.seedEntry(t, captain.CategoryAssigned, map[string]any{
		"repo": repo, "subject": subject, "identity_verified": captain.IdentityVerified(subject)})
}

func (f *captainPG) seedOffer(t *testing.T, repo, by, successor string) *audit.Entry {
	return f.seedEntry(t, captain.CategoryHandoverOffered, map[string]any{
		"repo": repo, "subject": by, "identity_verified": true, "successor": successor})
}

type captainChainRow struct {
	Category string
	Payload  map[string]any
}

// chain reads repo's captain entries straight from audit_entries (not
// through the code under test), ascending.
func (f *captainPG) chain(t *testing.T, repo string) []captainChainRow {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT category, payload FROM audit_entries
		WHERE run_id IS NULL AND category LIKE 'captain_%' AND payload->>'repo' = $1 ORDER BY sequence`, repo)
	if err != nil {
		t.Fatalf("read chain: %v", err)
	}
	defer rows.Close()
	var out []captainChainRow
	for rows.Next() {
		var r captainChainRow
		var raw []byte
		if err := rows.Scan(&r.Category, &raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		_ = json.Unmarshal(raw, &r.Payload)
		out = append(out, r)
	}
	return out
}

func (f *captainPG) get(t *testing.T, repo string) (captainResponse, string) {
	t.Helper()
	w := serveCaptain(t, f.srv, http.MethodGet, "/v0/captain?repo="+repo, "", &Identity{Subject: "github:reader"})
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v0/captain = %d:\n%s", w.Code, w.Body.String())
	}
	var body captainResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body, w.Body.String()
}

// verb POSTs one verb as subject (a cookie-session identity) and returns
// status + body.
func (f *captainPG) verb(t *testing.T, name, subject, body string) (int, string) {
	t.Helper()
	w := serveCaptain(t, f.srv, http.MethodPost, "/v0/captain/"+name, body, &Identity{Subject: subject})
	return w.Code, w.Body.String()
}

func captainBody(repo string) string { return `{"repo":"` + repo + `"}` }

func captainCode(t *testing.T, body string) string {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, body)
	}
	return env.Error.Code
}

var trivialSpec = captainApprovalSpec([2]string{})

// TestCaptain_HandoverEndToEnd: offer then accept returns 200, GET reports
// the NEW captain and the chain holds a captain_assigned naming the offer.
func TestCaptain_HandoverEndToEnd(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/handover"
	f.seedCaptain(t, repo, "github:alice")
	if code, body := f.verb(t, "offer", "github:alice", `{"repo":"`+repo+`","successor":"github:bob"}`); code != 200 {
		t.Fatalf("offer = %d %s", code, body)
	}
	got, _ := f.get(t, repo)
	if got.PendingOffer == nil || got.PendingOffer.Successor != "github:bob" || got.PendingOffer.OfferedBy != "github:alice" {
		t.Fatalf("pending_offer = %+v, want alice -> bob", got.PendingOffer)
	}
	if code, body := f.verb(t, "accept", "github:bob", captainBody(repo)); code != 200 {
		t.Fatalf("accept = %d %s", code, body)
	}
	got, _ = f.get(t, repo)
	if got.Captain == nil || got.Captain.Subject != "github:bob" || got.Captain.Basis != "assigned" || got.Captain.ClaimVerified != nil {
		t.Errorf("captain = %+v, want github:bob assigned with claim_verified null", got.Captain)
	}
	if got.PendingOffer != nil {
		t.Errorf("pending_offer = %+v, want cleared", got.PendingOffer)
	}
	chain := f.chain(t, repo)
	last := chain[len(chain)-1]
	if last.Category != captain.CategoryAssigned || last.Payload["previous_captain"] != "github:alice" || last.Payload["offer_entry_hash"] != got.History[1].EntryHash {
		t.Errorf("last entry = %+v, want captain_assigned naming the offer and alice", last)
	}
}

// TestCaptain_OfferThenWithdraw: GET reports the ORIGINAL captain and no
// pending offer.
func TestCaptain_OfferThenWithdraw(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/withdraw"
	f.seedCaptain(t, repo, "github:alice")
	if code, body := f.verb(t, "offer", "github:alice", `{"repo":"`+repo+`","successor":"github:bob"}`); code != 200 {
		t.Fatalf("offer = %d %s", code, body)
	}
	if code, body := f.verb(t, "withdraw", "github:alice", captainBody(repo)); code != 200 {
		t.Fatalf("withdraw = %d %s", code, body)
	}
	got, _ := f.get(t, repo)
	if got.Captain == nil || got.Captain.Subject != "github:alice" || got.PendingOffer != nil {
		t.Errorf("record = captain %+v offer %+v, want alice and no offer", got.Captain, got.PendingOffer)
	}
}

// TestCaptain_WithdrawByNonOffererRefused: the ONE withdraw-authorization
// check through the real handler — the offer survives.
func TestCaptain_WithdrawByNonOffererRefused(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/withdraw-other"
	f.seedCaptain(t, repo, "github:alice")
	f.seedOffer(t, repo, "github:alice", "github:carol")
	code, body := f.verb(t, "withdraw", "github:bob", captainBody(repo))
	if code != http.StatusForbidden || captainCode(t, body) != "captain_not_offerer" {
		t.Fatalf("withdraw by bob = %d %s, want 403 captain_not_offerer", code, body)
	}
	if got, _ := f.get(t, repo); got.PendingOffer == nil {
		t.Error("the offer was cleared by a non-offerer")
	}
}

// TestCaptain_AcceptWithoutOfferLeavesChainUnchanged (C3): 409
// captain_no_offer and the GET body is byte-unchanged.
func TestCaptain_AcceptWithoutOfferLeavesChainUnchanged(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/no-offer"
	f.seedCaptain(t, repo, "github:alice")
	_, before := f.get(t, repo)
	code, body := f.verb(t, "accept", "github:bob", captainBody(repo))
	if code != http.StatusConflict || captainCode(t, body) != "captain_no_offer" {
		t.Errorf("accept = %d %s, want 409 captain_no_offer", code, body)
	}
	if _, after := f.get(t, repo); after != before {
		t.Errorf("GET changed after a refused accept:\nbefore %s\nafter  %s", before, after)
	}
}

// TestCaptain_OfferByNonCaptainLeavesChainUnchanged (C2): bob, a non-agent
// who is not captain, cannot offer alice's seat.
func TestCaptain_OfferByNonCaptainLeavesChainUnchanged(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/offer-other"
	f.seedCaptain(t, repo, "github:alice")
	code, body := f.verb(t, "offer", "github:bob", `{"repo":"`+repo+`","successor":"github:carol"}`)
	if code != http.StatusForbidden || captainCode(t, body) != "captain_not_captain" {
		t.Errorf("offer by bob = %d %s, want 403 captain_not_captain", code, body)
	}
	if n := len(f.chain(t, repo)); n != 1 {
		t.Errorf("chain has %d entries, want 1 (no offer appended)", n)
	}
	if got, _ := f.get(t, repo); got.PendingOffer != nil {
		t.Errorf("pending_offer = %+v, want none", got.PendingOffer)
	}
}

// TestCaptain_ClaimWhileCaptainExistsRefused (C4): bob satisfies a
// non-trivial predicate, so only the seat-held check refuses him.
func TestCaptain_ClaimWhileCaptainExistsRefused(t *testing.T) {
	f := newCaptainPG(t, &fakeIdentityProvider{perm: identity.PermissionAdmin})
	const repo = "acme/held"
	f.seedRunSpec(t, repo, captainApprovalSpec([2]string{"admin", ""}))
	f.seedCaptain(t, repo, "github:alice")
	code, body := f.verb(t, "claim", "github:bob", captainBody(repo))
	if code != http.StatusConflict || captainCode(t, body) != "captain_exists" {
		t.Errorf("claim = %d %s, want 409 captain_exists", code, body)
	}
	if n := len(f.chain(t, repo)); n != 1 {
		t.Errorf("chain has %d entries, want 1", n)
	}
	if got, _ := f.get(t, repo); got.Captain == nil || got.Captain.Subject != "github:alice" {
		t.Errorf("captain = %+v, want alice", got.Captain)
	}
}

// TestCaptain_RelinquishByCaptainVacates: 200 and GET reports a vacant seat.
func TestCaptain_RelinquishByCaptainVacates(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/relinquish"
	f.seedCaptain(t, repo, "github:alice")
	if code, body := f.verb(t, "relinquish", "github:alice", captainBody(repo)); code != 200 {
		t.Fatalf("relinquish = %d %s", code, body)
	}
	got, _ := f.get(t, repo)
	if got.Captain != nil || got.LastCaptain == nil || *got.LastCaptain != "github:alice" {
		t.Errorf("record = captain %+v last %v, want vacant with last_captain alice", got.Captain, got.LastCaptain)
	}
}

// TestCaptain_RelinquishByNonCaptainRefused (C7).
func TestCaptain_RelinquishByNonCaptainRefused(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/relinquish-other"
	f.seedCaptain(t, repo, "github:alice")
	_, before := f.get(t, repo)
	code, body := f.verb(t, "relinquish", "github:bob", captainBody(repo))
	if code != http.StatusForbidden || captainCode(t, body) != "captain_not_captain" {
		t.Errorf("relinquish by bob = %d %s, want 403 captain_not_captain", code, body)
	}
	if _, after := f.get(t, repo); after != before {
		t.Errorf("GET changed after a refused relinquish:\nbefore %s\nafter  %s", before, after)
	}
}

// TestCaptain_TrivialPredicateClaimIsUnverified (C11): a claim under a
// positively-trivial predicate records claim_verified FALSE on the persisted
// payload and the GET body.
func TestCaptain_TrivialPredicateClaimIsUnverified(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/trivial"
	f.seedRunSpec(t, repo, trivialSpec)
	if code, body := f.verb(t, "claim", "github:qualified", captainBody(repo)); code != 200 {
		t.Fatalf("claim = %d %s", code, body)
	}
	chain := f.chain(t, repo)
	if len(chain) != 1 || chain[0].Category != captain.CategoryClaimed {
		t.Fatalf("chain = %+v, want one captain_claimed", chain)
	}
	p := chain[0].Payload
	if p["claim_verified"] != false || p["identity_verified"] != true || p["predicate_basis"] != captainTrivialBasis || p["page_pending"] != true {
		t.Errorf("payload = %v, want claim_verified false, identity_verified true, trivial basis, page_pending", p)
	}
	got, _ := f.get(t, repo)
	if got.Captain == nil || got.Captain.ClaimVerified == nil || *got.Captain.ClaimVerified {
		t.Errorf("GET captain = %+v, want claim_verified false", got.Captain)
	}
}

// TestCaptain_ReadRendersClaimVerifiedSeparately (C12): the one state where
// the two flags DISAGREE — identity_verified true, claim_verified false —
// asserted on the raw GET body.
func TestCaptain_ReadRendersClaimVerifiedSeparately(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/separate"
	f.seedRunSpec(t, repo, trivialSpec)
	if code, body := f.verb(t, "claim", "github:qualified", captainBody(repo)); code != 200 {
		t.Fatalf("claim = %d %s", code, body)
	}
	_, raw := f.get(t, repo)
	var body struct {
		Captain map[string]any `json:"captain"`
	}
	_ = json.Unmarshal([]byte(raw), &body)
	if body.Captain["identity_verified"] != true || body.Captain["claim_verified"] != false {
		t.Errorf("captain = %v, want identity_verified true and claim_verified false", body.Captain)
	}
}

// TestCaptain_NonTrivialSatisfiedClaimIsVerified: claim_verified TRUE.
func TestCaptain_NonTrivialSatisfiedClaimIsVerified(t *testing.T) {
	f := newCaptainPG(t, &fakeIdentityProvider{perm: identity.PermissionAdmin})
	const repo = "acme/verified"
	f.seedRunSpec(t, repo, captainApprovalSpec([2]string{"admin", ""}))
	if code, body := f.verb(t, "claim", "github:bob", captainBody(repo)); code != 200 {
		t.Fatalf("claim = %d %s", code, body)
	}
	chain := f.chain(t, repo)
	if len(chain) != 1 || chain[0].Payload["claim_verified"] != true || chain[0].Payload["predicate_basis"] != "min_permission:admin" {
		t.Errorf("chain = %+v, want one claim with claim_verified true", chain)
	}
	if got, _ := f.get(t, repo); got.Captain == nil || got.Captain.ClaimVerified == nil || !*got.Captain.ClaimVerified {
		t.Errorf("GET captain = %+v, want claim_verified true", got.Captain)
	}
}

// TestCaptain_ClaimFailingStrictestPredicateRefused (C8): min_permission
// admin on ONE gate, the claimant resolves to read — 403 and no entry.
func TestCaptain_ClaimFailingStrictestPredicateRefused(t *testing.T) {
	f := newCaptainPG(t, &fakeIdentityProvider{perm: identity.PermissionRead})
	const repo = "acme/rejected"
	f.seedRunSpec(t, repo, captainApprovalSpec([2]string{}, [2]string{"admin", ""}))
	code, body := f.verb(t, "claim", "github:bob", captainBody(repo))
	if code != http.StatusForbidden || captainCode(t, body) != "captain_predicate_rejected" {
		t.Errorf("claim = %d %s, want 403 captain_predicate_rejected", code, body)
	}
	if n := len(f.chain(t, repo)); n != 0 {
		t.Errorf("chain has %d entries, want 0", n)
	}
	if got, _ := f.get(t, repo); got.Captain != nil {
		t.Errorf("captain = %+v, want vacant", got.Captain)
	}
}

// TestCaptain_ClaimWithUndeterminablePredicateRefused (C9/C10): each
// undeterminable basis refuses 422 and appends nothing.
func TestCaptain_ClaimWithUndeterminablePredicateRefused(t *testing.T) {
	cases := []struct {
		name      string
		idp       *fakeIdentityProvider
		spec      []byte
		seedRun   bool
		wantBasis string
	}{
		{"no run", nil, nil, false, "undeterminable:no_run"},
		{"no cached spec", nil, nil, true, "undeterminable:no_cached_spec"},
		{"malformed spec", nil, []byte("version: [broken"), true, "undeterminable:spec_unparseable"},
		{"identity provider erroring", &fakeIdentityProvider{permErr: errors.New("forge down")}, captainApprovalSpec([2]string{"admin", ""}), true, "undeterminable:identity_unavailable"},
		{"identity provider unwired", nil, captainApprovalSpec([2]string{"admin", ""}), true, "undeterminable:identity_unconfigured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCaptainPG(t, tc.idp)
			const repo = "acme/undeterminable"
			if tc.seedRun {
				f.seedRunSpec(t, repo, tc.spec)
			}
			code, body := f.verb(t, "claim", "github:bob", captainBody(repo))
			if code != http.StatusUnprocessableEntity || captainCode(t, body) != "captain_predicate_undeterminable" {
				t.Fatalf("claim = %d %s, want 422 captain_predicate_undeterminable", code, body)
			}
			var env errorEnvelope
			_ = json.Unmarshal([]byte(body), &env)
			if env.Error.Details["predicate_basis"] != tc.wantBasis {
				t.Errorf("predicate_basis = %v, want %s", env.Error.Details["predicate_basis"], tc.wantBasis)
			}
			if n := len(f.chain(t, repo)); n != 0 {
				t.Errorf("chain has %d entries, want 0", n)
			}
			if got, _ := f.get(t, repo); got.Captain != nil {
				t.Errorf("captain = %+v, want vacant", got.Captain)
			}
		})
	}
}

// TestCaptain_ClaimAfterRelinquishRecordsPreviousCaptain (C14):
// assigned(alice) -> relinquished(alice) -> claim(bob) records
// previous_captain alice on the persisted payload.
func TestCaptain_ClaimAfterRelinquishRecordsPreviousCaptain(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/previous"
	f.seedRunSpec(t, repo, trivialSpec)
	f.seedCaptain(t, repo, "github:alice")
	if code, body := f.verb(t, "relinquish", "github:alice", captainBody(repo)); code != 200 {
		t.Fatalf("relinquish = %d %s", code, body)
	}
	if code, body := f.verb(t, "claim", "github:bob", captainBody(repo)); code != 200 {
		t.Fatalf("claim = %d %s", code, body)
	}
	chain := f.chain(t, repo)
	last := chain[len(chain)-1]
	if last.Category != captain.CategoryClaimed || last.Payload["previous_captain"] != "github:alice" {
		t.Errorf("last entry = %+v, want captain_claimed with previous_captain github:alice", last)
	}
}

// TestCaptain_StaticSubjectCarriesIdentityUnverified (C13): a static subject
// that claims (and is then offered to) carries identity_verified FALSE on
// the GET captain, the GET pending offer, and every entry payload.
func TestCaptain_StaticSubjectCarriesIdentityUnverified(t *testing.T) {
	f := newCaptainPG(t, nil)
	const repo = "acme/static"
	f.seedRunSpec(t, repo, trivialSpec)
	if code, body := f.verb(t, "claim", "brett@local-mcp", captainBody(repo)); code != 200 {
		t.Fatalf("claim = %d %s", code, body)
	}
	if code, body := f.verb(t, "offer", "brett@local-mcp", `{"repo":"`+repo+`","successor":"ops@local"}`); code != 200 {
		t.Fatalf("offer = %d %s", code, body)
	}
	got, _ := f.get(t, repo)
	if got.Captain == nil || got.Captain.IdentityVerified {
		t.Errorf("GET captain = %+v, want identity_verified false", got.Captain)
	}
	if got.PendingOffer == nil || got.PendingOffer.IdentityVerified {
		t.Errorf("GET pending_offer = %+v, want identity_verified false", got.PendingOffer)
	}
	for _, e := range f.chain(t, repo) {
		if e.Payload["identity_verified"] != false {
			t.Errorf("%s payload identity_verified = %v, want false", e.Category, e.Payload["identity_verified"])
		}
		if v, ok := e.Payload["successor_identity_verified"]; ok && v != false {
			t.Errorf("%s payload successor_identity_verified = %v, want false", e.Category, v)
		}
	}
}

// TestCaptain_AgentSubjectRefusedOnEveryVerb (C1): an operator-agent token
// holding write:approvals is refused 403 on every verb, in the one state per
// verb where it would otherwise succeed, and the chain gains nothing. A
// delegated:true human claim is refused the same way.
func TestCaptain_AgentSubjectRefusedOnEveryVerb(t *testing.T) {
	f := newCaptainPG(t, nil)
	agent := operatorrole.CampaignActorSubject
	agentID := &Identity{Subject: agent, TokenID: "tok-agent", Scopes: []string{scopeCaptainRead, scopeCaptainWrite}}
	cases := []struct {
		verb, repo, body string
		seed             func(repo string)
	}{
		{"claim", "acme/agent-claim", "", func(repo string) { f.seedRunSpec(t, repo, trivialSpec) }},
		{"offer", "acme/agent-offer", `,"successor":"github:bob"`, func(repo string) { f.seedCaptain(t, repo, agent) }},
		{"relinquish", "acme/agent-relinquish", "", func(repo string) { f.seedCaptain(t, repo, agent) }},
		{"accept", "acme/agent-accept", "", func(repo string) {
			f.seedCaptain(t, repo, "github:alice")
			f.seedOffer(t, repo, "github:alice", agent)
		}},
		{"withdraw", "acme/agent-withdraw", "", func(repo string) {
			f.seedCaptain(t, repo, agent)
			f.seedOffer(t, repo, agent, "github:bob")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			tc.seed(tc.repo)
			before := len(f.chain(t, tc.repo))
			_, getBefore := f.get(t, tc.repo)
			w := serveCaptain(t, f.srv, http.MethodPost, "/v0/captain/"+tc.verb, `{"repo":"`+tc.repo+`"`+tc.body+`}`, agentID)
			if w.Code != http.StatusForbidden || captainErrCode(t, w) != "captain_agent_identity_refused" {
				t.Errorf("%s by agent = %d %s, want 403 captain_agent_identity_refused", tc.verb, w.Code, w.Body.String())
			}
			if after := len(f.chain(t, tc.repo)); after != before {
				t.Errorf("chain grew %d -> %d on a refused agent %s", before, after, tc.verb)
			}
			if _, getAfter := f.get(t, tc.repo); getAfter != getBefore {
				t.Errorf("GET changed after a refused agent %s:\nbefore %s\nafter  %s", tc.verb, getBefore, getAfter)
			}
		})
	}
	t.Run("delegated human claim", func(t *testing.T) {
		const repo = "acme/delegated-claim"
		f.seedRunSpec(t, repo, trivialSpec)
		code, body := f.verb(t, "claim", "github:alice", `{"repo":"`+repo+`","delegated":true}`)
		if code != http.StatusForbidden || captainCode(t, body) != "captain_agent_identity_refused" {
			t.Errorf("delegated claim = %d %s, want 403 captain_agent_identity_refused", code, body)
		}
		if n := len(f.chain(t, repo)); n != 0 {
			t.Errorf("chain has %d entries, want 0", n)
		}
	})
}

// TestCaptain_SeatedCaptainDoesNotAffectApproval (ADR-083 rule 1): with a
// captain seated for the run's repo on a real captain store, a normal gate
// approval by a DIFFERENT subject advances with the same status, the same
// stage state and the same approval_submitted payload shape as the identical
// approval on a server with no captain record at all.
func TestCaptain_SeatedCaptainDoesNotAffectApproval(t *testing.T) {
	approve := func(withCaptain bool) (int, run.StageState, []string) {
		s, _, rr, au, _ := newApprovalServerWithIdentity(t, &fakeIdentityProvider{perm: identity.PermissionAdmin})
		stage := seedPredicateStage(rr, au, 1, "write", "", "github:author")
		if withCaptain {
			f := newCaptainPG(t, nil)
			repo := rr.runs[stage.RunID].Repo
			f.seedCaptain(t, repo, "github:captain-a")
			s.cfg.CaptainStore = f.srv.cfg.CaptainStore
			if got, _ := f.get(t, repo); got.Captain == nil || got.Captain.Subject != "github:captain-a" {
				t.Fatalf("captain not seated: %+v", got.Captain)
			}
		}
		w := submitApprovalAs(t, s, stage.ID, "github:approver-b", `{"decision":"approve"}`)
		var keys []string
		for _, p := range au.appended {
			if p.Category != "approval_submitted" {
				continue
			}
			var m map[string]any
			_ = json.Unmarshal(p.Payload, &m)
			for k := range m {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		return w.Code, rr.stages[stage.ID].State, keys
	}
	c0, s0, k0 := approve(false)
	c1, s1, k1 := approve(true)
	if c0 != http.StatusOK || c1 != c0 || s1 != s0 || !reflect.DeepEqual(k1, k0) {
		t.Errorf("with captain: status %d state %s keys %v; without: status %d state %s keys %v — want identical 200s",
			c1, s1, k1, c0, s0, k0)
	}
}

// seedAccountCaptain seats subject on repo inside acct's partition, by
// construction on the global chain.
func (f *captainPG) seedAccountCaptain(t *testing.T, acct uuid.UUID, repo, subject string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, acct, "acct-"+acct.String()[:8]); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{"repo": repo, "subject": subject, "identity_verified": captain.IdentityVerified(subject)})
	kind := audit.ActorUser
	if _, err := f.audit.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(), Category: captain.CategoryAssigned, ActorKind: &kind, ActorSubject: &subject,
		Payload: raw, AccountID: &acct,
	}); err != nil {
		t.Fatalf("seed account captain: %v", err)
	}
}

// TestCurrentCaptain_Trichotomy pins the shared read (E76.3 / #3766): each
// basis on its own fixture, against the REAL captain.Store.
func TestCurrentCaptain_Trichotomy(t *testing.T) {
	f := newCaptainPG(t, nil)
	f.seedCaptain(t, "acme/seated", "github:alice")
	f.seedCaptain(t, "acme/vacated", "github:bob")
	f.seedEntry(t, captain.CategoryRelinquished, map[string]any{"repo": "acme/vacated", "subject": "github:bob", "identity_verified": true})
	ctx := context.Background()

	t.Run("captain", func(t *testing.T) {
		subject, verified, basis := f.srv.currentCaptain(ctx, nil, "acme/seated")
		if subject != "github:alice" || !verified || basis != captainBasisCaptain {
			t.Errorf("got (%q, %v, %q), want (github:alice, true, captain)", subject, verified, basis)
		}
	})
	t.Run("static subject is seated but unverified", func(t *testing.T) {
		f.seedCaptain(t, "acme/static", "brett@local-mcp")
		subject, verified, basis := f.srv.currentCaptain(ctx, nil, "acme/static")
		if subject != "brett@local-mcp" || verified || basis != captainBasisCaptain {
			t.Errorf("got (%q, %v, %q), want (brett@local-mcp, false, captain)", subject, verified, basis)
		}
	})
	t.Run("vacant after relinquish", func(t *testing.T) {
		subject, _, basis := f.srv.currentCaptain(ctx, nil, "acme/vacated")
		if subject != "" || basis != captainBasisVacant {
			t.Errorf("got (%q, %q), want vacant", subject, basis)
		}
	})
	t.Run("vacant with no history", func(t *testing.T) {
		if _, _, basis := f.srv.currentCaptain(ctx, nil, "acme/never"); basis != captainBasisVacant {
			t.Errorf("basis = %q, want vacant", basis)
		}
	})
	t.Run("unavailable on a read error", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		subject, _, basis := f.srv.currentCaptain(cancelled, nil, "acme/seated")
		if subject != "" || basis != captainBasisUnavailable {
			t.Errorf("a failed read must be unavailable (never vacant); got (%q, %q)", subject, basis)
		}
	})
	t.Run("unavailable when the store is not wired", func(t *testing.T) {
		bare := New(Config{Addr: "127.0.0.1:0"})
		if _, _, basis := bare.currentCaptain(ctx, nil, "acme/seated"); basis != captainBasisUnavailable {
			t.Errorf("basis = %q, want unavailable", basis)
		}
		if bare.issueCommentCaptainResolver() != nil {
			t.Errorf("an unwired store must leave the notifier's resolver nil (today's notifier)")
		}
	})
}

// TestIssueCommentCaptainResolver_UsesRunAccountNotCtx pins approval
// condition 4: the notifier's resolver reads the partition named by the
// account id it is GIVEN (the run's), from a context carrying NO request
// identity. The seat exists only in acct's partition, so resolving from ctx
// (which names no account → the untenanted partition) would report vacant.
func TestIssueCommentCaptainResolver_UsesRunAccountNotCtx(t *testing.T) {
	f := newCaptainPG(t, nil)
	acct := uuid.New()
	f.seedAccountCaptain(t, acct, "acme/tenant", "github:alice")
	resolve := f.srv.issueCommentCaptainResolver()
	if resolve == nil {
		t.Fatal("a wired CaptainStore must yield a resolver")
	}
	ctx := context.Background() // no request identity
	if IdentityFrom(ctx).AccountID != "" {
		t.Fatal("fixture: ctx must carry no identity")
	}

	got := resolve(ctx, acct.String(), "acme/tenant")
	if got.Basis != captainBasisCaptain || got.Subject != "github:alice" || !got.IdentityVerified {
		t.Errorf("run-account read = %+v, want the seated github:alice", got)
	}
	if other := resolve(ctx, "", "acme/tenant"); other.Basis != captainBasisVacant {
		t.Errorf("the untenanted partition holds no seat; got %+v", other)
	}
	if bad := resolve(ctx, "not-a-uuid", "acme/tenant"); bad.Basis != captainBasisUnavailable {
		t.Errorf("an unparseable run account must be unavailable, never the untenanted read; got %+v", bad)
	}
}
