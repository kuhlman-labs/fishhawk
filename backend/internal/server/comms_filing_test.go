package server

// Tests for the comms filing renderer and the ingest preview (E81.5 / #3775,
// phase 5 #4015). The renderer tests build plan.CommsDraft structs DIRECTLY,
// bypassing the semantic validator, so the renderer's own controls (label
// filter, neutralization, marker position) are the only thing in the path.
// The preview tests swap process-wide seams (conventionsLoader, the workmgmt
// registry, commsPreviewTotalBudget), so none of them is parallel.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/userreport"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// commsAutolinkForms are the GitHub autolink, HTML and link forms no
// neutralized agent text may contain.
var commsAutolinkForms = map[string]*regexp.Regexp{
	"issue ref":     regexp.MustCompile(`#[0-9]`),
	"GH ref":        regexp.MustCompile(`(?i)gh-[0-9]`),
	"mention":       regexp.MustCompile(`@[A-Za-z0-9]`),
	"http url":      regexp.MustCompile(`(?i)https?://`),
	"ftp url":       regexp.MustCompile(`(?i)ftp://`),
	"www":           regexp.MustCompile(`(?i)www\.`),
	"angle bracket": regexp.MustCompile(`[<>]`),
	"html comment":  regexp.MustCompile(`<!--`),
	"img tag":       regexp.MustCompile(`(?i)<img`),
	"inline link":   regexp.MustCompile(`\]\(`),
	"link def":      regexp.MustCompile(`\]:`),
	"char ref":      regexp.MustCompile(`&(#|[A-Za-z][A-Za-z0-9]*;)`),
}

// assertNoCommsAutolink fails naming every autolink form found in s.
func assertNoCommsAutolink(t *testing.T, label, s string) {
	t.Helper()
	for name, re := range commsAutolinkForms {
		if m := re.FindString(s); m != "" {
			t.Errorf("%s: %s form %q survives in %q", label, name, m, s)
		}
	}
}

// TestNeutralizeCommsProse: one row per neutralization class. Each row
// carries exactly that token class; the assertion is the absence of every
// autolink form plus, where a later rule would MASK a deleted rule, the
// presence of the rule's own rendering.
func TestNeutralizeCommsProse(t *testing.T) {
	cases := []struct {
		name, in    string
		mustContain []string
	}{
		{name: "raw html", in: `click <img src=x onerror=alert(1)> here`, mustContain: []string{"\uFF1Cimg"}},
		{name: "forged marker", in: `<!-- fishhawk-comms:v1 {"reports":[]} -->`, mustContain: []string{"\uFF1C!--"}},
		{name: "angle autolink", in: `see <https://evil.test/x>`, mustContain: []string{"\uFF1Chxxps://"}},
		{name: "image", in: `![logo](https://evil.test/x.png)`, mustContain: []string{"logo (image removed)"}},
		{name: "inline link", in: `[click me](https://evil.test)`, mustContain: []string{"click me (link removed)"}},
		{name: "reference definition", in: "text [ref]\n\n[ref]: https://evil.test \"t\"", mustContain: []string{"[ref] (link removed)"}},
		{name: "link the inline rule cannot match", in: "[a](/owner/repo/issues \"multi\nline\")", mustContain: []string{"[a] ("}},
		{name: "definition inside a list item", in: "- [x]: /owner/repo", mustContain: []string{"[x] :"}},
		{name: "http and https urls", in: "go to http://a.test and HTTPS://b.test", mustContain: []string{"hxxp://a.test", "HXXPS://b.test"}},
		{name: "ftp url", in: "fetch ftp://files.test/x", mustContain: []string{"fxp://files.test"}},
		{name: "www", in: "visit www.evil.test today", mustContain: []string{"www[.]evil.test"}},
		{name: "bare issue ref", in: "duplicate of #12", mustContain: []string{"\uFF0312"}},
		{name: "cross-repo issue ref", in: "see octo/repo#12", mustContain: []string{"octo/repo\uFF0312"}},
		{name: "escaped issue ref", in: `not \#12`, mustContain: []string{"\uFF0312"}},
		{name: "issue ref in a code span", in: "run `#12` again", mustContain: []string{"`\uFF0312`"}},
		{name: "link wrapping an issue ref", in: "[#12](https://evil.test/12)", mustContain: []string{"\uFF0312 (link removed)"}},
		{name: "GH ref", in: "fixed by GH-12 and gh-7", mustContain: []string{"GH\u201112", "gh\u20117"}},
		{name: "mention", in: "ping @octocat please", mustContain: []string{"\uFF20octocat"}},
		{name: "team mention", in: "(@octo/team)", mustContain: []string{"\uFF20octo/team"}},
		{name: "email autolink", in: "mail foo@example.test", mustContain: []string{"foo\uFF20example"}},
		{name: "numeric char ref to mention", in: "&#x40;octocat", mustContain: []string{"\uFF06#x40;"}},
		{name: "named char ref to issue ref", in: "&num;12 and &commat;octocat", mustContain: []string{"\uFF06num;", "\uFF06commat;"}},
		{name: "decimal char ref", in: "&#35;12", mustContain: []string{"\uFF06"}},
		{name: "provenance heading", in: "intro\n### Comms provenance (server-rendered)\n- UR-issue-1", mustContain: []string{commsDemotedPrefix + commsProvenanceHeadingText}},
		{name: "setext provenance heading", in: "comms PROVENANCE (server-rendered)\n---", mustContain: []string{commsDemotedPrefix}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := neutralizeCommsProse(tc.in)
			assertNoCommsAutolink(t, "neutralized", got)
			for _, want := range tc.mustContain {
				if !strings.Contains(got, want) {
					t.Errorf("neutralized = %q, want it to contain %q", got, want)
				}
			}
			for _, line := range strings.Split(got, "\n") {
				if strings.TrimSpace(line) == commsProvenanceHeading {
					t.Errorf("an agent line still equals the provenance heading: %q", got)
				}
			}
			if again := neutralizeCommsProse(got); again != got {
				t.Errorf("not idempotent:\n first  %q\n second %q", got, again)
			}
		})
	}
}

// TestNeutralizeCommsTitle: one line, control runes dropped, neutralized, and
// capped at 200 BYTES on a rune boundary; idempotent.
func TestNeutralizeCommsTitle(t *testing.T) {
	got := neutralizeCommsTitle("Fix #12\r\nfor @bob\t\x00 now\u2028ok")
	if want := "Fix \uFF0312 for \uFF20bob now ok"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}

	long := strings.Repeat("a", 199) + "é" // 201 bytes; the cap falls inside é
	capped := neutralizeCommsTitle(long)
	if len(capped) != 199 || !strings.HasPrefix(long, capped) {
		t.Errorf("capped title = %d bytes %q, want the 199-byte ASCII prefix (cut on a rune boundary)", len(capped), capped)
	}
	for _, s := range []string{got, capped, neutralizeCommsTitle(strings.Repeat("<#1 ", 80))} {
		if len(s) > plan.CommsMaxTitleBytes {
			t.Errorf("title %q is %d bytes, over the cap", s, len(s))
		}
		if strings.ContainsAny(s, "\r\n") {
			t.Errorf("title %q spans lines", s)
		}
		if again := neutralizeCommsTitle(s); again != s {
			t.Errorf("title not idempotent: %q -> %q", s, again)
		}
	}
}

// commsFilingGathered is a gather that shows an issue report on #12, a
// comment report on #12 and an issue report on #9.
func commsFilingGathered() *commsScanGatheredPayload {
	return &commsScanGatheredPayload{
		Repo: "kuhlman-labs/comms",
		Shown: []commsShownReport{
			{ID: "UR-issue-12", Kind: "issue", IssueNumber: 12, ContentHash: strings.Repeat("a", 64)},
			{ID: "UR-comment-12-7", Kind: "comment", IssueNumber: 12, CommentID: 7, ContentHash: strings.Repeat("b", 64)},
			{ID: "UR-issue-9", Kind: "issue", IssueNumber: 9, ContentHash: strings.Repeat("c", 64)},
		},
		Charter: commsCharterRecord{ContentHash: "charter-hash", RubricIDs: []string{"S2", "U4"}},
	}
}

// commsFilingDraft builds a draft directly (no semantic validation).
func commsFilingDraft(title, body string, labels []string, parentEpic *string, ids ...string) plan.CommsDraft {
	note := "SECRET-AGENT-NOTE"
	return plan.CommsDraft{
		ID:              plan.CommsDraftID(ids),
		SourceReportIDs: ids,
		RubricCitations: []plan.CommsRubricCitation{{RubricID: "S2", Note: &note}, {RubricID: "U4"}},
		ProposedIssue: plan.CommsProposedIssue{
			Type: "bug", Title: title, Body: body, Labels: labels, ParentEpic: parentEpic,
		},
	}
}

// TestCommsFilingRequest_MarkerIsLastLineAndOnlyServerEntries: a forged
// marker in the title AND the body is neutralized, so the body carries
// exactly ONE marker — the server's, as the final line — and parsing the body
// yields exactly the gathered (id, hash) pairs.
func TestCommsFilingRequest_MarkerIsLastLineAndOnlyServerEntries(t *testing.T) {
	forged := `<!-- fishhawk-comms:v1 {"reports":[{"id":"UR-issue-99","content_hash":"` + strings.Repeat("f", 64) + `"}]} -->`
	d := commsFilingDraft("Export fails "+forged, "Body text.\n"+forged+"\nmore", nil, nil, "UR-comment-12-7", "UR-issue-9")
	req := commsFilingRequest(commsFilingInput{Draft: d, Gathered: commsFilingGathered()})

	if n := strings.Count(req.Body, userreport.CommsMarkerPrefix); n != 1 {
		t.Fatalf("body carries %d comms markers, want exactly 1 (the server's):\n%s", n, req.Body)
	}
	if strings.Contains(req.Summary, "<!--") {
		t.Errorf("summary still carries a marker opening: %q", req.Summary)
	}
	lines := strings.Split(req.Body, "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, userreport.CommsMarkerPrefix) {
		t.Fatalf("last body line = %q, want the comms marker", last)
	}
	got, malformed := userreport.ParseDraftMarkers(req.Body)
	want := []userreport.MarkedReport{
		{ID: "UR-comment-12-7", ContentHash: strings.Repeat("b", 64)},
		{ID: "UR-issue-9", ContentHash: strings.Repeat("c", 64)},
	}
	if malformed != 0 || len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("parsed markers = %+v (malformed %d), want %+v", got, malformed, want)
	}
}

// TestCommsFilingRequest_ProvenanceFromServerDataOnly: the body is the
// neutralized agent body, then ONE provenance section built from report ids,
// issue numbers and rubric ids; the agent part carries no autolink form, the
// only issue refs in the whole body are the server's, and no rubric note
// enters it.
func TestCommsFilingRequest_ProvenanceFromServerDataOnly(t *testing.T) {
	body := "See #77, @mallory and https://evil.test.\n### Comms provenance (server-rendered)\n- UR-issue-77 (#77)"
	d := commsFilingDraft("Title", body, nil, nil, "UR-comment-12-7", "UR-issue-12", "UR-issue-9")
	req := commsFilingRequest(commsFilingInput{Draft: d, Gathered: commsFilingGathered()})

	sep := "\n\n---\n" + commsProvenanceHeading + "\n"
	if n := strings.Count(req.Body, commsProvenanceHeading); n != 1 {
		t.Fatalf("provenance heading appears %d times, want 1:\n%s", n, req.Body)
	}
	agent, server, ok := strings.Cut(req.Body, sep)
	if !ok {
		t.Fatalf("body has no server provenance section:\n%s", req.Body)
	}
	assertNoCommsAutolink(t, "agent part", agent)

	for _, want := range []string{
		"- UR-comment-12-7 (comment on #12)\n",
		"- UR-issue-12 (#12)\n",
		"- UR-issue-9 (#9)\n",
		"\nCharter rubric:\n- S2\n- U4\n",
	} {
		if !strings.Contains(server, want) {
			t.Errorf("server section lacks %q:\n%s", want, server)
		}
	}
	refs := regexp.MustCompile(`#[0-9]+`).FindAllString(req.Body, -1)
	if got := strings.Join(refs, ","); got != "#12,#12,#9" {
		t.Errorf("issue refs in the body = %s, want exactly the server's #12,#12,#9", got)
	}
	if strings.Contains(req.Body, "SECRET-AGENT-NOTE") {
		t.Error("an agent rubric note entered the filing body")
	}
	if got := strings.Join(req.SourceRefs, ","); got != "#9,#12" {
		t.Errorf("source_refs = %s, want #9,#12 (distinct, numeric order, the comment report mapped to its issue)", got)
	}
}

// TestCommsFilingRequest_CharterTextRendering: with charter text the rubric
// line renders sanitized to one neutralized line of at most 300 bytes; with
// none (or an id the text map lacks) the id renders alone.
func TestCommsFilingRequest_CharterTextRendering(t *testing.T) {
	d := commsFilingDraft("Title", "Body", nil, nil, "UR-issue-12")
	text := map[string]string{"S2": "Is it <b>distinct</b>\nfrom #3? " + strings.Repeat("x", 400)}
	req := commsFilingRequest(commsFilingInput{Draft: d, Gathered: commsFilingGathered(), RubricText: text})

	_, server, _ := strings.Cut(req.Body, commsProvenanceHeading)
	var s2 string
	for _, line := range strings.Split(server, "\n") {
		if strings.HasPrefix(line, "- S2: ") {
			s2 = strings.TrimPrefix(line, "- S2: ")
		}
	}
	if !strings.HasPrefix(s2, "Is it \uFF1Cb\uFF1Edistinct\uFF1C/b\uFF1E from \uFF033? ") {
		t.Errorf("S2 charter text = %q, want it sanitized to one neutralized line", s2)
	}
	if len(s2) > commsCharterLineMaxBytes {
		t.Errorf("S2 charter text is %d bytes, over the %d cap", len(s2), commsCharterLineMaxBytes)
	}
	if !strings.Contains(server, "\n- U4\n") {
		t.Errorf("U4 (no charter text) must render as its id alone:\n%s", server)
	}
}

// TestCommsFilingRequest_DropsAutonomyAndForeignLabels: a draft struct built
// directly (so semantic rule h cannot mask the filter) carrying autonomy:high,
// priority:p1 and a duplicate files only area:/type:/phase: labels.
func TestCommsFilingRequest_DropsAutonomyAndForeignLabels(t *testing.T) {
	labels := []string{"autonomy:high", "area:backend", "priority:p1", "Type:Bug", "phase:build", "AUTONOMY:low", "area:backend", "epic"}
	d := commsFilingDraft("Title", "Body", labels, nil, "UR-issue-12")
	req := commsFilingRequest(commsFilingInput{Draft: d, Gathered: commsFilingGathered()})
	if got := strings.Join(req.Labels, ","); got != "area:backend,Type:Bug,phase:build" {
		t.Errorf("labels = %s, want area:backend,Type:Bug,phase:build", got)
	}
	if empty := commsFilingRequest(commsFilingInput{Draft: commsFilingDraft("T", "B", nil, nil, "UR-issue-12")}); empty.Labels == nil {
		t.Error("labels must be an empty slice, never nil")
	}
}

// TestCommsFilingRequest_ParentEpicNormalized: a bare or #-prefixed epic ref
// becomes #N; an unparsable one (unreachable after rule i) is dropped.
func TestCommsFilingRequest_ParentEpicNormalized(t *testing.T) {
	for in, want := range map[string]string{"389": "#389", "#389": "#389", "not-a-number": ""} {
		d := commsFilingDraft("Title", "Body", nil, strPtr(in), "UR-issue-12")
		if got := commsFilingRequest(commsFilingInput{Draft: d}).Relations.ParentEpic; got != want {
			t.Errorf("parent_epic %q -> %q, want %q", in, got, want)
		}
	}
	d := commsFilingDraft("Title", "Body", nil, nil, "UR-issue-12")
	if got := commsFilingRequest(commsFilingInput{Draft: d}).Relations.ParentEpic; got != "" {
		t.Errorf("absent parent_epic -> %q, want empty", got)
	}
}

// TestCommsFilingRequest_UnshownReportRendersAlone: an id the gather does not
// show (unreachable after the ingest's report_ref check) renders as its id,
// with no marker entry and no source ref; nil gather is tolerated.
func TestCommsFilingRequest_UnshownReportRendersAlone(t *testing.T) {
	d := commsFilingDraft("Title", "Body", nil, nil, "UR-issue-404")
	req := commsFilingRequest(commsFilingInput{Draft: d, Gathered: nil})
	if !strings.Contains(req.Body, "\n- UR-issue-404\n") {
		t.Errorf("an unshown id must render alone:\n%s", req.Body)
	}
	if strings.Contains(req.Body, userreport.CommsMarkerPrefix) || len(req.SourceRefs) != 0 {
		t.Errorf("an unshown id must carry no marker and no source ref: refs=%v body=\n%s", req.SourceRefs, req.Body)
	}
	if req.SourceRefs == nil {
		t.Error("source_refs must be an empty slice, never nil")
	}
}

// TestCommsFilingBodyDigest is the sha256 hex of the body.
func TestCommsFilingBodyDigest(t *testing.T) {
	sum := sha256.Sum256([]byte("body"))
	if got := commsFilingBodyDigest("body"); got != hex.EncodeToString(sum[:]) {
		t.Errorf("digest = %s", got)
	}
}

// ---------------------------------------------------------------------------
// Previews
// ---------------------------------------------------------------------------

// commsPreviewConventions is the default conventions with a `bug` whose title
// needs no {epic}/{n}, so a draft without a parent epic previews cleanly; the
// default label namespaces (and their autonomy:medium default) are kept.
func commsPreviewConventions() workmgmt.Conventions {
	conv := workmgmt.Default()
	types := make(map[string]workmgmt.ItemType, len(conv.Types))
	for k, v := range conv.Types {
		types[k] = v
	}
	bug := types["bug"]
	bug.TitleFormat = "{summary}"
	types["bug"] = bug
	conv.Types = types
	return conv
}

// commsPreviewReport is a report of the given drafts.
func commsPreviewReport(drafts ...plan.CommsDraft) *plan.CommsReport {
	return &plan.CommsReport{Kind: plan.KindCommsReport, ReportVersion: plan.CommsReportVersion, Drafts: drafts}
}

func commsPreviewRun(repo string) *run.Run {
	return &run.Run{ID: uuid.New(), Repo: repo, State: run.StateRunning}
}

// withCommsPreviewBudget shrinks the total preview budget for one test.
func withCommsPreviewBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := commsPreviewTotalBudget
	commsPreviewTotalBudget = d
	t.Cleanup(func() { commsPreviewTotalBudget = prev })
}

// TestCommsPreviewDrafts_RecordsRenderedPreview: a clean preview records the
// full rendered body (which opens with the renderer's body), the digest of
// the renderer's body, labels including the conventions-defaulted
// autonomy:medium the captain must see, and source refs; its JSON carries
// every preview key and no error/skipped key, and round-trips.
func TestCommsPreviewDrafts_RecordsRenderedPreview(t *testing.T) {
	p := &fakeWorkProvider{}
	registerFakeProvider(t, p)
	installConventions(t, commsPreviewConventions(), nil)
	s := New(Config{})

	d := commsFilingDraft("CSV export writes no rows", "Two users report an empty export.", []string{"area:backend"}, nil, "UR-issue-12")
	g := commsFilingGathered()
	set := s.commsPreviewDrafts(context.Background(), commsPreviewRun("kuhlman-labs/comms"), commsPreviewReport(d), g)

	if set.Degraded || len(set.Previews) != 1 {
		t.Fatalf("set = %+v, want one undegraded preview", set)
	}
	pv := set.Previews[0]
	req := commsFilingRequest(commsFilingInput{Draft: d, Gathered: g})
	if pv.DraftID != d.ID || pv.FilingBodyDigest != commsFilingBodyDigest(req.Body) {
		t.Errorf("entry identity = (%q, %q), want (%q, digest of the renderer body)", pv.DraftID, pv.FilingBodyDigest, d.ID)
	}
	if pv.CommsRenderedPreview == nil || pv.Error != nil || pv.Skipped != "" {
		t.Fatalf("entry = %+v, want exactly the rendered preview", pv)
	}
	if !strings.HasPrefix(pv.Body, req.Body) || pv.Title != "CSV export writes no rows" {
		t.Errorf("preview title/body = %q / %q, want the rendered filing", pv.Title, pv.Body)
	}
	if !containsString(pv.DefaultedLabels, "autonomy:medium") || !containsString(pv.Labels, "area:backend") {
		t.Errorf("labels = %v defaulted = %v; the captain must see the defaulted autonomy:medium", pv.Labels, pv.DefaultedLabels)
	}
	if strings.Join(pv.SourceRefs, ",") != "#12" {
		t.Errorf("source_refs = %v, want [#12]", pv.SourceRefs)
	}
	if p.called {
		t.Error("the preview reached provider.File")
	}

	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic struct {
		Previews []map[string]json.RawMessage `json:"previews"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"draft_id", "filing_body_digest", "title", "body", "labels", "defaulted_labels", "missing_label_namespaces", "source_refs", "intake"} {
		if _, ok := generic.Previews[0][k]; !ok {
			t.Errorf("recorded preview lacks key %q: %s", k, raw)
		}
	}
	for _, k := range []string{"error", "skipped"} {
		if _, ok := generic.Previews[0][k]; ok {
			t.Errorf("a rendered preview carries %q: %s", k, raw)
		}
	}
	var back commsPreviewSet
	if err := json.Unmarshal(raw, &back); err != nil || back.Previews[0].CommsRenderedPreview == nil || back.Previews[0].Body != pv.Body {
		t.Errorf("recorded set does not round-trip: err=%v back=%+v", err, back)
	}
}

// TestCommsPreviewDrafts_PreviewErrorRecordedNextDraftPreviews: a refused
// preview is recorded as error{code, message} and the next draft still
// previews.
func TestCommsPreviewDrafts_PreviewErrorRecordedNextDraftPreviews(t *testing.T) {
	registerFakeProvider(t, &fakeWorkProvider{})
	installConventions(t, commsPreviewConventions(), nil)
	s := New(Config{})

	bad := commsFilingDraft("Bad type", "Body", nil, nil, "UR-issue-12")
	bad.ProposedIssue.Type = "no_such_type"
	good := commsFilingDraft("Good", "Body", nil, nil, "UR-issue-9")
	set := s.commsPreviewDrafts(context.Background(), commsPreviewRun("kuhlman-labs/comms"), commsPreviewReport(bad, good), commsFilingGathered())

	if len(set.Previews) != 2 {
		t.Fatalf("previews = %+v, want 2", set.Previews)
	}
	if e := set.Previews[0].Error; e == nil || e.Code != "work_item_invalid" || e.Message == "" || set.Previews[0].CommsRenderedPreview != nil {
		t.Errorf("first entry = %+v, want error work_item_invalid only", set.Previews[0])
	}
	if set.Previews[0].FilingBodyDigest == "" {
		t.Error("a refused preview must still record its filing body digest")
	}
	if set.Previews[1].CommsRenderedPreview == nil || set.Previews[1].Title != "Good" {
		t.Errorf("second entry = %+v, want a rendered preview", set.Previews[1])
	}
}

// TestCommsPreviewDrafts_BudgetBoundsLockedPreview (approval condition 3,
// option b): the sequential-number lock a numbered-type preview takes is
// HELD, so previewWorkItem blocks uncancellably. commsPreviewDrafts must still
// return within the budget plus margin, recording preview_timeout for the
// blocked draft and budget_exhausted for the one after it.
func TestCommsPreviewDrafts_BudgetBoundsLockedPreview(t *testing.T) {
	budget := timescale.D(300 * time.Millisecond)
	withCommsPreviewBudget(t, budget)
	fp := &fakeSequentialNumberProvider{discovered: []int{79}}
	registerFakeSequentialNumberProvider(t, fp)
	conv := commsPreviewConventions()
	installConventions(t, conv, nil)
	s := New(Config{})

	repo := "kuhlman-labs/comms-locked"
	target := workmgmt.Target{Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "comms-locked"}}
	unlock := lockSequentialNumberKey(sequentialNumberLockKey(target, conv.Types["adr"].Numbering.Prefix))
	released := false
	release := func() {
		if !released {
			released = true
			unlock()
		}
	}
	t.Cleanup(release)

	first := commsFilingDraft("Numbered one", "Body", nil, nil, "UR-issue-12")
	first.ProposedIssue.Type = "adr"
	second := commsFilingDraft("Numbered two", "Body", nil, nil, "UR-issue-9")
	second.ProposedIssue.Type = "adr"

	done := make(chan commsPreviewSet, 1)
	go func() {
		done <- s.commsPreviewDrafts(context.Background(), commsPreviewRun(repo), commsPreviewReport(first, second), commsFilingGathered())
	}()
	var set commsPreviewSet
	select {
	case set = <-done:
	case <-time.After(timescale.D(300*time.Millisecond + 2*time.Second)):
		release() // let the wedged preview finish so the goroutine is not leaked
		t.Fatal("commsPreviewDrafts did not return within the budget plus margin while the numbering lock was held")
	}
	if len(set.Previews) != 2 {
		t.Fatalf("previews = %+v, want 2", set.Previews)
	}
	if got := set.Previews[0].Skipped; got != commsPreviewSkipTimeout {
		t.Errorf("blocked draft skipped = %q, want %q (entry %+v)", got, commsPreviewSkipTimeout, set.Previews[0])
	}
	if got := set.Previews[1].Skipped; got != commsPreviewSkipBudgetExhausted {
		t.Errorf("later draft skipped = %q, want %q (entry %+v)", got, commsPreviewSkipBudgetExhausted, set.Previews[1])
	}
	for _, pv := range set.Previews {
		if pv.FilingBodyDigest == "" || pv.CommsRenderedPreview != nil || pv.Error != nil {
			t.Errorf("skipped entry = %+v, want the digest and the skip only", pv)
		}
	}
	if fp.fileCalls != 0 {
		t.Error("the preview reached provider.File")
	}
}

// commsPanicProvider panics on number discovery.
type commsPanicProvider struct{ fakeWorkProvider }

func (*commsPanicProvider) DiscoverNumbers(context.Context, workmgmt.DiscoverNumbersRequest) ([]int, error) {
	panic("number discovery exploded")
}

// TestCommsPreviewDrafts_PanicRecorded: a panic inside a preview goroutine is
// recovered (no request frame recovers a goroutine) and recorded, and the next
// draft still previews. The repo is unique to this test because the panic
// abandons the sequential-number lock for its key.
func TestCommsPreviewDrafts_PanicRecorded(t *testing.T) {
	p := &commsPanicProvider{}
	p.name = workmgmt.Default().Provider
	workmgmt.Register(p)
	installConventions(t, commsPreviewConventions(), nil)
	s := New(Config{})

	boom := commsFilingDraft("Numbered", "Body", nil, nil, "UR-issue-12")
	boom.ProposedIssue.Type = "adr"
	good := commsFilingDraft("Good", "Body", nil, nil, "UR-issue-9")
	set := s.commsPreviewDrafts(context.Background(), commsPreviewRun("kuhlman-labs/comms-panic-"+uuid.NewString()[:8]), commsPreviewReport(boom, good), commsFilingGathered())

	if e := set.Previews[0].Error; e == nil || e.Code != commsPreviewErrorPanicked || !strings.Contains(e.Message, "number discovery exploded") {
		t.Errorf("first entry = %+v, want error %s naming the panic", set.Previews[0], commsPreviewErrorPanicked)
	}
	if set.Previews[1].CommsRenderedPreview == nil {
		t.Errorf("second entry = %+v, want a rendered preview", set.Previews[1])
	}
}

// TestCommsPreviewDrafts_RunWideDegrades: a malformed run repo and an
// unavailable conventions read each degrade EVERY draft with the named reason
// (no preview attempted), record preview_degraded, and still record each
// draft's filing body digest.
func TestCommsPreviewDrafts_RunWideDegrades(t *testing.T) {
	cases := map[string]struct {
		repo    string
		convErr error
		want    string
	}{
		"repo_malformed":          {repo: "not-a-repo", want: commsPreviewDegradeRepoMalformed},
		"conventions_unavailable": {repo: "kuhlman-labs/comms", convErr: errors.New("conventions read failed"), want: commsPreviewDegradeConventionsUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := &fakeWorkProvider{}
			registerFakeProvider(t, p)
			installConventions(t, commsPreviewConventions(), tc.convErr)
			s := New(Config{})

			d1 := commsFilingDraft("One", "Body", nil, nil, "UR-issue-12")
			d2 := commsFilingDraft("Two", "Body", nil, nil, "UR-issue-9")
			set := s.commsPreviewDrafts(context.Background(), commsPreviewRun(tc.repo), commsPreviewReport(d1, d2), commsFilingGathered())

			if !set.Degraded || set.DegradeReason != tc.want || set.CharterText != commsCharterTextUnavailable {
				t.Errorf("set = degraded %v reason %q charter %q, want true %q %q", set.Degraded, set.DegradeReason, set.CharterText, tc.want, commsCharterTextUnavailable)
			}
			if len(set.Previews) != 2 {
				t.Fatalf("previews = %+v, want 2", set.Previews)
			}
			for _, pv := range set.Previews {
				if pv.Skipped != tc.want || pv.Error != nil || pv.CommsRenderedPreview != nil || pv.FilingBodyDigest == "" {
					t.Errorf("entry = %+v, want skipped %q with a digest only", pv, tc.want)
				}
			}
		})
	}
}

// TestCommsPreviewDrafts_NilReport records an empty, always-array set.
func TestCommsPreviewDrafts_NilReport(t *testing.T) {
	set := New(Config{}).commsPreviewDrafts(context.Background(), commsPreviewRun("kuhlman-labs/comms"), nil, nil)
	if set.Previews == nil || len(set.Previews) != 0 {
		t.Errorf("previews = %#v, want an empty non-nil slice", set.Previews)
	}
}

// commsCharterServer wires a charter-declaring conventions and the igCharterDoc
// document seams, and returns the server plus the charter content hash the
// read produces.
func commsCharterServer(t *testing.T) (*Server, string) {
	t.Helper()
	registerFakeProvider(t, &fakeWorkProvider{})
	conv := commsPreviewConventions()
	conv.Charter = &workmgmt.Charter{Path: igCharterPath}
	installConventions(t, conv, nil)
	s := New(igCharterConfig(igCharterDoc, false))
	charter, reason, detail := s.resolveCharterDocument(context.Background(), conv, workmgmt.Target{Repo: workmgmt.Repo{Owner: "kuhlman-labs", Name: "comms"}})
	if reason != "" || charter.ContentHash == "" {
		t.Fatalf("fixture charter read = (%q, %q, %s), want a clean read with a hash", charter.ContentHash, reason, detail)
	}
	return s, charter.ContentHash
}

const commsS2Text = "Is the item distinct from what is already tracked?"

// TestCommsPreviewDrafts_CharterMatchedRendersText: the charter read at
// ingest has the gathered hash, so the rubric line text renders.
func TestCommsPreviewDrafts_CharterMatchedRendersText(t *testing.T) {
	s, hash := commsCharterServer(t)
	g := commsFilingGathered()
	g.Charter.ContentHash = hash
	d := commsFilingDraft("Title", "Body", nil, nil, "UR-issue-12")
	set := s.commsPreviewDrafts(context.Background(), commsPreviewRun("kuhlman-labs/comms"), commsPreviewReport(d), g)

	if set.CharterText != commsCharterTextRendered {
		t.Fatalf("charter_text = %q, want %q", set.CharterText, commsCharterTextRendered)
	}
	pv := set.Previews[0]
	if pv.CommsRenderedPreview == nil || !strings.Contains(pv.Body, "- S2: "+commsS2Text) {
		t.Errorf("preview = %+v, want the S2 charter text rendered", pv)
	}
}

// TestCommsPreviewDrafts_CharterChangedRendersIDsOnly: the charter read at
// ingest has a DIFFERENT hash than the gather recorded, so its text — which
// would otherwise render — is withheld and the ids render alone; the digest
// is the ids-only body's.
func TestCommsPreviewDrafts_CharterChangedRendersIDsOnly(t *testing.T) {
	s, _ := commsCharterServer(t)
	g := commsFilingGathered()
	g.Charter.ContentHash = strings.Repeat("0", 64)
	d := commsFilingDraft("Title", "Body", nil, nil, "UR-issue-12")
	set := s.commsPreviewDrafts(context.Background(), commsPreviewRun("kuhlman-labs/comms"), commsPreviewReport(d), g)

	if set.CharterText != commsCharterTextChanged {
		t.Fatalf("charter_text = %q, want %q", set.CharterText, commsCharterTextChanged)
	}
	pv := set.Previews[0]
	if pv.CommsRenderedPreview == nil || strings.Contains(pv.Body, commsS2Text) || !strings.Contains(pv.Body, "\n- S2\n") {
		t.Errorf("preview body = %q, want S2 as an id alone", pv.Body)
	}
	idsOnly := commsFilingRequest(commsFilingInput{Draft: d, Gathered: g})
	if pv.FilingBodyDigest != commsFilingBodyDigest(idsOnly.Body) {
		t.Error("filing_body_digest is not the ids-only body's")
	}
}

// TestCommsPreviewDrafts_CharterUnavailable: no document seam is wired, so
// the charter read fails and the ids render alone.
func TestCommsPreviewDrafts_CharterUnavailable(t *testing.T) {
	registerFakeProvider(t, &fakeWorkProvider{})
	conv := commsPreviewConventions()
	conv.Charter = &workmgmt.Charter{Path: igCharterPath}
	installConventions(t, conv, nil)
	s := New(Config{})
	d := commsFilingDraft("Title", "Body", nil, nil, "UR-issue-12")
	set := s.commsPreviewDrafts(context.Background(), commsPreviewRun("kuhlman-labs/comms"), commsPreviewReport(d), commsFilingGathered())
	if set.CharterText != commsCharterTextUnavailable || set.Degraded {
		t.Errorf("set = charter %q degraded %v, want %q and not degraded", set.CharterText, set.Degraded, commsCharterTextUnavailable)
	}
	if set.Previews[0].CommsRenderedPreview == nil {
		t.Errorf("entry = %+v, want a rendered preview", set.Previews[0])
	}
}
