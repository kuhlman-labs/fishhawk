package server

import (
	"bytes"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// postVouchCommit posts a vouch-commit request with the given identity
// mutator and JSON body.
func postVouchCommit(t *testing.T, s *Server, runID uuid.UUID, body vouchCommitRequest,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	return postVouchCommitRaw(t, s, runID, raw, withID)
}

// postVouchCommitRaw posts an arbitrary (possibly malformed) body so the
// decode-error path can be exercised.
func postVouchCommitRaw(t *testing.T, s *Server, runID uuid.UUID, raw []byte,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/vouch-commit", bytes.NewReader(raw))
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleVouchCommit(w, withID(req))
	return w
}

// withVouchOperator injects an operator fhk_ token identity carrying
// write:stages — the only credential vouch accepts. The shared withAuth
// helper is a scope-less cookie session, which vouch rejects (the binding
// approval condition enforces write:stages UNCONDITIONALLY, with no
// cookie-session bypass), so vouch tests use this scoped injector instead.
func withVouchOperator(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "github:ops", TokenID: "tok-op", Scopes: []string{"write:stages"},
	}))
}

// seedVouchServer wires a server with a run + audit/run repos for the
// vouch handler, returning the server, audit fake, and the run ID.
func seedVouchServer(t *testing.T) (*Server, *auditFake, uuid.UUID) {
	t.Helper()
	runID := uuid.New()
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning, InstallationID: instID(99)}
	stage := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement}
	s, _, au, _ := newLineageServer(t, nil, runRow, stage)
	return s, au, runID
}

// vouchAudit finds the operator_commit_vouched audit entry, if any.
func vouchAudit(au *auditFake) *audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	for i := range au.appended {
		if au.appended[i].Category == CategoryOperatorCommitVouched {
			return &au.appended[i]
		}
	}
	return nil
}

const vouchedSHA = "abc1230000000000000000000000000000000000"

func TestVouchCommit_HappyPath(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: vouchedSHA, Reason: "sync-schemas remediation commit"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	var resp vouchCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.VouchedSHA != vouchedSHA {
		t.Errorf("vouched_sha = %q, want %q", resp.VouchedSHA, vouchedSHA)
	}
	if resp.RunID != runID.String() {
		t.Errorf("run_id = %q, want %q", resp.RunID, runID)
	}

	a := vouchAudit(au)
	if a == nil {
		t.Fatal("no operator_commit_vouched audit entry written")
	}
	if a.ActorKind == nil || *a.ActorKind != audit.ActorUser {
		t.Errorf("actor kind = %v, want user", a.ActorKind)
	}
	var payload struct {
		RunID      string `json:"run_id"`
		VouchedSHA string `json:"vouched_sha"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(a.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.VouchedSHA != vouchedSHA {
		t.Errorf("payload vouched_sha = %q, want %q", payload.VouchedSHA, vouchedSHA)
	}
	if payload.Reason != "sync-schemas remediation commit" {
		t.Errorf("payload reason = %q", payload.Reason)
	}
}

func TestVouchCommit_MissingScope(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	withScopeless := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", TokenID: "tok-x", Scopes: []string{"read:runs"},
		}))
	}
	w := postVouchCommit(t, s, runID, vouchCommitRequest{SHA: vouchedSHA, Reason: "x"}, withScopeless)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("insufficient_scope")) {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite missing scope")
	}
}

// TestVouchCommit_RunBoundTokenForbidden is the BINDING amendment guard: a
// run-bound mcp:run token is rejected outright (403 run_token_forbidden),
// even for its OWN run — vouching git lineage is an operator action, and an
// agent self-declaring lineage for a commit on its own branch would defeat
// the ADR-035 sole-writer invariant.
func TestVouchCommit_RunBoundTokenForbidden(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	// The run-bound token's subject IS this run's id (its own run), and it
	// even carries write:stages — it must STILL be rejected.
	withOwnRunToken := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "mcp:run:" + runID.String(),
			TokenID: "tok-agent",
			Scopes:  []string{"mcp:read", "write:stages"},
		}))
	}
	w := postVouchCommit(t, s, runID, vouchCommitRequest{SHA: vouchedSHA, Reason: "x"}, withOwnRunToken)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("run_token_forbidden")) {
		t.Errorf("body missing run_token_forbidden: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite run-bound token rejection")
	}
}

func TestVouchCommit_EmptySHA(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	w := postVouchCommit(t, s, runID, vouchCommitRequest{SHA: "   ", Reason: "x"}, withVouchOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite empty sha")
	}
}

func TestVouchCommit_EmptyReason(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	w := postVouchCommit(t, s, runID, vouchCommitRequest{SHA: vouchedSHA, Reason: "  "}, withVouchOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite empty reason")
	}
}

func TestVouchCommit_RunNotFound(t *testing.T) {
	s, au, _ := seedVouchServer(t)

	unknown := uuid.New()
	w := postVouchCommit(t, s, unknown, vouchCommitRequest{SHA: vouchedSHA, Reason: "x"}, withVouchOperator)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("run_not_found")) {
		t.Errorf("body missing run_not_found: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written for an unknown run")
	}
}

// TestVouchCommit_EmptyTokenIDNoScope locks the high-severity fix: an
// authenticated identity with an EMPTY TokenID (the cookie-session shape)
// and no write:stages scope is rejected 403, NOT waved past the scope gate.
// Vouch enforces write:stages unconditionally — there is no cookie-session
// bypass, unlike the sibling reset-branch/waive/retry handlers.
func TestVouchCommit_EmptyTokenIDNoScope(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	withSessionNoScope := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", UserID: "u-1", SessionID: "s-1", // empty TokenID, empty Scopes
		}))
	}
	w := postVouchCommit(t, s, runID, vouchCommitRequest{SHA: vouchedSHA, Reason: "x"}, withSessionNoScope)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("insufficient_scope")) {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite empty-TokenID identity without scope")
	}
}

// TestVouchCommit_Unconfigured covers the 503 branch: nil RunRepo/AuditRepo.
func TestVouchCommit_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"}) // no RunRepo / AuditRepo

	w := postVouchCommit(t, s, uuid.New(), vouchCommitRequest{SHA: vouchedSHA, Reason: "x"}, withVouchOperator)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("vouch_unconfigured")) {
		t.Errorf("body missing vouch_unconfigured: %s", w.Body.String())
	}
}

// TestVouchCommit_MalformedBody covers the decode-error branch: a body that
// is not valid JSON returns 400 validation_failed and writes no audit entry.
func TestVouchCommit_MalformedBody(t *testing.T) {
	s, au, runID := seedVouchServer(t)

	w := postVouchCommitRaw(t, s, runID, []byte("{not json"), withVouchOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if vouchAudit(au) != nil {
		t.Error("audit written despite malformed body")
	}
}

// --- E64.14 / #3109: vouch triggers the audit-complete re-post ---

// vouchCheckCreator is a fake auditcheckpublisher.CheckRunCreator recording the
// params of each publish, with an injectable error for the failure path.
type vouchCheckCreator struct {
	mu    sync.Mutex
	calls []forge.CreateCheckRunParams
	err   error
}

func (f *vouchCheckCreator) CreateCheckRun(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, p forge.CreateCheckRunParams) (*forge.CreateCheckRunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, p)
	if f.err != nil {
		return nil, f.err
	}
	return &forge.CreateCheckRunResult{ID: 1}, nil
}

// vouchPRStub configures the httptest forge the vouch republish fixture reads
// the run's LIVE pull-request head from (E64.26 / #3129). Zero value serves a
// 200 whose head sha is the fixture's liveHeadSHA.
type vouchPRStub struct {
	// headSHA is the head sha the PR read returns. The fixture defaults it to
	// the vouched sha so a MATCH is the fixture's happy path.
	headSHA string
	// status, when non-zero and not 200, is returned instead of a PR body — the
	// GetPullRequest-error rung of resolveVouchPublishHead.
	status int
}

// vouchRepublishOpts tunes newVouchRepublishServer so each fail-closed rung of
// resolveVouchPublishHead is reachable BY CONSTRUCTION, never by calling the
// control inside the test's own setup.
type vouchRepublishOpts struct {
	pr vouchPRStub
	// noInstallation seeds the run row with a nil InstallationID.
	noInstallation bool
	// repoOverride, when non-empty, replaces the parseable "x/y" run repo.
	repoOverride string
	// noPRURL seeds the run row with no pull_request_url.
	noPRURL bool
	// noGitHubClient leaves Config.GitHub nil.
	noGitHubClient bool
	// noPublisher leaves s.auditCheckPublisher nil (the dev/CLI posture).
	noPublisher bool
}

// newVouchPRGitHubClient builds a *githubclient.Client pointed at an httptest
// mux serving GET /repos/{owner}/{repo}/pulls/{number} — the same construction
// shape newRebaseGitHubClient uses, because Config.GitHub is a CONCRETE client
// and the forge must therefore be faked at the HTTP layer.
func newVouchPRGitHubClient(t *testing.T, stub vouchPRStub) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}",
		func(w http.ResponseWriter, _ *http.Request) {
			if stub.status != 0 && stub.status != http.StatusOK {
				w.WriteHeader(stub.status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"node_id":"PR_x","state":"open","head":{"sha":%q,"ref":"run/branch"},"base":{"ref":"main"}}`,
				stub.headSHA)
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

// newVouchRepublishServer wires a vouch server over an autoDriveRepo (which
// supports GetRun + ListStagesForRun, so auditcomplete.ComputeResult runs) with
// an artifact + audit fake and an auditcheckpublisher over the given fake
// creator. It seeds a running run with an implement stage so the recompute has
// a chain to derive, plus an InstallationID + PullRequestURL and a real
// *githubclient.Client serving the PR read, so the E64.26 / #3129 publish bound
// resolves a live head. Returns the server, the audit fake, and the run id.
func newVouchRepublishServer(t *testing.T, creator *vouchCheckCreator) (*Server, *auditFake, uuid.UUID) {
	t.Helper()
	return newVouchRepublishServerOpts(t, creator, vouchRepublishOpts{})
}

// newVouchRepublishServerOpts is newVouchRepublishServer with the knobs each
// publish-bound arm needs.
func newVouchRepublishServerOpts(t *testing.T, creator *vouchCheckCreator, opts vouchRepublishOpts) (*Server, *auditFake, uuid.UUID) {
	t.Helper()
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	au := newAuditFake()
	ar := newFakeArtifactRepo()
	stub := opts.pr
	if stub.headSHA == "" && stub.status == 0 {
		// DEFAULT: the live head IS the vouched sha, so the fixture's happy
		// path is a MATCH and the pre-existing republish tests keep passing.
		stub.headSHA = vouchedSHA
	}
	cfg := Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    au,
		ArtifactRepo: ar,
	}
	if !opts.noGitHubClient {
		cfg.GitHub = newVouchPRGitHubClient(t, stub)
	}
	s := New(cfg)
	runID := uuid.New()
	implID := uuid.New()
	repoSlug := "x/y"
	if opts.repoOverride != "" {
		repoSlug = opts.repoOverride
	}
	runRow := &run.Run{ID: runID, Repo: repoSlug, State: run.StateRunning, InstallationID: instID(99)}
	if opts.noInstallation {
		runRow.InstallationID = nil
	}
	if !opts.noPRURL {
		prURL := "https://github.com/x/y/pull/7"
		runRow.PullRequestURL = &prURL
	}
	stage := &run.Stage{ID: implID, RunID: runID, Type: run.StageTypeImplement}
	repo.mu.Lock()
	repo.runs[runID] = runRow
	repo.stagesByRun[runID] = []*run.Stage{stage}
	repo.mu.Unlock()
	if opts.noPublisher {
		return s, au, runID
	}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      creator,
		Runs:        repo,
		Artifacts:   ar,
		Audit:       au,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if pub == nil {
		t.Fatal("publisher nil")
	}
	s.auditCheckPublisher = pub
	return s, au, runID
}

// TestVouchCommit_RepublishesAuditCheckAtVouchedHead pins the re-post trigger
// (E64.14 / #3109): a vouch publishes the fishhawk_audit_complete Check Run AT
// the VOUCHED sha (asserted on the recorded head_sha, not merely that a publish
// happened), and reports audit_check_republished:true with no warning.
// Counterfactual c4: deleting the republish call in handleVouchCommit turns this
// RED (zero Check Run creations recorded).
func TestVouchCommit_RepublishesAuditCheckAtVouchedHead(t *testing.T) {
	creator := &vouchCheckCreator{}
	s, _, runID := newVouchRepublishServer(t, creator)

	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: vouchedSHA, Reason: "sync-schemas remediation commit"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp vouchCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.AuditCheckRepublished {
		t.Error("audit_check_republished = false, want true")
	}
	if resp.AuditCheckRepublishWarning != "" {
		t.Errorf("unexpected republish warning: %q", resp.AuditCheckRepublishWarning)
	}
	creator.mu.Lock()
	defer creator.mu.Unlock()
	if len(creator.calls) != 1 {
		t.Fatalf("check run creations = %d, want 1 (the vouch re-posts)", len(creator.calls))
	}
	if got := creator.calls[0].HeadSHA; got != vouchedSHA {
		t.Errorf("check run head_sha = %q, want the vouched sha %q", got, vouchedSHA)
	}
}

// TestVouchCommit_RepublishFailure_Still200WithWarning pins binding condition 1b:
// a re-post FAILURE does not fail the vouch (still 200, vouch recorded) but is
// VISIBLE — audit_check_republished:false with a non-empty warning naming the
// missing check and the re-invoke retry, rather than being swallowed behind a
// clean success.
func TestVouchCommit_RepublishFailure_Still200WithWarning(t *testing.T) {
	creator := &vouchCheckCreator{err: errBoom}
	s, au, runID := newVouchRepublishServer(t, creator)

	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: vouchedSHA, Reason: "sync-schemas remediation commit"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a publish failure must not fail the vouch):\n%s", w.Code, w.Body.String())
	}
	var resp vouchCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.AuditCheckRepublished {
		t.Error("audit_check_republished = true on a publish failure, want false")
	}
	if resp.AuditCheckRepublishWarning == "" {
		t.Error("republish warning empty; the failure must be VISIBLE (binding condition 1b)")
	}
	if !strings.Contains(resp.AuditCheckRepublishWarning, "fishhawk_vouch_commit") {
		t.Errorf("warning must name the re-invoke retry path: %q", resp.AuditCheckRepublishWarning)
	}
	// The vouch itself is still recorded despite the re-post failure.
	if vouchAudit(au) == nil {
		t.Error("operator_commit_vouched entry not recorded; the vouch must succeed independently of the re-post")
	}
}

// vouchAuditCount returns how many operator_commit_vouched entries were appended.
func vouchAuditCount(au *auditFake) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for i := range au.appended {
		if au.appended[i].Category == CategoryOperatorCommitVouched {
			n++
		}
	}
	return n
}

// TestVouchCommit_RepeatVouchRetriesRepublish pins binding condition 1c — the
// sanctioned idempotent retry — end to end through handleVouchCommit, which
// diff-only review could not confirm: re-invoking fishhawk_vouch_commit on an
// ALREADY-VOUCHED sha REACHES a second republish attempt (the handler neither
// refuses nor dedups a repeat vouch short of the re-post). The first vouch's
// re-post fails (200, republished:false, warning); the second vouch of the SAME
// sha succeeds (200, republished:true, no warning) and re-fires the Check Run at
// the vouched head. Without a reachable second attempt the documented first-class
// recovery would be unreachable behind a doc claim.
func TestVouchCommit_RepeatVouchRetriesRepublish(t *testing.T) {
	creator := &vouchCheckCreator{err: errBoom}
	s, au, runID := newVouchRepublishServer(t, creator)

	// First vouch: the re-post fails; the vouch still succeeds with a warning.
	w1 := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: vouchedSHA, Reason: "sync-schemas remediation commit"}, withVouchOperator)
	if w1.Code != http.StatusOK {
		t.Fatalf("first vouch status = %d, want 200:\n%s", w1.Code, w1.Body.String())
	}
	var resp1 vouchCommitResponse
	if err := json.Unmarshal(w1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("unmarshal first: %v", err)
	}
	if resp1.AuditCheckRepublished {
		t.Fatalf("first vouch audit_check_republished = true, want false (re-post failed):\n%s", w1.Body.String())
	}

	// The publisher records only SUCCESSES, so the failed first attempt left the
	// dedup cache empty. The seam recovers; re-invoke the vouch on the SAME sha.
	creator.mu.Lock()
	creator.err = nil
	callsAfterFirst := len(creator.calls)
	creator.mu.Unlock()

	w2 := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: vouchedSHA, Reason: "retry the dropped audit-check re-post"}, withVouchOperator)
	if w2.Code != http.StatusOK {
		t.Fatalf("second vouch status = %d, want 200:\n%s", w2.Code, w2.Body.String())
	}
	var resp2 vouchCommitResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal second: %v", err)
	}
	if !resp2.AuditCheckRepublished {
		t.Errorf("second vouch audit_check_republished = false, want true (re-vouch retries the re-post):\n%s", w2.Body.String())
	}
	if resp2.AuditCheckRepublishWarning != "" {
		t.Errorf("second vouch warning non-empty on success: %q", resp2.AuditCheckRepublishWarning)
	}

	creator.mu.Lock()
	defer creator.mu.Unlock()
	// The repeat vouch REACHED a second republish attempt — not refused/deduped.
	if len(creator.calls) <= callsAfterFirst {
		t.Fatalf("check run creations did not increase on re-vouch: %d then %d (repeat vouch never reached the re-post)",
			callsAfterFirst, len(creator.calls))
	}
	if got := creator.calls[len(creator.calls)-1].HeadSHA; got != vouchedSHA {
		t.Errorf("re-vouch check run head_sha = %q, want the vouched sha %q", got, vouchedSHA)
	}
	// Two operator_commit_vouched entries were recorded: the handler does not
	// refuse a repeat vouch of the same sha (which would make 1c unreachable).
	if n := vouchAuditCount(au); n != 2 {
		t.Errorf("operator_commit_vouched entries = %d, want 2 (repeat vouch is not refused)", n)
	}
}

// --- E64.26 / #3129: the publish is BOUND to the run's own live PR head ---

// unrelatedPRHeadSHA is a 40-hex sha standing in for the head of an UNRELATED
// pull request — the commit the pre-#3129 handler would have stamped this run's
// recomputed check state onto. It is a distinct literal, seeded BY
// CONSTRUCTION, so the mismatch fixture never derives its non-matching state by
// calling the control under test.
const unrelatedPRHeadSHA = "def4560000000000000000000000000000000000"

// vouchCreatorCalls snapshots the publisher-seam call count. Every arm below
// asserts on THIS, not merely on the response flag: a skip that nonetheless
// published would show up here and nowhere else.
func vouchCreatorCalls(c *vouchCheckCreator) []forge.CreateCheckRunParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]forge.CreateCheckRunParams(nil), c.calls...)
}

// TestVouchCommit_PublishesOnlyAtLivePRHead is the MATCH arm (binding condition
// 3): the vouched sha IS the run's live pull-request head, so the
// fishhawk_audit_complete check is re-posted — exactly ONE publisher-seam call,
// recorded at the vouched sha — and the response reports republished:true with
// no warning.
func TestVouchCommit_PublishesOnlyAtLivePRHead(t *testing.T) {
	creator := &vouchCheckCreator{}
	s, au, runID := newVouchRepublishServerOpts(t, creator, vouchRepublishOpts{
		pr: vouchPRStub{headSHA: vouchedSHA},
	})

	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: vouchedSHA, Reason: "sync-schemas remediation commit"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp vouchCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.AuditCheckRepublished {
		t.Error("audit_check_republished = false, want true (the vouched sha IS the live head)")
	}
	if resp.AuditCheckRepublishWarning != "" {
		t.Errorf("unexpected warning on the match arm: %q", resp.AuditCheckRepublishWarning)
	}
	calls := vouchCreatorCalls(creator)
	if len(calls) != 1 {
		t.Fatalf("publisher seam calls = %d, want exactly 1", len(calls))
	}
	if got := calls[0].HeadSHA; got != vouchedSHA {
		t.Errorf("check run head_sha = %q, want the vouched sha %q", got, vouchedSHA)
	}
	if vouchAudit(au) == nil {
		t.Error("operator_commit_vouched entry not recorded")
	}
}

// TestVouchCommit_ShaNotLivePRHead_RecordsButSkipsPublish is the MISMATCH arm
// (binding condition 3) and the CENTRAL #3129 control: an operator names a
// commit that is NOT the run's live pull-request head — the head of an
// unrelated PR — and the handler RECORDS the declaration verbatim (still 200,
// operator_commit_vouched present) while publishing NOTHING. Asserted on the
// PUBLISHER SEAM CALL COUNT: zero. The warning names BOTH shas.
//
// COUNTERFACTUAL: deleting the sha-equality comparison in handleVouchCommit's
// publish bound turns this RED with 1 seam call where 0 is expected.
func TestVouchCommit_ShaNotLivePRHead_RecordsButSkipsPublish(t *testing.T) {
	creator := &vouchCheckCreator{}
	// The live head is an UNRELATED commit, seeded by construction.
	s, au, runID := newVouchRepublishServerOpts(t, creator, vouchRepublishOpts{
		pr: vouchPRStub{headSHA: unrelatedPRHeadSHA},
	})

	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: vouchedSHA, Reason: "stamp the check on someone else's PR"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the record is verbatim for ANY sha):\n%s", w.Code, w.Body.String())
	}
	var resp vouchCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if calls := vouchCreatorCalls(creator); len(calls) != 0 {
		t.Fatalf("publisher seam calls = %d, want 0 (the check must NEVER be stamped off the run's own head): %+v",
			len(calls), calls)
	}
	if resp.AuditCheckRepublished {
		t.Error("audit_check_republished = true on a mismatch, want false")
	}
	if !strings.Contains(resp.AuditCheckRepublishWarning, vouchedSHA) {
		t.Errorf("warning must name the VOUCHED sha %q: %q", vouchedSHA, resp.AuditCheckRepublishWarning)
	}
	if !strings.Contains(resp.AuditCheckRepublishWarning, unrelatedPRHeadSHA) {
		t.Errorf("warning must name the LIVE head %q: %q", unrelatedPRHeadSHA, resp.AuditCheckRepublishWarning)
	}
	// The RECORD is untouched by the bound: the declaration is still durable.
	e := vouchAudit(au)
	if e == nil {
		t.Fatal("operator_commit_vouched entry not recorded; the record must stay verbatim for any sha")
	}
	var payload map[string]any
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got, _ := payload[lineageVouchedSHAField].(string); got != vouchedSHA {
		t.Errorf("recorded %s = %q, want the vouched sha %q", lineageVouchedSHAField, got, vouchedSHA)
	}
}

// TestVouchCommit_UnresolvablePRHead_SkipsPublish is the UNRESOLVABLE arm
// (binding condition 3), one subtest per NAMED fail-closed rung of
// resolveVouchPublishHead. Each asserts the same three things: still 200, the
// declaration recorded, and ZERO publisher-seam calls — publishing at an
// unverified sha is refused, never defaulted to.
//
// COUNTERFACTUAL: deleting the head-resolution guard so an unresolvable head
// falls through to the publish turns every subtest RED on the call count.
func TestVouchCommit_UnresolvablePRHead_SkipsPublish(t *testing.T) {
	cases := []struct {
		name    string
		opts    vouchRepublishOpts
		wantMsg string
	}{
		{"no installation", vouchRepublishOpts{noInstallation: true}, "no installation"},
		{"unparseable repo", vouchRepublishOpts{repoOverride: "not-a-slug"}, "unparseable"},
		{"no pull request url", vouchRepublishOpts{noPRURL: true}, "no tracked pull request"},
		{"no github client wired", vouchRepublishOpts{noGitHubClient: true}, "no GitHub client"},
		{"pull request read failed", vouchRepublishOpts{pr: vouchPRStub{status: http.StatusInternalServerError}}, "read live pull request head failed"},
		{"empty head sha", vouchRepublishOpts{pr: vouchPRStub{headSHA: " "}}, "empty head sha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			creator := &vouchCheckCreator{}
			s, au, runID := newVouchRepublishServerOpts(t, creator, tc.opts)

			w := postVouchCommit(t, s, runID,
				vouchCommitRequest{SHA: vouchedSHA, Reason: "remediation commit"}, withVouchOperator)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (an unresolvable head must not fail the vouch):\n%s", w.Code, w.Body.String())
			}
			var resp vouchCommitResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if calls := vouchCreatorCalls(creator); len(calls) != 0 {
				t.Fatalf("publisher seam calls = %d, want 0 (fail closed on an unresolvable head): %+v",
					len(calls), calls)
			}
			if resp.AuditCheckRepublished {
				t.Error("audit_check_republished = true on an unresolvable head, want false")
			}
			if !strings.Contains(resp.AuditCheckRepublishWarning, tc.wantMsg) {
				t.Errorf("warning must name the resolution failure %q: %q", tc.wantMsg, resp.AuditCheckRepublishWarning)
			}
			if vouchAudit(au) == nil {
				t.Error("operator_commit_vouched entry not recorded; the record survives a skipped publish")
			}
		})
	}
}

// TestVouchCommit_HeadEqualityIsCaseInsensitive pins the EqualFold
// normalization: an operator pasting the live head in MIXED CASE names the same
// commit, so the publish lands. A raw == comparison fails this test.
func TestVouchCommit_HeadEqualityIsCaseInsensitive(t *testing.T) {
	creator := &vouchCheckCreator{}
	// The forge serves the canonical lowercase spelling; the operator vouches
	// the SAME commit in upper case.
	s, _, runID := newVouchRepublishServerOpts(t, creator, vouchRepublishOpts{
		pr: vouchPRStub{headSHA: vouchedSHA},
	})
	upper := strings.ToUpper(vouchedSHA)
	if upper == vouchedSHA {
		t.Fatalf("fixture sha %q has no letters to upper-case; the case control would be vacuous", vouchedSHA)
	}

	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: upper, Reason: "same commit, pasted upper-case"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp vouchCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if calls := vouchCreatorCalls(creator); len(calls) != 1 {
		t.Fatalf("publisher seam calls = %d, want 1 (a mixed-case spelling is the SAME commit)", len(calls))
	}
	if !resp.AuditCheckRepublished {
		t.Errorf("audit_check_republished = false on a mixed-case spelling of the live head:\n%s", w.Body.String())
	}
}

// TestVouchCommit_AbbreviatedShaTakesMismatchArm pins the documented,
// deliberate consequence of a FULL-string compare: an abbreviated sha is not
// the live head, so it takes the mismatch arm — recorded, not published, with
// both spellings named so the operator can re-vouch with the full sha.
func TestVouchCommit_AbbreviatedShaTakesMismatchArm(t *testing.T) {
	creator := &vouchCheckCreator{}
	s, au, runID := newVouchRepublishServerOpts(t, creator, vouchRepublishOpts{
		pr: vouchPRStub{headSHA: vouchedSHA},
	})
	short := vouchedSHA[:7]

	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: short, Reason: "abbreviated paste"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp vouchCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if calls := vouchCreatorCalls(creator); len(calls) != 0 {
		t.Fatalf("publisher seam calls = %d, want 0 (an abbreviated sha is not the live head)", len(calls))
	}
	if !strings.Contains(resp.AuditCheckRepublishWarning, vouchedSHA) {
		t.Errorf("warning must name the live head in full so the operator can re-vouch: %q",
			resp.AuditCheckRepublishWarning)
	}
	if vouchAudit(au) == nil {
		t.Error("operator_commit_vouched entry not recorded")
	}
}

// TestVouchCommit_NoPublisherWired_NoWarning pins the arm ORDERING: with no
// publisher wired (the dev/CLI posture) the vouch skips SILENTLY — no publish,
// and critically no warning. Without the nil-publisher arm ordered first, every
// dev-posture vouch would newly emit an unresolvable-head warning about a check
// it was never going to post.
func TestVouchCommit_NoPublisherWired_NoWarning(t *testing.T) {
	creator := &vouchCheckCreator{}
	// noPublisher AND an unresolvable head: if the ordering were dropped, the
	// head-resolution arm would fire and warn.
	s, au, runID := newVouchRepublishServerOpts(t, creator, vouchRepublishOpts{
		noPublisher: true,
		noPRURL:     true,
	})

	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: vouchedSHA, Reason: "dev posture vouch"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp vouchCommitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if calls := vouchCreatorCalls(creator); len(calls) != 0 {
		t.Fatalf("publisher seam calls = %d, want 0 (no publisher is wired)", len(calls))
	}
	if resp.AuditCheckRepublished {
		t.Error("audit_check_republished = true with no publisher wired, want false")
	}
	if resp.AuditCheckRepublishWarning != "" {
		t.Errorf("dev/CLI posture must stay warning-free (arm ordering): %q", resp.AuditCheckRepublishWarning)
	}
	if vouchAudit(au) == nil {
		t.Error("operator_commit_vouched entry not recorded")
	}
}
