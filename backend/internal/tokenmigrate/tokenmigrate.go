// Package tokenmigrate promotes pre-#526 operator tokens whose scope
// set is a strict subset of the current operator default to the full
// default scope set. It is a one-shot data migration exposed via
// `fishhawkd token migrate`.
package tokenmigrate

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB narrows pgxpool.Pool to the methods MigrateScopes needs so tests
// can stub it without spinning up a pool.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Summary aggregates per-run counters for the migration report.
type Summary struct {
	// Scanned is the total number of active non-MCP tokens examined.
	Scanned int
	// Migrated is the number of tokens whose scopes were (or would be)
	// promoted to the operator default.
	Migrated int
	// Skipped is the number of tokens left unchanged: either already at
	// the full default set, or carrying a scope outside the default set
	// (treated as intentional — operator must re-issue manually).
	Skipped int
}

// MigrateScopes scans active non-MCP tokens and promotes any whose
// scope set is a strict subset of operatorScopes to the full default.
// Tokens that contain a scope not in operatorScopes are left untouched;
// tokens already carrying the full default are counted as Skipped.
//
// When dryRun is true, the function reports what it would do but writes
// nothing to the database. Output lines describing each candidate are
// written to w before any DB write.
func MigrateScopes(ctx context.Context, db DB, operatorScopes []string, dryRun bool, w io.Writer) (Summary, error) {
	var summary Summary

	mode := "apply"
	if dryRun {
		mode = "dry-run"
	}
	_, _ = fmt.Fprintf(w, "mode=%s operator-default-scopes=%v\n", mode, operatorScopes)

	rows, err := db.Query(ctx,
		`SELECT id, subject, scopes FROM api_tokens
		 WHERE revoked_at IS NULL AND subject NOT LIKE 'mcp:%'
		 ORDER BY created_at`)
	if err != nil {
		return summary, fmt.Errorf("query tokens: %w", err)
	}
	defer rows.Close()

	type tokenRow struct {
		id      string
		subject string
		scopes  []string
	}
	var tokens []tokenRow
	for rows.Next() {
		var r tokenRow
		if err := rows.Scan(&r.id, &r.subject, &r.scopes); err != nil {
			return summary, fmt.Errorf("scan token row: %w", err)
		}
		tokens = append(tokens, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return summary, fmt.Errorf("iterate tokens: %w", err)
	}

	for _, tok := range tokens {
		summary.Scanned++

		if !isSubset(tok.scopes, operatorScopes) {
			_, _ = fmt.Fprintf(w, "skip  subject=%s id=%s (has non-default scope)\n",
				tok.subject, tok.id)
			summary.Skipped++
			continue
		}

		missing := missingScopes(tok.scopes, operatorScopes)
		if len(missing) == 0 {
			summary.Skipped++
			continue
		}

		_, _ = fmt.Fprintf(w, "migrate subject=%s id=%s add=%v\n",
			tok.subject, tok.id, missing)

		if !dryRun {
			if _, err := db.Exec(ctx,
				`UPDATE api_tokens SET scopes = $2 WHERE id = $1`,
				tok.id, operatorScopes); err != nil {
				return summary, fmt.Errorf("update token %s: %w", tok.id, err)
			}
		}
		summary.Migrated++
	}

	return summary, nil
}

// isSubset returns true when every element of sub is present in super.
// An empty sub is always a subset.
func isSubset(sub, super []string) bool {
	set := make(map[string]struct{}, len(super))
	for _, s := range super {
		set[s] = struct{}{}
	}
	for _, s := range sub {
		if _, ok := set[s]; !ok {
			return false
		}
	}
	return true
}

// missingScopes returns the elements of want that are absent from have.
func missingScopes(have, want []string) []string {
	set := make(map[string]struct{}, len(have))
	for _, s := range have {
		set[s] = struct{}{}
	}
	var missing []string
	for _, s := range want {
		if _, ok := set[s]; !ok {
			missing = append(missing, s)
		}
	}
	return missing
}
