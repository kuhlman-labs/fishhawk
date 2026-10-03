package permdrift

import (
	"reflect"
	"testing"
)

func grant(key, value string, rank int) Entry {
	return Entry{Key: key, Value: value, Rank: rank, Polarity: Grant}
}

func restriction(key, value string, rank int) Entry {
	return Entry{Key: key, Value: value, Rank: rank, Polarity: Restriction}
}

func set(es ...Entry) Grants {
	g := Grants{}
	for _, e := range es {
		g.Put(e)
	}
	return g
}

// TestCompare pins every Compare branch. The wildcard rows are the
// subsumption control. COUNTERFACTUAL: make coveredBy return false (body
// mutation) — the row "base write-all, head {contents: write}" has a head-only
// key jobs.build.contents whose ONLY counterpart is the base wildcard
// jobs.build.*, so without subsumption it is reported Widened where the row
// asserts no widening, and the row goes RED (and symmetrically the
// "base {contents: write}, head write-all" row gains a spurious narrowing).
func TestCompare(t *testing.T) {
	cases := []struct {
		name       string
		base, head Grants
		want       []Change
	}{
		{"grant added", set(), set(grant("a", "write", 2)),
			[]Change{{Key: "a", Before: Absent, After: "write", Direction: Widened}}},
		{"grant removed", set(grant("a", "write", 2)), set(),
			[]Change{{Key: "a", Before: "write", After: Absent, Direction: Narrowed}}},
		{"restriction added", set(), set(restriction("r", Present, 1)),
			[]Change{{Key: "r", Before: Absent, After: Present, Direction: Narrowed}}},
		{"restriction removed", set(restriction("r", Present, 1)), set(),
			[]Change{{Key: "r", Before: Present, After: Absent, Direction: Widened}}},
		{"rank up", set(grant("a", "read", 1)), set(grant("a", "write", 2)),
			[]Change{{Key: "a", Before: "read", After: "write", Direction: Widened}}},
		{"rank down", set(grant("a", "write", 2)), set(grant("a", "read", 1)),
			[]Change{{Key: "a", Before: "write", After: "read", Direction: Narrowed}}},
		{"rank equal", set(grant("a", "write", 2)), set(grant("a", "write (other display)", 2)), nil},
		{"restriction rank up widens", set(restriction("c", "medium", 1)), set(restriction("c", "high", 2)),
			[]Change{{Key: "c", Before: "medium", After: "high", Direction: Widened}}},
		{"base write-all, head {contents: write}",
			set(grant("jobs.build.*", "write", 2)), set(grant("jobs.build.contents", "write", 2)),
			[]Change{{Key: "jobs.build.*", Before: "write", After: Absent, Direction: Narrowed}}},
		{"base read-all, head {contents: write} widens past the wildcard rank",
			set(grant("jobs.build.*", "read", 1)), set(grant("jobs.build.contents", "write", 2)),
			[]Change{
				{Key: "jobs.build.*", Before: "read", After: Absent, Direction: Narrowed},
				{Key: "jobs.build.contents", Before: Absent, After: "write", Direction: Widened},
			}},
		{"base {contents: write}, head write-all",
			set(grant("jobs.build.contents", "write", 2)), set(grant("jobs.build.*", "write", 2)),
			[]Change{{Key: "jobs.build.*", Before: Absent, After: "write", Direction: Widened}}},
		{"wildcard covers one segment only",
			set(grant("a.*", "write", 2)), set(grant("a.b.c", "write", 2)),
			[]Change{
				{Key: "a.*", Before: "write", After: Absent, Direction: Narrowed},
				{Key: "a.b.c", Before: Absent, After: "write", Direction: Widened},
			}},
		{"restriction wildcard never subsumes",
			set(restriction("a.*", Present, 1)), set(grant("a.b", Present, 1)),
			[]Change{
				{Key: "a.*", Before: Present, After: Absent, Direction: Widened},
				{Key: "a.b", Before: Absent, After: Present, Direction: Widened},
			}},
		{"deterministic order", set(), set(grant("z", "x", 1), grant("a", "x", 1), grant("m", "x", 1)),
			[]Change{
				{Key: "a", Before: Absent, After: "x", Direction: Widened},
				{Key: "m", Before: Absent, After: "x", Direction: Widened},
				{Key: "z", Before: Absent, After: "x", Direction: Widened},
			}},
		{"nil sets", nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Compare(c.base, c.head)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Compare =\n  %+v\nwant\n  %+v", got, c.want)
			}
		})
	}
}

func TestLevelsAndStrings(t *testing.T) {
	if r, ok := AppLevels.Rank("admin"); !ok || r != 3 {
		t.Errorf("AppLevels.Rank(admin) = %d, %v", r, ok)
	}
	if _, ok := ActionsLevels.Rank("admin"); ok {
		t.Errorf("ActionsLevels accepted admin")
	}
	if _, err := ShellLevels.mustRank("x", "bogus"); err == nil {
		t.Errorf("mustRank accepted an unknown level")
	}
	for _, c := range []struct{ got, want string }{
		{Widened.String(), "widened"}, {Narrowed.String(), "narrowed"}, {Direction(0).String(), "unknown"},
		{Grant.String(), "grant"}, {Restriction.String(), "restriction"},
	} {
		if c.got != c.want {
			t.Errorf("String() = %q, want %q", c.got, c.want)
		}
	}
}
