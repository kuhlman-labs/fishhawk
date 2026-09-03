package workmgmt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// fakeProvider is a test double registered to exercise the registry.
type fakeProvider struct {
	name    string
	got     ProviderRequest
	gotTran TransitionRequest
	gotEpic EpicChildrenRequest
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) File(_ context.Context, req ProviderRequest) (*CreatedItem, error) {
	f.got = req
	return &CreatedItem{Provider: f.name, Number: 42, URL: "https://example/42"}, nil
}

func (f *fakeProvider) Transition(_ context.Context, req TransitionRequest) (*TransitionResult, error) {
	f.gotTran = req
	return &TransitionResult{Moved: true, From: "Backlog", To: "In Progress"}, nil
}

func (f *fakeProvider) EpicChildren(_ context.Context, req EpicChildrenRequest) (*EpicChildrenResult, error) {
	f.gotEpic = req
	return &EpicChildrenResult{
		Children: []EpicChild{
			{Number: 41, Title: "slice A", Autonomy: "low", Complete: true},
			{Number: 42, Title: "slice B", Autonomy: "high"},
		},
		Edges: []DependsEdge{{From: 42, To: 41}},
		// A closed-and-completed out-of-epic target the provider elided (#2953):
		// the dispatch-carrying test below obtains it ONLY through the interface,
		// so this proves SatisfiedEdges survives the cross-provider boundary.
		SatisfiedEdges: []SatisfiedEdge{
			{From: 2032, To: 1639, State: "closed", StateReason: "completed"},
		},
	}, nil
}

// TestTargetGitLab pins the additive Target.GitLab field: a zero Target
// leaves it nil (the not-a-gitlab-filing sentinel), and a Target carrying a
// GitLabConnection round-trips through the in-process struct so the filing
// endpoint can hand the concrete gitlab provider its project override. Target
// is in-process only (no serialization tags), so this asserts the struct
// carries the field, not a JSON round-trip.
func TestTargetGitLab(t *testing.T) {
	if (Target{}).GitLab != nil {
		t.Error("zero Target.GitLab should be nil (not a gitlab filing)")
	}
	tgt := Target{
		Repo:   Repo{Owner: "o", Name: "r"},
		GitLab: &GitLabConnection{Project: "group/app"},
	}
	if tgt.GitLab == nil {
		t.Fatal("Target.GitLab is nil after assignment")
	}
	if tgt.GitLab.Project != "group/app" {
		t.Errorf("Target.GitLab.Project = %q, want group/app", tgt.GitLab.Project)
	}
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	fp := &fakeProvider{name: "test_provider_registered"}
	Register(fp)

	got, err := Get("test_provider_registered")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name() != "test_provider_registered" {
		t.Errorf("got provider %q", got.Name())
	}
}

func TestRegistry_UnknownProviderFailsClosed(t *testing.T) {
	// An unregistered provider id (a config typo, or a backend not yet
	// implemented — jira is now real, so use a genuinely-never-registered
	// placeholder) must fail closed with a typed error naming the id.
	_, err := Get("gitlab")
	var upe *UnknownProviderError
	if !errors.As(err, &upe) {
		t.Fatalf("want *UnknownProviderError, got %v", err)
	}
	if upe.ID != "gitlab" {
		t.Errorf("error ID = %q, want gitlab", upe.ID)
	}
	if !strings.Contains(upe.Error(), "gitlab") {
		t.Errorf("error message must name the missing provider: %q", upe.Error())
	}
}

func TestUnknownProviderError_MessageForms(t *testing.T) {
	empty := (&UnknownProviderError{ID: "x"}).Error()
	if !strings.Contains(empty, "no providers registered") {
		t.Errorf("empty-registry message = %q", empty)
	}
	withKnown := (&UnknownProviderError{ID: "x", Known: []string{"github_projects"}}).Error()
	if !strings.Contains(withKnown, "github_projects") {
		t.Errorf("known-set message = %q", withKnown)
	}
}

func TestRegistry_DispatchPassesRequest(t *testing.T) {
	fp := &fakeProvider{name: "test_provider_dispatch"}
	Register(fp)
	p, err := Get("test_provider_dispatch")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	req := ProviderRequest{
		Item:   WorkItem{Type: "bug", Title: "boom"},
		Number: 0,
		Target: Target{Scope: forge.FromGitHubInstallationID(7), Repo: Repo{Owner: "o", Name: "r"}},
	}
	created, err := p.File(context.Background(), req)
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	if created.Number != 42 {
		t.Errorf("created.Number = %d, want 42", created.Number)
	}
	if fp.got.Item.Title != "boom" {
		t.Errorf("provider did not receive request: %+v", fp.got)
	}
}

func TestRegistry_DispatchTransition(t *testing.T) {
	fp := &fakeProvider{name: "test_provider_transition"}
	Register(fp)
	p, err := Get("test_provider_transition")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	tr, ok := p.(Transitioner)
	if !ok {
		t.Fatalf("provider does not implement Transitioner")
	}
	req := TransitionRequest{
		IssueNumber:          1012,
		Trigger:              "run_started",
		Target:               Target{Scope: forge.FromGitHubInstallationID(7), Repo: Repo{Owner: "o", Name: "r"}},
		CanonicalState:       CanonicalStateInProgress,
		ExpectedSourceStates: []string{CanonicalStateBacklog},
		States:               map[string]string{CanonicalStateBacklog: "Backlog", CanonicalStateInProgress: "In Progress"},
	}
	res, err := tr.Transition(context.Background(), req)
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if !res.Moved || res.To != "In Progress" {
		t.Errorf("result = %+v", res)
	}
	if fp.gotTran.IssueNumber != 1012 || fp.gotTran.CanonicalState != CanonicalStateInProgress {
		t.Errorf("provider did not receive transition request: %+v", fp.gotTran)
	}
}

func TestRegistry_DispatchEpicChildren(t *testing.T) {
	fp := &fakeProvider{name: "test_provider_epic_children"}
	Register(fp)
	p, err := Get("test_provider_epic_children")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	q, ok := p.(EpicChildrenQuerier)
	if !ok {
		t.Fatalf("provider does not implement EpicChildrenQuerier")
	}
	res, err := q.EpicChildren(context.Background(), EpicChildrenRequest{
		Target: Target{Scope: forge.FromGitHubInstallationID(7), Repo: Repo{Owner: "o", Name: "r"}},
		Epic:   "#1440",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	if len(res.Children) != 2 || res.Children[0].Number != 41 {
		t.Errorf("children = %+v", res.Children)
	}
	// The autonomy tier threads through EpicChild (the producer end of the
	// autonomy-aware campaign eligibility path, #1551).
	if res.Children[0].Autonomy != "low" || res.Children[1].Autonomy != "high" {
		t.Errorf("children autonomy = %q,%q, want low,high", res.Children[0].Autonomy, res.Children[1].Autonomy)
	}
	// The completion signal threads through EpicChild (the producer end of the
	// campaign subset satisfied-dependency path, #2120): #41 is closed-and-
	// completed, #42 is not.
	if !res.Children[0].Complete || res.Children[1].Complete {
		t.Errorf("children Complete = %v,%v, want true,false", res.Children[0].Complete, res.Children[1].Complete)
	}
	// A satisfied edge leaves Reason at the "" zero value, so the pre-#2120
	// equality assertion on Edges still holds unchanged (the compatibility the
	// additive DependsEdge.Reason field must preserve).
	if len(res.Edges) != 1 || res.Edges[0] != (DependsEdge{From: 42, To: 41}) {
		t.Errorf("edges = %+v", res.Edges)
	}
	if res.Edges[0].Reason != "" {
		t.Errorf("satisfied edge Reason = %q, want \"\" (zero)", res.Edges[0].Reason)
	}
	if fp.gotEpic.Epic != "#1440" {
		t.Errorf("provider did not receive epic-children request: %+v", fp.gotEpic)
	}
}

// TestEpicChild_CarriesBodyAndURLForAdoption pins the capability SHAPE the
// forge-state adoption path (#2064, E50.7) depends on: EpicChild carries the
// child's raw Body (the surface the idempotency lookup reads) and the forge's
// own URL (recorded verbatim on adoption). Both are additive — this asserts
// they exist, round-trip through an EpicChildrenResult, and that a marker
// stamped into a body is detectable off the mapped value.
func TestEpicChild_CarriesBodyAndURLForAdoption(t *testing.T) {
	key := MintIdempotencyKey("fishhawk-split-child", "run-abc", "0")
	res := &EpicChildrenResult{Children: []EpicChild{{
		Number: 100,
		Title:  "phase child",
		Body:   StampIdempotencyKey("## Summary\n\nphase child\n", key),
		// A GHE host on purpose: the URL is the forge's own, never composed.
		URL: "https://ghe.example.com/o/r/issues/100",
	}}}

	got := res.Children[0]
	if !BodyHasIdempotencyKey(got.Body, key) {
		t.Errorf("EpicChild.Body does not carry the stamped key: %q", got.Body)
	}
	if got.URL != "https://ghe.example.com/o/r/issues/100" {
		t.Errorf("EpicChild.URL = %q, want the forge-supplied url verbatim", got.URL)
	}
	if strings.HasPrefix(got.URL, "https://github.com/") {
		t.Errorf("EpicChild.URL must not be composed from a github.com prefix: %q", got.URL)
	}
}

// TestDropReasonWireValues pins the string values of the two #2953 drop reasons.
// They are the CONTRACT the server details keys and the fishhawk-mcp remedy
// renderer branch on, so a rename here silently breaks the operator remedy. A
// literal assertion (not a re-derivation) is the whole point.
func TestDropReasonWireValues(t *testing.T) {
	if DropTargetClosedIncomplete != "target_closed_incomplete" {
		t.Errorf("DropTargetClosedIncomplete = %q, want target_closed_incomplete", DropTargetClosedIncomplete)
	}
	if DropTargetStateUnreadable != "target_state_unreadable" {
		t.Errorf("DropTargetStateUnreadable = %q, want target_state_unreadable", DropTargetStateUnreadable)
	}
}

// TestSatisfiedEdgeCarriedThroughDispatch drives the SatisfiedEdges channel
// across the REAL registry dispatch seam (Register → Get → EpicChildrenQuerier
// type-assertion → EpicChildren) rather than reading back a literal in place: a
// registered provider PRODUCES the elided edge and the caller obtains it ONLY
// through the interface, so the field must survive the cross-provider interface
// boundary the campaign path relies on (#2953). It goes RED if EpicChildrenResult
// or EpicChildrenQuerier ever stops carrying the observed target state — the
// contract this package owns; the field-POPULATION logic is driven end to end in
// the github provider tests (TestResolveDependenciesClosedCompletedTargetSatisfied).
func TestSatisfiedEdgeCarriedThroughDispatch(t *testing.T) {
	fp := &fakeProvider{name: "test_provider_satisfied_edge"}
	Register(fp)
	p, err := Get("test_provider_satisfied_edge")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	q, ok := p.(EpicChildrenQuerier)
	if !ok {
		t.Fatalf("provider does not implement EpicChildrenQuerier")
	}
	res, err := q.EpicChildren(context.Background(), EpicChildrenRequest{
		Target: Target{Scope: forge.FromGitHubInstallationID(7), Repo: Repo{Owner: "o", Name: "r"}},
		Epic:   "#2953",
	})
	if err != nil {
		t.Fatalf("EpicChildren: %v", err)
	}
	if len(res.SatisfiedEdges) != 1 {
		t.Fatalf("SatisfiedEdges = %+v, want one carried through dispatch", res.SatisfiedEdges)
	}
	if e := res.SatisfiedEdges[0]; e != (SatisfiedEdge{From: 2032, To: 1639, State: "closed", StateReason: "completed"}) {
		t.Errorf("SatisfiedEdge = %+v, want {2032 1639 closed completed}", e)
	}
}

// TestDependsEdgeTargetRef pins the single rendering authority for a depends_on
// target (#2956), asserting the SHIPPED string shape: a resolvable edge renders
// issue:<To> unchanged; an unresolvable edge renders unparsable:<digest>:"token"
// or the <unprintable> sentinel; NO input renders issue:0; a display carrying an
// ANSI escape / newline / quote is control-free (strconv.Quote-escaped) in the
// output; and a 10,000-rune token — supplied DIRECTLY, bypassing the parser — is
// bounded (operator condition 1).
func TestDependsEdgeTargetRef(t *testing.T) {
	t.Run("resolvable renders issue:To unchanged", func(t *testing.T) {
		got := DependsEdge{From: 1, To: 99}.TargetRef()
		if got != "issue:99" {
			t.Errorf("TargetRef = %q, want issue:99", got)
		}
	})
	t.Run("unresolvable with display renders unparsable:digest:quoted", func(t *testing.T) {
		got := DependsEdge{From: 2032, To: 0, ToRef: "other/repo#12", ToRefDigest: "0123456789abcdef"}.TargetRef()
		want := `unparsable:0123456789abcdef:"other/repo#12"`
		if got != want {
			t.Errorf("TargetRef = %q, want %q", got, want)
		}
	})
	t.Run("unresolvable with empty display renders the sentinel", func(t *testing.T) {
		got := DependsEdge{From: 2032, To: 0, ToRef: "", ToRefDigest: "0123456789abcdef"}.TargetRef()
		want := "unparsable:0123456789abcdef:<unprintable>"
		if got != want {
			t.Errorf("TargetRef = %q, want %q", got, want)
		}
	})
	t.Run("unresolvable edge never renders issue:0", func(t *testing.T) {
		// Every case here is an UNRESOLVABLE edge (non-empty ToRefDigest, To=0);
		// the load-bearing assertion is that none render issue:0. A resolvable
		// To:0 edge legitimately renders issue:0 and is covered by the sibling
		// subtests, so it is not a case here (routed test-vacuity concern).
		for _, e := range []DependsEdge{
			{From: 1, To: 0, ToRef: "x", ToRefDigest: "deadbeefdeadbeef"},
			{From: 1, To: 0, ToRefDigest: "deadbeefdeadbeef"},
		} {
			got := e.TargetRef()
			if got == "issue:0" {
				t.Errorf("unresolvable edge %+v rendered issue:0 — forbidden", e)
			}
		}
	})
	t.Run("non-hex digest falls back to the sentinel", func(t *testing.T) {
		// A directly-constructed edge whose ToRefDigest is NOT the 16-hex parser
		// shape (an oversize, control-laden value a future constructor might
		// supply) must never interpolate the raw digest into the operator message:
		// TargetRef substitutes the <invalid-digest> sentinel (#2956, routed
		// digest-symmetry concern). Counterfactual vehicle for boundTargetRefDigest.
		got := DependsEdge{To: 0, ToRef: "other/repo#12",
			ToRefDigest: "NOThex\x1b[31m/" + strings.Repeat("z", 9000)}.TargetRef()
		if !strings.Contains(got, "<invalid-digest>") {
			t.Errorf("TargetRef = %q, want the <invalid-digest> sentinel for a non-hex digest", got)
		}
		if strings.Contains(got, "\x1b") || strings.Contains(got, "zzz") {
			t.Errorf("TargetRef = %q leaked the raw non-hex digest — must be replaced by the sentinel", got)
		}
		const ceiling = 200
		if len(got) > ceiling {
			t.Errorf("TargetRef len = %d, want <= %d (a non-hex digest must not blow up the render)", len(got), ceiling)
		}
	})
	t.Run("exact-length non-hex digest falls back to the sentinel", func(t *testing.T) {
		// The prior case is 9000 chars, so it returns at boundTargetRefDigest's
		// len != 16 branch and never reaches the per-rune check. This case pins the
		// rune loop: a digest that IS exactly 16 chars but carries a
		// non-lowercase-hex rune ('g') must still fall back to <invalid-digest>,
		// never leaking the raw digest (#2956, routed untested-path concern).
		got := DependsEdge{To: 0, ToRef: "other/repo#12",
			ToRefDigest: "0123456789abcdeg"}.TargetRef()
		if !strings.Contains(got, "<invalid-digest>") {
			t.Errorf("TargetRef = %q, want the <invalid-digest> sentinel for a 16-char non-hex digest", got)
		}
		if strings.Contains(got, "0123456789abcdeg") {
			t.Errorf("TargetRef = %q leaked the raw 16-char non-hex digest — must be replaced by the sentinel", got)
		}
	})
	t.Run("ANSI / newline / quote are escaped and control-free", func(t *testing.T) {
		got := DependsEdge{To: 0, ToRef: "a\x1b[31m\nb\"c", ToRefDigest: "0123456789abcdef"}.TargetRef()
		for _, bad := range []string{"\x1b", "\n"} {
			if strings.Contains(got, bad) {
				t.Errorf("TargetRef %q contains a raw control byte %q — must be escaped", got, bad)
			}
		}
		if !strings.HasPrefix(got, "unparsable:0123456789abcdef:") {
			t.Errorf("TargetRef = %q, want the unparsable-prefixed render", got)
		}
	})
	t.Run("10000-rune token renders bounded", func(t *testing.T) {
		got := DependsEdge{To: 0, ToRef: strings.Repeat("z", 10000), ToRefDigest: "0123456789abcdef"}.TargetRef()
		// Ceiling: prefix (~30) + a 64-rune quoted display + quotes/escapes.
		const ceiling = 200
		if len(got) > ceiling {
			t.Errorf("TargetRef len = %d, want <= %d (a directly-supplied oversize token must be bounded by the renderer)", len(got), ceiling)
		}
	})
}

// TestIssueSetResolutionTimeoutError pins the rendered message and, decisively,
// that a ZERO SuggestedLimit renders NO suggestion — 0 is the "no value can be
// proven to fit" signal, and a message advising an operator to request zero
// items would be worse than no advice at all (#3113).
func TestIssueSetResolutionTimeoutError(t *testing.T) {
	withSuggestion := (&IssueSetResolutionTimeout{Resolved: 12, Total: 60, SuggestedLimit: 12, Phase: "fetch_items"}).Error()
	for _, want := range []string{"resolved 12 of 60 issues", "during fetch_items", "a limit of 12 would have fit"} {
		if !strings.Contains(withSuggestion, want) {
			t.Fatalf("message %q missing %q", withSuggestion, want)
		}
	}
	none := (&IssueSetResolutionTimeout{Resolved: 0, Total: 60, SuggestedLimit: 0, Phase: "fetch_items"}).Error()
	if strings.Contains(none, "would have fit") {
		t.Fatalf("a zero SuggestedLimit must render no suggestion, got %q", none)
	}
	if !strings.Contains(none, "resolved 0 of 60 issues") {
		t.Fatalf("counts missing from %q", none)
	}
	if bare := (&IssueSetResolutionTimeout{Total: 3}).Error(); strings.Contains(bare, "during") {
		t.Fatalf("an empty Phase must render no phase clause, got %q", bare)
	}
}

// TestIssueSetResolutionTimeoutUnwrap: a caller that only wants to know the
// request ran out of time can errors.Is it against context.DeadlineExceeded,
// while errors.As reaches the counts — the two access paths the server's 504
// arm and the generic timeout handling each need.
func TestIssueSetResolutionTimeoutUnwrap(t *testing.T) {
	var err error = &IssueSetResolutionTimeout{Resolved: 1, Total: 2, SuggestedLimit: 1}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("errors.Is(err, context.DeadlineExceeded) must hold")
	}
	wrapped := fmt.Errorf("campaign: %w", err)
	var to *IssueSetResolutionTimeout
	if !errors.As(wrapped, &to) || to.Resolved != 1 || to.Total != 2 || to.SuggestedLimit != 1 {
		t.Fatalf("errors.As must reach the counts through a wrap, got %+v", to)
	}
}
