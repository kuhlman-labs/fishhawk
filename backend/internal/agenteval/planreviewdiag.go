// Live-run diagnostics for the plan-review catch-rate recorder (E55.21 /
// #4350): every live catch-rate run leaves a per-trial JSON Lines file,
// planreview-diag.jsonl, whether it passes, is refused or aborts.
//
// The file is DIAGNOSTIC ONLY. Nothing the gate reads (catchrategate,
// CheckCatchRateEvidence, the committed evidence pair) ever reads it, the
// miss classes never change which trials count as caught, and the observer
// that feeds it is carried on the context (the net/http/httptrace pattern),
// so the measured arms run byte-identically with or without it
// (TestRecordCatchRateEvidenceWithDiagnostics_EvidenceUnchanged).
//
// The file is written line by line as each trial completes (one unbuffered
// Write per line), so a killed or timed-out run still leaves every trial it
// finished: the header line first, one trial line per trial, and a summary
// line on every outcome RecordCatchRateEvidenceWithDiagnostics returns.
//
// This file is deliberately NOT a scripts/check-review-prompt-eval trigger
// path: it changes no prompt, corpus, probe or classifier. Its subset
// selection (CatchRateSubset.forRow, selectCatchCases) runs on the recording
// path in its INACTIVE form, which returns the row's own samples and cases
// unchanged; the guard that keeps a subset run from recording lives in the
// trigger file planreviewevidence.go.

package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/redaction"
)

const (
	// CatchRateDiagSchema names the diagnostics line format.
	CatchRateDiagSchema = "planreview-catchrate-diag-v1"
	// CatchRateDiagFileName is the diagnostics file's base name.
	CatchRateDiagFileName = "planreview-diag.jsonl"

	// CatchRateDiagDirEnv overrides the diagnostics root directory.
	CatchRateDiagDirEnv = "FISHHAWK_AGENTEVAL_DIAG_DIR"
	// CatchRateSubsetCasesEnv is a comma-separated case-id list: a subset dry
	// run measures only those cases.
	CatchRateSubsetCasesEnv = "FISHHAWK_AGENTEVAL_PLANREVIEW_CASES"
	// CatchRateSubsetTrialsEnv is a positive per-case sample count that
	// replaces every arm's floor-clearing one.
	CatchRateSubsetTrialsEnv = "FISHHAWK_AGENTEVAL_PLANREVIEW_TRIALS"

	// catchDiagTimestampLayout names a run's diagnostics subdirectory (UTC).
	catchDiagTimestampLayout = "20060102T150405Z"
	// catchDiagRedacted replaces an sk-ant token the earlier passes missed. It
	// carries no sk-ant prefix, so a scrubbed file never contains the
	// substring.
	catchDiagRedacted = "[REDACTED:anthropic-token]"
	// catchDiagUndecodableVerdict is a trial line's verdict when the response
	// decodes to no JSON object.
	catchDiagUndecodableVerdict = "undecodable"
)

// catchDiagSkAntRe is the last scrub: any sk-ant token, whatever its shape
// or length (redaction.DefaultPatterns matches only sk-ant-api03 keys of 40+
// characters, so an OAuth sk-ant-oat01 token passes it).
var catchDiagSkAntRe = regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]*`)

// ErrCatchRateSubsetNotRecordable marks the refusal of a subset or
// reduced-trial run that would record or pin: it measured part of the arm
// table, so it can never be evidence.
var ErrCatchRateSubsetNotRecordable = errors.New("a subset or reduced-trial catch-rate run is never recordable")

// CatchMissClass is the DIAGNOSTIC class of a missed catch-case trial. It
// separates a reviewer miss (the reviewer never raised the defect) from a
// scoring miss (it did, but the rule did not credit it). It never changes
// which trials count as caught.
type CatchMissClass string

const (
	// CatchMissNotMentioned means no concern matched the probe half or the
	// attribution half — a reviewer miss (a prompt problem).
	CatchMissNotMentioned CatchMissClass = "not_mentioned"
	// CatchMissUnattributed means a concern matched a probe but named neither a
	// planted criterion id nor an attribution phrase — a scoring miss.
	CatchMissUnattributed CatchMissClass = "unattributed"
	// CatchMissNoProbe means a concern named the planted criterion but matched no
	// probe — a scoring miss.
	CatchMissNoProbe CatchMissClass = "no_probe"
	// CatchMissWrongLabel means a concern matched both halves but its label failed
	// the rule's category predicate (the architect arm) — a scoring miss.
	CatchMissWrongLabel CatchMissClass = "category_mismatch"
	// CatchMissUndecodable means the response carried no decodable verdict.
	CatchMissUndecodable CatchMissClass = "undecodable"
	// CatchMissTruncated means the generate call stopped at max_tokens, so no
	// response was scored (an undecodable trial marked Truncated).
	CatchMissTruncated CatchMissClass = "truncated"
)

// classifyCatchMiss returns a catch-case trial's miss class: "" for a clean
// rule or a caught trial, CatchMissTruncated for an undecodable trial marked
// Truncated, CatchMissUndecodable for any other undecodable one. A
// missed trial's concerns are read with the classifier's own matcher helpers,
// in precedence order: a concern matching both halves under the wrong label
// (CatchMissWrongLabel), else a probe without attribution
// (CatchMissUnattributed), else an attribution without a probe
// (CatchMissNoProbe), else CatchMissNotMentioned.
func classifyCatchMiss(tr CatchTrial, rule CatchRule) CatchMissClass {
	if rule.Clean {
		return ""
	}
	switch tr.Outcome {
	case CatchUndecodable:
		if tr.Truncated {
			return CatchMissTruncated
		}
		return CatchMissUndecodable
	case CatchMissed:
	default:
		return ""
	}
	var wrongLabel, unattributed, noProbe bool
	for _, c := range tr.Concerns {
		_, probed := matchCatchProbe(c.Category, c.Note, rule.Probes)
		_, attributed := matchCatchAttribution(c.Category, c.Note, rule)
		switch {
		case probed && attributed && !matchCatchCategory(c.Category, rule):
			wrongLabel = true
		case probed && !attributed:
			unattributed = true
		case attributed && !probed:
			noProbe = true
		}
	}
	switch {
	case wrongLabel:
		return CatchMissWrongLabel
	case unattributed:
		return CatchMissUnattributed
	case noProbe:
		return CatchMissNoProbe
	default:
		return CatchMissNotMentioned
	}
}

// CatchRateSubset selects part of the arm table for a dry run. A subset is
// ACTIVE when either knob is set, whatever its value: listing every case, or
// a trial count at or above the floor, is still never recordable.
type CatchRateSubset struct {
	// Cases are the selected case ids (any arm's); empty = every case.
	Cases []string
	// TrialsPerCase replaces every arm's per-case sample count; 0 = keep it.
	TrialsPerCase int
}

func (s CatchRateSubset) active() bool { return len(s.Cases) > 0 || s.TrialsPerCase > 0 }

// ParseCatchRateSubset reads the two subset knobs through getenv. Unset knobs
// give the inactive subset. It refuses a case list that is blank after
// splitting on commas and trimming, and a trial count that is not a positive
// integer. Case ids are de-duplicated in first-seen order.
func ParseCatchRateSubset(getenv func(string) string) (CatchRateSubset, error) {
	var s CatchRateSubset
	if raw := getenv(CatchRateSubsetCasesEnv); raw != "" {
		seen := map[string]bool{}
		for _, part := range strings.Split(raw, ",") {
			id := strings.TrimSpace(part)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			s.Cases = append(s.Cases, id)
		}
		if len(s.Cases) == 0 {
			return CatchRateSubset{}, fmt.Errorf("agenteval: %s=%q names no case id (want a comma-separated list of case directory names)", CatchRateSubsetCasesEnv, raw)
		}
	}
	if raw := getenv(CatchRateSubsetTrialsEnv); raw != "" {
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || n < 1 {
			return CatchRateSubset{}, fmt.Errorf("agenteval: %s=%q is not a positive integer", CatchRateSubsetTrialsEnv, raw)
		}
		s.TrialsPerCase = n
	}
	return s, nil
}

// validate refuses a selected case id that matches no arm-table case.
func (s CatchRateSubset) validate(rows []catchRateArmRow) error {
	known := map[string]bool{}
	for _, row := range rows {
		for _, c := range row.cases {
			known[c.name] = true
		}
	}
	var unknown []string
	for _, id := range s.Cases {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("%s names case id(s) no arm runs: %s", CatchRateSubsetCasesEnv, strings.Join(unknown, ", "))
	}
	return nil
}

// forRow is one arm's share of the subset: its per-case samples, the case
// filter (nil = every case) and whether the arm runs at all. An inactive
// subset returns (row.samples, nil, true), so the recording path is
// unchanged; an arm none of whose cases is selected does not run.
func (s CatchRateSubset) forRow(row catchRateArmRow) (samples int, keep func(string) bool, ok bool) {
	if !s.active() {
		return row.samples, nil, true
	}
	samples = row.samples
	if s.TrialsPerCase > 0 {
		samples = s.TrialsPerCase
	}
	if len(s.Cases) == 0 {
		return samples, nil, true
	}
	selected := make(map[string]bool, len(s.Cases))
	for _, id := range s.Cases {
		selected[id] = true
	}
	runs := false
	for _, c := range row.cases {
		if selected[c.name] {
			runs = true
			break
		}
	}
	if !runs {
		return 0, nil, false
	}
	return samples, func(name string) bool { return selected[name] }, true
}

// selectCatchCases keeps the cases keep selects, in order; a nil keep returns
// cases unchanged.
func selectCatchCases[C any](cases []C, keep func(string) bool, name func(C) string) []C {
	if keep == nil {
		return cases
	}
	var out []C
	for _, c := range cases {
		if keep(name(c)) {
			out = append(out, c)
		}
	}
	return out
}

// ResolveCatchRateDiagPath is where a run's diagnostics go:
// $FISHHAWK_AGENTEVAL_DIAG_DIR/<UTC 20060102T150405Z>/planreview-diag.jsonl
// when the knob is set, else
// os.TempDir()/fishhawk-agenteval/<UTC 20060102T150405Z>/planreview-diag.jsonl.
// The per-run timestamp directory lets repeated runs share one root without
// tripping OpenCatchRateDiag's no-clobber rule. It refuses any path with a
// testdata element: the file is never committed evidence.
func ResolveCatchRateDiagPath(getenv func(string) string, now time.Time) (string, error) {
	stamp := now.UTC().Format(catchDiagTimestampLayout)
	root := filepath.Join(os.TempDir(), "fishhawk-agenteval")
	if dir := getenv(CatchRateDiagDirEnv); dir != "" {
		root = dir
	}
	abs, err := filepath.Abs(filepath.Join(root, stamp, CatchRateDiagFileName))
	if err != nil {
		return "", fmt.Errorf("agenteval: resolve the catch-rate diagnostics path: %w", err)
	}
	if catchDiagUnderTestdata(abs) {
		return "", fmt.Errorf("agenteval: catch-rate diagnostics path %q is under a testdata directory; diagnostics are never committed — point %s elsewhere", abs, CatchRateDiagDirEnv)
	}
	return abs, nil
}

// catchDiagUnderTestdata reports whether the cleaned path has a testdata
// element.
func catchDiagUnderTestdata(abs string) bool {
	for _, el := range strings.Split(filepath.ToSlash(filepath.Clean(abs)), "/") {
		if el == "testdata" {
			return true
		}
	}
	return false
}

// catchDiagCredentialEnvs are the variables whose values are redacted from
// the diagnostics by value.
var catchDiagCredentialEnvs = []string{
	"FISHHAWKD_ANTHROPIC_API_KEY",
	"FISHHAWKD_ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
}

// CatchRateDiagCredentials binds every set credential variable for
// known-value redaction.
func CatchRateDiagCredentials(getenv func(string) string) []redaction.KnownValue {
	var out []redaction.KnownValue
	for _, name := range catchDiagCredentialEnvs {
		if v := getenv(name); v != "" {
			out = append(out, redaction.KnownValue{Name: name, Value: v})
		}
	}
	return out
}

// CatchRateDiagHeader is the header line's run description.
type CatchRateDiagHeader struct {
	StartedAt           string   `json:"started_at,omitempty"`
	GeneratorModel      string   `json:"generator_model,omitempty"`
	DryRun              bool     `json:"dry_run"`
	PinBaseline         bool     `json:"pin_baseline"`
	SubsetCases         []string `json:"subset_cases,omitempty"`
	SubsetTrialsPerCase int      `json:"subset_trials_per_case,omitempty"`
	Arms                []string `json:"arms"`
}

// CatchRateDiagOptions configures a diagnostics writer.
type CatchRateDiagOptions struct {
	// Progress receives one line per trial and any write warning; nil = none.
	Progress io.Writer
	// Credentials are redacted from every line by value.
	Credentials []redaction.KnownValue
	// Header describes the run.
	Header CatchRateDiagHeader
}

// CatchRateDiag is a run's diagnostics writer. Its methods are safe for
// concurrent use. A write error is sticky: later lines are skipped, a
// warning goes to Progress, and Finish returns the error; the run is never
// aborted for it.
type CatchRateDiag struct {
	mu       sync.Mutex
	path     string
	w        io.WriteCloser
	progress io.Writer
	known    *redaction.KnownValues
	rules    map[string]map[string]catchRateArmCase
	err      error
	trials   int
	arms     map[string]*catchDiagArmAgg
	// notJudged marks a subset dry run: its summary outcome is not_judged.
	notJudged bool
	finished  bool
}

// catchDiagRules indexes an arm table's per-(arm, case) rules.
func catchDiagRules(rows []catchRateArmRow) map[string]map[string]catchRateArmCase {
	out := make(map[string]map[string]catchRateArmCase, len(rows))
	for _, row := range rows {
		m := make(map[string]catchRateArmCase, len(row.cases))
		for _, c := range row.cases {
			m[c.name] = c
		}
		out[row.name] = m
	}
	return out
}

// OpenCatchRateDiag creates the diagnostics file at path (its directory
// first) and writes the header line. It REFUSES to clobber: an existing file
// at path is an error and is left byte-identical.
func OpenCatchRateDiag(path string, opts CatchRateDiagOptions, rules map[string]map[string]catchRateArmCase) (*CatchRateDiag, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("agenteval: catch-rate diagnostics: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("agenteval: catch-rate diagnostics (an earlier run's file is never overwritten): %w", err)
	}
	return newCatchRateDiag(f, path, opts, rules)
}

// newCatchRateDiag is OpenCatchRateDiag over any writer (a test seam).
func newCatchRateDiag(w io.WriteCloser, path string, opts CatchRateDiagOptions, rules map[string]map[string]catchRateArmCase) (*CatchRateDiag, error) {
	known, below := redaction.NewKnownValues(opts.Credentials...)
	d := &CatchRateDiag{
		path: path, w: w, progress: opts.Progress, known: known, rules: rules,
		arms: map[string]*catchDiagArmAgg{},
	}
	for _, name := range below {
		d.warn(fmt.Sprintf("credential binding %s is shorter than %d bytes and is not redacted by value", name, redaction.MinKnownValueBytes))
	}
	h := opts.Header
	if h.Arms == nil {
		h.Arms = []string{}
		for arm := range rules {
			h.Arms = append(h.Arms, arm)
		}
		sort.Strings(h.Arms)
	}
	d.writeLine(struct {
		Kind   string `json:"kind"`
		Schema string `json:"schema"`
		CatchRateDiagHeader
	}{"header", CatchRateDiagSchema, h})
	return d, nil
}

// Path is the diagnostics file's path.
func (d *CatchRateDiag) Path() string { return d.path }

func (d *CatchRateDiag) warn(msg string) {
	if d.progress != nil {
		_, _ = fmt.Fprintf(d.progress, "planreview diagnostics: %s\n", msg)
	}
}

// writeLine marshals v and writes it as one line after known-value
// redaction and the sk-ant scrub. The caller must not hold d.mu.
func (d *CatchRateDiag) writeLine(v any) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeLineLocked(v)
}

func (d *CatchRateDiag) writeLineLocked(v any) bool {
	if d.err != nil {
		return false
	}
	line, err := json.Marshal(v)
	if err != nil {
		d.setErrLocked(err)
		return false
	}
	line = d.scrub(line)
	if _, err := d.w.Write(append(line, '\n')); err != nil {
		d.setErrLocked(err)
		return false
	}
	return true
}

// scrub is the last pass over every line and the returned summary text:
// known-value redaction, then the sk-ant scrub.
func (d *CatchRateDiag) scrub(b []byte) []byte {
	b, _ = d.known.Redact(b)
	return catchDiagSkAntRe.ReplaceAll(b, []byte(catchDiagRedacted))
}

func (d *CatchRateDiag) setErrLocked(err error) {
	d.err = err
	d.warn(fmt.Sprintf("write %s: %v; later diagnostics lines are skipped (the run continues)", d.path, err))
}

// catchDiagEmittedVerdict is the diagnostics' own lenient decode of a
// response: every emitted concern, before any ingest control, with the
// convention and criterion_id fields a reviewer may add.
type catchDiagEmittedVerdict struct {
	Verdict  catchDiagLenientString    `json:"verdict"`
	Concerns []catchDiagEmittedConcern `json:"concerns"`
}

type catchDiagEmittedConcern struct {
	Severity    catchDiagLenientString `json:"severity"`
	Category    catchDiagLenientString `json:"category"`
	Note        catchDiagLenientString `json:"note"`
	Convention  catchDiagLenientString `json:"convention"`
	CriterionID catchDiagLenientString `json:"criterion_id"`
}

// catchDiagLenientString decodes any JSON value: a string as itself, any
// other value as its JSON text, so a mistyped field never fails the decode.
type catchDiagLenientString string

func (s *catchDiagLenientString) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = catchDiagLenientString(str)
		return nil
	}
	if string(b) == "null" {
		*s = ""
		return nil
	}
	*s = catchDiagLenientString(b)
	return nil
}

// catchDiagRawConcern is one emitted concern on a trial line, with what the
// rule's halves make of it.
type catchDiagRawConcern struct {
	Severity    string `json:"severity"`
	Category    string `json:"category"`
	Note        string `json:"note"`
	Convention  string `json:"convention,omitempty"`
	CriterionID string `json:"criterion_id,omitempty"`
	// NamesCriterion is the planted criterion id the concern names.
	NamesCriterion string `json:"names_criterion,omitempty"`
	// Probe is the catch probe the concern matches.
	Probe string `json:"probe,omitempty"`
	// Attributed reports that the concern satisfies the attribution half.
	Attributed bool `json:"attributed"`
}

type catchDiagTrialLine struct {
	Kind            string                  `json:"kind"`
	Arm             string                  `json:"arm"`
	Case            string                  `json:"case"`
	Expect          string                  `json:"expect"`
	Trial           int                     `json:"trial"`
	Outcome         CatchOutcome            `json:"outcome"`
	Caught          bool                    `json:"caught"`
	MissClass       CatchMissClass          `json:"miss_class"`
	Verdict         string                  `json:"verdict"`
	MatchedProbe    string                  `json:"matched_probe,omitempty"`
	Attribution     string                  `json:"attribution,omitempty"`
	ConcernsTotal   int                     `json:"concerns_total"`
	RawConcerns     []catchDiagRawConcern   `json:"raw_concerns"`
	ScoredConcerns  []CatchRateTrialConcern `json:"scored_concerns"`
	ResponseExcerpt string                  `json:"response_excerpt,omitempty"`
}

// observe appends one trial line, folds the trial into the arm summary and
// writes one progress line.
func (d *CatchRateDiag) observe(arm, caseName, expect string, index int, responseText string, tr CatchTrial) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished {
		return
	}
	ac, ok := d.rules[arm][caseName]
	rule := ac.rule
	if !ok {
		rule = CatchRule{Clean: expect == CatchExpectClean}
	}
	line := catchDiagTrialLine{
		Kind: "trial", Arm: arm, Case: caseName, Expect: expect, Trial: index,
		Outcome: tr.Outcome, Caught: tr.Outcome == CatchCaught,
		MissClass:     classifyCatchMiss(tr, rule),
		MatchedProbe:  sanitizeCatchTrialText(tr.MatchedProbe),
		Attribution:   tr.Attribution,
		ConcernsTotal: tr.ConcernsTotal,
		RawConcerns:   []catchDiagRawConcern{},
	}
	var v catchDiagEmittedVerdict
	if err := json.Unmarshal([]byte(extractJSONObject(responseText)), &v); err != nil {
		line.Verdict = catchDiagUndecodableVerdict
	} else {
		line.Verdict = sanitizeCatchTrialText(string(v.Verdict))
		for _, c := range v.Concerns {
			cat, note := string(c.Category), string(c.Note)
			named, _ := matchCatchProbe(cat, note, rule.CriterionIDs)
			probe, _ := matchCatchProbe(cat, note, rule.Probes)
			_, attributed := matchCatchAttribution(cat, note, rule)
			line.RawConcerns = append(line.RawConcerns, catchDiagRawConcern{
				Severity:       sanitizeCatchTrialText(string(c.Severity)),
				Category:       sanitizeCatchTrialText(cat),
				Note:           sanitizeCatchTrialText(note),
				Convention:     sanitizeCatchTrialText(string(c.Convention)),
				CriterionID:    sanitizeCatchTrialText(string(c.CriterionID)),
				NamesCriterion: sanitizeCatchTrialText(named),
				Probe:          sanitizeCatchTrialText(probe),
				Attributed:     attributed,
			})
		}
	}
	line.ScoredConcerns = make([]CatchRateTrialConcern, 0, len(tr.Concerns))
	for _, c := range tr.Concerns {
		line.ScoredConcerns = append(line.ScoredConcerns, CatchRateTrialConcern{
			Severity:            sanitizeCatchTrialText(c.Severity),
			SeverityClampedFrom: sanitizeCatchTrialText(c.SeverityClampedFrom),
			QuoteUnverified:     c.QuoteUnverified,
			DocumentRef:         sanitizeCatchTrialText(c.DocumentRef),
			Category:            sanitizeCatchTrialText(c.Category),
			Note:                sanitizeCatchTrialText(c.Note),
			NearMiss:            c.NearMiss,
			RepoSpecificMarker:  c.RepoSpecificMarker,
			AbsentControl:       c.AbsentControl,
		})
	}
	if tr.Outcome == CatchUndecodable {
		line.ResponseExcerpt = sanitizeCatchTrialText(responseText)
	}
	if d.writeLineLocked(line) {
		d.trials++
	}
	d.armAgg(arm).add(line, rule.Clean, d.scrub)
	if d.progress != nil {
		_, _ = fmt.Fprintf(d.progress, "%s/%s/%d %s\n", caseName, arm, index, catchDiagProgressVerdict(line, rule.Clean))
	}
}

// catchDiagProgressVerdict is a progress line's result word. A clean case's
// undecodable trial reads false_alarm: the gate counts it as one.
func catchDiagProgressVerdict(line catchDiagTrialLine, clean bool) string {
	switch {
	case clean && line.Outcome == CatchNoFalseAlarm:
		return "no_false_alarm"
	case clean:
		return "false_alarm"
	case line.Caught:
		return "caught"
	default:
		return "miss:" + string(line.MissClass)
	}
}

// catchDiagArmAgg is one arm's running summary.
type catchDiagArmAgg struct {
	trials, decoded, catchTrials, caught, cleanTrials, falseAlarms, undecodable int
	concernsSum                                                                 int
	verdicts, labels                                                            map[string]int
	misses                                                                      map[CatchMissClass]int
	perCase                                                                     map[string]*catchDiagCaseAgg
}

type catchDiagCaseAgg struct {
	trials, caught, falseAlarms, undecodable int
	misses                                   map[CatchMissClass]int
}

func (d *CatchRateDiag) armAgg(arm string) *catchDiagArmAgg {
	a, ok := d.arms[arm]
	if !ok {
		a = &catchDiagArmAgg{verdicts: map[string]int{}, labels: map[string]int{}, misses: map[CatchMissClass]int{}, perCase: map[string]*catchDiagCaseAgg{}}
		d.arms[arm] = a
	}
	return a
}

// add folds one trial line in. scrub runs on each concern category BEFORE
// it is case-folded into the histogram: a folded credential no longer matches
// its known value.
func (a *catchDiagArmAgg) add(line catchDiagTrialLine, clean bool, scrub func([]byte) []byte) {
	pc, ok := a.perCase[line.Case]
	if !ok {
		pc = &catchDiagCaseAgg{misses: map[CatchMissClass]int{}}
		a.perCase[line.Case] = pc
	}
	a.trials++
	pc.trials++
	a.verdicts[line.Verdict]++
	if line.Outcome == CatchUndecodable {
		a.undecodable++
		pc.undecodable++
	} else {
		a.decoded++
		a.concernsSum += line.ConcernsTotal
	}
	for _, c := range line.RawConcerns {
		label := strings.ToLower(strings.TrimSpace(string(scrub([]byte(c.Category)))))
		if label == "" {
			label = "(empty)"
		}
		a.labels[label]++
	}
	if clean {
		a.cleanTrials++
		if line.Outcome == CatchFalseAlarm {
			a.falseAlarms++
			pc.falseAlarms++
		}
		return
	}
	a.catchTrials++
	if line.Caught {
		a.caught++
		pc.caught++
	}
	if line.MissClass != "" {
		a.misses[line.MissClass]++
		pc.misses[line.MissClass]++
	}
}

// CatchRateDiagArmSummary is one arm's summary-line entry.
type CatchRateDiagArmSummary struct {
	Trials                      int                                 `json:"trials"`
	Decoded                     int                                 `json:"decoded"`
	CatchTrials                 int                                 `json:"catch_trials"`
	Caught                      int                                 `json:"caught"`
	CleanTrials                 int                                 `json:"clean_trials"`
	FalseAlarms                 int                                 `json:"false_alarms"`
	Undecodable                 int                                 `json:"undecodable"`
	Verdicts                    map[string]int                      `json:"verdicts"`
	Labels                      map[string]int                      `json:"concern_categories"`
	MeanConcernsPerDecodedTrial float64                             `json:"mean_concerns_per_decoded_trial"`
	MissClasses                 map[CatchMissClass]int              `json:"miss_classes"`
	PerCase                     map[string]CatchRateDiagCaseSummary `json:"per_case"`
}

// CatchRateDiagCaseSummary is one case's summary-line entry.
type CatchRateDiagCaseSummary struct {
	Trials      int                    `json:"trials"`
	Caught      int                    `json:"caught"`
	FalseAlarms int                    `json:"false_alarms,omitempty"`
	Undecodable int                    `json:"undecodable"`
	MissClasses map[CatchMissClass]int `json:"miss_classes"`
}

// catchDiagSummaryLine is the summary line.
type catchDiagSummaryLine struct {
	Kind          string                             `json:"kind"`
	Outcome       string                             `json:"outcome"`
	Error         string                             `json:"error,omitempty"`
	FinishedAt    string                             `json:"finished_at"`
	TrialsWritten int                                `json:"trials_written"`
	Arms          map[string]CatchRateDiagArmSummary `json:"arms"`
}

// catchDiagOutcome is the summary outcome: passed, not_judged (a subset dry
// run), refused (the regression, portability or subset refusal) or error.
func catchDiagOutcome(runErr error, notJudged bool) string {
	switch {
	case runErr == nil && notJudged:
		return "not_judged"
	case runErr == nil:
		return "passed"
	case errors.Is(runErr, ErrCatchRateRegressed), errors.Is(runErr, ErrPortabilityRepoSpecificFinding), errors.Is(runErr, ErrCatchRateSubsetNotRecordable):
		return "refused"
	default:
		return "error"
	}
}

func (a *catchDiagArmAgg) summary() CatchRateDiagArmSummary {
	s := CatchRateDiagArmSummary{
		Trials: a.trials, Decoded: a.decoded, CatchTrials: a.catchTrials, Caught: a.caught,
		CleanTrials: a.cleanTrials, FalseAlarms: a.falseAlarms, Undecodable: a.undecodable,
		Verdicts: a.verdicts, Labels: a.labels, MissClasses: a.misses,
		PerCase: make(map[string]CatchRateDiagCaseSummary, len(a.perCase)),
	}
	if a.decoded > 0 {
		s.MeanConcernsPerDecodedTrial = float64(a.concernsSum) / float64(a.decoded)
	}
	for name, pc := range a.perCase {
		s.PerCase[name] = CatchRateDiagCaseSummary{Trials: pc.trials, Caught: pc.caught, FalseAlarms: pc.falseAlarms, Undecodable: pc.undecodable, MissClasses: pc.misses}
	}
	return s
}

// Finish writes the summary line for runErr, closes the file and returns the
// rendered text summary, scrubbed like every line. Its error is the sticky write error or the close
// error, naming the path; the run's own error is the caller's.
func (d *CatchRateDiag) Finish(runErr error) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished {
		return "", fmt.Errorf("agenteval: catch-rate diagnostics %s: already finished", d.path)
	}
	line := catchDiagSummaryLine{
		Kind: "summary", Outcome: catchDiagOutcome(runErr, d.notJudged),
		FinishedAt: time.Now().UTC().Format(time.RFC3339), TrialsWritten: d.trials,
		Arms: make(map[string]CatchRateDiagArmSummary, len(d.arms)),
	}
	if runErr != nil {
		line.Error = sanitizeCatchTrialText(runErr.Error())
	}
	for arm, a := range d.arms {
		line.Arms[arm] = a.summary()
	}
	d.writeLineLocked(line)
	d.finished = true
	cerr := d.w.Close()
	text := string(d.scrub([]byte(renderCatchRateDiagSummary(line))))
	if d.err != nil {
		return text, fmt.Errorf("agenteval: catch-rate diagnostics %s: write failed: %w", d.path, d.err)
	}
	if cerr != nil {
		return text, fmt.Errorf("agenteval: catch-rate diagnostics %s: close: %w", d.path, cerr)
	}
	return text, nil
}

// renderCatchRateDiagSummary is the summary's text form: arms in name order,
// histograms by count descending then name.
func renderCatchRateDiagSummary(s catchDiagSummaryLine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Diagnostics summary (outcome %s, %d trial lines; diagnostic only, never scored):\n", s.Outcome, s.TrialsWritten)
	arms := make([]string, 0, len(s.Arms))
	for arm := range s.Arms {
		arms = append(arms, arm)
	}
	sort.Strings(arms)
	for _, arm := range arms {
		a := s.Arms[arm]
		fmt.Fprintf(&b, "  %s: %d trials, %d decoded, %d undecodable; catch %d/%d; false alarms %d/%d; mean concerns per decoded trial %.2f\n",
			arm, a.Trials, a.Decoded, a.Undecodable, a.Caught, a.CatchTrials, a.FalseAlarms, a.CleanTrials, a.MeanConcernsPerDecodedTrial)
		fmt.Fprintf(&b, "    verdicts: %s\n", renderCatchDiagHistogram(a.Verdicts))
		fmt.Fprintf(&b, "    concern categories: %s\n", renderCatchDiagHistogram(a.Labels))
		if a.CatchTrials > 0 {
			fmt.Fprintf(&b, "    miss classes: %s\n", renderCatchDiagHistogram(missClassCounts(a.MissClasses)))
		}
		cases := make([]string, 0, len(a.PerCase))
		for name := range a.PerCase {
			cases = append(cases, name)
		}
		sort.Strings(cases)
		for _, name := range cases {
			if pc := a.PerCase[name]; len(pc.MissClasses) > 0 {
				fmt.Fprintf(&b, "      %s: caught %d/%d; %s\n", name, pc.Caught, pc.Trials, renderCatchDiagHistogram(missClassCounts(pc.MissClasses)))
			}
		}
	}
	return b.String()
}

func missClassCounts(m map[CatchMissClass]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}

func renderCatchDiagHistogram(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// catchTrialObserverKey carries a *CatchRateDiag on a context.
type catchTrialObserverKey struct{}

// withCatchTrialObserver attaches d to ctx: every trial loop run under the
// returned context reports each trial to it.
func withCatchTrialObserver(ctx context.Context, d *CatchRateDiag) context.Context {
	return context.WithValue(ctx, catchTrialObserverKey{}, d)
}

// observeCatchTrial reports one classified trial to ctx's observer; a no-op
// when none is attached.
func observeCatchTrial(ctx context.Context, arm, caseName, expect string, index int, responseText string, tr CatchTrial) {
	if ctx == nil {
		return
	}
	if d, _ := ctx.Value(catchTrialObserverKey{}).(*CatchRateDiag); d != nil {
		d.observe(arm, caseName, expect, index, responseText, tr)
	}
}

// RecordCatchRateEvidenceWithDiagnostics is RecordCatchRateEvidence with the
// diagnostics file written at diagPath: opened BEFORE any model call (an
// open failure refuses the run), fed one line per trial, and finished with a
// summary line on EVERY outcome — pass, refusal or transport error. The
// returned report carries the rendered summary and the diagnostics path; the
// error joins the run's error and any diagnostics write error.
func RecordCatchRateEvidenceWithDiagnostics(ctx context.Context, sender MessageSender, generatorModel string, in CatchRateInputs, evidencePath string, now time.Time, opts RecordCatchRateOptions, diagPath string, diagOpts CatchRateDiagOptions) (string, error) {
	// A table error is RecordCatchRateEvidence's to report; the diagnostics
	// still open (with no rules) so that outcome gets a summary line too.
	rows, _ := catchRateArmTable(in)
	diagOpts.Header.StartedAt = now.UTC().Format(time.RFC3339)
	diagOpts.Header.GeneratorModel = generatorModel
	diagOpts.Header.DryRun = opts.DryRun
	diagOpts.Header.PinBaseline = opts.PinBaseline
	diagOpts.Header.SubsetCases = opts.Subset.Cases
	diagOpts.Header.SubsetTrialsPerCase = opts.Subset.TrialsPerCase
	diag, err := OpenCatchRateDiag(diagPath, diagOpts, catchDiagRules(rows))
	if err != nil {
		return "", err
	}
	diag.notJudged = opts.Subset.active()
	report, err := RecordCatchRateEvidence(withCatchTrialObserver(ctx, diag), sender, generatorModel, in, evidencePath, now, opts)
	summary, ferr := diag.Finish(err)
	if report != "" {
		report += "\n"
	}
	report += summary + fmt.Sprintf("Diagnostics: %s\n", diagPath)
	return report, errors.Join(err, ferr)
}

// renderCatchRateSubsetDryRun is a subset dry run's report: the selected
// arms' per-case results, never judged.
func renderCatchRateSubsetDryRun(subset CatchRateSubset, rows []catchRateArmRow, reports map[string]CatchRateArmReport) string {
	var b strings.Builder
	cases := "every case"
	if len(subset.Cases) > 0 {
		cases = strings.Join(subset.Cases, ", ")
	}
	trials := "each arm's own samples per case"
	if subset.TrialsPerCase > 0 {
		trials = fmt.Sprintf("%d samples per case", subset.TrialsPerCase)
	}
	fmt.Fprintf(&b, "Subset dry run (%s; %s):\n", cases, trials)
	for _, row := range rows {
		r, ok := reports[row.name]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "  %s:\n", row.name)
		for _, name := range sortedCaseNames(r.PerCase) {
			c := r.PerCase[name]
			if c.IsClean() {
				fmt.Fprintf(&b, "    %s (clean): false alarms %d/%d, undecodable %d\n", name, c.FalseAlarms, c.Trials, c.Undecodable)
			} else {
				fmt.Fprintf(&b, "    %s: caught %d/%d, undecodable %d\n", name, c.Caught, c.Trials, c.Undecodable)
			}
		}
	}
	b.WriteString("Subset dry run: NOT judged against the gate and never recordable\n")
	return b.String()
}
