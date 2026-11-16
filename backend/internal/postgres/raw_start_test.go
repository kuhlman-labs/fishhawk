package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

type rawStartFakeContainer struct {
	testcontainers.Container
	terminated *atomic.Int32
}

func (c *rawStartFakeContainer) Terminate(context.Context, ...testcontainers.TerminateOption) error {
	c.terminated.Add(1)
	return nil
}

// Deliberately non-parallel: the shipped starter's factory/slots are replaced
// only while these hermetic tests run. Parallel migration tests start later.
func TestRawContainerStart_BoundsShippedCallSiteThroughPortInspection(t *testing.T) {
	originalRun, originalSlots := runRawPostgres, rawContainerStartSlots
	defer func() { runRawPostgres, rawContainerStartSlots = originalRun, originalSlots }()
	var terminated atomic.Int32
	synctest.Test(t, func(t *testing.T) {
		rawContainerStartSlots = make(chan struct{}, cap(originalSlots))
		hold := make(chan struct{})
		released := false
		defer func() {
			if !released {
				close(hold)
			}
		}()
		var started, active, peak, completed atomic.Int32
		runRawPostgres = func(context.Context) (testcontainers.Container, connStringFunc, error) {
			id := started.Add(1)
			return &rawStartFakeContainer{terminated: &terminated}, func(context.Context) (string, error) {
				n := active.Add(1)
				defer active.Add(-1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				<-hold // Model a Docker mapped-port inspection still in flight.
				return fmt.Sprintf("postgres://fixture/start-%d", id), nil
			}, nil
		}
		urls := make(chan string, 14)
		for range 14 {
			go func() {
				urls <- startRawContainer(t)
				completed.Add(1)
			}()
		}
		synctest.Wait()
		if got := started.Load(); got != 4 {
			t.Errorf("constructors admitted before port inspection finished = %d, want 4", got)
		}
		if got := completed.Load(); got != 0 {
			t.Errorf("completed before port inspection released = %d, want 0", got)
		}
		close(hold)
		released = true
		synctest.Wait()
		if got := peak.Load(); got > 4 {
			t.Errorf("peak concurrent port inspections = %d, want <= 4", got)
		}
		if got := completed.Load(); got != 14 {
			t.Fatalf("completed starts = %d, want 14", got)
		}
		if got := len(rawContainerStartSlots); got != 0 {
			t.Errorf("startup slots held after starts = %d, want 0", got)
		}
		if got := terminated.Load(); got != 0 {
			t.Errorf("cleanup ran before the test ended: %d calls", got)
		}
		unique := make(map[string]bool)
		for range 14 {
			unique[<-urls] = true
		}
		if len(unique) != 14 {
			t.Errorf("distinct fixture URLs = %d, want one per test", len(unique))
		}
	})
	if got := terminated.Load(); got != 14 {
		t.Errorf("container cleanups = %d, want 14", got)
	}
}

func TestRawContainerStart_FreshBudgetAfterAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slots := make(chan struct{}, 1)
		slots <- struct{}{}
		admission, cancel := context.WithTimeout(context.Background(), timescale.D(time.Second))
		defer cancel()
		admissionDeadline, _ := admission.Deadline()
		f := &fakeFataler{}
		var startedAt, startupDeadline time.Time
		done := make(chan string, 1)
		go func() {
			done <- startRawContainerAdmitted(f, noopCleanup, admission, slots,
				func(ctx context.Context) (testcontainers.Container, connStringFunc, error) {
					startedAt = time.Now()
					startupDeadline, _ = ctx.Deadline()
					return &fakeContainer{}, func(context.Context) (string, error) { return "fixture", nil }, nil
				})
		}()
		synctest.Wait()
		time.Sleep(timescale.D(500 * time.Millisecond)) // Virtual time spent waiting for admission.
		<-slots
		synctest.Wait()
		if got := <-done; got != "fixture" || f.fatalfCalled {
			t.Fatalf("start = %q, fatal = %v (%s)", got, f.fatalfCalled, f.message())
		}
		if !startupDeadline.After(admissionDeadline) || startupDeadline.Sub(startedAt) != timescale.D(rawContainerStartBudget) {
			t.Errorf("startup deadline %v spent admission budget (start %v, admission deadline %v)", startupDeadline, startedAt, admissionDeadline)
		}
	})
}

func TestRawContainerStart_AdmissionFailureDoesNotStartOrStealSlot(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%v", expired), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				slots := make(chan struct{}, 1)
				slots <- struct{}{}
				admission, cancel := context.WithTimeout(context.Background(), timescale.D(time.Second))
				defer cancel()
				if !expired {
					cancel()
				}
				f, called := &fakeFataler{}, false
				got := startRawContainerAdmitted(f, noopCleanup, admission, slots,
					func(context.Context) (testcontainers.Container, connStringFunc, error) {
						called = true
						return nil, nil, errors.New("must not start")
					})
				if got != "" || called || !f.fatalfCalled || f.skipfCalled || !strings.Contains(f.message(), "startup admission: context") {
					t.Errorf("admission = %q, called=%v fatal=%v skip=%v message=%q", got, called, f.fatalfCalled, f.skipfCalled, f.message())
				}
				if len(slots) != 1 {
					t.Errorf("admission consumed someone else's slot: %d held, want 1", len(slots))
				}
			})
		})
	}
}

func TestRawContainerStart_AlreadyCancelledWithFreeSlotDoesNotStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slots := make(chan struct{}, 1)
	f, called := &fakeFataler{}, false
	startRawContainerAdmitted(f, noopCleanup, ctx, slots,
		func(context.Context) (testcontainers.Container, connStringFunc, error) {
			called = true
			return nil, nil, errors.New("must not start")
		})
	if called || !f.fatalfCalled || len(slots) != 0 {
		t.Errorf("cancelled start: called=%v fatal=%v slots=%d", called, f.fatalfCalled, len(slots))
	}
}

func TestRawContainerStart_ReleasesOnStartAndPortInspectionErrors(t *testing.T) {
	for _, phase := range []string{"start", "port inspection"} {
		t.Run(phase, func(t *testing.T) {
			f, container := &fakeFataler{}, &fakeContainer{}
			slots := make(chan struct{}, 1)
			var cleanups []func()
			startRawContainerAdmitted(f, func(fn func()) { cleanups = append(cleanups, fn) }, context.Background(), slots,
				func(context.Context) (testcontainers.Container, connStringFunc, error) {
					if phase == "start" {
						return container, nil, errors.New("injected startup failure")
					}
					return container, func(context.Context) (string, error) { return "", errors.New("injected port inspection failure") }, nil
				})
			if !f.fatalfCalled || f.skipfCalled || len(slots) != 0 {
				t.Errorf("%s: fatal=%v skip=%v held slots=%d", phase, f.fatalfCalled, f.skipfCalled, len(slots))
			}
			for _, cleanup := range cleanups {
				cleanup()
			}
			if container.calls() != 1 {
				t.Errorf("%s cleanup calls = %d, want 1", phase, container.calls())
			}
		})
	}
}

func TestRawContainerStart_ReleasesOnPanicAndGoexit(t *testing.T) {
	for _, phase := range []string{"panic", "goexit"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				slots := make(chan struct{}, 1)
				done := make(chan struct{})
				go func() {
					defer close(done)
					defer func() { _ = recover() }()
					startRawContainerAdmitted(&fakeFataler{}, noopCleanup, context.Background(), slots,
						func(context.Context) (testcontainers.Container, connStringFunc, error) {
							if phase == "panic" {
								panic("injected startup panic")
							}
							runtime.Goexit() // testing.T.Fatalf uses this unwind path.
							return nil, nil, nil
						})
				}()
				synctest.Wait()
				<-done
				if len(slots) != 0 {
					t.Errorf("%s leaked %d startup slots", phase, len(slots))
				}
			})
		})
	}
}
