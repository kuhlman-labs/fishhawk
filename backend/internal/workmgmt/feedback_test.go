package workmgmt

import (
	"context"
	"errors"
	"testing"
)

type fakeFeedbackProvider struct{ name string }

func (f *fakeFeedbackProvider) Name() string { return f.name }
func (f *fakeFeedbackProvider) SearchOpenByFingerprint(context.Context, Target, string) (*ExistingReport, error) {
	return nil, nil
}
func (f *fakeFeedbackProvider) File(context.Context, Target, FeedbackReport) (*CreatedItem, error) {
	return &CreatedItem{}, nil
}
func (f *fakeFeedbackProvider) AppendOccurrence(context.Context, Target, int, string) error {
	return nil
}

func TestGetFeedback_RegisteredAndUnknown(t *testing.T) {
	RegisterFeedback(&fakeFeedbackProvider{name: "github_projects"})

	if _, err := GetFeedback("github_projects"); err != nil {
		t.Fatalf("GetFeedback(registered) = %v, want nil", err)
	}

	_, err := GetFeedback("jira")
	var unk *UnknownProviderError
	if !errors.As(err, &unk) {
		t.Fatalf("GetFeedback(unknown) error = %v, want *UnknownProviderError", err)
	}
	if unk.ID != "jira" {
		t.Errorf("UnknownProviderError.ID = %q, want jira", unk.ID)
	}

	var found bool
	for _, id := range RegisteredFeedback() {
		if id == "github_projects" {
			found = true
		}
	}
	if !found {
		t.Errorf("RegisteredFeedback() = %v, want it to include github_projects", RegisteredFeedback())
	}
}

// TestBoardingStatusOf covers every arm of the boarding-outcome
// classification (#1737). The point of the enum is that boarded=false
// alone cannot tell an operator whether there was nothing to board or
// whether placement was tried and failed, so each arm is pinned
// separately rather than only the happy path.
func TestBoardingStatusOf(t *testing.T) {
	proj := &Project{Owner: "kuhlman-labs", OwnerType: "user", Number: 7}
	tests := []struct {
		name    string
		target  Target
		created *CreatedItem
		want    string
	}{
		{
			name:   "dedup hit created nothing to board",
			target: Target{Project: proj},
			want:   BoardingStatusNotAttemptedNoReport,
		},
		{
			name:    "no project configured is a configuration state not an error",
			target:  Target{},
			created: &CreatedItem{Number: 7},
			want:    BoardingStatusNotAttemptedNoProject,
		},
		{
			name:    "placement attempted and failed",
			target:  Target{Project: proj},
			created: &CreatedItem{Number: 7, BoardingError: "projects API 403"},
			want:    BoardingStatusFailed,
		},
		{
			name:    "boarded",
			target:  Target{Project: proj},
			created: &CreatedItem{Number: 7, Boarded: true},
			want:    BoardingStatusBoarded,
		},
		{
			name:    "project configured but provider neither boarded nor named a cause",
			target:  Target{Project: proj},
			created: &CreatedItem{Number: 7},
			want:    BoardingStatusFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BoardingStatusOf(tt.target, tt.created); got != tt.want {
				t.Errorf("BoardingStatusOf = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBoardingStatus_NoProjectRecordsNoCause is the Condition-1 pin, in
// its own test because it is the contract an operator reads: the
// no-project arm must NOT masquerade as a failure and must NOT carry a
// cause string. The distinguishing signal is the status, not an error.
func TestBoardingStatus_NoProjectRecordsNoCause(t *testing.T) {
	created := &CreatedItem{Number: 7}
	noProject := BoardingStatusOf(Target{}, created)
	withProject := BoardingStatusOf(Target{Project: &Project{Number: 7}},
		&CreatedItem{Number: 7, BoardingError: "boom"})

	if noProject == withProject {
		t.Fatalf("no-project and placement-failure report the same status %q; "+
			"an operator cannot tell 'nothing to board' from 'boarding failed'", noProject)
	}
	if created.BoardingError != "" {
		t.Errorf("no-project path recorded a cause %q, want none", created.BoardingError)
	}
}
