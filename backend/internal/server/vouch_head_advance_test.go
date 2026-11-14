package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- #4856: a server-verified fast-forward vouch advances the recorded head ---

const (
	vouchProbeHead   = "a1a1a1a100000000000000000000000000000000" // the recorded head H
	vouchProbeTip    = "b2b2b2b200000000000000000000000000000000" // the vouched tip V
	vouchProbeMid    = "c3c3c3c300000000000000000000000000000000" // an intermediate commit A
	vouchProbeOther  = "d4d4d4d400000000000000000000000000000000" // a commit on H but not on V
	vouchProbeFanIn  = "e5e5e5e500000000000000000000000000000000" // a decomposed parent's integration merge F
	vouchProbeOlder  = "f6f6f6f600000000000000000000000000000000" // an older recorded head
	vouchProbeReason = "operator remediation commit on the run branch"
)

// vouchProbeForge is the httptest GitHub the probe compares against. compares
// maps "base...head" to the commit list the compare answers; statuses maps the
// same key to a non-200 status. An unstubbed compare answers 404.
type vouchProbeForge struct {
	mu           sync.Mutex
	compares     map[string][]string
	statuses     map[string]int
	compareCalls int
}

func (f *vouchProbeForge) set(base, head string, commits ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if commits == nil {
		commits = []string{}
	}
	f.compares[base+"..."+head] = commits
}

func (f *vouchProbeForge) fail(base, head string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[base+"..."+head] = status
}

func (f *vouchProbeForge) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.compareCalls
}

// newVouchProbeGitHubClient serves the compare route and a PR read (base ref
// main, head = the vouched tip) so resolveLastRunAuthoredHead can anchor.
func newVouchProbeGitHubClient(t *testing.T, f *vouchProbeForge) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("basehead")
		f.mu.Lock()
		f.compareCalls++
		status, failed := f.statuses[key]
		commits, ok := f.compares[key]
		f.mu.Unlock()
		if failed {
			w.WriteHeader(status)
			return
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		type c struct {
			SHA string `json:"sha"`
		}
		body := struct {
			Commits []c `json:"commits"`
		}{Commits: []c{}}
		for _, sha := range commits {
			body.Commits = append(body.Commits, c{SHA: sha})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"node_id":"PR_x","state":"open","head":{"sha":%q,"ref":"run/branch"},"base":{"ref":"main"}}`, vouchProbeTip)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
}

// vouchProbeOpts reach each cannot-probe rung BY CONSTRUCTION.
type vouchProbeOpts struct {
	noGitHubClient bool
	noInstallation bool
	repoOverride   string
}

type vouchProbeFixture struct {
	s       *Server
	au      *auditFake
	rr      *promptRunRepo
	forge   *vouchProbeForge
	runRow  *run.Run
	stageID uuid.UUID
	clock   time.Time
}

func newVouchProbeFixture(t *testing.T, opts vouchProbeOpts) *vouchProbeFixture {
	t.Helper()
	f := &vouchProbeFixture{
		forge: &vouchProbeForge{compares: map[string][]string{}, statuses: map[string]int{}},
		clock: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC),
	}
	var gh *githubclient.Client
	if !opts.noGitHubClient {
		gh = newVouchProbeGitHubClient(t, f.forge)
	}
	repo := "x/y"
	if opts.repoOverride != "" {
		repo = opts.repoOverride
	}
	prURL := "https://github.com/x/y/pull/7"
	f.runRow = &run.Run{ID: uuid.New(), Repo: repo, State: run.StateRunning, InstallationID: instID(99), PullRequestURL: &prURL}
	if opts.noInstallation {
		f.runRow.InstallationID = nil
	}
	f.stageID = uuid.New()
	stage := &run.Stage{ID: f.stageID, RunID: f.runRow.ID, Type: run.StageTypeImplement}
	f.s, _, f.au, f.rr = newLineageServer(t, gh, f.runRow, stage)
	f.au.stampSequence = true
	return f
}

// appendRow seeds a chain row BY CONSTRUCTION (never through the control under
// test), one minute after the previous row, so timestamp and sequence agree.
func (f *vouchProbeFixture) appendRow(category string, fields map[string]any) {
	f.clock = f.clock.Add(time.Minute)
	payload, _ := json.Marshal(fields)
	f.au.mu.Lock()
	f.au.appended = append(f.au.appended, audit.ChainAppendParams{
		RunID: f.runRow.ID, Timestamp: f.clock, Category: category, Payload: payload,
	})
	f.au.mu.Unlock()
}

func (f *vouchProbeFixture) seedHead(category, sha string) {
	f.appendRow(category, map[string]any{"head_sha": sha})
}

// seedVerifiedVouch seeds a server-verified vouch row in the writer's shape.
func (f *vouchProbeFixture) seedVerifiedVouch(prior, vouched string) {
	f.appendRow(CategoryOperatorCommitVouched, map[string]any{
		"run_id": f.runRow.ID.String(), auditcomplete.VouchedSHAField: vouched, "reason": vouchProbeReason,
		auditcomplete.VouchPriorHeadSHAField: prior, auditcomplete.VouchHeadAdvanceVerifiedField: true,
	})
}

// vouch drives the REAL handler and decodes the response.
func (f *vouchProbeFixture) vouch(t *testing.T, sha string) vouchCommitResponse {
	t.Helper()
	w := postVouchCommit(t, f.s, f.runRow.ID, vouchCommitRequest{SHA: sha, Reason: vouchProbeReason}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("vouch %s: status = %d, want 200:\n%s", sha, w.Code, w.Body.String())
	}
	var resp vouchCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal vouch response: %v", err)
	}
	return resp
}

// lastVouchPayload decodes the newest operator_commit_vouched row as a raw map.
func (f *vouchProbeFixture) lastVouchPayload(t *testing.T) map[string]any {
	t.Helper()
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	for i := len(f.au.appended) - 1; i >= 0; i-- {
		if f.au.appended[i].Category == CategoryOperatorCommitVouched {
			var m map[string]any
			if err := json.Unmarshal(f.au.appended[i].Payload, &m); err != nil {
				t.Fatalf("decode vouch payload: %v", err)
			}
			return m
		}
	}
	t.Fatal("no operator_commit_vouched row recorded")
	return nil
}

// assertNoAdvance asserts the row was recorded with today's key set, the
// response says the head did not advance, and the note names the rung.
func assertNoAdvance(t *testing.T, f *vouchProbeFixture, resp vouchCommitResponse, phrase string) {
	t.Helper()
	p := f.lastVouchPayload(t)
	if _, ok := p[auditcomplete.VouchPriorHeadSHAField]; ok {
		t.Errorf("non-advance payload carries %s: %v", auditcomplete.VouchPriorHeadSHAField, p)
	}
	if _, ok := p[auditcomplete.VouchHeadAdvanceVerifiedField]; ok {
		t.Errorf("non-advance payload carries %s: %v", auditcomplete.VouchHeadAdvanceVerifiedField, p)
	}
	if len(p) != 3 {
		t.Errorf("non-advance payload keys = %v, want exactly {run_id, vouched_sha, reason}", p)
	}
	if resp.RecordedHeadAdvanced {
		t.Error("recorded_head_advanced = true, want false")
	}
	if !strings.Contains(resp.RecordedHeadAdvanceNote, phrase) {
		t.Errorf("recorded_head_advance_note = %q, want it to contain %q", resp.RecordedHeadAdvanceNote, phrase)
	}
}

func assertAdvanced(t *testing.T, f *vouchProbeFixture, resp vouchCommitResponse, prior string) {
	t.Helper()
	if !resp.RecordedHeadAdvanced {
		t.Fatalf("recorded_head_advanced = false, want true (note %q)", resp.RecordedHeadAdvanceNote)
	}
	if resp.RecordedHeadAdvanceNote != "" {
		t.Errorf("recorded_head_advance_note = %q, want empty on an advance", resp.RecordedHeadAdvanceNote)
	}
	p := f.lastVouchPayload(t)
	if p[auditcomplete.VouchPriorHeadSHAField] != prior {
		t.Errorf("payload prior_head_sha = %v, want %s", p[auditcomplete.VouchPriorHeadSHAField], prior)
	}
	if p[auditcomplete.VouchHeadAdvanceVerifiedField] != true {
		t.Errorf("payload head_advance_verified = %v, want true", p[auditcomplete.VouchHeadAdvanceVerifiedField])
	}
}

// TestVouchHeadAdvance_FastForward_Advances: V fast-forwards the recorded head
// H and adds only itself, so the row carries both advance keys.
func TestVouchHeadAdvance_FastForward_Advances(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("acceptance_scenarios_pushed", vouchProbeHead)
	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeTip)
	assertAdvanced(t, f, f.vouch(t, vouchProbeTip), vouchProbeHead)
}

// TestVouchHeadAdvance_NotFastForward_NoAdvance: H carries a commit V lacks.
// ahead=[V] would pass every later rung, so the behind check is the only
// thing refusing the advance.
func TestVouchHeadAdvance_NotFastForward_NoAdvance(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("fixup_pushed", vouchProbeHead)
	f.forge.set(vouchProbeTip, vouchProbeHead, vouchProbeOther)
	f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeTip)
	assertNoAdvance(t, f, f.vouch(t, vouchProbeTip), "is not a fast-forward of the recorded head")
}

// TestVouchHeadAdvance_UnattributedIntermediateCommit_NoAdvance: A sits between
// H and V and is in no ledger category; the ledger is complete.
func TestVouchHeadAdvance_UnattributedIntermediateCommit_NoAdvance(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("fixup_pushed", vouchProbeHead)
	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeMid, vouchProbeTip)
	resp := f.vouch(t, vouchProbeTip)
	assertNoAdvance(t, f, resp, "is not run lineage")
	if !strings.Contains(resp.RecordedHeadAdvanceNote, vouchProbeMid) {
		t.Errorf("note %q does not name the offending commit %s", resp.RecordedHeadAdvanceNote, vouchProbeMid)
	}
}

// TestVouchHeadAdvance_IntermediateCommitVouchedFirst_Advances: A was the
// subject of an EARLIER verified vouch (recorded before the current seed, so
// the walk ignores it), so A is a closure-ledger member and V verifies.
func TestVouchHeadAdvance_IntermediateCommitVouchedFirst_Advances(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("pull_request_opened", vouchProbeOlder)
	f.seedVerifiedVouch(vouchProbeOlder, vouchProbeMid)
	f.seedHead("fixup_pushed", vouchProbeHead)
	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeMid, vouchProbeTip)
	assertAdvanced(t, f, f.vouch(t, vouchProbeTip), vouchProbeHead)
}

// TestVouchHeadAdvance_UnverifiedIntermediateVouch_NoAdvance (approval
// condition C5): a foreign A vouched UNVERIFIED (not a fast-forward) is a
// lineage-ledger member, but NOT a closure member, so V over A stays
// unverified. Transitive trust runs only through verified edges.
func TestVouchHeadAdvance_UnverifiedIntermediateVouch_NoAdvance(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("fixup_pushed", vouchProbeHead)
	f.forge.set(vouchProbeMid, vouchProbeHead, vouchProbeOther)
	f.forge.set(vouchProbeHead, vouchProbeMid, vouchProbeMid)
	assertNoAdvance(t, f, f.vouch(t, vouchProbeMid), "is not a fast-forward")

	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeMid, vouchProbeTip)
	resp := f.vouch(t, vouchProbeTip)
	assertNoAdvance(t, f, resp, "the commit "+vouchProbeMid+" between the recorded head")
}

// TestVouchHeadAdvance_NoRecordedHead_NoForgeCall: a vouch never invents a
// head, and the probe returns before any compare.
func TestVouchHeadAdvance_NoRecordedHead_NoForgeCall(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	assertNoAdvance(t, f, f.vouch(t, vouchProbeTip), "no recorded head")
	if n := f.forge.calls(); n != 0 {
		t.Errorf("compare calls = %d, want 0", n)
	}
}

// TestVouchHeadAdvance_AlreadyRecordedHead_NoAdvance: vouching the recorded
// head itself moves nothing.
func TestVouchHeadAdvance_AlreadyRecordedHead_NoAdvance(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("fixup_pushed", vouchProbeHead)
	assertNoAdvance(t, f, f.vouch(t, vouchProbeHead), "is already the run's recorded head")
	if n := f.forge.calls(); n != 0 {
		t.Errorf("compare calls = %d, want 0", n)
	}
}

// TestVouchHeadAdvance_CompareError_NoAdvance: each compare failing answers
// its own note.
func TestVouchHeadAdvance_CompareError_NoAdvance(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*vouchProbeForge)
		phrase string
	}{
		{"behind compare 500", func(g *vouchProbeForge) {
			g.fail(vouchProbeTip, vouchProbeHead, http.StatusInternalServerError)
			g.set(vouchProbeHead, vouchProbeTip, vouchProbeTip)
		}, "comparing the vouched commit " + vouchProbeTip + " against the recorded head"},
		{"ahead compare 500", func(g *vouchProbeForge) {
			g.set(vouchProbeTip, vouchProbeHead)
			g.fail(vouchProbeHead, vouchProbeTip, http.StatusInternalServerError)
		}, "comparing the recorded head " + vouchProbeHead + " against the vouched commit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVouchProbeFixture(t, vouchProbeOpts{})
			f.seedHead("fixup_pushed", vouchProbeHead)
			tt.setup(f.forge)
			assertNoAdvance(t, f, f.vouch(t, vouchProbeTip), tt.phrase)
		})
	}
}

// TestVouchHeadAdvance_EmptyAhead_NoAdvance: an empty ahead list is not ahead.
func TestVouchHeadAdvance_EmptyAhead_NoAdvance(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("fixup_pushed", vouchProbeHead)
	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip)
	assertNoAdvance(t, f, f.vouch(t, vouchProbeTip), "is not ahead of the recorded head")
}

// TestVouchHeadAdvance_AheadAtCap_NoAdvance: a compare at the 250-commit cap
// may be truncated, so the probe fails closed. Every listed commit is a
// recorded head, so the closure check alone would pass.
func TestVouchHeadAdvance_AheadAtCap_NoAdvance(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	ahead := make([]string, 0, vouchProbeCompareCap)
	for i := 0; i < vouchProbeCompareCap-1; i++ {
		sha := fmt.Sprintf("%040x", i+1)
		f.seedHead("child_pushed", sha)
		ahead = append(ahead, sha)
	}
	f.seedHead("fixup_pushed", vouchProbeHead)
	ahead = append(ahead, vouchProbeTip)
	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip, ahead...)
	assertNoAdvance(t, f, f.vouch(t, vouchProbeTip), "too many commits to prove")
}

// TestVouchHeadAdvance_LedgerIncomplete_NoAdvance: the closure ledger's
// slices_integrated read fails (a category the prior-head read never touches).
// ahead=[V] would pass the closure check on the partial ledger, so the
// completeness check is the only thing refusing the advance.
func TestVouchHeadAdvance_LedgerIncomplete_NoAdvance(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("fixup_pushed", vouchProbeHead)
	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeTip)
	f.au.listByCategoryErrCategory = lineageIntegrationLedgerCategory
	assertNoAdvance(t, f, f.vouch(t, vouchProbeTip), "the run lineage ledger could not be read completely")
}

// TestVouchHeadAdvance_RecordedHeadReadError_NoAdvance: a reported-head read
// error never stamps a prior from a partial read.
func TestVouchHeadAdvance_RecordedHeadReadError_NoAdvance(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("fixup_pushed", vouchProbeHead)
	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeTip)
	f.au.listByCategoryErrCategory = "acceptance_scenarios_pushed"
	assertNoAdvance(t, f, f.vouch(t, vouchProbeTip), "the recorded head could not be read")
}

// TestVouchHeadAdvance_CannotProbe_PayloadUnchanged: with no forge to ask, the
// vouch records today's key set. The forge stub would verify the advance, so
// each cannot-probe guard is the only thing refusing it.
func TestVouchHeadAdvance_CannotProbe_PayloadUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		opts   vouchProbeOpts
		phrase string
	}{
		{"no GitHub client", vouchProbeOpts{noGitHubClient: true}, "no GitHub client is wired"},
		{"no installation", vouchProbeOpts{noInstallation: true}, "the run has no installation"},
		{"unparseable repo", vouchProbeOpts{repoOverride: "not-a-slug"}, "the run repo is unparseable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVouchProbeFixture(t, tt.opts)
			f.seedHead("fixup_pushed", vouchProbeHead)
			f.forge.set(vouchProbeTip, vouchProbeHead)
			f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeTip)
			assertNoAdvance(t, f, f.vouch(t, vouchProbeTip), tt.phrase)
		})
	}
}

// TestVouchHeadAdvance_DecomposedParent_IntegrationSeed_Advances (approval
// condition C4): a decomposed parent's only head is its fan-in integration
// merge F. The acceptance-seeded walk resolves F, the fix-up walk has no seed,
// so F is the ONE prior and the vouch F→W verifies.
func TestVouchHeadAdvance_DecomposedParent_IntegrationSeed_Advances(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.appendRow(lineageIntegrationCommitCategory, map[string]any{"merge_sha": vouchProbeFanIn})
	f.forge.set(vouchProbeTip, vouchProbeFanIn)
	f.forge.set(vouchProbeFanIn, vouchProbeTip, vouchProbeTip)
	assertAdvanced(t, f, f.vouch(t, vouchProbeTip), vouchProbeFanIn)
}

// TestVouchHeadAdvance_WalksDisagree_NoAdvance (approval condition C4): the
// fix-up walk resolves the reported head H while the acceptance-seeded walk
// resolves the newer integration merge F. No single prior exists, so the vouch
// is recorded unverified, even though the forge would verify V over H.
func TestVouchHeadAdvance_WalksDisagree_NoAdvance(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("pull_request_opened", vouchProbeHead)
	f.appendRow(lineageIntegrationCommitCategory, map[string]any{"merge_sha": vouchProbeFanIn})
	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeTip)
	resp := f.vouch(t, vouchProbeTip)
	assertNoAdvance(t, f, resp, "the fix-up walk resolves the recorded head "+vouchProbeHead+" but the acceptance-seeded walk resolves "+vouchProbeFanIn)
}

// TestVouchHeadAdvance_ChainedVerifiedVouches_Advance: a second verified vouch
// stamps the head the first one advanced to (both walks agree on it).
func TestVouchHeadAdvance_ChainedVerifiedVouches_Advance(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("fixup_pushed", vouchProbeHead)
	f.forge.set(vouchProbeMid, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeMid, vouchProbeMid)
	assertAdvanced(t, f, f.vouch(t, vouchProbeMid), vouchProbeHead)
	f.forge.set(vouchProbeTip, vouchProbeMid)
	f.forge.set(vouchProbeMid, vouchProbeTip, vouchProbeTip)
	assertAdvanced(t, f, f.vouch(t, vouchProbeTip), vouchProbeMid)
}

// TestVouchCommit_RunBoundToken_RefusedBeforeProbe (approval condition C8): a
// run-bound mcp:run:* token is still refused on POST vouch-commit, before the
// probe runs, on a fixture where the probe would verify an advance.
func TestVouchCommit_RunBoundToken_RefusedBeforeProbe(t *testing.T) {
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.seedHead("fixup_pushed", vouchProbeHead)
	f.forge.set(vouchProbeTip, vouchProbeHead)
	f.forge.set(vouchProbeHead, vouchProbeTip, vouchProbeTip)
	runBound := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "mcp:run:" + f.runRow.ID.String(), TokenID: "tok-run", Scopes: []string{"write:stages"},
		}))
	}
	w := postVouchCommit(t, f.s, f.runRow.ID, vouchCommitRequest{SHA: vouchProbeTip, Reason: vouchProbeReason}, runBound)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "run_token_forbidden") {
		t.Fatalf("status = %d body = %s, want 403 run_token_forbidden", w.Code, w.Body.String())
	}
	if vouchAudit(f.au) != nil {
		t.Error("a run-bound token appended an operator_commit_vouched row")
	}
	if n := f.forge.calls(); n != 0 {
		t.Errorf("compare calls = %d, want 0 (the probe ran before the refusal)", n)
	}
}

// vouchProbeAuditRepo wraps the fixture's auditFake so a test can fail ONE
// run's read of a category, or only the Nth read of it, which the
// category-wide listByCategoryErrCategory injection cannot express. nilRows
// prepends a nil entry to every successful read.
type vouchProbeAuditRepo struct {
	*auditFake
	mu      sync.Mutex
	calls   map[string]int
	failRun uuid.UUID // uuid.Nil matches every run
	failCat string
	failNth int // 0 fails every matching read
	nilRows bool
}

func (r *vouchProbeAuditRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	r.mu.Lock()
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	r.calls[category]++
	n := r.calls[category]
	r.mu.Unlock()
	if category == r.failCat && (r.failRun == uuid.Nil || r.failRun == runID) && (r.failNth == 0 || r.failNth == n) {
		return nil, errors.New("vouchProbeAuditRepo: injected read error for " + category)
	}
	entries, err := r.auditFake.ListForRunByCategory(ctx, runID, category)
	if err != nil || !r.nilRows {
		return entries, err
	}
	return append([]*audit.Entry{nil}, entries...), nil
}

// wrapAudit installs a vouchProbeAuditRepo over the fixture's auditFake.
func (f *vouchProbeFixture) wrapAudit() *vouchProbeAuditRepo {
	w := &vouchProbeAuditRepo{auditFake: f.au}
	f.s.cfg.AuditRepo = w
	return w
}

// addChild registers a decomposition child of the fixture run.
func (f *vouchProbeFixture) addChild() *run.Run {
	parent := f.runRow.ID
	child := &run.Run{ID: uuid.New(), Repo: f.runRow.Repo, State: run.StateRunning, DecomposedFrom: &parent}
	f.rr.getRuns[child.ID] = child
	return child
}

// appendChildRow seeds a chain row on a child run BY CONSTRUCTION.
func (f *vouchProbeFixture) appendChildRow(childID uuid.UUID, category string, fields map[string]any) {
	f.clock = f.clock.Add(time.Minute)
	payload, _ := json.Marshal(fields)
	f.au.mu.Lock()
	f.au.appended = append(f.au.appended, audit.ChainAppendParams{
		RunID: childID, Timestamp: f.clock, Category: category, Payload: payload,
	})
	f.au.mu.Unlock()
}

// TestVouchClosureLedger_Members: the closure ledger holds the run's and its
// children's reported heads, clean recorded merges, fan-in merges and EARLIER
// verified vouches (lowercased), and excludes a non-clean rebase, an
// unverified vouch and undecodable rows. A nil entry in a read is skipped.
func TestVouchClosureLedger_Members(t *testing.T) {
	const (
		parentHead   = "1111111100000000000000000000000000000000"
		cleanMerge   = "2222222200000000000000000000000000000000"
		dirtyMerge   = "3333333300000000000000000000000000000000"
		sliceMerge   = "4444444400000000000000000000000000000000"
		fanInMerge   = "5555555500000000000000000000000000000000"
		verified     = "6666666600000000000000000000000000000000"
		unverified   = "7777777700000000000000000000000000000000"
		childHeadMix = "ABCDEF0000000000000000000000000000000000"
		childVouch   = "8888888800000000000000000000000000000000"
	)
	f := newVouchProbeFixture(t, vouchProbeOpts{})
	f.wrapAudit().nilRows = true
	f.seedHead("pull_request_opened", parentHead)
	f.appendRow(CategoryBranchRebased, map[string]any{
		"merge_commit_sha": cleanMerge, "new_head_sha": cleanMerge, "clean_verified": true,
	})
	f.appendRow(CategoryBranchRebased, map[string]any{
		"merge_commit_sha": dirtyMerge, "new_head_sha": "9999999900000000000000000000000000000000", "clean_verified": true,
	})
	f.appendRow(lineageIntegrationLedgerCategory, map[string]any{"integration_commit_shas": []string{sliceMerge}})
	f.appendRow(lineageIntegrationCommitCategory, map[string]any{"merge_sha": fanInMerge})
	f.seedVerifiedVouch(parentHead, verified)
	f.appendRow(CategoryOperatorCommitVouched, map[string]any{auditcomplete.VouchedSHAField: unverified, "reason": vouchProbeReason})
	f.au.mu.Lock()
	for _, cat := range []string{"fixup_pushed", CategoryBranchRebased, lineageIntegrationLedgerCategory, lineageIntegrationCommitCategory, CategoryOperatorCommitVouched} {
		f.au.appended = append(f.au.appended, audit.ChainAppendParams{RunID: f.runRow.ID, Category: cat, Payload: json.RawMessage(`not json`)})
	}
	f.au.mu.Unlock()
	child := f.addChild()
	f.appendChildRow(child.ID, "child_pushed", map[string]any{"head_sha": childHeadMix})
	f.appendChildRow(child.ID, CategoryOperatorCommitVouched, map[string]any{
		auditcomplete.VouchedSHAField: childVouch, auditcomplete.VouchPriorHeadSHAField: childHeadMix,
		auditcomplete.VouchHeadAdvanceVerifiedField: true,
	})

	ledger, complete := f.s.vouchClosureLedger(context.Background(), f.runRow)
	if !complete {
		t.Fatal("complete = false, want true")
	}
	for _, sha := range []string{parentHead, cleanMerge, sliceMerge, fanInMerge, verified, strings.ToLower(childHeadMix), childVouch} {
		if _, ok := ledger[sha]; !ok {
			t.Errorf("ledger lacks %s: %v", sha, ledger)
		}
	}
	for _, sha := range []string{dirtyMerge, unverified, childHeadMix} {
		if _, ok := ledger[sha]; ok {
			t.Errorf("ledger carries %s, want it excluded", sha)
		}
	}
	if len(ledger) != 7 {
		t.Errorf("ledger = %v, want exactly 7 members", ledger)
	}
}

// TestVouchClosureLedger_ReadFailure_Incomplete: every read the closure
// ledger performs failing, on the run or on a child, answers complete=false.
func TestVouchClosureLedger_ReadFailure_Incomplete(t *testing.T) {
	tests := []struct {
		name  string
		child bool
		cat   string
	}{
		{"parent reported head", false, "pull_request_opened"},
		{"parent branch_rebased", false, CategoryBranchRebased},
		{"parent slices_integrated", false, lineageIntegrationLedgerCategory},
		{"parent integration_commit_recorded", false, lineageIntegrationCommitCategory},
		{"parent vouches", false, CategoryOperatorCommitVouched},
		{"child reported head", true, "child_pushed"},
		{"child vouches", true, CategoryOperatorCommitVouched},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVouchProbeFixture(t, vouchProbeOpts{})
			child := f.addChild()
			w := f.wrapAudit()
			w.failCat = tt.cat
			if tt.child {
				w.failRun = child.ID
			} else {
				w.failRun = f.runRow.ID
			}
			if _, complete := f.s.vouchClosureLedger(context.Background(), f.runRow); complete {
				t.Errorf("complete = true with the %s read failing, want false", tt.cat)
			}
		})
	}
}

// TestVouchClosureLedger_ChildEnumeration_Incomplete: the children cannot be
// listed (no run repository, or a list error), so the ledger is incomplete.
func TestVouchClosureLedger_ChildEnumeration_Incomplete(t *testing.T) {
	t.Run("no run repository", func(t *testing.T) {
		f := newVouchProbeFixture(t, vouchProbeOpts{})
		f.s.cfg.RunRepo = nil
		if _, complete := f.s.vouchClosureLedger(context.Background(), f.runRow); complete {
			t.Error("complete = true with no run repository, want false")
		}
	})
	t.Run("list error", func(t *testing.T) {
		f := newVouchProbeFixture(t, vouchProbeOpts{})
		f.rr.listRunsErr = errors.New("runs unreadable")
		if _, complete := f.s.vouchClosureLedger(context.Background(), f.runRow); complete {
			t.Error("complete = true with the child list failing, want false")
		}
	})
}

// TestVouchPriorHead_Edges: no audit repository answers no prior; the
// acceptance-seeded walk's seed or walk read failing is returned, never a
// prior from a partial read.
func TestVouchPriorHead_Edges(t *testing.T) {
	t.Run("no audit repository", func(t *testing.T) {
		f := newVouchProbeFixture(t, vouchProbeOpts{})
		f.s.cfg.AuditRepo = nil
		prior, disagree, err := f.s.vouchPriorHead(context.Background(), f.runRow.ID)
		if prior != "" || disagree != "" || err != nil {
			t.Errorf("vouchPriorHead = (%q, %q, %v), want (\"\", \"\", nil)", prior, disagree, err)
		}
	})
	t.Run("acceptance seed read error", func(t *testing.T) {
		f := newVouchProbeFixture(t, vouchProbeOpts{})
		f.seedHead("fixup_pushed", vouchProbeHead)
		f.au.listByCategoryErrCategory = lineageIntegrationCommitCategory
		prior, _, err := f.s.vouchPriorHead(context.Background(), f.runRow.ID)
		if err == nil || prior != "" {
			t.Errorf("vouchPriorHead = (%q, %v), want (\"\", the seed read error)", prior, err)
		}
	})
	t.Run("acceptance walk read error", func(t *testing.T) {
		f := newVouchProbeFixture(t, vouchProbeOpts{})
		f.seedHead("fixup_pushed", vouchProbeHead)
		w := f.wrapAudit()
		w.failCat = CategoryBranchRebased
		w.failNth = 2 // the fix-up walk's read passes; the acceptance-seeded walk's fails
		prior, _, err := f.s.vouchPriorHead(context.Background(), f.runRow.ID)
		if err == nil || prior != "" {
			t.Errorf("vouchPriorHead = (%q, %v), want (\"\", the walk read error)", prior, err)
		}
	})
}

// TestVouchClosureLedger_FieldDecoders: an undecodable payload contributes no
// member from any decoder.
func TestVouchClosureLedger_FieldDecoders(t *testing.T) {
	bad := json.RawMessage(`not json`)
	for name, field := range map[string]func(json.RawMessage) []string{
		"headSHAField":               headSHAField,
		"cleanMergeCommitField":      cleanMergeCommitField,
		"integrationCommitSHAsField": integrationCommitSHAsField,
		"integrationMergeSHAField":   integrationMergeSHAField,
		"verifiedVouchField":         verifiedVouchField,
	} {
		if got := field(bad); got != nil {
			t.Errorf("%s(undecodable) = %v, want nil", name, got)
		}
	}
}
