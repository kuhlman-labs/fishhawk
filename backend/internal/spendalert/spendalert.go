// Package spendalert detects runaway model spend by comparing the
// current hour's estimated cost against a rolling average of recent
// hours. It is a warn-only signal (#649): an anomalous hour emits a
// spend_alert audit entry, it never blocks or fails a run.
//
// The detector is deliberately a pure function over priced cost
// samples (the backend reads them from the cost_recorded audit
// entries). Keeping it free of any repository dependency makes the
// trip condition trivially testable and means the wiring in the trace
// handler only has to shuttle samples in and an audit entry out.
//
// Why hourly buckets and a relative multiple rather than an absolute
// dollar ceiling: spend varies enormously between an idle loop and a
// busy one, so a fixed ceiling is either always tripping or never
// useful. A multiple of the recent rolling average catches the shape
// we actually care about — a sudden blowup relative to this
// deployment's own normal — whether that normal is cents or dollars
// per hour. The classic triggers are a runaway agent loop and an
// injection-driven token explosion.
package spendalert

import "time"

// DefaultMultiple is the trip threshold used when the operator hasn't
// configured one: the current hour's spend must exceed 3x the rolling
// average of prior hours before an alert fires. Chosen to be well
// clear of normal hour-to-hour variance while still catching a genuine
// blowup.
const DefaultMultiple = 3.0

// Window is how far back the rolling average looks. Only prior hour
// buckets that fall within [latestHour-Window, latestHour) and carry
// spend contribute to the average, so the baseline reflects recent
// activity rather than being diluted by long-idle stretches.
const Window = 24 * time.Hour

// Sample is one priced cost observation: when the spend happened and
// how many estimated US dollars it was. The backend builds these from
// the run's cost_recorded audit entries.
type Sample struct {
	Time time.Time
	USD  float64
}

// Decision is the outcome of an Evaluate call. It is fully populated
// whether or not the alert tripped so the caller can log the figures
// either way; Tripped is the only field that gates emission.
type Decision struct {
	// Tripped is true when LatestHourUSD exceeds Multiple * RollingAvgUSD
	// against a usable baseline. It is the sole emit gate.
	Tripped bool
	// LatestHourUSD is the summed spend in the bucket containing `now`.
	LatestHourUSD float64
	// RollingAvgUSD is the mean spend across the prior hour buckets that
	// carried any spend within Window. Zero when there is no baseline.
	RollingAvgUSD float64
	// Ratio is LatestHourUSD / RollingAvgUSD, or 0 when there is no
	// baseline. Surfaced so the alert payload can report "how anomalous."
	Ratio float64
	// Multiple is the threshold that was applied (after defaulting).
	Multiple float64
	// PriorHours is the count of prior hour buckets that contributed to
	// RollingAvgUSD. A trip is suppressed when this is zero — without a
	// baseline there is nothing to be anomalous against.
	PriorHours int
	// LatestHourStart is the UTC start of the current hour bucket.
	LatestHourStart time.Time
}

// Evaluate buckets the samples by UTC hour, computes the rolling
// average spend across the prior hour buckets within Window, and
// reports whether the bucket containing `now` exceeds multiple times
// that average.
//
// A non-positive multiple falls back to DefaultMultiple. The alert is
// suppressed (Tripped=false) when there is no usable baseline — no
// prior hour within Window carried spend — because a first-ever or
// long-idle hour has nothing meaningful to be compared against. This
// keeps the signal quiet until enough history exists to make "3x
// normal" a real statement.
func Evaluate(samples []Sample, now time.Time, multiple float64) Decision {
	if multiple <= 0 {
		multiple = DefaultMultiple
	}

	latestHourStart := now.UTC().Truncate(time.Hour)
	windowStart := latestHourStart.Add(-Window)

	d := Decision{Multiple: multiple, LatestHourStart: latestHourStart}

	// priorByHour sums spend per prior hour bucket so the average is
	// over distinct hours, not over sample count (one expensive hour
	// with many samples shouldn't dominate).
	priorByHour := make(map[time.Time]float64)
	for _, s := range samples {
		bucket := s.Time.UTC().Truncate(time.Hour)
		switch {
		case bucket.Equal(latestHourStart):
			d.LatestHourUSD += s.USD
		case !bucket.Before(windowStart) && bucket.Before(latestHourStart):
			priorByHour[bucket] += s.USD
		default:
			// Older than the window or in a future bucket — ignored.
		}
	}

	var total float64
	for _, v := range priorByHour {
		total += v
		d.PriorHours++
	}
	if d.PriorHours == 0 || total <= 0 {
		// No baseline: nothing to compare against, so never trip.
		return d
	}

	d.RollingAvgUSD = total / float64(d.PriorHours)
	d.Ratio = d.LatestHourUSD / d.RollingAvgUSD
	d.Tripped = d.LatestHourUSD > multiple*d.RollingAvgUSD
	return d
}
