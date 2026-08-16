package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// ---------------------------------------------------------------------------
// #2748 — migration-renumber RECOGNITION. One test per refusal branch, driven
// against REAL files in a t.TempDir so the os.Lstat / os.ReadDir behaviour is
// exercised rather than mocked.
// ---------------------------------------------------------------------------

// renumberRepo materialises the named repo-relative files (empty content) under
// a fresh temp dir and returns its path.
func renumberRepo(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("-- sql\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const (
	migDir     = "backend/internal/postgres/migrations"
	declaredUp = migDir + "/0070_campaigns_working_dir.up.sql"
	declaredDn = migDir + "/0070_campaigns_working_dir.down.sql"
	createdUp  = migDir + "/0071_campaigns_working_dir.up.sql"
	createdDn  = migDir + "/0071_campaigns_working_dir.down.sql"
)

var declaredPair = []string{declaredUp, declaredDn}

// TestRecognizeMigrationRenumbers_PositiveCase is the recognized shape: the
// declared 0070 pair is absent, exactly one same-slug 0071 pair exists on disk.
// It also pins that the returned paths are byte-identical to the
// slash-separated scope.files entries (a leaked filepath separator would fail
// here on any host).
func TestRecognizeMigrationRenumbers_PositiveCase(t *testing.T) {
	repo := renumberRepo(t, createdUp, createdDn)
	subs, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("substitutions = %d, want 1: %+v", len(subs), subs)
	}
	s := subs[0]
	if s.DeclaredUp != declaredUp || s.DeclaredDown != declaredDn ||
		s.CreatedUp != createdUp || s.CreatedDown != createdDn {
		t.Fatalf("substitution paths = %+v", s)
	}
	if s.DeclaredNumber != "0070" || s.CreatedNumber != "0071" || s.Slug != "campaigns_working_dir" {
		t.Errorf("substitution parts = %+v", s)
	}
}

// TestRecognizeMigrationRenumbers_NonMigrationsDirectory is C1's vehicle: a
// migration-shaped pair whose parent directory basename is not exactly
// `migrations` is NOT recognized.
func TestRecognizeMigrationRenumbers_NonMigrationsDirectory(t *testing.T) {
	for _, dir := range []string{
		"backend/testdata/migrations-fixtures",
		"backend/fixtures",
		"backend/internal/postgres/migrations-old",
	} {
		t.Run(dir, func(t *testing.T) {
			decl := []string{dir + "/0070_slug.up.sql", dir + "/0070_slug.down.sql"}
			repo := renumberRepo(t, dir+"/0071_slug.up.sql", dir+"/0071_slug.down.sql")
			subs, err := recognizeMigrationRenumbers(repo, decl)
			if err != nil {
				t.Fatalf("recognize: %v", err)
			}
			if len(subs) != 0 {
				t.Fatalf("a pair under %s must not be recognized, got %+v", dir, subs)
			}
		})
	}
}

// TestRecognizeMigrationRenumbers_MismatchedCreatedPair is one of C2's vehicles:
// an on-disk up at 0071 with a down at 0072 is not a coherent pair.
func TestRecognizeMigrationRenumbers_MismatchedCreatedPair(t *testing.T) {
	repo := renumberRepo(t, createdUp, migDir+"/0072_campaigns_working_dir.down.sql")
	subs, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("a mismatched up/down pair must not be recognized, got %+v", subs)
	}
}

// TestRecognizeMigrationRenumbers_CreatedHalfPair is C2's other vehicle: only
// the up file was created.
func TestRecognizeMigrationRenumbers_CreatedHalfPair(t *testing.T) {
	repo := renumberRepo(t, createdUp)
	subs, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("a created half-pair must not be recognized, got %+v", subs)
	}
}

// TestRecognizeMigrationRenumbers_DeclaredHalfPair: a declared half-pair is
// never even a candidate, so a complete on-disk pair beside it is not offered.
func TestRecognizeMigrationRenumbers_DeclaredHalfPair(t *testing.T) {
	repo := renumberRepo(t, createdUp, createdDn)
	subs, err := recognizeMigrationRenumbers(repo, []string{declaredUp})
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("a declared half-pair must not be a candidate, got %+v", subs)
	}
}

// TestRecognizeMigrationRenumbers_SlugMatchesNoDeclaredEntry: a created
// migration whose slug matches no declared entry is invisible to recognition —
// the gate must still fail the stage on it.
func TestRecognizeMigrationRenumbers_SlugMatchesNoDeclaredEntry(t *testing.T) {
	repo := renumberRepo(t,
		migDir+"/0071_unrelated_slug.up.sql",
		migDir+"/0071_unrelated_slug.down.sql")
	subs, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("an unrelated slug must not be recognized, got %+v", subs)
	}
}

// TestRecognizeMigrationRenumbers_DeclaredPresentOnDisk is C3's vehicle: the
// declared pair EXISTS in the work tree, so nothing was renumbered and no
// substitution may be offered even though a 0071 pair is also present.
func TestRecognizeMigrationRenumbers_DeclaredPresentOnDisk(t *testing.T) {
	repo := renumberRepo(t, declaredUp, declaredDn, createdUp, createdDn)
	subs, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("a present declared pair must disqualify the candidate, got %+v", subs)
	}
}

// TestRecognizeMigrationRenumbers_DeclaredIsDanglingSymlink pins the os.Lstat
// (not os.Stat) choice: a dangling symlink at a declared path counts as
// PRESENT, the fail-closed direction. Substituting os.Stat would make this
// candidate recognized.
func TestRecognizeMigrationRenumbers_DeclaredIsDanglingSymlink(t *testing.T) {
	repo := renumberRepo(t, createdUp, createdDn)
	for _, p := range declaredPair {
		if err := os.Symlink(filepath.Join(repo, "nowhere.sql"), filepath.Join(repo, filepath.FromSlash(p))); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
	}
	subs, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("a dangling symlink at a declared path must count as PRESENT, got %+v", subs)
	}
}

// TestRecognizeMigrationRenumbers_CreatedPairAlreadyDeclared: an
// already-folded created pair needs no amendment.
func TestRecognizeMigrationRenumbers_CreatedPairAlreadyDeclared(t *testing.T) {
	repo := renumberRepo(t, createdUp, createdDn)
	subs, err := recognizeMigrationRenumbers(repo, append(append([]string{}, declaredPair...), createdUp, createdDn))
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("an already-declared created pair must not be re-offered, got %+v", subs)
	}
}

// TestRecognizeMigrationRenumbers_AmbiguousCandidates is C4's vehicle: TWO
// complete same-slug on-disk pairs mean the runner cannot tell which one is the
// renumber, so nothing is offered.
func TestRecognizeMigrationRenumbers_AmbiguousCandidates(t *testing.T) {
	repo := renumberRepo(t, createdUp, createdDn,
		migDir+"/0072_campaigns_working_dir.up.sql",
		migDir+"/0072_campaigns_working_dir.down.sql")
	subs, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("two on-disk candidates must disqualify the candidate, got %+v", subs)
	}
}

// TestRecognizeMigrationRenumbers_DuplicateClaim is C5's vehicle: two DECLARED
// pairs sharing a slug both resolve to the SAME on-disk created pair, so the
// recognition is ambiguous and the WHOLE result is rejected — not just the
// offending entry.
func TestRecognizeMigrationRenumbers_DuplicateClaim(t *testing.T) {
	decl := []string{
		declaredUp, declaredDn,
		migDir + "/0080_campaigns_working_dir.up.sql",
		migDir + "/0080_campaigns_working_dir.down.sql",
		// An unrelated slug whose substitution WOULD be clean on its own; it
		// must be dropped too, proving the sweep rejects the whole result.
		migDir + "/0090_other_thing.up.sql",
		migDir + "/0090_other_thing.down.sql",
	}
	repo := renumberRepo(t, createdUp, createdDn,
		migDir+"/0091_other_thing.up.sql",
		migDir+"/0091_other_thing.down.sql")
	subs, err := recognizeMigrationRenumbers(repo, decl)
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("a cross-substitution 1:1 violation must reject the whole result, got %+v", subs)
	}
}

// skipIfRoot skips permission-based fixtures under a uid that ignores mode bits.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("running as root: mode bits do not deny access")
	}
}

// TestRecognizeMigrationRenumbers_ReadDirError: an unreadable-but-searchable
// migrations directory (mode 0111) lets the declared-absence Lstat succeed and
// fails the directory scan, which returns an error the CALLER turns into
// fail-open.
func TestRecognizeMigrationRenumbers_ReadDirError(t *testing.T) {
	skipIfRoot(t)
	repo := renumberRepo(t, createdUp, createdDn)
	dir := filepath.Join(repo, filepath.FromSlash(migDir))
	if err := os.Chmod(dir, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	// The MESSAGE discriminates which control raised it: an assertion on
	// err != nil alone would stay green under a mutation that swaps one error
	// source for the other.
	_, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err == nil || !strings.Contains(err.Error(), "read migrations dir") {
		t.Fatalf("err = %v, want the directory-scan error (fail-closed at the predicate)", err)
	}
}

// TestRecognizeMigrationRenumbers_LstatError: a migrations directory with no
// search permission (mode 0000) makes the declared-absence Lstat fail with a
// non-ErrNotExist error, which must NOT read as "absent".
func TestRecognizeMigrationRenumbers_LstatError(t *testing.T) {
	skipIfRoot(t)
	repo := renumberRepo(t, createdUp, createdDn)
	dir := filepath.Join(repo, filepath.FromSlash(migDir))
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	_, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err == nil || !strings.Contains(err.Error(), "stat declared migration") {
		t.Fatalf("err = %v, want the declared-path stat error — an unstatable declared path must never read as absent", err)
	}
}

// TestRecognizeMigrationRenumbers_MissingDirectoryIsNotAnError: a declared
// migrations directory that does not exist at all cannot hold a created pair,
// so the candidate is disqualified WITHOUT an error (no spurious fail-open log
// on the overwhelmingly common no-collision run).
func TestRecognizeMigrationRenumbers_MissingDirectoryIsNotAnError(t *testing.T) {
	repo := renumberRepo(t, "README.md")
	subs, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err != nil {
		t.Fatalf("a missing migrations dir must not error: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("substitutions = %+v, want none", subs)
	}
}

// TestRecognizeMigrationRenumbers_FreshSliceEachInvocation: a second call
// re-derives from the work tree as it stands THEN. No package state, no cached
// listing.
func TestRecognizeMigrationRenumbers_FreshSliceEachInvocation(t *testing.T) {
	repo := renumberRepo(t, createdUp, createdDn)
	first, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err != nil || len(first) != 1 {
		t.Fatalf("first = %+v err=%v", first, err)
	}
	// The renumber is undone between calls (the declared path reappears).
	for _, p := range declaredPair {
		if werr := os.WriteFile(filepath.Join(repo, filepath.FromSlash(p)), []byte("-- sql\n"), 0o644); werr != nil {
			t.Fatal(werr)
		}
	}
	second, err := recognizeMigrationRenumbers(repo, declaredPair)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second invocation must re-derive from the CURRENT tree, got %+v", second)
	}
	if len(first) != 1 {
		t.Fatalf("the first result must not be mutated by the second call, got %+v", first)
	}
}

// ---------------------------------------------------------------------------
// #2748 — the PARK DRIVER. One test per fail-open branch, each asserting that
// branch's observable behaviour (return value + the decision event).
// ---------------------------------------------------------------------------

// pinRenumberBudget shrinks the decision budget + poll cadence so the
// undecided-at-budget branch resolves in milliseconds rather than 15 minutes.
func pinRenumberBudget(t *testing.T, budget time.Duration) {
	t.Helper()
	origB, origP := migrationRenumberDecisionBudget, migrationRenumberPollInterval
	migrationRenumberDecisionBudget = budget
	migrationRenumberPollInterval = time.Millisecond
	t.Cleanup(func() {
		migrationRenumberDecisionBudget = origB
		migrationRenumberPollInterval = origP
	})
}

func renumberDriverCfg() *config {
	return &config{
		runID:   "11112222333344445555666677778888",
		stageID: "99990000aaaabbbbccccddddeeeeffff",
		scopeFiles: []upload.ScopeFile{
			{Path: declaredUp, Operation: "create"},
			{Path: declaredDn, Operation: "create"},
		},
	}
}

var renumberSubs = []migrationRenumber{{
	Dir: migDir, Slug: "campaigns_working_dir",
	DeclaredNumber: "0070", CreatedNumber: "0071",
	DeclaredUp: declaredUp, DeclaredDown: declaredDn,
	CreatedUp: createdUp, CreatedDown: createdDn,
}}

// decidedRow is the amendment row the fake returns once the operator answered.
func decidedRow(status string) []upload.ScopeAmendment {
	return []upload.ScopeAmendment{{
		ID:     "amd-renumber",
		Status: status,
		Paths: []upload.ScopeAmendmentPath{
			{Path: createdUp, Operation: "create"},
			{Path: createdDn, Operation: "create"},
		},
	}}
}

// runRenumberDriver drives the park driver and returns its result plus the log.
func runRenumberDriver(t *testing.T, ctx context.Context, fu uploadClient, cfg *config, token string) (bool, []scopeExemption, string) {
	t.Helper()
	var log strings.Builder
	approved, exemptions := parkForMigrationRenumberAmendment(ctx, fu, cfg, token, renumberSubs, &log)
	return approved, exemptions, log.String()
}

func assertDecidedEvent(t *testing.T, log, decision string) {
	t.Helper()
	if !strings.Contains(log, `"event":"migration_renumber_amendment_decided"`) {
		t.Fatalf("missing the decision event:\n%s", log)
	}
	if !strings.Contains(log, `"decision":"`+decision+`"`) {
		t.Errorf("decision event must name %q:\n%s", decision, log)
	}
}

// TestParkForMigrationRenumber_Approved: the operator approves — the created
// paths are folded via refreshScopeAmendments, both trace events are emitted,
// and one exemption per SUBSTITUTED DECLARED path is returned.
func TestParkForMigrationRenumber_Approved(t *testing.T) {
	pinRenumberBudget(t, time.Minute)
	fu := newFakeUploader(t)
	fu.amendmentsAfterRequest = decidedRow("approved")
	cfg := renumberDriverCfg()

	approved, exemptions, log := runRenumberDriver(t, context.Background(), fu, cfg, "fhm_token")
	if !approved {
		t.Fatalf("approved = false, want true:\n%s", log)
	}
	if len(exemptions) != 2 ||
		exemptions[0].Path != declaredUp || exemptions[1].Path != declaredDn {
		t.Fatalf("exemptions = %+v, want the two SUBSTITUTED DECLARED paths", exemptions)
	}
	for _, e := range exemptions {
		if !strings.Contains(e.Reason, "0070") || !strings.Contains(e.Reason, "0071") {
			t.Errorf("exemption reason must name the substitution: %q", e.Reason)
		}
	}
	// The fold really ran: the created paths are now in cfg.scopeFiles.
	got := scopePaths(cfg.scopeFiles)
	for _, want := range []string{createdUp, createdDn} {
		if !containsString(got, want) {
			t.Errorf("cfg.scopeFiles = %v, missing folded %s", got, want)
		}
	}
	// BOTH events on the approve path.
	if !strings.Contains(log, `"event":"migration_renumber_recognized"`) {
		t.Errorf("missing the recognition event:\n%s", log)
	}
	assertDecidedEvent(t, log, "approved")
	// The amendment named ONLY the recognized created paths.
	if len(fu.gotRequestAmendmentArgs) != 1 {
		t.Fatalf("RequestScopeAmendment calls = %d, want 1", len(fu.gotRequestAmendmentArgs))
	}
	req := fu.gotRequestAmendmentArgs[0]
	if len(req.Paths) != 2 || req.Paths[0].Path != createdUp || req.Paths[0].Operation != "create" ||
		req.Paths[1].Path != createdDn || req.Paths[1].Operation != "create" {
		t.Errorf("request paths = %+v, want exactly the two created paths with operation create", req.Paths)
	}
	if !strings.Contains(req.Reason, declaredUp) || !strings.Contains(req.Reason, createdUp) {
		t.Errorf("request reason must spell out the substitution: %q", req.Reason)
	}
	if req.MCPToken != "fhm_token" {
		t.Errorf("request MCPToken = %q, want the run-bound bearer", req.MCPToken)
	}
}

// TestParkForMigrationRenumber_FailOpenBranches covers every non-approved
// branch named in the approach, each asserting its own observable behaviour:
// the (false, nil) return, the named decision, and — for the branches that
// never reach the POST — that no request was made.
func TestParkForMigrationRenumber_FailOpenBranches(t *testing.T) {
	cases := []struct {
		name        string
		token       string
		nilClient   bool
		setup       func(*fakeUploader)
		ctx         func() (context.Context, context.CancelFunc)
		decision    string
		wantNoPost  bool
		wantDetails string
	}{
		{
			name: "denied", token: "fhm_token", decision: "denied",
			setup: func(f *fakeUploader) { f.amendmentsAfterRequest = decidedRow("denied") },
		},
		{
			name: "undecided_at_budget", token: "fhm_token", decision: "undecided",
			setup: func(f *fakeUploader) { f.amendmentsAfterRequest = decidedRow("pending") },
		},
		{
			name: "post_422_budget_exhausted", token: "fhm_token", decision: "unavailable",
			setup:       func(f *fakeUploader) { f.requestAmendmentErr = upload.ErrAmendmentBudgetExhausted },
			wantDetails: "budget exhausted",
		},
		{
			name: "post_409_stage_not_implement", token: "fhm_token", decision: "unavailable",
			setup:       func(f *fakeUploader) { f.requestAmendmentErr = upload.ErrStageNotImplement },
			wantDetails: "executing implement stage",
		},
		{
			name: "post_403_forbidden", token: "fhm_token", decision: "unavailable",
			setup:       func(f *fakeUploader) { f.requestAmendmentErr = upload.ErrScopeAmendmentForbidden },
			wantDetails: "may not request scope amendments",
		},
		{
			name: "post_returns_no_amendment", token: "fhm_token", decision: "unavailable",
			setup:       func(f *fakeUploader) { f.requestAmendmentNil = true },
			wantDetails: "returned no amendment",
		},
		{
			name: "fetch_failure", token: "fhm_token", decision: "unavailable",
			setup:       func(f *fakeUploader) { f.amendmentsErr = errors.New("backend unreachable") },
			wantDetails: "backend unreachable",
		},
		{
			name: "ctx_cancelled", token: "fhm_token", decision: "unavailable",
			setup: func(f *fakeUploader) { f.amendmentsAfterRequest = decidedRow("pending") },
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			wantDetails: "context canceled",
		},
		{
			name: "empty_token", token: "", decision: "unavailable", wantNoPost: true,
			wantDetails: "no run-bound amendment client",
		},
		{
			name: "nil_client", token: "fhm_token", nilClient: true, decision: "unavailable", wantNoPost: true,
			wantDetails: "no run-bound amendment client",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pinRenumberBudget(t, 5*time.Millisecond)
			fu := newFakeUploader(t)
			if tc.setup != nil {
				tc.setup(fu)
			}
			var client uploadClient = fu
			if tc.nilClient {
				client = nil
			}
			ctx := context.Background()
			if tc.ctx != nil {
				c, cancel := tc.ctx()
				defer cancel()
				ctx = c
			}
			cfg := renumberDriverCfg()
			before := len(cfg.scopeFiles)

			approved, exemptions, log := runRenumberDriver(t, ctx, client, cfg, tc.token)
			if approved || exemptions != nil {
				t.Fatalf("branch %s must fail open: approved=%v exemptions=%+v", tc.name, approved, exemptions)
			}
			if len(cfg.scopeFiles) != before {
				t.Errorf("a fail-open branch must not widen scope.files: %v", scopePaths(cfg.scopeFiles))
			}
			assertDecidedEvent(t, log, tc.decision)
			if tc.wantDetails != "" && !strings.Contains(log, tc.wantDetails) {
				t.Errorf("decision detail must name the cause %q:\n%s", tc.wantDetails, log)
			}
			if tc.wantNoPost && len(fu.gotRequestAmendmentArgs) != 0 {
				t.Errorf("branch %s must not POST, got %d request(s)", tc.name, len(fu.gotRequestAmendmentArgs))
			}
		})
	}
}

// TestMaybeParkForMigrationRenumber_ScanErrorFailsOpen: a recognition error
// (unreadable migrations directory) logs one reason line and offers no park.
func TestMaybeParkForMigrationRenumber_ScanErrorFailsOpen(t *testing.T) {
	skipIfRoot(t)
	repo := renumberRepo(t, createdUp, createdDn)
	dir := filepath.Join(repo, filepath.FromSlash(migDir))
	if err := os.Chmod(dir, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	fu := newFakeUploader(t)
	cfg := renumberDriverCfg()
	cfg.workingDir = repo
	var log strings.Builder
	if got, _ := maybeParkForMigrationRenumber(context.Background(), fu, cfg, "fhm_token", nil, &log); got != nil {
		t.Fatalf("a scan error must fail open, got %+v", got)
	}
	if !strings.Contains(log.String(), `"event":"migration_renumber_scan_skipped"`) {
		t.Errorf("a scan error must log its reason:\n%s", log.String())
	}
	if len(fu.gotRequestAmendmentArgs) != 0 {
		t.Errorf("a scan error must not POST an amendment")
	}
}

// TestMaybeParkForMigrationRenumber_NoRecognitionIsSilent: the overwhelmingly
// common case — nothing recognized — makes no request and writes no event.
func TestMaybeParkForMigrationRenumber_NoRecognitionIsSilent(t *testing.T) {
	repo := renumberRepo(t, declaredUp, declaredDn)
	fu := newFakeUploader(t)
	cfg := renumberDriverCfg()
	cfg.workingDir = repo
	var log strings.Builder
	if got, _ := maybeParkForMigrationRenumber(context.Background(), fu, cfg, "fhm_token", nil, &log); got != nil {
		t.Fatalf("nothing recognized must return nil, got %+v", got)
	}
	if log.String() != "" {
		t.Errorf("no recognition must be silent, got:\n%s", log.String())
	}
	if len(fu.gotRequestAmendmentArgs) != 0 {
		t.Errorf("no recognition must not POST an amendment")
	}
}

// ---------------------------------------------------------------------------
// #2748 fix-up — the decision budget bounds the BLOCKED request, not just the
// gaps between requests.
// ---------------------------------------------------------------------------

// TestBoundedRenumberWaitSeconds pins the per-request `?wait=N` cap: never more
// than the fixed hold, never more than the whole seconds left in the budget,
// and 0 (which omits the parameter entirely) once under a second remains.
func TestBoundedRenumberWaitSeconds(t *testing.T) {
	orig := migrationRenumberWaitSeconds
	migrationRenumberWaitSeconds = 30
	t.Cleanup(func() { migrationRenumberWaitSeconds = orig })

	cases := []struct {
		remaining time.Duration
		want      int
	}{
		{-time.Second, 0},
		{0, 0},
		{500 * time.Millisecond, 0},
		{time.Second, 1},
		{1500 * time.Millisecond, 1},
		{7 * time.Second, 7},
		{29999 * time.Millisecond, 29},
		{30 * time.Second, 30},
		{15 * time.Minute, 30},
	}
	for _, tc := range cases {
		if got := boundedRenumberWaitSeconds(tc.remaining); got != tc.want {
			t.Errorf("boundedRenumberWaitSeconds(%s) = %d, want %d", tc.remaining, got, tc.want)
		}
	}
}

// blockingAmendmentUploader is a fakeUploader whose FetchScopeAmendments
// genuinely BLOCKS — the shape the ordinary fake cannot produce, since it
// returns immediately and so never exercises the bound on a request in flight.
// It returns as soon as the request context is done (a well-behaved server
// honouring the deadline the client imposed) or after hold elapses (a server
// that ignores `?wait` entirely), whichever comes first.
type blockingAmendmentUploader struct {
	*fakeUploader
	hold      time.Duration
	waitSeen  []int
	unblocked int
}

func (b *blockingAmendmentUploader) FetchScopeAmendments(ctx context.Context, args upload.FetchScopeAmendmentsArgs) ([]upload.ScopeAmendment, error) {
	b.waitSeen = append(b.waitSeen, args.WaitSeconds)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(b.hold):
		b.unblocked++
		return b.fakeUploader.FetchScopeAmendments(context.Background(), args)
	}
}

// TestAwaitMigrationRenumberDecision_BlockedFetchHonoursBudget is the
// budget-boundary vehicle: the backend holds the long-poll far longer than the
// whole decision budget. The park must still return within the budget, and must
// classify the expiry as UNDECIDED (the amendment really is undecided) rather
// than as an unavailable backend.
//
// The bound is asserted on ELAPSED WALL CLOCK, not on error identity: a driver
// that checked the deadline only between requests returns the same "undecided"
// string — it just returns it ~hold later, having pinned the stage open.
func TestAwaitMigrationRenumberDecision_BlockedFetchHonoursBudget(t *testing.T) {
	const budget = 60 * time.Millisecond
	// hold is deliberately 50x the budget: an unbounded fetch overshoots by a
	// margin no scheduling jitter can explain away.
	const hold = 3 * time.Second
	pinRenumberBudget(t, budget)

	inner := newFakeUploader(t)
	inner.amendmentsAfterRequest = decidedRow("pending")
	bu := &blockingAmendmentUploader{fakeUploader: inner, hold: hold}
	cfg := renumberDriverCfg()

	start := time.Now()
	approved, exemptions, log := runRenumberDriver(t, context.Background(), bu, cfg, "fhm_token")
	elapsed := time.Since(start)

	if approved || exemptions != nil {
		t.Fatalf("a blocked fetch at the budget boundary must fail open: approved=%v exemptions=%+v", approved, exemptions)
	}
	assertDecidedEvent(t, log, "undecided")
	// Generous ceiling (10x the budget, still 5x under the hold) so the
	// assertion discriminates the unbounded fetch without being flaky.
	if elapsed > 10*budget {
		t.Errorf("the park held for %s with a %s budget — the blocked long-poll is not bounded by the remaining budget", elapsed, budget)
	}
	if bu.unblocked != 0 {
		t.Errorf("the fetch returned on its own %d time(s); the budget deadline must be what ends it", bu.unblocked)
	}
	if len(bu.waitSeen) == 0 {
		t.Fatal("no fetch was issued")
	}
	for i, w := range bu.waitSeen {
		if w > int(budget/time.Second) {
			t.Errorf("fetch %d asked the server to hold %ds, more than the %s budget", i, w, budget)
		}
	}
}

// TestAwaitMigrationRenumberDecision_PollPauseBoundedByBudget covers the other
// way the loop can outlive its budget: a server that returns IMMEDIATELY (no
// pending row to hold on) sends the driver into the poll-interval pause, which
// must itself be capped at the time left. With a 2s cadence and a 40ms budget an
// uncapped pause overshoots by ~50x.
func TestAwaitMigrationRenumberDecision_PollPauseBoundedByBudget(t *testing.T) {
	const budget = 40 * time.Millisecond
	origB, origP := migrationRenumberDecisionBudget, migrationRenumberPollInterval
	migrationRenumberDecisionBudget = budget
	migrationRenumberPollInterval = 2 * time.Second
	t.Cleanup(func() {
		migrationRenumberDecisionBudget = origB
		migrationRenumberPollInterval = origP
	})

	fu := newFakeUploader(t)
	fu.amendmentsAfterRequest = decidedRow("pending")
	cfg := renumberDriverCfg()

	start := time.Now()
	approved, exemptions, log := runRenumberDriver(t, context.Background(), fu, cfg, "fhm_token")
	elapsed := time.Since(start)

	if approved || exemptions != nil {
		t.Fatalf("must fail open: approved=%v exemptions=%+v", approved, exemptions)
	}
	assertDecidedEvent(t, log, "undecided")
	if elapsed > 10*budget {
		t.Errorf("the park held for %s with a %s budget — the poll pause is not capped at the remaining budget", elapsed, budget)
	}
}

// TestAwaitMigrationRenumberDecision_ParentContextStillUnavailable guards the
// classification split the budget bound introduces from over-broadening: the
// budget-expiry branch must claim ONLY our own derived deadline. A PARENT
// cancellation and a PARENT deadline (the stage-level timeout) are both
// `unavailable` — the amendment's fate is unknown because the process is going
// away, not because the decision budget ran out. The parent deadline is the
// discriminating case: its error IS context.DeadlineExceeded, so only the
// parent-health check keeps it out of the undecided branch.
func TestAwaitMigrationRenumberDecision_ParentContextStillUnavailable(t *testing.T) {
	cases := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"parent_cancelled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(20 * time.Millisecond)
				cancel()
			}()
			return ctx, cancel
		}},
		{"parent_deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 20*time.Millisecond)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A budget far longer than the parent's life, so only the parent
			// context can end the fetch.
			pinRenumberBudget(t, time.Minute)
			inner := newFakeUploader(t)
			inner.amendmentsAfterRequest = decidedRow("pending")
			bu := &blockingAmendmentUploader{fakeUploader: inner, hold: time.Minute}
			cfg := renumberDriverCfg()

			ctx, cancel := tc.ctx()
			defer cancel()

			approved, exemptions, log := runRenumberDriver(t, ctx, bu, cfg, "fhm_token")
			if approved || exemptions != nil {
				t.Fatalf("a dead parent context must fail open: approved=%v exemptions=%+v", approved, exemptions)
			}
			assertDecidedEvent(t, log, "unavailable")
		})
	}
}

// ---------------------------------------------------------------------------
// #2748 fix-up — approve, then re-invoke with a NEW number.
// ---------------------------------------------------------------------------

const (
	created2Up = migDir + "/0072_campaigns_working_dir.up.sql"
	created2Dn = migDir + "/0072_campaigns_working_dir.down.sql"
)

// TestMaybeParkForMigrationRenumber_ReinvokeRenumbersAgain is the
// approve-then-reinvoke-with-a-new-number regression. Attempt 1 renumbers
// 0070 → 0071 and the operator approves, folding the 0071 pair into
// cfg.scopeFiles. The base-rebase re-invoke then renumbers AGAIN, 0071 → 0072.
//
// Without the folded-path provenance the declared set now holds TWO absent
// same-slug pairs (0070 and the folded 0071) both claiming the one on-disk 0072
// pair; the strict global 1:1 sweep rejects the whole result and NO second
// amendment is offered — the second ship falls into category-B for making the
// correct rename, which is exactly the defect #2748 exists to close.
func TestMaybeParkForMigrationRenumber_ReinvokeRenumbersAgain(t *testing.T) {
	pinRenumberBudget(t, time.Minute)
	repo := renumberRepo(t, createdUp, createdDn)
	fu := newFakeUploader(t)
	fu.requestAmendmentDecision = "approved"
	cfg := renumberDriverCfg()
	cfg.workingDir = repo

	var log strings.Builder
	exemptions, subs := maybeParkForMigrationRenumber(context.Background(), fu, cfg, "fhm_token", nil, &log)
	if len(subs) != 1 || len(exemptions) != 2 {
		t.Fatalf("attempt 1: subs=%+v exemptions=%+v, want one substitution and two exemptions:\n%s", subs, exemptions, log.String())
	}
	if !containsString(scopePaths(cfg.scopeFiles), createdUp) {
		t.Fatalf("attempt 1's approval must fold the created pair: %v", scopePaths(cfg.scopeFiles))
	}

	// The re-invoked agent renumbers AGAIN: the folded 0071 pair leaves the
	// work tree and a 0072 pair takes its place.
	for _, p := range []string{createdUp, createdDn} {
		if err := os.Remove(filepath.Join(repo, filepath.FromSlash(p))); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{created2Up, created2Dn} {
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(p)), []byte("-- sql\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	log.Reset()
	exemptions2, subs2 := maybeParkForMigrationRenumber(context.Background(), fu, cfg, "fhm_token", subs, &log)
	if len(subs2) != 1 {
		t.Fatalf("the re-invoked renumber must be recognized, got %+v:\n%s", subs2, log.String())
	}
	if subs2[0].CreatedUp != created2Up || subs2[0].CreatedDown != created2Dn {
		t.Errorf("the second substitution must name the 0072 pair, got %+v", subs2[0])
	}
	if len(fu.gotRequestAmendmentArgs) != 2 {
		t.Fatalf("RequestScopeAmendment calls = %d, want 2 (one per renumber)", len(fu.gotRequestAmendmentArgs))
	}
	req := fu.gotRequestAmendmentArgs[1]
	want := []upload.ScopeAmendmentPath{
		{Path: created2Up, Operation: "create"},
		{Path: created2Dn, Operation: "create"},
	}
	if len(req.Paths) != 2 || req.Paths[0] != want[0] || req.Paths[1] != want[1] {
		t.Errorf("second amendment paths = %+v, want exactly %+v", req.Paths, want)
	}
	// The ABANDONED folded pair carries its own exemptions: it is still in
	// cfg.scopeFiles from attempt 1's fold but no longer in the work tree, so
	// without them the scope-completeness gate demands a file the re-invoke
	// deliberately dropped.
	gotExempt := map[string]bool{}
	for _, e := range exemptions2 {
		gotExempt[e.Path] = true
	}
	for _, p := range []string{createdUp, createdDn, declaredUp, declaredDn} {
		if !gotExempt[p] {
			t.Errorf("exemptions %+v missing %s", exemptions2, p)
		}
	}
}

// TestMaybeParkForMigrationRenumber_ReinvokeKeepsFoldedPair is the other half of
// the same control: when the re-invoked agent LEAVES the approved pair in
// place, the folded path stays declared, the "created pair already declared"
// branch disqualifies it, and NO second amendment is filed. Dropping the folded
// paths unconditionally — rather than only when they are absent — files a
// spurious second request and burns the stage's amendment budget.
func TestMaybeParkForMigrationRenumber_ReinvokeKeepsFoldedPair(t *testing.T) {
	pinRenumberBudget(t, time.Minute)
	repo := renumberRepo(t, createdUp, createdDn)
	fu := newFakeUploader(t)
	fu.requestAmendmentDecision = "approved"
	cfg := renumberDriverCfg()
	cfg.workingDir = repo

	var log strings.Builder
	_, subs := maybeParkForMigrationRenumber(context.Background(), fu, cfg, "fhm_token", nil, &log)
	if len(subs) != 1 {
		t.Fatalf("attempt 1 must recognize the renumber, got %+v:\n%s", subs, log.String())
	}

	// The re-invoke changes nothing about the migrations.
	exemptions2, subs2 := maybeParkForMigrationRenumber(context.Background(), fu, cfg, "fhm_token", subs, &log)
	if len(subs2) != 0 || exemptions2 != nil {
		t.Errorf("an unchanged re-invoke must offer nothing: subs=%+v exemptions=%+v", subs2, exemptions2)
	}
	if len(fu.gotRequestAmendmentArgs) != 1 {
		t.Errorf("RequestScopeAmendment calls = %d, want 1 — the already-folded pair must not be re-requested", len(fu.gotRequestAmendmentArgs))
	}
}

// TestDropAbsentFoldedCreations_HalfPresentPairStaysDeclared: a folded pair with
// only ONE path removed is left declared — the conservative direction, since a
// half-renumber is not a coherent substitution.
func TestDropAbsentFoldedCreations_HalfPresentPairStaysDeclared(t *testing.T) {
	repo := renumberRepo(t, createdUp)
	declared := []string{declaredUp, declaredDn, createdUp, createdDn}
	got, exemptions, err := dropAbsentFoldedCreations(repo, declared, renumberSubs)
	if err != nil {
		t.Fatalf("dropAbsentFoldedCreations: %v", err)
	}
	if len(got) != len(declared) || exemptions != nil {
		t.Errorf("a half-present folded pair must stay declared: got=%v exemptions=%+v", got, exemptions)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
