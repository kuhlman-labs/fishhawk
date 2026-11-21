package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Fix-up resume concern wire (#5390). The runner's resumed fix-up turn is the
// fetched prompt sent as the next turn of the implement agent's session
// (ADR-119, #5350), so the routed concerns reach a resumed agent ONLY if the
// served prompt carries them. This test is the BACKEND producer of the shared
// golden testdata/wire/fixup_resume_concerns_prompt.json: it routes two real
// concerns through the REAL fix-up handler, seeds the stage_fixup_triggered
// entry that handler wrote into the prompt resolver, and serves the signed
// implement prompt. The runner consumer
// (runner/cmd/fishhawk-runner/fixupresume_test.go,
// TestFixupResume_BackendConcernTextReachesResumedTurn) decodes the SAME file
// and asserts the resumed -p turn is that prompt byte for byte.

const fixupResumeWireUpdateCmd = "scripts/test single -run TestFixupResumeWire_ServedPromptMatchesGolden ./backend/internal/server/ -update"

// Fixed ids, so the served prompt is deterministic and the golden stable.
var (
	fixupWireRunID       = uuid.MustParse("5390a000-0000-4000-8000-000000000001")
	fixupWirePlanStageID = uuid.MustParse("5390a000-0000-4000-8000-000000000002")
	fixupWireImplStageID = uuid.MustParse("5390a000-0000-4000-8000-000000000003")
	fixupWireConcernA    = uuid.MustParse("5390c000-0000-4000-8000-00000000000a")
	fixupWireConcernB    = uuid.MustParse("5390c000-0000-4000-8000-00000000000b")
	fixupWireTriggerID   = uuid.MustParse("5390e000-0000-4000-8000-000000000001")
	fixupWireTimestamp   = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
)

// fixupWireConcerns are the two routed implement-review concerns: different
// severity and category, distinct notes.
var fixupWireConcerns = []struct {
	id                       uuid.UUID
	severity, category, note string
}{
	{fixupWireConcernA, "high", "correctness", "decideFixupResume must refuse a non-local runner channel before reading the ledger"},
	{fixupWireConcernB, "low", "docs", "name the fix-up resume opt-in in the runner README Tests paragraph"},
}

// fixupResumeWireGolden is the shared golden's shape. The runner decodes the
// same file into upload.FetchedPrompt (stage_type, fixup, prompt) plus these
// extra keys.
type fixupResumeWireGolden struct {
	StageType            string   `json:"stage_type"`
	Fixup                bool     `json:"fixup"`
	Prompt               string   `json:"prompt"`
	FixupConcernsSection string   `json:"fixup_concerns_section"`
	ConcernIDs           []string `json:"concern_ids"`
	Notes                []string `json:"notes"`
}

func fixupResumeWireGoldenPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "testdata", "wire", "fixup_resume_concerns_prompt.json")
}

// fixupConcernsSection extracts the "### Fix-up concerns" section from its
// heading up to the next "### " heading or the end of the prompt. Empty when
// the prompt carries no such heading.
func fixupConcernsSection(prompt string) string {
	const heading = "### Fix-up concerns"
	i := strings.Index(prompt, heading)
	if i < 0 {
		return ""
	}
	rest := prompt[i:]
	if j := strings.Index(rest[len(heading):], "\n### "); j >= 0 {
		return rest[:len(heading)+j+1]
	}
	return rest
}

// routeFixupWireTrigger routes both fixed concerns through the REAL fix-up
// handler and returns the stage_fixup_triggered entry it appended, unchanged
// apart from the entry identity the chain would stamp.
func routeFixupWireTrigger(t *testing.T) *audit.Entry {
	t.Helper()
	s, repo, au, cr := fixupServerWithConcerns(t)
	repo.seedRun(&run.Run{ID: fixupWireRunID, State: run.StateRunning})
	repo.mu.Lock()
	repo.stages[fixupWireImplStageID] = &run.Stage{
		ID: fixupWireImplStageID, RunID: fixupWireRunID, Sequence: 1,
		Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		State: run.StageStateAwaitingApproval, CreatedAt: fixupWireTimestamp, UpdatedAt: fixupWireTimestamp,
	}
	repo.mu.Unlock()
	ids := make([]string, 0, len(fixupWireConcerns))
	for i, c := range fixupWireConcerns {
		rows, err := cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
			RunID: fixupWireRunID, StageID: fixupWireImplStageID, StageKind: concern.StageKindImplement,
			ReviewerModel: "claude-opus-4-8", OriginReviewSequence: int64(101 + i),
			Concerns: []concern.RaisedConcern{{Severity: c.severity, Category: c.category, Note: c.note}},
		})
		if err != nil {
			t.Fatalf("seed concern: %v", err)
		}
		rows[0].ID = c.id // fixed id: the fake mints uuid.New()
		ids = append(ids, c.id.String())
	}

	w := postFixup(t, s, fixupWireImplStageID, fixupRequest{ConcernIDs: ids, Reason: "address both review concerns"})
	if w.Code != http.StatusOK {
		t.Fatalf("fix-up route status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var trigger *audit.Entry
	for _, p := range au.appended {
		if p.Category != CategoryStageFixupTriggered {
			continue
		}
		if trigger != nil {
			t.Fatal("the route appended more than one stage_fixup_triggered entry")
		}
		rid := p.RunID
		trigger = &audit.Entry{ID: fixupWireTriggerID, Sequence: 1, RunID: &rid, StageID: p.StageID,
			Timestamp: fixupWireTimestamp, Category: p.Category, ActorKind: p.ActorKind,
			ActorSubject: p.ActorSubject, Payload: p.Payload}
	}
	if trigger == nil {
		t.Fatal("the route appended no stage_fixup_triggered entry")
	}
	var payload struct {
		ConcernIDs []string `json:"concern_ids"`
	}
	if err := json.Unmarshal(trigger.Payload, &payload); err != nil {
		t.Fatalf("decode trigger payload: %v", err)
	}
	if strings.Join(payload.ConcernIDs, ",") != strings.Join(ids, ",") {
		t.Fatalf("trigger concern_ids = %v, want the routed %v", payload.ConcernIDs, ids)
	}
	return trigger
}

// serveFixupWirePrompt serves the signed implement prompt over a resolver
// seeded with trigger, built like
// TestGetStagePrompt_Implement_FixupConcerns_RetainsFullPlanScope.
func serveFixupWirePrompt(t *testing.T, trigger *audit.Entry) promptResponse {
	t.Helper()
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()
	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "make the fix-up resume decision refuse remote channels",
		Verification: plan.Verification{TestStrategy: "table rows per guard", RollbackPlan: "revert the PR"},
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "runner/cmd/fishhawk-runner/fixupresume.go", Operation: plan.FileOpModify},
			{Path: "runner/cmd/fishhawk-runner/fixupresume_test.go", Operation: plan.FileOpModify},
		}},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID: fixupWirePlanStageID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		fixupWireRunID: {
			{ID: fixupWirePlanStageID, RunID: fixupWireRunID, Type: run.StageTypePlan},
			{ID: fixupWireImplStageID, RunID: fixupWireRunID, Type: run.StageTypeImplement},
		},
	}
	rr.getRuns[fixupWireRunID] = &run.Run{ID: fixupWireRunID, Repo: "o/r", WorkflowID: "feature_change"}
	rr.getStages[fixupWireImplStageID] = &run.Stage{ID: fixupWireImplStageID, RunID: fixupWireRunID, Type: run.StageTypeImplement}

	priv, _ := sf.issue(t, fixupWireRunID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{fixupWireRunID: {trigger}}},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}
	w := promptRequest(t, s, fixupWireRunID, fixupWireImplStageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode prompt response: %v", err)
	}
	return resp
}

// TestFixupResumeWire_ServedPromptMatchesGolden is the cross-boundary
// producer: real fix-up route → persisted stage_fixup_triggered payload →
// prompt resolver → renderer → served signed response. The golden holds each
// concern under its durable id, so a dropped id pairing changes both the
// section and the prompt. Every golden read is a JSON-decoded string compare,
// never a raw-byte file compare. Regenerate with -update.
func TestFixupResumeWire_ServedPromptMatchesGolden(t *testing.T) {
	resp := serveFixupWirePrompt(t, routeFixupWireTrigger(t))
	if !resp.Fixup || resp.StageType != string(run.StageTypeImplement) {
		t.Fatalf("served fixup = %v stage_type = %q, want true / implement", resp.Fixup, resp.StageType)
	}
	section := fixupConcernsSection(resp.Prompt)
	if section == "" {
		t.Fatalf("served prompt has no ### Fix-up concerns section:\n%s", resp.Prompt)
	}
	ids := make([]string, 0, len(fixupWireConcerns))
	notes := make([]string, 0, len(fixupWireConcerns))
	for _, c := range fixupWireConcerns {
		ids = append(ids, c.id.String())
		notes = append(notes, c.note)
		for _, want := range []string{c.id.String(), c.note} {
			if !strings.Contains(section, want) {
				t.Errorf("served fix-up concerns section lacks %q:\n%s", want, section)
			}
		}
	}

	path := fixupResumeWireGoldenPath(t)
	if *updateGolden {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(fixupResumeWireGolden{
			StageType: resp.StageType, Fixup: resp.Fixup, Prompt: resp.Prompt,
			FixupConcernsSection: section, ConcernIDs: ids, Notes: notes,
		}); err != nil {
			t.Fatalf("encode golden: %v", err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil { //nolint:gosec // checked-in fixture
			t.Fatalf("write golden %s: %v", path, err)
		}
		t.Logf("regenerated golden %s", path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v\nregenerate with: %s", path, err, fixupResumeWireUpdateCmd)
	}
	var golden fixupResumeWireGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode golden %s: %v\nregenerate with: %s", path, err, fixupResumeWireUpdateCmd)
	}
	if golden.StageType != resp.StageType || golden.Fixup != resp.Fixup {
		t.Errorf("golden stage_type/fixup = %q/%v, served %q/%v\nregenerate with: %s",
			golden.StageType, golden.Fixup, resp.StageType, resp.Fixup, fixupResumeWireUpdateCmd)
	}
	if golden.Prompt != resp.Prompt {
		t.Errorf("served implement fix-up prompt differs from the shared golden %s (an intentional prompt change needs a regenerated golden)\nregenerate with: %s\n--- served ---\n%s",
			path, fixupResumeWireUpdateCmd, resp.Prompt)
	}
	if golden.FixupConcernsSection != section {
		t.Errorf("served fix-up concerns section differs from the golden's\n--- served ---\n%s\n--- golden ---\n%s\nregenerate with: %s",
			section, golden.FixupConcernsSection, fixupResumeWireUpdateCmd)
	}
	if !strings.Contains(golden.Prompt, golden.FixupConcernsSection) || golden.FixupConcernsSection == "" {
		t.Errorf("golden prompt does not contain its own fixup_concerns_section\nregenerate with: %s", fixupResumeWireUpdateCmd)
	}
	if strings.Join(golden.ConcernIDs, ",") != strings.Join(ids, ",") || strings.Join(golden.Notes, "\n") != strings.Join(notes, "\n") {
		t.Errorf("golden concern_ids/notes = %v / %q, want %v / %q\nregenerate with: %s",
			golden.ConcernIDs, golden.Notes, ids, notes, fixupResumeWireUpdateCmd)
	}
}
