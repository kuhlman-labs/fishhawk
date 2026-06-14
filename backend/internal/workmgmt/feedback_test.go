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
