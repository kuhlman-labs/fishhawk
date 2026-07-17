package webhook

import (
	"errors"
	"testing"
)

func TestVerifyGitLabToken_OK(t *testing.T) {
	if err := VerifyGitLabToken([]byte("s3cr3t"), "s3cr3t"); err != nil {
		t.Errorf("VerifyGitLabToken: %v", err)
	}
}

func TestVerifyGitLabToken_Missing(t *testing.T) {
	err := VerifyGitLabToken([]byte("s3cr3t"), "")
	if !errors.Is(err, ErrGitLabTokenMissing) {
		t.Errorf("err = %v, want ErrGitLabTokenMissing", err)
	}
}

func TestVerifyGitLabToken_Invalid(t *testing.T) {
	err := VerifyGitLabToken([]byte("right"), "wrong")
	if !errors.Is(err, ErrGitLabTokenInvalid) {
		t.Errorf("err = %v, want ErrGitLabTokenInvalid", err)
	}
}

func TestVerifyGitLabToken_Unconfigured(t *testing.T) {
	err := VerifyGitLabToken(nil, "anything")
	if !errors.Is(err, ErrSecretNotConfigured) {
		t.Errorf("err = %v, want ErrSecretNotConfigured", err)
	}
}

// Fixtures transcribed from GitLab's webhook events reference
// (https://docs.gitlab.com/user/project/integrations/webhook_events/),
// trimmed to the fields ParseGitLabEvent / the matchers read.
const (
	glMergeRequestFixture = `{
		"object_kind": "merge_request",
		"event_type": "merge_request",
		"user": {"username": "root"},
		"project": {"id": 1, "path_with_namespace": "mike/diaspora"},
		"object_attributes": {
			"iid": 1,
			"action": "open",
			"state": "opened",
			"url": "http://example.com/diaspora/-/merge_requests/1",
			"last_commit": {"id": "da1560886d4f094c3e6c9ef40349f7d38b5d27d7"}
		}
	}`

	glNoteFixture = `{
		"object_kind": "note",
		"event_type": "note",
		"user": {"username": "root"},
		"project": {"id": 5, "path_with_namespace": "gitlab-org/gitlab-test"},
		"object_attributes": {"note": "/fishhawk approve looks good", "noteable_type": "Issue"},
		"issue": {"iid": 17}
	}`

	glIssueFixture = `{
		"object_kind": "issue",
		"event_type": "issue",
		"user": {"username": "root"},
		"project": {"id": 1, "path_with_namespace": "mike/diaspora"},
		"object_attributes": {"iid": 23, "action": "update", "state": "opened"},
		"labels": [{"title": "fishhawk"}],
		"changes": {"labels": {"previous": [], "current": [{"title": "fishhawk"}]}}
	}`

	glPipelineFixture = `{
		"object_kind": "pipeline",
		"object_attributes": {"id": 31, "iid": 3, "status": "success"},
		"user": {"username": "root"},
		"project": {"id": 1, "path_with_namespace": "mike/diaspora"}
	}`

	glBuildFixture = `{
		"object_kind": "build",
		"build_id": 1977,
		"build_name": "test",
		"build_status": "success",
		"project": {"id": 380, "path_with_namespace": "gitlab-org/gitlab-test"},
		"user": {"username": "root"}
	}`
)

func TestParseGitLabEvent_AllKinds(t *testing.T) {
	cases := []struct {
		name        string
		eventType   string
		body        string
		wantType    string
		wantAction  string
		wantRepo    string
		wantSender  string
		wantCredRef string
	}{
		{"merge_request", "Merge Request Hook", glMergeRequestFixture, "merge_request", "open", "mike/diaspora", "root", "gitlab:1"},
		{"note", "Note Hook", glNoteFixture, "note", "", "gitlab-org/gitlab-test", "root", "gitlab:5"},
		{"issue", "Issue Hook", glIssueFixture, "issue", "update", "mike/diaspora", "root", "gitlab:1"},
		{"pipeline", "Pipeline Hook", glPipelineFixture, "pipeline", "success", "mike/diaspora", "root", "gitlab:1"},
		{"build", "Job Hook", glBuildFixture, "build", "success", "gitlab-org/gitlab-test", "root", "gitlab:380"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := ParseGitLabEvent(tc.eventType, "uuid-123", []byte(tc.body))
			if err != nil {
				t.Fatalf("ParseGitLabEvent: %v", err)
			}
			if ev.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", ev.Type, tc.wantType)
			}
			if ev.Action != tc.wantAction {
				t.Errorf("Action = %q, want %q", ev.Action, tc.wantAction)
			}
			if ev.Repo != tc.wantRepo {
				t.Errorf("Repo = %q, want %q", ev.Repo, tc.wantRepo)
			}
			if ev.Sender != tc.wantSender {
				t.Errorf("Sender = %q, want %q", ev.Sender, tc.wantSender)
			}
			if ev.CredentialRef != tc.wantCredRef {
				t.Errorf("CredentialRef = %q, want %q", ev.CredentialRef, tc.wantCredRef)
			}
			if ev.DeliveryID != "gitlab:uuid-123" {
				t.Errorf("DeliveryID = %q, want gitlab:uuid-123", ev.DeliveryID)
			}
			if ev.Forge != ForgeGitLab {
				t.Errorf("Forge = %q, want %q", ev.Forge, ForgeGitLab)
			}
		})
	}
}

func TestParseGitLabEvent_MissingEventHeader(t *testing.T) {
	_, err := ParseGitLabEvent("", "uuid-1", []byte(glIssueFixture))
	if !errors.Is(err, ErrGitLabEventMissing) {
		t.Errorf("err = %v, want ErrGitLabEventMissing", err)
	}
}

func TestParseGitLabEvent_MissingEventUUID(t *testing.T) {
	_, err := ParseGitLabEvent("Issue Hook", "", []byte(glIssueFixture))
	if !errors.Is(err, ErrGitLabEventUUIDMissing) {
		t.Errorf("err = %v, want ErrGitLabEventUUIDMissing", err)
	}
}

func TestParseGitLabEvent_MalformedBody(t *testing.T) {
	_, err := ParseGitLabEvent("Issue Hook", "uuid-1", []byte(`{not json`))
	if err == nil {
		t.Fatal("expected a parse error for malformed body")
	}
}

// TestParseGitLabEvent_PermissiveAbsentFields pins the GitHub-parity
// posture: absent JSON fields yield zero values (only missing headers
// error), so a body with no project id parses but leaves CredentialRef
// empty — the matcher's empty-CredentialRef skip is what refuses it.
func TestParseGitLabEvent_PermissiveAbsentFields(t *testing.T) {
	ev, err := ParseGitLabEvent("Issue Hook", "uuid-1", []byte(`{"object_kind":"issue"}`))
	if err != nil {
		t.Fatalf("ParseGitLabEvent: %v", err)
	}
	if ev.CredentialRef != "" {
		t.Errorf("CredentialRef = %q, want empty (no project id)", ev.CredentialRef)
	}
	if ev.Type != "issue" {
		t.Errorf("Type = %q, want issue", ev.Type)
	}
}
