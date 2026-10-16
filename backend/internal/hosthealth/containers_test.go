package hosthealth

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

const dockerPSCommand = "docker ps --filter name=fishhawk-gate- --format {{.Names}}"

func dockerStatsCommand(names ...string) string {
	return "docker stats --no-stream --format {{.Name}}\t{{.MemUsage}} " + strings.Join(names, " ")
}

// dockerFake answers `docker ps` with psOut and `docker stats <names>` with
// one line per REQUESTED name from stats, the way the real CLI does, so a
// name that slips past the prefix filter shows up in the reading.
type dockerFake struct {
	psOut      string
	psErr      error
	stats      map[string]string
	statsErr   error
	statsCalls [][]string
}

func (d *dockerFake) run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "docker" || len(args) == 0 {
		return nil, errors.New("unexpected command " + name)
	}
	switch args[0] {
	case "ps":
		if strings.Join(append([]string{name}, args...), " ") != dockerPSCommand {
			return nil, errors.New("unexpected ps argv")
		}
		return []byte(d.psOut), d.psErr
	case "stats":
		want := []string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}"}
		if len(args) < len(want) || strings.Join(args[:len(want)], "|") != strings.Join(want, "|") {
			return nil, errors.New("unexpected stats argv")
		}
		names := args[len(want):]
		d.statsCalls = append(d.statsCalls, names)
		if d.statsErr != nil {
			return nil, d.statsErr
		}
		var b strings.Builder
		for _, n := range names {
			b.WriteString(n + "\t" + d.stats[n] + "\n")
		}
		return []byte(b.String()), nil
	}
	return nil, errors.New("unexpected docker subcommand")
}

// TestContainers_FiltersByExactPrefix: docker's name filter is a substring
// match, so the fake lists an unrelated postgres-dev (30GiB) and a
// my-fishhawk-gate-x; only the exact-prefix gate containers are read.
func TestContainers_FiltersByExactPrefix(t *testing.T) {
	d := &dockerFake{
		psOut: "fishhawk-gate-0a1b2c3d4e5f\npostgres-dev\nfishhawk-gate-svc-aabbccddeeff\nmy-fishhawk-gate-x\n\n",
		stats: map[string]string{
			"fishhawk-gate-0a1b2c3d4e5f":     "1.5GiB / 15.6GiB",
			"fishhawk-gate-svc-aabbccddeeff": "512MiB / 15.6GiB",
			"postgres-dev":                   "30GiB / 64GiB",
			"my-fishhawk-gate-x":             "20GiB / 64GiB",
		},
	}
	cs, err := Reader{GOOS: "linux", Exec: d.run}.Containers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Container{
		{Name: "fishhawk-gate-0a1b2c3d4e5f", MemoryBytes: 3 << 29},
		{Name: "fishhawk-gate-svc-aabbccddeeff", MemoryBytes: 512 << 20},
	}
	if len(cs) != len(want) || cs[0] != want[0] || cs[1] != want[1] {
		t.Fatalf("Containers = %+v, want %+v", cs, want)
	}
	if len(d.statsCalls) != 1 || len(d.statsCalls[0]) != 2 {
		t.Errorf("docker stats asked about %v, want only the two gate containers", d.statsCalls)
	}
}

func TestContainers_NoneRunning(t *testing.T) {
	d := &dockerFake{psOut: "\n"}
	cs, err := Reader{GOOS: "darwin", Exec: d.run}.Containers(context.Background())
	if err != nil || len(cs) != 0 {
		t.Fatalf("no containers = %+v, %v; want an empty reading", cs, err)
	}
	if len(d.statsCalls) != 0 {
		t.Error("docker stats ran with no gate containers")
	}
}

func TestContainers_DockerAbsent(t *testing.T) {
	notFound := &exec.Error{Name: "docker", Err: exec.ErrNotFound}
	d := &dockerFake{psErr: notFound}
	if _, err := (Reader{GOOS: "linux", Exec: d.run}).Containers(context.Background()); !errors.Is(err, ErrRuntimeAbsent) {
		t.Errorf("docker absent at ps: %v, want ErrRuntimeAbsent", err)
	}
	d = &dockerFake{psOut: "fishhawk-gate-0a1b2c3d4e5f\n", statsErr: notFound}
	if _, err := (Reader{GOOS: "linux", Exec: d.run}).Containers(context.Background()); !errors.Is(err, ErrRuntimeAbsent) {
		t.Errorf("docker absent at stats: %v, want ErrRuntimeAbsent", err)
	}
}

// TestContainers_DockerAbsentRealExec drives the default exec with a PATH
// that holds no docker, so the real exec.ErrNotFound shape is mapped.
func TestContainers_DockerAbsentRealExec(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("no readers for %s", runtime.GOOS)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := (Reader{}).Containers(context.Background()); !errors.Is(err, ErrRuntimeAbsent) {
		t.Errorf("Containers with no docker on PATH: %v, want ErrRuntimeAbsent", err)
	}
}

func TestContainers_Errors(t *testing.T) {
	cases := map[string]*dockerFake{
		"ps fails (daemon down)": {psErr: errors.New("exit status 1")},
		"stats fails":            {psOut: "fishhawk-gate-0a1b2c3d4e5f\n", statsErr: errors.New("exit status 1")},
		"stats bad unit":         {psOut: "fishhawk-gate-0a1b2c3d4e5f\n", stats: map[string]string{"fishhawk-gate-0a1b2c3d4e5f": "1.5QiB / 2GiB"}},
		"stats empty usage":      {psOut: "fishhawk-gate-0a1b2c3d4e5f\n", stats: map[string]string{"fishhawk-gate-0a1b2c3d4e5f": ""}},
	}
	for name, d := range cases {
		cs, err := Reader{GOOS: "linux", Exec: d.run}.Containers(context.Background())
		if err == nil || errors.Is(err, ErrRuntimeAbsent) {
			t.Errorf("%s: Containers = %+v, %v; want a non-absent error", name, cs, err)
		}
	}
}

func TestParseDockerStats(t *testing.T) {
	if _, err := parseDockerStats("fishhawk-gate-a 1GiB / 2GiB\n"); err == nil {
		t.Error("a line with no tab parsed")
	}
	if _, err := parseDockerStats("\t1GiB / 2GiB\n"); err == nil {
		t.Error("a line with no name parsed")
	}
	cs, err := parseDockerStats("fishhawk-gate-a\x1b]0;x\x07\t1GiB / 2GiB\n")
	if err != nil || len(cs) != 1 || strings.ContainsAny(cs[0].Name, "\x1b\x07") {
		t.Errorf("control characters in a name = %+v, %v; want them stripped", cs, err)
	}
}

func TestParseDockerSize(t *testing.T) {
	good := map[string]uint64{
		"0B": 0, "12B": 12, "512KiB": 512 << 10, "1.5GiB": 3 << 29, "100MiB": 100 << 20, "1TiB": 1 << 40,
		"7kB": 7000, "2MB": 2e6, "3GB": 3e9, "1TB": 1e12, " 64MiB ": 64 << 20,
	}
	for in, want := range good {
		if got, err := parseDockerSize(in); err != nil || got != want {
			t.Errorf("parseDockerSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "GiB", "12", "1.5QiB", "-3MiB", "1..2GiB"} {
		if got, err := parseDockerSize(in); err == nil {
			t.Errorf("parseDockerSize(%q) = %d, want an error", in, got)
		}
	}
}
