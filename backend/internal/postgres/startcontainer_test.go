package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// These tests pin the #3122 raw-container leak fix WITHOUT Docker. They drive
// startContainerWith — the injectable seam CONTAINING the container-start error
// branch — so the assertions exercise the branch that decides whether the leaked
// container is terminated at all (operator condition 1: test the call site, not
// just the leaf helper). Deleting the terminateStartFailure CALL from that branch
// reddens TestStartContainerWith_TerminatesOnStartError.

// fakeContainer implements testcontainers.Container by embedding the interface
// (nil at runtime — the embedded methods are never called) and overriding only
// Terminate, which records its call count and the context it received. The
// Terminate signature MUST match Container.Terminate(ctx, ...TerminateOption)
// exactly, or it would not override the promoted interface method.
type fakeContainer struct {
	testcontainers.Container
	mu             sync.Mutex
	terminateCalls int
	lastCtx        context.Context
	lastErrAtCall  error // ctx.Err() captured AT the call, before terminateStartFailure's deferred cancel
	termErr        error
}

func (f *fakeContainer) Terminate(ctx context.Context, _ ...testcontainers.TerminateOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminateCalls++
	f.lastCtx = ctx
	f.lastErrAtCall = ctx.Err()
	return f.termErr
}

func (f *fakeContainer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.terminateCalls
}

// fakeFataler stands in for *testing.T. Unlike the real thing its Skipf/Fatalf
// RECORD and RETURN rather than ending the goroutine, so startContainerWith runs
// past them to its explicit post-Fatalf return — which is exactly how the seam
// lets a test observe the error branch.
type fakeFataler struct {
	skipfCalled  bool
	fatalfCalled bool
	lastFormat   string
	lastArgs     []any
}

func (f *fakeFataler) Helper() {}
func (f *fakeFataler) Skipf(format string, args ...any) {
	f.skipfCalled = true
	f.lastFormat, f.lastArgs = format, args
}
func (f *fakeFataler) Fatalf(format string, args ...any) {
	f.fatalfCalled = true
	f.lastFormat, f.lastArgs = format, args
}
func (f *fakeFataler) message() string {
	return fmt.Sprintf(f.lastFormat, f.lastArgs...)
}

// noopCleanup is a cleanup sink for the error-path tests, where the success-path
// cleanup registration is never reached.
func noopCleanup(func()) {}

// (a) Terminate IS called on the start-error path. This is the load-bearing
// counterfactual vehicle for condition 1: deleting terminateStartFailure(...)
// from startContainerWith's error branch leaves terminateCalls == 0 here.
func TestStartContainerWith_TerminatesOnStartError(t *testing.T) {
	t.Setenv("FISHHAWK_SKIP_INTEGRATION", "") // force the Fatalf branch, not Skipf
	fc := &fakeContainer{}
	f := &fakeFataler{}
	startErr := errors.New("start container: started hook: context deadline exceeded")

	got := startContainerWith(f, func(func()) {
		t.Fatal("success cleanup must not run on the start-error path")
	}, context.Background(), func(context.Context) (testcontainers.Container, connStringFunc, error) {
		return fc, nil, startErr
	})

	if got != "" {
		t.Errorf("startContainerWith on error = %q, want empty", got)
	}
	if fc.calls() != 1 {
		t.Errorf("Terminate called %d times on the start-error path, want 1", fc.calls())
	}
	if !f.fatalfCalled {
		t.Errorf("expected Fatalf on a non-docker-unavailable start error (skipf=%v)", f.skipfCalled)
	}
}

// (b) A TYPED-NIL handle (a nil *fakeContainer boxed in a non-nil interface) must
// not panic and must not reach Terminate. Deleting the reflect guard makes this
// PANIC (a bare c != nil is true for a typed nil).
func TestStartContainerWith_TypedNilHandleNoPanic(t *testing.T) {
	var typedNil *fakeContainer               // nil pointer
	var c testcontainers.Container = typedNil // non-nil interface, nil pointee
	f := &fakeFataler{}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("startContainerWith panicked on a typed-nil handle: %v", r)
		}
	}()

	startContainerWith(f, noopCleanup, context.Background(),
		func(context.Context) (testcontainers.Container, connStringFunc, error) {
			return c, nil, errors.New("boom with a typed-nil handle")
		})
	if !f.fatalfCalled && !f.skipfCalled {
		t.Error("expected a terminal Fatalf/Skipf after the typed-nil guard")
	}
}

// (c) An UNTYPED-NIL handle must not panic (the plain c == nil guard).
func TestStartContainerWith_UntypedNilHandleNoPanic(t *testing.T) {
	f := &fakeFataler{}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("startContainerWith panicked on an untyped-nil handle: %v", r)
		}
	}()
	startContainerWith(f, noopCleanup, context.Background(),
		func(context.Context) (testcontainers.Container, connStringFunc, error) {
			return nil, nil, errors.New("boom with a nil handle")
		})
	if !f.fatalfCalled && !f.skipfCalled {
		t.Error("expected a terminal Fatalf/Skipf after the untyped-nil guard")
	}
}

// (d) The context handed to Terminate carries its OWN fresh deadline, NOT the
// caller's remainder. startCtx is passed already EXPIRED (mimicking a start that
// fails late with ~0 budget left); the fix ignores it and derives a fresh 30s
// deadline from context.Background(), so the recorded ctx has ~30s remaining and
// is not yet Done. The counterfactual — deriving the cleanup ctx from startCtx —
// yields a deadline in the past and reddens the >=29s assertion (condition 3).
func TestStartContainerWith_FreshTerminateContext(t *testing.T) {
	fc := &fakeContainer{}
	f := &fakeFataler{}
	// Expired outer context: deadline one second in the PAST.
	startCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	startContainerWith(f, noopCleanup, startCtx,
		func(context.Context) (testcontainers.Container, connStringFunc, error) {
			return fc, nil, errors.New("late start failure")
		})

	if fc.lastCtx == nil {
		t.Fatal("Terminate was not called, so no cleanup context to inspect")
	}
	deadline, ok := fc.lastCtx.Deadline()
	if !ok {
		t.Fatal("cleanup context has no deadline; want a fresh bounded 30s deadline")
	}
	if remaining := time.Until(deadline); remaining < 29*time.Second {
		t.Errorf("cleanup context deadline is %v out, want >= ~29s (fresh 30s budget, not the caller's remainder)", remaining)
	}
	// Captured AT the Terminate call (before terminateStartFailure's deferred
	// cancel): a fresh context is live even though startCtx is already expired.
	// Reusing startCtx would make this DeadlineExceeded.
	if fc.lastErrAtCall != nil {
		t.Errorf("cleanup context was already errored when handed to Terminate (%v); a fresh context must be live even when startCtx is expired", fc.lastErrAtCall)
	}
}

// (e) A Terminate error is SWALLOWED: the ORIGINAL start error reaches the
// caller's Fatalf verbatim and the swallowed cleanup error never appears.
func TestStartContainerWith_SwallowsTerminateError(t *testing.T) {
	t.Setenv("FISHHAWK_SKIP_INTEGRATION", "") // force the Fatalf branch
	distinct := errors.New("terminate-boom-DISTINCT-SENTINEL")
	fc := &fakeContainer{termErr: distinct}
	f := &fakeFataler{}
	startErr := errors.New("start postgres: ORIGINAL-START-SIGNATURE")

	startContainerWith(f, noopCleanup, context.Background(),
		func(context.Context) (testcontainers.Container, connStringFunc, error) {
			return fc, nil, startErr
		})

	if !f.fatalfCalled {
		t.Fatalf("expected Fatalf on the start-error path (skipf=%v)", f.skipfCalled)
	}
	msg := f.message()
	if !strings.Contains(msg, "ORIGINAL-START-SIGNATURE") {
		t.Errorf("terminal message = %q, want it to carry the original start error verbatim", msg)
	}
	if strings.Contains(msg, "terminate-boom-DISTINCT-SENTINEL") {
		t.Errorf("terminal message leaked the swallowed Terminate error: %q", msg)
	}
	if fc.calls() != 1 {
		t.Errorf("Terminate called %d times, want 1", fc.calls())
	}
}
