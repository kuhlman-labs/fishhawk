package mergeoutcome

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const (
	testMergeSHA  = "1111111111111111111111111111111111111111"
	testRevertSHA = "2222222222222222222222222222222222222222"
	testOtherSHA  = "3333333333333333333333333333333333333333"
	testPRURL     = "https://github.com/acme/widgets/pull/42"
)

var testMergedAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// fakeForge is a mutex-guarded RevertForge that counts every call.
type fakeForge struct {
	mu             sync.Mutex
	pullsForCommit map[string][]forge.PullRequestRef
	listErrs       []error // returned, in order, before pullsForCommit answers
	prs            map[int]*forge.PullRequest
	files          map[string]*githubclient.CommitFiles
	listCalls      int
	getCalls       int
	filesCalls     int
}

func (f *fakeForge) ListPullRequestsForCommit(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, sha string) ([]forge.PullRequestRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if len(f.listErrs) > 0 {
		err := f.listErrs[0]
		f.listErrs = f.listErrs[1:]
		return nil, err
	}
	return f.pullsForCommit[sha], nil
}

func (f *fakeForge) GetPullRequest(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, number int) (*forge.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	pr, ok := f.prs[number]
	if !ok {
		return nil, fmt.Errorf("%w: get pr", forge.ErrNotFound)
	}
	return pr, nil
}

func (f *fakeForge) GetCommitFiles(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, sha string) (*githubclient.CommitFiles, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filesCalls++
	cf, ok := f.files[sha]
	if !ok {
		return nil, fmt.Errorf("%w: get commit files", forge.ErrNotFound)
	}
	return cf, nil
}

func (f *fakeForge) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls + f.getCalls + f.filesCalls
}

type fakeRuns struct {
	mu    sync.Mutex
	byURL map[string][]*run.Run
	err   error
	calls int
}

func (r *fakeRuns) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	if f.PullRequestURL == nil || f.Repo != "acme/widgets" || f.Limit <= 0 {
		return nil, nil
	}
	return r.byURL[*f.PullRequestURL], nil
}

func mergeFiles() *githubclient.CommitFiles {
	return &githubclient.CommitFiles{SHA: testMergeSHA, Files: []githubclient.CommitFile{
		{Path: "a.go", Status: "modified", Additions: 10, Deletions: 2},
		{Path: "b.go", Status: "added", Additions: 3},
	}}
}

func inverseFiles() *githubclient.CommitFiles {
	return &githubclient.CommitFiles{SHA: testRevertSHA, Files: []githubclient.CommitFile{
		{Path: "a.go", Status: "modified", Additions: 2, Deletions: 10},
		{Path: "b.go", Status: "removed", Deletions: 3},
	}}
}

type revertFixture struct {
	obs   *RevertObserver
	forge *fakeForge
	runs  *fakeRuns
	audit *memAudit
	runID uuid.UUID
	logs  *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newRevertFixture wires run R on PR 42, a merged PR 42 whose merge commit is
// testMergeSHA, the trailer SHA mapped to PR 42, and a genuinely inverse
// revert diff.
func newRevertFixture() *revertFixture {
	runID := uuid.New()
	ff := &fakeForge{
		pullsForCommit: map[string][]forge.PullRequestRef{testMergeSHA: {{Number: 42, URL: testPRURL}}},
		prs: map[int]*forge.PullRequest{42: {
			NodeID: "PR_42", Merged: true, State: "closed", MergeCommitSHA: testMergeSHA, MergedAt: &testMergedAt,
		}},
		files: map[string]*githubclient.CommitFiles{testMergeSHA: mergeFiles(), testRevertSHA: inverseFiles()},
	}
	fr := &fakeRuns{byURL: map[string][]*run.Run{testPRURL: {{ID: runID, Repo: "acme/widgets"}}}}
	ma := &memAudit{}
	logs := &syncBuffer{}
	return &revertFixture{
		obs: &RevertObserver{
			Forge: ff, Runs: fr, Audit: ma,
			Logger:       slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Now:          func() time.Time { return time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC) },
			RetryBackoff: time.Millisecond,
		},
		forge: ff, runs: fr, audit: ma, runID: runID, logs: logs,
	}
}

func testPush(ref string, commits ...PushCommit) *PushEvent {
	return &PushEvent{
		Ref: ref, After: testRevertSHA, FullName: "acme/widgets", HTMLURL: "https://github.com/acme/widgets",
		DefaultBranch: "main", InstallationID: 9, Commits: commits,
	}
}

func trailerMessage(sha string) string {
	return "Revert \"feat: add widgets (#42)\"\n\nThis reverts commit " + sha + ".\n"
}

func (f *revertFixture) rows(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range f.audit.rows(f.runID, CategoryRunMergeReverted) {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("payload: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// --- parsing ---------------------------------------------------------------

func TestParsePush_Fields(t *testing.T) {
	raw := []byte(`{"ref":"refs/heads/main","deleted":false,"after":"abc","sender":{"type":"Bot"},
		"repository":{"full_name":"acme/widgets","html_url":"https://github.com/acme/widgets/","default_branch":"main"},
		"installation":{"id":9},
		"commits":[{"id":"c1","message":"m1","timestamp":"2026-09-01T00:00:00Z","author":{"name":"x"}}]}`)
	p, err := ParsePush(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Ref != "refs/heads/main" || p.After != "abc" || p.FullName != "acme/widgets" ||
		p.HTMLURL != "https://github.com/acme/widgets" || p.DefaultBranch != "main" || p.InstallationID != 9 ||
		len(p.Commits) != 1 || p.Commits[0] != (PushCommit{ID: "c1", Message: "m1"}) {
		t.Fatalf("parsed %+v", p)
	}
	if !p.IsDefaultBranchPush() {
		t.Error("main push: IsDefaultBranchPush = false, want true")
	}
	if _, err := ParsePush([]byte(`{not json`)); err == nil {
		t.Error("malformed payload: want error")
	}
}

func TestParsePush_MissingDefaultBranch_NotDefault(t *testing.T) {
	// The bare "refs/heads/" ref isolates the guard: it is the one ref that
	// equals "refs/heads/"+"" once default_branch is missing.
	for _, ref := range []string{"refs/heads/main", "refs/heads/"} {
		p, err := ParsePush([]byte(`{"ref":"` + ref + `","repository":{"full_name":"acme/widgets"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if p.IsDefaultBranchPush() {
			t.Fatalf("ref %q with no repository.default_branch must not count as a default-branch push", ref)
		}
	}
}

func TestIsDefaultBranchPush(t *testing.T) {
	base := testPush("refs/heads/main")
	if !base.IsDefaultBranchPush() {
		t.Fatal("main: want true")
	}
	for name, p := range map[string]*PushEvent{
		"feature branch": testPush("refs/heads/feature"),
		"tag":            testPush("refs/tags/main"),
		"deleted":        func() *PushEvent { p := testPush("refs/heads/main"); p.Deleted = true; return p }(),
		"nil":            nil,
	} {
		if p.IsDefaultBranchPush() {
			t.Errorf("%s: IsDefaultBranchPush = true, want false", name)
		}
	}
}

func TestParseRevertSignals(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		shas []string
		pr   int
	}{
		{"git revert default", trailerMessage(testMergeSHA), []string{testMergeSHA}, 42},
		{"abbreviated trailer", "Revert \"x\"\n\nThis reverts commit 1234abc.", []string{"1234abc"}, 0},
		{"squashed revert PR", "Revert \"feat: x (#42)\" (#57)\n\n* body", nil, 42},
		{"last ref inside the quotes wins", "Revert \"fix (#7) follow-up (#42)\" (#99)", nil, 42},
		{"two trailers, one repeated", "squash\n\nThis reverts commit aaaaaaa.\nThis reverts commit bbbbbbb.\nThis reverts commit aaaaaaa.",
			[]string{"aaaaaaa", "bbbbbbb"}, 0},
		{"ordinary commit", "feat: add widgets (#42)\n\nbody", nil, 0},
		{"subject without PR ref", "Revert \"feat: add widgets\"", nil, 0},
		{"unterminated quote", "Revert \"feat (#42)", nil, 0},
		{"too-short sha", "This reverts commit abc12.", nil, 0},
		{"revert of revert, trailer kept", "Revert \"Revert \"x (#42)\"\"\n\nThis reverts commit 1234abcd.", []string{"1234abcd"}, 0},
	}
	for _, tc := range cases {
		got := ParseRevertSignals(tc.msg)
		if fmt.Sprint(got.RevertedSHAs) != fmt.Sprint(tc.shas) || got.PRNumber != tc.pr {
			t.Errorf("%s: got %+v, want shas=%v pr=%d", tc.name, got, tc.shas, tc.pr)
		}
		if got.Empty() != (len(tc.shas) == 0 && tc.pr == 0) {
			t.Errorf("%s: Empty() = %v", tc.name, got.Empty())
		}
	}
}

// TestParseRevertSignals_RevertOfRevert_NoSignal is control (4): the subject
// of a revert-of-revert with no trailer yields NO PR number, though its inner
// quoted text carries (#42). Counterfactual: delete the skip → PRNumber 42.
func TestParseRevertSignals_RevertOfRevert_NoSignal(t *testing.T) {
	got := ParseRevertSignals("Revert \"Revert \"feat: add widgets (#42)\"\"")
	if !got.Empty() {
		t.Fatalf("revert-of-revert: got %+v, want no signal", got)
	}
}

// --- attestation -------------------------------------------------------------

func TestClassifyAttestation(t *testing.T) {
	m := mergeFiles()
	cases := []struct {
		name          string
		revert, merge *githubclient.CommitFiles
		want, reason  string
	}{
		{"genuine inverse", inverseFiles(), m, AttestationInverseDiff, attestReasonAllInverted},
		{"partial inverse", &githubclient.CommitFiles{Files: []githubclient.CommitFile{{Path: "a.go", Additions: 2, Deletions: 10}}},
			m, AttestationPartialInverse, attestReasonSubsetInverted},
		{"unrelated file", &githubclient.CommitFiles{Files: []githubclient.CommitFile{{Path: "a.go", Additions: 2, Deletions: 10}, {Path: "z.go", Additions: 1}}},
			m, AttestationSignalOnly, attestReasonFileNotInMerge},
		{"same files, not inverted", &githubclient.CommitFiles{Files: []githubclient.CommitFile{{Path: "a.go", Additions: 10, Deletions: 2}}},
			m, AttestationSignalOnly, attestReasonNotInverted},
		{"empty revert", &githubclient.CommitFiles{}, m, AttestationSignalOnly, attestReasonRevertEmpty},
		{"revert unavailable", nil, m, AttestationSignalOnly, attestReasonFilesUnavailable},
		{"merge unavailable", inverseFiles(), nil, AttestationSignalOnly, attestReasonFilesUnavailable},
		{"revert truncated", func() *githubclient.CommitFiles { c := inverseFiles(); c.Truncated = true; return c }(),
			m, AttestationSignalOnly, attestReasonFilesTruncated},
		{"merge truncated", inverseFiles(), func() *githubclient.CommitFiles { c := mergeFiles(); c.Truncated = true; return c }(),
			AttestationSignalOnly, attestReasonFilesTruncated},
		{"rename reverted", &githubclient.CommitFiles{Files: []githubclient.CommitFile{{Path: "old.go", PreviousPath: "new.go", Additions: 1, Deletions: 4}}},
			&githubclient.CommitFiles{Files: []githubclient.CommitFile{{Path: "new.go", PreviousPath: "old.go", Additions: 4, Deletions: 1}}},
			AttestationInverseDiff, attestReasonAllInverted},
		{"duplicate revert path cannot cover twice", &githubclient.CommitFiles{Files: []githubclient.CommitFile{{Path: "b.go", Deletions: 3}, {Path: "b.go", Deletions: 3}}},
			m, AttestationSignalOnly, attestReasonFileNotInMerge},
	}
	for _, tc := range cases {
		got, reason := ClassifyAttestation(tc.revert, tc.merge)
		if got != tc.want || reason != tc.reason {
			t.Errorf("%s: got (%s, %s), want (%s, %s)", tc.name, got, reason, tc.want, tc.reason)
		}
	}
}

// --- observer ----------------------------------------------------------------

func TestObservePush_RevertOfRunMerge_RecordsInverseDiff(t *testing.T) {
	f := newRevertFixture()
	sum := f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d-1")
	rows := f.rows(t)
	if len(rows) != 1 || sum.Recorded != 1 {
		t.Fatalf("rows = %d recorded = %d, want 1", len(rows), sum.Recorded)
	}
	r := rows[0]
	for k, want := range map[string]any{
		"merge_commit_sha": testMergeSHA, "reverting_commit_sha": testRevertSHA, "detection": DetectionRevertTrailer,
		"attestation": AttestationInverseDiff, "pull_request_url": testPRURL, "pull_request_number": float64(42),
		"source": sourcePushWebhook, "delivery_id": "d-1", "pushed_ref": "refs/heads/main",
		"merged_at": "2026-09-01T12:00:00Z", "repo": "acme/widgets",
	} {
		if r[k] != want {
			t.Errorf("payload[%s] = %v, want %v", k, r[k], want)
		}
	}
	e := f.audit.rows(f.runID, CategoryRunMergeReverted)[0]
	if e.ActorSubject == nil || *e.ActorSubject != ActorSubject {
		t.Errorf("actor subject = %v, want %s", e.ActorSubject, ActorSubject)
	}
}

// TestRunMergeReverted_PayloadKeyAllowList: the recorded key set is exactly
// the forge-attested allow-list — no message, body, title or reason key.
func TestRunMergeReverted_PayloadKeyAllowList(t *testing.T) {
	f := newRevertFixture()
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d-1")
	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	var keys []string
	for k := range rows[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"attestation", "attestation_reason", "delivery_id", "detection", "merge_commit_sha", "merged_at",
		"observed_at", "pull_request_number", "pull_request_url", "push_after_sha", "pushed_ref", "repo",
		"reverting_commit_sha", "source"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("payload keys = %v, want %v", keys, want)
	}
	for _, v := range rows[0] {
		if s, ok := v.(string); ok && strings.Contains(s, "feat: add widgets") {
			t.Fatalf("payload carries commit-message text: %q", s)
		}
	}
}

// TestObservePush_FabricatedSubject_RecordsSignalOnly is operator condition 1:
// a `Revert "x (#42)"` subject on a commit whose diff does NOT invert PR 42's
// merge records attestation signal_only. Counterfactual: skip the diff
// comparison (ClassifyAttestation → inverse_diff) → RED.
func TestObservePush_FabricatedSubject_RecordsSignalOnly(t *testing.T) {
	f := newRevertFixture()
	f.forge.files[testOtherSHA] = &githubclient.CommitFiles{SHA: testOtherSHA, Files: []githubclient.CommitFile{
		{Path: "unrelated.go", Status: "modified", Additions: 1, Deletions: 1},
	}}
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testOtherSHA, Message: "Revert \"feat: add widgets (#42)\""}), "d-2")
	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (recorded in every case, with attestation)", len(rows))
	}
	if rows[0]["attestation"] != AttestationSignalOnly || rows[0]["detection"] != DetectionRevertSubject {
		t.Fatalf("attestation/detection = %v/%v, want signal_only/revert_subject", rows[0]["attestation"], rows[0]["detection"])
	}
}

// TestObservePush_TruncatedFileList_SignalOnly pins the amendment-approval
// condition: a truncated file list on either side yields signal_only, never
// inverse_diff, even when the visible files invert perfectly.
func TestObservePush_TruncatedFileList_SignalOnly(t *testing.T) {
	for _, side := range []string{testRevertSHA, testMergeSHA} {
		f := newRevertFixture()
		f.forge.files[side].Truncated = true
		f.obs.ObservePush(context.Background(),
			testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
		rows := f.rows(t)
		if len(rows) != 1 || rows[0]["attestation"] != AttestationSignalOnly || rows[0]["attestation_reason"] != attestReasonFilesTruncated {
			t.Fatalf("truncated %s: rows = %v, want one signal_only/file_list_truncated row", side, rows)
		}
	}
}

// TestObservePush_RealClient_MissingCounts_SignalOnly drives the observer over
// a REAL githubclient whose commit endpoint omits a file's additions count:
// the client marks the list truncated and the row is signal_only.
func TestObservePush_RealClient_MissingCounts_SignalOnly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/commits/"+testMergeSHA+"/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `[{"number":42,"html_url":%q,"merged_at":"2026-09-01T12:00:00Z"}]`, testPRURL)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/42", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"node_id":"PR_42","merged":true,"merge_commit_sha":%q,"merged_at":"2026-09-01T12:00:00Z"}`, testMergeSHA)
	})
	mux.HandleFunc("/repos/acme/widgets/commits/"+testMergeSHA, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"files":[{"filename":"a.go","additions":10,"deletions":2},{"filename":"b.go","additions":3,"deletions":0}]}`))
	})
	// a.go omits its additions count; b.go inverts. Were the incomplete entry
	// merely dropped, the visible b.go would read as partial_inverse.
	mux.HandleFunc("/repos/acme/widgets/commits/"+testRevertSHA, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"files":[{"filename":"a.go","deletions":10},{"filename":"b.go","additions":0,"deletions":3}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := githubclient.New(staticTokens{})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}

	f := newRevertFixture()
	f.obs.Forge = c
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
	rows := f.rows(t)
	if len(rows) != 1 || rows[0]["attestation"] != AttestationSignalOnly || rows[0]["attestation_reason"] != attestReasonFilesTruncated {
		t.Fatalf("rows = %v, want one signal_only/file_list_truncated row", rows)
	}
}

func TestObservePush_CommitFilesUnavailable_SignalOnly(t *testing.T) {
	f := newRevertFixture()
	delete(f.forge.files, testRevertSHA)
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
	rows := f.rows(t)
	if len(rows) != 1 || rows[0]["attestation_reason"] != attestReasonFilesUnavailable {
		t.Fatalf("rows = %v, want one signal_only/file_list_unavailable row", rows)
	}
}

// TestObservePush_NonDefaultBranch_RecordsNothing is control (1): the trailer
// names R's REAL merge SHA and the forge confirms it, so only the
// default-branch guard stops the row. Counterfactual: delete the guard → 1 row.
func TestObservePush_NonDefaultBranch_RecordsNothing(t *testing.T) {
	f := newRevertFixture()
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/feature", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
	if n := len(f.rows(t)); n != 0 || f.forge.total() != 0 {
		t.Fatalf("rows = %d forge calls = %d, want 0/0", n, f.forge.total())
	}
}

// TestObservePush_TrailerShaNotTheMergeCommit_RecordsNothing is control (2):
// the trailer SHA maps to R's merged PR (as GitHub does for a commit on the PR
// branch) but the PR's merge commit is a DIFFERENT SHA.
func TestObservePush_TrailerShaNotTheMergeCommit_RecordsNothing(t *testing.T) {
	f := newRevertFixture()
	f.forge.pullsForCommit[testOtherSHA] = []forge.PullRequestRef{{Number: 42, URL: testPRURL}}
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testOtherSHA)}), "d")
	if n := len(f.rows(t)); n != 0 {
		t.Fatalf("rows = %d, want 0 (merge_commit_sha does not match the trailer)", n)
	}
	if f.forge.getCalls != 1 {
		t.Fatalf("GetPullRequest calls = %d, want 1 (the candidate WAS resolved, then refused)", f.forge.getCalls)
	}
}

// TestObservePush_SubjectNamesUnmergedPR_RecordsNothing is control (3).
func TestObservePush_SubjectNamesUnmergedPR_RecordsNothing(t *testing.T) {
	f := newRevertFixture()
	f.forge.prs[42] = &forge.PullRequest{NodeID: "PR_42", State: "closed", Merged: false, MergeCommitSHA: testMergeSHA}
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: "Revert \"feat: add widgets (#42)\""}), "d")
	if n := len(f.rows(t)); n != 0 {
		t.Fatalf("rows = %d, want 0 (PR 42 never merged)", n)
	}
	if f.forge.getCalls != 1 {
		t.Fatalf("GetPullRequest calls = %d, want 1", f.forge.getCalls)
	}
}

func TestObservePush_MergedWithoutMergeCommit_RecordsNothing(t *testing.T) {
	f := newRevertFixture()
	f.forge.prs[42] = &forge.PullRequest{NodeID: "PR_42", Merged: true}
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: "Revert \"feat: add widgets (#42)\""}), "d")
	if n := len(f.rows(t)); n != 0 {
		t.Fatalf("rows = %d, want 0 (no merge_commit_sha)", n)
	}
}

// TestObservePush_RevertOfRevert_RecordsNothing is control (4) at the observer.
func TestObservePush_RevertOfRevert_RecordsNothing(t *testing.T) {
	f := newRevertFixture()
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: "Revert \"Revert \"feat: add widgets (#42)\"\""}), "d")
	if n := len(f.rows(t)); n != 0 || f.forge.total() != 0 {
		t.Fatalf("rows = %d forge calls = %d, want 0/0", n, f.forge.total())
	}
}

// TestObservePush_NoSignal_ZeroForgeCalls is control (5).
func TestObservePush_NoSignal_ZeroForgeCalls(t *testing.T) {
	f := newRevertFixture()
	sum := f.obs.ObservePush(context.Background(), testPush("refs/heads/main",
		PushCommit{ID: testRevertSHA, Message: "feat: add widgets (#42)\n\nreverts nothing"},
		PushCommit{ID: testOtherSHA, Message: "chore: bump"}), "d")
	if f.forge.total() != 0 || f.runs.calls != 0 || sum.Signals != 0 {
		t.Fatalf("forge calls = %d run lookups = %d signals = %d, want 0/0/0", f.forge.total(), f.runs.calls, sum.Signals)
	}
}

func TestObservePush_NoRunForPR_SkipsGetPullRequest(t *testing.T) {
	f := newRevertFixture()
	f.runs.byURL = map[string][]*run.Run{}
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
	if f.forge.getCalls != 0 || f.forge.filesCalls != 0 || len(f.rows(t)) != 0 {
		t.Fatalf("get = %d files = %d rows = %d, want 0/0/0 for a PR no run owns", f.forge.getCalls, f.forge.filesCalls, len(f.rows(t)))
	}
}

func TestObservePush_UnusableRepoOrInstallation_Skipped(t *testing.T) {
	for name, mutate := range map[string]func(*PushEvent){
		"no installation": func(p *PushEvent) { p.InstallationID = 0 },
		"bad full_name":   func(p *PushEvent) { p.FullName = "nope" },
		"no html_url":     func(p *PushEvent) { p.HTMLURL = "" },
	} {
		f := newRevertFixture()
		p := testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)})
		mutate(p)
		f.obs.ObservePush(context.Background(), p, "d")
		if f.forge.total() != 0 || len(f.rows(t)) != 0 {
			t.Errorf("%s: forge calls = %d rows = %d, want 0/0", name, f.forge.total(), len(f.rows(t)))
		}
	}
}

// TestObservePush_SignalCap_TruncatesAt50 is operator condition 2's cap: 51
// signals resolve 50 and log the named truncation.
func TestObservePush_SignalCap_TruncatesAt50(t *testing.T) {
	f := newRevertFixture()
	var commits []PushCommit
	for i := 0; i < DefaultMaxRevertSignals+1; i++ {
		commits = append(commits, PushCommit{ID: fmt.Sprintf("c%02d", i), Message: fmt.Sprintf("This reverts commit %07x0.", i+0x1000000)})
	}
	sum := f.obs.ObservePush(context.Background(), testPush("refs/heads/main", commits...), "d-cap")
	if f.forge.listCalls != DefaultMaxRevertSignals || !sum.Truncated || sum.Signals != 51 || sum.Resolved != 50 {
		t.Fatalf("list calls = %d summary = %+v, want 50 resolved of 51, truncated", f.forge.listCalls, sum)
	}
	if !strings.Contains(f.logs.String(), "revert_signals_truncated") {
		t.Fatalf("logs lack the named truncation:\n%s", f.logs.String())
	}
}

// TestObservePush_TransientForgeError_Retried: two 5xx then success records the
// row on the third attempt; a 404 is not retried.
func TestObservePush_TransientForgeError_Retried(t *testing.T) {
	f := newRevertFixture()
	f.forge.listErrs = []error{errors.New("githubclient: list pulls for commit: 502: x"), errors.New("githubclient: list pulls for commit: 503: y")}
	f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
	if f.forge.listCalls != 3 || len(f.rows(t)) != 1 {
		t.Fatalf("list calls = %d rows = %d, want 3 and 1", f.forge.listCalls, len(f.rows(t)))
	}

	g := newRevertFixture()
	g.forge.listErrs = []error{fmt.Errorf("%w: list pulls", forge.ErrNotFound)}
	sum := g.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
	if g.forge.listCalls != 1 || len(g.rows(t)) != 0 || sum.Errors != 1 {
		t.Fatalf("404: list calls = %d rows = %d errors = %d, want 1/0/1", g.forge.listCalls, len(g.rows(t)), sum.Errors)
	}
}

// TestObservePush_ErrorDoesNotAbortRemaining: a failing first signal is logged
// and the second still records.
func TestObservePush_ErrorDoesNotAbortRemaining(t *testing.T) {
	f := newRevertFixture()
	f.forge.listErrs = []error{fmt.Errorf("%w: list pulls", forge.ErrForbidden)}
	sum := f.obs.ObservePush(context.Background(), testPush("refs/heads/main",
		PushCommit{ID: testOtherSHA, Message: trailerMessage(testMergeSHA)},
		PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
	rows := f.rows(t)
	if len(rows) != 1 || rows[0]["reverting_commit_sha"] != testRevertSHA || sum.Errors != 1 {
		t.Fatalf("rows = %v errors = %d, want the second commit recorded and one error", rows, sum.Errors)
	}
}

func TestObservePush_RunLookupAndGetPRErrors_Logged(t *testing.T) {
	f := newRevertFixture()
	f.runs.err = errors.New("db down")
	sum := f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
	if sum.Errors != 1 || len(f.rows(t)) != 0 || f.forge.getCalls != 0 {
		t.Fatalf("run lookup error: summary %+v get calls %d", sum, f.forge.getCalls)
	}
	g := newRevertFixture()
	delete(g.forge.prs, 42)
	sum = g.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
	if sum.Errors != 1 || len(g.rows(t)) != 0 {
		t.Fatalf("get pr error: summary %+v", sum)
	}
}

// TestObservePush_RepeatedPush_RecordsOnce: the same reverting commit observed
// twice commits one row (the audit-layer dedup on reverting_commit_sha).
func TestObservePush_RepeatedPush_RecordsOnce(t *testing.T) {
	f := newRevertFixture()
	p := testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)})
	f.obs.ObservePush(context.Background(), p, "d-1")
	sum := f.obs.ObservePush(context.Background(), p, "d-2")
	if n := len(f.rows(t)); n != 1 || sum.Recorded != 0 {
		t.Fatalf("rows = %d second recorded = %d, want 1/0", n, sum.Recorded)
	}
}

func TestObservePush_AuditAppendError_Logged(t *testing.T) {
	f := newRevertFixture()
	f.audit.listErr = errors.New("db down")
	sum := f.obs.ObservePush(context.Background(),
		testPush("refs/heads/main", PushCommit{ID: testRevertSHA, Message: trailerMessage(testMergeSHA)}), "d")
	if sum.Errors != 1 || sum.Recorded != 0 {
		t.Fatalf("summary %+v, want one error and nothing recorded", sum)
	}
}

func TestRevertObserver_Defaults(t *testing.T) {
	o := &RevertObserver{}
	if o.logger() == nil {
		t.Error("nil Logger must default")
	}
	if time.Since(o.now()) > time.Minute {
		t.Error("nil Now must default to the wall clock")
	}
}
