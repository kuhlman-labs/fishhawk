package gateiso

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// Gate services (ADR-063 amendment gap 1, E51.4 / #2137). A daemon-dependent
// gate command (this repository's pgtest-backed backend suite) cannot start
// its own database inside the --network=none gate container, so the runner
// provisions a per-exec Postgres SERVICE container and shares ONLY its unix
// socket with the gate container, through one fresh named volume mounted
// read-only. Everything here is pure argv/env rendering; the runner executes
// the lifecycle (volume create → run -d → readiness → bootstrap → gate exec →
// rm -f -v → volume rm).

// Service names one runner-provisioned gate service.
type Service string

// ServicePostgres is the only supported gate service.
const ServicePostgres Service = "postgres"

// validServices is the closed set ParseServices accepts, in display order.
var validServices = []Service{ServicePostgres}

// ParseServices parses the FISHHAWK_GATE_SERVICES comma list. Members are
// trimmed, empty members ignored and duplicates collapsed; an empty list is
// nil (no services). An unknown member is an error naming it and the valid
// values — the runner surfaces that as a startup config error.
func ParseServices(raw string) ([]Service, error) {
	var out []Service
	for _, m := range strings.Split(raw, ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		svc := Service(m)
		known := false
		for _, v := range validServices {
			if svc == v {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("unknown gate service %q (valid: %s)", m, ServicePostgres)
		}
		dup := false
		for _, s := range out {
			if s == svc {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, svc)
		}
	}
	return out, nil
}

// DefaultPostgresImage is the service image when FISHHAWK_GATE_POSTGRES_IMAGE
// is unset — the image pgtest already starts. Operators should pin it by
// digest. An override MUST be PostgreSQL 16 or newer: the gate role holds
// CREATEROLE, which only 16 bounds, so BootstrapArgv refuses an older server
// (postgresMinVersionGuard).
const DefaultPostgresImage = "postgres:16-alpine"

// Service container layout and the in-gate contract.
const (
	// ServiceLabel marks every service container and volume so a leak (a
	// failed teardown, a runner SIGKILL mid-exec) is removable by label.
	ServiceLabel = "org.fishhawk.gate-service"
	// PostgresSocketDir is the service image's unix-socket directory; the
	// named volume is mounted there.
	PostgresSocketDir = "/var/run/postgresql"
	// PostgresDataDir is the image's declared VOLUME for PGDATA. A tmpfs is
	// mounted there so the daemon never creates an anonymous data volume;
	// PGDATA is a subdirectory because the tmpfs root is root-owned and the
	// service runs as the image's postgres user.
	PostgresDataDir = "/var/lib/postgresql/data"
	// MountPgSock is where the gate container sees the socket volume.
	MountPgSock = "/pgsock"
	// PostgresGateRole / PostgresGateDB are the least-privilege login role
	// and its database the gate's DSN connects as (never the superuser).
	PostgresGateRole = "fishhawk"
	PostgresGateDB   = "fishhawk"
	// PostgresGateURL is the FISHHAWK_TEST_PG_URL value pinned in the gate
	// container: the gate role over the read-only socket mount.
	PostgresGateURL = "postgres://" + PostgresGateRole + ":fishhawk@/" + PostgresGateDB + "?host=" + MountPgSock + "&sslmode=disable"
	// PostgresInitCompleteLine is printed by the image entrypoint only after
	// its temporary init server (which already answers pg_isready) stopped.
	PostgresInitCompleteLine = "PostgreSQL init process complete; ready for start up."
	// postgresSuperuser is the image's superuser; its password is per-service
	// random and appears only in the service container's env and the
	// bootstrap exec, never in the gate container.
	postgresSuperuser = "postgres"
	// postgresGateRoleAttributes is the gate role's posture (#2137 approval
	// condition 1, widened by #4050). The backend suite assumes the host
	// path's superuser: FORCE ROW LEVEL SECURITY tables seeded across accounts
	// need BYPASSRLS, and RLS tests create NOBYPASSRLS probe roles, which
	// needs CREATEROLE. NOSUPERUSER still closes the escape route: COPY … TO
	// PROGRAM and server-file access need superuser or pg_execute_server_program
	// / pg_read_server_files, and PostgreSQL 16's CREATEROLE cannot grant
	// SUPERUSER or a role it holds no ADMIN option on — a bound only 16
	// enforces, hence postgresMinVersionGuard. BYPASSRLS affects row
	// visibility only, inside a throwaway --network=none database. The
	// superuser password still never reaches the gate.
	postgresGateRoleAttributes = "LOGIN CREATEDB CREATEROLE BYPASSRLS NOSUPERUSER NOREPLICATION"
	// postgresMinVersionGuard is BootstrapArgv's first statement: it fails
	// the bootstrap (ON_ERROR_STOP) on a server older than PostgreSQL 16,
	// before the gate role exists. It is one argv token with no shell, so
	// the $$ quoting is literal.
	postgresMinVersionGuard = `DO $$BEGIN IF current_setting('server_version_num')::int < 160000 THEN RAISE EXCEPTION 'gate service Postgres must be 16 or newer (server_version_num %): the gate role holds CREATEROLE, which only PostgreSQL 16 bounds', current_setting('server_version_num'); END IF; END$$`
	// postgresInitdbArgs require a password on EVERY connection, the unix
	// socket included: the image's default `local all all trust` would let
	// the gate connect as the superuser without its password.
	postgresInitdbArgs = "--auth-local=scram-sha-256 --auth-host=scram-sha-256"
)

// serviceNamePattern is the only shape a service container/volume name — and
// therefore a ContainerSpec ServiceMount volume — may take.
var serviceNamePattern = regexp.MustCompile(`^fishhawk-gate-svc-[0-9a-f]{12}$`)

// PostgresService is one per-exec Postgres service. Name names BOTH the
// service container and its socket volume (separate runtime namespaces).
type PostgresService struct {
	Name  string
	Image string
	// superPassword is the per-service random superuser password.
	superPassword string
}

// NewPostgresService returns a service with a fresh random name and
// superuser password; an empty image selects DefaultPostgresImage.
func NewPostgresService(image string) PostgresService {
	if image == "" {
		image = DefaultPostgresImage
	}
	return PostgresService{
		Name:          "fishhawk-gate-svc-" + randomHex(6),
		Image:         image,
		superPassword: randomHex(16),
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("gateiso: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// argv validates the service and returns `<bin> <endpoint…> <args…>`. A
// runtime with no validated socket, a malformed name, an empty or
// flag-shaped image, or a missing superuser password renders nothing.
func (s PostgresService) argv(rt Runtime, args ...string) ([]string, error) {
	bin := rt.Kind.Binary()
	if bin == "" {
		return nil, fmt.Errorf("%w: runtime kind %q", ErrContainerSpec, rt.Kind)
	}
	endpoint, err := rt.EndpointArgs()
	if err != nil {
		return nil, err
	}
	switch {
	case !serviceNamePattern.MatchString(s.Name):
		return nil, fmt.Errorf("%w: service name %q does not match %s", ErrContainerSpec, s.Name, serviceNamePattern)
	case s.Image == "" || strings.HasPrefix(s.Image, "-"):
		return nil, fmt.Errorf("%w: service image %q is empty or flag-shaped", ErrContainerSpec, s.Image)
	case s.superPassword == "":
		return nil, fmt.Errorf("%w: service %q has no superuser password (use NewPostgresService)", ErrContainerSpec, s.Name)
	}
	out := append([]string{bin}, endpoint...)
	return append(out, args...), nil
}

// VolumeCreateArgv creates the labelled socket volume.
func (s PostgresService) VolumeCreateArgv(rt Runtime) ([]string, error) {
	return s.argv(rt, "volume", "create", "--label", ServiceLabel+"="+string(ServicePostgres), s.Name)
}

// RunArgv starts the detached service: no network, all capabilities dropped,
// no-new-privileges, the image's non-root postgres user, no published port,
// no host path — only the socket volume and a tmpfs PGDATA (so no anonymous
// data volume is created). Local connections require scram passwords.
func (s PostgresService) RunArgv(rt Runtime) ([]string, error) {
	return s.argv(rt, "run", "-d", "--name", s.Name,
		"--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--user", "postgres",
		"--label", ServiceLabel+"="+string(ServicePostgres),
		"--tmpfs", PostgresDataDir,
		"-e", "PGDATA="+PostgresDataDir+"/pgdata",
		"-e", "POSTGRES_USER="+postgresSuperuser,
		"-e", "POSTGRES_PASSWORD="+s.superPassword,
		"-e", "POSTGRES_INITDB_ARGS="+postgresInitdbArgs,
		"-v", s.Name+":"+PostgresSocketDir,
		s.Image,
	)
}

// LogsArgv reads the service log (PostgresInitComplete scans it).
func (s PostgresService) LogsArgv(rt Runtime) ([]string, error) {
	return s.argv(rt, "logs", s.Name)
}

// ReadyArgv probes the socket. pg_isready does not authenticate, and the
// image's temporary init server answers it too — so readiness is this probe
// succeeding AFTER PostgresInitComplete saw the init-complete line.
func (s PostgresService) ReadyArgv(rt Runtime) ([]string, error) {
	return s.argv(rt, "exec", s.Name, "pg_isready", "-h", PostgresSocketDir, "-U", postgresSuperuser, "-d", "postgres")
}

// BootstrapArgv creates the least-privilege gate role and its database as the
// superuser, inside the service container (the superuser password never
// leaves it). The PostgreSQL 16 guard runs first, so an older server creates
// no role. Each -c runs in its own transaction (CREATE DATABASE cannot run
// inside one); ON_ERROR_STOP makes any failure a non-zero exit.
func (s PostgresService) BootstrapArgv(rt Runtime) ([]string, error) {
	return s.argv(rt, "exec", "-e", "PGPASSWORD="+s.superPassword, s.Name,
		"psql", "-X", "-v", "ON_ERROR_STOP=1", "-h", PostgresSocketDir, "-U", postgresSuperuser, "-d", "postgres",
		"-c", postgresMinVersionGuard,
		"-c", "CREATE ROLE "+PostgresGateRole+" "+postgresGateRoleAttributes+" PASSWORD 'fishhawk'",
		"-c", "CREATE DATABASE "+PostgresGateDB+" OWNER "+PostgresGateRole,
	)
}

// RemoveArgv force-removes the service container and its anonymous volumes
// (-v: belt and braces should an overriding image declare another VOLUME).
func (s PostgresService) RemoveArgv(rt Runtime) ([]string, error) {
	return s.argv(rt, "rm", "-f", "-v", s.Name)
}

// VolumeRemoveArgv removes the socket volume.
func (s PostgresService) VolumeRemoveArgv(rt Runtime) ([]string, error) {
	return s.argv(rt, "volume", "rm", "-f", s.Name)
}

// GateMount is the read-only socket mount for the gate container.
func (s PostgresService) GateMount() ServiceMount {
	return ServiceMount{Volume: s.Name, Target: MountPgSock, ReadOnly: true}
}

// GateEnv is the gate container's service env, applied LAST (WithServiceEnv)
// so neither the sanitized env nor extras can redirect the DSN.
func (PostgresService) GateEnv() []string {
	return []string{"FISHHAWK_TEST_PG_URL=" + PostgresGateURL}
}

// PostgresInitComplete reports whether service logs carry the image's
// init-complete line. The runner reads the logs FIRST in each readiness
// iteration and runs ReadyArgv only once this is true.
func PostgresInitComplete(logs string) bool {
	return strings.Contains(logs, PostgresInitCompleteLine)
}
