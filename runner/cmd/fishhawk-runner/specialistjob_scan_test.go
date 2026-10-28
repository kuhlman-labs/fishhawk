package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Tests for the fanin_build, overlap_scan and plan_crosscheck specialist-job
// kinds (E85.9 / #4300). Every test drives REAL git against a temp fixture
// repo; go test runs through a scripted gate exec seam except in the
// end-to-end test, which runs the real toolchain.

// scanFixtureFiles is a tiny Go module whose one test passes.
var scanFixtureFiles = map[string]string{
	"go.mod":      "module example.com/scan\n\ngo 1.25\n",
	"lib.go":      "package scan\n\n// Answer is what the fixture test pins.\nfunc Answer() int { return 42 }\n",
	"lib_test.go": "package scan\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal(\"Answer drifted\")\n\t}\n}\n",
	"shared.txt":  "base\n",
}

// Run ids for the fixture's run refs.
const (
	scanRunA = "0b0e3c1e-5f4a-4d55-9a3c-6f1f2a9d0c0a"
	scanRunB = "0b0e3c1e-5f4a-4d55-9a3c-6f1f2a9d0c0b"
	scanRunC = "0b0e3c1e-5f4a-4d55-9a3c-6f1f2a9d0c0c"
	scanRunD = "0b0e3c1e-5f4a-4d55-9a3c-6f1f2a9d0c0d"
	// scanMissingSHA names no object in any fixture.
	scanMissingSHA = "abababababababababababababababababababab"
)

// scanFixture is a committed module plus branches cut from one base:
// a adds a.txt, b and c rewrite shared.txt differently (they conflict), d
// adds d.txt.
type scanFixture struct {
	repo, base string
	sha        map[string]string
}

func newScanFixture(t *testing.T) scanFixture {
	t.Helper()
	repo := t.TempDir()
	cfGit(t, repo, "init", "-q", "-b", "main")
	writeScanFiles(t, repo, scanFixtureFiles)
	cfGit(t, repo, "add", "-A")
	cfGit(t, repo, "commit", "-q", "-m", "base")
	f := scanFixture{repo: repo, base: cfGit(t, repo, "rev-parse", "HEAD"), sha: map[string]string{}}
	for name, files := range map[string]map[string]string{
		"a": {"a.txt": "a\n"},
		"b": {"shared.txt": "b\n"},
		"c": {"shared.txt": "c\n"},
		"d": {"d.txt": "d\n"},
	} {
		cfGit(t, repo, "checkout", "-q", "-B", "slice-"+name, f.base)
		writeScanFiles(t, repo, files)
		cfGit(t, repo, "add", "-A")
		cfGit(t, repo, "commit", "-q", "-m", "slice "+name)
		f.sha[name] = cfGit(t, repo, "rev-parse", "HEAD")
	}
	cfGit(t, repo, "checkout", "-q", "main")
	return f
}

func writeScanFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runRef(id, base, head string) map[string]any {
	return map[string]any{"run_id": id, "base_sha": base, "head_sha": head}
}

// test2json streams the scripted exec seam returns.
const (
	t2jPass      = "{\"Action\":\"run\",\"Test\":\"TestAnswer\"}\n{\"Action\":\"pass\",\"Test\":\"TestAnswer\"}\n{\"Action\":\"pass\"}\n"
	t2jFail      = "{\"Action\":\"run\",\"Test\":\"TestAnswer\"}\n{\"Action\":\"fail\",\"Test\":\"TestAnswer\"}\n{\"Action\":\"fail\"}\n"
	t2jBuildFail = "{\"ImportPath\":\"example.com/scan\",\"Action\":\"build-fail\"}\n{\"Action\":\"fail\",\"FailedBuild\":\"example.com/scan\"}\n"
)

// scanEvent is one entry of the ordered git / materialize / exec log the C3
// assertions read.
type scanEvent struct {
	kind string // git | materialize | exec
	dir  string // git: the dir; materialize: the SOURCE; exec: the run dir
	dest string // materialize: the returned clone
	argv []string
	lock bool // exec: whether the verify lock was held
	// scratchErr is requireScratchDir on the exec dir AT exec time (the
	// clone is discarded right after).
	scratchErr error
}

// scanRecorder wires an ordered event log and a scripted exec seam into r.
type scanRecorder struct {
	mu     sync.Mutex
	events []scanEvent
	out    string
	code   int
	disp   gateDisposition
	// inspect runs inside the exec seam (e.g. to read the tested tree).
	inspect func(dir string)
}

func recordScan(r *specialistJobRun) *scanRecorder {
	rec := &scanRecorder{out: t2jPass, disp: gateExecuted}
	innerGit := r.git.run
	r.git.run = func(ctx context.Context, dir string, args []string) (string, int, error) {
		rec.add(scanEvent{kind: "git", dir: dir, argv: append([]string(nil), args...)})
		return innerGit(ctx, dir, args)
	}
	innerMat := r.x.materialize
	r.x.materialize = func(ctx context.Context, src, sha, parent string) (string, error) {
		dir, err := innerMat(ctx, src, sha, parent)
		rec.add(scanEvent{kind: "materialize", dir: src, dest: dir})
		return dir, err
	}
	lockPath := r.x.lockPath
	r.x.exec = func(_ context.Context, argv []string, dir, _ string, _ time.Duration) (string, int, gateDisposition) {
		link, err := os.Readlink(lockPath)
		rec.add(scanEvent{kind: "exec", dir: dir, argv: append([]string(nil), argv...), lock: err == nil && strings.HasSuffix(link, ":runner"),
			scratchErr: requireScratchDir(dir, r.parent)})
		if rec.inspect != nil {
			rec.inspect(dir)
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		return rec.out, rec.code, rec.disp
	}
	return rec
}

func (rec *scanRecorder) add(e scanEvent) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.events = append(rec.events, e)
}

func (rec *scanRecorder) of(kind string) []scanEvent {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []scanEvent
	for _, e := range rec.events {
		if e.kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// assertC3 pins binding condition C3 on the recorded log: every exec held
// the verify lock and ran in a scratch clone of its own (no two execs and no
// git-touched clone share a dir), and no git call — nor a clone sourced from
// it — ever reached a clone after code executed in it.
func (rec *scanRecorder) assertC3(t *testing.T) {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	inside := func(d, root string) bool { return d == root || strings.HasPrefix(d, root+string(filepath.Separator)) }
	var executed []string
	for i, e := range rec.events {
		switch e.kind {
		case "exec":
			if !e.lock {
				t.Errorf("event %d: %v ran without the verify lock", i, e.argv)
			}
			if e.scratchErr != nil {
				t.Errorf("event %d: exec outside scratch: %v", i, e.scratchErr)
			}
			for _, x := range executed {
				if inside(e.dir, x) {
					t.Errorf("event %d: exec reused clone %s", i, x)
				}
			}
			executed = append(executed, e.dir)
		case "git", "materialize":
			for _, x := range executed {
				if inside(e.dir, x) {
					t.Errorf("event %d: %s %v touched %s after code executed in it", i, e.kind, e.argv, x)
				}
			}
		}
	}
	for _, e := range rec.events {
		if e.kind != "git" {
			continue
		}
		for _, x := range executed {
			if inside(e.dir, x) || inside(x, e.dir) {
				t.Errorf("git-touched dir %s shares a clone with exec dir %s", e.dir, x)
			}
		}
	}
}

// missingHeadRun is a run whose head the source does not hold. Its
// materialize seam is FAKE: a real clone whose rev-parse fails returns before
// its detached fetch maintenance settles (AGENTS.md #3503 trap).
func missingHeadRun(t *testing.T, f scanFixture, kind string, inputs any) *specialistJobRun {
	t.Helper()
	r := newTestSJRun(t, f.repo, kind, scanMissingSHA, inputs)
	r.x.materialize = func(context.Context, string, string, string) (string, error) {
		return "", fmt.Errorf("wrapped: %w", gateiso.ErrCommitNotFound)
	}
	return r
}

// ---- fanin_build ----

func faninInputs(pkgs []string, slices ...string) map[string]any {
	ss := make([]any, 0, len(slices))
	for _, s := range slices {
		ss = append(ss, map[string]any{"name": "slice-" + s, "head_sha": s})
	}
	return map[string]any{"slices": ss, "packages": pkgs}
}

func faninRun(t *testing.T, f scanFixture, pkgs []string, slices ...string) (*specialistJobRun, *scanRecorder) {
	t.Helper()
	shas := make([]string, len(slices))
	for i, s := range slices {
		shas[i] = s
		if sha, ok := f.sha[s]; ok {
			shas[i] = sha
		}
	}
	r := newTestSJRun(t, f.repo, "fanin_build", f.base, faninInputs(pkgs, shas...))
	return r, recordScan(r)
}

// TestFanin_ConflictFails: slices merge in order onto the head; the one that
// conflicts records its unmerged path and is aborted, so the NEXT slice still
// merges; packages are skipped and the verdict is fail.
func TestFanin_ConflictFails(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	r, rec := faninRun(t, f, []string{"."}, "a", "b", "c", "d")
	o := executeFaninJob(context.Background(), r)
	if o.Verdict != sjVerdictFail || o.Reason != "" {
		t.Fatalf("verdict %s/%s, want fail (%+v)", o.Verdict, o.Reason, o.Evidence)
	}
	ev := o.Evidence.(sjFaninEvidence)
	want := []string{faninSliceMerged, faninSliceMerged, faninSliceConflict, faninSliceMerged}
	for i, w := range want {
		if ev.Slices[i].Outcome != w {
			t.Fatalf("slice %d outcome %q, want %q (%+v)", i, ev.Slices[i].Outcome, w, ev.Slices)
		}
	}
	if got := ev.Slices[2].ConflictPaths; !reflect.DeepEqual(got, []string{"shared.txt"}) {
		t.Fatalf("conflict paths %v, want [shared.txt]", got)
	}
	if len(ev.Packages) != 1 || ev.Packages[0].Outcome != faninPkgSkipped || ev.Packages[0].Reason != faninReasonMergeIncomplete {
		t.Fatalf("packages %+v, want skipped/merge_incomplete", ev.Packages)
	}
	if n := len(rec.of("exec")); n != 0 {
		t.Fatalf("exec invocations = %d, want 0 for an incomplete merge", n)
	}
	if status := cfGit(t, f.repo, "status", "--porcelain"); status != "" {
		t.Fatalf("source checkout modified: %s", status)
	}
}

// TestFanin_PackageFailFails: every slice merges, and a failing (or
// non-compiling) package fails the fan-in.
func TestFanin_PackageFailFails(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	for _, tc := range []struct {
		name, out, reason string
	}{
		{"red", t2jFail, ""},
		{"build_failed", t2jBuildFail, cfReasonBuildFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, rec := faninRun(t, f, []string{"."}, "a")
			rec.out, rec.code = tc.out, 1
			o := executeFaninJob(context.Background(), r)
			ev := o.Evidence.(sjFaninEvidence)
			if o.Verdict != sjVerdictFail || ev.Packages[0].Outcome != faninPkgFail || ev.Packages[0].Reason != tc.reason {
				t.Fatalf("verdict %s, packages %+v; want fail with reason %q", o.Verdict, ev.Packages, tc.reason)
			}
			if ev.Slices[0].Outcome != faninSliceMerged {
				t.Fatalf("slice %+v, want merged", ev.Slices[0])
			}
		})
	}
}

// TestFanin_Pass: every slice merges and every package passes. Each package
// runs `go test -json -count=1` under the verify lock in its OWN fresh clone,
// materialized from the untouched merge clone and carrying every slice (C3).
func TestFanin_Pass(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	r, rec := faninRun(t, f, []string{".", "./..."}, "a", "d")
	var missing []string
	var mu sync.Mutex
	rec.inspect = func(dir string) {
		for _, name := range []string{"a.txt", "d.txt", "lib.go"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				mu.Lock()
				missing = append(missing, dir+"/"+name)
				mu.Unlock()
			}
		}
	}
	o := executeFaninJob(context.Background(), r)
	if o.Verdict != sjVerdictPass || o.Reason != "" {
		t.Fatalf("verdict %s/%s, want pass (%+v)", o.Verdict, o.Reason, o.Evidence)
	}
	if len(missing) != 0 {
		t.Fatalf("tested tree lacks merged files: %v", missing)
	}
	execs := rec.of("exec")
	if len(execs) != 2 {
		t.Fatalf("exec invocations = %d, want one per package", len(execs))
	}
	for i, want := range []string{"go test -json -count=1 .", "go test -json -count=1 ./..."} {
		if got := strings.Join(execs[i].argv, " "); got != want {
			t.Fatalf("exec %d argv %q, want %q", i, got, want)
		}
	}
	mats := rec.of("materialize")
	if len(mats) != 3 || mats[0].dir != f.repo {
		t.Fatalf("materializations %+v, want the merge clone from the source then one per package", mats)
	}
	mergeClone := mats[0].dest
	for i, e := range execs {
		if mats[i+1].dir != mergeClone || e.dir != mats[i+1].dest {
			t.Fatalf("package %d ran in %s, want a fresh clone of the merge clone %s (%+v)", i, e.dir, mergeClone, mats)
		}
	}
	rec.assertC3(t)
	if _, err := os.Stat(execs[0].dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("executed clone %s was not discarded: %v", execs[0].dir, err)
	}
}

// TestFaninMerge_UsesJobIdentityWithoutConfigWrite (ADR-115 D2, #4648): the
// fan-in merge commits carry the fan-in identity as author AND committer, set
// per command by hostJobGitRun, and the merge clone's config gains no
// user.name, user.email or maintenance.auto key. An AMBIENT identity is set in
// the process env first, so a runner that dropped IdentityEnv would record
// "ambient" (or fail under GIT_CONFIG_GLOBAL=/dev/null), never the fan-in one.
// Non-parallel: t.Setenv.
func TestFaninMerge_UsesJobIdentityWithoutConfigWrite(t *testing.T) {
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "ambient")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "ambient@example.invalid")
	}
	f := newScanFixture(t)
	r, rec := faninRun(t, f, []string{"."}, "a", "d")
	var mu sync.Mutex
	var idents []string
	configKeys := map[string]string{}
	configRead := false
	rec.inspect = func(dir string) {
		out, err := exec.Command("git", "-C", dir, "log", "--merges", "--format=%an <%ae> / %cn <%ce>").Output()
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			idents = append(idents, "log failed: "+err.Error())
			return
		}
		idents = append(idents, strings.Split(strings.TrimSpace(string(out)), "\n")...)
		if mats := rec.of("materialize"); len(mats) > 0 {
			cfgPath := filepath.Join(mats[0].dest, ".git", "config")
			if _, err := os.Stat(cfgPath); err == nil {
				configRead = true
			}
			for _, key := range []string{"user.name", "user.email", "maintenance.auto"} {
				if v, err := exec.Command("git", "config", "--file", cfgPath, "--get", key).Output(); err == nil {
					configKeys[key] = strings.TrimSpace(string(v))
				}
			}
		}
	}
	o := executeFaninJob(context.Background(), r)
	if o.Verdict != sjVerdictPass {
		t.Fatalf("verdict %s/%s, want pass (%+v)", o.Verdict, o.Reason, o.Evidence)
	}
	mu.Lock()
	defer mu.Unlock()
	want := faninCommitterName + " <" + faninCommitterEmail + "> / " + faninCommitterName + " <" + faninCommitterEmail + ">"
	if len(idents) != 2 {
		t.Fatalf("merge commits = %q, want the two fan-in merges", idents)
	}
	for _, got := range idents {
		if got != want {
			t.Errorf("fan-in merge identity = %q, want %q", got, want)
		}
	}
	if !configRead {
		t.Fatal("the merge clone's config was never read: the no-write assertion would be vacuous")
	}
	if len(configKeys) != 0 {
		t.Errorf("merge clone config gained %v, want no user.name/user.email/maintenance.auto write", configKeys)
	}
}

// TestHostJobGitRun_CarriesPinsAndIdentity (ADR-115 D2, #4648): every
// specialist-job git child carries the maintenance pins and the fan-in
// identity as command-scope env, independent of the process-wide install
// (cleared here). The fixture's repo-local maintenance.auto=true / gc.auto=6700
// is what a child without the pins reads (GIT_CONFIG_GLOBAL=/dev/null drops the
// package TestMain's global pin). A malformed inherited GIT_CONFIG_COUNT fails
// the call without spawning git. Non-parallel: t.Setenv.
func TestHostJobGitRun_CarriesPinsAndIdentity(t *testing.T) {
	isolateGitConfigEnv(t)
	repo := maskedMaintenanceRepo(t)
	ctx := context.Background()
	for key, want := range map[string]string{"maintenance.auto": "false", "gc.auto": "0"} {
		out, code, err := hostJobGitRun(ctx, repo, []string{"config", "--get", key})
		if err != nil || code != 0 || strings.TrimSpace(out) != want {
			t.Errorf("hostJobGitRun config --get %s = (%q, %d, %v), want %q", key, out, code, err, want)
		}
	}
	for _, v := range []string{"GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"} {
		out, code, err := hostJobGitRun(ctx, repo, []string{"var", v})
		if want := faninCommitterName + " <" + faninCommitterEmail + ">"; err != nil || code != 0 || !strings.HasPrefix(out, want) {
			t.Errorf("hostJobGitRun var %s = (%q, %d, %v), want prefix %q", v, out, code, err, want)
		}
	}
	t.Setenv("GIT_CONFIG_COUNT", "x")
	if _, code, err := hostJobGitRun(ctx, repo, []string{"rev-parse", "HEAD"}); err == nil || code != -1 || !strings.Contains(err.Error(), "GIT_CONFIG_COUNT") {
		t.Errorf("hostJobGitRun under GIT_CONFIG_COUNT=x = (code %d, %v), want -1 and an error naming GIT_CONFIG_COUNT", code, err)
	}
}

// TestFanin_TimeoutAndGateErrorsInconclusive maps every non-verdict exec
// result onto an inconclusive fan-in.
func TestFanin_TimeoutAndGateErrorsInconclusive(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	for _, tc := range []struct {
		name            string
		disp            gateDisposition
		out             string
		outcome, reason string
		verdictReason   string
	}{
		{"timeout", gateTimedOut, "", faninPkgTimeout, "", sjReasonTimeout},
		{"gate_refused", gateRefused, "", faninPkgError, cfReasonGateRefused, sjReasonExecutionError},
		{"gate_unavailable", gateUnavailable, "", faninPkgError, cfReasonGateUnavailable, sjReasonExecutionError},
		{"no_tests", gateExecuted, "{\"Action\":\"pass\"}\n", faninPkgError, cfReasonNoTestsMatched, sjReasonExecutionError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, rec := faninRun(t, f, []string{"."}, "a")
			rec.disp, rec.out = tc.disp, tc.out
			o := executeFaninJob(context.Background(), r)
			p := o.Evidence.(sjFaninEvidence).Packages[0]
			if o.Verdict != sjVerdictInconclusive || o.Reason != tc.verdictReason || p.Outcome != tc.outcome || p.Reason != tc.reason {
				t.Fatalf("verdict %s/%s package %+v; want inconclusive/%s with %s/%s", o.Verdict, o.Reason, p, tc.verdictReason, tc.outcome, tc.reason)
			}
		})
	}
	t.Run("lock_unavailable", func(t *testing.T) {
		t.Parallel()
		r, rec := faninRun(t, f, []string{"."}, "a")
		r.x.lockPath = ""
		o := executeFaninJob(context.Background(), r)
		p := o.Evidence.(sjFaninEvidence).Packages[0]
		if o.Verdict != sjVerdictInconclusive || p.Outcome != faninPkgError || p.Reason != cfReasonLockUnavailable || len(rec.of("exec")) != 0 {
			t.Fatalf("verdict %s package %+v execs %d; want error/verify_lock_unavailable and nothing run", o.Verdict, p, len(rec.of("exec")))
		}
	})
	t.Run("bad_package", func(t *testing.T) {
		t.Parallel()
		r, _ := faninRun(t, f, []string{"../escape"}, "a")
		p := executeFaninJob(context.Background(), r).Evidence.(sjFaninEvidence).Packages[0]
		if p.Outcome != faninPkgError || p.Reason != cfReasonInvalidSpec {
			t.Fatalf("package %+v, want error/invalid_spec", p)
		}
	})
	t.Run("package_clone_fails", func(t *testing.T) {
		t.Parallel()
		r, _ := faninRun(t, f, []string{"."}, "a")
		inner := r.x.materialize
		calls := 0
		r.x.materialize = func(ctx context.Context, src, sha, parent string) (string, error) {
			if calls++; calls > 1 {
				return "", errors.New("clone refused")
			}
			return inner(ctx, src, sha, parent)
		}
		p := executeFaninJob(context.Background(), r).Evidence.(sjFaninEvidence).Packages[0]
		if p.Outcome != faninPkgError || p.Reason != cfReasonCloneFailed {
			t.Fatalf("package %+v, want error/clone_failed", p)
		}
	})
}

// TestFanin_UnresolvableSliceInconclusive: a slice SHA the source does not
// hold is an error slice, packages are skipped and the reason names it.
func TestFanin_UnresolvableSliceInconclusive(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	r, rec := faninRun(t, f, []string{"."}, "a", scanMissingSHA)
	o := executeFaninJob(context.Background(), r)
	ev := o.Evidence.(sjFaninEvidence)
	if o.Verdict != sjVerdictInconclusive || o.Reason != sjReasonUnresolvableSHA {
		t.Fatalf("verdict %s/%s, want inconclusive/unresolvable_sha (%+v)", o.Verdict, o.Reason, ev)
	}
	if ev.Slices[0].Outcome != faninSliceMerged || ev.Slices[1].Outcome != faninSliceError || ev.Packages[0].Outcome != faninPkgSkipped {
		t.Fatalf("evidence %+v", ev)
	}
	if len(rec.of("exec")) != 0 {
		t.Fatal("packages ran on an incomplete merge")
	}
}

func TestFanin_RefusesInvalidInputsAndHead(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	ctx := context.Background()
	for name, in := range map[string]any{
		"no_slices":    map[string]any{"slices": []any{}, "packages": []string{"."}},
		"no_packages":  faninInputs([]string{}, f.sha["a"]),
		"flag_package": faninInputs([]string{"./-toolexec=x"}, f.sha["a"]),
		"unknown":      map[string]any{"slices": []any{}, "packages": []string{"."}, "extra": 1},
	} {
		if o := executeFaninJob(ctx, newTestSJRun(t, f.repo, "fanin_build", f.base, in)); o.Verdict != sjVerdictInconclusive || o.Reason != sjReasonInputsInvalid {
			t.Fatalf("%s: %+v, want inconclusive/inputs_invalid", name, o)
		}
	}
	if o := executeFaninJob(ctx, newTestSJRun(t, f.repo, "fanin_build", "HEAD", faninInputs([]string{"."}, f.sha["a"]))); o.Reason != sjReasonUnresolvableSHA {
		t.Fatalf("symbolic head: %+v, want unresolvable_sha", o)
	}
	if o := executeFaninJob(ctx, missingHeadRun(t, f, "fanin_build", faninInputs([]string{"."}, f.sha["a"]))); o.Reason != sjReasonUnresolvableSHA {
		t.Fatalf("missing head: %+v, want unresolvable_sha", o)
	}
}

// TestFreshCloneFrom_RefusesTaintedSource: the fan-in re-clones its merged
// tree from the merge clone, and cloning runs host git against that source —
// so a source that has executed code is refused before any clone runs.
func TestFreshCloneFrom_RefusesTaintedSource(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	r, rec := faninRun(t, f, []string{"."}, "a")
	ctx := context.Background()
	src, err := r.freshClone(ctx, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if dir, err := r.freshCloneFrom(ctx, src, f.base); err != nil || requireScratchDir(dir, r.parent) != nil {
		t.Fatalf("untainted source: %q, %v", dir, err)
	}
	before := len(rec.of("materialize"))
	r.git.taint(src)
	if _, err := r.freshCloneFrom(ctx, src, f.base); !errors.Is(err, errSpecialistJobCloneTainted) {
		t.Fatalf("tainted source: %v, want errSpecialistJobCloneTainted", err)
	}
	if _, err := r.freshCloneFrom(ctx, f.repo, f.base); !errors.Is(err, errSpecialistJobNotScratch) {
		t.Fatalf("source checkout as clone source: %v, want errSpecialistJobNotScratch", err)
	}
	if after := len(rec.of("materialize")); after != before {
		t.Fatalf("materialize ran %d time(s) after a refusal", after-before)
	}
}

// TestEnsureCommit_FetchesBySHAFromSource: a commit the source gained after
// the clone is fetched by SHA; a malformed or unknown SHA is refused.
func TestEnsureCommit_FetchesBySHAFromSource(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	r := newTestSJRun(t, f.repo, "overlap_scan", f.base, map[string]any{})
	ctx := context.Background()
	clone, err := r.freshClone(ctx, f.base)
	if err != nil {
		t.Fatal(err)
	}
	cfGit(t, f.repo, "commit", "-q", "--allow-empty", "-m", "after the clone")
	later := cfGit(t, f.repo, "rev-parse", "HEAD")
	if r.commitResolves(ctx, clone, later) {
		t.Fatal("fixture: the later commit is already in the clone")
	}
	if !r.ensureCommit(ctx, clone, later) || !r.commitResolves(ctx, clone, later) {
		t.Fatal("ensureCommit did not fetch the later commit by SHA")
	}
	if r.ensureCommit(ctx, clone, scanMissingSHA) {
		t.Fatal("ensureCommit resolved an unknown SHA")
	}
	// A malformed SHA is refused before ANY git runs (the equality check in
	// commitResolves would also reject it, so count the calls to isolate it).
	calls := 0
	inner := r.git.run
	r.git.run = func(ctx context.Context, dir string, args []string) (string, int, error) {
		calls++
		return inner(ctx, dir, args)
	}
	for _, bad := range []string{"HEAD", strings.ToUpper(later), "", "--upload-pack=x"} {
		if r.ensureCommit(ctx, clone, bad) || calls != 0 {
			t.Fatalf("ensureCommit(%q): ran %d git call(s)", bad, calls)
		}
	}
}

func TestFaninVerdict_Table(t *testing.T) {
	t.Parallel()
	slice := func(o string) sjFaninSliceEvidence { return sjFaninSliceEvidence{Outcome: o} }
	pkg := func(o string) sjFaninPackageEvidence { return sjFaninPackageEvidence{Outcome: o} }
	for _, tc := range []struct {
		ev              sjFaninEvidence
		unresolved      bool
		verdict, reason string
	}{
		{sjFaninEvidence{Slices: []sjFaninSliceEvidence{slice("merged")}, Packages: []sjFaninPackageEvidence{pkg("pass")}}, false, sjVerdictPass, ""},
		{sjFaninEvidence{Slices: []sjFaninSliceEvidence{slice("error"), slice("conflict")}}, false, sjVerdictFail, ""},
		{sjFaninEvidence{Slices: []sjFaninSliceEvidence{slice("merged")}, Packages: []sjFaninPackageEvidence{pkg("timeout"), pkg("fail")}}, false, sjVerdictFail, ""},
		{sjFaninEvidence{Slices: []sjFaninSliceEvidence{slice("error")}, Packages: []sjFaninPackageEvidence{pkg("skipped")}}, false, sjVerdictInconclusive, sjReasonExecutionError},
		{sjFaninEvidence{Slices: []sjFaninSliceEvidence{slice("error")}}, true, sjVerdictInconclusive, sjReasonUnresolvableSHA},
		{sjFaninEvidence{Packages: []sjFaninPackageEvidence{pkg("pass"), pkg("timeout")}}, false, sjVerdictInconclusive, sjReasonTimeout},
		{sjFaninEvidence{Packages: []sjFaninPackageEvidence{pkg("error")}}, false, sjVerdictInconclusive, sjReasonExecutionError},
	} {
		if v, r := faninVerdict(tc.ev, tc.unresolved); v != tc.verdict || r != tc.reason {
			t.Fatalf("%+v unresolved=%v → %s/%s, want %s/%s", tc.ev, tc.unresolved, v, r, tc.verdict, tc.reason)
		}
	}
}

// ---- overlap_scan ----

func overlapRun(t *testing.T, f scanFixture, watch []string, runs ...map[string]any) (*specialistJobRun, *scanRecorder) {
	t.Helper()
	in := map[string]any{"runs": runs}
	if watch != nil {
		in["watch_paths"] = watch
	}
	r := newTestSJRun(t, f.repo, "overlap_scan", f.base, in)
	return r, recordScan(r)
}

// TestOverlap_DetectsPairAndWatchHit isolates each fail arm: a pairwise
// overlap alone, a watch hit alone, and neither (pass).
func TestOverlap_DetectsPairAndWatchHit(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	ctx := context.Background()

	t.Run("overlap_only", func(t *testing.T) {
		t.Parallel()
		r, rec := overlapRun(t, f, nil, runRef(scanRunB, f.base, f.sha["b"]), runRef(scanRunC, f.base, f.sha["c"]))
		o := executeOverlapJob(ctx, r)
		ev := o.Evidence.(sjOverlapEvidence)
		want := []sjOverlapPairEvidence{{RunA: scanRunB, RunB: scanRunC, Paths: []string{"shared.txt"}}}
		if o.Verdict != sjVerdictFail || !reflect.DeepEqual(ev.Overlaps, want) || len(ev.WatchHits) != 0 {
			t.Fatalf("verdict %s overlaps %+v hits %+v; want fail on %+v", o.Verdict, ev.Overlaps, ev.WatchHits, want)
		}
		if len(rec.of("exec")) != 0 {
			t.Fatal("overlap_scan executed code")
		}
	})
	t.Run("watch_hit_only", func(t *testing.T) {
		t.Parallel()
		r, _ := overlapRun(t, f, []string{"a.*", "nothing/*"}, runRef(scanRunA, f.base, f.sha["a"]), runRef(scanRunD, f.base, f.sha["d"]))
		o := executeOverlapJob(ctx, r)
		ev := o.Evidence.(sjOverlapEvidence)
		want := []sjWatchHitEvidence{{RunID: scanRunA, WatchPath: "a.*", Paths: []string{"a.txt"}}}
		if o.Verdict != sjVerdictFail || len(ev.Overlaps) != 0 || !reflect.DeepEqual(ev.WatchHits, want) {
			t.Fatalf("verdict %s overlaps %+v hits %+v; want fail on %+v", o.Verdict, ev.Overlaps, ev.WatchHits, want)
		}
	})
	t.Run("disjoint_passes", func(t *testing.T) {
		t.Parallel()
		r, _ := overlapRun(t, f, []string{"nothing/*"}, runRef(scanRunA, f.base, f.sha["a"]), runRef(scanRunD, f.base, f.sha["d"]))
		o := executeOverlapJob(ctx, r)
		ev := o.Evidence.(sjOverlapEvidence)
		want := []sjOverlapRunEvidence{
			{RunID: scanRunA, Resolved: true, ChangedFiles: []string{"a.txt"}},
			{RunID: scanRunD, Resolved: true, ChangedFiles: []string{"d.txt"}},
		}
		if o.Verdict != sjVerdictPass || !reflect.DeepEqual(ev.Runs, want) {
			t.Fatalf("verdict %s runs %+v; want pass on %+v", o.Verdict, ev.Runs, want)
		}
	})
}

// TestOverlap_UnresolvableSHAInconclusive: an unresolvable run range makes a
// clean scan inconclusive, but a decided overlap still fails.
func TestOverlap_UnresolvableSHAInconclusive(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	ctx := context.Background()
	r, _ := overlapRun(t, f, nil, runRef(scanRunA, f.base, f.sha["a"]), runRef(scanRunD, scanMissingSHA, f.sha["d"]))
	o := executeOverlapJob(ctx, r)
	ev := o.Evidence.(sjOverlapEvidence)
	if o.Verdict != sjVerdictInconclusive || o.Reason != sjReasonUnresolvableSHA || ev.Runs[1].Resolved || len(ev.Runs[1].ChangedFiles) != 0 {
		t.Fatalf("verdict %s/%s runs %+v; want inconclusive/unresolvable_sha with run D unresolved", o.Verdict, o.Reason, ev.Runs)
	}
	r, _ = overlapRun(t, f, nil, runRef(scanRunB, f.base, f.sha["b"]), runRef(scanRunC, f.base, f.sha["c"]), runRef(scanRunD, f.base, scanMissingSHA))
	if o = executeOverlapJob(ctx, r); o.Verdict != sjVerdictFail {
		t.Fatalf("decided overlap beside an unresolved run: %s/%s, want fail", o.Verdict, o.Reason)
	}
}

func TestOverlap_RefusesInvalidInputsAndHead(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	ctx := context.Background()
	two := []map[string]any{runRef(scanRunA, f.base, f.sha["a"]), runRef(scanRunD, f.base, f.sha["d"])}
	for name, in := range map[string]map[string]any{
		"one_run":    {"runs": two[:1]},
		"bad_glob":   {"runs": two, "watch_paths": []string{"["}},
		"empty_glob": {"runs": two, "watch_paths": []string{""}},
		"unknown":    {"runs": two, "extra": true},
	} {
		if o := executeOverlapJob(ctx, newTestSJRun(t, f.repo, "overlap_scan", f.base, in)); o.Verdict != sjVerdictInconclusive || o.Reason != sjReasonInputsInvalid {
			t.Fatalf("%s: %+v, want inconclusive/inputs_invalid", name, o)
		}
	}
	if o := executeOverlapJob(ctx, newTestSJRun(t, f.repo, "overlap_scan", "", map[string]any{"runs": two})); o.Reason != sjReasonUnresolvableSHA {
		t.Fatalf("no head: %+v, want unresolvable_sha", o)
	}
	if o := executeOverlapJob(ctx, missingHeadRun(t, f, "overlap_scan", map[string]any{"runs": two})); o.Reason != sjReasonUnresolvableSHA {
		t.Fatalf("missing head: %+v, want unresolvable_sha", o)
	}
}

// TestOverlap_TruncatesChangedFiles: a run changing more than 500 files
// reports the first 500 and truncated.
func TestOverlap_TruncatesChangedFiles(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	cfGit(t, f.repo, "checkout", "-q", "-B", "big", f.base)
	files := map[string]string{}
	for i := 0; i <= scanMaxPaths; i++ {
		files[fmt.Sprintf("big/f%03d.txt", i)] = "x\n"
	}
	writeScanFiles(t, f.repo, files)
	cfGit(t, f.repo, "add", "-A")
	cfGit(t, f.repo, "commit", "-q", "-m", "big")
	big := cfGit(t, f.repo, "rev-parse", "HEAD")
	cfGit(t, f.repo, "checkout", "-q", "main")
	r, _ := overlapRun(t, f, nil, runRef(scanRunA, f.base, big), runRef(scanRunD, f.base, f.sha["d"]))
	o := executeOverlapJob(context.Background(), r)
	run := o.Evidence.(sjOverlapEvidence).Runs[0]
	if !run.Truncated || len(run.ChangedFiles) != scanMaxPaths || run.ChangedFiles[0] != "big/f000.txt" {
		t.Fatalf("run %d files truncated=%v first=%q; want %d truncated", len(run.ChangedFiles), run.Truncated, run.ChangedFiles[0], scanMaxPaths)
	}
}

func TestScanPathHelpers(t *testing.T) {
	t.Parallel()
	if got, tr := splitNULPaths("a\x00b\x00"); !reflect.DeepEqual(got, []string{"a", "b"}) || tr {
		t.Fatalf("complete: %v %v", got, tr)
	}
	if got, tr := splitNULPaths("a\x00b/partia"); !reflect.DeepEqual(got, []string{"a"}) || !tr {
		t.Fatalf("capped stdout: %v %v, want [a] truncated", got, tr)
	}
	if got, tr := splitNULPaths(""); len(got) != 0 || got == nil || tr {
		t.Fatalf("empty: %#v %v", got, tr)
	}
	long := strings.Repeat("d/", scanMaxPathLen/2) + "x"
	if got, tr := capPaths([]string{"a", long, "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) || !tr {
		t.Fatalf("over-long path: %v %v, want dropped and truncated", got, tr)
	}
	if got := intersectSorted([]string{"a", "c", "e"}, []string{"b", "c", "d", "e"}); !reflect.DeepEqual(got, []string{"c", "e"}) {
		t.Fatalf("intersect: %v", got)
	}
}

// ---- plan_crosscheck ----

func crosscheckRun(t *testing.T, f scanFixture, in map[string]any) (*specialistJobRun, *scanRecorder) {
	t.Helper()
	r := newTestSJRun(t, f.repo, "plan_crosscheck", f.base, in)
	return r, recordScan(r)
}

func focus(p, expect string) map[string]any { return map[string]any{"path": p, "expect": expect} }

// TestCrosscheck_FocusMismatchAndCompareRunOverlap isolates each verdict
// arm on its own fixture.
func TestCrosscheck_FocusMismatchAndCompareRunOverlap(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name            string
		in              map[string]any
		verdict, reason string
		check           func(t *testing.T, ev sjCrosscheckEvidence)
	}{
		{"focus_mismatch", map[string]any{"focus_files": []any{focus("lib.go", "present"), focus("nope.go", "present")}},
			sjVerdictFail, "", func(t *testing.T, ev sjCrosscheckEvidence) {
				if ev.FocusFiles[0].Observed != focusPresent || !ev.FocusFiles[0].Matched || ev.FocusFiles[1].Observed != focusAbsent || ev.FocusFiles[1].Matched {
					t.Fatalf("focus %+v", ev.FocusFiles)
				}
			}},
		{"compare_run_touches_focus", map[string]any{
			"focus_files":  []any{focus("shared.txt", "present"), focus("lib.go", "present")},
			"compare_runs": []any{runRef(scanRunB, f.base, f.sha["b"]), runRef(scanRunA, f.base, f.sha["a"])},
		}, sjVerdictFail, "", func(t *testing.T, ev sjCrosscheckEvidence) {
			if !ev.FocusFiles[0].Matched || !reflect.DeepEqual(ev.FocusFiles[0].TouchedBy, []string{scanRunB}) || len(ev.FocusFiles[1].TouchedBy) != 0 {
				t.Fatalf("focus %+v, want shared.txt touched by run B only", ev.FocusFiles)
			}
			want := []sjCompareRunEvidence{
				{RunID: scanRunB, Resolved: true, OverlapPaths: []string{"shared.txt"}},
				{RunID: scanRunA, Resolved: true, OverlapPaths: []string{}},
			}
			if !reflect.DeepEqual(ev.CompareRuns, want) {
				t.Fatalf("compare runs %+v, want %+v", ev.CompareRuns, want)
			}
		}},
		{"clean_passes", map[string]any{
			"focus_files":  []any{focus("./lib.go", "present"), focus("nope.go", "absent")},
			"compare_runs": []any{runRef(scanRunA, f.base, f.sha["a"])},
		}, sjVerdictPass, "", nil},
		{"unresolvable_compare_run", map[string]any{
			"focus_files":  []any{focus("lib.go", "present")},
			"compare_runs": []any{runRef(scanRunA, scanMissingSHA, f.sha["a"])},
		}, sjVerdictInconclusive, sjReasonUnresolvableSHA, func(t *testing.T, ev sjCrosscheckEvidence) {
			if ev.CompareRuns[0].Resolved {
				t.Fatalf("compare run %+v resolved", ev.CompareRuns[0])
			}
		}},
		{"focus_read_error", map[string]any{"focus_files": []any{focus(".git/config", "present")}},
			sjVerdictInconclusive, sjReasonProbeError, func(t *testing.T, ev sjCrosscheckEvidence) {
				if ev.FocusFiles[0].Observed != focusError || ev.FocusFiles[0].Matched {
					t.Fatalf("focus %+v, want error unmatched", ev.FocusFiles[0])
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, rec := crosscheckRun(t, f, tc.in)
			o := executeCrosscheckJob(ctx, r)
			if o.Verdict != tc.verdict || o.Reason != tc.reason {
				t.Fatalf("verdict %s/%s, want %s/%s (%+v)", o.Verdict, o.Reason, tc.verdict, tc.reason, o.Evidence)
			}
			if tc.check != nil {
				tc.check(t, o.Evidence.(sjCrosscheckEvidence))
			}
			if len(rec.of("exec")) != 0 {
				t.Fatal("focus/compare evaluation executed code")
			}
		})
	}
}

// TestCrosscheck_GoTestProbeAndCompareRuns is binding condition C3's test: a
// go_test question beside compare_runs. The compare-run diffs run in the
// crosscheck clone; the go_test probe runs under the verify lock in a clone
// of its own; no git call reaches a clone after code executed in it.
func TestCrosscheck_GoTestProbeAndCompareRuns(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	r, rec := crosscheckRun(t, f, map[string]any{
		"questions": []any{
			probeQ("answer-holds", "pass", map[string]any{"go_test": map[string]any{"tests": map[string]any{"run": "^TestAnswer$", "package": "."}}}),
			probeQ("lib-present", "pass", map[string]any{"path_exists": map[string]any{"path": "lib.go"}}),
		},
		"focus_files":  []any{focus("lib.go", "present")},
		"compare_runs": []any{runRef(scanRunA, f.base, f.sha["a"]), runRef(scanRunD, f.base, f.sha["d"])},
	})
	o := executeCrosscheckJob(context.Background(), r)
	if o.Verdict != sjVerdictPass || o.Reason != "" {
		t.Fatalf("verdict %s/%s, want pass (%+v)", o.Verdict, o.Reason, o.Evidence)
	}
	ev := o.Evidence.(sjCrosscheckEvidence)
	if len(ev.Questions) != 2 || !ev.Questions[0].Matched || ev.Questions[0].Observed != probeObservedPass || len(ev.CompareRuns) != 2 || !ev.CompareRuns[1].Resolved {
		t.Fatalf("evidence %+v", ev)
	}
	execs := rec.of("exec")
	if len(execs) != 1 || strings.Join(execs[0].argv, " ") != "go test -json -count=1 -run=^TestAnswer$ ." {
		t.Fatalf("execs %+v, want the one go_test probe", execs)
	}
	var diffDirs []string
	for _, g := range rec.of("git") {
		if len(g.argv) > 0 && g.argv[0] == "diff" {
			diffDirs = append(diffDirs, g.dir)
		}
	}
	if len(diffDirs) != 2 {
		t.Fatalf("compare-run diffs = %d, want 2", len(diffDirs))
	}
	if diffDirs[0] == execs[0].dir {
		t.Fatalf("go_test probe ran in the compare-run clone %s", execs[0].dir)
	}
	rec.assertC3(t)
}

func TestCrosscheck_RefusesInvalidInputsAndHead(t *testing.T) {
	t.Parallel()
	f := newScanFixture(t)
	ctx := context.Background()
	for name, in := range map[string]map[string]any{
		"nothing":     {"compare_runs": []any{runRef(scanRunA, f.base, f.sha["a"])}},
		"bad_expect":  {"focus_files": []any{focus("lib.go", "maybe")}},
		"unknown":     {"focus_files": []any{focus("lib.go", "present")}, "extra": 1},
		"too_many_cr": {"focus_files": []any{focus("lib.go", "present")}, "compare_runs": make([]any, scanMaxCompare+1)},
	} {
		if o := executeCrosscheckJob(ctx, newTestSJRun(t, f.repo, "plan_crosscheck", f.base, in)); o.Verdict != sjVerdictInconclusive || o.Reason != sjReasonInputsInvalid {
			t.Fatalf("%s: %+v, want inconclusive/inputs_invalid", name, o)
		}
	}
	in := map[string]any{"focus_files": []any{focus("lib.go", "present")}}
	if o := executeCrosscheckJob(ctx, newTestSJRun(t, f.repo, "plan_crosscheck", "", in)); o.Reason != sjReasonUnresolvableSHA {
		t.Fatalf("no head: %+v, want unresolvable_sha", o)
	}
	if o := executeCrosscheckJob(ctx, missingHeadRun(t, f, "plan_crosscheck", in)); o.Reason != sjReasonUnresolvableSHA {
		t.Fatalf("missing head: %+v, want unresolvable_sha", o)
	}
}

func TestCrosscheckVerdict_Table(t *testing.T) {
	t.Parallel()
	q := func(obs, exp string) sjProbeEvidence {
		return sjProbeEvidence{Observed: obs, Expected: exp, Matched: obs != probeObservedError && obs == exp}
	}
	ff := func(obs string, matched bool, touched ...string) sjFocusEvidence {
		return sjFocusEvidence{Observed: obs, Matched: matched, TouchedBy: append([]string{}, touched...)}
	}
	for _, tc := range []struct {
		ev              sjCrosscheckEvidence
		unresolved      string
		verdict, reason string
	}{
		{sjCrosscheckEvidence{Questions: []sjProbeEvidence{q("pass", "pass")}, FocusFiles: []sjFocusEvidence{ff("present", true)}}, "", sjVerdictPass, ""},
		{sjCrosscheckEvidence{Questions: []sjProbeEvidence{q("fail", "pass")}}, "", sjVerdictFail, ""},
		{sjCrosscheckEvidence{FocusFiles: []sjFocusEvidence{ff("absent", false)}}, sjReasonUnresolvableSHA, sjVerdictFail, ""},
		{sjCrosscheckEvidence{FocusFiles: []sjFocusEvidence{ff("present", true, scanRunA)}}, "", sjVerdictFail, ""},
		{sjCrosscheckEvidence{FocusFiles: []sjFocusEvidence{ff("present", true)}}, sjReasonExecutionError, sjVerdictInconclusive, sjReasonExecutionError},
		{sjCrosscheckEvidence{FocusFiles: []sjFocusEvidence{ff("error", false)}}, "", sjVerdictInconclusive, sjReasonProbeError},
		{sjCrosscheckEvidence{Questions: []sjProbeEvidence{q("error", "pass")}}, "", sjVerdictInconclusive, sjReasonProbeError},
	} {
		if v, r := crosscheckVerdict(tc.ev, tc.unresolved); v != tc.verdict || r != tc.reason {
			t.Fatalf("%+v unresolved=%q → %s/%s, want %s/%s", tc.ev, tc.unresolved, v, r, tc.verdict, tc.reason)
		}
	}
}

// ---- wire contract ----

// compileCompletionSchema compiles the CANONICAL specialist-job-v1 schema's
// $defs.completion with format assertion on, so a shipped completion is
// checked against exactly what the backend's ParseCompletion validates.
func compileCompletionSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "docs", "spec", "specialist-job-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	const id = "https://fishhawk.dev/spec/specialist-job-v1.schema.json"
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(id + "#/$defs/completion")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validateCompletion(t *testing.T, s *jsonschema.Schema, raw []byte) {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("completion fails the canonical schema: %v\n%s", err, raw)
	}
}

// TestScanKinds_EndToEnd drives `fishhawk-runner specialist-job` for each
// scan kind against the fixture repo and a fake backend, with the REAL gate
// exec seam (fanin_build runs the real go test). Each shipped completion
// validates against the canonical completion schema and decodes strictly
// into the kind's typed evidence.
func TestScanKinds_EndToEnd(t *testing.T) {
	useTestSJDeps(t, nil)
	f := newScanFixture(t)
	schema := compileCompletionSchema(t)
	for _, tc := range []struct {
		kind    string
		inputs  any
		verdict string
		target  func() any
	}{
		{"fanin_build", faninInputs([]string{"."}, f.sha["a"], f.sha["d"]), sjVerdictPass, func() any { return &sjFaninEvidence{} }},
		{"overlap_scan", map[string]any{
			"runs":        []any{runRef(scanRunB, f.base, f.sha["b"]), runRef(scanRunC, f.base, f.sha["c"]), runRef(scanRunD, scanMissingSHA, f.sha["d"])},
			"watch_paths": []string{"*.txt"},
		}, sjVerdictFail, func() any { return &sjOverlapEvidence{} }},
		{"plan_crosscheck", map[string]any{
			"questions":    []any{probeQ("lib-present", "pass", map[string]any{"path_exists": map[string]any{"path": "lib.go"}})},
			"focus_files":  []any{focus("lib.go", "present")},
			"compare_runs": []any{runRef(scanRunA, f.base, f.sha["a"])},
		}, sjVerdictPass, func() any { return &sjCrosscheckEvidence{} }},
	} {
		raw, _ := json.Marshal(tc.inputs)
		b, srv := newFakeSJBackend(t, upload.SpecialistJob{JobID: sjTestJobID, Kind: tc.kind, State: "queued",
			Subject: upload.SpecialistJobSubject{HeadSHA: f.base}, Inputs: raw})
		code, log := runSJSubcommand(srv.URL, f.repo)
		if code != exitOK {
			t.Fatalf("%s exit %d log %s", tc.kind, code, log)
		}
		_, _, results, nonce := b.snapshot()
		if len(results) != 1 || len(b.other) != 0 {
			t.Fatalf("%s results %d other %v", tc.kind, len(results), b.other)
		}
		validateCompletion(t, schema, results[0])
		c := decodeShippedCompletion(t, results[0])
		if c.Kind != tc.kind || c.Verdict != tc.verdict || c.ExecutionNonce != nonce {
			t.Fatalf("%s completion %+v (evidence %s)", tc.kind, c, c.Evidence)
		}
		if err := decodeJobInputs(c.Evidence, tc.target()); err != nil {
			t.Fatalf("%s evidence does not decode strictly: %v\n%s", tc.kind, err, c.Evidence)
		}
	}
	if status := cfGit(t, f.repo, "status", "--porcelain"); status != "" {
		t.Fatalf("source checkout modified: %s", status)
	}
}

// TestScanEvidence_SchemaValidEdgeShapes validates the evidence shapes the
// end-to-end run does not reach (conflict, skipped packages, truncation, an
// unresolved compare run) against the canonical completion schema.
func TestScanEvidence_SchemaValidEdgeShapes(t *testing.T) {
	t.Parallel()
	schema := compileCompletionSchema(t)
	for kind, o := range map[string]specialistJobOutcome{
		"fanin_build": {Verdict: sjVerdictFail, Evidence: sjFaninEvidence{
			Slices: []sjFaninSliceEvidence{
				{Name: "slice-1", HeadSHA: strings.Repeat("1", 40), Outcome: faninSliceConflict, ConflictPaths: []string{"shared.txt"}},
				{Name: "slice-2", HeadSHA: strings.Repeat("2", 40), Outcome: faninSliceError, ConflictPaths: []string{}},
			},
			Packages: []sjFaninPackageEvidence{{Package: ".", Outcome: faninPkgSkipped, Reason: faninReasonMergeIncomplete}, {Package: "./x/...", Outcome: faninPkgFail, Reason: cfReasonBuildFailed}},
		}},
		"overlap_scan": {Verdict: sjVerdictInconclusive, Reason: sjReasonUnresolvableSHA, Evidence: sjOverlapEvidence{
			Runs:      []sjOverlapRunEvidence{{RunID: scanRunA, ChangedFiles: []string{}}, {RunID: scanRunB, Resolved: true, ChangedFiles: []string{"a"}, Truncated: true}},
			Overlaps:  []sjOverlapPairEvidence{},
			WatchHits: []sjWatchHitEvidence{},
		}},
		"plan_crosscheck": {Verdict: sjVerdictInconclusive, Reason: sjReasonProbeError, Evidence: sjCrosscheckEvidence{
			Questions:   []sjProbeEvidence{{ID: "q1", Observed: probeObservedError, Expected: "pass", Reason: probeReasonInvalid}},
			FocusFiles:  []sjFocusEvidence{{Path: ".git/config", Expect: focusPresent, Observed: focusError, TouchedBy: []string{}}},
			CompareRuns: []sjCompareRunEvidence{{RunID: scanRunA, OverlapPaths: []string{}}},
		}},
	} {
		c, err := specialistJobCompletion(kind, "nonce", o, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(c)
		validateCompletion(t, schema, raw)
	}
}

// TestScanEvidence_MatchesSharedGolden pins this file's typed evidence
// against testdata/wire/specialist_job.json in both directions.
func TestScanEvidence_MatchesSharedGolden(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "specialist_job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Completions map[string]json.RawMessage `json:"completions"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	for kind, target := range map[string]func() any{
		"fanin_build":     func() any { return &sjFaninEvidence{} },
		"overlap_scan":    func() any { return &sjOverlapEvidence{} },
		"plan_crosscheck": func() any { return &sjCrosscheckEvidence{} },
	} {
		c := decodeShippedCompletion(t, golden.Completions[kind])
		v := target()
		if err := decodeJobInputs(c.Evidence, v); err != nil {
			t.Fatalf("%s golden evidence: %v", kind, err)
		}
		back, _ := json.Marshal(v)
		var got, want any
		_ = json.Unmarshal(back, &got)
		_ = json.Unmarshal(c.Evidence, &want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s evidence does not round-trip:\n got %s\nwant %s", kind, back, c.Evidence)
		}
	}
}
