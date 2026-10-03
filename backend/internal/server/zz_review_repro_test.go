package server

// Review repros for the E80.4 permission-drift server half (PR #3934). Each
// test asserts the CORRECT behaviour: a FAIL confirms the reviewer's finding.

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/permdrift"
)

const reproHead2 = "head2222"

const reproMCPScopesPath = "backend/internal/server/mcpscopes.go"

// reproShapeLostMCPScopes returns the real mcpscopes.go and a head that ADDS a
// tool whose anyOf is built by a call: a real admission widening the Go
// extractor cannot resolve, so Detect reports shape_unrecognized.
func reproShapeLostMCPScopes(t *testing.T) (base, head string) {
	t.Helper()
	b, err := os.ReadFile("mcpscopes.go")
	if err != nil {
		t.Fatal(err)
	}
	base = string(b)
	anchor := "\"fishhawk_get_active_run\":      mcpScopeAuthenticatedOnly,\n"
	if !strings.Contains(base, anchor) {
		t.Fatalf("anchor line not found in mcpscopes.go")
	}
	head = strings.Replace(base, anchor, anchor+"\t\"fishhawk_zz_evil\": {anyOf: evilScopes()},\n", 1)
	return base, head
}

func reproRowsWithPrefix(rows []*concern.Concern, prefix string) []*concern.Concern {
	var out []*concern.Concern
	for _, r := range rows {
		if strings.HasPrefix(r.CheckKey, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// F2 control: with no prior waive, the shape-lost head DOES raise
// shape_unrecognized (proves the fixture reaches the fail-closed branch).
func TestReviewRepro_F2_Control_ShapeUnrecognizedRaises(t *testing.T) {
	f := newDriftFixture(t)
	base, head := reproShapeLostMCPScopes(t)
	f.gh.put(driftBase, reproMCPScopesPath, base)
	f.gh.put(driftHead, reproMCPScopesPath, head)
	f.gh.changed = []string{reproMCPScopesPath}
	f.run(f.req(permissionDriftTriggerPROpened))
	if got := f.unevaluable(t)["mcp-tool-scopes"]; got != permdrift.ReasonShapeUnrecognized {
		t.Fatalf("unevaluable[mcp-tool-scopes] = %q, want shape_unrecognized", got)
	}
}

// F2: a human waives a TRANSIENT fetch_failed (forge 5xx) on mcpscopes.go;
// a later fix-up pass makes the file shape_unrecognized while adding a real
// admission. The later fail-closed result must still raise.
func TestReviewRepro_F2_WaivedFetchFailedMutesLaterShapeUnrecognized(t *testing.T) {
	f := newDriftFixture(t)
	base, head := reproShapeLostMCPScopes(t)

	// Check 1 (PR opened): head fetch 500 → fetch_failed.
	f.gh.put(driftBase, reproMCPScopesPath, base)
	f.gh.changed = []string{reproMCPScopesPath}
	f.gh.contentStatus[driftHead+"|"+reproMCPScopesPath] = http.StatusBadGateway
	f.run(f.req(permissionDriftTriggerPROpened))
	rows := f.rows(t)
	if len(rows) != 1 || f.unevaluable(t)["mcp-tool-scopes"] != permdrift.ReasonFetchFailed {
		t.Fatalf("setup: rows = %d unevaluable = %v, want one fetch_failed", len(rows), f.unevaluable(t))
	}
	// Human waives it as a transient forge outage.
	f.cr.mu.Lock()
	rows[0].State = concern.StateWaived
	f.cr.mu.Unlock()

	// Check 2 (fix-up pushed): previous head = base content, new head adds a
	// call-built anyOf → shape_unrecognized hiding a real widening.
	f.gh.mu.Lock()
	delete(f.gh.contentStatus, driftHead+"|"+reproMCPScopesPath)
	f.gh.mu.Unlock()
	f.gh.put(driftHead, reproMCPScopesPath, base)
	f.gh.put(reproHead2, reproMCPScopesPath, head)
	f.run(permissionDriftRequest{RunID: f.runRow.ID, StageID: f.stage.ID, Base: driftHead, Head: reproHead2, Trigger: permissionDriftTriggerFixupPushed})

	var sawShape bool
	for _, p := range f.detected(t) {
		for _, u := range p.Unevaluable {
			sawShape = sawShape || (u.Surface == "mcp-tool-scopes" && u.Reason == permdrift.ReasonShapeUnrecognized)
		}
	}
	open := 0
	for _, r := range reproRowsWithPrefix(f.rows(t), "permission_drift|mcp-tool-scopes|") {
		if r.State.IsOpen() {
			open++
		}
	}
	if !sawShape || open == 0 {
		t.Fatalf("after waiving a transient fetch_failed, a later shape_unrecognized on the same file was suppressed: "+
			"detected shape_unrecognized = %v, open mcp-tool-scopes rows = %d (rows total %d)", sawShape, open, len(f.rows(t)))
	}
}

// F2 (compare_truncated arm): one waived compare_truncated row for the glob
// Actions surface mutes every later truncated check on the stage, including
// one whose hidden (unlisted) workflow file widens.
func TestReviewRepro_F2_WaivedCompareTruncatedMutesLaterTruncation(t *testing.T) {
	f := newDriftFixture(t)
	f.gh.truncated = true
	f.gh.changed = nil
	f.run(f.req(permissionDriftTriggerPROpened))
	rows := reproRowsWithPrefix(f.rows(t), "permission_drift|gha-workflow-permissions|")
	if len(rows) != 1 || f.unevaluable(t)["gha-workflow-permissions"] != permdrift.ReasonCompareTruncated {
		t.Fatalf("setup: gha rows = %d unevaluable = %v, want one compare_truncated", len(rows), f.unevaluable(t))
	}
	f.cr.mu.Lock()
	rows[0].State = concern.StateWaived
	f.cr.mu.Unlock()

	// Fix-up pass: still truncated; ci.yml widens but is not in the listing.
	f.gh.put(driftHead, driftWFPath, wfBase)
	f.gh.put(reproHead2, driftWFPath, wfWidened)
	f.run(permissionDriftRequest{RunID: f.runRow.ID, StageID: f.stage.ID, Base: driftHead, Head: reproHead2, Trigger: permissionDriftTriggerFixupPushed})

	open := 0
	for _, r := range reproRowsWithPrefix(f.rows(t), "permission_drift|gha-workflow-permissions|") {
		if r.State.IsOpen() {
			open++
		}
	}
	if open == 0 {
		t.Fatalf("second truncated check (hidden ci.yml widening) raised nothing: detected entries = %d", len(f.detected(t)))
	}
}

// F3: the extension is read at req.Base, which for a fix-up is the PREVIOUS
// RUN-BRANCH HEAD (agent-authored). The implement pass removed the
// infra-app declaration; the fix-up pass widens infra/app.json. Per the README
// contract (read at the run's BASE commit) the widening must raise.
func TestReviewRepro_F3_FixupReadsExtensionAtAgentAuthoredHead(t *testing.T) {
	f := newDriftFixture(t)
	ext := "version: 1\nsurfaces:\n  - id: infra-app\n    kind: github_app_permissions_json\n    paths: [\"infra/app.json\"]\n"
	empty := "version: 1\nsurfaces: []\n"
	// Run base: declares infra-app.
	f.gh.put(driftBase, permdrift.RepoSurfacesPath, ext)
	f.gh.put(driftBase, "infra/app.json", `{"default_permissions":{"contents":"read"}}`)
	// Implement pass (driftHead): removed the declaration (flagged once at PR open).
	f.gh.put(driftHead, permdrift.RepoSurfacesPath, empty)
	f.gh.put(driftHead, "infra/app.json", `{"default_permissions":{"contents":"read"}}`)
	// Fix-up pass (head2222): widens infra/app.json.
	f.gh.put(reproHead2, permdrift.RepoSurfacesPath, empty)
	f.gh.put(reproHead2, "infra/app.json", `{"default_permissions":{"contents":"write"}}`)
	f.gh.changed = []string{"infra/app.json"}

	// This is exactly what succeedFixupPushStage builds: Base = pr.BaseSHA =
	// previous branch head.
	f.run(permissionDriftRequest{RunID: f.runRow.ID, StageID: f.stage.ID, Base: driftHead, Head: reproHead2, Trigger: permissionDriftTriggerFixupPushed})

	if got := reproRowsWithPrefix(f.rows(t), "permission_drift|infra-app|infra/app.json|"); len(got) == 0 {
		t.Fatalf("fix-up widening of a run-base-declared surface was not evaluated (extension read at the agent-authored previous head): rows = %d", len(f.rows(t)))
	}
}

// F8 control: base branch HAS the spec with both restrictions → the
// resolution's removal survives the intersection.
func TestReviewRepro_F8_Control_BaseBranchHasSpec(t *testing.T) {
	const mainRef = "main"
	f := newDriftFixture(t)
	f.gh.put(driftBase, driftSpecRef, driftSpec(`["secrets/**", "infra/**"]`))
	f.gh.put(driftHead, driftSpecRef, driftSpec(`["secrets/**"]`))
	f.gh.put(mainRef, driftSpecRef, driftSpec(`["secrets/**", "infra/**"]`))
	f.gh.changed = []string{driftSpecRef}
	f.seedConflictTrigger(t, mainRef)
	pr := &pullRequestBody{BaseSHA: driftBase, HeadSHA: driftHead}
	f.run(f.s.conflictResolutionDriftRequest(t.Context(), f.runRow.ID, f.stage.ID, pr))
	if got := reproRowsWithPrefix(f.rows(t), "permission_drift|"+permdrift.SurfaceIDForbiddenPaths+"|"); len(got) == 0 {
		t.Fatalf("control: rows = %d, want the forbidden_paths widening", len(f.rows(t)))
	}
}

// F8: the run ADDED .fishhawk/workflows.yaml (absent on the base branch → 404
// at IntersectRef). The conflict resolution then REMOVES a forbidden_paths
// restriction. The second comparison (absent → head) has no change for the
// removed key, so intersectDrift drops the widening instead of failing closed.
// (Same mechanism: the trigger's base branch deleted/renamed → 404 for every
// file at IntersectRef.)
func TestReviewRepro_F8_IntersectRef404DropsRestrictionRemoval(t *testing.T) {
	const mainRef = "main"
	f := newDriftFixture(t)
	f.gh.put(driftBase, driftSpecRef, driftSpec(`["secrets/**", "infra/**"]`))
	f.gh.put(driftHead, driftSpecRef, driftSpec(`["secrets/**"]`))
	// main: no spec at all → 404.
	f.gh.changed = []string{driftSpecRef}
	f.seedConflictTrigger(t, mainRef)
	pr := &pullRequestBody{BaseSHA: driftBase, HeadSHA: driftHead}
	f.run(f.s.conflictResolutionDriftRequest(t.Context(), f.runRow.ID, f.stage.ID, pr))
	if got := reproRowsWithPrefix(f.rows(t), "permission_drift|"+permdrift.SurfaceIDForbiddenPaths+"|"); len(got) == 0 {
		t.Fatalf("restriction removed by the conflict resolution was dropped by the intersection (IntersectRef side 404 = absent, not unevaluable): rows = %d, detected = %d",
			len(f.rows(t)), len(f.detected(t)))
	}
}

// F8 (deleted base branch): the trigger names a stacked base branch that was
// auto-deleted after it merged. The spec EXISTS at the run base and the
// previous head (both restrictions); the resolution drops "infra/**". GitHub
// answers /contents?ref=<deleted-branch> with 404, which the reader maps to an
// ABSENT side, so the second comparison is absent → head and the widening is
// intersected away. The README promises an unreadable base-branch side fails
// the pair closed; here nothing is raised at all.
func TestReviewRepro_F8_DeletedBaseBranch404DropsRestrictionRemoval(t *testing.T) {
	const stackedBase = "feature/stack-base" // deleted: no content at this ref
	f := newDriftFixture(t)
	f.gh.put(driftBase, driftSpecRef, driftSpec(`["secrets/**", "infra/**"]`))
	f.gh.put(driftHead, driftSpecRef, driftSpec(`["secrets/**"]`))
	f.gh.changed = []string{driftSpecRef}
	f.seedConflictTrigger(t, stackedBase)
	pr := &pullRequestBody{BaseSHA: driftBase, HeadSHA: driftHead}
	f.run(f.s.conflictResolutionDriftRequest(t.Context(), f.runRow.ID, f.stage.ID, pr))
	raisedOrUnevaluable := len(reproRowsWithPrefix(f.rows(t), "permission_drift|"+permdrift.SurfaceIDForbiddenPaths+"|")) > 0
	if !raisedOrUnevaluable {
		t.Fatalf("base branch unreadable (404 ref) — pair neither failed closed nor raised: rows = %d, detected = %d", len(f.rows(t)), len(f.detected(t)))
	}
}
