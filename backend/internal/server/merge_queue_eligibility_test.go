package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-093 slice 1 (E87.4 / #4295): the merge-queue eligibility predicate,
// one behavioural row per leg and failure mode, over the real Postgres run,
// audit and concern repositories and the mcq fake GitHub.

// mqeSpecYAML is a v1 feature_change spec (plan, implement with a verify
// command, review) whose review approval gate lists github:alice.
const mqeSpecYAML = `version: "1.0"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "scripts/test verify"
            timeout: "5m"
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvals:
              count: 1
              members: ["github:alice"]
`

// mqeApprovedBy is a reviews listing whose only review is APPROVED by login.
func mqeApprovedBy(login string) string {
	return `[{"id":1,"user":{"login":"` + login + `"},"state":"APPROVED","submitted_at":"2026-01-01T00:00:00Z"}]`
}

func (f *mcqFixture) runRow(r *mcqRun) *run.Run {
	f.t.Helper()
	rr, err := f.repo.GetRun(context.Background(), r.id)
	if err != nil {
		f.t.Fatalf("get run: %v", err)
	}
	return rr
}

// raiseConcern persists one open concern on r with severity and provenance.
func (f *mcqFixture) raiseConcern(r *mcqRun, severity, provenance string) {
	f.t.Helper()
	if _, err := concern.NewPostgresRepository(f.pool).InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: r.id, StageID: r.impl.ID, StageKind: concern.StageKindImplement, Provenance: provenance,
		OriginReviewSequence: 1, Concerns: []concern.RaisedConcern{{Severity: severity, Category: "correctness", Note: "n"}},
	}); err != nil {
		f.t.Fatalf("insert concern: %v", err)
	}
}

func requireReasons(t *testing.T, got mergeQueueEligibilityResult, want ...string) {
	t.Helper()
	if strings.Join(got.codes(), ",") != strings.Join(want, ",") {
		t.Fatalf("eligibility reasons = %v (%+v), want %v", got.codes(), got.Reasons, want)
	}
	if got.Eligible != (len(want) == 0) {
		t.Fatalf("eligible = %v with reasons %v", got.Eligible, got.codes())
	}
}

func TestMergeQueueEligibility_Legs(t *testing.T) {
	ctx := context.Background()
	gatedReviewers := strings.Replace(mqeSpecYAML,
		"        produces:\n          - artifact: pull_request\n",
		"        reviewers:\n          agent: 1\n          human: 0\n        produces:\n          - artifact: pull_request\n", 1)
	cases := []struct {
		name      string
		spec      string
		setup     func(f *mcqFixture, r *mcqRun)
		admission bool
		want      []string
		undet     bool
	}{
		{name: "approved", spec: mqeSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("alice") })
		}},
		{name: "no live approval", spec: mqeSpecYAML, want: []string{mergeQueueReasonNotApproved}},
		{name: "approval by unlisted reviewer", spec: mqeSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("mallory") })
		}, want: []string{mergeQueueReasonNotApproved}},
		{name: "approval dismissed", spec: mqeSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() {
				f.gh.reviews[r.pr] = `[{"id":1,"user":{"login":"alice"},"state":"APPROVED"},{"id":2,"user":{"login":"alice"},"state":"DISMISSED"}]`
			})
		}, want: []string{mergeQueueReasonNotApproved}},
		{name: "spec without review stage passes leg a", spec: mcvUndelegatedSpecYAML},
		{name: "absent spec is undeterminable (C4)", spec: "", want: []string{mergeQueueReasonUndeterminable}, undet: true},
		{name: "unparseable spec is undeterminable (C4)", spec: "version: [", want: []string{mergeQueueReasonUndeterminable}, undet: true},
		{name: "agent review pending", spec: gatedReviewers, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("alice") })
		}, want: []string{mergeQueueReasonReviewPending}},
		{name: "agent review terminal", spec: gatedReviewers, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("alice") })
			kind, subj := audit.ActorSystem, "reviewer"
			if _, err := f.audit.AppendChained(ctx, audit.ChainAppendParams{RunID: r.id, Timestamp: time.Now().UTC(),
				Category: "implement_reviewed", ActorKind: &kind, ActorSubject: &subj, Payload: []byte(`{}`)}); err != nil {
				t.Fatalf("seed implement_reviewed: %v", err)
			}
		}},
		{name: "required check red", spec: mcvUndelegatedSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.checks["h0"] = strings.Replace(mcqGreenChecks, `"success"`, `"failure"`, 1) })
		}, want: []string{mergeQueueReasonChecksRed}},
		{name: "required check red refuses admission", spec: mcvUndelegatedSpecYAML, admission: true, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.checks["h0"] = strings.Replace(mcqGreenChecks, `"success"`, `"failure"`, 1) })
		}, want: []string{mergeQueueReasonChecksRed}},
		{name: "required check pending", spec: mcvUndelegatedSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() {
				f.gh.checks["h0"] = `[{"id":1,"name":"ci","status":"in_progress","started_at":"2026-01-01T00:00:00Z","check_suite":{"id":1}}]`
			})
		}, want: []string{mergeQueueReasonChecksNotGreen}},
		{name: "required check pending admits", spec: mcvUndelegatedSpecYAML, admission: true, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() {
				f.gh.checks["h0"] = `[{"id":1,"name":"ci","status":"in_progress","started_at":"2026-01-01T00:00:00Z","check_suite":{"id":1}}]`
			})
		}},
		{name: "zero check runs is undeterminable (C5)", spec: mcvUndelegatedSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.checks["h0"] = `[]` })
		}, want: []string{mergeQueueReasonUndeterminable}, undet: true},
		{name: "zero check runs admits", spec: mcvUndelegatedSpecYAML, admission: true, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.checks["h0"] = `[]` })
		}},
		{name: "required snapshot check absent", spec: mcvUndelegatedSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.exec(`UPDATE runs SET required_checks_snapshot = '{"contexts":["build"]}' WHERE id = $1`, r.id)
		}, want: []string{mergeQueueReasonChecksNotGreen}},
		{name: "high concern open", spec: mcvUndelegatedSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.raiseConcern(r, "high", "")
		}, want: []string{mergeQueueReasonBlockingConcernOpen}},
		{name: "first_officer concern open", spec: mcvUndelegatedSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.raiseConcern(r, "low", concern.ProvenanceFirstOfficer)
		}, want: []string{mergeQueueReasonBlockingConcernOpen}},
		{name: "medium concern alone stays eligible", spec: mcvUndelegatedSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.raiseConcern(r, "medium", "")
		}},
		{name: "reviews read error", spec: mqeSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.reviewsStatus[r.pr] = http.StatusInternalServerError })
		}, want: []string{mergeQueueReasonUndeterminable}, undet: true},
		{name: "check-runs read error", spec: mcvUndelegatedSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.checksStatus["h0"] = http.StatusInternalServerError })
		}, want: []string{mergeQueueReasonUndeterminable}, undet: true},
		{name: "concern read error", spec: mcvUndelegatedSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.srv.cfg.ConcernRepo = nil
		}, want: []string{mergeQueueReasonUndeterminable}, undet: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCQFixture(t)
			var specBytes []byte
			if tc.spec != "" {
				specBytes = []byte(tc.spec)
			}
			r := f.seed("e", "h0", specBytes)
			if tc.setup != nil {
				tc.setup(f, r)
			}
			var got mergeQueueEligibilityResult
			if tc.admission {
				got = f.srv.mergeQueueAdmissionEligibility(ctx, f.runRow(r), "h0")
			} else {
				got = f.srv.mergeQueueEligibility(ctx, f.runRow(r), "h0")
			}
			requireReasons(t, got, tc.want...)
			if got.Undeterminable != tc.undet {
				t.Fatalf("undeterminable = %v, want %v (%+v)", got.Undeterminable, tc.undet, got.Reasons)
			}
		})
	}
}

// TestMergeQueueEligibility_AcceptanceLeg pins leg (b): a pending acceptance
// stage is not merge-eligible.
func TestMergeQueueEligibility_AcceptanceLeg(t *testing.T) {
	f := newMCQFixture(t)
	spec := `version: "2"
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor: {agent: claude-code}
      - id: accept
        type: acceptance
        executor: {agent: claude-code}
`
	r := f.seed("acc", "h0", []byte(spec))
	if _, err := f.repo.CreateStage(context.Background(), run.CreateStageParams{
		RunID: r.id, Sequence: 4, Type: run.StageTypeAcceptance, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	}); err != nil {
		t.Fatalf("create acceptance stage: %v", err)
	}
	got := f.srv.mergeQueueEligibility(context.Background(), f.runRow(r), "h0")
	requireReasons(t, got, mergeQueueReasonAcceptanceNotEligible)
}

// mqeErrAudit, mqeErrRuns and mqeErrConcerns fail the one read each leg
// makes, so a read error is pinned as undeterminable rather than a pass.
type mqeErrAudit struct{ audit.Repository }

func (mqeErrAudit) ListForRunByCategory(context.Context, uuid.UUID, string) ([]*audit.Entry, error) {
	return nil, errors.New("audit read failed")
}

type mqeErrRuns struct{ run.Repository }

func (mqeErrRuns) ListStagesForRun(context.Context, uuid.UUID) ([]*run.Stage, error) {
	return nil, errors.New("stages read failed")
}

type mqeErrConcerns struct{ concern.Repository }

func (mqeErrConcerns) ListOpenByRun(context.Context, uuid.UUID) ([]*concern.Concern, error) {
	return nil, errors.New("concerns read failed")
}

// TestMergeQueueEligibility_ApprovalGateShapes pins leg (a)'s spec reads: an
// approval gate with no members needs any live approval, a review stage with
// no approval gate passes, and a spec missing the run's workflow is
// undeterminable.
func TestMergeQueueEligibility_ApprovalGateShapes(t *testing.T) {
	ctx := context.Background()
	noMembers := strings.Replace(mqeSpecYAML, "              members: [\"github:alice\"]\n", "", 1)
	noGate := mqeSpecYAML[:strings.Index(mqeSpecYAML, "        gates:\n")]
	otherWorkflow := strings.Replace(mqeSpecYAML, "  feature_change:\n", "  other_change:\n", 1)
	cases := []struct {
		name  string
		spec  string
		setup func(f *mcqFixture, r *mcqRun)
		want  []string
		undet bool
	}{
		{name: "membersless gate without approval", spec: noMembers, want: []string{mergeQueueReasonNotApproved}},
		{name: "membersless gate with any approval", spec: noMembers, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("mallory") })
		}},
		{name: "review stage without approval gate", spec: noGate},
		{name: "workflow absent from spec", spec: otherWorkflow, want: []string{mergeQueueReasonUndeterminable}, undet: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCQFixture(t)
			r := f.seed("g", "h0", []byte(tc.spec))
			if tc.setup != nil {
				tc.setup(f, r)
			}
			got := f.srv.mergeQueueEligibility(ctx, f.runRow(r), "h0")
			requireReasons(t, got, tc.want...)
			if got.Undeterminable != tc.undet {
				t.Fatalf("undeterminable = %v, want %v (%+v)", got.Undeterminable, tc.undet, got.Reasons)
			}
		})
	}
}

// TestMergeQueueEligibility_UnreadableIsUndeterminable pins every leg's
// unwired or failing read as undeterminable (never eligible).
func TestMergeQueueEligibility_UnreadableIsUndeterminable(t *testing.T) {
	ctx := context.Background()
	gatedReviewers := strings.Replace(mcvUndelegatedSpecYAML,
		"        produces:\n          - artifact: pull_request\n",
		"        reviewers:\n          agent: 1\n          human: 0\n        produces:\n          - artifact: pull_request\n", 1)

	t.Run("nil run row", func(t *testing.T) {
		f := newMCQFixture(t)
		got := f.srv.mergeQueueEligibility(ctx, nil, "h0")
		requireReasons(t, got, mergeQueueReasonUndeterminable)
		if !got.Undeterminable || !strings.Contains(got.Reasons[0].Detail, "run: ") {
			t.Fatalf("got %+v, want an undeterminable run reason", got)
		}
	})

	evalCases := []struct {
		name string
		spec string
		mut  func(f *mcqFixture)
		leg  string
	}{
		{name: "no run repository", spec: mcvUndelegatedSpecYAML, mut: func(f *mcqFixture) { f.srv.cfg.RunRepo = nil }, leg: "acceptance: "},
		{name: "stages read error", spec: mcvUndelegatedSpecYAML, mut: func(f *mcqFixture) { f.srv.cfg.RunRepo = mqeErrRuns{f.repo} }, leg: "acceptance: "},
		{name: "no audit repository", spec: gatedReviewers, mut: func(f *mcqFixture) { f.srv.cfg.AuditRepo = nil }, leg: "review gate: "},
		{name: "terminal review read error", spec: gatedReviewers, mut: func(f *mcqFixture) { f.srv.cfg.AuditRepo = mqeErrAudit{f.audit} }, leg: "review gate: "},
		{name: "no GitHub client", spec: mcvUndelegatedSpecYAML, mut: func(f *mcqFixture) { f.srv.cfg.GitHub = nil }, leg: "required checks: "},
		{name: "open concerns read error", spec: mcvUndelegatedSpecYAML, mut: func(f *mcqFixture) {
			f.srv.cfg.ConcernRepo = mqeErrConcerns{concern.NewPostgresRepository(f.pool)}
		}, leg: "concerns: "},
	}
	for _, tc := range evalCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCQFixture(t)
			r := f.seed("u", "h0", []byte(tc.spec))
			row := f.runRow(r)
			tc.mut(f)
			got := f.srv.mergeQueueEligibility(ctx, row, "h0")
			if got.Eligible || !got.Undeterminable {
				t.Fatalf("got %+v, want undeterminable", got)
			}
			var found bool
			for _, x := range got.Reasons {
				if x.Code == mergeQueueReasonUndeterminable && strings.HasPrefix(x.Detail, tc.leg) {
					found = true
				}
			}
			if !found {
				t.Fatalf("reasons %+v carry no undeterminable %q reason", got.Reasons, tc.leg)
			}
		})
	}

	t.Run("checks leg without a head or with a malformed repo", func(t *testing.T) {
		f := newMCQFixture(t)
		r := f.seed("c", "h0", []byte(mcvUndelegatedSpecYAML))
		row := f.runRow(r)
		if _, _, err := f.srv.mergeQueueChecksLeg(ctx, row, "", false); err == nil {
			t.Fatal("empty head sha: want an error")
		}
		row.Repo = "not-a-repo"
		if _, _, err := f.srv.mergeQueueChecksLeg(ctx, row, "h0", false); err == nil {
			t.Fatal("malformed repo: want an error")
		}
	})
}
