// Severity-calibration corpus scaffolding (E50.22 / #3309).
//
// The corpus feed for backend/internal/agenteval's severity-calibration
// measurement: given a run's implement_reviewed verdicts and its
// concern-disposition audit entries, it scaffolds ONE candidate
// severity-calibration case (case.json + case.md) carrying each concern the
// operator dispositioned, with `operator_severity` left EMPTY so the
// agenteval loader REFUSES the unlabelled candidate. Selection, labelling
// and committing stay operator curation (#819 / ADR-040) — the tool
// scaffolds a CANDIDATE, not a corpus case.
//
// THE JOIN, AND WHERE THE PLAN'S PREMISE WAS WRONG (verified in the tree at
// this run's base, not taken on report):
//
//   - The disposition payloads DO carry a concern-store `concern_id`:
//     concern_waived and concern_deferred carry {concern_id, prior_state,
//     reason, stage_kind, severity, category} (server/waive.go
//     applyConcernWaive, server/defer_concern.go).
//   - The implement_reviewed payload does NOT. planreview.Concern is
//     {severity, category, note, suggested_patch, provenance} — the
//     concern-store UUID is assigned when the verdict is PERSISTED, and no
//     audit category records that assignment (there is no
//     `concern_recorded` category). So "join implement_reviewed concerns to
//     the dispositions BY concern_id" is not implementable as written: the
//     review side has no such key.
//   - concern_addressed_by_condition carries NEITHER severity NOR category
//     (server/condition_claims.go): its payload is {concern_id,
//     prior_state, approval_sequence, approver_subject,
//     confirming_review_sequence, reviewer_model, verdict,
//     confirming_review_qualified}.
//
// So the join actually implemented is: the DISPOSITION entries are the
// spine and supply the concern_id, the disposition and the disposition
// reason; the implement_reviewed concerns[] are a NOTE CATALOGUE matched
// ONE-TO-ONE by (severity, category) AND BY CHRONOLOGY, consumed in
// ascending audit-sequence order. Sorting ascending makes the catalogue
// COMPLETE before the first disposition consumes from it, which is exactly
// what would otherwise let a disposition reach FORWARD to a review recorded
// after it, so the disposition's own sequence bounds the candidate set. All
// THREE failure directions are LOUD rather than guessed:
//
//   - a disposition whose (severity, category) matches no unconsumed
//     catalogue concern is an error naming the concern_id (the plan's
//     "orphan concern_id" mode, restated against the real key);
//   - a disposition whose only unconsumed (severity, category) matches sit
//     at LATER sequences is a CHRONOLOGY error naming both sequences: a
//     review recorded after the disposition cannot have originated it, and
//     attributing that later reviewer's prose is the same silent corruption
//     the ambiguity mode refuses;
//   - a disposition matching MORE THAN ONE unconsumed catalogue concern is
//     an error naming the concern_id and the count, because attributing one
//     reviewer's prose to the wrong concern would silently corrupt a
//     LABELLED corpus and no key in the chain can disambiguate it. Such a
//     run is operator-curated by hand.
//
// A concern_addressed_by_condition entry is therefore UNJOINABLE from the
// audit chain (it carries no matchable key). It is not dropped and not
// guessed: it is listed in case.md's operator-curation section by
// concern_id so the operator can add it by hand, and it contributes no
// labelled concern.
//
// FREE TEXT: concern notes and disposition reasons are FREE-TEXT OPERATOR
// AND REVIEWER PROSE. They can carry tokens, hostnames, internal paths or
// customer detail regardless of the structured field they travel in, and no
// reusable free-text redactor exists in this repository to route them
// through (backend/internal/diagnostics takes a structured-fields-only
// posture — ClassifyFailureDetail maps a failure reason to a CLASS rather
// than scrubbing prose). This surface therefore makes NO
// redacted-by-construction claim; it renders a point-of-use
// TODO(operator) warning in case.md instead.

package corpusdistill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
)

// Audit categories the severity-calibration join reads. Mirrors
// server.CategoryConcernWaived / CategoryConcernDeferred /
// CategoryConcernAddressedByCondition and planreview's implement_reviewed.
// Duplicated rather than imported because corpusdistill is a dev-tool
// package and must not pull the server into its dependency graph; the
// FetchRunConcernDispositions test drives the same four strings.
const (
	categoryImplementReviewed           = "implement_reviewed"
	categoryConcernWaived               = "concern_waived"
	categoryConcernDeferred             = "concern_deferred"
	categoryConcernAddressedByCondition = "concern_addressed_by_condition"
)

// CalibrationAuditItem is the audit-endpoint item shape this mode reads. It
// differs from AuditItem (the plan-review-miss shape) by carrying
// `category`: that mode requests exactly ONE category so the discriminator
// is implicit, while this mode merges FOUR and must tell an
// implement_reviewed verdict from a disposition entry. The audit endpoint
// serves `category` on every item (server/reads.go auditEntryResponse).
type CalibrationAuditItem struct {
	Sequence  int64           `json:"sequence"`
	RunID     string          `json:"run_id"`
	Timestamp string          `json:"ts"`
	Category  string          `json:"category"`
	Payload   json.RawMessage `json:"payload"`
}

// CalibrationCategories is the ordered category set
// FetchRunConcernDispositions requests, one paged request each.
var CalibrationCategories = []string{
	categoryImplementReviewed,
	categoryConcernWaived,
	categoryConcernDeferred,
	categoryConcernAddressedByCondition,
}

// dispositionForCategory maps an audit category to the agenteval
// disposition string it labels a concern with. The values are members of
// agenteval.CalibrationDispositions; `addressed` and `superseded` have NO
// dedicated audit category and therefore no producer here — a case labelled
// with either is operator-curated (README, and agenteval's enum doc).
var dispositionForCategory = map[string]string{
	categoryConcernWaived:               "waived",
	categoryConcernDeferred:             "deferred",
	categoryConcernAddressedByCondition: "addressed_by_condition",
}

// CalibrationOptions configures a DistillSeverityCalibration /
// PreviewSeverityCalibration call.
type CalibrationOptions struct {
	// CaseName is the case slug and directory name. Required.
	CaseName string
	// Issue is the originating issue/run reference recorded in case.md.
	// Required for provenance.
	Issue string
	// OutDir is the corpus parent directory. Required.
	OutDir string
	// Force permits overwriting an existing case directory.
	Force bool
	// Fetched reports that the items came from FetchRunConcernDispositions
	// (the --run-id path) rather than --in/stdin. It changes only the
	// case.md provenance paragraph — it makes NO redaction claim, because
	// the audit payloads this mode reads carry free-text prose either way.
	Fetched bool
	// Diff is the unified diff the original implement stage produced. NO
	// audit payload carries it, so the operator supplies it via --diff; an
	// empty diff scaffolds a case the agenteval loader refuses (mode (c)),
	// which is the intended TODO signal.
	Diff string
	// PlanSummary is the approved-plan summary the reviewer read, supplied
	// via --plan-summary. Optional.
	PlanSummary string
	// Narrative is optional operator prose appended to case.md.
	Narrative string
}

// CalibrationCaseResult describes one would-be severity-calibration case.
type CalibrationCaseResult struct {
	CaseDir  string
	CaseJSON []byte
	CaseMD   string
	Case     agenteval.SeverityCalibrationCase
	// UnjoinableConcernIDs are concern_addressed_by_condition dispositions
	// whose payload carries no matchable key (see the file comment). They
	// are reported in case.md rather than guessed at.
	UnjoinableConcernIDs []string
}

// reviewPayload is the implement_reviewed subset the join reads.
type reviewPayload struct {
	ReviewerModel string `json:"reviewer_model"`
	Concerns      []struct {
		Severity string `json:"severity"`
		Category string `json:"category"`
		Note     string `json:"note"`
	} `json:"concerns"`
}

// dispositionPayload is the concern_* subset the join reads. severity /
// category are ABSENT on concern_addressed_by_condition; that absence is
// what makes such an entry unjoinable, and it is handled, not guessed.
type dispositionPayload struct {
	ConcernID  string `json:"concern_id"`
	PriorState string `json:"prior_state"`
	Reason     string `json:"reason"`
	StageKind  string `json:"stage_kind"`
	Severity   string `json:"severity"`
	Category   string `json:"category"`
}

// catalogueEntry is one implement_reviewed concern available to be joined.
type catalogueEntry struct {
	sequence      int64
	reviewerModel string
	severity      string
	category      string
	note          string
	consumed      bool
}

// DistillSeverityCalibration joins a run's implement_reviewed concerns to
// its concern-disposition entries and writes ONE candidate case directory
// (<case-name>/{case.json, case.md}) under OutDir, returning the dir.
//
// Fail-loud contract, mirroring DistillPlanReviewMiss: an undecodable
// payload is an error naming the item's sequence, a disposition that cannot
// be joined to exactly one catalogue concern is an error naming the
// concern_id, and ZERO joined concerns is an error, never an empty success.
func DistillSeverityCalibration(items []CalibrationAuditItem, opts CalibrationOptions) (string, error) {
	res, err := prepareSeverityCalibration(items, opts)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(res.CaseDir); statErr == nil {
		if !opts.Force {
			return "", fmt.Errorf("corpusdistill: case dir %q already exists; pass --force to overwrite", res.CaseDir)
		}
		if rmErr := os.RemoveAll(res.CaseDir); rmErr != nil {
			return "", fmt.Errorf("corpusdistill: remove existing case dir %q: %w", res.CaseDir, rmErr)
		}
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("corpusdistill: stat case dir %q: %w", res.CaseDir, statErr)
	}
	if err := os.MkdirAll(res.CaseDir, 0o755); err != nil {
		return "", fmt.Errorf("corpusdistill: create case dir %q: %w", res.CaseDir, err)
	}
	if err := os.WriteFile(filepath.Join(res.CaseDir, "case.json"), res.CaseJSON, 0o644); err != nil {
		return "", fmt.Errorf("corpusdistill: write case.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(res.CaseDir, "case.md"), []byte(res.CaseMD), 0o644); err != nil {
		return "", fmt.Errorf("corpusdistill: write case.md: %w", err)
	}
	return res.CaseDir, nil
}

// PreviewSeverityCalibration computes the would-be case exactly as
// DistillSeverityCalibration does but writes NOTHING — the --dry-run path.
// It surfaces the same validation errors.
func PreviewSeverityCalibration(items []CalibrationAuditItem, opts CalibrationOptions) (CalibrationCaseResult, error) {
	return prepareSeverityCalibration(items, opts)
}

// prepareSeverityCalibration performs the decode/join/render work shared by
// the write and preview paths, with no filesystem effect.
func prepareSeverityCalibration(items []CalibrationAuditItem, opts CalibrationOptions) (CalibrationCaseResult, error) {
	if opts.CaseName == "" {
		return CalibrationCaseResult{}, fmt.Errorf("corpusdistill: CaseName is required")
	}
	if err := validateCaseName(opts.CaseName); err != nil {
		return CalibrationCaseResult{}, err
	}
	if opts.Issue == "" {
		return CalibrationCaseResult{}, fmt.Errorf("corpusdistill: Issue is required")
	}
	if opts.OutDir == "" {
		return CalibrationCaseResult{}, fmt.Errorf("corpusdistill: OutDir is required")
	}

	// Sequence-ascending is the join order: the catalogue must be complete
	// before a disposition consumes from it, and FetchRunConcernDispositions
	// already merges ascending. Sorting here makes the join independent of
	// the caller's ordering (--in/stdin can hand over anything).
	ordered := make([]CalibrationAuditItem, len(items))
	copy(ordered, items)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })

	catalogue, err := buildCatalogue(ordered)
	if err != nil {
		return CalibrationCaseResult{}, err
	}

	var (
		concerns     []agenteval.LabelledConcern
		unjoinable   []string
		runID        string
		stageKind    string
		dispositions int
	)
	for _, item := range ordered {
		disp, ok := dispositionForCategory[item.Category]
		if !ok {
			continue
		}
		var p dispositionPayload
		if err := json.Unmarshal(item.Payload, &p); err != nil {
			return CalibrationCaseResult{}, fmt.Errorf("corpusdistill: decode %s payload at sequence %d: %w", item.Category, item.Sequence, err)
		}
		if strings.TrimSpace(p.ConcernID) == "" {
			return CalibrationCaseResult{}, fmt.Errorf("corpusdistill: %s entry at sequence %d carries no concern_id; the disposition cannot be attributed", item.Category, item.Sequence)
		}
		dispositions++
		if runID == "" {
			runID = item.RunID
		}
		if stageKind == "" {
			stageKind = p.StageKind
		}
		// concern_addressed_by_condition carries neither severity nor
		// category (server/condition_claims.go), so it has no key to join
		// on. Report it; never guess it into a labelled corpus.
		if strings.TrimSpace(p.Severity) == "" || strings.TrimSpace(p.Category) == "" {
			unjoinable = append(unjoinable, p.ConcernID)
			continue
		}
		entry, err := consumeCatalogue(catalogue, p, item.Sequence)
		if err != nil {
			return CalibrationCaseResult{}, err
		}
		concerns = append(concerns, agenteval.LabelledConcern{
			ConcernID:         p.ConcernID,
			ReviewerModel:     entry.reviewerModel,
			Severity:          p.Severity,
			Category:          p.Category,
			Note:              entry.note,
			Disposition:       disp,
			DispositionReason: p.Reason,
			// EMPTY BY DESIGN: the operator's severity label is the thing
			// that makes the corpus LABELLED, and the tool cannot know it.
			// agenteval's loader mode (g) REFUSES an unlabelled candidate.
			OperatorSeverity: "",
		})
	}

	if len(concerns) == 0 {
		if dispositions > 0 {
			return CalibrationCaseResult{}, fmt.Errorf(
				"corpusdistill: %d concern-disposition entr%s found but none could be joined to an implement_reviewed concern (%d unjoinable: %s); nothing to scaffold",
				dispositions, plural(dispositions), len(unjoinable), strings.Join(unjoinable, ", "))
		}
		return CalibrationCaseResult{}, fmt.Errorf("corpusdistill: no concern_waived / concern_deferred / concern_addressed_by_condition entries in the input; nothing to scaffold")
	}

	if runID == "" && len(ordered) > 0 {
		runID = ordered[0].RunID
	}
	c := agenteval.SeverityCalibrationCase{
		Name:        opts.CaseName,
		RunID:       runID,
		Diff:        opts.Diff,
		PlanSummary: opts.PlanSummary,
		Concerns:    concerns,
		// FALSE BY DESIGN: a distilled case is real, not hand-authored. The
		// committed seed fixtures set it true so no reader mistakes them
		// for production cases.
		Synthetic: false,
	}
	caseJSON, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return CalibrationCaseResult{}, fmt.Errorf("corpusdistill: marshal case.json: %w", err)
	}
	caseJSON = append(caseJSON, '\n')

	return CalibrationCaseResult{
		CaseDir:              filepath.Join(opts.OutDir, opts.CaseName),
		CaseJSON:             caseJSON,
		CaseMD:               renderCalibrationCaseMD(opts, c, unjoinable),
		Case:                 c,
		UnjoinableConcernIDs: unjoinable,
	}, nil
}

// buildCatalogue decodes every implement_reviewed entry into the note
// catalogue the join consumes from. An undecodable payload is an error
// naming the item's sequence.
func buildCatalogue(ordered []CalibrationAuditItem) ([]*catalogueEntry, error) {
	var out []*catalogueEntry
	for _, item := range ordered {
		if item.Category != categoryImplementReviewed {
			continue
		}
		var p reviewPayload
		if err := json.Unmarshal(item.Payload, &p); err != nil {
			return nil, fmt.Errorf("corpusdistill: decode implement_reviewed payload at sequence %d: %w", item.Sequence, err)
		}
		for _, cc := range p.Concerns {
			out = append(out, &catalogueEntry{
				sequence:      item.Sequence,
				reviewerModel: p.ReviewerModel,
				severity:      cc.Severity,
				category:      cc.Category,
				note:          cc.Note,
			})
		}
	}
	return out, nil
}

// consumeCatalogue matches a disposition to EXACTLY ONE unconsumed
// catalogue concern by (severity, category) AND by CHRONOLOGY, and marks it
// consumed.
//
// dispSequence is the audit sequence of the disposition entry itself. A
// catalogue concern recorded at a LATER sequence cannot have originated an
// EARLIER disposition — the reviewer had not emitted it yet — so such an
// entry is not a candidate. Without this the join was ordering-correct but
// not chronology-correct: sorting ascending guarantees the catalogue is
// COMPLETE before the first disposition consumes from it, which is exactly
// what lets a disposition at sequence 20 reach forward and consume the sole
// (severity, category) match at sequence 30. On the FetchRunConcernDispositions
// path a disposition is always preceded by its review, so this refuses
// nothing that path produces; it bites on the --in / stdin path, where the
// caller hands over an arbitrary slice of a run's audit chain and a
// truncated window can leave a disposition with only a later review to
// match against.
//
// THREE failure directions, all loud. ZERO eligible matches is the orphan
// mode: a disposition naming a concern no implement_reviewed verdict at or
// before its own sequence carries — reported as the CHRONOLOGY mode when
// the only (severity, category) matches were later ones, because that names
// the actual defect in the input rather than sending the operator looking
// for a missing entry. MORE THAN ONE eligible match is the ambiguity mode:
// the audit chain carries no concern-store id on the review verdict, so
// nothing can say WHICH reviewer note belongs to this concern_id, and
// attributing the wrong prose would silently corrupt a LABELLED corpus.
// None is guessed.
func consumeCatalogue(catalogue []*catalogueEntry, p dispositionPayload, dispSequence int64) (*catalogueEntry, error) {
	var matches, laterOnly []*catalogueEntry
	for _, e := range catalogue {
		if e.consumed {
			continue
		}
		if e.severity != p.Severity || e.category != p.Category {
			continue
		}
		if e.sequence > dispSequence {
			laterOnly = append(laterOnly, e)
			continue
		}
		matches = append(matches, e)
	}
	if len(matches) == 0 && len(laterOnly) > 0 {
		return nil, fmt.Errorf(
			"corpusdistill: disposition at sequence %d names concern_id %s (severity %q, category %q) and the only unconsumed implement_reviewed concerns matching it are at LATER sequences (%v); a review recorded after the disposition cannot have originated it, so the association is chronologically impossible and is refused rather than guessed — widen the audit window so the originating review is included",
			dispSequence, p.ConcernID, p.Severity, p.Category, entrySequences(laterOnly))
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf(
			"corpusdistill: disposition names concern_id %s (severity %q, category %q) but no unconsumed implement_reviewed concern in the input matches it; the review verdict carrying it is missing from the audit items",
			p.ConcernID, p.Severity, p.Category)
	case 1:
		matches[0].consumed = true
		return matches[0], nil
	default:
		return nil, fmt.Errorf(
			"corpusdistill: disposition concern_id %s (severity %q, category %q) matches %d unconsumed implement_reviewed concerns and the audit chain carries no concern id on a review verdict, so the note cannot be attributed; curate this run by hand",
			p.ConcernID, p.Severity, p.Category, len(matches))
	}
}

// calibrationFreeTextWarning is the POINT-OF-USE warning rendered into
// every scaffolded case.md. It is deliberately NOT a
// redacted-by-construction claim: the fields it names are free-text prose
// and this repository has no reusable redactor for them.
const calibrationFreeTextWarning = "TODO(operator): REVIEW FREE TEXT BEFORE COMMITTING\n" +
	"\n" +
	"The `note` on every concern below is REVIEWER prose and the\n" +
	"`disposition_reason` is OPERATOR prose. Both are FREE TEXT: they can\n" +
	"carry tokens, hostnames, internal paths, or customer detail regardless\n" +
	"of the structured field they travel in, and no reusable free-text\n" +
	"redactor exists in this repository to route them through. This case is\n" +
	"COMMITTED TO THE REPOSITORY once you keep it. READ every note and every\n" +
	"disposition_reason yourself before committing, and redact by hand what\n" +
	"should not land."

// renderCalibrationCaseMD produces the candidate case.md: provenance, the
// point-of-use free-text warning, the labelling TODO, and any unjoinable
// dispositions the operator must add by hand.
func renderCalibrationCaseMD(opts CalibrationOptions, c agenteval.SeverityCalibrationCase, unjoinable []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Case: %s\n\n", c.Name)
	b.WriteString(calibrationProvenanceBlock(opts))
	b.WriteString("\n\nScaffolded by `fishhawk-distill-corpus --severity-calibration` (E50.22 /\n" +
		"#3309). One candidate severity-calibration case: each concern below was\n" +
		"raised by an implement review and then DISPOSITIONED by an operator.\n" +
		"Selection, labelling and committing stay operator curation (#819 /\n" +
		"ADR-040) — this is a CANDIDATE, not a corpus case.\n\n")

	b.WriteString("## " + calibrationFreeTextWarning + "\n\n")

	b.WriteString("## TODO(operator): label every concern\n\n" +
		"`operator_severity` is EMPTY on every concern in case.json. It is the\n" +
		"severity YOU judged the concern to be worth when you dispositioned it —\n" +
		"one of `high`, `medium`, `low`. The agenteval loader REFUSES an\n" +
		"unlabelled case, so an unedited candidate cannot silently join the\n" +
		"corpus.\n\n")

	if strings.TrimSpace(c.Diff) == "" {
		b.WriteString("## TODO(operator): supply the diff\n\n" +
			"`diff` is EMPTY: no audit payload carries the implement-stage diff the\n" +
			"reviewer read. Supply it with `--diff <path>` (and the approved-plan\n" +
			"summary with `--plan-summary <path>`), or paste it into case.json. The\n" +
			"loader refuses a case with an empty diff.\n\n")
	}

	fmt.Fprintf(&b, "## Joined concerns (%d)\n\n", len(c.Concerns))
	fmt.Fprintf(&b, "Run %s.\n\n", c.RunID)
	for _, cc := range c.Concerns {
		fmt.Fprintf(&b, "- `%s` — reviewer severity `%s`, category `%s`, disposition `%s`\n",
			cc.ConcernID, cc.Severity, cc.Category, cc.Disposition)
	}

	if len(unjoinable) > 0 {
		fmt.Fprintf(&b, "\n## TODO(operator): %d unjoinable disposition(s)\n\n", len(unjoinable))
		b.WriteString("A `concern_addressed_by_condition` audit payload carries neither\n" +
			"`severity` nor `category`, and an `implement_reviewed` payload carries no\n" +
			"concern id, so these dispositions have NO key to join on. They are\n" +
			"listed rather than guessed; add them by hand if you want them labelled:\n\n")
		for _, id := range unjoinable {
			fmt.Fprintf(&b, "- `%s`\n", id)
		}
	}

	if strings.TrimSpace(opts.Narrative) != "" {
		fmt.Fprintf(&b, "\n## Distilled signal\n\n%s\n", opts.Narrative)
	}
	return b.String()
}

// calibrationProvenanceBlock returns the case.md provenance paragraph. It
// makes NO redaction claim on EITHER path — unlike the plan-review-miss
// mode, whose --run-id fetch reads structured verdict fields only, this
// mode's payloads carry free-text prose however they were sourced.
func calibrationProvenanceBlock(opts CalibrationOptions) string {
	if opts.Fetched {
		return fmt.Sprintf("**Provenance: PRODUCTION.** Distilled from real Fishhawk concern-disposition\n"+
			"audit entries (%s) fetched from the backend. The payloads carry free-text\n"+
			"reviewer and operator prose — see the review warning below.", opts.Issue)
	}
	return fmt.Sprintf("**Provenance: TODO(operator).** Scaffolded from operator-supplied audit\n"+
		"items (%s) via `--in`/stdin, so the tool cannot assert their origin. If\n"+
		"they came from a real run's audit feed, replace this line with\n"+
		"\"Provenance: PRODUCTION\"; if they are hand-authored, state that instead.\n"+
		"Either way the prose below is free text — see the review warning.", opts.Issue)
}

// entrySequences lists a catalogue slice's audit sequences, for the
// chronology error's operator-facing detail.
func entrySequences(es []*catalogueEntry) []int64 {
	out := make([]int64, 0, len(es))
	for _, e := range es {
		out = append(out, e.sequence)
	}
	return out
}
