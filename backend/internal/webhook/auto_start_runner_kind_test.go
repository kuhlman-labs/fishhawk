package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The webhook intake's auto-start runner kind (#4525): Dispatcher.RunnerKind,
// wired from fishhawkd's --auto-start-runner-kind. A host-dispatched kind
// (local) mints a local run, fires NO workflow_dispatch and parks the first
// stage pending -> awaiting_host_dispatch; the empty default keeps today's
// github_actions dispatch byte-for-byte.

// autoStartTriggers are the two GitHub intake triggers Handle mints a run for:
// a labeled issue and a `/fishhawk run` comment.
func autoStartTriggers(t *testing.T) map[string]Event {
	t.Helper()
	return map[string]Event{
		"issues.labeled":          issueLabeledEvent(t),
		"issue_comment /fishhawk": issueApproveCommentEvent(t, "/fishhawk run"),
	}
}

// dispatchAuditPayload decodes the single run_dispatched entry's payload.
func dispatchAuditPayload(t *testing.T, au *stubAudit) map[string]any {
	t.Helper()
	if len(au.appended) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(au.appended))
	}
	if au.appended[0].Category != "run_dispatched" {
		t.Fatalf("audit category = %q, want run_dispatched", au.appended[0].Category)
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[0].Payload, &payload); err != nil {
		t.Fatalf("decode run_dispatched payload: %v", err)
	}
	return payload
}

func TestHandle_AutoStartRunnerKindLocal_ParksWithoutDispatch(t *testing.T) {
	for name, ev := range autoStartTriggers(t) {
		t.Run(name, func(t *testing.T) {
			d, gh, runs, au := newDispatcherWithStubs(t)
			d.RunnerKind = run.RunnerKindLocal
			notifier := &stubIssueNotifier{}
			d.IssueNotifier = notifier
			logs := captureDispatcherLogs(d)

			if err := d.Handle(context.Background(), ev); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if gh.dispatchCalls != 0 {
				t.Errorf("DispatchWorkflow calls = %d, want 0 (a local run must not fire fishhawk.yml)", gh.dispatchCalls)
			}
			if len(runs.created) != 1 {
				t.Fatalf("runs created = %d, want 1", len(runs.created))
			}
			if got := runs.created[0].RunnerKind; got != run.RunnerKindLocal {
				t.Errorf("minted RunnerKind = %q, want local", got)
			}
			if len(runs.transitions) != 1 {
				t.Fatalf("stage transitions = %d, want exactly 1", len(runs.transitions))
			}
			if tr := runs.transitions[0]; tr.To != run.StageStateAwaitingHostDispatch || tr.StageID != runs.createdStages[0].ID {
				t.Errorf("transition = %+v, want first stage %s -> awaiting_host_dispatch", tr, runs.createdStages[0].ID)
			}

			payload := dispatchAuditPayload(t, au)
			if payload["outcome"] != "awaiting_host_dispatch" {
				t.Errorf("outcome = %v, want awaiting_host_dispatch", payload["outcome"])
			}
			if payload["runner_kind"] != run.RunnerKindLocal {
				t.Errorf("runner_kind = %v, want local", payload["runner_kind"])
			}
			if _, ok := payload["error"]; ok {
				t.Errorf("parked payload carries error: %v", payload["error"])
			}
			// A parked run is a successful pickup: the status comment fires.
			if len(notifier.statusCalls) != 1 {
				t.Errorf("status comment calls = %d, want 1", len(notifier.statusCalls))
			}
			out := logs.String()
			if !strings.Contains(out, `"msg":"webhook run parked awaiting host dispatch"`) {
				t.Errorf("missing park log line:\n%s", out)
			}
			if strings.Contains(out, `"msg":"webhook dispatched"`) {
				t.Errorf("parked run logged webhook dispatched:\n%s", out)
			}
		})
	}
}

func TestHandle_AutoStartRunnerKindDefault_DispatchesUnchanged(t *testing.T) {
	for name, ev := range autoStartTriggers(t) {
		t.Run(name, func(t *testing.T) {
			d, gh, runs, au := newDispatcherWithStubs(t)
			// RunnerKind left empty: the default deployment.

			if err := d.Handle(context.Background(), ev); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if gh.dispatchCalls != 1 {
				t.Errorf("DispatchWorkflow calls = %d, want 1", gh.dispatchCalls)
			}
			if len(runs.created) != 1 || runs.created[0].RunnerKind != run.RunnerKindGitHubActions {
				t.Fatalf("minted runs = %+v, want one github_actions run", runs.created)
			}
			if len(runs.transitions) != 1 || runs.transitions[0].To != run.StageStateDispatched {
				t.Errorf("transitions = %+v, want one -> dispatched", runs.transitions)
			}
			payload := dispatchAuditPayload(t, au)
			if payload["outcome"] != "dispatched" {
				t.Errorf("outcome = %v, want dispatched", payload["outcome"])
			}
			if _, ok := payload["runner_kind"]; ok {
				t.Errorf("default payload gained a runner_kind key: %v", payload)
			}
		})
	}
}

func TestHandle_AutoStartRunnerKindLocal_ParkFailureIsDispatchFailed(t *testing.T) {
	d, gh, runs, au := newDispatcherWithStubs(t)
	d.RunnerKind = run.RunnerKindLocal
	notifier := &stubIssueNotifier{}
	d.IssueNotifier = notifier
	runs.transitionErr = errors.New("row locked")

	if err := d.Handle(context.Background(), issueLabeledEvent(t)); err != nil {
		t.Fatalf("Handle: %v (a park failure is recorded, never a webhook retry)", err)
	}
	if gh.dispatchCalls != 0 {
		t.Errorf("DispatchWorkflow calls = %d, want 0", gh.dispatchCalls)
	}
	if len(runs.created) != 1 {
		t.Fatalf("runs created = %d, want 1", len(runs.created))
	}
	payload := dispatchAuditPayload(t, au)
	if payload["outcome"] != "dispatch_failed" {
		t.Errorf("outcome = %v, want dispatch_failed", payload["outcome"])
	}
	if e, _ := payload["error"].(string); !strings.Contains(e, "row locked") {
		t.Errorf("error = %v, want the park error", payload["error"])
	}
	if got := au.appended[0].RunID; got != runs.created[0].ID {
		t.Errorf("dispatch_failed audit RunID = %s, want the minted run %s", got, runs.created[0].ID)
	}
	if len(notifier.statusCalls) != 0 {
		t.Errorf("status comment calls = %d, want 0 on a park failure", len(notifier.statusCalls))
	}
	// The stage stays pending, which the host-dispatch marker still admits.
	if st := runs.createdStages[0].State; st != run.StageStatePending {
		t.Errorf("first stage state = %q, want pending", st)
	}
}
