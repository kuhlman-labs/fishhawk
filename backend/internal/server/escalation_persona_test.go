package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/escalation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Escalation-attached reviewer personas (ADR-084 D2(c) / E55.9 / #3754) — the
// runtime slice. Every review test drives the REAL runPlanReviews /
// runImplementReviews with workflow-v2 spec BYTES on the run row, so a run
// crosses spec.ParseBytes+Validate → escalation.Evaluate / PersonaAttachments →
// SelectNamedReviewerPersonas → PlanReviewers.For → repodoc remit resolution →
// prompt.Build → planreview payloads → the audit fake. Shared fixtures:
// personaSpec / newPersonaPlanRun (reviewer_persona_test.go) and
// personaImplRun (trace_test.go, reused unmodified).

const (
	escalatedGlob = "backend/internal/spec/**"
	escalatedPath = "backend/internal/spec/x.go"
)

// escalatedSpec is a v2 spec whose one escalation attaches `security` when a
// path under escalatedGlob is touched; attachOn optionally also attaches it
// statically.
func escalatedSpec(attachOn string) []byte {
	return personaSpec(personaSpecOpts{attachOn: attachOn, escalatePaths: []string{escalatedGlob}})
}

// escalatedRuleKey is escalation.RuleKey of escalatedSpec's one declaration.
func escalatedRuleKey(t *testing.T) string {
	t.Helper()
	parsed, err := spec.ParseBytes(escalatedSpec(""))
	if err != nil {
		t.Fatalf("parse escalated spec: %v", err)
	}
	esc := parsed.Workflows["feature_change"].Escalations
	if len(esc) != 1 {
		t.Fatalf("escalations = %d, want 1", len(esc))
	}
	return escalation.RuleKey(esc[0])
}

// decodePersonaAttached decodes every escalation_persona_attached entry.
func decodePersonaAttached(t *testing.T, au *auditFake) []escalationPersonaAttachedPayload {
	t.Helper()
	var out []escalationPersonaAttachedPayload
	for _, e := range auditFakeEntries(au, CategoryEscalationPersonaAttached) {
		var p escalationPersonaAttachedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", CategoryEscalationPersonaAttached, err)
		}
		out = append(out, p)
	}
	return out
}

func changedDiff(files ...policy.ChangedFile) policy.Diff {
	return policy.Diff{ChangedFiles: files}
}

// (1) SCOPE DRIFT attaches via the diff: the approved plan's scope is
// backend/internal/foo/foo.go only (newImplementReviewServerWithSet), the
// escalation matches backend/internal/spec/**, and the diff touches
// backend/internal/spec/x.go. The persona runs once, implement_review_started
// names and counts it, and ONE escalation_persona_attached entry records
// security ← fired [0] / its RuleKey, via_diff_only, path_source
// approved_plan_scope_and_diff, stamped on the implement stage.
//
// Counterfactual (the ticket's): drop the diff from the trace.go call site
// (plan scope only) — persona calls 0: RED. Mechanism: the fixture's plan scope
// deliberately excludes the escalated path, so only the diff union can fire
// the rule.
func TestImplementReview_EscalationPersona_DriftAttachesViaDiff(t *testing.T) {
	std, persona := approvingFake(), approvingFake()
	s, au, runRow, implStage, _ := personaImplRun(t, escalatedSpec(""), std, persona, "codex/"+personaAgentModel)
	diff := changedDiff(
		policy.ChangedFile{Path: "backend/internal/foo/foo.go", Status: policy.StatusModified},
		policy.ChangedFile{Path: escalatedPath, Status: policy.StatusModified},
	)
	if s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, diff, nil, "head-drift", nil) {
		t.Fatal("approving reviewers must not gate")
	}
	if n := len(reviewerCalls(persona)); n != 1 {
		t.Fatalf("persona calls = %d, want 1 — drift into the escalated path must attach the persona", n)
	}
	if n := len(reviewerCalls(std)); n != 1 {
		t.Errorf("standard calls = %d, want 1", n)
	}
	started := decodeStarted(t, au, "implement_review_started")
	if started.ConfiguredAgents != 2 || !slices.Equal(started.Personas, []string{personaTestName}) {
		t.Errorf("implement_review_started = configured %d personas %v, want 2 [%s]", started.ConfiguredAgents, started.Personas, personaTestName)
	}
	got := decodePersonaAttached(t, au)
	if len(got) != 1 {
		t.Fatalf("%s entries = %d, want 1", CategoryEscalationPersonaAttached, len(got))
	}
	e := got[0]
	if e.ReviewKind != "implement_review" || e.PathSource != escalationPathSourceApprovedAndDiff || e.HeadSHA != "head-drift" || e.StageID != implStage.ID.String() {
		t.Errorf("entry = %+v, want implement_review / %s / head-drift / stage %s", e, escalationPathSourceApprovedAndDiff, implStage.ID)
	}
	if len(e.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want 1", e.Attachments)
	}
	a := e.Attachments[0]
	if a.Persona != personaTestName || !slices.Equal(a.Fired, []int{0}) || !slices.Equal(a.FiredKeys, []string{escalatedRuleKey(t)}) || !a.ViaDiffOnly || a.AlsoStatic {
		t.Errorf("attachment = %+v, want security fired [0] keys [%s] via_diff_only, not also_static", a, escalatedRuleKey(t))
	}
	entries := auditFakeEntries(au, CategoryEscalationPersonaAttached)
	if entries[0].StageID == nil || *entries[0].StageID != implStage.ID {
		t.Errorf("entry stamped on %v, want the implement stage %s", entries[0].StageID, implStage.ID)
	}
}

// (2) A RENAME whose SOURCE is under the escalated glob attaches the persona:
// moving a file OUT of a sensitive path is a change to that path.
//
// Counterfactual: drop OldPath from the path set (diffReviewPaths reading
// only ChangedFile.Path) — persona calls 0: RED.
func TestImplementReview_EscalationPersona_RenameSourceAttaches(t *testing.T) {
	persona := approvingFake()
	s, au, runRow, implStage, _ := personaImplRun(t, escalatedSpec(""), approvingFake(), persona, "codex/"+personaAgentModel)
	diff := changedDiff(policy.ChangedFile{Path: "backend/internal/foo/moved.go", OldPath: escalatedPath, Status: policy.StatusRenamed})
	s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, diff, nil, "head-rename", nil)
	if n := len(reviewerCalls(persona)); n != 1 {
		t.Fatalf("persona calls = %d, want 1 — a rename source under the escalated glob must attach", n)
	}
	if got := decodePersonaAttached(t, au); len(got) != 1 || !got[0].Attachments[0].ViaDiffOnly {
		t.Errorf("entries = %+v, want one via_diff_only attachment", got)
	}
}

// (3) The STAGE-CUMULATIVE evaluation diff is covered. The implement-review
// harness cannot seed the cumulative set (resolveStageCumulativeEval needs a
// real push ledger and a git working dir; every degrade returns the pass
// diff), so this proves at the unit level that diffReviewPaths UNIONS every
// diff it is handed — a path present only in the second (cumulative) diff,
// and a rename source in it, are both in the result, de-duplicated
// and sorted. The trace.go call site passes (diff, evalDiff).
//
// Counterfactual: read only the first diff in diffReviewPaths — the
// cumulative-only path is missing: RED.
func TestDiffReviewPaths_UnionsEveryDiffWithRenameSources(t *testing.T) {
	pass := changedDiff(policy.ChangedFile{Path: "backend/internal/foo/foo.go", Status: policy.StatusModified})
	cumulative := changedDiff(
		policy.ChangedFile{Path: "backend/internal/foo/foo.go", Status: policy.StatusModified},
		policy.ChangedFile{Path: escalatedPath, Status: policy.StatusModified},
		policy.ChangedFile{Path: "docs/new.md", OldPath: "backend/internal/spec/old.md", Status: policy.StatusRenamed},
	)
	got := diffReviewPaths(pass, cumulative)
	want := []string{"backend/internal/foo/foo.go", "backend/internal/spec/old.md", escalatedPath, "docs/new.md"}
	if !slices.Equal(got, want) {
		t.Errorf("diffReviewPaths = %v, want %v", got, want)
	}
	if got := diffReviewPaths(); got != nil {
		t.Errorf("diffReviewPaths() = %v, want nil", got)
	}
}

// (4) COST ONLY WHERE ATTACHED: plan and diff touch only foo.go, so the
// escalation does not fire — the persona never runs, no
// escalation_persona_attached entry, configured_agents 1.
//
// Counterfactual: attach every declared require.reviewers persona regardless
// of firing (feed PersonaAttachments a Result with every declaration fired) —
// the persona runs: RED.
func TestImplementReview_EscalationPersona_NoEscalatedPathNoPersona(t *testing.T) {
	persona := approvingFake()
	s, au, runRow, implStage, f := personaImplRun(t, escalatedSpec(""), approvingFake(), persona, "codex/"+personaAgentModel)
	s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, reviewInjectionDiff(), nil, "head-none", nil)
	if n := len(reviewerCalls(persona)); n != 0 {
		t.Errorf("persona calls = %d, want 0 — a non-firing escalation attaches nothing", n)
	}
	if n := countAuditCategory(au, CategoryEscalationPersonaAttached); n != 0 {
		t.Errorf("%s entries = %d, want 0", CategoryEscalationPersonaAttached, n)
	}
	if started := decodeStarted(t, au, "implement_review_started"); started.ConfiguredAgents != 1 || len(started.Personas) != 0 {
		t.Errorf("implement_review_started = configured %d personas %v, want 1 and none", started.ConfiguredAgents, started.Personas)
	}
	if n := f.fetches(); n != 0 {
		t.Errorf("remit fetched %d times for an unattached persona, want 0", n)
	}
}

// planBodyWithScope returns the valid plan fixture with its scope.files
// replaced by paths.
func planBodyWithScope(t *testing.T, paths ...string) []byte {
	t.Helper()
	p := planfixture.Valid()
	files := make([]any, 0, len(paths))
	for _, path := range paths {
		files = append(files, map[string]any{"path": path, "operation": "modify"})
	}
	p["scope"] = map[string]any{"files": files}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// (5) PLAN REVIEW matches the plan UNDER REVIEW: a plan whose scope names the
// escalated path attaches the persona at plan review with path_source
// plan_scope (no diff, so never via_diff_only); a sibling plan that misses the
// path attaches none.
//
// Counterfactual: pass empty plan paths at the plan.go call site — the
// matching case runs no persona: RED.
func TestPlanReview_EscalationPersona_MatchesPlanUnderReview(t *testing.T) {
	for _, tc := range []struct {
		name        string
		scope       string
		wantPersona int
	}{
		{"plan scope names the escalated path", escalatedPath, 1},
		{"plan scope misses the escalated path", "backend/internal/foo/foo.go", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			persona := approvingFake()
			p := newPersonaPlanRun(t, escalatedSpec(""), approvingFake(), persona)
			p.planBody = planBodyWithScope(t, tc.scope)
			p.review(t)
			if n := len(reviewerCalls(persona)); n != tc.wantPersona {
				t.Fatalf("persona calls = %d, want %d", n, tc.wantPersona)
			}
			got := decodePersonaAttached(t, p.au)
			if tc.wantPersona == 0 {
				if len(got) != 0 {
					t.Errorf("entries = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].ReviewKind != "plan_review" || got[0].PathSource != escalationPathSourcePlanScope || got[0].HeadSHA != "" {
				t.Fatalf("entries = %+v, want one plan_review / plan_scope", got)
			}
			if a := got[0].Attachments; len(a) != 1 || a[0].Persona != personaTestName || a[0].ViaDiffOnly {
				t.Errorf("attachments = %+v, want security, not via_diff_only", a)
			}
			if started := decodeStarted(t, p.au, "plan_review_started"); started.ConfiguredAgents != 2 {
				t.Errorf("configured_agents = %d, want 2", started.ConfiguredAgents)
			}
		})
	}
}

// (6) A persona attached statically AND by an escalation runs EXACTLY ONCE;
// the attachment records also_static.
//
// Counterfactual: drop the de-dup in resolveReviewPersonaInvocations — the
// persona is invoked twice: RED.
func TestReview_EscalationPersona_StaticAndEscalatedDedup(t *testing.T) {
	persona := approvingFake()
	s, au, runRow, implStage, _ := personaImplRun(t, escalatedSpec("implement"), approvingFake(), persona, "codex/"+personaAgentModel)
	diff := changedDiff(policy.ChangedFile{Path: escalatedPath, Status: policy.StatusModified})
	s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, diff, nil, "head-dedup", nil)
	if n := len(reviewerCalls(persona)); n != 1 {
		t.Fatalf("persona calls = %d, want exactly 1", n)
	}
	started := decodeStarted(t, au, "implement_review_started")
	if started.ConfiguredAgents != 2 || !slices.Equal(started.Personas, []string{personaTestName}) {
		t.Errorf("started = configured %d personas %v, want 2 [%s]", started.ConfiguredAgents, started.Personas, personaTestName)
	}
	got := decodePersonaAttached(t, au)
	if len(got) != 1 || len(got[0].Attachments) != 1 || !got[0].Attachments[0].AlsoStatic {
		t.Errorf("entries = %+v, want one also_static attachment", got)
	}
}

// (7) A Match error (a malformed glob — unreachable through ParseBytes, so the
// workflow is built by construction) degrades with escalation_unevaluable
// rather than reading as "nothing fired"; and the orchestrator turns it into
// ONE counted persona_attachment_unresolvable pseudo invocation. A
// SelectNamedReviewerPersonas refusal (a hand-built spec naming an undeclared
// persona) degrades the same way.
//
// Counterfactual: swallow the Evaluate error as no-match — degraded is empty
// and no pseudo invocation is produced: RED.
func TestResolveEscalationPersonas_MatchErrorDegrades(t *testing.T) {
	runRow := &run.Run{ID: uuid.New(), WorkflowID: "feature_change"}
	persona := spec.ReviewerPersona{}
	persona.Agent.Provider = "codex"
	persona.Agent.Model = personaAgentModel
	persona.Remit.Path = personaRemitPath
	build := func(glob, name string) (*spec.Spec, spec.Workflow) {
		wf := spec.Workflow{Escalations: []spec.Escalation{{
			Match:   spec.Predicate{Paths: []string{glob}},
			Require: spec.EscalationRequirements{Reviewers: []string{name}},
		}}}
		return &spec.Spec{
			ReviewerPersonas: map[string]spec.ReviewerPersona{personaTestName: persona},
			Workflows:        map[string]spec.Workflow{"feature_change": wf},
		}, wf
	}
	paths := reviewPaths{plan: []string{escalatedPath}, source: escalationPathSourcePlanScope}

	parsed, wf := build("backend/[", personaTestName)
	if got := resolveEscalationPersonas(runRow, parsed, wf, paths); got.degraded != personaDetailEscalationUnevaluable || len(got.selected) != 0 {
		t.Errorf("malformed glob: %+v, want degraded %q and nothing selected", got, personaDetailEscalationUnevaluable)
	}
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{}), approvingFake(), approvingFake())
	invs := p.s.resolveParsedReviewPersonaInvocations(t.Context(), runRow, parsed, uuid.New(), "plan_review", paths)
	if len(invs) != 1 || invs[0].persona == nil || invs[0].persona.degraded != personaDetailEscalationUnevaluable ||
		invs[0].persona.reason != planreview.ReasonPersonaAttachmentUnresolvable {
		t.Errorf("invocations = %+v, want one escalation_unevaluable pseudo invocation", invs)
	}
	if !strings.Contains(p.logs.String(), "reviewer personas: evaluate escalation-attached personas") {
		t.Errorf("WARN log missing:\n%s", p.logs.String())
	}

	parsed, wf = build(escalatedGlob, "ghost")
	if got := resolveEscalationPersonas(runRow, parsed, wf, paths); got.degraded != personaDetailEscalationUnevaluable || len(got.selected) != 0 {
		t.Errorf("undeclared persona: %+v, want degraded %q", got, personaDetailEscalationUnevaluable)
	}

	parsed, wf = build(escalatedGlob, personaTestName)
	if got := resolveEscalationPersonas(runRow, parsed, wf, paths); got.degraded != "" || len(got.selected) != 1 || got.viaDiffOnly[personaTestName] {
		t.Errorf("well-formed control: %+v, want security selected via plan scope", got)
	}
	// A workflow with no require.reviewers short-circuits: nothing evaluated,
	// so even a malformed glob there cannot degrade the round.
	wf.Escalations[0].Require.Reviewers = nil
	wf.Escalations[0].Match.Paths = []string{"backend/["}
	if got := resolveEscalationPersonas(runRow, parsed, wf, paths); got.degraded != "" || got.selected != nil {
		t.Errorf("reviewers-less escalations: %+v, want the zero resolution", got)
	}
}

// (8) escalation_persona_attached is BEST-EFFORT: a failing append still runs
// the persona and WARN-logs naming it; a missing audit repository WARN-logs
// and writes nothing.
//
// Counterfactual: return before invoking the personas when the append fails
// (make the audit a precondition) — the persona is not invoked: RED.
func TestEscalationPersonaAttachedAudit_BestEffort(t *testing.T) {
	persona := approvingFake()
	s, au, runRow, implStage, _ := personaImplRun(t, escalatedSpec(""), approvingFake(), persona, "codex/"+personaAgentModel)
	logs := &bytes.Buffer{}
	s.cfg.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	au.appendErrCategory = CategoryEscalationPersonaAttached
	diff := changedDiff(policy.ChangedFile{Path: escalatedPath, Status: policy.StatusModified})
	s.runImplementReviews(t.Context(), runRow.ID, implStage.ID, diff, nil, "head-besteffort", nil)
	if n := len(reviewerCalls(persona)); n != 1 {
		t.Errorf("persona calls = %d, want 1 — an audit failure must not cancel the attachment", n)
	}
	if !strings.Contains(logs.String(), "escalation persona: append "+CategoryEscalationPersonaAttached+" failed") || !strings.Contains(logs.String(), personaTestName) {
		t.Errorf("WARN log does not name the failed append and the persona:\n%s", logs.String())
	}

	logs.Reset()
	s.cfg.AuditRepo = nil
	res := escalationPersonaResolution{attachments: []escalation.PersonaAttachment{{Persona: personaTestName}}}
	s.writeEscalationPersonaAttachedAudit(t.Context(), runRow, implStage.ID, "implement_review", reviewPaths{}, res, nil)
	if !strings.Contains(logs.String(), "no audit repository") || !strings.Contains(logs.String(), personaTestName) {
		t.Errorf("nil-repository WARN missing:\n%s", logs.String())
	}
}

// (Approval condition 2) The approval/delegation seams for a REVIEWERS-ONLY
// firing: resolveEscalations writes escalation_fired whenever res.Any() — not
// on Requirements.IsZero — so a firing reviewers-only escalation records ONE
// escalation_fired entry whose summary carries NO "Raised:" clause and whose
// payload carries no raised requirement, while the composed requirement stays
// zero (the approval gate is neither blocked nor marked escalated).
func TestResolveEscalations_ReviewersOnlyFiringAuditsWithoutRaise(t *testing.T) {
	s, au, runRow, implStage, _ := personaImplRun(t, personaSpec(personaSpecOpts{escalatePaths: []string{"backend/internal/foo/**"}}), approvingFake(), nil, "")
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		t.Fatal(err)
	}
	wf := parsed.Workflows["feature_change"]
	res, err := s.resolveEscalations(t.Context(), runRow, &wf, implStage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Any() || !res.Requirements.IsZero() {
		t.Fatalf("result = any %v zero %v, want a firing that raises nothing", res.Any(), res.Requirements.IsZero())
	}
	entries := auditFakeEntries(au, CategoryEscalationFired)
	if len(entries) != 1 {
		t.Fatalf("escalation_fired entries = %d, want 1 — the seam audits every firing", len(entries))
	}
	var p map[string]any
	if err := json.Unmarshal(entries[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	summary, _ := p["summary"].(string)
	if !strings.Contains(summary, "fired") || strings.Contains(summary, "Raised") {
		t.Errorf("summary = %q, want a firing with no Raised clause", summary)
	}
	for _, k := range []string{"required_count", "required_member_of", "required_min_permission", "max_autonomy"} {
		if _, ok := p[k]; ok {
			t.Errorf("payload carries %q for a reviewers-only firing: %v", k, p)
		}
	}
}
