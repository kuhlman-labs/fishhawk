package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- #5217 gap 3: the acceptance verdict republishes audit_complete AT the live head ---
//
// The strand these tests reproduce: an operator vouches the run's live PR head
// (vouchedSHA, an operator commit on top of the agent's agentHeadSHA) while the
// acceptance stage is still RUNNING. The vouch re-posts fishhawk_audit_complete
// at vouchedSHA, and the recompute there is pending (stage_not_terminal). The
// acceptance trace upload then settles the stage, and the verdict ships. Every
// unforced publish resolves its head through findHeadSHA, which reads only the
// head-report categories, so before #5217 nothing re-posted at vouchedSHA and
// the required check stayed in_progress there until merge_run's forced
// pre-dispatch republish.
//
// The fixture is composed here (approval condition K4): an autoDriveRepo run
// with a succeeded implement stage and a running acceptance stage, a
// SigningRepo for the ship, the auditcheckpublisher over a recording
// CheckRunCreator, and an httptest GitHub serving the live PR head. The
// recorded head (agentHeadSHA) differs from the live head (vouchedSHA) and the
// default findHeadSHA head stays resolvable, so each counterfactual changes
// which check runs are created.

// chainStampedAudit wraps auditFake so ListForRun returns a hash-linked chain.
// auditFake stores no entry hashes, so rule 4 of auditcomplete.ComputeResult
// (verifyChain) would otherwise report chain_invalid on every recompute and the
// fixture could never reach a pass. Only ListForRun changes; every other read
// and write is the embedded auditFake's.
type chainStampedAudit struct{ *auditFake }

func (c *chainStampedAudit) ListForRun(ctx context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	entries, err := c.auditFake.ListForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]*audit.Entry, 0, len(entries))
	var prev *string
	for _, e := range entries {
		cp := *e
		cp.PrevHash = prev
		h, herr := audit.ComputeEntryHash(audit.HashInputs{
			RunID: cp.RunID, StageID: cp.StageID, Timestamp: cp.Timestamp,
			Category: cp.Category, ActorKind: cp.ActorKind, ActorSubject: cp.ActorSubject,
			Payload: cp.Payload, PrevHash: prev,
		})
		if herr != nil {
			return nil, herr
		}
		cp.EntryHash = h
		hash := h
		prev = &hash
		out = append(out, &cp)
	}
	return out, nil
}

// republishPRForge is an httptest GitHub serving GET pulls/{n} whose head and
// status a test can switch mid-test: the vouch must read the live head, and
// the ship's read may then be made to fail.
type republishPRForge struct {
	mu      sync.Mutex
	headSHA string
	status  int
}

func (f *republishPRForge) set(headSHA string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headSHA, f.status = headSHA, status
}

func (f *republishPRForge) client(t *testing.T) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		head, status := f.headSHA, f.status
		f.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"node_id":"PR_x","state":"open","head":{"sha":%q,"ref":"run/branch"},"base":{"ref":"main"}}`, head)
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

type acceptanceRepublishFixture struct {
	s       *Server
	au      *auditFake
	repo    *autoDriveRepo
	creator *vouchCheckCreator
	pr      *republishPRForge
	runID   uuid.UUID
	accID   uuid.UUID
	priv    ed25519.PrivateKey
}

func newAcceptanceRepublishFixture(t *testing.T) *acceptanceRepublishFixture {
	t.Helper()
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	au := newAuditFake()
	chain := &chainStampedAudit{auditFake: au}
	ar := newFakeArtifactRepo()
	sf := newSigningFake()
	pr := &republishPRForge{headSHA: vouchedSHA}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		AuditRepo:    chain,
		ArtifactRepo: ar,
		SigningRepo:  sf,
		GitHub:       pr.client(t),
	})

	runID, implID, accID := uuid.New(), uuid.New(), uuid.New()
	prURL := "https://github.com/x/y/pull/7"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning, InstallationID: instID(99), PullRequestURL: &prURL}
	impl := &run.Stage{ID: implID, RunID: runID, Type: run.StageTypeImplement, State: run.StageStateSucceeded}
	acc := &run.Stage{ID: accID, RunID: runID, Type: run.StageTypeAcceptance, State: run.StageStateRunning}
	repo.mu.Lock()
	repo.runs[runID] = runRow
	repo.stagesByRun[runID] = []*run.Stage{impl, acc}
	repo.mu.Unlock()

	body, _ := json.Marshal(map[string]any{"head_sha": agentHeadSHA, "pr_number": 7})
	if _, err := ar.Create(context.Background(), artifact.CreateParams{
		StageID: implID, Kind: artifact.KindPullRequest, Content: body,
	}); err != nil {
		t.Fatalf("seed pull_request artifact: %v", err)
	}
	// The recorded head (pull_request_opened at agentHeadSHA) at-or-before the
	// acceptance dispatch anchor, so the verdict binds a validated head and
	// findHeadSHA resolves agentHeadSHA. The implement stage shipped its trace.
	seedHeadEntry(au, runID, nil, "pull_request_opened", 1, map[string]any{"head_sha": agentHeadSHA, "pr_number": 7})
	seedHeadEntry(au, runID, &accID, CategoryAcceptanceDispatched, 2, map[string]any{"stage_id": accID.String()})
	seedHeadEntry(au, runID, &implID, "trace_uploaded", 3, map[string]any{"variant": "raw"})
	seedHeadEntry(au, runID, &implID, "trace_uploaded", 4, map[string]any{"variant": "redacted"})

	creator := &vouchCheckCreator{}
	pub := auditcheckpublisher.New(auditcheckpublisher.Deps{
		GitHub:      creator,
		Runs:        repo,
		Artifacts:   ar,
		Audit:       chain,
		ExternalURL: "https://app.fishhawk.example.com",
	})
	if pub == nil {
		t.Fatal("publisher nil")
	}
	s.auditCheckPublisher = pub
	priv, _ := sf.issue(t, runID)
	return &acceptanceRepublishFixture{s: s, au: au, repo: repo, creator: creator, pr: pr, runID: runID, accID: accID, priv: priv}
}

// vouchWhileAcceptanceRunning reproduces the strand's precondition BY
// CONSTRUCTION through the real vouch handler: the vouch re-posts at the
// vouched live head, and the acceptance stage is still running, so the check
// there is in_progress with stage_not_terminal.
func (f *acceptanceRepublishFixture) vouchWhileAcceptanceRunning(t *testing.T) {
	t.Helper()
	w := postVouchCommit(t, f.s, f.runID,
		vouchCommitRequest{SHA: vouchedSHA, Reason: "operator remediation commit on the run branch"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("vouch status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	calls := vouchCreatorCalls(f.creator)
	if len(calls) != 1 {
		t.Fatalf("check runs after the vouch = %d, want 1 (the vouch re-post)", len(calls))
	}
	c := calls[0]
	if c.HeadSHA != vouchedSHA || c.Status != forge.CheckRunStatusInProgress ||
		!strings.Contains(c.OutputText, string(auditcomplete.MissingStageNotTerminal)) {
		t.Fatalf("precondition: the vouch re-post must be in_progress at %s naming %s; got head=%s status=%s text=%q",
			vouchedSHA, auditcomplete.MissingStageNotTerminal, c.HeadSHA, c.Status, c.OutputText)
	}
}

// settleAcceptance stands in for the runner's acceptance trace upload, which
// settles the stage BEFORE the verdict ships (runner main.go; see
// mcpserver/acceptance_verdict_wait.go).
func (f *acceptanceRepublishFixture) settleAcceptance(t *testing.T) {
	t.Helper()
	f.repo.mu.Lock()
	for _, st := range f.repo.stagesByRun[f.runID] {
		if st.ID == f.accID {
			st.State = run.StageStateSucceeded
		}
	}
	f.repo.mu.Unlock()
	seedHeadEntry(f.au, f.runID, &f.accID, "trace_uploaded", 5, map[string]any{"variant": "raw"})
	seedHeadEntry(f.au, f.runID, &f.accID, "trace_uploaded", 6, map[string]any{"variant": "redacted"})
}

func (f *acceptanceRepublishFixture) ship(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return shipAcceptanceRequest(t, f.s, f.runID, f.accID, f.priv, body, "")
}

// callsSince returns the check runs created after the first n.
func (f *acceptanceRepublishFixture) callsSince(n int) []forge.CreateCheckRunParams {
	calls := vouchCreatorCalls(f.creator)
	if n > len(calls) {
		return nil
	}
	return calls[n:]
}

// TestAcceptanceVerdict_RepublishesAuditCheckAtVouchedLiveHead is test (a), the
// end-to-end #5217 gap-3 reproduction: HTTP ship handler → audit →
// auditcomplete.ComputeResult → auditcheckpublisher → forge CreateCheckRun fake.
// After a vouch left the check in_progress at the vouched head, the verdict
// ship must post a COMPLETED success check AT vouchedSHA with no
// stage_not_terminal.
//
// Counterfactuals (run by hand): deleting the call site in handleShipAcceptance
// leaves no check created by the ship (RED on the count); replacing the
// verdictHead override with "" lands the check at agentHeadSHA (RED on the
// head).
func TestAcceptanceVerdict_RepublishesAuditCheckAtVouchedLiveHead(t *testing.T) {
	f := newAcceptanceRepublishFixture(t)
	f.vouchWhileAcceptanceRunning(t)
	f.settleAcceptance(t)
	before := len(vouchCreatorCalls(f.creator))

	w := f.ship(t, validAcceptanceBytes(t))
	if w.Code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	calls := f.callsSince(before)
	if len(calls) != 1 {
		t.Fatalf("check runs created by the verdict ship = %d, want 1 (the forced live-head republish)", len(calls))
	}
	c := calls[0]
	if c.HeadSHA != vouchedSHA {
		t.Errorf("republished check head_sha = %q, want the vouched live head %q (not the recorded head %q)",
			c.HeadSHA, vouchedSHA, agentHeadSHA)
	}
	if c.Status != forge.CheckRunStatusCompleted || c.Conclusion != forge.CheckRunConclusionSuccess {
		t.Errorf("republished check = status %q conclusion %q, want completed/success; text=%q", c.Status, c.Conclusion, c.OutputText)
	}
	if strings.Contains(c.OutputText, string(auditcomplete.MissingStageNotTerminal)) {
		t.Errorf("republished check still names %s:\n%s", auditcomplete.MissingStageNotTerminal, c.OutputText)
	}
}

// TestAcceptanceVerdict_UnresolvableLiveHead_SkipsRepublish is test (b): when
// the live PR head cannot be read at ship time the republish is SKIPPED, with
// zero check runs created, rather than falling back to the findHeadSHA head.
// The ship still answers 201.
//
// Counterfactual (run by hand): neutralising `if headErr != nil {` lets the
// empty head through, and the default-head publish creates a check at
// agentHeadSHA (RED on the count).
func TestAcceptanceVerdict_UnresolvableLiveHead_SkipsRepublish(t *testing.T) {
	f := newAcceptanceRepublishFixture(t)
	f.vouchWhileAcceptanceRunning(t)
	f.settleAcceptance(t)
	before := len(vouchCreatorCalls(f.creator))
	f.pr.set("", http.StatusNotFound)

	w := f.ship(t, validAcceptanceBytes(t))
	if w.Code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201 (an unresolvable head never fails the ship):\n%s", w.Code, w.Body.String())
	}
	if calls := f.callsSince(before); len(calls) != 0 {
		t.Fatalf("check runs created with an unresolvable live head = %d, want 0 (fail closed); first at head %q",
			len(calls), calls[0].HeadSHA)
	}
}

// TestAcceptanceVerdict_RepublishFailure_Still201 is test (c): a forge publish
// failure is best-effort. The ship answers 201 with the verdict recorded, and
// the attempt was made at the live head.
func TestAcceptanceVerdict_RepublishFailure_Still201(t *testing.T) {
	f := newAcceptanceRepublishFixture(t)
	f.vouchWhileAcceptanceRunning(t)
	f.settleAcceptance(t)
	before := len(vouchCreatorCalls(f.creator))
	f.creator.mu.Lock()
	f.creator.err = errBoom
	f.creator.mu.Unlock()

	w := f.ship(t, validAcceptanceBytes(t))
	if w.Code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201 (a publish failure must not fail the ship):\n%s", w.Code, w.Body.String())
	}
	if n := countByCategory(f.au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Errorf("acceptance_outcome_recorded entries = %d, want 1", n)
	}
	calls := f.callsSince(before)
	if len(calls) != 1 || calls[0].HeadSHA != vouchedSHA {
		t.Fatalf("publish attempts = %+v, want exactly one at %s", calls, vouchedSHA)
	}
}

// TestAcceptanceVerdict_TrueReplay_DoesNotRepublish is test (d): an identical
// re-delivery whose acceptance_outcome_recorded row ALREADY exists is a true
// replay. It answers 200 idempotent and creates no check run. The publish is
// forced, so a replay that republished would always create one.
//
// Counterfactual (run by hand): dropping the `if healed` guard on the replay
// republish makes the replay create a check (RED on the count).
func TestAcceptanceVerdict_TrueReplay_DoesNotRepublish(t *testing.T) {
	f := newAcceptanceRepublishFixture(t)
	f.vouchWhileAcceptanceRunning(t)
	f.settleAcceptance(t)
	body := validAcceptanceBytes(t)

	if w := f.ship(t, body); w.Code != http.StatusCreated {
		t.Fatalf("first ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	before := len(vouchCreatorCalls(f.creator))
	w := f.ship(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || !resp.Idempotent {
		t.Fatalf("replay must answer idempotent:true; err=%v body=%s", err, w.Body.String())
	}
	if calls := f.callsSince(before); len(calls) != 0 {
		t.Fatalf("check runs created by a true replay = %d, want 0", len(calls))
	}
}

// TestAcceptanceVerdict_ReplayHeal_Republishes is approval condition K2: the
// first ship persisted the artifact but its acceptance_outcome_recorded append
// failed (500, before the republish ran). The identical retry HEALS the row on
// the idempotent path, so it must republish at the live head then.
//
// Counterfactual (run by hand): deleting the heal-arm republish call leaves no
// check created by the healing retry (RED on the count).
func TestAcceptanceVerdict_ReplayHeal_Republishes(t *testing.T) {
	f := newAcceptanceRepublishFixture(t)
	f.vouchWhileAcceptanceRunning(t)
	f.settleAcceptance(t)
	body := validAcceptanceBytes(t)
	before := len(vouchCreatorCalls(f.creator))

	f.au.appendErrCategory = CategoryAcceptanceOutcomeRecorded
	if w := f.ship(t, body); w.Code != http.StatusInternalServerError {
		t.Fatalf("partial-write ship status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if calls := f.callsSince(before); len(calls) != 0 {
		t.Fatalf("check runs after the failed ship = %d, want 0", len(calls))
	}
	f.au.appendErrCategory = ""

	w := f.ship(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("healing retry status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if n := countByCategory(f.au, CategoryAcceptanceOutcomeRecorded); n != 1 {
		t.Fatalf("acceptance_outcome_recorded entries after the heal = %d, want 1", n)
	}
	calls := f.callsSince(before)
	if len(calls) != 1 {
		t.Fatalf("check runs created by the healing retry = %d, want 1", len(calls))
	}
	if c := calls[0]; c.HeadSHA != vouchedSHA || c.Conclusion != forge.CheckRunConclusionSuccess {
		t.Errorf("healed republish = head %q conclusion %q, want %s/success", c.HeadSHA, c.Conclusion, vouchedSHA)
	}
}

// TestAcceptanceVerdict_FailedVerdictReopen_PostsPendingNotSuccess is approval
// condition K6: a fresh FAILED verdict whose triage re-opens the acceptance
// stage (class 2, a skipped criterion with no failure) republishes at the live
// head with PENDING, never success, because the republish runs after triage.
func TestAcceptanceVerdict_FailedVerdictReopen_PostsPendingNotSuccess(t *testing.T) {
	f := newAcceptanceRepublishFixture(t)
	f.vouchWhileAcceptanceRunning(t)
	f.settleAcceptance(t)
	before := len(vouchCreatorCalls(f.creator))

	body := failedAcceptanceBytes(t, "assertion_fail", []acceptanceCriterionResult{
		{ID: "ac-create", Result: "passed"},
		{ID: "ac-list", Result: "skipped"},
	})
	w := f.ship(t, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("ship status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	st, err := f.repo.GetStage(context.Background(), f.accID)
	if err != nil {
		t.Fatalf("read acceptance stage: %v", err)
	}
	if st.State != run.StageStatePending {
		t.Fatalf("acceptance stage = %q, want pending (triage re-opened it)", st.State)
	}
	calls := f.callsSince(before)
	if len(calls) != 1 {
		t.Fatalf("check runs created by the verdict ship = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.HeadSHA != vouchedSHA {
		t.Errorf("republished check head_sha = %q, want %q", c.HeadSHA, vouchedSHA)
	}
	if c.Status != forge.CheckRunStatusInProgress || c.Conclusion == forge.CheckRunConclusionSuccess {
		t.Errorf("republished check = status %q conclusion %q, want in_progress and never success", c.Status, c.Conclusion)
	}
	if !strings.Contains(c.OutputText, string(auditcomplete.MissingStageNotTerminal)) {
		t.Errorf("pending check must name the re-opened stage (%s):\n%s", auditcomplete.MissingStageNotTerminal, c.OutputText)
	}
}

// TestAcceptanceVerdict_RunReadError_SkipsRepublish pins the run-read guard:
// an unreadable run row creates no check run and does not panic. The guard is
// MASKED by resolveVouchPublishHead's own nil-run rung (removing the early
// return still skips, run by hand), so this pins the observable outcome rather
// than serving as the guard's counterfactual vehicle.
func TestAcceptanceVerdict_RunReadError_SkipsRepublish(t *testing.T) {
	f := newAcceptanceRepublishFixture(t)
	before := len(vouchCreatorCalls(f.creator))
	f.s.republishAuditCheckOnAcceptanceVerdict(context.Background(), uuid.New())
	if calls := f.callsSince(before); len(calls) != 0 {
		t.Fatalf("check runs created for an unreadable run = %d, want 0", len(calls))
	}
}
