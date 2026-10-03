package permdrift

import (
	"reflect"
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
		{"read-all to write-all", "permissions: read-all\njobs:\n  x: {runs-on: a}\n", "permissions: write-all\njobs:\n  x: {runs-on: a}\n",
			[]Change{{Key: "jobs.x.*", Before: "read (read-all)", After: "write (write-all)", Direction: Widened}}},
		{"write-all to an explicit write scope is not a widening",
			"jobs:\n  x:\n    permissions: write-all\n", "jobs:\n  x:\n    permissions: {contents: write}\n",
			[]Change{{Key: "jobs.x.*", Before: "write (write-all)", After: Absent, Direction: Narrowed}}},
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
