package issuecomment

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// recordingChannel is a fake Channel that records every call and returns
// caller-configured errors / posted bits, so the Router fan-out can be
// asserted without any GitHub I/O.
type recordingChannel struct {
	statusUpdate int
	pageClass    int
	planReady    int
	ciRetry      int
	budgetAlert  int
	slashReply   int
	runRejected  int
	artifactWire bool

	// Configurable returns.
	err          error // returned from every error-returning method
	budgetPosted bool  // returned from NotifyBudgetAlert
}

func (c *recordingChannel) NotifyStatusUpdateForRun(_ context.Context, _ uuid.UUID) error {
	c.statusUpdate++
	return c.err
}

func (c *recordingChannel) NotifyPageClassForRun(_ context.Context, _ uuid.UUID) error {
	c.pageClass++
	return c.err
}

func (c *recordingChannel) NotifyPlanReady(_ context.Context, _ uuid.UUID, _ *run.Stage, _ *plan.Plan) error {
	c.planReady++
	return c.err
}

func (c *recordingChannel) NotifyCIRetry(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ string, _, _ int) error {
	c.ciRetry++
	return c.err
}

func (c *recordingChannel) NotifyBudgetAlert(_ context.Context, _ uuid.UUID, _ BudgetAlertPayload) (bool, error) {
	c.budgetAlert++
	return c.budgetPosted, c.err
}

func (c *recordingChannel) NotifySlashApprovalReply(_ context.Context, _ SlashApprovalReply) error {
	c.slashReply++
	return c.err
}

func (c *recordingChannel) NotifyRunRejected(_ context.Context, _ string, _ int64, _ int, _, _ string) error {
	c.runRejected++
	return c.err
}

func (c *recordingChannel) ArtifactListerWired() bool { return c.artifactWire }

var _ Channel = (*recordingChannel)(nil)

// TestRouter_FansOutEverySurface asserts each Notify* surface reaches every
// registered channel exactly once.
func TestRouter_FansOutEverySurface(t *testing.T) {
	a, b := &recordingChannel{}, &recordingChannel{}
	r := NewRouter(a, b)
	ctx := context.Background()

	if err := r.NotifyStatusUpdateForRun(ctx, uuid.New()); err != nil {
		t.Fatalf("NotifyStatusUpdateForRun: %v", err)
	}
	if err := r.NotifyPageClassForRun(ctx, uuid.New()); err != nil {
		t.Fatalf("NotifyPageClassForRun: %v", err)
	}
	if err := r.NotifyPlanReady(ctx, uuid.New(), nil, nil); err != nil {
		t.Fatalf("NotifyPlanReady: %v", err)
	}
	if err := r.NotifyCIRetry(ctx, uuid.New(), uuid.New(), "build", 1, 3); err != nil {
		t.Fatalf("NotifyCIRetry: %v", err)
	}
	if _, err := r.NotifyBudgetAlert(ctx, uuid.New(), BudgetAlertPayload{Tier: "warn"}); err != nil {
		t.Fatalf("NotifyBudgetAlert: %v", err)
	}
	if err := r.NotifySlashApprovalReply(ctx, SlashApprovalReply{}); err != nil {
		t.Fatalf("NotifySlashApprovalReply: %v", err)
	}
	if err := r.NotifyRunRejected(ctx, "x/y", 1, 2, "wf", "stage"); err != nil {
		t.Fatalf("NotifyRunRejected: %v", err)
	}

	for name, c := range map[string]*recordingChannel{"a": a, "b": b} {
		if c.statusUpdate != 1 || c.pageClass != 1 || c.planReady != 1 || c.ciRetry != 1 ||
			c.budgetAlert != 1 || c.slashReply != 1 || c.runRejected != 1 {
			t.Errorf("channel %s did not receive every surface once: %+v", name, c)
		}
	}
}

// TestRouter_JoinsErrors asserts errors.Join aggregation across channels:
// the joined error wraps every channel's error.
func TestRouter_JoinsErrors(t *testing.T) {
	errA := errors.New("channel a failed")
	errB := errors.New("channel b failed")
	a := &recordingChannel{err: errA}
	b := &recordingChannel{err: errB}
	r := NewRouter(a, b)

	err := r.NotifyStatusUpdateForRun(context.Background(), uuid.New())
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("joined error must wrap both channel errors; got %v", err)
	}
}

// TestRouter_BudgetPostedIsOR asserts NotifyBudgetAlert's posted bit is the
// OR across channels (true when ANY channel posted) and that a per-channel
// error is still joined alongside a true posted.
func TestRouter_BudgetPostedIsOR(t *testing.T) {
	ctx := context.Background()

	// None posted → false.
	r := NewRouter(&recordingChannel{}, &recordingChannel{})
	if posted, err := r.NotifyBudgetAlert(ctx, uuid.New(), BudgetAlertPayload{Tier: "warn"}); posted || err != nil {
		t.Fatalf("no channel posted: want (false,nil) got (%v,%v)", posted, err)
	}

	// One posted → true.
	bErr := errors.New("b failed")
	r = NewRouter(&recordingChannel{budgetPosted: true}, &recordingChannel{err: bErr})
	posted, err := r.NotifyBudgetAlert(ctx, uuid.New(), BudgetAlertPayload{Tier: "warn"})
	if !posted {
		t.Errorf("posted must be OR across channels; want true got false")
	}
	if !errors.Is(err, bErr) {
		t.Errorf("joined error must carry the failing channel's error; got %v", err)
	}
}

// TestRouter_ArtifactListerWiredIsOR asserts ArtifactListerWired ORs across
// channels.
func TestRouter_ArtifactListerWiredIsOR(t *testing.T) {
	if NewRouter(&recordingChannel{}, &recordingChannel{}).ArtifactListerWired() {
		t.Error("no channel wired → want false")
	}
	if !NewRouter(&recordingChannel{}, &recordingChannel{artifactWire: true}).ArtifactListerWired() {
		t.Error("one channel wired → want true (OR)")
	}
}

// TestRouter_NilSafety asserts a nil *Router and nil channel entries are
// safe no-ops, matching the nil-safe Notifier posture so call sites need
// no nil checks.
func TestRouter_NilSafety(t *testing.T) {
	ctx := context.Background()

	var nilRouter *Router
	if err := nilRouter.NotifyStatusUpdateForRun(ctx, uuid.New()); err != nil {
		t.Errorf("nil router NotifyStatusUpdateForRun: %v", err)
	}
	if posted, err := nilRouter.NotifyBudgetAlert(ctx, uuid.New(), BudgetAlertPayload{}); posted || err != nil {
		t.Errorf("nil router NotifyBudgetAlert: (%v,%v)", posted, err)
	}
	if nilRouter.ArtifactListerWired() {
		t.Error("nil router ArtifactListerWired: want false")
	}

	// A Router with a nil channel entry skips it and still reaches the
	// live channel.
	live := &recordingChannel{}
	r := NewRouter(nil, live)
	if err := r.NotifyStatusUpdateForRun(ctx, uuid.New()); err != nil {
		t.Fatalf("router with nil entry: %v", err)
	}
	if live.statusUpdate != 1 {
		t.Errorf("live channel must still receive the call past a nil entry; got %d", live.statusUpdate)
	}
}
