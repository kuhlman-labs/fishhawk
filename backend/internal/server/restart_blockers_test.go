package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// rbFixture is the restart-blockers server under test: the attention run fake
// (it carries a stage table) plus the audit fake, with the reconcile tests'
// fixed boot marker so "dispatched by this process" is deterministic.
type rbFixture struct {
	runs  *attnRunRepo
	audit *auditFake
}

func newRBFixture() *rbFixture {
	return &rbFixture{runs: newAttnRunRepo(), audit: newAuditFake()}
}

func (f *rbFixture) config() Config {
	return Config{Addr: "127.0.0.1:0", RunRepo: f.runs, AuditRepo: f.audit, ProcessStart: bootMarker}
}

func (f *rbFixture) server() *Server { return New(f.config()) }

// seedChild seeds a running decomposition child of parent with one implement
// stage in the given state.
func (f *rbFixture) seedChild(childID, parentID, stageID uuid.UUID, state run.StageState, created time.Time) {
	ru := f.runs.seed(childID, "acme/app", run.StateRunning, created, "")
	p := parentID
	ru.DecomposedFrom = &p
	f.runs.addStage(childID, stageID, run.StageTypeImplement, state, created)
}

// seedReviewRound seeds one *_review_started entry for a running run.
func (f *rbFixture) seedReviewRound(t *testing.T, runID uuid.UUID, kind string, seq int64, ts time.Time, configured int) {
	t.Helper()
	seedReviewAuditEntry(t, f.audit, runID, attnID(900), seq, ts, kind+"_review_started",
		planreview.ReviewStartedPayload{ConfiguredAgents: configured, Authority: planreview.AuthorityAdvisory})
}

func getRestartBlockers(t *testing.T, s *Server, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v0/restart-blockers", nil), id)
	rec := httptest.NewRecorder()
	s.handleListRestartBlockers(rec, req)
	return rec
}

func decodeRestartBlockers(t *testing.T, rec *httptest.ResponseRecorder) restartBlockersResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got restartBlockersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body %s", err, rec.Body.String())
	}
	return got
}

func anonymous() Identity { return Identity{Subject: "anonymous"} }

// TestRestartBlockers_UndispatchedChild — counterfactual: mutating the child
// predicate to never match yields zero items.
func TestRestartBlockers_UndispatchedChild(t *testing.T) {
	for _, state := range []run.StageState{run.StageStatePending, run.StageStateAwaitingHostDispatch} {
		t.Run(string(state), func(t *testing.T) {
			f := newRBFixture()
			f.seedChild(attnID(1), attnID(2), attnID(3), state, attnT0)
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if len(got.Items) != 1 {
				t.Fatalf("items = %+v, want one undispatched_child", got.Items)
			}
			it := got.Items[0]
			if it.RunID != attnID(1).String() || it.Reason != restartBlockerUndispatchedChild ||
				it.Stage != "implement" || it.ParentRunID != attnID(2).String() || it.StageState != string(state) {
				t.Errorf("item = %+v, want child %s of parent %s at %s", it, attnID(1), attnID(2), state)
			}
			if got.ScannedRuns != 1 || got.Truncated {
				t.Errorf("scanned_runs = %d truncated = %v, want 1/false", got.ScannedRuns, got.Truncated)
			}
		})
	}
}

// TestRestartBlockers_ChildDispatched_NotBlocker: a child whose implement
// stage already left pending/awaiting_host_dispatch blocks nothing.
func TestRestartBlockers_ChildDispatched_NotBlocker(t *testing.T) {
	for _, state := range []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded} {
		t.Run(string(state), func(t *testing.T) {
			f := newRBFixture()
			f.seedChild(attnID(1), attnID(2), attnID(3), state, attnT0)
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if len(got.Items) != 0 {
				t.Fatalf("items = %+v, want none for a dispatched child", got.Items)
			}
		})
	}
}

// TestRestartBlockers_NonChildPendingImplement_NotBlocker — counterfactual:
// deleting the DecomposedFrom check reports a non-child's pending implement.
func TestRestartBlockers_NonChildPendingImplement_NotBlocker(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
	f.runs.addStage(attnID(1), attnID(3), run.StageTypeImplement, run.StageStatePending, attnT0)
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 {
		t.Fatalf("items = %+v, want none for a non-decomposition run", got.Items)
	}
}

// TestRestartBlockers_ReviewInFlight: a round dispatched by THIS process with
// fewer verdicts than configured reviewers is a blocker, for both stages.
func TestRestartBlockers_ReviewInFlight(t *testing.T) {
	for _, kind := range []string{"plan", "implement"} {
		t.Run(kind, func(t *testing.T) {
			f := newRBFixture()
			f.runs.seed(attnID(1), "acme/app", run.StatePending, attnT0, "")
			f.seedReviewRound(t, attnID(1), kind, 1, afterBoot, 2)
			seedReviewAuditEntry(t, f.audit, attnID(1), attnID(900), 2, afterBoot, kind+"_reviewed", map[string]any{})
			got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
			if len(got.Items) != 1 {
				t.Fatalf("items = %+v, want one review_in_flight", got.Items)
			}
			it := got.Items[0]
			if it.Reason != restartBlockerReviewInFlight || it.Stage != kind || it.RunID != attnID(1).String() {
				t.Errorf("item = %+v, want %s review_in_flight on %s", it, kind, attnID(1))
			}
			if it.ConfiguredAgents == nil || *it.ConfiguredAgents != 2 || it.Landed == nil || *it.Landed != 1 {
				t.Errorf("counts = configured %v landed %v, want 2/1", it.ConfiguredAgents, it.Landed)
			}
		})
	}
}

// TestRestartBlockers_ReviewSettled_NotBlocker — counterfactual: mutating the
// landed comparison reports a settled round.
func TestRestartBlockers_ReviewSettled_NotBlocker(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
	f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 2)
	seedReviewAuditEntry(t, f.audit, attnID(1), attnID(900), 2, afterBoot, "implement_reviewed", map[string]any{})
	seedReviewAuditEntry(t, f.audit, attnID(1), attnID(900), 3, afterBoot, "implement_review_failed", map[string]any{})
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 {
		t.Fatalf("items = %+v, want none for a settled round", got.Items)
	}
}

// TestRestartBlockers_PriorProcessOrphan_NotBlocker — counterfactual: deleting
// the boot-marker condition reports a round an EARLIER process dispatched
// (the next boot sweep closes it; a restart loses nothing more).
func TestRestartBlockers_PriorProcessOrphan_NotBlocker(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
	f.seedReviewRound(t, attnID(1), "plan", 1, beforeBoot, 1)
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 {
		t.Fatalf("items = %+v, want none for a prior-process orphan", got.Items)
	}
}

// TestRestartBlockers_ZeroConfigured_NotBlocker: a round with no configured
// reviewer is never pending.
func TestRestartBlockers_ZeroConfigured_NotBlocker(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
	f.seedReviewRound(t, attnID(1), "plan", 1, afterBoot, 0)
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 {
		t.Fatalf("items = %+v, want none for a zero-reviewer round", got.Items)
	}
}

// TestRestartBlockers_AttemptCorrelation: the LATEST round decides, and only
// verdicts strictly after it count. Counterfactuals: reading the earliest
// round reports the settled arm; a run-wide terminal count hides the
// unsettled arm.
func TestRestartBlockers_AttemptCorrelation(t *testing.T) {
	t.Run("latest_settled_earlier_unsettled", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 2)
		f.seedReviewRound(t, attnID(1), "implement", 3, afterBoot, 1)
		seedReviewAuditEntry(t, f.audit, attnID(1), attnID(900), 4, afterBoot, "implement_reviewed", map[string]any{})
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 0 {
			t.Fatalf("items = %+v, want none: the latest round is settled", got.Items)
		}
	})
	t.Run("prior_round_verdict_not_counted", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 1)
		seedReviewAuditEntry(t, f.audit, attnID(1), attnID(900), 2, afterBoot, "implement_reviewed", map[string]any{})
		f.seedReviewRound(t, attnID(1), "implement", 3, afterBoot, 1)
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerReviewInFlight {
			t.Fatalf("items = %+v, want one review_in_flight for the fresh round", got.Items)
		}
		if *got.Items[0].Landed != 0 {
			t.Errorf("landed = %d, want 0 (the prior round's verdict must not count)", *got.Items[0].Landed)
		}
	})
}

// TestRestartBlockers_TerminalRunNotScanned — counterfactual: dropping the
// per-state filter scans a terminal run's stale started entry.
func TestRestartBlockers_TerminalRunNotScanned(t *testing.T) {
	f := newRBFixture()
	f.runs.seed(attnID(1), "acme/app", run.StateSucceeded, attnT0, "")
	f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 1)
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if len(got.Items) != 0 || got.ScannedRuns != 0 {
		t.Fatalf("items = %+v scanned_runs = %d, want nothing for a terminal run", got.Items, got.ScannedRuns)
	}
}

// TestRestartBlockers_CheckFailed: a per-run read error from a reachable
// daemon is reported as check_failed (scripts/dev refuses on it), never
// silently dropped. Counterfactual: mutating each error branch to `continue`
// drops the item.
func TestRestartBlockers_CheckFailed(t *testing.T) {
	t.Run("stage_read", func(t *testing.T) {
		f := newRBFixture()
		f.seedChild(attnID(1), attnID(2), attnID(3), run.StageStatePending, attnT0)
		f.runs.stageErr[attnID(1)] = errors.New("boom")
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed || got.Items[0].Stage != "implement" {
			t.Fatalf("items = %+v, want one implement check_failed", got.Items)
		}
	})
	t.Run("audit_started_read", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		f.audit.listByCategoryErrCategory = "plan_review_started"
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed || got.Items[0].Stage != "plan" {
			t.Fatalf("items = %+v, want one plan check_failed", got.Items)
		}
	})
	t.Run("started_payload_undecodable", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		rid, sid := attnID(1), attnID(900)
		f.audit.seeded = append(f.audit.seeded, &audit.Entry{
			RunID: &rid, StageID: &sid, Sequence: 1, Timestamp: afterBoot,
			Category: "implement_review_started", Payload: []byte("{not json"),
		})
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed || got.Items[0].Stage != "implement" {
			t.Fatalf("items = %+v, want one implement check_failed", got.Items)
		}
	})
	t.Run("audit_landed_read", func(t *testing.T) {
		f := newRBFixture()
		f.runs.seed(attnID(1), "acme/app", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, attnID(1), "implement", 1, afterBoot, 1)
		f.audit.listByCategoryErrCategory = "implement_review_skipped"
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
		if len(got.Items) != 1 || got.Items[0].Reason != restartBlockerCheckFailed || got.Items[0].Stage != "implement" {
			t.Fatalf("items = %+v, want one implement check_failed", got.Items)
		}
	})
}

// TestRestartBlockers_Truncated: the scan cap bites observably.
func TestRestartBlockers_Truncated(t *testing.T) {
	f := newRBFixture()
	for i := 0; i < restartBlockersRunScanLimit+1; i++ {
		f.runs.seed(uuid.New(), "acme/app", run.StateRunning, attnT0.Add(time.Duration(i)*time.Second), "")
	}
	got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), anonymous()))
	if !got.Truncated || got.ScannedRuns != restartBlockersRunScanLimit {
		t.Fatalf("truncated = %v scanned_runs = %d, want true/%d", got.Truncated, got.ScannedRuns, restartBlockersRunScanLimit)
	}
}

// TestRestartBlockers_Unconfigured: each required repository 503s by name.
func TestRestartBlockers_Unconfigured(t *testing.T) {
	f := newRBFixture()
	cfg := f.config()
	cfg.RunRepo = nil
	rec := getRestartBlockers(t, New(cfg), anonymous())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no run repo: status = %d, want 503", rec.Code)
	}
	assertErrorCode(t, rec, "run_repo_unconfigured")

	cfg = f.config()
	cfg.AuditRepo = nil
	rec = getRestartBlockers(t, New(cfg), anonymous())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no audit repo: status = %d, want 503", rec.Code)
	}
	assertErrorCode(t, rec, "audit_repo_unconfigured")
}

// TestRestartBlockers_ListRunsError: a run-scan failure is a 500, not an
// empty (clean-looking) list.
func TestRestartBlockers_ListRunsError(t *testing.T) {
	f := newRBFixture()
	f.runs.listErr = errors.New("db down")
	rec := getRestartBlockers(t, f.server(), anonymous())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
}

// TestRestartBlockers_Narrowing: the scan is bounded by the caller's account
// and the repo-visibility filter, exactly as GET /v0/runs; a filter fault
// fails the whole request closed.
func TestRestartBlockers_Narrowing(t *testing.T) {
	memberServer := func(f *rbFixture, vis RepoVisibility) *Server {
		cfg := f.config()
		cfg.AccountRoles = fakeAccountRoles{role: "member"}
		cfg.RepoVisibility = vis
		return New(cfg)
	}
	seedTwo := func(f *rbFixture) {
		f.runs.seed(attnID(1), "alpha/one", run.StateRunning, attnT0, "")
		f.seedReviewRound(t, attnID(1), "plan", 1, afterBoot, 1)
		f.runs.seed(attnID(4), "beta/two", run.StateRunning, attnT0.Add(time.Minute), "")
		f.seedReviewRound(t, attnID(4), "plan", 2, afterBoot, 1)
	}
	t.Run("repo_filter", func(t *testing.T) {
		f := newRBFixture()
		seedTwo(f)
		s := memberServer(f, newFakeRepoVisibility(map[string]bool{"alpha/one": true}))
		got := decodeRestartBlockers(t, getRestartBlockers(t, s, memberIdentity()))
		if len(got.Items) != 1 || got.Items[0].RunID != attnID(1).String() || got.ScannedRuns != 1 {
			t.Fatalf("items = %+v scanned = %d, want only alpha/one's run", got.Items, got.ScannedRuns)
		}
	})
	t.Run("repo_filter_error_fails_closed", func(t *testing.T) {
		f := newRBFixture()
		seedTwo(f)
		s := memberServer(f, fakeErrOnRepoVisibility{allowed: "alpha/one", failing: "beta/two"})
		rec := getRestartBlockers(t, s, memberIdentity())
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), attnID(1).String()) {
			t.Errorf("fail-closed body leaks a partially-narrowed page: %s", rec.Body.String())
		}
	})
	t.Run("account_filter", func(t *testing.T) {
		f := newRBFixture()
		seedTwo(f)
		acctA, acctB := uuid.NewString(), uuid.NewString()
		f.runs.runs[attnID(1)].AccountID = acctA
		f.runs.runs[attnID(4)].AccountID = acctB
		got := decodeRestartBlockers(t, getRestartBlockers(t, f.server(), Identity{Subject: "github:op", AccountID: acctA}))
		if len(got.Items) != 1 || got.Items[0].RunID != attnID(1).String() {
			t.Fatalf("items = %+v, want only account A's run", got.Items)
		}
	})
}

func restartBlockersGoldenPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot resolve the wire golden fixture path")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "testdata", "wire", "restart_blockers.json")
}

// TestRestartBlockers_EndToEnd_Golden drives the REAL mux (s.Handler()) with
// NO credential — the anonymous posture scripts/dev relies on — across all
// three reasons, and compares the RAW response bytes to the shared wire golden
// testdata/wire/restart_blockers.json. scripts/test-dev's RB-k case feeds the
// same file to scripts/dev's _parse_restart_blockers, so a field rename on
// either side fails one of the two. Regenerate with
// FISHHAWK_UPDATE_WIRE_GOLDEN=1.
func TestRestartBlockers_EndToEnd_Golden(t *testing.T) {
	f := newRBFixture()
	// undispatched_child
	f.seedChild(attnID(1), attnID(2), attnID(3), run.StageStateAwaitingHostDispatch, attnT0)
	// review_in_flight (plan)
	f.runs.seed(attnID(4), "acme/app", run.StatePending, attnT0.Add(time.Minute), "")
	f.seedReviewRound(t, attnID(4), "plan", 1, afterBoot, 2)
	// check_failed (child stage read)
	f.seedChild(attnID(5), attnID(2), attnID(6), run.StageStatePending, attnT0.Add(2*time.Minute))
	f.runs.stageErr[attnID(5)] = errors.New("boom")
	// a clean run that blocks nothing
	f.runs.seed(attnID(7), "acme/app", run.StateRunning, attnT0.Add(3*time.Minute), "")

	rec := httptest.NewRecorder()
	f.server().Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/restart-blockers", nil))
	got := decodeRestartBlockers(t, rec)
	if len(got.Items) != 3 || got.ScannedRuns != 4 || got.Truncated {
		t.Fatalf("items = %+v scanned = %d truncated = %v, want 3 items over 4 runs", got.Items, got.ScannedRuns, got.Truncated)
	}

	path := restartBlockersGoldenPath(t)
	if os.Getenv("FISHHAWK_UPDATE_WIRE_GOLDEN") == "1" {
		if err := os.WriteFile(path, rec.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wire golden %s: %v", path, err)
	}
	if !bytes.Equal(rec.Body.Bytes(), golden) {
		t.Fatalf("GET /v0/restart-blockers body drifted from the shared wire golden %s (scripts/test-dev RB-k parses that file — update both sides deliberately):\n--- got ---\n%s\n--- golden ---\n%s", path, rec.Body.String(), golden)
	}
}

// TestOpenAPI_RestartBlockersRouteDocumented: the route, its schemas and both
// closed enums are in the OpenAPI source of truth.
func TestOpenAPI_RestartBlockersRouteDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"\n  /v0/restart-blockers:\n", "operationId: listRestartBlockers",
		"\n    RestartBlockerList:\n", "\n    RestartBlocker:\n",
		"enum: [undispatched_child, review_in_flight, check_failed]",
		"enum: [plan, implement]", "audit_repo_unconfigured",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml is missing %q", strings.TrimSpace(want))
		}
	}
}
