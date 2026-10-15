package version

import (
	"runtime/debug"
	"testing"
)

// This file is deliberately non-parallel: TestString swaps the package
// seam and the stamp vars.

const testPseudo = "v0.0.0-20261015175154-d624cd36df36"

func buildInfo(mainVersion string, settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{
		Main:     debug.Module{Version: mainVersion},
		Settings: settings,
	}
}

func setting(k, v string) debug.BuildSetting { return debug.BuildSetting{Key: k, Value: v} }

func TestResolve(t *testing.T) {
	rich := buildInfo(testPseudo,
		setting("vcs.revision", "deadbeefcafe"), setting("vcs.modified", "true"))

	tests := []struct {
		name       string
		stampedV   string
		stampedSHA string
		info       *debug.BuildInfo
		wantV      string
		wantSHA    string
	}{
		{"a release stamp wins over build info", "v1.2.3", "abc123", rich, "v1.2.3", "abc123"},
		{"b scripts/dev mixed stamp stays dev", "dev", "abc1234-dirty", rich, "dev", "abc1234-dirty"},
		{"c module pseudo-version", "dev", "unknown", buildInfo(testPseudo), testPseudo, "unknown"},
		{"d devel with clean revision", "dev", "unknown",
			buildInfo("(devel)", setting("vcs.revision", "deadbeefcafe"), setting("vcs.modified", "false")),
			"dev", "deadbeefcafe"},
		{"e devel with dirty revision", "dev", "unknown",
			buildInfo("(devel)", setting("vcs.revision", "deadbeefcafe"), setting("vcs.modified", "true")),
			"dev", "deadbeefcafe-dirty"},
		{"f devel without settings", "dev", "unknown", buildInfo("(devel)"), "dev", "unknown"},
		{"g empty main version", "dev", "unknown", buildInfo(""), "dev", "unknown"},
		{"h nil build info", "dev", "unknown", nil, "dev", "unknown"},
		{"i modified without revision", "dev", "unknown",
			buildInfo("(devel)", setting("vcs.modified", "true")), "dev", "unknown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, sha := resolve(tc.stampedV, tc.stampedSHA, tc.info)
			if v != tc.wantV || sha != tc.wantSHA {
				t.Errorf("resolve(%q, %q) = (%q, %q), want (%q, %q)",
					tc.stampedV, tc.stampedSHA, v, sha, tc.wantV, tc.wantSHA)
			}
		})
	}
}

func TestString(t *testing.T) {
	origV, origSHA, origRead := Version, GitSHA, readBuildInfo
	t.Cleanup(func() { Version, GitSHA, readBuildInfo = origV, origSHA, origRead })

	tests := []struct {
		name string
		v    string
		sha  string
		read func() (*debug.BuildInfo, bool)
		want string
	}{
		{"unstamped go install", "dev", "unknown",
			func() (*debug.BuildInfo, bool) { return buildInfo(testPseudo), true }, testPseudo},
		{"unstamped build info unavailable", "dev", "unknown",
			func() (*debug.BuildInfo, bool) { return nil, false }, "dev"},
		{"unstamped clone build", "dev", "unknown",
			func() (*debug.BuildInfo, bool) {
				return buildInfo("(devel)", setting("vcs.revision", "deadbeefcafe")), true
			}, "dev (deadbeefcafe)"},
		{"stamped sha ignores reader", "dev", "abc1234",
			func() (*debug.BuildInfo, bool) { return buildInfo(testPseudo), true }, "dev (abc1234)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			Version, GitSHA, readBuildInfo = tc.v, tc.sha, tc.read
			if got := String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	tests := []struct{ v, sha, want string }{
		{"dev", "unknown", "dev"},
		{"dev", "", "dev"},
		{"v1.2.3", "abc123", "v1.2.3 (abc123)"},
	}
	for _, tc := range tests {
		if got := format(tc.v, tc.sha); got != tc.want {
			t.Errorf("format(%q, %q) = %q, want %q", tc.v, tc.sha, got, tc.want)
		}
	}
}
