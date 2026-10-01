package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Unit coverage for the run-admission document-base capture (E55.7 / #3746):
// one test per degrade mode, each asserting a NIL result AND the WARN reason
// that names it (several modes share the nil outcome, so the reason is what
// isolates each branch), plus the happy path, the CreateRunForTrigger stamp and
// the read-only API exposure.

const (
	// dbcHead is the default-branch head the fake commit resolver reports, in
	// UPPERCASE so the happy path also proves the stored value is lowercased
	// (the runs CHECK constraint accepts lowercase only).
	dbcHead      = "ABCDEF0123456789ABCDEF0123456789ABCDEF01"
	dbcHeadLower = "abcdef0123456789abcdef0123456789abcdef01"
	dbcBranch    = "main"
)

// dbcCommits is a repodoc commit resolver fake. It counts GetBranchSHA calls,
// and when block is set it waits for the caller's context to end (the timeout
// degrade) or for release to close (test teardown under a deleted deadline).
type dbcCommits struct {
	mu      sync.Mutex
	calls   int
	sha     string
	found   bool
	err     error
	block   bool
	release chan struct{}
}

func (c *dbcCommits) GetBranchSHA(ctx context.Context, _ forge.CredentialScope, _ forge.RepoRef, _ string) (string, bool, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.block {
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-c.release:
			return "", false, errors.New("released by test teardown")
		}
	}
	return c.sha, c.found, c.err
}

func (c *dbcCommits) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// dbcFetcher fails the test on any fetch: the capture pins a commit and must
// never read a document.
type dbcFetcher struct{ t *testing.T }

func (f dbcFetcher) FetchFile(context.Context, forge.CredentialScope, forge.RepoRef, string, string) (*forge.FileContent, error) {
	f.t.Errorf("document-base capture fetched a file; it must only pin a commit")
	return nil, forge.ErrNotFound
}

// dbcSeam is one configuration of the four document-seam members the capture
// reads, plus call counters for the scope and base-ref hooks.
type dbcSeam struct {
	commits      *dbcCommits
	noResolver   bool
	noBaseRef    bool
	scopeErr     error
	baseRef      string
	baseRefErr   error
	scopeCalls   int
	baseRefCalls int
}

// newDBCServer builds a Server with the seam configured per sc and a JSON
// logger whose output the caller inspects for the degrade reason.
func newDBCServer(t *testing.T, sc *dbcSeam) (*Server, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	cfg := Config{
		Addr:   "127.0.0.1:0",
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)),
		DocumentScope: func(context.Context, forge.RepoRef) (forge.CredentialScope, error) {
			sc.scopeCalls++
			if sc.scopeErr != nil {
				return forge.CredentialScope{}, sc.scopeErr
			}
			return forge.FromGitHubInstallationID(7), nil
		},
	}
	if !sc.noResolver {
		cfg.DocumentResolver = &repodoc.Resolver{Fetcher: dbcFetcher{t: t}, Commits: sc.commits}
	}
	if !sc.noBaseRef {
		cfg.DocumentBaseRef = func(context.Context, forge.RepoRef) (string, error) {
			sc.baseRefCalls++
			return sc.baseRef, sc.baseRefErr
		}
	}
	return New(cfg), &buf
}

// assertDBCReason fails unless the captured log carries a WARN with exactly
// the given reason.
func assertDBCReason(t *testing.T, buf *bytes.Buffer, reason string) {
	t.Helper()
	want := `"reason":"` + reason + `"`
	if !strings.Contains(buf.String(), want) {
		t.Errorf("log lacks %s:\n%s", want, buf.String())
	}
}

func dbcHappyCommits() *dbcCommits {
	return &dbcCommits{sha: dbcHead, found: true}
}

// TestCaptureDocumentBaseCommit_Degrades pins every NIL degrade mode, each with
// its own reason. The fake resolver would otherwise pin dbcHead successfully,
// so each cell's single fault is the only thing standing between it and a
// recorded commit.
func TestCaptureDocumentBaseCommit_Degrades(t *testing.T) {
	cases := []struct {
		name       string
		repo       string
		seam       *dbcSeam
		reason     string
		wantPins   int // expected GetBranchSHA calls
		wantLookup bool
	}{
		{
			name: "seam unwired: nil resolver", repo: "o/r",
			seam:   &dbcSeam{commits: dbcHappyCommits(), noResolver: true, baseRef: dbcBranch},
			reason: documentBaseReasonSeamUnwired,
		},
		{
			name: "seam unwired: nil base-ref hook", repo: "o/r",
			seam:   &dbcSeam{commits: dbcHappyCommits(), noBaseRef: true},
			reason: documentBaseReasonSeamUnwired,
		},
		{
			name: "unparseable repo", repo: "no-slash",
			seam:   &dbcSeam{commits: dbcHappyCommits(), baseRef: dbcBranch},
			reason: documentBaseReasonUnparseableRepo,
		},
		{
			name: "credential scope error", repo: "o/r",
			seam:   &dbcSeam{commits: dbcHappyCommits(), baseRef: dbcBranch, scopeErr: errors.New("no installation for o/r")},
			reason: documentBaseReasonScopeFailed,
		},
		{
			name: "base-ref lookup error", repo: "o/r",
			seam:   &dbcSeam{commits: dbcHappyCommits(), baseRefErr: errors.New("GetRepository: 502")},
			reason: documentBaseReasonBaseRefFailed, wantLookup: true,
		},
		{
			name: "empty default branch", repo: "o/r",
			seam:   &dbcSeam{commits: dbcHappyCommits(), baseRef: "  "},
			reason: documentBaseReasonEmptyBaseRef, wantLookup: true,
		},
		{
			name: "GetBranchSHA found=false", repo: "o/r",
			seam:   &dbcSeam{commits: &dbcCommits{sha: "", found: false}, baseRef: dbcBranch},
			reason: documentBaseReasonPinFailed, wantPins: 1, wantLookup: true,
		},
		{
			name: "non-commit resolver output", repo: "o/r",
			seam:   &dbcSeam{commits: &dbcCommits{sha: "HEAD", found: true}, baseRef: dbcBranch},
			reason: documentBaseReasonPinFailed, wantPins: 1, wantLookup: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, buf := newDBCServer(t, tc.seam)
			got := s.captureDocumentBaseCommit(context.Background(), tc.repo)
			if got != nil {
				t.Fatalf("captured %q, want nil on degrade", *got)
			}
			assertDBCReason(t, buf, tc.reason)
			if n := tc.seam.commits.callCount(); n != tc.wantPins {
				t.Errorf("GetBranchSHA calls = %d, want %d", n, tc.wantPins)
			}
			if lookedUp := tc.seam.baseRefCalls > 0; lookedUp != tc.wantLookup {
				t.Errorf("base-ref lookups = %d, want lookup=%v", tc.seam.baseRefCalls, tc.wantLookup)
			}
		})
	}
}

// TestCaptureDocumentBaseCommit_SeamUnwiredMakesNoForgeCalls pins that an
// unwired seam is decided BEFORE any forge round-trip: no scope resolution, no
// base-ref lookup, no commit pin.
func TestCaptureDocumentBaseCommit_SeamUnwiredMakesNoForgeCalls(t *testing.T) {
	sc := &dbcSeam{commits: dbcHappyCommits(), noResolver: true, baseRef: dbcBranch}
	s, _ := newDBCServer(t, sc)
	if got := s.captureDocumentBaseCommit(context.Background(), "o/r"); got != nil {
		t.Fatalf("captured %q, want nil", *got)
	}
	if sc.scopeCalls != 0 || sc.baseRefCalls != 0 || sc.commits.callCount() != 0 {
		t.Errorf("forge calls with an unwired seam: scope=%d baseRef=%d pin=%d, want all 0",
			sc.scopeCalls, sc.baseRefCalls, sc.commits.callCount())
	}
}

// TestCaptureDocumentBaseCommit_TimeoutDegrades pins the capture deadline: a
// commit resolver that never answers degrades to nil within the (shrunk)
// documentBaseCaptureTimeout instead of hanging the run create. The outer
// select is the test's own guard — with the deadline deleted the capture never
// returns and the test fails on it rather than hanging.
func TestCaptureDocumentBaseCommit_TimeoutDegrades(t *testing.T) {
	orig := documentBaseCaptureTimeout
	documentBaseCaptureTimeout = 50 * time.Millisecond
	t.Cleanup(func() { documentBaseCaptureTimeout = orig })

	commits := &dbcCommits{block: true, release: make(chan struct{})}
	t.Cleanup(func() { close(commits.release) })
	s, buf := newDBCServer(t, &dbcSeam{commits: commits, baseRef: dbcBranch})

	done := make(chan *string, 1)
	go func() { done <- s.captureDocumentBaseCommit(context.Background(), "o/r") }()
	select {
	case got := <-done:
		if got != nil {
			t.Fatalf("captured %q, want nil on timeout", *got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not return within 10s — the documentBaseCaptureTimeout deadline is not applied")
	}
	assertDBCReason(t, buf, documentBaseReasonPinFailed)
	if !strings.Contains(buf.String(), context.DeadlineExceeded.Error()) {
		t.Errorf("log does not name the deadline:\n%s", buf.String())
	}
}

// TestCaptureDocumentBaseCommit_HappyPath pins the success shape through both
// the internal and the exported (dispatcher-wired) entry points: the
// default-branch name is pinned through the commit resolver and the recorded
// value is the LOWERCASE 40-hex commit.
func TestCaptureDocumentBaseCommit_HappyPath(t *testing.T) {
	for _, exported := range []bool{false, true} {
		sc := &dbcSeam{commits: dbcHappyCommits(), baseRef: dbcBranch}
		s, buf := newDBCServer(t, sc)
		var got *string
		if exported {
			got = s.CaptureDocumentBaseCommit(context.Background(), "o/r")
		} else {
			got = s.captureDocumentBaseCommit(context.Background(), "o/r")
		}
		if got == nil {
			t.Fatalf("exported=%v: captured nil, want %q; log:\n%s", exported, dbcHeadLower, buf.String())
		}
		if *got != dbcHeadLower {
			t.Errorf("exported=%v: captured %q, want %q", exported, *got, dbcHeadLower)
		}
		if sc.commits.callCount() != 1 {
			t.Errorf("exported=%v: GetBranchSHA calls = %d, want 1", exported, sc.commits.callCount())
		}
		if strings.Contains(buf.String(), `"reason"`) {
			t.Errorf("exported=%v: happy path logged a degrade:\n%s", exported, buf.String())
		}
	}
}

// --- the document_base_commit STAMP at the CreateRunForTrigger seam

// dbcCaptureRepo records every CreateRunParams handed to CreateRun, so the
// stamp assertion reads exactly what the mint seam asked the repository to
// persist.
type dbcCaptureRepo struct {
	*driveE2ERepo
	mu     sync.Mutex
	params []run.CreateRunParams
}

func (r *dbcCaptureRepo) CreateRun(ctx context.Context, p run.CreateRunParams) (*run.Run, error) {
	r.mu.Lock()
	r.params = append(r.params, p)
	r.mu.Unlock()
	return r.driveE2ERepo.CreateRun(ctx, p)
}

// TestCreateRunForTrigger_StampsDocumentBaseCommit pins the stamp on the single
// integrating mint seam (POST /v0/runs and the campaign item-run start): the
// CreateRunParams carry the captured lowercase head when the seam resolves, and
// NIL (with the run still minted) when the seam is unwired.
//
// COUNTERFACTUAL: delete `createParams.DocumentBaseCommit = ...` in
// CreateRunForTrigger and the "captured" cell goes RED on the nil check.
func TestCreateRunForTrigger_StampsDocumentBaseCommit(t *testing.T) {
	cases := []struct {
		name string
		seam *dbcSeam
		want *string
	}{
		{name: "captured head stamped", seam: &dbcSeam{commits: dbcHappyCommits(), baseRef: dbcBranch}, want: ptrString(dbcHeadLower)},
		{name: "unwired seam stamps nil", seam: &dbcSeam{commits: dbcHappyCommits(), noResolver: true, noBaseRef: true}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newDBCServer(t, tc.seam)
			repo := &dbcCaptureRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
			s.cfg.RunRepo = repo
			s.cfg.AuditRepo = newAuditFake()
			ref := "issue:3746"
			if _, err := s.CreateRunForTrigger(context.Background(), CreateRunForTriggerParams{
				Repo: "o/r", WorkflowID: "feature_change", WorkflowSHA: "abc",
				TriggerSource: run.TriggerCLI, TriggerRef: &ref, RunnerKind: run.RunnerKindLocal,
			}); err != nil {
				t.Fatalf("CreateRunForTrigger: %v — the capture must never fail admission", err)
			}
			if len(repo.params) != 1 {
				t.Fatalf("CreateRun calls = %d, want 1", len(repo.params))
			}
			got := repo.params[0].DocumentBaseCommit
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("stamped document_base_commit = %q, want nil", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("stamped document_base_commit = nil, want %q", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Errorf("stamped document_base_commit = %q, want %q", *got, *tc.want)
			}
		})
	}
}

// TestRunResponse_DocumentBaseCommitExposure pins the read-only API exposure:
// a recorded commit renders under document_base_commit, and a row with no
// recorded commit omits the key rather than rendering null or "".
func TestRunResponse_DocumentBaseCommitExposure(t *testing.T) {
	commit := dbcHeadLower
	cases := []struct {
		name string
		val  *string
		want string
	}{
		{name: "recorded commit renders", val: &commit, want: `"document_base_commit":"` + dbcHeadLower + `"`},
		{name: "nil omitted", val: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &run.Run{ID: uuid.New(), Repo: "o/r", WorkflowID: "feature_change", DocumentBaseCommit: tc.val}
			b, err := json.Marshal(toRunResponse(r))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			body := string(b)
			if tc.want == "" {
				if strings.Contains(body, "document_base_commit") {
					t.Errorf("document_base_commit present on a row with no recorded commit:\n%s", body)
				}
				return
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("response lacks %s:\n%s", tc.want, body)
			}
		})
	}
}
