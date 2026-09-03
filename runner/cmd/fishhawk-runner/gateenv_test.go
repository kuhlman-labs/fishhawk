package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestSanitizeEnv_StripsSecretsKeepsToolchain feeds sanitizeEnv a base slice
// carrying each known runner secret alongside the allow-listed system/toolchain
// vars and asserts every secret is dropped while every allow-listed key
// survives with its value intact.
func TestSanitizeEnv_StripsSecretsKeepsToolchain(t *testing.T) {
	base := []string{
		// Secrets — must all be stripped.
		"FISHHAWK_GITHUB_TOKEN=ghs_secret",
		"FISHHAWK_GITLAB_TOKEN=glpat_secret",
		"GITHUB_TOKEN=gh_secret",
		"GH_TOKEN=gh_secret2",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"OPENAI_API_KEY=sk-oai-secret",
		"FISHHAWK_API_TOKEN=mcp-secret",
		// Allow-listed — must survive.
		"PATH=/usr/bin:/bin",
		"HOME=/home/runner",
		"GOCACHE=/tmp/gocache",
		"GOMODCACHE=/tmp/gomodcache",
	}
	got := sanitizeEnv(base)
	gotMap := envSliceToMap(t, got)

	for _, secret := range []string{
		"FISHHAWK_GITHUB_TOKEN", "FISHHAWK_GITLAB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN",
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "FISHHAWK_API_TOKEN",
	} {
		if _, present := gotMap[secret]; present {
			t.Errorf("secret %s must be stripped, but it survived", secret)
		}
	}
	want := map[string]string{
		"PATH":       "/usr/bin:/bin",
		"HOME":       "/home/runner",
		"GOCACHE":    "/tmp/gocache",
		"GOMODCACHE": "/tmp/gomodcache",
	}
	for k, v := range want {
		if gotMap[k] != v {
			t.Errorf("allow-listed %s = %q, want %q", k, gotMap[k], v)
		}
	}
}

// TestSanitizeEnv_EdgeCases covers the parser/allow-list edge cases: a
// malformed entry with no '=', an empty key, an empty value, an LC_* prefix
// match, a GO-prefixed var (GOFLAGS), and a non-allow-listed innocuous var
// (AWS_REGION) that must be dropped despite carrying no secret.
func TestSanitizeEnv_EdgeCases(t *testing.T) {
	base := []string{
		"MALFORMED_NO_EQUALS",  // no '=' — dropped
		"=novalue",             // empty key — dropped
		"TZ=",                  // allow-listed key, empty value — kept
		"LC_ALL=en_US.UTF-8",   // LC_* prefix — kept
		"GOFLAGS=-mod=mod",     // GO* prefix, value contains '=' — kept whole
		"AWS_REGION=us-east-1", // not allow-listed, not secret — dropped
	}
	got := sanitizeEnv(base)
	gotMap := envSliceToMap(t, got)

	for _, dropped := range []string{"MALFORMED_NO_EQUALS", "", "AWS_REGION"} {
		if _, present := gotMap[dropped]; present {
			t.Errorf("%q should have been dropped, but survived", dropped)
		}
	}
	if v, present := gotMap["TZ"]; !present || v != "" {
		t.Errorf("TZ should survive with empty value, got present=%v value=%q", present, v)
	}
	if gotMap["LC_ALL"] != "en_US.UTF-8" {
		t.Errorf("LC_ALL = %q, want en_US.UTF-8", gotMap["LC_ALL"])
	}
	// GOFLAGS value contains a '=' — the parser must split on the FIRST '='
	// only, preserving the full value.
	if gotMap["GOFLAGS"] != "-mod=mod" {
		t.Errorf("GOFLAGS = %q, want -mod=mod (split on first '=' only)", gotMap["GOFLAGS"])
	}
}

// TestSanitizeEnv_RedactsGoproxyUserinfo asserts that embedded URL userinfo is
// stripped from credentialed GO* values (notably GOPROXY) before they reach
// gate code, while non-credentialed forms (off, direct, bare host, no-userinfo
// URL) and the proxy host/path survive byte-identical.
func TestSanitizeEnv_RedactsGoproxyUserinfo(t *testing.T) {
	base := []string{
		// Single credentialed proxy — userinfo redacted, host/path kept.
		"GOPROXY=https://user:tok@proxy.example.com",
		// Comma-separated list — each credentialed entry redacted, the
		// uncredentialed 'direct' fall-through untouched, order/separators kept.
		"GOSUMDB=https://u:p@sum.example.com,direct",
		// '|'-separated list — same, with the alternate separator preserved.
		"GONOSUMCHECK=https://a:b@one.example.com|https://two.example.com",
		// No userinfo — must be byte-identical.
		"GOPRIVATE=https://proxy.example.com/path",
		// Non-URL GO* forms — must be byte-identical.
		"GO111MODULE=on",
		"GOFLAGS=-mod=mod",
		// Allow-listed non-GO var — never run through the transform.
		"PATH=/usr/bin:/bin",
	}
	got := sanitizeEnv(base)
	gotMap := envSliceToMap(t, got)

	want := map[string]string{
		"GOPROXY":      "https://proxy.example.com",
		"GOSUMDB":      "https://sum.example.com,direct",
		"GONOSUMCHECK": "https://one.example.com|https://two.example.com",
		"GOPRIVATE":    "https://proxy.example.com/path",
		"GO111MODULE":  "on",
		"GOFLAGS":      "-mod=mod",
		"PATH":         "/usr/bin:/bin",
	}
	for k, v := range want {
		if gotMap[k] != v {
			t.Errorf("%s = %q, want %q", k, gotMap[k], v)
		}
	}
}

// TestSanitizeEnv_RedactsMixedSeparatorGoproxy exercises a GOPROXY list that
// mixes both fall-through separators (',' and '|') alongside a bare 'direct'
// entry: each credentialed entry must be redacted, the uncredentialed/'direct'
// entries untouched, and BOTH separators plus the entry order preserved exactly.
func TestSanitizeEnv_RedactsMixedSeparatorGoproxy(t *testing.T) {
	const in = "GOPROXY=https://u:p@a.example.com,https://b.example.com|direct"
	const want = "https://a.example.com,https://b.example.com|direct"

	got := sanitizeEnv([]string{in})
	gotMap := envSliceToMap(t, got)
	if gotMap["GOPROXY"] != want {
		t.Errorf("GOPROXY = %q, want %q", gotMap["GOPROXY"], want)
	}
}

// TestSanitizeEnv_PinsGitConfigNeutralization is the DONE-MEANS behavioral test
// for the git-config pin (#3102): sanitizeEnv must emit GIT_CONFIG_GLOBAL and
// GIT_CONFIG_SYSTEM at /dev/null so a gate child's temp-repo `git commit` cannot
// inherit the operator's global commit.gpgsign. One assertion per named mode:
// (a) both keys present at /dev/null when the base has NEITHER; (b) an inherited
// hostile value is REPLACED, not honoured; (c) exactly ONE entry per key in both
// cases, so no duplicate the platform's undocumented Cmd.Env dedup could resolve
// to the ambient value; (d) the pins do not displace PATH/HOME/Go-toolchain vars.
func TestSanitizeEnv_PinsGitConfigNeutralization(t *testing.T) {
	// (a) neither key inherited — both pinned to /dev/null.
	absent := []string{"PATH=/usr/bin:/bin", "HOME=/home/runner", "GOCACHE=/tmp/gocache"}
	gotAbsent := sanitizeEnv(absent)
	mapAbsent := envSliceToMap(t, gotAbsent)
	for _, k := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if mapAbsent[k] != "/dev/null" {
			t.Errorf("(a) %s = %q, want /dev/null", k, mapAbsent[k])
		}
		if n := countKey(gotAbsent, k); n != 1 {
			t.Errorf("(a/c) expected exactly 1 %s entry, got %d in %v", k, n, gotAbsent)
		}
	}
	// (d) the pins do not displace the allow-listed essentials.
	for k, v := range map[string]string{"PATH": "/usr/bin:/bin", "HOME": "/home/runner", "GOCACHE": "/tmp/gocache"} {
		if mapAbsent[k] != v {
			t.Errorf("(d) allow-listed %s = %q, want %q", k, mapAbsent[k], v)
		}
	}

	// (b) inherited hostile values — replaced, not honoured, still one entry each.
	hostile := []string{
		"GIT_CONFIG_GLOBAL=/home/op/.gitconfig",
		"GIT_CONFIG_SYSTEM=/etc/gitconfig",
		"PATH=/usr/bin:/bin",
	}
	gotHostile := sanitizeEnv(hostile)
	mapHostile := envSliceToMap(t, gotHostile)
	for _, k := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if mapHostile[k] != "/dev/null" {
			t.Errorf("(b) inherited %s = %q, want it replaced by /dev/null", k, mapHostile[k])
		}
		if n := countKey(gotHostile, k); n != 1 {
			t.Errorf("(b/c) expected exactly 1 %s entry after replacing inherited, got %d in %v", k, n, gotHostile)
		}
	}
}

// TestRunBoundedGateCommand_ChildSeesNeutralizedGitConfig observes the REAL child
// process env, not the sanitizer's return value (binding condition test): it
// seeds a hostile GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM into the runner process via
// t.Setenv, drives runBoundedGateCommand — which assigns
// withIsolatedLintCache(sanitizedGateEnv(), …) to cmd.Env and spawns `sh -c` — and
// asserts the child observes /dev/null for both AND still has PATH. Unwiring the
// sanitized slice from the exec.Cmd.Env, not just breaking the sanitizer, turns
// this red.
func TestRunBoundedGateCommand_ChildSeesNeutralizedGitConfig(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/home/op/.gitconfig")
	t.Setenv("GIT_CONFIG_SYSTEM", "/etc/gitconfig")

	dir := t.TempDir()
	lintCache := t.TempDir()
	const command = `echo "global=[$GIT_CONFIG_GLOBAL] system=[$GIT_CONFIG_SYSTEM] path=[${PATH:+set}]"`

	output, exitCode := runBoundedGateCommand(context.Background(), command, dir, lintCache, 30*time.Second)
	if exitCode != 0 {
		t.Fatalf("gate command exited %d; output: %s", exitCode, output)
	}
	if !strings.Contains(output, "global=[/dev/null] system=[/dev/null] path=[set]") {
		t.Errorf("output = %q, want both git-config vars pinned to /dev/null AND PATH still set", output)
	}
}

// TestSanitizedGateEnv_StripsLiveSecret is a thin check that the public
// entrypoint reads the live process env and drops a planted secret while
// keeping PATH.
func TestSanitizedGateEnv_StripsLiveSecret(t *testing.T) {
	t.Setenv("FISHHAWK_GITHUB_TOKEN", "leak-canary")
	got := sanitizedGateEnv()
	for _, kv := range got {
		if strings.HasPrefix(kv, "FISHHAWK_GITHUB_TOKEN=") {
			t.Fatalf("sanitizedGateEnv leaked the secret: %q", kv)
		}
	}
	if os.Getenv("PATH") != "" {
		var sawPath bool
		for _, kv := range got {
			if strings.HasPrefix(kv, "PATH=") {
				sawPath = true
				break
			}
		}
		if !sawPath {
			t.Error("sanitizedGateEnv dropped PATH, which must be preserved")
		}
	}
}

// TestWithIsolatedLintCache pins the pure helper that forces
// GOLANGCI_LINT_CACHE to a per-invocation dir: a base env without the var gains
// exactly one entry equal to the override; an inherited GOLANGCI_LINT_CACHE is
// dropped and replaced (a single entry, never a duplicate the platform might
// resolve to the ambient value); unrelated entries are preserved untouched.
func TestWithIsolatedLintCache(t *testing.T) {
	const override = "/tmp/iso-lint-cache"

	// (a) No inherited GOLANGCI_LINT_CACHE — exactly one entry is appended.
	base := []string{"PATH=/usr/bin:/bin", "HOME=/home/runner"}
	got := withIsolatedLintCache(base, override)
	if n := countKey(got, "GOLANGCI_LINT_CACHE"); n != 1 {
		t.Errorf("expected exactly 1 GOLANGCI_LINT_CACHE entry, got %d in %v", n, got)
	}
	gotMap := envSliceToMap(t, got)
	if gotMap["GOLANGCI_LINT_CACHE"] != override {
		t.Errorf("GOLANGCI_LINT_CACHE = %q, want %q", gotMap["GOLANGCI_LINT_CACHE"], override)
	}

	// (b) Inherited GOLANGCI_LINT_CACHE=/shared — dropped and replaced by the
	// override, leaving a single entry (no ambient value can win).
	inherited := []string{"PATH=/usr/bin:/bin", "GOLANGCI_LINT_CACHE=/shared", "HOME=/home/runner"}
	got = withIsolatedLintCache(inherited, override)
	if n := countKey(got, "GOLANGCI_LINT_CACHE"); n != 1 {
		t.Errorf("expected exactly 1 GOLANGCI_LINT_CACHE entry after replacing inherited, got %d in %v", n, got)
	}
	gotMap = envSliceToMap(t, got)
	if gotMap["GOLANGCI_LINT_CACHE"] != override {
		t.Errorf("inherited GOLANGCI_LINT_CACHE not overridden: = %q, want %q", gotMap["GOLANGCI_LINT_CACHE"], override)
	}

	// (c) Unrelated entries survive with their values intact.
	for k, v := range map[string]string{"PATH": "/usr/bin:/bin", "HOME": "/home/runner"} {
		if gotMap[k] != v {
			t.Errorf("unrelated %s = %q, want %q", k, gotMap[k], v)
		}
	}
}

// --- #2504: GOOGLE_* no longer rides the bare "GO" allow-prefix --------------

// TestGateEnvAllowed_RejectsGoogleCredentialKeys is the DIRECT counterfactual
// vehicle for control 1 (the narrowed allow rule). gateEnvAllowed does not
// consult the denylist, so restoring "GO" to gateEnvAllowPrefix turns this red
// on its own — the layered GOOGLE_ deny cannot mask an allow-list regression.
func TestGateEnvAllowed_RejectsGoogleCredentialKeys(t *testing.T) {
	for _, k := range []string{"GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT"} {
		if gateEnvAllowed(k) {
			t.Errorf("gateEnvAllowed(%q) = true, want false — the allow-list must not admit Google credential keys", k)
		}
	}
}

// TestGateEnvDenied_GooglePrefix is the DIRECT counterfactual vehicle for
// control 2 (the GOOGLE_ deny prefix). Deleting the "GOOGLE_" entry from
// gateEnvDenyPrefix turns this red on its own while the narrowed allow-list
// still stands. It also asserts every existing exact denylist key stays denied
// and that a legitimate toolchain var (GOPATH) is NOT swept up by the prefix.
func TestGateEnvDenied_GooglePrefix(t *testing.T) {
	for _, k := range []string{"GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_ANYTHING"} {
		if !gateEnvDenied(k) {
			t.Errorf("gateEnvDenied(%q) = false, want true", k)
		}
	}
	for k := range gateEnvDeny {
		if !gateEnvDenied(k) {
			t.Errorf("existing denylist key %q is no longer denied", k)
		}
	}
	if gateEnvDenied("GOPATH") {
		t.Error("gateEnvDenied(GOPATH) = true, want false — a legitimate toolchain var must not be denied")
	}
}

// TestSanitizeEnv_DropsGoogleCredentials is the end-to-end pin over sanitizeEnv:
// both Google credential keys are dropped while PATH and the toolchain vars
// survive with their values intact.
func TestSanitizeEnv_DropsGoogleCredentials(t *testing.T) {
	base := []string{
		"GOOGLE_API_KEY=leak-api",
		"GOOGLE_APPLICATION_CREDENTIALS=/path/creds.json",
		"PATH=/usr/bin:/bin",
		"GOPATH=/home/runner/go",
		"GOPROXY=https://proxy.example.com",
	}
	got := sanitizeEnv(base)
	gotMap := envSliceToMap(t, got)
	for _, k := range []string{"GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if _, present := gotMap[k]; present {
			t.Errorf("%s survived sanitizeEnv, want it dropped", k)
		}
	}
	for k, v := range map[string]string{
		"PATH":    "/usr/bin:/bin",
		"GOPATH":  "/home/runner/go",
		"GOPROXY": "https://proxy.example.com",
	} {
		if gotMap[k] != v {
			t.Errorf("%s = %q, want %q", k, gotMap[k], v)
		}
	}
}

// TestGateEnvAllowsGoToolchainVars is the OVER-NARROWING guard: every name in
// gateEnvAllowGo must survive sanitizeEnv with its value byte-intact, so an
// over-narrow set (a legitimate toolchain var dropped) red-lines here rather
// than surfacing later as an infra-shaped gate failure.
func TestGateEnvAllowsGoToolchainVars(t *testing.T) {
	for key := range gateEnvAllowGo {
		val := "value-for-" + key
		got := sanitizeEnv([]string{key + "=" + val})
		gotMap := envSliceToMap(t, got)
		if gotMap[key] != val {
			t.Errorf("Go toolchain var %s = %q, want %q — sanitizeEnv dropped or altered an allow-listed name", key, gotMap[key], val)
		}
	}
}

// TestSanitizedGateEnv_StripsLiveGoogleCredential seeds GOOGLE_API_KEY into the
// live process env and asserts sanitizedGateEnv() drops it while keeping PATH.
func TestSanitizedGateEnv_StripsLiveGoogleCredential(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "live-google-leak-canary")
	got := sanitizedGateEnv()
	for _, kv := range got {
		if strings.HasPrefix(kv, "GOOGLE_API_KEY=") {
			t.Fatalf("sanitizedGateEnv leaked GOOGLE_API_KEY: %q", kv)
		}
	}
	if os.Getenv("PATH") != "" {
		sawPath := false
		for _, kv := range got {
			if strings.HasPrefix(kv, "PATH=") {
				sawPath = true
				break
			}
		}
		if !sawPath {
			t.Error("sanitizedGateEnv dropped PATH, which must be preserved")
		}
	}
}

// TestRunBoundedGateCommand_ChildDoesNotInheritGoogleCredentials (binding
// condition 1) observes a REAL CHILD PROCESS environment, not just the
// sanitizer's return value. It drives the runner's actual gate-command path —
// runBoundedGateCommand assigns withIsolatedLintCache(sanitizedGateEnv(), …) to
// cmd.Env and spawns `sh -c <command>` — so a regression that broke the wiring
// of the sanitized slice into exec.Cmd.Env (not just the sanitizer) would leak
// the seeded Google credentials into the child's captured output and fail here.
// The command also echoes PATH, so the fix cannot be a blanket empty
// environment that silently breaks every gate command.
//
// The preview-probe spawn site (previewprobe.go::runPreviewCommand) consumes the
// SAME sanitizedGateEnv() base (appending only two non-secret FISHHAWK_PREVIEW_*
// vars), so it shares this wiring; a leak there would require sanitizedGateEnv
// itself to regress, which this test and the sanitizer pins above both catch.
func TestRunBoundedGateCommand_ChildDoesNotInheritGoogleCredentials(t *testing.T) {
	const apiMarker = "google-api-leak-marker-4f2a"
	const credMarker = "google-creds-leak-marker-9b7c"
	t.Setenv("GOOGLE_API_KEY", apiMarker)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credMarker)

	dir := t.TempDir()
	lintCache := t.TempDir()
	const command = `echo "google_key=[$GOOGLE_API_KEY] google_creds=[$GOOGLE_APPLICATION_CREDENTIALS] path=[${PATH:+set}]"`

	output, exitCode := runBoundedGateCommand(context.Background(), command, dir, lintCache, 30*time.Second)
	if exitCode != 0 {
		t.Fatalf("gate command exited %d; output: %s", exitCode, output)
	}
	if strings.Contains(output, apiMarker) || strings.Contains(output, credMarker) {
		t.Errorf("the gate child inherited a Google credential from the runner process:\n%s", output)
	}
	if !strings.Contains(output, "google_key=[] google_creds=[] path=[set]") {
		t.Errorf("output = %q, want the Google credentials empty AND PATH still set", output)
	}
}

// TestGateEnvListsMatchCLICopy is the lockstep divergence detector. The runner
// and CLI copies live in separate Go modules (both package main) and cannot
// import each other, so this reads the CLI source through the workspace root and
// asserts every set — allow-exact, allow-Go, allow-prefix, deny-exact,
// deny-prefix — is identical to the runner's own. A single-copy edit fails here.
//
// Degrade (binding condition 3): it SKIPS only when go.work itself cannot be
// found (a vendored or module-cache build with no workspace). If go.work IS
// found but the peer CLI file cannot be read or a literal block cannot be
// parsed, it FAILS — an unparseable peer is a detector malfunction, not an
// environment without a workspace. Both branches print the reason.
func TestGateEnvListsMatchCLICopy(t *testing.T) {
	root, err := findWorkspaceRoot()
	if err != nil {
		t.Skipf("go.work not found from the test working directory (%v); skipping the CLI-copy cross-check", err)
	}
	cliPath := filepath.Join(root, "cli", "cmd", "fishhawk", "doctor_verify.go")
	data, err := os.ReadFile(cliPath) //nolint:gosec // fixed workspace-relative path
	if err != nil {
		t.Fatalf("go.work found at %s but the peer CLI file %s could not be read: %v", root, cliPath, err)
	}
	src := string(data)

	for _, tc := range []struct {
		cliVar string
		want   map[string]struct{}
	}{
		{"verifyEnvAllowExact", gateEnvAllowExact},
		{"verifyEnvAllowGo", gateEnvAllowGo},
		{"verifyEnvDeny", gateEnvDeny},
	} {
		keys, perr := parseVarKeys(src, tc.cliVar)
		if perr != nil {
			t.Fatalf("parsing CLI %s from %s: %v", tc.cliVar, cliPath, perr)
		}
		if !sameSet(keys, tc.want) {
			t.Errorf("CLI %s = %v, runner copy = %v — the two copies diverged",
				tc.cliVar, sortedKeys(keys), sortedMapKeys(tc.want))
		}
	}

	for _, tc := range []struct {
		cliVar string
		want   []string
	}{
		{"verifyEnvAllowPrefix", gateEnvAllowPrefix},
		{"verifyEnvDenyPrefix", gateEnvDenyPrefix},
		// The git-config pins are a []string of full "KEY=/dev/null" entries
		// (binding condition 1). parseVarKeys extracts each quoted entry, and
		// sameSlice compares them byte-for-byte AND order-sensitively — the
		// strongest guarantee, chosen over an order-insensitive set because both
		// copies are fixed literal slices whose order is deterministic and
		// trivially kept in sync, so any drift in key, value, or order fails here.
		{"verifyEnvGitConfigPins", gateEnvGitConfigPins},
	} {
		keys, perr := parseVarKeys(src, tc.cliVar)
		if perr != nil {
			t.Fatalf("parsing CLI %s from %s: %v", tc.cliVar, cliPath, perr)
		}
		if !sameSlice(keys, tc.want) {
			t.Errorf("CLI %s = %v, runner copy = %v — the two copies diverged", tc.cliVar, keys, tc.want)
		}
	}
}

// findWorkspaceRoot walks up from THIS test's source file looking for go.work,
// returning the directory that contains it. It anchors on runtime.Caller rather
// than os.Getwd() because the runner suite's TestMain chdirs into a throwaway
// temp dir, so the process working directory is not the source tree. In a
// vendored or module-cache build the embedded source path does not exist on
// disk, so the stat walk reaches the filesystem root and returns an error —
// which the caller renders as a SKIP (no workspace), per binding condition 3.
func findWorkspaceRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller could not locate the test source file")
	}
	start := filepath.Dir(thisFile)
	for dir := start; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.work at or above %s", start)
		}
		dir = parent
	}
}

// parseVarKeys extracts the double-quoted keys from a `var <name> = …`
// declaration in Go source. It handles a single-line composite literal
// (`var x = []string{"a", "b"}`) and a multi-line one (`var x = map[…]{ … }`),
// distinguished by whether the declaration line ends with `{`. It returns an
// error when the declaration is not found, is not terminated, or yields no keys
// — an unparseable peer must FAIL the detector, not silently pass.
func parseVarKeys(src, name string) ([]string, error) {
	lines := strings.Split(src, "\n")
	startPrefix := "var " + name + " "
	idx := -1
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), startPrefix) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("declaration %q not found", name)
	}
	if strings.HasSuffix(strings.TrimSpace(lines[idx]), "{") {
		var keys []string
		for j := idx + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "}" {
				return nonEmptyKeys(name, keys)
			}
			keys = append(keys, quotedStrings(lines[j])...)
		}
		return nil, fmt.Errorf("declaration %q is not terminated by a closing brace", name)
	}
	return nonEmptyKeys(name, quotedStrings(lines[idx]))
}

// nonEmptyKeys guards the "parsed but empty" case so a mis-scanned block FAILS
// rather than comparing an empty set that would spuriously match nothing.
func nonEmptyKeys(name string, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("declaration %q parsed to zero keys", name)
	}
	return keys, nil
}

// quotedStrings returns every double-quoted substring in s (keys in these
// declarations carry no escaped quotes).
func quotedStrings(s string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			if inQuote {
				out = append(out, b.String())
				b.Reset()
			}
			inQuote = !inQuote
		case inQuote:
			b.WriteByte(c)
		}
	}
	return out
}

// sameSet reports whether keys (deduped) is exactly the key set of want.
func sameSet(keys []string, want map[string]struct{}) bool {
	got := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		got[k] = struct{}{}
	}
	if len(got) != len(want) {
		return false
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			return false
		}
	}
	return true
}

// sameSlice reports whether a and b are equal in length, order, and contents.
func sameSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortedKeys(keys []string) []string {
	out := append([]string(nil), keys...)
	sort.Strings(out)
	return out
}

func sortedMapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// countKey returns how many "KEY=..." entries in env have the given key.
func countKey(env []string, key string) int {
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			n++
		}
	}
	return n
}

func envSliceToMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := make(map[string]string, len(env))
	for _, kv := range env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			t.Errorf("sanitizeEnv emitted a malformed entry %q", kv)
			continue
		}
		m[kv[:eq]] = kv[eq+1:]
	}
	return m
}
