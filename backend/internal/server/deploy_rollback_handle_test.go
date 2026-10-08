package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
)

// deploymentArtifact builds a stored deployment artifact on stageID whose
// content is body, created at base+offset. Tests seed these directly into a
// fakeArtifactRepo (bypassing Create) so CreatedAt and the list order are
// under the test's control.
func deploymentArtifact(t *testing.T, stageID uuid.UUID, base time.Time, offset time.Duration, body deploymentBody) *artifact.Artifact {
	t.Helper()
	content, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal deployment body: %v", err)
	}
	return &artifact.Artifact{
		ID:        uuid.New(),
		StageID:   stageID,
		Kind:      artifact.KindDeployment,
		Content:   content,
		CreatedAt: base.Add(offset),
	}
}

// forwardDeployment is a forward (non-rollback) deployment record carrying
// handle (empty for the github_actions reconciler's poll-state shape).
func forwardDeployment(handle string) deploymentBody {
	return deploymentBody{
		Environment:    "production",
		Ref:            "abc123",
		ExternalRunURL: "https://github.com/kuhlman-labs/example/actions/runs/1",
		Outcome:        "succeeded",
		RollbackHandle: handle,
	}
}

// TestStoredRollbackHandleFor pins the stored rollback_handle selection rule
// (E35.3 / #1600): the NEWEST FORWARD deployment record carrying a non-empty
// handle, by CreatedAt the function sorts itself. Every multi-record row seeds
// the fake NEWEST-FIRST (the reverse of ListForStage's documented ASC order),
// so a selection that trusted the list order instead of sorting returns the
// wrong record.
func TestStoredRollbackHandleFor(t *testing.T) {
	base := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	stageID := uuid.New()

	type want struct {
		handle string
		// artifactIdx indexes the seeded slice; -1 means uuid.Nil.
		artifactIdx int
	}
	rows := []struct {
		name    string
		seed    func() []*artifact.Artifact
		listErr error
		want    want
		wantErr bool
	}{
		{
			// (a) the handle-bearing pipeline callback is OLDER than a
			// handle-less reconciler record: the older handle wins.
			name: "older_handle_beats_newer_handleless_record",
			seed: func() []*artifact.Artifact {
				return []*artifact.Artifact{
					deploymentArtifact(t, stageID, base, 2*time.Minute, forwardDeployment("")),
					deploymentArtifact(t, stageID, base, time.Minute, forwardDeployment("rev-abc")),
				}
			},
			want: want{handle: "rev-abc", artifactIdx: 1},
		},
		{
			// (a2) two handle-bearing forward records: the NEWEST by
			// CreatedAt wins even though it is listed first.
			name: "newest_handle_wins_regardless_of_list_order",
			seed: func() []*artifact.Artifact {
				return []*artifact.Artifact{
					deploymentArtifact(t, stageID, base, 2*time.Minute, forwardDeployment("rev-new")),
					deploymentArtifact(t, stageID, base, time.Minute, forwardDeployment("rev-old")),
				}
			},
			want: want{handle: "rev-new", artifactIdx: 0},
		},
		{
			// (b1) a NEWER rollback sub-action record (rollback_action set)
			// carrying a different handle is not a forward record.
			name: "rollback_action_record_ignored",
			seed: func() []*artifact.Artifact {
				rb := forwardDeployment("rev-rollback")
				rb.RollbackAction = "initiated"
				return []*artifact.Artifact{
					deploymentArtifact(t, stageID, base, 2*time.Minute, rb),
					deploymentArtifact(t, stageID, base, time.Minute, forwardDeployment("rev-abc")),
				}
			},
			want: want{handle: "rev-abc", artifactIdx: 1},
		},
		{
			// (b2) a NEWER rolled_back-outcome record WITHOUT a rollback_action
			// is still a revert record, not the deploy to revert.
			name: "rolled_back_outcome_record_ignored",
			seed: func() []*artifact.Artifact {
				rb := forwardDeployment("rev-rollback")
				rb.Outcome = "rolled_back"
				return []*artifact.Artifact{
					deploymentArtifact(t, stageID, base, 2*time.Minute, rb),
					deploymentArtifact(t, stageID, base, time.Minute, forwardDeployment("rev-abc")),
				}
			},
			want: want{handle: "rev-abc", artifactIdx: 1},
		},
		{
			// (c) no forward record carries a handle: the NEWEST forward
			// record's id comes back with an empty handle.
			name: "no_handle_returns_newest_forward_artifact",
			seed: func() []*artifact.Artifact {
				return []*artifact.Artifact{
					deploymentArtifact(t, stageID, base, 2*time.Minute, forwardDeployment("")),
					deploymentArtifact(t, stageID, base, time.Minute, forwardDeployment("")),
				}
			},
			want: want{handle: "", artifactIdx: 0},
		},
		{
			// (d) no deployment artifacts at all: the zero value.
			name: "no_deployment_artifacts",
			seed: func() []*artifact.Artifact { return nil },
			want: want{artifactIdx: -1},
		},
		{
			// (e) a NEWER non-deployment artifact whose content happens to
			// decode as a handle-bearing deployment record is ignored.
			name: "non_deployment_kind_ignored",
			seed: func() []*artifact.Artifact {
				other := deploymentArtifact(t, stageID, base, 2*time.Minute, forwardDeployment("rev-plan"))
				other.Kind = artifact.KindPlan
				return []*artifact.Artifact{
					other,
					deploymentArtifact(t, stageID, base, time.Minute, forwardDeployment("rev-abc")),
				}
			},
			want: want{handle: "rev-abc", artifactIdx: 1},
		},
		{
			// (h) a NEWER undecodable deployment row is skipped, not fatal,
			// and never counts as the newest forward record. The older record
			// is handle-less so the skip is observable: a row that fell
			// through undecoded (a zero body reads as forward) would become
			// the newest forward artifact.
			name: "undecodable_row_skipped",
			seed: func() []*artifact.Artifact {
				bad := deploymentArtifact(t, stageID, base, 2*time.Minute, forwardDeployment(""))
				bad.Content = json.RawMessage(`{"rollback_handle": 7`)
				return []*artifact.Artifact{
					bad,
					deploymentArtifact(t, stageID, base, time.Minute, forwardDeployment("")),
				}
			},
			want: want{handle: "", artifactIdx: 1},
		},
		{
			// (f) a ListForStage error is returned, never swallowed.
			name:    "list_error_returned",
			seed:    func() []*artifact.Artifact { return nil },
			listErr: errors.New("artifact store down"),
			wantErr: true,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s, _, _, _ := newApprovalServer(t)
			logBuf := captureLogger(s)
			seeded := row.seed()
			repo := newFakeArtifactRepo()
			repo.all = seeded
			repo.listErr = row.listErr
			s.cfg.ArtifactRepo = repo

			got, err := s.storedRollbackHandleFor(context.Background(), stageID)
			if row.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want the ListForStage error; got %+v", got)
				}
				if !errors.Is(err, row.listErr) {
					t.Errorf("err = %v, want it to wrap %v", err, row.listErr)
				}
				if got != (storedRollbackHandle{}) {
					t.Errorf("got %+v on error, want the zero value", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("storedRollbackHandleFor: %v", err)
			}
			wantID := uuid.Nil
			if row.want.artifactIdx >= 0 {
				wantID = seeded[row.want.artifactIdx].ID
			}
			if got.Handle != row.want.handle || got.ArtifactID != wantID {
				t.Errorf("got {artifact %s, handle %q}, want {artifact %s, handle %q}",
					got.ArtifactID, got.Handle, wantID, row.want.handle)
			}
			if row.name == "undecodable_row_skipped" && !strings.Contains(logBuf.String(), "does not decode") {
				t.Errorf("undecodable row skipped without a WARN; log:\n%s", logBuf.String())
			}
		})
	}
}

// (g) a nil ArtifactRepo is the unconfigured posture: the zero value and no
// error, so a backend without an artifact store rolls back exactly as before
// the handle was wired.
func TestStoredRollbackHandleFor_NilArtifactRepo(t *testing.T) {
	s, _, _, _ := newApprovalServer(t)
	s.cfg.ArtifactRepo = nil
	got, err := s.storedRollbackHandleFor(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != (storedRollbackHandle{}) {
		t.Errorf("got %+v, want the zero value", got)
	}
	if got.artifactIDString() != "" {
		t.Errorf("artifactIDString() = %q, want empty for the zero value", got.artifactIDString())
	}
}
