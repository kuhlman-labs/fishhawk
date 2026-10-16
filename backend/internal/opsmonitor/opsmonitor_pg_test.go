package opsmonitor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// TestOpsMonitor_PostgresDedupAcrossTicksAndRestart crosses probe -> sweep ->
// emit -> persistence: a failing probe on the real audit repository writes
// exactly ONE global-chain ops_probe_failed row across two Ticks, and a fresh
// Ticker on the same database (a restart) stays suppressed through the real
// ListAll path.
func TestOpsMonitor_PostgresDedupAcrossTicksAndRestart(t *testing.T) {
	ctx := context.Background()
	repo := audit.NewPostgresRepository(pgtest.NewPool(t))
	failing := stubProbe{name: "disk", hostLocal: true, check: fixed(Result{
		Condition: ConditionDiskFreeLow, Status: StatusFailing, Observed: "1.0 GiB free of 100.0 GiB", Threshold: ">= 20.0 GiB free",
	})}
	// The clock is the real wall clock: the row timestamp and the window
	// comparison are both taken from it (single clock domain).
	first := &Ticker{Probes: []Probe{failing}, Deployment: DeploymentLocal, Audit: repo, Notifier: &countingNotifier{}, QuietWindow: time.Hour}
	n1 := first.Notifier.(*countingNotifier)
	first.Tick(ctx)
	first.Tick(ctx)

	category := CategoryOpsProbeFailed
	rows, err := repo.ListAll(ctx, audit.ListAllParams{Category: &category})
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(rows) != 1 || n1.count() != 1 {
		t.Fatalf("rows %d, notifies %d; want exactly 1/1", len(rows), n1.count())
	}
	row := rows[0]
	if row.RunID != nil || row.ActorKind == nil || *row.ActorKind != audit.ActorSystem {
		t.Errorf("row = %+v, want a run-less system-actor entry", row)
	}
	var p map[string]any
	if err := json.Unmarshal(row.Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if p["condition"] != ConditionDiskFreeLow || p["remedy"] == "" || p["notification"] != "posted" {
		t.Errorf("payload = %v", p)
	}
	global, err := repo.ListGlobal(ctx)
	if err != nil || len(global) != 1 || global[0].ID != row.ID {
		t.Errorf("global chain = %d rows (%v), want the one ops row", len(global), err)
	}

	restarted := &Ticker{Probes: []Probe{failing}, Deployment: DeploymentLocal, Audit: repo, Notifier: &countingNotifier{}, QuietWindow: time.Hour}
	restarted.Tick(ctx)
	if got := restarted.Notifier.(*countingNotifier).count(); got != 0 {
		t.Errorf("restarted Ticker notified %d times, want 0 (dedup via the audit log)", got)
	}
	rows, err = repo.ListAll(ctx, audit.ListAllParams{Category: &category})
	if err != nil || len(rows) != 1 {
		t.Errorf("rows after restart = %d (%v), want still 1", len(rows), err)
	}
}
