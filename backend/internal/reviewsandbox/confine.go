package reviewsandbox

// Reviewer read-confinement (#2522). Env scrubs the child's ENVIRONMENT and
// ExportTree bounds what the reviewer is POINTED at; neither bounds what a
// tool-enabled reviewer can ASK the filesystem for. This file adds the two
// per-adapter read bounds, and they are NOT the same mechanism — the asymmetry
// is load-bearing and is never collapsed into one word in these comments, the
// README, or the docs:
//
//   - codex  → CodexConfinedHome synthesizes a throwaway CODEX_HOME holding a
//     `confined` permission profile. codex-cli enforces it as a deny-by-default
//     ALLOWLIST at the OS level (an out-of-tree read fails with EPERM,
//     "Operation not permitted" — not a model refusal). That is real
//     confinement.
//
//   - claude → ClaudeDenyRules emits `--disallowed-tools` rules over a FIXED set
//     of well-known credential roots. That is a BLOCKLIST enforced at the tool
//     layer, with named readable gaps: anything outside those roots (/opt, /srv,
//     a mounted volume) stays readable. It is defence-in-depth, NOT confinement,
//     and a CI non-vacuity control asserts the gap rather than papering over it.
//
// Why a fixed root list and not ancestor-sibling enumeration: ${TMPDIR} on the
// operator host holds 111,026 entries. Enumerating siblings at the two-rule-form
// x three-tool shape is ~666,000 rules and, at ~60 bytes each, an argv on the
// order of 40 MB against a macOS ARG_MAX of ~1 MiB — every grounded review would
// fail to spawn with E2BIG (execve(2)). So the rule set is deterministic and
// small (30 spellings x rules on darwin, 18 on linux) and a test asserts the
// exact count, so a future root addition is a deliberate edit rather than silent
// growth back toward that failure.
//
// Everything here is FAIL CLOSED: a guard that cannot decide returns a named
// error and the adapter FAILS the invocation rather than spawning a grounded
// reviewer with no bound. The long-form contract and the named residuals live in
// this package's README.md.

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Named failure modes. Each is REACHABLE through the shipped builders and each
// has one behavioural test in confine_test.go.
var (
	// errDenyRootUnresolvable: a deny root that EXISTS could not be
	// canonicalized. Emitting rules for the remaining roots would silently
	// unprotect this one, so rule generation fails instead.
	errDenyRootUnresolvable = errors.New("reviewsandbox: applicable deny root could not be canonicalized")

	// errDenyRuleMetacharacter: an emitted path spelling carries a character
	// that is significant inside a `Tool(//path)` rule. Such a rule can misparse
	// and silently widen or narrow the bound, so it is never emitted.
	errDenyRuleMetacharacter = errors.New("reviewsandbox: deny path contains a rule metacharacter")

	// errDenyOverlapsExport: a deny root component-wise covers the exported
	// tree, so the rules would blind the reviewer against the very tree it is
	// meant to read.
	errDenyOverlapsExport = errors.New("reviewsandbox: deny root covers the exported tree")

	// errConfinedHomeOutsideDeniedRoot: the resolved real CODEX_HOME lies
	// outside every applicable claude deny root, so a synthesized home placed
	// inside it would hold a copy of the operator's credential in a region the
	// claude blocklist does not cover.
	errConfinedHomeOutsideDeniedRoot = errors.New("reviewsandbox: CODEX_HOME resolves outside every applicable denied root")

	// errOperatorConfigDeclaresPermissions: the operator's own config.toml
	// already declares a permissions posture, so appending ours would emit
	// duplicate-key TOML or silently lose one of the two.
	errOperatorConfigDeclaresPermissions = errors.New("reviewsandbox: operator config.toml already declares permissions")

	// errCredentialLockUnavailable: the credential lock is held, so the
	// copy-back is skipped. Fail SAFE — the operator's on-disk bytes are left
	// exactly as they are.
	errCredentialLockUnavailable = errors.New("reviewsandbox: credential lock unavailable")
)

// HostPaths is the injectable host-filesystem seam both bounds are built from.
// It is a parameter rather than a set of direct os calls so every test in this
// package — and every grounded-path test in the two adapter packages — runs
// against temp dirs and NEVER reads, writes, or litters the operator's real
// CODEX_HOME (a plain `go test` must not touch a live credential).
type HostPaths struct {
	// HomeDir is the operator's home directory. It is the primary claude deny
	// root: ~/.claude, ~/.codex, ~/.aws, ~/.ssh all live under it.
	HomeDir string
	// CodexHome is the real CODEX_HOME the synthesized confined home is placed
	// INSIDE, so the copied credential never leaves a claude-denied root.
	CodexHome string
	// TempDir is the host scratch root. Recorded so tests can assert the
	// non-vacuity control (a path under it is matched by NO deny rule).
	TempDir string
	// Canonical resolves a path through symlinks. Load-bearing at every site:
	// os.MkdirTemp on darwin returns /var/folders/... whose real path is
	// /private/var/folders/..., and an un-canonicalized codex grant would not
	// cover the child's actual working directory — BLINDING the reviewer
	// silently rather than failing loudly. A nil Canonical defaults to
	// filepath.EvalSymlinks.
	Canonical func(string) (string, error)

	// duringCredentialCopyBack is a TEST-ONLY hook fired inside the credential
	// lock, after the source hash is re-verified and before the rename. It is
	// how the copy-back's mutual exclusion and its named residual are asserted
	// behaviourally rather than reasoned about. Always nil in production.
	duringCredentialCopyBack func()
}

// DefaultHostPaths reads the real host locations. Adapters use it only when no
// HostPaths seam was injected.
func DefaultHostPaths() HostPaths {
	hp := HostPaths{TempDir: os.TempDir(), Canonical: filepath.EvalSymlinks}
	if home, err := os.UserHomeDir(); err == nil {
		hp.HomeDir = home
	}
	if ch := os.Getenv("CODEX_HOME"); ch != "" {
		hp.CodexHome = ch
	} else if hp.HomeDir != "" {
		hp.CodexHome = filepath.Join(hp.HomeDir, ".codex")
	}
	return hp
}

// canonical applies hp.Canonical, defaulting to filepath.EvalSymlinks.
func (hp HostPaths) canonical(path string) (string, error) {
	fn := hp.Canonical
	if fn == nil {
		fn = filepath.EvalSymlinks
	}
	return fn(path)
}

// platformDenyRoots is the FIXED candidate deny-root list for goos. It is pure
// and takes goos as a parameter (not runtime.GOOS inline) so BOTH platform
// tables are exercisable from either host.
//
// Universal: the operator home (~/.claude, ~/.codex, ~/.aws, ~/.ssh) and /etc.
// darwin adds /private/etc, /var/root and /private/var/root — measured on the
// operator host, /etc canonicalizes to /private/etc and /var/root to
// /private/var/root, and /root is ABSENT. linux adds /root, where the two
// /private forms do not exist.
func platformDenyRoots(hp HostPaths, goos string) []string {
	roots := []string{hp.HomeDir, "/etc"}
	switch goos {
	case "darwin":
		roots = append(roots, "/private/etc", "/var/root", "/private/var/root")
	case "linux":
		roots = append(roots, "/root")
	}
	return roots
}

// ClaudeDenyRules builds the claudecode `--disallowed-tools` rule set: a BOUNDED
// static credential-root BLOCKLIST, never called confinement. exportDir is the
// tree the reviewer must still be able to read; goos selects the platform table.
func ClaudeDenyRules(hp HostPaths, exportDir, goos string) ([]string, error) {
	return claudeDenyRules(hp, exportDir, platformDenyRoots(hp, goos))
}

// claudeDenyRules is ClaudeDenyRules over an explicit candidate list, so tests
// drive both platform tables and every guard hermetically.
func claudeDenyRules(hp HostPaths, exportDir string, candidates []string) ([]string, error) {
	canonExport, err := hp.canonical(exportDir)
	if err != nil {
		return nil, fmt.Errorf("reviewsandbox: canonicalize export dir %q: %w", exportDir, err)
	}

	spellings, err := denySpellings(hp, canonExport, candidates)
	if err != nil {
		return nil, err
	}

	// Two rule FORMS per spelling. The `/**` form covers everything beneath a
	// denied directory; the BARE form covers a plain FILE sitting AT the denied
	// path (a deny root that is itself a file, e.g. a credential file named as a
	// root) which `/**` alone does not match.
	rules := make([]string, 0, len(spellings)*6)
	for _, p := range spellings {
		// The `//` prefix is the CLI's absolute-path marker and the path follows
		// WITHOUT its own leading separator: the operator-verified shape is
		// `Read(//Users/**)` for /Users, not `Read(///Users/**)`.
		abs := "//" + strings.TrimPrefix(p, "/")
		for _, tool := range []string{"Read", "Grep", "Glob"} {
			rules = append(rules, tool+"("+abs+")", tool+"("+abs+"/**)")
		}
	}
	return rules, nil
}

// denySpellings resolves candidates into the deduped, ordered set of path
// spellings to emit rules for, applying every guard.
//
// A candidate that does not EXIST is skipped silently (nothing to protect, rules
// are still produced for the rest). A candidate that exists but cannot be
// canonicalized FAILS CLOSED.
//
// The emitted set is the UNION of the RAW and CANONICAL spellings, because a
// deny rule matches the path as the reviewer ASKS for it: on darwin a read of
// /etc/passwd is not covered by a rule naming only /private/etc. Both spellings
// then go through the SAME metacharacter guard — a raw HOME can carry
// whitespace, a parenthesis, a comma or an asterisk while canonicalizing to a
// perfectly clean target, and that unchecked raw spelling would be the one that
// misparses inside --disallowed-tools.
func denySpellings(hp HostPaths, canonExport string, candidates []string) ([]string, error) {
	var out []string
	seen := make(map[string]struct{}, len(candidates)*2)

	for _, raw := range candidates {
		if raw == "" {
			// An unresolvable HOME (os.UserHomeDir failed) must not silently
			// drop the primary credential root.
			return nil, fmt.Errorf("%w: empty deny root", errDenyRootUnresolvable)
		}
		canon, err := hp.canonical(raw)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Not present on this host — nothing to deny.
				continue
			}
			return nil, fmt.Errorf("%w: %q: %v", errDenyRootUnresolvable, raw, err)
		}
		if pathCovers(canon, canonExport) {
			return nil, fmt.Errorf("%w: %q covers %q", errDenyOverlapsExport, canon, canonExport)
		}
		for _, spelling := range []string{raw, canon} {
			if err := guardRuleMetacharacters(spelling); err != nil {
				return nil, err
			}
			if _, dup := seen[spelling]; dup {
				continue
			}
			seen[spelling] = struct{}{}
			out = append(out, spelling)
		}
	}
	return out, nil
}

// ruleMetacharacters are the characters that are significant inside a
// `Tool(//path)` rule or inside the argv element carrying it. A path containing
// one cannot be expressed as a rule unambiguously, so it FAILS CLOSED rather
// than emitting something that may misparse into a wider or narrower bound.
const ruleMetacharacters = "()*,?[]{}!\t\n\r "

// guardRuleMetacharacters rejects a path spelling that cannot be expressed as an
// unambiguous rule. Applied to EVERY emitted spelling — raw and canonical alike.
func guardRuleMetacharacters(p string) error {
	if i := strings.IndexAny(p, ruleMetacharacters); i >= 0 {
		return fmt.Errorf("%w: %q contains %q", errDenyRuleMetacharacter, p, string(p[i]))
	}
	return nil
}

// pathCovers reports whether root is candidate or an ancestor of it, comparing
// PATH COMPONENTS rather than string prefixes. A string prefix test would report
// that /var/root covers /var/rootless, which it does not.
func pathCovers(root, candidate string) bool {
	if root == candidate {
		return true
	}
	rc := splitPath(root)
	cc := splitPath(candidate)
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

func splitPath(p string) []string {
	cleaned := filepath.Clean(p)
	trimmed := strings.Trim(cleaned, string(filepath.Separator))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, string(filepath.Separator))
}

// ConfinedProfileName is the codex permission-profile name. It is the single
// source of truth for both halves of the pairing that makes confinement bind:
// the profile TOML written into the synthesized home, and the `--profile` flag
// the codex adapter appends. A drift between the two would leave the profile
// unselected and the reviewer unconfined with no error.
const ConfinedProfileName = "confined"

// confinedProfileName is the in-package spelling.
const confinedProfileName = ConfinedProfileName

// CodexConfinedHome synthesizes a throwaway CODEX_HOME carrying a `confined`
// codex permission profile and returns its path plus a cleanup func.
//
// PLACEMENT is the first thing it does and the reason the function is shaped
// this way. The synthesized home holds a COPY of the operator's auth.json, so
// putting it under ${TMPDIR} — as a bare os.MkdirTemp("") would — moves a live
// OpenAI credential out of a claude-denied root into the one region the claude
// blocklist deliberately does not cover. Instead it is created INSIDE the
// resolved real CODEX_HOME, and the function REFUSES outright
// (errConfinedHomeOutsideDeniedRoot) when that resolves outside every applicable
// deny root. An operator with CODEX_HOME=/tmp/foo gets a loud refusal, never a
// silent egress path.
//
// The GRANT deliberately does NOT include the synthesized home. codex-cli reads
// its own CODEX_HOME config and auth as the PROCESS, outside the profile's tool
// layer (operator-verified against 0.144.1), so authentication survives while
// the copied credential stays OUTSIDE the reviewer-readable set. That separation
// is the whole point: a prompt-injected diff steering the reviewer to read a
// credential into its verdict — which then lands in the audit log and PR
// comments — is the egress path this bound exists to close.
//
// cleanup ALWAYS removes the synthesized directory. It returns a non-nil error
// only as a WARNING (a skipped credential copy-back); the caller logs it and
// does not fail the invocation on it.
func CodexConfinedHome(hp HostPaths, exportDir, schemaPath, goos string) (home string, cleanup func() error, err error) {
	if hp.CodexHome == "" {
		return "", nil, fmt.Errorf("%w: CODEX_HOME is empty", errConfinedHomeOutsideDeniedRoot)
	}
	if mkErr := os.MkdirAll(hp.CodexHome, 0o700); mkErr != nil {
		return "", nil, fmt.Errorf("reviewsandbox: create CODEX_HOME %q: %w", hp.CodexHome, mkErr)
	}
	realCodexHome, err := hp.canonical(hp.CodexHome)
	if err != nil {
		return "", nil, fmt.Errorf("reviewsandbox: canonicalize CODEX_HOME %q: %w", hp.CodexHome, err)
	}

	// Placement guard: the real CODEX_HOME must sit under a root the claude
	// blocklist actually denies.
	covered := false
	for _, root := range platformDenyRoots(hp, goos) {
		if root == "" {
			continue
		}
		canonRoot, cErr := hp.canonical(root)
		if cErr != nil {
			continue // absent or unresolvable roots cannot vouch for placement
		}
		if pathCovers(canonRoot, realCodexHome) {
			covered = true
			break
		}
	}
	if !covered {
		return "", nil, fmt.Errorf("%w: %q", errConfinedHomeOutsideDeniedRoot, realCodexHome)
	}

	canonExport, err := hp.canonical(exportDir)
	if err != nil {
		return "", nil, fmt.Errorf("reviewsandbox: canonicalize export dir %q: %w", exportDir, err)
	}
	// An empty schemaPath means "no schema grant" (the codex adapter always
	// passes one; the seam allows omitting it). Canonicalizing "" would error, so
	// the empty case is skipped explicitly rather than failing on a path the
	// caller deliberately did not supply.
	var canonSchema string
	if schemaPath != "" {
		canonSchema, err = hp.canonical(schemaPath)
		if err != nil {
			return "", nil, fmt.Errorf("reviewsandbox: canonicalize schema path %q: %w", schemaPath, err)
		}
	}

	// The operator's own config.toml is the BASE: it carries model_provider,
	// base_url and model, and a review that silently dropped them would talk to
	// the wrong endpoint. It is copied VERBATIM — no TOML surgery on the
	// operator's file.
	operatorConfig, err := os.ReadFile(filepath.Join(realCodexHome, "config.toml"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("reviewsandbox: read operator config.toml: %w", err)
	}
	if err := guardOperatorConfig(operatorConfig); err != nil {
		return "", nil, err
	}

	dir, err := os.MkdirTemp(realCodexHome, "fishhawk-confined-")
	if err != nil {
		return "", nil, fmt.Errorf("reviewsandbox: create confined CODEX_HOME: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, fmt.Errorf("reviewsandbox: chmod confined CODEX_HOME: %w", err)
	}

	// Write the permission block into BOTH files. codex-cli 0.147.0's
	// `codex exec --help` documents `-p/--profile <CONFIG_PROFILE_V2>` as "Layer
	// $CODEX_HOME/<name>.config.toml on top of the base user config", but the
	// operator's live verification was against 0.144.1 with the block sitting in
	// config.toml itself. Writing both makes confinement apply under EITHER
	// reading rather than betting on one.
	block := confinedPermissionBlock(canonExport, canonSchema)
	base := composeBaseConfig(operatorConfig, block)
	if err := writeConfined(dir, base, block); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}

	authSrc := filepath.Join(realCodexHome, "auth.json")
	snapshot, copied, err := copyCredentialIn(authSrc, filepath.Join(dir, "auth.json"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}

	cleanup = func() error {
		var warn error
		if copied {
			warn = copyCredentialBack(hp, dir, authSrc, snapshot)
		}
		if rmErr := os.RemoveAll(dir); rmErr != nil && warn == nil {
			warn = fmt.Errorf("reviewsandbox: remove confined CODEX_HOME: %w", rmErr)
		}
		return warn
	}
	return dir, cleanup, nil
}

// guardOperatorConfig refuses an operator config that already declares a
// permissions posture: appending ours would emit duplicate-key TOML (which codex
// rejects) or silently lose one of the two declarations — and a silently lost
// confinement is exactly the unbounded-reviewer state this closes.
func guardOperatorConfig(cfg []byte) error {
	for _, line := range strings.Split(string(cfg), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "default_permissions") {
			return fmt.Errorf("%w: declares default_permissions", errOperatorConfigDeclaresPermissions)
		}
		if strings.HasPrefix(t, "[permissions."+confinedProfileName) {
			return fmt.Errorf("%w: declares a [permissions.%s] table", errOperatorConfigDeclaresPermissions, confinedProfileName)
		}
	}
	return nil
}

// confinedPermissionBlock renders the operator-verified profile TOML
// (codex-cli 0.144.1). ":minimal" is the base system read set — WITHOUT it every
// shell command aborts with SIGABRT/134 because /bin/zsh and its dylibs become
// unreadable — and it deliberately excludes $HOME, so the operator's home is
// out by DEFAULT rather than by enumeration.
//
// The synthesized home is NOT granted. The reviewer's tool layer therefore
// cannot read the copied auth.json, while the codex process still reads it for
// authentication.
func confinedPermissionBlock(canonExport, canonSchema string) string {
	var b strings.Builder
	b.WriteString("default_permissions = \"" + confinedProfileName + "\"\n")
	b.WriteString("\n[permissions." + confinedProfileName + ".filesystem]\n")
	b.WriteString("\":minimal\" = \"read\"\n")
	b.WriteString(tomlQuote(canonExport) + " = \"read\"\n")
	if canonSchema != "" && canonSchema != canonExport {
		b.WriteString(tomlQuote(canonSchema) + " = \"read\"\n")
	}
	return b.String()
}

// composeBaseConfig prepends the block's top-level key and appends its table to
// the operator's verbatim config. TOML requires every top-level key to precede
// the first table header, and a table to follow the keys it does not belong to —
// so the block is SPLIT rather than pasted whole.
func composeBaseConfig(operatorConfig []byte, block string) string {
	head, table, ok := strings.Cut(block, "\n[")
	if !ok {
		return string(operatorConfig) + "\n" + block
	}
	body := strings.TrimRight(string(operatorConfig), "\n")
	if body != "" {
		body += "\n"
	}
	return head + "\n" + body + "\n[" + table
}

func writeConfined(dir, base, block string) error {
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(base), 0o600); err != nil {
		return fmt.Errorf("reviewsandbox: write confined config.toml: %w", err)
	}
	profile := filepath.Join(dir, confinedProfileName+".config.toml")
	if err := os.WriteFile(profile, []byte(block), 0o600); err != nil {
		return fmt.Errorf("reviewsandbox: write %s: %w", filepath.Base(profile), err)
	}
	return nil
}

// copyCredentialIn copies the operator's auth.json into the synthesized home at
// 0600 and returns the SOURCE content hash, which gates the copy-back. A missing
// auth.json is NOT fatal — an OPENAI_API_KEY deployment has none.
func copyCredentialIn(src, dst string) (snapshot [32]byte, copied bool, err error) {
	data, rerr := os.ReadFile(src)
	if rerr != nil {
		if errors.Is(rerr, fs.ErrNotExist) {
			return snapshot, false, nil
		}
		return snapshot, false, fmt.Errorf("reviewsandbox: read auth.json: %w", rerr)
	}
	if werr := os.WriteFile(dst, data, 0o600); werr != nil {
		return snapshot, false, fmt.Errorf("reviewsandbox: write confined auth.json: %w", werr)
	}
	return sha256.Sum256(data), true, nil
}

// credentialLockRetries / credentialLockDelay bound the wait for the credential
// lock. Exhausting them SKIPS the copy-back — fail SAFE, the operator's on-disk
// bytes are left untouched.
const (
	credentialLockRetries = 50
	credentialLockDelay   = 20 * time.Millisecond
)

// withCredentialLock runs fn while holding an exclusive O_EXCL lock file beside
// the credential, so two concurrent grounded reviews cannot interleave their
// copy-backs. It is ADVISORY: it excludes other Fishhawk invocations, not an
// external writer (codex-cli itself refreshing auth.json) that does not take it.
func withCredentialLock(lockPath string, fn func() error) error {
	for i := 0; i < credentialLockRetries; i++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			defer func() { _ = os.Remove(lockPath) }()
			return fn()
		}
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %v", errCredentialLockUnavailable, err)
		}
		time.Sleep(credentialLockDelay)
	}
	return fmt.Errorf("%w: %q held", errCredentialLockUnavailable, lockPath)
}

// copyCredentialBack returns a refreshed credential to the operator's real
// auth.json, under the lock, gated on the SOURCE still hashing to the snapshot
// taken at copy-in, and via a same-directory temp file plus rename.
//
// What that DOES guarantee: an independently newer credential written by another
// LOCK-RESPECTING Fishhawk invocation is never clobbered — the re-verify happens
// INSIDE the lock, so a peer either wrote before we hashed (mismatch → skip) or
// is excluded until we finish.
//
// What it does NOT guarantee, stated plainly rather than asserted away: POSIX
// offers no compare-and-swap on a file. An EXTERNAL writer that does not take
// this lock — codex-cli refreshing the credential itself — can land between the
// re-verify and the rename and be overwritten. The window is narrow (a hash
// compare and a rename) but real, and it is the named residual in README.md.
func copyCredentialBack(hp HostPaths, dir, src string, snapshot [32]byte) error {
	confined := filepath.Join(dir, "auth.json")
	newData, err := os.ReadFile(confined)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("reviewsandbox: read confined auth.json: %w", err)
	}
	if sha256.Sum256(newData) == snapshot {
		return nil // unchanged; nothing to write back
	}

	return withCredentialLock(src+".fishhawk-lock", func() error {
		cur, rerr := os.ReadFile(src)
		if rerr != nil {
			return fmt.Errorf("reviewsandbox: skipped credential copy-back: re-read source: %w", rerr)
		}
		if sha256.Sum256(cur) != snapshot {
			return fmt.Errorf("reviewsandbox: skipped credential copy-back: %q changed since copy-in", src)
		}
		if hp.duringCredentialCopyBack != nil {
			hp.duringCredentialCopyBack()
		}
		tmp, terr := os.CreateTemp(filepath.Dir(src), "auth.json.fishhawk-")
		if terr != nil {
			return fmt.Errorf("reviewsandbox: skipped credential copy-back: %w", terr)
		}
		tmpName := tmp.Name()
		if _, werr := tmp.Write(newData); werr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
			return fmt.Errorf("reviewsandbox: skipped credential copy-back: %w", werr)
		}
		if cerr := tmp.Close(); cerr != nil {
			_ = os.Remove(tmpName)
			return fmt.Errorf("reviewsandbox: skipped credential copy-back: %w", cerr)
		}
		if cerr := os.Chmod(tmpName, 0o600); cerr != nil {
			_ = os.Remove(tmpName)
			return fmt.Errorf("reviewsandbox: skipped credential copy-back: %w", cerr)
		}
		if rerr := os.Rename(tmpName, src); rerr != nil {
			_ = os.Remove(tmpName)
			return fmt.Errorf("reviewsandbox: skipped credential copy-back: %w", rerr)
		}
		return nil
	})
}

// tomlQuote renders a basic TOML string. Paths carrying a quote or backslash are
// escaped rather than emitted raw, so a hostile-looking path cannot break out of
// the key and inject another table.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
