package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationconfirm"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// delegationConfirmPG drives the REAL handlers against a pgtest database: the
// production delegationconfirm.Store (with its captain-record lock), the
// Postgres run repository serving source=run_cache, and the real global
// chain.
type delegationConfirmPG struct {
	captainPG
}

func newDelegationConfirmPG(t *testing.T) *delegationConfirmPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	srv := New(Config{
		Addr:                   "127.0.0.1:0",
		RunRepo:                run.NewPostgresRepository(pool),
		CaptainStore:           captain.NewStore(pool),
		DelegationConfirmStore: delegationconfirm.NewStore(pool),
	})
	return &delegationConfirmPG{captainPG{pool: pool, audit: audit.NewPostgresRepository(pool), srv: srv}}
}

// seedSpecAt seeds a run of acme/app carrying specYAML, created at `at` so
// the NEWEST run (the one source=run_cache reads) is deterministic.
func (f *delegationConfirmPG) seedSpecAt(t *testing.T, specYAML string, at time.Time) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, workflow_spec, created_at, updated_at)
		VALUES ($1, 'acme/app', 'ship', 'sha', 'cli', 'succeeded', 'local', $2, $3, $3)`, uuid.New(), []byte(specYAML), at); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

func (f *delegationConfirmPG) countEntries(t *testing.T, category string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_entries
		WHERE run_id IS NULL AND category = $1 AND payload->>'repo' = 'acme/app'`, category).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestDelegationConfirmPG_EndToEnd is the cross-boundary test: request
// payload -> domain fold -> audit persistence -> both read surfaces, with
// the three arms (confirmed, hash_stale after a tightened spec, handover
// after a later captain_assigned). Staleness is asserted on the two
// delegation-confirmation reads only (approval condition 2).
func TestDelegationConfirmPG_EndToEnd(t *testing.T) {
	f := newDelegationConfirmPG(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	f.seedSpecAt(t, confirmSpecA, base)
	f.seedCaptain(t, "acme/app", confirmCaptain)

	first := readConfirmation(t, f.srv)
	if len(first.UnconfirmedWorkflows) != 2 {
		t.Fatalf("first handover unconfirmed = %v, want both workflows", first.UnconfirmedWorkflows)
	}

	hash := hashOfWorkflow(t, confirmSpecA, "ship")
	if w := serveDelegationConfirm(t, f.srv, "confirm", confirmBody("ship", hash), confirmCaptain); w.Code != http.StatusOK {
		t.Fatalf("confirm = %d: %s", w.Code, w.Body.String())
	}
	if n := f.countEntries(t, delegationconfirm.CategoryDelegationConfirmed); n != 1 {
		t.Fatalf("delegation_confirmed entries = %d, want exactly 1", n)
	}
	if st := statusFor(t, readConfirmation(t, f.srv).Workflows, "ship"); st.Status != delegationconfirm.StatusConfirmed || st.Confirmation.ContentHash != hash {
		t.Fatalf("confirmation read = %+v", st)
	}
	if c := delegationRespWorkflow(t, readDelegationBody(t, f.srv), "ship").Confirmation; c == nil || c.Status != delegationconfirm.StatusConfirmed {
		t.Fatalf("delegation read = %+v", c)
	}

	// Arm 2: the tightened spec becomes the newest run's.
	f.seedSpecAt(t, confirmSpecTightened, base.Add(time.Hour))
	if st := statusFor(t, readConfirmation(t, f.srv).Workflows, "ship"); st.Reason != delegationconfirm.ReasonHashStale {
		t.Fatalf("after tightening, confirmation read = %+v, want hash_stale", st)
	}
	if c := delegationRespWorkflow(t, readDelegationBody(t, f.srv), "ship").Confirmation; c.Reason != delegationconfirm.ReasonHashStale {
		t.Fatalf("after tightening, delegation read = %+v, want hash_stale", c)
	}

	// Arm 3: a handover voids every confirmation.
	f.seedCaptain(t, "acme/app", "github:successor")
	got := readConfirmation(t, f.srv)
	if st := statusFor(t, got.Workflows, "ship"); st.Reason != delegationconfirm.ReasonHandover {
		t.Fatalf("after handover = %+v, want handover", st)
	}
	if c := delegationRespWorkflow(t, readDelegationBody(t, f.srv), "ship").Confirmation; c.Reason != delegationconfirm.ReasonHandover {
		t.Fatalf("after handover delegation read = %+v, want handover", c)
	}

	// The outgoing captain can no longer confirm: the captain check runs on
	// state read under the captain record's lock.
	w := serveDelegationConfirm(t, f.srv, "confirm", confirmBody("ship", hashOfWorkflow(t, confirmSpecTightened, "ship")), confirmCaptain)
	wantCode(t, w, http.StatusForbidden, "delegation_not_captain")
	if n := f.countEntries(t, delegationconfirm.CategoryDelegationConfirmed); n != 1 {
		t.Fatalf("delegation_confirmed entries = %d after the refused confirm, want still 1", n)
	}
}

// TestDelegationConfirmPG_OutgoingCaptainConfirmAfterHandoverIgnored
// (approval condition 3): an outgoing captain's confirmation that reaches
// the chain AFTER a newer captain_assigned — seeded by construction,
// bypassing the handler's lock — leaves the workflow unconfirmed.
func TestDelegationConfirmPG_OutgoingCaptainConfirmAfterHandoverIgnored(t *testing.T) {
	f := newDelegationConfirmPG(t)
	f.seedSpecAt(t, confirmSpecA, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	f.seedCaptain(t, "acme/app", confirmCaptain)
	f.seedCaptain(t, "acme/app", "github:successor")
	f.seedEntry(t, delegationconfirm.CategoryDelegationConfirmed, map[string]any{
		"repo": "acme/app", "workflow": "ship", "subject": confirmCaptain, "content_hash": hashOfWorkflow(t, confirmSpecA, "ship")})
	got := readConfirmation(t, f.srv)
	if st := statusFor(t, got.Workflows, "ship"); st.Status != delegationconfirm.StatusUnconfirmed {
		t.Fatalf("ship = %+v, want unconfirmed", st)
	}
	if got.IgnoredEntries != 1 {
		t.Errorf("ignored_entries = %d, want 1", got.IgnoredEntries)
	}
}
