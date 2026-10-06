package gateiso

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	testRunA   = "11111111-2222-3333-4444-555555555555"
	testStageA = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testRunB   = "99999999-8888-7777-6666-555555555555"
	testStageB = "ffffffff-eeee-dddd-cccc-bbbbbbbbbbbb"
	// testCacheVolume is a fixed, pattern-valid cache volume name owned by
	// (testRunA, testStageA).
	testCacheVolume = "fishhawk-gate-cache-" + testRunA + "-" + testStageA + "-0a1b2c3d4e5f"
	testGateImage   = "ghcr.io/kuhlman-labs/fishhawk-gate@sha256:0123"
)

var (
	cvDockerRT = Runtime{Kind: KindDocker, Safe: true, SocketPath: testSock}
	cvPodmanRT = Runtime{Kind: KindPodman, Safe: true, Rootless: true, SocketPath: testSock}
	dockerBind = []string{"docker", "--host", "unix://" + testSock}
	podmanBind = []string{"podman", "--url", "unix://" + testSock}
)

func TestParseCacheMode(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want CacheMode
	}{
		{"", CacheModeProcess},
		{"  ", CacheModeProcess},
		{" process ", CacheModeProcess},
		{"process", CacheModeProcess},
		{"off", CacheModeOff},
		{"\toff\n", CacheModeOff},
	} {
		got, err := ParseCacheMode(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("ParseCacheMode(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{"bogus", "OFF", "Process", "process,off", "volume"} {
		got, err := ParseCacheMode(raw)
		if err == nil || got != "" {
			t.Errorf("ParseCacheMode(%q) = %q, %v; want an error", raw, got, err)
			continue
		}
		for _, want := range []string{`"` + raw + `"`, "process", "off"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ParseCacheMode(%q) error %q does not name %s", raw, err, want)
			}
		}
	}
}

func TestNewCacheVolume(t *testing.T) {
	a, err := NewCacheVolume(testRunA, testStageA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewCacheVolume(testRunA, testStageA)
	if err != nil {
		t.Fatal(err)
	}
	if !cacheVolumePattern.MatchString(a.Name) || !cacheVolumePattern.MatchString(b.Name) {
		t.Fatalf("names %q %q do not match %s", a.Name, b.Name, cacheVolumePattern)
	}
	if a.Name == b.Name {
		t.Fatalf("two mints for the same (run, stage) share the name %q: a retried stage would reuse a cache", a.Name)
	}
	if !strings.HasPrefix(a.Name, "fishhawk-gate-cache-"+testRunA+"-"+testStageA+"-") {
		t.Errorf("name %q does not embed the run and stage ids", a.Name)
	}
	// Upper-case ids are canonicalised, never rendered upper case.
	up, err := NewCacheVolume(strings.ToUpper(testRunA), " "+strings.ToUpper(testStageA)+" ")
	if err != nil {
		t.Fatal(err)
	}
	if run, stage, ok := up.Owner(); !ok || run != testRunA || stage != testStageA {
		t.Errorf("upper-case owner = %q %q %v; want canonical lower-case ids", run, stage, ok)
	}
	for _, ids := range [][2]string{
		{"rid", testStageA},
		{testRunA, ""},
		{"", ""},
		{testRunA, "../../var/run/docker.sock"},
		{"11111111-2222-3333-4444-55555555555", testStageA},
		{testRunA, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-x"},
		{"1111111122223333444455555555555a", testStageA},
	} {
		v, err := NewCacheVolume(ids[0], ids[1])
		if !errors.Is(err, ErrContainerSpec) || v.Name != "" {
			t.Errorf("NewCacheVolume(%q, %q) = %q, %v; want ErrContainerSpec and no name", ids[0], ids[1], v.Name, err)
		}
	}
}

// TestCacheVolume_BelongsTo pins the #3967 approval condition 2 guard: a
// volume minted for one (run, stage) pair never belongs to another, so an
// exec for a second pair mints (and mounts) its own volume instead.
func TestCacheVolume_BelongsTo(t *testing.T) {
	v := CacheVolume{Name: testCacheVolume}
	if run, stage, ok := v.Owner(); !ok || run != testRunA || stage != testStageA {
		t.Fatalf("Owner = %q %q %v", run, stage, ok)
	}
	for _, tc := range []struct {
		run, stage string
		want       bool
	}{
		{testRunA, testStageA, true},
		{strings.ToUpper(testRunA), " " + testStageA, true},
		{testRunB, testStageA, false},
		{testRunA, testStageB, false},
		{testRunB, testStageB, false},
		{testStageA, testRunA, false},
		{"rid", testStageA, false},
		{testRunA, "", false},
	} {
		if got := v.BelongsTo(tc.run, tc.stage); got != tc.want {
			t.Errorf("BelongsTo(%q, %q) = %v, want %v", tc.run, tc.stage, got, tc.want)
		}
	}
	for _, name := range []string{"", "fishhawk-gate-cache-0a1b2c3d4e5f", "/var/run/docker.sock", strings.ToUpper(testCacheVolume)} {
		bad := CacheVolume{Name: name}
		if bad.BelongsTo(testRunA, testStageA) {
			t.Errorf("malformed name %q belongs to a pair", name)
		}
		if _, _, ok := bad.Owner(); ok {
			t.Errorf("malformed name %q has an owner", name)
		}
	}
	// A second pair's mint never yields the first pair's volume.
	first, err := NewCacheVolume(testRunA, testStageA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCacheVolume(testRunB, testStageB)
	if err != nil {
		t.Fatal(err)
	}
	if first.BelongsTo(testRunB, testStageB) || second.BelongsTo(testRunA, testStageA) {
		t.Fatalf("a volume belongs to the other pair: %q / %q", first.Name, second.Name)
	}
	if !second.BelongsTo(testRunB, testStageB) || first.Name == second.Name {
		t.Fatalf("second pair's volume %q is not its own (first %q)", second.Name, first.Name)
	}
}

func TestCacheVolumeArgv_Golden(t *testing.T) {
	v := CacheVolume{Name: testCacheVolume}
	mount := []string{"-v", testCacheVolume + ":/gatecache", "--entrypoint", "", testGateImage}
	install := []string{"install", "-d", "-o", "501", "-g", "20", "-m", "0700", "/gatecache/gocache", "/gatecache/lintcache"}
	probe := []string{"sh", "-c", cacheWriteProbeScript, "sh", "/gatecache/gocache", "/gatecache/lintcache"}
	cat := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	contained := []string{"run", "--rm", "--network=none", "--cap-drop=ALL"}
	nnp := []string{"--security-opt=no-new-privileges"}
	for _, tc := range []struct {
		name string
		got  func() ([]string, error)
		want []string
	}{
		{"docker create", func() ([]string, error) { return v.CreateArgv(cvDockerRT) },
			cat(dockerBind, []string{"volume", "create", "--label", "org.fishhawk.gate-cache=process", testCacheVolume})},
		{"docker prepare", func() ([]string, error) { return v.PrepareArgv(cvDockerRT, testGateImage, 501, 20) },
			cat(dockerBind, contained, []string{"--cap-add=CHOWN"}, nnp, []string{"--user", "0:0"}, mount, install)},
		{"docker check", func() ([]string, error) { return v.CheckArgv(cvDockerRT, testGateImage, 501, 20) },
			cat(dockerBind, contained, nnp, []string{"--user", "501:20"}, mount, probe)},
		{"docker remove", func() ([]string, error) { return v.RemoveArgv(cvDockerRT) },
			cat(dockerBind, []string{"volume", "rm", "-f", testCacheVolume})},
		{"podman create", func() ([]string, error) { return v.CreateArgv(cvPodmanRT) },
			cat(podmanBind, []string{"volume", "create", "--label", "org.fishhawk.gate-cache=process", "--ignore", testCacheVolume})},
		// #3967 approval condition 1: rootless podman runs the helper AS the
		// caller under keep-id — no container root (a subuid), no capability,
		// no -o/-g.
		{"podman prepare", func() ([]string, error) { return v.PrepareArgv(cvPodmanRT, testGateImage, 501, 20) },
			cat(podmanBind, contained, nnp, []string{"--userns=keep-id", "--user", "501:20"}, mount,
				[]string{"install", "-d", "-m", "0700", "/gatecache/gocache", "/gatecache/lintcache"})},
		{"podman check", func() ([]string, error) { return v.CheckArgv(cvPodmanRT, testGateImage, 501, 20) },
			cat(podmanBind, contained, nnp, []string{"--userns=keep-id"}, mount, probe)},
		{"podman remove", func() ([]string, error) { return v.RemoveArgv(cvPodmanRT) },
			cat(podmanBind, []string{"volume", "rm", "-f", testCacheVolume})},
		{"rootful podman prepare takes the docker form", func() ([]string, error) {
			return v.PrepareArgv(Runtime{Kind: KindPodman, SocketPath: testSock}, testGateImage, 501, 20)
		}, cat(podmanBind, contained, []string{"--cap-add=CHOWN"}, nnp, []string{"--user", "0:0"}, mount, install)},
	} {
		got, err := tc.got()
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s argv mismatch\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
	if want := `set -e; for d in "$@"; do p="$d/.fishhawk-write-probe"; : > "$p"; rm -f "$p"; done`; cacheWriteProbeScript != want {
		t.Errorf("write probe script = %q, want %q", cacheWriteProbeScript, want)
	}
}

// TestCacheVolumePrepareArgv_Containment: every helper container is as
// contained as the gate itself, mounts ONLY the volume, and resets the
// entrypoint before the image; the docker prepare's only added capability is
// CHOWN, and no rootless-podman helper ever runs as container root.
func TestCacheVolumePrepareArgv_Containment(t *testing.T) {
	v := CacheVolume{Name: testCacheVolume}
	type helper struct {
		name   string
		argv   func() ([]string, error)
		capAdd []string
	}
	for _, h := range []helper{
		{"docker prepare", func() ([]string, error) { return v.PrepareArgv(cvDockerRT, testGateImage, 501, 20) }, []string{"--cap-add=CHOWN"}},
		{"docker check", func() ([]string, error) { return v.CheckArgv(cvDockerRT, testGateImage, 501, 20) }, nil},
		{"podman prepare", func() ([]string, error) { return v.PrepareArgv(cvPodmanRT, testGateImage, 501, 20) }, nil},
		{"podman check", func() ([]string, error) { return v.CheckArgv(cvPodmanRT, testGateImage, 501, 20) }, nil},
	} {
		got, err := h.argv()
		if err != nil {
			t.Fatalf("%s: %v", h.name, err)
		}
		img := idx(got, testGateImage)
		if img < 0 {
			t.Fatalf("%s: no image in %q", h.name, got)
		}
		opts := got[:img]
		for _, want := range []string{"--rm", "--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges"} {
			if idx(opts, want) < 0 {
				t.Errorf("%s: %s missing before the image in %q", h.name, want, got)
			}
		}
		var caps, mounts []string
		for i, a := range opts {
			switch {
			case strings.HasPrefix(a, "--cap-add"):
				caps = append(caps, a)
			case a == "-v":
				mounts = append(mounts, opts[i+1])
			case a == "--privileged" || strings.HasPrefix(a, "--device") || strings.HasPrefix(a, "--network=host") || strings.HasPrefix(a, "--pid"):
				t.Errorf("%s: forbidden option %q", h.name, a)
			}
		}
		if !reflect.DeepEqual(caps, h.capAdd) {
			t.Errorf("%s: added capabilities %q, want %q", h.name, caps, h.capAdd)
		}
		if len(mounts) != 1 || mounts[0] != testCacheVolume+":"+MountGateCache {
			t.Errorf("%s: mounts %q, want exactly the volume at %s", h.name, mounts, MountGateCache)
		}
		for _, m := range mounts {
			if src, _, _ := strings.Cut(m, ":"); filepath.IsAbs(src) || strings.Contains(src, "/") {
				t.Errorf("%s: path-shaped mount source %q", h.name, src)
			}
		}
		if e := idx(got, "--entrypoint"); e < 0 || got[e+1] != "" || e+2 != img {
			t.Errorf("%s: --entrypoint '' must immediately precede the image: %q", h.name, got)
		}
		if strings.HasPrefix(h.name, "podman") && strings.Contains(strings.Join(opts, " "), "--user 0:0") {
			t.Errorf("%s: rootless podman helper runs as container root (a subuid): %q", h.name, got)
		}
	}
}

// TestCacheVolumeArgv_Refusals: a name not shaped like a cache volume (a host
// path, the daemon socket, a service volume, an unscoped or upper-case name),
// an empty or flag-shaped image, an unbound runtime and a negative uid/gid
// each render NO argv from every builder that takes them.
func TestCacheVolumeArgv_Refusals(t *testing.T) {
	tmp := shortTempDir(t)
	builders := map[string]func(v CacheVolume, rt Runtime, image string, uid, gid int) ([]string, error){
		"create": func(v CacheVolume, rt Runtime, _ string, _, _ int) ([]string, error) { return v.CreateArgv(rt) },
		"prepare": func(v CacheVolume, rt Runtime, image string, uid, gid int) ([]string, error) {
			return v.PrepareArgv(rt, image, uid, gid)
		},
		"check": func(v CacheVolume, rt Runtime, image string, uid, gid int) ([]string, error) {
			return v.CheckArgv(rt, image, uid, gid)
		},
		"remove": func(v CacheVolume, rt Runtime, _ string, _, _ int) ([]string, error) { return v.RemoveArgv(rt) },
	}
	takesImage := map[string]bool{"prepare": true, "check": true}
	type row struct {
		name     string
		vol      string
		rt       Runtime
		image    string
		uid, gid int
		imageArg bool // the row only applies to builders taking image/uid/gid
	}
	ok := row{vol: testCacheVolume, rt: cvDockerRT, image: testGateImage, uid: 501, gid: 20}
	rows := []row{
		{name: "daemon socket as name", vol: "/var/run/docker.sock"},
		{name: "absolute host dir as name", vol: tmp},
		{name: "service volume name", vol: "fishhawk-gate-svc-0a1b2c3d4e5f"},
		{name: "unscoped name", vol: "fishhawk-gate-cache-0a1b2c3d4e5f"},
		{name: "upper-case name", vol: strings.ToUpper(testCacheVolume)},
		{name: "name with a mount suffix", vol: testCacheVolume + ":/etc"},
		{name: "empty name", vol: " "},
		{name: "unbound runtime", rt: Runtime{Kind: KindDocker, Safe: true}},
		{name: "no runtime", rt: Runtime{Kind: KindNone, SocketPath: testSock}},
		{name: "empty image", image: " ", imageArg: true},
		{name: "flag-shaped image", image: "--privileged", imageArg: true},
		{name: "negative uid", uid: -1, imageArg: true},
		{name: "negative gid", gid: -1, imageArg: true},
	}
	for _, r := range rows {
		if r.vol == "" {
			r.vol = ok.vol
		}
		if r.rt.Kind == "" {
			r.rt = ok.rt
		}
		switch r.image {
		case "":
			r.image = ok.image
		case " ":
			r.image = ""
		}
		if r.vol == " " {
			r.vol = ""
		}
		if r.uid == 0 {
			r.uid = ok.uid
		}
		if r.gid == 0 {
			r.gid = ok.gid
		}
		for name, build := range builders {
			if r.imageArg && !takesImage[name] {
				continue
			}
			got, err := build(CacheVolume{Name: r.vol}, r.rt, r.image, r.uid, r.gid)
			if !errors.Is(err, ErrContainerSpec) || got != nil {
				t.Errorf("%s / %s: argv %q, err %v; want ErrContainerSpec and nil argv", r.name, name, got, err)
			}
		}
	}
	// The valid baseline renders from every builder, so each refusal above is
	// attributable to its one mutated input.
	for name, build := range builders {
		if got, err := build(CacheVolume{Name: ok.vol}, ok.rt, ok.image, ok.uid, ok.gid); err != nil || got == nil {
			t.Errorf("baseline %s: %q, %v", name, got, err)
		}
	}
}

// TestCacheVolumeEnv_TargetsUnderMountAndPrepared: every cache env value lies
// under the volume's mount point AND is a directory PrepareArgv creates and
// CheckArgv probes — so the env can never point the gate at a directory
// nobody made writable.
func TestCacheVolumeEnv_TargetsUnderMountAndPrepared(t *testing.T) {
	v := CacheVolume{Name: testCacheVolume}
	env := CacheVolumeEnv()
	if want := []string{"GOCACHE=/gatecache/gocache", "GOLANGCI_LINT_CACHE=/gatecache/lintcache"}; !reflect.DeepEqual(env, want) {
		t.Fatalf("CacheVolumeEnv = %q, want %q", env, want)
	}
	for _, rt := range []Runtime{cvDockerRT, cvPodmanRT} {
		prep, err := v.PrepareArgv(rt, testGateImage, 501, 20)
		if err != nil {
			t.Fatal(err)
		}
		check, err := v.CheckArgv(rt, testGateImage, 501, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, kv := range env {
			_, val, _ := strings.Cut(kv, "=")
			if !underRoot(val, MountGateCache) || val == MountGateCache {
				t.Errorf("%s: %s is not under %s", rt.Kind, kv, MountGateCache)
			}
			if idx(prep[idx(prep, testGateImage):], val) < 0 {
				t.Errorf("%s: %s is not created by PrepareArgv %q", rt.Kind, val, prep)
			}
			if idx(check[idx(check, testGateImage):], val) < 0 {
				t.Errorf("%s: %s is not probed by CheckArgv %q", rt.Kind, val, check)
			}
		}
	}
	// WithCacheVolumeEnv overrides ContainerEnv's per-exec pins, keeps every
	// other entry, does not mutate its input, and the service DSN applied
	// after it is still last.
	base := ContainerEnv([]string{"GOFLAGS=-mod=mod"}, nil)
	got := WithCacheVolumeEnv(base)
	if idx(got, "GOCACHE=/gocache") >= 0 || idx(got, "GOLANGCI_LINT_CACHE=/lintcache") >= 0 {
		t.Errorf("per-exec cache pins survived: %q", got)
	}
	if idx(got, "GOCACHE=/gatecache/gocache") < 0 || idx(got, "GOLANGCI_LINT_CACHE=/gatecache/lintcache") < 0 ||
		idx(got, "GOMODCACHE=/gomodcache") < 0 || idx(got, "GOFLAGS=-mod=mod") < 0 || idx(got, GateContainerMarker) < 0 {
		t.Errorf("WithCacheVolumeEnv = %q", got)
	}
	if len(got) != len(base) {
		t.Errorf("WithCacheVolumeEnv changed the entry count %d → %d: %q", len(base), len(got), got)
	}
	if idx(base, "GOCACHE=/gocache") < 0 {
		t.Error("WithCacheVolumeEnv mutated its input")
	}
	withSvc := WithServiceEnv(got, fixedService().GateEnv())
	if withSvc[len(withSvc)-1] != "FISHHAWK_TEST_PG_URL="+PostgresGateURL {
		t.Errorf("service DSN not last after the cache env: %q", withSvc)
	}
}
