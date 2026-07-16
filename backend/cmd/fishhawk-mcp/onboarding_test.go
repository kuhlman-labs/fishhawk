package main

import (
	"context"
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
		// E34.4: the refinement intake one-liner names the tool.
		"fishhawk_draft_epic",
		runbookURI,
	} {
		if !strings.Contains(got, anchor) {
			t.Errorf("instructions missing happy-path anchor %q", anchor)
		}
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
