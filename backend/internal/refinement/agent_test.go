package refinement

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// fakeInferencer is a test double for the drafting Inferencer: it records the
// prompt it was handed and returns canned response/model/error.
type fakeInferencer struct {
	resp      string
	model     string
	err       error
	gotPrompt string
}

func (f *fakeInferencer) Inference(_ context.Context, prompt string) (string, string, planreview.Usage, error) {
	f.gotPrompt = prompt
	return f.resp, f.model, planreview.Usage{}, f.err
}

// repoDeclarationPromptGoldenPath is the drafting prompt for this
// repository's own declaration, captured ONCE from the unmodified pre-move
// Drafter (NewDrafter(inf, conv).Draft) before the conventions became a
// per-call argument (ADR-110 / ADR-114, #4626). There is deliberately no
// -update flag: a self-blessing flag would let a regression rewrite its own
// golden.
const repoDeclarationPromptGoldenPath = "testdata/repo-declaration-prompt.golden.txt"

// repoDeclarationPromptBrief is the fixed brief the prompt golden was
// captured with.
const repoDeclarationPromptBrief = "Render the draft_epic preview with the target repository's declared conventions."

// parseRepoDeclaration parses this repository's own work-management
// declaration READ-ONLY. Fishhawk-shape assertions (the area/autonomy/phase
// enumeration) read it rather than workmgmt.Default(), so the default's later
// neutralisation does not touch this file.
func parseRepoDeclaration(t *testing.T) workmgmt.Conventions {
	t.Helper()
	declPath := filepath.Join(pkgSrcDir, "..", "..", "..", ".fishhawk", "work-management.yaml")
	f, err := os.Open(declPath)
	if err != nil {
		t.Fatalf("open %s: %v — this repository's own declaration must exist", declPath, err)
	}
	defer f.Close()
	conv, err := workmgmt.Parse(f)
	if err != nil {
		t.Fatalf("Parse(%s): %v", declPath, err)
	}
	return conv
}

// TestDrafter_RepoDeclarationPromptGolden asserts the POST-move prompt for
// this repository's declaration is byte-equal to the golden captured from
// the unmodified Drafter: moving the conventions from construction time to
// the Draft call changed no prompt byte.
func TestDrafter_RepoDeclarationPromptGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(pkgSrcDir, repoDeclarationPromptGoldenPath))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	inf := &fakeInferencer{resp: wellFormedDraftJSON, model: "m"}
	if _, _, err := NewDrafter(inf).Draft(context.Background(), uuid.New(), repoDeclarationPromptBrief, parseRepoDeclaration(t)); err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if got := []byte(inf.gotPrompt); !bytes.Equal(got, want) {
		t.Errorf("drafting prompt diverged from %s:\n--- got ---\n%s\n--- want ---\n%s", repoDeclarationPromptGoldenPath, got, want)
	}
}

// TestDrafter_PromptUsesPerCallConventions pins that the prompt enumerates
// the PER-CALL conventions' required namespaces: one Drafter, two calls with
// different conventions, two different enumerations. A Drafter that read a
// construction-time or default conventions value would omit "component".
func TestDrafter_PromptUsesPerCallConventions(t *testing.T) {
	inf := &fakeInferencer{resp: wellFormedDraftJSON, model: "m"}
	d := NewDrafter(inf)

	custom := workmgmt.Conventions{Types: map[string]workmgmt.ItemType{
		"feature": {RequiredLabelNamespaces: []string{"component", "type"}},
	}}
	if _, _, err := d.Draft(context.Background(), uuid.New(), "brief", custom); err != nil {
		t.Fatalf("Draft(custom): %v", err)
	}
	if !strings.Contains(inf.gotPrompt, "Include a label in each of these namespaces: component, type (") {
		t.Errorf("prompt does not enumerate the per-call namespaces component, type:\n%s", inf.gotPrompt)
	}

	if _, _, err := d.Draft(context.Background(), uuid.New(), "brief", parseRepoDeclaration(t)); err != nil {
		t.Fatalf("Draft(repo declaration): %v", err)
	}
	if strings.Contains(inf.gotPrompt, "component") {
		t.Errorf("second call's prompt still carries the first call's conventions:\n%s", inf.gotPrompt)
	}
	if !strings.Contains(inf.gotPrompt, "namespaces: area, autonomy, phase (") {
		t.Errorf("prompt does not enumerate this repository's namespaces:\n%s", inf.gotPrompt)
	}

	// No feature type: no namespace sentence at all.
	if _, _, err := d.Draft(context.Background(), uuid.New(), "brief", workmgmt.Conventions{}); err != nil {
		t.Fatalf("Draft(empty): %v", err)
	}
	if strings.Contains(inf.gotPrompt, "Include a label in each of these namespaces") {
		t.Errorf("prompt enumerates namespaces for conventions with no feature type:\n%s", inf.gotPrompt)
	}
}

func TestDraft_HappyPath(t *testing.T) {
	// The agent narrates and fences its JSON — Draft must tolerate both and
	// return the validated draft plus the model id.
	inf := &fakeInferencer{
		resp:  "Here's the decomposition:\n\n```json\n" + wellFormedDraftJSON + "\n```",
		model: "claude-opus-4-8",
	}
	d := NewDrafter(inf)

	draft, model, err := d.Draft(context.Background(), uuid.New(), "stand up X", parseRepoDeclaration(t))
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if model != "claude-opus-4-8" {
		t.Errorf("model = %q, want claude-opus-4-8", model)
	}
	if draft.Epic.Summary != "stand up X" || len(draft.Children) != 1 {
		t.Errorf("draft = %+v, want epic 'stand up X' with 1 child", draft)
	}
}

func TestDraft_InferenceErrorPropagates(t *testing.T) {
	inf := &fakeInferencer{err: errors.New("backend down")}
	d := NewDrafter(inf)

	_, _, err := d.Draft(context.Background(), uuid.New(), "brief", parseRepoDeclaration(t))
	if err == nil {
		t.Fatal("Draft returned nil error when the Inferencer failed")
	}
	if !strings.Contains(err.Error(), "backend down") {
		t.Errorf("error %q does not carry the inference failure", err.Error())
	}
}

func TestDraft_InvalidDraftRejectedNamesEdge(t *testing.T) {
	// A syntactically-valid draft whose single child depends on a non-existent
	// sibling ordinal (9) must fail Validate with a wrapped dangling-dependency
	// error naming the edge.
	danglingJSON := `{
	  "epic": {"summary": "s", "scope": "sc", "out_of_scope": ""},
	  "children": [
	    {"summary": "only child", "proposal": "p", "done_means": "d",
	     "acceptance_criteria": ["a"], "labels": [], "depends_on": [9]}
	  ]
	}`
	inf := &fakeInferencer{resp: danglingJSON, model: "m"}
	d := NewDrafter(inf)

	_, _, err := d.Draft(context.Background(), uuid.New(), "brief", parseRepoDeclaration(t))
	if err == nil {
		t.Fatal("Draft accepted a draft with a dangling depends_on edge")
	}
	if !errors.Is(err, campaign.ErrDanglingDependency) {
		t.Fatalf("error = %v, want wrapped campaign.ErrDanglingDependency", err)
	}
	if !strings.Contains(err.Error(), "issue:1->issue:9") {
		t.Errorf("error %q does not name the dangling edge issue:1->issue:9", err.Error())
	}
}

func TestDraft_DecodeFailurePropagates(t *testing.T) {
	inf := &fakeInferencer{resp: "not json at all", model: "m"}
	d := NewDrafter(inf)

	if _, _, err := d.Draft(context.Background(), uuid.New(), "brief", parseRepoDeclaration(t)); err == nil {
		t.Fatal("Draft accepted undecodable inference output")
	}
}

func TestBuildPrompt_EnumeratesClosedFieldSetAndNamespaces(t *testing.T) {
	p := buildPrompt("build the thing", parseRepoDeclaration(t))

	for _, want := range []string{
		"build the thing",         // the brief
		"EXACTLY ONE JSON object", // single-object contract
		"unknown fields are rejected",
		"acceptance_criteria",             // inner-element shape
		"1-BASED ordinals",                // depends_on semantics
		"\"additionalProperties\": false", // the literal schema text
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// The feature type's required namespaces are enumerated for the agent.
	if !strings.Contains(p, "area") || !strings.Contains(p, "autonomy") {
		t.Errorf("prompt does not steer the agent to area/autonomy labels:\n%s", p)
	}
}

// TestDraft_EndToEnd is the cross-layer integration test (#618): a fake
// Inferencer emitting prose-prefixed fenced JSON drives
// Draft -> Validate -> CreateDraft (real Postgres) -> GetDraft -> RenderChild,
// asserting the reloaded draft renders a conventions-complete body. It crosses
// the agent-output / domain / persistence / render seams a per-layer unit
// misses.
func TestDraft_EndToEnd(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)
	conv := parseRepoDeclaration(t)

	// A two-child draft with a dependency edge, so persistence must preserve
	// both criteria and edges.
	e2eJSON := `Draft below:
	` + "```json\n" + `{
	  "epic": {"summary": "stand up refinement", "scope": "model + agent", "out_of_scope": "the HTTP surface"},
	  "children": [
	    {"summary": "domain model", "proposal": "define EpicDraft", "done_means": "types compile",
	     "acceptance_criteria": ["EpicDraft exists", "Validate rejects cycles"],
	     "labels": ["area:backend", "autonomy:medium"], "depends_on": []},
	    {"summary": "persistence", "proposal": "JSONB row", "done_means": "round-trips",
	     "acceptance_criteria": ["draft persists"], "labels": ["area:backend", "autonomy:medium"],
	     "depends_on": [1]}
	  ]
	}` + "\n```"
	inf := &fakeInferencer{resp: e2eJSON, model: "claude-opus-4-8"}
	d := NewDrafter(inf)
	sessionID := uuid.New()

	draft, model, err := d.Draft(context.Background(), sessionID, "stand up the refinement package", conv)
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}

	stored, err := repo.CreateDraft(context.Background(), CreateParams{
		SessionID: sessionID,
		Brief:     "stand up the refinement package",
		Draft:     draft,
		Model:     model,
	})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}

	got, err := repo.GetDraft(context.Background(), stored.ID)
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Model != "claude-opus-4-8" {
		t.Errorf("reloaded model = %q, want claude-opus-4-8", got.Model)
	}
	// Edges and criteria survive the JSONB round-trip.
	if len(got.Draft.Children) != 2 {
		t.Fatalf("reloaded children = %d, want 2", len(got.Draft.Children))
	}
	if got.Draft.Children[1].DependsOn == nil || got.Draft.Children[1].DependsOn[0] != 1 {
		t.Errorf("reloaded depends_on = %v, want [1]", got.Draft.Children[1].DependsOn)
	}
	if len(got.Draft.Children[0].AcceptanceCriteria) != 2 {
		t.Errorf("reloaded acceptance criteria = %v, want 2 entries", got.Draft.Children[0].AcceptanceCriteria)
	}

	// The reloaded draft renders a conventions-complete child with the
	// depends_on marker.
	item, err := RenderChild(got.Draft.Children[1], 2, RenderOptions{}, conv)
	if err != nil {
		t.Fatalf("RenderChild on reloaded draft: %v", err)
	}
	if item.Title != "[EX.2] persistence" {
		t.Errorf("rendered title = %q, want '[EX.2] persistence'", item.Title)
	}
	if !strings.Contains(item.Body, "Depends on: #1") {
		t.Errorf("rendered body missing depends_on marker:\n%s", item.Body)
	}

	// ListForSession finds the persisted draft.
	list, err := repo.ListForSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	if len(list) != 1 || list[0].ID != stored.ID {
		t.Errorf("ListForSession = %d drafts, want the one just created", len(list))
	}
}
