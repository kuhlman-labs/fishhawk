package hosthealth

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// GateContainerPrefix is the name prefix of every Fishhawk gate container
// (fishhawk-gate-<hex>) and gate service container (fishhawk-gate-svc-<hex>),
// as runner/internal/gateiso names them (container.go, service.go). A rename
// there silently drops those containers from Containers, so the prefix is
// this one constant.
const GateContainerPrefix = "fishhawk-gate-"

// ErrRuntimeAbsent is returned by Containers when the docker CLI is not on
// PATH (a podman-only host included). Callers map it to unknown.
var ErrRuntimeAbsent = errors.New("hosthealth: docker CLI not on PATH")

// Container is one running Fishhawk gate or service container.
type Container struct {
	Name        string
	MemoryBytes uint64
}

// Containers lists the running Fishhawk gate and service containers with
// their memory use, largest first. Zero containers is a valid reading. The
// docker name filter is a substring match, so every listed name is
// re-checked here against the exact GateContainerPrefix before docker stats
// is asked about it. Like every Reader method it is gated on the OS.
func (r Reader) Containers(ctx context.Context) ([]Container, error) {
	if _, err := r.platform(); err != nil {
		return nil, err
	}
	run := r.exec()
	out, err := run(ctx, "docker", "ps", "--filter", "name="+GateContainerPrefix, "--format", "{{.Names}}")
	if err != nil {
		return nil, dockerErr("docker ps", err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); strings.HasPrefix(name, GateContainerPrefix) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	args := append([]string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}"}, names...)
	out, err = run(ctx, "docker", args...)
	if err != nil {
		return nil, dockerErr("docker stats", err)
	}
	cs, err := parseDockerStats(string(out))
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(cs, func(a, b Container) int {
		if c := cmp.Compare(b.MemoryBytes, a.MemoryBytes); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return cs, nil
}

func dockerErr(what string, err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return ErrRuntimeAbsent
	}
	return fmt.Errorf("%s failed: %w", what, err)
}

// parseDockerStats parses `{{.Name}}\t{{.MemUsage}}` lines, MemUsage being
// `<usage> / <limit>`. docker stats reports only the names it was given,
// which Containers already prefix-filtered.
func parseDockerStats(s string) ([]Container, error) {
	var cs []Container
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, usage, ok := strings.Cut(line, "\t")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("docker stats: malformed line %q", SanitizeName(line))
		}
		used, _, _ := strings.Cut(usage, "/")
		b, err := parseDockerSize(used)
		if err != nil {
			return nil, fmt.Errorf("docker stats %s: %w", SanitizeName(name), err)
		}
		cs = append(cs, Container{Name: SanitizeName(name), MemoryBytes: b})
	}
	return cs, nil
}

// dockerUnits maps docker's size units: binary (KiB, MiB, GiB, TiB — what
// MemUsage prints) and decimal (kB, MB, GB, TB).
var dockerUnits = map[string]float64{
	"B":   1,
	"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40,
	"KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
}

func parseDockerSize(s string) (uint64, error) {
	t := strings.TrimSpace(s)
	i := 0
	for i < len(t) && (t[i] == '.' || (t[i] >= '0' && t[i] <= '9')) {
		i++
	}
	v, err := strconv.ParseFloat(t[:i], 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("malformed size %q", t)
	}
	mult, ok := dockerUnits[strings.ToUpper(strings.TrimSpace(t[i:]))]
	if !ok {
		return 0, fmt.Errorf("unknown size unit in %q", t)
	}
	return uint64(v * mult), nil
}
