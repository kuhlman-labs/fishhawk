package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// merge_readiness_test.go pins the two #4086 merge-readiness helpers at the
// unit level: acceptanceStaleRefusal (audit-chain only, fail-CLOSED) and
// approvalDismissedCheck (forge-backed, fail-OPEN). The handler wiring is
// pinned in merge_run_test.go.

// seedReadinessEntry seeds one audit entry at seq, stage-scoped when stageID is
// non-nil, carrying payload.
func seedReadinessEntry(au *auditFake, runID uuid.UUID, stageID *uuid.UUID, category string, seq int64, payload map[string]any) {
	rid := runID
	raw, _ := json.Marshal(payload)
	e := &audit.Entry{RunID: &rid, Category: category, Sequence: seq, Payload: raw}
	if stageID != nil {
		sid := *stageID
		e.StageID = &sid
	}
	au.mu.Lock()
	au.seeded = append(au.seeded, e)
	au.mu.Unlock()
}

// TestAcceptanceStaleRefusal drives the staleness table. Counterfactuals: a
// body that always returns nil reddens every refusing row; flipping the
// outcome-vs-reopen comparison makes "outcome newer than reopen" refuse and
// the stale rows admit.
func TestAcceptanceStaleRefusal(t *testing.T) {
	const h1, h2 = "aaaa000000000000000000000000000000000001", "bbbb000000000000000000000000000000000002"
	type tc struct {
		name      string
		accState  run.StageState
		noAccRow  bool
		seed      func(au *auditFake, runID, accID uuid.UUID)
		wantStale bool
		wantNext  string
		wantVerif string
		wantCur   string
	}
	staleSeed := func(au *auditFake, runID, accID uuid.UUID) {
		seedReadinessEntry(au, runID, &accID, CategoryAcceptanceOutcomeRecorded, 5, map[string]any{"verdict": "passed", "head_sha": h1})
		seedReadinessEntry(au, runID, &accID, CategoryAcceptanceReopened, 8, map[string]any{"head_sha": h2})
	}
	cases := []tc{
		{name: "no acceptance stage row", noAccRow: true, seed: staleSeed},
		{name: "outcome newer than reopen (re-run shipped)", accState: run.StageStateSucceeded,
			seed: func(au *auditFake, runID, accID uuid.UUID) {
				staleSeed(au, runID, accID)
				seedReadinessEntry(au, runID, &accID, CategoryAcceptanceOutcomeRecorded, 12, map[string]any{"verdict": "passed", "head_sha": h2})
			}},
		{name: "reopen scoped to another stage", accState: run.StageStatePending,
			seed: func(au *auditFake, runID, accID uuid.UUID) {
				other := uuid.New()
				seedReadinessEntry(au, runID, &accID, CategoryAcceptanceOutcomeRecorded, 5, map[string]any{"verdict": "passed", "head_sha": h1})
				seedReadinessEntry(au, runID, &other, CategoryAcceptanceReopened, 8, map[string]any{"head_sha": h2})
			}},
		{name: "no reopen at all", accState: run.StageStateSucceeded,
			seed: func(au *auditFake, runID, accID uuid.UUID) {
				seedReadinessEntry(au, runID, &accID, CategoryAcceptanceOutcomeRecorded, 5, map[string]any{"verdict": "passed", "head_sha": h1})
			}},
		{name: "no outcome recorded (gate pending owns it)", accState: run.StageStatePending,
			seed: func(au *auditFake, runID, accID uuid.UUID) {
				seedReadinessEntry(au, runID, &accID, CategoryAcceptanceReopened, 8, map[string]any{"head_sha": h2})
			}},
		{name: "pending stage refuses with dispatch", accState: run.StageStatePending, seed: staleSeed,
			wantStale: true, wantNext: "fishhawk_dispatch_stage", wantVerif: h1, wantCur: h2},
		{name: "dispatched stage refuses with await", accState: run.StageStateDispatched, seed: staleSeed,
			wantStale: true, wantNext: "fishhawk_await_stage", wantVerif: h1, wantCur: h2},
		{name: "running stage refuses with await", accState: run.StageStateRunning, seed: staleSeed,
			wantStale: true, wantNext: "fishhawk_await_stage", wantVerif: h1, wantCur: h2},
		{name: "failed re-run refuses with retry", accState: run.StageStateFailed, seed: staleSeed,
			wantStale: true, wantNext: "fishhawk_retry_stage", wantVerif: h1, wantCur: h2},
		{name: "outcome without head_sha still refuses", accState: run.StageStatePending,
			seed: func(au *auditFake, runID, accID uuid.UUID) {
				seedReadinessEntry(au, runID, &accID, CategoryAcceptanceOutcomeRecorded, 5, map[string]any{"verdict": "passed"})
				seedReadinessEntry(au, runID, &accID, CategoryAcceptanceReopened, 8, map[string]any{"head_sha": h2})
			},
			wantStale: true, wantNext: "fishhawk_dispatch_stage", wantVerif: "", wantCur: h2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			au := newAuditFake()
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
			runID := uuid.New()
			stages := acceptanceMergeStages(runID, c.accState)
			accID := stages[2].ID
			if c.noAccRow {
				stages = stages[:2]
			}
			c.seed(au, runID, accID)
			ref, err := s.acceptanceStaleRefusal(context.Background(), &run.Run{ID: runID}, stages)
			if err != nil {
				t.Fatalf("acceptanceStaleRefusal: %v", err)
			}
			if !c.wantStale {
				if ref != nil {
					t.Fatalf("refusal = %+v, want nil (admit)", ref)
				}
				return
			}
			if ref == nil {
				t.Fatal("refusal = nil, want acceptance_stale")
			}
			if ref.Code != mergeCodeAcceptanceStale {
				t.Errorf("code = %q, want %q", ref.Code, mergeCodeAcceptanceStale)
			}
			d := ref.Details
			if d["next_step"] != c.wantNext {
				t.Errorf("next_step = %v, want %s", d["next_step"], c.wantNext)
			}
			if d["verified_head_sha"] != c.wantVerif || d["current_head_sha"] != c.wantCur {
				t.Errorf("heads = verified %v / current %v, want %q / %q", d["verified_head_sha"], d["current_head_sha"], c.wantVerif, c.wantCur)
			}
			if d["acceptance_stage_id"] != accID.String() || d["acceptance_stage_state"] != string(c.accState) {
				t.Errorf("stage details = %v / %v, want %s / %s", d["acceptance_stage_id"], d["acceptance_stage_state"], accID, c.accState)
			}
			if d["outcome_sequence"] != int64(5) || d["reopened_sequence"] != int64(8) {
				t.Errorf("sequences = %v / %v, want 5 / 8", d["outcome_sequence"], d["reopened_sequence"])
			}
			if c.wantCur != "" && !strings.Contains(ref.Message, c.wantCur) {
				t.Errorf("message %q does not name the current head", ref.Message)
			}
			if c.wantVerif != "" && !strings.Contains(ref.Message, c.wantVerif) {
				t.Errorf("message %q does not name the verified head", ref.Message)
			}
		})
	}
}

// TestAcceptanceStaleRefusal_ReadErrorsFailClosed pins that both audit reads
// propagate their error (never resolve to admit), and that an unwired audit
// repo admits.
func TestAcceptanceStaleRefusal_ReadErrorsFailClosed(t *testing.T) {
	for _, cat := range []string{CategoryAcceptanceReopened, CategoryAcceptanceOutcomeRecorded} {
		t.Run(cat, func(t *testing.T) {
			au := newAuditFake()
			s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
			runID := uuid.New()
			stages := acceptanceMergeStages(runID, run.StageStatePending)
			accID := stages[2].ID
			seedReadinessEntry(au, runID, &accID, CategoryAcceptanceReopened, 8, map[string]any{"head_sha": "h2"})
			au.listByCategoryErrCategory = cat
			ref, err := s.acceptanceStaleRefusal(context.Background(), &run.Run{ID: runID}, stages)
			if err == nil {
				t.Fatalf("err = nil (refusal %+v), want the %s read error propagated", ref, cat)
			}
			if ref != nil {
				t.Errorf("refusal = %+v alongside an error, want nil", ref)
			}
		})
	}
	t.Run("unwired audit repo admits", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		runID := uuid.New()
		ref, err := s.acceptanceStaleRefusal(context.Background(), &run.Run{ID: runID}, acceptanceMergeStages(runID, run.StageStatePending))
		if err != nil || ref != nil {
			t.Fatalf("(%+v, %v), want (nil, nil)", ref, err)
		}
	})
}

// --- approvalDismissedCheck ---------------------------------------------------

// approvalGitHub serves the two forge reads approvalDismissedCheck makes:
// GET /pulls/{n} (merged, mergeable_state, head) and GET /pulls/{n}/reviews
// (a JSON array, a non-2xx, or an empty page advertising a next page — a
// truncated listing).
type approvalGitHub struct {
	mu             sync.Mutex
	prStatus       int // 0 => 200
	merged         bool
	mergeableState string
	headSHA        string
	reviews        []map[string]any
	reviewsStatus  int // 0 => 200
	truncated      bool
	reviewsCalls   int
}

func newApprovalGitHubClient(t *testing.T, stub *approvalGitHub) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if stub.prStatus != 0 && stub.prStatus != http.StatusOK {
			w.WriteHeader(stub.prStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		state := "open"
		if stub.merged {
			state = "closed"
		}
		fmt.Fprintf(w, `{"node_id":"PR_x","state":%q,"merged":%v,"mergeable":true,"mergeable_state":%q,"head":{"sha":%q,"ref":"fishhawk/run-x"},"base":{"ref":"main"}}`,
			state, stub.merged, stub.mergeableState, stub.headSHA)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		stub.reviewsCalls++
		if stub.reviewsStatus != 0 && stub.reviewsStatus != http.StatusOK {
			w.WriteHeader(stub.reviewsStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if stub.truncated {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, srvURL, r.URL.Path))
			_, _ = w.Write([]byte(`[]`))
			return
		}
		raw, _ := json.Marshal(stub.reviews)
		_, _ = w.Write(raw)
	})
	srv := httptest.NewServer(mux)
	srvURL = srv.URL
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// review renders one reviews-listing element.
func review(id int64, login, state, commit string) map[string]any {
	return map[string]any{"id": id, "user": map[string]any{"login": login}, "state": state, "commit_id": commit}
}

const (
	adHead     = "cccc000000000000000000000000000000000003"
	adApproved = "dddd000000000000000000000000000000000004"
)

// approvalFixture is a server + merge-ready run wired to an approvalGitHub.
type approvalFixture struct {
	s      *Server
	au     *auditFake
	runRow *run.Run
	gh     *approvalGitHub
}

func newApprovalFixture(t *testing.T, gh *approvalGitHub) *approvalFixture {
	t.Helper()
	au := newAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
	if gh != nil {
		s.cfg.GitHub = newApprovalGitHubClient(t, gh)
	}
	pr := mergePR
	runRow := &run.Run{ID: uuid.New(), Repo: "x/y", InstallationID: instID(42), PullRequestURL: &pr, State: run.StateRunning}
	return &approvalFixture{s: s, au: au, runRow: runRow, gh: gh}
}

func blockedDismissed() *approvalGitHub {
	return &approvalGitHub{mergeableState: "blocked", headSHA: adHead,
		reviews: []map[string]any{review(1, "alice", "DISMISSED", adApproved)}}
}

// TestApprovalDismissedCheck_FailOpen is the fail-open + fold table. Every
// admitting row asserts its Determined/Undetermined split. Counterfactuals:
// deleting the mergeable_state=="blocked" precondition reddens the "clean"
// and "unknown" rows (they would refuse); replacing the per-reviewer latest
// fold with "any DISMISSED refuses" reddens the "re-approved after dismissal"
// and "dismissed then changes requested" rows.
func TestApprovalDismissedCheck_FailOpen(t *testing.T) {
	type tc struct {
		name         string
		gh           *approvalGitHub // nil => no GitHub client wired
		mutate       func(r *run.Run)
		determined   bool
		undetermined string // substring; "" when determined
		refuse       bool
	}
	cases := []tc{
		{name: "nil GitHub", gh: nil, undetermined: "no GitHub client"},
		{name: "no installation", gh: blockedDismissed(), mutate: func(r *run.Run) { r.InstallationID = nil }, undetermined: "no GitHub installation"},
		{name: "unparseable repo", gh: blockedDismissed(), mutate: func(r *run.Run) { r.Repo = "not-a-repo" }, undetermined: "unparseable"},
		{name: "no PR number", gh: blockedDismissed(), mutate: func(r *run.Run) { r.PullRequestURL = nil }, undetermined: "pull request number"},
		{name: "PR read 500", gh: func() *approvalGitHub { g := blockedDismissed(); g.prStatus = http.StatusInternalServerError; return g }(), undetermined: "pull request read failed"},
		{name: "merged PR", gh: func() *approvalGitHub { g := blockedDismissed(); g.merged = true; return g }(), undetermined: "already merged"},
		{name: "mergeable_state clean with a DISMISSED review", gh: func() *approvalGitHub { g := blockedDismissed(); g.mergeableState = "clean"; return g }(), undetermined: `"clean"`},
		{name: "mergeable_state unknown", gh: func() *approvalGitHub { g := blockedDismissed(); g.mergeableState = "unknown"; return g }(), undetermined: `"unknown"`},
		{name: "reviews read 500", gh: func() *approvalGitHub {
			g := blockedDismissed()
			g.reviewsStatus = http.StatusInternalServerError
			return g
		}(), undetermined: "reviews read failed"},
		{name: "truncated listing", gh: func() *approvalGitHub { g := blockedDismissed(); g.truncated = true; return g }(), undetermined: "truncated"},
		{name: "re-approved after dismissal (same login)", gh: func() *approvalGitHub {
			g := blockedDismissed()
			g.reviews = []map[string]any{review(1, "alice", "DISMISSED", adApproved), review(2, "alice", "APPROVED", adHead)}
			return g
		}(), determined: true},
		{name: "dismissed then changes requested (same login)", gh: func() *approvalGitHub {
			g := blockedDismissed()
			g.reviews = []map[string]any{review(1, "alice", "DISMISSED", adApproved), review(2, "alice", "CHANGES_REQUESTED", adHead)}
			return g
		}(), determined: true},
		{name: "another reviewer's approval is live", gh: func() *approvalGitHub {
			g := blockedDismissed()
			g.reviews = []map[string]any{review(1, "alice", "DISMISSED", adApproved), review(2, "bob", "APPROVED", adHead)}
			return g
		}(), determined: true},
		{name: "never reviewed", gh: func() *approvalGitHub { g := blockedDismissed(); g.reviews = []map[string]any{}; return g }(), determined: true},
		{name: "dismissed with a later COMMENTED from another reviewer refuses", gh: func() *approvalGitHub {
			g := blockedDismissed()
			g.reviews = []map[string]any{review(1, "alice", "DISMISSED", adApproved), review(2, "bob", "COMMENTED", adHead)}
			return g
		}(), determined: true, refuse: true},
		{name: "older APPROVED then newer DISMISSED (same login) refuses", gh: func() *approvalGitHub {
			g := blockedDismissed()
			g.reviews = []map[string]any{review(1, "alice", "APPROVED", "eeee"), review(2, "alice", "DISMISSED", adApproved), review(3, "alice", "PENDING", "")}
			return g
		}(), determined: true, refuse: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newApprovalFixture(t, c.gh)
			if c.mutate != nil {
				c.mutate(f.runRow)
			}
			res := f.s.approvalDismissedCheck(context.Background(), f.runRow)
			if res.Determined != c.determined {
				t.Errorf("Determined = %v, want %v (undetermined %q)", res.Determined, c.determined, res.Undetermined)
			}
			if c.undetermined != "" && !strings.Contains(res.Undetermined, c.undetermined) {
				t.Errorf("Undetermined = %q, want it to name %q", res.Undetermined, c.undetermined)
			}
			if c.determined && res.Undetermined != "" {
				t.Errorf("Undetermined = %q on a determined result, want empty", res.Undetermined)
			}
			if (res.Refusal != nil) != c.refuse {
				t.Fatalf("refusal = %+v, want refuse=%v", res.Refusal, c.refuse)
			}
			if c.refuse {
				d := res.Refusal.Details
				if res.Refusal.Code != mergeCodeApprovalDismissed || d["approved_commit"] != adApproved || d["dismissing_commit"] != adHead ||
					d["dismissed_reviewer"] != "alice" || d["next_step"] != "approve_pr" {
					t.Errorf("refusal = %+v, want approval_dismissed alice %s -> %s next_step approve_pr", res.Refusal, adApproved, adHead)
				}
			}
		})
	}
}

// TestApprovalDismissedCheck_DismissingCause pins the chain-read cause ladder:
// a vouched head, a fix-up head, an unrecorded head, and an audit read error.
// Counterfactual: deleting the operator_commit_vouched / fixup_pushed rung
// collapses its row to "push".
func TestApprovalDismissedCheck_DismissingCause(t *testing.T) {
	cases := []struct {
		name string
		seed func(au *auditFake, runID uuid.UUID)
		want string
	}{
		{"vouched head", func(au *auditFake, runID uuid.UUID) {
			seedReadinessEntry(au, runID, nil, CategoryOperatorCommitVouched, 3, map[string]any{lineageVouchedSHAField: adHead})
		}, dismissingCauseVouchCommit},
		{"fix-up head", func(au *auditFake, runID uuid.UUID) {
			seedReadinessEntry(au, runID, nil, CategoryOperatorCommitVouched, 3, map[string]any{lineageVouchedSHAField: "ffff"})
			seedReadinessEntry(au, runID, nil, CategoryFixupPushed, 4, map[string]any{"head_sha": strings.ToUpper(adHead)})
		}, dismissingCauseFixupPush},
		{"unrecorded head", func(au *auditFake, runID uuid.UUID) {
			seedReadinessEntry(au, runID, nil, CategoryFixupPushed, 4, map[string]any{"head_sha": "ffff"})
		}, dismissingCausePush},
		{"audit read error", func(au *auditFake, _ uuid.UUID) {
			au.listByCategoryErrCategory = CategoryFixupPushed
		}, dismissingCauseUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newApprovalFixture(t, blockedDismissed())
			c.seed(f.au, f.runRow.ID)
			res := f.s.approvalDismissedCheck(context.Background(), f.runRow)
			if res.Refusal == nil {
				t.Fatalf("refusal = nil (undetermined %q), want approval_dismissed", res.Undetermined)
			}
			if got := res.Refusal.Details["dismissing_cause"]; got != c.want {
				t.Errorf("dismissing_cause = %v, want %s", got, c.want)
			}
			if !strings.Contains(res.Refusal.Message, dismissingCausePhrase(c.want)) {
				t.Errorf("message %q does not carry the cause phrase", res.Refusal.Message)
			}
		})
	}
}

// TestDismissingCause_UnwiredOrEmptyHead pins the two degrade arms that need
// no chain read.
func TestDismissingCause_UnwiredOrEmptyHead(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if got := s.dismissingCause(context.Background(), &run.Run{ID: uuid.New()}, adHead); got != dismissingCauseUnknown {
		t.Errorf("unwired audit repo: cause = %q, want unknown", got)
	}
	s2 := New(Config{Addr: "127.0.0.1:0", AuditRepo: newAuditFake()})
	if got := s2.dismissingCause(context.Background(), &run.Run{ID: uuid.New()}, ""); got != dismissingCauseUnknown {
		t.Errorf("empty head: cause = %q, want unknown", got)
	}
}

// TestMergeReadinessRenderHelpers pins the placeholder renders for an empty
// sha / login and the payload decoder's degrade on a non-object payload.
func TestMergeReadinessRenderHelpers(t *testing.T) {
	if got := shaOrUnrecorded(""); got != "(unrecorded)" {
		t.Errorf("shaOrUnrecorded(\"\") = %q", got)
	}
	if got := reviewerOrUnknown(""); got != "(a deleted account)" {
		t.Errorf("reviewerOrUnknown(\"\") = %q", got)
	}
	if got := auditPayloadString([]byte(`[1]`), "head_sha"); got != "" {
		t.Errorf("auditPayloadString on an array = %q, want empty", got)
	}
	if got := auditPayloadString([]byte(`{"head_sha":7}`), "head_sha"); got != "" {
		t.Errorf("auditPayloadString on a non-string = %q, want empty", got)
	}
}
