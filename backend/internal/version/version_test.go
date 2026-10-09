package version

import "testing"

func TestVersionNotEmpty(t *testing.T) {
	if Version == "" {
		t.Fatal("Version must not be empty; -ldflags overrides should never produce an empty string")
	}
}

func TestGitSHANotEmpty(t *testing.T) {
	if GitSHA == "" {
		t.Fatal("GitSHA must not be empty; -ldflags overrides should never produce an empty string")
	}
}

func TestFormat(t *testing.T) {
	for _, tc := range []struct {
		v, sha, want string
	}{
		{"dev", "unknown", "dev"},
		{"dev", "", "dev"},
		{"dev", "abc1234", "dev (abc1234)"},
		{"dev", "abc1234-dirty", "dev (abc1234-dirty)"},
		{"v1.2.3", "0123456789abcdef0123456789abcdef01234567", "v1.2.3 (0123456789abcdef0123456789abcdef01234567)"},
	} {
		if got := format(tc.v, tc.sha); got != tc.want {
			t.Errorf("format(%q, %q) = %q, want %q", tc.v, tc.sha, got, tc.want)
		}
	}
}

func TestString(t *testing.T) {
	if got, want := String(), format(Version, GitSHA); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	// An unstamped test binary carries the defaults.
	if Version == "dev" && GitSHA == "unknown" && String() != "dev" {
		t.Errorf("String() on an unstamped build = %q, want %q", String(), "dev")
	}
}
