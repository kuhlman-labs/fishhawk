package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/firstofficer"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/specialistjob"
)

// First-officer seat identity tests (E85.6 / #4148). Counterfactuals run by
// hand against these tests (control deleted or mutated, test run, RED
// observed, file restored byte-identically):
//
//   - C1 clamp: deleting the ClampScopes line in resolveFirstOfficerSeat
//     turned TestFirstOfficerSeat_HandMintedWriteApprovalsCannotApprove RED
//     (status 200, the plan stage approved to succeeded) and
//     TestFirstOfficerSeat_ClampsScopes RED.
//   - Also RED under mutation: the invalid-subject 401, the unwired-repo 503,
//     the run-seat own-run and campaign-membership checks in
//     firstOfficerRunInSeat, the revoked-only retirement row, and the
//     seat-only refusal hook (a non-seat 403 then wrote a row).
//   - C4 refused-action audit: deleting writeError's recordFirstOfficerRefusal
//     call turned TestFirstOfficerSeat_HandMintedWriteApprovalsCannotApprove
//     and TestFirstOfficerSeat_RefusalAuditRow RED (zero
//     first_officer_action_refused appends).
//   - C8 retirement: deleting the `st.missing || st.terminal` branch turned
//     TestFirstOfficerSeat_RetiresOnTerminalAnchor RED (the seat reached the
//     handler); skipping the Revoke call turned it RED on the committed-state
//     read (ListForSubject still returned the active token).
//   - C9 fail-closed: making the anchor read-error branch fall through turned
//     TestFirstOfficerSeat_AnchorReadError503 RED (the request reached the
//     handler).
//   - Condition 1 account stamp: deleting `id.AccountID = st.accountID` turned
//     TestFirstOfficerSeat_TenantedAnchor RED (same-account read 403
//     account_forbidden).

// foCampaignRepo is a campaign fake for the seat tests: campaigns by id, their
// accounts, and items keyed by run.
type foCampaignRepo struct {
	campaign.BaseFake
	mu        sync.Mutex
	campaigns map[uuid.UUID]*campaign.Campaign
	accounts  map[uuid.UUID]string
	byRun     map[uuid.UUID][]*campaign.Item
	getErr    error
	acctErr   error
	listErr   error
}

func newFOCampaignRepo() *foCampaignRepo {
	return &foCampaignRepo{
		campaigns: map[uuid.UUID]*campaign.Campaign{},
		accounts:  map[uuid.UUID]string{},
		byRun:     map[uuid.UUID][]*campaign.Item{},
	}
}

func (r *foCampaignRepo) GetCampaign(_ context.Context, id uuid.UUID) (*campaign.Campaign, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return nil, r.getErr
	}
	c, ok := r.campaigns[id]
	if !ok {
		return nil, campaign.ErrNotFound
	}
	return c, nil
}

func (r *foCampaignRepo) GetCampaignAccountID(_ context.Context, id uuid.UUID) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.acctErr != nil {
		return "", r.acctErr
	}
	acct, ok := r.accounts[id]
	if !ok {
		return "", campaign.ErrNotFound
	}
	return acct, nil
}

func (r *foCampaignRepo) ListCampaignItemsForRun(_ context.Context, runID uuid.UUID) ([]*campaign.Item, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.byRun[runID], nil
}

// foAuditFake records chained and global appends.
type foAuditFake struct {
	audit.BaseFake
	mu        sync.Mutex
	chained   []audit.ChainAppendParams
	global    []audit.GlobalChainAppendParams
	globalErr error
}

func (a *foAuditFake) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.chained = append(a.chained, p)
	return &audit.Entry{ID: uuid.New()}, nil
}

func (a *foAuditFake) AppendGlobalChained(_ context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.globalErr != nil {
		return nil, a.globalErr
	}
	a.global = append(a.global, p)
	return &audit.Entry{ID: uuid.New()}, nil
}

func (a *foAuditFake) globalOf(category string) []audit.GlobalChainAppendParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.GlobalChainAppendParams
	for _, p := range a.global {
		if p.Category == category {
			out = append(out, p)
		}
	}
	return out
}

func (a *foAuditFake) chainedOf(category string) []audit.ChainAppendParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, p := range a.chained {
		if p.Category == category {
			out = append(out, p)
		}
	}
	return out
}

type seatFixture struct {
	t      *testing.T
	s      *Server
	tokens *fakeTokenRepo
	runs   *fakeRepo
	camps  *foCampaignRepo
	aud    *foAuditFake
}

func newSeatFixture(t *testing.T) *seatFixture {
	t.Helper()
	f := &seatFixture{t: t, tokens: newFakeTokenRepo(), runs: newFakeRepo(), camps: newFOCampaignRepo(), aud: &foAuditFake{}}
	f.s = New(Config{
		Addr: "127.0.0.1:0", APITokenRepo: f.tokens, RunRepo: f.runs, CampaignRepo: f.camps,
		AuditRepo: f.aud, AccountRoles: fakeAccountRoles{role: account.RoleAdmin},
	})
	return f
}

func (f *seatFixture) seedRun(state run.State, acct string) uuid.UUID {
	id := uuid.New()
	f.runs.mu.Lock()
	f.runs.runs[id] = &run.Run{ID: id, Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", State: state, AccountID: acct,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	f.runs.mu.Unlock()
	return id
}

func (f *seatFixture) seedCampaign(state campaign.State, acct string) uuid.UUID {
	id := uuid.New()
	f.camps.mu.Lock()
	f.camps.campaigns[id] = &campaign.Campaign{ID: id, Repo: "kuhlman-labs/fishhawk", State: state}
	f.camps.accounts[id] = acct
	f.camps.mu.Unlock()
	return id
}

func (f *seatFixture) linkRun(campaignID, runID uuid.UUID) {
	f.camps.mu.Lock()
	f.camps.byRun[runID] = append(f.camps.byRun[runID], &campaign.Item{ID: uuid.New(), CampaignID: campaignID, RunID: &runID})
	f.camps.mu.Unlock()
}

// mint issues a token DIRECTLY in the token store (bypassing any attach
// route), so a test can hand-mint a seat token with any scope set.
func (f *seatFixture) mint(subject string, scopes ...string) string {
	f.t.Helper()
	tok, err := f.tokens.Issue(context.Background(), subject, scopes)
	if err != nil {
		f.t.Fatalf("issue: %v", err)
	}
	return tok.PlainText
}

func (f *seatFixture) activeTokens(subject string) int {
	f.t.Helper()
	toks, err := f.tokens.ListForSubject(context.Background(), subject)
	if err != nil {
		f.t.Fatalf("list: %v", err)
	}
	return len(toks)
}

func (f *seatFixture) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, req)
	return w
}

// through runs bearer through bearerAuth alone, returning the identity the
// next handler saw (reached=false when the middleware refused).
func (f *seatFixture) through(bearer string) (Identity, *httptest.ResponseRecorder, bool) {
	f.t.Helper()
	var got Identity
	reached := false
	h := f.s.bearerAuth(f.tokens, nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, reached = IdentityFrom(r.Context()), true
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return got, w, reached
}

func runSeat(id uuid.UUID) string {
	return firstofficer.SubjectFor(firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: id})
}

func campaignSeat(id uuid.UUID) string {
	return firstofficer.SubjectFor(firstofficer.Anchor{Kind: firstofficer.AnchorCampaign, ID: id})
}

func TestFirstOfficerSeat_ClampsScopes(t *testing.T) {
	f := newSeatFixture(t)
	runID := f.seedRun(run.StateRunning, "")
	bearer := f.mint(runSeat(runID), "write:approvals", "write:runs", "read:runs", "write:stages", "write:messages")
	id, w, reached := f.through(bearer)
	if !reached {
		t.Fatalf("live seat refused: %d %s", w.Code, w.Body.String())
	}
	if got, want := strings.Join(id.Scopes, ","), "read:runs,write:messages"; got != want {
		t.Fatalf("seat scopes = %q, want %q (clamped to the seat profile)", got, want)
	}
	// A non-seat token is never clamped.
	human := f.mint("github:octocat", "write:approvals", "write:runs")
	hid, _, _ := f.through(human)
	if got := strings.Join(hid.Scopes, ","); got != "write:approvals,write:runs" {
		t.Fatalf("human scopes = %q; the clamp must touch only seat subjects", got)
	}
}

// C1 + condition 6: a seat token hand-minted with write:approvals (bypassing
// any attach route) still cannot approve: the clamp drops the scope, the
// approval is refused 403 insufficient_scope, NO approval row is recorded,
// and the refusal leaves a first_officer_action_refused row.
func TestFirstOfficerSeat_HandMintedWriteApprovalsCannotApprove(t *testing.T) {
	tokens, ar, rr, aud := newFakeTokenRepo(), newFakeApprovalRepo(), newApprovalRunRepo(), &foAuditFake{}
	s := New(Config{Addr: "127.0.0.1:0", APITokenRepo: tokens, ApprovalRepo: ar, RunRepo: rr, AuditRepo: aud,
		IdentityProvider: &fakeIdentityProvider{perm: identity.PermissionAdmin, member: true}})
	stage := rr.seedStage(run.StageStateAwaitingApproval)
	rr.seedRun(&run.Run{ID: stage.RunID, Repo: "kuhlman-labs/fishhawk", State: run.StateRunning})
	tok, err := tokens.Issue(context.Background(), runSeat(stage.RunID), []string{"write:approvals", "write:runs", "read:runs"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/stages/"+stage.ID.String()+"/approvals", strings.NewReader(`{"decision":"approve"}`))
	req.Header.Set("Authorization", "Bearer "+tok.PlainText)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope")
	if n := len(ar.all); n != 0 {
		t.Fatalf("a seat recorded %d approval(s)", n)
	}
	refused := aud.globalOf(auditFirstOfficerActionRefused)
	if len(refused) != 1 {
		t.Fatalf("first_officer_action_refused appends = %d, want 1", len(refused))
	}
	var p map[string]any
	_ = json.Unmarshal(refused[0].Payload, &p)
	if p["seat_subject"] != runSeat(stage.RunID) || p["code"] != "insufficient_scope" ||
		p["required_scope"] != "write:approvals" || p["method"] != http.MethodPost {
		t.Fatalf("refusal payload = %v", p)
	}
	if refused[0].ActorKind == nil || *refused[0].ActorKind != audit.ActorAgent {
		t.Fatalf("refusal actor kind = %v, want agent", refused[0].ActorKind)
	}
}

// The other prohibited verbs (ADR-089 D2) are each a 403 plus a refusal row.
func TestFirstOfficerSeat_ProhibitedVerbsRefused(t *testing.T) {
	f := newSeatFixture(t)
	runID := f.seedRun(run.StateRunning, "")
	bearer := f.mint(runSeat(runID), "write:approvals", "write:runs", "write:stages", "read:runs")
	stageID := uuid.New()
	for _, path := range []string{
		"/v0/runs/" + runID.String() + "/merge",
		"/v0/runs/" + runID.String() + "/stages/" + stageID.String() + "/host-dispatch",
		"/v0/runs/" + runID.String() + "/stages/" + stageID.String() + "/reap-failure",
	} {
		before := len(f.aud.globalOf(auditFirstOfficerActionRefused))
		w := f.do(http.MethodPost, path, bearer, `{}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: status %d, want 403; body %s", path, w.Code, w.Body.String())
		}
		if after := len(f.aud.globalOf(auditFirstOfficerActionRefused)); after != before+1 {
			t.Fatalf("%s: refusal rows %d -> %d, want +1", path, before, after)
		}
	}
	// A non-seat 403 writes no refusal row.
	before := len(f.aud.globalOf(auditFirstOfficerActionRefused))
	human := f.mint("github:octocat", "read:runs")
	if w := f.do(http.MethodPost, "/v0/runs/"+runID.String()+"/merge", human, `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("human merge: %d", w.Code)
	}
	if after := len(f.aud.globalOf(auditFirstOfficerActionRefused)); after != before {
		t.Fatalf("a non-seat 403 wrote a first_officer_action_refused row")
	}
}

// The refusal hook is best-effort: an append failure never alters the 403.
func TestFirstOfficerSeat_RefusalAuditRow(t *testing.T) {
	f := newSeatFixture(t)
	runID := f.seedRun(run.StateRunning, "")
	bearer := f.mint(runSeat(runID), "read:runs")
	if w := f.do(http.MethodPost, "/v0/tokens", bearer, `{"scopes":["read:runs"]}`); w.Code != http.StatusForbidden {
		t.Fatalf("status %d", w.Code)
	}
	if n := len(f.aud.globalOf(auditFirstOfficerActionRefused)); n != 1 {
		t.Fatalf("refusal rows = %d, want 1", n)
	}
	f.aud.globalErr = errors.New("audit down")
	w := f.do(http.MethodPost, "/v0/tokens", bearer, `{"scopes":["read:runs"]}`)
	requireCrewRefusal(t, w, http.StatusForbidden, "first_officer_cannot_mint_tokens")
	// Unwired audit repo: no panic, same 403.
	f.s.cfg.AuditRepo = nil
	requireCrewRefusal(t, f.do(http.MethodPost, "/v0/tokens", bearer, `{}`), http.StatusForbidden, "first_officer_cannot_mint_tokens")
}

func TestFirstOfficerSeat_RetiresOnTerminalAnchor(t *testing.T) {
	cases := []struct {
		name  string
		state run.State
	}{
		{"cancelled", run.StateCancelled}, {"succeeded", run.StateSucceeded}, {"failed", run.StateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSeatFixture(t)
			runID := f.seedRun(tc.state, "")
			subj := runSeat(runID)
			bearer := f.mint(subj, "read:runs")
			f.mint(subj, "read:audit") // a second active token for the same seat
			_, w, reached := f.through(bearer)
			if reached {
				t.Fatal("a seat on a terminal run reached the handler")
			}
			requireCrewRefusal(t, w, http.StatusUnauthorized, "first_officer_seat_retired")
			if n := f.activeTokens(subj); n != 0 {
				t.Fatalf("active tokens after retirement = %d, want 0", n)
			}
			rows := f.aud.chainedOf(auditFirstOfficerSeatRetired)
			if len(rows) != 1 || rows[0].RunID != runID {
				t.Fatalf("retired rows = %+v, want one on the run chain", rows)
			}
			var p map[string]any
			_ = json.Unmarshal(rows[0].Payload, &p)
			if p["reason"] != seatRetiredAnchorTerminal || p["anchor_state"] != string(tc.state) ||
				len(p["revoked_token_ids"].([]any)) != 2 {
				t.Fatalf("retired payload = %v", p)
			}
			// The revoked token no longer authenticates: it falls through to
			// the anonymous identity, never the seat.
			if id, _, _ := f.through(bearer); id.Subject == subj {
				t.Fatal("retired token still resolves to the seat")
			}
		})
	}
}

func TestFirstOfficerSeat_RetiresOnTerminalCampaign(t *testing.T) {
	f := newSeatFixture(t)
	acct := uuid.NewString()
	cid := f.seedCampaign(campaign.StateSucceeded, acct)
	subj := campaignSeat(cid)
	_, w, reached := f.through(f.mint(subj, "read:runs"))
	if reached {
		t.Fatal("a seat on a terminal campaign reached the handler")
	}
	requireCrewRefusal(t, w, http.StatusUnauthorized, "first_officer_seat_retired")
	if n := f.activeTokens(subj); n != 0 {
		t.Fatalf("active tokens = %d, want 0", n)
	}
	rows := f.aud.globalOf(auditFirstOfficerSeatRetired)
	if len(rows) != 1 || rows[0].AccountID == nil || rows[0].AccountID.String() != acct {
		t.Fatalf("retired rows = %+v, want one on the campaign account's global chain", rows)
	}
}

func TestFirstOfficerSeat_RetiresOnMissingAnchor(t *testing.T) {
	for _, subj := range []string{runSeat(uuid.New()), campaignSeat(uuid.New())} {
		f := newSeatFixture(t)
		_, w, reached := f.through(f.mint(subj, "read:runs"))
		if reached {
			t.Fatalf("%s: a seat on a missing anchor reached the handler", subj)
		}
		requireCrewRefusal(t, w, http.StatusUnauthorized, "first_officer_seat_retired")
		if n := f.activeTokens(subj); n != 0 {
			t.Fatalf("%s: active tokens = %d, want 0", subj, n)
		}
		rows := f.aud.globalOf(auditFirstOfficerSeatRetired)
		if len(rows) != 1 {
			t.Fatalf("%s: retired global rows = %d, want 1", subj, len(rows))
		}
		var p map[string]any
		_ = json.Unmarshal(rows[0].Payload, &p)
		if p["reason"] != seatRetiredAnchorMissing {
			t.Fatalf("%s: reason = %v", subj, p["reason"])
		}
	}
}

// A revoke failure still refuses the request (the token is never honored past
// a terminal anchor) and writes no retirement row naming tokens it did not
// revoke; a list failure likewise refuses.
func TestFirstOfficerSeat_RetirementStoreFailuresStillRefuse(t *testing.T) {
	for _, mode := range []string{"revoke", "list"} {
		f := newSeatFixture(t)
		runID := f.seedRun(run.StateCancelled, "")
		bearer := f.mint(runSeat(runID), "read:runs")
		if mode == "revoke" {
			f.tokens.revokeErr = errors.New("db down")
		} else {
			f.tokens.listErr = errors.New("db down")
		}
		_, w, reached := f.through(bearer)
		if reached {
			t.Fatalf("%s failure: the request reached the handler", mode)
		}
		requireCrewRefusal(t, w, http.StatusUnauthorized, "first_officer_seat_retired")
		if n := len(f.aud.chainedOf(auditFirstOfficerSeatRetired)); n != 0 {
			t.Fatalf("%s failure: %d retirement rows recorded for unrevoked tokens", mode, n)
		}
	}
}

func TestFirstOfficerSeat_AnchorReadError503(t *testing.T) {
	cases := []struct {
		name string
		subj func(f *seatFixture) string
		fail func(f *seatFixture)
	}{
		{"run read", func(f *seatFixture) string { return runSeat(f.seedRun(run.StateRunning, "")) },
			func(f *seatFixture) { f.runs.getErr = errors.New("db down") }},
		{"campaign read", func(f *seatFixture) string { return campaignSeat(f.seedCampaign(campaign.StateRunning, "")) },
			func(f *seatFixture) { f.camps.getErr = errors.New("db down") }},
		{"campaign account read", func(f *seatFixture) string { return campaignSeat(f.seedCampaign(campaign.StateRunning, "")) },
			func(f *seatFixture) { f.camps.acctErr = errors.New("db down") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSeatFixture(t)
			subj := tc.subj(f)
			bearer := f.mint(subj, "read:runs")
			tc.fail(f)
			_, w, reached := f.through(bearer)
			if reached {
				t.Fatal("an unreadable anchor let the seat through")
			}
			requireCrewRefusal(t, w, http.StatusServiceUnavailable, "first_officer_seat_unverifiable")
			if n := f.activeTokens(subj); n != 1 {
				t.Fatalf("active tokens = %d, want 1 (a read error must revoke nothing)", n)
			}
			if len(f.aud.chained)+len(f.aud.global) != 0 {
				t.Fatal("a read error recorded a retirement")
			}
		})
	}
}

func TestFirstOfficerSeat_InvalidSubject401(t *testing.T) {
	f := newSeatFixture(t)
	for _, subj := range []string{"first_officer/run/not-a-uuid", "first_officer/lease/" + uuid.NewString(), "first_officer/"} {
		bearer := f.mint(subj, "write:approvals", "read:runs")
		_, w, reached := f.through(bearer)
		if reached {
			t.Fatalf("%q reached the handler", subj)
		}
		requireCrewRefusal(t, w, http.StatusUnauthorized, "first_officer_seat_invalid")
	}
}

func TestFirstOfficerSeat_UnwiredRepos503(t *testing.T) {
	cases := []struct {
		name  string
		unset func(c *Config)
		subj  string
	}{
		{"token repo", func(c *Config) { c.APITokenRepo = nil }, runSeat(uuid.New())},
		{"run repo", func(c *Config) { c.RunRepo = nil }, runSeat(uuid.New())},
		{"campaign repo", func(c *Config) { c.CampaignRepo = nil }, campaignSeat(uuid.New())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSeatFixture(t)
			bearer := f.mint(tc.subj, "read:runs")
			tc.unset(&f.s.cfg)
			_, w, reached := f.through(bearer)
			if reached {
				t.Fatal("an unverifiable seat reached the handler")
			}
			requireCrewRefusal(t, w, http.StatusServiceUnavailable, "first_officer_seat_unverifiable")
			if n := f.activeTokens(tc.subj); n != 1 {
				t.Fatalf("active tokens = %d, want 1", n)
			}
		})
	}
}

// Condition 1: the seat acts within its anchor's account. A run seat on a
// tenanted run reads that run (same account) and is refused 403 on a run of
// another account; it files a specialist job charged to its anchor's account.
// A campaign seat on a tenanted campaign carries the campaign's account. An
// untenanted anchor leaves the seat untenanted.
func TestFirstOfficerSeat_TenantedAnchor(t *testing.T) {
	f := newSeatFixture(t)
	acctA, acctB := uuid.NewString(), uuid.NewString()
	runA := f.seedRun(run.StateRunning, acctA)
	runB := f.seedRun(run.StateRunning, acctB)
	bearer := f.mint(runSeat(runA), "read:runs", "write:messages", specialistjob.RequestScope)

	id, _, reached := f.through(bearer)
	if !reached || id.AccountID != acctA {
		t.Fatalf("seat identity account = %q, want the anchor's %q", id.AccountID, acctA)
	}
	if w := f.do(http.MethodGet, "/v0/runs/"+runA.String(), bearer, ""); w.Code != http.StatusOK {
		t.Fatalf("same-account read: %d %s", w.Code, w.Body.String())
	}
	requireCrewRefusal(t, f.do(http.MethodGet, "/v0/runs/"+runB.String(), bearer, ""), http.StatusForbidden, "account_forbidden")
	requireCrewRefusal(t, f.do(http.MethodPost, "/v0/runs/"+runB.String()+"/merge", bearer, `{}`), http.StatusForbidden, "account_forbidden")

	// Same-account send: a specialist-job request is charged to the anchor's account.
	store := &foSpecialistJobStore{}
	f.s.cfg.SpecialistJobs = store
	body := sjRequestBody(t, specialistjob.KindInventory, map[string]any{"run_id": runA.String(), "head_sha": sjSHA1})
	if w := f.do(http.MethodPost, "/v0/specialist-jobs", bearer, body); w.Code != http.StatusCreated {
		t.Fatalf("same-account request: %d %s", w.Code, w.Body.String())
	}
	if store.last.AccountID == nil || store.last.AccountID.String() != acctA {
		t.Fatalf("job charged to account %v, want %s", store.last.AccountID, acctA)
	}

	// Campaign seat on a tenanted campaign.
	cid := f.seedCampaign(campaign.StateRunning, acctB)
	cid2, _, _ := f.through(f.mint(campaignSeat(cid), "read:runs"))
	if cid2.AccountID != acctB {
		t.Fatalf("campaign seat account = %q, want %q", cid2.AccountID, acctB)
	}
	// Untenanted anchor stays untenanted.
	runU := f.seedRun(run.StateRunning, "")
	uid, _, _ := f.through(f.mint(runSeat(runU), "read:runs"))
	if uid.AccountID != "" {
		t.Fatalf("untenanted anchor seat account = %q, want empty", uid.AccountID)
	}
	requireCrewRefusal(t, f.do(http.MethodGet, "/v0/runs/"+runA.String(), f.mint(runSeat(runU), "read:runs"), ""), http.StatusForbidden, "account_forbidden")
}

// foSpecialistJobStore records the last Create call.
type foSpecialistJobStore struct {
	sjFailingStore
	last specialistjob.CreateParams
}

func (s *foSpecialistJobStore) Create(_ context.Context, p specialistjob.CreateParams) (*specialistjob.Job, error) {
	s.last = p
	return &specialistjob.Job{ID: uuid.New(), Kind: p.Request.Kind, State: specialistjob.StateQueued,
		Requester: p.Requester, Subject: p.Request.Subject, Inputs: json.RawMessage(`{}`)}, nil
}

func TestFirstOfficerRunInSeat(t *testing.T) {
	f := newSeatFixture(t)
	ctx := context.Background()
	runA, runB := uuid.New(), uuid.New()
	cid := uuid.New()
	f.linkRun(cid, runA)
	f.linkRun(uuid.New(), runB)

	runAnchor := firstofficer.Anchor{Kind: firstofficer.AnchorRun, ID: runA}
	if in, err := f.s.firstOfficerRunInSeat(ctx, runAnchor, runA); err != nil || !in {
		t.Fatalf("run seat, own run: %v %v", in, err)
	}
	if in, err := f.s.firstOfficerRunInSeat(ctx, runAnchor, runB); err != nil || in {
		t.Fatalf("run seat, other run: %v %v", in, err)
	}
	campAnchor := firstofficer.Anchor{Kind: firstofficer.AnchorCampaign, ID: cid}
	if in, err := f.s.firstOfficerRunInSeat(ctx, campAnchor, runA); err != nil || !in {
		t.Fatalf("campaign seat, linked run: %v %v", in, err)
	}
	if in, err := f.s.firstOfficerRunInSeat(ctx, campAnchor, runB); err != nil || in {
		t.Fatalf("campaign seat, other campaign's run: %v %v", in, err)
	}
	f.camps.listErr = errors.New("db down")
	if _, err := f.s.firstOfficerRunInSeat(ctx, campAnchor, runA); err == nil {
		t.Fatal("list error not surfaced")
	}
	f.s.cfg.CampaignRepo = nil
	if _, err := f.s.firstOfficerRunInSeat(ctx, campAnchor, runA); err == nil {
		t.Fatal("unwired campaign repo not surfaced")
	}
}

func TestFirstOfficerSeatOf(t *testing.T) {
	runID := uuid.New()
	if a, ok := firstOfficerSeatOf(Identity{Subject: runSeat(runID)}); !ok || a.ID != runID || a.Kind != firstofficer.AnchorRun {
		t.Fatalf("seat of run seat = %+v %v", a, ok)
	}
	for _, subj := range []string{"github:42", "first_officer/run/x", ""} {
		if _, ok := firstOfficerSeatOf(Identity{Subject: subj}); ok {
			t.Errorf("firstOfficerSeatOf(%q) ok", subj)
		}
	}
}
