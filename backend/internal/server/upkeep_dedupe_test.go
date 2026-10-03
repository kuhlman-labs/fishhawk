package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// The upkeep dedupe adapter's tests reuse the intake hook's fakes
// (igReadProvider, igRegisterReadProvider, igLogSink) and the process-wide
// conventions seam (installConventions). Helpers here carry a ud prefix.

// udUpkeepLogMsg is the adapter's own degrade log message.
const udUpkeepLogMsg = "upkeep dedupe degraded; report ingested without duplicate marks"

// udRun is a run row for the repo the fakes serve.
func udRun(installationID *int64) *run.Run {
	return &run.Run{ID: uuid.New(), Repo: "kuhlman-labs/fishhawk", InstallationID: installationID}
}

// udProposals are two findings: one an open issue carries the marker for, one
// an open issue restates the title of, plus one nothing covers.
func udProposals() []upkeep.Proposal {
	return []upkeep.Proposal{
		{FindingID: "flake:TestWidgetSync", Title: "Quarantine flaky test TestWidgetSync", Type: "bug"},
		{FindingID: "deprecation:ioutil", Title: "Replace deprecated ioutil calls", Type: "chore"},
		{FindingID: "toolchain_drift:go", Title: "Align the drifting Go toolchain pins", Type: "chore"},
	}
}

// udAssertDegraded asserts res is a degradation with exactly want and an
// empty, NON-nil duplicates set.
func udAssertDegraded(t *testing.T, res upkeepDedupeResult, want string) {
	t.Helper()
	if !res.Degraded {
		t.Fatalf("result = %+v, want a degradation with reason %q", res, want)
	}
	if res.DegradeReason != want {
		t.Errorf("degrade reason = %q, want %q", res.DegradeReason, want)
	}
	if res.Duplicates == nil || len(res.Duplicates) != 0 {
		t.Errorf("duplicates = %#v, want a non-nil empty slice on a degrade", res.Duplicates)
	}
}

// udAssertAdapterLogged asserts the adapter WARN-logged reason under its own
// message.
func udAssertAdapterLogged(t *testing.T, sink *igLogSink, reason string) {
	t.Helper()
	for _, rec := range sink.snapshot() {
		if rec.msg == udUpkeepLogMsg && rec.reason == reason {
			if rec.detail == "" {
				t.Errorf("degradation %q logged with an empty detail", reason)
			}
			return
		}
	}
	t.Errorf("no adapter WARN record for degrade_reason %q; captured = %+v", reason, sink.snapshot())
}

// TestUpkeepDuplicates_SeededOpenDuplicatesMarked drives the adapter end to
// end: run row -> conventions -> run-scoped target -> intakeCandidates -> the
// registered fake reader -> upkeep.MarkDuplicates.
//
// Counterfactual: the window holds two OPEN duplicates (#41 carries the
// marker for the flake finding; #42 restates the deprecation title) plus one
// CLOSED issue carrying the deprecation's exact marker, so deleting the
// MarkDuplicates call (returning an empty set) drops both marks — RED. The
// closed #43 marks nothing either way: the deprecation is marked by #42 on
// similarity, never by #43's marker.
func TestUpkeepDuplicates_SeededOpenDuplicatesMarked(t *testing.T) {
	p := &igReadProvider{
		items: []workmgmt.WorkItemRecord{
			{Number: 43, Title: "Unrelated sweep", Body: upkeep.FindingMarker("deprecation:ioutil"), State: "closed", URL: "https://example.test/43"},
			{Number: 42, Title: "Replace deprecated ioutil calls", URL: "https://example.test/42"},
			{Number: 41, Title: "Unrelated invoices rollup", Body: "filed\n" + upkeep.FindingMarker("flake:TestWidgetSync"), URL: "https://example.test/41"},
		},
		truncated: true,
	}
	igRegisterReadProvider(t, p)
	installConventions(t, workmgmt.Default(), nil)
	s := New(Config{})

	inst := int64(4242)
	res := s.upkeepDuplicates(context.Background(), udRun(&inst), udProposals())

	if res.Degraded {
		t.Fatalf("result degraded (%q), want a healthy dedupe", res.DegradeReason)
	}
	want := []upkeep.Duplicate{
		{FindingID: "flake:TestWidgetSync", IssueNumber: 41, IssueURL: "https://example.test/41", Basis: upkeep.BasisMarker},
		{FindingID: "deprecation:ioutil", IssueNumber: 42, IssueURL: "https://example.test/42", Basis: upkeep.BasisSimilarity},
	}
	if len(res.Duplicates) != len(want) {
		t.Fatalf("duplicates = %+v, want %d marks", res.Duplicates, len(want))
	}
	for i, w := range want {
		g := res.Duplicates[i]
		if g.FindingID != w.FindingID || g.IssueNumber != w.IssueNumber || g.IssueURL != w.IssueURL || g.Basis != w.Basis {
			t.Errorf("duplicates[%d] = %+v, want %+v", i, g, w)
		}
	}
	if res.ScannedItems != 3 {
		t.Errorf("scanned items = %d, want 3 (the whole window, closed included)", res.ScannedItems)
	}
	if !res.WindowTruncated {
		t.Error("window truncated = false, want the reader's truncation carried through")
	}

	// The read was run-scoped: the run's coordinates and its installation.
	p.mu.Lock()
	got := p.gotRequest
	p.mu.Unlock()
	if got.Target.Repo != (workmgmt.Repo{Owner: "kuhlman-labs", Name: "fishhawk"}) {
		t.Errorf("reader target repo = %+v, want kuhlman-labs/fishhawk", got.Target.Repo)
	}
	if got.Target.Scope != forge.FromGitHubInstallationID(inst) {
		t.Errorf("reader target scope = %+v, want the run's installation %d", got.Target.Scope, inst)
	}
}

// TestUpkeepDuplicates_NoProposalsSkipsTheRead: a report with no findings is
// not deduped against the forge at all.
func TestUpkeepDuplicates_NoProposalsSkipsTheRead(t *testing.T) {
	p := &igReadProvider{}
	igRegisterReadProvider(t, p)
	installConventions(t, workmgmt.Default(), nil)
	s := New(Config{})

	res := s.upkeepDuplicates(context.Background(), udRun(nil), nil)
	if res.Degraded || res.Duplicates == nil || len(res.Duplicates) != 0 {
		t.Errorf("result = %#v, want a healthy, non-nil empty result", res)
	}
	p.mu.Lock()
	calls := p.listCalls
	p.mu.Unlock()
	if calls != 0 {
		t.Errorf("reader list calls = %d, want 0 for an empty report", calls)
	}
}

// TestUpkeepDuplicates_ReaderUnavailable: a File-only provider.
func TestUpkeepDuplicates_ReaderUnavailable(t *testing.T) {
	registerFakeProvider(t, &fakeWorkProvider{})
	installConventions(t, workmgmt.Default(), nil)
	s := New(Config{})

	res := s.upkeepDuplicates(context.Background(), udRun(nil), udProposals())
	udAssertDegraded(t, res, string(intakegroom.DegradeReasonReaderUnavailable))
}

// TestUpkeepDuplicates_ReaderError: the reader is present and refuses.
func TestUpkeepDuplicates_ReaderError(t *testing.T) {
	igRegisterReadProvider(t, &igReadProvider{listErr: errors.New("forge said no")})
	installConventions(t, workmgmt.Default(), nil)
	s := New(Config{})

	res := s.upkeepDuplicates(context.Background(), udRun(nil), udProposals())
	udAssertDegraded(t, res, string(intakegroom.DegradeReasonReaderError))
}

// TestUpkeepDuplicates_BudgetExceeded: a wedged (cancellation-cooperative)
// reader is bounded by the adapter's own deadline and reported as
// budget_exceeded. With the adapter's WithTimeout deleted the reader sees the
// caller's never-cancelled context, falls through to its wedge timer and
// returns a non-deadline error — reader_error, and the elapsed bound blown.
func TestUpkeepDuplicates_BudgetExceeded(t *testing.T) {
	deadline := timescale.D(200 * time.Millisecond)
	wedge := timescale.D(20 * time.Second)
	bound := deadline + timescale.D(2*time.Second)
	if bound >= wedge {
		t.Fatalf("test is not discriminating: bound %s >= wedge %s", bound, wedge)
	}
	igRegisterReadProvider(t, &igReadProvider{wedge: wedge})
	installConventions(t, workmgmt.Default(), nil)
	s := New(Config{IntakeGroomDeadline: deadline})

	start := time.Now()
	res := s.upkeepDuplicates(context.Background(), udRun(nil), udProposals())
	elapsed := time.Since(start)

	udAssertDegraded(t, res, string(intakegroom.DegradeReasonBudgetExceeded))
	if elapsed > bound {
		t.Errorf("dedupe took %s, want <= %s (the adapter's deadline did not bound the read)", elapsed, bound)
	}
}

// TestUpkeepDuplicates_ConventionsUnavailable: the conventions loader errors.
func TestUpkeepDuplicates_ConventionsUnavailable(t *testing.T) {
	p := &igReadProvider{}
	igRegisterReadProvider(t, p)
	installConventions(t, workmgmt.Conventions{}, errors.New("work-management.yaml unreadable"))
	cfg := Config{}
	sink := igCaptureDegrades(&cfg)
	s := New(cfg)

	res := s.upkeepDuplicates(context.Background(), udRun(nil), udProposals())
	udAssertDegraded(t, res, upkeepDedupeReasonConventionsUnavailable)
	udAssertAdapterLogged(t, sink, upkeepDedupeReasonConventionsUnavailable)
	if p.listCalls != 0 {
		t.Errorf("reader list calls = %d, want 0 without conventions", p.listCalls)
	}
}

// TestUpkeepDuplicates_RepoMalformed: a run Repo with no slash names no
// tracker target.
func TestUpkeepDuplicates_RepoMalformed(t *testing.T) {
	p := &igReadProvider{}
	igRegisterReadProvider(t, p)
	calls := installConventions(t, workmgmt.Default(), nil)
	cfg := Config{}
	sink := igCaptureDegrades(&cfg)
	s := New(cfg)

	runRow := udRun(nil)
	runRow.Repo = "fishhawk"
	res := s.upkeepDuplicates(context.Background(), runRow, udProposals())
	udAssertDegraded(t, res, upkeepDedupeReasonRepoMalformed)
	udAssertAdapterLogged(t, sink, upkeepDedupeReasonRepoMalformed)
	if *calls != 0 || p.listCalls != 0 {
		t.Errorf("conventions calls = %d, reader list calls = %d, want 0 for a malformed repo", *calls, p.listCalls)
	}
}

// TestUpkeepDuplicates_HookPanic: a panicking reader degrades to hook_panic.
//
// Counterfactual: removing the adapter's deferred recover lets the reader's
// panic out of upkeepDuplicates and crashes the test binary — the honest RED.
// The nil-run row exercises the same guard from the adapter's own frame and
// pins that the degrade log is nil-safe inside the recover.
func TestUpkeepDuplicates_HookPanic(t *testing.T) {
	t.Run("panicking reader", func(t *testing.T) {
		igRegisterReadProvider(t, &igReadProvider{panicOnList: true})
		installConventions(t, workmgmt.Default(), nil)
		cfg := Config{}
		sink := igCaptureDegrades(&cfg)
		s := New(cfg)

		res := s.upkeepDuplicates(context.Background(), udRun(nil), udProposals())
		udAssertDegraded(t, res, string(intakegroom.DegradeReasonHookPanic))
		udAssertAdapterLogged(t, sink, string(intakegroom.DegradeReasonHookPanic))
	})
	t.Run("nil run row", func(t *testing.T) {
		cfg := Config{}
		sink := igCaptureDegrades(&cfg)
		s := New(cfg)

		res := s.upkeepDuplicates(context.Background(), nil, udProposals())
		udAssertDegraded(t, res, string(intakegroom.DegradeReasonHookPanic))
		udAssertAdapterLogged(t, sink, string(intakegroom.DegradeReasonHookPanic))
	})
}

// udGitHubInstallServer is a GitHub API stub answering the repo-installation
// lookup with status and body, counting hits.
func udGitHubInstallServer(t *testing.T, status int, body string) (*githubclient.Client, *int) {
	t.Helper()
	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}, &hits
}

// TestUpkeepDuplicates_ResolvesInstallationWhenRunCarriesNone: a run with no
// installation id on a github_projects repo resolves the App installation, as
// the run-absent filing path does, and reads under that scope.
func TestUpkeepDuplicates_ResolvesInstallationWhenRunCarriesNone(t *testing.T) {
	gh, hits := udGitHubInstallServer(t, http.StatusOK, `{"id":77}`)
	p := &igReadProvider{}
	igRegisterReadProvider(t, p)
	installConventions(t, workmgmt.Default(), nil)
	s := New(Config{GitHub: gh})

	res := s.upkeepDuplicates(context.Background(), udRun(nil), udProposals())
	if res.Degraded {
		t.Fatalf("result degraded (%q), want a healthy dedupe", res.DegradeReason)
	}
	if *hits != 1 {
		t.Errorf("installation lookups = %d, want 1", *hits)
	}
	p.mu.Lock()
	scope := p.gotRequest.Target.Scope
	p.mu.Unlock()
	if scope != forge.FromGitHubInstallationID(77) {
		t.Errorf("reader target scope = %+v, want the resolved installation 77", scope)
	}
}

// TestUpkeepDuplicates_InstallationLookupErrorIsReaderError: a failed
// installation lookup means the window cannot be read; it degrades to
// reader_error without dialing the reader.
func TestUpkeepDuplicates_InstallationLookupErrorIsReaderError(t *testing.T) {
	gh, _ := udGitHubInstallServer(t, http.StatusInternalServerError, `{"message":"boom"}`)
	p := &igReadProvider{}
	igRegisterReadProvider(t, p)
	installConventions(t, workmgmt.Default(), nil)
	cfg := Config{GitHub: gh}
	sink := igCaptureDegrades(&cfg)
	s := New(cfg)

	res := s.upkeepDuplicates(context.Background(), udRun(nil), udProposals())
	udAssertDegraded(t, res, string(intakegroom.DegradeReasonReaderError))
	udAssertAdapterLogged(t, sink, string(intakegroom.DegradeReasonReaderError))
	if p.listCalls != 0 {
		t.Errorf("reader list calls = %d, want 0 after a failed installation lookup", p.listCalls)
	}
}

// TestUpkeepDuplicates_ConventionsDeadline_BudgetExceeded: a conventions load
// that outlives the dedupe budget is attributed to the budget, not to the
// conventions.
//
// The loader blocks until its context is done and returns ctx.Err(), so the
// ONLY way it errors is the adapter's deadline. Counterfactual: reverting the
// dctx.Err() attribution on the conventions branch reports
// conventions_unavailable — RED.
func TestUpkeepDuplicates_ConventionsDeadline_BudgetExceeded(t *testing.T) {
	p := &igReadProvider{}
	igRegisterReadProvider(t, p)
	prev := conventionsLoader
	conventionsLoader = func(ctx context.Context, _ string) (workmgmt.Conventions, error) {
		<-ctx.Done()
		return workmgmt.Conventions{}, ctx.Err()
	}
	t.Cleanup(func() { conventionsLoader = prev })
	cfg := Config{IntakeGroomDeadline: timescale.D(20 * time.Millisecond)}
	sink := igCaptureDegrades(&cfg)
	s := New(cfg)

	res := s.upkeepDuplicates(context.Background(), udRun(nil), udProposals())
	udAssertDegraded(t, res, string(intakegroom.DegradeReasonBudgetExceeded))
	udAssertAdapterLogged(t, sink, string(intakegroom.DegradeReasonBudgetExceeded))
	if p.listCalls != 0 {
		t.Errorf("reader list calls = %d, want 0 after the conventions load timed out", p.listCalls)
	}
}

// TestUpkeepDuplicates_InstallationLookupDeadline_BudgetExceeded: an
// installation lookup that outlives the dedupe budget is attributed to the
// budget, not reported as a reader error.
//
// The stub's installation endpoint holds the response timescale.D(200ms) —
// well past the timescale.D(20ms) budget — so the lookup fails only on the
// adapter's deadline. Counterfactual: reverting the dctx.Err() attribution on
// the installation branch reports reader_error — RED.
func TestUpkeepDuplicates_InstallationLookupDeadline_BudgetExceeded(t *testing.T) {
	hold := timescale.D(200 * time.Millisecond)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(hold):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":77}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "ghs_jwt", nil },
	}
	p := &igReadProvider{}
	igRegisterReadProvider(t, p)
	installConventions(t, workmgmt.Default(), nil)
	cfg := Config{GitHub: gh, IntakeGroomDeadline: timescale.D(20 * time.Millisecond)}
	sink := igCaptureDegrades(&cfg)
	s := New(cfg)

	res := s.upkeepDuplicates(context.Background(), udRun(nil), udProposals())
	udAssertDegraded(t, res, string(intakegroom.DegradeReasonBudgetExceeded))
	udAssertAdapterLogged(t, sink, string(intakegroom.DegradeReasonBudgetExceeded))
	if p.listCalls != 0 {
		t.Errorf("reader list calls = %d, want 0 after the installation lookup timed out", p.listCalls)
	}
}
