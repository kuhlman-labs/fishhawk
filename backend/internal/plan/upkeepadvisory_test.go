package plan_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// upkeepadvisory_test.go is the hermetic form of #3750 acceptance criterion 1:
// REAL `govulncheck -json` streams (captured, see testdata/README.md) are fed
// through the upkeep-report advisory rules. Each finding's trace goes in
// UNCHANGED — strictly decoded into plan.UpkeepAdvisoryFrame AND spliced as
// raw JSON into a report the schema validates — so the schema frame is proved
// to be govulncheck's frame shape, and an unexpected govulncheck key is caught
// rather than silently dropped. The deepest finding level per OSV id then
// decides reachability, and the severity cap is checked on it: a called
// vulnerable symbol may be high, an imported-but-never-called package may not.

// govulncheckMessage is one message of a `govulncheck -json` stream. Decoded
// with DisallowUnknownFields: a message type or finding key this test does not
// know fails the decode instead of being skipped.
type govulncheckMessage struct {
	Config   json.RawMessage     `json:"config"`
	SBOM     json.RawMessage     `json:"SBOM"`
	Progress json.RawMessage     `json:"progress"`
	OSV      json.RawMessage     `json:"osv"`
	Finding  *govulncheckFinding `json:"finding"`
}

type govulncheckFinding struct {
	OSV          string            `json:"osv"`
	FixedVersion string            `json:"fixed_version"`
	Trace        []json.RawMessage `json:"trace"`
}

// govulncheckDeepest holds the deepest finding seen for one OSV id.
type govulncheckDeepest struct {
	fixed        string
	reachability string
	frames       []plan.UpkeepAdvisoryFrame
	raw          []json.RawMessage
}

var upkeepReachabilityRank = map[string]int{
	plan.UpkeepReachabilityRequired: 1,
	plan.UpkeepReachabilityImported: 2,
	plan.UpkeepReachabilityCalled:   3,
}

// decodeGovulncheckStream reads a captured stream and returns, per OSV id, the
// deepest finding: its frames decoded strictly, its raw frames verbatim and the
// reachability UpkeepAdvisoryReachability derives from them.
func decodeGovulncheckStream(t *testing.T, path string) map[string]govulncheckDeepest {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	out := map[string]govulncheckDeepest{}
	sawConfig := false
	for {
		var msg govulncheckMessage
		if err := dec.Decode(&msg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("%s: decode govulncheck message: %v", path, err)
		}
		sawConfig = sawConfig || msg.Config != nil
		if msg.Finding == nil {
			continue
		}
		frames := make([]plan.UpkeepAdvisoryFrame, len(msg.Finding.Trace))
		for k, rf := range msg.Finding.Trace {
			if err := upkeepStrictFrame(rf, &frames[k]); err != nil {
				t.Fatalf("%s: %s trace frame %d does not decode strictly into plan.UpkeepAdvisoryFrame: %v", path, msg.Finding.OSV, k, err)
			}
		}
		level := plan.UpkeepAdvisoryReachability(frames)
		if prev, ok := out[msg.Finding.OSV]; ok && upkeepReachabilityRank[prev.reachability] >= upkeepReachabilityRank[level] {
			continue
		}
		out[msg.Finding.OSV] = govulncheckDeepest{fixed: msg.Finding.FixedVersion, reachability: level, frames: frames, raw: msg.Finding.Trace}
	}
	if !sawConfig {
		t.Fatalf("%s carries no config message; not a govulncheck -json stream", path)
	}
	return out
}

func upkeepStrictFrame(raw json.RawMessage, f *plan.UpkeepAdvisoryFrame) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(f)
}

// upkeepReportFromGovulncheck builds an upkeep report holding one advisory
// finding for osv, with the captured trace spliced in as RAW JSON (so the
// schema sees govulncheck's bytes, not a Go re-encoding) and the given
// severity. The package is the vulnerable MODULE, call_path[0].module.
func upkeepReportFromGovulncheck(t *testing.T, osv string, d govulncheckDeepest, severity string) []byte {
	t.Helper()
	module := d.frames[0].Module
	subject := osv + ":" + module
	var fixed any
	if d.fixed != "" {
		fixed = d.fixed
	}
	report := map[string]any{
		"kind":             plan.KindUpkeepReport,
		"report_version":   plan.UpkeepReportVersion,
		"ticket_reference": map[string]any{"type": "github_issue", "url": "https://github.com/kuhlman-labs/fishhawk/issues/3750", "id": "kuhlman-labs/fishhawk#3750"},
		"generated_by":     map[string]any{"agent": "claude-code", "model": "claude-opus-5", "timestamp": "2026-10-05T09:00:00Z"},
		"summary":          "govulncheck fixture scan",
		"sources_scanned":  []string{plan.UpkeepSourceAdvisory},
		"findings": []any{map[string]any{
			"id":       plan.UpkeepFindingID(plan.UpkeepSourceAdvisory, subject),
			"source":   plan.UpkeepSourceAdvisory,
			"subject":  subject,
			"evidence": []any{map[string]any{"kind": "file", "path": "go.mod", "line": 5, "value": module + " " + d.frames[0].Version}},
			"advisory": map[string]any{
				"ecosystem":     plan.UpkeepEcosystemGo,
				"package":       module,
				"version":       d.frames[0].Version,
				"advisory_ids":  []string{osv},
				"fixed_version": fixed,
				"scanner":       plan.UpkeepScannerGovulncheck,
				"reachability":  d.reachability,
				"call_path":     d.raw,
				"severity":      severity,
			},
			"proposed_issue": map[string]any{"title": "Bump " + module, "body": osv + " in " + module, "type": "bug", "labels": []string{}},
		}},
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	return b
}

const upkeepFixtureOSV = "GO-2022-1059"

func TestGovulncheckSymbolFixture_CalledMayBeHigh(t *testing.T) {
	deepest := decodeGovulncheckStream(t, "testdata/govulncheck-symbol.json")
	d, ok := deepest[upkeepFixtureOSV]
	if !ok {
		t.Fatalf("symbol fixture carries no %s finding", upkeepFixtureOSV)
	}
	if d.reachability != plan.UpkeepReachabilityCalled {
		t.Fatalf("deepest %s level = %q, want %q (the fixture calls language.ParseAcceptLanguage)", upkeepFixtureOSV, d.reachability, plan.UpkeepReachabilityCalled)
	}
	// govulncheck orders a trace from the vulnerable symbol to the entry
	// point: the rules read call_path[0] as the vulnerable end.
	if len(d.frames) != 2 || d.frames[0].Function != "ParseAcceptLanguage" || d.frames[1].Function != "main" {
		t.Fatalf("trace = %+v, want [ParseAcceptLanguage, main] (vulnerable end first)", d.frames)
	}
	if d.frames[0].Position == nil || d.frames[0].Position.Line != 158 || d.frames[0].Position.Filename != "language/parse.go" {
		t.Errorf("frame 0 position = %+v, want language/parse.go:158", d.frames[0].Position)
	}
	body := upkeepReportFromGovulncheck(t, upkeepFixtureOSV, d, plan.UpkeepSeverityHigh)
	r, err := plan.ParseUpkeepReport(body)
	if err != nil {
		t.Fatalf("ParseUpkeepReport(called, high) = %v, want accepted", err)
	}
	if got := r.Findings[0].Advisory.CallPath; len(got) != 2 || got[0].Function != "ParseAcceptLanguage" {
		t.Errorf("parsed call_path = %+v, want the captured two-frame trace", got)
	}
	if fv := r.Findings[0].Advisory.FixedVersion; fv == nil || *fv != "v0.3.8" {
		t.Errorf("parsed fixed_version = %v, want v0.3.8 from the captured finding", fv)
	}
}

func TestGovulncheckPackageFixture_ImportedCappedAtLow(t *testing.T) {
	deepest := decodeGovulncheckStream(t, "testdata/govulncheck-package.json")
	d, ok := deepest[upkeepFixtureOSV]
	if !ok {
		t.Fatalf("package fixture carries no %s finding", upkeepFixtureOSV)
	}
	if d.reachability != plan.UpkeepReachabilityImported {
		t.Fatalf("deepest %s level = %q, want %q (the fixture imports golang.org/x/text/language but calls no vulnerable symbol)", upkeepFixtureOSV, d.reachability, plan.UpkeepReachabilityImported)
	}
	err := plan.ValidateUpkeepReport(upkeepReportFromGovulncheck(t, upkeepFixtureOSV, d, plan.UpkeepSeverityHigh))
	var se *plan.SemanticError
	if !errors.As(err, &se) || !strings.HasPrefix(se.Message, "/findings/0/advisory/severity:") {
		t.Fatalf("ValidateUpkeepReport(imported, high) = %v, want a *SemanticError at /findings/0/advisory/severity", err)
	}
	if err := plan.ValidateUpkeepReport(upkeepReportFromGovulncheck(t, upkeepFixtureOSV, d, plan.UpkeepSeverityLow)); err != nil {
		t.Fatalf("ValidateUpkeepReport(imported, low) = %v, want accepted", err)
	}
}

// TestGovulncheckFixtures_ModuleOnlyIsRequired: GO-2026-5970 reaches neither
// fixture past the module level, so it derives `required`, capped at low.
func TestGovulncheckFixtures_ModuleOnlyIsRequired(t *testing.T) {
	for _, path := range []string{"testdata/govulncheck-symbol.json", "testdata/govulncheck-package.json"} {
		d, ok := decodeGovulncheckStream(t, path)["GO-2026-5970"]
		if !ok {
			t.Fatalf("%s carries no GO-2026-5970 finding", path)
		}
		if d.reachability != plan.UpkeepReachabilityRequired {
			t.Errorf("%s: GO-2026-5970 level = %q, want %q", path, d.reachability, plan.UpkeepReachabilityRequired)
		}
		if err := plan.ValidateUpkeepReport(upkeepReportFromGovulncheck(t, "GO-2026-5970", d, plan.UpkeepSeverityLow)); err != nil {
			t.Errorf("%s: ValidateUpkeepReport(required, low) = %v, want accepted", path, err)
		}
	}
}

// TestGovulncheckFrame_UnknownKeyCaught proves the two frame guards the
// fixtures rely on are live: a captured frame carrying one extra key fails the
// strict decode AND is refused by the schema.
func TestGovulncheckFrame_UnknownKeyCaught(t *testing.T) {
	d := decodeGovulncheckStream(t, "testdata/govulncheck-symbol.json")[upkeepFixtureOSV]
	var m map[string]any
	if err := json.Unmarshal(d.raw[0], &m); err != nil {
		t.Fatal(err)
	}
	m["inlined"] = true
	tampered, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var f plan.UpkeepAdvisoryFrame
	if err := upkeepStrictFrame(tampered, &f); err == nil {
		t.Error("strict frame decode accepted an unknown key")
	}
	d.raw = append([]json.RawMessage{tampered}, d.raw[1:]...)
	var se *plan.SchemaError
	if err := plan.ValidateUpkeepReport(upkeepReportFromGovulncheck(t, upkeepFixtureOSV, d, plan.UpkeepSeverityHigh)); !errors.As(err, &se) || !upkeepSchemaErrorAt(se, "/findings/0/advisory/call_path/0") {
		t.Errorf("ValidateUpkeepReport(tampered frame) = %v, want a *SchemaError at /findings/0/advisory/call_path/0", err)
	}
}
