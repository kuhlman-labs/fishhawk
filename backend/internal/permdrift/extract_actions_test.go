package permdrift

import (
	"reflect"
	"strings"
	"testing"
)

// diffActions extracts both sides and compares them, failing on an
// extraction error.
func diffActions(t *testing.T, base, head string) []Change {
	t.Helper()
	b, err := ExtractActions([]byte(base))
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	h, err := ExtractActions([]byte(head))
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return Compare(b, h)
}

const ciRead = `name: ci
on: [push]
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    steps: [{run: make}]
`

func TestExtractActions(t *testing.T) {
	cases := []struct {
		name       string
		base, head string
		want       []Change
	}{
		{"workflow gains contents: write", ciRead,
			`on: [push]
permissions:
  contents: write
jobs:
  build:
    runs-on: ubuntu-latest
`,
			[]Change{{Key: "jobs.build.contents", Before: "read", After: "write", Direction: Widened}}},
		{"job-level block overrides top-level", ciRead,
			`on: [push]
permissions:
  contents: read
jobs:
  build:
    permissions:
      contents: read
      id-token: write
    runs-on: ubuntu-latest
`,
			[]Change{{Key: "jobs.build.id-token", Before: Absent, After: "write", Direction: Widened}}},
		{"removed permission is a narrowing only",
			"jobs:\n  build:\n    permissions:\n      contents: read\n      issues: write\n",
			"jobs:\n  build:\n    permissions:\n      contents: read\n",
			[]Change{{Key: "jobs.build.issues", Before: "write", After: Absent, Direction: Narrowed}}},
		{"key reorder is no change",
			"jobs:\n  b:\n    permissions: {contents: read, issues: write}\n  a:\n    permissions: {checks: read}\n",
			"jobs:\n  a:\n    permissions: {checks: read}\n  b:\n    permissions: {issues: write, contents: read}\n",
			nil},
		{"removing the block entirely reaches the write default",
			ciRead, "on: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n",
			[]Change{
				{Key: "jobs.build.*", Before: Absent, After: defaultTokenValue, Direction: Widened},
			}},
		// write-all carries an explicit id-token entry next to its wildcard
		// (the default token's carve-out), so read-all -> write-all also
		// widens id-token, which read-all never granted.
		{"read-all to write-all", "permissions: read-all\njobs:\n  x: {runs-on: a}\n", "permissions: write-all\njobs:\n  x: {runs-on: a}\n",
			[]Change{
				{Key: "jobs.x.*", Before: "read (read-all)", After: "write (write-all)", Direction: Widened},
				{Key: "jobs.x.id-token", Before: Absent, After: writeAllIDTokenValue, Direction: Widened},
			}},
		{"write-all to an explicit write scope is not a widening",
			"jobs:\n  x:\n    permissions: write-all\n", "jobs:\n  x:\n    permissions: {contents: write}\n",
			[]Change{
				{Key: "jobs.x.*", Before: "write (write-all)", After: Absent, Direction: Narrowed},
				{Key: "jobs.x.id-token", Before: writeAllIDTokenValue, After: Absent, Direction: Narrowed},
			}},
		{"empty block grants nothing", "jobs:\n  x:\n    permissions: {}\n", "jobs:\n  x:\n    permissions: {contents: none}\n", nil},
		{"a new job with no block gets the default", "jobs:\n  x:\n    permissions: {}\n", "jobs:\n  x:\n    permissions: {}\n  y:\n    runs-on: a\n",
			[]Change{{Key: "jobs.y.*", Before: Absent, After: defaultTokenValue, Direction: Widened}}},
		{"no jobs keys the top-level block", "permissions:\n  contents: read\n", "permissions:\n  contents: write\n",
			[]Change{{Key: "permissions.contents", Before: "read", After: "write", Direction: Widened}}},
		{"absent file to a defaulted job", "", "jobs:\n  x: {runs-on: a}\n",
			[]Change{{Key: "jobs.x.*", Before: Absent, After: defaultTokenValue, Direction: Widened}}},
		{"null job permissions falls back to the top-level block",
			"permissions: {contents: read}\njobs:\n  x:\n    permissions:\n", "permissions: {contents: read}\njobs:\n  x: {runs-on: a}\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := diffActions(t, c.base, c.head); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("changes =\n  %+v\nwant\n  %+v", got, c.want)
			}
		})
	}
}

// TestExtractActions_Errors: every malformed input fails the extraction (the
// check then fails closed to unevaluable) rather than yielding a partial set.
func TestExtractActions_Errors(t *testing.T) {
	for name, in := range map[string]string{
		"malformed yaml":         "jobs: [unclosed\n",
		"jobs not a mapping":     "jobs: [a, b]\n",
		"job not a mapping":      "jobs:\n  x: 3\n",
		"unknown shorthand":      "permissions: admin-all\njobs:\n  x: {runs-on: a}\n",
		"unknown level":          "jobs:\n  x:\n    permissions: {contents: admin}\n",
		"non-string level":       "jobs:\n  x:\n    permissions: {contents: 3}\n",
		"block wrong type":       "jobs:\n  x:\n    permissions: [contents]\n",
		"top-level bad, no jobs": "permissions: 7\n",
	} {
		t.Run(name, func(t *testing.T) {
			if g, err := ExtractActions([]byte(in)); err == nil {
				t.Fatalf("ExtractActions = %+v, want an error", g)
			}
		})
	}
}

// TestExtractActions_FileDerivedWildcard pins item 10's wildcard half on the
// Actions extractor: a scope literally named "*" (or a job id carrying a '.')
// is a FILE-DERIVED segment, so it keys as an ordinary escaped segment and
// never as the extractor's own write-all wildcard.
//
// COUNTERFACTUAL: make keySegment return s unchanged (body mutation). The
// base scope "*" then keys `jobs.x.*` at write rank and coveredBy suppresses
// the head-only `contents: write` grant — the widening vanishes and the first
// assertion goes RED (observed: `changes = [] want [{Key:jobs.x.contents
// Before:(absent) After:write Direction:widened}]`);
// and job "a.b" with scope "c" collides with job "a" with scope "b.c", so the
// key-shape assertion goes RED too.
func TestExtractActions_FileDerivedWildcard(t *testing.T) {
	got := diffActions(t,
		"jobs:\n  x:\n    permissions: {\"*\": write}\n",
		"jobs:\n  x:\n    permissions: {\"*\": write, contents: write}\n")
	want := []Change{{Key: "jobs.x.contents", Before: Absent, After: "write", Direction: Widened}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes =\n  %+v\nwant\n  %+v", got, want)
	}
	g, err := ExtractActions([]byte("jobs:\n  a.b:\n    permissions: {c: write}\n  a:\n    permissions: {b.c: read}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keysOf(g), []string{"jobs.a%2Eb.c", "jobs.a.b%2Ec"}) {
		t.Fatalf("keys = %v, want the dotted job id and scope each escaped to one segment", keysOf(g))
	}
}

// TestExtractActions_DefaultTokenExcludesIDToken pins #3939 F4: a job with NO
// permissions block runs with the default GITHUB_TOKEN, which never carries
// id-token, so adding `id-token: write` (or write-all) to it is a widening.
// The base workflow has no block, so ExtractActions emits the `jobs.build.*`
// write wildcard (Except "id-token"); the head grants id-token explicitly.
//
// COUNTERFACTUALS (body mutations, one at a time):
//   - A: the default entry's Except set to "" — coveredBy subsumes the
//     head-only id-token grant under the base wildcard, Compare reports only a
//     `jobs.build.*` narrowing, and the "explicit id-token" arm goes RED;
//   - B: coveredBy's Except clause made always-true — the same arm (and the
//     TestCompare "except carve-out" row) goes RED;
//   - C: the write-all explicit id-token Put deleted — the "write-all" arm
//     goes RED (same wildcard key at the same rank on both sides, so nothing
//     else reports a change), and so does "write-all back to block-less".
//
// CONTROL: block-less -> {contents: write} stays at zero widenings (the
// carve-out names id-token only).
func TestExtractActions_DefaultTokenExcludesIDToken(t *testing.T) {
	const tmpl = "name: ci\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n%PERMS%    steps:\n      - run: echo\n"
	doc := func(perms string) string { return strings.Replace(tmpl, "%PERMS%", perms, 1) }
	blockless := doc("")
	wildcardGone := Change{Key: "jobs.build.*", Before: defaultTokenValue, After: Absent, Direction: Narrowed}
	cases := []struct {
		name       string
		base, head string
		want       []Change
	}{
		{"explicit id-token: write on a block-less job widens id-token",
			blockless, doc("    permissions:\n      id-token: write\n"),
			[]Change{wildcardGone, {Key: "jobs.build.id-token", Before: Absent, After: "write", Direction: Widened}}},
		{"write-all on a block-less job widens id-token",
			blockless, doc("    permissions: write-all\n"),
			[]Change{{Key: "jobs.build.id-token", Before: Absent, After: writeAllIDTokenValue, Direction: Widened}}},
		{"write-all back to block-less narrows id-token",
			doc("    permissions: write-all\n"), blockless,
			[]Change{{Key: "jobs.build.id-token", Before: writeAllIDTokenValue, After: Absent, Direction: Narrowed}}},
		{"control: {contents: write} on a block-less job widens nothing",
			blockless, doc("    permissions:\n      contents: write\n"),
			[]Change{wildcardGone}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := diffActions(t, c.base, c.head); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("changes =\n  %+v\nwant\n  %+v", got, c.want)
			}
		})
	}
	// The same through the product surface (Detect), the shape the server
	// calls: exactly one widening, on jobs.build.id-token.
	r := Detect(surfaceByID(t, "gha-workflow-permissions"), side(blockless), side(doc("    permissions:\n      id-token: write\n")))
	want := []Change{{Key: "jobs.build.id-token", Before: Absent, After: "write", Direction: Widened}}
	if r.Unevaluable != "" || !reflect.DeepEqual(r.Widened, want) {
		t.Fatalf("Detect = %+v; want exactly the widening %+v", r, want)
	}
	// The jobless top-level form models write-all the same way.
	got := diffActions(t, "permissions: read-all\n", "permissions: write-all\n")
	wantJobless := []Change{
		{Key: "permissions.*", Before: "read (read-all)", After: "write (write-all)", Direction: Widened},
		{Key: "permissions.id-token", Before: Absent, After: writeAllIDTokenValue, Direction: Widened},
	}
	if !reflect.DeepEqual(got, wantJobless) {
		t.Fatalf("jobless changes =\n  %+v\nwant\n  %+v", got, wantJobless)
	}
}
