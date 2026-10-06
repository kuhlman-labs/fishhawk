package gateiso

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const imagePasswd = "root:x:0:0:root:/root:/bin/sh\npostgres:x:70:70:Linux User,,,:/var/lib/postgresql:/bin/sh\n"

func TestBuildPasswd_UIDPresentUnchanged(t *testing.T) {
	if got := BuildPasswd([]byte(imagePasswd), 70, 70, "brett"); string(got) != imagePasswd {
		t.Errorf("uid 70 already mapped; got %q", got)
	}
}

func TestBuildPasswd_UIDAbsentAppendsOneLine(t *testing.T) {
	got := string(BuildPasswd([]byte(imagePasswd), 501, 20, "brett"))
	want := imagePasswd + "brett:x:501:20:fishhawk gate caller:/tmp:/bin/sh\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	// No trailing newline in the image file: the entry still starts a line.
	got = string(BuildPasswd([]byte("root:x:0:0:root:/root:/bin/sh"), 501, 20, "brett"))
	if got != "root:x:0:0:root:/root:/bin/sh\nbrett:x:501:20:fishhawk gate caller:/tmp:/bin/sh\n" {
		t.Errorf("unterminated image passwd: got %q", got)
	}
	// A uid that only appears as a GID or a substring is not "present".
	got = string(BuildPasswd([]byte("svc:x:5010:501::/:/bin/sh\n"), 501, 20, "brett"))
	if !strings.HasSuffix(got, "brett:x:501:20:fishhawk gate caller:/tmp:/bin/sh\n") {
		t.Errorf("uid 501 wrongly treated as present: %q", got)
	}
}

// TestBuildPasswd_HostileNamesFallBack: a name that could forge or split an
// entry, or read as a flag, is replaced by fishhawk-gate.
func TestBuildPasswd_HostileNamesFallBack(t *testing.T) {
	for _, name := range []string{"", "a:b", "evil\nroot:x:0:0::/:/bin/sh", "-rf", "spa ce", "ünï"} {
		got := string(BuildPasswd(nil, 501, 20, name))
		if got != "fishhawk-gate:x:501:20:fishhawk gate caller:/tmp:/bin/sh\n" {
			t.Errorf("name %q: got %q, want the fishhawk-gate fallback", name, got)
		}
	}
	if got := string(BuildPasswd(nil, 501, 20, "first.last_1-x")); !strings.HasPrefix(got, "first.last_1-x:") {
		t.Errorf("legal name rewritten: %q", got)
	}
}

// TestWellFormedPasswd_DropsRuntimeNoise: the read's output is the runtime
// CLI's combined stdout and stderr, so a cold pull's progress lines and a
// platform warning arrive interleaved with the file. Only the entries survive,
// each newline-terminated (CRLF and an unterminated last line included).
func TestWellFormedPasswd_DropsRuntimeNoise(t *testing.T) {
	noisy := "Unable to find image 'fishhawk-gate:main' locally\n" +
		"main: Pulling from kuhlman-labs/fishhawk-gate\n" +
		"4abcf2066143: Pull complete\n" +
		"Digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n" +
		"Status: Downloaded newer image for fishhawk-gate:main\n" +
		"WARNING: The requested image's platform (linux/amd64) does not match the detected host platform (linux/arm64/v8)\n" +
		"root:x:0:0:root:/root:/bin/sh\r\n" +
		"bad:x:notanumber:0::/:/bin/sh\n" +
		":x:1:1::/:/bin/sh\n" +
		"a:b:c:d:e:f:g:h\n" +
		"\n" +
		"postgres:x:70:70:Linux User,,,:/var/lib/postgresql:/bin/sh"
	if got := string(WellFormedPasswd([]byte(noisy))); got != imagePasswd {
		t.Errorf("got %q\nwant %q", got, imagePasswd)
	}
	if got := WellFormedPasswd([]byte("Unable to find image 'x' locally\nStatus: Downloaded newer image for x\n")); len(got) != 0 {
		t.Errorf("noise-only output kept %q, want nothing", got)
	}
	if got := string(WellFormedPasswd([]byte(imagePasswd))); got != imagePasswd {
		t.Errorf("a clean file was rewritten: %q", got)
	}
}

func TestPasswdReadArgv(t *testing.T) {
	got, err := PasswdReadArgv(dockerRT, "img:1", 501, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "--host", "unix://" + testSock, "run", "--rm", "--network=none", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--user", "501:20", "--entrypoint", "", "img:1", "cat", "/etc/passwd"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	rootless := Runtime{Kind: KindPodman, Safe: true, Rootless: true, SocketPath: testSock}
	if got, _ := PasswdReadArgv(rootless, "img:1", 501, 20); idx(got, "--userns=keep-id") < 0 || idx(got, "--user") >= 0 {
		t.Errorf("rootless podman: %q, want --userns=keep-id and no --user", got)
	}
	for _, tc := range []struct {
		name  string
		rt    Runtime
		image string
	}{
		{"unbound", Runtime{Kind: KindDocker, Safe: true}, "img:1"},
		{"no runtime", Runtime{Kind: KindNone, SocketPath: testSock}, "img:1"},
		{"empty image", dockerRT, ""},
		{"flag image", dockerRT, "--privileged"},
	} {
		if argv, err := PasswdReadArgv(tc.rt, tc.image, 501, 20); argv != nil || !errors.Is(err, ErrContainerSpec) {
			t.Errorf("%s: argv %q, err %v; want nil and ErrContainerSpec", tc.name, argv, err)
		}
	}
}

func TestWritePasswdFile_FreshReadOnlyFilePerCall(t *testing.T) {
	dir := t.TempDir()
	a, err := WritePasswdFile(dir, []byte("a\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := WritePasswdFile(dir, []byte("b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b || filepath.Dir(a) != filepath.Clean(dir) {
		t.Fatalf("paths %q, %q: want two distinct files under %s", a, b, dir)
	}
	for path, want := range map[string]string{a: "a\n", b: "b\n"} {
		got, _ := os.ReadFile(path)
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want || fi.Mode().Perm() != 0o444 {
			t.Errorf("%s: content %q mode %v; want %q 0444", path, got, fi.Mode().Perm(), want)
		}
	}
	if _, err := WritePasswdFile(filepath.Join(dir, "missing"), []byte("x")); err == nil {
		t.Error("write into a missing dir succeeded")
	}
}
