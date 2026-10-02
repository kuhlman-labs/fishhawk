package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/redaction"
)

// The diff secrets check (E80.3 / #3760), server half. Every synthetic
// credential here is BUILT AT RUNTIME (condition 7): no string literal in this
// file matches a redaction.DefaultPatterns regex on its own, so this file
// cannot trip the check it tests or forge push protection.
//
// COUNTERFACTUAL CONVENTION (condition 6): every arm guarding a control names
// the mutation that removes it and the fixture state that makes the removal
// observable as COMMITTED STATE (a concern row present or absent, its stored
// state, an audit entry) rather than as a log line.

// diffSecretsKey is a runtime-built github-pat-classic-shaped value.
func diffSecretsKey() string { return "ghp_" + strings.Repeat("Q", 36) }

// keyPatch adds the key at config/settings.go new-side line 2 and carries the
// SAME key on a REMOVED line of docs/old.go, which must not raise.
func keyPatch() string {
	return "diff --git a/config/settings.go b/config/settings.go\n" +
		"--- a/config/settings.go\n+++ b/config/settings.go\n" +
		"@@ -1,1 +1,2 @@\n package config\n+const githubPAT = \"" + diffSecretsKey() + "\"\n" +
		"diff --git a/docs/old.go b/docs/old.go\n" +
		"--- a/docs/old.go\n+++ b/docs/old.go\n" +
		"@@ -1,2 +1,1 @@\n package docs\n-const old = \"" + diffSecretsKey() + "\"\n"
}

func keyDiff() policy.Diff {
	return policy.Diff{
		ChangedFiles: []policy.ChangedFile{
			{Path: "config/settings.go", Status: policy.StatusModified},
			{Path: "docs/old.go", Status: policy.StatusModified},
		},
		Patch: keyPatch(),
	}
}

// serverCheckRows returns every server_check row on the run.
func serverCheckRows(t *testing.T, cr *fakeConcernRepo, runID uuid.UUID) []*concern.Concern {
	t.Helper()
	all, err := cr.ListByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	var out []*concern.Concern
	for _, c := range all {
		if c.IsServerCheck() {
			out = append(out, c)
		}
	}
	return out
}

// diffSecretsEntries decodes every diff_secrets_detected payload.
func diffSecretsEntries(t *testing.T, au *auditFake) ([]diffSecretsDetectedPayload, []audit.ChainAppendParams) {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var payloads []diffSecretsDetectedPayload
	var params []audit.ChainAppendParams
	for _, ap := range au.appended {
		if ap.Category != diffSecretsDetectedCategory {
			continue
		}
		var p diffSecretsDetectedPayload
		if err := json.Unmarshal(ap.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", diffSecretsDetectedCategory, err)
		}
		payloads = append(payloads, p)
		params = append(params, ap)
	}
	return payloads, params
}

// assertNoKeyBytes fails if the key appears in any concern field, any audit
// payload (all categories), any reviewer prompt or the log buffer.
func assertNoKeyBytes(t *testing.T, cr *fakeConcernRepo, au *auditFake, prompts []string, logs string) {
	t.Helper()
	key := diffSecretsKey()
	cr.mu.Lock()
	for _, c := range cr.rows {
		raw, _ := json.Marshal(c)
		if strings.Contains(string(raw), key) {
			t.Errorf("concern row %s carries the key bytes: %s", c.ID, raw)
		}
	}
	cr.mu.Unlock()
	au.mu.Lock()
	for _, ap := range au.appended {
		if strings.Contains(string(ap.Payload), key) {
			t.Errorf("audit %s payload carries the key bytes", ap.Category)
		}
	}
	au.mu.Unlock()
	for i, p := range prompts {
		if strings.Contains(p, key) {
			t.Errorf("reviewer prompt %d carries the key bytes", i)
		}
	}
	if strings.Contains(logs, key) {
		t.Errorf("log buffer carries the key bytes")
	}
}

// TestDiffSecrets_TraceUploadToHumanWaive is the CROSS-BOUNDARY test: a raw
// implement bundle whose patch adds a key travels handleShipTrace →
// advanceStageAfterTrace → runImplementReviewsForTree → raise → concern store
// → prompt render → reviewer adapter, and then through the waive handler.
//
// COUNTERFACTUALS: (1) delete the raiseDiffSecretConcerns call in
// runImplementReviewsForTree — this fixture's ONLY concern source is that call
// (the fake reviewer approves with no concerns), so the server_check count
// reads 0 → RED. (2) delete the trig.DiffPatch redaction line — the bundle's
// raw patch carries the key verbatim and the reviewer adapter records the
// prompt it was handed, so the captured prompt carries the key and the
// [REDACTED:github-pat-classic] marker is absent → RED.
func TestDiffSecrets_TraceUploadToHumanWaive(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr
	logs := captureLogger(s)
	priv, _ := sf.issue(t, runRow.ID)

	bundleBytes := implementDiffBundleWithPatch(t, []map[string]string{
		{"path": "config/settings.go", "status": "M"},
		{"path": "docs/old.go", "status": "M"},
	}, keyPatch())
	if w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundleBytes, ""); w.Code != http.StatusAccepted {
		t.Fatalf("ship status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	s.waitBackgroundReviews()

	rows := serverCheckRows(t, cr, runRow.ID)
	if len(rows) != 1 {
		t.Fatalf("server_check concerns = %d, want 1 (the removed-line copy must not raise): %+v", len(rows), rows)
	}
	row := rows[0]
	if row.StageID != implStage.ID || row.StageKind != concern.StageKindImplement ||
		row.Severity != "high" || row.Category != "security" || row.ReviewerModel != nil || row.State != concern.StateRaised {
		t.Errorf("concern row = %+v, want an implement/high/security/raised row with no reviewer model", row)
	}
	if row.CheckKey != "diff_secrets|github-pat-classic|config/settings.go" {
		t.Errorf("check_key = %q", row.CheckKey)
	}
	for _, want := range []string{"config/settings.go:2", "github-pat-classic"} {
		if !strings.Contains(row.Note, want) {
			t.Errorf("note missing %q: %s", want, row.Note)
		}
	}
	if strings.Contains(row.Note, "docs/old.go") {
		t.Errorf("note names the removed-line file: %s", row.Note)
	}

	payloads, params := diffSecretsEntries(t, au)
	if len(payloads) != 1 {
		t.Fatalf("%s entries = %d, want 1", diffSecretsDetectedCategory, len(payloads))
	}
	if params[0].ActorKind == nil || *params[0].ActorKind != audit.ActorKind("system") {
		t.Errorf("actor kind = %v, want system", params[0].ActorKind)
	}
	if params[0].StageID == nil || *params[0].StageID != implStage.ID {
		t.Errorf("entry stage = %v, want %s", params[0].StageID, implStage.ID)
	}
	p := payloads[0]
	if p.Check != "diff_secrets" || p.HitCount != 1 || len(p.Groups) != 1 ||
		p.Groups[0].Path != "config/settings.go" || p.Groups[0].Pattern != "github-pat-classic" ||
		len(p.Groups[0].Lines) != 1 || p.Groups[0].Lines[0] != 2 || p.Groups[0].CheckKey != row.CheckKey {
		t.Errorf("payload = %+v", p)
	}

	reviewer.mu.Lock()
	prompts := append([]string(nil), reviewer.calls...)
	reviewer.mu.Unlock()
	if len(prompts) != 1 {
		t.Fatalf("reviewer invocations = %d, want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], "[REDACTED:github-pat-classic]") {
		t.Errorf("reviewer prompt lacks the redaction marker")
	}
	assertNoKeyBytes(t, cr, au, prompts, logs.String())

	// The raised concern is human-only: an operator-agent token is refused...
	w := postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "agent says it is a fixture"}, withOperatorAgentAuth)
	assertRequiresHuman(t, w, row, clearVerbWaive, refusedActorAgentToken)
	assertState(t, cr, row.ID, concern.StateRaised)
	// ...and a human waive with a reason clears it.
	if w := postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "known test fixture"}, withAuth); w.Code != http.StatusOK {
		t.Fatalf("human waive status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	assertState(t, cr, row.ID, concern.StateWaived)
	assertNoKeyBytes(t, cr, au, nil, logs.String())
}

// specImplementNoReviewers declares an implement stage with NO reviewers.
var specImplementNoReviewers = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
`)

// TestDiffSecrets_RunsWithoutReviewersConfigured: the check is independent of
// the model reviewers. COUNTERFACTUAL: move the raiseDiffSecretConcerns call
// below the reviewer-configuration early return — this fixture's implement
// stage declares no reviewers, so that return fires first and zero concerns
// are raised → RED.
func TestDiffSecrets_RunsWithoutReviewersConfigured(t *testing.T) {
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-7"}
	s, _, au, _, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementNoReviewers)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr

	if s.runImplementReviews(context.Background(), runRow.ID, implStage.ID, keyDiff(), nil, "head-1", nil) {
		t.Errorf("runImplementReviews gated with no reviewers configured")
	}
	if n := len(serverCheckRows(t, cr, runRow.ID)); n != 1 {
		t.Fatalf("server_check concerns = %d, want 1 with no reviewer configured", n)
	}
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.calls) != 0 {
		t.Errorf("reviewer invoked %d times, want 0", len(reviewer.calls))
	}
	assertNoKeyBytes(t, cr, au, nil, "")
}

// diffSecretsServer wires only the two stores raiseDiffSecretConcerns reads.
func diffSecretsServer() (*Server, *auditFake, *fakeConcernRepo) {
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	return s, au, cr
}

// TestDiffSecrets_DedupeAcrossRounds: the check key suppresses a re-raise
// while a server_check row for it is open, waived or deferred on THIS stage,
// and does not once the row is superseded or when it sits on another stage.
//
// COUNTERFACTUAL: delete the `!recorded[g.Key()]` skip — the "repeat round"
// arm's fixture already holds one OPEN row with the same key on the same
// stage, so the second raise inserts a duplicate and the count reads 2 → RED.
func TestDiffSecrets_DedupeAcrossRounds(t *testing.T) {
	ctx := context.Background()
	raise := func(s *Server, runID, stageID uuid.UUID) {
		s.raiseDiffSecretConcerns(ctx, runID, stageID, keyDiff(), "head")
	}

	t.Run("repeat round adds nothing", func(t *testing.T) {
		s, au, cr := diffSecretsServer()
		runID, stageID := uuid.New(), uuid.New()
		raise(s, runID, stageID)
		raise(s, runID, stageID)
		if n := len(serverCheckRows(t, cr, runID)); n != 1 {
			t.Fatalf("rows after two rounds = %d, want 1", n)
		}
		if p, _ := diffSecretsEntries(t, au); len(p) != 1 {
			t.Errorf("%s entries = %d, want 1 (a fully de-duplicated round appends nothing)", diffSecretsDetectedCategory, len(p))
		}
	})
	for _, st := range []concern.State{concern.StateWaived, concern.StateDeferred, concern.StateAddressedPending} {
		t.Run("settled as "+string(st)+" suppresses", func(t *testing.T) {
			s, _, cr := diffSecretsServer()
			runID, stageID := uuid.New(), uuid.New()
			raise(s, runID, stageID)
			row := serverCheckRows(t, cr, runID)[0]
			if st == concern.StateAddressedPending {
				if err := cr.MarkAddressedPending(ctx, []uuid.UUID{row.ID}, "routed"); err != nil {
					t.Fatal(err)
				}
			} else if _, err := cr.ApplyResolution(ctx, row.ID, st, "human decision"); err != nil {
				t.Fatal(err)
			}
			raise(s, runID, stageID)
			if n := len(serverCheckRows(t, cr, runID)); n != 1 {
				t.Errorf("rows = %d, want 1 (a %s row suppresses the re-raise)", n, st)
			}
		})
	}
	t.Run("superseded re-raises", func(t *testing.T) {
		s, _, cr := diffSecretsServer()
		runID, stageID := uuid.New(), uuid.New()
		raise(s, runID, stageID)
		row := serverCheckRows(t, cr, runID)[0]
		if _, err := cr.ApplyResolution(ctx, row.ID, concern.StateSuperseded, "stage retried"); err != nil {
			t.Fatal(err)
		}
		raise(s, runID, stageID)
		if n := len(serverCheckRows(t, cr, runID)); n != 2 {
			t.Errorf("rows = %d, want 2 (a superseded row does not suppress)", n)
		}
	})
	t.Run("another stage's row does not suppress", func(t *testing.T) {
		s, _, cr := diffSecretsServer()
		runID := uuid.New()
		raise(s, runID, uuid.New())
		raise(s, runID, uuid.New())
		if n := len(serverCheckRows(t, cr, runID)); n != 2 {
			t.Errorf("rows = %d, want 2 (dedupe is stage-scoped)", n)
		}
	})
	t.Run("a reviewer row with no check key does not suppress", func(t *testing.T) {
		s, _, cr := diffSecretsServer()
		runID, stageID := uuid.New(), uuid.New()
		seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 5, "a reviewer finding")
		raise(s, runID, stageID)
		if n := len(serverCheckRows(t, cr, runID)); n != 1 {
			t.Errorf("server_check rows = %d, want 1", n)
		}
	})
}

// TestDiffSecrets_ConcurrentRaisesDoNotDuplicate pins the in-process mutex:
// many concurrent raises of the same diff on one stage mint exactly one row.
// COUNTERFACTUAL: remove the diffSecretsRaiseMu Lock/Unlock — every goroutine
// can then list before any inserts, and under -race the count exceeds 1 on
// most runs (sampling, so this arm is a likely-RED rather than a certain one).
func TestDiffSecrets_ConcurrentRaisesDoNotDuplicate(t *testing.T) {
	s, _, cr := diffSecretsServer()
	runID, stageID := uuid.New(), uuid.New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.raiseDiffSecretConcerns(context.Background(), runID, stageID, keyDiff(), "head")
		}()
	}
	wg.Wait()
	if n := len(serverCheckRows(t, cr, runID)); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}

// TestDiffSecrets_FailureModes: one arm per named mode.
func TestDiffSecrets_FailureModes(t *testing.T) {
	ctx := context.Background()

	// COUNTERFACTUAL (nil-repo guard deleted): a nil ConcernRepo or AuditRepo
	// interface is dereferenced on the first call → panic → RED.
	t.Run("nil concern repo is a no-op", func(t *testing.T) {
		au := newAuditFake()
		s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
		if got := s.raiseDiffSecretConcerns(ctx, uuid.New(), uuid.New(), keyDiff(), "h"); got != nil {
			t.Errorf("groups = %+v, want nil", got)
		}
		if p, _ := diffSecretsEntries(t, au); len(p) != 0 {
			t.Errorf("entries = %d, want 0", len(p))
		}
	})
	t.Run("nil audit repo is a no-op", func(t *testing.T) {
		cr := newFakeConcernRepo()
		s := New(Config{Addr: "127.0.0.1:0", ConcernRepo: cr})
		runID := uuid.New()
		if got := s.raiseDiffSecretConcerns(ctx, runID, uuid.New(), keyDiff(), "h"); got != nil {
			t.Errorf("groups = %+v, want nil", got)
		}
		if n := len(serverCheckRows(t, cr, runID)); n != 0 {
			t.Errorf("rows = %d, want 0", n)
		}
	})
	// COUNTERFACTUAL (the ChangedFiles>0 WARN deleted): this fixture carries
	// changed files but no patch, so the 'not scanned' line is absent → RED.
	t.Run("empty patch with changed files warns not scanned", func(t *testing.T) {
		s, au, cr := diffSecretsServer()
		logs := captureLogger(s)
		runID := uuid.New()
		d := keyDiff()
		d.Patch = ""
		s.raiseDiffSecretConcerns(ctx, runID, uuid.New(), d, "h")
		if n := len(serverCheckRows(t, cr, runID)); n != 0 {
			t.Errorf("rows = %d, want 0", n)
		}
		if p, _ := diffSecretsEntries(t, au); len(p) != 0 {
			t.Errorf("entries = %d, want 0", len(p))
		}
		if !strings.Contains(logs.String(), "added lines not scanned") || !strings.Contains(logs.String(), "level=WARN") {
			t.Errorf("missing WARN 'not scanned' line:\n%s", logs.String())
		}
	})
	t.Run("empty diff is silent", func(t *testing.T) {
		s, _, _ := diffSecretsServer()
		logs := captureLogger(s)
		s.raiseDiffSecretConcerns(ctx, uuid.New(), uuid.New(), policy.Diff{}, "h")
		if strings.Contains(logs.String(), "not scanned") {
			t.Errorf("an empty diff logged 'not scanned':\n%s", logs.String())
		}
	})
	t.Run("no hits raises nothing", func(t *testing.T) {
		s, au, cr := diffSecretsServer()
		runID := uuid.New()
		d := policy.Diff{Patch: "diff --git a/a.go b/a.go\n@@ -0,0 +1 @@\n+clean := 1\n"}
		if got := s.raiseDiffSecretConcerns(ctx, runID, uuid.New(), d, "h"); got != nil {
			t.Errorf("groups = %+v, want nil", got)
		}
		if n := len(serverCheckRows(t, cr, runID)); n != 0 {
			t.Errorf("rows = %d", n)
		}
		if p, _ := diffSecretsEntries(t, au); len(p) != 0 {
			t.Errorf("entries = %d", len(p))
		}
	})
	// COUNTERFACTUAL (the append-error `return` deleted): the fixture fails ONLY
	// the diff_secrets_detected append, so without the return InsertRaised runs
	// (on the nil entry it panics; with the entry stubbed non-nil one row reads
	// back) → RED either way.
	t.Run("audit append failure raises nothing", func(t *testing.T) {
		s, au, cr := diffSecretsServer()
		au.appendErrCategory = diffSecretsDetectedCategory
		logs := captureLogger(s)
		runID := uuid.New()
		s.raiseDiffSecretConcerns(ctx, runID, uuid.New(), keyDiff(), "h")
		if n := len(serverCheckRows(t, cr, runID)); n != 0 {
			t.Errorf("rows = %d, want 0 (no concern without its origin entry)", n)
		}
		if !strings.Contains(logs.String(), "append diff_secrets_detected failed") {
			t.Errorf("missing WARN:\n%s", logs.String())
		}
	})
	// COUNTERFACTUAL (fail-open replaced by `return nil` on a list error): the
	// fixture's list errors while InsertRaised works, so a fail-closed branch
	// reads back zero rows → RED.
	t.Run("list error fails open and still raises", func(t *testing.T) {
		s, _, cr := diffSecretsServer()
		runID := uuid.New()
		cr.listErr = errors.New("concern store unavailable")
		s.raiseDiffSecretConcerns(ctx, runID, uuid.New(), keyDiff(), "h")
		cr.listErr = nil
		if n := len(serverCheckRows(t, cr, runID)); n != 1 {
			t.Errorf("rows = %d, want 1 (a missed secret is silent; a duplicate is noise)", n)
		}
	})
	t.Run("insert failure keeps the audit entry", func(t *testing.T) {
		s, au, cr := diffSecretsServer()
		cr.insertErr = errors.New("insert failed")
		logs := captureLogger(s)
		s.raiseDiffSecretConcerns(ctx, uuid.New(), uuid.New(), keyDiff(), "h")
		if p, _ := diffSecretsEntries(t, au); len(p) != 1 {
			t.Errorf("entries = %d, want 1", len(p))
		}
		if !strings.Contains(logs.String(), "insert concerns failed") {
			t.Errorf("missing WARN:\n%s", logs.String())
		}
	})
	t.Run("truncated patch is recorded", func(t *testing.T) {
		s, au, _ := diffSecretsServer()
		d := keyDiff()
		d.PatchTruncated = true
		s.raiseDiffSecretConcerns(ctx, uuid.New(), uuid.New(), d, "h")
		p, _ := diffSecretsEntries(t, au)
		if len(p) != 1 || !p[0].PatchTruncated || p[0].HeadSHA != "h" {
			t.Errorf("payloads = %+v, want one with patch_truncated true", p)
		}
	})
}

// TestDiffSecrets_ReviewerCannotRetireServerCheck: a server_check row a fix-up
// routed back (addressed_pending) survives a reviewer `confirmed`, a reviewer
// `superseded`, and the clean-round auto-close.
//
// COUNTERFACTUAL (vetoReason's server_check arm deleted): the row's
// ReviewerModel is EMPTY, so the raiser-rejected arm cannot fire, and the
// fixture carries a routing trigger with NO operator_evidence followed by a
// fixup_pushed (so neither the operator-evidence nor the no-change arm fires)
// on a readable audit (no lookup failure) — nothing else stands between the
// confirm / auto-close and the addressed transition, so arms A and B read the
// row back ADDRESSED → RED. Arm C's counterfactual is dropping the
// `to == superseded && IsServerCheck()` clause in applyConcernResolutions: the
// row then reads back SUPERSEDED → RED.
func TestDiffSecrets_ReviewerCannotRetireServerCheck(t *testing.T) {
	seed := func(t *testing.T, au *auditFake, cr *fakeConcernRepo, runID, stageID uuid.UUID) *concern.Concern {
		t.Helper()
		row := seedServerCheckRow(t, cr, runID, stageID, "high")
		if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{row.ID}, "routed by an operator fix-up"); err != nil {
			t.Fatalf("route: %v", err)
		}
		seedStageAuditEntry(t, au, runID, stageID, 10, CategoryStageFixupTriggered, map[string]any{
			"concern_ids": []string{row.ID.String()},
		})
		seedStageAuditEntry(t, au, runID, stageID, 11, "fixup_pushed", map[string]any{"head_sha": "abc123"})
		return row
	}
	assertVetoed := func(t *testing.T, au *auditFake, cr *fakeConcernRepo, row *concern.Concern, resolution string) {
		t.Helper()
		if got := concernRowAfterRound(t, cr, row.ID); got.State != concern.StateAddressedPending {
			t.Errorf("state = %q, want addressed_pending (no reviewer verdict retires a server_check concern)", got.State)
		}
		v := vetoEntries(t, au)
		if len(v) != 1 || v[0].VetoReason != vetoServerCheckRequiresHuman || v[0].Resolution != resolution {
			t.Errorf("vetoes = %+v, want one %s on %s", v, vetoServerCheckRequiresHuman, resolution)
		}
	}

	t.Run("A confirmed", func(t *testing.T) {
		s, au, cr, runID, stageID := vetoRoundServer()
		row := seed(t, au, cr, runID, stageID)
		peer := confirmingReviewer("fable-5", row.ID.String(), "the credential is gone", planreview.VerdictApprove)
		s.runImplementReviewInvocations(context.Background(), runID, stageID,
			[]reviewerInvocation{{reviewer: peer}},
			planreview.AuthorityAdvisory, "prompt", "author-model", "", "", planreview.DefaultReviewBudget, "", 0)
		assertVetoed(t, au, cr, row, "confirmed")
	})
	t.Run("B clean-round auto-close", func(t *testing.T) {
		s, au, cr, runID, stageID := autoCloseRoundServer()
		row := seed(t, au.auditFake, cr, runID, stageID)
		silent := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "fable-5"}
		s.runImplementReviewInvocations(context.Background(), runID, stageID,
			[]reviewerInvocation{{reviewer: silent}},
			planreview.AuthorityAdvisory, "prompt", "author-model", "", "abc123", planreview.DefaultReviewBudget, "", 0)
		assertVetoed(t, au.auditFake, cr, row, "auto_close")
	})
	t.Run("C superseded", func(t *testing.T) {
		s, au, cr, runID, stageID := vetoRoundServer()
		row := seed(t, au, cr, runID, stageID)
		peer := &fakePlanReviewer{
			verdict: &planreview.ReviewVerdict{
				Verdict:            planreview.VerdictApprove,
				ConcernResolutions: []planreview.ConcernResolution{{ID: row.ID.String(), Resolution: "superseded", Note: "no longer relevant"}},
			},
			model: "fable-5",
		}
		s.runImplementReviewInvocations(context.Background(), runID, stageID,
			[]reviewerInvocation{{reviewer: peer}},
			planreview.AuthorityAdvisory, "prompt", "author-model", "", "", planreview.DefaultReviewBudget, "", 0)
		assertVetoed(t, au, cr, row, "superseded")
	})
	// Control: the superseded clause is provenance-scoped — a reviewer row is
	// still superseded by a reviewer resolution exactly as before.
	t.Run("reviewer row superseded as before", func(t *testing.T) {
		s, au, cr, runID, stageID := vetoRoundServer()
		row := seedRoutedConcern(t, cr, runID, stageID, "gpt-5.6-sol", "a finding", "routed")
		peer := &fakePlanReviewer{
			verdict: &planreview.ReviewVerdict{
				Verdict:            planreview.VerdictApprove,
				ConcernResolutions: []planreview.ConcernResolution{{ID: row.ID.String(), Resolution: "superseded", Note: "replaced"}},
			},
			model: "fable-5",
		}
		s.runImplementReviewInvocations(context.Background(), runID, stageID,
			[]reviewerInvocation{{reviewer: peer}},
			planreview.AuthorityAdvisory, "prompt", "author-model", "", "", planreview.DefaultReviewBudget, "", 0)
		if got := concernRowAfterRound(t, cr, row.ID); got.State != concern.StateSuperseded {
			t.Errorf("state = %q, want superseded", got.State)
		}
		if v := vetoEntries(t, au); len(v) != 0 {
			t.Errorf("vetoes = %+v, want none", v)
		}
	})
}

// TestDiffSecrets_PriorConcernsExcludeServerCheck: an open server_check row is
// absent from the delta-verification set while an open reviewer row is
// present. COUNTERFACTUAL: drop the IsServerCheck skip in
// priorConcernsForReview — the fixture's server_check row is OPEN on the same
// implement stage, so it then appears in the set → RED.
func TestDiffSecrets_PriorConcernsExcludeServerCheck(t *testing.T) {
	s, _, cr := diffSecretsServer()
	runID, stageID := uuid.New(), uuid.New()
	sc := seedServerCheckRow(t, cr, runID, stageID, "high")
	rv := seedConcernRow(t, cr, runID, stageID, concern.StageKindImplement, 100, "a reviewer finding")

	got := s.priorConcernsForReview(context.Background(), runID, stageID)
	if len(got) != 1 || got[0].ID != rv.ID.String() {
		t.Fatalf("prior concerns = %+v, want only the reviewer row %s (server_check %s excluded)", got, rv.ID, sc.ID)
	}
}

// mutableCompareClient is cannedComparePatchClient with a swappable body, so
// one test can drive two backstop rounds with different deltas.
func mutableCompareClient(t *testing.T, body *string, mu *sync.Mutex) *githubclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		b := *body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, b)
	}))
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "jwt", nil },
	}
}

// compareBody renders a GitHub compare response for one file whose patch is
// the given hunk text (no ---/+++ lines; ComparePatch reconstructs only the
// `diff --git` header around it).
func compareBody(t *testing.T, path, hunk string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"total_commits": 1,
		"commits":       []map[string]string{{"sha": "headsha1"}},
		"files":         []map[string]any{{"filename": path, "status": "modified", "changes": 2, "patch": hunk}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestDiffSecrets_FixupBackstopDeltaRaisesAndNormalRoundKeeps drives the REAL
// fix-up re-review backstop (maybeBackstopFixupReReview) with a
// compare-reconstructed delta — `diff --git` header, no ---/+++ — carrying a
// runtime-built key (condition 3), then a second fix-up round whose delta does
// NOT contain the key (condition 2): the row is still raised, because a normal
// fix-up round supersedes nothing.
//
// COUNTERFACTUALS: (1) delete the raiseDiffSecretConcerns call — the round-1
// delta is this fixture's only concern source, so the count reads 0 → RED.
// (2) drop the git-header path fallback in diffsecrets.Scan — the delta has no
// +++ line, so the hit is attributed to the unresolved placeholder and the
// note no longer names config/settings.go:2 → RED.
func TestDiffSecrets_FixupBackstopDeltaRaisesAndNormalRoundKeeps(t *testing.T) {
	var mu sync.Mutex
	body := compareBody(t, "config/settings.go", "@@ -1 +1,2 @@\n package config\n+const githubPAT = \""+diffSecretsKey()+"\"")
	reviewer := &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "claude-opus-4-8"}
	s, _, au, _, runRow, implStage := newFixupReReviewBackstopServer(t, reviewer, "", false)
	s.cfg.GitHub = mutableCompareClient(t, &body, &mu)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr
	logs := captureLogger(s)

	seedImplementReviewStarted(t, au, runRow.ID, implStage.ID, "head-old", time.Now().UTC())
	s.maybeBackstopFixupReReview(context.Background(), runRow.ID, implStage, "head-new", "base-old")
	s.waitBackgroundReviews()

	rows := serverCheckRows(t, cr, runRow.ID)
	if len(rows) != 1 {
		t.Fatalf("server_check concerns after the key-bearing delta = %d, want 1", len(rows))
	}
	if !strings.Contains(rows[0].Note, "config/settings.go:2") || rows[0].StageID != implStage.ID {
		t.Errorf("row = %+v, want a note naming config/settings.go:2 on the implement stage", rows[0])
	}

	// Round 2: a fix-up delta that does not touch the credential.
	mu.Lock()
	body = compareBody(t, "x.go", "@@ -1 +1 @@\n-a\n+b")
	mu.Unlock()
	s.maybeBackstopFixupReReview(context.Background(), runRow.ID, implStage, "head-newer", "head-new")
	s.waitBackgroundReviews()

	if got := startedHeadSHAs(t, au, runRow.ID); len(got) != 3 {
		t.Fatalf("implement_review_started heads = %v, want the seeded one plus two rounds", got)
	}
	rows = serverCheckRows(t, cr, runRow.ID)
	if len(rows) != 1 || rows[0].State != concern.StateRaised {
		t.Errorf("server_check rows after a key-free round = %+v, want the one row still raised", rows)
	}
	reviewer.mu.Lock()
	prompts := append([]string(nil), reviewer.calls...)
	reviewer.mu.Unlock()
	assertNoKeyBytes(t, cr, au, prompts, logs.String())
}

// TestRedactReviewPatch: a credential-free patch is returned byte-identical;
// a key-bearing one comes back redacted with only pattern counts logged.
func TestRedactReviewPatch(t *testing.T) {
	s, _, _ := diffSecretsServer()
	logs := captureLogger(s)
	clean := "diff --git a/a.go b/a.go\n@@ -0,0 +1 @@\n+x := 1\n"
	if got := s.redactReviewPatch(context.Background(), uuid.New(), uuid.New(), clean); got != clean {
		t.Errorf("clean patch changed: %q", got)
	}
	if got := s.redactReviewPatch(context.Background(), uuid.New(), uuid.New(), ""); got != "" {
		t.Errorf("empty patch changed: %q", got)
	}
	got := s.redactReviewPatch(context.Background(), uuid.New(), uuid.New(), keyPatch())
	if strings.Contains(got, diffSecretsKey()) || !strings.Contains(got, "[REDACTED:github-pat-classic]") {
		t.Errorf("redacted patch = %q", got)
	}
	if want, _ := redaction.RedactDefault([]byte(keyPatch())); got != string(want) {
		t.Errorf("redactReviewPatch diverges from redaction.RedactDefault")
	}
	if !strings.Contains(logs.String(), "github-pat-classic=2") || strings.Contains(logs.String(), diffSecretsKey()) {
		t.Errorf("log = %s", logs.String())
	}
}
