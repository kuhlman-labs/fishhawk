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
