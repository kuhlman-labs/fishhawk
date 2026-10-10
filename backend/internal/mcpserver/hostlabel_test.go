package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
)

// The test binary never resolves the REAL host label: NewServer-driven tests
// (server_test.go) reach the host-dispatch marker, and the production resolver
// would create a host-id file under the developer's real HOME. Config.internal
// still wires the processHostLabel variable, which is what
// TestConfigInternal_WiresLazyProcessHostLabel pins.
func init() {
	processHostLabel = fixedHostLabel("mcpserver-test-binary")
}

// fixedHostLabel is a resolver answering label (no label when empty), for the
// tests that only care what the marker sends.
func fixedHostLabel(label string) func() hostLabelResolution {
	return func() hostLabelResolution {
		if label == "" {
			return hostLabelResolution{Source: hostLabelSourceNone}
		}
		return hostLabelResolution{Label: label, Source: hostLabelSourceOverride}
	}
}

// testRandID is the injected random-id seam: a fixed, valid host id.
const testRandID = "host-0123456789abcdef"

// testHostLabelDeps resolves with no override, the given hostname, and the
// host-id file at path.
func testHostLabelDeps(hostname, path string) hostLabelDeps {
	return hostLabelDeps{
		getenv:   func(string) string { return "" },
		hostname: func() (string, error) { return hostname, nil },
		idPath:   func() (string, error) { return path, nil },
		randID:   func() (string, error) { return testRandID, nil },
	}
}

func freshHostIDPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state", "fishhawk", "host-id")
}

func writeHostIDFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestResolveHostLabel_OverrideWins: FISHHAWK_HOST_LABEL is sanitised and wins
// over a pre-seeded host-id file and the hostname, consulting neither.
func TestResolveHostLabel_OverrideWins(t *testing.T) {
	path := freshHostIDPath(t)
	writeHostIDFile(t, path, "other\n")
	d := hostLabelDeps{
		getenv: func(k string) string {
			if k == hostLabelEnv {
				return " pinned box "
			}
			return ""
		},
		hostname: func() (string, error) { t.Errorf("hostname consulted under an override"); return "machine", nil },
		idPath:   func() (string, error) { t.Errorf("idPath consulted under an override"); return path, nil },
		randID:   func() (string, error) { t.Errorf("randID consulted under an override"); return testRandID, nil },
	}
	got := resolveHostLabel(d)
	if got.Label != "pinned-box" || got.Source != hostLabelSourceOverride || got.Path != "" {
		t.Fatalf("resolution = %+v, want label pinned-box from the override", got)
	}
	if !strings.Contains(got.Note, "sanitised") || !strings.Contains(got.Note, hostLabelEnv) {
		t.Errorf("note = %q, want it to say the override was sanitised", got.Note)
	}
	if c := readFileString(t, path); c != "other\n" {
		t.Errorf("host-id file = %q, want it untouched", c)
	}

	// A clean override needs no sanitisation note.
	d.getenv = func(k string) string {
		if k == hostLabelEnv {
			return "pinned"
		}
		return ""
	}
	if got := resolveHostLabel(d); got.Label != "pinned" || got.Note != "Pinned by "+hostLabelEnv+"." {
		t.Errorf("clean override = %+v, want label pinned with the plain note", got)
	}
}

// TestResolveHostLabel_PersistedIDStableAcrossHostnames is the issue's
// acceptance check: two processes (fresh deps each) on one machine whose
// hostname changed in between (macOS following the network) resolve the SAME
// label, read from the one host-id file. The first creation is seeded from
// the then-current hostname, so rollout keeps today's group key.
func TestResolveHostLabel_PersistedIDStableAcrossHostnames(t *testing.T) {
	path := freshHostIDPath(t)
	first := resolveHostLabel(testHostLabelDeps("Bretts-MacBook-Pro.local", path))
	second := resolveHostLabel(testHostLabelDeps("Mac", path))
	if first.Source != hostLabelSourcePersisted || second.Source != hostLabelSourcePersisted {
		t.Fatalf("sources = %q/%q, want persisted for both", first.Source, second.Source)
	}
	if first.Label != "Bretts-MacBook-Pro.local" {
		t.Errorf("first label = %q, want the hostname seed Bretts-MacBook-Pro.local (rollout continuity)", first.Label)
	}
	if first.Label != second.Label {
		t.Fatalf("labels differ across a hostname change: %q vs %q", first.Label, second.Label)
	}
	if c := strings.TrimSpace(readFileString(t, path)); c != first.Label {
		t.Errorf("host-id file = %q, want the resolved label %q", c, first.Label)
	}
	if first.Path != path || second.Path != path {
		t.Errorf("paths = %q/%q, want %q", first.Path, second.Path, path)
	}
}

// TestReadOrCreateHostID_NeverOverwritesExisting: an existing id wins over the
// seed, which is never consulted, and the file is read back unchanged with no
// create attempted (no temp file).
func TestReadOrCreateHostID_NeverOverwritesExisting(t *testing.T) {
	path := freshHostIDPath(t)
	writeHostIDFile(t, path, "first\n")
	seeded := false
	got, err := readOrCreateHostID(path, func() string { seeded = true; return "second" })
	if err != nil || got != "first" {
		t.Fatalf("readOrCreateHostID = %q, %v; want first", got, err)
	}
	if seeded {
		t.Error("the seed was consulted for an existing host-id file, want it read without a create attempt")
	}
	if c := readFileString(t, path); c != "first\n" {
		t.Errorf("host-id file = %q, want it still first", c)
	}
	assertOnlyHostIDInDir(t, filepath.Dir(path))
}

// TestCreateHostIDExclusive_LosesToExistingFile: the exclusive create never
// clobbers a file another creator already linked; it returns that file's
// value and leaves no temp file behind.
func TestCreateHostIDExclusive_LosesToExistingFile(t *testing.T) {
	path := freshHostIDPath(t)
	writeHostIDFile(t, path, "winner\n")
	got, err := createHostIDExclusive(path, "loser")
	if err != nil || got != "winner" {
		t.Fatalf("createHostIDExclusive = %q, %v; want winner", got, err)
	}
	if c := readFileString(t, path); c != "winner\n" {
		t.Errorf("host-id file = %q, want winner unchanged", c)
	}
	assertOnlyHostIDInDir(t, filepath.Dir(path))
}

// TestCreateHostIDExclusive_LinkFailureIsAnError: a link failure other than
// EEXIST (here ENAMETOOLONG) is an error, and the temp file is still removed.
func TestCreateHostIDExclusive_LinkFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, strings.Repeat("x", 300))
	got, err := createHostIDExclusive(path, "seed")
	if err == nil || !strings.Contains(err.Error(), "link the host-id file") {
		t.Fatalf("createHostIDExclusive = %q, %v; want the link error", got, err)
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Errorf("dir entries = %v, want the temp file removed", entries)
	}
}

func assertOnlyHostIDInDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "host-id" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir entries = %v, want only host-id (no temp file left)", names)
	}
}

// TestReadOrCreateHostID_ConcurrentFirstCreateAgrees: concurrent first
// creators with distinct seeds all return one label, the one on disk. A
// supporting stress check for the exclusive create.
func TestReadOrCreateHostID_ConcurrentFirstCreateAgrees(t *testing.T) {
	path := freshHostIDPath(t)
	const n = 16
	labels := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			labels[i], errs[i] = readOrCreateHostID(path, func() string { return fmt.Sprintf("seed-%d", i) })
		}(i)
	}
	wg.Wait()
	onDisk := strings.TrimSpace(readFileString(t, path))
	for i := 0; i < n; i++ {
		if errs[i] != nil || labels[i] != onDisk {
			t.Errorf("creator %d = %q, %v; want the on-disk %q", i, labels[i], errs[i], onDisk)
		}
	}
	assertOnlyHostIDInDir(t, filepath.Dir(path))
}

// TestResolveHostLabel_FallsBackToHostname: every way the persisted rung can
// fail falls back to the sanitised hostname with a note naming the cause, and
// an existing file is never rewritten.
func TestResolveHostLabel_FallsBackToHostname(t *testing.T) {
	type setup func(t *testing.T) (deps hostLabelDeps, path, wantInNote, keepFile string)
	for name, fn := range map[string]setup{
		"idPath error": func(t *testing.T) (hostLabelDeps, string, string, string) {
			d := testHostLabelDeps("dev box", "")
			d.idPath = func() (string, error) { return "", errors.New("no home directory") }
			return d, "", "no home directory", ""
		},
		"parent path is a regular file": func(t *testing.T) (hostLabelDeps, string, string, string) {
			dir := t.TempDir()
			blocker := filepath.Join(dir, "state")
			if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(blocker, "fishhawk", "host-id")
			return testHostLabelDeps("dev box", path), path, path, ""
		},
		"directory cannot be created": func(t *testing.T) (hostLabelDeps, string, string, string) {
			dir := t.TempDir()
			link := filepath.Join(dir, "state")
			if err := os.Symlink(filepath.Join(dir, "missing", "target"), link); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(link, "host-id")
			return testHostLabelDeps("dev box", path), path, "create the host-id directory", ""
		},
		"empty host-id file": func(t *testing.T) (hostLabelDeps, string, string, string) {
			path := freshHostIDPath(t)
			writeHostIDFile(t, path, "  \n")
			return testHostLabelDeps("dev box", path), path, "delete it to regenerate", "  \n"
		},
		"malformed host-id file": func(t *testing.T) (hostLabelDeps, string, string, string) {
			path := freshHostIDPath(t)
			writeHostIDFile(t, path, "two words\n")
			return testHostLabelDeps("dev box", path), path, "delete it to regenerate", "two words\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, path, wantInNote, keepFile := fn(t)
			got := resolveHostLabel(d)
			if got.Label != "dev-box" || got.Source != hostLabelSourceHostname {
				t.Fatalf("resolution = %+v, want label dev-box from the hostname rung", got)
			}
			if !strings.Contains(got.Note, wantInNote) || !strings.Contains(got.Note, hostLabelEnv) {
				t.Errorf("note = %q, want it to name %q and %s", got.Note, wantInNote, hostLabelEnv)
			}
			if path != "" && !strings.Contains(got.Note, path) {
				t.Errorf("note = %q, want it to name the path %s", got.Note, path)
			}
			if got.Path != path {
				t.Errorf("path = %q, want %q", got.Path, path)
			}
			if keepFile != "" {
				if c := readFileString(t, path); c != keepFile {
					t.Errorf("host-id file = %q, want it left as %q", c, keepFile)
				}
			}
		})
	}
}

// TestResolveHostLabel_NoneWhenNothingResolves: no override, no usable
// host-id file and no hostname resolve no label (the server's unknown host).
func TestResolveHostLabel_NoneWhenNothingResolves(t *testing.T) {
	t.Run("idPath and hostname both fail", func(t *testing.T) {
		d := testHostLabelDeps("", "")
		d.hostname = func() (string, error) { return "", errors.New("no hostname") }
		d.idPath = func() (string, error) { return "", errors.New("no home directory") }
		got := resolveHostLabel(d)
		if got.Label != "" || got.Source != hostLabelSourceNone || !strings.Contains(got.Note, "no home directory") {
			t.Fatalf("resolution = %+v, want no label, source none, the cause named", got)
		}
	})
	t.Run("no hostname and no random id leaves no file", func(t *testing.T) {
		path := freshHostIDPath(t)
		d := testHostLabelDeps("", path)
		d.randID = func() (string, error) { return "host-must-not-be-used", errors.New("entropy unavailable") }
		got := resolveHostLabel(d)
		if got.Label != "" || got.Source != hostLabelSourceNone || !strings.Contains(got.Note, "no seed") {
			t.Fatalf("resolution = %+v, want no label, source none, the missing seed named", got)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("host-id file stat = %v, want it never created", err)
		}
	})
}

// TestResolveHostLabel_RandomSeedWhenNoHostname: with no hostname the host id
// is seeded from the random id, persisted, and reused.
func TestResolveHostLabel_RandomSeedWhenNoHostname(t *testing.T) {
	path := freshHostIDPath(t)
	d := testHostLabelDeps("", path)
	// A name returned beside an error is not a hostname.
	d.hostname = func() (string, error) { return "must-not-be-used", errors.New("no hostname") }
	got := resolveHostLabel(d)
	if got.Label != testRandID || got.Source != hostLabelSourcePersisted {
		t.Fatalf("resolution = %+v, want the random seed persisted", got)
	}
	if c := strings.TrimSpace(readFileString(t, path)); c != testRandID {
		t.Errorf("host-id file = %q, want %q", c, testRandID)
	}
}

// TestRandomHostID_Shape: the production random seed is host- plus 16 hex
// characters, inside the server's accepted label class.
func TestRandomHostID_Shape(t *testing.T) {
	id, err := randomHostID()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^host-[0-9a-f]{16}$`).MatchString(id) || sanitizeHostLabel(id) != id {
		t.Errorf("randomHostID = %q, want host-<16 hex>", id)
	}
}

// TestHostIDPath pins the path per environment.
func TestHostIDPath(t *testing.T) {
	cfgDir := func() (string, error) { return "/Users/op/Library/Application Support", nil }
	home := func() (string, error) { return "/home/op", nil }
	noHome := func() (string, error) { return "", errors.New("$HOME is not defined") }
	env := func(xdg string) func(string) string {
		return func(k string) string {
			if k == "XDG_STATE_HOME" {
				return xdg
			}
			return ""
		}
	}
	for _, tc := range []struct {
		name, xdg, goos    string
		cfgDir, home       func() (string, error)
		want, wantErrSubst string
	}{
		{"absolute XDG_STATE_HOME wins", "/xdg/state", "darwin", cfgDir, home, "/xdg/state/fishhawk/host-id", ""},
		{"relative XDG_STATE_HOME is ignored", "rel/state", "linux", cfgDir, home, "/home/op/.local/state/fishhawk/host-id", ""},
		{"darwin uses Application Support", "", "darwin", cfgDir, home, "/Users/op/Library/Application Support/fishhawk/host-id", ""},
		{"linux uses the XDG state default", "", "linux", cfgDir, home, "/home/op/.local/state/fishhawk/host-id", ""},
		{"linux without a home directory", "", "linux", cfgDir, noHome, "", "resolve the home directory"},
		{"darwin without a config directory", "", "darwin", noHome, home, "", "resolve the user config directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hostIDPath(env(tc.xdg), tc.goos, tc.cfgDir, tc.home)
			if tc.wantErrSubst != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSubst) {
					t.Fatalf("hostIDPath = %q, %v; want an error naming %q", got, err, tc.wantErrSubst)
				}
				return
			}
			if err != nil || got != filepath.FromSlash(tc.want) {
				t.Fatalf("hostIDPath = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestProductionHostLabelDeps_Wired: the production seams are the real process
// environment; checked without resolving (which would touch the real HOME).
func TestProductionHostLabelDeps_Wired(t *testing.T) {
	d := productionHostLabelDeps()
	if d.getenv == nil || d.hostname == nil || d.idPath == nil || d.randID == nil {
		t.Fatalf("production deps have a nil seam: %+v", d)
	}
	gotPath, gotErr := d.idPath()
	wantPath, wantErr := hostIDPath(os.Getenv, runtime.GOOS, os.UserConfigDir, os.UserHomeDir)
	if gotPath != wantPath || (gotErr == nil) != (wantErr == nil) {
		t.Errorf("idPath = %q, %v; want %q, %v", gotPath, gotErr, wantPath, wantErr)
	}
	if id, err := d.randID(); err != nil || !strings.HasPrefix(id, "host-") {
		t.Errorf("randID = %q, %v; want a host- id", id, err)
	}
}

// TestConfigInternal_WiresLazyProcessHostLabel: Config.internal wires the
// process resolver (so NewServer clients send a label), while a config{}
// literal carries none and sends no host body.
func TestConfigInternal_WiresLazyProcessHostLabel(t *testing.T) {
	c := Config{BackendURL: "http://x"}.internal()
	if c.hostLabel == nil {
		t.Fatal("Config.internal().hostLabel is nil, want the lazy processHostLabel")
	}
	if got := newAPIClient(c).hostLabelResolution().Label; got != processHostLabel().Label {
		t.Errorf("NewServer-path client label = %q, want the process label %q", got, processHostLabel().Label)
	}

	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"transitioned":true,"stage_state":"dispatched"}`))
	}))
	defer ts.Close()
	bare := newAPIClient(config{backendURL: ts.URL, apiToken: "tok"})
	if res := bare.hostLabelResolution(); res.Label != "" || res.Source != hostLabelSourceNone {
		t.Errorf("nil resolver = %+v, want no label, source none", res)
	}
	if _, err := bare.HostDispatchStage(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("HostDispatchStage: %v", err)
	}
	if gotBody != "" {
		t.Errorf("body = %q, want none from a config{} literal", gotBody)
	}
}

// TestNewServer_DoesNotResolveHostLabel pins the laziness: building the tool
// registry (fishhawkd's /mcp route does so per request) never resolves the
// label, so it never touches the filesystem.
func TestNewServer_DoesNotResolveHostLabel(t *testing.T) {
	saved := processHostLabel
	t.Cleanup(func() { processHostLabel = saved })
	calls := 0
	processHostLabel = func() hostLabelResolution {
		calls++
		return hostLabelResolution{Source: hostLabelSourceNone}
	}
	_ = NewServer(Config{BackendURL: "http://x", APIToken: "tok"})
	if calls != 0 {
		t.Errorf("NewServer resolved the host label %d times, want 0 (lazy)", calls)
	}
}

// TestDefaultGroupKeyParity pins the mirrored default-group constants to
// backend/internal/concurrency's.
func TestDefaultGroupKeyParity(t *testing.T) {
	for _, label := range []string{"h1", ""} {
		if got, want := defaultGroupKeyFor(label), concurrency.DefaultGroupKey(label); got != want {
			t.Errorf("defaultGroupKeyFor(%q) = %q, want concurrency.DefaultGroupKey's %q", label, got, want)
		}
	}
}
