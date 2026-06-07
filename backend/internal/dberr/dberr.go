// Package dberr classifies database errors so the layers above the
// repository can tell a *database-unavailable* condition (the server
// is down, the connection was refused, the pool is closed) apart from
// an ordinary query outcome (no matching row, a constraint violation).
//
// The distinction is load-bearing at the auth middleware: a token
// lookup that fails because the database is unreachable must surface
// as 503 service_unavailable, not 401 — a 401 falsely tells the caller
// their credential is bad and invites them to throw it away. An
// actually-bad token (pgx.ErrNoRows → the repo's ErrNotFound) is NOT
// unavailable and keeps its 401 (#764).
package dberr

import (
	"errors"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

// IsUnavailable reports whether err represents a database that could
// not be reached, as opposed to a query that ran and returned an
// ordinary error (e.g. no rows). It unwraps with errors.As /
// errors.Is, so a repo that wraps the underlying pgx error with %w is
// classified correctly without any repo-layer change.
//
// Returns false for nil and for any ordinary query error (including a
// pgx.ErrNoRows-derived not-found). The classifier is deliberately
// narrow: only the establish-connection and closed-pool signals count
// as unavailable, so it never misclassifies a genuine bad-credential
// lookup as an outage.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}

	// pgx surfaces a connection-establishment failure (server down,
	// `dial tcp :5432: connection refused`) as *pgconn.ConnectError.
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}

	// Acquiring from a pool that has been shut down returns
	// puddle/v2 ErrClosedPool — the pool the repo holds is gone, so
	// no query can run.
	if errors.Is(err, puddle.ErrClosedPool) {
		return true
	}

	// Fallback: a raw network-layer failure that didn't get wrapped
	// into a *pgconn.ConnectError (e.g. a mid-flight connection drop)
	// still means the database is unreachable.
	var netErr *net.OpError
	return errors.As(err, &netErr)
}
