package stallwatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recordingScanner records every Params it is called with.
type recordingScanner struct {
	mu    sync.Mutex
	calls []Params
	err   error
	panic bool
}

func (r *recordingScanner) scan(_ context.Context, p Params) (Report, error) {
	r.mu.Lock()
	r.calls = append(r.calls, p)
	r.mu.Unlock()
	if r.panic {
		panic("scanner exploded")
	}
	return Report{Scanned: 1, Paged: 1}, r.err
}

func (r *recordingScanner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func validTicker(a, b *recordingScanner) *Ticker {
	return &Ticker{FailedStages: a.scan, Children: b.scan, Threshold: 30 * time.Minute,
		Lookback: 168 * time.Hour, Interval: time.Hour}
}

func TestRun_RefusesDegenerateConfig(t *testing.T) {
	for name, mut := range map[string]func(*Ticker){
		"nil failed-stage scanner": func(t *Ticker) { t.FailedStages = nil },
		"nil child scanner":        func(t *Ticker) { t.Children = nil },
		"zero threshold":           func(t *Ticker) { t.Threshold = 0 },
		"negative threshold":       func(t *Ticker) { t.Threshold = -time.Minute },
		"zero interval":            func(t *Ticker) { t.Interval = 0 },
		"negative interval":        func(t *Ticker) { t.Interval = -time.Minute },
		"lookback below threshold": func(t *Ticker) { t.Lookback = 29 * time.Minute },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := &recordingScanner{}, &recordingScanner{}
			tk := validTicker(a, b)
			mut(tk)
			// Bounded, so a Run that wrongly starts returns nil instead of
			// blocking the test.
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			if err := tk.Run(ctx); err == nil {
				t.Fatal("Run accepted a degenerate config")
			}
			if a.count()+b.count() != 0 {
				t.Errorf("a refused Run still ticked: %d + %d scans", a.count(), b.count())
			}
		})
	}
}

func TestRun_TicksImmediatelyThenStops(t *testing.T) {
	a, b := &recordingScanner{}, &recordingScanner{}
	tk := validTicker(a, b)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tk.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for a.count() == 0 || b.count() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run did not tick immediately")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on cancel, want nil", err)
	}
}

func TestTick_PassesParamsToBothScanners(t *testing.T) {
	a, b := &recordingScanner{}, &recordingScanner{}
	tk := validTicker(a, b)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tk.Now = func() time.Time { return now }
	tk.Tick(context.Background())
	want := Params{Threshold: 30 * time.Minute, Lookback: 168 * time.Hour, Now: now}
	for name, r := range map[string]*recordingScanner{"failed": a, "children": b} {
		if r.count() != 1 || r.calls[0] != want {
			t.Errorf("%s scanner calls = %+v, want one with %+v", name, r.calls, want)
		}
	}
}

func TestTick_FailingScannerDoesNotStopTheOther(t *testing.T) {
	for name, setup := range map[string]func(a *recordingScanner){
		"error": func(a *recordingScanner) { a.err = errors.New("db down") },
		"panic": func(a *recordingScanner) { a.panic = true },
	} {
		t.Run(name, func(t *testing.T) {
			// The failing scanner runs FIRST, so the second only runs when
			// the first's failure is contained.
			a, b := &recordingScanner{}, &recordingScanner{}
			setup(a)
			tk := validTicker(a, b)
			tk.Tick(context.Background())
			if b.count() != 1 {
				t.Errorf("child scanner calls = %d after a failed-stage %s, want 1", b.count(), name)
			}
			// And the reverse order: a failing child scanner after a healthy one.
			c, d := &recordingScanner{}, &recordingScanner{}
			setup(d)
			tk = validTicker(c, d)
			tk.Tick(context.Background())
			if c.count() != 1 || d.count() != 1 {
				t.Errorf("calls = %d/%d, want 1/1", c.count(), d.count())
			}
		})
	}
}
