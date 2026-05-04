package prompt

import (
	"errors"
	"strings"
	"testing"
)

func TestBuild_Implement_FullContext(t *testing.T) {
	got, err := Build("implement", Trigger{
		Source:      "github_issue",
		IssueNumber: 42,
		IssueTitle:  "Add foo",
		IssueBody:   "We need a foo function in pkg/bar.",
		Repo:        "kuhlman-labs/example",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wants := []string{
		"`kuhlman-labs/example`",
		"Triggering issue: #42",
		"Title: Add foo",
		"We need a foo function in pkg/bar.",
		"smallest set of changes",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("prompt missing %q\n---\n%s", w, got)
		}
	}
}

func TestBuild_Implement_EmptyContext(t *testing.T) {
	got, err := Build("implement", Trigger{Repo: "x/y"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "no issue context provided") {
		t.Errorf("expected 'no issue context provided' fallback, got:\n%s", got)
	}
}

func TestBuild_Implement_BodyOnly(t *testing.T) {
	got, err := Build("implement", Trigger{
		IssueBody: "Just a description.",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "Just a description.") {
		t.Errorf("body missing from prompt:\n%s", got)
	}
	if strings.Contains(got, "Title:") {
		t.Errorf("Title: header should be omitted when title is empty:\n%s", got)
	}
}

func TestBuild_Plan(t *testing.T) {
	got, err := Build("plan", Trigger{
		IssueNumber: 7,
		IssueTitle:  "Plan a refactor",
		Repo:        "x/y",
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "implementation plan") {
		t.Errorf("plan prompt missing 'implementation plan':\n%s", got)
	}
	if !strings.Contains(got, "Do not modify source files") {
		t.Errorf("plan prompt missing read-only directive:\n%s", got)
	}
	if !strings.Contains(got, "Triggering issue: #7") {
		t.Errorf("plan prompt missing issue ref:\n%s", got)
	}
}

func TestBuild_UnsupportedStage(t *testing.T) {
	_, err := Build("review", Trigger{IssueTitle: "anything"})
	if !errors.Is(err, ErrUnsupportedStage) {
		t.Errorf("expected ErrUnsupportedStage, got %v", err)
	}
	if !strings.Contains(err.Error(), `"review"`) {
		t.Errorf("error should name the stage type, got %v", err)
	}
}

func TestBuild_UnknownStage(t *testing.T) {
	_, err := Build("nonsense", Trigger{})
	if !errors.Is(err, ErrUnsupportedStage) {
		t.Errorf("expected ErrUnsupportedStage, got %v", err)
	}
}

func TestBuild_NoRepo(t *testing.T) {
	got, err := Build("implement", Trigger{IssueTitle: "x"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.Contains(got, "this repository") {
		t.Errorf("expected 'this repository' fallback when Repo empty, got:\n%s", got)
	}
	if strings.Contains(got, "``") {
		t.Errorf("empty backtick block leaked into prompt:\n%s", got)
	}
}

func TestBuild_DeterministicOutput(t *testing.T) {
	tr := Trigger{
		Source:      "github_issue",
		IssueNumber: 42,
		IssueTitle:  "T",
		IssueBody:   "B",
		Repo:        "o/r",
	}
	a, _ := Build("implement", tr)
	b, _ := Build("implement", tr)
	if a != b {
		t.Errorf("Build is non-deterministic across calls:\nA: %s\nB: %s", a, b)
	}
}
