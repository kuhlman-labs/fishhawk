package server

// Cross-boundary flow test for the upkeep scan's advisory source (#3750):
//
//	POST /v0/runs/{id}/plan (signed, the shipped advisory example)
//	  → ParseUpkeepReport (schema + semantic rules (l)-(t))
//	  → upkeep_report ingest → upkeepCoverage through the listing seam serving
//	    ONE dependabot[bot] pull request bumping golang.org/x/net to 0.23.0 in
//	    /backend → upkeep_report_recorded {covered:[GO-2024-2687]}
//	POST /v0/runs/{id}/upkeep-dispositions (covered=approved, uncovered
//	  GO-2022-1059=approved, uncovered GHSA=rejected)
//	POST /v0/stages/{id}/approvals (approve) → applyApprovedUpkeep
//	  → covered_by_dependabot_pr skip, ONE server-rendered filing, not_approved
//
// It is built on upkeep_flow_test.go's fixture constructor (newUkFlowFixture,
// its CreateRun counter and fake filer) with its OWN ingest and dispose steps:
// that file's helpers are hard-wired to the six-finding report, so it is not
// edited. Counterfactuals:
//   - delete the `case isCovered:` skip in upkeep_apply.go → GO-2024-2687
//     (approved, not a duplicate) is filed too → two filer requests → red.
//   - record a constant [] for `covered` in upkeep_report.go → no skip → red.
//   - file the agent title/body for an advisory finding (upkeepFilingProse)
//     → the planted caller-frame quotes reach the filer → red.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

func TestUpkeepAdvisoryFlow_CoveredSkippedUncoveredFiledServerRendered(t *testing.T) {
	var listings atomic.Int32
	swapUpkeepListOpenPulls(t, ukPullsSeam(&listings, ukDependabotPull(3823, "golang.org/x/net", "0.22.0", "0.23.0", "/backend")))

	// The uncovered approved finding's AGENT prose quotes the call path the
	// report carries for GO-2024-2687 — the repository's own function,
	// qualified name and source file — so the "agent prose never reaches the
	// tracker" assertion below is not vacuous.
	const (
		agentTitle = "Bump golang.org/x/text; see serveH2 in internal/server/serve.go"
		agentBody  = "Reached via github.com/kuhlman-labs/fishhawk/backend/internal/server.serveH2 (internal/server/serve.go:88)."
	)
	body := ukAdvisoryBody(t, map[string]func(map[string]any){ukAdvText: func(issue map[string]any) {
		issue["title"] = agentTitle
		issue["body"] = agentBody
	}})
	report, err := plan.ParseUpkeepReport(body)
	if err != nil {
		t.Fatal(err)
	}

	f := newUkFlowFixture(t)

	// INGEST through the signed route.
	w := shipPlanRequest(t, f.s, f.runRow.ID, f.planStage.ID, f.priv, body, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("ingest status = %d, want 201: %s", w.Code, w.Body.String())
	}
	recorded := f.payloads(t, CategoryUpkeepReportRecorded)
	if len(recorded) != 1 {
		t.Fatalf("upkeep_report_recorded rows = %d, want 1", len(recorded))
	}
	var rec struct {
		Covered     []upkeep.Covered `json:"covered"`
		EntryCounts map[string]int   `json:"entry_counts"`
	}
	if err := json.Unmarshal(recorded[0], &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Covered) != 1 || rec.Covered[0].FindingID != ukAdvNet || rec.Covered[0].Pulls[0].Number != 3823 {
		t.Fatalf("recorded covered = %+v, want exactly %s by #3823", rec.Covered, ukAdvNet)
	}
	if rec.EntryCounts["advisory"] != 3 {
		t.Errorf("entry_counts = %v, want advisory 3", rec.EntryCounts)
	}
	if n := listings.Load(); n != 1 {
		t.Errorf("open-pull listings = %d, want 1", n)
	}

	// DISPOSE through the real capture route.
	w = postUK(t, f.s, f.runRow.ID.String(), fmt.Sprintf(`{"dispositions":[
		{"finding_id":%q,"verdict":"approved"},
		{"finding_id":%q,"verdict":"approved"},
		{"finding_id":%q,"verdict":"rejected"}]}`, ukAdvNet, ukAdvText, ukAdvGHSA), ukOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("disposition capture status = %d, want 200: %s", w.Code, w.Body.String())
	}

	// APPROVE through the real approvals route; the apply runs detached.
	f.decide(t, `{"decision":"approve","comment":"file the approved advisories"}`)

	reqs := f.provider.filer.requests()
	if len(reqs) != 1 {
		t.Fatalf("filer requests = %d, want exactly 1 (only the uncovered approved advisory)", len(reqs))
	}
	title, filed := reqs[0].Item.Title, reqs[0].Item.Body
	if want := "GO-2022-1059: golang.org/x/text v0.3.7 (low severity)"; title != want {
		t.Errorf("filed title = %q, want the server-rendered %q", title, want)
	}
	for _, want := range []string{upkeep.FindingMarker(ukAdvText), "### Advisory facts (server-rendered)",
		"- Advisory IDs: `GO-2022-1059`, `CVE-2022-32149`, `GHSA-69ch-w2m2-3vjp`",
		"- Fixed version: `v0.3.8`", "- Manifests: `runner/go.mod`"} {
		if !strings.Contains(filed, want) {
			t.Errorf("filed body lacks %q:\n%s", want, filed)
		}
	}
	// No call_path frame of ANY finding — function, qualified name or
	// filename — and none of the agent's prose reaches the tracker.
	leaks := []string{agentTitle, agentBody}
	for _, fd := range report.Findings {
		if fd.Advisory == nil {
			continue
		}
		for i, fr := range fd.Advisory.CallPath {
			if i == 0 {
				continue // the vulnerable dependency symbol, public in the advisory
			}
			if fr.Function != "" {
				leaks = append(leaks, fr.Function, fr.Package+"."+fr.Function)
			}
			if fr.Position != nil && fr.Position.Filename != "" {
				leaks = append(leaks, fr.Position.Filename)
			}
		}
	}
	if len(leaks) < 5 {
		t.Fatalf("leak probes = %v, want the example's caller frame to contribute (fixture drift)", leaks)
	}
	for _, leak := range leaks {
		if strings.Contains(title, leak) || strings.Contains(filed, leak) {
			t.Errorf("filing quotes %q:\ntitle: %s\nbody:\n%s", leak, title, filed)
		}
	}

	skips := map[string]upkeepFindingSkippedPayload{}
	for _, raw := range f.payloads(t, CategoryUpkeepFindingSkipped) {
		var p upkeepFindingSkippedPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		skips[p.FindingID] = p
	}
	if s := skips[ukAdvNet]; s.SkipReason != upkeepSkipCovered || len(s.CoveringPRNumbers) != 1 || s.CoveringPRNumbers[0] != 3823 {
		t.Errorf("covered skip = %+v, want covered_by_dependabot_pr by #3823", s)
	}
	if s := skips[ukAdvGHSA]; s.SkipReason != upkeepSkipNotApproved {
		t.Errorf("rejected skip = %+v, want not_approved", s)
	}
	if len(skips) != 2 {
		t.Errorf("skipped findings = %v, want exactly the covered and the rejected", ukSortedKeys(skips))
	}
	var filedRow upkeepFindingFiledPayload
	if rows := f.payloads(t, CategoryUpkeepFindingFiled); len(rows) != 1 {
		t.Fatalf("upkeep_finding_filed rows = %d, want 1", len(rows))
	} else if err := json.Unmarshal(rows[0], &filedRow); err != nil {
		t.Fatal(err)
	}
	if filedRow.FindingID != ukAdvText || !filedRow.ServerRendered {
		t.Errorf("filed row = %+v, want %s server_rendered", filedRow, ukAdvText)
	}
	var sum upkeepApplyCompletedPayload
	if rows := f.payloads(t, CategoryUpkeepApplyCompleted); len(rows) != 1 {
		t.Fatalf("upkeep_apply_completed rows = %d, want 1", len(rows))
	} else if err := json.Unmarshal(rows[0], &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Degraded || sum.Findings != 3 || sum.Filed != 1 || sum.Skipped != 2 {
		t.Errorf("upkeep_apply_completed = %+v, want {findings:3 filed:1 skipped:2}", sum)
	}
	if n := f.rr.creates.Load(); n != 0 {
		t.Errorf("CreateRun calls = %d, want 0 (the upkeep apply never creates a run)", n)
	}
}
