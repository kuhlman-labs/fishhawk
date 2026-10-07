package gateiso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// credCanary is a credential an operator config or a helper answer carries;
// it must never appear in the runner-owned config or in a probe result.
const credCanary = "FH-CRED-CANARY-4046"

func writeModeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// TestNewAnonymousDockerConfig_Content: the runner-owned config is a 0700 dir
// whose 0600 config.json holds ONLY an empty auths map and the plugin dirs —
// no credsStore, no credHelpers — and Remove deletes it.
func TestNewAnonymousDockerConfig_Content(t *testing.T) {
	cfg, err := NewAnonymousDockerConfig([]string{"/op/.docker/cli-plugins", "/Applications/Docker.app/Contents/Resources/cli-plugins"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cfg.Remove() })
	if cfg.Credentials != CredentialsAnonymous || !cfg.Owned() || !filepath.IsAbs(cfg.Dir) {
		t.Fatalf("cfg = %+v, want an owned, absolute anonymous config", cfg)
	}
	if info, err := os.Stat(cfg.Dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v (%v), want 0700", info.Mode().Perm(), err)
	}
	info, err := os.Stat(cfg.ConfigFile())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config.json mode = %v (%v), want 0600", info.Mode().Perm(), err)
	}
	b, _ := os.ReadFile(cfg.ConfigFile())
	if want := `{"auths":{},"cliPluginsExtraDirs":["/op/.docker/cli-plugins","/Applications/Docker.app/Contents/Resources/cli-plugins"]}`; string(b) != want {
		t.Errorf("config.json = %s, want %s", b, want)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"credsStore", "credHelpers"} {
		if _, ok := keys[k]; ok {
			t.Errorf("anonymous config carries %s", k)
		}
	}
	if err := cfg.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.Dir); !os.IsNotExist(err) {
		t.Errorf("Remove left the owned dir: %v", err)
	}
}

// TestOperatorPluginDirs (#4046 approval condition 3): the plugin dirs are
// read from the inherited DOCKER_CONFIG's config.json, else
// $HOME/.docker/config.json — only cliPluginsExtraDirs, absolute entries
// only, always preceded by that dir's cli-plugins. An absent or malformed
// file still mints the anonymous config with just the cli-plugins dir, and no
// credential from the operator's file ever reaches the minted one.
func TestOperatorPluginDirs(t *testing.T) {
	operatorFile := `{"auths":{"ghcr.io":{"auth":"` + credCanary + `"}},"credsStore":"desktop","credHelpers":{"ghcr.io":"gh"},` +
		`"cliPluginsExtraDirs":["/Applications/Docker.app/Contents/Resources/cli-plugins","relative/plugins"]}`
	rows := []struct {
		name  string
		setup func(t *testing.T) (getenv map[string]string, want []string)
	}{
		{"inherited DOCKER_CONFIG wins over HOME", func(t *testing.T) (map[string]string, []string) {
			dc, home := t.TempDir(), t.TempDir()
			writeModeFile(t, filepath.Join(dc, "config.json"), operatorFile, 0o600)
			writeModeFile(t, filepath.Join(home, ".docker", "config.json"), `{"cliPluginsExtraDirs":["/from/home"]}`, 0o600)
			return map[string]string{"DOCKER_CONFIG": dc, "HOME": home},
				[]string{filepath.Join(dc, "cli-plugins"), "/Applications/Docker.app/Contents/Resources/cli-plugins"}
		}},
		{"HOME/.docker when DOCKER_CONFIG is unset", func(t *testing.T) (map[string]string, []string) {
			home := t.TempDir()
			writeModeFile(t, filepath.Join(home, ".docker", "config.json"), operatorFile, 0o600)
			return map[string]string{"HOME": home},
				[]string{filepath.Join(home, ".docker", "cli-plugins"), "/Applications/Docker.app/Contents/Resources/cli-plugins"}
		}},
		{"absent file: just the cli-plugins dir", func(t *testing.T) (map[string]string, []string) {
			home := t.TempDir()
			return map[string]string{"HOME": home}, []string{filepath.Join(home, ".docker", "cli-plugins")}
		}},
		{"malformed file: just the cli-plugins dir", func(t *testing.T) (map[string]string, []string) {
			dc := t.TempDir()
			writeModeFile(t, filepath.Join(dc, "config.json"), `{"cliPluginsExtraDirs": [`, 0o600)
			return map[string]string{"DOCKER_CONFIG": dc}, []string{filepath.Join(dc, "cli-plugins")}
		}},
		{"wrong-typed key: just the cli-plugins dir", func(t *testing.T) (map[string]string, []string) {
			dc := t.TempDir()
			writeModeFile(t, filepath.Join(dc, "config.json"), `{"cliPluginsExtraDirs":"/not/a/list"}`, 0o600)
			return map[string]string{"DOCKER_CONFIG": dc}, []string{filepath.Join(dc, "cli-plugins")}
		}},
		{"no DOCKER_CONFIG and no HOME", func(t *testing.T) (map[string]string, []string) {
			return map[string]string{}, nil
		}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			env, want := r.setup(t)
			got := OperatorPluginDirs(func(k string) string { return env[k] })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("OperatorPluginDirs = %q, want %q", got, want)
			}
			cfg, err := NewAnonymousDockerConfig(got)
			if err != nil {
				t.Fatalf("an operator config this shape must still mint the anonymous config: %v", err)
			}
			t.Cleanup(func() { _ = cfg.Remove() })
			b, _ := os.ReadFile(cfg.ConfigFile())
			for _, leak := range []string{credCanary, "credsStore", "credHelpers", "desktop"} {
				if strings.Contains(string(b), leak) {
					t.Errorf("minted config carries %q from the operator file: %s", leak, b)
				}
			}
		})
	}
}

// TestLoadOperatorDockerConfig_Defects: one row per startup defect; each
// wraps ErrDockerConfig and names the cause.
func TestLoadOperatorDockerConfig_Defects(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	writeModeFile(t, file, "x", 0o600)
	mk := func(name, content string) string {
		dir := filepath.Join(base, name)
		writeModeFile(t, filepath.Join(dir, "config.json"), content, 0o600)
		return dir
	}
	noConfig := filepath.Join(base, "empty")
	if err := os.MkdirAll(noConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		name, dir, want string
	}{
		{"relative path", "relative/cfg", "not an absolute path"},
		{"missing dir", filepath.Join(base, "missing"), "no such file"},
		{"not a directory", file, "is not a directory"},
		{"no config.json", noConfig, "no readable config.json"},
		{"top level is an array", mk("array", `["a"]`), "not a JSON object"},
		{"top level is null", mk("null", `null`), "not a JSON object"},
		{"malformed JSON", mk("malformed", `{"credsStore":`), "unexpected end of JSON"},
		{"credsStore not a string", mk("storetype", `{"credsStore":7}`), "cannot unmarshal"},
		{"credsStore path traversal", mk("storename", `{"credsStore":"../../bin/sh"}`), `credsStore "../../bin/sh" is not a helper name`},
		{"credHelpers value with a space", mk("helpername", `{"credHelpers":{"ghcr.io":"gh; rm"}}`), `credHelpers["ghcr.io"] = "gh; rm" is not a helper name`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			cfg, err := LoadOperatorDockerConfig(r.dir)
			if !errors.Is(err, ErrDockerConfig) || !strings.Contains(err.Error(), r.want) {
				t.Fatalf("err = %v, want ErrDockerConfig naming %q", err, r.want)
			}
			if !reflect.DeepEqual(cfg, DockerConfig{}) {
				t.Errorf("a defect returned a config: %+v", cfg)
			}
		})
	}
}

// TestLoadOperatorDockerConfig_ValidIsNeverRemoved: a valid operator config is
// operator_config, not owned, and Remove leaves it in place.
func TestLoadOperatorDockerConfig_ValidIsNeverRemoved(t *testing.T) {
	dir := t.TempDir()
	writeModeFile(t, filepath.Join(dir, "config.json"), `{"auths":{},"credsStore":"desktop","credHelpers":{"ghcr.io":"gh","x.io":""}}`, 0o600)
	cfg, err := LoadOperatorDockerConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dir != dir || cfg.Credentials != CredentialsOperatorConfig || cfg.Owned() {
		t.Fatalf("cfg = %+v", cfg)
	}
	if err := cfg.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.ConfigFile()); err != nil {
		t.Fatalf("Remove deleted an operator config: %v", err)
	}
	if err := (DockerConfig{}).Remove(); err != nil {
		t.Errorf("zero-value Remove = %v, want a no-op", err)
	}
}

// TestProbesFor mirrors the docker CLI's store choice: credHelpers[server]
// when the key exists (an empty value is the file store), else credsStore;
// Docker Hub's key is https://index.docker.io/v1/; a pair no image matches is
// never returned.
func TestProbesFor(t *testing.T) {
	load := func(content string) DockerConfig {
		dir := t.TempDir()
		writeModeFile(t, filepath.Join(dir, "config.json"), content, 0o600)
		cfg, err := LoadOperatorDockerConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	anon, err := NewAnonymousDockerConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = anon.Remove() })
	ecr := "123456789012.dkr.ecr.us-east-1.amazonaws.com"
	rows := []struct {
		name   string
		cfg    DockerConfig
		images []string
		want   []CredentialProbe
	}{
		{"credsStore for docker hub", load(`{"credsStore":"desktop"}`), []string{"alpine:3.20", "docker.io/library/postgres:16"},
			[]CredentialProbe{{Helper: "desktop", Server: "https://index.docker.io/v1/"}}},
		{"credHelpers beats credsStore", load(`{"credsStore":"desktop","credHelpers":{"ghcr.io":"gh"}}`), []string{"ghcr.io/o/g:v1", "alpine"},
			[]CredentialProbe{{Helper: "gh", Server: "ghcr.io"}, {Helper: "desktop", Server: "https://index.docker.io/v1/"}}},
		{"unmatched ecr-login is never probed", load(`{"credHelpers":{"` + ecr + `":"ecr-login"}}`), []string{"ghcr.io/o/g:v1", "alpine"}, nil},
		{"matched ecr-login is probed", load(`{"credHelpers":{"` + ecr + `":"ecr-login"}}`), []string{ecr + "/gate:v1"},
			[]CredentialProbe{{Helper: "ecr-login", Server: ecr}}},
		{"empty credHelpers value selects the file store", load(`{"credsStore":"desktop","credHelpers":{"ghcr.io":""}}`), []string{"ghcr.io/o/g:v1"}, nil},
		{"no store at all", load(`{"auths":{}}`), []string{"alpine"}, nil},
		{"unparsable image skipped", load(`{"credsStore":"desktop"}`), []string{"Not/A/Valid:Ref!"}, nil},
		{"anonymous config probes nothing", anon, []string{"alpine", "ghcr.io/o/g:v1"}, nil},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if got := r.cfg.ProbesFor(r.images); !reflect.DeepEqual(got, r.want) {
				t.Fatalf("ProbesFor = %+v, want %+v", got, r.want)
			}
		})
	}
}

// helperDir writes docker-credential-<name> scripts into a fresh dir and
// returns the dir and a probe env whose PATH resolves them (plus the system
// dirs the scripts' own commands need).
func helperDir(t *testing.T, scripts map[string]string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		writeModeFile(t, filepath.Join(dir, "docker-credential-"+name), "#!/bin/sh\n"+body, 0o755)
	}
	return dir, []string{"PATH=" + dir + ":/bin:/usr/bin", "FH_PROBE_DIR=" + dir}
}

// TestProbeCredentialHelper_Outcomes: an answering helper (exit 0 or a
// "not found" exit 1) is answered and receives the server on stdin; a helper
// absent from the env's PATH is missing; a non-executable one is missing. No
// result ever carries the helper's output (the canary).
func TestProbeCredentialHelper_Outcomes(t *testing.T) {
	dir, env := helperDir(t, map[string]string{
		"ok":       `cat > "$FH_PROBE_DIR/ok.stdin"; echo '{"Username":"u","Secret":"` + credCanary + `"}'; echo ` + credCanary + ` >&2`,
		"notfound": `echo "credentials not found in native keychain"; exit 1`,
	})
	writeModeFile(t, filepath.Join(dir, "docker-credential-noexec"), "#!/bin/sh\nexit 0\n", 0o644)
	rows := []struct {
		helper string
		want   ProbeOutcome
		detail string
	}{
		{"ok", ProbeAnswered, ""},
		{"notfound", ProbeAnswered, ""},
		{"absent", ProbeMissing, "docker-credential-absent not found on the runtime CLI's PATH"},
		{"noexec", ProbeMissing, "docker-credential-noexec not found"},
	}
	for _, r := range rows {
		t.Run(r.helper, func(t *testing.T) {
			res := ProbeCredentialHelper(context.Background(), CredentialProbe{Helper: r.helper, Server: "ghcr.io"}, env, 10*time.Second)
			if res.Outcome != r.want || !strings.Contains(res.Detail, r.detail) {
				t.Fatalf("result = %+v, want %s (detail %q)", res, r.want, r.detail)
			}
			if strings.Contains(fmt.Sprintf("%+v", res), credCanary) {
				t.Fatalf("probe result carries the helper's output: %+v", res)
			}
		})
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ok.stdin")); err != nil || string(b) != "ghcr.io\n" {
		t.Errorf("helper stdin = %q (%v), want the server line", b, err)
	}
	// The helper is resolved on the ENV's PATH, not the process PATH.
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	if res := ProbeCredentialHelper(context.Background(), CredentialProbe{Helper: "ok", Server: "x"}, []string{"PATH=/nonexistent"}, time.Second); res.Outcome != ProbeMissing {
		t.Errorf("a helper only on the process PATH was resolved: %+v", res)
	}
}

// pidGone reports whether pid no longer exists, polling while it is a zombie
// awaiting its new parent's reap.
func pidGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture: %s not written before the bound expired: %v", filepath.Base(path), err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		t.Fatalf("fixture: bad pid %q", b)
	}
	return pid
}

// TestProbeCredentialHelper_BlockedKillsHelperAndDescendant: a helper that
// never answers is classified blocked at the bound, and BOTH the helper and
// the descendant it backgrounded are gone when the probe returns — the whole
// process group is killed, not only the helper. (A non-interactive sh runs
// `sleep &` without job control, so the sleep stays in the helper's group.)
func TestProbeCredentialHelper_BlockedKillsHelperAndDescendant(t *testing.T) {
	dir, env := helperDir(t, map[string]string{
		"block": `echo $$ > "$FH_PROBE_DIR/helper.pid"; sleep 600 & echo $! > "$FH_PROBE_DIR/child.pid"; wait`,
	})
	const bound = 2 * time.Second
	start := time.Now()
	res := ProbeCredentialHelper(context.Background(), CredentialProbe{Helper: "block", Server: "https://index.docker.io/v1/"}, env, bound)
	elapsed := time.Since(start)
	helper, child := readPid(t, filepath.Join(dir, "helper.pid")), readPid(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL); _ = syscall.Kill(helper, syscall.SIGKILL) })
	if res.Outcome != ProbeBlocked {
		t.Fatalf("outcome = %s, want blocked", res.Outcome)
	}
	if elapsed > bound+probeWaitDelay+5*time.Second {
		t.Errorf("probe returned after %s, far past the %s bound", elapsed, bound)
	}
	if !pidGone(helper, 5*time.Second) {
		t.Errorf("helper pid %d survived the probe", helper)
	}
	if !pidGone(child, 5*time.Second) {
		t.Errorf("the helper's backgrounded descendant pid %d survived the probe (only the helper was killed, not its process group)", child)
	}
	want := "container_credentials_blocked: credential helper docker-credential-block did not answer within 2s for https://index.docker.io/v1/ (a locked keychain/screen, or a slow network-backed helper)"
	if got := res.BlockedReason(); got != want {
		t.Errorf("BlockedReason = %q, want %q", got, want)
	}
}
