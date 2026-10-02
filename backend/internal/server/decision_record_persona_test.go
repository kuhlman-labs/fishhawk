package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Decision-record persona wiring (ADR-084 D4(b) / binding rule 4, E78.5 /
// #3756). Every integration test here drives the REAL runPlanReviews /
// runImplementReviews: spec YAML declaring reviewer_personas.<name>.
// decision_record → spec decode → SelectedReviewerPersona → reviewChangePaths
// → decisionrecord.Assemble → repodoc fetch at the run's admission commit
// (personaFetcher) → the persona's OWN prompt → one repodoc.AttributeSet →
// the audit fake. The selection engine's own unit tests live in
// backend/internal/decisionrecord.

const (
	drIndexPath   = "docs/adr/index.json"
	drAuditPath   = "docs/adr/101-audit-registry.md"
	drFrontPath   = "docs/adr/102-frontend-styling.md"
	drEmptyPath   = "docs/adr/103-no-applies-to.md"
	drUnknownPath = "docs/adr/104-audit-append-only.md"
	drAuditBody   = "DR-AUDIT SENTINEL: every emitted audit category is registered in categories.go."
	drFrontBody   = "DR-FRONT SENTINEL: the frontend uses one styling system."
	drEmptyBody   = "DR-EMPTY SENTINEL: a record that governs no path."
	drUnknownBody = "DR-UNKNOWN SENTINEL: audit entries are append-only."
	drChangePath  = "backend/internal/audit/categories.go"
	// drBranchSHA is a 40-hex ref OTHER than the admission commit, at which
	// personaFetcher.atRef serves branch-edited records.
	drBranchSHA       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	drBranchEditBody  = "DR-BRANCH-EDIT SENTINEL: this record was edited on the run branch."
	drDeclSite        = "reviewer_personas.security.decision_record.index in .fishhawk/workflows.yaml"
	drIndexHeading    = "### Decision record index"
	drAuditQuote      = "every emitted audit category is registered"
	drFrontQuote      = "the frontend uses one styling system"
	drAuditRecordHead = "Decision record ADR-101 — status: accepted"
	drUnknownHead     = "Decision record ADR-104 — status: unknown"
)

// drRecord is one adr-index-v1 record of the fixture index.
type drRecord struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Path         string   `json:"path"`
	Status       string   `json:"status"`
	Supersedes   []string `json:"supersedes"`
	SupersededBy []string `json:"superseded_by"`
	AppliesTo    []string `json:"applies_to"`
}

// drFixtureRecords is the four-record fixture: R-audit (accepted, governs the
// audit package), R-front (accepted, governs frontend/**), R-empty (accepted,
// governs nothing) and R-unknown (unknown, ALSO governs the audit package).
func drFixtureRecords() []drRecord {
	rec := func(id, title, path, status string, applies ...string) drRecord {
		if applies == nil {
			applies = []string{}
		}
		return drRecord{ID: id, Title: title, Path: path, Status: status, Supersedes: []string{}, SupersededBy: []string{}, AppliesTo: applies}
	}
	return []drRecord{
		rec("ADR-101", "Audit category registry", drAuditPath, "accepted", "backend/internal/audit/**"),
		rec("ADR-102", "Frontend styling system", drFrontPath, "accepted", "frontend/**"),
		rec("ADR-103", "A record that governs no path", drEmptyPath, "accepted"),
		rec("ADR-104", "Audit entries are append-only", drUnknownPath, "unknown", "backend/internal/audit/**"),
	}
}

// drIndexJSON renders records as an adr-index-v1 document.
func drIndexJSON(t *testing.T, records []drRecord) string {
	t.Helper()
	b, err := json.MarshalIndent(map[string]any{"schema_version": "adr-index-v1", "records": records}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedDecisionRecord serves the fixture index and every record body at the
// admission commit.
func seedDecisionRecord(t *testing.T, f *personaFetcher) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[drIndexPath] = drIndexJSON(t, drFixtureRecords())
	f.files[drAuditPath] = drAuditBody
	f.files[drFrontPath] = drFrontBody
	f.files[drEmptyPath] = drEmptyBody
	f.files[drUnknownPath] = drUnknownBody
}

// fetchedPaths returns every path the fetcher was asked for, in order.
func (f *personaFetcher) fetchedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths)
}

// fetchedRefs returns every ref the fetcher was asked for, in order.
func (f *personaFetcher) fetchedRefs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.refs)
}

// drPlanBody is a schema-valid plan whose scope is exactly paths.
func drPlanBody(t *testing.T, paths ...string) []byte {
	t.Helper()
	files := make([]any, 0, len(paths))
	for _, p := range paths {
		files = append(files, map[string]any{"path": p, "operation": "modify"})
	}
	b, err := json.Marshal(planfixture.Valid(func(m map[string]any) {
		m["scope"] = map[string]any{"files": files}
	}))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// drSpecOpts is a plan-attached decision_record persona.
func drSpecOpts(attachOn string) personaSpecOpts {
	return personaSpecOpts{attachOn: attachOn, decisionRecordIndex: drIndexPath}
}

// newDRPlanRun wires a plan-review run whose persona declares the fixture
// decision record and whose plan under review touches drChangePath.
func newDRPlanRun(t *testing.T, std, persona PlanReviewer) *personaPlanRun {
	t.Helper()
	p := newPersonaPlanRun(t, personaSpec(drSpecOpts("plan")), std, persona)
	seedDecisionRecord(t, p.fetcher)
	p.planBody = drPlanBody(t, drChangePath)
	return p
}

// drStandardBaseline returns the standard reviewer prompt of a persona-LESS
// gating plan review over the drChangePath plan — the byte-identity golden.
func drStandardBaseline(t *testing.T) string {
	t.Helper()
	std := approvingFake()
	base := newPersonaPlanRun(t, personaSpec(personaSpecOpts{}), std, nil)
	base.planBody = drPlanBody(t, drChangePath)
	base.review(t)
	calls := reviewerCalls(std)
	if len(calls) != 1 {
		t.Fatalf("baseline standard reviewer calls = %d, want 1", len(calls))
	}
	return calls[0]
}

// decodedEntry is one appended audit entry, in append order.
type decodedEntry struct {
	category string
	payload  map[string]any
}

// documentEntries returns every document_injected / document_truncated entry
// in APPEND order, decoded.
func documentEntries(t *testing.T, au *auditFake) []decodedEntry {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []decodedEntry
	for _, e := range au.appended {
		if e.Category != "document_injected" && e.Category != "document_truncated" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", e.Category, err)
		}
		out = append(out, decodedEntry{category: e.Category, payload: p})
	}
	return out
}

// injectedPaths returns the paths of every document_injected entry, sorted.
func injectedPaths(entries []decodedEntry) []string {
	var out []string
	for _, e := range entries {
		if e.category == "document_injected" {
			out = append(out, fmt.Sprint(e.payload["path"]))
		}
	}
	sort.Strings(out)
	return out
}

// (D1) PLAN REVIEW, end to end. The plan under review touches only the audit
// package. The persona's prompt carries, after its remit, the index and the
// FULL text of R-audit and R-unknown (accepted first), R-unknown labelled
// unknown and NOT settled; neither R-front's nor R-empty's body appears and
// neither path is ever fetched. The standard reviewer's prompt is
// byte-identical to a persona-less round's. The audit carries ONE injection
// set: document_injected for the remit, the index and both records, all at the
// admission commit with content hashes, sharing one injection_set_id.
//
// Counterfactuals (run): (a) delete the `record.PromptDocuments(...)` append
// in buildPersonaPrompt — the persona prompt lacks the index and records: RED;
// (b) replace the AttributeSet call with Attribute(*doc) — only the remit is
// attributed: RED on the injected-paths assertion.
func TestDecisionRecordPersona_PlanReview_SelectsAndAttributes(t *testing.T) {
	std, persona := approvingFake(), approvingFake()
	p := newDRPlanRun(t, std, persona)
	if p.review(t) {
		t.Fatal("approving reviewers must not gate")
	}
	perCalls := reviewerCalls(persona)
	if len(perCalls) != 1 {
		t.Fatalf("persona calls = %d, want 1", len(perCalls))
	}
	pp := perCalls[0]
	for _, want := range []string{personaRemitBody, drIndexHeading, drAuditBody, drUnknownBody, drAuditRecordHead, drUnknownHead, "NOT a settled decision", "ADR-101, ADR-104"} {
		if !strings.Contains(pp, want) {
			t.Errorf("persona prompt missing %q", want)
		}
	}
	for _, absent := range []string{drFrontBody, drEmptyBody, "Decision record ADR-104 — status: accepted"} {
		if strings.Contains(pp, absent) {
			t.Errorf("persona prompt carries %q", absent)
		}
	}
	remitAt, idxAt, auditAt, unknownAt := strings.Index(pp, personaRemitBody), strings.Index(pp, drIndexHeading), strings.Index(pp, drAuditBody), strings.Index(pp, drUnknownBody)
	if remitAt >= idxAt || idxAt >= auditAt || auditAt >= unknownAt {
		t.Errorf("render order remit %d < index %d < R-audit %d < R-unknown %d does not hold", remitAt, idxAt, auditAt, unknownAt)
	}
	for _, path := range p.fetcher.fetchedPaths() {
		if path == drFrontPath || path == drEmptyPath {
			t.Errorf("fetched %s, a record whose applies_to does not match the change", path)
		}
	}

	stdCalls := reviewerCalls(std)
	if len(stdCalls) != 1 {
		t.Fatalf("standard calls = %d, want 1", len(stdCalls))
	}
	if strings.Contains(stdCalls[0], drIndexHeading) || strings.Contains(stdCalls[0], drAuditBody) {
		t.Error("the decision record leaked into the standard reviewer's prompt")
	}
	if stdCalls[0] != drStandardBaseline(t) {
		t.Error("standard reviewer prompt is not byte-identical to the persona-less golden prompt")
	}

	entries := documentEntries(t, p.au)
	want := []string{drAuditPath, drUnknownPath, drIndexPath, personaRemitPath}
	if got := injectedPaths(entries); !slices.Equal(got, want) {
		t.Fatalf("document_injected paths = %v, want %v (ONE set: remit, index, both records)", got, want)
	}
	setID := entries[0].payload["injection_set_id"]
	for _, e := range entries {
		if e.category != "document_injected" {
			t.Errorf("unexpected %s entry %v — nothing was dropped", e.category, e.payload)
			continue
		}
		if e.payload["injection_set_id"] != setID || e.payload["document_count"] != float64(4) {
			t.Errorf("entry %v: not one 4-document set (want injection_set_id %v)", e.payload, setID)
		}
		if e.payload["commit"] != personaBaseCommit || e.payload["content_hash"] == "" || e.payload["content_hash"] == nil {
			t.Errorf("entry %v: want commit %s and a content_hash", e.payload, personaBaseCommit)
		}
	}
}

// (D2) IMPLEMENT REVIEW: the audit path appears ONLY in the diff (the
// approved plan's scope is backend/internal/foo/foo.go), so R-audit is
// selected only because changePaths carries the diff half.
//
// Counterfactual (run): make reviewChangePaths range over paths.plan only —
// no record matches, R-audit's body is absent: RED. Mechanism: the approved
// scope names no audit path.
func TestDecisionRecordPersona_ImplementReview_DiffOnlyPathSelects(t *testing.T) {
	std, persona := approvingFake(), approvingFake()
	s, au, runRow, implStage, f := personaImplRun(t, personaSpec(drSpecOpts("implement")), std, persona, "codex/"+personaAgentModel)
	seedDecisionRecord(t, f)
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: drChangePath, Status: policy.StatusModified}}}
	if s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, diff, nil, "head-dr", nil) {
		t.Fatal("approving reviewers must not gate")
	}
	perCalls := reviewerCalls(persona)
	if len(perCalls) != 1 {
		t.Fatalf("persona calls = %d, want 1", len(perCalls))
	}
	for _, want := range []string{drIndexHeading, drAuditBody, drUnknownBody} {
		if !strings.Contains(perCalls[0], want) {
			t.Errorf("persona prompt missing %q — the diff-only audit path must select R-audit", want)
		}
	}
	if stdCalls := reviewerCalls(std); len(stdCalls) != 1 || strings.Contains(stdCalls[0], drIndexHeading) {
		t.Error("standard reviewer did not run once on a decision-record-free prompt")
	}
	if got := injectedPaths(documentEntries(t, au)); !slices.Equal(got, []string{drAuditPath, drUnknownPath, drIndexPath, personaRemitPath}) {
		t.Errorf("document_injected paths = %v", got)
	}
}

// (D3) BRANCH EDIT: the run branch edited R-audit (served at drBranchSHA, which
// is also the reviewed head). The persona prompt carries the ADMISSION-commit
// text only, and every fetch was made at the admission commit.
//
// Counterfactual (run): hand decisionrecord.Assemble drBranchSHA (the reviewed
// head in this test) instead of the admission commit — the branch edit is
// injected and every fetch is at the branch ref: RED.
func TestDecisionRecordPersona_ImplementReview_ReadsAdmissionCommitNotBranch(t *testing.T) {
	std, persona := approvingFake(), approvingFake()
	s, _, runRow, implStage, f := personaImplRun(t, personaSpec(drSpecOpts("implement")), std, persona, "codex/"+personaAgentModel)
	seedDecisionRecord(t, f)
	f.atRef = map[string]map[string]string{drBranchSHA: {
		drIndexPath:   drIndexJSON(t, drFixtureRecords()),
		drAuditPath:   drBranchEditBody,
		drUnknownPath: drUnknownBody,
	}}
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: drChangePath, Status: policy.StatusModified}, {Path: drAuditPath, Status: policy.StatusModified}}}
	s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, diff, nil, drBranchSHA, nil)
	perCalls := reviewerCalls(persona)
	if len(perCalls) != 1 {
		t.Fatalf("persona calls = %d, want 1", len(perCalls))
	}
	if !strings.Contains(perCalls[0], drAuditBody) || strings.Contains(perCalls[0], drBranchEditBody) {
		t.Error("persona prompt does not carry exactly the admission-commit R-audit")
	}
	for i, ref := range f.fetchedRefs() {
		if ref != personaBaseCommit {
			t.Errorf("fetch %d (%s) at ref %q, want the admission commit %s", i, f.fetchedPaths()[i], ref, personaBaseCommit)
		}
	}
}

// (D4) OVER CAP: a resolver cap that fits the index and R-audit but not the
// 1500-byte R-unknown. The sum of rendered_bytes over the set's index+record
// document_injected entries stays within the cap, R-unknown is never shown,
// and ONE selection-level document_truncated (selection decision_record)
// names it with its rank — written BEFORE any document_injected.
//
// Counterfactual: the budget check itself lives in decisionrecord.Assemble
// (pinned there); here, replacing AttributeSet with Attribute over the
// documents only drops the selection entry: RED on the truncation assertion.
func TestDecisionRecordPersona_OverCap_DropsAndRecordsSelection(t *testing.T) {
	std, persona := approvingFake(), approvingFake()
	p := newDRPlanRun(t, std, persona)
	bigUnknown := drUnknownBody + strings.Repeat(" padding", 200)
	p.fetcher.files[drUnknownPath] = bigUnknown
	capBytes := len(p.fetcher.files[drIndexPath]) + len(drAuditBody) + 200
	p.s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: p.fetcher, MaxBytes: capBytes}
	p.review(t)

	perCalls := reviewerCalls(persona)
	if len(perCalls) != 1 {
		t.Fatalf("persona calls = %d, want 1", len(perCalls))
	}
	if !strings.Contains(perCalls[0], drAuditBody) || strings.Contains(perCalls[0], drUnknownBody) {
		t.Error("over cap: want R-audit shown and R-unknown NOT shown")
	}
	if !strings.Contains(perCalls[0], "NOT INCLUDED because of the injection cap") || !strings.Contains(perCalls[0], "ADR-104") {
		t.Error("the persona prompt does not name the dropped record")
	}

	entries := documentEntries(t, p.au)
	if len(entries) == 0 || entries[0].category != "document_truncated" {
		t.Fatalf("first document entry = %+v, want the selection document_truncated BEFORE any document_injected", entries)
	}
	sel := entries[0].payload
	if sel["selection"] != "decision_record" || sel["path"] != drIndexPath || sel["dropped_count"] != float64(1) || sel["cap_bytes"] != float64(capBytes) {
		t.Errorf("selection truncation = %v", sel)
	}
	dropped, _ := sel["dropped"].([]any)
	if len(dropped) != 1 || dropped[0].(map[string]any)["id"] != "ADR-104" || dropped[0].(map[string]any)["rank"] != float64(2) {
		t.Errorf("dropped = %v, want ADR-104 at rank 2", sel["dropped"])
	}
	sum := 0
	for _, e := range entries[1:] {
		if e.category != "document_injected" {
			t.Errorf("entry %v after the first document_injected-phase position", e)
			continue
		}
		if e.payload["injection_set_id"] != sel["injection_set_id"] {
			t.Errorf("entry %v is not in the selection's injection set", e.payload)
		}
		if e.payload["path"] != personaRemitPath {
			sum += int(e.payload["rendered_bytes"].(float64))
		}
	}
	if sum > capBytes {
		t.Errorf("index+records rendered_bytes = %d, exceeds the cap %d", sum, capBytes)
	}
	if got := injectedPaths(entries); !slices.Equal(got, []string{drAuditPath, drIndexPath, personaRemitPath}) {
		t.Errorf("document_injected paths = %v", got)
	}
}

// assertDRDegraded asserts the fail-closed contract for one decision-record
// degrade: the persona never ran, ONE persona_remit_unavailable skip names
// the persona and detail, NO document_injected entry exists (no standard
// document is configured, so any would belong to the failed set), and the
// standard reviewer ran exactly once on the persona-less golden prompt.
func assertDRDegraded(t *testing.T, p *personaPlanRun, wantDetail string) {
	t.Helper()
	if n := len(reviewerCalls(p.persona.(*fakePlanReviewer))); n != 0 {
		t.Errorf("persona invoked %d times, want 0 — a degraded persona must never run", n)
	}
	skips := decodeSkipped(t, p.au, "plan_review_skipped")
	if len(skips) != 1 {
		t.Fatalf("plan_review_skipped entries = %d (%+v), want 1", len(skips), skips)
	}
	if sk := skips[0]; sk.Reason != planreview.ReasonPersonaRemitUnavailable || sk.Persona != personaTestName || sk.Detail != wantDetail {
		t.Errorf("skip = {reason %q persona %q detail %q}, want {%q %q %q}",
			sk.Reason, sk.Persona, sk.Detail, planreview.ReasonPersonaRemitUnavailable, personaTestName, wantDetail)
	}
	if n := countAuditCategory(p.au, "document_injected"); n != 0 {
		t.Errorf("document_injected entries = %d, want 0 for a failed set", n)
	}
	stdCalls := reviewerCalls(p.std.(*fakePlanReviewer))
	if len(stdCalls) != 1 {
		t.Fatalf("standard reviewer calls = %d, want 1 — the standard reviewers must still run", len(stdCalls))
	}
	if stdCalls[0] != drStandardBaseline(t) {
		t.Error("standard reviewer prompt differs from the persona-less golden prompt")
	}
	started := decodeStarted(t, p.au, "plan_review_started")
	if started.ConfiguredAgents != 2 || !planreview.Settled(started.ConfiguredAgents, terminalCount(p.au, "plan")) {
		t.Errorf("round does not settle: configured_agents %d, terminal entries %d", started.ConfiguredAgents, terminalCount(p.au, "plan"))
	}
}

// (D5) One test per decision-record degrade detail through the real plan
// loop.
//
// Counterfactuals (run): index missing — map ErrIndexMissing to an empty
// selection (return a nil selection and "") so the persona runs: RED;
// malformed index — drop the ErrInvalidIndex case: detail becomes
// decision_record_unresolvable: RED; listed record missing — ignore
// Assemble's error — the persona runs on its remit alone: RED.
func TestDecisionRecordPersona_DegradeDetails(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f *personaFetcher)
		detail string
	}{
		{"index missing", func(_ *testing.T, f *personaFetcher) { delete(f.files, drIndexPath) }, personaDetailDecisionRecordIndexMissing},
		{"malformed index", func(_ *testing.T, f *personaFetcher) {
			f.files[drIndexPath] = `{"schema_version":"adr-index-v1","records":[{"id":"ADR-101","unknown_field":true}]}`
		}, personaDetailDecisionRecordInvalid},
		{"listed record missing", func(_ *testing.T, f *personaFetcher) { delete(f.files, drAuditPath) }, personaDetailDecisionRecordUnresolvable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newDRPlanRun(t, approvingFake(), approvingFake())
			tc.mutate(t, p.fetcher)
			if p.review(t) {
				t.Fatal("a degraded persona must not gate the stage")
			}
			assertDRDegraded(t, p, tc.detail)
			if !strings.Contains(p.logs.String(), "decision record could not be assembled") {
				t.Errorf("degrade WARN missing:\n%s", p.logs.String())
			}
		})
	}
}

// (D6, approval condition 1) The admission commit is checked BEFORE any
// dereference or read: nil, empty and non-40-hex all map to
// decision_record_unresolvable with zero fetches. Driven DIRECTLY on
// resolvePersonaDecisionRecord because through the review loops the remit's
// own guards fire first (run_base_commit_unrecorded for nil, remit_unresolvable
// for empty / non-hex) and would MASK this guard; the loop half is pinned by
// TestDecisionRecordPersona_UnusableBaseCommit_StandardUnaffected.
//
// Counterfactual (run): replace the guard with a direct `*commit` dereference
// — the nil case panics: RED. The empty and non-hex cases are masked by
// repodoc's run-admission refusal (also zero fetches, same detail): run, and
// reported in the PR notes as unverifiable by counterfactual.
func TestResolvePersonaDecisionRecord_UnusableBaseCommit(t *testing.T) {
	empty, short := "", "main"
	for _, tc := range []struct {
		name   string
		commit *string
	}{{"nil", nil}, {"empty", &empty}, {"non-40-hex", &short}} {
		t.Run(tc.name, func(t *testing.T) {
			p := newDRPlanRun(t, approvingFake(), approvingFake())
			runRow := p.rr.getRuns[p.runID]
			runRow.DocumentBaseCommit = tc.commit
			inv := &personaInvocation{
				selected: spec.SelectedReviewerPersona{
					Name: personaTestName, RemitPath: personaRemitPath, DeclarationSite: personaDeclSite,
					DecisionRecordIndex: drIndexPath, DecisionRecordDeclarationSite: drDeclSite,
				},
				changePaths: []string{drChangePath},
			}
			sel, detail := p.s.resolvePersonaDecisionRecord(t.Context(), runRow, forge.RepoRef{Owner: "kuhlman-labs", Name: "example"}, forge.CredentialScope{}, inv)
			if sel != nil || detail != personaDetailDecisionRecordUnresolvable {
				t.Errorf("got (%v, %q), want (nil, %q)", sel, detail, personaDetailDecisionRecordUnresolvable)
			}
			if n := p.fetcher.fetches(); n != 0 {
				t.Errorf("fetches = %d, want 0 — nothing is read without a usable admission commit", n)
			}
		})
	}
}

// (D6, loop half) Through the real plan loop, a decision_record persona on a
// run whose admission commit is nil, empty or non-40-hex never panics and
// never runs, while the standard reviewer runs on the golden prompt.
func TestDecisionRecordPersona_UnusableBaseCommit_StandardUnaffected(t *testing.T) {
	empty, short := "", "main"
	for _, tc := range []struct {
		name   string
		commit *string
		detail string
	}{
		{"nil", nil, personaDetailBaseCommitUnrecorded},
		{"empty", &empty, personaDetailRemitUnresolvable},
		{"non-40-hex", &short, personaDetailRemitUnresolvable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newDRPlanRun(t, approvingFake(), approvingFake())
			p.rr.getRuns[p.runID].DocumentBaseCommit = tc.commit
			p.review(t)
			assertDRDegraded(t, p, tc.detail)
			if n := p.fetcher.fetches(); n != 0 {
				t.Errorf("fetches = %d, want 0", n)
			}
		})
	}
}

// (D7) An attribution failure for the ONE set (remit + index + records)
// discards the persona prompt: remit_unattributed, the persona never runs.
func TestDecisionRecordPersona_SetUnattributed_Degrades(t *testing.T) {
	p := newDRPlanRun(t, approvingFake(), approvingFake())
	p.au.appendErrCategory = "document_injected"
	p.review(t)
	assertDRDegraded(t, p, personaDetailRemitUnattributed)
}

// (D8) QUOTE VERIFICATION covers the records: a persona concern quoting
// R-audit with document_ref R-audit's path keeps its severity and records
// R-audit's content_hash; one quoting R-front — a record NOT included — is
// demoted to low and marked quote_unverified.
//
// Counterfactual (run): compute quoteDocs from a Trigger WITHOUT the
// selection documents — the R-audit quote is demoted: RED.
func TestDecisionRecordPersona_QuoteVerificationCoversRecords(t *testing.T) {
	const includedNote, excludedNote = "quotes R-audit", "quotes R-front"
	persona := verdictFake(planreview.VerdictApproveWithConcerns, personaAgentModel,
		quotedConcern(includedNote, drAuditQuote, drAuditPath),
		quotedConcern(excludedNote, drFrontQuote, drFrontPath),
	)
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan", human: 1, decisionRecordIndex: drIndexPath}), approvingFake(), persona)
	seedDecisionRecord(t, p.fetcher)
	p.planBody = drPlanBody(t, drChangePath)
	cr := newFakeConcernRepo()
	p.s.cfg.ConcernRepo = cr
	p.review(t)

	var auditHash string
	for _, e := range documentEntries(t, p.au) {
		if e.category == "document_injected" && e.payload["path"] == drAuditPath {
			auditHash, _ = e.payload["content_hash"].(string)
		}
	}
	if auditHash == "" {
		t.Fatal("R-audit was not attributed")
	}
	pv := personaPlanVerdict(t, p.au)
	inc := payloadConcernByNote(t, pv.Concerns, includedNote)
	if inc.Severity != planreview.SeverityHigh || inc.QuoteUnverified || inc.QuoteVerifiedContentHash != auditHash {
		t.Errorf("R-audit quote concern = %+v, want high and verified with %s", inc, auditHash)
	}
	exc := payloadConcernByNote(t, pv.Concerns, excludedNote)
	if exc.Severity != planreview.SeverityLow || !exc.QuoteUnverified || exc.SeverityClampedFrom != planreview.SeverityHigh {
		t.Errorf("R-front quote concern = %+v, want low, quote_unverified", exc)
	}
}

// (D9) A persona WITHOUT decision_record makes no extra read: it fetches
// only its remit, its prompt carries no decision-record framing, and its set
// is the remit alone — the pre-#3756 persona path (the persona golden tests in
// plan_test.go / trace_test.go pin the prompt's content).
//
// Counterfactual (run): drop the `DecisionRecordIndex != ""` condition (always
// assemble) — the index read is attempted (a second fetch) and the persona
// degrades on the absent index: RED.
func TestDecisionRecordPersona_UndeclaredPersonaReadsRemitOnly(t *testing.T) {
	persona := approvingFake()
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), approvingFake(), persona)
	p.planBody = drPlanBody(t, drChangePath)
	p.review(t)
	if got := p.fetcher.fetchedPaths(); !slices.Equal(got, []string{personaRemitPath}) {
		t.Errorf("fetched %v, want only the remit", got)
	}
	perCalls := reviewerCalls(persona)
	if len(perCalls) != 1 || strings.Contains(perCalls[0], "Decision record") {
		t.Fatalf("persona calls = %d or its prompt carries decision-record framing", len(perCalls))
	}
	entries := documentEntries(t, p.au)
	if len(entries) != 1 || entries[0].category != "document_injected" || entries[0].payload["document_count"] != float64(1) {
		t.Errorf("document entries = %+v, want exactly the remit's one-document set", entries)
	}
}

// reviewChangePaths unions the plan and diff halves, de-duplicated and
// sorted.
func TestReviewChangePaths(t *testing.T) {
	got := reviewChangePaths(reviewPaths{plan: []string{"b.go", "a.go"}, diff: []string{"c.go", "a.go"}})
	if want := []string{"a.go", "b.go", "c.go"}; !slices.Equal(got, want) {
		t.Errorf("reviewChangePaths = %v, want %v", got, want)
	}
	if got := reviewChangePaths(reviewPaths{}); got != nil {
		t.Errorf("empty reviewChangePaths = %v, want nil", got)
	}
}
