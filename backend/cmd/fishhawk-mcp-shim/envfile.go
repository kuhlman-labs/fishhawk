package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Host settings from the checkout .env (#5592). When the shim's own
// environment lacks FISHHAWK_HOST_LABEL or FISHHAWK_HOST_CAPACITY, it sources
// them from the bound checkout's .env into every fishhawk-mcp child it spawns,
// so a session and the host dispatcher scripts/dev starts from the same file
// agree on the label and capacity. The file is evaluated by zsh itself, never
// parsed in Go: scripts/dev runs `set -a; source .env`, and a child of
// scripts/dev sees exactly what zsh exports, so zsh is the only faithful
// evaluator. Only the two allowlisted keys are ever read back.
const (
	hostLabelKey    = "FISHHAWK_HOST_LABEL"
	hostCapacityKey = "FISHHAWK_HOST_CAPACITY"

	// dotenvFrameMagic opens the NUL-separated frame the evaluator prints.
	dotenvFrameMagic = "fishhawk-dotenv-v1"
	// dotenvFrameFields is the field count of a well-formed frame: the magic,
	// a type and a value per key, and the empty tail after the final NUL.
	dotenvFrameFields = 6
	// dotenvStdoutMax caps what the evaluator may print.
	dotenvStdoutMax = 4 << 10
	// dotenvTimeout bounds one evaluation.
	dotenvTimeout = 5 * time.Second
	// dotenvWaitDelay bounds Wait once the evaluator has been killed or has
	// exited while something it started still holds the stdout pipe.
	dotenvWaitDelay = time.Second

	// maxDotenvCapacity mirrors mcpserver's maxHostCapacity: a sourced
	// capacity outside 1..64 is skipped, never passed to the child.
	maxDotenvCapacity = 64

	// dotenvScript sources the file with allexport, its own stdio pointed at
	// the null device, then prints the frame. ${(t)KEY} is empty for an unset
	// key and names the parameter type otherwise; dotenvExported keeps only an
	// exported scalar, integer or float. `builtin` bypasses any print function
	// or alias the file defines, and print -N NUL-separates and NUL-terminates
	// the fields.
	dotenvScript = `set -a; source "$1" >/dev/null 2>&1 </dev/null; builtin print -rN -- ` + dotenvFrameMagic +
		` "${(t)` + hostLabelKey + `-}" "${` + hostLabelKey + `-}"` +
		` "${(t)` + hostCapacityKey + `-}" "${` + hostCapacityKey + `-}"`
)

// dotenvKeys is the allowlist, in frame order. No other .env key can reach a
// child: the frame carries only these.
var dotenvKeys = [...]string{hostLabelKey, hostCapacityKey}

// dotenvOpts is how readDotenv evaluates a file. shell is resolved through
// PATH (tests point it at a fake /bin/sh script); owner reports a file's
// owning uid (tests substitute a foreign owner); waitDelay is cmd.WaitDelay
// (a test lengthens it so only the timeout's group kill bounds the return).
type dotenvOpts struct {
	shell     string
	timeout   time.Duration
	waitDelay time.Duration
	owner     func(fs.FileInfo) (int, bool)
}

var defaultDotenvOpts = dotenvOpts{shell: "zsh", timeout: dotenvTimeout, waitDelay: dotenvWaitDelay, owner: statOwner}

// statOwner reads the owning uid from info.Sys(). The *syscall.Stat_t type
// exists only on unix GOOS and this file carries no build tag, so the Uid
// field is read by name. A GOOS with no such field reports false, and the
// file is then refused as not owned by the current user.
func statOwner(info fs.FileInfo) (int, bool) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return 0, false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, false
	}
	f := v.FieldByName("Uid")
	if !f.IsValid() || !f.CanUint() {
		return 0, false
	}
	return int(f.Uint()), true
}

// readDotenv evaluates the .env at path with zsh and returns the allowlisted
// keys zsh exports. A missing file returns nil silently. Every other refusal
// writes exactly one warning naming the file and the reason, never a value or
// any text of the file, and returns nil.
func readDotenv(path string, o dotenvOpts, warn io.Writer) map[string]string {
	refuse := func(reason string) map[string]string {
		_, _ = fmt.Fprintf(warn, "fishhawk-mcp-shim: %s: %s; sourcing nothing from this file\n", path, reason)
		return nil
	}

	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return refuse("cannot stat it")
	}
	// A FIFO or device is never opened, so it can never block a spawn.
	if !info.Mode().IsRegular() {
		return refuse("not a regular file")
	}
	owner := o.owner
	if owner == nil {
		owner = statOwner
	}
	if uid, ok := owner(info); !ok || uid != os.Getuid() {
		return refuse("not owned by the current user")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return refuse("group- or world-writable")
	}

	shell, err := exec.LookPath(o.shell)
	if err != nil {
		return refuse("zsh not found on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-f", "-c", dotenvScript, "fishhawk-mcp-shim", path)
	cmd.Env = dotenvShellEnv()
	// Stdin and Stderr stay nil, the null device: the file's diagnostics are
	// discarded, so no .env text reaches the shim's stderr.
	out := &capWriter{max: dotenvStdoutMax}
	cmd.Stdout = out
	setPgid(cmd)
	// Kill the whole group on timeout, so a sleeping grandchild dies too.
	cmd.Cancel = func() error { signalKillGroup(cmd); return nil }
	// Bound Wait when something outside the group still holds stdout.
	cmd.WaitDelay = o.waitDelay

	runErr := cmd.Run()
	// Reap the group on every path, not only the timeout: anything the file
	// backgrounded would otherwise outlive this evaluation, once per spawn.
	// The leader is already waited, so a group with no member left is ESRCH.
	signalKillGroup(cmd)
	if runErr != nil {
		if ctx.Err() != nil {
			return refuse(fmt.Sprintf("timed out after %s", o.timeout))
		}
		if errors.Is(runErr, exec.ErrWaitDelay) {
			return refuse("left a process holding its output open")
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return refuse(fmt.Sprintf("exited unsuccessfully (exit status %d)", exitErr.ExitCode()))
		}
		return refuse("exited unsuccessfully")
	}
	if out.overflow {
		return refuse(fmt.Sprintf("printed more than %d KiB", dotenvStdoutMax>>10))
	}
	got, ok := parseDotenvFrame(out.buf)
	if !ok {
		return refuse("unexpected output from zsh (the file may call exit, print from a trap, or hold a NUL byte)")
	}
	return got
}

// dotenvShellEnv is the evaluator's whole environment: PATH and HOME, each
// only when set. No FISHHAWK_* variable of the shim's reaches the file, so an
// inherited value can never masquerade as a sourced one.
func dotenvShellEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// capWriter keeps at most max bytes and records an overflow. It always
// reports a full write, so the evaluator never sees a short write.
type capWriter struct {
	max      int
	buf      []byte
	overflow bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if len(w.buf)+len(p) > w.max {
		w.overflow = true
		if room := w.max - len(w.buf); room > 0 {
			w.buf = append(w.buf, p[:room]...)
		}
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// parseDotenvFrame decodes the evaluator's frame. It refuses anything but
// exactly dotenvFrameFields NUL-separated fields opening with the magic and
// ending in the empty tail, and records a key only when zsh typed it as an
// exported scalar, integer or float.
func parseDotenvFrame(out []byte) (map[string]string, bool) {
	fields := strings.Split(string(out), "\x00")
	if len(fields) != dotenvFrameFields || fields[0] != dotenvFrameMagic || fields[len(fields)-1] != "" {
		return nil, false
	}
	got := map[string]string{}
	for i, key := range dotenvKeys {
		typ, value := fields[1+2*i], fields[2+2*i]
		if dotenvExported(typ) {
			got[key] = value
		}
	}
	return got, true
}

// dotenvExported reports whether a ${(t)KEY} type is one an exec'd child
// would see: an exported scalar, integer or float. zsh types an exported
// array array-export but never exports it, a `typeset +x` key is a plain
// scalar, and an unset key has an empty type.
func dotenvExported(typ string) bool {
	parts := strings.Split(typ, "-")
	switch parts[0] {
	case "scalar", "integer", "float":
	default:
		return false
	}
	return slices.Contains(parts[1:], "export")
}

// mergeDotenv appends the sourced keys the base environment lacks. Presence,
// not non-emptiness, makes a base value explicit: a registration that sets
// FISHHAWK_HOST_LABEL= blocks sourcing. An empty sourced value and an invalid
// capacity are skipped. The one summary line names keys and the file, never
// values.
func mergeDotenv(base []string, sourced map[string]string, file string, warn io.Writer) []string {
	present := make(map[string]struct{}, len(base))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		present[k] = struct{}{}
	}
	env := slices.Clone(base)
	var added []string
	for _, key := range slices.Sorted(maps.Keys(sourced)) {
		value := sourced[key]
		if _, explicit := present[key]; explicit {
			continue
		}
		if value == "" {
			continue
		}
		if key == hostCapacityKey && !validDotenvCapacity(value) {
			_, _ = fmt.Fprintf(warn, "fishhawk-mcp-shim: %s: %s is not an integer in 1..%d; not sourcing it\n", file, key, maxDotenvCapacity)
			continue
		}
		env = append(env, key+"="+value)
		added = append(added, key)
	}
	if len(added) > 0 {
		_, _ = fmt.Fprintf(warn, "fishhawk-mcp-shim: sourced %s from %s\n", strings.Join(added, ", "), file)
	}
	return env
}

// validDotenvCapacity mirrors mcpserver.resolveHostCapacity: trimmed, parsed
// with strconv.Atoi, and in 1..64.
func validDotenvCapacity(value string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	return err == nil && n >= 1 && n <= maxDotenvCapacity
}

// childEnv is the environment for one child spawn: base plus whatever the
// env file sources. An empty envFile disables sourcing.
func childEnv(base []string, envFile string, o dotenvOpts, warn io.Writer) []string {
	if envFile == "" {
		return base
	}
	return mergeDotenv(base, readDotenv(envFile, o, warn), envFile, warn)
}

// defaultEnvFile is <dir of the shim executable>/../.env, the checkout .env
// beside the standard bin/ layout, or "" when the executable path is
// unavailable.
func defaultEnvFile(executable func() (string, error)) string {
	exe, err := executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "..", ".env")
}

// checkoutEnvFile gates the DEFAULT env file: it is sourced only when its
// directory is a fishhawk checkout, with a .git entry and a regular
// scripts/dev. Otherwise, as in a release install, it returns "" and writes
// one line naming the skipped path and the --env-file remedy. An explicit
// --env-file is not gated here.
func checkoutEnvFile(path string, warn io.Writer) string {
	if path == "" {
		return ""
	}
	root := filepath.Dir(path)
	_, gitErr := os.Stat(filepath.Join(root, ".git"))
	dev, devErr := os.Stat(filepath.Join(root, "scripts", "dev"))
	if gitErr != nil || devErr != nil || !dev.Mode().IsRegular() {
		_, _ = fmt.Fprintf(warn, "fishhawk-mcp-shim: %s: skipping the default env file: %s is not a fishhawk checkout (no .git or no regular scripts/dev); pass --env-file <path> to source one\n", path, root)
		return ""
	}
	return path
}
