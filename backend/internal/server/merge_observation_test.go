package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fakePRStateReader is the injected forge seam. It is DEFINITIONAL bad state:
// a reader constructed with pr.Merged=false IS a not-merged forge answer, so a
// refusal test's RED lands on the handler's behavioural assertion and never on
// fixture setup.
type fakePRStateReader struct {
	pr    *forge.PullRequest
	err   error
	calls int
	// lastScope records what credential scope the handler resolved, so a test
	// can assert the run's installation ref actually reached the forge read.
	lastScope forge.CredentialScope
	lastRepo  forge.RepoRef
	lastNum   int
}

func (f *fakePRStateReader) GetPullRequest(_ context.Context, scope forge.CredentialScope,
	repo forge.RepoRef, number int) (*forge.PullRequest, error) {
	f.calls++
	f.lastScope, f.lastRepo, f.lastNum = scope, repo, number
	if f.err != nil {
		return nil, f.err
	}
	return f.pr, nil
}

// mergedPR is the canonical merged forge answer: merged, with a real merge
// commit SHA and a real merge timestamp — every field the success ladder needs.
func mergedPR() *forge.PullRequest {
	at := time.Date(2026, 8, 30, 12, 34, 56, 0, time.UTC)
	return &forge.PullRequest{
		NodeID:         "PR_x",
		State:          "closed",
		Merged:         true,
		MergeCommitSHA: "cafebabe1234",
		MergedAt:       &at,
	}
}

// observationFixture wraps the shared pg-backed supersede fixture with the two
// things the observe verb needs: a recorded pull request URL on the run, and an
// injected forge reader.
type observationFixture struct {
	*supersedeFixture
	reader *fakePRStateReader
}

// newObservationFixture seeds the exact shape #3136 describes: a running run
// with a review stage parked at awaiting_approval, a PR URL, and NO merge
// evidence of any category on its chain.
func newObservationFixture(t *testing.T, reader *fakePRStateReader) *observationFixture {
	t.Helper()
	f := newSupersedeFixture(t, parkedShape())
	if _, err := f.runRepo.SetRunPullRequestURL(context.Background(), f.runID,
		"https://github.com/x/y/pull/3064"); err != nil {
		t.Fatalf("set pr url: %v", err)
	}
	f.s.cfg.PRStateReader = reader
	return &observationFixture{supersedeFixture: f, reader: reader}
}

// observationSubject is the KNOWN authenticated subject every observe POST in
// this file installs unless a test deliberately posts anonymously. It exists so
// the attribution assertion can name an identity the test SUPPLIED: asserting
// only that ActorSubject is non-empty passes with identity propagation entirely
// broken, because the handler substitutes "anonymous".
const observationSubject = "operator@example.test"

func (f *observationFixture) postObserve(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return f.postObserveID(t, f.runID.String())
}

func (f *observationFixture) postObserveID(t *testing.T, id string) *httptest.ResponseRecorder {
	t.Helper()
	return f.postObserveAs(t, id, observationSubject)
}

// postObserveAs posts with an explicit authenticated subject. An EMPTY subject
// installs no identity at all, which is the un-authenticated shape the
// "anonymous" fallback covers.
func (f *observationFixture) postObserveAs(t *testing.T, id, subject string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+id+"/record-merge-observation", nil)
	req.SetPathValue("run_id", id)
	if subject != "" {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{Subject: subject}))
	}
	w := httptest.NewRecorder()
	f.s.handleRecordMergeObservation(w, req)
	return w
}

// seedObservationRun creates an ADDITIONAL run in the fixture's repository,
// carrying the given repo string / credential fields and pull request URL. It
// is how the tests that need a DIFFERENT run shape (an unparseable repo field,
// a credential ref, a mismatched URL) get one without reshaping the shared
// supersede fixture every other test in this package depends on.
func (f *observationFixture) seedObservationRun(t *testing.T, p run.CreateRunParams, prURL string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	if p.WorkflowID == "" {
		p.WorkflowID = "feature_change"
	}
	if p.WorkflowSHA == "" {
		p.WorkflowSHA = "abc"
	}
	if p.TriggerSource == "" {
		p.TriggerSource = run.TriggerCLI
	}
	r, err := f.runRepo.CreateRun(ctx, p)
	if err != nil {
		t.Fatalf("create run %+v: %v", p, err)
	}
	if prURL != "" {
		if _, err := f.runRepo.SetRunPullRequestURL(ctx, r.ID, prURL); err != nil {
			t.Fatalf("set pr url: %v", err)
		}
	}
	return r.ID
}

// observationRowsFor is observationRows for a run other than the fixture's own.
func (f *observationFixture) observationRowsFor(t *testing.T, runID uuid.UUID) []*audit.Entry {
	t.Helper()
	entries, err := f.audit.ListForRunByCategory(context.Background(), runID, CategoryMergeObservationRecorded)
	if err != nil {
		t.Fatalf("list merge_observation_recorded rows: %v", err)
	}
	return entries
}

// observationRows reads the run's merge_observation_recorded entries back
// through the REAL audit repository. Every refusal test calls this: error
// IDENTITY alone is insufficient for a control whose effect is COMMITTED STATE,
// so the state read-back is what makes the assertion real.
func (f *observationFixture) observationRows(t *testing.T) []*audit.Entry {
	t.Helper()
	// Read through the real repository even when a test swapped a failing stub
	// into cfg, so a stub cannot hide a row it refused to report.
	entries, err := f.audit.ListForRunByCategory(context.Background(), f.runID, CategoryMergeObservationRecorded)
	if err != nil {
		t.Fatalf("list merge_observation_recorded rows: %v", err)
	}
	return entries
}

// assertObserveRefusal asserts BOTH the HTTP status and the machine error code
// of a refusal. Status and code are asserted together deliberately: a rung that
// returns the right code under the wrong status is still a wire-contract defect.
func assertObserveRefusal(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d:\n%s", w.Code, wantStatus, w.Body.String())
	}
	assertErrorCode(t, w, wantCode)
}

// ---------------------------------------------------------------------------
// Refusal rungs. One test per named failure mode, each asserting BOTH the
// status/code AND that the run's chain carries ZERO merge_observation_recorded
// entries afterwards.
// ---------------------------------------------------------------------------

// Rung 1.
func TestRecordMergeObservationRejectsNonUUIDRunID(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
	w := f.postObserveID(t, "not-a-uuid")
	assertObserveRefusal(t, w, http.StatusBadRequest, "validation_failed")
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0 on a refusal", len(rows))
	}
	if f.reader.calls != 0 {
		t.Errorf("forge calls = %d, want 0: a malformed id must not reach the forge", f.reader.calls)
	}
}

// Rung 2. TWO distinct ways to reach the same rung, both asserted: the
// repositories being unwired, and the forge reader being unwired.
func TestRecordMergeObservationUnconfigured(t *testing.T) {
	t.Run("repositories unwired", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", PRStateReader: &fakePRStateReader{pr: mergedPR()}})
		req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+uuid.NewString()+"/record-merge-observation", nil)
		req.SetPathValue("run_id", uuid.NewString())
		w := httptest.NewRecorder()
		s.handleRecordMergeObservation(w, req)
		assertObserveRefusal(t, w, http.StatusServiceUnavailable, "record_merge_observation_unconfigured")
	})
	t.Run("no forge pull-request reader", func(t *testing.T) {
		// Repositories ARE wired; only the reader is missing. cfg.GitHub is nil
		// too, so the fallback resolves to nil and the rung must still fire —
		// recording forge evidence the server could not read is exactly what
		// this refusal exists to prevent.
		f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
		f.s.cfg.PRStateReader = nil
		w := f.postObserve(t)
		assertObserveRefusal(t, w, http.StatusServiceUnavailable, "record_merge_observation_unconfigured")
		if rows := f.observationRows(t); len(rows) != 0 {
			t.Errorf("rows = %d, want 0 on a refusal", len(rows))
		}
	})
}

// Rung 3.
func TestRecordMergeObservationRunNotFound(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
	w := f.postObserveID(t, uuid.NewString())
	assertObserveRefusal(t, w, http.StatusNotFound, "run_not_found")
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0 on a refusal", len(rows))
	}
	if f.reader.calls != 0 {
		t.Errorf("forge calls = %d, want 0", f.reader.calls)
	}
}

// Rung 4.
func TestRecordMergeObservationNoPullRequest(t *testing.T) {
	// newSupersedeFixture directly: the run never gets a PullRequestURL, which
	// is the state under test — a run that never reached a PR.
	base := newSupersedeFixture(t, parkedShape())
	reader := &fakePRStateReader{pr: mergedPR()}
	base.s.cfg.PRStateReader = reader
	f := &observationFixture{supersedeFixture: base, reader: reader}

	w := f.postObserve(t)
	assertObserveRefusal(t, w, http.StatusConflict, "record_merge_observation_no_pull_request")
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0 on a refusal", len(rows))
	}
	if f.reader.calls != 0 {
		t.Errorf("forge calls = %d, want 0", f.reader.calls)
	}
}

// Rung 5. The recorded URL carries no /pull/<n> segment, so no PR number
// resolves.
func TestRecordMergeObservationMalformedPRURL(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
	if _, err := f.runRepo.SetRunPullRequestURL(context.Background(), f.runID,
		"https://github.com/x/y/issues/3064"); err != nil {
		t.Fatalf("set malformed pr url: %v", err)
	}
	w := f.postObserve(t)
	assertObserveRefusal(t, w, http.StatusBadRequest, "record_merge_observation_malformed_pr_url")
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0 on a refusal", len(rows))
	}
	if f.reader.calls != 0 {
		t.Errorf("forge calls = %d, want 0", f.reader.calls)
	}
}

// Rung 5, OTHER trigger condition. The rung guards TWO independent failures —
// an unparseable runRow.Repo and an unresolvable PR number — and the test above
// drives only the second. Here the URL is perfectly well formed and it is the
// RUN's repo field that is not owner/name, so the RED lands on the repo half
// specifically. Without both halves driven, the rung's counterfactual only ever
// proved one of its two conditions.
func TestRecordMergeObservationUnparseableRepoField(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
	id := f.seedObservationRun(t, run.CreateRunParams{Repo: "not-owner-slash-name"},
		"https://github.com/x/y/pull/3064")

	w := f.postObserveID(t, id.String())
	assertObserveRefusal(t, w, http.StatusBadRequest, "record_merge_observation_malformed_pr_url")
	if rows := f.observationRowsFor(t, id); len(rows) != 0 {
		t.Errorf("rows = %d, want 0 on a refusal", len(rows))
	}
	if f.reader.calls != 0 {
		t.Errorf("forge calls = %d, want 0: an unresolvable repository must not reach the forge", f.reader.calls)
	}
}

// Rung 5b — THE routed security guard. The forge read is scoped by
// runRow.Repo while the PR number comes from runRow.PullRequestURL, and the
// appended row records that URL as the pull request observed merged. Without
// this rung the handler confirms a DIFFERENT repository's pull request and then
// writes trusted evidence naming this run's URL, which reconcile-merge reads to
// settle the run.
//
// Each case pairs a mismatching URL with a run whose repo field is otherwise
// fine, so the refusal can only come from the comparison — never from rung 5
// catching a malformed value first. Every case asserts the zero-row chain AND
// that the forge was never called: refusing BEFORE the read is half the point.
func TestRecordMergeObservationPRURLRepoMismatch(t *testing.T) {
	cases := []struct {
		name  string
		repo  string
		prURL string
	}{
		{"different owner", "x/y", "https://github.com/attacker/y/pull/3064"},
		{"different repository", "x/y", "https://github.com/x/other/pull/3064"},
		{"both differ", "x/y", "https://github.com/attacker/other/pull/3064"},
		// A deeper path is not this run's repo either: the two segments ahead
		// of /pull/ must BE the repo, not merely contain it.
		{"nested path", "x/y", "https://github.com/org/x/y/pull/3064"},
		{"no host", "x/y", "/x/y/pull/3064"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
			id := f.seedObservationRun(t, run.CreateRunParams{Repo: tc.repo}, tc.prURL)

			w := f.postObserveID(t, id.String())
			assertObserveRefusal(t, w, http.StatusConflict, "record_merge_observation_pr_url_repo_mismatch")
			if rows := f.observationRowsFor(t, id); len(rows) != 0 {
				t.Errorf("rows = %d, want 0: a URL that is not this run's PR must never gain merge evidence", len(rows))
			}
			if f.reader.calls != 0 {
				t.Errorf("forge calls = %d, want 0: a mismatch must be refused BEFORE the forge read", f.reader.calls)
			}
		})
	}
}

// Rung 5b, forge half. The run authenticates against a NON-GitHub forge (a
// "<forge>:<id>" credential ref), so a GitHub-style /pull/ URL cannot be its
// pull request. The repo field MATCHES the URL here deliberately: this case can
// only pass because of the forge condition, not the owner/name comparison.
func TestRecordMergeObservationForgeMismatch(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
	ref := "gitlab:4242"
	id := f.seedObservationRun(t, run.CreateRunParams{Repo: "x/y", InstallationRef: &ref},
		"https://github.com/x/y/pull/3064")

	w := f.postObserveID(t, id.String())
	assertObserveRefusal(t, w, http.StatusConflict, "record_merge_observation_pr_url_repo_mismatch")
	if rows := f.observationRowsFor(t, id); len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
	if f.reader.calls != 0 {
		t.Errorf("forge calls = %d, want 0", f.reader.calls)
	}
}

// Rung 5b must not refuse a legitimate run. Two shapes that LOOK like
// mismatches and are not: a case-differing URL spelling (GitHub repo names are
// case-insensitive, so it is the SAME repository) and an enterprise host (the
// run row records no host, so the host is not what is being compared).
func TestRecordMergeObservationAcceptsEquivalentPRURLs(t *testing.T) {
	cases := []struct {
		name  string
		repo  string
		prURL string
	}{
		{"case differs", "x/y", "https://github.com/X/Y/pull/3064"},
		{"enterprise host", "x/y", "https://github.example.test/x/y/pull/3064"},
		{"trailing path segment", "x/y", "https://github.com/x/y/pull/3064/files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
			id := f.seedObservationRun(t, run.CreateRunParams{Repo: tc.repo}, tc.prURL)

			w := f.postObserveID(t, id.String())
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (this URL names the run's own repository):\n%s",
					w.Code, w.Body.String())
			}
			if rows := f.observationRowsFor(t, id); len(rows) != 1 {
				t.Errorf("rows = %d, want exactly 1", len(rows))
			}
		})
	}
}

// Rung 6, fail-closed half: an unreadable chain is a 500 and NEVER a write. A
// verb whose whole job is deciding whether evidence is MISSING must not treat
// "unknown" as "absent".
func TestRecordMergeObservationChainReadFailureFailsClosed(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
	f.s.cfg.AuditRepo = &msListCategoryErrAudit{
		Repository: f.audit, failCategory: CategoryPRMerged, err: errors.New("chain unreadable"),
	}
	w := f.postObserve(t)
	assertObserveRefusal(t, w, http.StatusInternalServerError, "internal_error")
	f.s.cfg.AuditRepo = f.audit
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0: an unreadable chain must not license a write", len(rows))
	}
	if f.reader.calls != 0 {
		t.Errorf("forge calls = %d, want 0: unknown evidence must not reach the forge", f.reader.calls)
	}
}

// Rung 7.
func TestRecordMergeObservationForgeUnavailable(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{err: errors.New("forge down")})
	w := f.postObserve(t)
	assertObserveRefusal(t, w, http.StatusBadGateway, "record_merge_observation_forge_unavailable")
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0 on a refusal", len(rows))
	}
}

// Rung 8 — THE guard that stops the verb manufacturing evidence for a change
// that never shipped. Bad state is definitional: Merged:false IS a not-merged
// forge answer.
func TestRecordMergeObservationPRNotMerged(t *testing.T) {
	notMerged := mergedPR()
	notMerged.Merged = false
	notMerged.State = "open"
	f := newObservationFixture(t, &fakePRStateReader{pr: notMerged})

	w := f.postObserve(t)
	assertObserveRefusal(t, w, http.StatusConflict, "record_merge_observation_pr_not_merged")
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0: an unmerged PR must never gain merge evidence", len(rows))
	}
	// And the consequence that matters: reconcile-merge is still refused, so no
	// path exists from an unmerged PR to a `succeeded` run.
	rec := f.postReconcile(t)
	assertObserveRefusal(t, rec, http.StatusConflict, "reconcile_merge_pr_not_merged")
}

// Rung 9 — merged but no merge commit SHA. This is also the rung that gives a
// GitLab run a truthful NAMED refusal until that adapter's deferred half lands.
func TestRecordMergeObservationNoMergeCommit(t *testing.T) {
	noSHA := mergedPR()
	noSHA.MergeCommitSHA = ""
	f := newObservationFixture(t, &fakePRStateReader{pr: noSHA})

	w := f.postObserve(t)
	assertObserveRefusal(t, w, http.StatusConflict, "record_merge_observation_no_merge_commit")
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0: an observation with no commit must not be recorded", len(rows))
	}
}

// Rung 10 (binding approval condition 2) — merged, WITH a SHA, but a nil
// merged_at. The payload and the response both promise the forge's real merge
// time, so a row here would claim evidence it does not carry. The SHA is
// deliberately present: this test can only pass because of the timestamp guard
// specifically, not because the rung-9 SHA guard caught it first.
func TestRecordMergeObservationNoMergeTimestamp(t *testing.T) {
	noTime := mergedPR()
	noTime.MergedAt = nil
	f := newObservationFixture(t, &fakePRStateReader{pr: noTime})

	w := f.postObserve(t)
	assertObserveRefusal(t, w, http.StatusConflict, "record_merge_observation_no_merge_timestamp")
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0: a partial observation must not be recorded", len(rows))
	}
	// The refusal must be reachable, i.e. the run is otherwise fully eligible:
	// giving it a timestamp turns the SAME request into a success.
	noTime.MergedAt = mergedPR().MergedAt
	ok := f.postObserve(t)
	if ok.Code != http.StatusOK {
		t.Fatalf("with a timestamp status = %d, want 200 (the run must be otherwise eligible):\n%s",
			ok.Code, ok.Body.String())
	}
}

// A nil *forge.PullRequest with a nil error is the same UNKNOWN as an error and
// must never be read as a merge. Guarded alongside rung 8 in one condition, so
// it gets its own assertion.
func TestRecordMergeObservationNilPRIsNotAMerge(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: nil})
	w := f.postObserve(t)
	assertObserveRefusal(t, w, http.StatusConflict, "record_merge_observation_pr_not_merged")
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0", len(rows))
	}
}

// An audit append failure is a 500, and the chain stays empty.
func TestRecordMergeObservationAppendFailureIsAnError(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
	f.s.cfg.AuditRepo = &msAppendErrAudit{Repository: f.audit, err: errors.New("chain down")}
	w := f.postObserve(t)
	assertObserveRefusal(t, w, http.StatusInternalServerError, "internal_error")
	f.s.cfg.AuditRepo = f.audit
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("rows = %d, want 0: a failed append must not be reported as recorded", len(rows))
	}
}

// ---------------------------------------------------------------------------
// Happy path + idempotence.
// ---------------------------------------------------------------------------

// TestRecordMergeObservationAppendsOneEntry pins the success ladder end to end:
// exactly one entry, actor_kind user with the authenticated subject, and EVERY
// payload key — including the two timestamps whose separation is the reason
// this category is distinct from pr_merged.
func TestRecordMergeObservationAppendsOneEntry(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
	before := time.Now().UTC()

	w := f.postObserve(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp recordMergeObservationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AlreadyRecorded {
		t.Error("already_recorded = true, want false on the first observation")
	}
	if resp.Observation.MergeCommitSHA != "cafebabe1234" {
		t.Errorf("response merge_commit_sha = %q, want cafebabe1234", resp.Observation.MergeCommitSHA)
	}

	// The forge read used the run's own repo and PR number.
	if f.reader.lastRepo.Owner != "x" || f.reader.lastRepo.Name != "y" {
		t.Errorf("forge repo = %v, want x/y", f.reader.lastRepo)
	}
	if f.reader.lastNum != 3064 {
		t.Errorf("forge pr number = %d, want 3064", f.reader.lastNum)
	}

	rows := f.observationRows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want exactly 1", len(rows))
	}
	e := rows[0]
	if e.ActorKind == nil || *e.ActorKind != audit.ActorUser {
		t.Errorf("actor_kind = %v, want user (an operator-invoked observation)", e.ActorKind)
	}
	// The EXACT subject the request installed, not merely "non-empty": the
	// handler substitutes "anonymous" when no identity is present, so a
	// non-empty assertion passes with identity propagation entirely broken
	// while guarding an audit-accountability claim.
	if e.ActorSubject == nil {
		t.Errorf("actor_subject = nil, want the authenticated %q", observationSubject)
	} else if *e.ActorSubject != observationSubject {
		t.Errorf("actor_subject = %q, want the authenticated %q", *e.ActorSubject, observationSubject)
	}
	var p struct {
		RunID                  string `json:"run_id"`
		PullRequestURL         string `json:"pull_request_url"`
		PullRequestNumber      int    `json:"pull_request_number"`
		MergeCommitSHA         string `json:"merge_commit_sha"`
		MergedAt               string `json:"merged_at"`
		ObservedAt             string `json:"observed_at"`
		ReconciledAfterTheFact bool   `json:"reconciled_after_the_fact"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.RunID != f.runID.String() {
		t.Errorf("payload run_id = %q, want %q", p.RunID, f.runID)
	}
	if p.PullRequestURL != "https://github.com/x/y/pull/3064" {
		t.Errorf("payload pull_request_url = %q", p.PullRequestURL)
	}
	if p.PullRequestNumber != 3064 {
		t.Errorf("payload pull_request_number = %d, want 3064", p.PullRequestNumber)
	}
	if p.MergeCommitSHA != "cafebabe1234" {
		t.Errorf("payload merge_commit_sha = %q, want cafebabe1234", p.MergeCommitSHA)
	}
	if !p.ReconciledAfterTheFact {
		t.Error("payload reconciled_after_the_fact = false, want true")
	}
	// merged_at is the FORGE's value, not now(); observed_at is now(). The two
	// being DIFFERENT is the whole reason this category is not a synthetic
	// pr_merged row.
	gotMerged, err := time.Parse(time.RFC3339Nano, p.MergedAt)
	if err != nil {
		t.Fatalf("parse merged_at %q: %v", p.MergedAt, err)
	}
	wantMerged := time.Date(2026, 8, 30, 12, 34, 56, 0, time.UTC)
	if !gotMerged.Equal(wantMerged) {
		t.Errorf("payload merged_at = %v, want the FORGE's %v (never now())", gotMerged, wantMerged)
	}
	gotObserved, err := time.Parse(time.RFC3339Nano, p.ObservedAt)
	if err != nil {
		t.Fatalf("parse observed_at %q: %v", p.ObservedAt, err)
	}
	if gotObserved.Before(before) {
		t.Errorf("payload observed_at = %v, want >= the pre-call wall clock %v", gotObserved, before)
	}
	if gotObserved.Equal(gotMerged) {
		t.Error("observed_at equals merged_at; the two timestamps must be recorded independently")
	}
}

// The "anonymous" substitution is a real branch and gets its own case, so the
// attribution test above never has to accommodate it. Posting with NO identity
// installed must still attribute — to the fallback, explicitly — rather than
// writing a nil or empty subject.
func TestRecordMergeObservationAnonymousFallback(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})

	w := f.postObserveAs(t, f.runID.String(), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	rows := f.observationRows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want exactly 1", len(rows))
	}
	if rows[0].ActorSubject == nil || *rows[0].ActorSubject != "anonymous" {
		t.Errorf("actor_subject = %v, want %q", rows[0].ActorSubject, "anonymous")
	}
	if rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorUser {
		t.Errorf("actor_kind = %v, want user", rows[0].ActorKind)
	}
}

// TestRecordMergeObservationCredentialScope pins the credential-scope ladder
// the handler resolves for the forge read. The fake reader records lastScope
// and nothing read it, so a regression in the InstallationRef / legacy
// InstallationID selection passed the suite while live forge reads used the
// wrong scope — or none at all.
func TestRecordMergeObservationCredentialScope(t *testing.T) {
	legacyID := int64(777)
	ref := "424242"
	otherRef := "515151"
	cases := []struct {
		name    string
		params  run.CreateRunParams
		wantRef string
	}{
		// The ADR-057 / ADR-058 forge-neutral ref is authoritative when set.
		{"installation ref", run.CreateRunParams{Repo: "x/y", InstallationRef: &ref}, "424242"},
		// Legacy fallback: no ref recorded, so the GitHub installation id is
		// what the scope must carry.
		{"legacy installation id", run.CreateRunParams{Repo: "x/y", InstallationID: &legacyID}, "777"},
		// Both present: the ref WINS. Asserting the fallback alone would pass
		// with the ladder inverted.
		{
			"ref wins over legacy id",
			run.CreateRunParams{Repo: "x/y", InstallationRef: &otherRef, InstallationID: &legacyID},
			"515151",
		},
		// Neither recorded: the zero scope, which the production reader
		// rejects — surfacing as the forge-unavailable rung rather than a
		// silent unauthenticated read.
		{"neither recorded", run.CreateRunParams{Repo: "x/y"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
			id := f.seedObservationRun(t, tc.params, "https://github.com/x/y/pull/3064")

			w := f.postObserveID(t, id.String())
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
			}
			if f.reader.calls != 1 {
				t.Fatalf("forge calls = %d, want 1", f.reader.calls)
			}
			if got := f.reader.lastScope.Ref(); got != tc.wantRef {
				t.Errorf("credential scope ref = %q, want %q", got, tc.wantRef)
			}
		})
	}
}

// TestRecordMergeObservationIsIdempotent pins the claim the code actually
// makes (binding approval condition 3): a SEQUENTIAL repeat POST appends
// nothing and reports already_recorded. It deliberately does NOT claim
// exactly-one under concurrency — the read-then-append is not atomic and this
// test structurally cannot observe that; the handler's doc comment states the
// weaker guarantee and why a concurrent duplicate is inert.
func TestRecordMergeObservationIsIdempotent(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})

	if w := f.postObserve(t); w.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	callsAfterFirst := f.reader.calls

	w := f.postObserve(t)
	if w.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp recordMergeObservationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.AlreadyRecorded {
		t.Error("already_recorded = false on the second POST, want true")
	}
	if resp.Observation.MergeCommitSHA != "" {
		t.Errorf("observation = %+v, want empty on a no-op: the call appended nothing and must not claim it did",
			resp.Observation)
	}
	if rows := f.observationRows(t); len(rows) != 1 {
		t.Errorf("rows = %d, want still exactly 1 after a sequential repeat POST", len(rows))
	}
	if f.reader.calls != callsAfterFirst {
		t.Errorf("forge calls = %d, want unchanged at %d: an already-observed run must not round-trip the forge",
			f.reader.calls, callsAfterFirst)
	}
}

// A run whose merge was already seen LIVE (a pr_merged row) must no-op rather
// than adding a second, competing observation under a different category.
func TestRecordMergeObservationNoOpsOnExistingPRMerged(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})
	f.observeMerge(t) // seeds pr_merged

	w := f.postObserve(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp recordMergeObservationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.AlreadyRecorded {
		t.Error("already_recorded = false, want true when pr_merged is already on the chain")
	}
	if rows := f.observationRows(t); len(rows) != 0 {
		t.Errorf("merge_observation_recorded rows = %d, want 0: a live observation must not draw a second, competing record", len(rows))
	}
	if f.reader.calls != 0 {
		t.Errorf("forge calls = %d, want 0", f.reader.calls)
	}
}

// ---------------------------------------------------------------------------
// Cross-layer.
// ---------------------------------------------------------------------------

// TestRecordThenReconcileSettlesStrandedRun walks the issue's OWN shape in one
// pass, across every layer scope.files spans — forge read -> audit append ->
// evidence gate -> stage transition -> run completion. Per-layer units all pass
// while this seam breaks, which is the #618 shape.
//
// The run starts exactly as run 96dcade0 sits today: merged on the forge, zero
// merge evidence on its chain, review stage parked at awaiting_approval, run
// stuck `running`.
func TestRecordThenReconcileSettlesStrandedRun(t *testing.T) {
	f := newObservationFixture(t, &fakePRStateReader{pr: mergedPR()})

	// 1. Before the observation, reconcile is correctly refused — the evidence
	//    it needs cannot appear, which is the defect #3136 reports.
	assertObserveRefusal(t, f.postReconcile(t), http.StatusConflict, "reconcile_merge_pr_not_merged")
	if got := f.stageState(t, f.stages[run.StageTypeReview].ID); got != run.StageStateAwaitingApproval {
		t.Fatalf("review stage = %q, want still awaiting_approval", got)
	}

	// 2. Observe. The forge answers merged; exactly one entry lands carrying
	//    the forge's SHA and timestamp.
	if w := f.postObserve(t); w.Code != http.StatusOK {
		t.Fatalf("observe status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	rows := f.observationRows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want exactly 1", len(rows))
	}
	var p struct {
		MergeCommitSHA string `json:"merge_commit_sha"`
		MergedAt       string `json:"merged_at"`
	}
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if p.MergeCommitSHA != "cafebabe1234" || p.MergedAt == "" {
		t.Errorf("payload carries sha=%q merged_at=%q, want the forge's values", p.MergeCommitSHA, p.MergedAt)
	}

	// 3. Settle. The settling verb reads only the chain and now finds evidence.
	w := f.postReconcile(t)
	if w.Code != http.StatusOK {
		t.Fatalf("reconcile status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var rec reconcileMergeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatalf("decode reconcile response: %v", err)
	}
	foundReview := false
	for _, sup := range rec.Superseded {
		if sup.StageType == string(run.StageTypeReview) {
			foundReview = true
			if sup.Reason != supersedeReasonOperatorReconcile {
				t.Errorf("review supersede reason = %q, want %q", sup.Reason, supersedeReasonOperatorReconcile)
			}
		}
	}
	if !foundReview {
		t.Errorf("reconcile superseded %+v, want the review stage among them", rec.Superseded)
	}
	if got := f.stageState(t, f.stages[run.StageTypeReview].ID); got != run.StageStateSuperseded {
		t.Errorf("review stage = %q, want superseded", got)
	}
	after, err := f.runRepo.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if after.State != run.StateSucceeded {
		t.Fatalf("run state = %q, want succeeded: the observe/settle pair must fully settle a stranded run", after.State)
	}
	if rec.RunState != string(run.StateSucceeded) {
		t.Errorf("reconcile response run_state = %q, want succeeded", rec.RunState)
	}
}

// TestRecordMergeObservationRouteRegistered pins the wire surface: the route
// exists on the real mux at the documented method+path, so an operator calling
// the documented verb reaches the handler rather than a 404.
func TestRecordMergeObservationRouteRegistered(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+uuid.NewString()+"/record-merge-observation", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want the route to resolve (any non-404/405):\n%s", w.Code, w.Body.String())
	}
}
