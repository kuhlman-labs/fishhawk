package mcpe2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// lcGateConcern is the subset of a fishhawk_get_gate_view concern this test
// reads: its state and the judgement record a relabel writes.
type lcGateConcern struct {
	ID                     uuid.UUID `json:"id"`
	State                  string    `json:"state"`
	Outcome                string    `json:"outcome"`
	Judgement              string    `json:"judgement"`
	JudgementInheritedFrom string    `json:"judgement_inherited_from"`
	DuplicateOf            string    `json:"duplicate_of"`
	JudgementActorKind     string    `json:"judgement_actor_kind"`
	JudgementBackfill      bool      `json:"judgement_backfill"`
}

// lcGateView reads every concern of runID through the real fishhawk-mcp
// binary's fishhawk_get_gate_view, open and settled merged by id.
func lcGateView(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) map[uuid.UUID]lcGateConcern {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_gate_view",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_gate_view: %v", err)
	}
	if res.IsError {
		t.Fatalf("fishhawk_get_gate_view returned a tool error: %s", toolContentString(t, res))
	}
	var out struct {
		GateView struct {
			Open    []lcGateConcern `json:"open"`
			Settled []lcGateConcern `json:"settled"`
		} `json:"gate_view"`
	}
	decodeStructured(t, res, &out)
	all := map[uuid.UUID]lcGateConcern{}
	for _, c := range append(out.GateView.Open, out.GateView.Settled...) {
		all[c.ID] = c
	}
	return all
}

// TestE2E_LabelConcern_RelabelDuplicateAndChains is the E94.8 / #4841
// (ADR-117 D1 phase 3) cross-boundary seam: fishhawk_label_concern's input ->
// the fishhawk-mcp binary's client body -> the REST handler's human-only,
// exclusivity and duplicate_of guards -> the REAL Postgres concern store ->
// the fishhawk_get_gate_view and fishhawk_list_audit read surfaces. The run is
// in a non-Fishhawk repository (ADR-110).
//
// It covers a judgement relabel, a duplicate that inherits and keeps
// inheriting after its root is relabelled, a chain C->B->A, a human waive on a
// duplicate that still reads the root's judgement (approval condition M2), the
// cross-run and missing-target refusals that read IDENTICALLY and never name
// the foreign run (M1), the not-earlier and self refusals with the row
// unchanged, a backfill import on a legacy-shaped waived row, the
// concern_labelled rows on the audit chain, and a cycle and a dangling link
// seeded by direct SQL that read unlabelled.
func TestE2E_LabelConcern_RelabelDuplicateAndChains(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	concernRepo := concern.NewPostgresRepository(fx.pool)
	auditRepo := audit.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signing.NewPostgresRepository(fx.pool),
		ArtifactRepo: artifact.NewPostgresRepository(fx.pool),
		ConcernRepo:  concernRepo,
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	newRun := func(sha string) (uuid.UUID, uuid.UUID) {
		t.Helper()
		requiresCharter := false
		rn, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo:            "acme/widgets",
			WorkflowID:      "feature_change",
			WorkflowSHA:     sha,
			TriggerSource:   runpkg.TriggerCLI,
			RequiresCharter: &requiresCharter,
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID:        rn.ID,
			Sequence:     1,
			Type:         runpkg.StageTypeImplement,
			ExecutorKind: runpkg.ExecutorAgent,
			ExecutorRef:  "fishhawk/runner@v1",
		})
		if err != nil {
			t.Fatalf("CreateStage: %v", err)
		}
		return rn.ID, stage.ID
	}
	// raise inserts ONE concern per call at its own origin sequence, so the
	// run ledger order (origin_review_sequence first) is deterministic.
	raise := func(runID, stageID uuid.UUID, seq int64, note string) uuid.UUID {
		t.Helper()
		rows, err := concernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
			RunID: runID, StageID: stageID, StageKind: concern.StageKindImplement,
			ReviewerModel: "gpt-6-astra", OriginReviewSequence: seq,
			Concerns: []concern.RaisedConcern{{Severity: "medium", Category: "correctness", Note: note}},
		})
		if err != nil {
			t.Fatalf("InsertRaised %s: %v", note, err)
		}
		return rows[0].ID
	}

	runID, stageID := newRun("sha-4841-e2e")
	foreignRunID, foreignStageID := newRun("sha-4841-e2e-foreign")
	a := raise(runID, stageID, 1, "A: the root")
	b := raise(runID, stageID, 2, "B: duplicates A")
	c := raise(runID, stageID, 3, "C: duplicates B")
	d := raise(runID, stageID, 4, "D: refusal target")
	e := raise(runID, stageID, 5, "E: legacy waived row")
	f := raise(runID, stageID, 6, "F: cycle with G")
	g := raise(runID, stageID, 7, "G: cycle with F")
	h := raise(runID, stageID, 8, "H: dangling link")
	// The foreign concern sits at a LOWER sequence than D, so the ordering
	// rule alone would ACCEPT the link: only the same-run check refuses it.
	x := raise(foreignRunID, foreignStageID, 1, "X: another run's concern")

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	label := func(args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_label_concern", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool fishhawk_label_concern %v: %v", args, err)
		}
		return res
	}
	mustLabel := func(args map[string]any) {
		t.Helper()
		if res := label(args); res.IsError {
			t.Fatalf("fishhawk_label_concern %v returned a tool error: %s", args, toolContentString(t, res))
		}
	}
	gateConcern := func(id uuid.UUID) lcGateConcern {
		t.Helper()
		gc, ok := lcGateView(t, ctx, session, runID)[id]
		if !ok {
			t.Fatalf("concern %s missing from the gate view", id)
		}
		return gc
	}

	// 1. A judgement label, read back with its actor kind.
	mustLabel(map[string]any{"concern_id": a.String(), "judgement": "defect"})
	if got := gateConcern(a); got.Judgement != "defect" || got.JudgementActorKind != "user" || got.JudgementInheritedFrom != "" || got.State != "raised" {
		t.Errorf("A after its label = %+v, want defect by user, still raised", got)
	}

	// 2. B duplicate_of A inherits A's judgement; C duplicate_of B resolves
	// through the chain to A.
	mustLabel(map[string]any{"concern_id": b.String(), "duplicate_of": a.String()})
	mustLabel(map[string]any{"concern_id": c.String(), "duplicate_of": b.String()})
	gv := lcGateView(t, ctx, session, runID)
	for id, wantDup := range map[uuid.UUID]uuid.UUID{b: a, c: b} {
		got := gv[id]
		if got.Judgement != "defect" || got.JudgementInheritedFrom != a.String() || got.DuplicateOf != wantDup.String() ||
			got.Outcome != "duplicate_of:"+wantDup.String() || got.State != "raised" {
			t.Errorf("concern %s after the duplicate labels = %+v, want defect inherited from A %s, duplicate_of %s", id, got, a, wantDup)
		}
	}

	// 3. Relabelling the root re-resolves the whole chain.
	mustLabel(map[string]any{"concern_id": a.String(), "judgement": "no_defect"})
	gv = lcGateView(t, ctx, session, runID)
	for _, id := range []uuid.UUID{a, b, c} {
		if got := gv[id]; got.Judgement != "no_defect" {
			t.Errorf("concern %s after the root relabel = %+v, want no_defect", id, got)
		}
	}

	// 4. M2: a human waive on a duplicate records its own judgement on the
	// waive's audit row, but the EFFECTIVE judgement stays the root's.
	waive, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_waive_concern", Arguments: map[string]any{
		"concern_id": b.String(), "reason": "covered by A", "judgement": "preference",
	}})
	if err != nil {
		t.Fatalf("CallTool fishhawk_waive_concern: %v", err)
	}
	if waive.IsError {
		t.Fatalf("fishhawk_waive_concern on the duplicate returned a tool error: %s", toolContentString(t, waive))
	}
	if got := gateConcern(b); got.State != "waived" || got.Judgement != "no_defect" || got.JudgementInheritedFrom != a.String() {
		t.Errorf("B after a human waive = %+v, want waived but still inheriting no_defect from A", got)
	}

	// 5. Refusals. Each is a tool error naming its rule, and D/A read back
	// unchanged afterwards.
	refuse := func(name string, args map[string]any, wantRule string) string {
		t.Helper()
		res := label(args)
		text := toolContentString(t, res)
		if !res.IsError || !strings.Contains(text, "validation_failed") || !strings.Contains(text, wantRule) {
			t.Errorf("%s: result = %s (error %v), want a validation_failed tool error naming %s", name, text, res.IsError, wantRule)
		}
		return text
	}
	crossRun := refuse("cross-run target", map[string]any{"concern_id": d.String(), "duplicate_of": x.String()}, concern.JudgementRuleDuplicateOfNotFound)
	missing := refuse("missing target", map[string]any{"concern_id": d.String(), "duplicate_of": uuid.NewString()}, concern.JudgementRuleDuplicateOfNotFound)
	refuse("later target", map[string]any{"concern_id": a.String(), "duplicate_of": d.String()}, concern.JudgementRuleDuplicateOfNotEarlier)
	refuse("self target", map[string]any{"concern_id": d.String(), "duplicate_of": d.String()}, concern.JudgementRuleDuplicateOfNotEarlier)
	// M1: the cross-run refusal never names the foreign run, and is
	// byte-identical to the missing-target refusal.
	if strings.Contains(crossRun, foreignRunID.String()) {
		t.Errorf("cross-run refusal names the foreign run %s:\n%s", foreignRunID, crossRun)
	}
	if crossRun != missing {
		t.Errorf("cross-run and missing-target refusals differ (M1 requires them identical):\ncross-run: %s\nmissing:   %s", crossRun, missing)
	}
	// The same, on the raw REST response body.
	postLabel := func(id uuid.UUID, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpSrv.URL+"/v0/concerns/"+id.String()+"/label", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+fx.operatorTok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST label: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}
	crossStatus, crossBody := postLabel(d, `{"duplicate_of":"`+x.String()+`"}`)
	missStatus, missBody := postLabel(d, `{"duplicate_of":"`+uuid.NewString()+`"}`)
	if crossStatus != http.StatusBadRequest || missStatus != http.StatusBadRequest {
		t.Errorf("REST statuses cross-run=%d missing=%d, want 400 both", crossStatus, missStatus)
	}
	if bytes.Contains(crossBody, []byte(foreignRunID.String())) || bytes.Contains(crossBody, []byte(foreignStageID.String())) {
		t.Errorf("REST cross-run body names the foreign run or stage:\n%s", crossBody)
	}
	var crossErr, missErr struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(crossBody, &crossErr); err != nil {
		t.Fatalf("decode cross-run body: %v\n%s", err, crossBody)
	}
	if err := json.Unmarshal(missBody, &missErr); err != nil {
		t.Fatalf("decode missing body: %v\n%s", err, missBody)
	}
	cj, _ := json.Marshal(crossErr)
	mj, _ := json.Marshal(missErr)
	if !bytes.Equal(cj, mj) || crossErr.Error.Details["run_id"] != runID.String() {
		t.Errorf("REST cross-run error %s != missing error %s, or details.run_id is not the labelled run %s", cj, mj, runID)
	}
	gv = lcGateView(t, ctx, session, runID)
	if got := gv[d]; got.Judgement != "unlabelled" || got.DuplicateOf != "" || got.JudgementActorKind != "" {
		t.Errorf("D after the refusals = %+v, want unlabelled with no duplicate_of", got)
	}
	if got := gv[a]; got.Judgement != "no_defect" || got.DuplicateOf != "" {
		t.Errorf("A after the not-earlier refusal = %+v, want its no_defect label and no duplicate_of", got)
	}

	// 6. Backfill on a legacy-shaped row: waived with an interim reason prefix
	// and no judgement. backfill:true with no judgement derives no_defect.
	if _, err := concernRepo.ApplyResolution(ctx, e, concern.StateWaived, "false_positive: the guard is upstream"); err != nil {
		t.Fatalf("seed legacy waived row: %v", err)
	}
	if got := gateConcern(e); got.Judgement != "unlabelled" {
		t.Fatalf("legacy row before backfill = %+v, want unlabelled", got)
	}
	mustLabel(map[string]any{"concern_id": e.String(), "backfill": true})
	if got := gateConcern(e); got.State != "waived" || got.Judgement != "no_defect" || !got.JudgementBackfill || got.JudgementActorKind != "user" {
		t.Errorf("legacy row after backfill = %+v, want waived, no_defect, judgement_backfill true", got)
	}

	// 7. The audit chain: one concern_labelled row per landed label, none for
	// a refusal, and no corrective concern_label_failed row at all.
	listAudit := func(category string) []map[string]any {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_list_audit",
			Arguments: map[string]any{"run_id": runID.String(), "category": category}})
		if err != nil {
			t.Fatalf("CallTool fishhawk_list_audit: %v", err)
		}
		if res.IsError {
			t.Fatalf("fishhawk_list_audit(%s) returned a tool error: %s", category, toolContentString(t, res))
		}
		var listed struct {
			Items []struct {
				ActorKind string         `json:"actor_kind"`
				Payload   map[string]any `json:"payload"`
			} `json:"items"`
		}
		decodeStructured(t, res, &listed)
		out := make([]map[string]any, 0, len(listed.Items))
		for _, it := range listed.Items {
			if it.ActorKind != "user" && category == server.CategoryConcernLabelled {
				t.Errorf("%s row actor_kind = %q, want user", category, it.ActorKind)
			}
			out = append(out, it.Payload)
		}
		return out
	}
	labelled := listAudit(server.CategoryConcernLabelled)
	type want struct{ concernID, judgement, duplicateOf, prior string }
	wants := []want{
		{a.String(), "defect", "", "unlabelled"},
		{b.String(), "", a.String(), "unlabelled"},
		{c.String(), "", b.String(), "unlabelled"},
		{a.String(), "no_defect", "", "defect"},
		{e.String(), "no_defect", "", "unlabelled"},
	}
	if len(labelled) != len(wants) {
		t.Fatalf("concern_labelled rows = %d, want %d (one per landed label, none per refusal): %v", len(labelled), len(wants), labelled)
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	for i, w := range wants {
		p := labelled[i]
		if str(p, "concern_id") != w.concernID || str(p, "judgement") != w.judgement || str(p, "duplicate_of") != w.duplicateOf || str(p, "prior_judgement") != w.prior {
			t.Errorf("concern_labelled[%d] = %v, want concern %s judgement %q duplicate_of %q prior_judgement %q", i, p, w.concernID, w.judgement, w.duplicateOf, w.prior)
		}
	}
	if bf, _ := labelled[4]["backfill"].(bool); !bf {
		t.Errorf("the backfill concern_labelled row = %v, want backfill true", labelled[4])
	}
	if failed := listAudit(server.CategoryConcernLabelFailed); len(failed) != 0 {
		t.Errorf("concern_label_failed rows = %v, want none (every refusal is a pre-write refusal)", failed)
	}

	// 8. Chains the API cannot write, seeded below it: a cycle F<->G and a
	// dangling link H -> (no row). Each also carries a STORED judgement of its
	// own, so a resolver that fell back to the row's own judgement would show
	// it; every one must read unlabelled with no judgement_inherited_from.
	seed := func(id uuid.UUID, judgement string, dup uuid.UUID) {
		t.Helper()
		if _, err := fx.pool.Exec(ctx, `UPDATE review_concerns SET judgement = $2, duplicate_of = $3 WHERE id = $1`, id, judgement, dup); err != nil {
			t.Fatalf("seed chain on %s: %v", id, err)
		}
	}
	seed(f, "defect", g)
	seed(g, "no_defect", f)
	seed(h, "defect", uuid.New())
	gv = lcGateView(t, ctx, session, runID)
	for _, id := range []uuid.UUID{f, g, h} {
		if got := gv[id]; got.Judgement != "unlabelled" || got.JudgementInheritedFrom != "" {
			t.Errorf("seeded broken chain %s = %+v, want unlabelled with no judgement_inherited_from", id, got)
		}
	}
	// The intact chain is unaffected by the broken ones beside it.
	if got := gv[c]; got.Judgement != "no_defect" || got.JudgementInheritedFrom != a.String() {
		t.Errorf("C beside the broken chains = %+v, want no_defect inherited from A", got)
	}
}
