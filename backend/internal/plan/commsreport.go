package plan

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// KindCommsReport is the top-level discriminator value carried by a
// comms_report artifact — the FOURTH additive sibling of the plan artifact
// (E81.5 / #3775, contract #4015), after clarification_request,
// grooming_report and upkeep_report.
//
// It is an untyped string const, NOT an ArtifactKind, on purpose: declaring
// the ArtifactKind obliges AllArtifactKinds (TestAllArtifactKinds_
// EnumeratesEveryDeclaredConst), and that obliges the server's settle-table
// row, which needs the ingest handler. The ArtifactKind, the schema embed and
// ParseCommsReport land with that handler (validate.go); this file is
// schema-free so it can land first (the #3921 upkeep precedent).
//
// prompt.CommsReportKind states the same literal to the comms scan agent;
// TestCommsReport_PromptParity pins the two together.
const KindCommsReport = "comms_report"

// CommsReportVersion is the artifact's report_version token, mirroring the
// canonical docs/spec/comms-report-v1.schema.json filename stem.
const CommsReportVersion = "comms_report_v1"

// The comms_report_v1 bounds. The array bounds mirror the schema's maxItems
// (TestCommsReport_BoundsMatchSchema); the title and body caps are BYTE caps
// enforced by rule (g), tighter than the schema's character maxLength.
const (
	CommsMaxDrafts          = 25
	CommsMaxNDrift          = 50
	CommsMaxNotDrafted      = 500
	CommsMaxSourceReportIDs = 20
	CommsMaxRubricCitations = 8
	CommsMaxTitleBytes      = 200
	CommsMaxBodyBytes       = 20000
)

// Not-drafted reasons (the not-drafted `reason` enum).
const (
	CommsNotDraftedNoise              = "noise"
	CommsNotDraftedQuestion           = "question"
	CommsNotDraftedAlreadyTracked     = "already_tracked"
	CommsNotDraftedInsufficientDetail = "insufficient_detail"
	CommsNotDraftedOther              = "other"
)

// CommsNotDraftedReasons returns the closed not_drafted reason set in schema
// enum order, which is also the order prompt.CommsNotDraftedReasons lists it
// (TestCommsReport_PromptParity). A fresh slice per call.
func CommsNotDraftedReasons() []string {
	return []string{
		CommsNotDraftedNoise, CommsNotDraftedQuestion, CommsNotDraftedAlreadyTracked,
		CommsNotDraftedInsufficientDetail, CommsNotDraftedOther,
	}
}

// CommsLabelNamespaces returns the only label prefixes a draft may propose
// (rule h). autonomy:* is never among them: a scan reading user reports may
// suggest what an issue is about, never how much delegation it gets. A fresh
// slice per call.
func CommsLabelNamespaces() []string {
	return []string{"area:", "type:", "phase:"}
}

var (
	commsRubricIDShape  = regexp.MustCompile(`^[A-Z][0-9]+$`)
	commsNonGoalIDShape = regexp.MustCompile(`^N[0-9]+$`)
)

// CommsValidRubricID reports whether id is a rubric id: ^[A-Z][0-9]+$ and NOT
// of the non-goal shape ^N[0-9]+$ — exactly the rubric lines the comms scan
// prompt renders.
func CommsValidRubricID(id string) bool {
	return commsRubricIDShape.MatchString(id) && !commsNonGoalIDShape.MatchString(id)
}

// CommsValidNonGoalID reports whether id is a non-goal id (^N[0-9]+$).
func CommsValidNonGoalID(id string) bool {
	return commsNonGoalIDShape.MatchString(id)
}

// CommsReport is a decoded comms_report artifact: what a `plan`-typed propose
// stage declaring `produces: comms_report` emits after reading the gathered
// user reports. JSON tags mirror comms-report-v1.schema.json. Every shown
// report is accounted for in exactly one of Drafts, NDrift or NotDrafted.
type CommsReport struct {
	Kind            string            `json:"kind"`
	ReportVersion   string            `json:"report_version"`
	TicketReference TicketReference   `json:"ticket_reference"`
	GeneratedBy     GeneratedBy       `json:"generated_by"`
	Summary         string            `json:"summary"`
	Drafts          []CommsDraft      `json:"drafts"`
	NDrift          []CommsNDrift     `json:"n_drift"`
	NotDrafted      []CommsNotDrafted `json:"not_drafted"`
}

// CommsDraft is one proposed issue clustering one or more shown reports.
type CommsDraft struct {
	ID              string                `json:"id"`
	SourceReportIDs []string              `json:"source_report_ids"`
	RubricCitations []CommsRubricCitation `json:"rubric_citations"`
	ProposedIssue   CommsProposedIssue    `json:"proposed_issue"`
}

// CommsRubricCitation names one charter rubric line a draft serves. Note is
// read at the gate and never rendered into a filed issue.
type CommsRubricCitation struct {
	RubricID string  `json:"rubric_id"`
	Note     *string `json:"note,omitempty"`
}

// CommsProposedIssue is the issue a draft proposes filing. ParentEpic is a
// pointer so present-but-empty is checked (rule i) rather than read as
// absent.
type CommsProposedIssue struct {
	Type       string   `json:"type"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Labels     []string `json:"labels"`
	ParentEpic *string  `json:"parent_epic,omitempty"`
}

// CommsNDrift flags reports requesting something a charter non-goal excludes.
type CommsNDrift struct {
	ID              string   `json:"id"`
	NonGoalID       string   `json:"non_goal_id"`
	SourceReportIDs []string `json:"source_report_ids"`
	Note            string   `json:"note"`
}

// CommsNotDrafted accounts for one shown report neither drafted nor flagged.
type CommsNotDrafted struct {
	ReportID string  `json:"report_id"`
	Reason   string  `json:"reason"`
	Note     *string `json:"note,omitempty"`
}

// commsJoinSorted returns ids sorted ascending by byte order and joined with
// "+". It sorts a copy, so the caller's slice is untouched.
func commsJoinSorted(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	return strings.Join(sorted, "+")
}

// CommsDraftID derives a draft id exactly as the comms scan prompt states it:
// "draft:" followed by the sorted source report ids joined with "+".
func CommsDraftID(sourceReportIDs []string) string {
	return "draft:" + commsJoinSorted(sourceReportIDs)
}

// CommsNDriftID derives an n_drift id exactly as the comms scan prompt states
// it: "ndrift:" + nonGoalID + ":" + the sorted source report ids joined with
// "+".
func CommsNDriftID(nonGoalID string, sourceReportIDs []string) string {
	return "ndrift:" + nonGoalID + ":" + commsJoinSorted(sourceReportIDs)
}

// CommsParentEpicNumber parses a parent_epic ref — a positive issue number,
// bare or with ONE leading `#` — and reports whether it is valid. No
// whitespace trim: the report is machine-written, so a padded value is a
// defect (the UpkeepValidEpicRef rule).
func CommsParentEpicNumber(ref string) (int, bool) {
	if !UpkeepValidEpicRef(ref) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(ref, "#"))
	if err != nil {
		return 0, false
	}
	return n, true
}

// CitedReportIDs returns every report id the report cites across drafts,
// n_drift and not_drafted, sorted and distinct. Always non-nil.
func (r *CommsReport) CitedReportIDs() []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, d := range r.Drafts {
		for _, id := range d.SourceReportIDs {
			add(id)
		}
	}
	for _, n := range r.NDrift {
		for _, id := range n.SourceReportIDs {
			add(id)
		}
	}
	for _, nd := range r.NotDrafted {
		add(nd.ReportID)
	}
	sort.Strings(out)
	return out
}

// CheckCommsReportSemantics enforces the comms_report_v1 cross-field rules
// the schema cannot express (docs/spec/comms-report-v1.md § "Semantic
// rules"). It runs AFTER the schema check; every violation is a
// *SemanticError whose message opens with the JSON pointer:
//
//	(a) a draft's source_report_ids sorted strictly ascending     /drafts/<i>/source_report_ids/<k>
//	(b) a draft's id == CommsDraftID(source_report_ids)           /drafts/<i>/id
//	(c) rubric_citations sorted strictly by rubric_id             /drafts/<i>/rubric_citations/<k>
//	(d) a rubric_id is never of the non-goal shape ^N[0-9]+$      /drafts/<i>/rubric_citations/<k>/rubric_id
//	(e) an n_drift's source_report_ids sorted strictly ascending  /n_drift/<i>/source_report_ids/<k>
//	    and its id == CommsNDriftID(non_goal_id, ids)             /n_drift/<i>/id
//	(f) every report id appears in at most ONE entry across
//	    drafts, n_drift and not_drafted                           the second occurrence
//	(g) title is one line (no CR, LF or control rune) and at most
//	    200 BYTES; body at most 20000 BYTES                       /drafts/<i>/proposed_issue/title (/body)
//	(h) each label carries an area:/type:/phase: namespace
//	    (autonomy:* named on its own) and passes the label syntax /drafts/<i>/proposed_issue/labels/<k>
//	(i) parent_epic, when present, is a positive issue number     /drafts/<i>/proposed_issue/parent_epic
//	(j) kind and report_version are exact                         /kind, /report_version
//
// Rule (j) duplicates the schema's const on purpose: the schema is the first
// line and (j) the defence in depth for a caller holding a decoded struct.
func CheckCommsReportSemantics(r *CommsReport) error {
	if r == nil {
		return &SemanticError{Message: "/: comms report is nil"}
	}
	// (j)
	if r.Kind != KindCommsReport {
		return &SemanticError{Message: fmt.Sprintf("/kind: %q is not %q", r.Kind, KindCommsReport)}
	}
	if r.ReportVersion != CommsReportVersion {
		return &SemanticError{Message: fmt.Sprintf("/report_version: %q is not %q", r.ReportVersion, CommsReportVersion)}
	}

	owner := map[string]string{} // report id -> pointer of the entry citing it (f)
	claim := func(id, pointer string) error {
		if prev, dup := owner[id]; dup {
			return &SemanticError{Message: fmt.Sprintf(
				"%s: report id %q is already accounted for at %s; each report id appears in at most one entry across drafts, n_drift and not_drafted",
				pointer, id, prev)}
		}
		owner[id] = pointer
		return nil
	}

	for i := range r.Drafts {
		d := &r.Drafts[i]
		base := fmt.Sprintf("/drafts/%d", i)
		// (a)
		if err := checkCommsSortedIDs(base+"/source_report_ids", d.SourceReportIDs); err != nil {
			return err
		}
		// (b)
		if want := CommsDraftID(d.SourceReportIDs); d.ID != want {
			return &SemanticError{Message: fmt.Sprintf("%s/id: %q is not the derived id %q (\"draft:\" + the sorted source_report_ids joined with \"+\")", base, d.ID, want)}
		}
		for k, c := range d.RubricCitations {
			p := fmt.Sprintf("%s/rubric_citations/%d", base, k)
			// (d)
			if !CommsValidRubricID(c.RubricID) {
				return &SemanticError{Message: fmt.Sprintf("%s/rubric_id: %q is not a rubric id (an uppercase letter then digits, never the non-goal shape N<digits>; a non-goal belongs in n_drift)", p, c.RubricID)}
			}
			// (c)
			if k > 0 && d.RubricCitations[k-1].RubricID >= c.RubricID {
				return &SemanticError{Message: fmt.Sprintf("%s: rubric_id %q does not sort strictly after %q; rubric_citations are sorted by rubric_id and unique", p, c.RubricID, d.RubricCitations[k-1].RubricID)}
			}
		}
		if err := checkCommsProposedIssue(base+"/proposed_issue", &d.ProposedIssue); err != nil {
			return err
		}
		// (f)
		for k, id := range d.SourceReportIDs {
			if err := claim(id, fmt.Sprintf("%s/source_report_ids/%d", base, k)); err != nil {
				return err
			}
		}
	}

	for i := range r.NDrift {
		n := &r.NDrift[i]
		base := fmt.Sprintf("/n_drift/%d", i)
		// (e)
		if err := checkCommsSortedIDs(base+"/source_report_ids", n.SourceReportIDs); err != nil {
			return err
		}
		if want := CommsNDriftID(n.NonGoalID, n.SourceReportIDs); n.ID != want {
			return &SemanticError{Message: fmt.Sprintf("%s/id: %q is not the derived id %q (\"ndrift:\" + non_goal_id + \":\" + the sorted source_report_ids joined with \"+\")", base, n.ID, want)}
		}
		// (f)
		for k, id := range n.SourceReportIDs {
			if err := claim(id, fmt.Sprintf("%s/source_report_ids/%d", base, k)); err != nil {
				return err
			}
		}
	}

	// (f)
	for i, nd := range r.NotDrafted {
		if err := claim(nd.ReportID, fmt.Sprintf("/not_drafted/%d/report_id", i)); err != nil {
			return err
		}
	}
	return nil
}

// checkCommsSortedIDs enforces rules (a) and (e)'s ordering half: ids sorted
// strictly ascending by byte order, so unique.
func checkCommsSortedIDs(pointer string, ids []string) error {
	for k := 1; k < len(ids); k++ {
		if ids[k-1] >= ids[k] {
			return &SemanticError{Message: fmt.Sprintf("%s/%d: %q does not sort strictly after %q; source_report_ids are sorted ascending and unique", pointer, k, ids[k], ids[k-1])}
		}
	}
	return nil
}

// checkCommsProposedIssue enforces rules (g), (h) and (i).
func checkCommsProposedIssue(pointer string, pi *CommsProposedIssue) error {
	// (g)
	for _, r := range pi.Title {
		// unicode.IsControl covers CR and LF; U+2028/U+2029 are the separator
		// runes that also break a line.
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return &SemanticError{Message: fmt.Sprintf("%s/title: carries a line break or control rune (%U); a title is one line", pointer, r)}
		}
	}
	if len(pi.Title) > CommsMaxTitleBytes {
		return &SemanticError{Message: fmt.Sprintf("%s/title: %d bytes exceeds the %d-byte cap", pointer, len(pi.Title), CommsMaxTitleBytes)}
	}
	if len(pi.Body) > CommsMaxBodyBytes {
		return &SemanticError{Message: fmt.Sprintf("%s/body: %d bytes exceeds the %d-byte cap", pointer, len(pi.Body), CommsMaxBodyBytes)}
	}
	// (h)
	for k, l := range pi.Labels {
		p := fmt.Sprintf("%s/labels/%d", pointer, k)
		ns := strings.ToLower(strings.TrimSpace(l))
		if strings.HasPrefix(ns, "autonomy:") {
			return &SemanticError{Message: fmt.Sprintf("%s: label %q sets an autonomy tier; a comms draft never proposes autonomy:* (the captain sets delegation, not a user report)", p, l)}
		}
		if !commsAllowedLabelNamespace(ns) {
			return &SemanticError{Message: fmt.Sprintf("%s: label %q is outside the allowed namespaces %s", p, l, strings.Join(CommsLabelNamespaces(), ", "))}
		}
		if !upkeepValidLabel(l) {
			return &SemanticError{Message: fmt.Sprintf(
				"%s: label %q is not a valid label name (non-empty, at most %d characters, no whitespace or control character, no leading or trailing punctuation or symbol)",
				p, l, upkeepMaxLabelLen)}
		}
	}
	// (i)
	if pi.ParentEpic != nil {
		if _, ok := CommsParentEpicNumber(*pi.ParentEpic); !ok {
			return &SemanticError{Message: fmt.Sprintf("%s/parent_epic: %q is not a positive issue number (bare like 389 or #-prefixed like #389)", pointer, *pi.ParentEpic)}
		}
	}
	return nil
}

// commsAllowedLabelNamespace reports whether the lower-cased, trimmed label
// ns opens with one of CommsLabelNamespaces.
func commsAllowedLabelNamespace(ns string) bool {
	for _, p := range CommsLabelNamespaces() {
		if strings.HasPrefix(ns, p) {
			return true
		}
	}
	return false
}
