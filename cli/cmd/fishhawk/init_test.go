package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/cli/internal/bridge"
	"github.com/kuhlman-labs/fishhawk/cli/internal/cmdinfo"
	"github.com/kuhlman-labs/fishhawk/cli/internal/scaffold"
	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// stubDoctorSeams makes the closing doctor preflight hermetic: every
// external command fails and every backend probe is unreachable. This
// mirrors the doctor-soft contract (an unreachable backend must not fail
// init) without touching the real Docker/git/gh/network environment.
func stubDoctorSeams(t *testing.T) {
	t.Helper()
	origRun := doctorRunOutput
	origHTTP := doctorHTTPDo
	doctorRunOutput = func(name string, _ ...string) (string, error) {
		return "", fmt.Errorf("stubbed: %s unavailable", name)
	}
	doctorHTTPDo = func(_ *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("stubbed: backend unreachable")
	}
	t.Cleanup(func() {
		doctorRunOutput = origRun
		doctorHTTPDo = origHTTP
	})
}

// newInitRepo returns a fresh temp dir carrying a `.git` marker so
// resolveRepoRoot treats it as the repo root.
func newInitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("create .git marker: %v", err)
	}
	return dir
}

// stubOrigin pins gitRemoteOriginURL so forge detection never reads a real
// remote.
func stubOrigin(t *testing.T, url string, err error) {
	t.Helper()
	orig := gitRemoteOriginURL
	gitRemoteOriginURL = func(string) (string, error) { return url, err }
	t.Cleanup(func() { gitRemoteOriginURL = orig })
}

func initSpecPath(dir string) string {
	return filepath.Join(dir, ".fishhawk", "workflows.yaml")
}

func TestInit_Golden(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)

	var stdout strings.Builder
	got := run([]string{"init", "--working-dir", dir, "--backend-url", "http://127.0.0.1:0"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK\n%s", got, stdout.String())
	}

	// SHIPPED spec is schema-valid — not merely that the path was touched.
	data, err := os.ReadFile(initSpecPath(dir))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	if err := spec.ValidateBytes(data); err != nil {
		t.Errorf("written spec fails ValidateBytes: %v", err)
	}

	// Bridge files: AGENTS.md carries the managed marker; CLAUDE.md imports it.
	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if !strings.Contains(string(agents), bridge.BeginMarker) {
		t.Errorf("AGENTS.md missing managed marker %q:\n%s", bridge.BeginMarker, agents)
	}
	claude, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if !strings.Contains(string(claude), bridge.ImportLine) {
		t.Errorf("CLAUDE.md missing %q import:\n%s", bridge.ImportLine, claude)
	}
}

// TestInit_ExistingSpecSkippedWithoutForce pins the relaxed re-run
// contract (#3718): an existing spec without --force is reported
// skipped-existing with the --force hint, left byte-identical, and init
// exits 0; --force regenerates it.
func TestInit_ExistingSpecSkippedWithoutForce(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "", fmt.Errorf("no origin"))
	dir := newInitRepo(t)
	specPath := initSpecPath(dir)
	if err := os.MkdirAll(filepath.Dir(specPath), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("version: 0.1\n# hand-written; must not be clobbered\n")
	if err := os.WriteFile(specPath, original, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK\n%s", got, stdout.String())
	}
	if !strings.Contains(stdout.String(), ".fishhawk/workflows.yaml skipped-existing (pass --force to regenerate") {
		t.Errorf("stdout missing the skipped-existing + --force hint line:\n%s", stdout.String())
	}
	after, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Errorf("re-run without --force modified the spec:\n%s", after)
	}

	// --force overwrites, and the result still validates.
	stdout.Reset()
	if got := run([]string{"init", "--working-dir", dir, "--force"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK\n%s", got, stdout.String())
	}
	if !strings.Contains(stdout.String(), ".fishhawk/workflows.yaml regenerated (--force)") {
		t.Errorf("stdout missing the regenerated line:\n%s", stdout.String())
	}
	forced, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(forced, original) {
		t.Error("--force did not overwrite the existing spec")
	}
	if err := spec.ValidateBytes(forced); err != nil {
		t.Errorf("--force spec fails ValidateBytes: %v", err)
	}
}

func TestInit_ChecklistNamesOutOfBandPrereqs(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)

	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	for _, want := range []string{
		"https://github.com/apps/fishhawk/installations/new", // (a) App install
		"fishhawkd token issue",                              // (b) token issue
		"fishhawk.yml",                                       // (c) execution-path trio
		"FISHHAWK_BACKEND_URL",
		"ANTHROPIC_API_KEY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("checklist missing %q:\n%s", want, out)
		}
	}
}

func TestInit_PresetHighMatchesGenerate(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)

	if got := run([]string{"init", "--working-dir", dir, "--preset", "high"}, io.Discard, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	data, err := os.ReadFile(initSpecPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	want, err := spec.Generate(spec.PresetHigh, spec.Deltas{})
	if err != nil {
		t.Fatalf("reference Generate: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("--preset high bytes differ from spec.Generate(PresetHigh, {})\ngot:\n%s\nwant:\n%s", data, want)
	}
}

func TestInit_DeltasApplied(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)

	if got := run([]string{
		"init", "--working-dir", dir,
		"--preset", "medium",
		"--budget-usd", "250",
		"--single-reviewer",
		"--human-gates", "plan",
	}, io.Discard, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	data, err := os.ReadFile(initSpecPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	limit := 250
	want, err := spec.Generate(spec.PresetMedium, spec.Deltas{
		BudgetLimitUSD: &limit,
		SingleReviewer: true,
		HumanGates:     []string{"plan"},
	})
	if err != nil {
		t.Fatalf("reference Generate: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("delta-applied bytes differ from the equivalent spec.Generate call\ngot:\n%s", data)
	}
	if err := spec.ValidateBytes(data); err != nil {
		t.Errorf("delta-applied spec fails ValidateBytes: %v", err)
	}
}

func TestInit_ShapeConfigOnly(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)

	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--shape", "config-only"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK\n%s", got, stdout.String())
	}
	data, err := os.ReadFile(initSpecPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	// The written spec is schema-valid.
	if err := spec.ValidateBytes(data); err != nil {
		t.Errorf("config-only spec fails ValidateBytes: %v", err)
	}
	// It is byte-identical to the equivalent Generate call.
	want, err := spec.Generate(spec.PresetMedium, spec.Deltas{Shape: spec.ShapeConfigOnly})
	if err != nil {
		t.Fatalf("reference Generate: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("--shape config-only bytes differ from spec.Generate(medium, {Shape: config-only})\ngot:\n%s", data)
	}
	// stdout announces the shape.
	if !strings.Contains(stdout.String(), "shape: config-only") {
		t.Errorf("stdout missing 'shape: config-only':\n%s", stdout.String())
	}
	// No live verify: line survives in the written config-only spec.
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "verify:") {
			t.Errorf("config-only spec ships a live verify key: %s", line)
		}
	}
}

func TestInit_DefaultShapeIsApp(t *testing.T) {
	stubDoctorSeams(t)
	dir := newInitRepo(t)

	if got := run([]string{"init", "--working-dir", dir}, io.Discard, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	data, err := os.ReadFile(initSpecPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	// Omitting --shape yields bytes identical to the app-shape Generate.
	want, err := spec.Generate(spec.PresetMedium, spec.Deltas{})
	if err != nil {
		t.Fatalf("reference Generate: %v", err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("default-shape bytes differ from spec.Generate(medium, {})\ngot:\n%s", data)
	}
}

func TestInit_UnknownShape(t *testing.T) {
	dir := newInitRepo(t)

	var stderr strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--shape", "bogus"}, io.Discard, &stderr); got != exitUsage {
		t.Fatalf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "unknown --shape") {
		t.Errorf("stderr missing 'unknown --shape': %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "app, config-only") {
		t.Errorf("stderr missing the valid-shapes hint: %s", stderr.String())
	}
	// The bad shape must short-circuit before any spec is written.
	if _, err := os.Stat(initSpecPath(dir)); !os.IsNotExist(err) {
		t.Errorf("spec written despite unknown shape (stat err = %v)", err)
	}
}

func TestInit_UnknownPreset(t *testing.T) {
	dir := newInitRepo(t)

	var stderr strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--preset", "bogus"}, io.Discard, &stderr); got != exitUsage {
		t.Fatalf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "unknown --preset") {
		t.Errorf("stderr missing 'unknown --preset': %s", stderr.String())
	}
	// The bad preset must short-circuit before any spec is written.
	if _, err := os.Stat(initSpecPath(dir)); !os.IsNotExist(err) {
		t.Errorf("spec written despite unknown preset (stat err = %v)", err)
	}
}

func TestInit_DoctorSoft_UnreachableBackendStillExitsOK(t *testing.T) {
	stubDoctorSeams(t) // backend unreachable + every doctor rung degraded
	dir := newInitRepo(t)

	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--backend-url", "http://127.0.0.1:0"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK (doctor failure must not fail init)", got)
	}
	// The scaffold itself still landed.
	if _, err := os.Stat(initSpecPath(dir)); err != nil {
		t.Errorf("spec not written on the doctor-soft path: %v", err)
	}
	// And init flagged that doctor reported issues rather than swallowing them.
	if !strings.Contains(stdout.String(), "doctor reported issues") {
		t.Errorf("doctor-soft note missing from stdout:\n%s", stdout.String())
	}
}

// newGitInitRepo returns a fresh temp dir initialised by a real `git init`,
// so resolveRepoRoot and the doctor rungs see a genuine repository.
func newGitInitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return dir
}

func readScaffolded(t *testing.T, dir, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return data
}

var fourFiles = []string{".fishhawk/workflows.yaml", scaffold.CharterPath, scaffold.OperatorPath, scaffold.WorkManagementPath}

// TestInit_ScaffoldsFourFilesInTempRepo is the end-to-end crossing:
// template -> assembler -> writer -> init -> doctor, all over a scratch git
// repository.
func TestInit_ScaffoldsFourFilesInTempRepo(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "git@github.com:acme/widgets.git", nil)
	dir := newGitInitRepo(t)

	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--backend-url", "http://127.0.0.1:0"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d, want exitOK\n%s", got, stdout.String())
	}
	out := stdout.String()
	for _, p := range fourFiles {
		if !strings.Contains(out, p+" created") {
			t.Errorf("stdout does not report %s created:\n%s", p, out)
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			t.Errorf("%s not written under the temp repo: %v", p, err)
		}
	}
	if err := spec.ValidateConventionsDocument(readScaffolded(t, dir, scaffold.WorkManagementPath)); err != nil {
		t.Errorf("written work-management config fails the schema: %v", err)
	}

	var doc strings.Builder
	if got := runDoctor([]string{"--spec-only", "--working-dir", dir}, &doc, io.Discard); got != exitOK {
		t.Fatalf("doctor --spec-only = %d over the scaffolded repo\n%s", got, doc.String())
	}
	for _, label := range []string{"workflow spec present", "charter document"} {
		if !rungStatusIs(doc.String(), label, "ok") {
			t.Errorf("doctor rung %q not ok:\n%s", label, doc.String())
		}
	}
}

// rungStatusIs reports whether the doctor line for label ends in status.
func rungStatusIs(out, label, status string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, label) {
			return strings.HasSuffix(strings.TrimSpace(line), " "+status)
		}
	}
	return false
}

// TestInit_ForceRegeneratesSpecButNotCharter (C2): --force reaches the spec
// and nothing else.
func TestInit_ForceRegeneratesSpecButNotCharter(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "", fmt.Errorf("no origin"))
	dir := newInitRepo(t)
	sentinel := []byte("# Our charter\n\nsentinel-3718-human-authored\n")
	oldSpec := []byte("version: 0.1\n# existing spec\n")
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{scaffold.CharterPath: sentinel, ".fishhawk/workflows.yaml": oldSpec} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(path)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--force"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d\n%s", got, stdout.String())
	}
	if bytes.Equal(readScaffolded(t, dir, ".fishhawk/workflows.yaml"), oldSpec) {
		t.Error("--force did not regenerate the spec")
	}
	if got := readScaffolded(t, dir, scaffold.CharterPath); !bytes.Equal(got, sentinel) {
		t.Errorf("--force rewrote the charter:\n%s", got)
	}
	if !strings.Contains(stdout.String(), scaffold.CharterPath+" skipped-existing") {
		t.Errorf("charter not reported skipped-existing:\n%s", stdout.String())
	}
}

type wmView struct {
	Provider string         `yaml:"provider"`
	Project  map[string]any `yaml:"project"`
	GitLab   map[string]any `yaml:"gitlab"`
}

func readWM(t *testing.T, dir string) (wmView, string) {
	t.Helper()
	raw := readScaffolded(t, dir, scaffold.WorkManagementPath)
	var v wmView
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode work-management: %v", err)
	}
	return v, string(raw)
}

// TestInit_GitLabOriginEmitsGitLabProvider (m5).
func TestInit_GitLabOriginEmitsGitLabProvider(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "git@gitlab.com:group/sub/proj.git", nil)
	dir := newInitRepo(t)
	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d\n%s", got, stdout.String())
	}
	v, raw := readWM(t, dir)
	if v.Provider != "gitlab" || v.GitLab == nil || v.Project != nil {
		t.Errorf("want provider gitlab, a gitlab block and no project block; got %+v\n%s", v, raw)
	}
	out := stdout.String()
	if !strings.Contains(out, "forge: gitlab (project group/sub/proj, from git origin)") {
		t.Errorf("stdout missing the gitlab forge line:\n%s", out)
	}
	if strings.Contains(out, appInstallURL) {
		t.Errorf("gitlab checklist names the GitHub App install:\n%s", out)
	}
}

// TestInit_GitLabProjectFlagSelectsGitLab: --gitlab-project wins over a
// github origin, and the github-only flags are reported ignored.
func TestInit_GitLabProjectFlagSelectsGitLab(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "https://github.com/acme/widgets.git", nil)
	dir := newInitRepo(t)
	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--gitlab-project", "grp/proj", "--project-number", "3"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d\n%s", got, stdout.String())
	}
	v, raw := readWM(t, dir)
	if v.Provider != "gitlab" || v.GitLab["project"] != "grp/proj" || v.Project != nil {
		t.Errorf("want gitlab project grp/proj; got %+v\n%s", v, raw)
	}
	if !strings.Contains(stdout.String(), "--project-owner/--project-number ignored") {
		t.Errorf("ignored github flags not reported:\n%s", stdout.String())
	}
}

// TestInit_UnknownOriginDegradesAndReports (m6): both an absent and an
// unrecognised origin degrade to github_projects with an owner marker.
func TestInit_UnknownOriginDegradesAndReports(t *testing.T) {
	for name, c := range map[string]struct {
		url  string
		err  error
		want string
	}{
		"absent":       {"", fmt.Errorf("no origin"), "forge: no git origin remote; defaulting to github_projects"},
		"unrecognised": {"https://example.org/x/y.git", nil, `forge: origin "https://example.org/x/y.git" is not a github.com or GitLab remote; defaulting to github_projects`},
	} {
		t.Run(name, func(t *testing.T) {
			stubDoctorSeams(t)
			stubOrigin(t, c.url, c.err)
			dir := newInitRepo(t)
			var stdout strings.Builder
			if got := run([]string{"init", "--working-dir", dir}, &stdout, io.Discard); got != exitOK {
				t.Fatalf("status = %d\n%s", got, stdout.String())
			}
			if !strings.Contains(stdout.String(), c.want) {
				t.Errorf("stdout missing degrade line %q:\n%s", c.want, stdout.String())
			}
			v, raw := readWM(t, dir)
			if v.Provider != "github_projects" {
				t.Errorf("provider = %q, want github_projects", v.Provider)
			}
			if !strings.Contains(raw, "#   owner: FILL ME IN") {
				t.Errorf("owner not rendered as a fill-me-in marker:\n%s", raw)
			}
			if !strings.Contains(stdout.String(), "project.owner and project.number") {
				t.Errorf("stdout does not name both missing fields:\n%s", stdout.String())
			}
		})
	}
}

// TestInit_MissingProjectNumberReportsIncomplete (m7).
func TestInit_MissingProjectNumberReportsIncomplete(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "git@github.com:acme/widgets.git", nil)
	dir := newInitRepo(t)
	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d\n%s", got, stdout.String())
	}
	v, raw := readWM(t, dir)
	if v.Project != nil {
		t.Errorf("project block is live without --project-number: %v", v.Project)
	}
	if !strings.Contains(raw, "# REQUIRED — FILL ME IN (project.number)") || !strings.Contains(raw, "#   owner: acme") {
		t.Errorf("commented project block lacks the required-fill marker / detected owner:\n%s", raw)
	}
	out := stdout.String()
	if !strings.Contains(out, ".fishhawk/work-management.yaml is incomplete: fill project.number") {
		t.Errorf("stdout does not name project.number:\n%s", out)
	}
	if !strings.Contains(out, "and fill project.number in .fishhawk/work-management.yaml") {
		t.Errorf("checklist does not name project.number:\n%s", out)
	}
}

// TestInit_ProjectNumberRendersCompleteConnection (m8): with the number
// supplied (and the owner overridden) the project block is live and
// complete, and nothing is reported incomplete.
func TestInit_ProjectNumberRendersCompleteConnection(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "git@github.com:acme/widgets.git", nil)
	dir := newInitRepo(t)
	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--project-number", "12", "--project-owner", "acme-org"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("status = %d\n%s", got, stdout.String())
	}
	v, raw := readWM(t, dir)
	if v.Project["owner"] != "acme-org" || v.Project["number"] != 12 || v.Project["owner_type"] != "user" {
		t.Errorf("project block = %v\n%s", v.Project, raw)
	}
	if strings.Contains(stdout.String(), "is incomplete") {
		t.Errorf("complete connection reported incomplete:\n%s", stdout.String())
	}
	if err := spec.ValidateConventionsDocument([]byte(raw)); err != nil {
		t.Errorf("complete config fails the schema: %v", err)
	}
}

func TestInit_InvalidProjectNumberIsUsageError(t *testing.T) {
	dir := newInitRepo(t)
	var stderr strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--project-number", "0"}, io.Discard, &stderr); got != exitUsage {
		t.Fatalf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--project-number must be >= 1") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".fishhawk")); !os.IsNotExist(err) {
		t.Errorf(".fishhawk written despite the usage error (stat err = %v)", err)
	}
}

// snapshotTree maps every file under dir (except .git) to its bytes.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		snap[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestInit_SecondRunSkipsEverythingAndExitsZero (m9).
func TestInit_SecondRunSkipsEverythingAndExitsZero(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "git@github.com:acme/widgets.git", nil)
	dir := newInitRepo(t)
	if got := run([]string{"init", "--working-dir", dir}, io.Discard, io.Discard); got != exitOK {
		t.Fatalf("first run status = %d", got)
	}
	before := snapshotTree(t, dir)
	if len(before) < 6 {
		t.Fatalf("first run wrote %d files, want the four .fishhawk files plus AGENTS.md and CLAUDE.md: %v", len(before), before)
	}
	var stdout strings.Builder
	if got := run([]string{"init", "--working-dir", dir, "--preset", "high", "--project-number", "9"}, &stdout, io.Discard); got != exitOK {
		t.Fatalf("second run status = %d\n%s", got, stdout.String())
	}
	for _, p := range fourFiles {
		if !strings.Contains(stdout.String(), p+" skipped-existing") {
			t.Errorf("second run does not report %s skipped-existing:\n%s", p, stdout.String())
		}
	}
	if after := snapshotTree(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("second run changed bytes:\nbefore %v\nafter  %v", before, after)
	}
}

// TestInit_ChecklistNamesLocalPathFirst (m10): the local runner path comes
// before the GitHub Actions alternative, and every `fishhawk` verb the
// checklist names exists in the cmdinfo inventory.
func TestInit_ChecklistNamesLocalPathFirst(t *testing.T) {
	var buf strings.Builder
	printOnboardingChecklist(&buf, "acme/widgets", scaffold.ProviderGitHubProjects, []string{"project.number"})
	out := buf.String()
	local := strings.Index(out, "--runner-kind local")
	actions := strings.Index(out, "Alternative — the GitHub Actions execution path")
	if local < 0 || actions < 0 || local > actions {
		t.Errorf("local path (%d) must precede the GitHub Actions alternative (%d):\n%s", local, actions, out)
	}
	known := map[string]bool{}
	for _, c := range cmdinfo.Commands() {
		known[c.Key] = true
	}
	verbs := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f != "fishhawk" || i+1 >= len(fields) {
				continue
			}
			verbs++
			one := fields[i+1]
			two := ""
			if i+2 < len(fields) {
				two = one + " " + fields[i+2]
			}
			if !known[one] && !known[two] {
				t.Errorf("checklist names a verb the CLI does not have: %q", line)
			}
		}
	}
	if verbs < 2 {
		t.Errorf("checklist names %d fishhawk verbs, want the run start + runner start pair:\n%s", verbs, out)
	}
	if !strings.Contains(out, "fishhawkd token issue") || !strings.Contains(out, appInstallURL) {
		t.Errorf("checklist lost the token / App-install steps:\n%s", out)
	}
}

// TestInit_ScaffoldRenderFailureFailsInitWithNothingWritten covers init's
// half of C7: the render seam's error fails init before any write.
func TestInit_ScaffoldRenderFailureFailsInitWithNothingWritten(t *testing.T) {
	stubDoctorSeams(t)
	stubOrigin(t, "", fmt.Errorf("no origin"))
	orig := initScaffoldFiles
	initScaffoldFiles = func(scaffold.Options) (map[string][]byte, error) {
		return nil, fmt.Errorf("scaffold: rendered .fishhawk/work-management.yaml is invalid: spec_version missing")
	}
	t.Cleanup(func() { initScaffoldFiles = orig })
	dir := newInitRepo(t)
	var stderr strings.Builder
	if got := run([]string{"init", "--working-dir", dir}, io.Discard, &stderr); got != exitFailure {
		t.Fatalf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "spec_version") {
		t.Errorf("stderr does not surface the render error: %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".fishhawk")); !os.IsNotExist(err) {
		t.Errorf(".fishhawk written despite the render failure (stat err = %v)", err)
	}
}

// TestInit_ScaffoldWriteErrorFailsInit: a scaffold write failure (a
// read-only .fishhawk dir holding only the spec) fails init.
func TestInit_ScaffoldWriteErrorFailsInit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	stubDoctorSeams(t)
	stubOrigin(t, "", fmt.Errorf("no origin"))
	dir := newInitRepo(t)
	hidden := filepath.Join(dir, ".fishhawk")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "workflows.yaml"), []byte("version: 0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hidden, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o755) })
	var stderr strings.Builder
	if got := run([]string{"init", "--working-dir", dir}, io.Discard, &stderr); got != exitFailure {
		t.Fatalf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), scaffold.CharterPath) {
		t.Errorf("stderr does not name the failing path: %q", stderr.String())
	}
}

func TestParseGitLabRemote(t *testing.T) {
	for raw, want := range map[string]string{
		"https://gitlab.com/g/p.git":              "g/p",
		"https://gitlab.com/g/sub/p":              "g/sub/p",
		"git@gitlab.com:g/p.git":                  "g/p",
		"ssh://git@gitlab.corp.example:2222/g/p/": "g/p",
		"https://user@gitlab.example.com/a/b.git": "a/b",
		"https://github.com/acme/widgets.git":     "",
		"git@github.com:acme/widgets.git":         "",
		"https://gitlab.com/solo":                 "",
		"https://gitlab.com":                      "",
		"git@gitlab.com":                          "",
		"/local/path/to/repo":                     "",
	} {
		got, ok := parseGitLabRemote(raw)
		if ok != (want != "") || got != want {
			t.Errorf("parseGitLabRemote(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}
