package gateiso

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseServices(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    []Service
		wantErr bool
	}{
		{"", nil, false},
		{"postgres", []Service{ServicePostgres}, false},
		{" postgres ", []Service{ServicePostgres}, false},
		{"postgres,postgres", []Service{ServicePostgres}, false},
		{"postgres, ,", []Service{ServicePostgres}, false},
		{"redis", nil, true},
		{"postgres,redis", nil, true},
		{"Postgres", nil, true},
	} {
		got, err := ParseServices(tc.raw)
		if (err != nil) != tc.wantErr {
			t.Fatalf("ParseServices(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
		}
		if tc.wantErr {
			if got != nil || !strings.Contains(err.Error(), "valid: postgres") {
				t.Errorf("ParseServices(%q) = %q, %v; want nil and an error naming the valid values", tc.raw, got, err)
			}
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseServices(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// fixedService is a deterministic PostgresService for argv goldens.
func fixedService() PostgresService {
	return PostgresService{Name: "fishhawk-gate-svc-0a1b2c3d4e5f", Image: "postgres:16-alpine", superPassword: "s3cret"}
}

var dockerRT = Runtime{Kind: KindDocker, Safe: true, SocketPath: testSock}

func TestPostgresServiceArgv_Golden(t *testing.T) {
	s := fixedService()
	bind := []string{"docker", "--host", "unix://" + testSock}
	for _, tc := range []struct {
		name  string
		build func(Runtime) ([]string, error)
		want  []string
	}{
		{"volume create", s.VolumeCreateArgv, []string{"volume", "create", "--label", "org.fishhawk.gate-service=postgres", s.Name}},
		{"run", s.RunArgv, []string{"run", "-d", "--name", s.Name,
			"--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user", "postgres",
			"--label", "org.fishhawk.gate-service=postgres", "--tmpfs", "/var/lib/postgresql/data",
			"-e", "PGDATA=/var/lib/postgresql/data/pgdata", "-e", "POSTGRES_USER=postgres", "-e", "POSTGRES_PASSWORD=s3cret",
			"-e", "POSTGRES_INITDB_ARGS=--auth-local=scram-sha-256 --auth-host=scram-sha-256",
			"-v", s.Name + ":/var/run/postgresql", "postgres:16-alpine"}},
		{"logs", s.LogsArgv, []string{"logs", s.Name}},
		{"ready", s.ReadyArgv, []string{"exec", s.Name, "pg_isready", "-h", "/var/run/postgresql", "-U", "postgres", "-d", "postgres"}},
		{"bootstrap", s.BootstrapArgv, []string{"exec", "-e", "PGPASSWORD=s3cret", s.Name,
			"psql", "-X", "-v", "ON_ERROR_STOP=1", "-h", "/var/run/postgresql", "-U", "postgres", "-d", "postgres",
			"-c", "CREATE ROLE fishhawk LOGIN CREATEDB NOSUPERUSER NOCREATEROLE NOBYPASSRLS NOREPLICATION PASSWORD 'fishhawk'",
			"-c", "CREATE DATABASE fishhawk OWNER fishhawk"}},
		{"rm", s.RemoveArgv, []string{"rm", "-f", "-v", s.Name}},
		{"volume rm", s.VolumeRemoveArgv, []string{"volume", "rm", "-f", s.Name}},
	} {
		got, err := tc.build(dockerRT)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if want := append(append([]string(nil), bind...), tc.want...); !reflect.DeepEqual(got, want) {
			t.Errorf("%s argv\n got %q\nwant %q", tc.name, got, want)
		}
	}
	podman, err := s.RunArgv(Runtime{Kind: KindPodman, Safe: true, SocketPath: testSock})
	if err != nil || strings.Join(podman[:3], " ") != "podman --url unix://"+testSock {
		t.Errorf("podman RunArgv = %q, %v; want podman --url unix://%s …", podman, err, testSock)
	}
}

// TestPostgresServiceRunArgv_Containment pins the service's containment
// independent of the golden's token order: no network, no published port,
// exactly one -v (the socket volume, never a host path), a tmpfs PGDATA.
func TestPostgresServiceRunArgv_Containment(t *testing.T) {
	s := fixedService()
	argv, err := s.RunArgv(dockerRT)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges"} {
		if idx(argv, want) < 0 {
			t.Errorf("RunArgv lacks %s: %q", want, argv)
		}
	}
	var vols []string
	for i, tok := range argv {
		switch {
		case tok == "-p" || tok == "--publish" || strings.HasPrefix(tok, "--publish=") || tok == "-P":
			t.Errorf("RunArgv publishes a port: %q", argv)
		case tok == "-v" && i+1 < len(argv):
			vols = append(vols, argv[i+1])
		}
	}
	if len(vols) != 1 || vols[0] != s.Name+":"+PostgresSocketDir {
		t.Errorf("RunArgv volumes = %q, want exactly the socket volume", vols)
	}
	if i := idx(argv, "--tmpfs"); i < 0 || argv[i+1] != PostgresDataDir {
		t.Errorf("RunArgv lacks the tmpfs PGDATA (no anonymous data volume): %q", argv)
	}
}

func TestServiceArgv_Refusals(t *testing.T) {
	builders := func(s PostgresService) map[string]func(Runtime) ([]string, error) {
		return map[string]func(Runtime) ([]string, error){
			"volume create": s.VolumeCreateArgv, "run": s.RunArgv, "logs": s.LogsArgv, "ready": s.ReadyArgv,
			"bootstrap": s.BootstrapArgv, "rm": s.RemoveArgv, "volume rm": s.VolumeRemoveArgv,
		}
	}
	bad := func(mut func(*PostgresService)) PostgresService {
		s := fixedService()
		mut(&s)
		return s
	}
	for _, tc := range []struct {
		name string
		svc  PostgresService
		rt   Runtime
	}{
		{"unbound runtime", fixedService(), Runtime{Kind: KindDocker, Safe: true}},
		{"no runtime", fixedService(), Runtime{Kind: KindNone, SocketPath: testSock}},
		{"host path name", bad(func(s *PostgresService) { s.Name = "/var/run" }), dockerRT},
		{"socket name", bad(func(s *PostgresService) { s.Name = "docker.sock" }), dockerRT},
		{"uppercase name", bad(func(s *PostgresService) { s.Name = "fishhawk-gate-svc-0A1B2C3D4E5F" }), dockerRT},
		{"empty image", bad(func(s *PostgresService) { s.Image = "" }), dockerRT},
		{"flag image", bad(func(s *PostgresService) { s.Image = "--privileged" }), dockerRT},
		{"no password", bad(func(s *PostgresService) { s.superPassword = "" }), dockerRT},
	} {
		for op, build := range builders(tc.svc) {
			argv, err := build(tc.rt)
			if !errors.Is(err, ErrContainerSpec) || argv != nil {
				t.Errorf("%s / %s: argv %q, err %v; want nil argv and ErrContainerSpec", tc.name, op, argv, err)
			}
		}
	}
}

func TestNewPostgresService(t *testing.T) {
	a, b := NewPostgresService(""), NewPostgresService("postgres@sha256:abc")
	if !serviceNamePattern.MatchString(a.Name) || !serviceNamePattern.MatchString(b.Name) || a.Name == b.Name {
		t.Errorf("names %q, %q: want two distinct fishhawk-gate-svc-<12 hex>", a.Name, b.Name)
	}
	if a.Image != DefaultPostgresImage || b.Image != "postgres@sha256:abc" {
		t.Errorf("images %q, %q", a.Image, b.Image)
	}
	if len(a.superPassword) != 32 || a.superPassword == b.superPassword {
		t.Errorf("superuser passwords not fresh 32-hex: %q %q", a.superPassword, b.superPassword)
	}
	if got := a.GateMount(); got != (ServiceMount{Volume: a.Name, Target: MountPgSock, ReadOnly: true}) {
		t.Errorf("GateMount = %+v", got)
	}
	env := a.GateEnv()
	if !reflect.DeepEqual(env, []string{"FISHHAWK_TEST_PG_URL=postgres://fishhawk:fishhawk@/fishhawk?host=/pgsock&sslmode=disable"}) {
		t.Errorf("GateEnv = %q", env)
	}
	if strings.Contains(strings.Join(env, " "), a.superPassword) {
		t.Error("superuser password leaked into the gate env")
	}
}

// TestPostgresInitComplete: the image's temporary init server answers
// pg_isready BEFORE the init-complete line, so readiness keys on the line.
func TestPostgresInitComplete(t *testing.T) {
	during := "creating configuration files ... ok\nwaiting for server to start....2024 LOG:  database system is ready to accept connections\n done\nserver started\n"
	if PostgresInitComplete(during) {
		t.Error("temporary init server output counted as init complete")
	}
	if !PostgresInitComplete(during + "\nPostgreSQL init process complete; ready for start up.\n\n") {
		t.Error("init-complete line not recognised")
	}
}

// --- live (docker) ----------------------------------------------------------

// liveRuntime returns a SAFE detected runtime with the service image present,
// or skips naming why.
func liveRuntime(t *testing.T, image string) (Runtime, []string) {
	t.Helper()
	if testing.Short() || os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		t.Skip("live gate-service test skipped (-short / FISHHAWK_SKIP_INTEGRATION)")
	}
	rt := DetectRuntime(context.Background(), DefaultProbes())
	if !rt.Safe {
		t.Skipf("no safe container runtime: %s", rt.Reason)
	}
	env, err := rt.BindEndpointEnv(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	ep, _ := rt.EndpointArgs()
	inspect := append(append([]string{}, ep...), "image", "inspect", image)
	if _, err := runLive(t, rt, env, 30*time.Second, inspect...); err != nil {
		pull := append(append([]string{}, ep...), "pull", image)
		if out, err := runLive(t, rt, env, 3*time.Minute, pull...); err != nil {
			t.Skipf("service image %s unavailable: %v: %s", image, err, out)
		}
	}
	return rt, env
}

func runLive(t *testing.T, rt Runtime, env []string, d time.Duration, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, rt.Kind.Binary(), args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runArgv(t *testing.T, env []string, d time.Duration, argv []string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	out, rerr := cmd.CombinedOutput()
	return string(out), rerr
}

// TestPostgresService_LiveLeastPrivilegeOverReadOnlySocket drives the real
// service lifecycle through this file's argv builders and a gate container
// rendered by BuildArgv (passwd file + read-only socket mount + service env),
// then asserts from the gate's DSN: SELECT and CREATE DATABASE work, while
// CREATE ROLE, COPY … TO PROGRAM and a passwordless superuser connection are
// refused, /pgsock is read-only, the caller uid resolves and the marker is
// pinned. Host side: the service carries exactly one mount (the named socket
// volume — no anonymous PGDATA volume, no bind), and teardown leaves neither
// the container nor the volume.
func TestPostgresService_LiveLeastPrivilegeOverReadOnlySocket(t *testing.T) {
	svc := NewPostgresService("")
	rt, env := liveRuntime(t, svc.Image)
	ep, _ := rt.EndpointArgs()
	host := func(args ...string) string {
		out, _ := runLive(t, rt, env, 30*time.Second, append(append([]string{}, ep...), args...)...)
		return strings.TrimSpace(out)
	}
	if out, err := runArgv(t, env, time.Minute, must(t)(svc.VolumeCreateArgv(rt))); err != nil {
		t.Fatalf("volume create: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_, _ = runArgv(t, env, time.Minute, must(t)(svc.RemoveArgv(rt)))
		_, _ = runArgv(t, env, time.Minute, must(t)(svc.VolumeRemoveArgv(rt)))
	})
	if out, err := runArgv(t, env, 3*time.Minute, must(t)(svc.RunArgv(rt))); err != nil {
		t.Fatalf("service run: %v: %s", err, out)
	}
	ready := false
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		logs, _ := runArgv(t, env, 30*time.Second, must(t)(svc.LogsArgv(rt)))
		if !PostgresInitComplete(logs) {
			continue
		}
		if _, err := runArgv(t, env, 30*time.Second, must(t)(svc.ReadyArgv(rt))); err == nil {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("service never ready: %s", host("logs", svc.Name))
	}
	if out, err := runArgv(t, env, time.Minute, must(t)(svc.BootstrapArgv(rt))); err != nil {
		t.Fatalf("bootstrap: %v: %s", err, out)
	}
	mounts := host("inspect", "-f", "{{range .Mounts}}{{.Type}}:{{.Name}}:{{.Destination}};{{end}}", svc.Name)
	if mounts != "volume:"+svc.Name+":"+PostgresSocketDir+";" {
		t.Errorf("service mounts = %q, want exactly the named socket volume (no anonymous data volume, no bind)", mounts)
	}
	if nm := host("inspect", "-f", "{{.HostConfig.NetworkMode}}", svc.Name); nm != "none" {
		t.Errorf("service NetworkMode = %q, want none", nm)
	}

	// The gate container, rendered by BuildArgv.
	m := newMounts(t)
	passwdOut, err := runArgv(t, env, time.Minute, must(t)(PasswdReadArgv(rt, svc.Image, os.Getuid(), os.Getgid())))
	if err != nil {
		t.Fatalf("passwd read: %v: %s", err, passwdOut)
	}
	pwFile, err := WritePasswdFile(m.root, BuildPasswd([]byte(passwdOut), os.Getuid(), os.Getgid(), "gatecaller"))
	if err != nil {
		t.Fatal(err)
	}
	script := `psql "$FISHHAWK_TEST_PG_URL" -tAc 'select 1'; echo "sel=$?"
psql "$FISHHAWK_TEST_PG_URL" -qtAc 'create database fh_live_probe'; echo "createdb=$?"
psql "$FISHHAWK_TEST_PG_URL" -tAc 'create role fh_live_x'; echo "createrole=$?"
psql "$FISHHAWK_TEST_PG_URL" -tAc "copy (select 1) to program 'true'"; echo "copyprogram=$?"
psql -w 'postgres://postgres@/postgres?host=/pgsock&sslmode=disable' -tAc 'select 1'; echo "superuser=$?"
touch /pgsock/planted; echo "touch=$?"
echo "user=$(id -un)" "marker=$FISHHAWK_GATE_CONTAINER"`
	spec := ContainerSpec{
		Runtime: rt, Image: svc.Image, Name: NewContainerName(),
		Checkout: m.checkout, GoCache: m.gocache, GoModCache: m.gomod, LintCache: m.lint,
		PasswdFile: pwFile, ServiceMounts: []ServiceMount{svc.GateMount()},
		UID: os.Getuid(), GID: os.Getgid(),
		Env:  WithServiceEnv(ContainerEnv(nil, []string{"FISHHAWK_TEST_PG_URL=postgres://attacker@evil/x"}), svc.GateEnv()),
		Argv: []string{"sh", "-c", script},
	}
	out, _ := runArgv(t, env, 2*time.Minute, must(t)(spec.BuildArgv(MountPolicy{Permitted: []string{m.root}, DaemonSocket: rt.SocketPath})))
	for _, want := range []string{"sel=0", "createdb=0", "createrole=1", "permission denied to create role",
		"copyprogram=1", "pg_execute_server_program", "superuser=2", "touch=1", "user=gatecaller", "marker=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("gate output lacks %q:\n%s", want, out)
		}
	}

	if out, err := runArgv(t, env, time.Minute, must(t)(svc.RemoveArgv(rt))); err != nil {
		t.Fatalf("rm: %v: %s", err, out)
	}
	if out, err := runArgv(t, env, time.Minute, must(t)(svc.VolumeRemoveArgv(rt))); err != nil {
		t.Fatalf("volume rm: %v: %s", err, out)
	}
	if left := host("ps", "-aq", "--filter", "name="+svc.Name); left != "" {
		t.Errorf("service container left after teardown: %q", left)
	}
	if left := host("volume", "ls", "-q", "--filter", "name="+svc.Name); left != "" {
		t.Errorf("service volume left after teardown: %q", left)
	}
}

// must fails the test on a render error and returns the argv.
func must(t *testing.T) func([]string, error) []string {
	return func(argv []string, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return argv
	}
}
