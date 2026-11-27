package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
)

// The in-image reach load on the container gate path (#5524, ADR-090 A3.7 as
// amended). Most tests stub sliceReachContainerExecFn; the route, launch,
// bound and seed tests drive the REAL runGateInContainer with the runtime
// emulated on execBoundedHostArgvFn.

var sliceReachTestContainerSel = gateiso.Selection{Path: gateiso.PathContainer, Profile: gateiso.ProfileLocal}

// sliceReachExecCall is one captured reach exec request.
type sliceReachExecCall struct {
	ctx     context.Context
	sel     gateiso.Selection
	argv    []string
	dir     string
	lintDir string
	env     []string
	extras  []string
	timeout time.Duration
}

// sliceReachStubExec stubs the reach exec seam with respond and records calls.
func sliceReachStubExec(t *testing.T, respond func(c sliceReachExecCall, nonce string) (string, int, gateDisposition)) *[]sliceReachExecCall {
	t.Helper()
	var calls []sliceReachExecCall
	prev := sliceReachContainerExecFn
	sliceReachContainerExecFn = func(ctx context.Context, sel gateiso.Selection, argv []string, dir, lintDir string, env, extras []string, timeout time.Duration) (string, int, gateDisposition) {
		c := sliceReachExecCall{ctx, sel, argv, dir, lintDir, env, extras, timeout}
		calls = append(calls, c)
		return respond(c, sliceReachExtra(extras, sliceReachNonceEnv))
	}
	t.Cleanup(func() { sliceReachContainerExecFn = prev })
	return &calls
}

func sliceReachExtra(extras []string, key string) string {
	for _, kv := range extras {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func sliceReachResultLine(nonce, doc string) string {
	return sliceReachResultMarker + " " + nonce + " " + doc + "\n"
}

const sliceReachTestGraph = `{"imports":{"a":[],"b":["a"],"c":[]},"emittable":["a","b","c"]}`

func sliceReachCaptureLog(t *testing.T) *strings.Builder {
	t.Helper()
	var b strings.Builder
	prev := sliceReachLogSink
	sliceReachLogSink = &b
	t.Cleanup(func() { sliceReachLogSink = prev })
	return &b
}

// TestSliceReachLoadContainer_FailureModes: one row per named exec outcome.
func TestSliceReachLoadContainer_FailureModes(t *testing.T) {
	sliceGateWithSelection(t, sliceReachTestContainerSel)
	for _, tc := range []struct {
		name string
		out  string
		code int
		disp gateDisposition
		want string
	}{
		{"refused", "gate isolation refused: x", -1, gateRefused, "reach_container_unavailable:refused"},
		{"unavailable", "gate container: x", -1, gateUnavailable, "reach_container_unavailable:unavailable"},
		{"checkout_refused", "gate container: seed", -1, gateCheckoutRefused, "reach_container_unavailable:checkout_refused"},
		{"launch_failed", "launch failed", 125, gateLaunchFailed, "reach_container_unavailable:launch_failed"},
		{"timed out", "", -1, gateTimedOut, "reach_load_failed:load_timeout"},
		{"non-zero exit", "go: SECRET-TAIL-TEXT\n", 3, gateExecuted, "reach_container_exit:3"},
		{"exit 0 no line", "nothing\n", 0, gateExecuted, "reach_container_output_unparseable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := sliceReachCaptureLog(t)
			sliceReachStubExec(t, func(sliceReachExecCall, string) (string, int, gateDisposition) {
				return tc.out, tc.code, tc.disp
			})
			imports, emittable, reason := sliceReachLoadContainer(context.Background(), t.TempDir())
			if reason != tc.want || imports != nil || emittable != nil {
				t.Fatalf("(imports, emittable, reason) = (%v, %v, %q), want nil/nil/%q", imports, emittable, reason, tc.want)
			}
			if tc.code == 3 {
				if !strings.Contains(log.String(), `"event":"slice_reach_container_failed"`) ||
					!strings.Contains(log.String(), "SECRET-TAIL-TEXT") || !strings.Contains(log.String(), `"exit_code":"3"`) {
					t.Errorf("log lacks the slice_reach_container_failed line with code and tail:\n%s", log.String())
				}
				if strings.Contains(reason, "SECRET") {
					t.Errorf("the output tail leaked into the reason %q", reason)
				}
			}
		})
	}
}

// TestSliceReachLoadContainer_FailedExecHandsSeedOn (approval condition M1
// over the failure arms): through the REAL runGateInContainer, a reach exec
// whose seed succeeded but whose exec then fails to launch, times out, or
// loses its parent ctx widens with the reason that actually results AND still
// hands its seed to the deciding verify on the same clone, so the tree pays
// exactly one host-side seed across the reach exec plus the verify.
func TestSliceReachLoadContainer_FailedExecHandsSeedOn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reach    func(ctx context.Context, cancel context.CancelFunc, marker string) (string, int, bool)
		want     string
		wantKill bool
	}{
		{"launch failure", func(context.Context, context.CancelFunc, string) (string, int, bool) {
			return "docker: Error response from daemon: oci runtime\n", 125, false
		}, "reach_container_unavailable:launch_failed", false},
		{"exec timeout", func(_ context.Context, _ context.CancelFunc, marker string) (string, int, bool) {
			return marker + "go: still loading\n", -1, true
		}, "reach_load_failed:load_timeout", true},
		{"parent ctx cancelled", func(ctx context.Context, cancel context.CancelFunc, marker string) (string, int, bool) {
			// Like execBoundedHostArgv: a parent cancellation mid-exec is
			// (-1, timedOut=false), so the script's "exit" is -1.
			cancel()
			<-ctx.Done()
			return marker, -1, false
		}, "reach_container_exit:-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sliceReachQuietLog(t)
			st := containerState("img:1", "/nonexistent/daemon.sock", &strings.Builder{})
			installGateState(t, st)
			scriptAuxExec(t, nil)
			seeds := sliceReachCountSeeds(t, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var kills, verifies int
			prev := execBoundedHostArgvFn
			execBoundedHostArgvFn = func(c context.Context, argv []string, _ string, _ []string, _ time.Duration) (string, int, bool) {
				if n := len(argv); n >= 3 && argv[n-3] == "rm" && argv[n-2] == "-f" {
					kills++
					return "", 0, false
				}
				marker := ""
				if n := sliceReachArgvLaunchNonce(argv); n != "" {
					marker = gateiso.LaunchMarkerLine(n) + "\n"
				}
				for _, a := range argv {
					if strings.HasPrefix(a, sliceReachHelperEnv+"=") {
						return tc.reach(c, cancel, marker)
					}
				}
				verifies++
				return marker + "verify ok", 0, false
			}
			t.Cleanup(func() { execBoundedHostArgvFn = prev })
			root := t.TempDir()
			if _, _, reason := sliceReachLoadContainer(ctx, root); reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if (kills > 0) != tc.wantKill {
				t.Errorf("container kills = %d, want kill %v", kills, tc.wantKill)
			}
			st.seedHandoffMu.Lock()
			h := st.seedHandoff
			st.seedHandoffMu.Unlock()
			if h == nil || h.dir != root {
				t.Errorf("seed handoff after the failed reach exec = %+v, want one for %s", h, root)
			}
			out, code, disp := runBoundedGateArgvDisposed(withLaunchMarker(withDecidingGate(context.Background())), []string{"true"}, root,
				filepath.Join(t.TempDir(), "lc"), time.Minute)
			if code != 0 || disp != gateExecuted || !strings.Contains(out, "verify ok") || verifies != 1 {
				t.Fatalf("verify (out, code, disp, verifies) = (%q, %d, %s, %d)", out, code, disp, verifies)
			}
			if seeds[root] != 1 {
				t.Errorf("seed calls on the tree across the failed reach exec plus the verify = %d, want exactly 1", seeds[root])
			}
			if h != nil {
				if _, err := os.Stat(h.vc.Root); !os.IsNotExist(err) {
					t.Errorf("handed-off caches %s not removed after the verify consumed them (err %v)", h.vc.Root, err)
				}
			}
		})
	}
}

// TestSliceReachLoadContainer_RefusesOffContainerPath: defence in depth — a
// selection that is not a non-hosted container path never reaches the exec.
func TestSliceReachLoadContainer_RefusesOffContainerPath(t *testing.T) {
	for _, sel := range []gateiso.Selection{
		{Path: gateiso.PathClone, Profile: gateiso.ProfileLocal},
		{Path: gateiso.PathCloneSandbox, Profile: gateiso.ProfileLocal},
		{Path: gateiso.PathContainer, Profile: gateiso.ProfileHosted},
		{Path: gateiso.PathRefused},
		hostExecSelection(),
	} {
		t.Run(string(sel.Path)+"/"+string(sel.Profile), func(t *testing.T) {
			sliceGateWithSelection(t, sel)
			calls := sliceReachStubExec(t, func(sliceReachExecCall, string) (string, int, gateDisposition) {
				return "", 0, gateExecuted
			})
			if _, _, reason := sliceReachLoadContainer(context.Background(), t.TempDir()); reason != "reach_container_unavailable:not_container_path" {
				t.Errorf("reason = %q, want reach_container_unavailable:not_container_path", reason)
			}
			if len(*calls) != 0 {
				t.Errorf("exec reached %d times off the container path", len(*calls))
			}
		})
	}
}

// TestSliceReachLoadContainer_ExecRequestShape pins the one exec request.
func TestSliceReachLoadContainer_ExecRequestShape(t *testing.T) {
	sliceGateWithSelection(t, sliceReachTestContainerSel)
	wt := t.TempDir()
	calls := sliceReachStubExec(t, func(c sliceReachExecCall, nonce string) (string, int, gateDisposition) {
		if _, err := os.Stat(c.lintDir); err != nil {
			t.Errorf("lint dir missing during the exec: %v", err)
		}
		return "noise\n" + sliceReachResultLine(nonce, sliceReachTestGraph), 0, gateExecuted
	})
	imports, emittable, reason := sliceReachLoadContainer(context.Background(), wt)
	if reason != "" || len(imports) != 3 || !emittable["b"] {
		t.Fatalf("(imports, emittable, reason) = (%v, %v, %q), want the graph", imports, emittable, reason)
	}
	if len(*calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(*calls))
	}
	c := (*calls)[0]
	want := []string{"sh", "-c", `set -eu; d=$(mktemp -d); printf "%s" "$FISHHAWK_SLICE_REACH_HELPER_B64" | base64 -d > "$d/main.go"; cd "$d"; exec env GOWORK=off GOFLAGS= CGO_ENABLED=0 GOENV=off GOCACHE="$d/gocache" go run ./main.go /work`}
	if !reflect.DeepEqual(c.argv, want) {
		t.Errorf("argv = %q, want %q", c.argv, want)
	}
	if c.dir != wt || c.sel != sliceReachTestContainerSel || c.timeout != sliceReachContainerTimeout {
		t.Errorf("dir/sel/timeout = %q/%+v/%v", c.dir, c.sel, c.timeout)
	}
	if !skipsGateServices(c.ctx) || !wantsLaunchMarker(c.ctx) || !handsOffSeed(c.ctx) {
		t.Errorf("ctx markers: services skipped %v, launch marker %v, seed handoff %v; want all true",
			skipsGateServices(c.ctx), wantsLaunchMarker(c.ctx), handsOffSeed(c.ctx))
	}
	if isDecidingGate(c.ctx) || requiresGitWorkTree(c.ctx) {
		t.Errorf("the reach exec must not be marked a deciding gate or require a work-tree preflight")
	}
	var b64 string
	for _, kv := range c.extras {
		if v, ok := strings.CutPrefix(kv, sliceReachHelperEnv+"="); ok {
			b64 = v
			if len(kv) >= 100<<10 {
				t.Errorf("encoded helper entry is %d bytes, want < 100 KiB", len(kv))
			}
		}
	}
	src, err := os.ReadFile(filepath.Join(pkgSrcDir, "slicereachhelper", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if dec, err := base64.StdEncoding.DecodeString(b64); err != nil || !bytes.Equal(dec, src) || strings.ContainsAny(b64, "\n\r") {
		t.Errorf("helper env does not decode byte-identically to slicereachhelper/main.go (err %v)", err)
	}
	if n := sliceReachExtra(c.extras, sliceReachNonceEnv); len(n) != 32 {
		t.Errorf("nonce entry = %q, want a 32-hex nonce", n)
	}
	if _, err := os.Stat(c.lintDir); !os.IsNotExist(err) {
		t.Errorf("lint dir %q not removed after the load (stat err %v)", c.lintDir, err)
	}
	var lintPinned bool
	for _, kv := range c.env {
		if strings.HasPrefix(kv, "GOLANGCI_LINT_CACHE=") && strings.Contains(kv, c.lintDir) {
			lintPinned = true
		}
	}
	if !lintPinned {
		t.Errorf("sanitized env lacks the isolated lint cache: %q", c.env)
	}
}

// TestSliceReachHelper_Embedded: the embedded bytes are the helper file.
func TestSliceReachHelper_Embedded(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(pkgSrcDir, "slicereachhelper", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sliceReachHelperSource, src) {
		t.Fatal("sliceReachHelperSource differs from slicereachhelper/main.go")
	}
	if n := len(sliceReachHelperEnv) + 1 + base64.StdEncoding.EncodedLen(len(src)); n >= 100<<10 {
		t.Errorf("encoded helper entry is %d bytes, want < 100 KiB (MAX_ARG_STRLEN headroom)", n)
	}
}

// TestSliceReachParseContainerOutput: one row per unparseable mode, plus the
// accepted shapes.
func TestSliceReachParseContainerOutput(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef"
	line := func(doc string) string { return sliceReachResultLine(nonce, doc) }
	long := "reach_load_failed:" + strings.Repeat("x", 400)
	for _, tc := range []struct {
		name       string
		out        string
		wantReason string
		wantGraph  bool
	}{
		{"valid graph", "go: noise\n" + line(sliceReachTestGraph), "", true},
		{"valid graph CRLF", strings.TrimSuffix(line(sliceReachTestGraph), "\n") + "\r\n", "", true},
		{"forged wrong-nonce line then genuine", sliceReachResultLine("ffffffffffffffffffffffffffffffff", `{"reason":"reach_unsupported_workspace"}`) + line(sliceReachTestGraph), "", true},
		{"valid helper reason", line(`{"reason":"reach_unsupported_workspace:metadata_not_regular:root"}`), "reach_unsupported_workspace:metadata_not_regular:root", false},
		{"bare unsupported", line(`{"reason":"reach_unsupported_workspace"}`), "reach_unsupported_workspace", false},
		{"dir outside clone", line(`{"reason":"reach_dir_outside_clone:example.com/x"}`), "reach_dir_outside_clone:example.com/x", false},
		{"long reason bounded", line(`{"reason":"` + long + `"}`), long[:sliceGateReasonMaxBytes], false},
		{"no result line", "ok\n", sliceReachReasonContainerUnparseable, false},
		{"two matching lines", line(sliceReachTestGraph) + line(sliceReachTestGraph), sliceReachReasonContainerUnparseable, false},
		{"malformed json", line(`{"imports":`), sliceReachReasonContainerUnparseable, false},
		{"unknown field", line(`{"imports":{"a":[]},"emittable":["a"],"extra":1}`), sliceReachReasonContainerUnparseable, false},
		{"trailing data", line(`{"imports":{"a":[]},"emittable":["a"]} {}`), sliceReachReasonContainerUnparseable, false},
		{"over the cap", line(`{"reason":"reach_load_failed:` + strings.Repeat("y", sliceReachResultCap) + `"}`), sliceReachReasonContainerUnparseable, false},
		{"foreign reason", line(`{"reason":"go: build failed"}`), sliceReachReasonContainerUnparseable, false},
		{"reason with control char", line(`{"reason":"reach_load_failed:a\u001bb"}`), sliceReachReasonContainerUnparseable, false},
		{"reason carrying a graph", line(`{"reason":"reach_unsupported_workspace","imports":{"a":[]},"emittable":["a"]}`), sliceReachReasonContainerUnparseable, false},
		{"empty graph", line(`{}`), sliceReachReasonContainerUnparseable, false},
		{"dotdot and abs dirs only defect", line(`{"imports":{"../x":["/abs"],"/abs":[]},"emittable":["../x"]}`), sliceReachReasonContainerUnparseable, false},
		{"dotdot dir", line(`{"imports":{"../x":[]},"emittable":["../x"]}`), sliceReachReasonContainerUnparseable, false},
		{"absolute dir", line(`{"imports":{"/abs":[]},"emittable":["/abs"]}`), sliceReachReasonContainerUnparseable, false},
		{"unclean dir", line(`{"imports":{"a/./b":[]},"emittable":["a/./b"]}`), sliceReachReasonContainerUnparseable, false},
		{"empty dir", line(`{"imports":{"":[]},"emittable":[""]}`), sliceReachReasonContainerUnparseable, false},
		{"control-char dir", line(`{"imports":{"a\u0000b":[]},"emittable":["a\u0000b"]}`), sliceReachReasonContainerUnparseable, false},
		{"emittable not a node", line(`{"imports":{"a":[]},"emittable":["a","b"]}`), sliceReachReasonContainerUnparseable, false},
		{"edge target not a node", line(`{"imports":{"a":["b"]},"emittable":["a"]}`), sliceReachReasonContainerUnparseable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imports, emittable, reason := sliceReachParseContainerOutput(tc.out, nonce)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if !tc.wantGraph {
				if imports != nil || emittable != nil {
					t.Errorf("a refusal returned a graph: %v / %v", imports, emittable)
				}
				return
			}
			wantImports := map[string][]string{"a": {}, "b": {"a"}, "c": {}}
			if !reflect.DeepEqual(imports, wantImports) || !reflect.DeepEqual(emittable, map[string]bool{"a": true, "b": true, "c": true}) {
				t.Errorf("graph = %v / %v", imports, emittable)
			}
		})
	}
}

// TestSliceReachContainerScript_Hermetic runs the EXACT in-container script
// under /bin/sh with the real mktemp and base64 and a fake `go` on PATH that
// records its argv, env, cwd and the decoded main.go, then relays a result
// line. The caller's env presets the opposite of every pinned key (GOCACHE
// at the gate cache volume's path), so only the script's own `env` pins can
// produce the asserted values.
func TestSliceReachContainerScript_Hermetic(t *testing.T) {
	for _, tool := range []string{"/bin/sh", "mktemp", "base64"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	bin, rec, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(bin, "go"), `#!/bin/sh
printf '%s\n' "$@" > "$REC/argv"
env > "$REC/env"
pwd > "$REC/pwd"
cp ./main.go "$REC/main.go"
printf 'FISHHAWK_SLICE_REACH_RESULT %s {"reason":"reach_unsupported_workspace"}\n' "$FISHHAWK_SLICE_REACH_NONCE"
`)
	if err := os.Chmod(filepath.Join(bin, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	const nonce = "abcdefabcdefabcdefabcdefabcdefab"
	cmd := exec.Command("/bin/sh", "-c", sliceReachContainerArgv[2])
	cmd.Dir = t.TempDir()
	cmd.Env = []string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"TMPDIR=" + tmp, "HOME=" + tmp, "REC=" + rec,
		sliceReachHelperEnv + "=" + base64.StdEncoding.EncodeToString(sliceReachHelperSource),
		sliceReachNonceEnv + "=" + nonce,
		"GOWORK=/elsewhere/go.work", "GOFLAGS=-mod=mod", "CGO_ENABLED=1", "GOENV=/elsewhere/env",
		"GOCACHE=" + gateiso.GateGoCache,
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(rec, "main.go"))
	if err != nil || !bytes.Equal(got, sliceReachHelperSource) {
		t.Errorf("decoded main.go differs from the embedded helper (err %v, %d vs %d bytes)", err, len(got), len(sliceReachHelperSource))
	}
	argv, _ := os.ReadFile(filepath.Join(rec, "argv"))
	if string(argv) != "run\n./main.go\n/work\n" {
		t.Errorf("go argv = %q, want run ./main.go /work", argv)
	}
	envBytes, _ := os.ReadFile(filepath.Join(rec, "env"))
	env := map[string]string{}
	for _, l := range strings.Split(string(envBytes), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			env[k] = v
		}
	}
	for k, want := range map[string]string{"GOWORK": "off", "GOFLAGS": "", "CGO_ENABLED": "0", "GOENV": "off", sliceReachNonceEnv: nonce} {
		if v, ok := env[k]; !ok || v != want {
			t.Errorf("go saw %s=%q (present %v), want %q", k, v, ok, want)
		}
	}
	pwd, _ := os.ReadFile(filepath.Join(rec, "pwd"))
	p := strings.TrimSpace(string(pwd))
	if p == "" || p == cmd.Dir || strings.HasPrefix(p, cmd.Dir+"/") {
		t.Errorf("go ran in %q, want the fresh mktemp dir, not the start dir %q", p, cmd.Dir)
	}
	// GOCACHE is the exec's own fresh dir, never the inherited cache volume.
	if got, want := env["GOCACHE"], p+"/gocache"; p == "" || got != want {
		t.Errorf("go saw GOCACHE=%q, want %q (a fresh dir under the mktemp dir)", got, want)
	}
	if _, _, reason := sliceReachParseContainerOutput(string(out), nonce); reason != "reach_unsupported_workspace" {
		t.Errorf("relayed output %q parsed to %q, want reach_unsupported_workspace", out, reason)
	}
}

// sliceReachEmulator is an emulated container runtime on execBoundedHostArgvFn.
// For the reach `run` argv it decodes the helper from the -e entry and runs it
// with the host's `go run` against the checkout named by `-v <checkout>:/work`;
// any other run argv prints the launch marker and "verify ok".
type sliceReachEmulator struct {
	mu       sync.Mutex
	reaches  int
	verifies int
	timeouts []time.Duration
}

func sliceReachArgvLaunchNonce(argv []string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == gateiso.LaunchMarkerArg0 {
			return argv[i+1]
		}
	}
	return ""
}

func installSliceReachEmulator(t *testing.T, reach func(argv []string, checkout, helperB64, nonce string) (string, int)) *sliceReachEmulator {
	t.Helper()
	em := &sliceReachEmulator{}
	prev := execBoundedHostArgvFn
	execBoundedHostArgvFn = func(ctx context.Context, argv []string, _ string, _ []string, timeout time.Duration) (string, int, bool) {
		// Like execBoundedHostArgv: a parent ctx already done is (-1, false).
		if ctx.Err() != nil {
			return "", -1, false
		}
		var checkout, b64, nonce string
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "-v" {
				if src, ok := strings.CutSuffix(argv[i+1], ":"+gateiso.MountWork); ok {
					checkout = src
				}
			}
			if argv[i] == "-e" {
				if v, ok := strings.CutPrefix(argv[i+1], sliceReachHelperEnv+"="); ok {
					b64 = v
				}
				if v, ok := strings.CutPrefix(argv[i+1], sliceReachNonceEnv+"="); ok {
					nonce = v
				}
			}
		}
		marker := ""
		if n := sliceReachArgvLaunchNonce(argv); n != "" {
			marker = gateiso.LaunchMarkerLine(n) + "\n"
		}
		em.mu.Lock()
		em.timeouts = append(em.timeouts, timeout)
		isReach := b64 != ""
		if isReach {
			em.reaches++
		} else {
			em.verifies++
		}
		em.mu.Unlock()
		if !isReach {
			return marker + "verify ok", 0, false
		}
		out, code := reach(argv, checkout, b64, nonce)
		return marker + out, code, false
	}
	t.Cleanup(func() { execBoundedHostArgvFn = prev })
	return em
}

// sliceReachRunHelper runs the decoded helper with the host go, exactly as
// the in-container script does (lone file, GOWORK=off, no module).
func sliceReachRunHelper(t *testing.T, checkout, b64, nonce string) (string, int) {
	t.Helper()
	src, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode helper: %v", err)
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "main.go"), string(src))
	cmd := exec.Command("go", "run", "./main.go", checkout)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=", "CGO_ENABLED=0", "GOENV=off", "GOTOOLCHAIN=local", sliceReachNonceEnv+"="+nonce)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("helper: %v\n%s", err, out)
		return string(out), 1
	}
	return string(out), 0
}

// sliceReachCountSeeds stubs the host-side seed and counts calls per checkout.
func sliceReachCountSeeds(t *testing.T, delay time.Duration) map[string]int {
	t.Helper()
	counts := map[string]int{}
	var mu sync.Mutex
	stubSeed(t, func(checkout string) error {
		time.Sleep(delay)
		mu.Lock()
		counts[checkout]++
		mu.Unlock()
		return nil
	})
	return counts
}

// TestSliceGateReach_ContainerRouteEndToEnd is the cross-boundary test: the
// hook (sg.resolve) → sliceReachLoadContainer → the REAL runGateInContainer
// (argv build, env projection, mount guard, launch marker) → the emulated
// runtime running the decoded helper over the mounted checkout → the parser →
// sliceGateResolveReach. The scoped packages equal the host loader's on the
// same fixture. Then the deciding verify on the SAME clone pays no second
// seed (approval condition M1: exactly one seed per tree, two trees → two).
func TestSliceGateReach_ContainerRouteEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	sliceReachQuietLog(t)
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", &strings.Builder{}))
	scriptAuxExec(t, nil)
	seeds := sliceReachCountSeeds(t, 0)
	em := installSliceReachEmulator(t, func(_ []string, checkout, b64, nonce string) (string, int) {
		return sliceReachRunHelper(t, checkout, b64, nonce)
	})

	var trees []string
	for i := 0; i < 2; i++ {
		root := sliceReachWriteFixture(t, sliceReachBaseFiles())
		trees = append(trees, root)
		hostImports, hostEmit, hostReason := sliceReachLoad(context.Background(), root)
		if hostReason != "" {
			t.Fatalf("host loader refused the fixture: %s", hostReason)
		}
		want, wantReason := sliceGateResolveReach(hostImports, hostEmit, []string{"m1/a/a.go"}, nil)
		if wantReason != "" || len(want) == 0 {
			t.Fatalf("host reach = %v / %q", want, wantReason)
		}

		sg := newSliceGateReach([]string{"m1/a/a.go"}, nil)
		got := sg.resolve(context.Background(), root, "tree")
		d, _ := sg.take()
		if !reflect.DeepEqual(got, want) || d.Shape != sliceGateShapeScoped {
			t.Fatalf("container route = %v (%+v), want the host loader's %v", got, d, want)
		}
		// The deciding verify on the same clone, through the gate seam.
		out, code, disp := runBoundedGateArgvDisposed(withLaunchMarker(withDecidingGate(context.Background())), []string{"true"}, root,
			filepath.Join(t.TempDir(), "lc"), time.Minute)
		if code != 0 || disp != gateExecuted || !strings.Contains(out, "verify ok") {
			t.Fatalf("verify (out, code, disp) = (%q, %d, %s)", out, code, disp)
		}
		if seeds[root] != 1 {
			t.Errorf("tree %d: seed calls = %d across the reach load plus the verify, want exactly 1", i, seeds[root])
		}
	}
	if len(seeds) != 2 || em.reaches != 2 || em.verifies != 2 {
		t.Errorf("seeds %v reaches %d verifies %d, want one seed per tree over 2 trees", seeds, em.reaches, em.verifies)
	}
	if em.timeouts[0] != sliceReachContainerTimeout || em.timeouts[1] != time.Minute {
		t.Errorf("exec timeouts = %v, want [%v (reach) %v (verify) ...]", em.timeouts, sliceReachContainerTimeout, time.Minute)
	}
	// A verify on a tree with no reach exec before it seeds for itself.
	other := sliceReachWriteFixture(t, sliceReachBaseFiles())
	if _, code, _ := runBoundedGateArgvDisposed(withLaunchMarker(context.Background()), []string{"true"}, other, filepath.Join(t.TempDir(), "lc"), time.Minute); code != 0 || seeds[other] != 1 {
		t.Errorf("an unpaired verify seeded %d times (code %d), want 1", seeds[other], code)
	}
	_ = trees
}

// TestSliceReachLoadContainer_BoundCoversExecOnly (approval condition M2): a
// pre-exec step (the seed) slower than the reach bound does NOT time the load
// out; the bound is the exec's own timeout.
func TestSliceReachLoadContainer_BoundCoversExecOnly(t *testing.T) {
	prev := sliceReachContainerTimeout
	sliceReachContainerTimeout = 30 * time.Millisecond
	t.Cleanup(func() { sliceReachContainerTimeout = prev })
	installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", &strings.Builder{}))
	scriptAuxExec(t, nil)
	sliceReachCountSeeds(t, 200*time.Millisecond)
	em := installSliceReachEmulator(t, func(_ []string, _, _, nonce string) (string, int) {
		return sliceReachResultLine(nonce, sliceReachTestGraph), 0
	})
	_, _, reason := sliceReachLoadContainer(context.Background(), t.TempDir())
	if reason != "" {
		t.Fatalf("reason = %q, want the graph (a slow pre-exec step is not a reach timeout)", reason)
	}
	if len(em.timeouts) != 1 || em.timeouts[0] != 30*time.Millisecond {
		t.Errorf("exec timeouts = %v, want [30ms]: the reach bound is the exec's", em.timeouts)
	}
}

// TestSliceReachLoadContainer_LaunchFailureNamed (approval condition M3): a
// runtime 125 BEFORE the launch marker is launch_failed; the same code AFTER
// the marker is the script's own exit.
func TestSliceReachLoadContainer_LaunchFailureNamed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker bool
		want   string
	}{
		{"before marker", false, "reach_container_unavailable:launch_failed"},
		{"after marker", true, "reach_container_exit:125"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sliceReachQuietLog(t)
			installGateState(t, containerState("img:1", "/nonexistent/daemon.sock", &strings.Builder{}))
			scriptAuxExec(t, nil)
			sliceReachCountSeeds(t, 0)
			prev := execBoundedHostArgvFn
			execBoundedHostArgvFn = func(_ context.Context, argv []string, _ string, _ []string, _ time.Duration) (string, int, bool) {
				out := "docker: Error response from daemon: oci runtime\n"
				if n := sliceReachArgvLaunchNonce(argv); tc.marker && n != "" {
					out = gateiso.LaunchMarkerLine(n) + "\n" + out
				}
				return out, 125, false
			}
			t.Cleanup(func() { execBoundedHostArgvFn = prev })
			if _, _, reason := sliceReachLoadContainer(context.Background(), t.TempDir()); reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
		})
	}
}

// TestSliceReachContainer_RealGateImage runs the in-image load against a REAL
// container when FISHHAWK_SLICE_REACH_TEST_IMAGE names an image with go, sh,
// mktemp and base64. Opt-in; it never counts toward dockerFixturesRan.
func TestSliceReachContainer_RealGateImage(t *testing.T) {
	image := os.Getenv("FISHHAWK_SLICE_REACH_TEST_IMAGE")
	if image == "" {
		t.Skip("FISHHAWK_SLICE_REACH_TEST_IMAGE not set")
	}
	sliceReachQuietLog(t)
	st, err := configureGateIsolation(fakeEnv(map[string]string{gateIsolationModeEnvVar: "container", gateImageEnvVar: image, gateCacheEnvVar: "off"}), gateiso.DefaultProbes(), &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	installGateState(t, st)
	sel := st.selection(context.Background())
	if sel.Path != gateiso.PathContainer || !sel.Runtime.Safe {
		t.Skipf("no safe container runtime: %+v", sel)
	}
	// The REAL seed, counted: the reach exec plus the verify below pay one.
	seeds := 0
	prevSeed := seedModCacheFn
	seedModCacheFn = func(ctx context.Context, run gateiso.SeedExecFunc, checkout, hostModCache string, dest *gateiso.VisibleCaches, baseEnv []string, timeout time.Duration) (gateiso.SeedReport, error) {
		seeds++
		return prevSeed(ctx, run, checkout, hostModCache, dest, baseEnv, timeout)
	}
	t.Cleanup(func() { seedModCacheFn = prevSeed })
	root := sliceReachWriteFixture(t, sliceReachBaseFiles())
	hostImports, hostEmit, hostReason := sliceReachLoad(context.Background(), root)
	imports, emittable, reason := sliceReachLoadContainer(context.Background(), root)
	gotImp, _ := sliceReachParityNormalize(imports, nil)
	wantImp, _ := sliceReachParityNormalize(hostImports, nil)
	if reason != hostReason || !reflect.DeepEqual(emittable, hostEmit) || !reflect.DeepEqual(gotImp, wantImp) {
		t.Fatalf("in-image (%v, %v, %q) != host (%v, %v, %q)", imports, emittable, reason, hostImports, hostEmit, hostReason)
	}
	// The deciding verify on the same clone mounts the reach exec's
	// handed-off caches in a second real container and seeds nothing.
	out, code, disp := runBoundedGateArgvDisposed(withLaunchMarker(withDecidingGate(context.Background())),
		[]string{"go", "list", "-deps", "example.com/m1/d"}, root, filepath.Join(t.TempDir(), "lc"), 2*time.Minute)
	if code != 0 || disp != gateExecuted || !strings.Contains(out, "example.com/m1/a") {
		t.Fatalf("verify on the handed-off caches (out, code, disp) = (%q, %d, %s)", out, code, disp)
	}
	if seeds != 1 {
		t.Errorf("real seed calls across the reach exec plus the verify = %d, want exactly 1", seeds)
	}
}
