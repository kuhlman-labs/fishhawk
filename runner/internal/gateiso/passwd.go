package gateiso

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// Caller passwd entry (#2137). The gate container runs as the caller's
// numeric uid:gid, which the gate image's /etc/passwd does not name — tools
// that resolve the current user (`id -un`, git, libpq's default user) then
// fail. The runner reads the IMAGE's /etc/passwd once (PasswdReadArgv),
// appends an entry for the caller uid (BuildPasswd, mirroring scripts/test's
// _gate_image_passwd), writes it to a fresh per-exec file (WritePasswdFile)
// and mounts it read-only at /etc/passwd (ContainerSpec.PasswdFile) — on
// EVERY container exec, not only when services are configured.

// passwdFallbackName is the entry name when the caller's name is unusable.
const passwdFallbackName = "fishhawk-gate"

// passwdNamePattern is the only caller-name shape written into the file: no
// ':' or newline can forge or split an entry, and no leading '-' is
// flag-shaped to a tool reading the name.
var passwdNamePattern = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)

// BuildPasswd returns imagePasswd with an entry for uid appended, unless an
// entry already maps uid (then imagePasswd is returned unchanged). name falls
// back to fishhawk-gate when empty or not [A-Za-z0-9._-]+ (or '-'-led).
func BuildPasswd(imagePasswd []byte, uid, gid int, name string) []byte {
	want := []byte(strconv.Itoa(uid))
	for _, line := range bytes.Split(imagePasswd, []byte("\n")) {
		fields := bytes.Split(line, []byte(":"))
		if len(fields) >= 3 && bytes.Equal(fields[2], want) {
			return imagePasswd
		}
	}
	if !passwdNamePattern.MatchString(name) {
		name = passwdFallbackName
	}
	out := append([]byte(nil), imagePasswd...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, fmt.Sprintf("%s:x:%d:%d:fishhawk gate caller:/tmp:/bin/sh\n", name, uid, gid)...)
}

// PasswdReadArgv prints the image's /etc/passwd: endpoint-bound, and as
// contained as the gate exec itself (no network, all capabilities dropped,
// no-new-privileges, a non-root user pin, entrypoint reset).
func PasswdReadArgv(rt Runtime, image string, uid, gid int) ([]string, error) {
	bin := rt.Kind.Binary()
	if bin == "" {
		return nil, fmt.Errorf("%w: runtime kind %q", ErrContainerSpec, rt.Kind)
	}
	if image == "" || image[0] == '-' {
		return nil, fmt.Errorf("%w: image %q is empty or flag-shaped", ErrContainerSpec, image)
	}
	endpoint, err := rt.EndpointArgs()
	if err != nil {
		return nil, err
	}
	argv := append([]string{bin}, endpoint...)
	argv = append(argv, "run", "--rm", "--network=none", "--cap-drop=ALL", "--security-opt=no-new-privileges")
	argv = append(argv, userArgs(rt, uid, gid)...)
	return append(argv, "--entrypoint", "", image, "cat", "/etc/passwd"), nil
}

// userArgs is the user pin shared by BuildArgv and PasswdReadArgv.
func userArgs(rt Runtime, uid, gid int) []string {
	if rt.Kind == KindPodman && rt.Rootless {
		return []string{"--userns=keep-id"}
	}
	return []string{"--user", fmt.Sprintf("%d:%d", uid, gid)}
}

// WritePasswdFile writes content to a FRESH file under dir (mode 0444) and
// returns its path: each exec gets its own file, so a concurrent exec can
// never observe a half-written or replaced one. The caller removes dir.
func WritePasswdFile(dir string, content []byte) (string, error) {
	f, err := os.CreateTemp(dir, "passwd-*")
	if err != nil {
		return "", fmt.Errorf("create passwd file: %w", err)
	}
	path := f.Name()
	_, werr := f.Write(content)
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(path, 0o444)
	}
	if werr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("write passwd file: %w", werr)
	}
	return filepath.Clean(path), nil
}
