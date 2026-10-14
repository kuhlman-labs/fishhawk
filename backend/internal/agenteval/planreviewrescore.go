// Offline RE-SCORE of a recorded plan-review catch-rate trial sidecar (E55.22 /
// #4355).
//
// RescoreCatchRateTrials re-classifies every stored trial of an
// evidence.trials.json (planreviewevidence.go) under the CURRENT corpus and
// catch rule, with no model call and no network, so a probe or rule change can
// be evaluated against an existing recording before paying for a new one. It
// is a DIAGNOSTIC, never a gate verdict: it writes nothing, and the offline
// gate (CheckCatchRateEvidence) never reads it.
//
// THE RE-SCORE IS A LOWER BOUND, PER ARM. The sidecar truncates every stored
// note and category at note_max_runes, keeps at most max_concerns_per_trial
// concerns per trial, and keeps only the MATCHING concern of a caught trial.
// Both halves of the catch rule are case-folded substring tests, so missing
// text can only remove a match: a re-score can under-count catches, never
// over-count them. The arm DELTA carries no such bound, because the two arms'
// under-counts need not be equal. A re-scored rate is also IN-SAMPLE for any
// probe set tuned on the same sidecar; only a fresh recording measures out of
// sample.
//
// ONE CLASSIFIER. A decodable trial's stored concerns are re-wrapped, in
// stored order, as a verdict and passed to ClassifyCatchTrial, so the outcome,
// the matched probe, the attribution kind and the near-miss tags come from the
// measurement's own code path (matchCatchConcern); no second matcher exists.
// An UNDECODABLE trial stays undecodable and is never caught: its response did
// not decode, and its stored response_excerpt is never re-parsed.
//
// THE ARM TABLE. The re-score iterates catchRateRescoreArms rather than fixed
// without/with fields, and returns every re-scored trial (RescoredCatchTrial),
// so a later named arm (#4313/#4314) is one more table row and a per-trial
// classifier (#4350) can read the re-scored concerns and their near-miss tags.

package agenteval

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// catchRateCeilingMarginPercent is the margin, in percentage points, within
// which an arm's re-scored rate counts as AT the attribution-only rate: the
// probe half then rejects almost nothing the attribution half accepts, so the
// corpus no longer discriminates (#4361). Compared in integers, exactly.
const catchRateCeilingMarginPercent = 3

// catchRateRescoreArm is one row of the re-score's arm table: an arm name and
// the accessor selecting that arm's trials from a sidecar.
type catchRateRescoreArm struct {
	name   string
	trials func(CatchRateTrialsArms) map[string][]CatchRateTrialRecord
}

// catchRateRescoreArms is the ARM TABLE RescoreCatchRateTrials iterates, in
// report order. The first row is the reference arm every later arm's delta is
// taken against.
var catchRateRescoreArms = []catchRateRescoreArm{
	{ArmWithoutConventions, func(a CatchRateTrialsArms) map[string][]CatchRateTrialRecord { return a.WithoutConventions.PerCase }},
	{ArmWithConventions, func(a CatchRateTrialsArms) map[string][]CatchRateTrialRecord { return a.WithConventions.PerCase }},
}

// RescoredCatchTrial is one stored trial re-classified under the current rule.
type RescoredCatchTrial struct {
	// Trial is the stored 1-based sample index.
	Trial int
	// Recorded is the outcome the recording stored.
	Recorded CatchOutcome
	// Rescored is the trial re-classified by ClassifyCatchTrial over the
	// stored concerns: its Outcome, MatchedProbe and Attribution, its
	// Concerns (the matching one of a catch, every stored one near-miss
	// tagged of a miss) and the recorded uncapped ConcernsTotal. An
	// undecodable trial carries its stored excerpt in Response, unparsed.
	Rescored CatchTrial
	// Attributed is true when some stored concern satisfies the attribution
	// half of the rule on its own (matchCatchAttribution): the
	// attribution-only count, the ceiling any probe set can reach on these
	// stored concerns.
	Attributed bool
	// LowerBound is true for a re-scored MISS whose stored record is
	// incomplete — concerns_total exceeds the stored concerns, or a stored
	// note or category ends in the truncation marker — so the full response
	// might have caught. A re-scored catch cannot be under-counted.
	LowerBound bool
}

// CatchRescoreCounts are re-score tallies over a set of stored trials.
type CatchRescoreCounts struct {
	Trials      int
	Undecodable int
	// RecordedCaught is the trials the recording stored as caught;
	// RescoredCaught the trials the current rule catches.
	RecordedCaught int
	RescoredCaught int
	// Gained is recorded missed -> re-scored caught; Lost is recorded caught
	// -> re-scored missed.
	Gained int
	Lost   int
	// Attributed is the attribution-only count (RescoredCatchTrial.Attributed).
	Attributed int
	// LowerBound is the re-scored misses the sidecar stores incompletely.
	LowerBound int
}

func (c *CatchRescoreCounts) add(o CatchRescoreCounts) {
	c.Trials += o.Trials
	c.Undecodable += o.Undecodable
	c.RecordedCaught += o.RecordedCaught
	c.RescoredCaught += o.RescoredCaught
	c.Gained += o.Gained
	c.Lost += o.Lost
	c.Attributed += o.Attributed
	c.LowerBound += o.LowerBound
}

func (c *CatchRescoreCounts) count(t RescoredCatchTrial) {
	c.Trials++
	switch t.Recorded {
	case CatchCaught:
		c.RecordedCaught++
	case CatchUndecodable:
		c.Undecodable++
	}
	caught := t.Rescored.Outcome == CatchCaught
	if caught {
		c.RescoredCaught++
	}
	if caught && t.Recorded == CatchMissed {
		c.Gained++
	}
	if !caught && t.Recorded == CatchCaught {
		c.Lost++
	}
	if t.Attributed {
		c.Attributed++
	}
	if t.LowerBound {
		c.LowerBound++
	}
}

// CaseCatchRescore is one case's re-score in one arm.
type CaseCatchRescore struct {
	Case string
	CatchRescoreCounts
	// TrialRecords are the case's re-scored trials, in stored order.
	TrialRecords []RescoredCatchTrial
}

// CatchRateRescoreArm is one arm's re-score.
type CatchRateRescoreArm struct {
	Arm string
	// PerCase holds every case present in BOTH the sidecar arm and the
	// current corpus, in case-name order.
	PerCase []CaseCatchRescore
	// Unscored names the sidecar cases absent from the current corpus (no
	// current rule to score them under); NotRecorded the corpus cases absent
	// from this sidecar arm. Neither is an error.
	Unscored    []string
	NotRecorded []string
}

// Pooled sums the arm's scored cases.
func (a CatchRateRescoreArm) Pooled() CatchRescoreCounts {
	var p CatchRescoreCounts
	for _, c := range a.PerCase {
		p.add(c.CatchRescoreCounts)
	}
	return p
}

// CatchRateRescore is the result of RescoreCatchRateTrials.
type CatchRateRescore struct {
	// TrialsPath is the re-scored sidecar.
	TrialsPath string
	// RecordedAt, PromptFingerprint, NoteMaxRunes and MaxConcernsPerTrial are
	// the sidecar's own.
	RecordedAt          string
	PromptFingerprint   string
	NoteMaxRunes        int
	MaxConcernsPerTrial int
	// CatchRule is the label of the current catchRuleVersion the trials were
	// re-scored under.
	CatchRule string
	// Arms is in arm-table order.
	Arms []CatchRateRescoreArm
}

// RescoreCatchRateTrials re-scores the trial sidecar at trialsPath under the
// CURRENT corpus at corpusDir (loaded by LoadPlanReviewCatchCorpus, so every
// loader mode applies) and the current catch rule. It never writes. It FAILS
// CLOSED on: an absent or unreadable sidecar; malformed JSON, an unknown field
// or trailing content; a schema other than CatchRateTrialsSchema; a stored
// outcome outside caught|missed|undecodable (in any case, scored or not); an
// unavailable corpus. A sidecar case absent from the corpus (Unscored) and a
// corpus case absent from the sidecar (NotRecorded) are reported, not errors,
// so a sidecar recorded under an earlier corpus stays re-scorable.
func RescoreCatchRateTrials(trialsPath, corpusDir string) (CatchRateRescore, error) {
	fail := func(format string, args ...any) (CatchRateRescore, error) {
		return CatchRateRescore{}, fmt.Errorf("agenteval: re-score of catch-rate trial sidecar %q: "+format+"; see %s", append(append([]any{trialsPath}, args...), catchRateRunbook)...)
	}
	raw, err := os.ReadFile(trialsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fail("absent: nothing to re-score (a recording writes the sidecar beside its evidence record)")
		}
		return fail("read: %v", err)
	}
	var trials CatchRateTrials
	if err := decodeStrictJSON(raw, &trials); err != nil {
		return fail("malformed JSON, unknown field or trailing content: %v", err)
	}
	if trials.Schema != CatchRateTrialsSchema {
		return fail("schema %q is not %q", trials.Schema, CatchRateTrialsSchema)
	}
	cases, err := LoadPlanReviewCatchCorpus(corpusDir)
	if err != nil {
		return fail("corpus unavailable: %v", err)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })
	label, _, _ := strings.Cut(catchRuleVersion, ":")
	out := CatchRateRescore{
		TrialsPath:          trialsPath,
		RecordedAt:          trials.RecordedAt,
		PromptFingerprint:   trials.PromptFingerprint,
		NoteMaxRunes:        trials.NoteMaxRunes,
		MaxConcernsPerTrial: trials.MaxConcernsPerTrial,
		CatchRule:           label,
	}
	for _, row := range catchRateRescoreArms {
		recorded := row.trials(trials.Arms)
		arm := CatchRateRescoreArm{Arm: row.name}
		inCorpus := make(map[string]bool, len(cases))
		for _, c := range cases {
			inCorpus[c.Name] = true
			list, ok := recorded[c.Name]
			if !ok {
				arm.NotRecorded = append(arm.NotRecorded, c.Name)
				continue
			}
			cr, err := rescoreCatchCase(c.Name, list, c.CatchRule())
			if err != nil {
				return fail("arm %q case %q: %v", row.name, c.Name, err)
			}
			arm.PerCase = append(arm.PerCase, cr)
		}
		for _, name := range sortedTrialCaseNames(recorded) {
			if inCorpus[name] {
				continue
			}
			for _, tr := range recorded[name] {
				if !knownCatchOutcome(tr.Outcome) {
					return fail("arm %q case %q (not in the current corpus): trial %d outcome %q is not one of %s|%s|%s", row.name, name, tr.Trial, tr.Outcome, CatchCaught, CatchMissed, CatchUndecodable)
				}
			}
			arm.Unscored = append(arm.Unscored, name)
		}
		out.Arms = append(out.Arms, arm)
	}
	return out, nil
}

func knownCatchOutcome(o CatchOutcome) bool {
	return o == CatchCaught || o == CatchMissed || o == CatchUndecodable
}

// rescoreCatchCase re-scores one case's stored trials under rule.
func rescoreCatchCase(name string, list []CatchRateTrialRecord, rule CatchRule) (CaseCatchRescore, error) {
	out := CaseCatchRescore{Case: name, TrialRecords: make([]RescoredCatchTrial, 0, len(list))}
	for _, tr := range list {
		rt := RescoredCatchTrial{Trial: tr.Trial, Recorded: tr.Outcome}
		switch tr.Outcome {
		case CatchUndecodable:
			rt.Rescored = CatchTrial{Outcome: CatchUndecodable, ConcernsTotal: tr.ConcernsTotal, Response: tr.ResponseExcerpt}
		case CatchCaught, CatchMissed:
			rt.Rescored = classifyStoredCatchTrial(tr, rule)
			rt.Attributed = storedTrialAttributed(tr, rule)
			rt.LowerBound = rt.Rescored.Outcome == CatchMissed && storedTrialIncomplete(tr)
		default:
			return CaseCatchRescore{}, fmt.Errorf("trial %d outcome %q is not one of %s|%s|%s", tr.Trial, tr.Outcome, CatchCaught, CatchMissed, CatchUndecodable)
		}
		out.count(rt)
		out.TrialRecords = append(out.TrialRecords, rt)
	}
	return out, nil
}

// classifyStoredCatchTrial re-wraps a decodable trial's stored concerns, in
// stored order, as a verdict and classifies it with ClassifyCatchTrial — the
// measurement's own classifier, near-miss tagging included. ConcernsTotal is
// the recorded uncapped count, not the stored one.
func classifyStoredCatchTrial(tr CatchRateTrialRecord, rule CatchRule) CatchTrial {
	type concern struct {
		Severity string `json:"severity"`
		Category string `json:"category"`
		Note     string `json:"note"`
	}
	v := struct {
		Verdict  string    `json:"verdict"`
		Concerns []concern `json:"concerns"`
	}{Verdict: "approve_with_concerns", Concerns: make([]concern, 0, len(tr.Concerns))}
	for _, c := range tr.Concerns {
		v.Concerns = append(v.Concerns, concern{Severity: c.Severity, Category: c.Category, Note: c.Note})
	}
	// Marshalling a struct of strings cannot fail (invalid UTF-8 is coerced).
	b, _ := json.Marshal(v)
	trial := ClassifyCatchTrial(string(b), rule)
	trial.ConcernsTotal = tr.ConcernsTotal
	return trial
}

// storedTrialAttributed reports whether some stored concern satisfies the
// attribution half of the rule on its own.
func storedTrialAttributed(tr CatchRateTrialRecord, rule CatchRule) bool {
	for _, c := range tr.Concerns {
		if _, ok := matchCatchAttribution(c.Category, c.Note, rule); ok {
			return true
		}
	}
	return false
}

// storedTrialIncomplete reports whether the sidecar stores less of the trial
// than the reviewer emitted: concerns past the cap, or a note or category cut
// at note_max_runes.
func storedTrialIncomplete(tr CatchRateTrialRecord) bool {
	if tr.ConcernsTotal > len(tr.Concerns) {
		return true
	}
	for _, c := range tr.Concerns {
		if strings.HasSuffix(c.Note, catchTrialTruncationMarker) || strings.HasSuffix(c.Category, catchTrialTruncationMarker) {
			return true
		}
	}
	return false
}

// atCatchCeiling reports whether p's re-scored rate is within
// catchRateCeilingMarginPercent points of its attribution-only rate.
func atCatchCeiling(p CatchRescoreCounts) bool {
	return p.Trials > 0 && 100*(p.Attributed-p.RescoredCaught) <= catchRateCeilingMarginPercent*p.Trials
}

func catchRateOf(caught, trials int) string {
	if trials == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", float64(caught)/float64(trials))
}

// Render is the re-score report: the diagnostic banner, the sidecar's
// identity, the lower-bound statement, then per arm the pooled recorded and
// re-scored rates beside the attribution-only rate, each later arm's delta
// against the first, and per case the recorded -> re-scored counts, naming
// every unscored and not-recorded case.
func (r CatchRateRescore) Render() string {
	var b strings.Builder
	b.WriteString("OFFLINE RE-SCORE — a diagnostic, not a gate verdict; it never writes evidence\n\n")
	fmt.Fprintf(&b, "Sidecar: %s\n  recorded_at: %s\n  prompt_fingerprint: %s\n", r.TrialsPath, r.RecordedAt, r.PromptFingerprint)
	fmt.Fprintf(&b, "Re-scored under the CURRENT corpus and catch rule (%s), with no model call.\n\n", r.CatchRule)
	fmt.Fprintf(&b, "LOWER BOUND: the sidecar truncates every stored note and category at note_max_runes (%d runes), "+
		"stores at most max_concerns_per_trial (%d) concerns per trial, and stores only the matching concern of a caught trial. "+
		"Matching is a case-folded substring test, so missing text can only remove a match: a re-score can under-count catches "+
		"but never over-count them, and each arm's re-scored rate is a lower bound on what the full responses would score. "+
		"The arm DELTA is NOT a bound: the arms' under-counts need not be equal. A re-scored rate is IN-SAMPLE for any probe set "+
		"tuned on this sidecar; only a fresh recording is an out-of-sample measurement. The attribution-only rate counts a "+
		"decodable trial when some stored concern names the planted criterion, whatever its probe: the ceiling any probe set can "+
		"reach on these stored concerns.\n\n", r.NoteMaxRunes, r.MaxConcernsPerTrial)

	b.WriteString("Per arm (caught/trials over the cases in both the sidecar and the current corpus):\n")
	pooled := make([]CatchRescoreCounts, len(r.Arms))
	for i, a := range r.Arms {
		p := a.Pooled()
		pooled[i] = p
		fmt.Fprintf(&b, "  %s: recorded %d/%d (%s) → re-scored %d/%d (%s), +%d gained / -%d lost; attribution-only %d/%d (%s); %d undecodable; %d lower-bound trials\n",
			a.Arm, p.RecordedCaught, p.Trials, catchRateOf(p.RecordedCaught, p.Trials),
			p.RescoredCaught, p.Trials, catchRateOf(p.RescoredCaught, p.Trials), p.Gained, p.Lost,
			p.Attributed, p.Trials, catchRateOf(p.Attributed, p.Trials), p.Undecodable, p.LowerBound)
		if atCatchCeiling(p) {
			fmt.Fprintf(&b, "    AT CEILING: the re-scored rate is within %.2f of the attribution-only rate — the probe half rejects almost nothing the attribution half accepts, so the corpus is at ceiling for this arm (#4361)\n",
				float64(catchRateCeilingMarginPercent)/100)
		}
	}
	for i := 1; i < len(r.Arms); i++ {
		ref, p := pooled[0], pooled[i]
		if ref.Trials == 0 || p.Trials == 0 {
			fmt.Fprintf(&b, "  arm delta (%s - %s): n/a (an arm has no scored trials)\n", r.Arms[0].Arm, r.Arms[i].Arm)
			continue
		}
		rec := float64(ref.RecordedCaught)/float64(ref.Trials) - float64(p.RecordedCaught)/float64(p.Trials)
		res := float64(ref.RescoredCaught)/float64(ref.Trials) - float64(p.RescoredCaught)/float64(p.Trials)
		fmt.Fprintf(&b, "  arm delta (%s - %s): recorded %+.3f → re-scored %+.3f (NOT a bound)\n", r.Arms[0].Arm, r.Arms[i].Arm, rec, res)
	}

	b.WriteString("\nPer case (recorded → re-scored caught/trials, +gained/-lost, attribution-only, lower-bound trials):\n")
	byArm := make([]map[string]CaseCatchRescore, len(r.Arms))
	var names []string
	seen := map[string]bool{}
	for i, a := range r.Arms {
		byArm[i] = make(map[string]CaseCatchRescore, len(a.PerCase))
		for _, c := range a.PerCase {
			byArm[i][c.Case] = c
			if !seen[c.Case] {
				seen[c.Case] = true
				names = append(names, c.Case)
			}
		}
	}
	sort.Strings(names)
	for _, name := range names {
		parts := make([]string, 0, len(r.Arms))
		for i, a := range r.Arms {
			c, ok := byArm[i][name]
			if !ok {
				parts = append(parts, a.Arm+" not recorded")
				continue
			}
			parts = append(parts, fmt.Sprintf("%s %d/%d → %d/%d (+%d/-%d, attributed %d, lower-bound %d)",
				a.Arm, c.RecordedCaught, c.Trials, c.RescoredCaught, c.Trials, c.Gained, c.Lost, c.Attributed, c.LowerBound))
		}
		fmt.Fprintf(&b, "  %s: %s\n", name, strings.Join(parts, "; "))
	}
	for _, a := range r.Arms {
		if len(a.Unscored) > 0 {
			fmt.Fprintf(&b, "  %s UNSCORED (in the sidecar, not in the current corpus): %s\n", a.Arm, strings.Join(a.Unscored, ", "))
		}
		if len(a.NotRecorded) > 0 {
			fmt.Fprintf(&b, "  %s NOT RECORDED (in the current corpus, not in the sidecar): %s\n", a.Arm, strings.Join(a.NotRecorded, ", "))
		}
	}
	return b.String()
}
