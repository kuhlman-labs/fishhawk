package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// errTestAssemble is the injected per-run assembly failure for the 500 path.
var errTestAssemble = errors.New("assemble boom")

// ---------------------------------------------------------------------
// Report test helpers.
// ---------------------------------------------------------------------

// reportSeedEntry describes one audit entry to seed for a report run, with
// per-entry control over category, actor, stage, and payload — the shapes the
// report's category-keyed fold needs.
type reportSeedEntry struct {
	category  string
	actorKind audit.ActorKind // "" → nil actor_kind
	subject   string          // "" → nil actor_subject
	stageID   *uuid.UUID
	payload   string
}

// chainReportEntries builds a genuinely-chained audit entry set (real
// prev_hash / entry_hash, StageID included in the hash) for the report tests.
func chainReportEntries(t *testing.T, runID *uuid.UUID, specs ...reportSeedEntry) []*audit.Entry {
	t.Helper()
	var out []*audit.Entry
	var prev *string
	for i, sp := range specs {
		ts := time.Date(2026, 6, 1, 12, i, 0, 0, time.UTC)
		var ak *audit.ActorKind
		var subj *string
		if sp.actorKind != "" {
			a := sp.actorKind
			ak = &a
		}
		if sp.subject != "" {
			s := sp.subject
			subj = &s
		}
		payload := json.RawMessage(sp.payload)
		h, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID:        runID,
			StageID:      sp.stageID,
			Timestamp:    ts,
			Category:     sp.category,
			ActorKind:    ak,
			ActorSubject: subj,
			Payload:      payload,
			PrevHash:     prev,
		})
		if err != nil {
			t.Fatalf("compute hash: %v", err)
		}
		out = append(out, &audit.Entry{
			ID:           uuid.New(),
			Sequence:     int64(i + 1),
			RunID:        runID,
			StageID:      sp.stageID,
			Timestamp:    ts,
			Category:     sp.category,
			ActorKind:    ak,
			ActorSubject: subj,
			Payload:      payload,
			PrevHash:     prev,
			EntryHash:    h,
		})
		hh := h
		prev = &hh
	}
	return out
}

// seedReportRun seeds a run with an explicit workflow id, trigger ref, and
// created_at, returning the run so tests can read its id.
func seedReportRun(fr *fakeRepo, repo, workflowID, triggerRef string, createdAt time.Time) *run.Run {
	r := seedRun(fr, repo, workflowID, run.StateSucceeded, createdAt)
	if triggerRef != "" {
		r.TriggerRef = strPtr(triggerRef)
	}
	return r
}

func doReport(s *Server, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/reports/agent-changes"+query, nil)
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func doReportMarkdown(s *Server, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/reports/agent-changes.md"+query, nil)
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeReport unmarshals a JSON report body.
func decodeReport(t *testing.T, body []byte) agentChangesReport {
	t.Helper()
	var rep agentChangesReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("decode report: %v\nbody: %s", err, body)
	}
	return rep
}

// ---------------------------------------------------------------------
// (a) end-to-end happy path: every done-means column populated.
// ---------------------------------------------------------------------

// buildHappyPathFixture seeds two agent runs (one merged with approvals,
// heterogeneous reviews, and an acceptance outcome; one unmerged PR with no
// acceptance), one human_led_change run, and one run with no PR. It returns the
// fake repos and the seeded run ids so the JSON and markdown tests share one
// fixture.
func buildHappyPathFixture(t *testing.T) (*exportAuditFake, *fakeRepo, *run.Run, *run.Run, *run.Run, *run.Run) {
	t.Helper()
	fr := newFakeRepo()

	implStage := uuid.New()
	acceptStage := uuid.New()

	// Merged agent run — newest, so it sorts first in created_at DESC order.
	merged := seedReportRun(fr, "acme/app", "feature_change", "issue:1606",
		time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))
	// Unmerged agent run.
	unmerged := seedReportRun(fr, "acme/app", "feature_change", "issue:1600",
		time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	// Human-led change.
	humanLed := seedReportRun(fr, "acme/app", "human_led_change", "issue:1590",
		time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC))
	// Run with no PR.
	noPR := seedReportRun(fr, "acme/app", "feature_change", "issue:1580",
		time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC))

	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		merged.ID: chainReportEntries(t, &merged.ID,
			reportSeedEntry{category: "run_created", actorKind: audit.ActorSystem, subject: "sys@fishhawk", payload: `{}`},
			reportSeedEntry{category: "pull_request_opened", actorKind: audit.ActorAgent, subject: "agent@fishhawk", stageID: &implStage,
				payload: `{"pr_url":"https://github.com/acme/app/pull/42","pr_number":42,"branch":"fh/run-merged","head_sha":"deadbeef"}`},
			reportSeedEntry{category: "plan_reviewed", actorKind: audit.ActorAgent, subject: "opus@fishhawk", stageID: &implStage,
				payload: `{"reviewer_kind":"agent","reviewer_model":"opus-4-8","authority":"advisory","verdict":"approve"}`},
			reportSeedEntry{category: "implement_reviewed", actorKind: audit.ActorAgent, subject: "gpt@fishhawk", stageID: &implStage,
				payload: `{"reviewer_kind":"agent","reviewer_model":"gpt-5.5","authority":"advisory","verdict":"approve_with_concerns"}`},
			reportSeedEntry{category: "approval_submitted", actorKind: audit.ActorUser, subject: "alice@acme",
				payload: `{"decision":"approve","surface":"review","approver":"alice@acme"}`},
			// A GitHub-side PR approval — no payload decode; subject/actor_kind
			// come from the entry, decision/surface stay empty.
			reportSeedEntry{category: CategoryPRApprovedOnGitHub, actorKind: audit.ActorUser, subject: "dave@acme", payload: `{}`},
			reportSeedEntry{category: CategoryAcceptanceOutcomeRecorded, actorKind: audit.ActorAgent, subject: "agent@fishhawk", stageID: &acceptStage,
				payload: `{"verdict":"passed","content_hash":"acc-hash-1","evidence_hashes":["ev-hash-a","ev-hash-b"]}`},
			reportSeedEntry{category: CategoryPRMerged, actorKind: audit.ActorUser, subject: "bob@acme",
				payload: `{"pr_url":"https://github.com/acme/app/pull/42","merger":"bob@acme","head_sha":"deadbeef","base_sha":"cafef00d"}`},
		),
		unmerged.ID: chainReportEntries(t, &unmerged.ID,
			reportSeedEntry{category: "run_created", actorKind: audit.ActorSystem, subject: "sys@fishhawk", payload: `{}`},
			reportSeedEntry{category: "pull_request_opened", actorKind: audit.ActorAgent, subject: "agent@fishhawk", stageID: &implStage,
				payload: `{"pr_url":"https://github.com/acme/app/pull/43","pr_number":43,"branch":"fh/run-unmerged","head_sha":"beefdead"}`},
			reportSeedEntry{category: "implement_reviewed", actorKind: audit.ActorAgent, subject: "opus@fishhawk", stageID: &implStage,
				payload: `{"reviewer_kind":"agent","reviewer_model":"opus-4-8","authority":"advisory","verdict":"approve"}`},
		),
		humanLed.ID: chainReportEntries(t, &humanLed.ID,
			reportSeedEntry{category: "run_created", actorKind: audit.ActorSystem, subject: "sys@fishhawk", payload: `{}`},
			reportSeedEntry{category: "approval_submitted", actorKind: audit.ActorUser, subject: "carol@acme",
				payload: `{"decision":"approve","surface":"review","approver":"carol@acme"}`},
			// A review entry that MUST be dropped for a human-led item.
			reportSeedEntry{category: "implement_reviewed", actorKind: audit.ActorAgent, subject: "opus@fishhawk",
				payload: `{"reviewer_kind":"agent","reviewer_model":"opus-4-8","authority":"advisory","verdict":"approve"}`},
			reportSeedEntry{category: "pull_request_opened", actorKind: audit.ActorUser, subject: "carol@acme",
				payload: `{"pr_url":"https://github.com/acme/app/pull/40","pr_number":40,"branch":"human/surface","head_sha":"0ddba11"}`},
			reportSeedEntry{category: CategoryPRMerged, actorKind: audit.ActorUser, subject: "carol@acme",
				payload: `{"pr_url":"https://github.com/acme/app/pull/40","merger":"carol@acme","head_sha":"0ddba11","base_sha":"feedface"}`},
		),
		noPR.ID: chainReportEntries(t, &noPR.ID,
			reportSeedEntry{category: "run_created", actorKind: audit.ActorSystem, subject: "sys@fishhawk", payload: `{}`},
			reportSeedEntry{category: "plan_generated", actorKind: audit.ActorAgent, subject: "agent@fishhawk", payload: `{}`},
		),
	}}
	return af, fr, merged, unmerged, humanLed, noPR
}

func TestAgentChangesReport_HappyPath(t *testing.T) {
	af, fr, merged, unmerged, humanLed, _ := buildHappyPathFixture(t)

	t.Run("relative evidence links when ExternalURL unset", func(t *testing.T) {
		s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
		rec := doReport(s, "?repo=acme/app&from=2026-06-01T00:00:00Z&to=2026-06-30T00:00:00Z")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		rep := decodeReport(t, rec.Body.Bytes())

		if rep.Schema != agentChangesReportSchema {
			t.Errorf("schema = %q, want %q", rep.Schema, agentChangesReportSchema)
		}
		// Totals: 4 runs in range, 2 agent, 1 human-led, 1 without change.
		if rep.Totals.RunsInRange != 4 || rep.Totals.AgentChanges != 2 ||
			rep.Totals.HumanLedChanges != 1 || rep.Totals.RunsWithoutChange != 1 {
			t.Fatalf("totals = %+v", rep.Totals)
		}
		if len(rep.AgentChanges) != 2 {
			t.Fatalf("agent_changes = %d, want 2", len(rep.AgentChanges))
		}
		// created_at DESC: merged first, unmerged second.
		got0 := rep.AgentChanges[0]
		if got0.RunID != merged.ID.String() {
			t.Fatalf("first agent change = %s, want merged run %s", got0.RunID, merged.ID)
		}

		// what-changed
		if got0.PR.URL != "https://github.com/acme/app/pull/42" || got0.PR.Number != 42 ||
			got0.PR.Branch != "fh/run-merged" || got0.PR.HeadSHA != "deadbeef" {
			t.Errorf("PR = %+v", got0.PR)
		}
		if got0.TriggerRef != "issue:1606" {
			t.Errorf("trigger_ref = %q", got0.TriggerRef)
		}
		// merge
		if got0.Merge == nil || !got0.Merge.Merged || got0.Merge.Merger != "bob@acme" ||
			got0.Merge.BaseSHA != "cafef00d" {
			t.Errorf("merge = %+v", got0.Merge)
		}
		// who approved (subject + timestamp): the operator approval_submitted
		// then the GitHub pr_approved_on_github, in chain order.
		if len(got0.Approvals) != 2 {
			t.Fatalf("approvals = %+v, want 2 (operator + github)", got0.Approvals)
		}
		if got0.Approvals[0].Subject != "alice@acme" ||
			got0.Approvals[0].Decision != "approve" || got0.Approvals[0].ActorKind != "user" {
			t.Errorf("operator approval = %+v", got0.Approvals[0])
		}
		if got0.Approvals[0].Timestamp.IsZero() {
			t.Error("approval timestamp is zero")
		}
		// GitHub PR approval: subject/actor_kind mapped from the entry, and
		// decision/surface empty because that branch does not decode a payload.
		if got0.Approvals[1].Subject != "dave@acme" || got0.Approvals[1].ActorKind != "user" ||
			got0.Approvals[1].Decision != "" || got0.Approvals[1].Surface != "" {
			t.Errorf("github approval = %+v, want subject dave@acme with empty decision/surface", got0.Approvals[1])
		}
		if got0.Approvals[1].Timestamp.IsZero() {
			t.Error("github approval timestamp is zero")
		}
		// what reviewed it (both reviewer models + verdicts)
		gotReviews := map[string]string{}
		for _, rv := range got0.Reviews {
			gotReviews[rv.ReviewerModel] = rv.Stage + ":" + rv.Verdict
		}
		if gotReviews["opus-4-8"] != "plan:approve" {
			t.Errorf("opus review = %q, want plan:approve", gotReviews["opus-4-8"])
		}
		if gotReviews["gpt-5.5"] != "implement:approve_with_concerns" {
			t.Errorf("gpt review = %q, want implement:approve_with_concerns", gotReviews["gpt-5.5"])
		}
		// what validated it (verdict + evidence hashes + content hash)
		if got0.Acceptance == nil || got0.Acceptance.Verdict != "passed" ||
			got0.Acceptance.ContentHash != "acc-hash-1" {
			t.Errorf("acceptance = %+v", got0.Acceptance)
		}
		if strings.Join(got0.Acceptance.EvidenceHashes, ",") != "ev-hash-a,ev-hash-b" {
			t.Errorf("evidence hashes = %v", got0.Acceptance.EvidenceHashes)
		}
		// audit-chain span: 8 entries, sequences 1..8.
		if got0.AuditChain.EntryCount != 8 || got0.AuditChain.FirstSequence != 1 ||
			got0.AuditChain.LastSequence != 8 {
			t.Errorf("audit_chain = %+v", got0.AuditChain)
		}
		if got0.AuditChain.FirstEntryHash == "" || got0.AuditChain.LastEntryHash == "" {
			t.Error("audit_chain entry hashes empty")
		}
		// evidence links — RELATIVE (no ExternalURL prefix).
		if got0.EvidenceLinks.Run != "/v0/runs/"+merged.ID.String() {
			t.Errorf("run link = %q", got0.EvidenceLinks.Run)
		}
		if got0.EvidenceLinks.Audit != "/v0/runs/"+merged.ID.String()+"/audit" {
			t.Errorf("audit link = %q", got0.EvidenceLinks.Audit)
		}
		if got0.EvidenceLinks.Export != "/v0/audit/export?run_id="+merged.ID.String() {
			t.Errorf("export link = %q", got0.EvidenceLinks.Export)
		}
		// Artifacts for the implement + acceptance stages seen in the chain.
		if len(got0.EvidenceLinks.Artifacts) != 2 {
			t.Errorf("artifact links = %v, want 2 (implement + acceptance stage)", got0.EvidenceLinks.Artifacts)
		}
		for _, art := range got0.EvidenceLinks.Artifacts {
			if !strings.HasPrefix(art, "/v0/stages/") || !strings.HasSuffix(art, "/artifacts") {
				t.Errorf("artifact link malformed: %q", art)
			}
		}

		// Unmerged run: PR present, merge nil, no acceptance.
		got1 := rep.AgentChanges[1]
		if got1.RunID != unmerged.ID.String() {
			t.Fatalf("second agent change = %s, want unmerged %s", got1.RunID, unmerged.ID)
		}
		if got1.Merge != nil {
			t.Errorf("unmerged run has merge = %+v, want nil", got1.Merge)
		}
		if got1.Acceptance != nil {
			t.Errorf("unmerged run has acceptance = %+v, want nil", got1.Acceptance)
		}

		// Human-led item: its own section, reviews + acceptance absent.
		if len(rep.HumanLedChanges) != 1 {
			t.Fatalf("human_led_changes = %d, want 1", len(rep.HumanLedChanges))
		}
		hl := rep.HumanLedChanges[0]
		if hl.RunID != humanLed.ID.String() {
			t.Errorf("human-led run = %s, want %s", hl.RunID, humanLed.ID)
		}
		if len(hl.Reviews) != 0 {
			t.Errorf("human-led item carries reviews %+v, want none (reduced evidence)", hl.Reviews)
		}
		if hl.Acceptance != nil {
			t.Errorf("human-led item carries acceptance, want none")
		}
		// But it DOES carry approval + PR + chain span (reduced ≠ empty).
		if len(hl.Approvals) != 1 || hl.Approvals[0].Subject != "carol@acme" {
			t.Errorf("human-led approvals = %+v", hl.Approvals)
		}
		if hl.PR.Number != 40 {
			t.Errorf("human-led PR = %+v", hl.PR)
		}
		if hl.AuditChain.EntryCount == 0 {
			t.Error("human-led audit chain span missing")
		}
	})

	t.Run("ExternalURL-prefixed evidence links", func(t *testing.T) {
		s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{},
			ExternalURL: "https://fishhawk.example.com/"})
		rec := doReport(s, "?repo=acme/app")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		rep := decodeReport(t, rec.Body.Bytes())
		got0 := rep.AgentChanges[0]
		want := "https://fishhawk.example.com/v0/runs/" + merged.ID.String()
		if got0.EvidenceLinks.Run != want {
			t.Errorf("run link = %q, want %q (ExternalURL-prefixed, no double slash)", got0.EvidenceLinks.Run, want)
		}
		if !strings.HasPrefix(got0.EvidenceLinks.Export, "https://fishhawk.example.com/v0/audit/export") {
			t.Errorf("export link = %q, want ExternalURL-prefixed", got0.EvidenceLinks.Export)
		}
	})
}

// ---------------------------------------------------------------------
// (b) golden-file test on the markdown render.
// ---------------------------------------------------------------------

// goldenFixtureReport is a deterministic report model (fixed UUIDs, hashes,
// timestamps) so the golden markdown is byte-stable across runs.
func goldenFixtureReport() agentChangesReport {
	return agentChangesReport{
		Schema:      agentChangesReportSchema,
		GeneratedAt: time.Date(2026, 7, 2, 14, 45, 0, 0, time.UTC),
		Filters:     agentChangesFilters{Repo: "acme/app", From: "2026-06-01T00:00:00Z", To: "2026-06-30T00:00:00Z"},
		Complete:    true,
		AgentChanges: []agentChangeItem{{
			RunID:      "11111111-1111-1111-1111-111111111111",
			Repo:       "acme/app",
			WorkflowID: "feature_change",
			TriggerRef: "issue:1606",
			PR:         agentChangePR{URL: "https://github.com/acme/app/pull/42", Number: 42, Branch: "fh/run-merged", HeadSHA: "deadbeef"},
			Merge: &agentChangeMerge{Merged: true, Merger: "bob@acme", HeadSHA: "deadbeef", BaseSHA: "cafef00d",
				MergedAt: time.Date(2026, 6, 10, 12, 6, 0, 0, time.UTC)},
			Approvals: []agentChangeApproval{{Subject: "alice@acme", ActorKind: "user", Decision: "approve", Surface: "review",
				Timestamp: time.Date(2026, 6, 10, 12, 4, 0, 0, time.UTC)}},
			Reviews: []agentChangeReview{
				{Stage: "plan", ReviewerKind: "agent", ReviewerModel: "opus-4-8", Verdict: "approve",
					Timestamp: time.Date(2026, 6, 10, 12, 2, 0, 0, time.UTC)},
				{Stage: "implement", ReviewerKind: "agent", ReviewerModel: "gpt-5.5", Verdict: "approve_with_concerns",
					Timestamp: time.Date(2026, 6, 10, 12, 3, 0, 0, time.UTC)},
			},
			Acceptance: &agentChangeAcceptance{Verdict: "passed", EvidenceHashes: []string{"ev-hash-a", "ev-hash-b"}, ContentHash: "acc-hash-1",
				Timestamp: time.Date(2026, 6, 10, 12, 5, 0, 0, time.UTC)},
			AuditChain: agentChangeAuditChain{FirstSequence: 1, LastSequence: 7, FirstEntryHash: "hash-first", LastEntryHash: "hash-last", EntryCount: 7},
			EvidenceLinks: agentChangeEvidenceLinks{
				Run:       "/v0/runs/11111111-1111-1111-1111-111111111111",
				Audit:     "/v0/runs/11111111-1111-1111-1111-111111111111/audit",
				Export:    "/v0/audit/export?run_id=11111111-1111-1111-1111-111111111111",
				Artifacts: []string{"/v0/stages/22222222-2222-2222-2222-222222222222/artifacts"},
			},
		}},
		HumanLedChanges: []agentChangeItem{{
			RunID:      "33333333-3333-3333-3333-333333333333",
			Repo:       "acme/app",
			WorkflowID: "human_led_change",
			TriggerRef: "issue:1590",
			PR:         agentChangePR{URL: "https://github.com/acme/app/pull/40", Number: 40, Branch: "human/surface", HeadSHA: "0ddba11"},
			Merge: &agentChangeMerge{Merged: true, Merger: "carol@acme", HeadSHA: "0ddba11", BaseSHA: "feedface",
				MergedAt: time.Date(2026, 6, 8, 12, 4, 0, 0, time.UTC)},
			Approvals: []agentChangeApproval{{Subject: "carol@acme", ActorKind: "user", Decision: "approve", Surface: "review",
				Timestamp: time.Date(2026, 6, 8, 12, 1, 0, 0, time.UTC)}},
			AuditChain: agentChangeAuditChain{FirstSequence: 1, LastSequence: 5, FirstEntryHash: "hl-first", LastEntryHash: "hl-last", EntryCount: 5},
			EvidenceLinks: agentChangeEvidenceLinks{
				Run:    "/v0/runs/33333333-3333-3333-3333-333333333333",
				Audit:  "/v0/runs/33333333-3333-3333-3333-333333333333/audit",
				Export: "/v0/audit/export?run_id=33333333-3333-3333-3333-333333333333",
			},
		}},
		Totals: agentChangesTotals{RunsInRange: 4, AgentChanges: 1, HumanLedChanges: 1, RunsWithoutChange: 2},
	}
}

func TestAgentChangesReport_MarkdownGolden(t *testing.T) {
	got := renderAgentChangesMarkdown(goldenFixtureReport())
	const goldenPath = "testdata/agent_changes_report.golden.md"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with UPDATE_GOLDEN=1 to regenerate): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("markdown render drifted from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A failed-acceptance item renders its failure_mode on the acceptance line —
// the non-empty branch of renderChangeItem that the passed-verdict golden
// fixture (verdict=passed, no failure_mode) never exercises.
func TestAgentChangesReport_AcceptanceFailureModeRendered(t *testing.T) {
	report := agentChangesReport{
		Schema:      agentChangesReportSchema,
		GeneratedAt: time.Date(2026, 7, 2, 14, 45, 0, 0, time.UTC),
		Complete:    true,
		AgentChanges: []agentChangeItem{{
			RunID:      "44444444-4444-4444-4444-444444444444",
			Repo:       "acme/app",
			WorkflowID: "feature_change",
			PR:         agentChangePR{URL: "https://github.com/acme/app/pull/99", Number: 99},
			Acceptance: &agentChangeAcceptance{Verdict: "failed", FailureMode: "criteria_unmet",
				Timestamp: time.Date(2026, 6, 10, 12, 5, 0, 0, time.UTC)},
			EvidenceLinks: agentChangeEvidenceLinks{
				Run:    "/v0/runs/44444444-4444-4444-4444-444444444444",
				Audit:  "/v0/runs/44444444-4444-4444-4444-444444444444/audit",
				Export: "/v0/audit/export?run_id=44444444-4444-4444-4444-444444444444",
			},
		}},
	}
	md := string(renderAgentChangesMarkdown(report))
	if !strings.Contains(md, "- Acceptance: verdict=failed failure_mode=criteria_unmet") {
		t.Errorf("markdown missing failed-acceptance failure_mode line:\n%s", md)
	}
}

// ---------------------------------------------------------------------
// (c) per-failure-mode tests, one behavioral assertion each.
// ---------------------------------------------------------------------

func TestAgentChangesReport_Unconfigured(t *testing.T) {
	af := &exportAuditFake{}
	fr := newFakeRepo()
	sf := &exportSigningFake{}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil audit", Config{RunRepo: fr, SigningRepo: sf}},
		{"nil run", Config{AuditRepo: af, SigningRepo: sf}},
		{"nil signing", Config{AuditRepo: af, RunRepo: fr}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg)
			rec := doReport(s, "")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "audit_export_unconfigured") {
				t.Errorf("body = %s, want audit_export_unconfigured", rec.Body.String())
			}
		})
	}
}

func TestAgentChangesReport_BadFromTimestamp(t *testing.T) {
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	rec := doReport(s, "?from=not-a-time")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "validation_failed") {
		t.Errorf("body = %s, want validation_failed", rec.Body.String())
	}
}

func TestAgentChangesReport_RunIDMutualExclusion(t *testing.T) {
	// run_id combined with repo/date proves the shared resolveExportPage path.
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	rec := doReport(s, "?run_id="+uuid.New().String()+"&repo=acme/app&from=2026-06-01T00:00:00Z")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "validation_failed") {
		t.Errorf("body = %s, want validation_failed", rec.Body.String())
	}
}

// A partial page sets complete=false + a next cursor in BOTH headers and the
// JSON body, and the markdown render carries the PARTIAL banner.
func TestAgentChangesReport_PartialPage(t *testing.T) {
	fr := newFakeRepo()
	implStage := uuid.New()
	// Two runs; limit=1 yields a partial first page.
	r1 := seedReportRun(fr, "acme/app", "feature_change", "issue:1", time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))
	r2 := seedReportRun(fr, "acme/app", "feature_change", "issue:2", time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC))
	prPayload := func(n int) string {
		return `{"pr_url":"https://github.com/acme/app/pull/` + strings.Repeat("9", n) + `","pr_number":` + strings.Repeat("9", n) + `}`
	}
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		r1.ID: chainReportEntries(t, &r1.ID,
			reportSeedEntry{category: "pull_request_opened", actorKind: audit.ActorAgent, subject: "a@x", stageID: &implStage, payload: prPayload(1)}),
		r2.ID: chainReportEntries(t, &r2.ID,
			reportSeedEntry{category: "pull_request_opened", actorKind: audit.ActorAgent, subject: "a@x", stageID: &implStage, payload: prPayload(2)}),
	}}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	t.Run("json body + headers carry continuation", func(t *testing.T) {
		rec := doReport(s, "?limit=1")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if c := rec.Header().Get("X-Fishhawk-Export-Complete"); c != "false" {
			t.Errorf("header Complete = %q, want false", c)
		}
		next := rec.Header().Get("X-Fishhawk-Export-Next-Cursor")
		if next == "" {
			t.Fatal("partial page missing next-cursor header")
		}
		rep := decodeReport(t, rec.Body.Bytes())
		if rep.Complete {
			t.Error("body complete = true, want false on a partial page")
		}
		if rep.NextCursor != next {
			t.Errorf("body next_cursor = %q, header = %q; want equal", rep.NextCursor, next)
		}
	})

	t.Run("markdown banner carries the header's resume cursor", func(t *testing.T) {
		rec := doReportMarkdown(s, "?limit=1")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		next := rec.Header().Get("X-Fishhawk-Export-Next-Cursor")
		if next == "" {
			t.Fatal("partial markdown page missing next-cursor header")
		}
		// Header/body/markdown cursor consistency: the banner must resume with
		// the SAME cursor emitted in X-Fishhawk-Export-Next-Cursor, not just
		// contain a PARTIAL marker.
		wantBanner := "**PARTIAL REPORT — resume with cursor " + next + "**"
		if !strings.Contains(rec.Body.String(), wantBanner) {
			t.Errorf("markdown banner missing exact header cursor %q:\n%s", next, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
			t.Errorf("Content-Type = %q", ct)
		}
	})
}

// A malformed implement_reviewed payload is skipped without failing the
// request — the item still renders (PR present), the review is simply absent.
func TestAgentChangesReport_MalformedReviewSkipped(t *testing.T) {
	fr := newFakeRepo()
	implStage := uuid.New()
	r := seedReportRun(fr, "acme/app", "feature_change", "issue:1", time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		r.ID: chainReportEntries(t, &r.ID,
			reportSeedEntry{category: "pull_request_opened", actorKind: audit.ActorAgent, subject: "a@x", stageID: &implStage,
				payload: `{"pr_url":"https://github.com/acme/app/pull/1","pr_number":1}`},
			// Malformed: implement_reviewed payload is not an object.
			reportSeedEntry{category: "implement_reviewed", actorKind: audit.ActorAgent, subject: "opus@x", stageID: &implStage,
				payload: `"this is a string, not an object"`},
		),
	}}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
	rec := doReport(s, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (malformed payload must not 500); body %s", rec.Code, rec.Body.String())
	}
	rep := decodeReport(t, rec.Body.Bytes())
	if len(rep.AgentChanges) != 1 {
		t.Fatalf("agent_changes = %d, want 1 (item still renders)", len(rep.AgentChanges))
	}
	if len(rep.AgentChanges[0].Reviews) != 0 {
		t.Errorf("reviews = %+v, want none (malformed review skipped)", rep.AgentChanges[0].Reviews)
	}
	// The PR (the valid entry) is still present — the request succeeded.
	if rep.AgentChanges[0].PR.Number != 1 {
		t.Errorf("PR = %+v, want the valid entry", rep.AgentChanges[0].PR)
	}
}

// A per-run assembly error surfaces as a clean 500 internal_error (the
// buildAgentChangesReport → assembleRunData failure branch).
func TestAgentChangesReport_AssembleError(t *testing.T) {
	fr := newFakeRepo()
	seedReportRun(fr, "acme/app", "feature_change", "issue:1", time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{listForRunErr: errTestAssemble}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
	rec := doReport(s, "?include_global=false")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "internal_error") {
		t.Errorf("body = %s, want internal_error", rec.Body.String())
	}
}

// ---------------------------------------------------------------------
// (d) JSON–markdown parity: every pr_url in the JSON body appears in the
// markdown render of the same request (one-model-two-renders invariant).
// ---------------------------------------------------------------------

func TestAgentChangesReport_JSONMarkdownParity(t *testing.T) {
	af, fr, _, _, _, _ := buildHappyPathFixture(t)
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	jsonRec := doReport(s, "?repo=acme/app")
	if jsonRec.Code != http.StatusOK {
		t.Fatalf("json status %d: %s", jsonRec.Code, jsonRec.Body.String())
	}
	rep := decodeReport(t, jsonRec.Body.Bytes())

	mdRec := doReportMarkdown(s, "?repo=acme/app")
	if mdRec.Code != http.StatusOK {
		t.Fatalf("markdown status %d: %s", mdRec.Code, mdRec.Body.String())
	}
	md := mdRec.Body.String()

	var prURLs []string
	for _, item := range rep.AgentChanges {
		prURLs = append(prURLs, item.PR.URL)
	}
	for _, item := range rep.HumanLedChanges {
		prURLs = append(prURLs, item.PR.URL)
	}
	if len(prURLs) == 0 {
		t.Fatal("fixture produced no PR URLs to compare")
	}
	for _, u := range prURLs {
		if !strings.Contains(md, u) {
			t.Errorf("pr_url %q present in JSON but absent from markdown render (parity broken)", u)
		}
	}
}

// ---------------------------------------------------------------------
// (e) route registered (both paths reach the handler).
// ---------------------------------------------------------------------

func TestAgentChangesReport_RoutesRegistered(t *testing.T) {
	s := New(Config{})
	for _, rec := range []*httptest.ResponseRecorder{doReport(s, ""), doReportMarkdown(s, "")} {
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 (route reaches handler)", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "audit_export_unconfigured") {
			t.Errorf("body = %s, want audit_export_unconfigured", rec.Body.String())
		}
	}
}
