package modeloracle_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// fakeFetcher is an in-memory Fetcher (no network) whose result is swappable
// between Refresh calls so a test can drive success → failure → success.
type fakeFetcher struct {
	mu   sync.Mutex
	ids  []string
	err  error
	hits int
}

func (f *fakeFetcher) Fetch(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if f.err != nil {
		return nil, f.err
	}
	return append([]string(nil), f.ids...), nil
}

func (f *fakeFetcher) set(ids []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids, f.err = ids, err
}

// TestCached_UnregisteredProviderFailsOpen asserts a provider with no registered
// Fetcher reports (nil,false,false) — fail-open (binding condition 6).
func TestCached_UnregisteredProviderFailsOpen(t *testing.T) {
	o := modeloracle.NewCached(map[string]modeloracle.Fetcher{}, time.Hour, discardLogger())
	models, fresh, ok := o.Snapshot(context.Background(), "claudecode")
	if ok || fresh || models != nil {
		t.Errorf("Snapshot = (%v,%v,%v), want (nil,false,false) for an unregistered provider", models, fresh, ok)
	}
}

// TestCached_BeforeFirstSuccessFailsOpen asserts a registered provider whose
// fetch has not yet run reports ok=false (no Refresh called).
func TestCached_BeforeFirstSuccessFailsOpen(t *testing.T) {
	o := modeloracle.NewCached(
		map[string]modeloracle.Fetcher{"claudecode": &fakeFetcher{ids: []string{"claude-opus-4-8"}}},
		time.Hour, discardLogger())
	_, _, ok := o.Snapshot(context.Background(), "claudecode")
	if ok {
		t.Error("ok = true before any Refresh, want false (fail-open)")
	}
}

// TestCached_SuccessfulFetchAccepted asserts a fresh successful fetch reports the
// id set with fresh=true, ok=true.
func TestCached_SuccessfulFetchAccepted(t *testing.T) {
	f := &fakeFetcher{ids: []string{"claude-opus-4-8", "claude-sonnet-4-6"}}
	o := modeloracle.NewCached(map[string]modeloracle.Fetcher{"claudecode": f}, time.Hour, discardLogger())
	o.Refresh(context.Background())

	models, fresh, ok := o.Snapshot(context.Background(), "claudecode")
	if !ok || !fresh {
		t.Fatalf("Snapshot fresh=%v ok=%v, want both true", fresh, ok)
	}
	if !contains(models, "claude-opus-4-8") {
		t.Errorf("models = %v, want it to contain claude-opus-4-8", models)
	}
}

// TestCached_FreshnessDecaysPastThreshold asserts that with the default 24h
// staleness threshold, a snapshot fetched at T0 reads fresh just under 24h later
// and stale just over — driven by an injected clock (also covers the done-means
// 24h default behaviorally, condition 1).
func TestCached_FreshnessDecaysPastThreshold(t *testing.T) {
	base := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	f := &fakeFetcher{ids: []string{"claude-opus-4-8"}}
	o := modeloracle.NewCached(
		map[string]modeloracle.Fetcher{"claudecode": f},
		24*time.Hour, discardLogger(), modeloracle.WithClock(clock.now))
	o.Refresh(context.Background()) // lastSuccess = base

	clock.set(base.Add(23 * time.Hour))
	if _, fresh, ok := o.Snapshot(context.Background(), "claudecode"); !ok || !fresh {
		t.Errorf("at +23h: fresh=%v ok=%v, want both true", fresh, ok)
	}

	clock.set(base.Add(25 * time.Hour))
	if _, fresh, ok := o.Snapshot(context.Background(), "claudecode"); !ok || fresh {
		t.Errorf("at +25h: fresh=%v ok=%v, want ok=true fresh=false (stale → fail-open)", fresh, ok)
	}
}

// TestCached_FailedRefreshKeepsPriorModels asserts a fetch failure after a prior
// success keeps the prior models, stays ok=true, and (crucially) does NOT advance
// lastSuccess — so freshness keeps decaying from the last GOOD fetch rather than
// being reset by the failed attempt (binding condition 6).
func TestCached_FailedRefreshKeepsPriorModels(t *testing.T) {
	base := time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	f := &fakeFetcher{ids: []string{"claude-opus-4-8"}}
	o := modeloracle.NewCached(
		map[string]modeloracle.Fetcher{"claudecode": f},
		24*time.Hour, discardLogger(), modeloracle.WithClock(clock.now))
	o.Refresh(context.Background()) // lastSuccess = base

	// 10h later the provider goes down. lastAttempt advances to base+10h but
	// lastSuccess must stay at base, so the snapshot is still fresh (age 10h).
	clock.set(base.Add(10 * time.Hour))
	f.set(nil, errors.New("provider down"))
	o.Refresh(context.Background())

	models, fresh, ok := o.Snapshot(context.Background(), "claudecode")
	if !ok || !fresh {
		t.Fatalf("after failed refresh: fresh=%v ok=%v, want both true (prior snapshot retained)", fresh, ok)
	}
	if !contains(models, "claude-opus-4-8") {
		t.Errorf("models = %v, want the prior set retained", models)
	}

	// If lastSuccess had been bumped to base+10h, the snapshot would still read
	// fresh at base+30h. It must read stale, proving freshness decays from the
	// last GOOD fetch (base), not the failed attempt.
	clock.set(base.Add(30 * time.Hour))
	if _, fresh, _ := o.Snapshot(context.Background(), "claudecode"); fresh {
		t.Error("at +30h after a failed refresh: fresh=true, want false — lastSuccess must not have advanced")
	}
}

// TestCached_SetChangeAndDisappearedLogs asserts the membership-drift logging:
// an info line on the changed set and a high-signal WARN when a previously
// present id disappears (binding condition 6 observability).
func TestCached_SetChangeAndDisappearedLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := &fakeFetcher{ids: []string{"claude-opus-4-8", "claude-sonnet-4-6"}}
	o := modeloracle.NewCached(map[string]modeloracle.Fetcher{"claudecode": f}, time.Hour, logger)

	o.Refresh(context.Background()) // first success: populate line, no drift warn
	if !strings.Contains(buf.String(), "model snapshot populated") {
		t.Errorf("first refresh did not log a populate line; got:\n%s", buf.String())
	}

	buf.Reset()
	// Drop sonnet, add haiku → one added, one removed.
	f.set([]string{"claude-opus-4-8", "claude-haiku-4-5"}, nil)
	o.Refresh(context.Background())

	out := buf.String()
	if !strings.Contains(out, "model snapshot changed") {
		t.Errorf("missing change log; got:\n%s", out)
	}
	if !strings.Contains(out, "disappeared") {
		t.Errorf("missing disappeared WARN; got:\n%s", out)
	}
	if !strings.Contains(out, "claude-sonnet-4-6") {
		t.Errorf("disappeared log does not name the removed id; got:\n%s", out)
	}
	if !strings.Contains(out, "claude-haiku-4-5") {
		t.Errorf("change log does not name the added id; got:\n%s", out)
	}
}

// TestCached_NoChangeNoDriftLog asserts a refresh that returns the identical set
// logs no "changed"/"disappeared" line (only the silent happy path).
func TestCached_NoChangeNoDriftLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := &fakeFetcher{ids: []string{"gpt-5.5"}}
	o := modeloracle.NewCached(map[string]modeloracle.Fetcher{"codex": f}, time.Hour, logger)
	o.Refresh(context.Background())
	buf.Reset()
	o.Refresh(context.Background()) // identical set
	if strings.Contains(buf.String(), "model snapshot changed") {
		t.Errorf("unexpected change log on an unchanged set; got:\n%s", buf.String())
	}
}

// TestCached_RunStopsOnContextCancel asserts Run returns when its context is
// cancelled (the server signal-context shutdown path, condition 6).
func TestCached_RunStopsOnContextCancel(t *testing.T) {
	f := &fakeFetcher{ids: []string{"claude-opus-4-8"}}
	o := modeloracle.NewCached(map[string]modeloracle.Fetcher{"claudecode": f}, time.Hour, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx, time.Hour) // initial refresh fires, then blocks on the ticker
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of context cancellation")
	}
}

// --- cross-boundary seam: oracle <-> spec.ValidateModels (#1339 activation) ---
//
// This test lives in package modeloracle_test (external) and imports spec; spec
// imports modeloracle, so a `package modeloracle` test importing spec would be an
// import cycle (binding condition 2).

// TestCrossBoundary_FreshOracleDrivesValidation injects a live Cached oracle
// (seeded fresh under the existing "claudecode" key, condition 3) into
// spec.ValidateModels and asserts the three severity routes: a present model
// accepts, a typo'd model hard-rejects with a did-you-mean, and a never-fetched
// (ok=false) oracle fails open with a model_unverifiable warning.
func TestCrossBoundary_FreshOracleDrivesValidation(t *testing.T) {
	seeded := func() *modeloracle.Cached {
		o := modeloracle.NewCached(
			map[string]modeloracle.Fetcher{"claudecode": &fakeFetcher{ids: []string{"claude-opus-4-8", "claude-sonnet-4-6"}}},
			time.Hour, discardLogger())
		o.Refresh(context.Background()) // fresh + ok under "claudecode"
		return o
	}

	t.Run("present accepts", func(t *testing.T) {
		warnings, err := spec.ValidateModels(specWith("claude-code", "claude-opus-4-8"), seeded())
		if err != nil {
			t.Fatalf("err = %v, want nil for a present model", err)
		}
		if len(warnings) != 0 {
			t.Errorf("warnings = %v, want none", warnings)
		}
	})

	t.Run("typo hard-rejects with did-you-mean", func(t *testing.T) {
		_, err := spec.ValidateModels(specWith("claude-code", "claude-opus-4-9"), seeded())
		var ve *spec.ValidationError
		if !errors.As(err, &ve) {
			t.Fatalf("err = %v (%T), want *spec.ValidationError", err, err)
		}
		if !strings.Contains(ve.Message, "claude-opus-4-9") {
			t.Errorf("message %q does not name the rejected model", ve.Message)
		}
		if !strings.Contains(ve.Message, "did you mean") {
			t.Errorf("message %q lacks a did-you-mean suggestion", ve.Message)
		}
	})

	t.Run("never-fetched fails open", func(t *testing.T) {
		// A Cached oracle with the provider registered but never refreshed →
		// ok=false → fail open with a warning, never a reject.
		unfetched := modeloracle.NewCached(
			map[string]modeloracle.Fetcher{"claudecode": &fakeFetcher{ids: []string{"claude-opus-4-8"}}},
			time.Hour, discardLogger())
		warnings, err := spec.ValidateModels(specWith("claude-code", "claude-typo-9"), unfetched)
		if err != nil {
			t.Fatalf("err = %v, want nil (fail open on never-fetched)", err)
		}
		if len(warnings) != 1 || warnings[0].Code != spec.WarningCodeModelUnverifiable {
			t.Fatalf("warnings = %#v, want one model_unverifiable", warnings)
		}
	})
}

// specWith builds a one-workflow spec whose single implement stage names the
// given executor agent + model.
func specWith(agent, execModel string) *spec.Spec {
	return &spec.Spec{
		Version: "0.3",
		Workflows: map[string]spec.Workflow{
			"wf": {
				Stages: []spec.Stage{{
					ID:       "implement",
					Type:     spec.StageTypeImplement,
					Executor: spec.Executor{Agent: agent, Model: execModel},
				}},
			},
		},
	}
}

// fakeClock is a settable monotonic-free clock for freshness-decay tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(new(bytes.Buffer), nil))
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
