package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewsandbox"
)

// This file carries the BEHAVIOURAL controls on the #2522 codex credential
// boundary (#3082). It replaces shape-only assertions with a real spawned child
// that reports what IT can observe of itself, swept by the parent — which is the
// only side that holds the real CODEX_HOME, the sentinel and the fixture digest.
//
// WHAT IS CLAIMED, stated once, with its exemption inside the claim:
//
//	The child's environment, argv, cwd and resolved config lookups contain NO
//	ROUTE to any path under the real CODEX_HOME, OTHER THAN the synthesized home
//	it was deliberately given and that home's contents.
//
// WHAT IS NOT CLAIMED, and why. The synthesized home is created INSIDE the real
// CODEX_HOME (reviewsandbox/confine.go, os.MkdirTemp(realCodexHome,
// "fishhawk-confined-")) — the right call, because a ${TMPDIR} placement would
// move a copied live credential out of the one root the claude blocklist denies.
// The consequence is that the child's own CODEX_HOME carries the real path as
// its literal prefix and one filepath.Dir call recovers it. NON-DISCLOSURE of
// the real CODEX_HOME is therefore FALSE BY CONSTRUCTION and is claimed nowhere
// in this file. Containment of ROUTES is a different, true, falsifiable claim.
//
// WHAT ENFORCEMENT IS NOT PROVEN HERE. Everything below is CONFIGURATION: what
// the builder writes, what the child's environment routes to, and what the child
// could read. Whether the operating-system sandbox turns the grant set into an
// actual denial is not observable without a real codex binary; it is carried by
// the opt-in TestLive_* harness under FISHHAWK_LIVE_CONFINEMENT=1 (#3059).

// probeReportEnv names the env var carrying the path the confinement probe
// writes its self-report to. It is passed through cfg.EnvPassthrough and points
// at a parent-created temp file OUTSIDE the real CODEX_HOME (asserted by the
// sweep), so the report channel cannot itself become an exposed route.
const probeReportEnv = "FISHHAWK_PROBE_REPORT"

// Fixture credential bytes. The probe's mutating variants rewrite the confined
// copy with EXACT literal bytes so the parent can compare on disk byte-for-byte
// after the adapter's cleanup has run.
const (
	// probeAuthFixture is what the parent plants at <real CODEX_HOME>/auth.json.
	probeAuthFixture = `{"OPENAI_API_KEY":"sk-fixture-not-a-real-key","account_id":"acct-fixture"}`
	// probeAuthRefresh keeps the top-level key set and changes the values — a
	// credential-shaped refresh copyCredentialBack MUST land.
	probeAuthRefresh = `{"OPENAI_API_KEY":"sk-refreshed-not-a-real-key","account_id":"acct-refreshed"}`
	// probeAuthBadShape CHANGES the top-level key set — guardCredentialShape
	// MUST refuse it before the credential lock is taken.
	probeAuthBadShape = `{"OPENAI_API_KEY":"sk-swapped-not-a-real-key","account_id":"acct-x","EXTRA_KEY":"attacker"}`
)

// probeSentinelName is a file planted in the real CODEX_HOME and NEVER copied
// into the synthesized home. No reported route may reach it.
const probeSentinelName = "operator-sentinel.txt"

// pathPair records a path as the child saw it (Raw) and as filepath.EvalSymlinks
// resolved it (Resolved, empty when resolution failed). Both are reported
// because a symlinked route would slip past a raw-string comparison: on darwin
// /var is a symlink to /private/var, so a t.TempDir path and its resolved form
// differ.
type pathPair struct {
	Raw      string `json:"raw"`
	Resolved string `json:"resolved"`
}

// configLoad is one config file the child would load, plus the filesystem grant
// keys it parses out of that file's [permissions.confined.filesystem] table.
type configLoad struct {
	Path   pathPair   `json:"path"`
	Exists bool       `json:"exists"`
	Grants []pathPair `json:"grants"`
}

// authRead is the OUTCOME of the child actually attempting to read the
// credential it was handed. A listing proves presence; a successful read with a
// matching length and digest proves the child could USE it. The digest crosses
// the report boundary, never the bytes.
type authRead struct {
	OK     bool   `json:"ok"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
	Err    string `json:"err"`
}

// mutationResult is the OUTCOME of the child's attempt to rewrite its OWN copy
// of auth.json, and it is LOAD-BEARING rather than decorative. A copy-back
// refusal test that asserts only "the operator's file still holds the fixture
// bytes" passes IDENTICALLY when the guard correctly refuses the write and when
// the write never happened at all — the helper's mode mapping removed, or the
// os.WriteFile failing. Those two are indistinguishable from the operator's file
// alone, which makes such a test a shape assertion wearing a behavioural name.
// So the child reports whether the bad-shape bytes actually LANDED in its
// confined copy (with a read-back digest), and the parent asserts the ATTACK
// OCCURRED before it asserts the attack was REFUSED. Order matters.
type mutationResult struct {
	Attempted bool   `json:"attempted"`
	OK        bool   `json:"ok"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
	Err       string `json:"err"`
}

// probeReport is what the fake `codex` child records about ITSELF. It is told no
// path it could not derive on its own; the parent owns every comparison.
type probeReport struct {
	// RawEnviron is the child's os.Environ() verbatim (KEY=VALUE entries), kept
	// alongside Env because the exactly-one-CODEX_HOME-entry control is a claim
	// about ENTRIES, which the value-only view cannot express.
	RawEnviron []string   `json:"raw_environ"`
	Env        []pathPair `json:"env"`
	Args       []pathPair `json:"args"`
	Cwd        pathPair   `json:"cwd"`
	CodexHome  pathPair   `json:"codex_home"`
	Entries    []string   `json:"entries"`
	Configs    []configLoad
	Auth       authRead       `json:"auth"`
	Mutation   mutationResult `json:"mutation"`
}

// resolvePair records s raw plus its EvalSymlinks form (empty when unresolvable).
func resolvePair(s string) pathPair {
	p := pathPair{Raw: s}
	if r, err := filepath.EvalSymlinks(s); err == nil {
		p.Resolved = r
	}
	return p
}

func resolvePairs(ss []string) []pathPair {
	out := make([]pathPair, 0, len(ss))
	for _, s := range ss {
		out = append(out, resolvePair(s))
	}
	return out
}

// probeFilesystemGrants extracts the quoted keys of a config file's
// [permissions.confined.filesystem] table, including the ":minimal" pseudo-entry
// (the parent asserts the EXACT set, so nothing may be dropped here).
func probeFilesystemGrants(body string) []string {
	var out []string
	inTable := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			inTable = t == "[permissions."+reviewsandbox.ConfinedProfileName+".filesystem]"
			continue
		}
		if !inTable || !strings.HasPrefix(t, `"`) {
			continue
		}
		key, _, ok := strings.Cut(strings.TrimPrefix(t, `"`), `"`)
		if !ok {
			continue
		}
		out = append(out, key)
	}
	return out
}

// writeConfinementProbeReport is the CHILD half of the probe, called from
// TestHelperProcess. It records only what the child can observe of itself, then
// attempts the credential read. mutate, when non-empty, is written over
// $CODEX_HOME/auth.json before returning — the copy-back controls' vehicle.
func writeConfinementProbeReport(mutate string) {
	dest := os.Getenv(probeReportEnv)
	if dest == "" {
		return
	}
	home := os.Getenv("CODEX_HOME")
	rep := probeReport{
		RawEnviron: os.Environ(),
		Env:        resolvePairs(envValues(os.Environ())),
		Args:       resolvePairs(os.Args),
		CodexHome:  resolvePair(home),
	}
	if wd, err := os.Getwd(); err == nil {
		rep.Cwd = resolvePair(wd)
	}
	if ents, err := os.ReadDir(home); err == nil {
		for _, e := range ents {
			rep.Entries = append(rep.Entries, e.Name())
		}
	}
	for _, name := range []string{"config.toml", reviewsandbox.ConfinedProfileName + ".config.toml"} {
		path := filepath.Join(home, name)
		load := configLoad{Path: resolvePair(path)}
		if body, err := os.ReadFile(path); err == nil {
			load.Exists = true
			load.Grants = resolvePairs(probeFilesystemGrants(string(body)))
		}
		rep.Configs = append(rep.Configs, load)
	}

	authPath := filepath.Join(home, "auth.json")
	if data, err := os.ReadFile(authPath); err != nil {
		rep.Auth.Err = err.Error()
	} else {
		sum := sha256.Sum256(data)
		rep.Auth = authRead{OK: true, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])}
	}

	// The mutation runs BEFORE the report is serialized so its outcome can be
	// carried in the report. Written the other way round the parent has no way
	// to tell a refused write from a write that was never attempted.
	if mutate != "" {
		rep.Mutation.Attempted = true
		switch err := os.WriteFile(authPath, []byte(mutate), 0o600); {
		case err != nil:
			rep.Mutation.Err = "write: " + err.Error()
		default:
			back, rerr := os.ReadFile(authPath)
			switch {
			case rerr != nil:
				rep.Mutation.Err = "read-back: " + rerr.Error()
			case string(back) != mutate:
				rep.Mutation.Err = "read-back did not match the bytes written"
			default:
				sum := sha256.Sum256(back)
				rep.Mutation.OK = true
				rep.Mutation.Bytes = len(back)
				rep.Mutation.SHA256 = hex.EncodeToString(sum[:])
			}
		}
	}

	body, err := json.Marshal(rep)
	if err != nil {
		return
	}
	_ = os.WriteFile(dest, body, 0o600)
}

// envValues returns the VALUE half of each KEY=VALUE entry. Names are not
// paths; values are what could carry a route.
func envValues(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if _, v, ok := strings.Cut(kv, "="); ok {
			out = append(out, v)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The route predicate
// ---------------------------------------------------------------------------

// coversPath reports whether root is candidate or an ancestor of it, comparing
// PATH COMPONENTS rather than string prefixes: "/home/u/.codex-backup" must NOT
// be treated as covered by "/home/u/.codex". Same semantics as reviewsandbox's
// unexported pathCovers, which is not reachable from this package.
func coversPath(root, candidate string) bool {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	if root == candidate {
		return true
	}
	rc := strings.Split(root, string(filepath.Separator))
	cc := strings.Split(candidate, string(filepath.Separator))
	if len(rc) > len(cc) {
		return false
	}
	for i, seg := range rc {
		if cc[i] != seg {
			return false
		}
	}
	return true
}

// routeSpec holds the parent-side comparands for the sweep. BOTH SIDES ARE
// RESOLVED: a one-sided resolution produces silent mismatches in either
// direction (darwin's /var → /private/var makes a t.TempDir path and its
// EvalSymlinks form differ), so each side carries every spelling it has.
type routeSpec struct {
	// real is every spelling of the real CODEX_HOME.
	real []string
	// confined is every spelling of the child's synthesized CODEX_HOME — THE
	// SINGLE NAMED EXEMPTION, part of the claim rather than bolted on after it.
	confined []string
}

// spellings returns p raw-and-cleaned plus its EvalSymlinks form, de-duplicated.
func spellings(p string) []string {
	out := []string{filepath.Clean(p)}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		if c := filepath.Clean(r); c != out[0] {
			out = append(out, c)
		}
	}
	return out
}

// newRouteSpec resolves BOTH SIDES. The real home is resolved parent-side; the
// synthesized home is resolved from THREE sources, because by the time the sweep
// runs the adapter's deferred cleanup has already REMOVED it and a parent-side
// EvalSymlinks would fail — leaving only the raw spelling exempt while the real
// home carried its resolved spelling too, and every confined path would then
// look like a route (a FALSE RED, one-sided resolution exactly as condition 3
// warns). The three sources are: the child's own raw and resolved forms
// (recorded while the directory still existed), a best-effort parent-side
// resolution, and — spelling-independent — the synthesized home's base name
// re-joined onto EVERY spelling of the real home.
func newRouteSpec(realHome string, confinedHome pathPair) routeSpec {
	rs := routeSpec{real: spellings(realHome)}
	add := func(p string) {
		if p == "" {
			return
		}
		c := filepath.Clean(p)
		if !slices.Contains(rs.confined, c) {
			rs.confined = append(rs.confined, c)
		}
	}
	for _, p := range spellings(confinedHome.Raw) {
		add(p)
	}
	add(confinedHome.Resolved)
	base := filepath.Base(filepath.Clean(confinedHome.Raw))
	for _, r := range rs.real {
		add(filepath.Join(r, base))
	}
	return rs
}

// isRoute reports whether the reported value v is a ROUTE to the real
// CODEX_HOME, and is THE ONE PLACE the predicate is stated.
//
// A reported value is a ROUTE iff, for some spelling R of the resolved real
// CODEX_HOME:
//
//	PATH FORM      — v's own cleaned (or symlink-resolved) form EQUALS R, or is
//	                 UNDER R;
//	COMPOSITE FORM — some path-shaped TOKEN extracted from v, once cleaned and
//	                 symlink-resolved, EQUALS R or is UNDER R;
//	EMBEDDED FORM  — v's raw text CONTAINS the spelling R at a path boundary, and
//	                 the remainder of v from that point, once cleaned and
//	                 symlink-resolved, EQUALS R or is UNDER R.
//
// An ANCESTOR IS NOT A ROUTE. ${TMPDIR} does not grant access to a credential
// nested inside it, and the child legitimately reports TMPDIR, HOME and other
// ancestors — a predicate that rejected them would make the clean baseline RED.
//
// The composite form exists because comparing each whole env value or argv
// element as a single filesystem path cannot see `--config=/real/home/auth.json`,
// a PATH-style list, or a path embedded in JSON — yet the claim covers exactly
// those.
//
// The EMBEDDED form is SUPPLEMENTARY to the composite one and exists because
// pathTokens cuts on a FIXED delimiter set that includes whitespace and colon.
// A real CODEX_HOME whose own path contains one of those characters is therefore
// FRAGMENTED by the tokenizer and no token can equal or fall under it:
// `--config=/Users/Jane Doe/.codex/auth.json` splits at the space, no fragment
// is a route, and a token-only sweep stays GREEN while the value literally
// embeds the credential path. Home directories with spaces are ordinary on
// macOS, so that is reachable in production rather than hypothetical. Widening
// the delimiter set would only move the boundary to the next character, so the
// answer is to locate R by RAW SUBSTRING instead — a net no tokenizer blind spot
// can slip through — and then decide on the located remainder.
//
// THE SINGLE EXEMPTION is the synthesized home and its contents, and it is
// applied ONLY AFTER the candidate path has been normalised — never to the raw
// TEXT of the value. Eliding the confined home's text first would accept
// `--config=<confined home>/../auth.json`: the exempt prefix is substituted away
// and the `..` is never resolved, though the parsed path lands squarely in the
// real CODEX_HOME. The same blind spot swallows an embedded SYMLINK, which only
// resolves once it has been extracted from its surroundings. So the order is
// EXTRACT (by token, or by substring boundary), then CLEAN/RESOLVE, then decide
// — and that ordering is what lets the supplementary check keep the exemption
// intact rather than re-opening the traversal hole it closed.
func (rs routeSpec) isRoute(v pathPair) (bool, string) {
	for _, form := range []string{v.Raw, v.Resolved} {
		if form == "" {
			continue
		}
		if route, why := rs.classify(form); route {
			return true, "path form " + why + ": " + form
		}
		for _, tok := range pathTokens(form) {
			if tok == form {
				continue // already decided by the path form above
			}
			if route, why := rs.classify(tok); route {
				return true, "composite value embeds " + tok + ", which " + why + ": " + form
			}
		}
		if route, why := rs.embedsRealHomeSpelling(form); route {
			return true, "composite value embeds the real CODEX_HOME spelling in text no path token could isolate — " + why + ": " + form
		}
	}
	return false, ""
}

// embedsRealHomeSpelling is the SUPPLEMENTARY raw-substring half of the
// composite check, and it runs LAST — after token-level extraction has already
// decided every exemption and normalisation it can. Its job is only the
// remainder the tokenizer cannot see.
//
// It locates each occurrence of a real-CODEX_HOME spelling in the RAW text, then
// hands the text FROM that occurrence TO THE END OF THE VALUE to classify. Two
// details carry the whole design:
//
//   - The occurrence must sit at a path boundary — start of value, or preceded
//     by one of pathTokens' delimiters. Without it `/opt/home/u/.codex/auth.json`
//     would be read as a route to `/home/u/.codex`, which it is not.
//   - The candidate is the MAXIMAL remainder, never the bare spelling. The bare
//     spelling always equals the real home and would flag the confined home's own
//     files (whose paths carry the real home as a literal prefix — see the file
//     header) as routes, a false RED on every legitimate value. Classifying the
//     remainder instead keeps the confined-home exemption intact, still catches
//     `<confined>/../auth.json` because cleaning resolves the traversal, and
//     leaves "/home/u/.codex-backup/auth.json" GREEN because coversPath compares
//     components rather than string prefixes.
func (rs routeSpec) embedsRealHomeSpelling(form string) (bool, string) {
	for _, r := range rs.real {
		for i := 0; i+len(r) <= len(form); i++ {
			if !strings.HasPrefix(form[i:], r) {
				continue
			}
			if i > 0 {
				prev, _ := utf8.DecodeLastRuneInString(form[:i])
				if !isPathDelimiter(prev) {
					continue // a proper substring of a LONGER path component
				}
			}
			if route, why := rs.classify(form[i:]); route {
				return true, why
			}
		}
	}
	return false, ""
}

// classify NORMALISES p first — cleaning `..` traversals, and resolving symlinks
// where the path exists — and only then applies the exemption and the route
// test. It reports why p is a route, or false.
//
// Every form of p is judged independently: a path whose CLEANED form sits inside
// the synthesized home but whose RESOLVED form escapes into the real home is a
// route on the strength of the resolved form. Exemption suppresses only the form
// it actually covers.
func (rs routeSpec) classify(p string) (bool, string) {
	if p == "" {
		return false, ""
	}
	for _, form := range normalizations(p) {
		exempt := false
		for _, c := range rs.confined {
			if coversPath(c, form) {
				exempt = true
				break
			}
		}
		if exempt {
			continue
		}
		for _, r := range rs.real {
			if coversPath(r, form) {
				return true, "normalises to " + form + ", under the real CODEX_HOME " + r
			}
		}
	}
	return false, ""
}

// normalizations returns p cleaned, plus its EvalSymlinks form when the path
// exists and differs. Cleaning is what turns a `..` traversal into the path it
// actually names; symlink resolution is what turns a link inside the confined
// home into the real-home path it points at.
func normalizations(p string) []string {
	out := []string{filepath.Clean(p)}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		if c := filepath.Clean(r); c != out[0] {
			out = append(out, c)
		}
	}
	return out
}

// pathTokens splits a composite value into the candidate filesystem paths
// embedded in it, cutting on the characters that separate a path from its
// surroundings in the shapes this claim covers: a flag's `=` assignment, a
// `:`-delimited PATH-style list, JSON punctuation and quoting, and whitespace.
// Only tokens carrying a separator are returned — the rest cannot be paths.
//
// Tokenizing rather than substring-matching is also what keeps the composite
// check off "/home/u/.codex-backup/auth.json" for a real home of
// "/home/u/.codex": the token is compared COMPONENT-WISE by coversPath, so a
// sibling directory sharing a string prefix is not a finding and the clean
// baseline stays GREEN.
//
// The delimiter set is FIXED, and that is a known, bounded blind spot rather
// than an oversight: a real CODEX_HOME whose own path contains one of these
// characters (a space is ordinary on macOS) is fragmented and unmatchable here.
// embedsRealHomeSpelling covers exactly that remainder, which is why this set is
// deliberately NOT widened — widening only moves the boundary to the next
// character.
func pathTokens(s string) []string {
	fields := strings.FieldsFunc(s, isPathDelimiter)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if strings.ContainsRune(f, filepath.Separator) {
			out = append(out, f)
		}
	}
	return out
}

// isPathDelimiter reports whether r separates a path from its surroundings in a
// composite value. It is shared by pathTokens (which cuts on it) and
// embedsRealHomeSpelling (which requires a located spelling to be preceded by
// one), so the two halves of the composite check agree on one boundary notion.
func isPathDelimiter(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '=', ':', ',', ';', '"', '\'', '`',
		'{', '}', '[', ']', '(', ')', '<', '>', '|', '&', '?', '*':
		return true
	}
	return false
}

// sweepRoutes returns one finding per reported value that is a route to the real
// CODEX_HOME outside the single named exemption. adapterArgv is the argv the
// ADAPTER built (captured by capturingHelper); the child's own os.Args is the
// re-exec'd test binary, so both are swept.
func (rs routeSpec) sweepRoutes(rep probeReport, adapterArgv []string) []string {
	var findings []string
	check := func(where string, v pathPair) {
		if route, why := rs.isRoute(v); route {
			findings = append(findings, where+": "+why)
		}
	}
	for i, v := range rep.Env {
		check("child env value["+itoa(i)+"]", v)
	}
	for i, v := range rep.Args {
		check("child argv["+itoa(i)+"]", v)
	}
	for i, v := range resolvePairs(adapterArgv) {
		check("adapter argv["+itoa(i)+"]", v)
	}
	check("child cwd", rep.Cwd)
	for _, cfg := range rep.Configs {
		check("resolved config path", cfg.Path)
		for _, g := range cfg.Grants {
			check("parsed filesystem grant", g)
		}
	}
	return findings
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{digits[i%10]}, b...)
		i /= 10
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Driving the real adapter
// ---------------------------------------------------------------------------

// probeRun is one end-to-end confinement-probe invocation: it drives the REAL
// adapter path (Client.InferenceInTree → reviewsandbox.CodexConfinedHome → a
// spawned child) and returns what that child observed.
type probeRun struct {
	hp       *reviewsandbox.HostPaths
	realHome string
	sentinel string
	authPath string
	treeDir  string
	report   probeReport
	argv     []string
	spec     routeSpec
}

// runConfinementProbe plants the fixture credential and the never-copied
// sentinel in a temp real CODEX_HOME, seeds the environment so the INHERITED
// CODEX_HOME entry genuinely exists (CODEX_HOME is on reviewsandbox.CodexAllow,
// so it survives the scrub — confirmed in env.go), and drives the adapter with
// cmd.Env left nil so the ADAPTER builds the real child environment.
func runConfinementProbe(t *testing.T, mode string) *probeRun {
	t.Helper()
	hp := testHostPaths(t)
	r := &probeRun{
		hp:       hp,
		realHome: hp.CodexHome,
		sentinel: filepath.Join(hp.CodexHome, probeSentinelName),
		authPath: filepath.Join(hp.CodexHome, "auth.json"),
		treeDir:  t.TempDir(),
	}
	if err := os.WriteFile(r.authPath, []byte(probeAuthFixture), 0o600); err != nil {
		t.Fatalf("plant fixture auth.json: %v", err)
	}
	if err := os.WriteFile(r.sentinel, []byte("OPERATOR-SENTINEL-NEVER-COPIED"), 0o600); err != nil {
		t.Fatalf("plant sentinel: %v", err)
	}
	// OUTSIDE the real CODEX_HOME, so the report channel cannot itself be a route.
	reportPath := filepath.Join(t.TempDir(), "probe-report.json")

	t.Setenv("GO_HELPER_PROCESS", "1")
	t.Setenv("HELPER_MODE", mode)
	t.Setenv(probeReportEnv, reportPath)
	t.Setenv("CODEX_HOME", hp.CodexHome)

	cfg := testConfig()
	cfg.EnvPassthrough = []string{"GO_HELPER_PROCESS", "HELPER_MODE", probeReportEnv}
	c := NewClient(cfg)
	// passEnv=false leaves cmd.Env nil so the ADAPTER's scrub + appendEnvOverride
	// build the environment under test.
	c.Cmd = capturingHelper(mode, false, &r.argv, nil)
	c.HostPaths = hp
	c.GOOS = "linux"

	if _, _, _, err := c.InferenceInTree(context.Background(), "review", r.treeDir); err != nil {
		t.Fatalf("InferenceInTree(%s): %v", mode, err)
	}
	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("the probe wrote no report to %q: %v", reportPath, err)
	}
	if err := json.Unmarshal(body, &r.report); err != nil {
		t.Fatalf("decode probe report: %v", err)
	}
	if r.report.CodexHome.Raw == "" {
		t.Fatal("the child reported an EMPTY CODEX_HOME — the confined home never reached it")
	}
	r.spec = newRouteSpec(r.realHome, r.report.CodexHome)
	if coversPath(r.realHome, reportPath) {
		t.Fatalf("the report channel %q lies INSIDE the real CODEX_HOME %q — it would be a route of the test's own making", reportPath, r.realHome)
	}
	return r
}

// assertExemptionHolds pins the SINGLE named exemption rather than assuming it:
// the child's CODEX_HOME must be a PROPER subdirectory of the real CODEX_HOME
// carrying the `fishhawk-confined-` prefix.
func (r *probeRun) assertExemptionHolds(t *testing.T) {
	t.Helper()
	home := r.report.CodexHome.Raw
	if !coversPath(r.realHome, home) || filepath.Clean(home) == filepath.Clean(r.realHome) {
		t.Fatalf("child CODEX_HOME = %q, want a PROPER subdirectory of the real CODEX_HOME %q", home, r.realHome)
	}
	if !strings.HasPrefix(filepath.Base(home), "fishhawk-confined-") {
		t.Fatalf("child CODEX_HOME base = %q, want the `fishhawk-confined-` prefix", filepath.Base(home))
	}
}

// assertProbeMutationLanded proves THE ATTACK HAPPENED, and must be called
// before any assertion that the attack was refused. Without it, a refusal test
// reading only the operator's file cannot distinguish "the guard refused" from
// "no mutation was ever attempted" — the helper's mode mapping deleted, or the
// child's os.WriteFile failing — and would stay GREEN in both.
func (r *probeRun) assertProbeMutationLanded(t *testing.T, want string) {
	t.Helper()
	m := r.report.Mutation
	if !m.Attempted {
		t.Fatalf("the probe attempted NO mutation of its confined auth.json: nothing was refused, so a refusal assertion here would pass VACUOUSLY (helper mode mapping missing?)")
	}
	if !m.OK {
		t.Fatalf("the probe's mutation of its confined auth.json did NOT land (%s): nothing was refused, so a refusal assertion here would pass VACUOUSLY", m.Err)
	}
	sum := sha256.Sum256([]byte(want))
	if wantSum := hex.EncodeToString(sum[:]); m.Bytes != len(want) || m.SHA256 != wantSum {
		t.Fatalf("the probe wrote %d bytes sha256 %s into its confined auth.json, want the %d-byte %s — the mutation under test is not the one this test names",
			m.Bytes, m.SHA256, len(want), wantSum)
	}
}

// TestConfinementBoundary_ChildRoutesNameOnlyTheSynthesizedHome is the central
// control, and the BASELINE condition-1 requires: it is GREEN on a clean child
// whose environment legitimately carries TMPDIR, HOME and other ANCESTORS of the
// real CODEX_HOME. An ancestor is not a route.
//
// This is a containment claim about ROUTES, not a non-disclosure claim: the
// child can recover the real CODEX_HOME from its own CODEX_HOME with one
// filepath.Dir call, and this test does not pretend otherwise.
func TestConfinementBoundary_ChildRoutesNameOnlyTheSynthesizedHome(t *testing.T) {
	r := runConfinementProbe(t, "confinement_probe")
	r.assertExemptionHolds(t)

	if findings := r.spec.sweepRoutes(r.report, r.argv); len(findings) != 0 {
		t.Errorf("the child holds %d route(s) to the real CODEX_HOME %q outside the synthesized home %q:\n  %s",
			len(findings), r.realHome, r.report.CodexHome.Raw, strings.Join(findings, "\n  "))
	}

	// Named, separately: nothing reaches the never-copied sentinel or the
	// operator's real auth.json.
	for _, target := range []struct{ label, path string }{
		{"the never-copied operator sentinel", r.sentinel},
		{"the operator's real auth.json", r.authPath},
	} {
		for _, v := range append(append([]pathPair{}, r.report.Env...), r.report.Args...) {
			for _, form := range []string{v.Raw, v.Resolved} {
				if form != "" && strings.Contains(form, target.path) {
					t.Errorf("a reported value names %s (%q): %q", target.label, target.path, form)
				}
			}
		}
	}
	// The sentinel was never copied into the synthesized home.
	if slices.Contains(r.report.Entries, probeSentinelName) {
		t.Errorf("the synthesized home holds %q — the sentinel was copied in; entries=%v", probeSentinelName, r.report.Entries)
	}
}

// TestConfinementBoundary_ExactlyOneCodexHomeEntry is keyed to appendEnvOverride
// (client.go), which STRIPS every existing "CODEX_HOME=" entry before appending.
// CODEX_HOME is on reviewsandbox.CodexAllow, so the inherited real-CODEX_HOME
// entry genuinely survives the scrub and a plain append would be SHADOWED by it.
//
// The two failure modes are diagnosed DISTINCTLY so they cannot look alike.
func TestConfinementBoundary_ExactlyOneCodexHomeEntry(t *testing.T) {
	r := runConfinementProbe(t, "confinement_probe")
	r.assertExemptionHolds(t)

	var entries []string
	for _, kv := range r.report.RawEnviron {
		if strings.HasPrefix(kv, "CODEX_HOME=") {
			entries = append(entries, kv)
		}
	}
	switch {
	case len(entries) > 1:
		t.Fatalf("duplicate CODEX_HOME entries in the child environ (appendEnvOverride did not strip the "+
			"inherited entry) — this is an override-plumbing defect, NOT an exposed directory; observed: %q", entries)
	case len(entries) == 0:
		t.Fatalf("the child environ carries NO CODEX_HOME entry at all — the confined home never reached it")
	}
	got := strings.TrimPrefix(entries[0], "CODEX_HOME=")
	if got != r.report.CodexHome.Raw {
		t.Fatalf("child CODEX_HOME entry = %q, want the synthesized home %q — an exposed directory; observed entries: %q",
			got, r.report.CodexHome.Raw, entries)
	}
}

// TestConfinementBoundary_ChildReadsTheCopiedCredential is the half of the
// copy's meaning nothing observed before: the copy is only worth making if the
// child can actually READ it. The probe ATTEMPTS the read rather than listing
// the directory; the digest, never the bytes, crosses the report boundary.
func TestConfinementBoundary_ChildReadsTheCopiedCredential(t *testing.T) {
	r := runConfinementProbe(t, "confinement_probe")

	if !r.report.Auth.OK {
		t.Fatalf("the child could NOT read the credential it was handed: %s", r.report.Auth.Err)
	}
	if want := len(probeAuthFixture); r.report.Auth.Bytes != want {
		t.Errorf("the child read %d bytes, want the planted fixture's %d", r.report.Auth.Bytes, want)
	}
	sum := sha256.Sum256([]byte(probeAuthFixture))
	if want := hex.EncodeToString(sum[:]); r.report.Auth.SHA256 != want {
		t.Errorf("the child read sha256 %s, want the planted fixture's %s", r.report.Auth.SHA256, want)
	}
}

// TestConfinementBoundary_GrantSetExcludesRealHomeAndCredential is where the
// boundary claim actually lives: the codex permission GRANT SET. It is a
// CONFIGURATION assertion about what the builder writes and what the child would
// load. Whether the kernel turns that configuration into a denial is NOT
// observable here — that is the requires_live_validation half (#3059).
func TestConfinementBoundary_GrantSetExcludesRealHomeAndCredential(t *testing.T) {
	r := runConfinementProbe(t, "confinement_probe")
	r.assertExemptionHolds(t)

	schemaPath := argvValue(r.argv, "--output-schema")
	if schemaPath == "" {
		t.Fatalf("adapter argv carries no --output-schema pair: %q", r.argv)
	}
	want := []string{":minimal", r.treeDir, schemaPath}
	slices.Sort(want)

	forbidden := []struct{ label, path string }{
		{"the real CODEX_HOME", r.realHome},
		{"the never-copied operator sentinel", r.sentinel},
		{"the operator's real auth.json", r.authPath},
		{"the synthesized home (which holds the copied credential)", r.report.CodexHome.Raw},
	}

	if len(r.report.Configs) == 0 {
		t.Fatal("the child parsed NO config files")
	}
	for _, cfg := range r.report.Configs {
		if !cfg.Exists {
			t.Errorf("the child could not load %q", cfg.Path.Raw)
			continue
		}
		var got []string
		for _, g := range cfg.Grants {
			got = append(got, g.Raw)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s filesystem grants = %v, want exactly %v", filepath.Base(cfg.Path.Raw), got, want)
		}
		for _, g := range cfg.Grants {
			if g.Raw == ":minimal" {
				continue
			}
			for _, f := range forbidden {
				if coversPath(g.Raw, f.path) {
					t.Errorf("%s grants %q, which COVERS %s (%q)", filepath.Base(cfg.Path.Raw), g.Raw, f.label, f.path)
				}
			}
		}
	}
}

// argvValue returns the value following flag in argv, or "".
func argvValue(argv []string, flag string) string {
	i := slices.Index(argv, flag)
	if i < 0 || i+1 >= len(argv) {
		return ""
	}
	return argv[i+1]
}

// TestCredentialCopyBack_ProbeMutationReachesOperatorFile drives the FULL
// adapter path (so the deferred cleanup runs) with a probe that rewrites its own
// copy of auth.json with a CREDENTIAL-SHAPED refresh, then reads the OPERATOR'S
// file from disk AFTER the call returns. Read-insufficient by construction: an
// error identity would be byte-identical whether or not the write landed.
func TestCredentialCopyBack_ProbeMutationReachesOperatorFile(t *testing.T) {
	r := runConfinementProbe(t, "confinement_probe_refresh")

	got, err := os.ReadFile(r.authPath)
	if err != nil {
		t.Fatalf("read operator auth.json: %v", err)
	}
	if string(got) != probeAuthRefresh {
		t.Errorf("operator auth.json = %q, want the child's credential-shaped refresh %q", got, probeAuthRefresh)
	}
}

// TestCredentialCopyBack_ProbeShapeViolationLeavesOperatorFileByteIdentical is
// the mutually-unsatisfiable twin: the probe writes a CHANGED top-level key set,
// which guardCredentialShape must refuse BEFORE the credential lock is taken. No
// single implementation state passes both this and the refresh case.
//
// ORDER IS THE POINT: prove the attack HAPPENED, then prove it was REFUSED. The
// operator's file alone cannot tell the two apart — see mutationResult.
func TestCredentialCopyBack_ProbeShapeViolationLeavesOperatorFileByteIdentical(t *testing.T) {
	r := runConfinementProbe(t, "confinement_probe_badshape")
	r.assertProbeMutationLanded(t, probeAuthBadShape)

	got, err := os.ReadFile(r.authPath)
	if err != nil {
		t.Fatalf("read operator auth.json: %v", err)
	}
	if string(got) == probeAuthBadShape {
		t.Fatalf("the shape-violating swap LANDED on the operator's credential: %q", got)
	}
	if string(got) != probeAuthFixture {
		t.Errorf("operator auth.json = %q, want it BYTE-IDENTICAL to the planted fixture %q", got, probeAuthFixture)
	}
}

// TestCoversPath_ComponentWiseNotStringPrefix pins the sweep's own matcher, so
// it cannot silently become a substring check.
func TestCoversPath_ComponentWiseNotStringPrefix(t *testing.T) {
	cases := []struct {
		root, candidate string
		want            bool
	}{
		{"/home/u/.codex", "/home/u/.codex", true},
		{"/home/u/.codex", "/home/u/.codex/auth.json", true},
		{"/home/u/.codex", "/home/u/.codex-backup", false},
		{"/home/u/.codex", "/home/u/.codex-backup/auth.json", false},
		{"/home/u/.codex", "/home/u", false}, // an ANCESTOR is not covered
		{"/home/u/.codex", "/home/u/.cod", false},
		{"/home/u/.codex/", "/home/u/.codex/x", true}, // trailing separator cleaned
	}
	for _, c := range cases {
		if got := coversPath(c.root, c.candidate); got != c.want {
			t.Errorf("coversPath(%q, %q) = %v, want %v", c.root, c.candidate, got, c.want)
		}
	}
}

// TestSweepRoutes_NoticesWholeValueAndCompositeExposures drives the sweep
// hermetically over synthesized reports, one case per thing the predicate must
// and must not notice. It establishes exactly one thing: THE SWEEP NOTICES AN
// EXPOSED PATH IT WAS NOT TOLD TO EXPECT. It does not establish non-disclosure.
func TestSweepRoutes_NoticesWholeValueAndCompositeExposures(t *testing.T) {
	const realHome = "/home/u/.codex"
	confined := realHome + "/fishhawk-confined-abc"
	spec := routeSpec{real: []string{realHome}, confined: []string{confined}}

	clean := probeReport{
		Env: []pathPair{
			{Raw: "/tmp"},                            // an ANCESTOR-shaped path: not a route
			{Raw: "/home/u"},                         // the real home's PARENT: not a route
			{Raw: "/home/u/.codex-backup/auth.json"}, // a sibling by string prefix only
			{Raw: confined},
			{Raw: confined + "/auth.json"},
			{Raw: "--config=" + confined + "/config.toml"},
		},
		Cwd: pathPair{Raw: "/tmp/tree"},
	}
	if got := spec.sweepRoutes(clean, nil); len(got) != 0 {
		t.Errorf("clean report produced findings (a FALSE RED): %v", got)
	}

	exposures := []struct {
		name   string
		report probeReport
		argv   []string
	}{
		{
			name:   "whole-value sibling under the real home",
			report: probeReport{Env: []pathPair{{Raw: realHome + "/operator-sentinel.txt"}}},
		},
		{
			name:   "the real home itself",
			report: probeReport{Env: []pathPair{{Raw: realHome}}},
		},
		{
			name:   "composite argv element",
			report: probeReport{Args: []pathPair{{Raw: "--config=" + realHome + "/auth.json"}}},
		},
		{
			name:   "PATH-style list embedding the real home",
			report: probeReport{Env: []pathPair{{Raw: "/usr/bin:" + realHome + "/bin:/bin"}}},
		},
		{
			name:   "path embedded in JSON",
			report: probeReport{Env: []pathPair{{Raw: `{"creds":"` + realHome + `/auth.json"}`}}},
		},
		{
			name:   "cwd inside the real home",
			report: probeReport{Cwd: pathPair{Raw: realHome}},
		},
		{
			name:   "a parsed grant covering the real home",
			report: probeReport{Configs: []configLoad{{Grants: []pathPair{{Raw: realHome}}}}},
		},
		{
			name:   "adapter argv composite",
			report: probeReport{},
			argv:   []string{"codex", "--config=" + realHome + "/auth.json"},
		},
		{
			name:   "resolved-only exposure (raw form is clean)",
			report: probeReport{Env: []pathPair{{Raw: "/link", Resolved: realHome + "/auth.json"}}},
		},
		{
			// The exemption must be applied AFTER normalisation. Eliding the
			// confined home's TEXT first substitutes the exempt prefix away and
			// never resolves the `..`, so this traversal — which lands in the
			// real home — would be accepted.
			name:   "composite `..` traversal out of the confined home into the real one",
			report: probeReport{Args: []pathPair{{Raw: "--config=" + confined + "/../auth.json"}}},
		},
		{
			name:   "PATH-style list whose entry traverses out of the confined home",
			report: probeReport{Env: []pathPair{{Raw: "/usr/bin:" + confined + "/../bin:/bin"}}},
		},
	}
	for _, c := range exposures {
		if got := spec.sweepRoutes(c.report, c.argv); len(got) == 0 {
			t.Errorf("%s: the sweep saw NO route — this exposure is unpinned", c.name)
		}
	}

	// A real CODEX_HOME whose own path carries one of pathTokens' delimiters.
	// The tokenizer FRAGMENTS such a home — `--config=/Users/Jane Doe/.codex/
	// auth.json` splits at the space and no fragment equals or falls under the
	// real home — so a token-only sweep stays GREEN on a value that literally
	// embeds the credential path. These rows are load-bearing on
	// embedsRealHomeSpelling: delete it and the whole block goes green.
	//
	// A space is ordinary in a macOS home directory; a colon is a legal byte in a
	// POSIX path and the customary PATH-list separator, which is what makes it the
	// second-worst case. Both are pure strings here — no filesystem is involved,
	// so neither depends on what the host allows in a filename.
	for _, home := range []struct{ label, real string }{
		{"a real CODEX_HOME containing a SPACE", "/Users/Jane Doe/.codex"},
		{"a real CODEX_HOME containing a COLON", "/home/j:doe/.codex"},
	} {
		conf := home.real + "/fishhawk-confined-abc"
		dspec := routeSpec{real: []string{home.real}, confined: []string{conf}}

		// The clean control FIRST: the supplementary check must not turn the
		// exempt confined home — whose path carries the real home as a literal
		// prefix — or a string-prefix sibling into a false RED.
		dclean := probeReport{
			Env: []pathPair{
				{Raw: conf},
				{Raw: conf + "/auth.json"},
				{Raw: "--config=" + conf + "/config.toml"},
				{Raw: home.real + "-backup/auth.json"},
				{Raw: "/opt" + home.real + "/auth.json"}, // a DIFFERENT home that merely ends in the same spelling
				{Raw: "/tmp"},
				{Raw: filepath.Dir(home.real)},
			},
			Cwd: pathPair{Raw: "/tmp/tree"},
		}
		if got := dspec.sweepRoutes(dclean, nil); len(got) != 0 {
			t.Errorf("%s: clean report produced findings (a FALSE RED): %v", home.label, got)
		}

		for _, c := range []struct{ name, value string }{
			{"composite argv element", "--config=" + home.real + "/auth.json"},
			{"PATH-style list", "/usr/bin:" + home.real + "/bin:/bin"},
			{"path embedded in JSON", `{"creds":"` + home.real + `/auth.json"}`},
			{"composite `..` traversal out of the confined home", "--config=" + conf + "/../auth.json"},
		} {
			if got := dspec.sweepRoutes(probeReport{Args: []pathPair{{Raw: c.value}}}, nil); len(got) == 0 {
				t.Errorf("%s / %s: the sweep saw NO route in %q — this escape is unpinned", home.label, c.name, c.value)
			}
		}
	}

	// A composite whose EMBEDDED path is a SYMLINK resolving into the real home.
	// This one needs a real filesystem: EvalSymlinks over the WHOLE composite
	// never resolves (it is not a path), and the link only resolves once it has
	// been extracted from `--config=`. Eliding the confined home's text first
	// hides it completely — the link's own spelling is inside the exempt home.
	realDir := filepath.Join(t.TempDir(), ".codex")
	confinedDir := filepath.Join(realDir, "fishhawk-confined-abc")
	if err := os.MkdirAll(confinedDir, 0o700); err != nil {
		t.Fatalf("mkdir confined dir: %v", err)
	}
	realAuth := filepath.Join(realDir, "auth.json")
	if err := os.WriteFile(realAuth, []byte(probeAuthFixture), 0o600); err != nil {
		t.Fatalf("plant real auth.json: %v", err)
	}
	link := filepath.Join(confinedDir, "operator-auth-link")
	if err := os.Symlink(realAuth, link); err != nil {
		t.Fatalf("symlink into the real home: %v", err)
	}
	liveSpec := newRouteSpec(realDir, pathPair{Raw: confinedDir})

	// Control: the confined home's own real contents stay GREEN under the same
	// spec, so the case below cannot be green-by-accident on a spec that flags
	// everything.
	ownFile := filepath.Join(confinedDir, "config.toml")
	if err := os.WriteFile(ownFile, []byte("# confined\n"), 0o600); err != nil {
		t.Fatalf("plant confined config.toml: %v", err)
	}
	if got := liveSpec.sweepRoutes(probeReport{Args: []pathPair{{Raw: "--config=" + ownFile}}}, nil); len(got) != 0 {
		t.Errorf("a composite naming the confined home's OWN file produced findings (a FALSE RED): %v", got)
	}
	if got := liveSpec.sweepRoutes(probeReport{Args: []pathPair{{Raw: "--config=" + link}}}, nil); len(got) == 0 {
		t.Error("composite whose embedded path is a symlink resolving into the real home: the sweep saw NO route — this escape is unpinned")
	}
}
