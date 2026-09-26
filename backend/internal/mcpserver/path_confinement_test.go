package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestConfinePath drives one case per NAMED branch of confinePath (E66.63 /
// #3589). Bad state is seeded BY CONSTRUCTION — the outside-root directory is a
// SECOND, independently created t.TempDir(), definitionally outside the
// configured root, never derived by calling the control in setup.
func TestConfinePath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // independently created — definitionally outside root

	t.Run("no_roots_configured", func(t *testing.T) {
		// FAIL CLOSED: over HTTP with NO root configured, even a path that
		// exists and is perfectly ordinary is refused.
		r := &runResolver{httpTransport: true}
		err := r.confinePath("working_dir", root)
		if err == nil {
			t.Fatal("expected a refusal with no allowed root configured")
		}
		if !strings.Contains(err.Error(), pathOutsideAllowedRootsCode) {
			t.Errorf("error should carry %q; got %v", pathOutsideAllowedRootsCode, err)
		}
		if !strings.Contains(err.Error(), "fail closed") {
			t.Errorf("error should name the fail-closed posture; got %v", err)
		}
	})

	t.Run("inside_root_accepted", func(t *testing.T) {
		// The self-paired ACCEPT control: without it the test could pass by
		// refusing everything.
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		if err := r.confinePath("working_dir", filepath.Join(root, "sub", "deep")); err != nil {
			t.Fatalf("a path inside the root must be accepted, got %v", err)
		}
		if err := r.confinePath("working_dir", root); err != nil {
			t.Fatalf("the root ITSELF must be accepted, got %v", err)
		}
	})

	t.Run("outside_root_refused", func(t *testing.T) {
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		err := r.confinePath("working_dir", outside)
		if err == nil {
			t.Fatal("expected a refusal for a path outside every root")
		}
		if !strings.Contains(err.Error(), pathOutsideAllowedRootsCode) {
			t.Errorf("error should carry %q; got %v", pathOutsideAllowedRootsCode, err)
		}
		if !strings.Contains(err.Error(), "working_dir") {
			t.Errorf("error should name the field; got %v", err)
		}
	})

	t.Run("sibling_prefix_near_miss", func(t *testing.T) {
		// `<root>-evil` shares root's STRING prefix but is not inside it. A bare
		// strings.HasPrefix would accept it; the separator anchor refuses.
		// Created for real so the case cannot pass on a resolution failure.
		evil := root + "-evil"
		if err := os.MkdirAll(evil, 0o700); err != nil {
			t.Fatalf("mkdir sibling: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(evil) })

		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		if err := r.confinePath("working_dir", evil); err == nil {
			t.Fatalf("%q must NOT be inside %q — the containment check is not separator-anchored", evil, root)
		}
	})

	t.Run("symlink_escapes_root", func(t *testing.T) {
		// A symlink INSIDE the root whose target is OUTSIDE it. filepath.Clean
		// alone cannot see this: the lexical path is inside the root. Only the
		// symlink-evaluation step refuses it.
		link := filepath.Join(root, "escape")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlink unsupported here: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(link) })

		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		if err := r.confinePath("working_dir", link); err == nil {
			t.Fatalf("a symlink at %q pointing outside every root must be refused", link)
		}
		// And the same escape reached through a deeper path component.
		if err := r.confinePath("spec_file", filepath.Join(link, "workflows.yaml")); err == nil {
			t.Fatal("a path traversing the escaping symlink must be refused")
		}
	})

	t.Run("root_reached_through_symlink", func(t *testing.T) {
		// t.TempDir() on macOS lives under /var/folders/... which resolves
		// through /private/var, so the ROOT side must be symlink-resolved too or
		// every accept case here would refuse. Made explicit rather than relying
		// on the platform: the root is supplied through a symlink to it.
		linkDir := filepath.Join(t.TempDir(), "rootlink")
		if err := os.Symlink(root, linkDir); err != nil {
			t.Skipf("symlink unsupported here: %v", err)
		}
		r := &runResolver{httpTransport: true, allowedRoots: []string{linkDir}}
		if err := r.confinePath("working_dir", filepath.Join(root, "sub")); err != nil {
			t.Fatalf("a root supplied through a symlink must still admit its own contents, got %v", err)
		}
	})

	t.Run("nonexistent_leaf_inside_root", func(t *testing.T) {
		// filepath.EvalSymlinks errors on a path that does not exist, so a
		// not-yet-created spec_file must be confined via the longest EXISTING
		// ancestor. Two levels of missing directories, to prove the rejoin.
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		leaf := filepath.Join(root, "not", "created", "yet", "workflows.yaml")
		if _, err := os.Stat(leaf); !os.IsNotExist(err) {
			t.Fatalf("fixture precondition: %q must not exist (stat err = %v)", leaf, err)
		}
		if err := r.confinePath("spec_file", leaf); err != nil {
			t.Fatalf("a not-yet-existing leaf inside the root must be accepted, got %v", err)
		}
	})

	t.Run("nonexistent_leaf_outside_root", func(t *testing.T) {
		// The mirror of the case above: non-existence does NOT buy admission.
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		if err := r.confinePath("spec_file", filepath.Join(outside, "nope", "workflows.yaml")); err == nil {
			t.Fatal("a not-yet-existing leaf OUTSIDE every root must be refused")
		}
	})

	t.Run("root_not_absolute", func(t *testing.T) {
		// A relative configured root is operator misconfiguration: refuse loudly
		// naming the bad root rather than silently dropping it.
		r := &runResolver{httpTransport: true, allowedRoots: []string{"relative/root"}}
		err := r.confinePath("working_dir", root)
		if err == nil {
			t.Fatal("expected a refusal for a non-absolute configured root")
		}
		if !strings.Contains(err.Error(), "relative/root") {
			t.Errorf("error should name the bad root; got %v", err)
		}
	})

	t.Run("relative_input_refused", func(t *testing.T) {
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		err := r.confinePath("working_dir", "sub/dir")
		if err == nil {
			t.Fatal("expected a refusal for a relative input")
		}
		if !strings.Contains(err.Error(), pathOutsideAllowedRootsCode) {
			t.Errorf("error should carry %q; got %v", pathOutsideAllowedRootsCode, err)
		}
	})

	t.Run("empty_input_deferred_to_the_2479_ladder", func(t *testing.T) {
		// Presence/absence is resolveWorkingDir's decision, not confinement's.
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		if err := r.confinePath("working_dir", ""); err != nil {
			t.Fatalf("an EMPTY input must be deferred to the #2479 ladder, got %v", err)
		}
	})

	t.Run("stdio_inert_with_roots_configured", func(t *testing.T) {
		// Operator option (c): stdio is deliberately UNCHANGED. Roots are
		// configured and the path is outside them, and it is still accepted.
		r := &runResolver{httpTransport: false, allowedRoots: []string{root}}
		if err := r.confinePath("working_dir", outside); err != nil {
			t.Fatalf("confinement must be inert on the stdio transport, got %v", err)
		}
	})

	t.Run("existence_non_leak", func(t *testing.T) {
		// The refusal must not be an existence oracle: the message for an
		// EXISTING outside-root file and a NON-EXISTING outside-root path are
		// compared to EACH OTHER (not to a literal), so the case cannot pass
		// vacuously if the wording changes.
		existing := filepath.Join(outside, "present.yaml")
		if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		absent := filepath.Join(outside, "absent.yaml")
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			t.Fatalf("fixture precondition: %q must not exist", absent)
		}

		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		errExisting := r.confinePath("spec_file", existing)
		errAbsent := r.confinePath("spec_file", absent)
		if errExisting == nil || errAbsent == nil {
			t.Fatalf("both outside-root paths must be refused; existing=%v absent=%v", errExisting, errAbsent)
		}
		// Byte-identical apart from the caller's own path, which the caller
		// supplied and already knows. Normalise that out and compare.
		normExisting := strings.ReplaceAll(errExisting.Error(), existing, "<PATH>")
		normAbsent := strings.ReplaceAll(errAbsent.Error(), absent, "<PATH>")
		if normExisting != normAbsent {
			t.Errorf("refusal leaks existence:\n existing: %s\n absent:   %s", normExisting, normAbsent)
		}
	})

	t.Run("second_root_admits", func(t *testing.T) {
		// Multiple roots: containment is satisfied by ANY of them, so the loop
		// must not refuse on the first miss.
		r := &runResolver{httpTransport: true, allowedRoots: []string{root, outside}}
		if err := r.confinePath("working_dir", filepath.Join(outside, "sub")); err != nil {
			t.Fatalf("a path inside the SECOND root must be accepted, got %v", err)
		}
	})
}

// TestPathWithinRoot pins the separator anchor directly, including the
// filesystem-root case TrimSuffix exists for.
func TestPathWithinRoot(t *testing.T) {
	sep := string(filepath.Separator)
	base := sep + "roots" + sep + "repo"
	cases := []struct {
		candidate, root string
		want            bool
	}{
		{base, base, true},
		{base + sep + "sub", base, true},
		{base + "-evil", base, false},
		{sep + "roots", base, false},
		{base, sep, true}, // the filesystem root contains everything
	}
	for _, tc := range cases {
		if got := pathWithinRoot(tc.candidate, tc.root); got != tc.want {
			t.Errorf("pathWithinRoot(%q, %q) = %v, want %v", tc.candidate, tc.root, got, tc.want)
		}
	}
}

// TestResolveForConfinement_UnresolvableRefuses covers the non-not-exist
// EvalSymlinks failure branch: a symlink LOOP is neither resolvable nor
// missing, so containment is undecidable and confinePath must refuse.
func TestResolveForConfinement_UnresolvableRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink loop fixture is unix-specific")
	}
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	if _, err := resolveForConfinement(a); err == nil {
		t.Fatal("a symlink loop must surface as a resolution error, not a resolved path")
	}
	r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
	err := r.confinePath("working_dir", a)
	if err == nil {
		t.Fatal("an unresolvable path must be REFUSED (fail closed), not accepted")
	}
	if !strings.Contains(err.Error(), pathOutsideAllowedRootsCode) {
		t.Errorf("error should carry %q; got %v", pathOutsideAllowedRootsCode, err)
	}
}

// TestPathConfinementSchemaNote_MirroredIntoEveryPathTakingInput derives the
// prose check from the canonical const rather than asserting a full sentence in
// prose: struct tags cannot hold a Go expression, so pathConfinementSchemaNote's
// text is MIRRORED into each path-taking input's jsonschema description. This
// pins that mirroring so a reworded const, or a new path-taking input that
// forgets the note, fails here instead of silently drifting.
func TestPathConfinementSchemaNote_MirroredIntoEveryPathTakingInput(t *testing.T) {
	// The note's DISTINCTIVE substring — the error token plus the flag pair —
	// rather than the whole sentence, which a copy-edit would churn.
	for _, want := range []string{pathOutsideAllowedRootsCode, "--mcp-allowed-roots", "--allowed-roots"} {
		if !strings.Contains(pathConfinementSchemaNote, want) {
			t.Fatalf("pathConfinementSchemaNote should mention %q; got %q", want, pathConfinementSchemaNote)
		}
	}
	files := []string{
		"tools.go", "validate_spec.go", "campaign.go",
		"run_stage.go", "dispatch_stage.go", "run_children.go", "drive_run.go",
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(src), pathOutsideAllowedRootsCode) {
			t.Errorf("%s carries a path-taking input but no %s note in its jsonschema description — mirror pathConfinementSchemaNote into it",
				f, pathOutsideAllowedRootsCode)
		}
	}
}

// dialRecorder is a counting reverse proxy in front of the fakeBackend mux: it
// records the method+path of EVERY request the resolver's apiClient makes and
// then delegates. Error IDENTITY alone cannot prove a refusal committed nothing
// (a control that fires and is then rolled back returns the same error), so the
// cross-verb rows below read the RECORDED DIALS after each call returns.
type dialRecorder struct {
	mu    sync.Mutex
	dials []string
	next  http.Handler
}

func (d *dialRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	d.dials = append(d.dials, r.Method+" "+r.URL.Path)
	d.mu.Unlock()
	d.next.ServeHTTP(w, r)
}

func (d *dialRecorder) recorded() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dials...)
}

// newConfinementFixture builds a resolver whose backend dials are all recorded,
// whose runner spawns are all captured, and whose allowed root is `root`.
func newConfinementFixture(t *testing.T, root string) (*fakeBackend, *runResolver, *dialRecorder, *[][]string) {
	t.Helper()
	fb, srv := newFakeBackend(t)
	rec := &dialRecorder{next: srv.Config.Handler}
	counted := httptest.NewServer(rec)
	t.Cleanup(counted.Close)

	r := newResolver(counted, nil)
	r.httpTransport = true
	if root != "" {
		r.allowedRoots = []string{root}
	}
	calls := captureAllArgv(t)
	return fb, r, rec, calls
}

// TestPathConfinement_EveryPathTakingVerb drives the REAL handler of all eight
// path-taking verbs over an HTTP-posture resolver with an out-of-root path and
// asserts, per row (E66.63 / #3589, binding condition 2):
//
//   - the named path_outside_allowed_roots refusal, AND
//   - that NOTHING was committed: zero backend dials, zero runner spawns, no
//     host-dispatch marker — because error identity alone cannot prove a
//     refusal committed nothing.
//
// Two rows are deliberately NOT refusals: the inherited-binding row (which may
// perform the ONE run-read needed to obtain the stored working_dir, and no
// other dial) and the inline-workflow_spec row (which reads no filesystem and
// must still SUCCEED with no roots configured, proving the fail-closed default
// does not over-refuse).
func TestPathConfinement_EveryPathTakingVerb(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir() // independently created — definitionally outside root

	// Supplied-path refusal rows: working_dir (or spec_file) points OUTSIDE the
	// single configured root, so nothing may be read, dialed or spawned.
	suppliedRows := []struct {
		name string
		call func(t *testing.T, fb *fakeBackend, r *runResolver) error
	}{
		{"fishhawk_validate/working_dir", func(_ *testing.T, _ *fakeBackend, r *runResolver) error {
			_, _, err := r.validateSpec(context.Background(), nil, ValidateSpecInput{WorkingDir: outside})
			return err
		}},
		{"fishhawk_validate/spec_file", func(_ *testing.T, _ *fakeBackend, r *runResolver) error {
			_, _, err := r.validateSpec(context.Background(), nil, ValidateSpecInput{SpecFile: filepath.Join(outside, "workflows.yaml")})
			return err
		}},
		{"fishhawk_start_run/working_dir", func(t *testing.T, _ *fakeBackend, r *runResolver) error {
			_, _, err := r.startRun(context.Background(), nil, StartRunInput{
				Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", RunnerKind: "local",
				WorkingDir: outside, WorkflowSpec: minimalWorkflowSpecYAML(t), WorkflowSHA: "deadbeef",
			})
			return err
		}},
		{"fishhawk_start_run/spec_file", func(t *testing.T, _ *fakeBackend, r *runResolver) error {
			_, _, err := r.startRun(context.Background(), nil, StartRunInput{
				Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", RunnerKind: "local",
				WorkingDir: root, SpecFile: filepath.Join(outside, "workflows.yaml"),
				WorkflowSpec: minimalWorkflowSpecYAML(t), WorkflowSHA: "deadbeef",
			})
			return err
		}},
		{"fishhawk_run_stage", func(_ *testing.T, fb *fakeBackend, r *runResolver) error {
			runID, stageID := uuid.New(), uuid.New()
			seedStageOfType(fb, runID, stageID, "implement", "pending")
			_, _, err := r.runStage(context.Background(), nil, RunStageInput{
				RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
				WorkingDir: outside, GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
			})
			return err
		}},
		{"fishhawk_dispatch_stage", func(_ *testing.T, fb *fakeBackend, r *runResolver) error {
			runID, stageID := uuid.New(), uuid.New()
			seedStageOfType(fb, runID, stageID, "implement", "pending")
			_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
				RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
				WorkingDir: outside, GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
			})
			return err
		}},
		{"fishhawk_run_children", func(_ *testing.T, fb *fakeBackend, r *runResolver) error {
			runID := uuid.New()
			seedRunWorkingDir(fb, runID, "")
			_, _, err := r.runChildren(context.Background(), nil, RunChildrenInput{
				RunID: runID.String(), Workflow: "feature_change", WorkingDir: outside, GitHubRepo: "x/y",
			})
			return err
		}},
		{"fishhawk_drive_run", func(_ *testing.T, fb *fakeBackend, r *runResolver) error {
			runID := uuid.New()
			seedRunWorkingDir(fb, runID, "")
			_, _, err := r.driveRun(context.Background(), nil, DriveRunInput{
				RunID: runID.String(), WorkingDir: outside, GitHubRepo: "x/y",
			})
			return err
		}},
		{"fishhawk_start_campaign", func(_ *testing.T, _ *fakeBackend, r *runResolver) error {
			_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
				Repo: "kuhlman-labs/fishhawk", Items: []string{"issue:1"}, WorkingDir: outside,
			})
			return err
		}},
		{"fishhawk_start_campaign_item_run", func(_ *testing.T, _ *fakeBackend, r *runResolver) error {
			_, _, err := r.startCampaignItemRun(context.Background(), nil, StartCampaignItemRunInput{
				CampaignID: uuid.NewString(), IssueRef: "issue:1", WorkflowID: "feature_change",
				RunnerKind: "local", WorkingDir: outside,
			})
			return err
		}},
	}

	for _, row := range suppliedRows {
		t.Run(row.name, func(t *testing.T) {
			fb, r, rec, calls := newConfinementFixture(t, root)

			err := row.call(t, fb, r)
			if err == nil {
				t.Fatal("expected a refusal for an out-of-root path")
			}
			if !strings.Contains(err.Error(), pathOutsideAllowedRootsCode) {
				t.Errorf("error should carry %q; got %v", pathOutsideAllowedRootsCode, err)
			}
			// Committed-state assertions: the refusal is only meaningful if it
			// happened BEFORE anything was committed.
			if dials := rec.recorded(); len(dials) != 0 {
				t.Errorf("the refusal dialed the backend %d time(s): %v; a supplied-path refusal must dial ZERO times", len(dials), dials)
			}
			if len(*calls) != 0 {
				t.Errorf("the refusal spawned %d runner(s): %v; a refusal must spawn none", len(*calls), *calls)
			}
			fb.mu.Lock()
			hostDispatches := len(fb.hostDispatchCalledByID)
			fb.mu.Unlock()
			if hostDispatches != 0 {
				t.Errorf("the refusal recorded %d host-dispatch marker(s); a refusal must record none", hostDispatches)
			}
		})
	}

	// The E66.42 INHERITED-binding row: working_dir is OMITTED and the run
	// carries a binding that is itself OUTSIDE every root. Per binding condition
	// 2 this row may perform the ONE run-read needed to obtain the stored
	// working_dir — and no other dial, and no spawn.
	t.Run("inherited_binding_outside_roots", func(t *testing.T) {
		fb, r, rec, calls := newConfinementFixture(t, root)
		runID, stageID := uuid.New(), uuid.New()
		// Bad state BY CONSTRUCTION: the binding is the independent outside dir.
		seedRunWorkingDir(fb, runID, outside)
		seedStageOfType(fb, runID, stageID, "implement", "pending")

		_, _, err := r.dispatchStage(context.Background(), nil, DispatchStageInput{
			RunID: runID.String(), Workflow: "feature_change", Stage: "implement",
			GitHubRepo: "x/y", PushAndOpenPR: boolPtr(false),
			// working_dir OMITTED — the out-of-root binding is inherited.
		})
		if err == nil {
			t.Fatal("expected a refusal for an INHERITED binding outside every root")
		}
		if !strings.Contains(err.Error(), pathOutsideAllowedRootsCode) {
			t.Errorf("error should carry %q; got %v", pathOutsideAllowedRootsCode, err)
		}

		wantRead := "GET /v0/runs/" + runID.String()
		dials := rec.recorded()
		if len(dials) != 1 || dials[0] != wantRead {
			t.Errorf("dials = %v, want EXACTLY the one run-read %q that obtains the stored working_dir", dials, wantRead)
		}
		if len(*calls) != 0 {
			t.Errorf("the refusal spawned %d runner(s): %v; an out-of-root binding must spawn none", len(*calls), *calls)
		}
		fb.mu.Lock()
		hostDispatches := len(fb.hostDispatchCalledByID)
		fb.mu.Unlock()
		if hostDispatches != 0 {
			t.Errorf("the refusal recorded %d host-dispatch marker(s); it must record none", hostDispatches)
		}
	})

	// The no-over-refusal control: fishhawk_validate's INLINE workflow_spec arm
	// reads no filesystem, so it must still SUCCEED over HTTP with NO root
	// configured. Without this row the fail-closed default could be over-broad
	// and every refusal row would still pass.
	t.Run("inline_workflow_spec_succeeds_with_no_roots", func(t *testing.T) {
		_, r, rec, calls := newConfinementFixture(t, "") // NO roots configured
		if len(r.allowedRoots) != 0 {
			t.Fatalf("fixture precondition: allowedRoots must be empty, got %v", r.allowedRoots)
		}

		_, out, err := r.validateSpec(context.Background(), nil, ValidateSpecInput{
			WorkflowSpec: minimalWorkflowSpecYAML(t),
		})
		if err != nil {
			t.Fatalf("the inline arm reads no filesystem and must not be confined, got %v", err)
		}
		if !out.Valid || out.Source != "inline" {
			t.Errorf("out = {valid:%v source:%q}, want a valid inline verdict", out.Valid, out.Source)
		}
		if dials := rec.recorded(); len(dials) != 0 {
			t.Errorf("fishhawk_validate is in-process and must dial nothing; got %v", dials)
		}
		if len(*calls) != 0 {
			t.Errorf("fishhawk_validate must spawn nothing; got %v", *calls)
		}
	})
}

// TestPathConfinement_DiscoveredSpecSymlinkEscapeRefused is binding approval
// condition 1: confining the SUPPLIED working_dir is not enough, because
// discoverSpec selects the file it actually READS. Here working_dir is a
// perfectly ALLOWED root whose `.fishhawk/workflows.yaml` is a SYMLINK to a
// spec outside every root. The call must be refused
// path_outside_allowed_roots having read ZERO bytes.
//
// Three assertions carry that, in increasing strength:
//
//   - the caller-visible contract: the refusal carries the named code, the
//     output is zero (nothing echoed), and nothing was dialed or spawned;
//   - a chmod-000 READ BARRIER on the escape target, installed AFTER the
//     fixture has proven the symlink reads through: any open() this code
//     attempts now fails EACCES and surfaces as a `read …: permission denied`
//     error instead of the confinement refusal, so "refused before reading" is
//     distinguished from "read, then refused" by the error IDENTITY rather
//     than only inferred from the absence of output. The barrier is skipped
//     where chmod cannot deny the running user (Windows, and root); the test
//     then says so rather than claiming evidence it does not have.
func TestPathConfinement_DiscoveredSpecSymlinkEscapeRefused(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// The escape TARGET: a real, readable, VALID spec outside every root. If the
	// confinement check were missing, discoverSpec would read it and the verb
	// would return valid:true — which is exactly what this test refutes.
	target := filepath.Join(outside, "secret-workflows.yaml")
	if err := os.WriteFile(target, []byte(minimalWorkflowSpecYAML(t)), 0o600); err != nil {
		t.Fatalf("write escape target: %v", err)
	}

	// working_dir is the ALLOWED root; its spec path is a symlink pointing out.
	if err := os.MkdirAll(filepath.Join(root, ".fishhawk"), 0o700); err != nil {
		t.Fatalf("mkdir .fishhawk: %v", err)
	}
	link := filepath.Join(root, specFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	// Precondition, by construction: the symlink IS readable, so a refusal
	// cannot be mistaken for an ordinary read failure.
	if _, err := os.ReadFile(link); err != nil {
		t.Fatalf("fixture precondition: the symlink must be readable through, got %v", err)
	}

	// The READ BARRIER: with the escape target unreadable, any open() this code
	// attempts fails loudly and cannot be mistaken for the confinement refusal.
	// Installed only after the readability precondition above, and only when it
	// is actually EFFECTIVE for the running user — root ignores mode bits, and
	// Windows does not model them this way.
	barrier := false
	if runtime.GOOS != "windows" {
		t.Cleanup(func() { _ = os.Chmod(target, 0o600) })
		if err := os.Chmod(target, 0o000); err == nil {
			if _, rerr := os.ReadFile(link); rerr != nil {
				barrier = true
			}
		}
	}
	if !barrier {
		t.Logf("read barrier not effective here (GOOS=%s, possibly running as root): "+
			"this run pins only the indirect assertions (refusal identity, zero output, zero dials)", runtime.GOOS)
	}

	_, r, rec, calls := newConfinementFixture(t, root)

	_, out, err := r.validateSpec(context.Background(), nil, ValidateSpecInput{WorkingDir: root})
	if err == nil {
		t.Fatalf("a discovered spec symlinked outside every root must be refused; got out = %+v", out)
	}
	if !strings.Contains(err.Error(), pathOutsideAllowedRootsCode) {
		t.Errorf("error should carry %q; got %v", pathOutsideAllowedRootsCode, err)
	}
	if barrier && strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the escape target was OPENED: the error is a read failure against the chmod-000 barrier, "+
			"not the confinement refusal; got %v", err)
	}
	// ZERO bytes read: the refusal names the discovered path but echoes no
	// content, and the verb returned no verdict about the escape target.
	if out.Valid || out.Source != "" || out.Path != "" {
		t.Errorf("the refused call must return a ZERO output (nothing read); got %+v", out)
	}
	if dials := rec.recorded(); len(dials) != 0 {
		t.Errorf("the refusal must dial nothing; got %v", dials)
	}
	if len(*calls) != 0 {
		t.Errorf("the refusal must spawn nothing; got %v", *calls)
	}

	// The same escape through fishhawk_start_run's discovery, which is the arm
	// that would otherwise ship the stolen bytes to the backend as
	// workflow_spec.
	_, r2, rec2, _ := newConfinementFixture(t, root)
	_, _, err = r2.startRun(context.Background(), nil, StartRunInput{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", RunnerKind: "local",
		WorkingDir: root, WorkflowSHA: "deadbeef",
	})
	if err == nil {
		t.Fatal("start_run must refuse a discovered spec symlinked outside every root")
	}
	if !strings.Contains(err.Error(), pathOutsideAllowedRootsCode) {
		t.Errorf("start_run error should carry %q; got %v", pathOutsideAllowedRootsCode, err)
	}
	if barrier && strings.Contains(err.Error(), "permission denied") {
		t.Errorf("start_run OPENED the escape target: the error is a read failure against the chmod-000 barrier; got %v", err)
	}
	for _, d := range rec2.recorded() {
		if strings.HasPrefix(d, "POST /v0/runs") {
			t.Errorf("the refusal reached %s — the discovered bytes were shipped to the backend", d)
		}
	}
}

// TestConfineSpecCandidate covers confineSpecCandidate's three verdicts
// directly, including the STOP-WALK boundary that must NOT be a refusal: the
// discovery walk climbing above every root is the ordinary end of the search
// (like the existing `.git` boundary), not a poisoned input.
func TestConfineSpecCandidate(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	t.Run("inside_root_allowed", func(t *testing.T) {
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		if got := r.confineSpecCandidate(filepath.Join(root, specFileName)); got != specCandidateAllow {
			t.Errorf("verdict = %v, want specCandidateAllow", got)
		}
	})
	t.Run("above_every_root_stops_the_walk", func(t *testing.T) {
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		if got := r.confineSpecCandidate(filepath.Join(outside, specFileName)); got != specCandidateStopWalk {
			t.Errorf("verdict = %v, want specCandidateStopWalk (a walk leaving the roots is the search boundary, not an escape)", got)
		}
	})
	t.Run("symlink_escape_refuses", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(root, ".fishhawk"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		target := filepath.Join(outside, "target.yaml")
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		link := filepath.Join(root, specFileName)
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink unsupported here: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(link) })
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		if got := r.confineSpecCandidate(link); got != specCandidateRefuse {
			t.Errorf("verdict = %v, want specCandidateRefuse", got)
		}
	})
	t.Run("no_roots_refuses", func(t *testing.T) {
		r := &runResolver{httpTransport: true}
		if got := r.confineSpecCandidate(filepath.Join(root, specFileName)); got != specCandidateRefuse {
			t.Errorf("verdict = %v, want specCandidateRefuse (fail closed)", got)
		}
	})
	t.Run("non_absolute_refuses", func(t *testing.T) {
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		if got := r.confineSpecCandidate("rel/.fishhawk/workflows.yaml"); got != specCandidateRefuse {
			t.Errorf("verdict = %v, want specCandidateRefuse", got)
		}
	})
	t.Run("non_absolute_root_refuses", func(t *testing.T) {
		r := &runResolver{httpTransport: true, allowedRoots: []string{"rel/root"}}
		if got := r.confineSpecCandidate(filepath.Join(root, specFileName)); got != specCandidateRefuse {
			t.Errorf("verdict = %v, want specCandidateRefuse", got)
		}
	})
	t.Run("stdio_inert", func(t *testing.T) {
		r := &runResolver{httpTransport: false}
		if got := r.confineSpecCandidate(filepath.Join(outside, specFileName)); got != specCandidateAllow {
			t.Errorf("verdict = %v, want specCandidateAllow (stdio is deliberately unconfined)", got)
		}
	})
}

// TestNewServer_AllowedRootsReachesTheResolver is binding approval condition 4:
// an ACCEPT path through the FULL Config.AllowedRoots -> internal() ->
// NewServer -> runResolver -> handler chain, not a hand-built resolver. A unit
// test on confinePath stays green even if NewServer forgets to thread the field
// — this is what catches that. The mirror REFUSAL through the same chain proves
// the accept is not vacuous.
func TestNewServer_AllowedRootsReachesTheResolver(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withFakeRunner(t, "exit 0")
	runID, stageID := uuid.New(), uuid.New()
	seedStageOfType(fb, runID, stageID, "implement", "pending")

	root := t.TempDir()
	outside := t.TempDir()

	args := func(wd string) map[string]any {
		return map[string]any{
			"run_id": runID.String(), "workflow": "feature_change", "stage": "implement",
			"working_dir": wd, "github_repo": "x/y", "push_and_open_pr": false,
			"runner_binary": "/fake/fishhawk-runner",
		}
	}
	cfg := Config{BackendURL: srv.URL, APIToken: "tok", HTTPTransport: true, AllowedRoots: []string{root}}

	// ACCEPT: a path inside the configured root goes through.
	res := callDispatchViaNewServer(t, cfg, args(root))
	if res.IsError {
		t.Fatalf("a working_dir inside Config.AllowedRoots must be accepted through NewServer; content: %+v", res.Content)
	}

	// REFUSE: the independently-created outside dir, same chain, same config.
	res = callDispatchViaNewServer(t, cfg, args(outside))
	if !res.IsError {
		t.Fatal("a working_dir outside Config.AllowedRoots must be refused through NewServer")
	}
	if !strings.Contains(toolResultText(t, res), pathOutsideAllowedRootsCode) {
		t.Errorf("the refusal should carry %q; content: %+v", pathOutsideAllowedRootsCode, res.Content)
	}
}

// toolResultText flattens a CallToolResult's text content for substring
// assertions.
func toolResultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestDiscoverSpecConfined_MatchesUnconfinedWalkOnStdio pins the equivalence the
// duplicated walk in discoverSpecConfined depends on: on the stdio posture — and
// for an EXPLICIT path on either posture — it must return EXACTLY what the
// original discoverSpec returns, because both delegate. And on the HTTP posture
// with the whole tree inside an allowed root it must return the same result the
// unconfined walk does, so confinement changes only the REFUSAL cases and never
// the discovery semantics (the `.git` boundary, the walk-up, the blob SHA).
//
// This is the guard the comment on discoverSpecConfined names: if either
// boundary condition drifts out of step with discoverSpec, this goes red.
func TestDiscoverSpecConfined_MatchesUnconfinedWalkOnStdio(t *testing.T) {
	// A repo-shaped fixture: <root>/repo/.git, a spec at the repo root, and a
	// nested dir the walk must climb out of to find it.
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	nested := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".fishhawk"), 0o700); err != nil {
		t.Fatalf("mkdir .fishhawk: %v", err)
	}
	specPath := filepath.Join(repo, specFileName)
	body := minimalWorkflowSpecYAML(t)
	if err := os.WriteFile(specPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	same := func(t *testing.T, label string, got, want *discoveredSpec) {
		t.Helper()
		switch {
		case got == nil && want == nil:
			return
		case got == nil || want == nil:
			t.Fatalf("%s: got %v, want %v (one is nil)", label, got, want)
		}
		if got.Path != want.Path || string(got.Contents) != string(want.Contents) || got.BlobSHA != want.BlobSHA {
			t.Errorf("%s: confined walk diverged\n got  = {Path:%q BlobSHA:%q len:%d}\n want = {Path:%q BlobSHA:%q len:%d}",
				label, got.Path, got.BlobSHA, len(got.Contents), want.Path, want.BlobSHA, len(want.Contents))
		}
	}

	t.Run("stdio_walk_from_nested_is_identical", func(t *testing.T) {
		want, werr := discoverSpec(nested, "")
		r := &runResolver{httpTransport: false}
		got, gerr := r.discoverSpecConfined(nested, "")
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("error disagreement: unconfined=%v confined=%v", werr, gerr)
		}
		same(t, "stdio walk", got, want)
		if got == nil || got.Path != specPath {
			t.Fatalf("expected the repo-root spec at %q, got %v", specPath, got)
		}
	})

	t.Run("http_in_root_walk_is_identical", func(t *testing.T) {
		// The WHOLE tree is inside the allowed root, so confinement must not
		// change the outcome at all.
		want, werr := discoverSpec(nested, "")
		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		got, gerr := r.discoverSpecConfined(nested, "")
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("error disagreement: unconfined=%v confined=%v", werr, gerr)
		}
		same(t, "http in-root walk", got, want)
	})

	t.Run("explicit_path_delegates_on_both_transports", func(t *testing.T) {
		want, werr := discoverSpec(nested, specPath)
		if werr != nil {
			t.Fatalf("unconfined explicit: %v", werr)
		}
		for _, http := range []bool{false, true} {
			r := &runResolver{httpTransport: http, allowedRoots: []string{root}}
			got, gerr := r.discoverSpecConfined(nested, specPath)
			if gerr != nil {
				t.Fatalf("http=%v explicit: %v", http, gerr)
			}
			same(t, "explicit", got, want)
		}
	})

	t.Run("no_spec_anywhere_yields_nil_nil_on_both", func(t *testing.T) {
		// The `.git` boundary case: a repo with NO spec returns (nil, nil), not
		// an error, on the confined walk exactly as on the unconfined one.
		bare := t.TempDir()
		if err := os.MkdirAll(filepath.Join(bare, ".git"), 0o700); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
		want, werr := discoverSpec(bare, "")
		if werr != nil || want != nil {
			t.Fatalf("unconfined baseline: got %v, %v; want nil, nil", want, werr)
		}
		r := &runResolver{httpTransport: true, allowedRoots: []string{bare}}
		got, gerr := r.discoverSpecConfined(bare, "")
		if gerr != nil || got != nil {
			t.Errorf("confined walk: got %v, %v; want nil, nil (the .git boundary)", got, gerr)
		}
	})
}

// swapSpecOnce installs the TEST-ONLY specOpenRaceHook so that the FIRST time
// the discovery walk is about to open `candidate`, `swap` runs — deterministically
// occupying the TOCTOU window between the pre-open confinement check and the
// open. Without this seam the window could only be hit probabilistically, and a
// probabilistic counterfactual is not evidence.
func swapSpecOnce(t *testing.T, candidate string, swap func()) *bool {
	t.Helper()
	var once sync.Once
	fired := false
	specOpenRaceHook = func(c string) {
		if c != candidate {
			return
		}
		once.Do(func() {
			fired = true
			swap()
		})
	}
	t.Cleanup(func() { specOpenRaceHook = nil })
	return &fired
}

// TestPathConfinement_DiscoveredSpecReplacedAfterCheck is the TOCTOU half of
// binding approval condition 1, raised as a high-tier fix-up concern: confining
// the PATHNAME is not sufficient, because the open re-traverses it. In a
// supported writable checkout an agent can replace the validated leaf — or an
// ancestor directory — with an out-of-root symlink between the check and the
// open, and a pathname-only control would then hand the outside file's bytes to
// validation diagnostics or to start_run's backend submission.
//
// Both rows start from a state the pre-open check ALLOWS (a real, in-root spec),
// so the refusal cannot come from the stationary check already covered above:
// it can only come from the post-open containment of the file actually opened.
func TestPathConfinement_DiscoveredSpecReplacedAfterCheck(t *testing.T) {
	// The escape target is a VALID spec, so a control that reads it would
	// succeed rather than fail for an unrelated reason — the read would be
	// invisible in an error-identity-only assertion.
	outside := t.TempDir()
	target := filepath.Join(outside, "secret-workflows.yaml")
	if err := os.WriteFile(target, []byte(minimalWorkflowSpecYAML(t)), 0o600); err != nil {
		t.Fatalf("write escape target: %v", err)
	}

	// newRoot builds an allowed root holding a REAL, readable, in-root spec
	// whose contents are distinguishable from the escape target's, plus a .git
	// dir so the walk is bounded at the root.
	inRootBody := "# in-root spec, must be what a non-raced call returns\n" + minimalWorkflowSpecYAML(t)
	newRoot := func(t *testing.T) (root, specPath string) {
		t.Helper()
		root = t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".fishhawk"), 0o700); err != nil {
			t.Fatalf("mkdir .fishhawk: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
			t.Fatalf("mkdir .git: %v", err)
		}
		specPath = filepath.Join(root, specFileName)
		if err := os.WriteFile(specPath, []byte(inRootBody), 0o600); err != nil {
			t.Fatalf("write in-root spec: %v", err)
		}
		return root, specPath
	}

	assertRefused := func(t *testing.T, got *discoveredSpec, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("a spec replaced between the confinement check and the open must be refused; got %+v", got)
		}
		if !strings.Contains(err.Error(), pathOutsideAllowedRootsCode) {
			t.Errorf("error should carry %q; got %v", pathOutsideAllowedRootsCode, err)
		}
		if got != nil {
			t.Fatalf("the refusal must return NO spec; got Path=%q len=%d", got.Path, len(got.Contents))
		}
	}

	t.Run("leaf_replaced_with_outside_symlink", func(t *testing.T) {
		root, specPath := newRoot(t)
		fired := swapSpecOnce(t, specPath, func() {
			if err := os.Remove(specPath); err != nil {
				t.Errorf("race swap: remove: %v", err)
				return
			}
			if err := os.Symlink(target, specPath); err != nil {
				t.Errorf("race swap: symlink: %v", err)
			}
		})

		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		got, err := r.discoverSpecConfined(root, "")
		if !*fired {
			t.Fatal("the race window never opened — the test proves nothing")
		}
		assertRefused(t, got, err)
	})

	t.Run("ancestor_dir_replaced_with_outside_symlink", func(t *testing.T) {
		root, specPath := newRoot(t)
		// The ESCAPE seen through an ancestor: `<root>/.fishhawk` becomes a
		// symlink to a directory outside every root that holds a spec of its own.
		outsideDir := filepath.Join(outside, "fishhawk-dir")
		if err := os.MkdirAll(outsideDir, 0o700); err != nil {
			t.Fatalf("mkdir outside dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(outsideDir, "workflows.yaml"), []byte(minimalWorkflowSpecYAML(t)), 0o600); err != nil {
			t.Fatalf("write outside spec: %v", err)
		}
		fishhawkDir := filepath.Join(root, ".fishhawk")
		fired := swapSpecOnce(t, specPath, func() {
			if err := os.RemoveAll(fishhawkDir); err != nil {
				t.Errorf("race swap: remove dir: %v", err)
				return
			}
			if err := os.Symlink(outsideDir, fishhawkDir); err != nil {
				t.Errorf("race swap: symlink dir: %v", err)
			}
		})

		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		got, err := r.discoverSpecConfined(root, "")
		if !*fired {
			t.Fatal("the race window never opened — the test proves nothing")
		}
		assertRefused(t, got, err)
	})

	t.Run("unraced_call_still_reads_the_in_root_spec", func(t *testing.T) {
		// The self-paired ACCEPT: with the window occupied by a NO-OP swap, the
		// same code path must still return the in-root spec. Without this row
		// the two refusals above would also be satisfied by a control that
		// refuses every discovered spec.
		root, specPath := newRoot(t)
		fired := swapSpecOnce(t, specPath, func() {})

		r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
		got, err := r.discoverSpecConfined(root, "")
		if err != nil {
			t.Fatalf("an unreplaced in-root spec must still be read; got %v", err)
		}
		if !*fired {
			t.Fatal("the hook never fired — the accept row is not exercising the same path")
		}
		if got == nil || string(got.Contents) != inRootBody {
			t.Fatalf("expected the in-root spec contents; got %+v", got)
		}
	})
}

// TestPathConfinement_DiscoveredSpecRaceInvariantUnderConcurrentSwap is the
// unseamed companion to the deterministic rows above: a real goroutine flips the
// discovered spec between an in-root file and an out-of-root symlink while the
// walk runs. It asserts the INVARIANT rather than a particular outcome — every
// call returns the in-root spec, no spec, or a refusal, and NEVER the escape
// target's bytes — so it cannot flake: the outcome distribution may vary, the
// invariant may not.
func TestPathConfinement_DiscoveredSpecRaceInvariantUnderConcurrentSwap(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	const escapeMarker = "ESCAPE-TARGET-CONTENTS-MUST-NEVER-BE-RETURNED"
	target := filepath.Join(outside, "secret-workflows.yaml")
	if err := os.WriteFile(target, []byte("# "+escapeMarker+"\n"+minimalWorkflowSpecYAML(t)), 0o600); err != nil {
		t.Fatalf("write escape target: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".fishhawk"), 0o700); err != nil {
		t.Fatalf("mkdir .fishhawk: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	specPath := filepath.Join(root, specFileName)
	inRoot := "# in-root\n" + minimalWorkflowSpecYAML(t)
	if err := os.WriteFile(specPath, []byte(inRoot), 0o600); err != nil {
		t.Fatalf("write in-root spec: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, ".fishhawk", "escape.yaml")); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	staged := filepath.Join(root, ".fishhawk", "escape.yaml")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Flip the discovered pathname between the real in-root file and a
			// symlink out of the roots, using rename so each state is durable.
			_ = os.Remove(specPath)
			_ = os.Symlink(staged, specPath)
			_ = os.Remove(specPath)
			_ = os.WriteFile(specPath, []byte(inRoot), 0o600)
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })

	r := &runResolver{httpTransport: true, allowedRoots: []string{root}}
	for i := 0; i < 400; i++ {
		got, err := r.discoverSpecConfined(root, "")
		if err != nil {
			// ANY error is acceptable here — a confinement refusal, or a
			// transient I/O failure against a pathname the flipper is
			// mid-replacement on. What may never happen is a SUCCESS carrying
			// the escape target's bytes, which is the invariant below. Asserting
			// a particular error shape would make this row flaky without
			// strengthening it.
			continue
		}
		if got != nil && strings.Contains(string(got.Contents), escapeMarker) {
			t.Fatalf("iteration %d: the walk returned the ESCAPE TARGET's bytes under a concurrent path swap", i)
		}
	}
}
