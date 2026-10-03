package plan

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// KindUpkeepReport is the top-level discriminator value carried by an
// upkeep_report artifact — the THIRD additive sibling of the plan artifact
// (E79 / #3726, contract #3921), after clarification_request and
// grooming_report.
//
// It is an untyped string const, NOT an ArtifactKind, on purpose: declaring
// the ArtifactKind obliges AllArtifactKinds (TestAllArtifactKinds_
// EnumeratesEveryDeclaredConst), and that obliges the server's settle-table
// row, which needs the ingest handler. The ArtifactKind, the schema embed and
// ValidateUpkeepReport/ParseUpkeepReport land with that handler (validate.go);
// this file is schema-free so it can land first.
const KindUpkeepReport = "upkeep_report"

// UpkeepReportVersion is the artifact's schema_version token, mirroring the
// canonical docs/spec/upkeep-report-v1.schema.json filename stem and the
// workflow-spec `schema:` an upkeep_report-producing stage must declare
// (spec.UpkeepReportSchemaVersion).
const UpkeepReportVersion = "upkeep_report_v1"

// Detector sources. Each is the leading segment of a derived finding id.
const (
	UpkeepSourceFlake          = "flake"
	UpkeepSourceToolchainDrift = "toolchain_drift"
	UpkeepSourceDeprecation    = "deprecation"
)

// Evidence-ref discriminators (the evidence-ref oneOf's `kind` const).
const (
	UpkeepEvidenceKindRun  = "run"
	UpkeepEvidenceKindFile = "file"
)

// UpkeepMaxDistinctRunRefs bounds the distinct run ids one report may cite
// (rule k). The ingest makes one run lookup per distinct id, so this is the
// per-ingest lookup ceiling.
const UpkeepMaxDistinctRunRefs = 200

// upkeepMaxLabelLen mirrors workmgmt.groomingMaxLabelLen (rule i).
const upkeepMaxLabelLen = 50

// UpkeepReport is a decoded upkeep_report artifact: the maintenance findings
// a `plan`-typed propose stage declaring `produces: upkeep_report` emits.
// JSON tags mirror upkeep-report-v1.schema.json. SourcesScanned records what
// the scan COVERED, so an empty Findings reads "none found", not "not
// scanned".
type UpkeepReport struct {
	Kind            string          `json:"kind"`
	ReportVersion   string          `json:"report_version"`
	TicketReference TicketReference `json:"ticket_reference"`
	GeneratedBy     GeneratedBy     `json:"generated_by"`
	Summary         string          `json:"summary"`
	SourcesScanned  []string        `json:"sources_scanned"`
	Findings        []UpkeepFinding `json:"findings"`
}

// UpkeepFinding is one finding: a derived id, its source and subject, the
// evidence for it and the issue it proposes filing.
type UpkeepFinding struct {
	ID            string              `json:"id"`
	Source        string              `json:"source"`
	Subject       string              `json:"subject"`
	Evidence      []UpkeepEvidenceRef `json:"evidence"`
	ProposedIssue UpkeepProposedIssue `json:"proposed_issue"`
}

// UpkeepEvidenceRef is one evidence pointer, discriminated by Kind: a run ref
// (RunID, StageID, TraceRef) or a file ref (Path, Line, Value). Detail is
// shared.
//
// StageID and Line are pointers so absent and empty/zero stay distinct: a
// present-but-empty stage_id must fail rule (e), not read as absent.
// Only RunID is verified against the run store on ingest; StageID and
// TraceRef are agent-asserted and never resolved.
type UpkeepEvidenceRef struct {
	Kind     string  `json:"kind"`
	RunID    string  `json:"run_id,omitempty"`
	StageID  *string `json:"stage_id,omitempty"`
	TraceRef string  `json:"trace_ref,omitempty"`
	Path     string  `json:"path,omitempty"`
	Line     *int    `json:"line,omitempty"`
	Value    string  `json:"value,omitempty"`
	Detail   string  `json:"detail,omitempty"`
}

// UpkeepProposedIssue is the issue a finding proposes filing. ParentEpic is a
// pointer so a present-but-malformed value is checked by rule (j).
type UpkeepProposedIssue struct {
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Type       string   `json:"type"`
	Labels     []string `json:"labels"`
	ParentEpic *string  `json:"parent_epic,omitempty"`
}

// UpkeepFindingID is the single owner of the finding-id derivation:
// `<source>:<subject>`. Derived, never minted per run, so two reports over the
// same tree diff mechanically and a filed issue's hidden marker keeps
// matching.
func UpkeepFindingID(source, subject string) string {
	return source + ":" + subject
}

// RunRefIDs returns the report's distinct cited run ids in first-seen order.
// Identity is the parsed UUID, so two spellings of one id count once. A
// run_id that does not parse, or is the nil UUID, is skipped: rule (e)
// rejects such a report, so on a validated report nothing is skipped.
func (r *UpkeepReport) RunRefIDs() []uuid.UUID {
	if r == nil {
		return nil
	}
	seen := map[uuid.UUID]struct{}{}
	out := []uuid.UUID{}
	for _, f := range r.Findings {
		for _, ev := range f.Evidence {
			if ev.Kind != UpkeepEvidenceKindRun {
				continue
			}
			id, err := uuid.Parse(ev.RunID)
			if err != nil || id == uuid.Nil {
				continue
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// CheckUpkeepReportSemantics enforces the rules upkeep-report-v1.schema.json
// cannot express (docs/spec/upkeep-report-v1.md § "Semantic rules"). It runs
// AFTER the schema check; every violation is a *SemanticError naming the JSON
// pointer:
//
//	(a) id == UpkeepFindingID(source, subject)                     /findings/<i>/id
//	(b) ids are unique report-wide                                 /findings/<i>/id
//	(c) every finding's source appears in sources_scanned          /findings/<i>/source
//	(d) subject carries no control rune                            /findings/<i>/subject
//	(e) a run ref's run_id (and stage_id when present) is a
//	    non-nil UUID                                               /findings/<i>/evidence/<j>/run_id
//	(f) a flake cites at least one run ref                         /findings/<i>/evidence
//	(g) a toolchain_drift's lined file refs name >= 2 DISTINCT
//	    paths                                                      /findings/<i>/evidence
//	(h) a deprecation cites at least one file ref                  /findings/<i>/evidence
//	(i) each label passes the grooming label rule                  /findings/<i>/proposed_issue/labels/<k>
//	(j) parent_epic, when present, is a positive integer, bare or
//	    `#`-prefixed                                               /findings/<i>/proposed_issue/parent_epic
//	(k) at most UpkeepMaxDistinctRunRefs distinct run ids          /findings
//
// Evidence COUNT is the schema's job (minItems 1) and is deliberately not
// re-checked here, so a schema regression is not masked by this layer.
func CheckUpkeepReportSemantics(r *UpkeepReport) error {
	if r == nil {
		return &SemanticError{Message: "/: upkeep report is nil"}
	}
	scanned := make(map[string]bool, len(r.SourcesScanned))
	for _, s := range r.SourcesScanned {
		scanned[s] = true
	}
	seenIDs := make(map[string]int, len(r.Findings))
	for i := range r.Findings {
		f := &r.Findings[i]
		// Rule (c).
		if !scanned[f.Source] {
			return &SemanticError{Message: fmt.Sprintf(
				"/findings/%d/source: source %q does not appear in sources_scanned %v; a report cannot carry a finding from a detector it did not run",
				i, f.Source, r.SourcesScanned)}
		}
		// Rule (d).
		if strings.IndexFunc(f.Subject, unicode.IsControl) >= 0 {
			return &SemanticError{Message: fmt.Sprintf(
				"/findings/%d/subject: subject %q carries a control character; the subject is the finding id's suffix and the dedupe marker must match it exactly",
				i, f.Subject)}
		}
		// Rule (a).
		if want := UpkeepFindingID(f.Source, f.Subject); f.ID != want {
			return &SemanticError{Message: fmt.Sprintf(
				"/findings/%d/id: finding id %q is not derived from its source and subject; expected %q. Finding ids MUST be derived as <source>:<subject>, never minted per run",
				i, f.ID, want)}
		}
		// Rule (b).
		if prev, dup := seenIDs[f.ID]; dup {
			return &SemanticError{Message: fmt.Sprintf(
				"/findings/%d/id: duplicate finding id %q (already used by /findings/%d); the id is the join key from report to dedupe marker to filed issue, so a duplicate is ambiguous",
				i, f.ID, prev)}
		}
		seenIDs[f.ID] = i
		if err := checkUpkeepEvidence(i, f); err != nil {
			return err
		}
		if err := checkUpkeepProposedIssue(i, &f.ProposedIssue); err != nil {
			return err
		}
	}
	// Rule (k). After rule (e), so every cited run_id parses and RunRefIDs
	// skips nothing.
	if n := len(r.RunRefIDs()); n > UpkeepMaxDistinctRunRefs {
		return &SemanticError{Message: fmt.Sprintf(
			"/findings: the report cites %d distinct run ids; at most %d are allowed (each is looked up on ingest)",
			n, UpkeepMaxDistinctRunRefs)}
	}
	return nil
}

// checkUpkeepEvidence enforces rule (e) per run ref and the per-source shape
// rules (f), (g) and (h).
func checkUpkeepEvidence(i int, f *UpkeepFinding) error {
	runRefs, fileRefs := 0, 0
	linedPaths := map[string]bool{}
	for j, ev := range f.Evidence {
		switch ev.Kind {
		case UpkeepEvidenceKindRun:
			runRefs++
			if err := checkUpkeepUUID(fmt.Sprintf("/findings/%d/evidence/%d/run_id", i, j), ev.RunID); err != nil {
				return err
			}
			if ev.StageID != nil {
				if err := checkUpkeepUUID(fmt.Sprintf("/findings/%d/evidence/%d/stage_id", i, j), *ev.StageID); err != nil {
					return err
				}
			}
		case UpkeepEvidenceKindFile:
			fileRefs++
			if ev.Line != nil {
				linedPaths[ev.Path] = true
			}
		}
	}
	pointer := fmt.Sprintf("/findings/%d/evidence", i)
	switch f.Source {
	case UpkeepSourceFlake:
		// Rule (f).
		if runRefs == 0 {
			return &SemanticError{Message: pointer + ": a flake finding must cite at least one run ref (kind: run); a flake is observed in recorded runs"}
		}
	case UpkeepSourceToolchainDrift:
		// Rule (g).
		if len(linedPaths) < 2 {
			return &SemanticError{Message: fmt.Sprintf(
				"%s: a toolchain_drift finding must name at least 2 DISTINCT paths among its file refs that carry a line (got %d); drift is one tool pinned differently in different files",
				pointer, len(linedPaths))}
		}
	case UpkeepSourceDeprecation:
		// Rule (h).
		if fileRefs == 0 {
			return &SemanticError{Message: pointer + ": a deprecation finding must cite at least one file ref (kind: file) naming where the deprecated use is"}
		}
	}
	return nil
}

// checkUpkeepUUID is rule (e) for one id: it parses and is not the nil UUID.
func checkUpkeepUUID(pointer, s string) error {
	id, err := uuid.Parse(s)
	if err != nil {
		return &SemanticError{Message: fmt.Sprintf("%s: %q is not a UUID: %v", pointer, s, err)}
	}
	if id == uuid.Nil {
		return &SemanticError{Message: pointer + ": the nil UUID names no run or stage"}
	}
	return nil
}

// checkUpkeepProposedIssue enforces rules (i) and (j).
func checkUpkeepProposedIssue(i int, pi *UpkeepProposedIssue) error {
	for k, l := range pi.Labels {
		if !upkeepValidLabel(l) {
			return &SemanticError{Message: fmt.Sprintf(
				"/findings/%d/proposed_issue/labels/%d: label %q is not a valid label name (non-empty, at most %d characters, no whitespace or control character, no leading or trailing punctuation or symbol)",
				i, k, l, upkeepMaxLabelLen)}
		}
	}
	if pi.ParentEpic != nil && !upkeepValidEpicRef(*pi.ParentEpic) {
		return &SemanticError{Message: fmt.Sprintf(
			"/findings/%d/proposed_issue/parent_epic: %q is not a positive issue number (bare like 389 or #-prefixed like #389)",
			i, *pi.ParentEpic)}
	}
	return nil
}

// upkeepValidLabel is a self-contained copy of workmgmt.groomingValidLabel
// (backend/internal/workmgmt/grooming_apply.go). It is COPIED, not imported,
// because workmgmt imports plan; keep the two in step.
func upkeepValidLabel(name string) bool {
	if name == "" {
		return false
	}
	runes := []rune(name)
	if len(runes) > upkeepMaxLabelLen {
		return false
	}
	for _, r := range runes {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	for _, r := range []rune{runes[0], runes[len(runes)-1]} {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return false
		}
	}
	return true
}

// upkeepValidEpicRef reports whether s is a positive issue number, bare or
// with ONE leading `#`. Stricter than workmgmt.groomingEpicRef (no whitespace
// trim): the report is machine-written, so a padded value is a defect.
func upkeepValidEpicRef(s string) bool {
	digits := strings.TrimPrefix(s, "#")
	// The digit walk is what rejects a sign ("+12"), which strconv.Atoi
	// would accept; Atoi then bounds the value and rejects "" and overflow.
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(digits)
	return err == nil && n > 0
}
