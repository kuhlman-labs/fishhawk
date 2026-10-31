package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// E93.2 / #4658 slice 2: the run's declared merge method on every server
// merge path. The pre-claim refusals (inline merge_run, the delegated
// may_merge arm, merge-on-pass) fire BEFORE the merge-dispatch claim and write
// nothing; the post-dispatch refusal ForgeMerger attributes is a named 409 /
// note / ejection built from closed-set fields only. The tests drive the
// pgtest-backed mqmWorld (real Postgres queue, run and audit repositories and
// the real merge_dispatch_claims ledger). The pre-claim fixtures carry
// installation_ref gitlab:7 AND the fixture's GitHub installation — an
// artificial shape that lets the run reach every later gate, so the static
// check is the only thing between it and the claim (production GitLab runs
// never reach merge-on-pass's claim).

// mmrSpec is a minimal non-Fishhawk workflow-v2 spec delegating merge at mode
// auto with a verify command and NO review approval gate (so the ADR-112 fold
// never refuses first), declaring method ("" declares none).
func mmrSpec(method string) string {
	decl := ""
	if method != "" {
		decl = "merge_method: " + method + "\n"
	}
	return `version: "2"
` + decl + `workflows:
  feature_change:
    actions:
      merge:
        mode: auto
        when: gates_resolved_ci_green
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
          verify:
            command: "scripts/test verify"
            timeout: "5m"
        produces:
          - artifact: pull_request
`
}

// mmrGitLabRef points r's installation_ref at a GitLab instance while its
// GitHub installation stays the fixture's.
func (w *mqmWorld) mmrGitLabRef(r *mcqRun) {
	w.exec(`UPDATE runs SET installation_ref = 'gitlab:7' WHERE id = $1`, r.id)
}

// mmrUnparseable replaces r's cached admission spec with bytes ParseBytes
// refuses.
func (w *mqmWorld) mmrUnparseable(r *mcqRun) {
	w.exec(`UPDATE runs SET workflow_spec = $1 WHERE id = $2`, fmmUnparseableSpec, r.id)
}

// mmrErrorDetails decodes an error envelope, asserts its code and returns its
// details.
func mmrErrorDetails(t *testing.T, body []byte, code string) map[string]any {
	t.Helper()
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if env.Error.Code != code {
		t.Fatalf("code = %q, want %s: %s", env.Error.Code, code, body)
	}
	return env.Error.Details
}

func mmrKeys(d map[string]any) string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

const (
	mmrPreClaimKeys     = "allowed_merge_methods,merge_method,merge_method_citation,merge_method_source,next_step,pr_url,reason,run_id"
	mmrPostDispatchKeys = "allowed_merge_methods,merge_method,merge_method_citation,merge_method_source,next_step,pr_url,reason,run_id,verdict_sequence"
)

// requireNoForgeText asserts s carries neither half of the sentinel.
func requireNoForgeText(t *testing.T, where, s string) {
	t.Helper()
	for _, bad := range []string{"ghs_SENTINEL", "FORGE-SECRET-TEXT"} {
		if strings.Contains(s, bad) {
			t.Fatalf("%s carries forge text %q: %s", where, bad, s)
		}
	}
}

// TestMergeRunMergeMethod_InlinePreClaimRefusal: a gitlab-family run declaring
// rebase answers 409 merge_method_refused (forge_unsupported, the exact
// pre-claim key set, allowed [squash merge]) with NOTHING recorded: no claim,
// no verdict, no merge_queue_authorized row, no dispatch. Counterfactual
// (13): moving the check after claimInlineMergeDispatch records a verdict and
// a claim (RED).
func TestMergeRunMergeMethod_InlinePreClaimRefusal(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("mmr-in", mmrSpec("rebase"))
	w.mmrGitLabRef(r)
	resp := w.mergeRun(r)
	if resp.Code != http.StatusConflict {
		t.Fatalf("merge = %d %s, want 409", resp.Code, resp.Body.String())
	}
	d := mmrErrorDetails(t, resp.Body.Bytes(), mergeCodeMergeMethodRefused)
	if got := mmrKeys(d); got != mmrPreClaimKeys {
		t.Fatalf("detail keys = %s, want %s", got, mmrPreClaimKeys)
	}
	allowed, _ := json.Marshal(d["allowed_merge_methods"])
	if d["reason"] != MergeMethodRefusalForgeUnsupported || d["merge_method"] != "rebase" ||
		d["merge_method_source"] != MergeMethodSourceDeclared || d["next_step"] != mergeMethodNextStepMergeByHand ||
		string(allowed) != `["squash","merge"]` {
		t.Fatalf("details = %v, want forge_unsupported rebase declared, allowed [squash merge], merge_by_hand", d)
	}
	w.requireNothingRecorded(r, m)
}

// TestMergeRunMergeMethod_InlineUndeterminableFailsClosed (approval condition
// C2): an unparseable admission spec is refused merge_method_refused /
// merge_method_undeterminable before the claim, nothing recorded.
// Counterfactual: staticMergeMethodRefusal returning nil on a resolution error
// falls through to the forge-approval fold's forge_approval_required — RED on
// the code (the zero-claims assertion alone is masked by that fold).
func TestMergeRunMergeMethod_InlineUndeterminableFailsClosed(t *testing.T) {
	w, m := newMQEWorld(t)
	r := w.faeInline("mmr-und", mmrSpec(""))
	w.mmrUnparseable(r)
	resp := w.mergeRun(r)
	d := mmrErrorDetails(t, resp.Body.Bytes(), mergeCodeMergeMethodRefused)
	if d["reason"] != MergeMethodRefusalUndeterminable || mmrKeys(d) != mmrPreClaimKeys {
		t.Fatalf("details = %v, want reason %s with the pre-claim key set", d, MergeMethodRefusalUndeterminable)
	}
	w.requireNothingRecorded(r, m)
}

// mmrForgeWorld is an mqmWorld whose GateMerger is a REAL ForgeMerger over
// leaf, logging to a captured JSON buffer.
func mmrForgeWorld(t *testing.T, leaf *fmmPolicyLeaf) (*mqmWorld, func() string) {
	t.Helper()
	w := newMQMWorld(t)
	logger, buf := fmmCapture()
	w.srv.cfg.GateMerger = ForgeMerger{GitHub: leaf, Logger: logger}
	w.srv.cfg.Logger = logger
	return w, buf.String
}

// TestMergeRunMergeMethod_InlinePostDispatchRefusal: a REST 405 the real
// ForgeMerger attributes to the declared merge method (the reader disallows
// merge) answers 409 merge_method_refused (forge_disallows, the exact
// post-dispatch key set, a closed-set allowed list) whose body and log carry
// no forge text, and the claim is RELEASED through the unchanged classifier.
// Counterfactuals: (10) deleting the post-dispatch branch answers 502 (RED);
// (21) appending merr.Error() to the message carries the sentinel (RED).
func TestMergeRunMergeMethod_InlinePostDispatchRefusal(t *testing.T) {
	leaf := &fmmPolicyLeaf{err: fmmNotMergeable(), allowed: forge.AllowedMergeMethods{Squash: true, Rebase: true}}
	w, logs := mmrForgeWorld(t, leaf)
	r := w.faeInline("mmr-post", mmrSpec("merge"))
	resp := w.mergeRun(r)
	if resp.Code != http.StatusConflict {
		t.Fatalf("merge = %d %s, want 409", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	requireNoForgeText(t, "409 body", body)
	d := mmrErrorDetails(t, resp.Body.Bytes(), mergeCodeMergeMethodRefused)
	if got := mmrKeys(d); got != mmrPostDispatchKeys {
		t.Fatalf("detail keys = %s, want %s", got, mmrPostDispatchKeys)
	}
	if d["reason"] != MergeMethodRefusalForgeDisallows || d["merge_method"] != "merge" ||
		d["next_step"] != mergeMethodNextStepAllowOrMergeHand || d["verdict_sequence"] == nil {
		t.Fatalf("details = %v, want forge_disallows merge with verdict_sequence", d)
	}
	allowed, _ := d["allowed_merge_methods"].([]any)
	if len(allowed) == 0 {
		t.Fatalf("allowed_merge_methods = %v, want the reader's allowed names", d["allowed_merge_methods"])
	}
	for _, a := range allowed {
		if a != "squash" && a != "merge" && a != "rebase" {
			t.Fatalf("allowed_merge_methods carries %v, outside the closed set", a)
		}
	}
	if !strings.Contains(body, "released") {
		t.Errorf("message does not report the released claim: %s", body)
	}
	if n := w.claims(r); n != 0 {
		t.Fatalf("claims = %d, want 0 (released)", n)
	}
	settled := w.rows(r, categoryMergeDispatchSettled)
	if len(settled) != 1 || w.payload(settled[0])["outcome"] != string(mergeDispatchRelease) {
		t.Fatalf("merge_dispatch_settled rows = %d, want one released", len(settled))
	}
	if n := len(w.rows(r, CategoryMergeVerdictRecorded)); n != 1 {
		t.Fatalf("verdict rows = %d, want 1 (durable)", n)
	}
	requireNoForgeText(t, "server log", logs())
	if !strings.Contains(logs(), `"merge_method":"merge"`) {
		t.Errorf("log carries no merge_method attribute: %s", logs())
	}
}

// TestMergeRunMergeMethod_InlineGraphQLRefusalNamesRearm (approval condition
// C5): a refusal over a GraphQL validation envelope settles dispatch_failed
// under the unchanged classifier, so the 409 remedy names the operator RE-ARM
// explicitly in addition to the settings change.
func TestMergeRunMergeMethod_InlineGraphQLRefusalNamesRearm(t *testing.T) {
	leaf := &fmmPolicyLeaf{err: errors.Join(forge.ErrValidation, errors.New(fmmSecretText)), allowed: forge.AllowedMergeMethods{Squash: true}}
	w, _ := mmrForgeWorld(t, leaf)
	r := w.faeInline("mmr-gql", mmrSpec("merge"))
	resp := w.mergeRun(r)
	d := mmrErrorDetails(t, resp.Body.Bytes(), mergeCodeMergeMethodRefused)
	if d["reason"] != MergeMethodRefusalForgeDisallows {
		t.Fatalf("details = %v, want forge_disallows", d)
	}
	body := resp.Body.String()
	requireNoForgeText(t, "409 body", body)
	if !strings.Contains(body, "operator must RE-ARM the claim AND change the repository's merge settings") {
		t.Fatalf("message does not name the operator re-arm explicitly: %s", body)
	}
	if st, _ := w.claimRow(r, new(string)); st != string(mergequeue.DispatchFailed) {
		t.Fatalf("claim state = %q, want dispatch_failed", st)
	}
}

// TestMergeRunMergeMethod_502NamesResolvedMethod: an allowed method keeps the
// 502 merge_dispatch_failed, whose message names the method actually
// dispatched and where it came from — declared merge, or the squash default.
// Counterfactual (22): rendering the literal squash reddens the declared arm.
func TestMergeRunMergeMethod_502NamesResolvedMethod(t *testing.T) {
	for _, tc := range []struct{ name, method, want string }{
		{"declared merge", "merge", "merge method merge: .fishhawk/workflows.yaml merge_method"},
		{"default squash", "", "merge method squash: default (no merge_method declared)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf := &fmmPolicyLeaf{err: fmmNotMergeable(), allowed: forge.AllowedMergeMethods{Squash: true, Merge: true, Rebase: true}}
			w, _ := mmrForgeWorld(t, leaf)
			r := w.faeInline("mmr-502", mmrSpec(tc.method))
			resp := w.mergeRun(r)
			if resp.Code != http.StatusBadGateway {
				t.Fatalf("merge = %d %s, want 502", resp.Code, resp.Body.String())
			}
			mmrErrorDetails(t, resp.Body.Bytes(), "merge_dispatch_failed")
			if !strings.Contains(resp.Body.String(), tc.want) {
				t.Fatalf("502 message does not name %q: %s", tc.want, resp.Body.String())
			}
		})
	}
}

// mmrDelegatedReady seeds a may_merge-ready run on spec (admqReady's shape).
func (w *mqmWorld) mmrDelegatedReady(name, spec string) *mcqRun {
	w.t.Helper()
	r := w.seed(name, "h1", []byte(spec))
	w.setStage(r.review.ID, run.StageStateSucceeded)
	w.mqeRebased(r, "pre-h1", "h1", "h1", false)
	w.mqePassed(r, "h1")
	w.mqeAppend(r, drive.Category, map[string]any{"rule": string(drive.RuleChecksGreenAwaitingMerge)})
	return r
}

// TestAutoDriveMergeMethod_DelegatedPreClaimRefusal: the delegated arm refuses
// a gitlab-family rebase declaration observe-only BEFORE the fold and the
// claim: no claim, no merge_queue_authorized row, the merger never called.
// Counterfactual (14): moving the check after ClaimMergeDispatch takes a
// delegated claim (RED).
func TestAutoDriveMergeMethod_DelegatedPreClaimRefusal(t *testing.T) {
	w := newMQMWorld(t)
	r := w.mmrDelegatedReady("mmr-del", mmrSpec("rebase"))
	w.mmrGitLabRef(r)
	m := &admqMerger{}
	out := w.admqDrive(r, m)
	if out.Acted || !strings.HasPrefix(out.Note, "merge_method_refused (forge_unsupported)") {
		t.Fatalf("outcome = %+v, want observe-only merge_method_refused (forge_unsupported)", out)
	}
	if m.calls() != 0 || w.claims(r) != 0 || len(w.rows(r, CategoryMergeQueueAuthorized)) != 0 {
		t.Fatalf("merger calls %d, claims %d, authorized rows %d; want all 0",
			m.calls(), w.claims(r), len(w.rows(r, CategoryMergeQueueAuthorized)))
	}
}

// TestAutoDriveMergeMethod_DelegatedUndeterminableFailsClosed (approval
// condition C2): the delegated dispatch tail driven with an unparseable
// admission spec refuses mergeDispatchMethodUnsupported (reason
// merge_method_undeterminable) before the fold and the claim. Counterfactual:
// a nil on a resolution error yields the fold's
// mergeDispatchApprovalNotQualified instead — RED on the outcome.
func TestAutoDriveMergeMethod_DelegatedUndeterminableFailsClosed(t *testing.T) {
	w := newMQMWorld(t)
	r := w.mmrDelegatedReady("mmr-del-und", mmrSpec(""))
	ctx := context.Background()
	runRow, err := w.repo.GetRun(ctx, r.id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	runRow.WorkflowSpec = fmmUnparseableSpec
	stages, err := w.repo.ListStagesForRun(ctx, r.id)
	if err != nil {
		t.Fatalf("stages: %v", err)
	}
	m := &admqMerger{}
	outcome, _, _, derr := w.srv.dispatchAcceptanceGatedMergeSettled(ctx, runRow, stages, m, "h1")
	var ref *MergeMethodRefusalError
	if outcome != mergeDispatchMethodUnsupported || !errors.As(derr, &ref) || ref.Reason != MergeMethodRefusalUndeterminable {
		t.Fatalf("outcome = %v err %v, want mergeDispatchMethodUnsupported / %s", outcome, derr, MergeMethodRefusalUndeterminable)
	}
	if m.calls() != 0 || w.claims(r) != 0 || len(w.rows(r, CategoryMergeQueueAuthorized)) != 0 {
		t.Fatalf("merger calls %d, claims %d; want 0", m.calls(), w.claims(r))
	}
}

// TestAutoDriveMergeMethod_DelegatedPostDispatchNote: a dispatch refusal the
// real ForgeMerger attributes to the declared method is the note
// "merge_method_refused: " + FieldDetail(), and neither the note nor any log
// record carries forge text. Counterfactual (23): building the note from
// derr.Error() or logging the error attribute carries the sentinel (RED).
func TestAutoDriveMergeMethod_DelegatedPostDispatchNote(t *testing.T) {
	leaf := &fmmPolicyLeaf{err: fmmNotMergeable(), allowed: forge.AllowedMergeMethods{Squash: true}}
	w := newMQMWorld(t)
	logger, buf := fmmCapture()
	w.srv.cfg.Logger = logger
	merger := ForgeMerger{GitHub: leaf, Logger: logger}
	r := w.mmrDelegatedReady("mmr-del-post", mmrSpec("merge"))
	runRow, err := w.repo.GetRun(context.Background(), r.id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	out, derr := w.srv.AutoDriveRunGate(context.Background(), runRow, campaignOperatorIdentity(), merger, nil)
	var ref *MergeMethodRefusalError
	if !errors.As(derr, &ref) {
		t.Fatalf("AutoDriveRunGate err = %v, want a MergeMethodRefusalError", derr)
	}
	if out.Note != "merge_method_refused: "+ref.FieldDetail() {
		t.Fatalf("note = %q, want merge_method_refused: + FieldDetail", out.Note)
	}
	requireNoForgeText(t, "note", out.Note)
	requireNoForgeText(t, "log", buf.String())
	found := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec["msg"] == "auto-drive: the forge refused the delegated merge for the run's merge method" {
			found = true
			if rec["merge_method"] != "merge" || rec["reason"] != MergeMethodRefusalForgeDisallows || rec["error"] != nil {
				t.Fatalf("log record = %v, want method attributes and no error", rec)
			}
		}
	}
	if !found {
		t.Fatalf("no merge_method_refused log line: %s", buf.String())
	}
}

// TestMergeOnPassMergeMethod_PreClaimEject: merge-on-pass ejects a
// gitlab-family rebase run pre-lock (forge_merge_refused / forge_reason
// merge_method_refused) with ZERO claims on the ledger and the table, no
// merge call and no verdict. Counterfactual (15): moving the check after
// ClaimMergeDispatch claims queue@h1 (RED).
func TestMergeOnPassMergeMethod_PreClaimEject(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("mmr-mop", "h1", mmrSpec("rebase"))
	w.mmrGitLabRef(r)
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonForgeMergeRefused)
	if out.State != mergequeue.StateEjected || out.ForgeReason != mergeQueueForgeReasonMergeMethodRefused {
		t.Fatalf("outcome = %+v, want ejected / merge_method_refused", out)
	}
	w.ledger.mu.Lock()
	ledgerClaims := len(w.ledger.claims)
	w.ledger.mu.Unlock()
	if ledgerClaims != 0 || w.claims(r) != 0 || len(w.rows(r, CategoryMergeVerdictRecorded)) != 0 {
		t.Fatalf("ledger claims %d, table claims %d, verdicts %d; want 0", ledgerClaims, w.claims(r), len(w.rows(r, CategoryMergeVerdictRecorded)))
	}
	w.requireMerges(0)
}

// TestMergeOnPassMergeMethod_UndeterminableFailsClosed (approval condition
// C2): an unparseable admission spec ejects pre-lock with zero claims.
// Counterfactual: a nil on a resolution error leaves merge-on-pass to its
// later gates, which never answer this eject (RED on the outcome).
func TestMergeOnPassMergeMethod_UndeterminableFailsClosed(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("mmr-mop-und", "h1", mmrSpec(""))
	w.mmrUnparseable(r)
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonForgeMergeRefused)
	if out.ForgeReason != mergeQueueForgeReasonMergeMethodRefused || !strings.Contains(out.ForgeDetail, "undeterminable") {
		t.Fatalf("outcome = %+v, want merge_method_refused undeterminable", out)
	}
	if w.claims(r) != 0 || len(w.ledger.claims) != 0 {
		t.Fatalf("claims = %d / %d, want 0", w.claims(r), len(w.ledger.claims))
	}
	w.requireMerges(0)
}

// TestMergeOnPassMergeMethod_PostDispatchEjectHygiene: a pinned merge refusal
// carrying a MergeMethodRefusalError (its Cause holding a token-bearing URL)
// ejects forge_reason merge_method_refused, and NEITHER the outcome detail nor
// the merge_queue_ejected / settled rows carry the sentinel. Counterfactual
// (24): ForgeDetail = mergeErr.Error()-with-Cause carries it (RED).
func TestMergeOnPassMergeMethod_PostDispatchEjectHygiene(t *testing.T) {
	w := newMQMWorld(t)
	r, req := w.ready("mmr-mop-post", "h1", mmrSpec("merge"))
	w.merger.err = &MergeMethodRefusalError{Method: "merge", Source: MergeMethodSourceDeclared,
		Reason: MergeMethodRefusalForgeDisallows, Allowed: []string{"squash"}, Cause: fmmNotMergeable()}
	out := w.decide(req)
	requireOutcome(t, out, mergeOnPassSettle, mergeCandidateQueueReasonForgeMergeRefused)
	if out.ForgeReason != mergeQueueForgeReasonMergeMethodRefused {
		t.Fatalf("forge_reason = %q, want merge_method_refused", out.ForgeReason)
	}
	requireNoForgeText(t, "outcome detail", out.Detail)
	requireNoForgeText(t, "forge_detail", out.ForgeDetail)
	var n int
	w.srv.applyMergeOnPassOutcome(context.Background(), req.Entry, out, &n)
	ejected := w.rows(r, CategoryMergeQueueEjected)
	if len(ejected) != 1 {
		t.Fatalf("merge_queue_ejected rows = %d, want 1", len(ejected))
	}
	p := w.payload(ejected[0])
	if p["forge_reason"] != mergeQueueForgeReasonMergeMethodRefused {
		t.Fatalf("ejected payload = %v, want forge_reason merge_method_refused", p)
	}
	for _, c := range []string{CategoryMergeQueueEjected, CategoryMergeCandidateQueueSettled} {
		for _, e := range w.rows(r, c) {
			requireNoForgeText(t, c+" row", string(e.Payload))
		}
	}

}
