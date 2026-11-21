package diffsecrets

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/redaction"
)

// Every credential below is BUILT AT RUNTIME (see diffsecrets_test.go).

// slugCredential is a runtime-built gitlab-pat that is ALSO a well-formed
// rule id slug, so only the redaction-hit check can refuse it as an id.
func slugCredential() string { return "glpat-" + strings.Repeat("n", 20) }

// rule renders one YAML rule with a two-space list indent.
func rule(id, path, pattern, reason string) string {
	return "  - id: " + id + "\n    paths: [\"" + path + "\"]\n    patterns: [\"" + pattern + "\"]\n    reason: \"" + reason + "\"\n"
}

const goodNeighbour = "  - id: neighbour\n    paths: [\"keep/**\"]\n    patterns: [\"github-pat-classic\"]\n    reason: \"kept\"\n"

func TestParseAllowlist(t *testing.T) {
	t.Run("accepted rules in file order", func(t *testing.T) {
		doc := "version: 1\nentries:\n" + rule("docs-a", "docs/*.md", "github-pat-classic", "docs") +
			"markers:\n" + rule("fix-b", "fix/**", "aws-access-key-id", "fixtures")
		al, rej, err := ParseAllowlist([]byte(doc), redaction.DefaultPatterns)
		if err != nil || len(rej) != 0 {
			t.Fatalf("err=%v rej=%+v", err, rej)
		}
		want := &Allowlist{
			Entries: []Rule{{ID: "docs-a", Paths: []string{"docs/*.md"}, Patterns: []string{"github-pat-classic"}, Reason: "docs"}},
			Markers: []Rule{{ID: "fix-b", Paths: []string{"fix/**"}, Patterns: []string{"aws-access-key-id"}, Reason: "fixtures"}},
		}
		if !reflect.DeepEqual(al, want) {
			t.Errorf("allowlist = %+v, want %+v", al, want)
		}
	})
	t.Run("empty and comment-only are no rules", func(t *testing.T) {
		for _, in := range []string{"", "  \n", "# nothing here\n"} {
			al, rej, err := ParseAllowlist([]byte(in), redaction.DefaultPatterns)
			if err != nil || len(rej) != 0 || al == nil || len(al.Entries)+len(al.Markers) != 0 {
				t.Errorf("ParseAllowlist(%q) = %+v, %+v, %v", in, al, rej, err)
			}
		}
	})
	t.Run("whole-file errors never quote content", func(t *testing.T) {
		cred := classicPAT()
		for name, doc := range map[string]string{
			"unknown top-level key": "version: 1\n" + cred + ": x\n",
			"unknown rule key":      "version: 1\nentries:\n  - id: a\n    " + cred + ": [x]\n",
			"missing version":       "entries: []\n# " + cred + "\n",
			"version 2":             "version: 2\n# " + cred + "\n",
			"unparseable yaml":      "version: [" + cred + "\n",
			"wrong type":            "version: " + cred + "\n",
		} {
			al, rej, err := ParseAllowlist([]byte(doc), redaction.DefaultPatterns)
			if !errors.Is(err, ErrAllowlistInvalid) || al != nil || rej != nil {
				t.Errorf("%s: (%+v, %+v, %v), want ErrAllowlistInvalid", name, al, rej, err)
				continue
			}
			if strings.Contains(err.Error(), cred) || strings.Contains(err.Error(), doc) {
				t.Errorf("%s: error text quotes content: %v", name, err)
			}
		}
	})
	// One row per rejection reason. Each bad rule sits beside goodNeighbour:
	// the bad rule is dropped and the neighbour still loads.
	//
	// COUNTERFACTUAL (redaction_hit): make redactionHit return false. The
	// three redaction rows are otherwise VALID rules (slug id or a valid id,
	// a valid glob, a known pattern, a non-empty reason), so with the check
	// gone each LOADS and the "dropped" assertion goes RED.
	cases := []struct {
		name, rule, reason string
		wantID             string
	}{
		{"bad_id", rule("Bad_ID", "x/**", "github-pat-classic", "r"), RejectBadID, ""},
		{"duplicate_id", rule("neighbour", "y/**", "github-pat-classic", "r"), RejectDuplicateID, "neighbour"},
		{"empty_paths", "  - id: nopaths\n    paths: []\n    patterns: [\"github-pat-classic\"]\n    reason: r\n", RejectEmptyPaths, "nopaths"},
		{"bad_path", rule("badglob", "x/[", "github-pat-classic", "r"), RejectBadPath, "badglob"},
		{"empty_patterns", "  - id: nopat\n    paths: [\"x/**\"]\n    patterns: []\n    reason: r\n", RejectEmptyPatterns, "nopat"},
		{"unknown_pattern", rule("unk", "x/**", "not-a-pattern", "r"), RejectUnknownPattern, "unk"},
		{"empty_reason (whitespace)", rule("blank", "x/**", "github-pat-classic", "   "), RejectEmptyReason, "blank"},
		{"redaction_hit in glob", rule("glob-cred", "fixtures/"+classicPAT()+"/**", "github-pat-classic", "r"), RejectRedactionHit, ""},
		{"redaction_hit in id", rule(slugCredential(), "x/**", "github-pat-classic", "r"), RejectRedactionHit, ""},
		{"redaction_hit in reason", rule("reason-cred", "x/**", "github-pat-classic", "token "+classicPAT()), RejectRedactionHit, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, list := range []string{ListEntries, ListMarkers} {
				// The neighbour comes FIRST so a duplicate of it is the later row.
				doc := "version: 1\n" + list + ":\n" + goodNeighbour + c.rule
				al, rej, err := ParseAllowlist([]byte(doc), redaction.DefaultPatterns)
				if err != nil {
					t.Fatalf("%s: err = %v", list, err)
				}
				rules := al.Entries
				if list == ListMarkers {
					rules = al.Markers
				}
				if len(rules) != 1 || rules[0].ID != "neighbour" {
					t.Errorf("%s: accepted = %+v, want only the neighbour (bad rule dropped)", list, rules)
				}
				want := []Rejection{{List: list, Index: 1, Reason: c.reason, RuleID: c.wantID}}
				if !reflect.DeepEqual(rej, want) {
					t.Errorf("%s: rejected = %+v, want %+v", list, rej, want)
				}
			}
		})
	}
	t.Run("ids are unique across both lists", func(t *testing.T) {
		doc := "version: 1\nentries:\n" + goodNeighbour + "markers:\n" + goodNeighbour
		al, rej, err := ParseAllowlist([]byte(doc), redaction.DefaultPatterns)
		if err != nil || len(al.Entries) != 1 || len(al.Markers) != 0 ||
			!reflect.DeepEqual(rej, []Rejection{{List: ListMarkers, Index: 0, Reason: RejectDuplicateID, RuleID: "neighbour"}}) {
			t.Errorf("(%+v, %+v, %v)", al, rej, err)
		}
	})
	t.Run("pattern names are checked against the supplied set", func(t *testing.T) {
		doc := "version: 1\nentries:\n" + rule("a", "x/**", "github-pat-classic", "r")
		var reduced []redaction.Pattern
		for _, p := range redaction.DefaultPatterns {
			if p.Name != "github-pat-classic" {
				reduced = append(reduced, p)
			}
		}
		_, rej, _ := ParseAllowlist([]byte(doc), reduced)
		if len(rej) != 1 || rej[0].Reason != RejectUnknownPattern {
			t.Errorf("rejected = %+v, want unknown_pattern", rej)
		}
	})
}

func TestRuleFingerprint(t *testing.T) {
	// Golden: printf 'entries\0docs/*.md\0github-pat-classic' | sha256 | cut -c1-16.
	if got := RuleFingerprint(ListEntries, "docs/*.md", "github-pat-classic"); got != "fb5dd9d565bfba97" {
		t.Errorf("RuleFingerprint = %q", got)
	}
	base := RuleFingerprint(ListEntries, "docs/*.md", "github-pat-classic")
	for name, other := range map[string]string{
		"list":    RuleFingerprint(ListMarkers, "docs/*.md", "github-pat-classic"),
		"glob":    RuleFingerprint(ListEntries, "docs/**", "github-pat-classic"),
		"pattern": RuleFingerprint(ListEntries, "docs/*.md", "aws-access-key-id"),
		// The NUL separator: shifting a byte across a field boundary changes it.
		"boundary": RuleFingerprint(ListEntries, "docs/*.mdg", "ithub-pat-classic"),
	} {
		if other == base || !fingerprintPattern.MatchString(other) {
			t.Errorf("%s change: fingerprint %q (base %q)", name, other, base)
		}
	}
}

func TestSafeRuleID(t *testing.T) {
	for in, want := range map[string]string{
		"fixture-tests":  "fixture-tests",
		"Bad_ID":         InvalidRuleID,
		"":               InvalidRuleID,
		"a\nb":           InvalidRuleID,
		slugCredential(): InvalidRuleID,
		classicPAT():     InvalidRuleID,
	} {
		if got := SafeRuleID(in); got != want {
			t.Errorf("SafeRuleID(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SafeFingerprint("0123456789abcdef"); got != "0123456789abcdef" {
		t.Errorf("SafeFingerprint(hex) = %q", got)
	}
	for _, bad := range []string{"", "0123456789ABCDEF", "0123456789abcdef0", classicPAT()} {
		if got := SafeFingerprint(bad); got != InvalidFingerprint {
			t.Errorf("SafeFingerprint(%q) = %q", bad, got)
		}
	}
}

// TestAllowlist_Partition: one row per arm. Each guard row's fixture is a
// hit the guard alone keeps: every other rule in the allowlist WOULD match it.
func TestAllowlist_Partition(t *testing.T) {
	const pA, pB, pLow = "github-pat-classic", "aws-access-key-id", "json-password-field"
	lowScope := Rule{ID: "pw-s", Paths: []string{"fix/**"}, Patterns: []string{pLow}, Reason: "r"}
	lowEntry := Rule{ID: "pw-e", Paths: []string{"fix/p.go"}, Patterns: []string{pLow}, Reason: "r"}
	entry := Rule{ID: "docs-p", Paths: []string{"docs/p.md"}, Patterns: []string{pA}, Reason: "r"}
	scope := Rule{ID: "fix-s", Paths: []string{"fix/**"}, Patterns: []string{pA}, Reason: "r"}
	everything := Rule{ID: "all", Paths: []string{"**"}, Patterns: []string{pA}, Reason: "r"}
	fpEntry := RuleFingerprint(ListEntries, "docs/p.md", pA)
	fpScope := RuleFingerprint(ListMarkers, "fix/**", pA)
	cases := []struct {
		name string
		al   *Allowlist
		hit  Hit
		want *Suppression // nil = kept
	}{
		// (a)
		{"entry suppresses its pattern on its path", &Allowlist{Entries: []Rule{entry}},
			Hit{Path: "docs/p.md", Line: 1, Pattern: pA},
			&Suppression{By: ByEntry, RuleID: "docs-p", RuleFingerprint: fpEntry}},
		// (b) COUNTERFACTUAL: delete the containsString(pattern) check — the
		// B hit is on the entry's own path, so it is then suppressed, RED.
		{"entry keeps another pattern on its path", &Allowlist{Entries: []Rule{entry}},
			Hit{Path: "docs/p.md", Line: 1, Pattern: pB}, nil},
		// (c)
		{"entry keeps its pattern on another path", &Allowlist{Entries: []Rule{entry}},
			Hit{Path: "docs/q.md", Line: 1, Pattern: pA}, nil},
		// (d)
		{"scope suppresses a marked hit inside it", &Allowlist{Markers: []Rule{scope}},
			Hit{Path: "fix/a.go", Line: 2, Pattern: pA, Marked: true, Fingerprint: "00000000000000aa"},
			&Suppression{By: ByMarker, RuleID: "fix-s", RuleFingerprint: fpScope}},
		// (e) COUNTERFACTUAL: make the marker glob check always match — the
		// hit is marked and of the scope's pattern, so only the glob keeps it.
		{"scope keeps a marked hit outside every scope", &Allowlist{Markers: []Rule{scope}},
			Hit{Path: "src/a.go", Line: 2, Pattern: pA, Marked: true}, nil},
		// (f) COUNTERFACTUAL: delete the `if h.Marked` condition — the hit is
		// inside the scope and of its pattern, so only Marked keeps it.
		{"scope keeps an UNMARKED hit inside it", &Allowlist{Markers: []Rule{scope}},
			Hit{Path: "fix/a.go", Line: 2, Pattern: pA}, nil},
		// (g)
		{"scope keeps a marked hit of another pattern", &Allowlist{Markers: []Rule{scope}},
			Hit{Path: "fix/a.go", Line: 2, Pattern: pB, Marked: true}, nil},
		// (h) COUNTERFACTUAL: delete the UnresolvedPath guard — `**` matches
		// the placeholder, so the hit is suppressed, RED.
		{"unresolved path is never suppressed", &Allowlist{Entries: []Rule{everything}, Markers: []Rule{everything}},
			Hit{Path: UnresolvedPath, Line: 1, Pattern: pA, Marked: true}, nil},
		// (i) COUNTERFACTUAL: delete the PathRedacted guard — `**` matches,
		// RED.
		{"redacted path is never suppressed", &Allowlist{Entries: []Rule{everything}, Markers: []Rule{everything}},
			Hit{Path: "fixtures/[REDACTED:github-pat-classic].txt", Line: 1, Pattern: pA, Marked: true, PathRedacted: true}, nil},
		// (j) COUNTERFACTUAL: delete the AllowlistPath self-exclusion — `**`
		// matches the allowlist file, RED.
		{"the allowlist file itself is never suppressed", &Allowlist{Entries: []Rule{everything}, Markers: []Rule{everything}},
			Hit{Path: AllowlistPath, Line: 1, Pattern: pA, Marked: true}, nil},
		// (m) COUNTERFACTUAL: delete the PathAmbiguous guard — `**` matches,
		// RED.
		{"ambiguous path is never suppressed", &Allowlist{Entries: []Rule{everything}, Markers: []Rule{everything}},
			Hit{Path: "docs/p.md", Line: 1, Pattern: pA, Marked: true, PathAmbiguous: true}, nil},
		// (k) COUNTERFACTUAL: check markers before entries — the hit is
		// covered by both, so it then reads By=marker, RED.
		{"entry wins over a scope covering the same marked hit",
			&Allowlist{Entries: []Rule{{ID: "fix-e", Paths: []string{"fix/**"}, Patterns: []string{pA}, Reason: "r"}}, Markers: []Rule{scope}},
			Hit{Path: "fix/a.go", Line: 2, Pattern: pA, Marked: true},
			&Suppression{By: ByEntry, RuleID: "fix-e", RuleFingerprint: RuleFingerprint(ListEntries, "fix/**", pA)}},
		// (n) COUNTERFACTUAL: revert the Partition marker condition to
		// `if h.Marked {` — the hit is marked, inside the scope and of its class
		// (its fingerprint is preset, so only this guard can keep it), RED.
		{"scope keeps a marked LOW-ENTROPY hit inside a scope naming its class", &Allowlist{Markers: []Rule{lowScope}},
			Hit{Path: "fix/p.go", Line: 2, Pattern: pLow, Marked: true, Fingerprint: "00000000000000bb"}, nil},
		// (o) the human-reviewed route still suppresses a low-entropy class.
		{"entry still suppresses a low-entropy class", &Allowlist{Entries: []Rule{lowEntry}},
			Hit{Path: "fix/p.go", Line: 2, Pattern: pLow, Marked: true},
			&Suppression{By: ByEntry, RuleID: "pw-e", RuleFingerprint: RuleFingerprint(ListEntries, "fix/p.go", pLow)}},
		// (l)
		{"nil allowlist keeps everything", nil, Hit{Path: "docs/p.md", Line: 1, Pattern: pA, Marked: true}, nil},
		{"empty allowlist keeps everything", &Allowlist{}, Hit{Path: "docs/p.md", Line: 1, Pattern: pA, Marked: true}, nil},
		{"first matching glob in file order names the fingerprint",
			&Allowlist{Entries: []Rule{{ID: "two", Paths: []string{"docs/*.md", "docs/**"}, Patterns: []string{pA}, Reason: "r"}}},
			Hit{Path: "docs/p.md", Line: 1, Pattern: pA},
			&Suppression{By: ByEntry, RuleID: "two", RuleFingerprint: RuleFingerprint(ListEntries, "docs/*.md", pA)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kept, sup := c.al.Partition([]Hit{c.hit})
			if c.want == nil {
				if len(sup) != 0 || !reflect.DeepEqual(kept, []Hit{c.hit}) {
					t.Fatalf("kept=%+v suppressed=%+v, want the hit kept", kept, sup)
				}
				return
			}
			want := *c.want
			want.Hit = c.hit
			if len(kept) != 0 || !reflect.DeepEqual(sup, []Suppression{want}) {
				t.Fatalf("kept=%+v suppressed=%+v, want %+v", kept, sup, want)
			}
		})
	}
}

// jsonPasswordMatches returns every json-password-field match on line, the
// bytes hitFingerprint would have hashed.
func jsonPasswordMatches(t *testing.T, line string) []string {
	t.Helper()
	for _, p := range redaction.DefaultPatterns {
		if p.Name == "json-password-field" {
			return p.Regex.FindAllString(line, -1)
		}
	}
	t.Fatal("json-password-field is not a DefaultPatterns class")
	return nil
}

// TestMarker_LowEntropyFixtureLineRaises is the in-package end-to-end of the
// #5462 ruling: a markers rule naming json-password-field parses, yet a marked
// line carrying a guessable password value inside its scope is KEPT (so the
// server raises it high), is never fingerprinted, and leaves no suppression
// record or check key. COUNTERFACTUAL: make markerSuppressible return true —
// the hit is then marker-suppressed with a per-hit fingerprint, so kept is empty
// and the summary carries a check key over the guessable value's digest, RED.
func TestMarker_LowEntropyFixtureLineRaises(t *testing.T) {
	doc := "version: 1\nmarkers:\n" + rule("pw-fixtures", "fix/**", "json-password-field", "synthetic fixtures")
	al, rej, err := ParseAllowlist([]byte(doc), redaction.DefaultPatterns)
	if err != nil || len(rej) != 0 || len(al.Markers) != 1 {
		t.Fatalf("rule must still parse (inert for markers): err=%v rej=%+v al=%+v", err, rej, al)
	}
	line := passwordLine() + " // " + MarkerToken
	hits := Scan(section("fix/p.go", "@@ -0,0 +1 @@\n+"+line+"\n"), redaction.DefaultPatterns).Hits
	kept, sup := al.Partition(hits)
	if len(sup) != 0 {
		t.Errorf("suppressed = %+v, want none", sup)
	}
	if len(kept) != 1 || kept[0].Pattern != "json-password-field" || kept[0].Path != "fix/p.go" || !kept[0].Marked || kept[0].Fingerprint != "" {
		t.Fatalf("kept = %+v, want the one marked, unfingerprinted json-password-field hit", kept)
	}
	sum := NewSuppressionSummary(&AllowlistStatusRecord{Status: StatusLoaded}, sup)
	raw, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["marker_suppressed_hit_count"] != float64(0) || len(m["suppressed"].([]any)) != 0 {
		t.Errorf("summary = %s, want marker_suppressed_hit_count 0 and no records", raw)
	}
	for _, r := range GroupSuppressions(sup) {
		if r.CheckKey != "" {
			t.Errorf("record carries check_key %q", r.CheckKey)
		}
	}
	digest := hitFingerprint("fix/p.go", "json-password-field", jsonPasswordMatches(t, line))
	if strings.Contains(string(raw), digest) {
		t.Errorf("summary carries the digest %s of the guessable value", digest)
	}
}

// TestMarker_DualHitLineKeepsLowEntropyHit (#5462, N2): ONE marked line can
// match a high-entropy class and a low-entropy class together — a JSON `token`
// key holding a ghp_ value, or a bearer header holding one. The
// scope names both classes, yet only the high-entropy hit is marker-suppressed;
// the low-entropy hit on the same line is KEPT (raising high), so a marker no
// longer fully quiets the line. The remedy is an `entries` rule, shown last.
// COUNTERFACTUAL: make markerSuppressible return true — both hits are then
// suppressed and kept is empty, RED.
func TestMarker_DualHitLineKeepsLowEntropyHit(t *testing.T) {
	for _, c := range []struct{ name, line, low string }{
		{"json token key holding a PAT", jsonTokenLine(), "json-password-field"},
		{"bearer header holding a PAT", bearerPATLine(), "authorization-bearer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			scope := Rule{ID: "fix-s", Paths: []string{"fix/**"}, Patterns: []string{"github-pat-classic", c.low}, Reason: "r"}
			hits := Scan(section("fix/p.go", "@@ -0,0 +1 @@\n+"+c.line+" // "+MarkerToken+"\n"), redaction.DefaultPatterns).Hits
			if len(hits) != 2 || hits[0].Pattern != "github-pat-classic" || hits[1].Pattern != c.low {
				t.Fatalf("hits = %+v, want github-pat-classic then %s", hits, c.low)
			}
			kept, sup := (&Allowlist{Markers: []Rule{scope}}).Partition(hits)
			if len(sup) != 1 || sup[0].By != ByMarker || sup[0].Hit.Pattern != "github-pat-classic" || !fingerprintPattern.MatchString(sup[0].Hit.Fingerprint) {
				t.Errorf("suppressed = %+v, want only the marker-suppressed github-pat-classic hit", sup)
			}
			if len(kept) != 1 || kept[0].Pattern != c.low || kept[0].Fingerprint != "" {
				t.Fatalf("kept = %+v, want only the unfingerprinted %s hit", kept, c.low)
			}
			// The remedy: an entries rule names the low-entropy class on that path.
			entry := Rule{ID: "fix-e", Paths: []string{"fix/p.go"}, Patterns: []string{c.low}, Reason: "r"}
			kept, _ = (&Allowlist{Entries: []Rule{entry}, Markers: []Rule{scope}}).Partition(hits)
			if len(kept) != 0 {
				t.Errorf("with an entries rule kept = %+v, want the whole line quiet", kept)
			}
		})
	}
}

func TestGroupSuppressions(t *testing.T) {
	const p = "github-pat-classic"
	sup := []Suppression{
		{Hit: Hit{Path: "fix/a.go", Line: 9, Pattern: p, Marked: true, Fingerprint: "bbbbbbbbbbbbbbbb"}, By: ByMarker, RuleID: "s", RuleFingerprint: "1111111111111111"},
		{Hit: Hit{Path: "docs/p.md", Line: 7, Pattern: p}, By: ByEntry, RuleID: "e", RuleFingerprint: "2222222222222222"},
		{Hit: Hit{Path: "docs/p.md", Line: 3, Pattern: p}, By: ByEntry, RuleID: "e", RuleFingerprint: "2222222222222222"},
		{Hit: Hit{Path: "docs/p.md", Line: 7, Pattern: p}, By: ByEntry, RuleID: "e", RuleFingerprint: "2222222222222222"},
		// Same marked bytes on two lines fold; different bytes do not (K1).
		{Hit: Hit{Path: "fix/a.go", Line: 2, Pattern: p, Marked: true, Fingerprint: "aaaaaaaaaaaaaaaa"}, By: ByMarker, RuleID: "s", RuleFingerprint: "1111111111111111"},
		{Hit: Hit{Path: "fix/a.go", Line: 4, Pattern: p, Marked: true, Fingerprint: "aaaaaaaaaaaaaaaa"}, By: ByMarker, RuleID: "s", RuleFingerprint: "1111111111111111"},
	}
	want := []SuppressionRecord{
		{Path: "docs/p.md", Lines: []int{3, 7}, Pattern: p, By: ByEntry, RuleID: "e", RuleFingerprint: "2222222222222222"},
		{Path: "fix/a.go", Lines: []int{2, 4}, Pattern: p, By: ByMarker, RuleID: "s", RuleFingerprint: "1111111111111111",
			CheckKey: MarkerCheckKey(p, "s", "1111111111111111", "aaaaaaaaaaaaaaaa", "fix/a.go")},
		{Path: "fix/a.go", Lines: []int{9}, Pattern: p, By: ByMarker, RuleID: "s", RuleFingerprint: "1111111111111111",
			CheckKey: MarkerCheckKey(p, "s", "1111111111111111", "bbbbbbbbbbbbbbbb", "fix/a.go")},
	}
	if got := GroupSuppressions(sup); !reflect.DeepEqual(got, want) {
		t.Fatalf("records = %+v\nwant      %+v", got, want)
	}
	if got := GroupSuppressions(nil); got == nil || len(got) != 0 {
		t.Errorf("GroupSuppressions(nil) = %#v, want a non-nil empty slice", got)
	}
	s := NewSuppressionSummary(&AllowlistStatusRecord{Status: StatusLoaded}, sup)
	if s.SuppressedHitCount != 6 || s.EntrySuppressedHitCount != 3 || s.MarkerSuppressedHitCount != 3 || len(s.Suppressed) != 3 {
		t.Errorf("summary counts = %+v", s)
	}
}

// TestMarkerCheckKey_Injective extends TestCheckKey_Injective: distinct
// (pattern, rule id, rule fingerprint, hit fingerprint, path) tuples never
// collide, a '|' in the path cannot forge a field, and the prefix never
// equals a CheckKey's.
func TestMarkerCheckKey_Injective(t *testing.T) {
	seen := map[string]string{}
	for _, p := range redaction.DefaultPatterns {
		for _, id := range []string{"a", "a-b"} {
			for _, rfp := range []string{"1111111111111111", "2222222222222222"} {
				for _, hfp := range []string{"aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"} {
					for _, path := range []string{"a.go", "a|b.go", "|", "x|aaaaaaaaaaaaaaaa|y"} {
						k := MarkerCheckKey(p.Name, id, rfp, hfp, path)
						tuple := strings.Join([]string{p.Name, id, rfp, hfp, path}, "\x00")
						if prev, dup := seen[k]; dup && prev != tuple {
							t.Fatalf("collision %q: %q vs %q", k, prev, tuple)
						}
						seen[k] = tuple
						if strings.HasPrefix(k, CheckName+"|") || k == CheckKey(p.Name, path) {
							t.Errorf("marker key %q shares the group key namespace", k)
						}
					}
				}
			}
		}
	}
}

// TestSuppressionSummary_JSONShape pins the json tag names every reader uses.
func TestSuppressionSummary_JSONShape(t *testing.T) {
	s := SuppressionSummary{
		Allowlist: &AllowlistStatusRecord{Path: AllowlistPath, Ref: "abc", Status: StatusLoaded, Detail: "d", RejectedCount: 1,
			Rejected: []RejectionRecord{{List: ListEntries, Index: 2, Reason: RejectBadID, RuleID: "x"}}},
		SuppressedHitCount: 1, EntrySuppressedHitCount: 1, MarkerSuppressedHitCount: 0,
		Suppressed: []SuppressionRecord{{Path: "p", Lines: []int{1}, Pattern: "q", By: ByMarker, RuleID: "r", RuleFingerprint: "f", CheckKey: "k"}},
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	keys := func(v map[string]any) []string {
		var out []string
		for k := range v {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	if got := keys(m); !reflect.DeepEqual(got, []string{"allowlist", "entry_suppressed_hit_count", "marker_suppressed_hit_count", "suppressed", "suppressed_hit_count"}) {
		t.Errorf("summary keys = %v", got)
	}
	al := m["allowlist"].(map[string]any)
	if got := keys(al); !reflect.DeepEqual(got, []string{"detail", "path", "ref", "rejected", "rejected_count", "status"}) {
		t.Errorf("allowlist keys = %v", got)
	}
	if got := keys(al["rejected"].([]any)[0].(map[string]any)); !reflect.DeepEqual(got, []string{"index", "list", "reason", "rule_id"}) {
		t.Errorf("rejection keys = %v", got)
	}
	if got := keys(m["suppressed"].([]any)[0].(map[string]any)); !reflect.DeepEqual(got, []string{"by", "check_key", "lines", "path", "pattern", "rule_fingerprint", "rule_id"}) {
		t.Errorf("record keys = %v", got)
	}
}

func TestRejectionRecords(t *testing.T) {
	var rej []Rejection
	for i := 0; i < MaxRejectedRecords+5; i++ {
		rej = append(rej, Rejection{List: ListEntries, Index: i, Reason: RejectEmptyReason, RuleID: "ok"})
	}
	// A corrupted RuleID (a slug-shaped credential) is dropped, not echoed.
	rej[0].RuleID = slugCredential()
	got := RejectionRecords(rej)
	if len(got) != MaxRejectedRecords {
		t.Fatalf("records = %d, want cap %d", len(got), MaxRejectedRecords)
	}
	if got[0].RuleID != "" || got[1].RuleID != "ok" {
		t.Errorf("rule ids = %q, %q", got[0].RuleID, got[1].RuleID)
	}
}
