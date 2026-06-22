package tokenmigrate

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

// --- unit tests (no DB) ---

func TestIsSubset_AllPresent(t *testing.T) {
	defaults := []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages"}
	if !isSubset(defaults, defaults) {
		t.Error("full default set should be a subset of itself")
	}
}

func TestIsSubset_StrictSubset(t *testing.T) {
	defaults := []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages"}
	partial := []string{"read:runs", "read:audit"}
	if !isSubset(partial, defaults) {
		t.Error("partial set should be a subset of defaults")
	}
}

func TestIsSubset_EmptyTokenScopes(t *testing.T) {
	defaults := []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages"}
	if !isSubset([]string{}, defaults) {
		t.Error("empty set should be a subset of any set")
	}
}

func TestIsSubset_NonDefaultScope(t *testing.T) {
	defaults := []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages"}
	extra := []string{"read:runs", "read:audit", "custom:scope"}
	if isSubset(extra, defaults) {
		t.Error("set with custom:scope should not be a subset of defaults")
	}
}

func TestMissingScopes_AllPresent(t *testing.T) {
	defaults := []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages"}
	if got := missingScopes(defaults, defaults); len(got) != 0 {
		t.Errorf("missing from full set = %v, want empty", got)
	}
}

func TestMissingScopes_StrictSubset(t *testing.T) {
	defaults := []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages"}
	partial := []string{"read:runs", "read:audit"}
	got := missingScopes(partial, defaults)
	if len(got) != 3 {
		t.Errorf("missing count = %d, want 3; got %v", len(got), got)
	}
}

func TestMissingScopes_EmptyHave(t *testing.T) {
	defaults := []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages"}
	got := missingScopes([]string{}, defaults)
	if len(got) != len(defaults) {
		t.Errorf("missing from empty = %d, want %d", len(got), len(defaults))
	}
}
func seedToken(t *testing.T, pool *pgxpool.Pool, subject string, scopes []string) string {
	t.Helper()
	id := uuid.New().String()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO api_tokens (id, subject, token_hash, scopes)
		 VALUES ($1, $2, $3, $4)`,
		id, subject, "hash-"+id, scopes)
	if err != nil {
		t.Fatalf("seed token subject=%s: %v", subject, err)
	}
	return id
}

func queryScopes(t *testing.T, pool *pgxpool.Pool, id string) []string {
	t.Helper()
	var scopes []string
	err := pool.QueryRow(context.Background(),
		`SELECT scopes FROM api_tokens WHERE id = $1`, id).Scan(&scopes)
	if err != nil {
		t.Fatalf("query scopes id=%s: %v", id, err)
	}
	return scopes
}

var testDefaults = []string{"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages"}

func TestMigrateScopes_Integration(t *testing.T) {
	pool := pgtest.NewPool(t)

	// Token A: full default set — should be skipped.
	idA := seedToken(t, pool, "operator:full", testDefaults)
	// Token B: strict subset — should be migrated.
	idB := seedToken(t, pool, "operator:subset", []string{"read:runs", "read:audit"})
	// Token C: has a non-default scope — should be skipped.
	idC := seedToken(t, pool, "operator:custom", []string{"read:runs", "custom:scope"})

	var buf bytes.Buffer

	// Dry-run: correct summary, nothing written.
	summary, err := MigrateScopes(context.Background(), pool, testDefaults, true, &buf)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if summary.Scanned != 3 || summary.Migrated != 1 || summary.Skipped != 2 {
		t.Errorf("dry-run summary = %+v, want Scanned=3 Migrated=1 Skipped=2", summary)
	}
	if got := queryScopes(t, pool, idB); len(got) != 2 {
		t.Errorf("dry-run mutated token B: scopes=%v, want original 2", got)
	}

	buf.Reset()

	// Apply: promotes token B.
	summary, err = MigrateScopes(context.Background(), pool, testDefaults, false, &buf)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if summary.Scanned != 3 || summary.Migrated != 1 || summary.Skipped != 2 {
		t.Errorf("apply summary = %+v, want Scanned=3 Migrated=1 Skipped=2", summary)
	}
	if got := queryScopes(t, pool, idB); !equalStringSlices(got, testDefaults) {
		t.Errorf("token B scopes after apply = %v, want %v", got, testDefaults)
	}
	// A and C unchanged.
	if got := queryScopes(t, pool, idA); !equalStringSlices(got, testDefaults) {
		t.Errorf("token A unexpectedly changed: %v", got)
	}
	if got := queryScopes(t, pool, idC); !equalStringSlices(got, []string{"read:runs", "custom:scope"}) {
		t.Errorf("token C unexpectedly changed: %v", got)
	}

	buf.Reset()

	// Second apply is a no-op.
	summary, err = MigrateScopes(context.Background(), pool, testDefaults, false, &buf)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if summary.Migrated != 0 {
		t.Errorf("second apply Migrated = %d, want 0 (idempotent)", summary.Migrated)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, s := range a {
		set[s]++
	}
	for _, s := range b {
		set[s]--
		if set[s] < 0 {
			return false
		}
	}
	return true
}
