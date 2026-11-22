package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// sliceGateTestFullGatePaths is the planSliceGate fixture's served full-gate
// set. It deliberately omits go.work, go.mod and go.sum, so the go-module rule
// is the only thing that widens those rows.
var sliceGateTestFullGatePaths = []string{".fishhawk/workflows.yaml", "Makefile", "build/**", "scripts/**"}

func TestSliceGatePlan(t *testing.T) {
	full := func(reason string) sliceGateDecision {
		return sliceGateDecision{Shape: sliceGateShapeFull, Reason: reason}
	}
	eligible := sliceGateDecision{Shape: sliceGateShapeScoped}
	cases := []struct {
		name        string
		offered     bool
		unavailable string
		scope       []string
		want        sliceGateDecision
	}{
		{"not offered", false, "", []string{"m1/a/a.go"}, full("slice_gate_not_offered")},
		{"unavailable passes through", true, "surface_incomplete", []string{"m1/a/a.go"}, full("surface_incomplete")},
		{"empty scope", true, "", nil, full("empty_scope")},
		{"comma", true, "", []string{"m1/a/a,b.go"}, full("undecodable_path")},
		{"tab", true, "", []string{"m1/a/a\tb.go"}, full("undecodable_path")},
		{"newline", true, "", []string{"m1/a/a\nb.go"}, full("undecodable_path")},
		{"backslash", true, "", []string{`m1\a\a.go`}, full("undecodable_path")},
		{"absolute", true, "", []string{"/etc/passwd"}, full("undecodable_path")},
		{"dotdot", true, "", []string{"m1/../x.go"}, full("undecodable_path")},
		{"dot prefix", true, "", []string{"./scripts/test"}, full("undecodable_path")},
		{"empty entry", true, "", []string{"m1/a/a.go", ""}, full("undecodable_path")},
		{"dir prefix", true, "", []string{"m1/a/"}, full("non_concrete_scope_entry:m1/a/")},
		{"star glob", true, "", []string{"docs/*.md"}, full("non_concrete_scope_entry:docs/*.md")},
		{"class glob", true, "", []string{"m1/a/[ab].go"}, full("non_concrete_scope_entry:m1/a/[ab].go")},
		// The plan's `m1/{a,b}/x.go` row carries a comma, so the earlier
		// undecodable rule decides it (still full); the comma-free brace row
		// pins the non-concrete token for braces.
		{"brace glob with comma", true, "", []string{"m1/{a,b}/x.go"}, full("undecodable_path")},
		{"brace glob no comma", true, "", []string{"m1/{a}/x.go"}, full("non_concrete_scope_entry:m1/{a}/x.go")},
		{"go.work", true, "", []string{"m1/a/a.go", "go.work"}, full("go_module_file:go.work")},
		{"nested go.mod", true, "", []string{"a/go.mod"}, full("go_module_file:a/go.mod")},
		{"nested go.sum", true, "", []string{"a/go.sum"}, full("go_module_file:a/go.sum")},
		{"global surface", true, "", []string{"m1/a/a.go", "scripts/test"}, full("global_surface:scripts/test")},
		{"workflow spec path", true, "", []string{".fishhawk/workflows.yaml"}, full("global_surface:.fishhawk/workflows.yaml")},
		{"go only is eligible", true, "", []string{"m1/a/a.go", "m1/b/b_test.go"}, eligible},
		{"inert doc is eligible", true, "", []string{"docs/x.md"}, eligible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planSliceGate(tc.offered, tc.unavailable, sliceGateTestFullGatePaths, tc.scope)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("planSliceGate(%q) = %+v, want %+v", tc.scope, got, tc.want)
			}
			if got.eligible() != reflect.DeepEqual(tc.want, eligible) {
				t.Fatalf("eligible() = %v for %+v", got.eligible(), got)
			}
		})
	}
}

// TestSliceGatePlan_RuleOrder pins first-match ordering across the whole
// scope: the earlier rule decides even when a later entry trips a later rule
// first in list order.
func TestSliceGatePlan_RuleOrder(t *testing.T) {
	got := planSliceGate(true, "", sliceGateTestFullGatePaths, []string{"scripts/test", "a/go.mod", "m1/a/"})
	if got.Reason != "non_concrete_scope_entry:m1/a/" {
		t.Fatalf("reason = %q, want the non-concrete rule to decide before go_module_file and global_surface", got.Reason)
	}
}

func TestSliceGatePlan_ReasonBounded(t *testing.T) {
	long := "m1/" + strings.Repeat("é", 400) + "/"
	got := planSliceGate(true, "", nil, []string{long})
	if got.Shape != sliceGateShapeFull || !strings.HasPrefix(got.Reason, "non_concrete_scope_entry:m1/") {
		t.Fatalf("decision = %+v", got)
	}
	if len(got.Reason) > sliceGateReasonMaxBytes || !utf8.ValidString(got.Reason) {
		t.Fatalf("reason not bounded on a UTF-8 boundary: len=%d", len(got.Reason))
	}
}

func TestSliceGateApplies(t *testing.T) {
	cases := []struct {
		name      string
		stageType string
		parent    string
		fixup     bool
		want      bool
	}{
		{"implement child", "implement", "parent-run", false, true},
		{"fix-up child", "implement", "parent-run", true, false},
		{"standalone", "implement", "", false, false},
		{"plan stage child", "plan", "parent-run", false, false},
	}
	for _, tc := range cases {
		if got := sliceGateApplies(tc.stageType, tc.parent, tc.fixup); got != tc.want {
			t.Errorf("%s: sliceGateApplies = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// sliceGateTestGraph is the synthetic primitive graph: b imports a, d imports
// b, c is independent, e imports a (the test-only edge the loader produces),
// f in a second module imports a, and m3/w imports a but is traversal-only
// (not emittable).
func sliceGateTestGraph() (map[string][]string, map[string]bool) {
	imports := map[string][]string{
		"m1/a": nil,
		"m1/b": {"m1/a"},
		"m1/d": {"m1/b"},
		"m1/c": nil,
		"m1/e": {"m1/a"},
		"m2/f": {"m1/a"},
		"m3/w": {"m1/a"},
	}
	emittable := map[string]bool{
		"m1/a": true, "m1/b": true, "m1/d": true, "m1/c": true, "m1/e": true, "m2/f": true,
	}
	return imports, emittable
}

func TestSliceGateResolveReach(t *testing.T) {
	imports, emittable := sliceGateTestGraph()
	inert := []string{"docs/**"}

	cases := []struct {
		name       string
		scope      []string
		wantPkgs   []string
		wantReason string
	}{
		{"reverse closure", []string{"m1/a/a.go"}, []string{"m1/a", "m1/b", "m1/d", "m1/e", "m2/f"}, ""},
		{"non-go path seeds ancestor node", []string{"m1/a/testdata/x.json"}, []string{"m1/a", "m1/b", "m1/d", "m1/e", "m2/f"}, ""},
		{"independent package", []string{"m1/c/c.go"}, []string{"m1/c"}, ""},
		{"inert only", []string{"docs/x.md"}, nil, "no_reach_packages"},
		{"inert plus go", []string{"m1/c/c.go", "docs/x.md"}, []string{"m1/c"}, ""},
		{"unclassified", []string{"m1/a/a.go", "tools/gen.sh"}, nil, "unclassified_path:tools/gen.sh"},
		{"unmapped go path", []string{"m1/a/a.go", "m1/zz/z.go"}, nil, "unmapped_go_path:m1/zz/z.go"},
		{"traversal-only seed", []string{"m3/w/w.go"}, nil, "no_reach_packages"},
		{"reach is everything", []string{"m1/a/a.go", "m1/c/c.go"}, nil, "reach_is_everything"},
		// Phase 1 refuses an absolute path, but phase 2 must still terminate
		// on one: path.Dir("/") is "/", so the ancestor walk stops there.
		{"absolute non-go path terminates", []string{"/x.json"}, nil, "unclassified_path:/x.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			var pkgs []string
			var reason string
			go func() {
				defer close(done)
				pkgs, reason = sliceGateResolveReach(imports, emittable, tc.scope, inert)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("sliceGateResolveReach(%q) did not terminate", tc.scope)
			}
			if !reflect.DeepEqual(pkgs, tc.wantPkgs) || reason != tc.wantReason {
				t.Fatalf("sliceGateResolveReach(%q) = (%q, %q), want (%q, %q)", tc.scope, pkgs, reason, tc.wantPkgs, tc.wantReason)
			}
		})
	}
}

func TestSliceGateResolveReach_RootPackage(t *testing.T) {
	imports := map[string][]string{".": nil, "sub": {"."}, "other": nil}
	emittable := map[string]bool{".": true, "sub": true, "other": true}
	pkgs, reason := sliceGateResolveReach(imports, emittable, []string{"main.go"}, nil)
	if !reflect.DeepEqual(pkgs, []string{".", "sub"}) || reason != "" {
		t.Fatalf("root package reach = (%q, %q)", pkgs, reason)
	}
	pkgs, reason = sliceGateResolveReach(imports, emittable, []string{"README.md"}, nil)
	if !reflect.DeepEqual(pkgs, []string{".", "sub"}) || reason != "" {
		t.Fatalf("root non-go path reach = (%q, %q)", pkgs, reason)
	}
}

func TestSliceGateResolveReach_TooLarge(t *testing.T) {
	imports, emittable := sliceGateTestGraph()
	old := sliceGateMaxPackagesBytes
	sliceGateMaxPackagesBytes = 10
	t.Cleanup(func() { sliceGateMaxPackagesBytes = old })
	pkgs, reason := sliceGateResolveReach(imports, emittable, []string{"m1/a/a.go"}, nil)
	if pkgs != nil || reason != "reach_too_large" {
		t.Fatalf("got (%q, %q), want (nil, reach_too_large)", pkgs, reason)
	}
}

func TestSliceGateResolveReach_UndecodableDir(t *testing.T) {
	imports := map[string][]string{"m1/a": nil, "m1/x,y": {"m1/a"}, "m1/c": nil}
	emittable := map[string]bool{"m1/a": true, "m1/x,y": true, "m1/c": true}
	pkgs, reason := sliceGateResolveReach(imports, emittable, []string{"m1/a/a.go"}, nil)
	if pkgs != nil || reason != "undecodable_path" {
		t.Fatalf("got (%q, %q), want (nil, undecodable_path)", pkgs, reason)
	}
}

func TestSliceGateEvent(t *testing.T) {
	var pkgs []string
	for i := 0; i < 60; i++ {
		pkgs = append(pkgs, "m1/p"+strings.Repeat("x", i))
	}
	d := sliceGateDecision{Shape: sliceGateShapeScoped, Packages: pkgs}
	ev := sliceGateEvent(d, 2)
	if ev.Kind != "verify_slice_gate" {
		t.Fatalf("kind = %q", ev.Kind)
	}
	var p struct {
		Shape     string   `json:"shape"`
		Reason    string   `json:"reason"`
		Packages  []string `json:"packages"`
		Count     int      `json:"packages_count"`
		Truncated bool     `json:"packages_truncated"`
		Iteration int      `json:"iteration"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Shape != "slice_scoped" || len(p.Packages) != 50 || p.Count != 60 || !p.Truncated || p.Iteration != 2 {
		t.Fatalf("payload = %+v", p)
	}
	if !reflect.DeepEqual(p.Packages, pkgs[:50]) {
		t.Fatal("event packages are not the first 50 dirs")
	}

	long := sliceGateDecision{Shape: sliceGateShapeFull, Reason: "unclassified_path:" + strings.Repeat("z", 1000)}
	if err := json.Unmarshal(sliceGateEvent(long, 0).Payload, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Reason) != sliceGateReasonMaxBytes || p.Count != 0 || p.Truncated {
		t.Fatalf("full event payload = shape %q reason len %d count %d", p.Shape, len(p.Reason), p.Count)
	}
}
