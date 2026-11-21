package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/diffsecrets"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
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
		if got := s.raiseDiffSecretConcerns(ctx, uuid.New(), uuid.New(), keyDiff(), "h"); got.PersonaGroups != nil || got.FreshGroups != nil {
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
		if got := s.raiseDiffSecretConcerns(ctx, runID, uuid.New(), keyDiff(), "h"); got.PersonaGroups != nil || got.FreshGroups != nil {
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
		if got := s.raiseDiffSecretConcerns(ctx, runID, uuid.New(), d, "h"); got.PersonaGroups != nil || got.FreshGroups != nil {
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

// ---- Reviewed allowlist and fixture markers (ADR-084 A2, #5230), slice 1 ----
//
// Every credential below is BUILT AT RUNTIME. The allowlist is served by the
// same-package driftGH fake at the run's ADMISSION commit (alAdmit, 40 hex).
// K6: each counterfactual names the fixture state that makes its deletion
// observable.

const (
	alAdmit = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	alHead  = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
)

// alKey is a runtime-built github-pat-classic-shaped value; a distinct fill
// rune gives distinct matched bytes (and so a distinct per-hit fingerprint).
func alKey(fill string) string { return "ghp_" + strings.Repeat(fill, 36) }

// alAllowlist is the admission allowlist: entry docs-placeholder covers
// docs/p.md, marker scope fixture-scope covers fix/**, both for
// github-pat-classic.
const alAllowlist = `version: 1
entries:
  - id: docs-placeholder
    paths: ["docs/p.md"]
    patterns: ["github-pat-classic"]
    reason: "documented placeholder"
markers:
  - id: fixture-scope
    paths: ["fix/**"]
    patterns: ["github-pat-classic"]
    reason: "synthetic fixture keys"
`

// alPermissive suppresses every github-pat-classic hit anywhere: a fixture
// rung that wrongly loaded it would suppress the hit, so the raise reads 0.
const alPermissive = `version: 1
entries:
  - id: everything
    paths: ["**"]
    patterns: ["github-pat-classic"]
    reason: "permissive fixture"
`

// alFile is one file's added lines.
type alFile struct {
	path  string
	lines []string
}

// alPatch renders a unified diff adding each file's lines from new-side 1.
func alPatch(files ...alFile) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString("diff --git a/" + f.path + " b/" + f.path + "\n--- a/" + f.path + "\n+++ b/" + f.path + "\n")
		b.WriteString("@@ -0,0 +1," + strconv.Itoa(len(f.lines)) + " @@\n")
		for _, l := range f.lines {
			b.WriteString("+" + l + "\n")
		}
	}
	return b.String()
}

func alLine(key string) string        { return `const k = "` + key + `"` }
func alMarked(key string) string      { return `const k = "` + key + `" // ` + diffsecrets.MarkerToken }
func alDiff(patch string) policy.Diff { return policy.Diff{Patch: patch} }

// alStandardPatch: (1) docs/p.md entry-suppressed, (2) fix/m.go marked inside
// the marker scope, (3) fix/a.go UNMARKED inside the scope (kept), (4) src/b.go
// outside everything (kept).
func alStandardPatch() string {
	return alPatch(
		alFile{"docs/p.md", []string{alLine(alKey("A"))}},
		alFile{"fix/m.go", []string{alMarked(alKey("B"))}},
		alFile{"fix/a.go", []string{alLine(alKey("C"))}},
		alFile{"src/b.go", []string{alLine(alKey("D"))}},
	)
}

func alNeedles() []string {
	return []string{alKey("A"), alKey("B"), alKey("C"), alKey("D"), alKey("E")}
}

// alFixture is the drift fixture with a 40-hex admission commit and a
// captured log buffer.
type alFixture struct {
	*driftFixture
	logs *bytes.Buffer
}

func newALFixture(t *testing.T) *alFixture {
	t.Helper()
	f := newDriftFixture(t)
	admit := alAdmit
	f.runRow.DocumentBaseCommit = &admit
	logs := captureLogger(f.s)
	return &alFixture{driftFixture: f, logs: logs}
}

func (f *alFixture) raise(patch, head string) diffSecretsRound {
	return f.s.raiseDiffSecretConcerns(context.Background(), f.runRow.ID, f.stage.ID, alDiff(patch), head)
}

func (f *alFixture) rows(t *testing.T) []*concern.Concern {
	return serverCheckRows(t, f.cr.fakeConcernRepo, f.runRow.ID)
}

func (f *alFixture) entries(t *testing.T) []diffSecretsDetectedPayload {
	p, _ := diffSecretsEntries(t, f.au.auditFake)
	return p
}

func (f *alFixture) allowlistReads() int { return len(f.gh.readRefs(diffsecrets.AllowlistPath)) }

// byCategory splits server_check rows into high security and low marker rows.
func byCategory(rows []*concern.Concern) (high, marker []*concern.Concern) {
	for _, r := range rows {
		switch r.Category {
		case "security":
			high = append(high, r)
		case diffSecretsMarkerConcernCategory:
			marker = append(marker, r)
		}
	}
	return high, marker
}

func rowPaths(rows []*concern.Concern) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.CheckKey[strings.LastIndex(r.CheckKey, "|")+1:])
	}
	sort.Strings(out)
	return out
}

// TestDiffSecrets_AllowlistSuppressesAtAdmissionCommit: the admission
// allowlist suppresses the entry hit (counted only) and the marked in-scope
// hit (listed, plus ONE low marker concern); the unmarked in-scope hit and the
// out-of-scope hit still raise high.
//
// COUNTERFACTUAL: delete the Partition call (raise over every hit) — this
// fixture's docs/p.md hit is covered by the entry, so it then raises a third
// high row and the high-path assertion reads [docs/p.md fix/a.go fix/m.go
// src/b.go] → RED.
func TestDiffSecrets_AllowlistSuppressesAtAdmissionCommit(t *testing.T) {
	f := newALFixture(t)
	f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
	f.raise(alStandardPatch(), alHead)

	high, marker := byCategory(f.rows(t))
	if got := rowPaths(high); strings.Join(got, ",") != "fix/a.go,src/b.go" {
		t.Errorf("high rows = %v, want fix/a.go and src/b.go only", got)
	}
	for _, r := range high {
		if r.Severity != "high" || r.Provenance != concern.ProvenanceServerCheck {
			t.Errorf("high row = %+v", r)
		}
	}
	if len(marker) != 1 {
		t.Fatalf("marker rows = %d, want 1: %+v", len(marker), marker)
	}
	m := marker[0]
	if m.Severity != "low" || m.Provenance != concern.ProvenanceServerCheck || m.State != concern.StateRaised {
		t.Errorf("marker row = %+v, want low/server_check/raised", m)
	}
	ps := f.entries(t)
	if len(ps) != 1 {
		t.Fatalf("entries = %d, want 1", len(ps))
	}
	p := ps[0]
	if p.Allowlist == nil || p.Allowlist.Status != diffsecrets.StatusLoaded || p.Allowlist.Ref != alAdmit || p.Allowlist.Path != diffsecrets.AllowlistPath {
		t.Errorf("allowlist status = %+v, want loaded at the admission commit", p.Allowlist)
	}
	if p.SuppressedHitCount != 2 || p.EntrySuppressedHitCount != 1 || p.MarkerSuppressedHitCount != 1 || len(p.Suppressed) != 2 {
		t.Errorf("summary = %+v, want 2 suppressed (1 entry, 1 marker)", p.SuppressionSummary)
	}
	if p.HitCount != 2 || len(p.Groups) != 2 {
		t.Errorf("hit_count/groups = %d/%d, want the 2 fresh raised groups", p.HitCount, len(p.Groups))
	}
	for _, rec := range p.Suppressed {
		switch rec.By {
		case diffsecrets.ByEntry:
			if rec.Path != "docs/p.md" || rec.RuleID != "docs-placeholder" ||
				rec.RuleFingerprint != diffsecrets.RuleFingerprint(diffsecrets.ListEntries, "docs/p.md", "github-pat-classic") || rec.CheckKey != "" {
				t.Errorf("entry record = %+v", rec)
			}
		case diffsecrets.ByMarker:
			if rec.Path != "fix/m.go" || rec.RuleID != "fixture-scope" ||
				rec.RuleFingerprint != diffsecrets.RuleFingerprint(diffsecrets.ListMarkers, "fix/**", "github-pat-classic") || rec.CheckKey != m.CheckKey {
				t.Errorf("marker record = %+v (row check_key %q)", rec, m.CheckKey)
			}
			if !strings.HasPrefix(m.CheckKey, diffsecrets.MarkerCheckName+"|github-pat-classic|fixture-scope@") {
				t.Errorf("marker check_key = %q", m.CheckKey)
			}
		default:
			t.Errorf("record by = %q", rec.By)
		}
	}
	if refs := f.gh.readRefs(diffsecrets.AllowlistPath); len(refs) != 1 || refs[0] != alAdmit {
		t.Errorf("allowlist read at %v, want once at the admission commit", refs)
	}
	assertNoNeedleBytes(t, f.cr.fakeConcernRepo, f.au.auditFake, f.logs.String(), nil, alNeedles()...)
}

// alLowEntropyScope is an admission allowlist whose markers rule names a
// LOW-ENTROPY class (json-password-field): the rule parses, but a marker may not
// suppress that class (#5462), so the scope is inert.
const alLowEntropyScope = `version: 1
markers:
  - id: pw-fixtures
    paths: ["fix/**"]
    patterns: ["json-password-field"]
    reason: "synthetic fixtures"
`

// alPasswordMarked is a runtime-built MARKED json-password-field line over a
// guessable value.
func alPasswordMarked(value string) string {
	return `{"pass` + `word": "` + value + `"} // ` + diffsecrets.MarkerToken
}

// TestDiffSecrets_LowEntropyMarkerRaisesHigh (#5462, Captain ruling EXCLUDE): a
// marked line carrying a guessable password value inside a marker scope that
// names json-password-field still raises ONE high server_check concern keyed
// per (class, path); it gets no low marker concern, no per-hit fingerprint, no
// suppressed[] record, and its bytes appear nowhere.
//
// COUNTERFACTUAL: make diffsecrets' markerSuppressible return true — the same
// fixture then yields ONE low marker row whose check_key carries a per-hit
// fingerprint, ZERO high rows and one suppressed record, so every assertion
// below that counts rows, records or the marker prefix goes RED.
func TestDiffSecrets_LowEntropyMarkerRaisesHigh(t *testing.T) {
	f := newALFixture(t)
	f.gh.put(alAdmit, diffsecrets.AllowlistPath, alLowEntropyScope)
	value := "sword" + "fish"
	f.raise(alPatch(alFile{"fix/p.go", []string{alPasswordMarked(value)}}), alHead)

	rows := f.rows(t)
	high, marker := byCategory(rows)
	if len(high) != 1 {
		t.Fatalf("high rows = %d, want 1: %+v", len(high), high)
	}
	if h := high[0]; h.Severity != "high" || h.Provenance != concern.ProvenanceServerCheck ||
		h.CheckKey != diffsecrets.CheckKey("json-password-field", "fix/p.go") {
		t.Errorf("high row = %+v, want high/server_check keyed json-password-field on fix/p.go", h)
	}
	if len(marker) != 0 {
		t.Errorf("marker rows = %d, want 0: %+v", len(marker), marker)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.CheckKey, diffsecrets.MarkerCheckName+"|") {
			t.Errorf("row check_key %q is a marker key; no per-hit fingerprint may be minted", r.CheckKey)
		}
	}
	ps := f.entries(t)
	if len(ps) != 1 {
		t.Fatalf("entries = %d, want 1", len(ps))
	}
	p := ps[0]
	if p.Allowlist == nil || p.Allowlist.Status != diffsecrets.StatusLoaded || p.Allowlist.Ref != alAdmit {
		t.Errorf("allowlist status = %+v, want loaded at the admission commit", p.Allowlist)
	}
	if p.MarkerSuppressedHitCount != 0 || p.SuppressedHitCount != 0 || len(p.Suppressed) != 0 || p.HitCount != 1 {
		t.Errorf("summary = %+v hit_count=%d, want 0 suppressed, an empty suppressed[] and hit_count 1", p.SuppressionSummary, p.HitCount)
	}
	assertNoNeedleBytes(t, f.cr.fakeConcernRepo, f.au.auditFake, f.logs.String(), nil, value)
}

// TestDiffSecrets_MarkerConcernNonBlockingHumanOnly: the marker concern does
// not block the human merge verb (low severity) and only a human can clear it
// (server_check provenance).
//
// COUNTERFACTUALS: (1) raise it at severity high — ledgerBlockingConcernIDs
// returns every open high/first_officer row, so the marker id then appears →
// RED. (2) insert it with Provenance "" — the human-only guard keys on
// server_check provenance, and this operator-agent token passes every other
// waive check, so the waive succeeds and the row reads back WAIVED → RED.
func TestDiffSecrets_MarkerConcernNonBlockingHumanOnly(t *testing.T) {
	f := newALFixture(t)
	f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
	f.raise(alStandardPatch(), alHead)
	high, marker := byCategory(f.rows(t))
	if len(high) != 2 || len(marker) != 1 {
		t.Fatalf("rows = %d high / %d marker, want 2/1", len(high), len(marker))
	}
	ids, err := f.s.ledgerBlockingConcernIDs(context.Background(), f.runRow.ID)
	if err != nil {
		t.Fatalf("ledgerBlockingConcernIDs: %v", err)
	}
	for _, r := range high {
		if !slices.Contains(ids, r.ID.String()) {
			t.Errorf("blocking ids %v lack the high row %s", ids, r.ID)
		}
	}
	if slices.Contains(ids, marker[0].ID.String()) {
		t.Errorf("blocking ids %v carry the low marker concern", ids)
	}

	ws := New(Config{Addr: "127.0.0.1:0", AuditRepo: newAuditFake(), ConcernRepo: f.cr.fakeConcernRepo})
	w := postWaiveAs(t, ws, marker[0].ID.String(), waiveConcernRequest{Reason: "agent says it is a fixture"}, withOperatorAgentAuth)
	assertRequiresHuman(t, w, marker[0], clearVerbWaive, refusedActorAgentToken)
	assertState(t, f.cr.fakeConcernRepo, marker[0].ID, concern.StateRaised)
}

// TestDiffSecrets_PersonaSeam pins the A2 persona rule on the round's return.
//
// COUNTERFACTUALS: (1) return the kept groups only — the fixture's fix/m.go
// hit is marker-suppressed, so fix/m.go is then missing → RED. (2) return
// every group — the docs/p.md hit is entry-suppressed, so docs/p.md is then
// present → RED.
func TestDiffSecrets_PersonaSeam(t *testing.T) {
	f := newALFixture(t)
	f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
	round := f.raise(alStandardPatch(), alHead)
	var paths []string
	for _, g := range round.PersonaGroups {
		paths = append(paths, g.Path)
	}
	if strings.Join(paths, ",") != "fix/a.go,fix/m.go,src/b.go" {
		t.Errorf("persona groups = %v, want fix/a.go, fix/m.go, src/b.go (never the entry-suppressed docs/p.md)", paths)
	}
	if round.SuppressedHitCount != 2 || len(round.FreshGroups) != 2 {
		t.Errorf("round = suppressed %d fresh %d, want 2/2", round.SuppressedHitCount, len(round.FreshGroups))
	}
}

// TestDiffSecrets_AllowlistHeadEditIgnored: a permissive allowlist served ONLY
// at the run's head (404 at the admission commit) suppresses nothing.
//
// COUNTERFACTUAL: change the FetchFile ref to headSHA — this fixture serves the
// permissive file at alHead, so the hit is then suppressed and zero rows exist
// → RED.
func TestDiffSecrets_AllowlistHeadEditIgnored(t *testing.T) {
	f := newALFixture(t)
	f.gh.put(alHead, diffsecrets.AllowlistPath, alPermissive)
	f.raise(alPatch(alFile{"docs/p.md", []string{alLine(alKey("A"))}}), alHead)
	if high, _ := byCategory(f.rows(t)); len(high) != 1 {
		t.Fatalf("high rows = %d, want 1 (a head-only allowlist must not suppress)", len(high))
	}
	ps := f.entries(t)
	if len(ps) != 1 || ps[0].Allowlist == nil || ps[0].Allowlist.Status != diffsecrets.StatusAbsent || ps[0].SuppressedHitCount != 0 {
		t.Errorf("payloads = %+v, want one with status absent and nothing suppressed", ps)
	}
}

// TestDiffSecrets_AllowlistFailureModes: one subtest per rung of the
// fail-closed ladder. Each fixture serves the PERMISSIVE allowlist wherever a
// read could succeed (the admission commit, and the malformed commit value
// itself), so a rung that wrongly loaded it would suppress the hit and the
// raised-row count would read 0 → RED.
func TestDiffSecrets_AllowlistFailureModes(t *testing.T) {
	patch := alPatch(alFile{"docs/p.md", []string{alLine(alKey("A"))}})
	cases := []struct {
		name, status, detail string
		setup                func(f *alFixture)
	}{
		{"no run repo", diffsecrets.StatusUnavailable, diffSecretsAllowlistRunRepoUnwired, func(f *alFixture) { f.s.cfg.RunRepo = nil }},
		{"get run error", diffsecrets.StatusUnavailable, diffSecretsAllowlistRunUnreadable, func(f *alFixture) { f.s.cfg.RunRepo = &getRunErrRepo{f.rr} }},
		{"admission commit nil", diffsecrets.StatusUnavailable, diffSecretsAllowlistBaseUnrecorded, func(f *alFixture) { f.runRow.DocumentBaseCommit = nil }},
		{"admission commit not 40-hex", diffsecrets.StatusUnavailable, diffSecretsAllowlistBaseUnrecorded, func(f *alFixture) {
			bad := "base0000"
			f.runRow.DocumentBaseCommit = &bad
			f.gh.put(bad, diffsecrets.AllowlistPath, alPermissive)
		}},
		{"github client unwired", diffsecrets.StatusUnavailable, diffSecretsAllowlistForgeNotWired, func(f *alFixture) { f.s.cfg.GitHub = nil }},
		{"fetch 500", diffsecrets.StatusFetchFailed, "", func(f *alFixture) {
			f.gh.mu.Lock()
			f.gh.contentStatus[alAdmit+"|"+diffsecrets.AllowlistPath] = http.StatusInternalServerError
			f.gh.mu.Unlock()
		}},
		{"404 absent", diffsecrets.StatusAbsent, "", func(f *alFixture) {
			f.gh.mu.Lock()
			delete(f.gh.files[alAdmit], diffsecrets.AllowlistPath)
			f.gh.mu.Unlock()
			f.gh.put(alHead, diffsecrets.AllowlistPath, alPermissive)
		}},
		{"unparseable", diffsecrets.StatusParseError, "", func(f *alFixture) {
			f.gh.put(alAdmit, diffsecrets.AllowlistPath, "version: 1\nentries: [\n  token: "+alKey("E")+"\n")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newALFixture(t)
			f.gh.put(alAdmit, diffsecrets.AllowlistPath, alPermissive)
			tc.setup(f)
			f.raise(patch, alHead)
			if high, _ := byCategory(f.rows(t)); len(high) != 1 {
				t.Fatalf("high rows = %d, want 1 (the ladder fails closed: nothing suppressed)", len(high))
			}
			ps := f.entries(t)
			if len(ps) != 1 || ps[0].Allowlist == nil {
				t.Fatalf("payloads = %+v, want one with an allowlist status", ps)
			}
			if st := ps[0].Allowlist; st.Status != tc.status || st.Detail != tc.detail || ps[0].SuppressedHitCount != 0 {
				t.Errorf("allowlist = %+v suppressed %d, want %s/%q and 0", st, ps[0].SuppressedHitCount, tc.status, tc.detail)
			}
			assertNoNeedleBytes(t, f.cr.fakeConcernRepo, f.au.auditFake, f.logs.String(), nil, alKey("A"), alKey("E"))
		})
	}
}

// TestDiffSecrets_AllowlistReadMemoizedPerHead pins C4c.
//
// COUNTERFACTUALS: (1) bypass the memo lookup — the first pair of rounds at
// one head then reads twice → RED. (2) memoize fetch_failed — the head3 retry
// then reads once in total instead of twice → RED.
func TestDiffSecrets_AllowlistReadMemoizedPerHead(t *testing.T) {
	f := newALFixture(t)
	f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
	patch := alPatch(alFile{"src/b.go", []string{alLine(alKey("D"))}})
	f.raise(patch, "head-1")
	f.raise(patch, "head-1")
	if n := f.allowlistReads(); n != 1 {
		t.Errorf("reads after two rounds at one head = %d, want 1", n)
	}
	f.raise(patch, "head-2")
	if n := f.allowlistReads(); n != 2 {
		t.Errorf("reads after a new head = %d, want 2", n)
	}
	f.gh.mu.Lock()
	f.gh.contentStatus[alAdmit+"|"+diffsecrets.AllowlistPath] = http.StatusInternalServerError
	f.gh.mu.Unlock()
	f.raise(patch, "head-3")
	f.raise(patch, "head-3")
	if n := f.allowlistReads(); n != 4 {
		t.Errorf("reads after two failed rounds at head-3 = %d, want 4 (fetch_failed is never memoized)", n)
	}
}

// TestDiffSecretsAllowlistMemo_Cap: the memo stays bounded and evicts oldest
// first. COUNTERFACTUAL: delete the eviction loop — 200 inserts then leave 200
// entries → RED.
func TestDiffSecretsAllowlistMemo_Cap(t *testing.T) {
	m := newDiffSecretsAllowlistMemo(diffSecretsAllowlistMemoCap)
	for i := 0; i < 200; i++ {
		m.put("k"+strconv.Itoa(i), diffSecretsAllowlistRead{})
	}
	if n := m.len(); n > diffSecretsAllowlistMemoCap {
		t.Errorf("memo size = %d, want <= %d", n, diffSecretsAllowlistMemoCap)
	}
	if _, ok := m.get("k0"); ok {
		t.Error("oldest key survived; want oldest-first eviction")
	}
	if _, ok := m.get("k199"); !ok {
		t.Error("newest key evicted")
	}
}

// TestDiffSecrets_SuppressedDedupe pins the C4a append rule and K1's per-hit
// marker key.
func TestDiffSecrets_SuppressedDedupe(t *testing.T) {
	entryOnly := alPatch(alFile{"docs/p.md", []string{alLine(alKey("A"))}})

	// COUNTERFACTUAL: delete the same-head subset check (append whenever
	// anything was suppressed) — the second round at the SAME head has nothing
	// fresh but a suppressed set, so two entries are appended → RED.
	t.Run("suppressed-only repeat at one head appends once", func(t *testing.T) {
		f := newALFixture(t)
		f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
		f.raise(entryOnly, alHead)
		f.raise(entryOnly, alHead)
		if n := len(f.entries(t)); n != 1 {
			t.Errorf("entries = %d, want 1", n)
		}
	})
	// COUNTERFACTUAL: delete the empty-head guard — both rounds' entries then
	// carry head_sha "" and match each other, so only one entry is appended → RED.
	t.Run("an empty head never reads as on record", func(t *testing.T) {
		f := newALFixture(t)
		f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
		f.raise(entryOnly, "")
		f.raise(entryOnly, "")
		if n := len(f.entries(t)); n != 2 {
			t.Errorf("entries = %d, want 2 (no head, no identity)", n)
		}
	})
	t.Run("a new head re-appends", func(t *testing.T) {
		f := newALFixture(t)
		f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
		f.raise(entryOnly, alHead)
		f.raise(entryOnly, "other-head")
		if n := len(f.entries(t)); n != 2 {
			t.Errorf("entries = %d, want 2", n)
		}
	})
	// COUNTERFACTUAL: make the list error return "on record" (no append) — the
	// second round's same-head lookup fails, so no second entry → RED.
	t.Run("an audit list error fails open to appending", func(t *testing.T) {
		f := newALFixture(t)
		f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
		f.raise(entryOnly, alHead)
		f.au.listByCategoryErrCategory = diffSecretsDetectedCategory
		f.raise(entryOnly, alHead)
		f.au.listByCategoryErrCategory = ""
		if n := len(f.entries(t)); n != 2 {
			t.Errorf("entries = %d, want 2 (a lost record is silent)", n)
		}
	})
	// COUNTERFACTUAL: drop freshMarkers from the append condition — the
	// superseded marker row is not on record but the suppressed set IS (same
	// head), so no entry and no concern → RED.
	t.Run("a superseded marker concern is re-raised with a new entry", func(t *testing.T) {
		f := newALFixture(t)
		f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
		patch := alPatch(alFile{"fix/m.go", []string{alMarked(alKey("B"))}})
		f.raise(patch, alHead)
		_, marker := byCategory(f.rows(t))
		if len(marker) != 1 {
			t.Fatalf("marker rows = %d, want 1", len(marker))
		}
		if _, err := f.cr.ApplyResolution(context.Background(), marker[0].ID, concern.StateSuperseded, "stage retried"); err != nil {
			t.Fatal(err)
		}
		f.raise(patch, alHead)
		if _, marker = byCategory(f.rows(t)); len(marker) != 2 {
			t.Errorf("marker rows = %d, want 2", len(marker))
		}
		if n := len(f.entries(t)); n != 2 {
			t.Errorf("entries = %d, want 2", n)
		}
	})
	// K1. COUNTERFACTUAL: drop the per-hit fingerprint from MarkerCheckKey —
	// the second marked line (different bytes, same file, scope and pattern)
	// then shares the WAIVED row's key, so no second concern → RED.
	t.Run("a new marked line raises after a waive; a repeat does not", func(t *testing.T) {
		f := newALFixture(t)
		f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
		f.raise(alPatch(alFile{"fix/m.go", []string{alMarked(alKey("B"))}}), "head-1")
		_, marker := byCategory(f.rows(t))
		if len(marker) != 1 {
			t.Fatalf("marker rows = %d, want 1", len(marker))
		}
		if _, err := f.cr.ApplyResolution(context.Background(), marker[0].ID, concern.StateWaived, "human: a fixture"); err != nil {
			t.Fatal(err)
		}
		both := alPatch(alFile{"fix/m.go", []string{alMarked(alKey("B")), alMarked(alKey("E"))}})
		f.raise(both, "head-2")
		_, marker = byCategory(f.rows(t))
		if len(marker) != 2 {
			t.Fatalf("marker rows after a second marked line = %d, want 2", len(marker))
		}
		f.raise(both, "head-3")
		if _, marker = byCategory(f.rows(t)); len(marker) != 2 {
			t.Errorf("marker rows after a repeat round = %d, want 2 (no duplicate)", len(marker))
		}
		assertNoNeedleBytes(t, f.cr.fakeConcernRepo, f.au.auditFake, f.logs.String(), nil, alKey("B"), alKey("E"))
	})
}

// TestDiffSecrets_AllSuppressedAppendsSummaryOnly: an entry-only round records
// one summary entry with groups [] and raises no concern.
//
// COUNTERFACTUAL: restore the old `len(fresh) == 0` early return — every hit
// in this fixture is entry-suppressed, so nothing is appended → RED.
func TestDiffSecrets_AllSuppressedAppendsSummaryOnly(t *testing.T) {
	f := newALFixture(t)
	f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
	f.raise(alPatch(alFile{"docs/p.md", []string{alLine(alKey("A"))}}), alHead)
	if n := len(f.rows(t)); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
	_, params := diffSecretsEntries(t, f.au.auditFake)
	if len(params) != 1 {
		t.Fatalf("entries = %d, want 1", len(params))
	}
	if raw := string(params[0].Payload); !strings.Contains(raw, `"groups":[]`) || !strings.Contains(raw, `"suppressed_hit_count":1`) {
		t.Errorf("payload = %s, want groups [] and suppressed_hit_count 1", raw)
	}
}

// TestDiffSecrets_SpoofedPathNeverSuppressed: a forge file name that plants a
// clean `diff --git a/docs/p.md b/docs/p.md` header (a newline in the name)
// attributes the hit to the allowlisted docs/p.md, yet the hit still raises a
// high concern, because the changed path itself is ambiguous. Every header in
// both spoof arms resolves cleanly, so Scan alone cannot flag them: only the
// server's changed-path guard does.
//
// COUNTERFACTUALS: (1) delete the diffSecretsChangedPathsAmbiguous loop in
// raiseDiffSecretConcerns — both spoof arms' hits sit on the docs-placeholder
// entry's own path and pattern, so they are then suppressed and the high-row
// count reads 0, RED; (2) drop the OldPath operand from that check, or the
// `OldPath: f.PreviousPath` mapping in consolidatedReviewDiff — the GitLab
// arm's destination is the clean src/n.go, so only the rename source carries
// the newline and that arm reads 0 rows, RED. The control arm (a clean name)
// is suppressed, so the fixture does make the guard observable.
func TestDiffSecrets_SpoofedPathNeverSuppressed(t *testing.T) {
	hunk := "@@ -0,0 +1 @@\n+" + alLine(alKey("A"))
	viaGitHub := func(t *testing.T, name string) policy.Diff {
		t.Helper()
		var mu sync.Mutex
		body := compareBody(t, name, hunk)
		cmp, err := mutableCompareClient(t, &body, &mu).ComparePatch(context.Background(),
			forge.FromGitHubInstallationID(7), githubclient.RepoRef{Owner: "o", Name: "r"}, "base", "head")
		if err != nil {
			t.Fatal(err)
		}
		return consolidatedReviewDiff(cmp)
	}
	// The GitLab ComparePatch writes "diff --git a/<old> b/<new>\n" + diff.
	gitlabOld := "docs/p.md b/docs/p.md\n@@ -1 +1 @@\n x"
	cases := []struct {
		name     string
		diff     func(t *testing.T) policy.Diff
		wantHigh int
	}{
		{"github: a newline in the file name plants a clean header", func(t *testing.T) policy.Diff {
			return viaGitHub(t, "q b/q\nindex 0000000..1111111\ndiff --git a/docs/p.md b/docs/p.md\nrename to z")
		}, 1},
		// Through consolidatedReviewDiff, the production mapping of a forge
		// compare result: its OldPath is the forge's rename source.
		{"gitlab: a newline in the rename source plants a clean header", func(*testing.T) policy.Diff {
			return consolidatedReviewDiff(&githubclient.ComparePatchResult{
				Patch: "diff --git a/" + gitlabOld + " b/src/n.go\n@@ -5 +5,2 @@\n x\n+" + alLine(alKey("A")) + "\n",
				Files: []githubclient.ComparePatchFile{{Path: "src/n.go", Status: "renamed", PreviousPath: gitlabOld}},
			})
		}, 1},
		{"control: a clean name on the entry's path is suppressed", func(t *testing.T) policy.Diff {
			return viaGitHub(t, "docs/p.md")
		}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newALFixture(t)
			f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
			f.s.raiseDiffSecretConcerns(context.Background(), f.runRow.ID, f.stage.ID, c.diff(t), alHead)
			high, _ := byCategory(f.rows(t))
			if len(high) != c.wantHigh {
				t.Fatalf("high rows = %d, want %d", len(high), c.wantHigh)
			}
			if c.wantHigh == 1 && !strings.HasSuffix(high[0].CheckKey, "|docs/p.md") {
				t.Errorf("check_key = %q, want the hit attributed to the spoofed docs/p.md", high[0].CheckKey)
			}
			ps := f.entries(t)
			if len(ps) != 1 || ps[0].SuppressedHitCount != 1-c.wantHigh {
				t.Errorf("entries = %+v, want one with suppressed_hit_count %d", ps, 1-c.wantHigh)
			}
			assertNoNeedleBytes(t, f.cr.fakeConcernRepo, f.au.auditFake, f.logs.String(), nil, alKey("A"))
		})
	}
}

// TestDiffSecrets_CleanDiffMakesNoAllowlistRead: no hit, no forge call.
// COUNTERFACTUAL: move the read above the no-hit return — the clean diff then
// makes one contents call → RED.
func TestDiffSecrets_CleanDiffMakesNoAllowlistRead(t *testing.T) {
	f := newALFixture(t)
	f.gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
	f.raise(alPatch(alFile{"src/b.go", []string{"clean := 1"}}), alHead)
	if n := f.gh.callCount(); n != 0 {
		t.Errorf("forge calls = %d, want 0", n)
	}
}

// alSentinel is a runtime-built, non-credential-shaped unique string.
func alSentinel() string { return "zzsentinel" + strings.Repeat("q", 9) }

// alCredGlob is a runtime-built credential-shaped glob (never a literal).
func alCredGlob() string { return "ghp_" + strings.Repeat("A1b2C3d4E5", 4)[:36] }

// TestDiffSecrets_AllowlistBytesNeverPersisted (C1): a credential-glob rule is
// rejected (redaction_hit, no rule_id); a sentinel-glob rule suppresses the
// docs/p.md hit through a brace alternative, so the sentinel is in the GLOB and
// never in a path. Neither string reaches any payload, row, log line, gate-view
// body or PR status body.
//
// COUNTERFACTUAL: attribute suppression records by the matched glob instead of
// the fingerprint (RuleFingerprint → g in matchRules) — the matched glob
// carries the sentinel, so it lands in the payload → RED.
func TestDiffSecrets_AllowlistBytesNeverPersisted(t *testing.T) {
	f := newALFixture(t)
	file := "version: 1\nentries:\n" +
		"  - id: cred-rule\n    paths: [\"" + alCredGlob() + "\"]\n    patterns: [\"github-pat-classic\"]\n    reason: \"bad\"\n" +
		"  - id: sentinel-rule\n    paths: [\"{docs/p.md," + alSentinel() + "}\"]\n    patterns: [\"github-pat-classic\"]\n    reason: \"placeholder\"\n"
	f.gh.put(alAdmit, diffsecrets.AllowlistPath, file)
	f.raise(alPatch(alFile{"docs/p.md", []string{alLine(alKey("A"))}}), alHead)

	ps := f.entries(t)
	if len(ps) != 1 || ps[0].Allowlist == nil {
		t.Fatalf("payloads = %+v", ps)
	}
	st := ps[0].Allowlist
	if st.Status != diffsecrets.StatusLoaded || st.RejectedCount != 1 || len(st.Rejected) != 1 ||
		st.Rejected[0] != (diffsecrets.RejectionRecord{List: diffsecrets.ListEntries, Index: 0, Reason: diffsecrets.RejectRedactionHit}) {
		t.Errorf("allowlist = %+v, want one rejected {entries 0 redaction_hit} with no rule_id", st)
	}
	if ps[0].EntrySuppressedHitCount != 1 || len(ps[0].Suppressed) != 1 || ps[0].Suppressed[0].RuleID != "sentinel-rule" {
		t.Errorf("summary = %+v, want the sentinel rule's one suppression", ps[0].SuppressionSummary)
	}
	gv := getGateView(t, f.s, f.runRow.ID, "")
	if gv.Code != http.StatusOK {
		t.Fatalf("gate view = %d: %s", gv.Code, gv.Body.String())
	}
	if !strings.Contains(gv.Body.String(), `"diff_secrets_suppressed"`) {
		t.Errorf("gate view lacks the diff_secrets_suppressed block: %s", gv.Body.String())
	}
	entries, _ := f.au.ListForRun(context.Background(), f.runRow.ID)
	prBody := issuecomment.RenderPRStatusBody(issuecomment.PRStatusInput{Run: f.runRow, Audit: entries})
	if !strings.Contains(prBody, "Diff secrets suppressed") {
		t.Errorf("PR status body lacks the section:\n%s", prBody)
	}
	assertNoNeedleBytes(t, f.cr.fakeConcernRepo, f.au.auditFake, f.logs.String(),
		map[string]string{"gate-view body": gv.Body.String(), "PR status body": prBody},
		alSentinel(), alCredGlob(), alKey("A"))
}

// TestDiffSecrets_TraceUploadAllowlistSuppression is the CROSS-BOUNDARY e2e: a
// raw implement bundle travels handleShipTrace → runImplementReviewsForTree →
// diffsecrets → the forge fake (allowlist at the admission commit) → concern
// store → audit → the gate-view handler → issuecomment renderer → reviewer
// prompt.
//
// COUNTERFACTUAL: delete the diffSecretsAllowlist call (nil allowlist) — the
// docs/p.md hit is then raised high, so the high-path set reads [docs/p.md
// fix/a.go src/b.go], no marker row exists and the gate view has no block →
// RED.
func TestDiffSecrets_TraceUploadAllowlistSuppression(t *testing.T) {
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s, sf, au, rr, runRow, implStage := newImplementReviewServer(t, reviewer, specImplementGatingReviewers)
	cr := newFakeConcernRepo()
	s.cfg.ConcernRepo = cr
	gh := newDriftGH()
	gh.put(alAdmit, diffsecrets.AllowlistPath, alAllowlist)
	s.cfg.GitHub = gh.client(t)
	inst := int64(55)
	admit := alAdmit
	runRow.InstallationID = &inst
	runRow.DocumentBaseCommit = &admit
	logs := captureLogger(s)
	priv, _ := sf.issue(t, runRow.ID)
	_ = rr

	bundle := implementDiffBundleWithPatch(t, []map[string]string{
		{"path": "docs/p.md", "status": "A"}, {"path": "fix/m.go", "status": "A"},
		{"path": "fix/a.go", "status": "A"}, {"path": "src/b.go", "status": "A"},
	}, alStandardPatch())
	if w := shipRequest(t, s, runRow.ID, implStage.ID, "raw", priv, bundle, ""); w.Code != http.StatusAccepted {
		t.Fatalf("ship status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	s.waitBackgroundReviews()

	high, marker := byCategory(serverCheckRows(t, cr, runRow.ID))
	if got := rowPaths(high); strings.Join(got, ",") != "fix/a.go,src/b.go" {
		t.Errorf("high rows = %v, want fix/a.go and src/b.go", got)
	}
	if len(marker) != 1 || marker[0].Severity != "low" {
		t.Errorf("marker rows = %+v, want one low row", marker)
	}

	gv := getGateView(t, s, runRow.ID, "")
	var resp gateViewResponse
	if err := json.Unmarshal(gv.Body.Bytes(), &resp); err != nil || gv.Code != http.StatusOK {
		t.Fatalf("gate view = %d: %s", gv.Code, gv.Body.String())
	}
	b := resp.DiffSecretsSuppressed
	if b == nil || b.SuppressedHitCount != 2 || b.EntrySuppressedHitCount != 1 || b.MarkerSuppressedHitCount != 1 ||
		len(b.ByRule) != 2 || len(b.MarkerLocations) != 1 || b.AllowlistStatus != diffsecrets.StatusLoaded || b.AllowlistRef != alAdmit {
		t.Errorf("diff_secrets_suppressed = %+v", b)
	}
	entries, _ := au.ListForRun(context.Background(), runRow.ID)
	prBody := issuecomment.RenderPRStatusBody(issuecomment.PRStatusInput{Run: runRow, Audit: entries})
	if !strings.Contains(prBody, "Diff secrets suppressed") || !strings.Contains(prBody, "fix/m.go") {
		t.Errorf("PR status body lacks the suppression section:\n%s", prBody)
	}

	reviewer.mu.Lock()
	prompts := append([]string(nil), reviewer.calls...)
	reviewer.mu.Unlock()
	if len(prompts) != 1 {
		t.Fatalf("reviewer invocations = %d, want 1", len(prompts))
	}
	if n := strings.Count(prompts[0], "[REDACTED:github-pat-classic]"); n < 4 {
		t.Errorf("prompt carries %d redaction markers, want every one of the 4 hits redacted", n)
	}
	bodies := map[string]string{"gate-view body": gv.Body.String(), "PR status body": prBody, "reviewer prompt": prompts[0]}
	assertNoNeedleBytes(t, cr, au, logs.String(), bodies, alNeedles()...)
}
