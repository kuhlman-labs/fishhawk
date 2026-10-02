package spec

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The CLI half of the workflow `schedule` grammar (E79.1 / #3725). Every
// validation-mode test below drives the REAL `fishhawk validate` path
// (ValidateBytes: schema, then the semantic sweep) over a fully valid v2
// document whose only defect is the one the mode names, and asserts the entry
// at /workflows/sched/schedule carrying the backend's byte-identical named
// message.

const (
	scheduleBackendSource = "../../../backend/internal/spec/schedule.go"
	scheduleCLISource     = "schedule.go"
)

// TestScheduleMessageParityAcrossModules is the MECHANICAL drift guard for the
// four hand-duplicated schedule message constants (the
// TestCharterMessageParityAcrossModules posture): the backend and the CLI live
// in separate Go modules and cannot share a constant, so parity is asserted by
// reading BOTH source files and requiring each constant declared as the
// identical single-line `const NAME = "<value>"`. `want` is derived from THIS
// package's value, so it matches the VALUE, not just the name.
//
// COUNTERFACTUAL (run, RED): changing one character of the CLI's
// MsgFmtScheduleNeverFires makes `want` stop matching the unchanged backend
// source.
func TestScheduleMessageParityAcrossModules(t *testing.T) {
	lines := []string{
		"const MsgFmtScheduleRequiresScheduledTrigger = " + strconv.Quote(MsgFmtScheduleRequiresScheduledTrigger),
		"const MsgFmtScheduleCronInvalid = " + strconv.Quote(MsgFmtScheduleCronInvalid),
		"const MsgFmtScheduleTimezoneUnknown = " + strconv.Quote(MsgFmtScheduleTimezoneUnknown),
		"const MsgFmtScheduleNeverFires = " + strconv.Quote(MsgFmtScheduleNeverFires),
	}
	for _, path := range []string{scheduleBackendSource, scheduleCLISource} {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, want := range lines {
			if !strings.Contains(string(src), want) {
				t.Errorf("%s does not declare the schedule message verbatim;\nwant the line: %s\n"+
					"The two modules cannot share a constant, so the backend and CLI copies must stay byte-identical or "+
					"`fishhawk validate` and the backend will report different text for the same schedule refusal.", path, want)
			}
		}
	}
}

// scheduleParityExempt names the declarations each side may carry ALONE: the
// backend's validation entry point is bound to its typed *Workflow, the CLI's
// to this package's raw yaml.v3 tree. Everything else is the shared parser.
var scheduleParityExempt = map[string]map[string]bool{
	scheduleBackendSource: {"validateSchedule": true, "declaresScheduledTrigger": true},
	scheduleCLISource:     {"checkSchedule": true, "scheduleDeclaresScheduledTrigger": true},
}

// scheduleDecls parses a Go source file and returns its top-level non-import
// declarations keyed by name (methods as Recv.Name), each mapped to its exact
// SOURCE TEXT — doc comments excluded, so each copy may document itself, but
// every in-body comment, string and token included.
func scheduleDecls(t *testing.T, path string) map[string]string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	text := func(n ast.Node) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) == 1 {
				recv := d.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			out[name] = text(d)
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					out[s.Name.Name] = d.Tok.String() + " " + text(s)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						out[n.Name] = d.Tok.String() + " " + text(s)
					}
				}
			}
		}
	}
	return out
}

// TestScheduleSourceParityAcrossModules holds the WHOLE ported parser — not
// only the four message constants — byte-identical to the backend's. The
// cron-invalid message embeds the parser's detail string (MsgFmtScheduleCronInvalid's
// last %s), so the message is only byte-identical across modules if every
// cronInvalid format string is too; and the evaluation (Next / Prev / the
// Vixie day rule / never-fires) must agree for `fishhawk validate` to accept
// exactly what the backend accepts. It is BIDIRECTIONAL: a declaration either
// side adds without the other (bar scheduleParityExempt) fails, so a future
// backend parser change cannot land unported.
//
// COUNTERFACTUAL (run, RED): editing one character of a cronInvalid detail
// string in the CLI's parseCronField (a string NOT covered by the message
// const test) fails this test naming parseCronField.
func TestScheduleSourceParityAcrossModules(t *testing.T) {
	backend := scheduleDecls(t, scheduleBackendSource)
	cli := scheduleDecls(t, scheduleCLISource)
	for path, decls := range map[string]map[string]string{scheduleBackendSource: backend, scheduleCLISource: cli} {
		for name := range scheduleParityExempt[path] {
			if _, ok := decls[name]; !ok {
				t.Errorf("%s no longer declares exempt %s; update scheduleParityExempt", path, name)
			}
		}
	}
	var compared []string
	for name, b := range backend {
		if scheduleParityExempt[scheduleBackendSource][name] {
			continue
		}
		c, ok := cli[name]
		if !ok {
			t.Errorf("%s declares %s but %s does not; port it verbatim", scheduleBackendSource, name, scheduleCLISource)
			continue
		}
		if b != c {
			t.Errorf("%s differs between modules; the CLI copy must be byte-identical to the backend's.\nbackend:\n%s\ncli:\n%s", name, b, c)
		}
		compared = append(compared, name)
	}
	for name := range cli {
		if scheduleParityExempt[scheduleCLISource][name] {
			continue
		}
		if _, ok := backend[name]; !ok {
			t.Errorf("%s declares %s but %s does not; the CLI may only add the exempt raw-map entry point", scheduleCLISource, name, scheduleBackendSource)
		}
	}
	// Non-vacuity: the load-bearing declarations were actually compared.
	sort.Strings(compared)
	for _, must := range []string{
		"ParseSchedule", "parseCron", "parseCronField", "cronNumber", "cronValue",
		"CronSchedule.Next", "CronSchedule.Prev", "CronSchedule.dayMatches", "CronSchedule.neverFires",
		"MsgFmtScheduleRequiresScheduledTrigger", "MsgFmtScheduleCronInvalid",
		"MsgFmtScheduleTimezoneUnknown", "MsgFmtScheduleNeverFires",
	} {
		if i := sort.SearchStrings(compared, must); i >= len(compared) || compared[i] != must {
			t.Errorf("parity never compared %s (compared: %v)", must, compared)
		}
	}
}

// --- validation modes through ValidateBytes ---------------------------------

// scheduleDocV2 renders a one-workflow v2 document named `sched` whose
// workflow-level block (applies_to / schedule, already indented four spaces)
// is supplied by the caller. The document is otherwise fully valid, so the
// block is the only thing a test varies.
func scheduleDocV2(block string) []byte {
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

const scheduleTriggerBlock = `    applies_to:
      trigger: [scheduled, on_demand]
`

func scheduleBlockFor(cron, tz string) string {
	b := "    schedule:\n      cron: \"" + cron + "\"\n"
	if tz != "" {
		b += "      timezone: \"" + tz + "\"\n"
	}
	return b
}

// requireScheduleEntry asserts ValidateBytes failed with exactly ONE entry, at
// the schedule pointer, and returns its message — exactly one, because the
// backend's first-error return reports one schedule error per workflow.
func requireScheduleEntry(t *testing.T, doc []byte) string {
	t.Helper()
	err := ValidateBytes(doc)
	if err == nil {
		t.Fatal("ValidateBytes succeeded, want a schedule ValidationError")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("ValidateBytes error = %T %v, want *ValidationError", err, err)
	}
	if len(ve.Errors) != 1 {
		t.Fatalf("ValidateBytes entries = %+v, want exactly one schedule entry", ve.Errors)
	}
	if got := ve.Errors[0].Path; got != "/workflows/sched/schedule" {
		t.Errorf("entry path = %q, want /workflows/sched/schedule", got)
	}
	return ve.Errors[0].Message
}

// TestValidateBytes_Schedule_Accepted is the positive control: a well-formed
// schedule on a workflow that opts in to `scheduled` validates clean, so every
// RED below is the named mode and not a broken fixture.
func TestValidateBytes_Schedule_Accepted(t *testing.T) {
	doc := scheduleDocV2(scheduleTriggerBlock + "    schedule:\n      cron: \"0 9 * * 1-5\"\n      timezone: America/Chicago\n      issue: 42\n")
	if err := ValidateBytes(doc); err != nil {
		t.Fatalf("ValidateBytes: %v", err)
	}
	if err := ValidateBytes(scheduleDocV2(scheduleTriggerBlock)); err != nil {
		t.Fatalf("ValidateBytes (no schedule): %v", err)
	}
}

// TestValidateBytes_Schedule_RequiresScheduledTrigger covers the cross-field
// mode in each of its three shapes.
//
// COUNTERFACTUAL (run, RED): deleting the checkSchedule call from
// validateAgentVersions makes every case validate clean. The fixture state
// that makes the deletion observable: a well-formed cron ("0 9 * * *") on an
// otherwise valid document, so the schema — which PERMITS `schedule` — admits
// it, and the missing `scheduled` trigger is the ONLY defect left for anything
// to reject. Mutating scheduleDeclaresScheduledTrigger to return true is RED
// here too (no entry at all, since the cron is valid).
func TestValidateBytes_Schedule_RequiresScheduledTrigger(t *testing.T) {
	want := fmt.Sprintf(MsgFmtScheduleRequiresScheduledTrigger, "sched")
	cases := map[string]string{
		"trigger list lacks scheduled": "    applies_to:\n      trigger: [on_demand]\n",
		"no applies_to at all":         "",
		"applies_to without trigger":   "    applies_to:\n      labels: [chore]\n",
	}
	for name, appliesTo := range cases {
		t.Run(name, func(t *testing.T) {
			if got := requireScheduleEntry(t, scheduleDocV2(appliesTo+scheduleBlockFor("0 9 * * *", ""))); got != want {
				t.Errorf("message = %q, want %q", got, want)
			}
		})
	}
}

// TestValidateBytes_Schedule_CronInvalid covers every malformed-cron arm,
// asserting the NAMED message prefix plus the parser detail naming the
// offending field / value.
//
// The fixture state that makes a deleted parser guard observable: each cron
// has exactly ONE defect, on a workflow whose trigger rule is satisfied and
// whose timezone is the default, so the detail fragment can only come from
// the guard under test — a deleted guard either admits the input (no entry)
// or lets a DIFFERENT guard reject it (a different detail), both RED.
func TestValidateBytes_Schedule_CronInvalid(t *testing.T) {
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
			got := requireScheduleEntry(t, scheduleDocV2(scheduleTriggerBlock+scheduleBlockFor(tc.cron, "")))
			prefix := fmt.Sprintf(MsgFmtScheduleCronInvalid, "sched", tc.cron, "")
			if !strings.HasPrefix(got, prefix) {
				t.Errorf("message = %q, want the MsgFmtScheduleCronInvalid prefix %q", got, prefix)
			}
			if !strings.Contains(got, tc.detail) {
				t.Errorf("message = %q, want detail containing %q", got, tc.detail)
			}
		})
	}
}

// TestValidateBytes_Schedule_TimezoneUnknown — the fixture state that makes a
// deleted timezone case observable: a VALID cron, so with the
// ScheduleErrTimezoneUnknown case gone the error falls to the switch default
// and renders MsgFmtScheduleCronInvalid instead (RED on the exact message).
func TestValidateBytes_Schedule_TimezoneUnknown(t *testing.T) {
	for _, tz := range []string{"Mars/Olympus_Mons", "Local", "UTC+5"} {
		t.Run(tz, func(t *testing.T) {
			got := requireScheduleEntry(t, scheduleDocV2(scheduleTriggerBlock+scheduleBlockFor("0 9 * * *", tz)))
			if want := fmt.Sprintf(MsgFmtScheduleTimezoneUnknown, "sched", tz); got != want {
				t.Errorf("message = %q, want %q", got, want)
			}
		})
	}
}

// TestValidateBytes_Schedule_NeverFires — the fixture state that makes a
// deleted never-fires case observable: a grammatically valid cron in the
// default zone, so with the ScheduleErrNeverFires case gone the error falls to
// the switch default (MsgFmtScheduleCronInvalid with an empty detail), RED on
// the exact message.
func TestValidateBytes_Schedule_NeverFires(t *testing.T) {
	for _, cron := range []string{"0 0 30 2 *", "0 0 31 4,6,9,11 *", "0 0 30-31 2 */7"} {
		t.Run(cron, func(t *testing.T) {
			got := requireScheduleEntry(t, scheduleDocV2(scheduleTriggerBlock+scheduleBlockFor(cron, "")))
			if want := fmt.Sprintf(MsgFmtScheduleNeverFires, "sched", cron); got != want {
				t.Errorf("message = %q, want %q", got, want)
			}
		})
	}
}

// TestValidateBytes_Schedule_SparseButValidExpressionsValidate is the
// never-fires rule's other edge: an expression that fires rarely is not one
// that never fires, and the Vixie OR rule rescues a day-of-month no month has.
func TestValidateBytes_Schedule_SparseButValidExpressionsValidate(t *testing.T) {
	for _, cron := range []string{"0 0 29 2 *", "0 0 29 2 */7", "0 0 30 2 1", "0 0 31 * *"} {
		t.Run(cron, func(t *testing.T) {
			if err := ValidateBytes(scheduleDocV2(scheduleTriggerBlock + scheduleBlockFor(cron, ""))); err != nil {
				t.Fatalf("ValidateBytes: %v", err)
			}
		})
	}
}

// TestValidateBytes_Schedule_TriggerRuleReportedFirst pins the backend's order
// contract: a workflow wrong in its routing AND its cron AND its timezone
// reports the routing rule, and only that one entry. The fixture state that
// makes a reordering observable: all three defects at once, so moving the
// trigger check after ParseSchedule reports the cron message instead (RED).
func TestValidateBytes_Schedule_TriggerRuleReportedFirst(t *testing.T) {
	got := requireScheduleEntry(t, scheduleDocV2(scheduleBlockFor("bogus", "Mars/Olympus_Mons")))
	if want := fmt.Sprintf(MsgFmtScheduleRequiresScheduledTrigger, "sched"); got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// TestValidateBytes_Schedule_AppliesToEntryLeads pins the one documented
// divergence from the backend's first-error return: a workflow whose
// applies_to is itself invalid reports that entry FIRST (matching the
// backend's single error), and the collected sweep may add the schedule entry
// after it.
func TestValidateBytes_Schedule_AppliesToEntryLeads(t *testing.T) {
	// change_kind passes the schema but is refused by checkAppliesTo's rung 1.
	doc := scheduleDocV2("    applies_to:\n      trigger: [scheduled]\n      change_kind: [feature]\n" + scheduleBlockFor("0 0 30 2 *", ""))
	err := ValidateBytes(doc)
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Errors) != 2 {
		t.Fatalf("ValidateBytes = %v, want a ValidationError with an applies_to and a schedule entry", err)
	}
	if got := ve.Errors[0].Path; !strings.HasPrefix(got, "/workflows/sched/applies_to") {
		t.Errorf("first entry path = %q, want under /workflows/sched/applies_to (entries %+v)", got, ve.Errors)
	}
	if got, want := ve.Errors[1].Message, fmt.Sprintf(MsgFmtScheduleNeverFires, "sched", "0 0 30 2 *"); got != want {
		t.Errorf("second entry message = %q, want %q", got, want)
	}
}

// TestValidateBytes_Schedule_SchemaShape pins the schema half: cron is
// required, the issue anchor is a positive integer, and the block is closed —
// each refused by the CLI's schema mirror before the semantic sweep runs.
func TestValidateBytes_Schedule_SchemaShape(t *testing.T) {
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
			err := ValidateBytes(scheduleDocV2(scheduleTriggerBlock + block))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("ValidateBytes = %T %v, want a schema *ValidationError", err, err)
			}
			for _, e := range ve.Errors {
				if strings.HasPrefix(e.Message, "workflow \"sched\" declares schedule") {
					t.Errorf("entry %+v is the semantic sweep's; want the schema to refuse this shape first", e)
				}
			}
		})
	}
}

// TestCheckSchedule_ShapeTolerance drives checkSchedule directly over raw
// trees the schema would have rejected, pinning that the sweep SKIPS a shape
// it cannot read rather than coercing it. Each skip fixture pairs the bad
// shape with an otherwise-REFUSABLE schedule (a never-firing cron, an absent
// trigger), so a deleted skip guard is observable as a spurious entry.
func TestCheckSchedule_ShapeTolerance(t *testing.T) {
	trig := map[string]any{"trigger": []any{"scheduled"}}
	cases := []struct {
		name string
		wf   map[string]any
		want int
	}{
		{"no schedule", map[string]any{}, 0},
		{"schedule not an object", map[string]any{"schedule": "0 0 30 2 *"}, 0},
		// Deleting the cron guard makes cron "" — a 0-field cron-invalid entry.
		{"cron not a string", map[string]any{"applies_to": trig, "schedule": map[string]any{"cron": 5}}, 0},
		// Deleting the timezone guard evaluates in UTC — a never-fires entry.
		{"timezone not a string", map[string]any{"applies_to": trig, "schedule": map[string]any{"cron": "0 0 30 2 *", "timezone": 5}}, 0},
		{"applies_to not an object", map[string]any{"applies_to": "scheduled", "schedule": map[string]any{"cron": "0 9 * * *"}}, 1},
		{"trigger not a string list", map[string]any{"applies_to": map[string]any{"trigger": []any{"scheduled", 1}}, "schedule": map[string]any{"cron": "0 9 * * *"}}, 1},
		{"valid", map[string]any{"applies_to": trig, "schedule": map[string]any{"cron": "0 9 * * *", "timezone": "Europe/Berlin"}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errs []ValidationErrorEntry
			checkSchedule(tc.wf, "sched", &errs)
			if len(errs) != tc.want {
				t.Fatalf("checkSchedule entries = %+v, want %d", errs, tc.want)
			}
			if tc.want == 1 && errs[0].Message != fmt.Sprintf(MsgFmtScheduleRequiresScheduledTrigger, "sched") {
				t.Errorf("message = %q, want the trigger-rule message", errs[0].Message)
			}
		})
	}
}

// --- the parser: error kinds and a shared cron table -------------------------

func TestParseSchedule_ErrorKinds(t *testing.T) {
	cases := []struct {
		s    Schedule
		kind ScheduleErrorKind
	}{
		{Schedule{Cron: "0 9 * *"}, ScheduleErrCronInvalid},
		{Schedule{Cron: "0 9 * * *", Timezone: "Nowhere/Atall"}, ScheduleErrTimezoneUnknown},
		{Schedule{Cron: "0 0 30 2 *"}, ScheduleErrNeverFires},
	}
	for _, tc := range cases {
		_, err := ParseSchedule(tc.s)
		var se *ScheduleError
		if !errors.As(err, &se) || se.Kind != tc.kind {
			t.Errorf("ParseSchedule(%+v) = %v, want ScheduleError kind %q", tc.s, err, tc.kind)
		}
	}
	c := scheduleMust(t, "0 9 * * *", "")
	if c.Location().String() != "UTC" {
		t.Errorf("Location() = %q, want UTC", c.Location())
	}
	if got := (Schedule{Cron: "* * * * *"}).EffectiveTimezone(); got != DefaultScheduleTimezone {
		t.Errorf("EffectiveTimezone() = %q, want %q", got, DefaultScheduleTimezone)
	}
}

func scheduleMust(t *testing.T, cron, tz string) *CronSchedule {
	t.Helper()
	c, err := ParseSchedule(Schedule{Cron: cron, Timezone: tz})
	if err != nil {
		t.Fatalf("ParseSchedule(%q, %q): %v", cron, tz, err)
	}
	return c
}

func scheduleUTC(y int, m time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, m, d, h, mi, s, 0, time.UTC)
}

// scheduleCronCase is one Next / Prev expectation: from `at`, Next must
// return `next` and Prev must return `prev` (instants compared with Equal; a
// zero expectation is not asserted). The instants are FIXED, the same ones
// the backend's schedule_test.go asserts, so both modules' evaluators are held
// to one calendar rather than to each other.
type scheduleCronCase struct {
	name, cron, tz string
	at, next, prev time.Time
}

func TestCronSchedule_NextPrev_SharedTable(t *testing.T) {
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	local := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, chi) }
	u := scheduleUTC
	cases := []scheduleCronCase{
		{"every 15 minutes", "*/15 * * * *", "", u(2026, 10, 2, 10, 7, 0), u(2026, 10, 2, 10, 15, 0), u(2026, 10, 2, 10, 0, 0)},
		{"exact fire instant: Next strictly after, Prev returns it", "*/15 * * * *", "", u(2026, 10, 2, 10, 15, 0), u(2026, 10, 2, 10, 30, 0), u(2026, 10, 2, 10, 15, 0)},
		{"sub-minute offset truncates", "*/15 * * * *", "", u(2026, 10, 2, 10, 15, 30), u(2026, 10, 2, 10, 30, 0), u(2026, 10, 2, 10, 15, 0)},
		{"daily across midnight and month end", "0 9 * * *", "", u(2026, 10, 31, 10, 0, 0), u(2026, 11, 1, 9, 0, 0), u(2026, 10, 31, 9, 0, 0)},
		{"weekdays only (2026-10-03 is a Saturday)", "0 9 * * 1-5", "", u(2026, 10, 3, 0, 0, 0), u(2026, 10, 5, 9, 0, 0), u(2026, 10, 2, 9, 0, 0)},
		{"range with step", "0 8-18/5 * * *", "", u(2026, 10, 2, 13, 30, 0), u(2026, 10, 2, 18, 0, 0), u(2026, 10, 2, 13, 0, 0)},
		{"month restriction rolls the year", "0 0 1 3 *", "", u(2026, 10, 2, 0, 0, 0), u(2027, 3, 1, 0, 0, 0), u(2026, 3, 1, 0, 0, 0)},
		{"Feb 29 (leap day)", "0 0 29 2 *", "", u(2026, 3, 1, 0, 0, 0), u(2028, 2, 29, 0, 0, 0), u(2024, 2, 29, 0, 0, 0)},
		// Vixie OR: the 13th OR any Friday (2026-10-01 is a Thursday).
		{"vixie dom-or-dow: Friday arm", "0 0 13 * 5", "", u(2026, 10, 1, 0, 0, 0), u(2026, 10, 2, 0, 0, 0), u(2026, 9, 25, 0, 0, 0)},
		{"vixie dom-or-dow: day-of-month arm", "0 0 13 * 5", "", u(2026, 10, 10, 0, 0, 0), u(2026, 10, 13, 0, 0, 0), u(2026, 10, 9, 0, 0, 0)},
		{"star-led day field keeps AND", "0 0 */13 * 5", "", u(2026, 10, 1, 0, 0, 0), u(2026, 11, 27, 0, 0, 0), time.Time{}},
		{"day-of-week 7 is Sunday (2026-10-04)", "0 0 * * 7", "", u(2026, 10, 2, 0, 0, 0), u(2026, 10, 4, 0, 0, 0), u(2026, 9, 27, 0, 0, 0)},
		// America/Chicago 2026: spring forward 03-08 02:00 CST -> 03:00 CDT;
		// fall back 11-01 02:00 CDT -> 01:00 CST.
		{"daily 09:00 into spring-forward day", "0 9 * * *", "America/Chicago", u(2026, 3, 7, 16, 0, 0), u(2026, 3, 8, 14, 0, 0), u(2026, 3, 7, 15, 0, 0)},
		{"daily 09:00 into fall-back day", "0 9 * * *", "America/Chicago", u(2026, 10, 31, 15, 0, 0), u(2026, 11, 1, 15, 0, 0), u(2026, 10, 31, 14, 0, 0)},
		{"spring-forward skipped time never fires", "30 2 * * *", "America/Chicago", local(2026, 3, 7, 3, 0), local(2026, 3, 9, 2, 30), local(2026, 3, 7, 2, 30)},
		{"fall-back: first 01:30 (CDT)", "30 1 * * *", "America/Chicago", u(2026, 11, 1, 5, 0, 0), u(2026, 11, 1, 6, 30, 0), u(2026, 10, 31, 6, 30, 0)},
		{"fall-back: second 01:30 (CST) is a distinct window", "30 1 * * *", "America/Chicago", u(2026, 11, 1, 6, 30, 0), u(2026, 11, 1, 7, 30, 0), u(2026, 11, 1, 6, 30, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := scheduleMust(t, tc.cron, tc.tz)
			if got := c.Next(tc.at); !got.Equal(tc.next) {
				t.Errorf("Next(%s) = %s, want %s", tc.at.UTC(), got.UTC(), tc.next.UTC())
			}
			if !tc.prev.IsZero() {
				if got := c.Prev(tc.at); !got.Equal(tc.prev) {
					t.Errorf("Prev(%s) = %s, want %s", tc.at.UTC(), got.UTC(), tc.prev.UTC())
				}
			}
		})
	}
}

// TestCronSchedule_SkippedLocalMidnight drives Next into a DST transition
// that skips local MIDNIGHT (America/Havana, 2026-03-08), which the embedded
// time/tzdata must resolve on any host.
func TestCronSchedule_SkippedLocalMidnight(t *testing.T) {
	hav, err := time.LoadLocation("America/Havana")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	c := scheduleMust(t, "0 12 8 * *", "America/Havana")
	if got, want := c.Next(time.Date(2026, 3, 7, 13, 0, 0, 0, hav)), time.Date(2026, 3, 8, 12, 0, 0, 0, hav); !got.Equal(want) {
		t.Errorf("Next = %s, want %s", got, want)
	}
}
