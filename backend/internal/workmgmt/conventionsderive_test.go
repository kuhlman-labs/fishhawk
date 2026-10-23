package workmgmt

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// epicConv returns conventions declaring only an epic type with format.
func epicConv(format string) Conventions {
	return Conventions{Types: map[string]ItemType{"epic": {TitleFormat: format}}}
}

// todaysEpicFormat is the epic title_format this repository declares today.
const todaysEpicFormat = "[E{number}] {summary}"

// TestEpicTitleRegexp_TodaysFormatMatchesTheRetiredLiterals pins that the
// derived patterns for today's "[E{number}] {summary}" are EXACTLY the two
// hard-coded regexps they replace (workitems.go's open form and the
// live-validation walk's closed form, #2179), so the derivation is a
// behaviour-preserving substitution for this repository.
func TestEpicTitleRegexp_TodaysFormatMatchesTheRetiredLiterals(t *testing.T) {
	conv := epicConv(todaysEpicFormat)
	if got, want := epicTitleRegexp(conv, false).String(), `^\s*\[E(\d+)`; got != want {
		t.Errorf("open pattern = %q, want %q", got, want)
	}
	if got, want := epicTitleRegexp(conv, true).String(), `^\s*\[E(\d+)\]`; got != want {
		t.Errorf("closed pattern = %q, want %q", got, want)
	}
}

// TestEpicTitleNumber table-drives the open and closed parse over today's
// format, a custom format, a regex-metacharacter format, and every ok=false
// mode (no epic type, no {number}, no match).
func TestEpicTitleNumber(t *testing.T) {
	cases := []struct {
		name     string
		conv     Conventions
		title    string
		open     string
		openOK   bool
		closed   string
		closedOK bool
	}{
		{"epic title", epicConv(todaysEpicFormat), "[E22] x", "22", true, "22", true},
		{"child title: open stops at the digit run, closed rejects", epicConv(todaysEpicFormat), "[E22.3] x", "22", true, "", false},
		{"leading whitespace", epicConv(todaysEpicFormat), "  [E7] y", "7", true, "7", true},
		{"no token", epicConv(todaysEpicFormat), "A plain title", "", false, "", false},
		{"token not leading", epicConv(todaysEpicFormat), "see [E7] y", "", false, "", false},
		{"custom format epic", epicConv("EPIC-{number}: {summary}"), "EPIC-12: Foo", "12", true, "12", true},
		{"custom format child", epicConv("EPIC-{number}: {summary}"), "EPIC-12/1 Bar", "12", true, "", false},
		{"metacharacters are quoted", epicConv("(EPIC.{number}) {summary}"), "(EPIC.12) x", "12", true, "12", true},
		{"a quoted '.' is not a wildcard", epicConv("(EPIC.{number}) {summary}"), "(EPICx12) x", "", false, "", false},
		{"whitespace terminator: boundary required", epicConv("E{number} {summary}"), "E5 x", "5", true, "5", true},
		{"whitespace terminator rejects a child", epicConv("E{number} {summary}"), "E5.1 x", "5", true, "", false},
		{"number ends the format", epicConv("{summary} #{number}"), "Foo #9", "9", true, "9", true},
		{"no epic type", Conventions{Types: map[string]ItemType{"chore": {TitleFormat: todaysEpicFormat}}}, "[E22] x", "", false, "", false},
		{"no types at all", Conventions{}, "[E22] x", "", false, "", false},
		{"epic format without {number}", epicConv("[E{epic}] {summary}"), "[E22] x", "", false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := EpicTitleNumber(tc.conv, tc.title, false); got != tc.open || ok != tc.openOK {
				t.Errorf("open: EpicTitleNumber(%q) = (%q, %v), want (%q, %v)", tc.title, got, ok, tc.open, tc.openOK)
			}
			if got, ok := EpicTitleNumber(tc.conv, tc.title, true); got != tc.closed || ok != tc.closedOK {
				t.Errorf("closed: EpicTitleNumber(%q) = (%q, %v), want (%q, %v)", tc.title, got, ok, tc.closed, tc.closedOK)
			}
		})
	}
}

// TestTypeLabelNamespaces covers the union, de-duplication, sorting and the
// bare-label / empty-namespace drops.
func TestTypeLabelNamespaces(t *testing.T) {
	cases := []struct {
		name string
		it   ItemType
		want []string
	}{
		{"empty type", ItemType{}, []string{}},
		{"bare default labels name no namespace", ItemType{DefaultLabels: []string{"adr", "epic"}}, []string{}},
		{"union, sorted and unique", ItemType{
			RequiredLabelNamespaces: []string{"phase", "area", " area "},
			LabelDefaults:           map[string]string{"autonomy": "autonomy:medium", "phase": "phase:alpha"},
			DefaultLabels:           []string{"type:bug", "type:extra", "plain", ":nons"},
		}, []string{"area", "autonomy", "phase", "type"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TypeLabelNamespaces(tc.it)
			if got == nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("TypeLabelNamespaces = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// TestRepositoryDeclarationDerivations pins both helpers against THIS
// repository's own .fishhawk/work-management.yaml (read-only): its epic
// format parses an epic title and rejects a child in the closed form, and its
// bug, chore and feature types declare exactly {area, autonomy, phase, type}.
func TestRepositoryDeclarationDerivations(t *testing.T) {
	path := filepath.Join(pkgSrcDir, "..", "..", "..", ".fishhawk", "work-management.yaml")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	conv, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse(%s) = %v", path, err)
	}
	if n, ok := EpicTitleNumber(conv, "[E89] Catch-all", true); !ok || n != "89" {
		t.Errorf("closed epic parse = (%q, %v), want (89, true)", n, ok)
	}
	if n, ok := EpicTitleNumber(conv, "[E89.25] child", true); ok {
		t.Errorf("closed parse of a child title = (%q, true), want no match", n)
	}
	want := []string{"area", "autonomy", "phase", "type"}
	for _, typ := range []string{"bug", "chore", "feature"} {
		if got := TypeLabelNamespaces(conv.Types[typ]); !reflect.DeepEqual(got, want) {
			t.Errorf("TypeLabelNamespaces(%s) = %v, want %v", typ, got, want)
		}
	}
}
