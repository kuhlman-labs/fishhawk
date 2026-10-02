package spec

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	// The zone database is embedded so a schedule's `timezone` validates and
	// evaluates identically on a developer host and on a minimal image that
	// ships no /usr/share/zoneinfo (https://pkg.go.dev/time/tzdata). Without
	// it `fishhawk validate` could accept a zone the deployed fishhawkd then
	// cannot load — or the reverse.
	_ "time/tzdata"
)

// Schedule is a workflow's declared cadence (E79.1 / #3725): the `schedule`
// member of a workflow-v2 workflow. Nil on Workflow means the workflow is
// never started by the scheduler.
//
// It sits BESIDE applies_to rather than inside it: applies_to is the shared
// Predicate also consumed by escalations.match and review_conventions, and a
// cadence is not a match criterion. The cross-field rule that keeps the two
// consistent (a schedule requires applies_to.trigger to list `scheduled`) is
// validateSchedule's.
type Schedule struct {
	// Cron is a five-field numeric cron expression (minute hour
	// day-of-month month day-of-week). See ParseSchedule for the grammar.
	Cron string `json:"cron" yaml:"cron"`
	// Timezone is the IANA zone the cron fields are evaluated in. Empty
	// means DefaultScheduleTimezone.
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	// Issue is an optional anchor issue number. Zero means the scheduled
	// run carries no issue anchor.
	Issue int `json:"issue,omitempty" yaml:"issue,omitempty"`
}

// DefaultScheduleTimezone is the zone a Schedule declaring no `timezone` is
// evaluated in.
const DefaultScheduleTimezone = "UTC"

// EffectiveTimezone returns the declared zone name, or DefaultScheduleTimezone
// when none was declared.
func (s Schedule) EffectiveTimezone() string {
	if s.Timezone == "" {
		return DefaultScheduleTimezone
	}
	return s.Timezone
}

// ScheduleErrorKind classifies a ParseSchedule failure. Each kind maps to one
// named validation message (MsgFmtScheduleCronInvalid,
// MsgFmtScheduleTimezoneUnknown, MsgFmtScheduleNeverFires).
type ScheduleErrorKind string

const (
	// ScheduleErrCronInvalid means the cron expression does not parse.
	ScheduleErrCronInvalid ScheduleErrorKind = "cron_invalid"
	// ScheduleErrTimezoneUnknown means the timezone is not a loadable IANA name.
	ScheduleErrTimezoneUnknown ScheduleErrorKind = "timezone_unknown"
	// ScheduleErrNeverFires means the cron parses but matches no calendar date.
	ScheduleErrNeverFires ScheduleErrorKind = "never_fires"
)

// ScheduleError is ParseSchedule's typed failure. Detail carries the
// field-naming reason for ScheduleErrCronInvalid and is empty otherwise.
type ScheduleError struct {
	Kind   ScheduleErrorKind
	Detail string
}

func (e *ScheduleError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("spec: schedule: %s: %s", e.Kind, e.Detail)
	}
	return fmt.Sprintf("spec: schedule: %s", e.Kind)
}

// scheduleHorizonYears bounds Next's search. It is deliberately longer than
// any gap a VALIDATED expression can have. The sparsest admissible expression
// is February 29th on one weekday — spelled with a star-led day-of-week so
// the AND day rule applies, e.g. `0 0 29 2 */7` (Sundays only) — which
// recurs at most every 40 years across a skipped century leap year (2100).
// Fifty years therefore keeps Next non-zero for every expression
// ParseSchedule accepts, so "never fires" is decided statically (neverFires)
// rather than by a search horizon that would make validation depend on the
// date it runs on.
const scheduleHorizonYears = 50

// prevLookbacks are Prev's growing windows. Each is tried in turn and the
// first one containing a fire decides, so a frequent schedule costs a handful
// of Next steps and a sparse one a handful more; the last entry is the full
// horizon, so Prev finds a fire whenever Next could.
var prevLookbacks = []time.Duration{
	time.Hour,
	25 * time.Hour,
	32 * 24 * time.Hour,
	367 * 24 * time.Hour,
	5 * 366 * 24 * time.Hour,
	scheduleHorizonYears * 366 * 24 * time.Hour,
}

// cronField describes one of the five positional cron fields.
type cronField struct {
	name     string
	min, max int
}

var cronFields = [5]cronField{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 7},
}

// bitset holds the matching values of one field (all fields fit in 64 bits).
type bitset uint64

func (b bitset) has(v int) bool { return v >= 0 && v < 64 && b&(1<<uint(v)) != 0 }

// CronSchedule is a parsed, evaluable Schedule.
type CronSchedule struct {
	minute, hour, dom, month, dow bitset
	// domStar / dowStar record that the field's text began with `*` —
	// Vixie cron's DOM_STAR / DOW_STAR flags. They decide the day rule in
	// dayMatches.
	domStar, dowStar bool
	loc              *time.Location
}

// Location returns the zone the schedule is evaluated in.
func (c *CronSchedule) Location() *time.Location { return c.loc }

// ParseSchedule parses and checks a Schedule. The cron grammar is five
// whitespace-separated NUMERIC fields — minute 0-59, hour 0-23, day-of-month
// 1-31, month 1-12, day-of-week 0-7 (0 and 7 both Sunday) — each a comma list
// of `*`, `N`, `A-B`, `*/S` or `A-B/S`. Names (MON, JAN), macros (@daily) and
// a bare `N/S` are refused. The timezone must be a loadable IANA name (empty
// means UTC; `Local` is refused because it means whatever the host is set
// to). An expression matching no calendar date at all (`0 0 30 2 *`) is
// refused as never-firing.
//
// Every failure is a *ScheduleError whose Kind names the validation mode.
func ParseSchedule(s Schedule) (*CronSchedule, error) {
	c, err := parseCron(s.Cron)
	if err != nil {
		return nil, err
	}
	loc, err := loadScheduleLocation(s.EffectiveTimezone())
	if err != nil {
		return nil, err
	}
	c.loc = loc
	if c.neverFires() {
		return nil, &ScheduleError{Kind: ScheduleErrNeverFires}
	}
	return c, nil
}

func loadScheduleLocation(name string) (*time.Location, error) {
	if name == "Local" {
		return nil, &ScheduleError{Kind: ScheduleErrTimezoneUnknown}
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, &ScheduleError{Kind: ScheduleErrTimezoneUnknown}
	}
	return loc, nil
}

func cronInvalid(format string, args ...any) error {
	return &ScheduleError{Kind: ScheduleErrCronInvalid, Detail: fmt.Sprintf(format, args...)}
}

func parseCron(expr string) (*CronSchedule, error) {
	trimmed := strings.TrimSpace(expr)
	if strings.HasPrefix(trimmed, "@") {
		return nil, cronInvalid("macro %q is not accepted; spell the five numeric fields (minute hour day-of-month month day-of-week)", trimmed)
	}
	parts := strings.Fields(trimmed)
	if len(parts) != len(cronFields) {
		return nil, cronInvalid("expected 5 whitespace-separated fields (minute hour day-of-month month day-of-week), got %d", len(parts))
	}
	var sets [5]bitset
	for i, f := range cronFields {
		set, err := parseCronField(f, parts[i])
		if err != nil {
			return nil, err
		}
		sets[i] = set
	}
	// Day-of-week 7 is Sunday, the same day as 0.
	dow := sets[4]
	if dow.has(7) {
		dow = (dow | 1) &^ (1 << 7)
	}
	return &CronSchedule{
		minute:  sets[0],
		hour:    sets[1],
		dom:     sets[2],
		month:   sets[3],
		dow:     dow,
		domStar: strings.HasPrefix(parts[2], "*"),
		dowStar: strings.HasPrefix(parts[4], "*"),
	}, nil
}

func parseCronField(f cronField, text string) (bitset, error) {
	var set bitset
	for _, item := range strings.Split(text, ",") {
		if item == "" {
			return 0, cronInvalid("%s field %q has an empty list item", f.name, text)
		}
		rangePart, stepPart, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := cronNumber(f, text, stepPart)
			if err != nil {
				return 0, err
			}
			if n < 1 {
				return 0, cronInvalid("%s field %q: step %q must be a positive integer", f.name, text, stepPart)
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case rangePart == "*":
		case strings.Contains(rangePart, "-"):
			a, b, _ := strings.Cut(rangePart, "-")
			var err error
			if lo, err = cronValue(f, text, a); err != nil {
				return 0, err
			}
			if hi, err = cronValue(f, text, b); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, cronInvalid("%s field %q: range %q starts after it ends", f.name, text, rangePart)
			}
		default:
			if hasStep {
				return 0, cronInvalid("%s field %q: a step needs a range (`*/S` or `A-B/S`), not a single value %q", f.name, text, rangePart)
			}
			v, err := cronValue(f, text, rangePart)
			if err != nil {
				return 0, err
			}
			lo, hi = v, v
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << uint(v)
		}
	}
	return set, nil
}

// cronNumber parses a non-negative decimal integer token. A token carrying
// anything but ASCII digits — a name such as MON or JAN, a sign — is refused
// with a message saying the grammar is numeric only.
func cronNumber(f cronField, text, tok string) (int, error) {
	if tok == "" {
		return 0, cronInvalid("%s field %q has an empty value", f.name, text)
	}
	for _, r := range tok {
		if r < '0' || r > '9' {
			return 0, cronInvalid("%s field %q: value %q is not numeric (only numbers are accepted; names such as MON or JAN are not)", f.name, text, tok)
		}
	}
	n, err := strconv.Atoi(tok)
	if err != nil {
		return 0, cronInvalid("%s field %q: value %q is not a usable number", f.name, text, tok)
	}
	return n, nil
}

// cronValue parses a field value and range-checks it against the field.
func cronValue(f cronField, text, tok string) (int, error) {
	n, err := cronNumber(f, text, tok)
	if err != nil {
		return 0, err
	}
	if n < f.min || n > f.max {
		return 0, cronInvalid("%s field %q: value %d is out of range %d-%d", f.name, text, n, f.min, f.max)
	}
	return n, nil
}

// daysInMonthMax is the most days each month can have (February in a leap
// year), indexed by month 1-12.
var daysInMonthMax = [13]int{0, 31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}

// neverFires reports whether the expression matches no calendar date at all.
// It is decided STATICALLY, never by searching from "now", so validation is
// independent of the date it runs on. Under the OR day rule (both day fields
// restricted) the day-of-week arm alone matches every week, so only the AND
// rule can starve: there the expression fires iff some declared month has a
// declared day-of-month that exists in it (February counted at 29 days).
// The day-of-week arm cannot starve it further, because every calendar date
// falls on every weekday across the 400-year Gregorian cycle.
func (c *CronSchedule) neverFires() bool {
	if !c.domStar && !c.dowStar {
		return false
	}
	for m := 1; m <= 12; m++ {
		if !c.month.has(m) {
			continue
		}
		for d := 1; d <= daysInMonthMax[m]; d++ {
			if c.dom.has(d) {
				return false
			}
		}
	}
	return true
}

// dayMatches applies Vixie cron's day rule: when BOTH day-of-month and
// day-of-week are restricted (neither field begins with `*`), a day matches
// if EITHER matches; otherwise both must.
func (c *CronSchedule) dayMatches(t time.Time) bool {
	domOK := c.dom.has(t.Day())
	dowOK := c.dow.has(int(t.Weekday()))
	if !c.domStar && !c.dowStar {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// Next returns the first fire time strictly after t, evaluated in the
// schedule's location, or the zero time when none falls within the search
// horizon (which no expression ParseSchedule accepts can exhaust).
//
// The search walks real INSTANTS, never constructed wall-clock times, so
// daylight-saving transitions fall out of the walk: a local time skipped by
// a spring-forward transition never occurs and so never fires, and a local
// time repeated by a fall-back transition occurs twice and fires twice (two
// distinct instants, two distinct windows).
func (c *CronSchedule) Next(t time.Time) time.Time {
	cur := t.In(c.loc)
	// The first candidate is the next whole local minute strictly after t.
	cur = cur.Add(time.Minute - time.Duration(cur.Second())*time.Second - time.Duration(cur.Nanosecond()))
	limit := t.AddDate(scheduleHorizonYears, 0, 0)
	for !cur.After(limit) {
		switch {
		case !c.month.has(int(cur.Month())):
			y, m, _ := cur.Date()
			cur = advanceTo(cur, time.Date(y, m+1, 1, 0, 0, 0, 0, c.loc))
		case !c.dayMatches(cur):
			y, m, d := cur.Date()
			cur = advanceTo(cur, time.Date(y, m, d+1, 0, 0, 0, 0, c.loc))
		case !c.hour.has(cur.Hour()):
			cur = nextLocalHour(cur)
		case !c.minute.has(cur.Minute()):
			cur = cur.Add(time.Minute)
		default:
			return cur
		}
	}
	return time.Time{}
}

// nextLocalHour advances a minute-aligned local instant to the next local
// hour boundary.
func nextLocalHour(cur time.Time) time.Time {
	return cur.Add(time.Duration(60-cur.Minute()) * time.Minute)
}

// advanceTo returns target when it lies after cur, and otherwise the next
// local hour boundary. time.Date may resolve a local midnight that a DST
// transition skipped to an instant BEFORE the transition (still on the
// current day); without this guard the walk would re-derive the same target
// forever.
func advanceTo(cur, target time.Time) time.Time {
	if target.After(cur) {
		return target
	}
	return nextLocalHour(cur)
}

// Prev returns the latest fire time at or before t, or the zero time when
// none falls within the search horizon. It steps Next forward from t-L for
// growing lookbacks L (prevLookbacks) and keeps the last value <= t, which is
// bounded and needs no reverse field arithmetic.
func (c *CronSchedule) Prev(t time.Time) time.Time {
	for _, lookback := range prevLookbacks {
		var last time.Time
		for fire := c.Next(t.Add(-lookback)); !fire.IsZero() && !fire.After(t); fire = c.Next(fire) {
			last = fire
		}
		if !last.IsZero() {
			return last
		}
	}
	return time.Time{}
}

// MsgFmtScheduleRequiresScheduledTrigger rejects a workflow declaring a
// `schedule` whose applies_to.trigger does not list `scheduled` — including a
// workflow with no applies_to and one whose applies_to declares no trigger
// criterion. Argument: the workflow name. Exported (and kept a single-line
// const) so the CLI's byte-identical copy can be held to it by a parity test.
const MsgFmtScheduleRequiresScheduledTrigger = "workflow %q declares a schedule but its applies_to.trigger does not list `scheduled`: every run the scheduler starts carries the `scheduled` trigger form, so a scheduled workflow must opt in to it explicitly — declare applies_to.trigger including scheduled (e.g. [scheduled, on_demand]) or remove the schedule. A trigger list that omits it would have every scheduled run refused at admission, and an absent one leaves the routing declaration silent about the runs that start this workflow."

// MsgFmtScheduleCronInvalid rejects a malformed schedule.cron. Arguments: the
// workflow name, the cron text, and the parser's reason naming the offending
// field and value.
const MsgFmtScheduleCronInvalid = "workflow %q declares schedule.cron %q, which is not a valid five-field numeric cron expression: %s"

// MsgFmtScheduleTimezoneUnknown rejects a schedule.timezone that is not a
// loadable IANA zone name. Arguments: the workflow name, the timezone.
const MsgFmtScheduleTimezoneUnknown = "workflow %q declares schedule.timezone %q, which is not a known IANA time zone name (e.g. UTC, America/Chicago, Europe/Berlin); `Local` is refused because it means whatever the host is set to"

// MsgFmtScheduleNeverFires rejects a schedule.cron that parses but matches no
// calendar date. Arguments: the workflow name, the cron text.
const MsgFmtScheduleNeverFires = "workflow %q declares schedule.cron %q, which can never fire: no declared month has a declared day-of-month (e.g. day 30 of February), so the scheduler would never start a run"

// validateSchedule checks a workflow's `schedule` (E79.1 / #3725). A nil
// schedule is valid. ORDER IS A CONTRACT, mirroring validateAppliesTo's
// structural-first reading: the cross-field trigger rule is reported first
// (a schedule the workflow's routing would refuse is wrong whatever its cron
// says), then the cron grammar, the timezone, and finally never-fires (only
// meaningful once the expression is well-formed). Every mode reports at
// /workflows/<name>/schedule with its own named message.
//
// Called AFTER validateAppliesTo, so the trigger list it reads is already a
// well-formed predicate. Version-agnostic in code but unreachable below
// major 2: no v0/v1 schema declares `schedule`. It is not inherited through
// `extends` (which folds stages only), so the rule reads this workflow's own
// applies_to against its own schedule.
func validateSchedule(name string, wf *Workflow) error {
	if wf == nil || wf.Schedule == nil {
		return nil
	}
	ptr := fmt.Sprintf("/workflows/%s/schedule", name)
	if !declaresScheduledTrigger(wf.AppliesTo) {
		return &ValidationError{Path: ptr, Message: fmt.Sprintf(MsgFmtScheduleRequiresScheduledTrigger, name)}
	}
	if _, err := ParseSchedule(*wf.Schedule); err != nil {
		var se *ScheduleError
		if !errors.As(err, &se) {
			return &ValidationError{Path: ptr, Message: err.Error()}
		}
		switch se.Kind {
		case ScheduleErrTimezoneUnknown:
			return &ValidationError{Path: ptr, Message: fmt.Sprintf(MsgFmtScheduleTimezoneUnknown, name, wf.Schedule.Timezone)}
		case ScheduleErrNeverFires:
			return &ValidationError{Path: ptr, Message: fmt.Sprintf(MsgFmtScheduleNeverFires, name, wf.Schedule.Cron)}
		default:
			return &ValidationError{Path: ptr, Message: fmt.Sprintf(MsgFmtScheduleCronInvalid, name, wf.Schedule.Cron, se.Detail)}
		}
	}
	return nil
}

// declaresScheduledTrigger reports whether a routing predicate explicitly
// lists the `scheduled` trigger form. An absent predicate, or one declaring
// no trigger criterion, does not.
func declaresScheduledTrigger(p *Predicate) bool {
	if p == nil {
		return false
	}
	for _, f := range p.Triggers {
		if f == TriggerScheduled {
			return true
		}
	}
	return false
}
