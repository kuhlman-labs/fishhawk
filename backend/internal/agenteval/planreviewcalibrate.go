// Catch-rate calibration dry run (E78.11 / #4313): before a new arm's cases
// are recorded into the catch-rate evidence, is each catch case of
// MID difficulty — caught between 30% and 80% of the time — so the arm can
// show a regression in either direction instead of saturating at 0% or 100%?
//
// CalibrateCatchRateArm runs ONE arm of the evidence arm table
// (catchRateArmTable) — any row, so a later arm (#4314's security rows)
// calibrates the same way — at a caller-chosen sample count, and reports per
// case its trials, caught or false alarms, undecodable, rate and band verdict.
// A catch case is IN BAND for 30% <= caught/trials <= 80% (both bounds
// inclusive, decided in exact rationals), else BELOW or ABOVE. A clean case
// has nothing to catch: it reports its false-alarm rate
// ((false_alarms + undecodable) / trials, the evidence rule's fail-safe
// direction) with band n/a.
//
// It NEVER writes evidence: it reads the same inputs the recorder reads and
// returns a value; the markdown table (CatchRateCalibration.Markdown) is for
// the PR. The live run is operator-executed (planreviewpersonalive_test.go);
// everything here is offline-tested with fake senders.

package agenteval

import (
	"context"
	"fmt"
	"math/big"
	"strings"
)

// CalibrationBand is the band verdict of one calibrated case.
type CalibrationBand string

const (
	// CalibrationInBand is a catch case caught in [30%, 80%] of its trials.
	CalibrationInBand CalibrationBand = "IN BAND"
	// CalibrationBelow is a catch case caught in under 30% of its trials.
	CalibrationBelow CalibrationBand = "BELOW"
	// CalibrationAbove is a catch case caught in over 80% of its trials.
	CalibrationAbove CalibrationBand = "ABOVE"
	// CalibrationNotApplicable is a clean case: it has nothing to catch.
	CalibrationNotApplicable CalibrationBand = "n/a"
)

// The calibration band's bounds, both inclusive, as exact rationals.
var (
	calibrationBandLow  = big.NewRat(3, 10)
	calibrationBandHigh = big.NewRat(8, 10)
)

// CatchCalibrationCase is one case's calibration result.
type CatchCalibrationCase struct {
	Case string
	// Expect is CatchExpectCatch or CatchExpectClean (from the arm table, so
	// a conventions arm's catch cases carry it too).
	Expect      string
	Trials      int
	Caught      int
	FalseAlarms int
	Undecodable int
	// Rate is caught / trials for a catch case (undecodable is a miss) and
	// (false_alarms + undecodable) / trials for a clean case.
	Rate float64
	Band CalibrationBand
}

// CatchRateCalibration is one arm's calibration dry run, cases in name order.
type CatchRateCalibration struct {
	Arm            string
	Kind           string
	SamplesPerCase int
	Cases          []CatchCalibrationCase
}

// calibrationBandFor is the band verdict of caught out of trials (trials >= 1).
func calibrationBandFor(caught, trials int) CalibrationBand {
	r := big.NewRat(int64(caught), int64(trials))
	switch {
	case r.Cmp(calibrationBandLow) < 0:
		return CalibrationBelow
	case r.Cmp(calibrationBandHigh) > 0:
		return CalibrationAbove
	}
	return CalibrationInBand
}

// CalibrateCatchRateArm runs the named arm of the arm table built from in at
// samples per case against sender and returns its per-case band table. Fail
// closed, with the zero calibration and a named error: samples < 1 or a nil
// sender (both checked before any input is read), an unavailable input, an
// arm absent from the table, a transport or render error, and a report
// missing a case. It never writes evidence.
func CalibrateCatchRateArm(ctx context.Context, sender MessageSender, in CatchRateInputs, arm string, samples int) (CatchRateCalibration, error) {
	if samples < 1 {
		return CatchRateCalibration{}, fmt.Errorf("agenteval: calibrating arm %q: samples must be >= 1, got %d", arm, samples)
	}
	if sender == nil {
		return CatchRateCalibration{}, fmt.Errorf("agenteval: calibrating arm %q: sender is required", arm)
	}
	rows, err := catchRateArmTable(in)
	if err != nil {
		return CatchRateCalibration{}, fmt.Errorf("agenteval: calibrating arm %q: %w", arm, err)
	}
	var row *catchRateArmRow
	names := make([]string, 0, len(rows))
	for i := range rows {
		names = append(names, rows[i].name)
		if rows[i].name == arm {
			row = &rows[i]
		}
	}
	if row == nil {
		return CatchRateCalibration{}, fmt.Errorf("agenteval: calibrating arm %q: no such arm in the arm table (known: %s)", arm, strings.Join(names, ", "))
	}
	report, err := row.run(ctx, sender, samples)
	if err != nil {
		return CatchRateCalibration{}, fmt.Errorf("agenteval: calibrating arm %q: %w", arm, err)
	}
	return calibrationFromReport(*row, report, samples)
}

// calibrationFromReport scores one arm report against its arm-table row. It
// refuses a row case the report has no trials for.
func calibrationFromReport(row catchRateArmRow, report CatchRateArmReport, samples int) (CatchRateCalibration, error) {
	out := CatchRateCalibration{Arm: row.name, Kind: row.kind, SamplesPerCase: samples, Cases: make([]CatchCalibrationCase, 0, len(row.cases))}
	for _, c := range row.cases {
		counts, ok := report.PerCase[c.name]
		if !ok || counts.Trials < 1 {
			return CatchRateCalibration{}, fmt.Errorf("agenteval: calibrating arm %q: the measurement has no trials for case %q", row.name, c.name)
		}
		cc := CatchCalibrationCase{Case: c.name, Expect: c.expect, Trials: counts.Trials, Caught: counts.Caught,
			FalseAlarms: counts.FalseAlarms, Undecodable: counts.Undecodable}
		if c.expect == CatchExpectClean {
			cc.Rate = float64(counts.FalseAlarms+counts.Undecodable) / float64(counts.Trials)
			cc.Band = CalibrationNotApplicable
		} else {
			cc.Rate = float64(counts.Caught) / float64(counts.Trials)
			cc.Band = calibrationBandFor(counts.Caught, counts.Trials)
		}
		out.Cases = append(out.Cases, cc)
	}
	return out, nil
}

// OutOfBand names every CATCH case whose band is not IN BAND, in case order.
// A clean case (band n/a) is never out of band.
func (c CatchRateCalibration) OutOfBand() []string {
	var out []string
	for _, cc := range c.Cases {
		if cc.Expect != CatchExpectClean && cc.Band != CalibrationInBand {
			out = append(out, cc.Case)
		}
	}
	return out
}

// Markdown renders the calibration as the PR's band table.
func (c CatchRateCalibration) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Calibration dry run: arm `%s` (%s), %d samples per case. Band: a catch case is IN BAND for 30%% <= caught/trials <= 80%%; a clean case reports its false-alarm rate ((false_alarms + undecodable) / trials), band n/a.\n\n",
		c.Arm, c.Kind, c.SamplesPerCase)
	b.WriteString("| Case | Expect | Trials | Caught | False alarms | Undecodable | Rate | Band |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, cc := range c.Cases {
		caught, alarms, rate := fmt.Sprint(cc.Caught), "-", fmt.Sprintf("%.1f%% caught", 100*cc.Rate)
		if cc.Expect == CatchExpectClean {
			caught, alarms, rate = "-", fmt.Sprint(cc.FalseAlarms), fmt.Sprintf("%.1f%% false alarm", 100*cc.Rate)
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %s | %d | %s | %s |\n", cc.Case, cc.Expect, cc.Trials, caught, alarms, cc.Undecodable, rate, cc.Band)
	}
	if oob := c.OutOfBand(); len(oob) > 0 {
		fmt.Fprintf(&b, "\nOut of band: %s.\n", strings.Join(oob, ", "))
	} else {
		b.WriteString("\nEvery catch case is in band.\n")
	}
	return b.String()
}
