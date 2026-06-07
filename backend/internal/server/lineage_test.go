package server

import (
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
			stub.mu.Unlock()
			if stub.prStatus != 0 && stub.prStatus != http.StatusOK {
				w.WriteHeader(stub.prStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"node_id":"PR_x","state":"open","head":{"sha":"H"},"base":{"ref":%q}}`, stub.baseRef)
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
