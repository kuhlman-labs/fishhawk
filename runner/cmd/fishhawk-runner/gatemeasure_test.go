package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
)

// ---------------------------------------------------------------------------
// Three-way gate measurement harness (E51.18 / #3967, plan step 7).
//
// TestGateMeasure_ThreeWayFullVerify runs the SAME commit's full verify on
// three arms — clone (host exec), container-cold (FISHHAWK_GATE_CACHE=off) and
// container-volume (FISHHAWK_GATE_CACHE=process, exec 1 cold volume, exec 2
// warm) — and prints wall time, CPU and the gate_container_timing phase split
// per exec. It is OPT-IN on FISHHAWK_GATE_MEASURE_IMAGE and NEVER counts
// toward the dockerFixturesRan sentinel: one walk is five full verifies
// (~1.5–2h) on an idle docker host with no live runner. The decision rule its
// numbers feed is in runner/internal/gateiso/README.md § "Persistent cache
// volume". The pure helpers below run in-loop via TestGateMeasureHelpers.
// ---------------------------------------------------------------------------

// Measurement knobs. Only the image is required; it opts the walk in.
const (
	gateMeasureImageEnvVar   = "FISHHAWK_GATE_MEASURE_IMAGE"
	gateMeasureCommandEnvVar = "FISHHAWK_GATE_MEASURE_COMMAND"
	gateMeasureSHAEnvVar     = "FISHHAWK_GATE_MEASURE_SHA"
	gateMeasureOutEnvVar     = "FISHHAWK_GATE_MEASURE_OUT"
	// gateMeasureCommandDefault is the runner's committed-tree verify.
	gateMeasureCommandDefault = "scripts/test verify"
	// gateMeasureCPUMarker separates the gate output from the container's
	// cgroup cpu.stat the wrapper appends.
	gateMeasureCPUMarker = "@@FISHHAWK_MEASURE_CPU@@"
	// gateMeasureExecTimeout bounds ONE full verify.
	gateMeasureExecTimeout = 90 * time.Minute
	// gateMeasureDefaultRule is the wall-time ratio at or under which the
	// container path with FISHHAWK_GATE_CACHE=process becomes this
	// repository's local default (gateiso README decision rule).
	gateMeasureDefaultRule = 1.25
)

// Arm names, as printed in the table.
const (
	armClone           = "clone"
	armContainerCold   = "container-cold"
	armContainerVolume = "container-volume"
)

// measureArm is one gate configuration the walk drives through
// configureGateIsolation. wantCache is the gate_container_timing cache field
// every exec of a container arm must report ("" = a host arm, which logs no
// timing line): a degraded volume arm would otherwise silently measure the
// cold posture and invalidate the comparison.
type measureArm struct {
	name      string
	env       map[string]string
	execs     int
	container bool
	wantCache string
}

// gateMeasureArms is the walk's fixed arm order: clone x2, container-cold x1,
// container-volume x2 on ONE state (exec 1 = cold volume, exec 2 = warm).
func gateMeasureArms(image string) []measureArm {
	container := func(cache string) map[string]string {
		return map[string]string{
			gateIsolationModeEnvVar: string(gateiso.ModeContainer),
			gateImageEnvVar:         image,
			gateServicesEnvVar:      string(gateiso.ServicePostgres),
			gateCacheEnvVar:         cache,
		}
	}
	return []measureArm{
		{name: armClone, env: map[string]string{gateIsolationModeEnvVar: string(gateiso.ModeClone)}, execs: 2},
		{name: armContainerCold, env: container(string(gateiso.CacheModeOff)), execs: 1, container: true, wantCache: string(gateiso.CacheModeOff)},
		{name: armContainerVolume, env: container(string(gateiso.CacheModeProcess)), execs: 2, container: true, wantCache: string(gateiso.CacheModeProcess)},
	}
}

// gateTimingLine is one gate_container_timing runner log line.
type gateTimingLine struct {
	Event       string `json:"event"`
	Cache       string `json:"cache"`
	CacheVolume string `json:"cache_volume"`
	CacheMS     int64  `json:"cache_ms"`
	SeedMS      int64  `json:"seed_ms"`
	ServiceMS   int64  `json:"service_ms"`
	ExecMS      int64  `json:"exec_ms"`
	ExitCode    int    `json:"exit_code"`
}

// measureRow is one exec's result.
type measureRow struct {
	arm    string
	exec   int
	wall   time.Duration
	cpu    string
	exit   int
	timing *gateTimingLine
}

// measureCPUCommand wraps cmd so the container prints its own cgroup v2
// cpu.stat after the gate, preserving the gate's exit code.
func measureCPUCommand(cmd string) string {
	return cmd + "; rc=$?; echo " + gateMeasureCPUMarker + "; cat /sys/fs/cgroup/cpu.stat 2>/dev/null; exit $rc"
}

// splitMeasureCPU splits wrapped output at the LAST marker (the wrapper
// prints it after the gate) and parses usage_usec from what follows. ok is
// false when the marker or the field is absent (no cgroup v2 cpu.stat).
func splitMeasureCPU(out string) (body string, usage time.Duration, ok bool) {
	i := strings.LastIndex(out, gateMeasureCPUMarker)
	if i < 0 {
		return out, 0, false
	}
	usage, ok = parseCPUStatUsage(out[i+len(gateMeasureCPUMarker):])
	return out[:i], usage, ok
}

// parseCPUStatUsage reads the usage_usec field of a cgroup v2 cpu.stat.
func parseCPUStatUsage(text string) (time.Duration, bool) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || f[0] != "usage_usec" {
			continue
		}
		n, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return time.Duration(n) * time.Microsecond, true
	}
	return 0, false
}

// parseTimingLines returns every gate_container_timing line in a runner log,
// skipping lines that are not JSON or carry another event.
func parseTimingLines(log string) []gateTimingLine {
	var out []gateTimingLine
	sc := bufio.NewScanner(strings.NewReader(log))
	for sc.Scan() {
		var l gateTimingLine
		if json.Unmarshal([]byte(sc.Text()), &l) == nil && l.Event == "gate_container_timing" {
			out = append(out, l)
		}
	}
	return out
}

// measurePostureError checks one exec's timing lines against its arm: a host
// arm logs none; a container arm logs exactly one whose cache field is
// wantCache; and a volume arm's later exec mounts the SAME volume as its
// first (firstVolume), so exec 2 really is the warm one. nil means valid.
func measurePostureError(arm measureArm, exec int, lines []gateTimingLine, firstVolume string) error {
	if !arm.container {
		if len(lines) != 0 {
			return fmt.Errorf("%s exec %d: host arm logged %d gate_container_timing lines", arm.name, exec, len(lines))
		}
		return nil
	}
	if len(lines) != 1 {
		return fmt.Errorf("%s exec %d: %d gate_container_timing lines, want 1", arm.name, exec, len(lines))
	}
	l := lines[0]
	if l.Cache != arm.wantCache {
		return fmt.Errorf("%s exec %d: cache %q, want %q (a degraded arm measures the wrong posture)", arm.name, exec, l.Cache, arm.wantCache)
	}
	if arm.wantCache == string(gateiso.CacheModeProcess) && exec > 1 && l.CacheVolume != firstVolume {
		return fmt.Errorf("%s exec %d: mounted %q, exec 1 mounted %q (not the warm volume)", arm.name, exec, l.CacheVolume, firstVolume)
	}
	return nil
}

// formatMeasureCPU renders a CPU cell.
func formatMeasureCPU(d time.Duration, source string, ok bool) string {
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%.1fs (%s)", d.Seconds(), source)
}

// renderMeasureTable renders the walk's markdown table plus the stage-shaped
// decision line.
func renderMeasureTable(rows []measureRow) string {
	var b strings.Builder
	b.WriteString("| arm | exec | wall | cpu | exit | cache | cache ms | seed ms | service ms | exec ms |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		phase := "| - | - | - | - | - |"
		if r.timing != nil {
			phase = fmt.Sprintf("| %s | %d | %d | %d | %d |", r.timing.Cache, r.timing.CacheMS, r.timing.SeedMS, r.timing.ServiceMS, r.timing.ExecMS)
		}
		fmt.Fprintf(&b, "| %s | %d | %.1fs | %s | %d %s\n", r.arm, r.exec, r.wall.Seconds(), r.cpu, r.exit, phase)
	}
	b.WriteString("\n" + measureDecisionLine(rows) + "\n")
	return b.String()
}

// measureDecisionLine applies the decision rule to the stage-shaped sequence
// (exec 1 + exec 2 wall) of the clone and container-volume arms.
func measureDecisionLine(rows []measureRow) string {
	sum := func(arm string) (time.Duration, bool) {
		var d time.Duration
		var n int
		for _, r := range rows {
			if r.arm == arm && (r.exec == 1 || r.exec == 2) && r.exit == 0 {
				d += r.wall
				n++
			}
		}
		return d, n == 2
	}
	clone, okC := sum(armClone)
	volume, okV := sum(armContainerVolume)
	if !okC || !okV || clone <= 0 {
		return "decision: incomplete (needs two exit-0 execs on both the clone and container-volume arms)"
	}
	ratio := volume.Seconds() / clone.Seconds()
	verdict := "stay clone (container stays opt-in; pursue the cross-run trusted-base follow-up)"
	if ratio <= gateMeasureDefaultRule {
		verdict = "make the container path with FISHHAWK_GATE_CACHE=process the local default"
	}
	return fmt.Sprintf("decision: stage-shaped wall (exec 1 + exec 2) clone %.1fs, container-volume %.1fs, ratio %.2f (rule <= %.2f) -> %s",
		clone.Seconds(), volume.Seconds(), ratio, gateMeasureDefaultRule, verdict)
}

// measureUUID is a random v4 UUID: process mode embeds the bound run/stage
// ids in the volume name, and non-UUID ids degrade every exec.
func measureUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("crypto/rand: %v", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// measureLog is a goroutine-safe runner log sink that hands back what each
// exec appended.
type measureLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *measureLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *measureLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// childCPU is the user+sys CPU of this process's waited-for descendants.
func childCPU(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// measureEnsureImage makes image present locally (inspect, else pull) so no
// arm pays a pull inside its timed exec.
func measureEnsureImage(t *testing.T, rt gateiso.Runtime, image string) {
	t.Helper()
	if _, err := runtimeHostCmd(t, rt, "image", "inspect", image); err == nil {
		return
	}
	if out, err := runtimeHostCmd(t, rt, "pull", image); err != nil {
		t.Fatalf("image %s neither present nor pullable: %v\n%s", image, err, out)
	}
}

// measureRepo returns the enclosing repository root and the SHA to measure.
func measureRepo(t *testing.T) (string, string) {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: no source path")
	}
	top, err := exec.Command("git", "-C", filepath.Dir(self), "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("repository root not resolvable from %s: %v", self, err)
	}
	root := strings.TrimSpace(string(top))
	rev := strings.TrimSpace(os.Getenv(gateMeasureSHAEnvVar))
	if rev == "" {
		rev = "HEAD"
	}
	sha, err := exec.Command("git", "-C", root, "rev-parse", "--verify", rev+"^{commit}").Output()
	if err != nil {
		t.Fatalf("%s=%q does not resolve to a commit in %s: %v", gateMeasureSHAEnvVar, rev, root, err)
	}
	return root, strings.TrimSpace(string(sha))
}

// TestGateMeasure_ThreeWayFullVerify is the opt-in three-way walk. Invocation
// (idle docker host, NO live runner — verify lock and shared Postgres):
//
//	(cd runner && FISHHAWK_GATE_MEASURE_IMAGE=<digest-pinned fishhawk-gate> \
//	  go test -run TestGateMeasure_ThreeWayFullVerify -timeout 4h -v ./cmd/fishhawk-runner/)
//
// Optional: FISHHAWK_GATE_MEASURE_COMMAND (default `scripts/test verify`),
// FISHHAWK_GATE_MEASURE_SHA (default HEAD), FISHHAWK_GATE_MEASURE_OUT (a
// markdown file the table is also written to). Every exec runs on a FRESH
// clone at the same SHA and must exit 0 — a red tree invalidates the
// comparison.
func TestGateMeasure_ThreeWayFullVerify(t *testing.T) {
	image := strings.TrimSpace(os.Getenv(gateMeasureImageEnvVar))
	if image == "" {
		t.Skipf("%s unset: opt-in operator walk (~1.5-2h); set it to a digest-pinned fishhawk-gate image", gateMeasureImageEnvVar)
	}
	command := strings.TrimSpace(os.Getenv(gateMeasureCommandEnvVar))
	if command == "" {
		command = gateMeasureCommandDefault
	}
	root, sha := measureRepo(t)
	rt := gateiso.DetectRuntime(context.Background(), gateiso.DefaultProbes())
	if !rt.Safe {
		// Opted in: a silent skip would record nothing.
		t.Fatalf("%s set but no safe container runtime: %s", gateMeasureImageEnvVar, rt.Reason)
	}
	measureEnsureImage(t, rt, image)
	measureEnsureImage(t, rt, gateiso.DefaultPostgresImage)
	t.Logf("measuring %q at %s on %s", command, sha, image)

	var rows []measureRow
	for _, arm := range gateMeasureArms(image) {
		log := &measureLog{}
		st, err := configureGateIsolation(fakeEnv(arm.env), gateiso.DefaultProbes(), log)
		if err != nil {
			t.Fatalf("%s: configure: %v", arm.name, err)
		}
		st.bindOwner(measureUUID(t), measureUUID(t))
		prev := gateIsolation
		gateIsolation = st
		firstVolume := ""
		for i := 1; i <= arm.execs; i++ {
			checkout, err := materializeGateCheckout(context.Background(), root, sha, t.TempDir())
			if err != nil {
				t.Fatalf("%s exec %d: materialize %s: %v", arm.name, i, sha, err)
			}
			cmd := command
			if arm.container {
				cmd = measureCPUCommand(command)
			}
			mark := len(log.String())
			cpuBefore := childCPU(t)
			start := time.Now()
			out, code, disp := runBoundedGateCommandDisposed(context.Background(), cmd, checkout, filepath.Join(t.TempDir(), "lc"), gateMeasureExecTimeout)
			row := measureRow{arm: arm.name, exec: i, wall: time.Since(start), exit: code}
			if arm.container {
				var usage time.Duration
				var ok bool
				out, usage, ok = splitMeasureCPU(out)
				row.cpu = formatMeasureCPU(usage, "cgroup", ok)
			} else {
				row.cpu = formatMeasureCPU(childCPU(t)-cpuBefore, "rusage", true)
			}
			lines := parseTimingLines(log.String()[mark:])
			if len(lines) == 1 {
				row.timing = &lines[0]
				if i == 1 {
					firstVolume = lines[0].CacheVolume
				}
			}
			rows = append(rows, row)
			if err := measurePostureError(arm, i, lines, firstVolume); err != nil {
				t.Errorf("%v\nrunner log:\n%s", err, log.String()[mark:])
			}
			if code != 0 || disp != gateExecuted {
				t.Errorf("%s exec %d: exit %d, disposition %s (a red or unexecuted gate invalidates the comparison):\n%s",
					arm.name, i, code, disp, gateOutputTail(out))
			}
		}
		st.cleanup()
		gateIsolation = prev
	}

	table := renderMeasureTable(rows)
	t.Logf("three-way measurement (%s @ %s, image %s):\n%s", command, sha, image, table)
	if path := strings.TrimSpace(os.Getenv(gateMeasureOutEnvVar)); path != "" {
		doc := fmt.Sprintf("# Gate measurement\n\n- command: `%s`\n- sha: `%s`\n- image: `%s`\n- recorded: %s\n\n%s",
			command, sha, image, time.Now().UTC().Format(time.RFC3339), table)
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Errorf("%s: write %s: %v", gateMeasureOutEnvVar, path, err)
		}
	}
}

// TestGateMeasureHelpers pins the harness's pure helpers in-loop (no docker).
func TestGateMeasureHelpers(t *testing.T) {
	t.Run("cpu wrapper round trip", func(t *testing.T) {
		wrapped := measureCPUCommand("scripts/test verify")
		if !strings.HasPrefix(wrapped, "scripts/test verify; rc=$?; ") || !strings.HasSuffix(wrapped, "exit $rc") {
			t.Fatalf("wrapper %q does not run the command first and preserve its exit code", wrapped)
		}
		out := "ok\nfake " + gateMeasureCPUMarker + " printed by a test\n" + gateMeasureCPUMarker +
			"\nusage_usec 2500000\nuser_usec 2000000\nsystem_usec 500000\n"
		body, usage, ok := splitMeasureCPU(out)
		if !ok || usage != 2500*time.Millisecond {
			t.Fatalf("usage = %v ok=%v, want 2.5s true", usage, ok)
		}
		if !strings.Contains(body, "printed by a test") || strings.Contains(body, "usage_usec") {
			t.Errorf("body split at the wrong marker: %q", body)
		}
	})
	t.Run("cpu stat absent or malformed", func(t *testing.T) {
		for name, out := range map[string]string{
			"no marker":     "ok\nusage_usec 5\n",
			"no cpu.stat":   "ok\n" + gateMeasureCPUMarker + "\n",
			"non-numeric":   gateMeasureCPUMarker + "\nusage_usec lots\n",
			"negative":      gateMeasureCPUMarker + "\nusage_usec -1\n",
			"wrong field":   gateMeasureCPUMarker + "\nuser_usec 5\n",
			"extra columns": gateMeasureCPUMarker + "\nusage_usec 5 6\n",
		} {
			if _, usage, ok := splitMeasureCPU(out); ok {
				t.Errorf("%s: parsed usage %v, want n/a", name, usage)
			}
		}
		if got := formatMeasureCPU(0, "cgroup", false); got != "n/a" {
			t.Errorf("formatMeasureCPU(!ok) = %q, want n/a", got)
		}
	})
	t.Run("timing lines", func(t *testing.T) {
		log := `{"event":"gate_cache_volume_ready","volume":"v","reused":false}` + "\n" +
			"not json\n" +
			`{"event":"gate_container_timing","cache":"process","cache_volume":"v","cache_ms":12,"seed_ms":3400,"service_ms":2100,"exec_ms":600000,"exit_code":0}` + "\n"
		lines := parseTimingLines(log)
		want := gateTimingLine{Event: "gate_container_timing", Cache: "process", CacheVolume: "v", CacheMS: 12, SeedMS: 3400, ServiceMS: 2100, ExecMS: 600000}
		if len(lines) != 1 || lines[0] != want {
			t.Fatalf("parseTimingLines = %+v, want [%+v]", lines, want)
		}
	})
	t.Run("posture", func(t *testing.T) {
		arms := map[string]measureArm{}
		for _, a := range gateMeasureArms("img@sha256:abc") {
			arms[a.name] = a
		}
		vol := func(cache, name string) []gateTimingLine {
			return []gateTimingLine{{Event: "gate_container_timing", Cache: cache, CacheVolume: name}}
		}
		valid := []struct {
			arm   string
			exec  int
			lines []gateTimingLine
			first string
		}{
			{armClone, 1, nil, ""},
			{armContainerCold, 1, vol("off", ""), ""},
			{armContainerVolume, 1, vol("process", "a"), ""},
			{armContainerVolume, 2, vol("process", "a"), "a"},
		}
		for _, c := range valid {
			if err := measurePostureError(arms[c.arm], c.exec, c.lines, c.first); err != nil {
				t.Errorf("%s exec %d: unexpected %v", c.arm, c.exec, err)
			}
		}
		invalid := []struct {
			name  string
			arm   string
			exec  int
			lines []gateTimingLine
			first string
		}{
			{"host arm logged a timing line", armClone, 1, vol("off", ""), ""},
			{"container arm logged none", armContainerCold, 1, nil, ""},
			{"container arm logged two", armContainerCold, 1, append(vol("off", ""), vol("off", "")...), ""},
			{"volume arm degraded", armContainerVolume, 1, vol("degraded", ""), ""},
			{"cold arm mounted a volume", armContainerCold, 1, vol("process", "a"), ""},
			{"warm exec on a different volume", armContainerVolume, 2, vol("process", "b"), "a"},
		}
		for _, c := range invalid {
			if err := measurePostureError(arms[c.arm], c.exec, c.lines, c.first); err == nil {
				t.Errorf("%s: accepted, want a posture error", c.name)
			}
		}
	})
	t.Run("arms", func(t *testing.T) {
		arms := gateMeasureArms("img@sha256:abc")
		if len(arms) != 3 || arms[0].name != armClone || arms[1].name != armContainerCold || arms[2].name != armContainerVolume {
			t.Fatalf("arm order %+v", arms)
		}
		for _, a := range arms {
			st, err := configureGateIsolation(fakeEnv(a.env), gateiso.DefaultProbes(), nil)
			if err != nil {
				t.Fatalf("%s: env does not configure: %v", a.name, err)
			}
			wantMode := gateiso.ModeClone
			if a.container {
				wantMode = gateiso.ModeContainer
			}
			if st.mode != wantMode {
				t.Errorf("%s: mode %q, want %q", a.name, st.mode, wantMode)
			}
			if a.container && (string(st.cacheMode) != a.wantCache || !st.postgresServiceWanted() || st.image != "img@sha256:abc") {
				t.Errorf("%s: cache %q services-postgres %v image %q", a.name, st.cacheMode, st.postgresServiceWanted(), st.image)
			}
		}
		if arms[0].execs != 2 || arms[1].execs != 1 || arms[2].execs != 2 {
			t.Errorf("exec counts %d/%d/%d, want 2/1/2", arms[0].execs, arms[1].execs, arms[2].execs)
		}
	})
	t.Run("uuid", func(t *testing.T) {
		a, b := measureUUID(t), measureUUID(t)
		if a == b {
			t.Fatalf("two UUIDs equal: %s", a)
		}
		if _, err := gateiso.NewCacheVolume(a, b); err != nil {
			t.Errorf("measureUUID ids do not mint a cache volume: %v", err)
		}
	})
	t.Run("table and decision", func(t *testing.T) {
		timing := &gateTimingLine{Cache: "process", CacheMS: 900, SeedMS: 3000, ServiceMS: 2000, ExecMS: 500000}
		rows := []measureRow{
			{arm: armClone, exec: 1, wall: 600 * time.Second, cpu: "2000.0s (rusage)"},
			{arm: armClone, exec: 2, wall: 400 * time.Second, cpu: "1500.0s (rusage)"},
			{arm: armContainerCold, exec: 1, wall: 900 * time.Second, cpu: "n/a"},
			{arm: armContainerVolume, exec: 1, wall: 800 * time.Second, cpu: "2500.0s (cgroup)", timing: timing},
			{arm: armContainerVolume, exec: 2, wall: 400 * time.Second, cpu: "900.0s (cgroup)", timing: timing},
		}
		table := renderMeasureTable(rows)
		for _, want := range []string{
			"| clone | 1 | 600.0s | 2000.0s (rusage) | 0 | - | - | - | - | - |",
			"| container-volume | 1 | 800.0s | 2500.0s (cgroup) | 0 | process | 900 | 3000 | 2000 | 500000 |",
			"ratio 1.20 (rule <= 1.25) -> make the container path",
		} {
			if !strings.Contains(table, want) {
				t.Errorf("table lacks %q:\n%s", want, table)
			}
		}
		rows[4].wall = 600 * time.Second // 1400 / 1000 = 1.40
		if got := measureDecisionLine(rows); !strings.Contains(got, "ratio 1.40") || !strings.Contains(got, "stay clone") {
			t.Errorf("over-rule decision = %q", got)
		}
		rows[1].exit = 1
		if got := measureDecisionLine(rows); !strings.Contains(got, "incomplete") {
			t.Errorf("a red exec must make the decision incomplete, got %q", got)
		}
		if got := measureDecisionLine(rows[:2]); !strings.Contains(got, "incomplete") {
			t.Errorf("a missing arm must make the decision incomplete, got %q", got)
		}
	})
}
