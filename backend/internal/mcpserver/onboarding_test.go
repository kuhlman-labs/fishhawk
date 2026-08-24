package mcpserver

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectInMemory builds a server the same way newServer does
// (buildServer + registerTools + registerOnboardingResources), connects an
// in-memory client/server pair, and returns the live client session. This is
// the stdio-equivalent round-trip: the in-memory transport exercises the same
// registration->transport seam the StdioTransport does.
func connectInMemory(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok-test"}
	srv := buildServer(cfg)
	registerTools(srv, &runResolver{api: newAPIClient(cfg), getenv: envFunc(nil)})
	registerOnboardingResources(srv)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "onboarding-probe", Version: "0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

// TestOnboardingContent_NonEmpty is the fail-fast unit guard: a renamed or
// missing runbook.md embed (or an emptied instructions const) trips here
// before the slower round-trip tests, with an actionable message.
func TestOnboardingContent_NonEmpty(t *testing.T) {
	if strings.TrimSpace(onboardingInstructions) == "" {
		t.Error("onboardingInstructions is empty — the initialize instructions field would be blank")
	}
	if strings.TrimSpace(runbookMarkdown) == "" {
		t.Error("runbookMarkdown is empty — runbook.md failed to embed (renamed or missing?)")
	}
}

// TestOnboarding_InstructionsDeliveredOnInitialize asserts the server
// instructions reach the client verbatim on the handshake and carry the
// happy-path verb anchors — a behavioral done-means check, so an empty/stub
// instructions string fails where a mere presence gate would pass.
func TestOnboarding_InstructionsDeliveredOnInitialize(t *testing.T) {
	cs := connectInMemory(t)
	got := cs.InitializeResult().Instructions
	if strings.TrimSpace(got) == "" {
		t.Fatal("InitializeResult().Instructions is empty; want the onboarding guide")
	}
	for _, anchor := range []string{
		"fishhawk_start_run",
		"fishhawk_approve_plan",
		"fishhawk_dispatch_stage",
		// E31.9: the acceptance happy-path line + gate-semantics bullet.
		"acceptance_passed",
		"acceptance stage",
		// #2347: the happy-path line must name the not-validated merge-eligible
		// state too, and carry the acknowledgement ask — otherwise an operator
		// reading the onboarding guide is told to merge only on acceptance_passed
		// and treats a short-circuited run as blocked (or as a pass).
		"acceptance_not_validated",
		"acknowledge that in your merge verdict",
		// #2512: the same argument for the undecidable state. An operator whose
		// onboarding guide omits it reads a merge-eligible run as blocked and
		// goes looking for an arbitration that does not exist, which is the
		// wedge this change removes rather than adds.
		"acceptance_undecidable",
		"say which criteria went undecided in your merge verdict",
		// E34.4: the refinement intake one-liner names the tool.
		"fishhawk_draft_epic",
		runbookURI,
	} {
		if !strings.Contains(got, anchor) {
			t.Errorf("instructions missing happy-path anchor %q", anchor)
		}
	}
}

// TestOnboardingInstructions_DirectTheAgentToResolveItsOwnCheckout is the
// Half-2 DONE-MEANS for the onboarding surface (E66.42 / #2482): the
// onboardingInstructions string must carry THREE separate claims about
// working_dir — the requirement on start_run, the instruction that the CALLING
// AGENT resolves its OWN checkout, and that the later verbs INHERIT it — as
// three independent assertions so a half-edit fails where a presence gate would
// pass.
func TestOnboardingInstructions_DirectTheAgentToResolveItsOwnCheckout(t *testing.T) {
	got := onboardingInstructions
	lower := strings.ToLower(got)

	// (1) The working_dir requirement on start_run, stated as an ABSOLUTE path
	// (dropping the absolute-path requirement fails here, not just a bare
	// mention of the field).
	if !strings.Contains(lower, "working_dir") {
		t.Errorf("onboardingInstructions must name working_dir on start_run; got:\n%s", got)
	}
	if !strings.Contains(lower, "absolute") {
		t.Errorf("onboardingInstructions must state working_dir is an ABSOLUTE path; got:\n%s", got)
	}
	// (2) The calling agent resolves its OWN checkout.
	if !strings.Contains(lower, "resolve your own checkout") {
		t.Errorf("onboardingInstructions must instruct the calling agent to resolve its OWN checkout; got:\n%s", got)
	}
	// (3) The later verbs INHERIT it.
	if !strings.Contains(lower, "inherit") {
		t.Errorf("onboardingInstructions must state the later verbs inherit the binding; got:\n%s", got)
	}
}

// TestOnboardingInstructions_NameAwaitStage is the #2491 onboarding done-means:
// the initialize instructions must (a) name fishhawk_await_stage on the
// dispatch step and (b) carry the backgrounding-client trade-off clause — as
// two independent assertions, so a half-edit that names the verb but drops the
// trade-off (or vice versa) fails where a bare presence gate would pass.
func TestOnboardingInstructions_NameAwaitStage(t *testing.T) {
	got := onboardingInstructions
	lower := strings.ToLower(got)
	// (a) The dispatch step names the new terminal-wait verb.
	if !strings.Contains(got, "fishhawk_await_stage") {
		t.Errorf("onboardingInstructions must name fishhawk_await_stage on the dispatch step; got:\n%s", got)
	}
	// (b) The backgrounding-client trade-off is stated as a separate clause.
	if !strings.Contains(lower, "backgrounds long") {
		t.Errorf("onboardingInstructions must state the backgrounding-client trade-off (a client that BACKGROUNDS long tool calls); got:\n%s", got)
	}
	if !strings.Contains(lower, "in-band mid-stage scope-amendment channel") {
		t.Errorf("onboardingInstructions must state dispatch_stage's advantage is its in-band amendment channel; got:\n%s", got)
	}
}

// TestOnboardingInstructions_AwaitStageObservesAmendments is the #2588
// onboarding pin: the instructions must state that the amendment channel is
// OBSERVABLE from the recommended wait (status amendment_pending, no separate
// fishhawk_list_scope_amendments poll cycle). Before #2588 the claim would have
// been false — await_stage never released on an amendment — so a revert of the
// prose fails here rather than leaving the operator hand-polling.
func TestOnboardingInstructions_AwaitStageObservesAmendments(t *testing.T) {
	got := onboardingInstructions
	lower := strings.ToLower(got)
	if !strings.Contains(lower, "amendment_pending") {
		t.Errorf("onboardingInstructions must name the amendment_pending release status; got:\n%s", got)
	}
	if !strings.Contains(lower, "no separate fishhawk_list_scope_amendments poll") {
		t.Errorf("onboardingInstructions must state no separate fishhawk_list_scope_amendments poll cycle is needed; got:\n%s", got)
	}
}

// TestStartRunSchema_WorkingDirInstructsTheCaller walks the schema ACTUALLY
// REGISTERED on the server via a tools/list round-trip (not jsonschema.For on
// the Go struct — so a registration that overrides the inferred schema is
// caught too, #2482 concern 5) and asserts the working_dir description carries
// the requirement SEMANTICS, not just the substring "required": the
// absolute-path requirement, an un-negated required-ness (a rewording to "not
// required"/"optional" fails), the calling-agent-resolves-its-own-checkout
// instruction, and the later-verbs-inherit claim. This is the surface an agent
// actually reads (E66.42 / #2482).
func TestStartRunSchema_WorkingDirInstructsTheCaller(t *testing.T) {
	ctx := context.Background()
	cs := connectInMemory(t)
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var schema any
	for _, tool := range list.Tools {
		if tool.Name == "fishhawk_start_run" {
			schema = tool.InputSchema
			break
		}
	}
	if schema == nil {
		t.Fatal("fishhawk_start_run not registered/visible over ListTools")
	}
	// Over the wire the SDK decodes Tool.InputSchema (typed `any`) into a
	// map[string]any, so walk the registered JSON Schema object directly.
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("registered start_run InputSchema is %T, want a JSON object map", schema)
	}
	props, ok := schemaMap["properties"].(map[string]any)
	if !ok {
		t.Fatalf("registered start_run schema has no properties object; got %v", schemaMap["properties"])
	}
	wd, ok := props["working_dir"].(map[string]any)
	if !ok {
		t.Fatal("working_dir property missing from the registered start_run schema")
	}
	descRaw, _ := wd["description"].(string)
	desc := strings.ToLower(descRaw)

	// (1) The absolute-path requirement — removing it fails here.
	if !strings.Contains(desc, "absolute path") {
		t.Errorf("working_dir description must state it is an ABSOLUTE path; got %q", descRaw)
	}
	// (2) Required-ness present AND not negated — a rewording to "not
	// required"/"optional" fails despite still containing the substring
	// "required".
	if !strings.Contains(desc, "required") {
		t.Errorf("working_dir description must state it is REQUIRED (over HTTP for a local run); got %q", descRaw)
	}
	if strings.Contains(desc, "not required") || strings.Contains(desc, "optional") {
		t.Errorf("working_dir description must not describe the field as optional / not required; got %q", descRaw)
	}
	// (3) The calling agent resolves its OWN checkout.
	if !strings.Contains(desc, "resolve your own checkout") {
		t.Errorf("working_dir description must instruct the calling agent to resolve its OWN checkout; got %q", descRaw)
	}
	// (4) The later verbs INHERIT it.
	if !strings.Contains(desc, "inherit") {
		t.Errorf("working_dir description must state the later verbs inherit it; got %q", descRaw)
	}
}

// TestOnboarding_RunbookResourceListedAndReadable asserts the runbook
// resource crosses the registration->transport seam: it is listable and its
// read returns non-empty text/markdown carrying the edge-case anchors the
// binding conditions require.
func TestOnboarding_RunbookResourceListedAndReadable(t *testing.T) {
	ctx := context.Background()
	cs := connectInMemory(t)

	list, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	found := false
	for _, r := range list.Resources {
		if r.URI == runbookURI {
			found = true
			if r.MIMEType != "text/markdown" {
				t.Errorf("runbook MIMEType = %q, want text/markdown", r.MIMEType)
			}
		}
	}
	if !found {
		t.Fatalf("ListResources did not include %s", runbookURI)
	}

	res, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: runbookURI})
	if err != nil {
		t.Fatalf("ReadResource(%s): %v", runbookURI, err)
	}
	if len(res.Contents) == 0 {
		t.Fatal("ReadResource returned no contents")
	}
	c := res.Contents[0]
	if c.MIMEType != "text/markdown" {
		t.Errorf("content MIMEType = %q, want text/markdown", c.MIMEType)
	}
	if strings.TrimSpace(c.Text) == "" {
		t.Fatal("runbook content is empty")
	}
	// Edge-case anchors the binding conditions require the runbook to carry.
	for _, anchor := range []string{
		"runner_kind:local",
		"fixup",
		"scope amendment",
		"review",
		"clean",
		// E31.9 acceptance-stage playbook anchors.
		"Acceptance stage",
		"acceptance_passed",
		"retry_dispatched",
		// #2347: the runbook's merge-eligible-state list + the not-validated
		// section (zero criteria verified, say so in the merge verdict).
		"acceptance_not_validated",
		"succeeded_acceptance_not_validated",
		"say so in your merge verdict",
		// #2512: the runbook's merge-eligible-state list + the undecidable
		// section. The three anchors are the section's load-bearing claims —
		// the state name, its terminal-run twin, and that it is NOT a triage
		// (the claim that stops an operator hunting for an arbitration).
		"acceptance_undecidable",
		"succeeded_acceptance_undecidable",
		"**nothing to arbitrate**",
		// E34.4 refinement intake loop anchors (incl. the rejection/re-draft path).
		"Refinement intake loop",
		"Rejection / re-draft path",
		// E34.5 / #1597: the criteria-gate advisory surface must be documented in
		// the runbook — a dropped or reworded-away criteria-pre-check edit fails here.
		"criteria_precheck",
		// #1916: the three runbook additions — failed-run revive pre-dispatch check,
		// the decomposed-parent native path, and the drive_run loop shape. Anchored on
		// tool names, audit categories, and stop-reason/clamp tokens (not sentence
		// fragments) so future rewording does not fail spuriously, and pinning each
		// binding-condition token (paged: stop reason, [1,240] clamp, pre(plan)/post(review)
		// gates) so every promised runbook statement is test-load-bearing.
		"fishhawk_run_children",
		"fishhawk_consolidate_slices",
		"awaiting_children",
		"pre(plan)",
		"post(review)",
		"fishhawk_drive_run",
		"decision_required",
		"paged:",
		"dispatched_stale",
		"[1,240]",
		// The revive pre-dispatch check reads this audit category before dispatching
		// a re-parked acceptance stage. `acceptance_outcome_recorded` alone is NOT
		// load-bearing for that section — it pre-exists in the acceptance/settled-outcome
		// text — so pin the section by its unique bold heading, which fails if the
		// paragraph is dropped or reworded away.
		"Pre-dispatch check for a re-parked acceptance stage",
		"acceptance_outcome_recorded",
	} {
		if !strings.Contains(c.Text, anchor) {
			t.Errorf("runbook missing edge-case anchor %q", anchor)
		}
	}

	// E48.12 / #1959: the Batch-as-campaign section is asserted SECTION-SCOPED
	// (binding condition 2), not runbook-wide — a token merely present elsewhere
	// in the runbook (e.g. runner_kind:local in the local-dogfood section) must
	// not satisfy a batch-as-campaign anchor. Extract the section substring from
	// its heading to the next same-level (`### `) heading and assert every anchor
	// WITHIN it, so a dropped or reworded-away batch statement fails here.
	const batchHeading = "### Batch-as-campaign"
	start := strings.Index(c.Text, batchHeading)
	if start < 0 {
		t.Fatalf("runbook missing the %q section heading", batchHeading)
	}
	rest := c.Text[start+len(batchHeading):]
	end := strings.Index(rest, "\n### ")
	if end < 0 {
		t.Fatalf("Batch-as-campaign section has no following same-level heading; cannot bound the section")
	}
	section := rest[:end]
	for _, anchor := range []string{
		// The four campaign verbs the section maps a batch instruction onto.
		"fishhawk_start_campaign",
		"fishhawk_start_campaign_item_run",
		"fishhawk_get_campaign_status",
		"fishhawk_resume_campaign",
		// The eligibility-refusal and resume-guard error codes it quotes.
		"item_not_eligible",
		"campaign_not_paused",
		// Binding condition 2's extended anchor set.
		"runner_kind:local",             // the always-local start rule
		"single status surface",         // get_campaign_status is the one status read
		"one item at a time",            // the serialization rule
		"before the next eligible item", // the ordered post-merge-before-next-item rule
		"post-merge",                    // the scripts/dev post-merge step
		"#1918",                         // the pending two-concurrent-local-runs experiment
		// Binding condition 1: the section cites the completed live validation.
		"80a69eba-1ca1-4deb-a12e-db1d8ad4d9f7", // the campaign id
		"#1940",                                // the campaign's epic
	} {
		if !strings.Contains(section, anchor) {
			t.Errorf("Batch-as-campaign section missing anchor %q", anchor)
		}
	}
}

// groomingSectionHeading is the exact heading the E54.35 grooming section ships
// under. The three grooming tests below all bound their assertions to that
// section, so the heading is the one string they share.
const groomingSectionHeading = "### Backlog grooming loop (non-diff)"

// groomingSection slices the embedded runbook from the grooming heading to the
// next same-level (`### `) heading, or EOF when it is the last section. A
// missing heading is a t.Fatal with an actionable message rather than an empty
// slice: an empty slice would let the presence and ordering assertions pass
// vacuously, which is exactly the failure the counterfactual (delete the
// section, observe RED) exists to rule out.
func groomingSection(t *testing.T) string {
	t.Helper()
	start := strings.Index(runbookMarkdown, groomingSectionHeading)
	if start < 0 {
		t.Fatalf("runbook.md is missing the %q section heading — the E54.35 grooming section is absent or its heading was renamed", groomingSectionHeading)
	}
	rest := runbookMarkdown[start+len(groomingSectionHeading):]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// markdownInline strips the inline-emphasis punctuation markdown uses for code
// spans (`) and bold/italic (*), so an anchor keys on the WORDS a claim is made
// of rather than on the markup wrapping them: dropping a pair of backticks or
// moving a bold span changes no meaning, and a test that reddens on that gets
// deleted by the next person to copy-edit the runbook.
var markdownInline = strings.NewReplacer("`", "", "*", "")

// flattenSection normalizes a runbook section for anchoring: markup stripped,
// lowercased, and whitespace-folded so an anchor spanning a line wrap still
// matches and a re-wrap of the prose does not redden the assertions.
func flattenSection(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(markdownInline.Replace(s))), " ")
}

// TestRunbook_DocumentsGroomingLoop is the E54.35 done-means: the EMBEDDED
// runbook (not runbook.md on disk — asserting the embedded value is what proves
// the text travels inside the binary to a client connecting from another
// repository) must carry every load-bearing claim of the grooming loop. Each
// claim is asserted independently so a half-written section names which one is
// missing.
func TestRunbook_DocumentsGroomingLoop(t *testing.T) {
	flat := flattenSection(groomingSection(t))
	for _, c := range []struct {
		anchor string
		claim  string
	}{
		{"backlog_grooming", "names the backlog_grooming workflow"},
		{"fishhawk_approve_plan", "names the approving verb that triggers the write"},
		{"executes the approved mutations server-side", "states that approving EXECUTES the mutations server-side"},
		{"no separate apply step", "states there is no separate apply step to reconsider at"},
		{"the gate is the apply trigger", "states the gate IS the apply trigger"},
		{"trigger_source", "names the trigger_source requirement"},
		{"on_demand", "names the on_demand trigger form"},
		{"the stage type", "distinguishes the stage TYPE from the stage id"},
		{"hygiene", "names hygiene as the applying class"},
		{"objective_reversible", "names hygiene's backend-evaluable condition"},
		{"ordering", "names the ordering class that stays a proposal"},
		{"dedup", "names the dedup class that stays a proposal"},
		{"scoping", "names the scoping class that stays a proposal"},
		{"refused at parse time", "states mode: auto on those three is refused at PARSE time"},
		{"max_autonomy", "states an escalation ceiling can clamp further"},
		{"forge", "says to verify applied mutations on the FORGE"},
		{"not from the run summary", "says the run summary is not the verification surface"},
		{"grooming_run_id", "names the campaign-seeding parameter"},
		{"grooming_order_not_approved", "names the un-ratified-order refusal"},
		{"grooming_order_absent", "names the no-report refusal"},
		{"grooming_order_superseded", "names the superseded-order refusal"},
		{"grooming_run_not_found", "names the unknown-run refusal"},
	} {
		if !strings.Contains(flat, strings.ToLower(c.anchor)) {
			t.Errorf("grooming section missing anchor %q — the section must state that it %s", c.anchor, c.claim)
		}
	}

	// The stage id the stage TYPE is confused with, asserted as a whole WORD
	// rather than as a table substring: with the code-span backticks stripped,
	// a bare "groom" substring is carried by every "backlog_grooming" and
	// "grooming" in the section, so a substring anchor would pass vacuously.
	// \bgroom\b matches only the standalone id.
	if !regexp.MustCompile(`\bgroom\b`).MatchString(flat) {
		t.Error("grooming section never names the stage id `groom` as a standalone word — it must name the id the stage TYPE is confused with")
	}
}

// TestRunbook_GroomingHazardPrecedesInvocation asserts ORDER, not presence: a
// section that documents the loop correctly but buries the approving-is-a-write
// hazard under the invocation fails here, where a presence-only table passes.
//
// Anchored on SHORT load-bearing tokens — the workflow name and two tool verbs
// — rather than on sentences, so a legitimate copy-edit of the surrounding
// prose does not redden it while a genuinely reordered section still does
// (binding condition C2).
func TestRunbook_GroomingHazardPrecedesInvocation(t *testing.T) {
	section := groomingSection(t)

	hazard := strings.Index(section, "fishhawk_approve_plan")
	if hazard < 0 {
		t.Fatal("grooming section never names fishhawk_approve_plan; the write hazard is unstated")
	}
	workflow := strings.Index(section, "backlog_grooming")
	if workflow < 0 {
		t.Fatal("grooming section never names the backlog_grooming workflow")
	}
	for _, inv := range []struct {
		anchor string
		what   string
	}{
		{"fishhawk_start_run", "the run-minting verb"},
		{"trigger_source", "the trigger_source invocation detail"},
	} {
		at := strings.Index(section, inv.anchor)
		if at < 0 {
			t.Errorf("grooming section never names %s (%q)", inv.what, inv.anchor)
			continue
		}
		if hazard >= at {
			t.Errorf("grooming section states the write hazard (fishhawk_approve_plan, index %d) AFTER %s (%q, index %d); the hazard must lead the section so a reader who skims only the opening still learns that approving writes", hazard, inv.what, inv.anchor, at)
		}
		if workflow >= at {
			t.Errorf("grooming section names the backlog_grooming workflow (index %d) AFTER %s (%q, index %d); the section must say WHAT the loop is before how to invoke it", workflow, inv.what, inv.anchor, at)
		}
	}
}

// TestRunbook_GroomingSectionIsRepoAgnostic pins acceptance criterion 5: the
// runbook ships to any repository, so a grooming section carrying THIS
// repository's state is actively misleading elsewhere.
//
// Deliberately SECTION-SCOPED, not file-wide: the rest of runbook.md
// legitimately carries kuhlman-labs issue links (the Batch-as-campaign section
// alone links eight), so a file-wide assertion would be false. The check can
// only prove the absence of the owner/URL string — the remaining repo-specific
// classes (a bare issue number used as a step, a backlog count) are review-
// verified, per the issue.
func TestRunbook_GroomingSectionIsRepoAgnostic(t *testing.T) {
	section := groomingSection(t)
	for _, banned := range []string{
		"kuhlman-labs",
		"https://github.com/",
	} {
		if strings.Contains(section, banned) {
			t.Errorf("grooming section contains repo-specific string %q; the runbook ships to every connecting repository, so the section must describe the MECHANISM and carry no owner, org or forge URL", banned)
		}
	}
}

// TestOnboardingInstructions_PointAtGroomingLoop is the E54.35 onboarding
// pin: the initialize instructions must name the non-diff grooming loop, state
// that approving its gate WRITES, name the on_demand trigger requirement, and
// point at the runbook section — four independent assertions, so a half-edit
// that names the workflow but drops the hazard fails where a presence gate
// would pass.
func TestOnboardingInstructions_PointAtGroomingLoop(t *testing.T) {
	got := onboardingInstructions
	lower := strings.ToLower(got)
	for _, c := range []struct {
		anchor string
		claim  string
	}{
		{"backlog_grooming", "name the backlog_grooming workflow"},
		{"non-diff", "state that it is a non-diff workflow"},
		{"executes the approved mutations", "state that approving EXECUTES the mutations"},
		{"no separate apply step", "state there is no separate apply step"},
		{"trigger_source:on_demand", "name the on_demand trigger requirement"},
		{"backlog grooming loop", "point at the runbook's Backlog grooming loop section"},
	} {
		if !strings.Contains(lower, strings.ToLower(c.anchor)) {
			t.Errorf("onboardingInstructions missing %q — the grooming pointer must %s; got:\n%s", c.anchor, c.claim, got)
		}
	}
}
