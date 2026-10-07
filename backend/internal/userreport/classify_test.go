package userreport

import (
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// intakeBody is a body carrying the REAL intake marker, rendered by the
// producer rather than spelled as a literal.
func intakeBody(t *testing.T) string {
	t.Helper()
	body := intakegroom.RenderBody("Filed by Fishhawk.", intakegroom.Signals{ScannedItems: 3})
	if !strings.Contains(body, intakegroom.MarkerPrefix) {
		t.Fatalf("intakegroom.RenderBody output carries no marker: %q", body)
	}
	return body
}

func author(login, assoc string, internal, bot bool) workmgmt.ReportAuthor {
	return workmgmt.ReportAuthor{Login: login, Association: assoc, AssociationResolved: true, Internal: internal, Bot: bot}
}

// TestClassify_FixtureRepository is the issue's fixture repository: a
// maintainer issue, an outsider issue, a Fishhawk-filed issue and a bot
// comment each land in their class.
func TestClassify_FixtureRepository(t *testing.T) {
	cases := []struct {
		name      string
		item      workmgmt.UserReportItem
		wantClass Classification
		wantBasis string
	}{
		{"maintainer OWNER issue", workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, Author: author("maint", "OWNER", true, false), Body: "bug"}, ClassInternal, "association:OWNER"},
		{"outsider NONE issue", workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, Author: author("rando", "NONE", false, false), Body: "please fix"}, ClassExternal, "association:NONE"},
		{"bot-authored issue with intake marker", workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, Author: author("fishhawk-dev[bot]", "NONE", false, true), Body: intakeBody(t)}, ClassFishhawkFiled, "marker:fishhawk-intake:v1"},
		{"dependabot comment", workmgmt.UserReportItem{Kind: workmgmt.UserReportKindComment, Author: author("dependabot[bot]", "NONE", false, true), Body: "bump"}, ClassBot, BasisBot},
		{"unresolved association", workmgmt.UserReportItem{Author: workmgmt.ReportAuthor{Login: "x"}}, ClassExternal, BasisAssociationUnresolved},
		{"resolved with empty association", workmgmt.UserReportItem{Author: workmgmt.ReportAuthor{Login: "x", AssociationResolved: true}}, ClassExternal, BasisDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.item, ClassifyContext{})
			if got.Class != tc.wantClass || got.Basis != tc.wantBasis || got.MarkerFromExternal {
				t.Errorf("Classify = %+v, want class %q basis %q MarkerFromExternal false", got, tc.wantClass, tc.wantBasis)
			}
		})
	}
}

// TestClassify_ForgedMarkerFromExternalStaysExternal: an outsider's body
// carrying the intake marker is NOT fishhawk_filed — the marker is
// attacker-writable — and is flagged. Mechanism: without the
// author-not-external guard the marker alone yields fishhawk_filed.
func TestClassify_ForgedMarkerFromExternalStaysExternal(t *testing.T) {
	cases := map[string]workmgmt.ReportAuthor{
		"NONE association":           author("rando", "NONE", false, false),
		"unresolved association":     {Login: "rando"},
		"gitlab non-member bot name": {Login: "project_1_bot_x", Association: "non_member", AssociationResolved: true},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			got := Classify(workmgmt.UserReportItem{Author: a, Body: "hi\n" + intakegroom.MarkerPrefix + "{} -->"}, ClassifyContext{})
			if got.Class != ClassExternal || !got.MarkerFromExternal {
				t.Errorf("Classify = %+v, want external with MarkerFromExternal", got)
			}
		})
	}
}

// TestClassify_FishhawkFiledTakesPrecedenceOverBotAndInternal: a marker on a
// bot or internal author's item classifies fishhawk_filed.
func TestClassify_FishhawkFiledTakesPrecedenceOverBotAndInternal(t *testing.T) {
	for name, a := range map[string]workmgmt.ReportAuthor{
		"bot":      author("app[bot]", "NONE", false, true),
		"internal": author("maint", "MEMBER", true, false),
	} {
		t.Run(name, func(t *testing.T) {
			got := Classify(workmgmt.UserReportItem{Author: a, Body: upkeep.FindingMarker("flake:TestX")}, ClassifyContext{})
			if got.Class != ClassFishhawkFiled || got.Basis != "marker:fishhawk-upkeep:v1" {
				t.Errorf("Classify = %+v, want fishhawk_filed via the upkeep marker", got)
			}
		})
	}
	// Bot outranks internal when both flags are set.
	if got := Classify(workmgmt.UserReportItem{Author: author("app[bot]", "MEMBER", true, true)}, ClassifyContext{}); got.Class != ClassBot {
		t.Errorf("bot+internal author = %+v, want bot", got)
	}
}

// TestClassify_CommsMarker pins the comms marker's classification (E81.5 /
// #3775): an internal author's comms-marked draft is fishhawk_filed via the
// comms marker, and an external author's forged comms marker stays external
// with MarkerFromExternal — the marker is attacker-writable body text.
// Mechanism: without the recognisedMarkers entry the internal row classifies
// internal, and the forged row loses its MarkerFromExternal flag.
func TestClassify_CommsMarker(t *testing.T) {
	marker := DraftMarker([]MarkedReport{{ID: "UR-comment-4-9", ContentHash: strings.Repeat("b", 64)}})
	if marker == "" {
		t.Fatal("DraftMarker rendered nothing for a valid entry")
	}
	body := "Thanks for the report.\n\n" + marker

	got := Classify(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindComment, Author: author("maint", "MEMBER", true, false), Body: body}, ClassifyContext{})
	if got.Class != ClassFishhawkFiled || got.Basis != "marker:fishhawk-comms:v1" || got.MarkerFromExternal {
		t.Errorf("internal author comms marker: Classify = %+v, want fishhawk_filed via marker:fishhawk-comms:v1", got)
	}

	forged := Classify(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindComment, Author: author("rando", "NONE", false, false), Body: body}, ClassifyContext{})
	if forged.Class != ClassExternal || !forged.MarkerFromExternal {
		t.Errorf("external author comms marker: Classify = %+v, want external with MarkerFromExternal", forged)
	}
}

// TestClassify_SystemNoteNeverFishhawkFiled (#3771 concern 13c665e0): a
// system note is forge-rendered around actor-controlled text, so a marker in
// it never earns fishhawk_filed — it stays bot, with MarkerFromExternal set —
// even when its author is a bot or internal. Without a marker it is plain
// bot. Counterfactual: dropping the System arm classifies the marker-bearing
// rows fishhawk_filed.
func TestClassify_SystemNoteNeverFishhawkFiled(t *testing.T) {
	marker := "changed title from **x** to **" + intakegroom.MarkerPrefix + "{} -->**"
	for name, a := range map[string]workmgmt.ReportAuthor{
		"bot author":      author("alice", "access_level:30", true, true),
		"internal author": author("maint", "access_level:40", true, false),
	} {
		t.Run(name, func(t *testing.T) {
			got := Classify(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindComment, System: true, Author: a, Body: marker}, ClassifyContext{})
			if got != (Result{Class: ClassBot, Basis: BasisSystemNote, MarkerFromExternal: true}) {
				t.Errorf("Classify = %+v, want bot via system_note with MarkerFromExternal", got)
			}
		})
	}
	plain := Classify(workmgmt.UserReportItem{System: true, Author: author("maint", "access_level:40", true, false), Body: "closed"}, ClassifyContext{})
	if plain != (Result{Class: ClassBot, Basis: BasisSystemNote}) {
		t.Errorf("markerless system note = %+v, want bot via system_note", plain)
	}
}

// TestClassify_CaptainLoginIsInternal: an association-NONE author whose login
// equals the captain's (case-insensitively) is internal. Deleting the captain
// arm leaves it external.
func TestClassify_CaptainLoginIsInternal(t *testing.T) {
	it := workmgmt.UserReportItem{Author: author("Alice", "NONE", false, false)}
	if got := Classify(it, ClassifyContext{CaptainLogin: "alice"}); got.Class != ClassInternal || got.Basis != BasisCaptain {
		t.Errorf("captain-authored = %+v, want internal via captain", got)
	}
	if got := Classify(it, ClassifyContext{CaptainLogin: "bob"}); got.Class != ClassExternal {
		t.Errorf("non-captain = %+v, want external", got)
	}
	// An empty login never matches an empty captain login.
	if got := Classify(workmgmt.UserReportItem{Author: workmgmt.ReportAuthor{AssociationResolved: true}}, ClassifyContext{}); got.Class != ClassExternal {
		t.Errorf("empty login = %+v, want external", got)
	}
}

// TestCaptainLoginFor_StaticOrOtherForgeSubjectDoesNotMatch: only a subject
// provider-qualified for the page's own forge yields a login.
func TestCaptainLoginFor_StaticOrOtherForgeSubjectDoesNotMatch(t *testing.T) {
	cases := []struct {
		subject, forge, want string
	}{
		{"github:alice", workmgmt.UserReportForgeGitHub, "alice"},
		{"gitlab:alice", workmgmt.UserReportForgeGitLab, "alice"},
		{"gitlab:alice", workmgmt.UserReportForgeGitHub, ""},
		{"github:alice", workmgmt.UserReportForgeGitLab, ""},
		{"brett@local-mcp", workmgmt.UserReportForgeGitHub, ""},
		{"github:", workmgmt.UserReportForgeGitHub, ""},
		{"github:alice", "", ""},
	}
	for _, tc := range cases {
		if got := CaptainLoginFor(tc.subject, tc.forge); got != tc.want {
			t.Errorf("CaptainLoginFor(%q, %q) = %q, want %q", tc.subject, tc.forge, got, tc.want)
		}
	}
}

// TestClassify_MarkerPrefixesMatchProducers pins the recognised-marker list to
// its producers: intake through intakegroom.RenderBody, upkeep through
// upkeep.FindingMarker, and the two literal-only markers by their documented
// shapes.
func TestClassify_MarkerPrefixesMatchProducers(t *testing.T) {
	cases := map[string]string{
		"fishhawk-intake:v1":   intakeBody(t),
		"fishhawk-upkeep:v1":   upkeep.FindingMarker("flake:TestA"),
		"fishhawk-fingerprint": "<!-- fishhawk-fingerprint:abc123 -->",
		"fishhawk-sticky":      "<!-- fishhawk-sticky locus=anchor run=00000000-0000-0000-0000-000000000000 -->",
		"fishhawk-comms:v1":    DraftMarker([]MarkedReport{{ID: "UR-issue-1", ContentHash: strings.Repeat("a", 64)}}),
	}
	if len(cases) != len(recognisedMarkers) {
		t.Fatalf("recognisedMarkers has %d entries, this test covers %d — add the new marker's producer here", len(recognisedMarkers), len(cases))
	}
	for name, body := range cases {
		if got := markerIn(body); got != name {
			t.Errorf("markerIn(%q) = %q, want %q", body, got, name)
		}
	}
	if got := markerIn("<!-- some-other-tool:v1 -->"); got != "" {
		t.Errorf("markerIn(foreign marker) = %q, want none", got)
	}
}
