package postgres

import "testing"

// TestNormalizeDatabaseURL covers every branch of the URL-scheme
// rewrite. Pure unit test — runs in milliseconds, doesn't need
// Docker.
func TestNormalizeDatabaseURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "postgres scheme rewritten",
			in:   "postgres://u:p@host:5432/db",
			want: "pgx5://u:p@host:5432/db",
		},
		{
			name: "postgresql scheme rewritten",
			in:   "postgresql://u:p@host:5432/db?sslmode=disable",
			want: "pgx5://u:p@host:5432/db?sslmode=disable",
		},
		{
			name: "pgx5 passes through unchanged",
			in:   "pgx5://u:p@host:5432/db",
			want: "pgx5://u:p@host:5432/db",
		},
		{
			name: "unknown scheme passes through unchanged",
			in:   "mysql://u:p@host/db",
			want: "mysql://u:p@host/db",
		},
		{
			name: "empty string passes through",
			in:   "",
			want: "",
		},
		{
			name: "scheme-only postgres rewritten to scheme-only pgx5",
			in:   "postgres://",
			want: "pgx5://",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeDatabaseURL(tc.in); got != tc.want {
				t.Errorf("normalizeDatabaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
