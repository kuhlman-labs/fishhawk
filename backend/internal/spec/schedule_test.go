package spec_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// scheduleWorkflowV2 renders a one-workflow v2 document named `sched` whose
// workflow-level block (applies_to / schedule, already indented four spaces)
// is supplied by the caller.
func scheduleWorkflowV2(block string) []byte {
	return []byte(`
version: "2"
workflows:
  sched:
` + block + `
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`)
}

const scheduledTrigger = `    applies_to:
      trigger: [scheduled, on_demand]
`

// scheduleBlock renders a `schedule:` block with the given cron and optional
// timezone (empty = absent).
func scheduleBlock(cron, tz string) string {
	b := "    schedule:\n      cron: \"" + cron + "\"\n"
	if tz != "" {
		b += "      timezone: \"" + tz + "\"\n"
	}
	return b
}

// requireScheduleValidationError asserts ParseBytes failed with a
// *spec.ValidationError at the schedule pointer, and returns its message.
func requireScheduleValidationError(t *testing.T, doc []byte) string {
	t.Helper()
	_, err := spec.ParseBytes(doc)
	if err == nil {
		t.Fatal("ParseBytes succeeded, want a schedule ValidationError")
	}
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("ParseBytes error = %T %v, want *spec.ValidationError", err, err)
	}
	if ve.Path != "/workflows/sched/schedule" {
		t.Errorf("ValidationError.Path = %q, want /workflows/sched/schedule", ve.Path)
	}
	return ve.Message
}

// TestParse_Schedule_V2RoundTrip is the positive control and the
// DisallowUnknownFields proof: a schema-permitted `schedule` the struct
// omitted would fail the typed decode outright.
func TestParse_Schedule_V2RoundTrip(t *testing.T) {
	s, err := spec.ParseBytes(scheduleWorkflowV2(scheduledTrigger + `    schedule:
      cron: "0 9 * * 1-5"
      timezone: America/Chicago
      issue: 42
`))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	got := s.Workflows["sched"].Schedule
	if got == nil {
		t.Fatal("Workflow.Schedule is nil, want the declared schedule")
	}
	want := spec.Schedule{Cron: "0 9 * * 1-5", Timezone: "America/Chicago", Issue: 42}
	if *got != want {
		t.Errorf("Workflow.Schedule = %+v, want %+v", *got, want)
	}
}

// TestParse_Schedule_Absent_IsNil pins that an unscheduled workflow parses to
// a nil Schedule — the scheduler's "never touch it" signal.
func TestParse_Schedule_Absent_IsNil(t *testing.T) {
	s, err := spec.ParseBytes(scheduleWorkflowV2(scheduledTrigger))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if got := s.Workflows["sched"].Schedule; got != nil {
		t.Errorf("Workflow.Schedule = %+v, want nil", *got)
	}
}

func TestSchedule_EffectiveTimezone_DefaultsToUTC(t *testing.T) {
	if got := (spec.Schedule{Cron: "* * * * *"}).EffectiveTimezone(); got != "UTC" {
		t.Errorf("EffectiveTimezone() = %q, want UTC", got)
	}
	if got := (spec.Schedule{Cron: "* * * * *", Timezone: "Europe/Berlin"}).EffectiveTimezone(); got != "Europe/Berlin" {
		t.Errorf("EffectiveTimezone() = %q, want Europe/Berlin", got)
	}
}

// TestValidate_Schedule_RequiresScheduledTrigger covers the cross-field mode
// in each of its three shapes. COUNTERFACTUAL (run, RED): deleting the
// validateSchedule call from validateWorkflow makes every case parse — the
// schema PERMITS the property, so nothing else in the path rejects it, and
// the fixture is otherwise a fully valid document.
func TestValidate_Schedule_RequiresScheduledTrigger(t *testing.T) {
	want := fmt.Sprintf(spec.MsgFmtScheduleRequiresScheduledTrigger, "sched")
	cases := map[string]string{
		"trigger list lacks scheduled": "    applies_to:\n      trigger: [on_demand]\n",
		"no applies_to at all":         "",
		"applies_to without trigger":   "    applies_to:\n      labels: [chore]\n",
	}
	for name, appliesTo := range cases {
		t.Run(name, func(t *testing.T) {
			got := requireScheduleValidationError(t, scheduleWorkflowV2(appliesTo+scheduleBlock("0 9 * * *", "")))
			if got != want {
				t.Errorf("message = %q, want %q", got, want)
			}
		})
	}
}

// TestValidate_Schedule_CronInvalid covers every malformed-cron arm. Each case
// asserts the NAMED message prefix plus the parser detail fragment that names
// the offending field / value, so a deleted guard that lets a DIFFERENT guard
// reject the input (an empty value instead of an empty list item, an
// unusable-number instead of the numeric-only reason) still goes RED.
func TestValidate_Schedule_CronInvalid(t *testing.T) {
	cases := []struct {
		name, cron, detail string
	}{
		{"too few fields", "0 9 * *", "expected 5 whitespace-separated fields (minute hour day-of-month month day-of-week), got 4"},
		{"too many fields", "0 9 * * * *", "got 6"},
		{"macro", "@daily", `macro "@daily" is not accepted`},
		{"minute out of range", "60 9 * * *", `minute field "60": value 60 is out of range 0-59`},
		{"hour out of range", "0 24 * * *", `hour field "24": value 24 is out of range 0-23`},
		{"day-of-month zero", "0 9 0 * *", `day-of-month field "0": value 0 is out of range 1-31`},
		{"month out of range", "0 9 * 13 *", `month field "13": value 13 is out of range 1-12`},
		{"day-of-week out of range", "0 9 * * 8", `day-of-week field "8": value 8 is out of range 0-7`},
		{"zero step", "*/0 9 * * *", `minute field "*/0": step "0" must be a positive integer`},
		{"step on a single value", "5/10 * * * *", `minute field "5/10": a step needs a range`},
		{"reversed range", "0 9 20-10 * *", `day-of-month field "20-10": range "20-10" starts after it ends`},
		{"day name", "0 9 * * MON", `day-of-week field "MON": value "MON" is not numeric (only numbers are accepted; names such as MON or JAN are not)`},
		{"month name", "0 9 1 JAN *", `month field "JAN": value "JAN" is not numeric`},
		{"empty list item", "0 9,,10 * * *", `hour field "9,,10" has an empty list item`},
		{"empty range end", "0 9-  * * *", `hour field "9-" has an empty value`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := requireScheduleValidationError(t, scheduleWorkflowV2(scheduledTrigger+scheduleBlock(tc.cron, "")))
			prefix := fmt.Sprintf(spec.MsgFmtScheduleCronInvalid, "sched", tc.cron, "")
			if !strings.HasPrefix(got, prefix) {
				t.Errorf("message = %q, want the MsgFmtScheduleCronInvalid prefix %q", got, prefix)
			}
			if !strings.Contains(got, tc.detail) {
				t.Errorf("message = %q, want detail containing %q", got, tc.detail)
			}
		})
	}
}

func TestValidate_Schedule_TimezoneUnknown(t *testing.T) {
	for _, tz := range []string{"Mars/Olympus_Mons", "Local", "UTC+5"} {
		t.Run(tz, func(t *testing.T) {
			got := requireScheduleValidationError(t, scheduleWorkflowV2(scheduledTrigger+scheduleBlock("0 9 * * *", tz)))
			if want := fmt.Sprintf(spec.MsgFmtScheduleTimezoneUnknown, "sched", tz); got != want {
				t.Errorf("message = %q, want %q", got, want)
			}
		})
	}
}

func TestValidate_Schedule_NeverFires(t *testing.T) {
	for _, cron := range []string{"0 0 30 2 *", "0 0 31 4,6,9,11 *", "0 0 30-31 2 */7"} {
		t.Run(cron, func(t *testing.T) {
			got := requireScheduleValidationError(t, scheduleWorkflowV2(scheduledTrigger+scheduleBlock(cron, "")))
			if want := fmt.Sprintf(spec.MsgFmtScheduleNeverFires, "sched", cron); got != want {
				t.Errorf("message = %q, want %q", got, want)
			}
		})
	}
}

// TestValidate_Schedule_SparseButValidExpressionsParse is the never-fires
// rule's other edge: an expression that fires rarely is not one that never
// fires, and the Vixie OR rule rescues a day-of-month no month has.
func TestValidate_Schedule_SparseButValidExpressionsParse(t *testing.T) {
	for _, cron := range []string{"0 0 29 2 *", "0 0 29 2 */7", "0 0 30 2 1", "0 0 31 * *"} {
		t.Run(cron, func(t *testing.T) {
			if _, err := spec.ParseBytes(scheduleWorkflowV2(scheduledTrigger + scheduleBlock(cron, ""))); err != nil {
				t.Fatalf("ParseBytes: %v", err)
			}
		})
	}
}

// TestValidate_Schedule_TriggerRuleReportedFirst pins the order contract: a
// workflow wrong in its routing AND its cron reports the routing rule.
func TestValidate_Schedule_TriggerRuleReportedFirst(t *testing.T) {
	got := requireScheduleValidationError(t, scheduleWorkflowV2(scheduleBlock("bogus", "Mars/Olympus_Mons")))
	if want := fmt.Sprintf(spec.MsgFmtScheduleRequiresScheduledTrigger, "sched"); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// TestParse_Schedule_SchemaShape pins the schema half: cron is required, the
// issue anchor is a positive integer, and the block is closed.
func TestParse_Schedule_SchemaShape(t *testing.T) {
	cases := map[string]string{
		"missing cron":  "    schedule:\n      timezone: UTC\n",
		"issue zero":    "    schedule:\n      cron: \"0 9 * * *\"\n      issue: 0\n",
		"unknown key":   "    schedule:\n      cron: \"0 9 * * *\"\n      every: 1h\n",
		"empty cron":    "    schedule:\n      cron: \"\"\n",
		"empty zone":    "    schedule:\n      cron: \"0 9 * * *\"\n      timezone: \"\"\n",
		"string issue":  "    schedule:\n      cron: \"0 9 * * *\"\n      issue: \"42\"\n",
		"not an object": "    schedule: \"0 9 * * *\"\n",
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := spec.ParseBytes(scheduleWorkflowV2(scheduledTrigger + block))
			var se *spec.SchemaError
			if !errors.As(err, &se) {
				t.Fatalf("ParseBytes error = %T %v, want *spec.SchemaError", err, err)
			}
		})
	}
}

func mustSchedule(t *testing.T, cron, tz string) *spec.CronSchedule {
	t.Helper()
	c, err := spec.ParseSchedule(spec.Schedule{Cron: cron, Timezone: tz})
	if err != nil {
		t.Fatalf("ParseSchedule(%q, %q): %v", cron, tz, err)
	}
	return c
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestParseSchedule_ErrorKinds(t *testing.T) {
	cases := []struct {
		s    spec.Schedule
		kind spec.ScheduleErrorKind
	}{
		{spec.Schedule{Cron: "0 9 * *"}, spec.ScheduleErrCronInvalid},
		{spec.Schedule{Cron: "0 9 * * *", Timezone: "Nowhere/Atall"}, spec.ScheduleErrTimezoneUnknown},
		{spec.Schedule{Cron: "0 0 30 2 *"}, spec.ScheduleErrNeverFires},
	}
	for _, tc := range cases {
		_, err := spec.ParseSchedule(tc.s)
		var se *spec.ScheduleError
		if !errors.As(err, &se) || se.Kind != tc.kind {
			t.Errorf("ParseSchedule(%+v) = %v, want ScheduleError kind %q", tc.s, err, tc.kind)
		}
	}
	c := mustSchedule(t, "0 9 * * *", "")
	if c.Location().String() != "UTC" {
		t.Errorf("Location() = %q, want UTC", c.Location())
	}
}

// cronCase is one Next / Prev expectation: from `at`, Next must return `next`
// and Prev must return `prev` (instants compared with Equal).
type cronCase struct {
	name, cron, tz string
	at, next, prev time.Time
}

func runCronCases(t *testing.T, cases []cronCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustSchedule(t, tc.cron, tc.tz)
			if !tc.next.IsZero() {
				if got := c.Next(tc.at); !got.Equal(tc.next) {
					t.Errorf("Next(%s) = %s, want %s", tc.at.UTC(), got.UTC(), tc.next.UTC())
				}
			}
			if !tc.prev.IsZero() {
				if got := c.Prev(tc.at); !got.Equal(tc.prev) {
					t.Errorf("Prev(%s) = %s, want %s", tc.at.UTC(), got.UTC(), tc.prev.UTC())
				}
			}
		})
	}
}

func utc(y int, m time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, m, d, h, mi, s, 0, time.UTC)
}

func TestCronSchedule_NextPrev_UTC(t *testing.T) {
	runCronCases(t, []cronCase{
		{"every 15 minutes", "*/15 * * * *", "", utc(2026, 10, 2, 10, 7, 0), utc(2026, 10, 2, 10, 15, 0), utc(2026, 10, 2, 10, 0, 0)},
		{"exact fire instant: Next strictly after, Prev returns it", "*/15 * * * *", "", utc(2026, 10, 2, 10, 15, 0), utc(2026, 10, 2, 10, 30, 0), utc(2026, 10, 2, 10, 15, 0)},
		{"sub-minute offset truncates", "*/15 * * * *", "", utc(2026, 10, 2, 10, 15, 30), utc(2026, 10, 2, 10, 30, 0), utc(2026, 10, 2, 10, 15, 0)},
		{"hour rollover", "*/15 * * * *", "", utc(2026, 10, 2, 10, 50, 0), utc(2026, 10, 2, 11, 0, 0), utc(2026, 10, 2, 10, 45, 0)},
		{"every minute", "* * * * *", "", utc(2026, 10, 2, 10, 7, 12), utc(2026, 10, 2, 10, 8, 0), utc(2026, 10, 2, 10, 7, 0)},
		{"daily across midnight and month end", "0 9 * * *", "", utc(2026, 10, 31, 10, 0, 0), utc(2026, 11, 1, 9, 0, 0), utc(2026, 10, 31, 9, 0, 0)},
		{"weekdays only (2026-10-03 is a Saturday)", "0 9 * * 1-5", "", utc(2026, 10, 3, 0, 0, 0), utc(2026, 10, 5, 9, 0, 0), utc(2026, 10, 2, 9, 0, 0)},
		{"range with step", "0 8-18/5 * * *", "", utc(2026, 10, 2, 13, 30, 0), utc(2026, 10, 2, 18, 0, 0), utc(2026, 10, 2, 13, 0, 0)},
		{"list", "0 9,17 * * *", "", utc(2026, 10, 2, 12, 0, 0), utc(2026, 10, 2, 17, 0, 0), utc(2026, 10, 2, 9, 0, 0)},
		{"month restriction rolls the year", "0 0 1 3 *", "", utc(2026, 10, 2, 0, 0, 0), utc(2027, 3, 1, 0, 0, 0), utc(2026, 3, 1, 0, 0, 0)},
		{"Feb 29 (leap day)", "0 0 29 2 *", "", utc(2026, 3, 1, 0, 0, 0), utc(2028, 2, 29, 0, 0, 0), utc(2024, 2, 29, 0, 0, 0)},
		// Vixie OR: both day fields restricted, so the 13th OR any Friday.
		// 2026-10-01 is a Thursday; 2026-10-13 is a Tuesday.
		{"vixie dom-or-dow: Friday arm", "0 0 13 * 5", "", utc(2026, 10, 1, 0, 0, 0), utc(2026, 10, 2, 0, 0, 0), utc(2026, 9, 25, 0, 0, 0)},
		{"vixie dom-or-dow: day-of-month arm", "0 0 13 * 5", "", utc(2026, 10, 10, 0, 0, 0), utc(2026, 10, 13, 0, 0, 0), utc(2026, 10, 9, 0, 0, 0)},
		// Star-led day-of-month (*/13 = 1,14,27) keeps the AND rule: a Friday
		// that is also the 1st, 14th or 27th. The next is 2026-11-27.
		{"star-led day field keeps AND", "0 0 */13 * 5", "", utc(2026, 10, 1, 0, 0, 0), utc(2026, 11, 27, 0, 0, 0), time.Time{}},
		{"day-of-week 7 is Sunday (2026-10-04)", "0 0 * * 7", "", utc(2026, 10, 2, 0, 0, 0), utc(2026, 10, 4, 0, 0, 0), utc(2026, 9, 27, 0, 0, 0)},
	})
}

// TestCronSchedule_DayOfWeekSevenEqualsZero pins the 0/7 Sunday alias by
// comparing the two spellings over a stretch of fires.
func TestCronSchedule_DayOfWeekSevenEqualsZero(t *testing.T) {
	seven, zero := mustSchedule(t, "30 6 * * 7", ""), mustSchedule(t, "30 6 * * 0", "")
	a, b := utc(2026, 1, 1, 0, 0, 0), utc(2026, 1, 1, 0, 0, 0)
	for i := 0; i < 20; i++ {
		a, b = seven.Next(a), zero.Next(b)
		if !a.Equal(b) || a.Weekday() != time.Sunday {
			t.Fatalf("fire %d: dow 7 -> %s, dow 0 -> %s, want equal Sundays", i, a, b)
		}
	}
}

// TestCronSchedule_DST_Chicago pins the window semantics across both 2026
// America/Chicago transitions (spring forward 2026-03-08 02:00 CST -> 03:00
// CDT; fall back 2026-11-01 02:00 CDT -> 01:00 CST).
func TestCronSchedule_DST_Chicago(t *testing.T) {
	chi := mustZone(t, "America/Chicago")
	local := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, chi) }
	runCronCases(t, []cronCase{
		// Daily 09:00 local fires at 15:00Z in CST and 14:00Z in CDT.
		{"daily 09:00 into spring-forward day", "0 9 * * *", "America/Chicago", utc(2026, 3, 7, 16, 0, 0), utc(2026, 3, 8, 14, 0, 0), utc(2026, 3, 7, 15, 0, 0)},
		{"daily 09:00 into fall-back day", "0 9 * * *", "America/Chicago", utc(2026, 10, 31, 15, 0, 0), utc(2026, 11, 1, 15, 0, 0), utc(2026, 10, 31, 14, 0, 0)},
		// 02:30 does not exist on 2026-03-08: that day yields NO fire.
		{"spring-forward skipped time never fires", "30 2 * * *", "America/Chicago", local(2026, 3, 7, 3, 0), local(2026, 3, 9, 2, 30), local(2026, 3, 7, 2, 30)},
		{"spring-forward: Prev on the skipped day returns the day before", "30 2 * * *", "America/Chicago", local(2026, 3, 8, 12, 0), local(2026, 3, 9, 2, 30), local(2026, 3, 7, 2, 30)},
		// 01:30 occurs twice on 2026-11-01: 06:30Z (CDT) and 07:30Z (CST).
		{"fall-back: first 01:30 (CDT)", "30 1 * * *", "America/Chicago", utc(2026, 11, 1, 5, 0, 0), utc(2026, 11, 1, 6, 30, 0), utc(2026, 10, 31, 6, 30, 0)},
		{"fall-back: second 01:30 (CST) is a distinct window", "30 1 * * *", "America/Chicago", utc(2026, 11, 1, 6, 30, 0), utc(2026, 11, 1, 7, 30, 0), utc(2026, 11, 1, 6, 30, 0)},
		{"fall-back: Prev inside the repeated hour", "30 1 * * *", "America/Chicago", utc(2026, 11, 1, 7, 45, 0), utc(2026, 11, 2, 7, 30, 0), utc(2026, 11, 1, 7, 30, 0)},
	})
}

// TestCronSchedule_SkippedLocalMidnight drives Next across a DST transition
// that skips local MIDNIGHT (America/Havana, 2026-03-08 00:00 CST -> 01:00
// CDT). time.Date resolves the nonexistent midnight to 23:00 the PREVIOUS
// day, so the day-advance would re-derive that same instant forever.
// COUNTERFACTUAL (run, RED as a test timeout): making advanceTo return its
// target unconditionally hangs this case — the fixture state that makes the
// deletion observable is a day-restricted cron (`8` only) evaluated from the
// 7th, which forces a day advance INTO the skipped midnight.
func TestCronSchedule_SkippedLocalMidnight(t *testing.T) {
	hav := mustZone(t, "America/Havana")
	c := mustSchedule(t, "0 12 8 * *", "America/Havana")
	got := c.Next(time.Date(2026, 3, 7, 13, 0, 0, 0, hav))
	if want := time.Date(2026, 3, 8, 12, 0, 0, 0, hav); !got.Equal(want) {
		t.Errorf("Next = %s, want %s", got, want)
	}
}

// TestCronSchedule_SparseExpressionWithinHorizon pins the horizon claim: the
// sparsest admissible expression (Feb 29 on Sundays only, via the AND rule a
// star-led day-of-week selects) is found from every start year across the
// skipped 2100 leap day.
func TestCronSchedule_SparseExpressionWithinHorizon(t *testing.T) {
	c := mustSchedule(t, "0 0 29 2 */7", "")
	for y := 2080; y <= 2110; y++ {
		from := utc(y, 1, 1, 0, 0, 0)
		got := c.Next(from)
		if got.IsZero() {
			t.Fatalf("Next(%d-01-01) = zero, want a Feb 29 Sunday within the horizon", y)
		}
		if got.Month() != time.February || got.Day() != 29 || got.Weekday() != time.Sunday {
			t.Fatalf("Next(%d-01-01) = %s, want a Feb 29 Sunday", y, got)
		}
		if p := c.Prev(got); !p.Equal(got) {
			t.Fatalf("Prev(%s) = %s, want the fire itself", got, p)
		}
	}
}
