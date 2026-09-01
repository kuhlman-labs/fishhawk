package webhook

import (
	"errors"
	"strings"
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

// TestVerifyGitLabToken_WrongTokenDifferentLength exercises the
// length-independence the SHA-256-digest comparison provides: a wrong
// token that also DIFFERS in length must still reject as
// ErrGitLabTokenInvalid, not leak the secret's length via an early
// length-mismatch return. (Timing itself isn't asserted — that's not
// observable in a unit test — but pinning the both-length branches keeps
// a regression to a raw ConstantTimeCompare from changing the outcome.)
func TestVerifyGitLabToken_WrongTokenDifferentLength(t *testing.T) {
	secret := []byte("the-configured-secret")
	for _, tok := range []string{"x", "short", "a-very-very-long-wrong-token-value"} {
		if err := VerifyGitLabToken(secret, tok); !errors.Is(err, ErrGitLabTokenInvalid) {
			t.Errorf("VerifyGitLabToken(secret, %q) = %v, want ErrGitLabTokenInvalid", tok, err)
		}
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

	// The three fixtures below pin the Pipeline Hook REF SHAPES the CI-retry
	// classification turns on (E45.30 / #2881). Read what they do and do NOT
	// prove: each pins a TRANSCRIPTION (or, where said so below, an INFERENCE)
	// and the parse built on it. None of them independently confirms that GitLab
	// emits that shape — no test in this repository observes a live delivery.
	// The per-fixture URL is what makes a wrong transcription DETECTABLE by a
	// reader checking it against upstream; it is a review aid, not an automated
	// check. Every fixture is TRIMMED to the fields ParseGitLabEvent and the
	// matchers read.

	// glBranchPipelineFixture — a BRANCH pipeline on a Fishhawk run branch.
	// Transcribed from the "Pipeline events" example on GitLab's webhook events
	// reference (https://docs.gitlab.com/user/project/integrations/webhook_events/):
	// object_attributes.source = "push", and NO top-level merge_request block.
	glBranchPipelineFixture = `{
		"object_kind": "pipeline",
		"object_attributes": {
			"id": 31,
			"iid": 3,
			"ref": "fishhawk/run-abcdef12",
			"sha": "bcbb5ec396a2c0f828686f14fac9b80b780504f2",
			"source": "push",
			"status": "failed"
		},
		"user": {"username": "root"},
		"project": {"id": 1, "path_with_namespace": "mike/diaspora"}
	}`

	// glMergeRequestPipelineFixture — a MERGE-REQUEST pipeline as the Pipeline
	// Hook documents it. Transcribed from the "Pipeline events" example on
	// https://docs.gitlab.com/user/project/integrations/webhook_events/ : the ref
	// is the MR's TARGET BRANCH ("master"), the discriminator is
	// object_attributes.source = "merge_request_event", and the MR coordinates
	// ride in the documented top-level merge_request block. This is the shape the
	// SOURCE signal exists for, and the only MR shape documented for THIS payload
	// type.
	glMergeRequestPipelineFixture = `{
		"object_kind": "pipeline",
		"object_attributes": {
			"id": 32,
			"iid": 4,
			"ref": "master",
			"sha": "bcbb5ec396a2c0f828686f14fac9b80b780504f2",
			"source": "merge_request_event",
			"status": "failed"
		},
		"user": {"username": "root"},
		"project": {"id": 1, "path_with_namespace": "mike/diaspora"},
		"merge_request": {"iid": 7, "source_branch": "feature", "target_branch": "master"}
	}`

	// glMergedResultsPipelineFixture — a MERGED-RESULTS pipeline ref.
	//
	// THE REF SHAPE HERE IS INFERRED, NOT TRANSCRIBED FROM A WEBHOOK EXAMPLE.
	// refs/merge-requests/<iid>/merge and .../head are documented for
	// CI_MERGE_REQUEST_REF_PATH — a RUNNER-SIDE predefined variable
	// (https://docs.gitlab.com/ci/variables/predefined_variables/) — and on the
	// merged-results pipelines page
	// (https://docs.gitlab.com/ci/pipelines/merged_results_pipelines/). No
	// upstream Pipeline Hook EXAMPLE shows object_attributes.ref carrying either
	// form; the documented Pipeline Hook payload carries the target branch with
	// source = "merge_request_event" (see glMergeRequestPipelineFixture). Do not
	// read this fixture as evidence that the Pipeline Hook emits this shape — it
	// is not, and that has not been confirmed upstream. It pins the DEFENSIVE ref
	// arm, which may never fire on a real Pipeline Hook.
	//
	// The source field is omitted on purpose: this fixture must exercise the ref
	// signal ALONE, so the source signal cannot mask its deletion.
	glMergedResultsPipelineFixture = `{
		"object_kind": "pipeline",
		"object_attributes": {
			"id": 33,
			"iid": 5,
			"ref": "refs/merge-requests/7/merge",
			"sha": "bcbb5ec396a2c0f828686f14fac9b80b780504f2",
			"status": "failed"
		},
		"user": {"username": "root"},
		"project": {"id": 1, "path_with_namespace": "mike/diaspora"}
	}`
)

// TestParseGitLabEvent_PipelineFixtureRefShapes is the DONE-MEANS test for the
// committed fixtures (E45.30 / #2881). The fixture bodies are transcribed
// constants whose correctness no compiler enforces, so they are asserted
// BEHAVIOURALLY — driven through the real ParseGitLabEvent + MatchGitLabEvent
// — rather than by a presence check a comment-only edit would satisfy.
//
// The property the whole change turns on is the last one: NEITHER merge-request
// shape (target-branch ref, or refs/merge-requests/<iid>/merge) carries the
// gitLabRunBranchNamespace prefix, while the branch fixture does. That is why
// an MR pipeline lands in the not-a-run-branch arm at all.
func TestParseGitLabEvent_PipelineFixtureRefShapes(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantRef       string
		wantSource    string
		wantMRIID     int
		wantRunBranch bool
	}{
		{"branch_pipeline", glBranchPipelineFixture, "fishhawk/run-abcdef12", "push", 0, true},
		{"merge_request_pipeline", glMergeRequestPipelineFixture, "master", "merge_request_event", 7, false},
		{"merged_results_pipeline", glMergedResultsPipelineFixture, "refs/merge-requests/7/merge", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := ParseGitLabEvent("Pipeline Hook", "uuid-123", []byte(tc.body))
			if err != nil {
				t.Fatalf("ParseGitLabEvent: %v", err)
			}
			if ev.Type != "pipeline" {
				t.Fatalf("Type = %q, want pipeline", ev.Type)
			}
			m := MatchGitLabEvent(ev)
			if m.Skip || m.Action != MatchActionCIFailureRetry {
				t.Fatalf("match = %+v, want MatchActionCIFailureRetry", m)
			}
			if m.PipelineRef == nil {
				t.Fatal("PipelineRef = nil")
			}
			if got := m.PipelineRef.Ref; got != tc.wantRef {
				t.Errorf("Ref = %q, want %q", got, tc.wantRef)
			}
			if got := m.PipelineRef.Source; got != tc.wantSource {
				t.Errorf("Source = %q, want %q", got, tc.wantSource)
			}
			if got := m.PipelineRef.MergeRequestIID; got != tc.wantMRIID {
				t.Errorf("MergeRequestIID = %d, want %d", got, tc.wantMRIID)
			}
			gotRunBranch := strings.HasPrefix(m.PipelineRef.Ref, gitLabRunBranchNamespace)
			if gotRunBranch != tc.wantRunBranch {
				t.Errorf("ref %q has run-branch prefix = %v, want %v",
					m.PipelineRef.Ref, gotRunBranch, tc.wantRunBranch)
			}
		})
	}
}

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
