package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationconfirm"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/handoverbrief"
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

// TestCaptainOffer_StampsCanonicalBriefHash (approval condition 1, end to
// end): the committed captain_handover_offered payload carries the brief_hash
// of the canonical, unbounded brief composed for that offer plus its window,
// and a render bounded far below the brief's size — the Bound the MCP tool
// re-applies at its session budget — reports the SAME hash; the REST route's
// bounded render reports the canonical hash of its own composition, never a
// re-hash of its bounded body.
func TestCaptainOffer_StampsCanonicalBriefHash(t *testing.T) {
	f := newBriefPG(t)
	f.seedCaptain(t, "github:alice")
	for i := 0; i < 4; i++ {
		f.seedMerge(t)
	}
	ctx := context.Background()

	// REST render at this chain state vs the canonical hash of the same
	// composition (successor unset: no offer is pending yet).
	restBefore := f.get(t, "")
	canonBefore, err := handoverbrief.Compose(ctx, f.srv.handoverBriefDeps(ctx, briefPGRepo), handoverbrief.Request{Repo: briefPGRepo})
	if err != nil {
		t.Fatal(err)
	}
	if restBefore.BriefHash != canonBefore.BriefHash || restBefore.BriefHash != handoverbrief.Hash(canonBefore) {
		t.Errorf("REST brief_hash = %q, want the canonical hash %q", restBefore.BriefHash, canonBefore.BriefHash)
	}

	// The canonical brief the offer composes: same chain state, same
	// successor. Nothing writes between this compose and the POST.
	canon, err := handoverbrief.Compose(ctx, f.srv.handoverBriefDeps(ctx, briefPGRepo),
		handoverbrief.Request{Repo: briefPGRepo, Subject: "github:alice", Successor: "github:carol"})
	if err != nil {
		t.Fatal(err)
	}
	f.offer(t, "github:alice", "github:carol")

	p := f.offerPayload(t)
	if p["brief_hash"] != canon.BriefHash || canon.BriefHash == "" {
		t.Errorf("committed brief_hash = %v, want the canonical %q", p["brief_hash"], canon.BriefHash)
	}
	if p["brief_from_sequence"] != float64(canon.Window.FromSequence) || p["brief_to_sequence"] != float64(canon.Window.ToSequence) {
		t.Errorf("committed window = %v..%v, want %d..%d", p["brief_from_sequence"], p["brief_to_sequence"], canon.Window.FromSequence, canon.Window.ToSequence)
	}
	if _, ok := p["brief_unavailable"]; ok {
		t.Errorf("a composed brief must not record brief_unavailable: %v", p)
	}

	// Tighten the budget until Bound actually truncates (above its floor),
	// so the hash check runs against a genuinely cut render.
	full, _ := json.Marshal(canon)
	var bounded handoverbrief.Brief
	for budget := len(full) - 1; budget > 0; budget -= 16 {
		b, err := handoverbrief.Bound(canon, budget)
		if err != nil {
			break
		}
		if b.Truncated {
			bounded = b
			break
		}
	}
	if !bounded.Truncated {
		t.Fatalf("no budget above the floor truncated the brief; the check would be vacuous")
	}
	if bounded.BriefHash != p["brief_hash"] {
		t.Errorf("bounded render brief_hash = %q, want the committed %v", bounded.BriefHash, p["brief_hash"])
	}

	// The derived pending offer surfaces the recorded brief.
	w := serveCaptain(t, f.srv, http.MethodGet, "/v0/captain?repo="+briefPGRepo, "", &Identity{Subject: "github:reader"})
	var rec captainResponse
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil || rec.PendingOffer == nil {
		t.Fatalf("GET /v0/captain = %d %s", w.Code, w.Body.String())
	}
	if rec.PendingOffer.BriefHash != canon.BriefHash || rec.PendingOffer.BriefToSequence != canon.Window.ToSequence {
		t.Errorf("pending_offer brief = %q..%d, want %q..%d", rec.PendingOffer.BriefHash, rec.PendingOffer.BriefToSequence, canon.BriefHash, canon.Window.ToSequence)
	}
}

// TestCaptainOffer_FailOpenMarksBriefUnavailable (approval condition 2): a
// reachable TOTAL failure — the brief's window (chain-head) read fails while
// the captain store keeps working — still commits the offer, and the COMMITTED
// payload records brief_unavailable with the reason and NO brief_hash. The
// assertion reads the chain entry back rather than the response.
func TestCaptainOffer_FailOpenMarksBriefUnavailable(t *testing.T) {
	f := newBriefPG(t)
	f.seedCaptain(t, "github:alice")
	f.srv.cfg.DigestStore = digest.NewStore(failingDB{})
	f.offer(t, "github:alice", "github:carol")
	p := f.offerPayload(t)
	if p["brief_unavailable"] != true || p["brief_unavailable_reason"] != handoverbrief.ReasonWindowReadFailed {
		t.Errorf("committed payload = %v, want brief_unavailable with reason %s", p, handoverbrief.ReasonWindowReadFailed)
	}
	if _, ok := p["brief_hash"]; ok {
		t.Errorf("an unavailable brief must carry no brief_hash: %v", p)
	}
	if p["successor"] != "github:carol" {
		t.Errorf("the offer itself must still commit: %v", p)
	}
}

// TestCaptainOffer_UnwiredDigestMarksDependencyUnconfigured: with no digest
// store wired at all the offer still commits, recording
// brief_unavailable/dependency_unconfigured — the captain verbs never depend
// on the brief surface being configured.
func TestCaptainOffer_UnwiredDigestMarksDependencyUnconfigured(t *testing.T) {
	f := newBriefPG(t)
	f.seedCaptain(t, "github:alice")
	f.srv.cfg.DigestStore = nil
	f.offer(t, "github:alice", "github:carol")
	p := f.offerPayload(t)
	if p["brief_unavailable"] != true || p["brief_unavailable_reason"] != handoverbrief.ReasonDependencyUnconfigured {
		t.Errorf("committed payload = %v, want brief_unavailable/%s", p, handoverbrief.ReasonDependencyUnconfigured)
	}
}

// TestCaptainOffer_DigestSectionErrorStillHashes (approval condition 2): a
// SECTION read error (the decision index backing merges / waivers fails) is a
// degradation — the brief composes with those parts marked unavailable, and
// the offer still records a brief_hash, never brief_unavailable.
func TestCaptainOffer_DigestSectionErrorStillHashes(t *testing.T) {
	f := newBriefPG(t)
	f.seedCaptain(t, "github:alice")
	f.seedMerge(t)
	f.srv.cfg.DigestIndex = failingIndex{}
	ctx := context.Background()

	b, err := handoverbrief.Compose(ctx, f.srv.handoverBriefDeps(ctx, briefPGRepo),
		handoverbrief.Request{Repo: briefPGRepo, Subject: "github:alice", Successor: "github:carol"})
	if err != nil {
		t.Fatalf("a section error must not fail the brief: %v", err)
	}
	if m := briefPart(t, b, handoverbrief.SectionWhatChanged, handoverbrief.PartMerges); !m.Unavailable {
		t.Errorf("merges part = %+v, want unavailable", m)
	}
	if !briefHasDegradation(b, handoverbrief.DegradationDigestSectionFailed) {
		t.Errorf("degradations = %+v, want digest_section_failed", b.Degradations)
	}

	f.offer(t, "github:alice", "github:carol")
	p := f.offerPayload(t)
	if p["brief_hash"] != b.BriefHash || b.BriefHash == "" {
		t.Errorf("committed brief_hash = %v, want the degraded brief's %q", p["brief_hash"], b.BriefHash)
	}
	if _, ok := p["brief_unavailable"]; ok {
		t.Errorf("a degraded brief is not unavailable: %v", p)
	}
}

// TestCaptainVerbs_OnlyOfferRecordsABrief: withdraw, accept, relinquish and
// claim never carry brief keys — the brief is composed on the offer path only.
func TestCaptainVerbs_OnlyOfferRecordsABrief(t *testing.T) {
	f := newBriefPG(t)
	f.seedCaptain(t, "github:alice")
	f.offer(t, "github:alice", "github:carol")
	w := serveCaptain(t, f.srv, http.MethodPost, "/v0/captain/accept", `{"repo":"`+briefPGRepo+`"}`, &Identity{Subject: "github:carol"})
	if w.Code != http.StatusOK {
		t.Fatalf("accept = %d:\n%s", w.Code, w.Body.String())
	}
	var raw []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT payload FROM audit_entries
		WHERE run_id IS NULL AND category = 'captain_assigned' ORDER BY sequence DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "brief_") {
		t.Errorf("captain_assigned payload carries brief keys: %s", raw)
	}
}

// --- delegation_unconfirmed on the hand-off surface (E76.5 / #3768) ---

// captainUnconfirmed GETs /v0/captain for acme/app and returns the block,
// failing when it is absent.
func captainUnconfirmed(t *testing.T, f *delegationConfirmPG) *captainDelegationUnconfirmed {
	t.Helper()
	body, raw := f.get(t, "acme/app")
	if body.DelegationUnconfirmed == nil {
		t.Fatalf("GET /v0/captain carries no delegation_unconfirmed block:\n%s", raw)
	}
	return body.DelegationUnconfirmed
}

func sameIDs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// TestCaptainDelegationUnconfirmed_FirstHandoverListsEveryWorkflow (approval
// condition 1): with a captain seated and NO confirmation entries, the
// hand-off surface lists every workflow of the view — never-confirmed ones
// included — and says it reports no hash staleness.
func TestCaptainDelegationUnconfirmed_FirstHandoverListsEveryWorkflow(t *testing.T) {
	f := newDelegationConfirmPG(t)
	f.seedSpecAt(t, confirmSpecA, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	f.seedCaptain(t, "acme/app", confirmCaptain)
	got := captainUnconfirmed(t, f)
	if !sameIDs(got.Workflows, "ship", "guarded") {
		t.Errorf("workflows = %v, want every workflow of the spec (ship, guarded)", got.Workflows)
	}
	if got.Source != delegationSourceRunCache || got.HashStalenessReported || got.SeatSequence == 0 ||
		got.InventoryUnavailable != "" || got.Unavailable != "" {
		t.Errorf("block = %+v, want source run_cache, hash_staleness_reported false, a seat sequence, no unavailability", got)
	}
	// The normal (GET /v0/captain) hand-off path delivers unconfirmed_since —
	// the seat-change timestamp — alongside seat_sequence (fix-up 3).
	if got.UnconfirmedSince == nil || got.UnconfirmedSince.IsZero() {
		t.Errorf("unconfirmed_since = %v, want the seat-change timestamp", got.UnconfirmedSince)
	}
}

// TestCaptainDelegationUnconfirmed_NoCaptainListsNothing: before any captain
// has sat there is no handover to confirm.
func TestCaptainDelegationUnconfirmed_NoCaptainListsNothing(t *testing.T) {
	f := newDelegationConfirmPG(t)
	f.seedSpecAt(t, confirmSpecA, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	got := captainUnconfirmed(t, f)
	if len(got.Workflows) != 0 || got.SeatSequence != 0 || got.UnconfirmedSince != nil {
		t.Errorf("block = %+v, want no workflows, seat_sequence 0 and no unconfirmed_since", got)
	}
}

// TestCaptainDelegationUnconfirmed_ChainOnlyNoStaleness (approval condition
// 2): a confirmed workflow leaves the list, and a later TIGHTENED spec — whose
// hash differs by construction — does NOT put it back on the hand-off surface,
// while the delegation-confirmation read DOES report hash_stale (which proves
// the fixture really is stale).
func TestCaptainDelegationUnconfirmed_ChainOnlyNoStaleness(t *testing.T) {
	f := newDelegationConfirmPG(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	f.seedSpecAt(t, confirmSpecA, base)
	f.seedCaptain(t, "acme/app", confirmCaptain)
	if w := serveDelegationConfirm(t, f.srv, "confirm", confirmBody("ship", hashOfWorkflow(t, confirmSpecA, "ship")), confirmCaptain); w.Code != http.StatusOK {
		t.Fatalf("confirm = %d: %s", w.Code, w.Body.String())
	}
	if got := captainUnconfirmed(t, f); !sameIDs(got.Workflows, "guarded") {
		t.Fatalf("after confirm, workflows = %v, want [guarded]", got.Workflows)
	}

	f.seedSpecAt(t, confirmSpecTightened, base.Add(time.Hour))
	if st := statusFor(t, readConfirmation(t, f.srv).Workflows, "ship"); st.Reason != delegationconfirm.ReasonHashStale {
		t.Fatalf("confirmation read = %+v, want hash_stale (the fixture is not stale)", st)
	}
	got := captainUnconfirmed(t, f)
	if !sameIDs(got.Workflows, "guarded") || got.HashStalenessReported {
		t.Errorf("after tightening, captain block = %+v, want [guarded] with no staleness reported", got)
	}
}

// TestCaptainDelegationUnconfirmed_AcceptResponseListsEveryWorkflow: the
// outgoing captain confirmed `ship`; the successor's ACCEPT response already
// lists it again, because confirmation state resets at every seat change.
func TestCaptainDelegationUnconfirmed_AcceptResponseListsEveryWorkflow(t *testing.T) {
	f := newDelegationConfirmPG(t)
	f.seedSpecAt(t, confirmSpecA, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	f.seedCaptain(t, "acme/app", confirmCaptain)
	if w := serveDelegationConfirm(t, f.srv, "confirm", confirmBody("ship", hashOfWorkflow(t, confirmSpecA, "ship")), confirmCaptain); w.Code != http.StatusOK {
		t.Fatalf("confirm = %d: %s", w.Code, w.Body.String())
	}
	f.seedOffer(t, "acme/app", confirmCaptain, "github:successor")
	code, body := f.verb(t, "accept", "github:successor", captainBody("acme/app"))
	if code != http.StatusOK {
		t.Fatalf("accept = %d: %s", code, body)
	}
	var resp captainVerbResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.DelegationUnconfirmed == nil || !sameIDs(resp.DelegationUnconfirmed.Workflows, "ship", "guarded") {
		t.Errorf("accept delegation_unconfirmed = %+v, want both workflows", resp.DelegationUnconfirmed)
	}
	if resp.DelegationUnconfirmed != nil && resp.DelegationUnconfirmed.SeatSequence != resp.Event.Sequence {
		t.Errorf("seat_sequence = %d, want the accept's own sequence %d", resp.DelegationUnconfirmed.SeatSequence, resp.Event.Sequence)
	}
	// unconfirmed_since is the timestamp of the handover that opened this seat —
	// the accept's OWN entry (fix-up 3). Counterfactual: dropping the builder's
	// UnconfirmedSince assignment leaves it nil and this fails.
	if resp.DelegationUnconfirmed == nil || resp.DelegationUnconfirmed.UnconfirmedSince == nil ||
		!resp.DelegationUnconfirmed.UnconfirmedSince.Equal(resp.Event.At) {
		t.Errorf("unconfirmed_since = %v, want the accept event's timestamp %v", resp.DelegationUnconfirmed.UnconfirmedSince, resp.Event.At)
	}
}

// TestCaptainDelegationUnconfirmed_OutgoingCaptainConfirmIgnored (approval
// condition 3): an outgoing captain's confirmation that lands AFTER a newer
// captain_assigned — seeded by construction, bypassing the handler's lock —
// leaves the workflow on the hand-off surface's unconfirmed list.
func TestCaptainDelegationUnconfirmed_OutgoingCaptainConfirmIgnored(t *testing.T) {
	f := newDelegationConfirmPG(t)
	f.seedSpecAt(t, confirmSpecA, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	f.seedCaptain(t, "acme/app", confirmCaptain)
	f.seedCaptain(t, "acme/app", "github:successor")
	for _, wf := range []string{"ship", "guarded"} {
		f.seedEntry(t, delegationconfirm.CategoryDelegationConfirmed, map[string]any{
			"repo": "acme/app", "workflow": wf, "subject": confirmCaptain, "content_hash": hashOfWorkflow(t, confirmSpecA, wf)})
	}
	if got := captainUnconfirmed(t, f); !sameIDs(got.Workflows, "ship", "guarded") {
		t.Errorf("workflows = %v, want both still unconfirmed (the outgoing captain's confirmations must not count)", got.Workflows)
	}
}

// TestCaptainDelegationUnconfirmed_InventoryUnavailable: every branch that
// cannot read the workflow SET names its reason and lists nothing, so an
// empty list is never read as "all confirmed".
func TestCaptainDelegationUnconfirmed_InventoryUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name string
		repo run.Repository
		want string
	}{
		{"no run repository", nil, captainDelegationNoRunRepository},
		{"list runs failed", &captainRunRepo{err: errors.New("db down")}, captainDelegationListRunsFailed},
		{"no run", &captainRunRepo{}, captainDelegationNoCachedSpec},
		{"no cached spec", &captainRunRepo{rows: []*run.Run{{Repo: "acme/app"}}}, captainDelegationNoCachedSpec},
		{"spec unparseable", &captainRunRepo{rows: []*run.Run{{Repo: "acme/app", WorkflowSpec: []byte("version: [")}}}, captainDelegationSpecUnparseable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDelegationConfirmPG(t)
			f.seedCaptain(t, "acme/app", confirmCaptain)
			f.srv.cfg.RunRepo = tc.repo
			got := captainUnconfirmed(t, f)
			if got.InventoryUnavailable != tc.want || len(got.Workflows) != 0 || got.Workflows == nil {
				t.Errorf("block = %+v, want inventory_unavailable %q and an empty (non-null) list", got, tc.want)
			}
		})
	}
}

// TestCaptainDelegationUnconfirmed_ChainReadFailure: a failed confirmation
// read degrades to a fixed reason in the block, never a failed GET and never
// the raw error text.
func TestCaptainDelegationUnconfirmed_ChainReadFailure(t *testing.T) {
	f := newDelegationConfirmPG(t)
	f.seedSpecAt(t, confirmSpecA, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	f.seedCaptain(t, "acme/app", confirmCaptain)
	f.srv.cfg.DelegationConfirmStore = &memConfirmStore{readErr: errors.New("secret-dsn unreachable")}
	got := captainUnconfirmed(t, f)
	if got.Unavailable != captainDelegationChainReadFailed || len(got.Workflows) != 0 {
		t.Errorf("block = %+v, want unavailable %q and no workflows", got, captainDelegationChainReadFailed)
	}
	_, raw := f.get(t, "acme/app")
	if strings.Contains(raw, "secret-dsn") {
		t.Errorf("the raw chain error leaked into the 200 body:\n%s", raw)
	}
}

// TestCaptainDelegationUnconfirmed_NilStoreOmitsBlock: without the store the
// key is ABSENT on GET and on a verb — never an empty list.
func TestCaptainDelegationUnconfirmed_NilStoreOmitsBlock(t *testing.T) {
	f := newCaptainPG(t, nil)
	f.seedCaptain(t, "acme/app", "github:alice")
	if _, raw := f.get(t, "acme/app"); strings.Contains(raw, "delegation_unconfirmed") {
		t.Errorf("GET carries delegation_unconfirmed with no store wired:\n%s", raw)
	}
	code, body := f.verb(t, "relinquish", "github:alice", captainBody("acme/app"))
	if code != http.StatusOK || strings.Contains(body, "delegation_unconfirmed") {
		t.Errorf("relinquish = %d %s, want 200 without delegation_unconfirmed", code, body)
	}
}
