package diagnostics

import "testing"

// TestClassifyFailureDetail is the done-means table test: it pins the
// detail-class extraction for the known real-world git-stderr shapes,
// one behavioral assertion per class and per fail-open / precedence mode.
func TestClassifyFailureDetail(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   string
	}{
		// auth-401 — the #1933-shape credential failures.
		{
			name:   "could-not-read-username / terminal-prompts",
			reason: "fatal: could not read Username for 'https://github.com': terminal prompts disabled",
			want:   "auth-401",
		},
		{
			name:   "requested-url-401",
			reason: "fatal: unable to access 'https://github.com/kuhlman-labs/fishhawk/': The requested URL returned error: 401",
			want:   "auth-401",
		},
		{
			name:   "authentication-failed",
			reason: "remote: Authentication failed for 'https://github.com/kuhlman-labs/fishhawk/'",
			want:   "auth-401",
		},
		// bad-object-ref — the #1932-shape ref failures.
		{
			name:   "couldnt-find-remote-ref",
			reason: "fatal: couldn't find remote ref refs/heads/fishhawk/run-abc",
			want:   "bad-object-ref",
		},
		{
			name:   "bad-object",
			reason: "fatal: bad object 0123456789abcdef0123456789abcdef01234567",
			want:   "bad-object-ref",
		},
		{
			name:   "unknown-revision",
			reason: "fatal: ambiguous argument 'HEAD~1': unknown revision or path not in the working tree",
			want:   "bad-object-ref",
		},
		// target-unreachable — network shapes.
		{
			name:   "could-not-resolve-host",
			reason: "fatal: unable to access 'https://github.com/kuhlman-labs/fishhawk/': Could not resolve host: github.com",
			want:   "target-unreachable",
		},
		{
			name:   "connection-refused",
			reason: "ssh: connect to host github.com port 22: Connection refused",
			want:   "target-unreachable",
		},
		{
			name:   "connection-timed-out",
			reason: "fatal: unable to access 'https://github.com/': Failed to connect to github.com port 443: Connection timed out",
			want:   "target-unreachable",
		},
		// Precedence: a line carrying BOTH the shared "unable to access"
		// prefix and a "401" suffix must classify auth-401, NOT
		// target-unreachable. This fails if "unable to access" is ever
		// added as a marker or the auth/unreachable order flips.
		{
			name:   "access-failure-with-401-is-auth",
			reason: "fatal: unable to access 'https://github.com/kuhlman-labs/fishhawk/': The requested URL returned error: 401",
			want:   "auth-401",
		},
		// Fail-open: empty and arbitrary unrecognized text classify "".
		{
			name:   "empty",
			reason: "",
			want:   "",
		},
		{
			name:   "unrecognized",
			reason: "the agent edited a forbidden path and the policy gate rejected the commit",
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyFailureDetail(tc.reason); got != tc.want {
				t.Errorf("ClassifyFailureDetail(%q) = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}
