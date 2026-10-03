package permdrift

import (
	"reflect"
	"testing"
)

func diffManifest(t *testing.T, base, head string) []Change {
	t.Helper()
	b, err := ExtractManifest([]byte(base))
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	h, err := ExtractManifest([]byte(head))
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return Compare(b, h)
}

const manifestBase = `{"name": "x", "url": "{{BACKEND_URL}}",
 "default_permissions": {"contents": "read", "issues": "write"},
 "default_events": ["push", "issues"]}`

func TestExtractManifest(t *testing.T) {
	cases := []struct {
		name string
		head string
		want []Change
	}{
		{"permission level raise", `{"default_permissions": {"contents": "write", "issues": "write"}, "default_events": ["push", "issues"]}`,
			[]Change{{Key: "default_permissions.contents", Before: "read", After: "write", Direction: Widened}}},
		{"write to admin", `{"default_permissions": {"contents": "read", "issues": "admin"}, "default_events": ["push", "issues"]}`,
			[]Change{{Key: "default_permissions.issues", Before: "write", After: "admin", Direction: Widened}}},
		{"event added", `{"default_permissions": {"contents": "read", "issues": "write"}, "default_events": ["push", "issues", "workflow_run"]}`,
			[]Change{{Key: "default_events.workflow_run", Before: Absent, After: Present, Direction: Widened}}},
		{"reorder is no change", `{"default_events": ["issues", "push"], "default_permissions": {"issues": "write", "contents": "read"}}`, nil},
		{"permission set to none is a narrowing", `{"default_permissions": {"contents": "none", "issues": "write"}, "default_events": ["push", "issues"]}`,
			[]Change{{Key: "default_permissions.contents", Before: "read", After: Absent, Direction: Narrowed}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := diffManifest(t, manifestBase, c.head); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("changes =\n  %+v\nwant\n  %+v", got, c.want)
			}
		})
	}
	if g, err := ExtractManifest(nil); err != nil || len(g) != 0 {
		t.Errorf("absent manifest = %+v, %v; want empty, nil", g, err)
	}
}

func TestExtractManifest_Errors(t *testing.T) {
	for name, in := range map[string]string{
		"malformed json":     `{"default_permissions": `,
		"unknown level":      `{"default_permissions": {"contents": "owner"}}`,
		"wrong-typed events": `{"default_events": "push"}`,
		"wrong-typed perms":  `{"default_permissions": ["contents"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if g, err := ExtractManifest([]byte(in)); err == nil {
				t.Fatalf("ExtractManifest = %+v, want an error", g)
			}
		})
	}
}

// TestExtractManifest_FileDerivedWildcard pins item 10's wildcard half on the
// manifest: a permission literally named "*" is a FILE-DERIVED segment and
// must not become a Grant wildcard that subsumes a sibling's widening.
//
// COUNTERFACTUAL: make keySegment return s unchanged (body mutation). The base
// `default_permissions.*` key is then a Grant wildcard at admin rank, and
// coveredBy suppresses the head-only `members: admin` grant, so the widening
// vanishes and the Detect assertion goes RED (observed: `Detect =
// {Widened:[] ...}, want widened [{Key:default_permissions.members ...}]`).
func TestExtractManifest_FileDerivedWildcard(t *testing.T) {
	base := `{"default_permissions": {"*": "admin"}}`
	head := `{"default_permissions": {"*": "admin", "members": "admin"}}`
	r := Detect(surfaceByID(t, "github-app-permissions-template"), side(base), side(head))
	want := []Change{{Key: "default_permissions.members", Before: Absent, After: "admin", Direction: Widened}}
	if r.Unevaluable != "" || !reflect.DeepEqual(r.Widened, want) {
		t.Fatalf("Detect = %+v, want widened %+v", r, want)
	}
	g, err := ExtractManifest([]byte(`{"default_permissions": {"*": "write", "a.b": "read"}, "default_events": ["x.*"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keysOf(g), []string{"default_events.x%2E%2A", "default_permissions.%2A", "default_permissions.a%2Eb"}) {
		t.Fatalf("keys = %v, want every file-derived name escaped to one segment", keysOf(g))
	}
}
