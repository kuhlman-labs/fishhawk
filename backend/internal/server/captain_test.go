package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

const captainUnitRepo = "acme/captain-unit"

// captainRunRepo serves ListRuns from a fixed newest-first row set and
// records the filter it was called with.
type captainRunRepo struct {
	run.BaseFake
	rows    []*run.Run
	err     error
	filters []run.ListRunsFilter
}

func (r *captainRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.filters = append(r.filters, f)
	if r.err != nil {
		return nil, r.err
	}
	rows := r.rows
	if f.Limit > 0 && len(rows) > f.Limit {
		rows = rows[:f.Limit]
	}
	return rows, nil
}

// captainApprovalSpec renders a version 1.0 spec with one approval gate
// per (minPermission, memberOf) pair; an empty field is omitted.
func captainApprovalSpec(gates ...[2]string) []byte {
	var b strings.Builder
	b.WriteString(`version: "1.0"
workflows:
  feature_change:
    stages:
`)
	for i, g := range gates {
		fmt.Fprintf(&b, `      - id: gate%d
        type: acceptance
        executor:
          human: true
        gates:
          - type: approval
            approvals:
              count: 1
              not: [author, agent]
`, i)
		if g[0] != "" {
			fmt.Fprintf(&b, "              min_permission: %s\n", g[0])
		}
		if g[1] != "" {
			fmt.Fprintf(&b, "              member_of: %s\n", g[1])
		}
	}
	return []byte(b.String())
}

func captainRunRow(specBytes []byte) *run.Run {
	return &run.Run{Repo: captainUnitRepo, WorkflowID: "feature_change", WorkflowSpec: specBytes}
}

// TestClassifyRepoPredicate_Trichotomy pins one case per outcome AND per
// undeterminable basis: an unreadable predicate is NEVER trivial.
func TestClassifyRepoPredicate_Trichotomy(t *testing.T) {
	cases := []struct {
		name      string
		runs      run.Repository
		idp       *fakeIdentityProvider
		want      captain.PredicateOutcome
		wantBasis string
	}{
		{"no run repository", nil, nil, captain.PredicateUndeterminable, "undeterminable:no_run_repository"},
		{"list runs failed", &captainRunRepo{err: errors.New("db down")}, nil, captain.PredicateUndeterminable, "undeterminable:list_runs_failed"},
		{"no run", &captainRunRepo{}, nil, captain.PredicateUndeterminable, "undeterminable:no_run"},
		{"no cached spec", &captainRunRepo{rows: []*run.Run{captainRunRow(nil)}}, nil, captain.PredicateUndeterminable, "undeterminable:no_cached_spec"},
		{"malformed spec", &captainRunRepo{rows: []*run.Run{captainRunRow([]byte("version: [not yaml"))}}, nil, captain.PredicateUndeterminable, "undeterminable:spec_unparseable"},
		{"legacy approvers gate", &captainRunRepo{rows: []*run.Run{captainRunRow([]byte(dashSpec))}}, nil, captain.PredicateUndeterminable, "undeterminable:legacy_approvers_gate"},
		{"trivial", &captainRunRepo{rows: []*run.Run{captainRunRow(captainApprovalSpec([2]string{}))}}, nil, captain.PredicateTrivial, captainTrivialBasis},
		{"non-trivial satisfied", &captainRunRepo{rows: []*run.Run{captainRunRow(captainApprovalSpec([2]string{"admin", ""}))}},
			&fakeIdentityProvider{perm: identity.PermissionAdmin}, captain.PredicateNonTrivialSatisfied, "min_permission:admin"},
		{"non-trivial rejected", &captainRunRepo{rows: []*run.Run{captainRunRow(captainApprovalSpec([2]string{"admin", ""}))}},
			&fakeIdentityProvider{perm: identity.PermissionRead}, captain.PredicateNonTrivialRejected, "min_permission:admin"},
		{"identity unavailable", &captainRunRepo{rows: []*run.Run{captainRunRow(captainApprovalSpec([2]string{"admin", ""}))}},
			&fakeIdentityProvider{permErr: identity.ErrRateLimited}, captain.PredicateUndeterminable, "undeterminable:identity_unavailable"},
		{"identity unconfigured", &captainRunRepo{rows: []*run.Run{captainRunRow(captainApprovalSpec([2]string{"admin", ""}))}},
			nil, captain.PredicateUndeterminable, "undeterminable:identity_unconfigured"},
		{"member_of satisfied", &captainRunRepo{rows: []*run.Run{captainRunRow(captainApprovalSpec([2]string{"", "acme/leads"}))}},
			&fakeIdentityProvider{member: true}, captain.PredicateNonTrivialSatisfied, "member_of:acme/leads"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{RunRepo: tc.runs}
			if tc.idp != nil {
				cfg.IdentityProvider = tc.idp
			}
			s := New(cfg)
			got, basis := s.classifyRepoPredicate(context.Background(), captainUnitRepo, "github:claimant")
			if got != tc.want || basis != tc.wantBasis {
				t.Errorf("classifyRepoPredicate = (%d, %q), want (%d, %q)", got, basis, tc.want, tc.wantBasis)
			}
		})
	}
}

// TestClassifyRepoPredicate_UsesNewestRunSpec: the predicate is read from
// the repo's NEWEST run only (Limit 1, repo-filtered) — an older run's
// stricter spec does not apply.
func TestClassifyRepoPredicate_UsesNewestRunSpec(t *testing.T) {
	rr := &captainRunRepo{rows: []*run.Run{
		captainRunRow(captainApprovalSpec([2]string{})),
		captainRunRow(captainApprovalSpec([2]string{"admin", ""})),
	}}
	s := New(Config{RunRepo: rr})
	got, _ := s.classifyRepoPredicate(context.Background(), captainUnitRepo, "github:claimant")
	if got != captain.PredicateTrivial {
		t.Errorf("outcome = %d, want trivial from the newest run's spec", got)
	}
	if len(rr.filters) != 1 || rr.filters[0].Limit != 1 || rr.filters[0].Repo != captainUnitRepo {
		t.Errorf("ListRuns filters = %+v, want one Limit-1 call for %s", rr.filters, captainUnitRepo)
	}
}

// TestFoldRepoApprovals_StrictestPerDimension: every approval gate of every
// workflow folds to max count, strictest min_permission and the union of
// member_of — and the parsed spec is not mutated.
func TestFoldRepoApprovals_StrictestPerDimension(t *testing.T) {
	parsed, err := spec.ParseBytes(captainApprovalSpec(
		[2]string{"write", "acme/a"}, [2]string{"admin", ""}, [2]string{"", "acme/b"}))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	folded, legacy := foldRepoApprovals(parsed)
	if legacy {
		t.Fatal("legacy = true on a v1 approvals-only spec")
	}
	if folded.minPermission != "admin" {
		t.Errorf("minPermission = %q, want admin (strictest)", folded.minPermission)
	}
	if strings.Join(folded.memberOf, ",") != "acme/a,acme/b" {
		t.Errorf("memberOf = %v, want the union [acme/a acme/b]", folded.memberOf)
	}
	if got := parsed.Workflows["feature_change"].Stages[0].Gates[0].Approvals.MinPermission; got != "write" {
		t.Errorf("parsed spec mutated: gate0 min_permission = %q, want write", got)
	}
	if b := captainPredicateBasis(folded); b != "min_permission:admin;member_of:acme/a,acme/b" {
		t.Errorf("basis = %q", b)
	}
}

// TestCaptainErrorStatus_EachRefusalDistinct: every typed refusal maps to its
// OWN code (no refusal mode collapses into another), wrapping is honoured,
// and an unknown error is a 500.
func TestCaptainErrorStatus_EachRefusalDistinct(t *testing.T) {
	want := map[error]captainRefusal{
		captain.ErrActorRequired:           {http.StatusUnauthorized, "authentication_required"},
		captain.ErrAgentIdentity:           {http.StatusForbidden, "captain_agent_identity_refused"},
		captain.ErrNotCaptain:              {http.StatusForbidden, "captain_not_captain"},
		captain.ErrNotOfferer:              {http.StatusForbidden, "captain_not_offerer"},
		captain.ErrOfferSuccessorMismatch:  {http.StatusForbidden, "captain_offer_successor_mismatch"},
		captain.ErrPredicateRejected:       {http.StatusForbidden, "captain_predicate_rejected"},
		captain.ErrNoCaptain:               {http.StatusConflict, "captain_no_captain"},
		captain.ErrNoOffer:                 {http.StatusConflict, "captain_no_offer"},
		captain.ErrCaptainExists:           {http.StatusConflict, "captain_exists"},
		captain.ErrSelfHandover:            {http.StatusConflict, "captain_self_handover"},
		captain.ErrSuccessorRequired:       {http.StatusBadRequest, "captain_successor_required"},
		captain.ErrRepoRequired:            {http.StatusBadRequest, "validation_failed"},
		captain.ErrPredicateUndeterminable: {http.StatusUnprocessableEntity, "captain_predicate_undeterminable"},
	}
	seen := map[string]error{}
	for err, w := range want {
		got, ok := captainErrorStatus(fmt.Errorf("wrapped: %w", err))
		if !ok || got != w {
			t.Errorf("%v -> %+v (ok=%v), want %+v", err, got, ok, w)
		}
		if prev, dup := seen[got.code]; dup {
			t.Errorf("%v and %v share code %q", prev, err, got.code)
		}
		seen[got.code] = err
	}
	if len(captainRefusals) != len(want) {
		t.Errorf("captainRefusals has %d entries, test pins %d", len(captainRefusals), len(want))
	}
	if got, ok := captainErrorStatus(errors.New("boom")); ok || got.status != http.StatusInternalServerError {
		t.Errorf("unknown error -> %+v ok=%v, want 500 not-ok", got, ok)
	}
}

// serveCaptain drives one request through the registered mux with id in
// context.
func serveCaptain(t *testing.T, s *Server, method, target, body string, id *Identity) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if id != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, *id))
	}
	w := httptest.NewRecorder()
	switch {
	case method == http.MethodGet:
		s.handleGetCaptain(w, req)
	case strings.HasSuffix(target, "/offer"):
		s.handleCaptainOffer(w, req)
	case strings.HasSuffix(target, "/withdraw"):
		s.handleCaptainWithdraw(w, req)
	case strings.HasSuffix(target, "/accept"):
		s.handleCaptainAccept(w, req)
	case strings.HasSuffix(target, "/relinquish"):
		s.handleCaptainRelinquish(w, req)
	case strings.HasSuffix(target, "/claim"):
		s.handleCaptainClaim(w, req)
	default:
		t.Fatalf("no captain route for %s %s", method, target)
	}
	return w
}

func captainErrCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, w.Body.String())
	}
	return env.Error.Code
}

var captainVerbPaths = []string{"offer", "withdraw", "accept", "relinquish", "claim"}

// TestCaptain_UnconfiguredReturns501: a nil CaptainStore degrades all six
// routes to the named 501, never a nil dereference.
func TestCaptain_UnconfiguredReturns501(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	id := &Identity{Subject: "github:alice"}
	w := serveCaptain(t, s, http.MethodGet, "/v0/captain?repo="+captainUnitRepo, "", id)
	if w.Code != http.StatusNotImplemented || captainErrCode(t, w) != "captain_unconfigured" {
		t.Errorf("GET = %d %s, want 501 captain_unconfigured", w.Code, w.Body.String())
	}
	for _, v := range captainVerbPaths {
		w := serveCaptain(t, s, http.MethodPost, "/v0/captain/"+v, `{"repo":"`+captainUnitRepo+`"}`, id)
		if w.Code != http.StatusNotImplemented || captainErrCode(t, w) != "captain_unconfigured" {
			t.Errorf("POST %s = %d %s, want 501 captain_unconfigured", v, w.Code, w.Body.String())
		}
	}
}

// TestCaptain_RequestValidation pins each pre-Apply 4xx: anonymous (401),
// missing scope (403), bad JSON / unknown field / missing repo /
// successor-on-a-non-offer verb (400). The store's pool is nil, so reaching
// Apply would panic — every case must refuse before it.
func TestCaptain_RequestValidation(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", CaptainStore: captain.NewStore(nil)})
	human := &Identity{Subject: "github:alice"}
	readOnly := &Identity{Subject: "github:alice", TokenID: "t", Scopes: []string{scopeCaptainRead}}
	cases := []struct {
		name, method, target, body string
		id                         *Identity
		wantStatus                 int
		wantCode                   string
	}{
		{"anonymous verb", http.MethodPost, "/v0/captain/claim", `{"repo":"a/b"}`, nil, 401, "authentication_required"},
		{"anonymous read", http.MethodGet, "/v0/captain?repo=a/b", "", nil, 401, "authentication_required"},
		{"verb without write:approvals", http.MethodPost, "/v0/captain/claim", `{"repo":"a/b"}`, readOnly, 403, "insufficient_scope"},
		{"read missing repo", http.MethodGet, "/v0/captain", "", human, 400, "validation_failed"},
		{"bad json", http.MethodPost, "/v0/captain/offer", `{`, human, 400, "validation_failed"},
		{"unknown field", http.MethodPost, "/v0/captain/offer", `{"repo":"a/b","bogus":1}`, human, 400, "validation_failed"},
		{"missing repo", http.MethodPost, "/v0/captain/claim", `{}`, human, 400, "validation_failed"},
		{"successor on claim", http.MethodPost, "/v0/captain/claim", `{"repo":"a/b","successor":"github:bob"}`, human, 400, "validation_failed"},
		{"successor on accept", http.MethodPost, "/v0/captain/accept", `{"repo":"a/b","successor":"github:bob"}`, human, 400, "validation_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveCaptain(t, s, tc.method, tc.target, tc.body, tc.id)
			if w.Code != tc.wantStatus || captainErrCode(t, w) != tc.wantCode {
				t.Errorf("= %d %s, want %d %s", w.Code, w.Body.String(), tc.wantStatus, tc.wantCode)
			}
		})
	}
}

// TestCaptainActorIsAgent: the operator-agent token family and run-bound
// MCP subjects classify as agents; humans and static subjects do not.
func TestCaptainActorIsAgent(t *testing.T) {
	for subject, want := range map[string]bool{
		"operator-agent/campaign": true,
		"mcp:run:1234":            true,
		"github:alice":            false,
		"brett@local-mcp":         false,
	} {
		if got := captainActorIsAgent(subject); got != want {
			t.Errorf("captainActorIsAgent(%q) = %v, want %v", subject, got, want)
		}
	}
}

// TestCaptainRoutes_Registered: all six routes are on the mux (an anonymous
// call reaches the handler's auth gate — 401 — rather than 404/405).
func TestCaptainRoutes_Registered(t *testing.T) {
	h := New(Config{Addr: "127.0.0.1:0"}).Handler()
	targets := []struct{ method, path string }{{http.MethodGet, "/v0/captain?repo=o/r"}}
	for _, v := range captainVerbPaths {
		targets = append(targets, struct{ method, path string }{http.MethodPost, "/v0/captain/" + v})
	}
	for _, c := range targets {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{"repo":"o/r"}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (route registered, auth gate reached)", c.method, c.path, w.Code)
		}
	}
}
