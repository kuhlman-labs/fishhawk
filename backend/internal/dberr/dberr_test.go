package dberr

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

func TestIsUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("some query failed"), false},
		{
			// The bad-token case: pgx.ErrNoRows wrapped by a repo's
			// ErrNotFound must NOT be unavailable — it keeps its 401.
			name: "not-found sentinel",
			err:  errSentinelNotFound,
			want: false,
		},
		{
			name: "pgconn.ConnectError",
			err:  &pgconn.ConnectError{},
			want: true,
		},
		{
			// Repos wrap the underlying pgx error with %w; errors.As
			// must still unwrap to the connect error.
			name: "wrapped pgconn.ConnectError",
			err:  fmt.Errorf("apitoken: lookup: %w", &pgconn.ConnectError{}),
			want: true,
		},
		{
			name: "puddle ErrClosedPool",
			err:  puddle.ErrClosedPool,
			want: true,
		},
		{
			name: "wrapped ErrClosedPool",
			err:  fmt.Errorf("acquire: %w", puddle.ErrClosedPool),
			want: true,
		},
		{
			name: "net.OpError fallback",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsUnavailable(tc.err); got != tc.want {
				t.Fatalf("IsUnavailable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// errSentinelNotFound stands in for a repo's ErrNotFound (an ordinary
// "no row" outcome). Declared here rather than importing apitoken to
// keep the dberr package free of repo dependencies.
var errSentinelNotFound = errors.New("not found")
