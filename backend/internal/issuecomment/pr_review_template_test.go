package issuecomment_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// TestPRReviewEventComment_IsCommentInvariant pins the load-bearing safety
// property (binding condition 2): the agent-review PR-review event constant is
// COMMENT and can NEVER be a branch-protection-blocking event. A regression to
// APPROVE / REQUEST_CHANGES would silently change gate semantics.
func TestPRReviewEventComment_IsCommentInvariant(t *testing.T) {
	if issuecomment.PRReviewEventComment != "COMMENT" {
		t.Fatalf("PRReviewEventComment = %q, want COMMENT", issuecomment.PRReviewEventComment)
	}
	if issuecomment.PRReviewEventComment == "APPROVE" || issuecomment.PRReviewEventComment == "REQUEST_CHANGES" {
		t.Fatalf("PRReviewEventComment must never be a branch-protection-blocking event; got %q",
			issuecomment.PRReviewEventComment)
	}
}

func implementReviewedEntry(t *testing.T, seq int64, p planreview.ImplementReviewedPayload) *audit.Entry {
	t.Helper()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return &audit.Entry{Sequence: seq, Category: "implement_reviewed", Payload: body}
}

func TestRenderPRReviewBody(t *testing.T) {
	runRow := &run.Run{ID: uuid.New()}
	const externalURL = "https://app.example"

	t.Run("approve renders verdict + attribution", func(t *testing.T) {
		entry := implementReviewedEntry(t, 5, planreview.ImplementReviewedPayload{
			ReviewerModel: "claude-opus-4-8",
			Verdict:       planreview.VerdictApprove,
			FreeForm:      "Looks correct and well tested.",
		})
		got := issuecomment.RenderPRReviewBody(entry, runRow, externalURL)
		if !strings.Contains(got, "`approve`") {
			t.Errorf("missing verdict token; got:\n%s", got)
		}
		if !strings.Contains(got, "Looks correct and well tested.") {
			t.Errorf("missing free_form; got:\n%s", got)
		}
		// Attribution line names the reviewer model + a run link.
		if !strings.Contains(got, "claude-opus-4-8") {
			t.Errorf("attribution missing reviewer model; got:\n%s", got)
		}
		if !strings.Contains(got, "/runs/"+runRow.ID.String()) {
			t.Errorf("attribution missing run link; got:\n%s", got)
		}
	})

	t.Run("reject with severity-bucketed concerns", func(t *testing.T) {
		entry := implementReviewedEntry(t, 6, planreview.ImplementReviewedPayload{
			ReviewerModel: "gpt-5.5",
			Verdict:       planreview.VerdictReject,
			Concerns: []planreview.Concern{
				{Severity: planreview.SeverityHigh, Category: "correctness", Note: "off-by-one in the loop"},
				{Severity: planreview.SeverityLow, Category: "style", Note: "rename foo"},
			},
		})
		got := issuecomment.RenderPRReviewBody(entry, runRow, externalURL)
		if !strings.Contains(got, "`reject`") {
			t.Errorf("missing reject verdict; got:\n%s", got)
		}
		// Severity-bucketed count suffix on the header.
		if !strings.Contains(got, "(1 high · 1 low)") {
			t.Errorf("missing severity-bucketed count suffix; got:\n%s", got)
		}
		// Concern list rows reuse the anchor shape.
		if !strings.Contains(got, "- **high** (correctness): off-by-one in the loop") {
			t.Errorf("missing high concern row; got:\n%s", got)
		}
		if !strings.Contains(got, "- **low** (style): rename foo") {
			t.Errorf("missing low concern row; got:\n%s", got)
		}
	})

	t.Run("post-fixup round renders concern-resolutions arc", func(t *testing.T) {
		entry := implementReviewedEntry(t, 9, planreview.ImplementReviewedPayload{
			ReviewerModel: "claude-opus-4-8",
			Verdict:       planreview.VerdictApprove,
			ConcernResolutions: []planreview.ConcernResolution{
				{ID: "11111111-2222-3333-4444-555555555555", Resolution: "confirmed", Note: "the fix-up resolves it"},
				{ID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Resolution: "reopened"},
			},
		})
		got := issuecomment.RenderPRReviewBody(entry, runRow, externalURL)
		if !strings.Contains(got, "Prior concerns") {
			t.Errorf("missing prior-concerns arc header; got:\n%s", got)
		}
		if !strings.Contains(got, "`11111111` → **confirmed**: the fix-up resolves it") {
			t.Errorf("missing confirmed resolution row; got:\n%s", got)
		}
		if !strings.Contains(got, "`aaaaaaaa` → **reopened**") {
			t.Errorf("missing reopened resolution row; got:\n%s", got)
		}
	})

	t.Run("undecodable payload renders empty", func(t *testing.T) {
		entry := &audit.Entry{Sequence: 1, Category: "implement_reviewed", Payload: json.RawMessage(`not json`)}
		if got := issuecomment.RenderPRReviewBody(entry, runRow, externalURL); got != "" {
			t.Errorf("undecodable payload should render empty; got:\n%s", got)
		}
	})

	t.Run("verdictless payload renders empty", func(t *testing.T) {
		entry := implementReviewedEntry(t, 2, planreview.ImplementReviewedPayload{ReviewerModel: "x"})
		if got := issuecomment.RenderPRReviewBody(entry, runRow, externalURL); got != "" {
			t.Errorf("verdictless payload should render empty; got:\n%s", got)
		}
	})

	t.Run("nil entry renders empty", func(t *testing.T) {
		if got := issuecomment.RenderPRReviewBody(nil, runRow, externalURL); got != "" {
			t.Errorf("nil entry should render empty; got:\n%s", got)
		}
	})
}
