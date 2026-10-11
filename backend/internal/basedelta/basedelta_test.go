package basedelta

import (
	"context"
	"errors"
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

// fakeLoader serves a fixed graph (or error) per workspace dir.
type fakeLoader struct {
	graphs map[string]*Graph
	errs   map[string]error
	panics bool
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
		{"shared_artifact", oneWS(baseGraph()), []string{"frontend/src/a.ts"}, []string{"frontend/src/b.ts"}, nil,
			Result{false, "shared_artifact:frontend/src"}},
		{"shared_artifact_floor", oneWS(baseGraph()), []string{"frontend/a.ts"}, []string{"frontend/src/b.ts"}, nil,
			Result{false, "shared_artifact:frontend"}},
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
			got := tc.c.Classify(context.Background(), Input{DeltaPaths: tc.delta, PRPaths: tc.pr, Workspaces: ws})
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
	for _, in := range []Input{
		{DeltaPaths: []string{"./docs/ARCHITECTURE.md"}, PRPaths: []string{"backend/internal/d/d.go"}, Workspaces: []string{"ws"}},
		{DeltaPaths: []string{"backend/internal/d/d.go"}, PRPaths: []string{"./docs/ARCHITECTURE.md"}, Workspaces: []string{"ws"}},
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
	})
	if got != (Result{false, "undecidable:load:go list: package x: boom"}) {
		t.Fatalf("Classify = %+v", got)
	}
}

func TestClassify_NilGraph(t *testing.T) {
	t.Parallel()
	c := &Classifier{Loader: &fakeLoader{graphs: map[string]*Graph{}}}
	got := c.Classify(context.Background(), Input{DeltaPaths: []string{"README.md"}, PRPaths: []string{"x.txt"}, Workspaces: []string{"ws"}})
	if got != (Result{false, "undecidable:load:nil_graph"}) {
		t.Fatalf("Classify = %+v", got)
	}
}

func TestClassify_GuardInputs(t *testing.T) {
	t.Parallel()
	in := Input{DeltaPaths: []string{"README.md"}, PRPaths: []string{"x.txt"}, Workspaces: []string{"ws"}}
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
	got := c.Classify(context.Background(), Input{DeltaPaths: []string{"README.md"}, PRPaths: []string{"x.txt"}, Workspaces: []string{"ws"}})
	if got != (Result{false, "undecidable:panic"}) {
		t.Fatalf("Classify = %+v", got)
	}
}

// slowLoader takes `each` per Load but honours ctx, so a deadline shared by
// every Load cuts the second one while a per-Load deadline would not.
type slowLoader struct{ each time.Duration }

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
	})
	b := c.Classify(context.Background(), Input{
		DeltaPaths: []string{"backend/internal/a/a.go", "backend/internal/d/d.go"},
		PRPaths:    []string{"backend/internal/b/b.go", "backend/internal/c/c.go"},
		Workspaces: []string{"ws"},
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
