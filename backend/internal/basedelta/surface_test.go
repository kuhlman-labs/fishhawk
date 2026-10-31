package basedelta

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// thisRepoFixture is this repository's committed EXAMPLE declaration
// (ADR-124 D6), exactly the block the operator pastes into the live spec.
const thisRepoFixture = "testdata/fishhawk-verdict-transfer.yaml"

// minimalV2Spec is a minimal valid workflow-v2 document the example is
// spliced into to resolve it on its own.
const minimalV2Spec = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

func readFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, thisRepoFixture))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// thisRepoSurface is the ADR-124 D4 harness switch. It resolves the LIVE
// .fishhawk/workflows.yaml with the production resolver. When the live file
// declares a verdict_transfer block, that live Surface is returned, after
// asserting its Digest equals the committed example's, so the example cannot
// drift from it. When it declares none (true until the operator lands the
// block after deploy), it returns the committed example resolved INSIDE the
// live file, proving the operator's paste validates. The fallback is safe:
// with no live block the product refuses every transfer as
// surface_undeclared, so the example never stands in for a live declaration
// that loosens coverage.
func thisRepoSurface(t *testing.T) *Surface {
	t.Helper()
	live, err := os.ReadFile(filepath.Join(repoRoot(), WorkflowSpecPath))
	if err != nil {
		t.Fatal(err)
	}
	fixture := readFixture(t)
	s, err := ResolveSurface(live)
	if err != nil {
		t.Fatalf("ResolveSurface(live %s): %v", WorkflowSpecPath, err)
	}
	if s != nil {
		example, err := ResolveSurface(append([]byte(minimalV2Spec), fixture...))
		if err != nil || example == nil {
			t.Fatalf("ResolveSurface(example) = %v, %v", example, err)
		}
		if s.Digest() != example.Digest() {
			t.Fatalf("the live %s verdict_transfer block (%s) differs from %s (%s): update the example with the live declaration",
				WorkflowSpecPath, s.Digest(), thisRepoFixture, example.Digest())
		}
		return s
	}
	spliced := append(append(append([]byte{}, live...), '\n'), fixture...)
	s, err = ResolveSurface(spliced)
	if err != nil || s == nil {
		t.Fatalf("ResolveSurface(live %s + %s) = %v, %v: the example does not validate inside the live spec", WorkflowSpecPath, thisRepoFixture, s, err)
	}
	t.Logf("%s declares no verdict_transfer block yet; read the committed example %s", WorkflowSpecPath, thisRepoFixture)
	return s
}

func TestThisRepoDeclarationValid(t *testing.T) {
	t.Parallel()
	s := thisRepoSurface(t)
	if bad, ok := s.firstInvalid(); ok {
		t.Errorf("invalid declared entry %q", bad)
	}
	if s.incomplete() || s.EnforceWithheld() {
		t.Errorf("declaration incomplete=%v enforceWithheld=%v, want neither", s.incomplete(), s.EnforceWithheld())
	}
	for name, list := range map[string][]string{
		"global_surface": s.Global, "shared_artifact_dirs": s.SharedDirs, "doc_check_inputs": s.DocCheck,
		"inert_paths": s.Inert, "opaque_paths": s.Opaque,
	} {
		seen := map[string]bool{}
		for _, e := range list {
			if seen[e] {
				t.Errorf("%s lists %q twice", name, e)
			}
			seen[e] = true
		}
	}
}

func TestMatchGlobal_RootAnchored(t *testing.T) {
	t.Parallel()
	s := thisRepoSurface(t)
	for _, p := range []string{
		"testdata/wire/x.json", "backend/go.mod", "go.mod", "go.work", "docs/ARCHITECTURE.md",
		"scripts/test", "backend/internal/postgres/migrations/0102_x.up.sql",
		"cli/cmd/fishhawk-docgen/main.go", "deploy/helm/fishhawk/values.yaml",
	} {
		if _, ok := s.MatchGlobal(p); !ok {
			t.Errorf("%s should be global", p)
		}
	}
	for _, p := range []string{
		"backend/internal/x/testdata/y.json", "docs/README.md", "backend/internal/server/server.go",
		"frontend/src/a.ts", "README.md", "x/scripts/y", "backend/internal/spec/testdata/other.json",
	} {
		if pat, ok := s.MatchGlobal(p); ok {
			t.Errorf("%s should not be global (matched %q)", p, pat)
		}
	}
	if !s.inert("README.md") {
		t.Error("README.md should be declared inert")
	}
	if d, ok := s.sharedDir("frontend/src/a.ts", "frontend/b.ts"); !ok || d != "frontend/" {
		t.Errorf("sharedDir(frontend/src/a.ts, frontend/b.ts) = %q, %v", d, ok)
	}
	if _, ok := s.sharedDir("backend/x.go", "backend/y.go"); ok {
		t.Error("backend/ is no shared dir")
	}
}

// adr096D3Floor is ADR-096 D3 as #4292 enumerates it, hard-coded here (this
// repository's rule-5 conformance tooling) and NOT read from the
// declaration, so dropping a floor pattern from the declaration fails. Each
// pattern carries paths it must keep forcing a pass.
var adr096D3Floor = []struct {
	pattern string
	samples []string
}{
	{"go.work", []string{"go.work"}},
	{"**/go.mod", []string{"go.mod", "backend/go.mod", "a/b/go.mod"}},
	{"**/go.sum", []string{"go.sum", "backend/go.sum"}},
	{"scripts/**", []string{"scripts/test", "scripts/lib/x.sh"}},
	{".golangci.yml", []string{".golangci.yml"}},
	{"deploy/gate-image/**", []string{"deploy/gate-image/Dockerfile"}},
	{".fishhawk/**", []string{".fishhawk/workflows.yaml", ".fishhawk/x/y.yaml"}},
	{"docs/spec/**", []string{"docs/spec/workflow-v2.schema.json"}},
	{"docs/api/**", []string{"docs/api/v0.openapi.yaml"}},
	{"site/**", []string{"site/astro.config.mjs", "site/src/content/docs/reference/cli.md"}},
	{"docs/adr/**", []string{"docs/adr/index.json"}},
	{"docs/ARCHITECTURE.md", []string{"docs/ARCHITECTURE.md"}},
	{"backend/internal/postgres/migrations/**", []string{"backend/internal/postgres/migrations/0001_init.up.sql"}},
	{"backend/internal/audit/categories.go", []string{"backend/internal/audit/categories.go"}},
	{"testdata/**", []string{"testdata/wire/restart_blockers.json", "testdata/policy/agent-instruction-paths.json"}},
}

// docClassFloor are the ADR-098 D-class doc patterns, the only floor
// patterns that may move from G to the doc-check class.
var docClassFloor = map[string]bool{"docs/api/**": true, "site/**": true, "docs/adr/**": true, "docs/ARCHITECTURE.md": true}

// checkFloor is the pure check behind TestDeclarationKeepsADR096D3Floor.
func checkFloor(s *Surface) []error {
	var errs []error
	v := s.view()
	inG, inD := toSet(v.Global), toSet(v.DocCheck)
	for _, f := range adr096D3Floor {
		inDocClass := docClassFloor[f.pattern] && inD[f.pattern]
		if !inG[f.pattern] && !inDocClass {
			errs = append(errs, fmt.Errorf("ADR-096 D3 floor pattern %q is missing from the declaration's global_surface; the floor shrinks only through an ADR amendment", f.pattern))
		}
		for _, sample := range f.samples {
			if !s.matchVerdictWide(sample) && !s.matchDocCheck(sample) {
				errs = append(errs, fmt.Errorf("%s (ADR-096 D3 floor %q) does not force a pass", sample, f.pattern))
			}
		}
	}
	return errs
}

func TestDeclarationKeepsADR096D3Floor(t *testing.T) {
	t.Parallel()
	for _, e := range checkFloor(thisRepoSurface(t)) {
		t.Error(e)
	}
}

func TestCheckFloor_Synthetic(t *testing.T) {
	t.Parallel()
	full := make([]string, 0, len(adr096D3Floor))
	for _, f := range adr096D3Floor {
		full = append(full, f.pattern)
	}
	without := func(drop string) []string {
		var out []string
		for _, p := range full {
			if p != drop {
				out = append(out, p)
			}
		}
		return out
	}
	if errs := checkFloor(&Surface{Global: full}); len(errs) != 0 {
		t.Fatalf("full floor: %v", errs)
	}
	if errs := checkFloor(&Surface{Global: without("scripts/**")}); len(errs) == 0 {
		t.Error("dropping scripts/** passed")
	}
	if errs := checkFloor(&Surface{Global: without("docs/adr/**"), DocCheck: []string{"docs/adr/**"}}); len(errs) != 0 {
		t.Errorf("moving docs/adr/** to doc-check: %v", errs)
	}
	if errs := checkFloor(&Surface{Global: without("docs/adr/**")}); len(errs) == 0 {
		t.Error("dropping docs/adr/** passed")
	}
	if errs := checkFloor(&Surface{Global: without("scripts/**"), DocCheck: []string{"scripts/**"}}); len(errs) == 0 {
		t.Error("moving scripts/** (not a D-class pattern) to doc-check passed")
	}
}

// Every list entry of the example carries its rationale as a trailing
// comment (replaces the README set table and its sync test).
func TestThisRepoDeclarationEntriesCarryRationale(t *testing.T) {
	t.Parallel()
	sc := bufio.NewScanner(bytes.NewReader(readFixture(t)))
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, `- "`) {
			continue
		}
		n++
		_, comment, ok := strings.Cut(line, `"  #`)
		if !ok || strings.TrimSpace(comment) == "" {
			t.Errorf("entry %q carries no rationale comment", line)
		}
	}
	if n < 80 {
		t.Errorf("only %d entries scanned: parser collapse?", n)
	}
}

func TestSurface_NilSafe(t *testing.T) {
	t.Parallel()
	var s *Surface
	if s.matchVerdictWide("go.mod") || s.matchDocCheck("x") || s.matchOpaque("x") || s.inert("x") {
		t.Error("a nil surface matched a declared class")
	}
	if !s.matchVerdictWide(WorkflowSpecPath) {
		t.Error("the workflow spec must force a pass even under a nil surface")
	}
	if _, ok := s.MatchGlobal("go.mod"); ok {
		t.Error("nil MatchGlobal matched")
	}
	if _, ok := s.sharedDir("a/x", "a/y"); ok {
		t.Error("nil sharedDir matched")
	}
	if _, ok := s.firstInvalid(); ok {
		t.Error("nil firstInvalid reported an entry")
	}
	if !s.incomplete() || s.EnforceWithheld() || s.Digest() != "" || s.Unmatched([]string{"a"}) != nil {
		t.Error("nil surface: want incomplete, not withheld, empty digest, nothing unmatched")
	}
}

func TestSurface_FirstInvalid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		s    Surface
		bad  string
	}{
		{"empty", Surface{Global: []string{"go.mod", ""}}, ""},
		{"leading_slash", Surface{Global: []string{"/etc/**"}}, "/etc/**"},
		{"leading_dot_slash", Surface{Inert: []string{"./docs/**"}}, "./docs/**"},
		{"dotdot_segment", Surface{Opaque: []string{"a/../b"}}, "a/../b"},
		{"backslash", Surface{DocCheck: []string{`docs\x`}}, `docs\x`},
		{"control_char", Surface{Global: []string{"a\nb"}}, "a\nb"},
		{"del_char", Surface{Global: []string{"a\x7fb"}}, "a\x7fb"},
		{"bad_glob", Surface{Global: []string{"scripts/["}}, "scripts/["},
		{"invalid_utf8", Surface{Global: []string{"a\xffb"}}, "a\xffb"},
		{"dir_no_slash", Surface{SharedDirs: []string{"docs"}}, "docs"},
		{"dir_root", Surface{SharedDirs: []string{"/"}}, "/"},
		{"dir_non_canonical", Surface{SharedDirs: []string{"a//b/"}}, "a//b/"},
		{"dir_dot", Surface{SharedDirs: []string{"a/./"}}, "a/./"},
		{"dir_dotdot", Surface{SharedDirs: []string{"a/../"}}, "a/../"},
	} {
		got, ok := tc.s.firstInvalid()
		if !ok || got != tc.bad {
			t.Errorf("%s: firstInvalid = %q, %v; want %q", tc.name, got, ok, tc.bad)
		}
	}
	valid := &Surface{Global: []string{"**/go.mod", "go.work"}, SharedDirs: []string{"docs/", "a/b/"},
		Inert: []string{"**"}, Opaque: []string{"vendor/**"}, DocCheck: []string{"docs/api/**"}}
	if bad, ok := valid.firstInvalid(); ok {
		t.Errorf("valid surface reported %q", bad)
	}
}

func TestSurface_Digest(t *testing.T) {
	t.Parallel()
	base := Surface{Global: []string{"a/**", "b/**"}, SharedDirs: []string{"x/", "y/"}, Inert: []string{"c"}, CompletenessCheck: "check"}
	d := base.Digest()
	if !strings.HasPrefix(d, "sha256:") || len(d) != len("sha256:")+64 {
		t.Fatalf("Digest = %q", d)
	}
	perm := Surface{Global: []string{"b/**", "a/**"}, SharedDirs: []string{"y/", "x/"}, Inert: []string{"c"}, CompletenessCheck: "check"}
	if perm.Digest() != d {
		t.Error("a permuted class changed the digest")
	}
	var nilS *Surface
	if nilS.Digest() == (&Surface{}).Digest() {
		t.Error("nil and declared-empty share a digest")
	}
	for name, mut := range map[string]Surface{
		"add":        {Global: []string{"a/**", "b/**", "z"}, SharedDirs: base.SharedDirs, Inert: base.Inert, CompletenessCheck: "check"},
		"remove":     {Global: []string{"a/**"}, SharedDirs: base.SharedDirs, Inert: base.Inert, CompletenessCheck: "check"},
		"edit":       {Global: []string{"a/**", "b/*"}, SharedDirs: base.SharedDirs, Inert: base.Inert, CompletenessCheck: "check"},
		"move_class": {Global: base.Global, SharedDirs: base.SharedDirs, Opaque: []string{"c"}, CompletenessCheck: "check"},
		"check":      {Global: base.Global, SharedDirs: base.SharedDirs, Inert: base.Inert, CompletenessCheck: "other"},
	} {
		if mut.Digest() == d {
			t.Errorf("%s: digest unchanged", name)
		}
	}
}

func TestSurface_Unmatched(t *testing.T) {
	t.Parallel()
	s := &Surface{Global: []string{"go.mod", "proto/**", "proto/**"}, SharedDirs: []string{"web/", "docs/"},
		DocCheck: []string{"site/**"}, Inert: []string{"**/README.md"}, Opaque: []string{"vendor/**", "scripts/["}}
	got := s.Unmatched([]string{"go.mod", "docs/a.md", "README.md"})
	want := []string{"proto/**", "site/**", "vendor/**", "web/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Unmatched = %v, want %v", got, want)
	}
	if got := (&Surface{Global: []string{"go.mod"}}).Unmatched([]string{"go.mod"}); got != nil {
		t.Errorf("fully matched = %v, want nil", got)
	}
}

func TestSurface_EnforceWithheld(t *testing.T) {
	t.Parallel()
	var nilS *Surface
	for _, tc := range []struct {
		s    *Surface
		want bool
	}{
		{nilS, false},
		{&Surface{Global: []string{"go.mod"}}, true},
		{&Surface{Global: []string{"go.mod"}, CompletenessCheck: " \t\n"}, true},
		{&Surface{Global: []string{"go.mod"}, CompletenessCheck: "go test ./x"}, false},
	} {
		if got := tc.s.EnforceWithheld(); got != tc.want {
			t.Errorf("EnforceWithheld(%+v) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestListTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"b/x.go": "package b\n", "a.txt": "a\n", "c/d/e.md": "e\n"})
	if err := os.Symlink("../a.txt", filepath.Join(root, "b", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "c"), filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	got, err := ListTree(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a.txt", "b/link", "b/x.go", "c/d/e.md", "dirlink"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListTree = %v, want %v (symlinks listed, never followed)", got, want)
	}
	if _, err := ListTree(filepath.Join(root, "absent")); err == nil {
		t.Error("ListTree of a missing dir succeeded")
	}
}

// shellMetachars is a completeness_check full of shell syntax: a declarative
// reference fishhawkd never executes or interpolates, so it must resolve to
// exactly these bytes (approval condition C5).
const shellMetachars = "go test ./x -run 'A|B' && echo $HOME ${PATH} $(id) `whoami`; rm -rf / > /dev/null"

func TestResolveSurface(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string][]byte{
		"nil":       nil,
		"v0_5":      []byte("version: \"0.5\"\nworkflows:\n  feature_change:\n    stages:\n      - id: plan\n        type: plan\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: plan\n            schema: standard_v1\n"),
		"v2_absent": []byte(minimalV2Spec),
	} {
		if s, err := ResolveSurface(raw); s != nil || err != nil {
			t.Errorf("%s: ResolveSurface = %+v, %v; want nil, nil", name, s, err)
		}
	}
	if s, err := ResolveSurface([]byte("version: [")); s != nil || err == nil {
		t.Errorf("unparseable: ResolveSurface = %+v, %v; want nil, error", s, err)
	}
	empty, err := ResolveSurface([]byte(minimalV2Spec + "verdict_transfer: {}\n"))
	if err != nil || empty == nil || !reflect.DeepEqual(*empty, Surface{}) {
		t.Errorf("empty block: ResolveSurface = %+v, %v; want non-nil empty", empty, err)
	}
	full, err := ResolveSurface([]byte(minimalV2Spec + `verdict_transfer:
  global_surface: ["go.work", "**/go.mod"]
  shared_artifact_dirs: ["web/"]
  doc_check_inputs: ["docs/api/**"]
  inert_paths: ["**/README.md"]
  opaque_paths: ["vendor/**"]
  completeness_check: "` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(shellMetachars) + `"
`))
	want := Surface{Global: []string{"go.work", "**/go.mod"}, SharedDirs: []string{"web/"}, DocCheck: []string{"docs/api/**"},
		Inert: []string{"**/README.md"}, Opaque: []string{"vendor/**"}, CompletenessCheck: shellMetachars}
	if err != nil || full == nil || !reflect.DeepEqual(*full, want) {
		t.Errorf("full block: ResolveSurface = %+v, %v; want %+v", full, err, want)
	}
	if full != nil && full.EnforceWithheld() {
		t.Error("a shell-metacharacter completeness_check is plain text that names a check; enforce must not be withheld")
	}
}

func TestWorkflowSpecPathMatchesProduct(t *testing.T) {
	t.Parallel()
	if WorkflowSpecPath != githubclient.WorkflowSpecPath {
		t.Fatalf("WorkflowSpecPath = %q, githubclient.WorkflowSpecPath = %q", WorkflowSpecPath, githubclient.WorkflowSpecPath)
	}
}
