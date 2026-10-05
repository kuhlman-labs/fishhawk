package plan

import (
	"fmt"
	"path"
	"sort"
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
	// UpkeepSourceAdvisory (#3750): a dependency named by a published
	// vulnerability advisory. Its findings carry an UpkeepAdvisory.
	UpkeepSourceAdvisory = "advisory"
)

// UpkeepSources returns the closed detector-source set, in schema enum
// order: the single Go-side owner of upkeep-report-v1's $defs.source.enum
// (TestUpkeepSources_MatchSchemaEnum pins the two together). A fresh slice
// per call, so a caller cannot mutate the set.
func UpkeepSources() []string {
	return []string{UpkeepSourceFlake, UpkeepSourceToolchainDrift, UpkeepSourceDeprecation, UpkeepSourceAdvisory}
}

// Advisory ecosystems (the advisory object's `ecosystem` enum).
const (
	UpkeepEcosystemGo  = "go"
	UpkeepEcosystemNPM = "npm"
)

// Advisory scanners (the advisory object's `scanner` enum). UpkeepScannerOSV
// is RESERVED: no OSV command is instructed yet (#3750).
const (
	UpkeepScannerGovulncheck = "govulncheck"
	UpkeepScannerPnpmAudit   = "pnpm_audit"
	UpkeepScannerOSV         = "osv"
)

// Advisory reachability classes, strongest first. called/imported/required
// are govulncheck's symbol/package/module finding levels; unanalyzed is
// every scanner that reports no reachability.
const (
	UpkeepReachabilityCalled     = "called"
	UpkeepReachabilityImported   = "imported"
	UpkeepReachabilityRequired   = "required"
	UpkeepReachabilityUnanalyzed = "unanalyzed"
)

// Advisory severities (the advisory object's `severity` enum).
const (
	UpkeepSeverityHigh   = "high"
	UpkeepSeverityMedium = "medium"
	UpkeepSeverityLow    = "low"
)

// Source-degrade reasons (the source-degrade `reason` enum). Only
// UpkeepDegradePartial pairs with the source being in sources_scanned.
const (
	UpkeepDegradeNetworkUnavailable = "network_unavailable"
	UpkeepDegradeToolUnavailable    = "tool_unavailable"
	UpkeepDegradeToolFailed         = "tool_failed"
	UpkeepDegradeBudgetExceeded     = "budget_exceeded"
	UpkeepDegradePartial            = "partial"
)

// UpkeepMaxCallPathFrames mirrors the schema's call_path maxItems. A longer
// govulncheck trace keeps index 0 (the vulnerable end) onward and truncates
// the far end.
const UpkeepMaxCallPathFrames = 32

// upkeepManifestBasenames is the CLOSED set of manifest basenames an
// advisory finding's file refs are matched against (rule q) and the only
// file refs that contribute a coverage directory (UpkeepAdvisoryManifestDirs).
// A call-site source file is allowed as evidence but never names a manifest.
var upkeepManifestBasenames = map[string]bool{
	"go.mod":         true,
	"pnpm-lock.yaml": true,
	"package.json":   true,
}

// UpkeepManifestBasenames returns the closed manifest basename set, sorted.
func UpkeepManifestBasenames() []string {
	out := make([]string, 0, len(upkeepManifestBasenames))
	for b := range upkeepManifestBasenames {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

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
	// SourceDegrades names each source that could not run, or ran only in
	// part (#3750). Optional; absent on every pre-#3750 report.
	SourceDegrades []UpkeepSourceDegrade `json:"source_degrades,omitempty"`
}

// UpkeepSourceDegrade is one named degradation of a source (rule r).
type UpkeepSourceDegrade struct {
	Source string `json:"source"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// UpkeepFinding is one finding: a derived id, its source and subject, the
// evidence for it and the issue it proposes filing.
type UpkeepFinding struct {
	ID       string              `json:"id"`
	Source   string              `json:"source"`
	Subject  string              `json:"subject"`
	Evidence []UpkeepEvidenceRef `json:"evidence"`
	// Advisory is present on, and only on, an advisory finding (rule l).
	Advisory      *UpkeepAdvisory     `json:"advisory,omitempty"`
	ProposedIssue UpkeepProposedIssue `json:"proposed_issue"`
}

// UpkeepAdvisory is an advisory finding's advisory facts (#3750). They are
// AGENT-ASSERTED, copied from the scanner output: the server cannot re-run
// the scanner, so rules (m)-(q) bound the claim instead.
//
// FixedVersion is a pointer with NO omitempty: the schema requires the key,
// and an explicit null is the stated "no fix" — never an absent key.
type UpkeepAdvisory struct {
	Ecosystem    string                `json:"ecosystem"`
	Package      string                `json:"package"`
	Version      string                `json:"version"`
	AdvisoryIDs  []string              `json:"advisory_ids"`
	FixedVersion *string               `json:"fixed_version"`
	Scanner      string                `json:"scanner"`
	Reachability string                `json:"reachability"`
	CallPath     []UpkeepAdvisoryFrame `json:"call_path,omitempty"`
	Severity     string                `json:"severity"`
}

// UpkeepAdvisoryFrame is one govulncheck `-json` trace frame. The field
// names and json tags are govulncheck's own (golang.org/x/vuln
// internal/govulncheck Frame), so a trace is copied VERBATIM; the
// captured-stream test decodes real frames into this type with
// DisallowUnknownFields.
type UpkeepAdvisoryFrame struct {
	Module   string                  `json:"module"`
	Version  string                  `json:"version,omitempty"`
	Package  string                  `json:"package,omitempty"`
	Function string                  `json:"function,omitempty"`
	Receiver string                  `json:"receiver,omitempty"`
	Position *UpkeepAdvisoryPosition `json:"position,omitempty"`
}

// UpkeepAdvisoryPosition is govulncheck's Position, tags verbatim.
type UpkeepAdvisoryPosition struct {
	Filename string `json:"filename,omitempty"`
	Offset   int    `json:"offset"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
}

// UpkeepAdvisoryReachability derives the reachability class from a
// govulncheck call path. callPath[0] is the VULNERABLE end (govulncheck
// orders a trace from the vulnerable symbol toward the entry point), so its
// most specific field decides: a function -> called, else a package ->
// imported, else a module -> required. An empty path (or a frame naming
// nothing) derives "" — no class, which rule (o) refuses.
func UpkeepAdvisoryReachability(callPath []UpkeepAdvisoryFrame) string {
	if len(callPath) == 0 {
		return ""
	}
	switch f := callPath[0]; {
	case f.Function != "":
		return UpkeepReachabilityCalled
	case f.Package != "":
		return UpkeepReachabilityImported
	case f.Module != "":
		return UpkeepReachabilityRequired
	}
	return ""
}

// upkeepManifestDir reports whether a file ref's path names a manifest
// (basename in the closed set, repository-relative, not escaping the root)
// and, if so, the manifest's directory: "." for the repository root, else
// the cleaned slash path ("backend", "site").
func upkeepManifestDir(p string) (string, bool) {
	c := path.Clean(p)
	if p == "" || path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	if !upkeepManifestBasenames[path.Base(c)] {
		return "", false
	}
	return path.Dir(c), true
}

// UpkeepAdvisoryManifestDirs returns the distinct directories of the
// MANIFEST file refs a finding cites, in first-seen order and never nil.
// Only a manifest ref (basename go.mod, pnpm-lock.yaml or package.json)
// contributes; a call-site source file cited as evidence never does. This is
// the directory set Dependabot coverage must reach in full. It is only as
// complete as the agent's manifest citations: a manifest the finding fails to
// cite is never checked, so an under-cited finding can be marked covered by a
// bump that misses that manifest — a FALSE-COVER risk (the unsafe direction),
// unlike the conservative residuals that only leave a finding uncovered.
func UpkeepAdvisoryManifestDirs(f *UpkeepFinding) []string {
	out := []string{}
	if f == nil {
		return out
	}
	seen := map[string]bool{}
	for _, ev := range f.Evidence {
		if ev.Kind != UpkeepEvidenceKindFile {
			continue
		}
		dir, ok := upkeepManifestDir(ev.Path)
		if !ok || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return out
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
//	(l) source advisory <=> an advisory object is present          /findings/<i>/advisory
//	(m) an advisory's subject == advisory_ids[0] + ":" + package   /findings/<i>/subject
//	(n) scanner/ecosystem pairing: govulncheck => go,
//	    pnpm_audit => npm, osv => either                           /findings/<i>/advisory/scanner
//	(o) reachability consistency: govulncheck => a non-empty
//	    call_path, reachability == UpkeepAdvisoryReachability
//	    (call_path) and call_path[0].module == package; pnpm_audit
//	    and osv => unanalyzed with no call_path                    /findings/<i>/advisory/reachability
//	                                                               (/advisory/package for the module)
//	(p) severity cap: called -> any; imported/required -> low;
//	    unanalyzed -> medium or low                                /findings/<i>/advisory/severity
//	(q) an advisory cites at least one MANIFEST file ref
//	    (basename go.mod, pnpm-lock.yaml or package.json)          /findings/<i>/evidence
//	(r) source_degrades: at most one entry per source; reason
//	    partial => the source IS in sources_scanned, any other
//	    reason => it is NOT                                        /source_degrades/<k>
//	(s) a non-null fixed_version is ONE bare version, never a
//	    range: none of < > = ^ ~ | *, whitespace or a comma        /findings/<i>/advisory/fixed_version
//	(t) a report carrying an advisory finding or any
//	    source_degrades entry accounts for the advisory source:
//	    it is in sources_scanned (it ran; a partial run adds a
//	    partial degrade) or named by a non-partial degrade (it
//	    did not run) — with (r), exactly one of the two            /sources_scanned
//
// Rules (l)-(t) fire only on an advisory finding, an advisory object or a
// source_degrades entry, so every pre-#3750 stored report (re-parsed by the
// dispositions capture and the apply) still parses. The residual (t) leaves
// open: a report with neither carries no signal that the advisory source
// exists, so zero findings from a scan that never ran advisories is
// indistinguishable from a pre-#3750 report.
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
		if err := checkUpkeepAdvisory(i, f); err != nil {
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
	if err := checkUpkeepSourceDegrades(r, scanned); err != nil {
		return err
	}
	return checkUpkeepAdvisoryAccounted(r, scanned)
}

// upkeepVersionRangeChars are the characters rule (s) refuses in a
// fixed_version: npm/semver range operators, set unions and wildcards.
// Whitespace and commas are refused separately (hyphen ranges and
// comparator sets need one of them).
const upkeepVersionRangeChars = "<>=^~|*,"

// checkUpkeepAdvisoryAccounted enforces rule (t). Two earlier rules carry
// part of it, so only the remainder is checked here: rule (c) already puts
// the advisory source in sources_scanned whenever the report carries an
// advisory finding, and rule (r) already refuses the source being BOTH
// scanned and degraded as not run. What is left is a source_degrades entry
// with the advisory source in NEITHER place. Past the scanned early return,
// rule (r) guarantees an advisory degrade is a not-run reason (a partial one
// would require the source to be scanned), so any advisory entry accounts.
func checkUpkeepAdvisoryAccounted(r *UpkeepReport, scanned map[string]bool) error {
	if len(r.SourceDegrades) == 0 || scanned[UpkeepSourceAdvisory] {
		return nil
	}
	for _, d := range r.SourceDegrades {
		if d.Source == UpkeepSourceAdvisory {
			return nil
		}
	}
	return &SemanticError{Message: fmt.Sprintf(
		"/sources_scanned: the report carries a source_degrades entry, so it must account for the %q source in exactly one place: list it in sources_scanned %v if it ran (a run over only part of the tree adds a %q degrade), or name it in source_degrades with the reason it did not run; an unaccounted advisory source reads as a clean scan that may never have run",
		UpkeepSourceAdvisory, r.SourcesScanned, UpkeepDegradePartial)}
}

// checkUpkeepAdvisory enforces rules (l) through (q) and (s) on one finding.
func checkUpkeepAdvisory(i int, f *UpkeepFinding) error {
	isAdvisory := f.Source == UpkeepSourceAdvisory
	// Rule (l).
	if isAdvisory != (f.Advisory != nil) {
		if isAdvisory {
			return &SemanticError{Message: fmt.Sprintf(
				"/findings/%d/advisory: an advisory finding must carry an advisory object (ids, package, versions, scanner, reachability, severity)", i)}
		}
		return &SemanticError{Message: fmt.Sprintf(
			"/findings/%d/advisory: only an advisory finding may carry an advisory object; this finding's source is %q", i, f.Source)}
	}
	if !isAdvisory {
		return nil
	}
	a := f.Advisory
	// Rule (m). The schema guarantees advisory_ids is non-empty; the length
	// guard keeps a struct-literal caller from panicking.
	primary := ""
	if len(a.AdvisoryIDs) > 0 {
		primary = a.AdvisoryIDs[0]
	}
	if want := primary + ":" + a.Package; f.Subject != want {
		return &SemanticError{Message: fmt.Sprintf(
			"/findings/%d/subject: an advisory finding's subject %q is not derived from its primary advisory id and package; expected %q (advisory_ids[0] + \":\" + package)",
			i, f.Subject, want)}
	}
	// Rule (n).
	switch {
	case a.Scanner == UpkeepScannerGovulncheck && a.Ecosystem != UpkeepEcosystemGo,
		a.Scanner == UpkeepScannerPnpmAudit && a.Ecosystem != UpkeepEcosystemNPM:
		return &SemanticError{Message: fmt.Sprintf(
			"/findings/%d/advisory/scanner: scanner %q cannot report ecosystem %q (govulncheck scans go, pnpm_audit scans npm, osv either)",
			i, a.Scanner, a.Ecosystem)}
	}
	// Rule (o).
	if a.Scanner == UpkeepScannerGovulncheck {
		derived := UpkeepAdvisoryReachability(a.CallPath)
		if derived == "" {
			return &SemanticError{Message: fmt.Sprintf(
				"/findings/%d/advisory/reachability: a govulncheck finding must carry a non-empty call_path (the finding's trace, copied verbatim) naming at least a module in call_path[0]; reachability is derived from it", i)}
		}
		if a.Reachability != derived {
			return &SemanticError{Message: fmt.Sprintf(
				"/findings/%d/advisory/reachability: reachability %q does not match the level derived from call_path[0] (%q): a function frame is called, a package frame imported, a module frame required",
				i, a.Reachability, derived)}
		}
		if m := a.CallPath[0].Module; m != a.Package {
			return &SemanticError{Message: fmt.Sprintf(
				"/findings/%d/advisory/package: package %q is not the vulnerable module %q named by call_path[0]; for Go the package is the MODULE path, never a package path inside it",
				i, a.Package, m)}
		}
	} else if a.Reachability != UpkeepReachabilityUnanalyzed || len(a.CallPath) > 0 {
		return &SemanticError{Message: fmt.Sprintf(
			"/findings/%d/advisory/reachability: scanner %q reports no reachability, so its finding must be %q with no call_path (got reachability %q and %d call_path frames)",
			i, a.Scanner, UpkeepReachabilityUnanalyzed, a.Reachability, len(a.CallPath))}
	}
	// Rule (p).
	capped := false
	switch a.Reachability {
	case UpkeepReachabilityImported, UpkeepReachabilityRequired:
		capped = a.Severity != UpkeepSeverityLow
	case UpkeepReachabilityUnanalyzed:
		capped = a.Severity == UpkeepSeverityHigh
	}
	if capped {
		return &SemanticError{Message: fmt.Sprintf(
			"/findings/%d/advisory/severity: severity %q exceeds the cap for reachability %q (only called may be high; imported and required are at most low; unanalyzed is at most medium)",
			i, a.Severity, a.Reachability)}
	}
	// Rule (q).
	if len(UpkeepAdvisoryManifestDirs(f)) == 0 {
		return &SemanticError{Message: fmt.Sprintf(
			"/findings/%d/evidence: an advisory finding must cite at least one manifest file ref (a repository-relative path with basename %s) pinning the affected version; a call-site source file alone names no manifest",
			i, strings.Join(UpkeepManifestBasenames(), ", "))}
	}
	// Rule (s). A range would make "covered by a bump to at least the fix"
	// undecidable and reads to the captain as a fix that is not one version.
	if fv := a.FixedVersion; fv != nil {
		if strings.ContainsAny(*fv, upkeepVersionRangeChars) || strings.IndexFunc(*fv, unicode.IsSpace) >= 0 {
			return &SemanticError{Message: fmt.Sprintf(
				"/findings/%d/advisory/fixed_version: fixed_version %q is a range, not ONE bare version: report the lowest patched version on the in-use major line (no %s, whitespace or comma), or null when no fix is published",
				i, *fv, strings.TrimSuffix(upkeepVersionRangeChars, ","))}
		}
	}
	return nil
}

// checkUpkeepSourceDegrades enforces rule (r).
func checkUpkeepSourceDegrades(r *UpkeepReport, scanned map[string]bool) error {
	seen := make(map[string]int, len(r.SourceDegrades))
	for k, d := range r.SourceDegrades {
		pointer := fmt.Sprintf("/source_degrades/%d", k)
		if prev, dup := seen[d.Source]; dup {
			return &SemanticError{Message: fmt.Sprintf(
				"%s: source %q is already degraded by /source_degrades/%d; at most one entry per source", pointer, d.Source, prev)}
		}
		seen[d.Source] = k
		partial := d.Reason == UpkeepDegradePartial
		if partial && !scanned[d.Source] {
			return &SemanticError{Message: fmt.Sprintf(
				"%s: a %q degrade means source %q ran in part, so it must appear in sources_scanned %v", pointer, d.Reason, d.Source, r.SourcesScanned)}
		}
		if !partial && scanned[d.Source] {
			return &SemanticError{Message: fmt.Sprintf(
				"%s: source %q is degraded %q (it did not run), so it must NOT appear in sources_scanned %v; a source that ran in part is degraded %q",
				pointer, d.Source, d.Reason, r.SourcesScanned, UpkeepDegradePartial)}
		}
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
	if pi.ParentEpic != nil && !UpkeepValidEpicRef(*pi.ParentEpic) {
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

// UpkeepValidEpicRef reports whether s is a positive issue number, bare or
// with ONE leading `#`. Stricter than workmgmt.groomingEpicRef (no whitespace
// trim): the report is machine-written, so a padded value is a defect.
// Exported (#3923) so the upkeep-dispositions capture validates a captain's
// parent_epic override with the report's own rule (j) instead of a copy.
func UpkeepValidEpicRef(s string) bool {
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
