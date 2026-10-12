package basedelta

import (
	"reflect"
	"sort"
	"testing"
)

func closureDirs(g *Graph, seedDirs ...string) []string {
	var seeds []string
	for _, d := range seedDirs {
		seeds = append(seeds, modPrefix+d)
	}
	var out []string
	for ip := range g.reverseClosure(seeds) {
		out = append(out, g.pkgs[ip].Dir)
	}
	sort.Strings(out)
	return out
}

func TestReverseClosure_FixedPoint(t *testing.T) {
	t.Parallel()
	chain := NewGraph([]Package{
		pkg("a", "."), pkg("b", ".", "a"), pkg("c", ".", "b"), pkg("d", ".", "c"), pkg("z", "."),
	})
	if got, want := closureDirs(chain, "a"), []string{"a", "b", "c", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("chain closure = %v, want %v", got, want)
	}
	diamond := NewGraph([]Package{
		pkg("a", "."), pkg("l", ".", "a"), pkg("r", ".", "a"), pkg("top", ".", "l", "r"),
	})
	if got, want := closureDirs(diamond, "a"), []string{"a", "l", "r", "top"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("diamond closure = %v, want %v", got, want)
	}
	if got, want := closureDirs(diamond, "top"), []string{"top"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("leaf closure = %v, want %v", got, want)
	}
}

func TestPackagesFor(t *testing.T) {
	t.Parallel()
	g := NewGraph([]Package{
		pkg("cli/internal/x", "cli"),
		pkg("cli/cmd/fishhawk", "cli"),
		pkg("backend/internal/p", "backend"),
		pkg("backend/internal/p/q", "backend"),
		pkg("backend/internal/d", "backend"),
	})
	dirsOf := func(ips []string) []string {
		var out []string
		for _, ip := range ips {
			out = append(out, g.pkgs[ip].Dir)
		}
		sort.Strings(out)
		return out
	}
	cases := []struct {
		path    string
		want    []string
		wantErr string
	}{
		{"cli/go.mod", []string{"cli/cmd/fishhawk", "cli/internal/x"}, ""},
		{"cli/go.sum", []string{"cli/cmd/fishhawk", "cli/internal/x"}, ""},
		{"go.work", []string{"backend/internal/d", "backend/internal/p", "backend/internal/p/q", "cli/cmd/fishhawk", "cli/internal/x"}, ""},
		{"verifier/go.mod", nil, "unmapped_go_mod:verifier/go.mod"},
		{"backend/internal/p/q/q.go", []string{"backend/internal/p/q"}, ""},
		{"backend/internal/p/q/x.json", []string{"backend/internal/p", "backend/internal/p/q"}, ""},
		{"backend/internal/p/q/testdata/y.json", []string{"backend/internal/p", "backend/internal/p/q"}, ""},
		{"backend/internal/d/testdata/fixture.go", []string{"backend/internal/d"}, ""},
		{"tools/gen.go", nil, "unmapped_go_path:tools/gen.go"},
		{"README.md", nil, ""},
		// Defense in depth behind Classify's path validation: an absolute
		// path terminates (path.Dir("/") is "/", never ".").
		{"/etc/passwd", nil, ""},
		{"/x.go", nil, "unmapped_go_path:/x.go"},
	}
	for _, tc := range cases {
		got, err := g.packagesFor(tc.path)
		if tc.wantErr != "" {
			if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("packagesFor(%s) err = %v, want %s", tc.path, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("packagesFor(%s): %v", tc.path, err)
		}
		if d := dirsOf(got); !reflect.DeepEqual(d, tc.want) {
			t.Fatalf("packagesFor(%s) = %v, want %v", tc.path, d, tc.want)
		}
	}
}

func TestUnion(t *testing.T) {
	t.Parallel()
	g1 := NewGraph([]Package{pkg("a", "."), pkg("q", ".")})
	g2 := NewGraph([]Package{pkg("a", "."), pkg("q", ".", "a"), pkg("n", ".")})
	u := Union(g1, nil, g2)
	if u.Len() != 3 {
		t.Fatalf("Union Len = %d, want 3", u.Len())
	}
	if got := u.byDir["a"]; len(got) != 1 {
		t.Fatalf("Union duplicated dir a: %v", got)
	}
	q, ok := u.Package(modPrefix + "q")
	if !ok || !reflect.DeepEqual(q.Imports, []string{modPrefix + "a"}) {
		t.Fatalf("Union lost q's head-only edge: %+v", q)
	}
	if _, ok := u.Package(modPrefix + "missing"); ok {
		t.Fatal("Package found a missing import path")
	}
	var nilG *Graph
	if nilG.Len() != 0 {
		t.Fatal("nil graph Len != 0")
	}
	// One import path under two dirs (a moved package) keeps both dirs.
	moved := Union(NewGraph([]Package{pkg("old", ".")}), NewGraph([]Package{{ImportPath: modPrefix + "old", Dir: "new", ModuleDir: "."}}))
	if len(moved.byDir["old"]) != 1 || len(moved.byDir["new"]) != 1 {
		t.Fatalf("Union lost a moved package dir: %v", moved.byDir)
	}
	again := Union(moved)
	if len(again.byDir["new"]) != 1 {
		t.Fatalf("re-Union lost the second dir: %v", again.byDir)
	}
}
