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
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/invariantmonitor"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// lineageGitHub is an in-test GitHub stub for the branch-lineage guard:
// it serves GET /pulls/{n} (the PR base ref) and GET /compare/{base...head}
// (the branch's commits) and records what compare base was actually used,
// so assertions can prove the anchor is the PR base ref — never the
// runner-reported base_sha.
type lineageGitHub struct {
	baseRef       string              // base.ref returned by GET /pulls/{n}
	headSHA       string              // head.sha returned by GET /pulls/{n}; "" => "H"
	commitsByBase map[string][]string // compare base -> commit SHAs returned
	prStatus      int                 // 0 => 200; non-2xx exercises GetPullRequest errors
	compareStatus int                 // 0 => 200; non-2xx exercises the fail-open path

	mu              sync.Mutex
	lastCompareBase string
	compareCalled   bool
	prCalled        bool
}

func newLineageGitHubClient(t *testing.T, stub *lineageGitHub) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}",
		func(w http.ResponseWriter, _ *http.Request) {
			stub.mu.Lock()
			stub.prCalled = true
			head := stub.headSHA
			stub.mu.Unlock()
			if stub.prStatus != 0 && stub.prStatus != http.StatusOK {
				w.WriteHeader(stub.prStatus)
				return
			}
			if head == "" {
				head = "H"
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"node_id":"PR_x","state":"open","head":{"sha":%q},"base":{"ref":%q}}`, head, stub.baseRef)
		})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead...}",
		func(w http.ResponseWriter, r *http.Request) {
			basehead := r.PathValue("basehead")
			base := basehead
			if i := strings.Index(basehead, "..."); i >= 0 {
				base = basehead[:i]
			}
			stub.mu.Lock()
			stub.compareCalled = true
			stub.lastCompareBase = base
			stub.mu.Unlock()
			if stub.compareStatus != 0 && stub.compareStatus != http.StatusOK {
				w.WriteHeader(stub.compareStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			var sb strings.Builder
			sb.WriteString(`{"commits":[`)
			for i, sha := range stub.commitsByBase[base] {
				if i > 0 {
					sb.WriteString(",")
				}
				fmt.Fprintf(&sb, `{"sha":%q}`, sha)
			}
			sb.WriteString(`]}`)
			_, _ = w.Write([]byte(sb.String()))
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

// newReverifyGitHubClient builds the lineage GitHub stub with the live PR
// head SHA set, so GetPullRequest returns it for ReverifyBranchLineage's
// compare anchor.
func newReverifyGitHubClient(t *testing.T, stub *lineageGitHub, headSHA string) *githubclient.Client {
	t.Helper()
	stub.headSHA = headSHA
	return newLineageGitHubClient(t, stub)
}

// newLineageServer wires a PR-upload server with GitHub stubbed for the
// lineage guard, seeding the given run + stage.
func newLineageServer(t *testing.T, gh *githubclient.Client, runRow *run.Run, stage *run.Stage) (*Server, *signingFake, *auditFake, *promptRunRepo) {
	t.Helper()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stage.ID] = stage
	rr.getRuns[runRow.ID] = runRow
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
		GitHub:       gh,
	})
	return s, sf, au, rr
}

func instID(v int64) *int64 { return &v }

// foreignViolation finds the invariant_violation audit entry the guard
// emits for a foreign commit, if any.
func foreignViolation(au *auditFake) *audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	for i := range au.appended {
		p := au.appended[i]
		if p.Category != invariantmonitor.CategoryInvariantViolation {
			continue
		}
		var payload struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(p.Payload, &payload) == nil &&
			payload.Kind == invariantmonitor.KindForeignCommitOnBranch {
			return &au.appended[i]
		}
	}
	return nil
}

func transitionedTo(rr *promptRunRepo, to run.StageState) bool {
	for _, c := range rr.transitionStageCalls {
		if c.To == to {
			return true
		}
	}
	return false
}

// TestVerifyBranchLineage_CaseA_Contamination reproduces #797's shape: a
// foreign commit that is the PARENT of the reported head (an ancestor the
// runner-reported base would have hidden) rides the run branch. The guard
// must fail the implement stage category-B, emit a foreign_commit_on_branch
// invariant_violation naming the foreign SHA, and NOT open the review gate.
func TestVerifyBranchLineage_CaseA_Contamination(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	const head = "1111111111111111111111111111111111111111"
	const foreign = "ffffffffffffffffffffffffffffffffffffffff" // parent of head, not in ledger

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {foreign, head}},
	}
	gh := newLineageGitHubClient(t, stub)
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning, InstallationID: instID(99)}
	stage := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateRunning, RequiresApproval: true}
	s, sf, au, rr := newLineageServer(t, gh, runRow, stage)
	priv, _ := sf.issue(t, runID)

	body := mustPRBody(t, head)
	w := shipPRRequest(t, s, runID, stageID, priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	v := foreignViolation(au)
	if v == nil {
		t.Fatal("expected a foreign_commit_on_branch invariant_violation audit entry, got none")
	}
	var payload struct {
		OffendingSHA string `json:"offending_sha"`
	}
	if err := json.Unmarshal(v.Payload, &payload); err != nil {
		t.Fatalf("unmarshal violation payload: %v", err)
	}
	if payload.OffendingSHA != foreign {
		t.Errorf("offending_sha = %q, want %q", payload.OffendingSHA, foreign)
	}
	if !transitionedTo(rr, run.StageStateFailed) {
		t.Error("stage was not failed")
	}
	if transitionedTo(rr, run.StageStateAwaitingApproval) {
		t.Error("stage advanced to the review gate despite a lineage violation")
	}
	// The anchor must be the PR base ref ("main"), NOT the report's base_sha.
	if stub.lastCompareBase != "main" {
		t.Errorf("compare base = %q, want %q (PR base ref, not report base_sha)", stub.lastCompareBase, "main")
	}
}

// TestVerifyBranchLineage_CaseB_CleanPasses is the control: every branch
// commit is a ledger member, so the guard passes and the review gate opens
// with no violation.
func TestVerifyBranchLineage_CaseB_CleanPasses(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	const head = "1111111111111111111111111111111111111111"

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {head}}, // only the run's own commit
	}
	gh := newLineageGitHubClient(t, stub)
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning, InstallationID: instID(99)}
	stage := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateRunning, RequiresApproval: true}
	s, sf, au, rr := newLineageServer(t, gh, runRow, stage)
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, mustPRBody(t, head), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if v := foreignViolation(au); v != nil {
		t.Fatalf("unexpected violation on a clean branch: %+v", v)
	}
	if !transitionedTo(rr, run.StageStateAwaitingApproval) {
		t.Error("clean branch did not advance to the review gate")
	}
}

// TestVerifyBranchLineage_CaseB_FixupVariant exercises the fixup_pushed
// boundary with a two-member ledger (a prior pull_request_opened head plus
// the fix-up head). Both compare-returned SHAs are members, so it passes.
// The PR number is resolved from the run's pull_request_url (no body PR).
func TestVerifyBranchLineage_CaseB_FixupVariant(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	const h1 = "1111111111111111111111111111111111111111" // original PR-open head
	const h2 = "2222222222222222222222222222222222222222" // fix-up head

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {h1, h2}},
	}
	gh := newLineageGitHubClient(t, stub)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	stage := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateRunning, RequiresApproval: true}
	s, sf, au, rr := newLineageServer(t, gh, runRow, stage)
	// Seed the prior pull_request_opened ledger entry so h1 is a member.
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: "pull_request_opened",
		Payload:  json.RawMessage(fmt.Sprintf(`{"head_sha":%q}`, h1)),
	})
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, mustFixupBody(t, h2), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if v := foreignViolation(au); v != nil {
		t.Fatalf("unexpected violation on a clean fix-up: %+v", v)
	}
	if !transitionedTo(rr, run.StageStateAwaitingApproval) {
		t.Error("clean fix-up did not advance the stage")
	}
	if stub.lastCompareBase != "main" {
		t.Errorf("compare base = %q, want %q", stub.lastCompareBase, "main")
	}
}

// TestVerifyBranchLineage_NonDefaultBase is the binding-condition guard: a
// run whose REAL base is a non-default branch, carrying commits that are
// legitimately on that base, must PASS. A hardcoded "main" anchor would
// false-flag the base-branch-only commits; resolving the PR's actual base
// ref ("release-1.0") excludes them.
func TestVerifyBranchLineage_NonDefaultBase(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	const head = "1111111111111111111111111111111111111111"
	const baseOnly = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	stub := &lineageGitHub{
		baseRef: "release-1.0",
		commitsByBase: map[string][]string{
			// Against the REAL base, the merge-base excludes base-only commits.
			"release-1.0": {head},
			// A hardcoded "main" would wrongly surface the base-only commit.
			"main": {baseOnly, head},
		},
	}
	gh := newLineageGitHubClient(t, stub)
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning, InstallationID: instID(99)}
	stage := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateRunning, RequiresApproval: true}
	s, sf, au, rr := newLineageServer(t, gh, runRow, stage)
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, mustPRBody(t, head), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if v := foreignViolation(au); v != nil {
		t.Fatalf("false-flagged a legit non-default-base run: %+v", v)
	}
	if !transitionedTo(rr, run.StageStateAwaitingApproval) {
		t.Error("legit non-default-base run did not advance to the review gate")
	}
	if stub.lastCompareBase != "release-1.0" {
		t.Errorf("compare base = %q, want %q (the run's real base, not the default branch)",
			stub.lastCompareBase, "release-1.0")
	}
}

// TestVerifyBranchLineage_ChildPush_Contamination exercises the guard's
// SECOND call site — succeedChildPushStage (Outcome="pushed"), the
// decomposed-child shared-branch boundary. A foreign commit on the shared
// branch must fail the child implement stage category-B, emit a
// foreign_commit_on_branch violation, and NOT advance the stage. The child
// push body carries no PR number, so the anchor resolves from the run's
// tracked pull_request_url.
func TestVerifyBranchLineage_ChildPush_Contamination(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	const head = "1111111111111111111111111111111111111111"
	const foreign = "ffffffffffffffffffffffffffffffffffffffff" // not in ledger

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {foreign, head}},
	}
	gh := newLineageGitHubClient(t, stub)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	stage := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateRunning, RequiresApproval: true}
	s, sf, au, rr := newLineageServer(t, gh, runRow, stage)
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, mustChildPushBody(t, head), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}

	v := foreignViolation(au)
	if v == nil {
		t.Fatal("expected a foreign_commit_on_branch invariant_violation audit entry, got none")
	}
	var payload struct {
		OffendingSHA string `json:"offending_sha"`
	}
	if err := json.Unmarshal(v.Payload, &payload); err != nil {
		t.Fatalf("unmarshal violation payload: %v", err)
	}
	if payload.OffendingSHA != foreign {
		t.Errorf("offending_sha = %q, want %q", payload.OffendingSHA, foreign)
	}
	if !transitionedTo(rr, run.StageStateFailed) {
		t.Error("child-push stage was not failed")
	}
	if transitionedTo(rr, run.StageStateAwaitingApproval) {
		t.Error("child-push stage advanced despite a lineage violation")
	}
	if stub.lastCompareBase != "main" {
		t.Errorf("compare base = %q, want %q (PR base ref)", stub.lastCompareBase, "main")
	}
}

// TestVerifyBranchLineage_LedgerDegradesOnReadError exercises
// buildReportedHeadLedger's WARN-and-skip branch: when ListForRunByCategory
// returns an error for the ledger categories, the ledger degrades gracefully
// (falling back to the current report's head_sha bootstrap) and a clean run
// still PASSES rather than being blocked. A read error must never produce a
// false foreign-commit verdict.
func TestVerifyBranchLineage_LedgerDegradesOnReadError(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	const head = "1111111111111111111111111111111111111111"

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {head}}, // only the run's own commit
	}
	gh := newLineageGitHubClient(t, stub)
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning, InstallationID: instID(99)}
	stage := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateRunning, RequiresApproval: true}
	s, sf, au, rr := newLineageServer(t, gh, runRow, stage)
	au.listByCategoryErr = errors.New("audit read boom") // ledger category reads fail
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, mustPRBody(t, head), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if v := foreignViolation(au); v != nil {
		t.Fatalf("ledger read-error path emitted a violation: %+v", v)
	}
	if !transitionedTo(rr, run.StageStateAwaitingApproval) {
		t.Error("ledger degradation path did not advance the happy path")
	}
}

// TestVerifyBranchLineage_MultiPushLedgerReadErrorFailsOpen is the regression
// for the partial-ledger false-block: on a MULTI-push run (compare returns the
// original PR-open head h1 PLUS the current head h2 — both legitimate), if the
// audit reads fail, an incomplete ledger would degrade to {h2} only and
// false-flag h1 as foreign, producing a false category-B failure of a clean
// run. The guard must instead FAIL OPEN when the ledger cannot be built
// completely (a contamination MISS is acceptable; a false BLOCK is not). With
// the fix the run advances with no violation; pre-fix it failed the stage and
// emitted a foreign_commit_on_branch violation against h1.
func TestVerifyBranchLineage_MultiPushLedgerReadErrorFailsOpen(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	const h1 = "1111111111111111111111111111111111111111" // original PR-open head (legit)
	const h2 = "2222222222222222222222222222222222222222" // current fix-up head (legit)

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {h1, h2}}, // both legitimate run commits
	}
	gh := newLineageGitHubClient(t, stub)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	stage := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateRunning, RequiresApproval: true}
	s, sf, au, rr := newLineageServer(t, gh, runRow, stage)
	// h1's ledger entry exists, but every category read fails — so it cannot be
	// loaded and the ledger is incomplete. Without fail-open, h1 is mis-flagged.
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: "pull_request_opened",
		Payload:  json.RawMessage(fmt.Sprintf(`{"head_sha":%q}`, h1)),
	})
	au.listByCategoryErr = errors.New("audit read boom")
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, mustFixupBody(t, h2), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if v := foreignViolation(au); v != nil {
		t.Fatalf("incomplete-ledger path false-flagged a legitimate prior head: %+v", v)
	}
	if !transitionedTo(rr, run.StageStateAwaitingApproval) {
		t.Error("incomplete-ledger fail-open did not advance the clean multi-push run")
	}
}

// TestVerifyBranchLineage_CaseC_FailOpenOnCompareError: a CompareCommits
// error (transient GitHub failure) must WARN and proceed — the happy path
// advances, no violation.
func TestVerifyBranchLineage_CaseC_FailOpenOnCompareError(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	const head = "1111111111111111111111111111111111111111"

	stub := &lineageGitHub{
		baseRef:       "main",
		compareStatus: http.StatusInternalServerError,
	}
	gh := newLineageGitHubClient(t, stub)
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning, InstallationID: instID(99)}
	stage := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateRunning, RequiresApproval: true}
	s, sf, au, rr := newLineageServer(t, gh, runRow, stage)
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, mustPRBody(t, head), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if v := foreignViolation(au); v != nil {
		t.Fatalf("fail-open path emitted a violation: %+v", v)
	}
	if !transitionedTo(rr, run.StageStateAwaitingApproval) {
		t.Error("fail-open path did not advance the happy path")
	}
}

// TestVerifyBranchLineage_CaseC_FailOpenOnNilInstallation: a run with no
// installation ID has nothing to anchor on — the guard fails open before any
// GitHub call, the happy path advances, no violation.
func TestVerifyBranchLineage_CaseC_FailOpenOnNilInstallation(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	const head = "1111111111111111111111111111111111111111"

	stub := &lineageGitHub{baseRef: "main", commitsByBase: map[string][]string{"main": {head}}}
	gh := newLineageGitHubClient(t, stub)
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning, InstallationID: nil}
	stage := &run.Stage{ID: stageID, RunID: runID, Type: run.StageTypeImplement,
		State: run.StageStateRunning, RequiresApproval: true}
	s, sf, au, rr := newLineageServer(t, gh, runRow, stage)
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, mustPRBody(t, head), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	if v := foreignViolation(au); v != nil {
		t.Fatalf("nil-installation path emitted a violation: %+v", v)
	}
	if !transitionedTo(rr, run.StageStateAwaitingApproval) {
		t.Error("nil-installation path did not advance the happy path")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.compareCalled || stub.prCalled {
		t.Error("nil-installation path made a GitHub call; expected early fail-open")
	}
}

// notifyCount returns how many lineage_violation status notifies were
// recorded by counting status-comment refire attempts is not directly
// observable; instead we assert on the emitted invariant audit entries,
// which the shared writer emits in lockstep with the notify. A second
// helper counts the foreign_commit_on_branch audit rows.
func foreignViolationCount(au *auditFake) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for i := range au.appended {
		p := au.appended[i]
		if p.Category != invariantmonitor.CategoryInvariantViolation {
			continue
		}
		var payload struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(p.Payload, &payload) == nil &&
			payload.Kind == invariantmonitor.KindForeignCommitOnBranch {
			n++
		}
	}
	return n
}

// TestReverifyBranchLineage_EmptySeed_FlagsForeignTip proves the critical
// out-of-band subtlety: ReverifyBranchLineage seeds the ledger with "" so
// the live PR tip is NOT auto-whitelisted. A foreign current tip absent from
// any reported-head ledger entry is flagged (clean=false) with an emitted
// invariant audit — exactly the contamination #862's merge-resolution check
// must catch.
func TestReverifyBranchLineage_EmptySeed_FlagsForeignTip(t *testing.T) {
	runID := uuid.New()
	const head = "1111111111111111111111111111111111111111" // live PR tip, NOT in ledger

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {head}},
	}
	gh := newReverifyGitHubClient(t, stub, head)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, _ := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})

	clean := s.ReverifyBranchLineage(context.Background(), runID, 42)
	if clean {
		t.Fatal("expected clean=false for a foreign live tip under empty seed")
	}
	if foreignViolationCount(au) != 1 {
		t.Fatalf("foreign_commit_on_branch audit count = %d, want 1", foreignViolationCount(au))
	}
	v := foreignViolation(au)
	var payload struct {
		OffendingSHA string `json:"offending_sha"`
		StageID      string `json:"stage_id"`
	}
	if err := json.Unmarshal(v.Payload, &payload); err != nil {
		t.Fatalf("unmarshal violation payload: %v", err)
	}
	if payload.OffendingSHA != head {
		t.Errorf("offending_sha = %q, want %q", payload.OffendingSHA, head)
	}
	if payload.StageID != "" {
		t.Errorf("stage_id = %q, want empty (no producing stage at merge time)", payload.StageID)
	}
	// Detect-only: no stage was failed.
	if v.StageID != nil {
		t.Errorf("audit StageID = %v, want nil at merge resolution", v.StageID)
	}
}

// TestReverifyBranchLineage_CleanBranch: every commit is attributable (a
// reported-head ledger entry covers the tip), so ReverifyBranchLineage returns
// clean=true and emits no audit.
func TestReverifyBranchLineage_CleanBranch(t *testing.T) {
	runID := uuid.New()
	const head = "1111111111111111111111111111111111111111"

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {head}},
	}
	gh := newReverifyGitHubClient(t, stub, head)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, _ := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
	// Seed the run's own PR-open head so the tip is a ledger member.
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: "pull_request_opened",
		Payload:  json.RawMessage(fmt.Sprintf(`{"head_sha":%q}`, head)),
	})

	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
		t.Fatal("expected clean=true for an attributable branch")
	}
	if v := foreignViolation(au); v != nil {
		t.Fatalf("unexpected violation on a clean branch: %+v", v)
	}
}

// TestReverifyBranchLineage_FailOpen covers the fail-open paths: nil GitHub,
// empty base ref, and a CompareCommits error each return clean=true and emit
// no audit. A transient failure must never wrongly refuse a merged run.
func TestReverifyBranchLineage_FailOpen(t *testing.T) {
	runID := uuid.New()
	const head = "1111111111111111111111111111111111111111"
	prURL := "https://github.com/x/y/pull/42"

	t.Run("nil github", func(t *testing.T) {
		runRow := &run.Run{ID: runID, Repo: "x/y", InstallationID: instID(99), PullRequestURL: &prURL}
		s, _, au, _ := newLineageServer(t, nil, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
		if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
			t.Error("nil GitHub should fail open clean")
		}
		if foreignViolationCount(au) != 0 {
			t.Error("nil GitHub emitted an audit")
		}
	})

	t.Run("empty base ref", func(t *testing.T) {
		stub := &lineageGitHub{baseRef: "", commitsByBase: map[string][]string{"main": {head}}}
		gh := newReverifyGitHubClient(t, stub, head)
		runRow := &run.Run{ID: runID, Repo: "x/y", InstallationID: instID(99), PullRequestURL: &prURL}
		s, _, au, _ := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
		if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
			t.Error("empty base ref should fail open clean")
		}
		if foreignViolationCount(au) != 0 {
			t.Error("empty base ref emitted an audit")
		}
	})

	t.Run("compare error", func(t *testing.T) {
		stub := &lineageGitHub{baseRef: "main", compareStatus: http.StatusInternalServerError}
		gh := newReverifyGitHubClient(t, stub, head)
		runRow := &run.Run{ID: runID, Repo: "x/y", InstallationID: instID(99), PullRequestURL: &prURL}
		s, _, au, _ := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
		if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
			t.Error("compare error should fail open clean")
		}
		if foreignViolationCount(au) != 0 {
			t.Error("compare error emitted an audit")
		}
	})

	t.Run("get run error", func(t *testing.T) {
		stub := &lineageGitHub{baseRef: "main", commitsByBase: map[string][]string{"main": {head}}}
		gh := newReverifyGitHubClient(t, stub, head)
		runRow := &run.Run{ID: runID, Repo: "x/y", InstallationID: instID(99), PullRequestURL: &prURL}
		s, _, au, rr := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
		rr.runErr = errors.New("load run boom")
		if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
			t.Error("get run error should fail open clean")
		}
		if foreignViolationCount(au) != 0 {
			t.Error("get run error emitted an audit")
		}
	})

	t.Run("unparseable repo", func(t *testing.T) {
		stub := &lineageGitHub{baseRef: "main", commitsByBase: map[string][]string{"main": {head}}}
		gh := newReverifyGitHubClient(t, stub, head)
		runRow := &run.Run{ID: runID, Repo: "noslash", InstallationID: instID(99), PullRequestURL: &prURL}
		s, _, au, _ := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
		if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
			t.Error("unparseable repo should fail open clean")
		}
		if foreignViolationCount(au) != 0 {
			t.Error("unparseable repo emitted an audit")
		}
	})

	t.Run("get pull request error", func(t *testing.T) {
		stub := &lineageGitHub{baseRef: "main", prStatus: http.StatusInternalServerError,
			commitsByBase: map[string][]string{"main": {head}}}
		gh := newReverifyGitHubClient(t, stub, head)
		runRow := &run.Run{ID: runID, Repo: "x/y", InstallationID: instID(99), PullRequestURL: &prURL}
		s, _, au, _ := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
		if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
			t.Error("get pull request error should fail open clean")
		}
		if foreignViolationCount(au) != 0 {
			t.Error("get pull request error emitted an audit")
		}
	})

	t.Run("no pr number and no pr url", func(t *testing.T) {
		stub := &lineageGitHub{baseRef: "main", commitsByBase: map[string][]string{"main": {head}}}
		gh := newReverifyGitHubClient(t, stub, head)
		// prNumber=0 and PullRequestURL nil => no anchor; fail open before
		// any GitHub call.
		runRow := &run.Run{ID: runID, Repo: "x/y", InstallationID: instID(99)}
		s, _, au, _ := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
		if clean := s.ReverifyBranchLineage(context.Background(), runID, 0); !clean {
			t.Error("missing pr number/url should fail open clean")
		}
		if foreignViolationCount(au) != 0 {
			t.Error("missing pr number/url emitted an audit")
		}
		stub.mu.Lock()
		defer stub.mu.Unlock()
		if stub.prCalled || stub.compareCalled {
			t.Error("missing pr number/url made a GitHub call; expected early fail-open")
		}
	})
}

// categoryErrAudit wraps auditFake to fail ListForRunByCategory for the
// named categories while leaving every other category readable (the embedded
// fake answers them). This lets a test fail JUST the merge-resolution dedup
// read of invariant_violation (#862) without also breaking the lineage-ledger
// category reads — the plain auditFake's listByCategoryErr fails all reads.
// AppendChained/seeded still flow through the embedded fake, so
// foreignViolationCount(au) observes the emit.
type categoryErrAudit struct {
	*auditFake
	errFor map[string]error
}

func (c *categoryErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if err := c.errFor[category]; err != nil {
		return nil, err
	}
	return c.auditFake.ListForRunByCategory(ctx, runID, category)
}

// TestReverifyBranchLineage_DedupReadErrorProceedsToEmit covers the
// foreignCommitAlreadyRecorded read-error fail-open: when the dedup read of
// the invariant_violation category fails, the guard cannot prove the
// contamination is already on record, so it proceeds with the emit (a
// duplicate emit is preferable to suppressing a genuine violation). The
// lineage-ledger reads still succeed (only the invariant_violation category
// read fails), so detection reaches the dedup check rather than failing open
// at the ledger. A matching prior entry is seeded that WOULD dedup if the
// read succeeded — proving the emit is driven by the read error, not absence.
func TestReverifyBranchLineage_DedupReadErrorProceedsToEmit(t *testing.T) {
	runID := uuid.New()
	const head = "1111111111111111111111111111111111111111" // foreign live tip, not in ledger

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {head}},
	}
	gh := newReverifyGitHubClient(t, stub, head)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	stage := &run.Stage{ID: uuid.New(), RunID: runID}
	s, _, au, _ := newLineageServer(t, gh, runRow, stage)
	// A matching prior violation that would dedup the emit IF the read
	// succeeded — so a non-emit here could only mean the read worked.
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: invariantmonitor.CategoryInvariantViolation,
		Payload: json.RawMessage(fmt.Sprintf(
			`{"kind":%q,"offending_sha":%q,"head_sha":%q}`,
			invariantmonitor.KindForeignCommitOnBranch, head, head)),
	})
	// Fail ONLY the dedup read; leave the lineage-ledger categories readable
	// so detection completes and reaches foreignCommitAlreadyRecorded. The
	// wrapper delegates every other category to au, so AppendChained + seeded
	// still flow through au and foreignViolationCount(au) sees the emit.
	s.cfg.AuditRepo = &categoryErrAudit{
		auditFake: au,
		errFor: map[string]error{
			invariantmonitor.CategoryInvariantViolation: errors.New("dedup read boom"),
		},
	}

	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); clean {
		t.Fatal("expected clean=false for a foreign tip")
	}
	if got := foreignViolationCount(au); got != 1 {
		t.Fatalf("dedup read-error path: audit count = %d, want 1 (proceed-with-emit)", got)
	}
}

// TestReverifyBranchLineage_Idempotent is the binding-condition test: two
// consecutive ReverifyBranchLineage calls on the SAME contaminated head emit
// the invariant audit + notify exactly ONCE (the merge reconciler re-polls
// the parked run every tick). The second call still returns clean=false but
// does not re-emit. A DIFFERENT offending SHA emits again.
func TestReverifyBranchLineage_Idempotent(t *testing.T) {
	runID := uuid.New()
	const head1 = "1111111111111111111111111111111111111111"

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {head1}},
	}
	gh := newReverifyGitHubClient(t, stub, head1)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, _ := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})

	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); clean {
		t.Fatal("first call: expected clean=false")
	}
	if got := foreignViolationCount(au); got != 1 {
		t.Fatalf("after first call: audit count = %d, want 1", got)
	}
	// Second call on the same contamination: still clean=false, NO re-emit.
	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); clean {
		t.Fatal("second call: expected clean=false (run stays parked/flagged)")
	}
	if got := foreignViolationCount(au); got != 1 {
		t.Fatalf("after second call: audit count = %d, want 1 (idempotent, no spam)", got)
	}

	// A genuinely different foreign commit must emit again.
	const head2 = "2222222222222222222222222222222222222222"
	stub.mu.Lock()
	stub.baseRef = "main"
	stub.commitsByBase = map[string][]string{"main": {head2}}
	stub.headSHA = head2
	stub.mu.Unlock()
	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); clean {
		t.Fatal("different-SHA call: expected clean=false")
	}
	if got := foreignViolationCount(au); got != 2 {
		t.Fatalf("after different-SHA call: audit count = %d, want 2 (distinct contamination emits)", got)
	}
}

// seedDecompositionChild registers a decomposition child run (DecomposedFrom
// = parentID) in the run repo and seeds an audit entry of the given category
// (child_pushed / fixup_pushed) carrying headSHA on the CHILD's chain — the
// real placement (#1038 root cause): succeedChildPushStage appends with the
// reporting child's run ID, never the parent's.
func seedDecompositionChild(rr *promptRunRepo, au *auditFake, parentID uuid.UUID,
	category, headSHA string) uuid.UUID {
	childID := uuid.New()
	parent := parentID
	rr.getRuns[childID] = &run.Run{ID: childID, Repo: "x/y", DecomposedFrom: &parent}
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &childID,
		Category: category,
		Payload:  json.RawMessage(fmt.Sprintf(`{"head_sha":%q}`, headSHA)),
	})
	return childID
}

// TestReverifyBranchLineage_DecompositionFanOutClean is the #1038 regression
// (the wedge/self-heal test): a decomposition parent whose shared branch
// carries three sibling child commits, each reported as child_pushed on the
// CHILD's own chain, while the parent's own chain holds only the
// consolidated-PR head (the branch tip). Pre-fix the parent-side
// merge-resolution re-check saw only the tip in the ledger and false-flagged
// the earlier siblings as foreign, parking the run forever. With the
// decomposition-aware ledger the fan-out re-verifies clean=true — exactly
// the verdict that lets the reconciler's resolve path terminalize the parked
// parent. A previously-recorded violation entry (the false positive already
// on the wedged run's chain) is seeded to prove a now-clean verdict is not
// suppressed by prior contamination history.
func TestReverifyBranchLineage_DecompositionFanOutClean(t *testing.T) {
	runID := uuid.New()
	const c1 = "1111111111111111111111111111111111111111" // slice 1 child commit
	const c2 = "2222222222222222222222222222222222222222" // slice 2 child commit
	const c3 = "3333333333333333333333333333333333333333" // slice 3 child commit = branch tip

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {c1, c2, c3}},
	}
	gh := newReverifyGitHubClient(t, stub, c3)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, rr := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
	// The parent's OWN chain carries only the consolidated-PR head (the tip).
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: "pull_request_opened",
		Payload:  json.RawMessage(fmt.Sprintf(`{"head_sha":%q}`, c3)),
	})
	seedDecompositionChild(rr, au, runID, "child_pushed", c1)
	seedDecompositionChild(rr, au, runID, "child_pushed", c2)
	seedDecompositionChild(rr, au, runID, "child_pushed", c3)
	// The false positive a pre-fix tick already recorded on the wedged run.
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: invariantmonitor.CategoryInvariantViolation,
		Payload: json.RawMessage(fmt.Sprintf(
			`{"kind":%q,"offending_sha":%q,"head_sha":%q}`,
			invariantmonitor.KindForeignCommitOnBranch, c1, c3)),
	})

	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
		t.Fatal("expected clean=true for a fan-out whose commits all carry child provenance")
	}
	if got := foreignViolationCount(au); got != 0 {
		t.Fatalf("clean fan-out emitted %d foreign_commit_on_branch entries, want 0", got)
	}
}

// TestReverifyBranchLineage_NoProvenanceCommitStillFlags is the load-bearing
// negative: the decomposition-aware ledger must not whitelist the branch
// wholesale. A commit with NO child_pushed/fixup_pushed provenance riding the
// shared fan-out branch (the real #797/#856 class) still fires
// foreign_commit_on_branch with that SHA.
func TestReverifyBranchLineage_NoProvenanceCommitStillFlags(t *testing.T) {
	runID := uuid.New()
	const c1 = "1111111111111111111111111111111111111111"
	const c2 = "2222222222222222222222222222222222222222"
	const foreign = "ffffffffffffffffffffffffffffffffffffffff" // no provenance anywhere

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {c1, foreign, c2}},
	}
	gh := newReverifyGitHubClient(t, stub, c2)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, rr := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
	seedDecompositionChild(rr, au, runID, "child_pushed", c1)
	seedDecompositionChild(rr, au, runID, "child_pushed", c2)

	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); clean {
		t.Fatal("expected clean=false for a no-provenance commit on the fan-out branch")
	}
	v := foreignViolation(au)
	if v == nil {
		t.Fatal("expected a foreign_commit_on_branch invariant_violation audit entry, got none")
	}
	var payload struct {
		OffendingSHA string `json:"offending_sha"`
	}
	if err := json.Unmarshal(v.Payload, &payload); err != nil {
		t.Fatalf("unmarshal violation payload: %v", err)
	}
	if payload.OffendingSHA != foreign {
		t.Errorf("offending_sha = %q, want %q", payload.OffendingSHA, foreign)
	}
}

// TestReverifyBranchLineage_ChildFixupHeadAccepted: a decomposed child's
// fix-up pushes onto the same shared parent branch (the decomposed fixup
// branch is fishhawk/run-<shortID(decomposedFromRunID)>), so a child
// fixup_pushed head must attribute cleanly in the parent's ledger.
func TestReverifyBranchLineage_ChildFixupHeadAccepted(t *testing.T) {
	runID := uuid.New()
	const c1 = "1111111111111111111111111111111111111111" // child implement commit
	const f1 = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" // child fix-up commit = tip

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {c1, f1}},
	}
	gh := newReverifyGitHubClient(t, stub, f1)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, rr := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
	childID := seedDecompositionChild(rr, au, runID, "child_pushed", c1)
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &childID,
		Category: "fixup_pushed",
		Payload:  json.RawMessage(fmt.Sprintf(`{"head_sha":%q}`, f1)),
	})

	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
		t.Fatal("expected clean=true when the tip is a child fixup_pushed head")
	}
	if got := foreignViolationCount(au); got != 0 {
		t.Fatalf("child-fixup fan-out emitted %d violations, want 0", got)
	}
}

// TestLineage_ChildEnumerationErrorAsymmetry pins the error-path asymmetry
// when the decomposition-child enumeration (ListRuns) fails: the detect path
// (ReverifyBranchLineage) FAILS OPEN — clean=true, no emit, never a false
// block on a lookup error — while the reset classifier
// (resolveLastRunAuthoredHead) FAILS CLOSED — ok=false, so an uncertain
// ledger can never drive a destructive force-update.
func TestLineage_ChildEnumerationErrorAsymmetry(t *testing.T) {
	runID := uuid.New()
	const head = "1111111111111111111111111111111111111111" // tip with no ledger entry

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {head}},
	}
	gh := newReverifyGitHubClient(t, stub, head)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, rr := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
	rr.listRunsErr = errors.New("list children boom")

	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
		t.Error("detect path: child enumeration error should fail open (clean=true)")
	}
	if got := foreignViolationCount(au); got != 0 {
		t.Errorf("detect path emitted %d violations on a lookup error, want 0", got)
	}

	repo := githubclient.RepoRef{Owner: "x", Name: "y"}
	if _, _, _, ok := s.resolveLastRunAuthoredHead(context.Background(), runRow, 99, repo, head, 42); ok {
		t.Error("reset classifier: child enumeration error should fail closed (ok=false)")
	}
}

// childChainErrAudit wraps auditFake to fail ListForRunByCategory for ONE
// run ID (a child's chain) while delegating every other run's reads to the
// embedded fake — so a test can fail JUST a per-child ledger read (#1038)
// while the parent's own chain stays readable.
type childChainErrAudit struct {
	*auditFake
	errRunID uuid.UUID
	err      error
}

func (c *childChainErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if runID == c.errRunID {
		return nil, c.err
	}
	return c.auditFake.ListForRunByCategory(ctx, runID, category)
}

// TestReverifyBranchLineage_ChildChainReadErrorFailsOpen: a read error on a
// CHILD's audit chain marks the ledger incomplete, so the detect path fails
// open (clean=true, no emit) — a partial child ledger would false-flag the
// unreadable child's legitimate commit as foreign, exactly the false block
// this guard must never produce. The parent's own chain reads fine, proving
// the per-child branch (not the own-chain degrade) drives the verdict.
func TestReverifyBranchLineage_ChildChainReadErrorFailsOpen(t *testing.T) {
	runID := uuid.New()
	const c1 = "1111111111111111111111111111111111111111" // readable child's commit
	const c2 = "2222222222222222222222222222222222222222" // unreadable child's commit

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {c1, c2}},
	}
	gh := newReverifyGitHubClient(t, stub, c2)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, rr := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
	seedDecompositionChild(rr, au, runID, "child_pushed", c1)
	unreadable := seedDecompositionChild(rr, au, runID, "child_pushed", c2)
	s.cfg.AuditRepo = &childChainErrAudit{
		auditFake: au,
		errRunID:  unreadable,
		err:       errors.New("child chain read boom"),
	}

	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
		t.Error("child chain read error should fail open (clean=true)")
	}
	if got := foreignViolationCount(au); got != 0 {
		t.Errorf("child chain read-error path emitted %d violations, want 0", got)
	}
}

// TestLineage_VouchedCommitClosesWriteReadSeam is the cross-layer
// assertion for the #1044 vouch path: it drives the SAME
// buildReportedHeadLedger path the merge reconciler's ReverifyBranchLineage
// uses, proving a vouched SHA RECORDED BY THE HANDLER (handleVouchCommit)
// is attributed clean by the detection core (the handler-write → ledger-read
// seam), while an UN-vouched foreign commit on the same branch still flags
// (fail-closed preserved). The vouch payload field is asserted via the
// shared lineageVouchedSHAField constant the handler writes — one literal,
// not two.
func TestLineage_VouchedCommitClosesWriteReadSeam(t *testing.T) {
	runID := uuid.New()
	const authoredHead = "1111111111111111111111111111111111111111"  // run's own PR-open head (ledger member)
	const vouchedCommit = "2222222222222222222222222222222222222222" // operator remediation commit, vouched
	const stillForeign = "ffffffffffffffffffffffffffffffffffffffff"  // never vouched → must still flag

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {authoredHead, vouchedCommit, stillForeign}},
	}
	gh := newReverifyGitHubClient(t, stub, stillForeign)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, _ := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: "pull_request_opened",
		Payload:  json.RawMessage(fmt.Sprintf(`{"head_sha":%q}`, authoredHead)),
	})

	// WRITE side of the seam: vouch the remediation commit via the handler.
	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: vouchedCommit, Reason: "sync-schemas output on the fan-out branch"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("vouch status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	// The handler wrote the vouched SHA under the shared payload-field
	// constant — the literal the ledger READ side keys on.
	a := vouchAudit(au)
	if a == nil {
		t.Fatal("no operator_commit_vouched audit entry written")
	}
	var payloadMap map[string]any
	if err := json.Unmarshal(a.Payload, &payloadMap); err != nil {
		t.Fatalf("unmarshal vouch payload: %v", err)
	}
	if payloadMap[lineageVouchedSHAField] != vouchedCommit {
		t.Errorf("payload[%q] = %v, want %q", lineageVouchedSHAField, payloadMap[lineageVouchedSHAField], vouchedCommit)
	}

	// READ side of the seam: ReverifyBranchLineage builds the ledger that
	// now unions the vouched commit. The vouched commit is attributed clean
	// (it is NOT the offender); the never-vouched foreign commit still flags.
	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); clean {
		t.Fatal("expected clean=false: a still-unvouched foreign commit must flag (fail-closed)")
	}
	v := foreignViolation(au)
	if v == nil {
		t.Fatal("expected a foreign_commit_on_branch entry for the unvouched commit")
	}
	var vp struct {
		OffendingSHA string `json:"offending_sha"`
	}
	if err := json.Unmarshal(v.Payload, &vp); err != nil {
		t.Fatalf("unmarshal violation payload: %v", err)
	}
	if vp.OffendingSHA != stillForeign {
		t.Errorf("offending_sha = %q, want %q (the vouched commit must NOT be flagged)", vp.OffendingSHA, stillForeign)
	}
}

// TestLineage_VouchUnwedgesRun proves the full remediation: vouching the
// only foreign commit on the branch flips the merge-resolution re-check from
// clean=false to clean=true — exactly the verdict that lets the reconciler
// terminalize the run an operator remediation commit had wedged (#1044/#1043).
func TestLineage_VouchUnwedgesRun(t *testing.T) {
	runID := uuid.New()
	const authoredHead = "1111111111111111111111111111111111111111"
	const operatorCommit = "2222222222222222222222222222222222222222" // remediation commit = tip

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {authoredHead, operatorCommit}},
	}
	gh := newReverifyGitHubClient(t, stub, operatorCommit)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, _ := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: "pull_request_opened",
		Payload:  json.RawMessage(fmt.Sprintf(`{"head_sha":%q}`, authoredHead)),
	})

	// Pre-vouch: the operator commit flags (wedged).
	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); clean {
		t.Fatal("expected clean=false before vouching")
	}

	// Vouch it via the handler, then re-check: now clean.
	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: operatorCommit, Reason: "operator remediation"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("vouch status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
		t.Fatal("expected clean=true after vouching the only foreign commit")
	}
}

// TestLineage_VouchOnDecompositionParent is the decomposition-parent
// variant: an operator vouches a remediation commit pushed on top of a
// shared fan-out branch against the PARENT run, and the parent's ledger —
// which already unions the children's reported heads — additionally unions
// the parent's vouch, so the merge reconciler's re-check on the parent
// returns clean.
func TestLineage_VouchOnDecompositionParent(t *testing.T) {
	runID := uuid.New()
	const c1 = "1111111111111111111111111111111111111111"       // slice 1 child commit
	const c2 = "2222222222222222222222222222222222222222"       // slice 2 child commit (consolidated head)
	const opCommit = "3333333333333333333333333333333333333333" // operator remediation on top = tip

	stub := &lineageGitHub{
		baseRef:       "main",
		commitsByBase: map[string][]string{"main": {c1, c2, opCommit}},
	}
	gh := newReverifyGitHubClient(t, stub, opCommit)
	prURL := "https://github.com/x/y/pull/42"
	runRow := &run.Run{ID: runID, Repo: "x/y", State: run.StateRunning,
		InstallationID: instID(99), PullRequestURL: &prURL}
	s, _, au, rr := newLineageServer(t, gh, runRow, &run.Stage{ID: uuid.New(), RunID: runID})
	// Parent's own chain holds the consolidated head; children carry their
	// own commits on their own chains.
	au.seeded = append(au.seeded, &audit.Entry{
		RunID:    &runID,
		Category: "pull_request_opened",
		Payload:  json.RawMessage(fmt.Sprintf(`{"head_sha":%q}`, c2)),
	})
	seedDecompositionChild(rr, au, runID, "child_pushed", c1)
	seedDecompositionChild(rr, au, runID, "child_pushed", c2)

	// Pre-vouch: the operator commit on the shared branch flags.
	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); clean {
		t.Fatal("expected clean=false before vouching the operator commit")
	}

	// Vouch the operator commit against the PARENT run.
	w := postVouchCommit(t, s, runID,
		vouchCommitRequest{SHA: opCommit, Reason: "operator remediation on the fan-out branch"}, withVouchOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("vouch status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if clean := s.ReverifyBranchLineage(context.Background(), runID, 42); !clean {
		t.Fatal("expected clean=true after vouching the operator commit on the parent")
	}
}

func TestParsePRNumberFromURL(t *testing.T) {
	n := "https://github.com/x/y/pull/42"
	cases := []struct {
		in   *string
		want int
	}{
		{nil, 0},
		{ptr(""), 0},
		{ptr(n), 42},
		{ptr("https://github.com/x/y/pull/42/files"), 42},
		{ptr("https://github.com/x/y/pull/7#discussion_r1"), 7},
		{ptr("https://github.com/x/y/issues/42"), 0},
		{ptr("https://github.com/x/y/pull/notanumber"), 0},
	}
	for _, tc := range cases {
		if got := parsePRNumberFromURL(tc.in); got != tc.want {
			t.Errorf("parsePRNumberFromURL(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func ptr(s string) *string { return &s }

func mustPRBody(t *testing.T, headSHA string) []byte {
	t.Helper()
	b, err := json.Marshal(pullRequestBody{
		PRNumber:          42,
		PRURL:             "https://github.com/x/y/pull/42",
		Branch:            "fishhawk/run/stage",
		HeadSHA:           headSHA,
		BaseSHA:           "2222222222222222222222222222222222222222",
		Title:             "A change.",
		FilesChangedCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustChildPushBody(t *testing.T, headSHA string) []byte {
	t.Helper()
	b, err := json.Marshal(pullRequestBody{
		Outcome: "pushed",
		Branch:  "fishhawk/run/stage",
		HeadSHA: headSHA,
		BaseSHA: "2222222222222222222222222222222222222222",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustFixupBody(t *testing.T, headSHA string) []byte {
	t.Helper()
	b, err := json.Marshal(pullRequestBody{
		Outcome: "fixup_pushed",
		Branch:  "fishhawk/run/stage",
		HeadSHA: headSHA,
		BaseSHA: "2222222222222222222222222222222222222222",
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
