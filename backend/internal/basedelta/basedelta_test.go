package basedelta

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

const modPrefix = "github.com/kuhlman-labs/fishhawk/"

// pkg builds a fake package rooted at a repo-relative dir; imports name dirs.
func pkg(dir, moduleDir string, importDirs ...string) Package {
	imps := make([]string, 0, len(importDirs))
	for _, d := range importDirs {
		imps = append(imps, modPrefix+d)
	}
	return Package{ImportPath: modPrefix + dir, Dir: dir, ModuleDir: moduleDir, Imports: imps}
}

// fakeLoader serves a fixed graph (or error) per workspace dir; a workspace
// in unsupported has no adapter (Supports refuses it) whatever Load serves.
type fakeLoader struct {
	graphs      map[string]*Graph
	errs        map[string]error
	unsupported map[string]bool
	panics      bool
}

func (f *fakeLoader) Supports(ws string) error {
	if f.unsupported[ws] {
		return fmt.Errorf("%w: fake", ErrUnsupportedLanguage)
	}
	return nil
}

func (f *fakeLoader) Load(_ context.Context, ws string) (*Graph, error) {
	if f.panics {
		panic("loader blew up")
	}
	if err, ok := f.errs[ws]; ok {
		return nil, err
	}
	return f.graphs[ws], nil
}

// baseGraph: a, d unrelated; b imports a; c test-imports b (modelled as an
// import); e imports b.
func baseGraph() *Graph {
	return NewGraph([]Package{
		pkg("backend/internal/a", "backend"),
		pkg("backend/internal/b", "backend", "backend/internal/a"),
		pkg("backend/internal/c", "backend", "backend/internal/b"),
		pkg("backend/internal/d", "backend"),
		pkg("backend/internal/foo", "backend"),
		pkg("cli/internal/x", "cli"),
		pkg("cli/cmd/fishhawk", "cli", "cli/internal/x"),
	})
}

// goModSurface is a minimal valid declaration: G covers go.mod only.
var goModSurface = &Surface{Global: []string{"go.mod"}}

func oneWS(g *Graph) *Classifier {
	return &Classifier{Loader: &fakeLoader{graphs: map[string]*Graph{"ws": g}}}
}

func TestClassify_Table(t *testing.T) {
	t.Parallel()
	hPrime := NewGraph([]Package{
		pkg("backend/internal/a", "backend"),
		pkg("backend/internal/q", "backend"),
		pkg("backend/internal/p", "backend"),
		pkg("backend/internal/r", "backend", "backend/internal/p"),
	})
	head := NewGraph([]Package{
		pkg("backend/internal/a", "backend"),
		pkg("backend/internal/q", "backend", "backend/internal/a"),
		pkg("backend/internal/p", "backend"),
		pkg("backend/internal/p/q", "backend"),
		pkg("backend/internal/r", "backend", "backend/internal/p"),
	})
	two := &Classifier{Loader: &fakeLoader{graphs: map[string]*Graph{"bprime": hPrime, "head": head}}}
	// This repository's golden: every pre-declaration result holds under its
	// declaration, except frontend/* now forces verification through G.
	// README.md stays inert; infra/* exercises shared-artifact behavior.
	surface := thisRepoSurface(t)

	cases := []struct {
		name       string
		c          *Classifier
		delta, pr  []string
		workspaces []string
		want       Result
	}{
		{"docs_only_global", oneWS(baseGraph()), []string{"docs/ARCHITECTURE.md"}, []string{"backend/internal/foo/foo.go"}, nil,
			Result{false, "global_surface:docs/ARCHITECTURE.md"}},
		{"disjoint_packages", oneWS(baseGraph()), []string{"backend/internal/a/a.go"}, []string{"backend/internal/d/d.go"}, nil,
			Result{true, "independent"}},
		{"direct_import", oneWS(baseGraph()), []string{"backend/internal/a/a.go"}, []string{"backend/internal/b/b.go"}, nil,
			Result{false, "reach_overlap:backend/internal/b"}},
		{"chain_import", oneWS(baseGraph()), []string{"backend/internal/a/a.go"}, []string{"backend/internal/c/c.go"}, nil,
			Result{false, "reach_overlap:backend/internal/c"}},
		{"test_import_chain", oneWS(baseGraph()), []string{"backend/internal/a/a.go"}, []string{"backend/internal/c/c_test.go"}, nil,
			Result{false, "reach_overlap:backend/internal/c"}},
		{"testdata_under_pr_package", oneWS(baseGraph()), []string{"backend/internal/d/testdata/x.json"}, []string{"backend/internal/d/d.go"}, nil,
			Result{false, "reach_overlap:backend/internal/d"}},
		{"migration_both_sides", oneWS(baseGraph()),
			[]string{"backend/internal/postgres/migrations/0102_a.up.sql"}, []string{"backend/internal/postgres/migrations/0102_b.up.sql"}, nil,
			Result{false, "global_surface:backend/internal/postgres/migrations/0102_a.up.sql"}},
		{"pr_global_surface", oneWS(baseGraph()), []string{"backend/internal/a/a.go"}, []string{".golangci.yml"}, nil,
			Result{false, "global_surface:.golangci.yml"}},
		{"pr_added_import_edge", two, []string{"backend/internal/a/a.go"}, []string{"backend/internal/q/q.go"}, []string{"bprime", "head"},
			Result{false, "reach_overlap:backend/internal/q"}},
		{"every_ancestor_package", two, []string{"backend/internal/p/q/x.json"}, []string{"backend/internal/r/r.go"}, []string{"bprime", "head"},
			Result{false, "reach_overlap:backend/internal/r"}},
		{"go_mod_in_pr", oneWS(baseGraph()), []string{"cli/internal/x/x.go"}, []string{"cli/go.mod"}, nil,
			Result{false, "global_surface:cli/go.mod"}},
		{"go_work_in_pr", oneWS(baseGraph()), []string{"cli/internal/x/x.go"}, []string{"go.work"}, nil,
			Result{false, "global_surface:go.work"}},
		{"frontend_global", oneWS(baseGraph()), []string{"frontend/src/a.ts"}, []string{"backend/internal/q/q.go"}, nil,
			Result{false, "global_surface:frontend/src/a.ts"}},
		{"shared_artifact", oneWS(baseGraph()), []string{"infra/src/a.tf"}, []string{"infra/src/b.tf"}, nil,
			Result{false, "shared_artifact:infra/src"}},
		{"shared_artifact_floor", oneWS(baseGraph()), []string{"infra/a.tf"}, []string{"infra/src/b.tf"}, nil,
			Result{false, "shared_artifact:infra"}},
		{"same_path_both_sides", oneWS(baseGraph()), []string{"README.md"}, []string{"README.md"}, nil,
			Result{false, "shared_artifact:README.md"}},
		{"inert_disjoint", oneWS(baseGraph()), []string{"README.md"}, []string{"backend/internal/d/d.go"}, nil,
			Result{true, "independent"}},
		{"empty_delta", oneWS(baseGraph()), nil, []string{"backend/internal/d/d.go"}, nil,
			Result{false, "undecidable:empty_delta"}},
		{"empty_pr", oneWS(baseGraph()), []string{"backend/internal/d/d.go"}, nil, nil,
			Result{false, "undecidable:empty_pr_diff"}},
		{"absolute_path", oneWS(baseGraph()), []string{"/etc/passwd"}, []string{"README.md"}, nil,
			Result{false, "undecidable:invalid_path:/etc/passwd"}},
		{"escaping_path", oneWS(baseGraph()), []string{"docs/../../x"}, []string{"README.md"}, nil,
			Result{false, "undecidable:invalid_path:docs/../../x"}},
		{"empty_path", oneWS(baseGraph()), []string{""}, []string{"README.md"}, nil,
			Result{false, "undecidable:invalid_path:"}},
		{"backslash_path", oneWS(baseGraph()), []string{`backend\internal\a\a.go`}, []string{"README.md"}, nil,
			Result{false, `undecidable:invalid_path:backend\internal\a\a.go`}},
		{"dot_slash_path", oneWS(baseGraph()), []string{"README.md"}, []string{"./docs/ARCHITECTURE.md"}, nil,
			Result{false, "undecidable:invalid_path:./docs/ARCHITECTURE.md"}},
		{"double_slash_path", oneWS(baseGraph()), []string{"a//b"}, []string{"README.md"}, nil,
			Result{false, "undecidable:invalid_path:a//b"}},
		{"trailing_slash_path", oneWS(baseGraph()), []string{"backend/internal/a/"}, []string{"README.md"}, nil,
			Result{false, "undecidable:invalid_path:backend/internal/a/"}},
		{"unmapped_go", oneWS(baseGraph()), []string{"tools/x.go"}, []string{"backend/internal/d/d.go"}, nil,
			Result{false, "undecidable:unmapped_go_path:tools/x.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ws := tc.workspaces
			if ws == nil {
				ws = []string{"ws"}
			}
			got := tc.c.Classify(context.Background(), Input{DeltaPaths: tc.delta, PRPaths: tc.pr, Workspaces: ws, Surface: surface})
			if got != tc.want {
				t.Fatalf("Classify = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The canonical-path control (C8): a non-canonical spelling of a global path
// must never reach independent, whichever side carries it.
func TestClassify_NonCanonicalGlobalPathNeverIndependent(t *testing.T) {
	t.Parallel()
	c := oneWS(baseGraph())
	s := thisRepoSurface(t)
	for _, in := range []Input{
		{DeltaPaths: []string{"./docs/ARCHITECTURE.md"}, PRPaths: []string{"backend/internal/d/d.go"}, Workspaces: []string{"ws"}, Surface: s},
		{DeltaPaths: []string{"backend/internal/d/d.go"}, PRPaths: []string{"./docs/ARCHITECTURE.md"}, Workspaces: []string{"ws"}, Surface: s},
	} {
		got := c.Classify(context.Background(), in)
		if got.Independent || got.Reason != "undecidable:invalid_path:./docs/ARCHITECTURE.md" {
			t.Fatalf("Classify(%v) = %+v, want undecidable:invalid_path:./docs/ARCHITECTURE.md", in, got)
		}
	}
}

func TestClassify_LoaderError(t *testing.T) {
	t.Parallel()
	c := &Classifier{Loader: &fakeLoader{errs: map[string]error{"ws": errors.New("go list: package x: boom")}}}
	got := c.Classify(context.Background(), Input{
		DeltaPaths: []string{"backend/internal/a/README.md"},
		PRPaths:    []string{"backend/internal/d/notes.txt"},
		Workspaces: []string{"ws"},
		Surface:    goModSurface,
	})
	if got != (Result{false, "undecidable:load:go list: package x: boom"}) {
		t.Fatalf("Classify = %+v", got)
	}
}

func TestClassify_NilGraph(t *testing.T) {
	t.Parallel()
	c := &Classifier{Loader: &fakeLoader{graphs: map[string]*Graph{}}}
	got := c.Classify(context.Background(), Input{DeltaPaths: []string{"README.md"}, PRPaths: []string{"x.txt"}, Workspaces: []string{"ws"}, Surface: goModSurface})
	if got != (Result{false, "undecidable:load:nil_graph"}) {
		t.Fatalf("Classify = %+v", got)
	}
}

func TestClassify_GuardInputs(t *testing.T) {
	t.Parallel()
	in := Input{DeltaPaths: []string{"README.md"}, PRPaths: []string{"x.txt"}, Workspaces: []string{"ws"}, Surface: goModSurface}
	var nilC *Classifier
	if got := nilC.Classify(context.Background(), in); got.Reason != "undecidable:nil_classifier" {
		t.Fatalf("nil classifier = %+v", got)
	}
	if got := (&Classifier{}).Classify(context.Background(), in); got.Reason != "undecidable:no_loader" {
		t.Fatalf("nil loader = %+v", got)
	}
	noWS := in
	noWS.Workspaces = nil
	if got := oneWS(baseGraph()).Classify(context.Background(), noWS); got.Reason != "undecidable:no_workspace" {
		t.Fatalf("no workspace = %+v", got)
	}
	blankWS := in
	blankWS.Workspaces = []string{" "}
	if got := oneWS(baseGraph()).Classify(context.Background(), blankWS); got.Reason != "undecidable:invalid_workspace" {
		t.Fatalf("blank workspace = %+v", got)
	}
	ctl := in
	ctl.DeltaPaths = []string{"a\nb"}
	if got := oneWS(baseGraph()).Classify(context.Background(), ctl); got.Reason != "undecidable:invalid_path:a b" {
		t.Fatalf("control-char path = %+v", got)
	}
}

func TestClassify_Panic(t *testing.T) {
	t.Parallel()
	c := &Classifier{Loader: &fakeLoader{panics: true}}
	got := c.Classify(context.Background(), Input{DeltaPaths: []string{"README.md"}, PRPaths: []string{"x.txt"}, Workspaces: []string{"ws"}, Surface: goModSurface})
	if got != (Result{false, "undecidable:panic"}) {
		t.Fatalf("Classify = %+v", got)
	}
}

// slowLoader takes `each` per Load but honours ctx, so a deadline shared by
// every Load cuts the second one while a per-Load deadline would not.
type slowLoader struct{ each time.Duration }

func (slowLoader) Supports(string) error { return nil }

func (s slowLoader) Load(ctx context.Context, _ string) (*Graph, error) {
	select {
	case <-time.After(s.each):
		return baseGraph(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestClassify_OneDeadlineCoversAllLoads(t *testing.T) {
	t.Parallel()
	deadline := timescale.D(200 * time.Millisecond)
	c := &Classifier{Loader: slowLoader{each: deadline * 6 / 10}, Deadline: deadline}
	start := time.Now()
	got := c.Classify(context.Background(), Input{
		DeltaPaths: []string{"backend/internal/a/a.go"},
		PRPaths:    []string{"backend/internal/d/d.go"},
		Workspaces: []string{"bprime", "head"},
		Surface:    goModSurface,
	})
	if got != (Result{false, "undecidable:deadline_exceeded"}) {
		t.Fatalf("Classify = %+v, want undecidable:deadline_exceeded", got)
	}
	if el := time.Since(start); el > deadline+timescale.D(time.Second) {
		t.Fatalf("Classify took %s, past the %s deadline plus slack", el, deadline)
	}
}

func TestClassify_OrderIndependent(t *testing.T) {
	t.Parallel()
	c := oneWS(baseGraph())
	a := c.Classify(context.Background(), Input{
		DeltaPaths: []string{"backend/internal/d/d.go", "backend/internal/a/a.go", "backend/internal/a/a.go"},
		PRPaths:    []string{"backend/internal/c/c.go", "backend/internal/b/b.go"},
		Workspaces: []string{"ws"},
		Surface:    goModSurface,
	})
	b := c.Classify(context.Background(), Input{
		DeltaPaths: []string{"backend/internal/a/a.go", "backend/internal/d/d.go"},
		PRPaths:    []string{"backend/internal/b/b.go", "backend/internal/c/c.go"},
		Workspaces: []string{"ws"},
		Surface:    goModSurface,
	})
	if a != b || a.Reason != "reach_overlap:backend/internal/b" {
		t.Fatalf("permuted inputs disagree: %+v vs %+v", a, b)
	}
}

func TestBoundReason(t *testing.T) {
	t.Parallel()
	long := "undecidable:" + strings.Repeat("é", 300)
	got := boundReason(long)
	if len(got) > MaxReasonLen || !strings.HasPrefix(got, "undecidable:") {
		t.Fatalf("boundReason len=%d", len(got))
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("boundReason cut a rune: %q", got[len(got)-4:])
	}
	if got := boundReason("a\r\nb"); got != "a  b" {
		t.Fatalf("boundReason newline = %q", got)
	}
}

// --- ADR-124: the declared surface and the workspace adapter ---------------

// disjoint is a delta/PR pair in unrelated packages of baseGraph: with no
// refusal in the path it classifies independent, so each guard below is the
// only thing between it and a transfer.
func disjoint(s *Surface) Input {
	return Input{DeltaPaths: []string{"backend/internal/a/a.go"}, PRPaths: []string{"backend/internal/d/d.go"},
		Workspaces: []string{"ws"}, Surface: s}
}

func TestClassify_UndeclaredSurfaceFailsClosed(t *testing.T) {
	t.Parallel()
	if got := oneWS(baseGraph()).Classify(context.Background(), disjoint(nil)); got != (Result{false, "undecidable:surface_undeclared"}) {
		t.Fatalf("Classify = %+v, want undecidable:surface_undeclared", got)
	}
	// Positive control: the same input under a declaration is independent.
	if got := oneWS(baseGraph()).Classify(context.Background(), disjoint(goModSurface)); !got.Independent {
		t.Fatalf("control Classify = %+v, want independent", got)
	}
}

func TestClassify_InvalidSurfaceFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		s    *Surface
		bad  string
	}{
		{"malformed_glob", &Surface{Global: []string{"scripts/["}}, "scripts/["},
		{"absolute", &Surface{Global: []string{"go.mod", "/etc/**"}}, "/etc/**"},
		{"dot_slash", &Surface{Global: []string{"go.mod"}, Inert: []string{"./x/**"}}, "./x/**"},
		{"dotdot", &Surface{Global: []string{"go.mod"}, Opaque: []string{"../x"}}, "../x"},
		{"backslash", &Surface{Global: []string{"go.mod"}, DocCheck: []string{`a\b`}}, `a\b`},
		{"empty", &Surface{Global: []string{"go.mod", ""}}, ""},
		{"dir_without_slash", &Surface{Global: []string{"go.mod"}, SharedDirs: []string{"web"}}, "web"},
		{"dir_non_canonical", &Surface{Global: []string{"go.mod"}, SharedDirs: []string{"a//b/"}}, "a//b/"},
	} {
		got := oneWS(baseGraph()).Classify(context.Background(), disjoint(tc.s))
		if want := (Result{false, "undecidable:invalid_surface:" + tc.bad}); got != want {
			t.Errorf("%s: Classify = %+v, want %+v", tc.name, got, want)
		}
	}
}

func TestClassify_IncompleteSurfaceFailsClosed(t *testing.T) {
	t.Parallel()
	for _, s := range []*Surface{{}, {Inert: []string{"**"}}} {
		if got := oneWS(baseGraph()).Classify(context.Background(), disjoint(s)); got != (Result{false, "undecidable:surface_incomplete"}) {
			t.Errorf("Classify(%+v) = %+v, want undecidable:surface_incomplete", s, got)
		}
	}
}

// The fake marks the workspace unsupported but its Load serves a graph that
// covers both paths, isolating the adapter check from any loader refusal.
func TestClassify_UnsupportedWorkspaceRefusedBeforeEveryPathRule(t *testing.T) {
	t.Parallel()
	c := &Classifier{Loader: &fakeLoader{graphs: map[string]*Graph{"ws": baseGraph()}, unsupported: map[string]bool{"ws": true}}}
	in := disjoint(goModSurface)
	if got := c.Classify(context.Background(), in); got != (Result{false, "undecidable:unsupported_language"}) {
		t.Errorf("disjoint: Classify = %+v, want undecidable:unsupported_language", got)
	}
	in.DeltaPaths = []string{"go.mod", "backend/internal/a/a.go"}
	if got := c.Classify(context.Background(), in); got != (Result{false, "undecidable:unsupported_language"}) {
		t.Errorf("with a G path: Classify = %+v, want undecidable:unsupported_language (decided before rule (b))", got)
	}
	// A Supports error that is not the sentinel is a load failure.
	l := &fakeLoader{graphs: map[string]*Graph{"ws": baseGraph()}}
	bad := &Classifier{Loader: &supportsErrLoader{fakeLoader: l, err: errors.New("stat: denied")}}
	if got := bad.Classify(context.Background(), disjoint(goModSurface)); got != (Result{false, "undecidable:load:stat: denied"}) {
		t.Errorf("resolution error: Classify = %+v", got)
	}
	// The loader's own defence-in-depth refusal maps to the same code.
	ld := &Classifier{Loader: &fakeLoader{errs: map[string]error{"ws": fmt.Errorf("%w: x", ErrUnsupportedLanguage)}}}
	if got := ld.Classify(context.Background(), disjoint(goModSurface)); got != (Result{false, "undecidable:unsupported_language"}) {
		t.Errorf("Load refusal: Classify = %+v", got)
	}
}

type supportsErrLoader struct {
	*fakeLoader
	err error
}

func (l *supportsErrLoader) Supports(string) error { return l.err }

// A REAL GoListLoader over non-Go workspaces: Python only, and Python plus a
// nested Go module (not a workspace root). The declaration would make every
// path G or inert, so only the adapter check stands between them and
// independent.
func TestClassify_NonGoRepositoryNeverIndependent(t *testing.T) {
	t.Parallel()
	s := &Surface{Global: []string{"requirements.txt"}, Inert: []string{"**"}, CompletenessCheck: "check"}
	for name, files := range map[string]map[string]string{
		"python_only": {"app/main.py": "print(1)\n", "lib/util.py": "X = 1\n", "requirements.txt": "x\n"},
		"nested_go_module": {"app/main.py": "print(1)\n", "lib/util.py": "X = 1\n", "requirements.txt": "x\n",
			"tools/go.mod": "module example.com/t\n\ngo 1.25.0\n", "tools/x.go": "package t\n"},
	} {
		base, head := t.TempDir(), t.TempDir()
		writeTree(t, base, files)
		writeTree(t, head, files)
		got := (&Classifier{Loader: &GoListLoader{Env: emptyModCache(t)}}).Classify(context.Background(), Input{
			DeltaPaths: []string{"lib/util.py"}, PRPaths: []string{"app/main.py"}, Workspaces: []string{base, head}, Surface: s})
		if got != (Result{false, "undecidable:unsupported_language"}) {
			t.Errorf("%s: Classify = %+v, want undecidable:unsupported_language", name, got)
		}
	}
}

func TestClassify_UnclassifiedPathFailsClosed(t *testing.T) {
	t.Parallel()
	for _, in := range []Input{
		{DeltaPaths: []string{"tools/gen.py"}, PRPaths: []string{"backend/internal/d/d.go"}},
		{DeltaPaths: []string{"backend/internal/d/d.go"}, PRPaths: []string{"tools/gen.py"}},
	} {
		in.Workspaces, in.Surface = []string{"ws"}, goModSurface
		if got := oneWS(baseGraph()).Classify(context.Background(), in); got != (Result{false, "undecidable:unclassified_path:tools/gen.py"}) {
			t.Errorf("Classify(%v/%v) = %+v, want undecidable:unclassified_path:tools/gen.py", in.DeltaPaths, in.PRPaths, got)
		}
	}
}

// An inert-declared README under package a still seeds a: inert only rescues
// a path the graph does not cover.
func TestClassify_InertNeverRemovesGraphReach(t *testing.T) {
	t.Parallel()
	s := &Surface{Global: []string{"go.mod"}, Inert: []string{"**/README.md"}}
	got := oneWS(baseGraph()).Classify(context.Background(), Input{
		DeltaPaths: []string{"backend/internal/a/README.md"}, PRPaths: []string{"backend/internal/b/b.go"},
		Workspaces: []string{"ws"}, Surface: s})
	if got != (Result{false, "reach_overlap:backend/internal/b"}) {
		t.Fatalf("Classify = %+v, want reach_overlap:backend/internal/b", got)
	}
}

func TestClassify_WorkflowSpecSelfCovered(t *testing.T) {
	t.Parallel()
	s := &Surface{Global: []string{"go.mod"}, Inert: []string{"**"}}
	for _, in := range []Input{
		{DeltaPaths: []string{WorkflowSpecPath}, PRPaths: []string{"backend/internal/d/d.go"}},
		{DeltaPaths: []string{"backend/internal/d/d.go"}, PRPaths: []string{WorkflowSpecPath}},
	} {
		in.Workspaces, in.Surface = []string{"ws"}, s
		if got := oneWS(baseGraph()).Classify(context.Background(), in); got != (Result{false, "global_surface:" + WorkflowSpecPath}) {
			t.Errorf("Classify = %+v, want global_surface:%s", got, WorkflowSpecPath)
		}
	}
}

func TestClassify_PathPrecedence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		s         *Surface
		delta, pr string
		want      Result
	}{
		{"g_beats_opaque", &Surface{Global: []string{"gen/**"}, Opaque: []string{"gen/**"}},
			"gen/x", "backend/internal/d/d.go", Result{false, "global_surface:gen/x"}},
		{"opaque_beats_graph", &Surface{Global: []string{"go.mod"}, Opaque: []string{"backend/internal/a/**"}},
			"backend/internal/a/a.go", "backend/internal/d/d.go", Result{false, "undecidable:opaque_path:backend/internal/a/a.go"}},
		{"opaque_pr_side", &Surface{Global: []string{"go.mod"}, Opaque: []string{"vendor/**"}},
			"backend/internal/a/a.go", "vendor/x.go", Result{false, "undecidable:opaque_path:vendor/x.go"}},
		{"inert_never_exempts_shared_artifact", &Surface{Global: []string{"go.mod"}, SharedDirs: []string{"web/"}, Inert: []string{"web/**"}},
			"web/a.ts", "web/b.ts", Result{false, "shared_artifact:web"}},
		{"doc_check", &Surface{Global: []string{"go.mod"}, DocCheck: []string{"notes/**"}, Inert: []string{"notes/**"}},
			"notes/x.md", "backend/internal/d/d.go", Result{false, "doc_check:notes/x.md"}},
		{"inert_declared_unmapped_go", &Surface{Global: []string{"go.mod"}, Inert: []string{"tools/**"}},
			"tools/x.go", "backend/internal/d/d.go", Result{true, "independent"}},
		{"undeclared_unmapped_go_keeps_reason", &Surface{Global: []string{"go.mod"}},
			"tools/x.go", "backend/internal/d/d.go", Result{false, "undecidable:unmapped_go_path:tools/x.go"}},
	} {
		got := oneWS(baseGraph()).Classify(context.Background(), Input{
			DeltaPaths: []string{tc.delta}, PRPaths: []string{tc.pr}, Workspaces: []string{"ws"}, Surface: tc.s})
		if got != tc.want {
			t.Errorf("%s: Classify = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Approval condition C1: a delta path and a PR path are dependent when ANY
// declared shared-artifact dir contains both, whatever the declaration order.
func TestClassify_SharedArtifactOrderIndependent(t *testing.T) {
	t.Parallel()
	in := Input{DeltaPaths: []string{"assets/narrow/a.txt"}, PRPaths: []string{"assets/other/b.txt"}, Workspaces: []string{"ws"}}
	fwd := &Surface{Global: []string{"go.mod"}, SharedDirs: []string{"assets/narrow/", "assets/"}, Inert: []string{"assets/**"}}
	rev := &Surface{Global: []string{"go.mod"}, SharedDirs: []string{"assets/", "assets/narrow/"}, Inert: []string{"assets/**"}}
	for name, s := range map[string]*Surface{"narrow_first": fwd, "wide_first": rev} {
		in.Surface = s
		if got := oneWS(baseGraph()).Classify(context.Background(), in); got != (Result{false, "shared_artifact:assets"}) {
			t.Errorf("%s: Classify = %+v, want shared_artifact:assets", name, got)
		}
	}
	if fwd.Digest() != rev.Digest() {
		t.Errorf("surface_digest differs by shared-dir order: %s vs %s", fwd.Digest(), rev.Digest())
	}
}
