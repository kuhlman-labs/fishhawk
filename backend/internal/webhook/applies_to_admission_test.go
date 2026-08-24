package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/appliesto"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Coverage for the webhook applies_to admission gate (E53.10 / #2361). The gate
// is a fail-closed governance control, so per operator condition 1 / #1199 each
// enumerated fail-closed / degrade branch gets its OWN assertion — the happy
// path plus a subset is the shape that ships a control which admits (or refuses)
// everything and still passes.

// appliesToSpecYAML builds a two-workflow v2 spec: `feature_change` (the
// dispatcher's DefaultWorkflowID) carries the given applies_to block, and
// `routine_change` declares none so it is always a satisfying alternative. Both
// are minimal but real (plan + implement) so spec.ParseBytes accepts them.
func appliesToSpecYAML(appliesToBlock string) string {
	fc := "  feature_change:\n"
	if appliesToBlock != "" {
		fc += appliesToBlock
	}
	fc += `    stages:
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
        produces:
          - artifact: pull_request
`
	return "version: \"2\"\nworkflows:\n" + fc + `  routine_change:
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
        produces:
          - artifact: pull_request
`
}

// parseFeatureChange parses yaml and returns the spec + its feature_change
// workflow, failing the test on a parse error or a missing workflow.
func parseFeatureChange(t *testing.T, yaml string) (*spec.Spec, spec.Workflow) {
	t.Helper()
	parsed, err := spec.ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	wf, ok := parsed.Workflows[DefaultWorkflowID]
	if !ok {
		t.Fatalf("workflow %q missing from parsed spec", DefaultWorkflowID)
	}
	return parsed, wf
}

// labeledRunMatch builds a MatchActionRun Match for an issue-labeled trigger
// carrying the given forge-authoritative issue labels.
func labeledRunMatch(issueNumber int, labels ...string) Match {
	return Match{
		Action:        MatchActionRun,
		WorkflowID:    DefaultWorkflowID,
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    "issue:1",
		IssueRef:      &IssueRef{Number: issueNumber, IssueLabels: labels},
	}
}

// appliesToEvent is the run-less Event the direct-call gate tests pass; the gate
// reads only Repo / DeliveryID / InstallationID / Type off it.
func appliesToEvent() Event {
	return Event{Type: "issues", Action: "labeled", Repo: "o/r", DeliveryID: "d1", InstallationID: 42}
}

// countGlobalCategory counts run_rejected_applies_to-style global appends of a
// category recorded on the stub audit.
func countGlobalCategory(au *stubAudit, category string) int {
	au.mu.Lock()
	defer au.mu.Unlock()
	n := 0
	for _, e := range au.globalAppended {
		if e.Category == category {
			n++
		}
	}
	return n
}

// firstGlobalPayload decodes the payload of the first global append of a
// category, or fails the test.
func firstGlobalPayload(t *testing.T, au *stubAudit, category string) map[string]any {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	for _, e := range au.globalAppended {
		if e.Category == category {
			var m map[string]any
			if err := json.Unmarshal(e.Payload, &m); err != nil {
				t.Fatalf("decode %s payload: %v", category, err)
			}
			return m
		}
	}
	t.Fatalf("no %s global append recorded", category)
	return nil
}

// --- Refuse: an unsatisfied labels criterion ---

func TestAppliesToWebhook_UnsatisfiedLabels_Refuses(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	m := labeledRunMatch(7, "bug") // carries a label, but not `docs`

	if !d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("an unsatisfied labels criterion must refuse")
	}
	if n := countGlobalCategory(au, "run_rejected_applies_to"); n != 1 {
		t.Fatalf("run_rejected_applies_to entries = %d, want exactly 1", n)
	}
	payload := firstGlobalPayload(t, au, "run_rejected_applies_to")
	if payload["criterion"] != "labels" {
		t.Errorf("payload.criterion = %v, want labels", payload["criterion"])
	}
	if len(notifier.notApplicableCalls) != 1 {
		t.Fatalf("NotifyRunNotApplicable calls = %d, want 1", len(notifier.notApplicableCalls))
	}
	if msg := notifier.notApplicableCalls[0].message; !strings.Contains(msg, "labels") || !strings.Contains(msg, "routine_change") {
		t.Errorf("comment message %q must name the labels criterion and the satisfying workflow", msg)
	}
	// The webhook tail must never advertise applies_to_override (condition 3).
	if strings.Contains(notifier.notApplicableCalls[0].message, "applies_to_override") {
		t.Errorf("webhook refusal message must not mention applies_to_override:\n%s", notifier.notApplicableCalls[0].message)
	}
}

// --- Refuse: an UNLABELLED issue reports the observed value with the
// issue-context-PRESENT sentence, not the issue-less one ---

func TestAppliesToWebhook_UnlabelledIssue_ReportsTheObservedValue(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	// A /fishhawk run on an issue carrying zero labels: issue context is
	// PRESENT, the label set is empty.
	m := labeledRunMatch(9) // no labels at all

	if !d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("an unlabelled issue must refuse a labels criterion")
	}
	if len(notifier.notApplicableCalls) != 1 {
		t.Fatalf("NotifyRunNotApplicable calls = %d, want 1", len(notifier.notApplicableCalls))
	}
	msg := notifier.notApplicableCalls[0].message
	if strings.Contains(msg, "NO issue context") {
		t.Errorf("message %q claims NO issue context, but the issue is present with zero labels — the two have different fixes", msg)
	}
	if !strings.Contains(msg, "carries no labels at all") {
		t.Errorf("message %q must use the unlabelled-issue sentence", msg)
	}
	if !strings.Contains(msg, "the change's labels is []") {
		t.Errorf("message %q must report the observed value as []", msg)
	}
	payload := firstGlobalPayload(t, au, "run_rejected_applies_to")
	if payload["absent_issue_context"] != true {
		t.Errorf("payload.absent_issue_context = %v, want true", payload["absent_issue_context"])
	}
}

// --- Refuse: an unsatisfied trigger criterion (independent of labels) ---

func TestAppliesToWebhook_UnsatisfiedTrigger_Refuses(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier
	// trigger: [scheduled]; a github_issue run maps to `diff`, so it fails.
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML("    applies_to:\n      trigger:\n        - scheduled\n"))
	m := labeledRunMatch(3, "docs") // labels are irrelevant to a trigger criterion

	if !d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("an unsatisfied trigger criterion must refuse")
	}
	payload := firstGlobalPayload(t, au, "run_rejected_applies_to")
	if payload["criterion"] != "trigger" {
		t.Errorf("payload.criterion = %v, want trigger", payload["criterion"])
	}
}

// --- Fail-closed: the admission gate cannot produce a Match error (the
// inverted-guard tripwire) ---
//
// spec.Predicate.Match has exactly two error modes: an EMPTY predicate and a
// malformed `paths` glob. ParseBytes runs Validate, which rejects both before
// the gate ever sees the spec, and the ADMISSION sub-predicate never carries
// `paths` (it is the plan gate's criterion) — so no declaration reachable here
// can make Match error, exactly as the server admission gate cannot
// (server.TestAppliesTo_AdmissionPhase_CannotProduceAMatchError). The
// `if matchErr != nil { refuse }` branch in refusedByAppliesTo is therefore
// defense in depth, and this test is the tripwire on the reasoning: if a future
// phase split routes `paths` — or any error-capable criterion — into the
// admission half, Match errors become reachable at admission and this fails,
// forcing whoever made that change to cover the fail-closed branch for real
// (and to reject the tempting `if err != nil { admit }` inverted guard).
func TestAppliesToWebhook_UnevaluablePredicate_FailsClosed(t *testing.T) {
	for _, p := range []spec.Predicate{
		{Labels: []string{"docs"}},
		{Triggers: []spec.TriggerForm{spec.TriggerScheduled}},
		{Labels: []string{"docs"}, Paths: []string{"docs/["}}, // malformed paths glob
		{Paths: []string{"docs/["}},
		{},
	} {
		sub, constrains := appliesto.PhasePredicate(p, appliesto.PhaseAdmission)
		if len(sub.Paths) > 0 {
			t.Fatalf("the admission sub-predicate of %+v carries paths; Match can now error at admission and refusedByAppliesTo's matchErr branch needs a real behavioral test", p)
		}
		if !constrains {
			continue // Match is never called — the gate admits without evaluating.
		}
		c := appliesto.AdmissionChange(string(run.TriggerGitHubIssue), []string{"bug"})
		if _, err := sub.Match(c); err != nil {
			t.Fatalf("admission sub-predicate %+v errored (%v); the matchErr branch is now reachable and must be driven through refusedByAppliesTo", sub, err)
		}
	}
}

// --- Fail-closed: the MatchErr branch's OBSERVABLE effects, driven behaviorally ---
//
// refusedByAppliesTo's matchErr branch cannot be exercised end to end: the
// admission sub-predicate never carries an error-capable criterion (paths is
// deferred to the plan gate; the empty predicate is filtered by `constrains`
// before Match is called), which is exactly what the structural tripwire above
// proves. So a Match error cannot reach the gate through a real spec, and no
// call to refusedByAppliesTo can produce one. But the branch has two OBSERVABLE
// effects that must not be left to the tripwire's promise — the "could not be
// evaluated" fail-closed refusal MESSAGE and the evaluation_error AUDIT payload
// field — and both are behavioral. This drives a Match error through the webhook
// seam's renderer and its audit builder and asserts each, mirroring the accepted
// server precedent (server.TestAppliesTo_M13_MatchError_FailsClosed).
func TestAppliesToWebhook_MatchError_RendersEvaluationErrorAndRefusesShape(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	matchErr := errors.New(`path predicate: malformed glob "docs/["`)

	// (1) The webhook seam renders a Match error as a REFUSAL, never an admit —
	// a declaration that cannot be evaluated is never treated as match-all.
	rj := appliesto.Rejection{
		WorkflowID: DefaultWorkflowID,
		MatchErr:   matchErr,
		Phase:      appliesto.PhaseAdmission,
		Seam:       appliesto.SeamWebhook,
	}
	msg := appliesto.RenderRejection(rj)
	if !strings.Contains(msg, "could not be evaluated") || !strings.Contains(msg, "never treated as match-all") {
		t.Errorf("a MatchErr must render as a fail-closed refusal; got %q", msg)
	}
	// The webhook tail still must not advertise an override on this path either.
	if strings.Contains(msg, "applies_to_override") {
		t.Errorf("webhook MatchErr message must not mention applies_to_override:\n%s", msg)
	}

	// (2) The refusal audit carries the evaluation_error payload field naming the
	// cause — the observable a downstream operator uses to distinguish an
	// unevaluable declaration from an ordinary unsatisfied criterion.
	d.appendAppliesToRejectedAudit(context.Background(), appliesToEvent(), labeledRunMatch(1, "bug"), rj, msg, "sha", d.now())
	payload := firstGlobalPayload(t, au, "run_rejected_applies_to")
	if payload["evaluation_error"] != matchErr.Error() {
		t.Errorf("payload.evaluation_error = %v, want %q", payload["evaluation_error"], matchErr.Error())
	}
}

// --- Admit: no applies_to declaration (back-compat) ---

func TestAppliesToWebhook_NoDeclaration_Admits(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML(""))
	m := labeledRunMatch(1, "anything")

	if d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("a workflow with no applies_to must admit")
	}
	if n := countGlobalCategory(au, "run_rejected_applies_to"); n != 0 {
		t.Errorf("run_rejected_applies_to entries = %d, want 0 on admit", n)
	}
	if len(notifier.notApplicableCalls) != 0 {
		t.Errorf("NotifyRunNotApplicable calls = %d, want 0 on admit", len(notifier.notApplicableCalls))
	}
}

// --- Admit at admission: a paths-only declaration is deferred to the plan
// gate (the positive proof `paths` is not evaluated against zero paths) ---

func TestAppliesToWebhook_PathsOnly_AdmitsAtAdmission(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML("    applies_to:\n      paths:\n        - \"docs/**\"\n"))
	m := labeledRunMatch(1, "bug")

	if d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("a paths-only declaration must admit at the webhook admission gate; paths is deferred to the plan gate")
	}
	if n := countGlobalCategory(au, "run_rejected_applies_to"); n != 0 {
		t.Errorf("run_rejected_applies_to entries = %d, want 0 (paths deferred, not evaluated against zero paths)", n)
	}
}

// --- Admit: satisfied labels ---

func TestAppliesToWebhook_SatisfiedLabels_Dispatches(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	m := labeledRunMatch(1, "docs", "bug") // carries the required `docs`

	if d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("a run whose labels satisfy the declaration must admit")
	}
	if n := countGlobalCategory(au, "run_rejected_applies_to"); n != 0 {
		t.Errorf("run_rejected_applies_to entries = %d, want 0 on the happy path", n)
	}
	if len(notifier.notApplicableCalls) != 0 {
		t.Errorf("NotifyRunNotApplicable calls = %d, want 0 on the happy path", len(notifier.notApplicableCalls))
	}
}

// --- Degrade: a nil audit repository leaves the refusal intact ---

func TestAppliesToWebhook_AuditUnavailable_StillRefuses(t *testing.T) {
	d, _, _, _ := newDispatcherWithStubs(t)
	d.Audit = nil // no audit repository wired
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	m := labeledRunMatch(1, "bug")

	if !d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("a nil audit repository must NOT soften the refusal")
	}
	// The refusal still surfaces to the customer even when it cannot be audited.
	if len(notifier.notApplicableCalls) != 1 {
		t.Errorf("NotifyRunNotApplicable calls = %d, want 1", len(notifier.notApplicableCalls))
	}
}

// --- Degrade: an audit append error leaves the refusal intact ---

func TestAppliesToWebhook_AuditAppendFailure_StillRefuses(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	logs := captureDispatcherLogs(d)
	au.appendErr = errAuditDown
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	m := labeledRunMatch(1, "bug")

	if !d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("an audit append failure must NOT soften the refusal")
	}
	if len(notifier.notApplicableCalls) != 1 {
		t.Errorf("NotifyRunNotApplicable calls = %d, want 1 — the comment still fires despite the audit failure", len(notifier.notApplicableCalls))
	}
	// The approval condition's other observable for a degraded path: the failure
	// is LOGGED. A regression deleting this warning would otherwise pass the
	// refusal-survives assertion above.
	if got := logs.String(); !strings.Contains(got, "append run_rejected_applies_to audit entry failed") || !strings.Contains(got, errAuditDown.Error()) {
		t.Errorf("audit append failure must be warn-logged (message + cause); logs:\n%s", got)
	}
}

// --- Degrade: a nil notifier leaves the refusal intact ---

func TestAppliesToWebhook_NotifierNil_StillRefuses(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	d.IssueNotifier = nil // no notifier wired
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	m := labeledRunMatch(1, "bug")

	if !d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("a nil notifier must NOT soften the refusal")
	}
	// The refusal is still audited even when it cannot be surfaced.
	if n := countGlobalCategory(au, "run_rejected_applies_to"); n != 1 {
		t.Errorf("run_rejected_applies_to entries = %d, want 1", n)
	}
}

// --- Degrade: a nil IssueRef refuses (fail-closed) and skips the comment ---
//
// Both real entry points (matchIssue, matchIssueComment) always populate
// IssueRef on a MatchActionRun, so this is defense in depth. A nil ref yields an
// empty label set — a fail-closed refusal against a labels criterion — and the
// comment gate is skipped because there is no issue to comment on. The
// empty-set-refuses AND comment-skips combination is asserted nowhere else.
func TestAppliesToWebhook_NilIssueRef_RefusesAndSkipsComment(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	m := Match{
		Action:        MatchActionRun,
		WorkflowID:    DefaultWorkflowID,
		TriggerSource: run.TriggerGitHubIssue,
		TriggerRef:    "issue:1",
		IssueRef:      nil, // no issue reference at all
	}

	if !d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("a nil IssueRef must fail closed: an empty label set cannot satisfy a labels criterion")
	}
	// The refusal is still audited...
	if n := countGlobalCategory(au, "run_rejected_applies_to"); n != 1 {
		t.Errorf("run_rejected_applies_to entries = %d, want 1", n)
	}
	// ...but no comment is posted: there is no issue to comment on.
	if len(notifier.notApplicableCalls) != 0 {
		t.Errorf("NotifyRunNotApplicable calls = %d, want 0 (no IssueRef to comment on)", len(notifier.notApplicableCalls))
	}
}

// --- Degrade: a notifier post error leaves the refusal intact ---

func TestAppliesToWebhook_NotifierError_StillRefuses(t *testing.T) {
	d, _, _, au := newDispatcherWithStubs(t)
	logs := captureDispatcherLogs(d)
	notifier := &stubIssueNotifier{err: errAuditDown}
	d.IssueNotifier = notifier
	parsed, wf := parseFeatureChange(t, appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	m := labeledRunMatch(1, "bug")

	if !d.refusedByAppliesTo(context.Background(), appliesToEvent(), m, wf, parsed, forge.FromGitHubInstallationID(42), "sha", d.now()) {
		t.Fatal("a notifier post failure must NOT soften the refusal")
	}
	if len(notifier.notApplicableCalls) != 1 {
		t.Errorf("NotifyRunNotApplicable calls = %d, want 1 (the post was attempted)", len(notifier.notApplicableCalls))
	}
	if n := countGlobalCategory(au, "run_rejected_applies_to"); n != 1 {
		t.Errorf("run_rejected_applies_to entries = %d, want 1", n)
	}
	// The approval condition's other observable for a degraded path: the comment
	// failure is LOGGED. A regression deleting this warning would otherwise pass
	// the refusal-survives assertion above.
	if got := logs.String(); !strings.Contains(got, "run-not-applicable comment failed") || !strings.Contains(got, errAuditDown.Error()) {
		t.Errorf("notifier post failure must be warn-logged (message + cause); logs:\n%s", got)
	}
}

// --- CROSS-BOUNDARY INTEGRATION TEST: match → spec parse → shared core →
// audit → notifier, driven through the real Dispatcher.Handle ---
//
// Asserts in ONE test that a webhook whose labels do not satisfy the workflow's
// applies_to (a) never calls CreateRun, (b) writes exactly one
// run_rejected_applies_to GLOBAL entry carrying criterion=labels + the observed
// set + satisfying_workflows, (c) posts exactly one NotifyRunNotApplicable
// comment to the triggering issue, and (d) returns nil (202, no GitHub
// redelivery). Per-layer units alone would pass while this seam broke.
func TestDispatcherHandle_AppliesToRefusal_EndToEnd(t *testing.T) {
	d, gh, runs, au := newDispatcherWithStubs(t)
	gh.specContent = []byte(appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier

	// The issueLabeledEvent fixture carries the `fishhawk` trigger label and no
	// issue.labels[], so the captured set is [fishhawk] — which does not satisfy
	// applies_to.labels: [docs].
	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle returned non-nil (must return nil so GitHub doesn't retry): %v", err)
	}

	// (a) no run row.
	if len(runs.created) != 0 {
		t.Errorf("runs created = %d, want 0 (refused before CreateRun)", len(runs.created))
	}
	// (b) exactly one global run_rejected_applies_to entry with the expected shape.
	if n := countGlobalCategory(au, "run_rejected_applies_to"); n != 1 {
		t.Fatalf("run_rejected_applies_to entries = %d, want exactly 1", n)
	}
	payload := firstGlobalPayload(t, au, "run_rejected_applies_to")
	if payload["criterion"] != "labels" {
		t.Errorf("payload.criterion = %v, want labels", payload["criterion"])
	}
	observed, _ := payload["observed"].([]any)
	if len(observed) != 1 || observed[0] != "fishhawk" {
		t.Errorf("payload.observed = %v, want [fishhawk] (the forge-authoritative set)", payload["observed"])
	}
	sat, _ := payload["satisfying_workflows"].([]any)
	if len(sat) != 1 || sat[0] != "routine_change" {
		t.Errorf("payload.satisfying_workflows = %v, want [routine_change]", payload["satisfying_workflows"])
	}
	// (c) exactly one comment on the triggering issue.
	if len(notifier.notApplicableCalls) != 1 {
		t.Fatalf("NotifyRunNotApplicable calls = %d, want 1", len(notifier.notApplicableCalls))
	}
	if got := notifier.notApplicableCalls[0].issueNumber; got != 1247 {
		t.Errorf("comment issue number = %d, want 1247", got)
	}
	// No status-comment or dispatch happened.
	if len(notifier.statusCalls) != 0 {
		t.Errorf("status comment calls = %d, want 0 on a refusal", len(notifier.statusCalls))
	}
}

// TestDispatcherHandle_AppliesTo_RunsBeforeProtectionGate pins the ORDERING
// (operator condition 2): the applies_to gate runs BEFORE the branch-protection
// snapshot gate. A refusing applies_to declaration short-circuits Handle before
// resolveRequiredChecks is ever called, so GetBranchProtection is never queried
// — the discriminator that proves the gate precedes protection rather than
// following it.
func TestDispatcherHandle_AppliesTo_RunsBeforeProtectionGate(t *testing.T) {
	d, gh, runs, au := newDispatcherWithStubs(t)
	gh.specContent = []byte(appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	// Make the protection gate ALSO fail, so that whichever gate runs first
	// decides the outcome. If protection ran first it would refuse and write no
	// run_rejected_applies_to entry; asserting that entry exists pins the order.
	gh.branchProtection = &forge.BranchProtection{}
	gh.rulesets = nil
	d.IssueNotifier = &stubIssueNotifier{}

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle returned non-nil: %v", err)
	}
	if len(runs.created) != 0 {
		t.Errorf("runs created = %d, want 0", len(runs.created))
	}
	if n := countGlobalCategory(au, "run_rejected_applies_to"); n != 1 {
		t.Fatalf("run_rejected_applies_to entries = %d, want 1 — applies_to must run BEFORE the protection gate", n)
	}
	if gh.branchProtectionCalls != 0 {
		t.Errorf("GetBranchProtection called %d times; applies_to must short-circuit BEFORE the protection snapshot", gh.branchProtectionCalls)
	}
}

// TestDispatcherHandle_AppliesTo_SatisfiedDispatches is the positive companion
// to the end-to-end refusal: a run whose forge labels satisfy the declaration
// dispatches through the full pipeline (protection gate runs, CreateRun fires),
// and writes no rejection entry.
func TestDispatcherHandle_AppliesTo_SatisfiedDispatches(t *testing.T) {
	d, gh, runs, au := newDispatcherWithStubs(t)
	gh.specContent = []byte(appliesToSpecYAML("    applies_to:\n      labels:\n        - docs\n"))
	d.IssueNotifier = &stubIssueNotifier{}

	if err := d.Handle(context.Background(), issueLabeledEventWithLabels(t, "docs")); err != nil {
		t.Fatalf("Handle returned non-nil: %v", err)
	}
	if len(runs.created) != 1 {
		t.Fatalf("runs created = %d, want 1 (satisfied labels dispatch)", len(runs.created))
	}
	if n := countGlobalCategory(au, "run_rejected_applies_to"); n != 0 {
		t.Errorf("run_rejected_applies_to entries = %d, want 0", n)
	}
	if gh.branchProtectionCalls == 0 {
		t.Error("GetBranchProtection was never called; on the admit path the protection gate must run")
	}
}

// errAuditDown is the injected failure the degrade tests use.
var errAuditDown = errors.New("audit down")

// issueLabeledEventWithLabels builds an issues.labeled event whose issue object
// carries the given labels (plus the triggering `fishhawk` label), so the
// dispatcher captures a forge-authoritative set that satisfies an applies_to
// labels declaration.
func issueLabeledEventWithLabels(t *testing.T, labels ...string) Event {
	t.Helper()
	labelObjs := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		labelObjs = append(labelObjs, map[string]any{"name": l})
	}
	body, _ := json.Marshal(map[string]any{
		"action": "labeled",
		"issue": map[string]any{
			"number": 1247,
			"labels": labelObjs,
		},
		"label": map[string]any{"name": "fishhawk"},
		"repository": map[string]any{
			"full_name": "kuhlman-labs/fishhawk",
		},
		"installation": map[string]any{"id": 42},
		"sender":       map[string]any{"login": "alice", "type": "User"},
	})
	ev, err := ParseEvent("issues", "deliv-labels", body)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// --- GitLab creation path (E45.22 / #2043) ---

// TestAppliesToWebhook_GitLabTrigger_GatesTheGitLabCreationPath pins that the
// applies_to routing gate holds for a GitLab-triggered run exactly as it does
// for a GitHub one — the gate is not forge-aware, and the GitLab create path
// calls it in the same position (before any run row exists).
//
// The refusing cell also documents a real consequence of the GitLab matcher:
// matchGitLabIssue captures no forge label set, so a `labels` declaration
// evaluates against an EMPTY set and fails CLOSED. That is the correct
// posture — a routing declaration that cannot be evaluated must not admit —
// and the admitting cell proves the refusal is not simply "GitLab never runs".
func TestAppliesToWebhook_GitLabTrigger_GatesTheGitLabCreationPath(t *testing.T) {
	cases := []struct {
		name        string
		appliesTo   string
		wantCreated int
		wantRefused int
	}{
		{
			"labels_declaration_fails_closed_on_gitlab",
			"    applies_to:\n      labels:\n        - docs\n",
			0, 1,
		},
		{
			"no_declaration_admits",
			"",
			1, 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			specYAML := appliesToSpecYAML(tc.appliesTo)
			d, _, runs, au := newDispatcherWithStubs(t)
			d.GitLabFiles = &stubFileFetcher{content: []byte(specYAML), sha: "g1t1absha"}

			if err := d.Handle(context.Background(), gitlabIssueTriggerEvent()); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if len(runs.created) != tc.wantCreated {
				t.Errorf("runs created = %d, want %d", len(runs.created), tc.wantCreated)
			}
			if n := countGlobalCategory(au, "run_rejected_applies_to"); n != tc.wantRefused {
				t.Errorf("run_rejected_applies_to entries = %d, want %d", n, tc.wantRefused)
			}
		})
	}
}
