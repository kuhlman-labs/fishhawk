package spec

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/bmatcuk/doublestar/v4"
)

// The predicate is byte-duplicated across backend/internal/spec and
// cli/internal/spec (the two modules cannot share a package). This test file
// is ALSO byte-identical across both modules — it is `package spec` (internal,
// so it can reach unexported helpers if the decoder grows any), references no
// module-specific symbol, reads its fixtures from the module-local
// testdata/predicate-fixtures.json (mirrored from docs/spec/ by
// scripts/sync-schemas), and reads the sibling module's source over the repo
// root for the byte-identity assertion. So the same bytes compile and pass in
// both modules, which is exactly the parity the primitive requires.

// --- fixture corpus loader ---------------------------------------------------

// encStr is a fixture string value that may arrive as a bare JSON string or as
// an object carrying a byte-exact encoding. A JSON string cannot represent
// arbitrary invalid UTF-8 (encoding/json replaces invalid bytes with U+FFFD),
// so an adversarial path such as 0xFF 0xFE is carried via `hex`. Decode
// precedence when more than one field is present: hex > b64 > str.
type encStr struct {
	Str string `json:"str"`
	Hex string `json:"hex"`
	B64 string `json:"b64"`
}

func (e *encStr) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		e.Str, e.Hex, e.B64 = s, "", ""
		return nil
	}
	var obj struct {
		Str string `json:"str"`
		Hex string `json:"hex"`
		B64 string `json:"b64"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	e.Str, e.Hex, e.B64 = obj.Str, obj.Hex, obj.B64
	return nil
}

// decode resolves the byte-exact value, honouring the hex > b64 > str
// precedence. It fails the test rather than returning a mangled value, so a
// loader bug is loud rather than silently weakening every row that depends on
// the value.
func (e encStr) decode(t *testing.T) string {
	t.Helper()
	if e.Hex != "" {
		b, err := hex.DecodeString(e.Hex)
		if err != nil {
			t.Fatalf("decode hex %q: %v", e.Hex, err)
		}
		return string(b)
	}
	if e.B64 != "" {
		b, err := base64.StdEncoding.DecodeString(e.B64)
		if err != nil {
			t.Fatalf("decode b64 %q: %v", e.B64, err)
		}
		return string(b)
	}
	return e.Str
}

// fxChange is the JSON shape of a match subject; its paths/labels carry the
// byte-safe encoding so an adversarial path can be a match input.
type fxChange struct {
	Paths      []encStr `json:"paths"`
	Labels     []encStr `json:"labels"`
	ChangeKind string   `json:"change_kind"`
	Trigger    string   `json:"trigger"`
}

func (f fxChange) toChange(t *testing.T) Change {
	t.Helper()
	return Change{
		Paths:      decodeStrs(t, f.Paths),
		Labels:     decodeStrs(t, f.Labels),
		ChangeKind: f.ChangeKind,
		Trigger:    TriggerForm(f.Trigger),
	}
}

func decodeStrs(t *testing.T, in []encStr) []string {
	t.Helper()
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, e := range in {
		out[i] = e.decode(t)
	}
	return out
}

// predicateFixtures is the whole corpus. Predicate rows decode straight into
// the production Predicate type (its json tags are the spec surface), so a
// row is exactly what a consumer would declare.
type predicateFixtures struct {
	Encoding []struct {
		Value    encStr `json:"value"`
		BytesHex string `json:"bytes_hex"`
	} `json:"encoding"`
	PathMatch []struct {
		Glob     encStr `json:"glob"`
		Path     encStr `json:"path"`
		Expected bool   `json:"expected"`
	} `json:"path_match"`
	Decode []struct {
		Raw     string  `json:"raw"`
		Decoded *encStr `json:"decoded"`
		Error   bool    `json:"error"`
	} `json:"decode"`
	PredicateMatch []struct {
		Predicate Predicate `json:"predicate"`
		Change    fxChange  `json:"change"`
		Expected  bool      `json:"expected"`
	} `json:"predicate_match"`
	Validate []struct {
		Predicate Predicate `json:"predicate"`
		Error     bool      `json:"error"`
	} `json:"validate"`
	SplitNUL []struct {
		InputHex string   `json:"input_hex"`
		Expected []encStr `json:"expected"`
	} `json:"split_nul"`
}

func loadFixtures(t *testing.T) predicateFixtures {
	t.Helper()
	raw, err := os.ReadFile("testdata/predicate-fixtures.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fx predicateFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	return fx
}

// --- CONDITION 1: the encoding itself round-trips ---------------------------

func TestPredicateFixtureEncodingRoundTrips(t *testing.T) {
	fx := loadFixtures(t)
	if len(fx.Encoding) == 0 {
		t.Fatal("no encoding fixtures")
	}
	sawNonUTF8 := false
	for i, row := range fx.Encoding {
		got := hex.EncodeToString([]byte(row.Value.decode(t)))
		if got != row.BytesHex {
			t.Errorf("encoding[%d]: decoded hex = %q, want %q", i, got, row.BytesHex)
		}
		if row.BytesHex == "fffe" {
			sawNonUTF8 = true
		}
	}
	if !sawNonUTF8 {
		t.Error("encoding fixtures must include a genuinely invalid UTF-8 sequence (0xFF 0xFE)")
	}
}

// --- the shared-corpus behavioural harness ----------------------------------

func TestPredicateFixtureCorpus(t *testing.T) {
	fx := loadFixtures(t)

	t.Run("path_match", func(t *testing.T) {
		if len(fx.PathMatch) == 0 {
			t.Fatal("no path_match fixtures")
		}
		for i, row := range fx.PathMatch {
			glob := row.Glob.decode(t)
			path := row.Path.decode(t)
			p := Predicate{Paths: []string{glob}}
			got, err := p.Match(Change{Paths: []string{path}})
			if err != nil {
				t.Fatalf("path_match[%d] Match(%q,%q): %v", i, glob, path, err)
			}
			if got != row.Expected {
				t.Errorf("path_match[%d] Match(%q,%q) = %v, want %v", i, glob, path, got, row.Expected)
			}
		}
	})

	t.Run("predicate_match", func(t *testing.T) {
		if len(fx.PredicateMatch) == 0 {
			t.Fatal("no predicate_match fixtures")
		}
		for i, row := range fx.PredicateMatch {
			got, err := row.Predicate.Match(row.Change.toChange(t))
			if err != nil {
				t.Fatalf("predicate_match[%d]: %v", i, err)
			}
			if got != row.Expected {
				t.Errorf("predicate_match[%d] = %v, want %v (predicate %+v change %+v)", i, got, row.Expected, row.Predicate, row.Change)
			}
		}
	})

	t.Run("validate", func(t *testing.T) {
		if len(fx.Validate) == 0 {
			t.Fatal("no validate fixtures")
		}
		for i, row := range fx.Validate {
			err := row.Predicate.Validate("/p")
			if row.Error && err == nil {
				t.Errorf("validate[%d] %+v: want error, got nil", i, row.Predicate)
			}
			if !row.Error && err != nil {
				t.Errorf("validate[%d] %+v: want nil, got %v", i, row.Predicate, err)
			}
		}
	})

	t.Run("decode", func(t *testing.T) {
		if len(fx.Decode) == 0 {
			t.Fatal("no decode fixtures")
		}
		for i, row := range fx.Decode {
			got, err := DecodeGitPath(row.Raw)
			if row.Error {
				if err == nil {
					t.Errorf("decode[%d] DecodeGitPath(%q): want error, got %q", i, row.Raw, got)
				}
				continue
			}
			if err != nil {
				t.Fatalf("decode[%d] DecodeGitPath(%q): %v", i, row.Raw, err)
			}
			want := row.Decoded.decode(t)
			if got != want {
				t.Errorf("decode[%d] DecodeGitPath(%q) = %x, want %x", i, row.Raw, got, want)
			}
		}
	})

	t.Run("split_nul", func(t *testing.T) {
		if len(fx.SplitNUL) == 0 {
			t.Fatal("no split_nul fixtures")
		}
		for i, row := range fx.SplitNUL {
			in, err := hex.DecodeString(row.InputHex)
			if err != nil {
				t.Fatalf("split_nul[%d] bad input_hex: %v", i, err)
			}
			got := SplitNULPaths(in)
			want := decodeStrs(t, row.Expected)
			if len(got) != len(want) {
				t.Fatalf("split_nul[%d] len = %d, want %d (%q)", i, len(got), len(want), got)
			}
			for j := range want {
				if got[j] != want[j] {
					t.Errorf("split_nul[%d][%d] = %x, want %x", i, j, got[j], want[j])
				}
			}
		}
	})
}

// TestPredicatePathsMatchDoublestarSemantics proves the matcher is
// SEMANTICALLY IDENTICAL to the bare doublestar.Match call the test_conventions
// sweep makes in backend/internal/server/test_sweep.go — identity is
// machine-proven, not asserted in a comment, so a no-op or comment-only matcher
// fails here.
func TestPredicatePathsMatchDoublestarSemantics(t *testing.T) {
	fx := loadFixtures(t)
	for i, row := range fx.PathMatch {
		glob := row.Glob.decode(t)
		path := row.Path.decode(t)
		want, derr := doublestar.Match(glob, path)
		if derr != nil {
			t.Fatalf("path_match[%d] doublestar.Match(%q,%q): %v", i, glob, path, derr)
		}
		p := Predicate{Paths: []string{glob}}
		got, err := p.Match(Change{Paths: []string{path}})
		if err != nil {
			t.Fatalf("path_match[%d] predicate Match: %v", i, err)
		}
		if got != want {
			t.Errorf("path_match[%d] predicate=%v doublestar=%v for (%q,%q)", i, got, want, glob, path)
		}
	}
}

// TestPredicateAdversarialPaths drives real Go strings carrying a double quote,
// a backslash, a space, a DEL control byte, a non-ASCII rune, an embedded
// literal newline and an invalid UTF-8 sequence through SplitNULPaths and
// Match. A NUL-joined stream must split back into exactly those names — the
// newline-bearing name surviving as ONE path — and each matches `**`.
func TestPredicateAdversarialPaths(t *testing.T) {
	names := []string{
		`a"b.go`,
		`a\b.go`,
		`a b.go`,
		"a\x7fb.go",
		"café.go",
		"a\nb.go",
		"\xff\xfe.go",
	}
	var buf []byte
	for _, n := range names {
		buf = append(buf, n...)
		buf = append(buf, 0)
	}
	got := SplitNULPaths(buf)
	if len(got) != len(names) {
		t.Fatalf("SplitNULPaths len = %d, want %d: %q", len(got), len(names), got)
	}
	for i, n := range names {
		if got[i] != n {
			t.Errorf("SplitNULPaths[%d] = %x, want %x", i, got[i], n)
		}
	}
	// The newline-bearing name is index 5 and must be a single element.
	if got[5] != "a\nb.go" {
		t.Errorf("newline path split apart: got %x", got[5])
	}
	p := Predicate{Paths: []string{"**"}}
	for _, n := range names {
		ok, err := p.Match(Change{Paths: []string{n}})
		if err != nil {
			t.Fatalf("Match(%x): %v", n, err)
		}
		if !ok {
			t.Errorf("`**` did not match adversarial path %x", n)
		}
	}
}

// --- one test per named fail-closed / defensive branch ----------------------

func TestPredicateValidateRejectsEmpty(t *testing.T) {
	err := Predicate{}.Validate("/wf/applies_to")
	if err == nil {
		t.Fatal("empty predicate must be rejected, not treated as match-all")
	}
	if !strings.Contains(err.Error(), "/wf/applies_to") {
		t.Errorf("error must name the pointer, got %q", err)
	}
}

func TestPredicateValidateRejectsMalformedGlob(t *testing.T) {
	err := Predicate{Paths: []string{"["}}.Validate("/p")
	if err == nil {
		t.Fatal("malformed glob must be rejected at Validate")
	}
	if !strings.Contains(err.Error(), "[") {
		t.Errorf("error must name the offending glob, got %q", err)
	}
}

func TestPredicateValidateRejectsEmptyListEntry(t *testing.T) {
	if err := (Predicate{Labels: []string{""}}).Validate("/p"); err == nil {
		t.Error("empty label entry must be rejected")
	}
	if err := (Predicate{ChangeKinds: []string{""}}).Validate("/p"); err == nil {
		t.Error("empty change_kind entry must be rejected")
	}
}

func TestPredicateValidateRejectsUnknownTrigger(t *testing.T) {
	err := Predicate{Triggers: []TriggerForm{"weekly"}}.Validate("/p")
	if err == nil {
		t.Fatal("unknown trigger must be rejected")
	}
	for _, form := range []string{"diff", "scheduled", "on_demand"} {
		if !strings.Contains(err.Error(), form) {
			t.Errorf("error must list the valid set (%s), got %q", form, err)
		}
	}
}

func TestDecodeGitPathTrailingBackslash(t *testing.T) {
	raw := `"ab\"` // a quoted path whose body ends in a lone backslash
	_, err := DecodeGitPath(raw)
	if err == nil {
		t.Fatal("trailing backslash must fail closed")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", raw)) {
		t.Errorf("error must name the raw path, got %q", err)
	}
}

func TestDecodeGitPathBadOctalEscape(t *testing.T) {
	raw := `"\39"` // \3 then a non-octal 9, and only two octal digits
	_, err := DecodeGitPath(raw)
	if err == nil {
		t.Fatal("short/non-octal escape must fail closed")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", raw)) {
		t.Errorf("error must name the raw path, got %q", err)
	}
}

func TestDecodeGitPathUnknownEscape(t *testing.T) {
	raw := `"a\zb"` // \z is not a git C-escape
	_, err := DecodeGitPath(raw)
	if err == nil {
		t.Fatal("unknown escape must fail closed")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%q", raw)) {
		t.Errorf("error must name the raw path, got %q", err)
	}
}

func TestDecodeGitPathsPropagatesFirstError(t *testing.T) {
	_, err := DecodeGitPaths([]string{"ok/path.go", `"a\zb"`, "also/ok.go"})
	if err == nil {
		t.Fatal("DecodeGitPaths must return the first undecodable path's error, not a silent drop")
	}
}

// TestPredicateMatchFailsClosedOnBadPattern proves Match returns an ERROR
// rather than a silent false when a caller bypasses Validate and hands it a
// malformed glob against a non-empty change path — the deliberate divergence
// from the test-sweep's advisory fail-open.
func TestPredicateMatchFailsClosedOnBadPattern(t *testing.T) {
	p := Predicate{Paths: []string{"["}}
	ok, err := p.Match(Change{Paths: []string{"a.go"}})
	if err == nil {
		t.Fatal("Match must fail closed on a malformed glob, not return silent false")
	}
	if ok {
		t.Error("Match returned true on an errored glob")
	}
	if !strings.Contains(err.Error(), "[") {
		t.Errorf("error must name the offending glob, got %q", err)
	}
}

// --- semantics: AND across types, OR within a list --------------------------

func TestPredicateMultiCriteriaSemantics(t *testing.T) {
	p := Predicate{Paths: []string{"**/*.go"}, Labels: []string{"a", "b"}}
	cases := []struct {
		name string
		c    Change
		want bool
	}{
		{"both types satisfied (OR within labels)", Change{Paths: []string{"x.go"}, Labels: []string{"b"}}, true},
		{"paths ok but labels miss -> AND fails", Change{Paths: []string{"x.go"}, Labels: []string{"c"}}, false},
		{"labels ok but paths miss -> AND fails", Change{Paths: []string{"x.py"}, Labels: []string{"a"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.Match(tc.c)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPredicateLabelsAndChangeKind(t *testing.T) {
	if ok, _ := (Predicate{Labels: []string{"urgent"}}).Match(Change{Labels: []string{"urgent"}}); !ok {
		t.Error("labels-only predicate should match")
	}
	if ok, _ := (Predicate{ChangeKinds: []string{"feature"}}).Match(Change{ChangeKind: "feature"}); !ok {
		t.Error("change_kind-only predicate should match")
	}
	if ok, _ := (Predicate{ChangeKinds: []string{"feature"}}).Match(Change{ChangeKind: "bugfix"}); ok {
		t.Error("change_kind mismatch should not match")
	}
}

// TestPredicateNonDiffTriggers covers the ADR-067 non-diff seam: a scheduled or
// on_demand predicate matches a zero-path change, an empty Trigger normalizes
// to diff, and a paths-declaring predicate does NOT match a no-diff change.
func TestPredicateNonDiffTriggers(t *testing.T) {
	if ok, _ := (Predicate{Triggers: []TriggerForm{TriggerScheduled}}).Match(Change{Trigger: TriggerScheduled}); !ok {
		t.Error("scheduled predicate should match a scheduled no-path change")
	}
	if ok, _ := (Predicate{Triggers: []TriggerForm{TriggerOnDemand}}).Match(Change{Trigger: TriggerOnDemand}); !ok {
		t.Error("on_demand predicate should match an on_demand no-path change")
	}
	if ok, _ := (Predicate{Triggers: []TriggerForm{TriggerDiff}}).Match(Change{}); !ok {
		t.Error("empty Trigger should normalize to diff")
	}
	if ok, _ := (Predicate{Paths: []string{"**/*.go"}}).Match(Change{Trigger: TriggerScheduled}); ok {
		t.Error("paths-declaring predicate must NOT match a no-diff change")
	}
}

func TestValidTriggerForms(t *testing.T) {
	got := ValidTriggerForms()
	want := []TriggerForm{TriggerDiff, TriggerScheduled, TriggerOnDemand}
	if len(got) != len(want) {
		t.Fatalf("ValidTriggerForms = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ValidTriggerForms[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// --- CONDITION 3: byte-identity of the duplicated source, asserted not diffed -

func TestPredicateSourceParityAcrossModules(t *testing.T) {
	pairs := []struct {
		name string
		a    string
		b    string
	}{
		{"predicate.go", "../../../backend/internal/spec/predicate.go", "../../../cli/internal/spec/predicate.go"},
		{"predicate_test.go", "../../../backend/internal/spec/predicate_test.go", "../../../cli/internal/spec/predicate_test.go"},
	}
	for _, p := range pairs {
		a, err := os.ReadFile(p.a)
		if err != nil {
			t.Fatalf("read %s: %v", p.a, err)
		}
		b, err := os.ReadFile(p.b)
		if err != nil {
			t.Fatalf("read %s: %v", p.b, err)
		}
		if bytes.Equal(a, b) {
			continue
		}
		la := strings.Split(string(a), "\n")
		lb := strings.Split(string(b), "\n")
		firstDiff := "one file has extra trailing lines"
		for i := 0; i < len(la) && i < len(lb); i++ {
			if la[i] != lb[i] {
				firstDiff = firstDiff2(i, la[i], lb[i])
				break
			}
		}
		t.Errorf("%s: backend and cli copies must be byte-identical; %s", p.name, firstDiff)
	}
}

func firstDiff2(line int, a, b string) string {
	return "first differing line " + itoa(line+1) + ":\n  backend: " + a + "\n  cli:     " + b
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

// --- CONSUMABILITY: compile-time signature assertions the children rely on ---

var (
	_ Predicate                             = Predicate{Paths: nil, Labels: nil, ChangeKinds: nil, Triggers: nil}
	_ Change                                = Change{}
	_ TriggerForm                           = TriggerDiff
	_ func(Predicate, string) error         = Predicate.Validate
	_ func(Predicate, Change) (bool, error) = Predicate.Match
	_ func([]byte) []string                 = SplitNULPaths
	_ func(string) (string, error)          = DecodeGitPath
	_ func([]string) ([]string, error)      = DecodeGitPaths
	_ func() []TriggerForm                  = ValidTriggerForms
)
