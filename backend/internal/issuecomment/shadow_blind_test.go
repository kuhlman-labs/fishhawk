package issuecomment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// shadow_blind_test.go pins the issue-thread half of the E82.1 / #3778
// blindness contract (ADR-085 rule 4): the record-only
// `delegation_shadow_evaluated` stamp is never an activity category, so
// neither the living anchor nor the status comment ever renders it to the
// deciding human. The stamp is the NEWEST entry and carries a unique marker,
// and an ordinary activity row proves the timeline itself is rendered, so the
// stamp's absence is not vacuous.

const (
	shadowCategory = "delegation_shadow_evaluated"
	shadowMarker   = "SHADOW-MARKER-3778"
)

// shadowBlindEntries is an ordinary rendered activity row plus a newer
// marker-bearing stamp.
func shadowBlindEntries(t *testing.T) []*audit.Entry {
	t.Helper()
	stamp, err := json.Marshal(map[string]any{
		"shadow_version": 1, "action": "approve", "verdict": "unmet",
		"reason": shadowMarker + ": 1 of 2 approve verdicts", "human_decision": "approve",
	})
	if err != nil {
		t.Fatalf("marshal stamp: %v", err)
	}
	system := audit.ActorKind("system")
	return []*audit.Entry{
		{Sequence: 5, Category: "run_dispatched", Timestamp: time.Unix(5, 0).UTC()},
		{Sequence: 9, Category: shadowCategory, ActorKind: &system, Payload: stamp, Timestamp: time.Unix(9, 0).UTC()},
	}
}

func assertShadowBlind(t *testing.T, surface, body string) {
	t.Helper()
	if !strings.Contains(body, "Fishhawk run dispatched") {
		t.Fatalf("%s did not render the ordinary activity row; the timeline must be exercised:\n%s", surface, body)
	}
	if strings.Contains(body, shadowCategory) {
		t.Errorf("%s renders the %s category; the shadow stamp is blind:\n%s", surface, shadowCategory, body)
	}
	if strings.Contains(body, shadowMarker) {
		t.Errorf("%s renders the stamp marker %q; the shadow stamp is blind:\n%s", surface, shadowMarker, body)
	}
}

// TestShadowStampIsNotAnActivityCategory pins the allow-list itself: the
// stamp must never be added to activityCategories.
func TestShadowStampIsNotAnActivityCategory(t *testing.T) {
	if RendersActivity(shadowCategory) {
		t.Fatalf("RendersActivity(%q) = true; the delegation shadow stamp is record-only and blind (ADR-085 rule 4) and must never be an issue-comment activity category", shadowCategory)
	}
}

// TestShadowStampNeverRenderedOnAnchorOrStatus renders both issue-thread
// surfaces over entries carrying the stamp and asserts neither carries the
// category or the marker.
func TestShadowStampNeverRenderedOnAnchorOrStatus(t *testing.T) {
	entries := shadowBlindEntries(t)
	stages := []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}}
	now := time.Unix(1000, 0).UTC()

	anchor := RenderAnchorBody(AnchorInput{
		Run:         anchorRun(),
		Stages:      stages,
		Audit:       entries,
		ExternalURL: "https://app.example",
		Now:         now,
	})
	assertShadowBlind(t, "anchor", anchor)

	status := RenderStatusBody(anchorRun(), stages, entries, "https://app.example", now)
	assertShadowBlind(t, "status comment", status)
}
