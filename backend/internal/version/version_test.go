package version

import (
	"regexp"
	"strings"
	"testing"
)

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

func TestResolveMinRunnerVersion(t *testing.T) {
	const floor = "v0.1.0\n"
	for _, tc := range []struct {
		name, version, stamped, floor, want string
	}{
		{"dev build", "dev", "dev", floor, "dev"},
		{"ci image", "ci-0123abc", "dev", floor, "dev"},
		{"main image", "sha-0123abc", "dev", floor, "dev"},
		{"unknown version", "unknown", "dev", floor, "dev"},
		{"no leading v", "1.2.3", "dev", floor, "dev"},
		{"two parts", "v1.2", "dev", floor, "dev"},
		{"four parts", "v1.2.3.4", "dev", floor, "dev"},
		{"first release", "v0.1.0", "dev", floor, "v0.1.0"},
		{"release", "v1.2.3", "dev", floor, "v0.1.0"},
		{"pre-release", "v1.2.3-rc.1", "dev", floor, "v0.1.0"},
		{"explicit stamp wins on a release", "v1.2.3", "v0.9.0", floor, "v0.9.0"},
		{"explicit stamp wins on a dev build", "dev", "v0.9.0", floor, "v0.9.0"},
		{"empty floor on a release", "v1.2.3", "dev", "", "dev"},
		{"whitespace floor on a release", "v1.2.3", "dev", " \n\t", "dev"},
		{"empty stamp is not an override", "v1.2.3", "", floor, "v0.1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMinRunnerVersion(tc.version, tc.stamped, tc.floor); got != tc.want {
				t.Errorf("resolveMinRunnerVersion(%q, %q, %q) = %q, want %q", tc.version, tc.stamped, tc.floor, got, tc.want)
			}
		})
	}
}

// The runner's parseSemver needs exactly three numeric parts; a malformed
// committed floor would otherwise degrade silently to no enforcement there.
func TestCommittedMinRunnerVersionWellFormed(t *testing.T) {
	floor := strings.TrimSpace(minRunnerFloorFile)
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(floor) {
		t.Fatalf("min-runner-version = %q, want vMAJOR.MINOR.PATCH", floor)
	}
}

// Pins the init-resolved package var: dev on the normal unstamped verify, the
// committed floor under scripts/test-release-ldflags r9's release stamp.
func TestMinRunnerVersion_MatchesBuildStamp(t *testing.T) {
	want := "dev"
	if isReleaseVersion(Version) {
		want = strings.TrimSpace(minRunnerFloorFile)
	}
	t.Logf("resolved min_runner_version=%s (Version=%s)", MinRunnerVersion, Version)
	if MinRunnerVersion != want {
		t.Errorf("MinRunnerVersion = %q for Version %q, want %q", MinRunnerVersion, Version, want)
	}
}
