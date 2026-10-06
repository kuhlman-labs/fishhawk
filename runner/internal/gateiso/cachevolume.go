package gateiso

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Persistent gate cache volume (E51.18 / #3967). The container path's GOCACHE
// and golangci-lint cache move out of the per-exec throwaway host directories
// into ONE runner-owned NAMED volume per runner PROCESS, mounted read-write
// at MountGateCache and reused by every later container exec of that process.
// A runner process serves exactly one stage of one run, so the poisoning
// boundary is the RUN: the name is crypto-random per process (never derived
// from shared state, never reused), and it also EMBEDS the owning run and
// stage ids so an exec for a different (run, stage) pair can tell the volume
// is not its own and mint its own instead (BelongsTo, #3967 approval
// condition 2). The module cache keeps its per-exec host-seeded posture
// (cache.go).
//
// Everything here is pure argv/env rendering; the runner executes the
// lifecycle per container exec: CreateArgv (idempotent on the name) →
// PrepareArgv (make the subdirectories owned by the gate uid) → CheckArgv
// (prove the gate uid can write them; #3967 approval condition 1) → the gate
// exec with ContainerSpec.CacheVolume set → RemoveArgv once at process exit.
// Any failed step DEGRADES to the per-exec caches; none is a refusal.

// CacheMode is the FISHHAWK_GATE_CACHE posture.
type CacheMode string

// Cache modes. CacheModeProcess (the default) mounts the per-process volume;
// CacheModeOff keeps the pre-#3967 per-exec cold caches.
const (
	CacheModeProcess CacheMode = "process"
	CacheModeOff     CacheMode = "off"
)

// ParseCacheMode parses FISHHAWK_GATE_CACHE (trimmed). Empty selects
// CacheModeProcess; an unknown value is an error naming it and the valid set
// — the runner surfaces that as a startup config error.
func ParseCacheMode(raw string) (CacheMode, error) {
	switch m := CacheMode(strings.TrimSpace(raw)); m {
	case "":
		return CacheModeProcess, nil
	case CacheModeProcess, CacheModeOff:
		return m, nil
	}
	return "", fmt.Errorf("unknown gate cache mode %q (valid: %s, %s)", strings.TrimSpace(raw), CacheModeProcess, CacheModeOff)
}

// Cache volume layout and the in-gate contract.
const (
	// CacheVolumeLabel marks every cache volume so one orphaned by a runner
	// SIGKILL is removable by label (`docker volume ls -q --filter
	// label=org.fishhawk.gate-cache`, only with no live runner).
	CacheVolumeLabel = "org.fishhawk.gate-cache"
	// MountGateCache is where the gate container sees the volume.
	MountGateCache = "/gatecache"
	// GateGoCache / GateLintCache are the two subdirectories PrepareArgv
	// creates and CacheVolumeEnv points GOCACHE / GOLANGCI_LINT_CACHE at.
	GateGoCache   = MountGateCache + "/gocache"
	GateLintCache = MountGateCache + "/lintcache"
	// cacheVolumePrefix starts every cache volume name.
	cacheVolumePrefix = "fishhawk-gate-cache-"
	// cacheWriteProbe is the file CheckArgv creates and removes in each
	// subdirectory to prove the gate uid can write it.
	cacheWriteProbe = ".fishhawk-write-probe"
)

// uuidExpr is a lower-case canonical UUID; the name embeds the run and stage
// ids in this form.
const uuidExpr = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

// cacheVolumePattern is the only shape a cache volume name — and therefore a
// ContainerSpec.CacheVolume — may take:
// fishhawk-gate-cache-<run uuid>-<stage uuid>-<12 hex>. It carries no '/',
// '.', ':' or upper case, so it can never name a host path, the daemon socket
// or a second mount field.
var cacheVolumePattern = regexp.MustCompile(`^` + cacheVolumePrefix + `(` + uuidExpr + `)-(` + uuidExpr + `)-[0-9a-f]{12}$`)

// uuidPattern validates one id handed to NewCacheVolume / BelongsTo.
var uuidPattern = regexp.MustCompile(`^` + uuidExpr + `$`)

// CacheVolume is one runner process's persistent gate cache volume.
type CacheVolume struct {
	Name string
}

// canonicalID lower-cases a trimmed id and reports whether it is a UUID.
func canonicalID(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	return id, uuidPattern.MatchString(id)
}

// NewCacheVolume mints a fresh cache volume owned by (runID, stageID): the
// name embeds both ids (lower-cased) and 6 crypto-random bytes, so two
// processes — a retried stage included — never share one. Ids that are not
// UUIDs are an error (the runner degrades to the per-exec caches): an
// unscoped volume would defeat BelongsTo.
func NewCacheVolume(runID, stageID string) (CacheVolume, error) {
	run, okRun := canonicalID(runID)
	stage, okStage := canonicalID(stageID)
	if !okRun || !okStage {
		return CacheVolume{}, fmt.Errorf("%w: cache volume owner (run %q, stage %q) must be two UUIDs", ErrContainerSpec, runID, stageID)
	}
	return CacheVolume{Name: cacheVolumePrefix + run + "-" + stage + "-" + randomHex(6)}, nil
}

// Owner returns the run and stage ids the name embeds; ok is false for a
// name not matching the cache volume pattern.
func (v CacheVolume) Owner() (runID, stageID string, ok bool) {
	m := cacheVolumePattern.FindStringSubmatch(v.Name)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// BelongsTo reports whether the volume is owned by (runID, stageID): the
// runtime guard an exec runs before reusing a minted volume, so an exec for a
// different pair creates its own instead (#3967 approval condition 2). A
// malformed name or a non-UUID id never belongs.
func (v CacheVolume) BelongsTo(runID, stageID string) bool {
	run, stage, ok := v.Owner()
	if !ok {
		return false
	}
	wantRun, okRun := canonicalID(runID)
	wantStage, okStage := canonicalID(stageID)
	return okRun && okStage && run == wantRun && stage == wantStage
}

// validate refuses a name not matching cacheVolumePattern.
func (v CacheVolume) validate() error {
	if !cacheVolumePattern.MatchString(v.Name) {
		return fmt.Errorf("%w: cache volume %q refused: not a gate cache volume (%s)", ErrContainerSpec, v.Name, cacheVolumePattern)
	}
	return nil
}

// argv validates the volume and runtime and returns `<bin> <endpoint…>
// <args…>`. A runtime with no validated socket, an unsupported kind or a
// malformed name renders nothing.
func (v CacheVolume) argv(rt Runtime, args ...string) ([]string, error) {
	bin := rt.Kind.Binary()
	if bin == "" {
		return nil, fmt.Errorf("%w: runtime kind %q", ErrContainerSpec, rt.Kind)
	}
	endpoint, err := rt.EndpointArgs()
	if err != nil {
		return nil, err
	}
	if err := v.validate(); err != nil {
		return nil, err
	}
	out := append([]string{bin}, endpoint...)
	return append(out, args...), nil
}

// helperArgv renders a contained one-shot helper container over the volume:
// no network, all capabilities dropped (plus capAdd), no-new-privileges, the
// given user pin, ONLY the volume mounted, entrypoint reset. An empty or
// flag-shaped image or a negative uid/gid renders nothing.
func (v CacheVolume) helperArgv(rt Runtime, image string, uid, gid int, capAdd string, user []string, cmd ...string) ([]string, error) {
	switch {
	case image == "" || strings.HasPrefix(image, "-"):
		return nil, fmt.Errorf("%w: cache helper image %q is empty or flag-shaped", ErrContainerSpec, image)
	case uid < 0 || gid < 0:
		return nil, fmt.Errorf("%w: cache helper uid/gid %d:%d must be non-negative", ErrContainerSpec, uid, gid)
	}
	args := []string{"run", "--rm", "--network=none", "--cap-drop=ALL"}
	if capAdd != "" {
		args = append(args, "--cap-add="+capAdd)
	}
	args = append(args, "--security-opt=no-new-privileges")
	args = append(args, user...)
	args = append(args, "-v", v.Name+":"+MountGateCache, "--entrypoint", "", image)
	return v.argv(rt, append(args, cmd...)...)
}

// CreateArgv creates the labelled volume. Docker's `volume create` re-uses an
// existing name without error; podman errors unless --ignore, so every exec
// can run it (a volume removed mid-stage is re-created, never auto-created by
// `run -v`).
func (v CacheVolume) CreateArgv(rt Runtime) ([]string, error) {
	args := []string{"volume", "create", "--label", CacheVolumeLabel + "=" + string(CacheModeProcess)}
	if rt.Kind == KindPodman {
		args = append(args, "--ignore")
	}
	return v.argv(rt, append(args, v.Name)...)
}

// PrepareArgv makes GateGoCache and GateLintCache exist, mode 0700, owned by
// the gate's uid:gid, before EVERY exec. A fresh named volume's root is
// root-owned, so a gate running --user uid:gid could not create them itself.
//
// Docker (and any non-rootless runtime): the helper runs as container root
// with exactly CAP_CHOWN added back and cachePrepareScript — `mkdir -p -m
// 0700` THEN `chown uid:gid`, the directories and the owner passed as
// positional arguments, never interpolated into the script. Not `install -d
// -o -g -m`: busybox install chowns BEFORE it chmods, and root without
// CAP_FOWNER cannot chmod a directory it no longer owns, so it failed EPERM
// on a fresh volume and on every re-prepare (caught live by fixture (o)).
// mkdir -p is a no-op on an existing directory, so a re-prepare only
// re-chowns.
// Rootless podman (#3967 approval condition 1): under --userns=keep-id
// container root maps to a SUBORDINATE uid, not the caller, so the helper
// runs AS the caller (`--userns=keep-id --user uid:gid`), with no capability
// and no -o/-g — the directories it creates are owned by the caller's uid by
// construction. Either way CheckArgv then proves the gate uid can write them.
func (v CacheVolume) PrepareArgv(rt Runtime, image string, uid, gid int) ([]string, error) {
	user := fmt.Sprintf("%d:%d", uid, gid)
	if rt.Kind == KindPodman && rt.Rootless {
		return v.helperArgv(rt, image, uid, gid, "", []string{"--userns=keep-id", "--user", user},
			"install", "-d", "-m", "0700", GateGoCache, GateLintCache)
	}
	return v.helperArgv(rt, image, uid, gid, "CHOWN", []string{"--user", "0:0"},
		"sh", "-c", cachePrepareScript, "sh", strconv.Itoa(uid)+":"+strconv.Itoa(gid), GateGoCache, GateLintCache)
}

// cachePrepareScript creates every directory argument after the first (mode
// 0700) and then chowns them to the first argument (uid:gid). The only
// capability it needs is CAP_CHOWN: root owns each directory it creates, so
// no chmod of a foreign-owned directory ever runs.
const cachePrepareScript = `o="$1"; shift; mkdir -p -m 0700 "$@" && chown "$o" "$@"`

// cacheWriteProbeScript creates and removes cacheWriteProbe in every
// directory argument; any failure is a non-zero exit.
const cacheWriteProbeScript = `set -e; for d in "$@"; do p="$d/` + cacheWriteProbe + `"; : > "$p"; rm -f "$p"; done`

// CheckArgv is the post-prepare ownership check (#3967 approval condition 1):
// running with the SAME user pin as the gate exec (userArgs), it creates and
// removes a probe file in GateGoCache and GateLintCache. A non-zero exit means
// the gate uid cannot write its cache, and the runner DEGRADES instead of
// letting the gate fail later as a misattributed red verify.
func (v CacheVolume) CheckArgv(rt Runtime, image string, uid, gid int) ([]string, error) {
	return v.helperArgv(rt, image, uid, gid, "", userArgs(rt, uid, gid),
		"sh", "-c", cacheWriteProbeScript, "sh", GateGoCache, GateLintCache)
}

// RemoveArgv removes the volume.
func (v CacheVolume) RemoveArgv(rt Runtime) ([]string, error) {
	return v.argv(rt, "volume", "rm", "-f", v.Name)
}

// CacheVolumeEnv is the gate env that points the build and lint caches into
// the volume.
func CacheVolumeEnv() []string {
	return []string{"GOCACHE=" + GateGoCache, "GOLANGCI_LINT_CACHE=" + GateLintCache}
}

// WithCacheVolumeEnv applies CacheVolumeEnv drop-then-append over a
// ContainerEnv result (before WithServiceEnv, which stays last). It never
// mutates env.
func WithCacheVolumeEnv(env []string) []string {
	out := append([]string(nil), env...)
	for _, kv := range CacheVolumeEnv() {
		out = dropThenAppend(out, kv)
	}
	return out
}
