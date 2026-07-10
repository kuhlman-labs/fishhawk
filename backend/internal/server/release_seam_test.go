package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/releaseevidence"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// This file is the E33 capstone (E33.6 / #1591, ADR-051): a single
// cross-boundary integration test that stitches the whole release seam in one
// flow — evidence assembly (releaseevidence) -> notes render (releasenotes) ->
// prepare persist (handleReleaseNotesPersist) -> cut decision
// (handleReleaseCut) -> publish body/asset via a fake GitHub publisher
// (handleReleasePublish) -> the run's audit hash-chain. It asserts what the
// per-slice unit tests cannot: that the persisted/published notes body's
// evidence lines resolve to the SEEDED plan + both implement_reviewed verdicts
// + acceptance_outcome_recorded rows (evidence-linked, never fabricated — the
// ADR-051 honesty constraint), that both release_cut and release_published
// audit entries land on the release run's chain with the expected payloads, and
// that the run's audit hash-chain is verifiable end to end (prev_hash -> hash
// continuity across the two entries — the deterministic in-tree analogue of the
// operator's live "release_published verifiable on the chain" Done-means).
//
// It reuses the in-package pgtest harness (newReleaseNotesHarness /
// seedLoopRun / seedStage / fakeReleaseResolver from release_notes_test.go) and
// the fakeReleasePublisher from release_publish_test.go, so the whole flow runs
// offline and deterministically. The one real published GitHub Release named in
// the issue's Done-means is an OPERATOR-EXECUTED live walk (real git tag push +
// real GitHub Release) unreachable by the sandboxed implement/acceptance agents;
// this integration test is the deterministic in-tree proof of the same seam.

// seedEvidenceRun seeds a loop-merged run carrying the full evidence trail the
// release notes must resolve to: an approved standard_v1 plan (via seedLoopRun)
// plus two implement_reviewed verdicts and an acceptance_outcome_recorded entry
// appended to the run's audit chain. It returns the run id so the caller can
// assert the notes resolve to THESE rows (not fabricated). Reuses seedLoopRun
// and looks the run up by PR URL rather than redeclaring the plan-seed logic.
func (h *releaseNotesHarness) seedEvidenceRun(prURL, summary string, reviews []releaseevidence.ReviewerVerdict, acceptanceVerdict string) uuid.UUID {
	h.t.Helper()
	h.seedLoopRun(prURL, summary)

	runID := h.runByPR(prURL)
	for _, rv := range reviews {
		h.appendChainEntry(runID, "implement_reviewed", map[string]any{
			"reviewer_model": rv.ReviewerModel,
			"verdict":        rv.Verdict,
		})
	}
	h.appendChainEntry(runID, CategoryAcceptanceOutcomeRecorded, map[string]any{
		"verdict": acceptanceVerdict,
	})
	return runID
}

// runByPR resolves the single run mapped to prURL (the loop-merged evidence
// run seedLoopRun created), failing the test when the lookup does not return
// exactly one run.
func (h *releaseNotesHarness) runByPR(prURL string) uuid.UUID {
	h.t.Helper()
	url := prURL
	runs, err := h.runRepo.ListRuns(h.ctx, run.ListRunsFilter{PullRequestURL: &url, Limit: 100})
	if err != nil {
		h.t.Fatalf("list runs by pr: %v", err)
	}
	if len(runs) != 1 {
		h.t.Fatalf("runs for %s = %d, want 1", prURL, len(runs))
	}
	return runs[0].ID
}

// appendChainEntry appends a system-actor audit entry to runID's chain via the
// real AppendChained path, so the entry links into the run's hash-chain exactly
// as production writes it.
func (h *releaseNotesHarness) appendChainEntry(runID uuid.UUID, category string, payload map[string]any) {
	h.t.Helper()
	sys := audit.ActorSystem
	body, _ := json.Marshal(payload)
	if _, err := h.auditRepo.AppendChained(h.ctx, audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &sys,
		Payload:   body,
	}); err != nil {
		h.t.Fatalf("append %s entry: %v", category, err)
	}
}

// seedReleaseRun creates the release run + a stage on it. The release run is
// the chain release_cut and release_published are keyed to; the stage is the
// prepare-persist target for the release_notes artifact. Returns (runID,
// stageID).
func (h *releaseNotesHarness) seedReleaseRun() (uuid.UUID, uuid.UUID) {
	h.t.Helper()
	r, err := h.runRepo.CreateRun(h.ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		h.t.Fatalf("create release run: %v", err)
	}
	st, err := h.runRepo.CreateStage(h.ctx, run.CreateStageParams{
		RunID:        r.ID,
		Sequence:     0,
		Type:         run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		h.t.Fatalf("create release stage: %v", err)
	}
	return r.ID, st.ID
}

// TestReleaseSeam_EndToEnd drives the whole release arc in one flow and asserts
// the seam the per-slice unit tests cannot: evidence-linked notes, both audit
// entries on the release run's chain, and a verifiable hash-chain.
func TestReleaseSeam_EndToEnd(t *testing.T) {
	const (
		prURL       = "https://github.com/kuhlman-labs/fishhawk/pull/100"
		planSummary = "assemble release evidence for the E33 capstone"
		tag         = "v0.2.0"
		releaseURL  = "https://github.com/kuhlman-labs/fishhawk/releases/tag/v0.2.0"
	)
	reviews := []releaseevidence.ReviewerVerdict{
		{ReviewerModel: "claude-opus-4-8", Verdict: "approve"},
		{ReviewerModel: "gpt-5.5", Verdict: "approve_with_nits"},
	}

	h := newReleaseNotesHarness(t, mixedPRs()...)
	pub := &fakeReleasePublisher{
		instID: 77, releaseID: 555, body: "stale body", htmlURL: releaseURL,
	}
	h.server.releasePublisherOverride = pub

	// Seed the loop-merged evidence run (plan + two reviews + acceptance) and a
	// separate release run whose chain will carry cut + published.
	h.seedEvidenceRun(prURL, planSummary, reviews, "passed")
	releaseRunID, releaseStageID := h.seedReleaseRun()

	// --- prepare: assemble + render + persist the release_notes artifact ---
	prepareBody, _ := json.Marshal(releaseNotesPersistRequest{
		Repo: "kuhlman-labs/fishhawk", From: "v0.1.0", To: "main", StageID: releaseStageID.String(),
	})
	prepareRec := httptest.NewRecorder()
	h.server.handleReleaseNotesPersist(prepareRec,
		withReleaseOperator(httptest.NewRequest(http.MethodPost, "/v0/releases/notes", strings.NewReader(string(prepareBody)))))
	if prepareRec.Code != http.StatusCreated {
		t.Fatalf("prepare status = %d, want 201:\n%s", prepareRec.Code, prepareRec.Body.String())
	}
	var prepareResp releaseNotesPersistResponse
	if err := json.Unmarshal(prepareRec.Body.Bytes(), &prepareResp); err != nil {
		t.Fatalf("decode prepare response: %v", err)
	}

	// (a) The rendered/persisted notes body's evidence lines resolve to the
	// SEEDED plan + both reviewer verdicts + acceptance — never fabricated.
	md := prepareResp.Markdown
	for _, want := range []string{
		"- Plan: " + prURL,
		planSummary,
		"Reviewer verdicts:",
		"- claude-opus-4-8: approve",
		"- gpt-5.5: approve_with_nits",
		"Acceptance: passed",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("persisted notes missing evidence line %q:\n%s", want, md)
		}
	}
	// Honesty constraint: the reduced-evidence PR (pull/101, no seeded run) is
	// marked reduced rather than carrying fabricated verdicts.
	if !strings.Contains(md, "> **Reduced evidence.**") {
		t.Errorf("persisted notes missing reduced-evidence marker for the unmapped PR:\n%s", md)
	}
	markdownHash := sha256Hex([]byte(md))

	// --- cut: record the ratified version decision on the release run's chain ---
	cutBody, _ := json.Marshal(releaseCutRequest{
		Repo: "kuhlman-labs/fishhawk", RunID: releaseRunID.String(), StageID: releaseStageID.String(),
		ArtifactID: prepareResp.ArtifactID, Version: tag, BumpLevel: "minor",
	})
	cutRec := httptest.NewRecorder()
	h.server.handleReleaseCut(cutRec,
		withReleaseOperator(httptest.NewRequest(http.MethodPost, "/v0/releases/cut", strings.NewReader(string(cutBody)))))
	if cutRec.Code != http.StatusCreated {
		t.Fatalf("cut status = %d, want 201:\n%s", cutRec.Code, cutRec.Body.String())
	}
	var cutResp releaseCutResponse
	if err := json.Unmarshal(cutRec.Body.Bytes(), &cutResp); err != nil {
		t.Fatalf("decode cut response: %v", err)
	}
	if cutResp.ContentHash != prepareResp.ContentHash || cutResp.BumpLevel != "minor" || !cutResp.Recorded {
		t.Errorf("cut response = %+v, want content_hash %q, bump minor, recorded", cutResp, prepareResp.ContentHash)
	}

	// --- publish: set the Release body + asset and record release_published ---
	publishBody, _ := json.Marshal(releasePublishRequest{
		Repo: "kuhlman-labs/fishhawk", Tag: tag, RunID: releaseRunID.String(),
		StageID: releaseStageID.String(), ArtifactID: prepareResp.ArtifactID,
	})
	publishRec := httptest.NewRecorder()
	h.server.handleReleasePublish(publishRec,
		withReleaseOperator(httptest.NewRequest(http.MethodPost, "/v0/releases/publish", strings.NewReader(string(publishBody)))))
	if publishRec.Code != http.StatusOK {
		t.Fatalf("publish status = %d, want 200:\n%s", publishRec.Code, publishRec.Body.String())
	}
	var publishResp releasePublishResponse
	if err := json.Unmarshal(publishRec.Body.Bytes(), &publishResp); err != nil {
		t.Fatalf("decode publish response: %v", err)
	}
	if !publishResp.Published || publishResp.Idempotent {
		t.Errorf("publish flags = %+v, want published:true idempotent:false", publishResp)
	}
	// The Release body + asset were set to the rendered notes markdown.
	if pub.body != md {
		t.Errorf("release body = %q, want the rendered notes markdown", pub.body)
	}
	asset, ok := pub.assetByName(releaseNotesAssetName)
	if !ok {
		t.Fatalf("no %s asset uploaded", releaseNotesAssetName)
	}
	if string(asset.content) != md {
		t.Errorf("asset content = %q, want the rendered notes markdown", asset.content)
	}

	// (b) Both release_cut and release_published audit entries land on the
	// release run's chain with the expected payloads.
	entries, err := h.auditRepo.ListForRun(h.ctx, releaseRunID)
	if err != nil {
		t.Fatalf("list release run entries: %v", err)
	}
	cut := findEntry(t, entries, CategoryReleaseCut)
	published := findEntry(t, entries, CategoryReleasePublished)

	var cutPayload struct {
		Version     string `json:"version"`
		ArtifactID  string `json:"artifact_id"`
		BumpLevel   string `json:"bump_level"`
		ContentHash string `json:"content_hash"`
	}
	if err := json.Unmarshal(cut.Payload, &cutPayload); err != nil {
		t.Fatalf("decode release_cut payload: %v", err)
	}
	if cutPayload.Version != tag || cutPayload.ArtifactID != prepareResp.ArtifactID ||
		cutPayload.BumpLevel != "minor" || cutPayload.ContentHash != prepareResp.ContentHash {
		t.Errorf("release_cut payload = %+v", cutPayload)
	}

	var pubPayload struct {
		Tag         string `json:"tag"`
		ReleaseURL  string `json:"release_url"`
		ArtifactID  string `json:"artifact_id"`
		ContentHash string `json:"content_hash"`
	}
	if err := json.Unmarshal(published.Payload, &pubPayload); err != nil {
		t.Fatalf("decode release_published payload: %v", err)
	}
	if pubPayload.Tag != tag || pubPayload.ReleaseURL != releaseURL ||
		pubPayload.ArtifactID != prepareResp.ArtifactID || pubPayload.ContentHash != markdownHash {
		t.Errorf("release_published payload = %+v, want tag %q release_url %q artifact %q content_hash %q",
			pubPayload, tag, releaseURL, prepareResp.ArtifactID, markdownHash)
	}
	if publishResp.ContentHash != markdownHash {
		t.Errorf("publish response content_hash = %q, want the markdown hash %q", publishResp.ContentHash, markdownHash)
	}

	// (c) The release run's audit hash-chain is verifiable end to end: read the
	// entries ascending and assert prev_hash -> hash continuity across the whole
	// chain, including the release_cut -> release_published link. This is the
	// deterministic in-tree analogue of the operator's live "release_published
	// verifiable on the chain" Done-means.
	assertChainLinks(t, entries)
	// release_cut precedes release_published on the chain, and published links
	// directly back to cut.
	if cut.Sequence >= published.Sequence {
		t.Errorf("release_cut sequence %d not before release_published %d", cut.Sequence, published.Sequence)
	}
	if published.PrevHash == nil || *published.PrevHash != cut.EntryHash {
		t.Errorf("release_published prev_hash does not link to release_cut hash %q (got %v)", cut.EntryHash, published.PrevHash)
	}
	// The release run's chain starts clean: release_cut is its first entry, so
	// its prev_hash is nil.
	if cut.PrevHash != nil {
		t.Errorf("release_cut prev_hash = %v, want nil (first entry on the release run's chain)", *cut.PrevHash)
	}
}

// findEntry returns the single entry with the given category, failing the test
// when zero or more than one is present.
func findEntry(t *testing.T, entries []*audit.Entry, category string) *audit.Entry {
	t.Helper()
	var found *audit.Entry
	for _, e := range entries {
		if e.Category != category {
			continue
		}
		if found != nil {
			t.Fatalf("multiple %s entries on the chain, want exactly 1", category)
		}
		found = e
	}
	if found == nil {
		t.Fatalf("no %s entry on the chain", category)
	}
	return found
}

// assertChainLinks walks entries in ascending order and asserts prev_hash ->
// hash continuity: the first entry's prev_hash is nil and every later entry's
// prev_hash equals the preceding entry's hash. A broken link surfaces the exact
// pair that diverges.
func assertChainLinks(t *testing.T, entries []*audit.Entry) {
	t.Helper()
	if len(entries) == 0 {
		t.Fatal("no entries to verify")
	}
	if entries[0].PrevHash != nil {
		t.Errorf("first entry prev_hash = %v, want nil", *entries[0].PrevHash)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].PrevHash == nil {
			t.Errorf("entry %d (%s) prev_hash = nil, want link to entry %d hash %q",
				i, entries[i].Category, i-1, entries[i-1].EntryHash)
			continue
		}
		if *entries[i].PrevHash != entries[i-1].EntryHash {
			t.Errorf("entry %d (%s) prev_hash = %q, want preceding entry %d hash %q",
				i, entries[i].Category, *entries[i].PrevHash, i-1, entries[i-1].EntryHash)
		}
	}
}
