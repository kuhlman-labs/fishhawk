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
// ONE CLASSIFIER. A decodable trial's stored concerns, in stored order, go to
// classifyCatchConcerns — the measurement's own matcher for every arm — so
// the outcome, the matched probe, the attribution kind, the near-miss tags and
// a clean case's false alarm come from the measurement's code path; no second
// matcher exists. An UNDECODABLE trial stays undecodable and is never caught:
// its response did not decode, and its stored response_excerpt is never
// re-parsed.
//
// THE ARM TABLE. The re-score iterates the evidence arm table
// (catchRateArmTable, E78.11 / #4313) — the conventions arms and the architect
// persona arm, and any later row (#4314) — and returns every re-scored trial
// (RescoredCatchTrial), so a per-trial classifier (#4350) can read the
// re-scored concerns and their near-miss tags. It reads a v2 sidecar and,
// so #4355's diagnostic survives the format change, a legacy v1 sidecar too
// (its two conventions arms; every other arm is reported NOT RECORDED).

package agenteval

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

// catchRateCeilingMarginPercent is the margin, in percentage points, within
// which an arm's re-scored rate counts as AT the attribution-only rate: the
// probe half then rejects almost nothing the attribution half accepts, so the
// corpus no longer discriminates (#4361). Compared in integers, exactly.
const catchRateCeilingMarginPercent = 3

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
	// Clean is true for a trial of a CLEAN case (scored for false alarms,
	// never for a catch).
	Clean bool
	// LowerBound is true for a re-scored MISS whose stored record is
	// incomplete — concerns_total exceeds the stored concerns, or a stored
	// note or category ends in the truncation marker — so the full response
	// might have caught. A re-scored catch cannot be under-counted.
	LowerBound bool
}

// CatchRescoreCounts are re-score tallies over a set of stored trials.
type CatchRescoreCounts struct {
	// Trials counts every stored trial; CleanTrials the clean-case ones among
	// them (catch rates are over Trials - CleanTrials).
	Trials      int
	CleanTrials int
	Undecodable int
	// RecordedCaught is the trials the recording stored as caught;
	// RescoredCaught the trials the current rule catches.
	RecordedCaught int
	RescoredCaught int
	// Gained is recorded missed -> re-scored caught; Lost is recorded caught
	// -> re-scored missed.
	Gained int
	Lost   int
	// RecordedFalseAlarms / RescoredFalseAlarms count a CLEAN case's
	// false-alarm trials as recorded and as re-scored.
	RecordedFalseAlarms int
	RescoredFalseAlarms int
	// Attributed is the attribution-only count (RescoredCatchTrial.Attributed).
	Attributed int
	// LowerBound is the re-scored misses the sidecar stores incompletely.
	LowerBound int
}

func (c *CatchRescoreCounts) add(o CatchRescoreCounts) {
	c.Trials += o.Trials
	c.CleanTrials += o.CleanTrials
	c.Undecodable += o.Undecodable
	c.RecordedCaught += o.RecordedCaught
	c.RescoredCaught += o.RescoredCaught
	c.Gained += o.Gained
	c.Lost += o.Lost
	c.RecordedFalseAlarms += o.RecordedFalseAlarms
	c.RescoredFalseAlarms += o.RescoredFalseAlarms
	c.Attributed += o.Attributed
	c.LowerBound += o.LowerBound
}

func (c *CatchRescoreCounts) count(t RescoredCatchTrial) {
	c.Trials++
	if t.Clean {
		c.CleanTrials++
	}
	switch t.Recorded {
	case CatchCaught:
		c.RecordedCaught++
	case CatchUndecodable:
		c.Undecodable++
	case CatchFalseAlarm:
		c.RecordedFalseAlarms++
	}
	if t.Rescored.Outcome == CatchFalseAlarm {
		c.RescoredFalseAlarms++
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
	// Recorded is false when the sidecar carries no such arm (a v1 sidecar's
	// architect arm): every corpus case is then NotRecorded.
	Recorded bool
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
	// Arms is in arm-table order, the reference arm (without_conventions)
	// first.
	Arms []CatchRateRescoreArm
	// UnknownArms names the sidecar arms the arm table has no row for.
	UnknownArms []string
}

// RescoreCatchRateTrials re-scores the trial sidecar at trialsPath under the
// CURRENT arm table built from in (catchRateArmTable, so every corpus loader
// mode applies) and the current catch rule. It never writes. It FAILS CLOSED
// on: an absent or unreadable sidecar; malformed JSON, an unknown field or
// trailing content; a schema other than CatchRateTrialsSchema or the legacy
// v1 sidecar schema; a stored outcome outside the case's expectation class
// (caught|missed|undecodable for a catch case, false_alarm|no_false_alarm|
// undecodable for a clean one; any of the five for an unscored case); an
// unavailable input. A sidecar case absent from the corpus (Unscored), a
// corpus case absent from the sidecar (NotRecorded), a table arm absent from
// the sidecar (NOT RECORDED) and a sidecar arm absent from the table
// (UnknownArms) are reported, not errors, so a sidecar recorded under an
// earlier corpus stays re-scorable.
func RescoreCatchRateTrials(trialsPath string, in CatchRateInputs) (CatchRateRescore, error) {
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
	if trials.Schema != CatchRateTrialsSchema && trials.Schema != catchRateTrialsSchemaV1 {
		return fail("schema %q is not %q (or the legacy %q)", trials.Schema, CatchRateTrialsSchema, catchRateTrialsSchemaV1)
	}
	rows, err := catchRateArmTable(in)
	if err != nil {
		return fail("%v", err)
	}
	// The reference arm leads the report, its conventions pair second (the
	// one delta the report takes), then every other arm by name.
	rank := func(name string) int {
		switch name {
		case ArmWithoutConventions:
			return 0
		case ArmWithConventions:
			return 1
		}
		return 2
	}
	sort.SliceStable(rows, func(i, j int) bool { return rank(rows[i].name) < rank(rows[j].name) })
	label, _, _ := strings.Cut(catchRuleVersion, ":")
	out := CatchRateRescore{
		TrialsPath:          trialsPath,
		RecordedAt:          trials.RecordedAt,
		PromptFingerprint:   trials.PromptFingerprint,
		NoteMaxRunes:        trials.NoteMaxRunes,
		MaxConcernsPerTrial: trials.MaxConcernsPerTrial,
		CatchRule:           label,
	}
	known := make(map[string]bool, len(rows))
	for _, row := range rows {
		known[row.name] = true
		ta, recorded := trials.Arms[row.name]
		arm := CatchRateRescoreArm{Arm: row.name, Recorded: recorded}
		inCorpus := make(map[string]bool, len(row.cases))
		for _, c := range row.cases {
			inCorpus[c.name] = true
			list, ok := ta.PerCase[c.name]
			if !ok {
				arm.NotRecorded = append(arm.NotRecorded, c.name)
				continue
			}
			cr, err := rescoreCatchCase(c.name, list, c.rule)
			if err != nil {
				return fail("arm %q case %q: %v", row.name, c.name, err)
			}
			arm.PerCase = append(arm.PerCase, cr)
		}
		for _, name := range sortedTrialCaseNames(ta.PerCase) {
			if inCorpus[name] {
				continue
			}
			for _, tr := range ta.PerCase[name] {
				if !knownCatchOutcome(tr.Outcome) {
					return fail("arm %q case %q (not in the current corpus): trial %d outcome %q is not one of %s", row.name, name, tr.Trial, tr.Outcome, allCatchOutcomes)
				}
			}
			arm.Unscored = append(arm.Unscored, name)
		}
		out.Arms = append(out.Arms, arm)
	}
	for _, name := range sortedTrialArmNames(trials.Arms) {
		if !known[name] {
			out.UnknownArms = append(out.UnknownArms, name)
		}
	}
	return out, nil
}

const allCatchOutcomes = "caught|missed|false_alarm|no_false_alarm|undecodable"

func knownCatchOutcome(o CatchOutcome) bool {
	switch o {
	case CatchCaught, CatchMissed, CatchFalseAlarm, CatchNoFalseAlarm, CatchUndecodable:
		return true
	}
	return false
}

// rescoreCatchCase re-scores one case's stored trials under rule. A stored
// outcome outside the rule's expectation class is refused.
func rescoreCatchCase(name string, list []CatchRateTrialRecord, rule CatchRule) (CaseCatchRescore, error) {
	out := CaseCatchRescore{Case: name, TrialRecords: make([]RescoredCatchTrial, 0, len(list))}
	decodable, class := []CatchOutcome{CatchCaught, CatchMissed}, "caught|missed|undecodable"
	if rule.Clean {
		decodable, class = []CatchOutcome{CatchFalseAlarm, CatchNoFalseAlarm}, "false_alarm|no_false_alarm|undecodable"
	}
	for _, tr := range list {
		rt := RescoredCatchTrial{Trial: tr.Trial, Recorded: tr.Outcome, Clean: rule.Clean}
		switch {
		case tr.Outcome == CatchUndecodable:
			rt.Rescored = CatchTrial{Outcome: CatchUndecodable, ConcernsTotal: tr.ConcernsTotal, Response: tr.ResponseExcerpt}
		case slices.Contains(decodable, tr.Outcome):
			rt.Rescored = classifyStoredCatchTrial(tr, rule)
			rt.Attributed = !rule.Clean && storedTrialAttributed(tr, rule)
			rt.LowerBound = (rt.Rescored.Outcome == CatchMissed || rt.Rescored.Outcome == CatchNoFalseAlarm) && storedTrialIncomplete(tr)
		default:
			return CaseCatchRescore{}, fmt.Errorf("trial %d outcome %q is not one of %s", tr.Trial, tr.Outcome, class)
		}
		out.count(rt)
		out.TrialRecords = append(out.TrialRecords, rt)
	}
	return out, nil
}

// classifyStoredCatchTrial classifies a decodable trial's stored concerns, in
// stored order, with classifyCatchConcerns — the measurement's own matcher,
// near-miss tagging included. Every stored field rides through. ConcernsTotal
// is the recorded uncapped count, not the stored one.
func classifyStoredCatchTrial(tr CatchRateTrialRecord, rule CatchRule) CatchTrial {
	concerns := make([]CatchTrialConcern, 0, len(tr.Concerns))
	for _, c := range tr.Concerns {
		concerns = append(concerns, CatchTrialConcern{
			Severity: c.Severity, SeverityClampedFrom: c.SeverityClampedFrom, QuoteUnverified: c.QuoteUnverified,
			DocumentRef: c.DocumentRef, Category: c.Category, Note: c.Note,
		})
	}
	return classifyCatchConcerns(concerns, tr.ConcernsTotal, rule)
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
// catchRateCeilingMarginPercent points of its attribution-only rate, over the
// catch-case trials (a clean case has nothing to catch). An arm no stored
// concern attributes is not at ceiling: the probe half has nothing to reject.
func atCatchCeiling(p CatchRescoreCounts) bool {
	n := p.Trials - p.CleanTrials
	return n > 0 && p.Attributed > 0 && 100*(p.Attributed-p.RescoredCaught) <= catchRateCeilingMarginPercent*n
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
		if !a.Recorded {
			fmt.Fprintf(&b, "  %s: NOT RECORDED (the sidecar carries no such arm)\n", a.Arm)
			continue
		}
		if p.CleanTrials > 0 {
			fmt.Fprintf(&b, "  %s clean cases: recorded %d/%d false alarms → re-scored %d/%d\n", a.Arm, p.RecordedFalseAlarms, p.CleanTrials, p.RescoredFalseAlarms, p.CleanTrials)
		}
		n := p.Trials - p.CleanTrials
		fmt.Fprintf(&b, "  %s: recorded %d/%d (%s) → re-scored %d/%d (%s), +%d gained / -%d lost; attribution-only %d/%d (%s); %d undecodable; %d lower-bound trials\n",
			a.Arm, p.RecordedCaught, n, catchRateOf(p.RecordedCaught, n),
			p.RescoredCaught, n, catchRateOf(p.RescoredCaught, n), p.Gained, p.Lost,
			p.Attributed, n, catchRateOf(p.Attributed, n), p.Undecodable, p.LowerBound)
		if atCatchCeiling(p) {
			fmt.Fprintf(&b, "    AT CEILING: the re-scored rate is within %.2f of the attribution-only rate — the probe half rejects almost nothing the attribution half accepts, so the corpus is at ceiling for this arm (#4361)\n",
				float64(catchRateCeilingMarginPercent)/100)
		}
	}
	for i := 1; i < len(r.Arms); i++ {
		ref, p := pooled[0], pooled[i]
		if r.Arms[i].Arm != ArmWithConventions {
			// Only the conventions pair shares a case set: a delta against a
			// persona arm compares different corpora.
			continue
		}
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
			if c.CleanTrials > 0 {
				parts = append(parts, fmt.Sprintf("%s clean, false alarms %d/%d → %d/%d (lower-bound %d)",
					a.Arm, c.RecordedFalseAlarms, c.Trials, c.RescoredFalseAlarms, c.Trials, c.LowerBound))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s %d/%d → %d/%d (+%d/-%d, attributed %d, lower-bound %d)",
				a.Arm, c.RecordedCaught, c.Trials, c.RescoredCaught, c.Trials, c.Gained, c.Lost, c.Attributed, c.LowerBound))
		}
		fmt.Fprintf(&b, "  %s: %s\n", name, strings.Join(parts, "; "))
	}
	if len(r.UnknownArms) > 0 {
		fmt.Fprintf(&b, "  UNKNOWN ARMS (in the sidecar, not in the arm table): %s\n", strings.Join(r.UnknownArms, ", "))
	}
	for _, a := range r.Arms {
		if !a.Recorded {
			fmt.Fprintf(&b, "  %s NOT RECORDED (the sidecar carries no such arm)\n", a.Arm)
			continue
		}
		if len(a.Unscored) > 0 {
			fmt.Fprintf(&b, "  %s UNSCORED (in the sidecar, not in the current corpus): %s\n", a.Arm, strings.Join(a.Unscored, ", "))
		}
		if len(a.NotRecorded) > 0 {
			fmt.Fprintf(&b, "  %s NOT RECORDED (in the current corpus, not in the sidecar): %s\n", a.Arm, strings.Join(a.NotRecorded, ", "))
		}
	}
	return b.String()
}
