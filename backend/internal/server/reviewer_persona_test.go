package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Reviewer personas (ADR-084 / E55.8 / #3753) — the runtime half. Every test
// here drives the REAL runPlanReviews / runImplementReviews with a v2 workflow
// spec on the run row, so a run crosses spec parse → SelectReviewerPersonas →
// PlanReviewers.For → repodoc.Resolver (over personaFetcher, at the run's
// recorded DocumentBaseCommit) → prompt.Build → planreview payloads → the audit
// fake. The plan-path golden/grounding/gating cases live in plan_test.go, the
// implement-path ones in trace_test.go; the shared fixtures and the
// per-failure-mode remit-degrade cases live here.

const (
	personaTestName     = "security"
	personaRemitPath    = "docs/review/security-remit.md"
	personaRemitBody    = "PERSONA REMIT SENTINEL: audit every authorization boundary."
	personaBaseCommit   = "cccccccccccccccccccccccccccccccccccccccc"
	personaStdModel     = "std-model"
	personaAgentModel   = "persona-model"
	personaDeclSite     = "reviewer_personas.security.remit in .fishhawk/workflows.yaml"
	personaRemitHeading = "### Reviewer persona remit: security"
)

// personaSpecOpts shapes personaSpec.
type personaSpecOpts struct {
	// attachOn is the stage TYPE ("plan" / "implement") whose reviewers block
	// attaches the persona; "" declares and attaches no persona at all (the
	// persona-less golden baseline).
	attachOn string
	// human is reviewers.human on both stages (0 = gating, 1 = advisory).
	human int
	// personaProvider is the persona agent's provider ("codex" by default).
	personaProvider string
}

// personaSpec renders a complete workflow-v2 document: a plan and an
// implement stage, each with one standard anthropic/std-model agent reviewer,
// optionally a declared `security` persona attached on one of them.
func personaSpec(o personaSpecOpts) []byte {
	pp := o.personaProvider
	if pp == "" {
		pp = "codex"
	}
	var b strings.Builder
	b.WriteString("version: \"2\"\n")
	if o.attachOn != "" {
		fmt.Fprintf(&b, "reviewer_personas:\n  security:\n    agent:\n      provider: %s\n      model: %s\n    remit:\n      path: %s\n", pp, personaAgentModel, personaRemitPath)
	}
	b.WriteString("workflows:\n  feature_change:\n    stages:\n")
	stage := func(id, typ, produces string) {
		fmt.Fprintf(&b, "      - id: %s\n        type: %s\n        executor:\n          agent: claude-code\n        produces:\n%s", id, typ, produces)
		fmt.Fprintf(&b, "        reviewers:\n          human: %d\n          agents:\n            - provider: anthropic\n              model: %s\n", o.human, personaStdModel)
		if o.attachOn == typ {
			b.WriteString("          personas: [security]\n")
		}
	}
	stage("plan", "plan", "          - artifact: plan\n            schema: standard_v1\n")
	stage("implement", "implement", "          - artifact: pull_request\n")
	return []byte(b.String())
}

// personaFetcher serves files keyed by PATH at personaBaseCommit only (every
// other ref is not-found, so a read at a mutable ref is observable), and
// records every fetch.
type personaFetcher struct {
	mu    sync.Mutex
	files map[string]string
	refs  []string
	paths []string
	// err, when set, is returned for every fetch (a forge failure that is
	// NOT not-found).
	err error
}

func newPersonaFetcher() *personaFetcher {
	return &personaFetcher{files: map[string]string{personaRemitPath: personaRemitBody}}
}

func (f *personaFetcher) FetchFile(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, p, ref string) (*forge.FileContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs = append(f.refs, ref)
	f.paths = append(f.paths, p)
	if f.err != nil {
		return nil, f.err
	}
	c, ok := f.files[p]
	if !ok || ref != personaBaseCommit {
		return nil, forge.ErrNotFound
	}
	return &forge.FileContent{Path: p, Content: []byte(c), SHA: "blobblobblobblobblobblobblobblobblobblob"}, nil
}

func (f *personaFetcher) fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.refs)
}

// personaReviewerSet resolves by provider+"/"+model, so a standard reviewer
// and a persona on DIFFERENT providers get distinct capturing fakes; For errors
// for an absent key (the persona-provider-unavailable case).
type personaReviewerSet struct {
	def   PlanReviewer
	byKey map[string]PlanReviewer
}

func (s personaReviewerSet) Default() PlanReviewer { return s.def }

func (s personaReviewerSet) For(provider, model string, _ ...string) (PlanReviewer, error) {
	r, ok := s.byKey[provider+"/"+model]
	if !ok {
		return nil, fmt.Errorf("reviewer provider %q is not configured", provider)
	}
	return r, nil
}

func approvingFake() *fakePlanReviewer {
	return &fakePlanReviewer{verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}, model: "reviewer-model"}
}

// personaPlanRun is one wired plan-review run: the server, its fakes and the
// reviewer fakes the set resolves.
type personaPlanRun struct {
	s        *Server
	au       *auditFake
	rr       *promptRunRepo
	runID    uuid.UUID
	stageID  uuid.UUID
	std      PlanReviewer
	persona  PlanReviewer
	fetcher  *personaFetcher
	logs     *bytes.Buffer
	planBody []byte
}

// newPersonaPlanRun wires a plan-review run over spec with the standard fake
// at anthropic/std-model and the persona fake at codex/persona-model (either
// may be nil to leave its key unresolvable), a persona fetcher on the
// DocumentResolver, and the run's recorded admission commit.
func newPersonaPlanRun(t *testing.T, spec []byte, std, persona PlanReviewer) *personaPlanRun {
	t.Helper()
	runID, stageID := uuid.New(), uuid.New()
	byKey := map[string]PlanReviewer{}
	if std != nil {
		byKey["anthropic/"+personaStdModel] = std
	}
	if persona != nil {
		byKey["codex/"+personaAgentModel] = persona
	}
	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s, _, _, au, rr := newPlanServerWithReviewerSet(t, runID, stageID, personaReviewerSet{def: std, byKey: byKey}, spec, logger)
	st := rr.getStages[stageID]
	st.Type = run.StageTypePlan
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {st}}
	commit := personaBaseCommit
	rr.getRuns[runID].DocumentBaseCommit = &commit
	f := newPersonaFetcher()
	s.cfg.DocumentResolver = &repodoc.Resolver{Fetcher: f}
	return &personaPlanRun{s: s, au: au, rr: rr, runID: runID, stageID: stageID, std: std, persona: persona, fetcher: f, logs: logs, planBody: validPlanBytes(t)}
}

// review runs the REAL runPlanReviews and waits for a detached loop.
func (p *personaPlanRun) review(t *testing.T) bool {
	t.Helper()
	got := p.s.runPlanReviews(t.Context(), p.runID, p.stageID, p.planBody, nil, nil, nil, nil, nil)
	p.s.waitBackgroundReviews()
	return got
}

// personaLessPlanPrompt returns the standard reviewer prompt of a persona-LESS
// gating plan review over the same plan — the golden baseline every
// byte-identity assertion compares against.
func personaLessPlanPrompt(t *testing.T) string {
	t.Helper()
	std := approvingFake()
	base := newPersonaPlanRun(t, personaSpec(personaSpecOpts{}), std, nil)
	base.review(t)
	calls := reviewerCalls(std)
	if len(calls) != 1 {
		t.Fatalf("baseline standard reviewer calls = %d, want 1", len(calls))
	}
	return calls[0]
}

// decodeSkipped decodes every entry of category as a ReviewSkippedPayload.
func decodeSkipped(t *testing.T, au *auditFake, category string) []planreview.ReviewSkippedPayload {
	t.Helper()
	var out []planreview.ReviewSkippedPayload
	for _, e := range auditFakeEntries(au, category) {
		var p planreview.ReviewSkippedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", category, err)
		}
		out = append(out, p)
	}
	return out
}

// decodeStarted decodes the single *_review_started entry of category.
func decodeStarted(t *testing.T, au *auditFake, category string) planreview.ReviewStartedPayload {
	t.Helper()
	entries := auditFakeEntries(au, category)
	if len(entries) != 1 {
		t.Fatalf("%s entries = %d, want 1", category, len(entries))
	}
	var p planreview.ReviewStartedPayload
	if err := json.Unmarshal(entries[0].Payload, &p); err != nil {
		t.Fatalf("decode %s: %v", category, err)
	}
	return p
}

// remitInjections returns the document_injected payloads naming the remit.
func remitInjections(t *testing.T, au *auditFake) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range auditFakeEntries(au, "document_injected") {
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode document_injected: %v", err)
		}
		if p["path"] == personaRemitPath {
			out = append(out, p)
		}
	}
	return out
}

// terminalCount counts the round's terminal entries (verdict, failed, skipped)
// for the given review kind ("plan" / "implement").
func terminalCount(au *auditFake, kind string) int {
	return countAuditCategory(au, kind+"_reviewed") + countAuditCategory(au, kind+"_review_failed") + countAuditCategory(au, kind+"_review_skipped")
}

// assertPersonaDegraded asserts the fail-closed contract for one remit-degrade
// detail: the persona never ran, one persona_remit_unavailable skip names the
// persona and the detail, no remit injection was claimed, the standard
// reviewer ran exactly once on the persona-less golden prompt, and the round
// settles at configured_agents == 2.
func assertPersonaDegraded(t *testing.T, p *personaPlanRun, wantDetail string) {
	t.Helper()
	if n := len(reviewerCalls(p.persona.(*fakePlanReviewer))); n != 0 {
		t.Errorf("persona reviewer invoked %d times, want 0 — a degraded persona must never run", n)
	}
	skips := decodeSkipped(t, p.au, "plan_review_skipped")
	if len(skips) != 1 {
		t.Fatalf("plan_review_skipped entries = %d (%+v), want 1", len(skips), skips)
	}
	sk := skips[0]
	if sk.Reason != planreview.ReasonPersonaRemitUnavailable || sk.Persona != personaTestName || sk.Detail != wantDetail {
		t.Errorf("skip = {reason %q persona %q detail %q}, want {%q %q %q}",
			sk.Reason, sk.Persona, sk.Detail, planreview.ReasonPersonaRemitUnavailable, personaTestName, wantDetail)
	}
	if sk.ConfiguredAgents != 2 || sk.Provider != "codex" {
		t.Errorf("skip configured_agents/provider = %d/%q, want 2/codex", sk.ConfiguredAgents, sk.Provider)
	}
	if got := remitInjections(t, p.au); len(got) != 0 {
		t.Errorf("document_injected names the remit %d times for a degraded persona, want 0", len(got))
	}
	stdCalls := reviewerCalls(p.std.(*fakePlanReviewer))
	if len(stdCalls) != 1 {
		t.Fatalf("standard reviewer calls = %d, want 1 — the standard reviewers must still run", len(stdCalls))
	}
	if stdCalls[0] != personaLessPlanPrompt(t) {
		t.Error("standard reviewer prompt differs from the persona-less golden prompt")
	}
	started := decodeStarted(t, p.au, "plan_review_started")
	if started.ConfiguredAgents != 2 || !planreview.Settled(started.ConfiguredAgents, terminalCount(p.au, "plan")) {
		t.Errorf("round does not settle: configured_agents %d, terminal entries %d", started.ConfiguredAgents, terminalCount(p.au, "plan"))
	}
}

// (R3) A remit missing at the recorded admission commit degrades the persona
// with detail remit_missing.
//
// Counterfactual: make the loop's degraded branch fall through to the standard
// prompt — the persona fake is invoked once and this goes RED.
func TestPersona_RemitMissing_DegradesPersonaOnly(t *testing.T) {
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), approvingFake(), approvingFake())
	delete(p.fetcher.files, personaRemitPath)
	if p.review(t) {
		t.Fatal("a degraded persona must not gate the stage")
	}
	assertPersonaDegraded(t, p, personaDetailRemitMissing)
}

// (R4) A run with no recorded admission commit degrades the persona with
// detail run_base_commit_unrecorded and never reads the forge.
//
// Counterfactual: delete the DocumentBaseCommit nil check — repodoc's
// run-admission guard (ErrUnpinnedBaseRef) would still degrade the persona,
// MASKING the deletion on invocation count, which is why the DETAIL is asserted:
// it becomes remit_unresolvable and this goes RED. (With the nil check gone the
// dereference panics first; either way the arm is a control.)
func TestPersona_BaseCommitUnrecorded_DegradesPersonaOnly(t *testing.T) {
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), approvingFake(), approvingFake())
	p.rr.getRuns[p.runID].DocumentBaseCommit = nil
	p.review(t)
	assertPersonaDegraded(t, p, personaDetailBaseCommitUnrecorded)
	if n := p.fetcher.fetches(); n != 0 {
		t.Errorf("forge fetched %d times with no recorded commit, want 0", n)
	}
}

// (R5) No DocumentResolver on the deployment degrades the persona with detail
// document_resolver_unconfigured while the standard reviewer still runs.
//
// Counterfactual: delete the nil-resolver check — Resolve on the nil
// *repodoc.Resolver panics and this goes RED.
func TestPersona_ResolverUnconfigured_DegradesPersonaOnly(t *testing.T) {
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), approvingFake(), approvingFake())
	p.s.cfg.DocumentResolver = nil
	p.review(t)
	assertPersonaDegraded(t, p, personaDetailResolverUnconfigured)
}

// (R6) A document_injected append failure for the remit degrades the persona
// with detail remit_unattributed and discards its prompt. No standard
// declaration is configured, so the remit is the only document attributed.
//
// Counterfactual: ignore the Attribute error — the persona fake is invoked and
// this goes RED.
func TestPersona_RemitUnattributed_DegradesPersonaOnly(t *testing.T) {
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), approvingFake(), approvingFake())
	p.au.appendErrCategory = "document_injected"
	p.review(t)
	assertPersonaDegraded(t, p, personaDetailRemitUnattributed)
}

// A forge failure other than not-found degrades with remit_unresolvable — the
// detail distinguishes "the file is absent" from "the read failed".
//
// Counterfactual: map every Resolve error to remit_missing — this goes RED.
func TestPersona_RemitUnresolvable_DegradesPersonaOnly(t *testing.T) {
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), approvingFake(), approvingFake())
	p.fetcher.err = errors.New("forge 502")
	p.review(t)
	assertPersonaDegraded(t, p, personaDetailRemitUnresolvable)
}

// A credential-scope failure degrades with remit_unresolvable and reads
// nothing.
//
// Counterfactual: drop the DocumentScope error branch (proceed with the zero
// scope) — the remit resolves, the persona runs and this goes RED.
func TestPersona_ScopeFailure_DegradesPersonaOnly(t *testing.T) {
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), approvingFake(), approvingFake())
	p.s.cfg.DocumentScope = func(context.Context, forge.RepoRef) (forge.CredentialScope, error) {
		return forge.CredentialScope{}, errors.New("no installation")
	}
	p.review(t)
	assertPersonaDegraded(t, p, personaDetailRemitUnresolvable)
	if n := p.fetcher.fetches(); n != 0 {
		t.Errorf("forge fetched %d times after a scope failure, want 0", n)
	}
}

// (R9) A persona whose provider this deployment cannot run degrades through
// the standard reviewer_unavailable path stamped with the persona — and its
// remit is never resolved or attributed (it will not run).
//
// Counterfactual: resolve the remit even when resolveErr is set (drop the
// resolveErr skip in buildPersonaPrompts) — a remit document_injected entry
// appears and this goes RED.
func TestPersona_ProviderUnavailable_SkipsWithoutRemitRead(t *testing.T) {
	std := approvingFake()
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), std, nil)
	p.review(t)
	skips := decodeSkipped(t, p.au, "plan_review_skipped")
	if len(skips) != 1 || skips[0].Reason != planreview.ReasonReviewerUnavailable || skips[0].Persona != personaTestName || skips[0].Provider != "codex" {
		t.Fatalf("skips = %+v, want one reviewer_unavailable for persona %q on codex", skips, personaTestName)
	}
	if got := remitInjections(t, p.au); len(got) != 0 {
		t.Errorf("remit attributed %d times for a persona that never runs, want 0", len(got))
	}
	if n := p.fetcher.fetches(); n != 0 {
		t.Errorf("remit fetched %d times for an unavailable persona, want 0", n)
	}
	if n := len(reviewerCalls(std)); n != 1 {
		t.Errorf("standard reviewer calls = %d, want 1", n)
	}
}

// (Condition 2) personas resolve from the stage ACTUALLY under review: with
// two plan stages where only the SECOND attaches the persona, reviewing the
// first runs no persona and reviewing the second runs it.
//
// Counterfactual: replace specStageForRunStage with a first-of-type lookup —
// reviewing the second stage then finds the persona-less first stage and the
// persona is never invoked: RED.
func TestPersona_ResolvesFromReviewedStageNotFirstOfType(t *testing.T) {
	spec := []byte(`version: "2"
reviewer_personas:
  security:
    agent:
      provider: codex
      model: ` + personaAgentModel + `
    remit:
      path: ` + personaRemitPath + `
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
        reviewers:
          human: 0
          agents:
            - provider: anthropic
              model: ` + personaStdModel + `
      - id: plan_again
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        reviewers:
          human: 0
          agents:
            - provider: anthropic
              model: ` + personaStdModel + `
          personas: [security]
`)
	for _, tc := range []struct {
		name        string
		reviewIndex int
		wantPersona int
	}{
		{"first plan stage attaches nothing", 0, 0},
		{"second plan stage attaches the persona", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			persona := approvingFake()
			p := newPersonaPlanRun(t, spec, approvingFake(), persona)
			first := p.rr.getStages[p.stageID]
			first.Sequence = 0
			second := &run.Stage{ID: uuid.New(), RunID: p.runID, Sequence: 1, Type: run.StageTypePlan}
			p.rr.getStages[second.ID] = second
			p.rr.stagesByRunID[p.runID] = []*run.Stage{second, first} // repo order is not trusted
			if tc.reviewIndex == 1 {
				p.stageID = second.ID
			}
			p.review(t)
			if n := len(reviewerCalls(persona)); n != tc.wantPersona {
				t.Errorf("persona calls = %d, want %d", n, tc.wantPersona)
			}
		})
	}
}

// resolveStageReviewerPersonas degrades to NO persona — WARN-logged, never a
// guess — when the reviewed stage cannot be located; and a persona-less spec
// never touches the run repo at all.
func TestResolveStageReviewerPersonas_FailurePaths(t *testing.T) {
	withPersona := personaSpec(personaSpecOpts{attachOn: "plan"})
	cases := []struct {
		name     string
		spec     []byte
		mutate   func(p *personaPlanRun)
		wantWarn string
	}{
		{"persona-less spec reads nothing", personaSpec(personaSpecOpts{}), func(p *personaPlanRun) {
			p.rr.stagesByRunID = nil // ListStagesForRun would error if consulted
			p.rr.stageErr = errors.New("GetStage must not be consulted")
		}, ""},
		{"unknown workflow", withPersona, func(p *personaPlanRun) { p.rr.getRuns[p.runID].WorkflowID = "nope" }, "workflow not in spec"},
		{"reviewed stage unloadable", withPersona, func(p *personaPlanRun) { p.rr.stageErr = errors.New("db down") }, "load reviewed stage"},
		{"run stages unlistable", withPersona, func(p *personaPlanRun) { p.rr.stagesByRunID = nil }, "list run stages"},
		{"stage absent from the run's rows", withPersona, func(p *personaPlanRun) {
			p.rr.stagesByRunID[p.runID] = []*run.Stage{{ID: uuid.New(), RunID: p.runID, Type: run.StageTypePlan}}
		}, "map reviewed stage"},
		{"reviewed stage type has no spec stage", withPersona, func(p *personaPlanRun) {
			p.rr.getStages[p.stageID].Type = run.StageTypeReview
		}, "map reviewed stage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPersonaPlanRun(t, tc.spec, approvingFake(), approvingFake())
			tc.mutate(p)
			got := p.s.resolveStageReviewerPersonas(t.Context(), p.rr.getRuns[p.runID], p.stageID)
			if got != nil {
				t.Errorf("personas = %+v, want nil", got)
			}
			logs := p.logs.String()
			if tc.wantWarn == "" {
				if logs != "" {
					t.Errorf("persona-less spec logged:\n%s", logs)
				}
				return
			}
			if !strings.Contains(logs, "reviewer personas: "+tc.wantWarn) {
				t.Errorf("WARN log missing %q:\n%s", tc.wantWarn, logs)
			}
		})
	}
}

// The persona framing and invocation helpers: promptFor routes a persona to
// its own prompt/tree and a standard reviewer to the shared ones;
// personaFailureReason prefixes only persona reasons; personaNames keeps
// attachment order and is nil for a persona-less round.
func TestPersonaInvocationHelpers(t *testing.T) {
	std := reviewerInvocation{}
	per := reviewerInvocation{persona: &personaInvocation{promptText: "P", treeDir: "T"}}
	per.persona.selected.Name = "security"
	if pr, tr := std.promptFor("S", "ST"); pr != "S" || tr != "ST" {
		t.Errorf("standard promptFor = %q/%q, want the shared S/ST", pr, tr)
	}
	if pr, tr := per.promptFor("S", "ST"); pr != "P" || tr != "T" {
		t.Errorf("persona promptFor = %q/%q, want its own P/T", pr, tr)
	}
	if got := std.personaFailureReason("boom"); got != "boom" {
		t.Errorf("standard reason = %q, want unchanged", got)
	}
	if got := per.personaFailureReason("boom"); got != "persona security: boom" {
		t.Errorf("persona reason = %q", got)
	}
	if got := personaNames([]reviewerInvocation{std, std}); got != nil {
		t.Errorf("persona-less names = %v, want nil", got)
	}
	if got := personaNames([]reviewerInvocation{std, per}); !slices.Equal(got, []string{"security"}) {
		t.Errorf("names = %v", got)
	}
}

// An unparseable run repo ref degrades the persona with remit_unresolvable
// before any forge read (the standard prompt differs here only because the
// repo is rendered into it, so the golden comparison is not applicable).
//
// Counterfactual: drop the parseRepoRef error branch (resolve with the zero
// RepoRef) — the fetcher serves the remit, the persona runs and this goes RED.
func TestPersona_BadRepoRef_DegradesPersonaOnly(t *testing.T) {
	persona := approvingFake()
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), approvingFake(), persona)
	p.rr.getRuns[p.runID].Repo = "not-a-repo-ref"
	p.review(t)
	if n := len(reviewerCalls(persona)); n != 0 {
		t.Errorf("persona invoked %d times, want 0", n)
	}
	skips := decodeSkipped(t, p.au, "plan_review_skipped")
	if len(skips) != 1 || skips[0].Detail != personaDetailRemitUnresolvable {
		t.Errorf("skips = %+v, want one remit_unresolvable", skips)
	}
	if n := p.fetcher.fetches(); n != 0 {
		t.Errorf("fetched %d times with an unparseable repo, want 0", n)
	}
}

// A persona prompt that fails to build degrades with
// persona_prompt_build_failed and writes NO remit attribution (build before
// attribute). Unreachable through the production loop — the persona Trigger is
// the standard one plus a document, and the standard build already succeeded —
// so the helper is driven directly with an unsupported prompt kind.
//
// Counterfactual: attribute before building — a document_injected entry is
// left for a prompt that never existed and this goes RED.
func TestPersona_PromptBuildFailure_DegradesWithoutAttribution(t *testing.T) {
	p := newPersonaPlanRun(t, personaSpec(personaSpecOpts{attachOn: "plan"}), approvingFake(), approvingFake())
	runRow := p.rr.getRuns[p.runID]
	invs := p.s.resolvePersonaInvocations(p.s.resolveStageReviewerPersonas(t.Context(), runRow, p.stageID))
	if len(invs) != 1 {
		t.Fatalf("persona invocations = %d, want 1", len(invs))
	}
	p.s.buildPersonaPrompts(t.Context(), runRow, p.stageID, "no_such_prompt_kind", prompt.Trigger{Repo: runRow.Repo}, nil, "", invs)
	if got := invs[0].persona.degraded; got != personaDetailPromptBuildFailed {
		t.Errorf("degraded = %q, want %q", got, personaDetailPromptBuildFailed)
	}
	if invs[0].persona.promptText != "" {
		t.Error("a failed build left a prompt behind")
	}
	if n := len(remitInjections(t, p.au)); n != 0 {
		t.Errorf("remit attributed %d times for a prompt that never built, want 0", n)
	}
}
