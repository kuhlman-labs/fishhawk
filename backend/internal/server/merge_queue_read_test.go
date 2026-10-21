package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The ADR-093 slice 1 per-base merge-queue read (E87.4 / #4295) over the mcq
// fixture: the REAL mergequeue.PostgresStore, run and audit repositories on
// one pgtest pool, and the multi-PR fake GitHub.

// mqrOperator is a cookie-session operator in account acct ("" untenanted):
// requireWriteScope admits a session without a token scope check.
func mqrOperator(acct string) Identity {
	return Identity{Subject: "github:ops", UserID: "00000000-0000-0000-0000-0000000000b1", AccountID: acct}
}

// getMergeQueue drives handleGetMergeQueue directly for repo x/y with id.
func getMergeQueue(t *testing.T, s *Server, query string, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v0/repos/x/y/merge-queue"
	if query != "" {
		path += "?" + query
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetPathValue("owner", "x")
	req.SetPathValue("name", "y")
	w := httptest.NewRecorder()
	s.handleGetMergeQueue(w, withIdentity(req, id))
	return w
}

func decodeMergeQueue(t *testing.T, w *httptest.ResponseRecorder) mergeQueueReadResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("merge-queue read = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp mergeQueueReadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode merge-queue: %v\n%s", err, w.Body.String())
	}
	return resp
}

// requireErrorCode asserts the status and the error envelope's code.
func requireErrorCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if w.Code != status || env.Error.Code != code {
		t.Fatalf("response = %d %q, want %d %q:\n%s", w.Code, env.Error.Code, status, code, w.Body.String())
	}
}

// seedLineTurn inserts a MERGED entry for runID on base BY CONSTRUCTION,
// admitted turn before it settled, so its line turn is exact.
func (f *mcqFixture) seedLineTurn(runID uuid.UUID, baseRef string, turn time.Duration) {
	f.t.Helper()
	f.exec(`INSERT INTO merge_candidate_queue_entries
  (run_id, repo, base_ref, state, enqueued_at, anchored_head_sha, admitted_at, settled_at)
SELECT $1, 'x/y', $2, 'merged', c.ts - interval '2 hours', 'h', c.ts - interval '1 hour' - make_interval(secs => $3::float8), c.ts - interval '1 hour'
FROM (SELECT clock_timestamp() AS ts) c`, runID, baseRef, turn.Seconds())
}

// mqrAccount inserts an account row and returns its id.
func (f *mcqFixture) mqrAccount() uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, id, "mqr-"+id.String())
	return id
}

// TestMergeQueueRead_FIFOPositionsIntentHeadOfLineAndETA is the operator
// criterion merge-queue-read-head-of-line-and-eta on HTTP: A holds the line
// (live, verifying), B was held by the rebase path (no intent), C was queued
// by merge_run (intent). Three merged line turns (60s, 100s, 20s; median 60s)
// give B eta 60 and C eta 120; A's PR has moved past its anchor, so the head
// of line carries both heads.
func TestMergeQueueRead_FIFOPositionsIntentHeadOfLineAndETA(t *testing.T) {
	f := newMCQFixture(t)
	f.srv.cfg.GateMerger = &fakeMerger{}
	a, b := f.wave()
	c := f.seed("c", "c0", []byte(mqeSpecYAML))
	f.behind(c, "c0", "c1")
	f.gh.set(func() { f.gh.reviews[c.pr] = mqeApprovedBy("alice") })
	if w := f.mergeRun(c); w.Code != http.StatusAccepted {
		t.Fatalf("merge C = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	f.srv.bgMergeQueuePump.Wait()
	hist := f.seed("hist", "h0", []byte(mcvUndelegatedSpecYAML))
	for _, turn := range []time.Duration{60 * time.Second, 100 * time.Second, 20 * time.Second} {
		f.seedLineTurn(hist.id, mcqBase, turn)
	}
	f.gh.set(func() { f.gh.prs[a.pr].head = "a2" })
	// Another repository's queue in the same account never appears.
	inst := int64(99)
	other, err := f.repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "x/z", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI, InstallationID: &inst,
	})
	if err != nil {
		t.Fatalf("create x/z run: %v", err)
	}
	if _, err := f.store.Enqueue(context.Background(), mergequeue.EnqueueRequest{RunID: other.ID, BaseRef: mcqBase, HeadSHA: "z0"}); err != nil {
		t.Fatalf("enqueue x/z: %v", err)
	}

	w := getMergeQueue(t, f.srv, "", mqrOperator(""))
	resp := decodeMergeQueue(t, w)
	if resp.Repo != "x/y" || len(resp.Bases) != 1 || resp.Bases[0].BaseRef != mcqBase {
		t.Fatalf("bases = %+v, want one %s block for x/y", resp.Bases, mcqBase)
	}
	base := resp.Bases[0]
	if len(base.Entries) != 3 {
		t.Fatalf("entries = %+v, want A, B, C", base.Entries)
	}
	ea, eb, ec := base.Entries[0], base.Entries[1], base.Entries[2]
	if ea.RunID != a.id.String() || ea.State != "live" || ea.Phase != string(mergequeue.PhaseVerifying) ||
		ea.Position != 0 || ea.ETASeconds != nil || ea.AdmittedAt == nil || ea.AnchoredHeadSHA != "a1" {
		t.Errorf("A = %+v, want live/verifying at a1, no position, no eta", ea)
	}
	if eb.RunID != b.id.String() || eb.State != "held" || eb.Phase != "" || eb.Position != 1 ||
		eb.MergeIntent || eb.MergeIntentAt != nil || eb.ETASeconds == nil || *eb.ETASeconds != 60 {
		t.Errorf("B = %+v, want held position 1, no intent, eta 60", eb)
	}
	if ec.RunID != c.id.String() || ec.State != "held" || ec.Position != 2 || !ec.MergeIntent ||
		ec.MergeIntentAt == nil || ec.ETASeconds == nil || *ec.ETASeconds != 120 || ec.EntryID != f.entry(c).ID.String() {
		t.Errorf("C = %+v, want held position 2 with intent, eta 120", ec)
	}
	hol := base.HeadOfLine
	if hol == nil || hol.RunID != a.id.String() || hol.State != "live" || hol.Phase != string(mergequeue.PhaseVerifying) ||
		hol.AnchoredHeadSHA != "a1" || hol.CurrentHeadSHA != "a2" || hol.CurrentHeadUnavailable != "" {
		t.Errorf("head_of_line = %+v, want A live/verifying anchored a1, current a2", hol)
	}
	if base.ETABasis.Samples != 3 || base.ETABasis.MedianSeconds == nil || *base.ETABasis.MedianSeconds != 60 ||
		!strings.Contains(base.ETABasis.Note, "operator's merge step") {
		t.Errorf("eta_basis = %+v, want 3 samples, median 60, a note naming the operator merge step", base.ETABasis)
	}

	// From the raw bytes: merge_intent is ON the wire as false for B (absent
	// and false are different claims), merge_intent_at is absent.
	var raw struct {
		Bases []struct {
			Entries []map[string]json.RawMessage `json:"entries"`
		} `json:"bases"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	rawB := raw.Bases[0].Entries[1]
	if got, ok := rawB["merge_intent"]; !ok || string(got) != "false" {
		t.Errorf("B merge_intent on the wire = %s (present %v), want false", got, ok)
	}
	if _, ok := rawB["merge_intent_at"]; ok {
		t.Errorf("B carries merge_intent_at, want it absent without intent")
	}

	// ?base_ref names one base; a base with nothing active answers an empty
	// block with no head of line and no median.
	if one := decodeMergeQueue(t, getMergeQueue(t, f.srv, "base_ref="+mcqBase, mqrOperator(""))); len(one.Bases) != 1 || len(one.Bases[0].Entries) != 3 {
		t.Errorf("base_ref=%s = %+v, want the same three entries", mcqBase, one.Bases)
	}
	empty := decodeMergeQueue(t, getMergeQueue(t, f.srv, "base_ref=release", mqrOperator("")))
	if len(empty.Bases) != 1 || empty.Bases[0].BaseRef != "release" || len(empty.Bases[0].Entries) != 0 ||
		empty.Bases[0].HeadOfLine != nil || empty.Bases[0].ETABasis.Samples != 0 || empty.Bases[0].ETABasis.MedianSeconds != nil {
		t.Errorf("base_ref=release = %+v, want one empty block, no head of line, no median", empty.Bases)
	}
}

// TestMergeQueueRead_ETAOmittedWithoutHistory: with no merged line turn on
// the base, held entries carry no eta_seconds and the basis no median.
func TestMergeQueueRead_ETAOmittedWithoutHistory(t *testing.T) {
	f := newMCQFixture(t)
	f.wave()
	w := getMergeQueue(t, f.srv, "", mqrOperator(""))
	resp := decodeMergeQueue(t, w)
	if len(resp.Bases) != 1 || len(resp.Bases[0].Entries) != 2 {
		t.Fatalf("bases = %+v, want one base with A and B", resp.Bases)
	}
	if e := resp.Bases[0].Entries[1]; e.Position != 1 || e.ETASeconds != nil {
		t.Errorf("B = %+v, want held position 1 with no eta", e)
	}
	if basis := resp.Bases[0].ETABasis; basis.Samples != 0 || basis.MedianSeconds != nil {
		t.Errorf("eta_basis = %+v, want no samples and no median", basis)
	}
	if strings.Contains(w.Body.String(), `"eta_seconds"`) || strings.Contains(w.Body.String(), `"median_seconds"`) {
		t.Errorf("body carries an eta key without history:\n%s", w.Body.String())
	}
}

// TestMedianLineTurnSeconds pins the median: odd and even counts (mean of the
// middle two), rounding to the second, input order, and no samples.
func TestMedianLineTurnSeconds(t *testing.T) {
	cases := []struct {
		name  string
		turns []time.Duration
		want  int64
		ok    bool
	}{
		{"none", nil, 0, false},
		{"one", []time.Duration{42 * time.Second}, 42, true},
		{"odd unsorted", []time.Duration{100 * time.Second, 20 * time.Second, 60 * time.Second}, 60, true},
		{"even mean of middle two", []time.Duration{10 * time.Second, 40 * time.Second, 20 * time.Second, 90 * time.Second}, 30, true},
		{"rounds to the second", []time.Duration{1500 * time.Millisecond}, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]time.Duration(nil), tc.turns...)
			got, ok := medianLineTurnSeconds(in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("median(%v) = %d, %v; want %d, %v", tc.turns, got, ok, tc.want, tc.ok)
			}
			for i := range in {
				if in[i] != tc.turns[i] {
					t.Fatalf("median reordered its input: %v", in)
				}
			}
		})
	}
}

// TestMergeQueueRead_OtherAccountInvisible: accounts A and B each have an
// entry on the SAME (repo, base) by construction. A caller in A sees only
// A's, a caller in B only B's, and an untenanted caller neither — both
// listing every base and naming the base. B's merged history never reaches
// A's ETA basis.
func TestMergeQueueRead_OtherAccountInvisible(t *testing.T) {
	f := newMCQFixture(t)
	ctx := context.Background()
	acctA, acctB := f.mqrAccount(), f.mqrAccount()
	ra := f.seed("ra", "ra0", []byte(mcvUndelegatedSpecYAML))
	rb := f.seed("rb", "rb0", []byte(mcvUndelegatedSpecYAML))
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acctA, ra.id)
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, acctB, rb.id)
	for _, r := range []*mcqRun{ra, rb} {
		if _, err := f.store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: "h-" + r.branch}); err != nil {
			t.Fatalf("enqueue %s: %v", r.id, err)
		}
	}
	f.seedLineTurn(rb.id, mcqBase, 30*time.Second)

	for _, tc := range []struct {
		name    string
		acct    string
		wantRun string
		samples int
	}{
		{"account A", acctA.String(), ra.id.String(), 0},
		{"account B", acctB.String(), rb.id.String(), 1},
		{"untenanted", "", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			all := decodeMergeQueue(t, getMergeQueue(t, f.srv, "", mqrOperator(tc.acct)))
			named := decodeMergeQueue(t, getMergeQueue(t, f.srv, "base_ref="+mcqBase, mqrOperator(tc.acct)))
			if len(named.Bases) != 1 {
				t.Fatalf("base_ref read = %+v, want one block", named.Bases)
			}
			if tc.wantRun == "" {
				if len(all.Bases) != 0 || len(named.Bases[0].Entries) != 0 {
					t.Fatalf("untenanted read = %+v / %+v, want no base and no entry", all.Bases, named.Bases)
				}
				return
			}
			if len(all.Bases) != 1 || len(all.Bases[0].Entries) != 1 || all.Bases[0].Entries[0].RunID != tc.wantRun {
				t.Fatalf("listing read = %+v, want only run %s", all.Bases, tc.wantRun)
			}
			got := named.Bases[0]
			if len(got.Entries) != 1 || got.Entries[0].RunID != tc.wantRun || got.HeadOfLine == nil || got.HeadOfLine.RunID != tc.wantRun {
				t.Fatalf("base_ref read = %+v, want only run %s at the head of line", got, tc.wantRun)
			}
			if got.ETABasis.Samples != tc.samples {
				t.Fatalf("eta_basis samples = %d, want %d (account-scoped history)", got.ETABasis.Samples, tc.samples)
			}
		})
	}
}

// TestMergeQueueRead_AuthLadder is the operator criterion
// merge-queue-read-refuses-run-bound plus the scope tier: a run-bound token
// is refused 403 run_token_forbidden EVEN when it carries read:audit (so the
// subject check, not the scope check, is what refuses it); anonymous is 401
// (through the shipped route too, which proves the registration); a token
// without read:audit is 403 insufficient_scope; an operator token with it and
// a cookie-session operator read 200.
func TestMergeQueueRead_AuthLadder(t *testing.T) {
	f := newMCQFixture(t)
	f.wave()
	runBound := Identity{Subject: "mcp:run:" + uuid.NewString(), TokenID: "tok-run", Scopes: []string{scopeRunBoundRead, scopeGateViewRead}}
	requireErrorCode(t, getMergeQueue(t, f.srv, "", runBound), http.StatusForbidden, "run_token_forbidden")
	requireErrorCode(t, getMergeQueue(t, f.srv, "", Identity{Subject: "anonymous"}), http.StatusUnauthorized, "authentication_required")
	noScope := Identity{Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"read:runs"}}
	requireErrorCode(t, getMergeQueue(t, f.srv, "", noScope), http.StatusForbidden, "insufficient_scope")
	if w := getMergeQueue(t, f.srv, "", gateViewReadIdentity()); w.Code != http.StatusOK {
		t.Fatalf("read:audit token = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if w := getMergeQueue(t, f.srv, "", mqrOperator("")); w.Code != http.StatusOK {
		t.Fatalf("cookie operator = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/repos/x/y/merge-queue", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET through the route = %d, want 401 (a 404 means the route is not registered):\n%s", rec.Code, rec.Body.String())
	}
}

// TestMergeQueueRead_Unconfigured: an unwired queue is 503.
func TestMergeQueueRead_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	requireErrorCode(t, getMergeQueue(t, s, "", mqrOperator("")), http.StatusServiceUnavailable, "merge_queue_unconfigured")
}

// TestMergeQueueRead_NonUUIDAccountFailsClosed: a corrupted session account
// is a 500, never a read widened to the untenanted queue.
func TestMergeQueueRead_NonUUIDAccountFailsClosed(t *testing.T) {
	f := newMCQFixture(t)
	f.wave()
	requireErrorCode(t, getMergeQueue(t, f.srv, "", mqrOperator("not-a-uuid")), http.StatusInternalServerError, "internal_error")
}

// TestMergeQueueRead_RepoForbidden: a repository the caller cannot see on
// the forge is a point-read DENY (403 repo_forbidden).
func TestMergeQueueRead_RepoForbidden(t *testing.T) {
	f := newMCQFixture(t)
	f.wave()
	f.srv.cfg.RepoVisibility = newFakeRepoVisibility(map[string]bool{})
	f.srv.cfg.AccountRoles = fakeAccountRoles{role: account.RoleMember}
	requireErrorCode(t, getMergeQueue(t, f.srv, "", memberIdentity()), http.StatusForbidden, "repo_forbidden")
}

// mqrFaultStore wraps the real store and fails the named read.
type mqrFaultStore struct {
	mergequeue.Store
	failBases, failList, failTurns bool
}

func (s *mqrFaultStore) ActiveBases(ctx context.Context) ([]mergequeue.Base, error) {
	if s.failBases {
		return nil, errMCQStore
	}
	return s.Store.ActiveBases(ctx)
}

func (s *mqrFaultStore) ListForBase(ctx context.Context, b mergequeue.Base) ([]mergequeue.Entry, error) {
	if s.failList {
		return nil, errMCQStore
	}
	return s.Store.ListForBase(ctx, b)
}

func (s *mqrFaultStore) RecentLineTurns(ctx context.Context, b mergequeue.Base, limit int) ([]time.Duration, error) {
	if s.failTurns {
		return nil, errMCQStore
	}
	return s.Store.RecentLineTurns(ctx, b, limit)
}

// TestMergeQueueRead_StoreErrorsFailClosed: each store read error is a 500
// with no partial body, never a 200 that drops a base or an entry.
func TestMergeQueueRead_StoreErrorsFailClosed(t *testing.T) {
	f := newMCQFixture(t)
	f.wave()
	for _, tc := range []struct {
		name  string
		query string
		store *mqrFaultStore
	}{
		{"active bases", "", &mqrFaultStore{Store: f.store, failBases: true}},
		{"list for base", "", &mqrFaultStore{Store: f.store, failList: true}},
		{"list for a named base", "base_ref=" + mcqBase, &mqrFaultStore{Store: f.store, failList: true}},
		{"line turns", "", &mqrFaultStore{Store: f.store, failTurns: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.srv.cfg.MergeQueue = tc.store
			w := getMergeQueue(t, f.srv, tc.query, mqrOperator(""))
			requireErrorCode(t, w, http.StatusInternalServerError, "internal_error")
			if strings.Contains(w.Body.String(), `"bases"`) {
				t.Fatalf("a failed read carries a partial body:\n%s", w.Body.String())
			}
		})
	}
}

// mqrFailGetRun fails every GetRun.
type mqrFailGetRun struct{ run.Repository }

func (mqrFailGetRun) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, errors.New("run store unavailable")
}

// TestMergeQueueRead_CurrentHeadUnavailable: every head-of-line degrade
// answers 200 with the entries intact, no current_head_sha and the named
// reason.
func TestMergeQueueRead_CurrentHeadUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *mcqFixture, a *mcqRun)
		want   string
	}{
		{"forge read error", func(f *mcqFixture, a *mcqRun) { f.gh.set(func() { delete(f.gh.prs, a.pr) }) }, mergeQueueHeadForgeReadFailed},
		{"empty forge head", func(f *mcqFixture, a *mcqRun) { f.gh.set(func() { f.gh.prs[a.pr].head = "" }) }, mergeQueueHeadForgeReadFailed},
		{"no pull request", func(f *mcqFixture, a *mcqRun) {
			f.exec(`UPDATE runs SET pull_request_url = NULL WHERE id = $1`, a.id)
		}, mergeQueueHeadPullRequestUnknown},
		{"no installation", func(f *mcqFixture, a *mcqRun) {
			f.exec(`UPDATE runs SET installation_id = NULL WHERE id = $1`, a.id)
		}, mergeQueueHeadForgeUnwired},
		{"no forge client", func(f *mcqFixture, _ *mcqRun) { f.srv.cfg.GitHub = nil }, mergeQueueHeadForgeUnwired},
		{"run read error", func(f *mcqFixture, _ *mcqRun) { f.srv.cfg.RunRepo = mqrFailGetRun{f.repo} }, mergeQueueHeadRunReadFailed},
		{"no run repository", func(f *mcqFixture, _ *mcqRun) { f.srv.cfg.RunRepo = nil }, mergeQueueHeadRunReadFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCQFixture(t)
			a, _ := f.wave()
			tc.mutate(f, a)
			resp := decodeMergeQueue(t, getMergeQueue(t, f.srv, "", mqrOperator("")))
			if len(resp.Bases) != 1 || len(resp.Bases[0].Entries) != 2 {
				t.Fatalf("bases = %+v, want the two entries intact", resp.Bases)
			}
			hol := resp.Bases[0].HeadOfLine
			if hol == nil || hol.RunID != a.id.String() || hol.CurrentHeadSHA != "" || hol.CurrentHeadUnavailable != tc.want {
				t.Fatalf("head_of_line = %+v, want run %s with current_head_unavailable %q", hol, a.id, tc.want)
			}
		})
	}
}

// TestMergeQueueRead_HeldHeadOfLine: with no live entry the head of line is
// the next held entry to admit, carrying no phase and its current head.
func TestMergeQueueRead_HeldHeadOfLine(t *testing.T) {
	f := newMCQFixture(t)
	r := f.seed("solo", "s0", []byte(mcvUndelegatedSpecYAML))
	if _, err := f.store.Enqueue(context.Background(), mergequeue.EnqueueRequest{RunID: r.id, BaseRef: mcqBase, HeadSHA: "s-anchor"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	resp := decodeMergeQueue(t, getMergeQueue(t, f.srv, "", mqrOperator("")))
	if len(resp.Bases) != 1 {
		t.Fatalf("bases = %+v, want one", resp.Bases)
	}
	hol := resp.Bases[0].HeadOfLine
	if hol == nil || hol.RunID != r.id.String() || hol.State != "held" || hol.Phase != "" ||
		hol.AnchoredHeadSHA != "s-anchor" || hol.CurrentHeadSHA != "s0" {
		t.Fatalf("head_of_line = %+v, want held run %s anchored s-anchor, current s0", hol, r.id)
	}
}
