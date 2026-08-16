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
	if got := maybeParkForMigrationRenumber(context.Background(), fu, cfg, "fhm_token", &log); got != nil {
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
	if got := maybeParkForMigrationRenumber(context.Background(), fu, cfg, "fhm_token", &log); got != nil {
		t.Fatalf("nothing recognized must return nil, got %+v", got)
	}
	if log.String() != "" {
		t.Errorf("no recognition must be silent, got:\n%s", log.String())
	}
	if len(fu.gotRequestAmendmentArgs) != 0 {
		t.Errorf("no recognition must not POST an amendment")
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
