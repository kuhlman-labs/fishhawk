package opsmonitor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
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

// TestOpsMonitor_PostgresAmendmentProbesEndToEnd crosses scope-amendment SQL
// -> lister -> both amendment probes -> Ticker -> real audit repository. Ages
// are backdated DB-side (now() - interval), so both sides of every age are the
// database clock (#3048). A live-stage row 6m old fires the window probe, a
// terminal-stage row fires the terminal probe, a lapsed row (#4127) on a
// terminal stage fires nothing, and a second Tick adds no row.
func TestOpsMonitor_PostgresAmendmentProbesEndToEnd(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stage := func(seq int) uuid.UUID {
		st, err := runRepo.CreateStage(ctx, run.CreateStageParams{
			RunID: r.ID, Sequence: seq, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
		})
		if err != nil {
			t.Fatalf("create stage: %v", err)
		}
		return st.ID
	}
	liveStage, terminalStage, lapsedStage := stage(1), stage(2), stage(3)
	amendRepo := scopeamendment.NewPostgresRepository(pool)
	amend := func(stageID uuid.UUID) uuid.UUID {
		a, err := amendRepo.Create(ctx, scopeamendment.CreateParams{
			RunID: r.ID, StageID: stageID, Reason: "needed",
			Paths: []scopeamendment.PathEntry{{Path: "backend/x.go", Operation: scopeamendment.OperationModify}},
		})
		if err != nil {
			t.Fatalf("create amendment: %v", err)
		}
		return a.ID
	}
	liveID, terminalID, lapsedID := amend(liveStage), amend(terminalStage), amend(lapsedStage)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE scope_amendments SET requested_at = now() - interval '6 minutes' WHERE id = ANY($1)`, []any{[]uuid.UUID{liveID, terminalID, lapsedID}}},
		{`UPDATE stages SET state = 'succeeded' WHERE id = ANY($1)`, []any{[]uuid.UUID{terminalStage, lapsedStage}}},
		{`UPDATE scope_amendments SET status = 'lapsed' WHERE id = $1`, []any{lapsedID}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.sql, err)
		}
	}

	lister := scopeamendment.NewPendingLister(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	notifier := &countingNotifier{}
	tk := &Ticker{
		Probes: []Probe{
			AmendmentWindowProbe{Amendments: lister, Window: 15 * time.Minute, WarnLead: 10 * time.Minute, Lookback: 7 * 24 * time.Hour},
			AmendmentTerminalProbe{Amendments: lister, Lookback: 7 * 24 * time.Hour},
		},
		Deployment: DeploymentLocal, Audit: auditRepo, Notifier: notifier, QuietWindow: time.Hour,
	}
	tk.Tick(ctx)
	tk.Tick(ctx)

	category := CategoryOpsProbeFailed
	rows, err := auditRepo.ListAll(ctx, audit.ListAllParams{Category: &category})
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(rows) != 2 || notifier.count() != 2 {
		t.Fatalf("rows %d, notifies %d; want exactly 2/2 (one window, one terminal)", len(rows), notifier.count())
	}
	bySubject := map[string]map[string]any{}
	for _, row := range rows {
		var p map[string]any
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			t.Fatalf("payload: %v", err)
		}
		bySubject[p["subject"].(string)] = p
	}
	if _, ok := bySubject[lapsedID.String()]; ok {
		t.Errorf("the lapsed amendment %s was reported", lapsedID)
	}
	w, ok := bySubject[liveID.String()]
	if !ok || w["condition"] != ConditionAmendmentWindowClosing {
		t.Fatalf("no window row for the live amendment %s: %v", liveID, bySubject)
	}
	fields, _ := w["fields"].(map[string]any)
	if fields["run_id"] != r.ID.String() {
		t.Errorf("window fields.run_id = %v, want %s", fields["run_id"], r.ID)
	}
	if rem, _ := fields["time_remaining_seconds"].(float64); rem < 480 || rem > 540 {
		t.Errorf("window fields.time_remaining_seconds = %v, want in [480,540] (DB-clock age ~6m)", fields["time_remaining_seconds"])
	}
	term, ok := bySubject[terminalID.String()]
	if !ok || term["condition"] != ConditionAmendmentPendingOnTerminalStage {
		t.Errorf("no terminal row for amendment %s: %v", terminalID, bySubject)
	} else if tf, _ := term["fields"].(map[string]any); tf["stage_id"] != terminalStage.String() {
		t.Errorf("terminal fields.stage_id = %v, want %s", tf["stage_id"], terminalStage)
	}
}

// TestOpsMonitor_PostgresAlertRestartDedup crosses hosthealth parsers ->
// MemoryProbe -> latch -> Ticker -> the real audit repository: the done-means
// macOS host writes ONE global-chain ops_monitor_alert row, and a FRESH
// Ticker with a FRESH probe (empty memory and an unlatched latch, so it
// re-crosses) inside the quiet window writes none — only the audit lookup
// can suppress it (counterfactual 6).
func TestOpsMonitor_PostgresAlertRestartDedup(t *testing.T) {
	ctx := context.Background()
	repo := audit.NewPostgresRepository(pgtest.NewPool(t))
	host := darwinDoneMeans()
	newTk := func() (*Ticker, *MemoryProbe) {
		p := &MemoryProbe{Reader: host.reader("darwin")}
		return &Ticker{Probes: []Probe{p}, Deployment: DeploymentLocal, Audit: repo, QuietWindow: time.Hour}, p
	}
	first, _ := newTk()
	first.Tick(ctx)
	first.Tick(ctx)

	category := CategoryOpsMonitorAlert
	rows, err := repo.ListAll(ctx, audit.ListAllParams{Category: &category})
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ops_monitor_alert rows = %d, want 1", len(rows))
	}
	if rows[0].RunID != nil || rows[0].AccountID != nil || rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorSystem {
		t.Errorf("row = %+v, want a run-less, untenanted, system-actor entry", rows[0])
	}
	var p alertBody
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if len(p.Contributors) == 0 || p.Contributors[0].Name != "fseventsd" || !p.Critical || p.Crossings[0].Condition != ConditionHostMemoryPressure {
		t.Errorf("payload = %+v", p)
	}

	restarted, _ := newTk()
	rs := restarted.Tick(ctx)
	if r := resultFor(t, rs, "memory"); !r.Crossing {
		t.Fatalf("restarted probe result = %+v, want a re-crossing (fresh latch)", r)
	}
	rows, err = repo.ListAll(ctx, audit.ListAllParams{Category: &category})
	if err != nil || len(rows) != 1 {
		t.Errorf("ops_monitor_alert rows after restart = %d (%v), want still 1 (dedup via the audit log)", len(rows), err)
	}
}
