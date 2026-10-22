package server

import (
	"context"
	"encoding/json"
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
			// ADR-093 slice 2: leg (a) is bound to the latest round, so the
			// terminal row must follow a started row.
			f.mqeAppend(r, "implement_review_started", map[string]any{"configured_agents": 1})
			f.mqeAppend(r, "implement_reviewed", map[string]any{})
		}},
		{name: "required check red", spec: mcvUndelegatedSpecYAML, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.checks["h0"] = strings.Replace(mcqGreenChecks, `"success"`, `"failure"`, 1) })
		}, want: []string{mergeQueueReasonChecksRed}},
		{name: "required check red refuses admission", spec: mcvUndelegatedSpecYAML, admission: true, setup: func(f *mcqFixture, r *mcqRun) {
			// A snapshot naming ci makes the red run REQUIRED, so it stays red
			// at admission (a nil snapshot downgrades it, the next row).
			f.exec(`UPDATE runs SET required_checks_snapshot = '{"contexts":["ci"]}' WHERE id = $1`, r.id)
			f.gh.set(func() { f.gh.checks["h0"] = strings.Replace(mcqGreenChecks, `"success"`, `"failure"`, 1) })
		}, want: []string{mergeQueueReasonChecksRed}},
		{name: "nil snapshot optional red admits", spec: mcvUndelegatedSpecYAML, admission: true, setup: func(f *mcqFixture, r *mcqRun) {
			f.gh.set(func() { f.gh.checks["h0"] = strings.Replace(mcqGreenChecks, `"success"`, `"failure"`, 1) })
		}},
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
		if _, _, err := f.srv.mergeQueueChecksLeg(ctx, row, "", mergeQueueVariantEntry); err == nil {
			t.Fatal("empty head sha: want an error")
		}
		row.Repo = "not-a-repo"
		if _, _, err := f.srv.mergeQueueChecksLeg(ctx, row, "h0", mergeQueueVariantEntry); err == nil {
			t.Fatal("malformed repo: want an error")
		}
	})
}

// ADR-093 slice 2 (D3/D6, E87.5 / #4296): leg (a) bound to the latest
// implement-review round, changes_requested, the D6 approval transfer across
// clean advances, the merge variant and the nil-snapshot downgrade.

// mqeAppend appends one chained row on r BY CONSTRUCTION.
func (f *mcqFixture) mqeAppend(r *mcqRun, category string, payload map[string]any) *audit.Entry {
	f.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatalf("marshal %s: %v", category, err)
	}
	kind, subj := audit.ActorSystem, "test"
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{RunID: r.id, Timestamp: time.Now().UTC(),
		Category: category, ActorKind: &kind, ActorSubject: &subj, Payload: raw})
	if err != nil {
		f.t.Fatalf("seed %s: %v", category, err)
	}
	return e
}

// mqeIntent records a merge_queue_entered row on mcqBase anchored at head and
// returns its anchor.
func (f *mcqFixture) mqeIntent(r *mcqRun, head string) mergeQueueIntentAnchor {
	e := f.mqeAppend(r, CategoryMergeQueueEntered, map[string]any{
		"base_ref": mcqBase, "anchored_head_sha": head, "verdict": "ship it at " + head})
	return mergeQueueIntentAnchor{Sequence: e.Sequence, BaseRef: mcqBase}
}

// mqeRebased records a performed branch_rebased advance prior -> next whose
// merge commit is mergeCommit (clean when mergeCommit == next). Like the
// production writer, a clean advance carries clean_verified true (ADR-112
// R1(a)(ii): its merge-commit parents verified).
func (f *mcqFixture) mqeRebased(r *mcqRun, prior, next, mergeCommit string, upToDate bool) {
	f.mqeAppend(r, CategoryBranchRebased, map[string]any{"base_ref": mcqBase,
		"prior_head_sha": prior, "new_head_sha": next, "merge_commit_sha": mergeCommit, "already_up_to_date": upToDate,
		"clean_verified": !upToDate && mergeCommit != "" && mergeCommit == next})
}

// mqeUnverifiedAdvance records a clean-shaped advance prior -> next whose
// merge-commit parents did NOT verify (clean_verified false): it carries no
// forge approval (ADR-112).
func (f *mcqFixture) mqeUnverifiedAdvance(r *mcqRun, prior, next string) {
	f.mqeAppend(r, CategoryBranchRebased, map[string]any{"base_ref": mcqBase,
		"prior_head_sha": prior, "new_head_sha": next, "merge_commit_sha": next, "already_up_to_date": false,
		"clean_verified": false})
}

// mqePassed records a passed merge-candidate verdict at head.
func (f *mcqFixture) mqePassed(r *mcqRun, head string) {
	f.mqeAppend(r, CategoryMergeCandidateVerified, map[string]any{"head_sha": head, "result": mergeCandidateStatePassed})
}

// mqeDismissed is a reviews listing whose only approval (alice's) was
// dismissed, as GitHub's dismiss-stale setting does on an advance.
const mqeDismissed = `[{"id":1,"user":{"login":"alice"},"state":"APPROVED"},{"id":2,"user":{"login":"alice"},"state":"DISMISSED"}]`

// mqeNoVerifySpecYAML is mqeSpecYAML with no verify command, so every head's
// merge-candidate verify state is not_required.
var mqeNoVerifySpecYAML = strings.Replace(mqeSpecYAML,
	"          verify:\n            command: \"scripts/test verify\"\n            timeout: \"5m\"\n", "", 1)

// mqeGatedReviewersSpec is mqeSpecYAML with one configured agent implement
// reviewer.
var mqeGatedReviewersSpec = strings.Replace(mqeSpecYAML,
	"        produces:\n          - artifact: pull_request\n",
	"        reviewers:\n          agent: 1\n          human: 0\n        produces:\n          - artifact: pull_request\n", 1)

func requireBasis(t *testing.T, got mergeQueueEligibilityResult, want string) {
	t.Helper()
	if got.ApprovalBasis != want {
		t.Fatalf("approval basis = %q, want %q (%+v)", got.ApprovalBasis, want, got)
	}
}

// TestMergeQueueEligibility_ReviewLegBoundToLatestRound pins leg (a) to the
// LATEST implement-review round: an earlier fully terminal round never
// satisfies a newer started round with zero terminals.
func TestMergeQueueEligibility_ReviewLegBoundToLatestRound(t *testing.T) {
	ctx := context.Background()
	f := newMCQFixture(t)
	r := f.seed("round", "h0", []byte(mqeGatedReviewersSpec))
	f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("alice") })
	f.mqeAppend(r, "implement_review_started", map[string]any{"configured_agents": 1})
	f.mqeAppend(r, "implement_reviewed", map[string]any{})
	requireReasons(t, f.srv.mergeQueueEligibility(ctx, f.runRow(r), "h0"))

	// A fix-up re-review dispatches a fresh round with no terminal entry yet.
	f.mqeAppend(r, "implement_review_started", map[string]any{"configured_agents": 1})
	got := f.srv.mergeQueueEligibility(ctx, f.runRow(r), "h0")
	requireReasons(t, got, mergeQueueReasonReviewPending)
	if !strings.Contains(got.Reasons[0].Detail, "0 of 1") {
		t.Fatalf("detail = %q, want the latest round's 0 of 1", got.Reasons[0].Detail)
	}

	f.mqeAppend(r, "implement_review_failed", map[string]any{})
	requireReasons(t, f.srv.mergeQueueEligibility(ctx, f.runRow(r), "h0"))
}

// mqeErrStartedAudit fails only the implement_review_started read.
type mqeErrStartedAudit struct{ audit.Repository }

func (a mqeErrStartedAudit) ListForRunByCategory(ctx context.Context, id uuid.UUID, cat string) ([]*audit.Entry, error) {
	if cat == "implement_review_started" {
		return nil, errors.New("started read failed")
	}
	return a.Repository.ListForRunByCategory(ctx, id, cat)
}

// TestMergeQueueEligibility_ReviewRoundUnreadableIsUndeterminable pins an
// unreadable round as undeterminable, never a pass or a pending.
func TestMergeQueueEligibility_ReviewRoundUnreadableIsUndeterminable(t *testing.T) {
	f := newMCQFixture(t)
	r := f.seed("unr", "h0", []byte(mqeGatedReviewersSpec))
	f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("alice") })
	f.mqeAppend(r, "implement_review_started", map[string]any{"configured_agents": 1})
	f.mqeAppend(r, "implement_reviewed", map[string]any{})
	row := f.runRow(r)
	f.srv.cfg.AuditRepo = mqeErrStartedAudit{f.audit}
	got := f.srv.mergeQueueEligibility(context.Background(), row, "h0")
	requireReasons(t, got, mergeQueueReasonUndeterminable)
	if !got.Undeterminable || !strings.HasPrefix(got.Reasons[0].Detail, "review gate: ") {
		t.Fatalf("got %+v, want an undeterminable review-gate reason", got)
	}
}

// TestMergeQueueEligibility_ChangesRequestedFailsLegA pins changes_requested
// on every variant when an approval gate is declared (even beside a listed
// live approval, and with a transfer available), and its absence when the
// review stage declares no approval gate (approval condition b).
func TestMergeQueueEligibility_ChangesRequestedFailsLegA(t *testing.T) {
	ctx := context.Background()
	const requested = `[{"id":1,"user":{"login":"alice"},"state":"APPROVED"},{"id":2,"user":{"login":"bob"},"state":"COMMENTED"},` +
		`{"id":3,"user":{"login":"bob"},"state":"CHANGES_REQUESTED"},{"id":4,"user":{"login":"bob"},"state":"COMMENTED"}]`

	t.Run("every variant", func(t *testing.T) {
		f := newMCQFixture(t)
		r := f.seed("cr", "h0", []byte(mqeNoVerifySpecYAML))
		f.gh.set(func() { f.gh.reviews[r.pr] = requested })
		anchor := f.mqeIntent(r, "h0")
		row := f.runRow(r)
		for name, got := range map[string]mergeQueueEligibilityResult{
			"entry":        f.srv.mergeQueueEligibility(ctx, row, "h0"),
			"admission":    f.srv.mergeQueueAdmissionEligibility(ctx, row, "h0"),
			"admission at": f.srv.mergeQueueAdmissionEligibilityAt(ctx, row, "h0", anchor),
		} {
			t.Run(name, func(t *testing.T) {
				requireReasons(t, got, mergeQueueReasonChangesRequested)
				if !strings.Contains(got.Reasons[0].Detail, "bob") {
					t.Fatalf("detail = %q, want it to name bob", got.Reasons[0].Detail)
				}
				requireBasis(t, got, "")
			})
		}
		// ADR-112 variant restriction: the merge variant reads no forge
		// review (the in-lock fold judges the veto).
		requireReasons(t, f.srv.mergeQueueMergeEligibility(ctx, row, "h0", anchor))
	})

	t.Run("blocks a transfer", func(t *testing.T) {
		f := newMCQFixture(t)
		r := f.seed("crt", "h0", []byte(mqeNoVerifySpecYAML))
		anchor := f.mqeIntent(r, "h0")
		f.mqeRebased(r, "h0", "h1", "h1", false)
		f.gh.set(func() {
			f.gh.reviews[r.pr] = `[{"id":1,"user":{"login":"alice"},"state":"APPROVED"},{"id":2,"user":{"login":"alice"},"state":"DISMISSED"},` +
				`{"id":3,"user":{"login":"bob"},"state":"CHANGES_REQUESTED"}]`
		})
		requireReasons(t, f.srv.mergeQueueAdmissionEligibilityAt(ctx, f.runRow(r), "h1", anchor), mergeQueueReasonChangesRequested)
	})

	t.Run("no approval gate declared", func(t *testing.T) {
		f := newMCQFixture(t)
		noGate := mqeSpecYAML[:strings.Index(mqeSpecYAML, "        gates:\n")]
		r := f.seed("crn", "h0", []byte(noGate))
		f.gh.set(func() { f.gh.reviews[r.pr] = requested })
		got := f.srv.mergeQueueEligibility(ctx, f.runRow(r), "h0")
		requireReasons(t, got)
		requireBasis(t, got, "")
	})
}

// TestMergeQueueEligibility_ApprovalTransfersAcrossCleanAdvance pins D6: a
// dismissed approval carries across clean advances from the intent's head for
// the admission variant; the anchorless entry variant never transfers; a live
// approval reports the live basis. (The merge variant reads no forge review,
// ADR-112: TestMergeQueueEligibility_MergeVariantMakesNoForgeReviewRead.)
func TestMergeQueueEligibility_ApprovalTransfersAcrossCleanAdvance(t *testing.T) {
	ctx := context.Background()
	f := newMCQFixture(t)
	r := f.seed("tr", "h0", []byte(mqeSpecYAML))
	f.gh.set(func() { f.gh.reviews[r.pr] = mqeDismissed })
	anchor := f.mqeIntent(r, "h0")
	f.mqeRebased(r, "h0", "h1", "h1", false)
	f.mqeRebased(r, "h1", "h2", "h2", false)
	row := f.runRow(r)

	got := f.srv.mergeQueueAdmissionEligibilityAt(ctx, row, "h2", anchor)
	requireReasons(t, got)
	requireBasis(t, got, mergeQueueApprovalTransferred)

	requireReasons(t, f.srv.mergeQueueEligibility(ctx, row, "h2"), mergeQueueReasonNotApproved)
	requireReasons(t, f.srv.mergeQueueAdmissionEligibility(ctx, row, "h2"), mergeQueueReasonNotApproved)

	f.gh.set(func() { f.gh.reviews[r.pr] = mqeApprovedBy("alice") })
	got = f.srv.mergeQueueAdmissionEligibilityAt(ctx, row, "h2", anchor)
	requireReasons(t, got)
	requireBasis(t, got, mergeQueueApprovalLive)
}

// TestMergeQueueEligibility_ApprovalNotCarriedAcrossConflictResolution pins
// that a conflict-resolution push never carries the approval: neither to its
// own head (the fixture's ONLY route to the head) nor through it to a later
// clean advance.
func TestMergeQueueEligibility_ApprovalNotCarriedAcrossConflictResolution(t *testing.T) {
	ctx := context.Background()
	t.Run("conflict head", func(t *testing.T) {
		f := newMCQFixture(t)
		r := f.seed("cf", "h0", []byte(mqeNoVerifySpecYAML))
		f.gh.set(func() { f.gh.reviews[r.pr] = mqeDismissed })
		anchor := f.mqeIntent(r, "h0")
		f.mqeAppend(r, CategoryConflictResolutionPushed, map[string]any{"head_sha": "h1"})
		requireReasons(t, f.srv.mergeQueueAdmissionEligibilityAt(ctx, f.runRow(r), "h1", anchor), mergeQueueReasonNotApproved)
	})
	t.Run("clean advance after a conflict resolution", func(t *testing.T) {
		f := newMCQFixture(t)
		r := f.seed("cf2", "h0", []byte(mqeNoVerifySpecYAML))
		f.gh.set(func() { f.gh.reviews[r.pr] = mqeDismissed })
		anchor := f.mqeIntent(r, "h0")
		f.mqeRebased(r, "h0", "h1", "h1", false)
		f.mqeAppend(r, CategoryConflictResolutionPushed, map[string]any{"head_sha": "h2"})
		f.mqeRebased(r, "h2", "h3", "h3", false)
		got := f.srv.mergeQueueAdmissionEligibilityAt(ctx, f.runRow(r), "h3", anchor)
		requireReasons(t, got, mergeQueueReasonNotApproved)
		if !strings.Contains(got.Reasons[0].Detail, "clean advances only") {
			t.Fatalf("detail = %q, want the transfer refusal", got.Reasons[0].Detail)
		}
	})
}

// TestMergeQueueEligibility_ApprovalNotCarriedAcrossNonCleanAdvance pins the
// clean test: an advance whose new head is not its merge commit, one with no
// merge commit, and an already-up-to-date one carry nothing.
func TestMergeQueueEligibility_ApprovalNotCarriedAcrossNonCleanAdvance(t *testing.T) {
	ctx := context.Background()
	for name, adv := range map[string]struct {
		merge    string
		upToDate bool
	}{
		"new head is not the merge commit": {merge: "m1"},
		"no merge commit":                  {merge: ""},
		"already up to date":               {merge: "h1", upToDate: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCQFixture(t)
			r := f.seed("nc", "h0", []byte(mqeNoVerifySpecYAML))
			f.gh.set(func() { f.gh.reviews[r.pr] = mqeDismissed })
			anchor := f.mqeIntent(r, "h0")
			f.mqeRebased(r, "h0", "h1", adv.merge, adv.upToDate)
			requireReasons(t, f.srv.mergeQueueAdmissionEligibilityAt(ctx, f.runRow(r), "h1", anchor), mergeQueueReasonNotApproved)
		})
	}
}

// TestMergeQueueEligibility_UndecodableAdvanceCarriesNothing pins that a
// branch_rebased row whose payload does not decode carries no approval.
func TestMergeQueueEligibility_UndecodableAdvanceCarriesNothing(t *testing.T) {
	f := newMCQFixture(t)
	r := f.seed("ud", "h0", []byte(mqeNoVerifySpecYAML))
	f.gh.set(func() { f.gh.reviews[r.pr] = mqeDismissed })
	anchor := f.mqeIntent(r, "h0")
	f.mqeAppend(r, CategoryBranchRebased, map[string]any{"prior_head_sha": "h0", "new_head_sha": "h1",
		"merge_commit_sha": "h1", "already_up_to_date": "no"})
	requireReasons(t, f.srv.mergeQueueAdmissionEligibilityAt(context.Background(), f.runRow(r), "h1", anchor),
		mergeQueueReasonNotApproved)
}

// TestMergeQueueEligibility_MergeVariantMakesNoForgeReviewRead pins ADR-112's
// variant restriction (#4619): the merge variant makes ZERO REST review
// listings and ZERO GraphQL snapshots (a 500 listing and a dismissed-only
// listing both pass it, basis ""), while the entry and admission variants
// keep their REST legs and still make zero GraphQL calls.
func TestMergeQueueEligibility_MergeVariantMakesNoForgeReviewRead(t *testing.T) {
	ctx := context.Background()
	f := newMCQFixture(t)
	r := f.seed("mv", "h0", []byte(mqeNoVerifySpecYAML))
	f.gh.set(func() { f.gh.reviews[r.pr] = mqeDismissed })
	anchor := f.mqeIntent(r, "h0")
	row := f.runRow(r)
	got := f.srv.mergeQueueMergeEligibility(ctx, row, "h0", anchor)
	requireReasons(t, got)
	requireBasis(t, got, "")
	f.gh.set(func() { f.gh.reviewsStatus[r.pr] = 500 })
	requireReasons(t, f.srv.mergeQueueMergeEligibility(ctx, row, "h0", anchor))
	if got := f.srv.mergeQueueEligibility(ctx, row, "h0"); !got.Undeterminable {
		t.Fatalf("entry variant = %+v, want undeterminable on the 500 listing (its REST leg is kept)", got)
	}
	f.gh.set(func() { delete(f.gh.reviewsStatus, r.pr) })
	requireReasons(t, f.srv.mergeQueueEligibility(ctx, row, "h0"), mergeQueueReasonNotApproved)
	requireReasons(t, f.srv.mergeQueueAdmissionEligibility(ctx, row, "h0"), mergeQueueReasonNotApproved)
	requireReasons(t, f.srv.mergeQueueAdmissionEligibilityAt(ctx, row, "h0", anchor), mergeQueueReasonNotApproved)
	if n := f.gh.snapshotCalls(); n != 0 {
		t.Fatalf("GraphQL snapshot calls = %d, want 0 on every eligibility variant", n)
	}
}

// mqeErrCategoryAudit fails the read of one category.
type mqeErrCategoryAudit struct {
	audit.Repository
	category string
}

func (a mqeErrCategoryAudit) ListForRunByCategory(ctx context.Context, id uuid.UUID, cat string) ([]*audit.Entry, error) {
	if cat == a.category {
		return nil, errors.New(cat + " read failed")
	}
	return a.Repository.ListForRunByCategory(ctx, id, cat)
}

// TestMergeQueueEligibility_NoIntentAnchorNoTransfer pins that a transfer
// needs an anchor naming a recorded intent row, an advance off its head, and
// readable advance rows.
func TestMergeQueueEligibility_NoIntentAnchorNoTransfer(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*mcqFixture, *mcqRun, mergeQueueIntentAnchor) {
		f := newMCQFixture(t)
		r := f.seed("na", "h0", []byte(mqeNoVerifySpecYAML))
		f.gh.set(func() { f.gh.reviews[r.pr] = mqeDismissed })
		anchor := f.mqeIntent(r, "h0")
		f.mqeRebased(r, "h0", "h1", "h1", false)
		return f, r, anchor
	}
	t.Run("zero anchor", func(t *testing.T) {
		f := newMCQFixture(t)
		r := f.seed("za", "h0", []byte(mqeNoVerifySpecYAML))
		f.gh.set(func() { f.gh.reviews[r.pr] = mqeDismissed })
		// A row with an EMPTY base_ref, followed by a clean advance, would
		// match a zero anchor's legacy fallback and transfer, so only the
		// zero-anchor guard refuses here.
		f.mqeAppend(r, CategoryMergeQueueEntered, map[string]any{"base_ref": "", "anchored_head_sha": "h0"})
		f.mqeRebased(r, "h0", "h1", "h1", false)
		requireReasons(t, f.srv.mergeQueueAdmissionEligibilityAt(ctx, f.runRow(r), "h1", mergeQueueIntentAnchor{}), mergeQueueReasonNotApproved)
	})
	t.Run("anchor names no row", func(t *testing.T) {
		f, r, anchor := setup(t)
		got := f.srv.mergeQueueAdmissionEligibilityAt(ctx, f.runRow(r), "h1", mergeQueueIntentAnchor{Sequence: anchor.Sequence + 1000, BaseRef: mcqBase})
		requireReasons(t, got, mergeQueueReasonNotApproved)
		if !strings.Contains(got.Reasons[0].Detail, "no merge_queue_entered row") {
			t.Fatalf("detail = %q", got.Reasons[0].Detail)
		}
	})
	t.Run("anchor names another base", func(t *testing.T) {
		f, r, anchor := setup(t)
		requireReasons(t, f.srv.mergeQueueAdmissionEligibilityAt(ctx, f.runRow(r), "h1",
			mergeQueueIntentAnchor{Sequence: anchor.Sequence, BaseRef: "release"}), mergeQueueReasonNotApproved)
	})
	t.Run("head has not advanced", func(t *testing.T) {
		// The anchored head itself never transfers: an approval missing
		// there was withdrawn, not dismissed by an advance.
		f, r, anchor := setup(t)
		got := f.srv.mergeQueueAdmissionEligibilityAt(ctx, f.runRow(r), "h0", anchor)
		requireReasons(t, got, mergeQueueReasonNotApproved)
		if !strings.Contains(got.Reasons[0].Detail, "has not advanced") {
			t.Fatalf("detail = %q", got.Reasons[0].Detail)
		}
	})
	for _, cat := range []string{CategoryMergeQueueEntered, CategoryBranchRebased, CategoryConflictResolutionPushed} {
		t.Run(cat+" unreadable", func(t *testing.T) {
			f, r, anchor := setup(t)
			row := f.runRow(r)
			f.srv.cfg.AuditRepo = mqeErrCategoryAudit{Repository: f.audit, category: cat}
			got := f.srv.mergeQueueAdmissionEligibilityAt(ctx, row, "h1", anchor)
			if !got.Undeterminable || got.Eligible {
				t.Fatalf("got %+v, want undeterminable", got)
			}
		})
	}
	t.Run("no audit repository", func(t *testing.T) {
		f := newMCQFixture(t)
		f.srv.cfg.AuditRepo = nil
		if _, _, err := f.srv.mergeQueueIntentRow(ctx, uuid.New(), mergeQueueIntentAnchor{Sequence: 1, BaseRef: mcqBase}); err == nil {
			t.Fatal("want an error with no audit repository")
		}
	})
}

// TestMergeQueueEligibility_IntentRowSelectedByRecordedSequence pins that the
// intent row is the one AT the anchor's recorded sequence: a newer orphan row
// naming another head is ignored. A legacy anchor (sequence 0) falls back to
// the newest row naming the base.
func TestMergeQueueEligibility_IntentRowSelectedByRecordedSequence(t *testing.T) {
	ctx := context.Background()
	f := newMCQFixture(t)
	r := f.seed("seq", "h0", []byte(mqeNoVerifySpecYAML))
	f.gh.set(func() { f.gh.reviews[r.pr] = mqeDismissed })
	anchor := f.mqeIntent(r, "h0")
	f.mqeRebased(r, "h0", "h1", "h1", false)
	orphan := f.mqeIntent(r, "hX")
	row := f.runRow(r)

	got := f.srv.mergeQueueAdmissionEligibilityAt(ctx, row, "h1", anchor)
	requireReasons(t, got)
	requireBasis(t, got, mergeQueueApprovalTransferred)

	rec, found, err := f.srv.mergeQueueIntentRow(ctx, r.id, anchor)
	if err != nil || !found || rec.Sequence != anchor.Sequence || rec.AnchoredHeadSHA != "h0" || rec.Verdict != "ship it at h0" || rec.Entry == nil {
		t.Fatalf("intent row = %+v found=%v err=%v, want the row at %d", rec, found, err, anchor.Sequence)
	}
	legacy, found, err := f.srv.mergeQueueIntentRow(ctx, r.id, mergeQueueIntentAnchor{BaseRef: mcqBase})
	if err != nil || !found || legacy.Sequence != orphan.Sequence || legacy.AnchoredHeadSHA != "hX" {
		t.Fatalf("legacy intent row = %+v found=%v err=%v, want the newest row %d", legacy, found, err, orphan.Sequence)
	}
	// The legacy fallback resolves the newest row, whose head hX never
	// reached h1: no transfer.
	requireReasons(t, f.srv.mergeQueueAdmissionEligibilityAt(ctx, row, "h1", mergeQueueIntentAnchor{BaseRef: mcqBase}),
		mergeQueueReasonNotApproved)
}

// TestMergeQueueEligibility_NilSnapshotOptionalRedHoldsAtAdmission pins leg
// (c) with no required-checks snapshot: an observed red is not green, not
// red, for the queue variants — admission passes and merge holds — while the
// entry variant and a snapshot-named red stay checks_red.
func TestMergeQueueEligibility_NilSnapshotOptionalRedHoldsAtAdmission(t *testing.T) {
	ctx := context.Background()
	red := strings.Replace(mcqGreenChecks, `"success"`, `"failure"`, 1)
	f := newMCQFixture(t)
	r := f.seed("ns", "h0", []byte(mcvUndelegatedSpecYAML))
	f.gh.set(func() { f.gh.checks["h0"] = red })
	row := f.runRow(r)
	requireReasons(t, f.srv.mergeQueueAdmissionEligibility(ctx, row, "h0"))
	requireReasons(t, f.srv.mergeQueueAdmissionEligibilityAt(ctx, row, "h0", mergeQueueIntentAnchor{Sequence: 1, BaseRef: mcqBase}))
	merge := f.srv.mergeQueueMergeEligibility(ctx, row, "h0", mergeQueueIntentAnchor{})
	requireReasons(t, merge, mergeQueueReasonChecksNotGreen)
	if !strings.Contains(merge.Reasons[0].Detail, "no required-checks snapshot") {
		t.Fatalf("detail = %q", merge.Reasons[0].Detail)
	}
	requireReasons(t, f.srv.mergeQueueEligibility(ctx, row, "h0"), mergeQueueReasonChecksRed)

	f.exec(`UPDATE runs SET required_checks_snapshot = '{"contexts":["ci"]}' WHERE id = $1`, r.id)
	row = f.runRow(r)
	requireReasons(t, f.srv.mergeQueueMergeEligibility(ctx, row, "h0", mergeQueueIntentAnchor{}), mergeQueueReasonChecksRed)
	requireReasons(t, f.srv.mergeQueueAdmissionEligibility(ctx, row, "h0"), mergeQueueReasonChecksRed)
}

// TestMergeQueueEligibility_MergeVariantRequiresGreen pins the merge variant's
// leg (c): pending and zero check runs hold it like the entry variant.
func TestMergeQueueEligibility_MergeVariantRequiresGreen(t *testing.T) {
	ctx := context.Background()
	f := newMCQFixture(t)
	r := f.seed("mg", "h0", []byte(mcvUndelegatedSpecYAML))
	row := f.runRow(r)
	requireReasons(t, f.srv.mergeQueueMergeEligibility(ctx, row, "h0", mergeQueueIntentAnchor{}))
	f.gh.set(func() {
		f.gh.checks["h0"] = `[{"id":1,"name":"ci","status":"in_progress","started_at":"2026-01-01T00:00:00Z","check_suite":{"id":1}}]`
	})
	requireReasons(t, f.srv.mergeQueueMergeEligibility(ctx, row, "h0", mergeQueueIntentAnchor{}), mergeQueueReasonChecksNotGreen)
	f.gh.set(func() { f.gh.checks["h0"] = `[]` })
	got := f.srv.mergeQueueMergeEligibility(ctx, row, "h0", mergeQueueIntentAnchor{})
	if !got.Undeterminable {
		t.Fatalf("zero check runs: got %+v, want undeterminable", got)
	}
}
