package gateiso

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestProbeSandbox_Rows(t *testing.T) {
	okLook := func(string) (string, error) { return "/usr/bin/x", nil }
	okRun := func(context.Context, string, ...string) error { return nil }
	rows := []struct {
		name      string
		goos      string
		lookPath  func(string) (string, error)
		run       func(context.Context, string, ...string) error
		available bool
		reason    string
	}{
		{"non-linux is the ADR-063 gap", "darwin", okLook, okRun, false, SandboxUnavailableNonLinux},
		{"windows likewise", "windows", okLook, okRun, false, SandboxUnavailableNonLinux},
		{"linux without unshare", "linux", func(b string) (string, error) {
			if b == "unshare" {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + b, nil
		}, okRun, false, "unshare not on PATH"},
		{"linux without ip", "linux", func(b string) (string, error) {
			if b == "ip" {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + b, nil
		}, okRun, false, "ip not on PATH"},
		{"linux namespace probe fails", "linux", okLook, func(context.Context, string, ...string) error {
			return errors.New("unshare: unshare failed: Operation not permitted")
		}, false, "namespace probe failed (unshare: unshare failed: Operation not permitted)"},
		{"linux with a working probe", "linux", okLook, okRun, true, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			var seen []string
			run := func(ctx context.Context, name string, args ...string) error {
				seen = append([]string{name}, args...)
				return row.run(ctx, name, args...)
			}
			got, reason := probeSandbox(context.Background(), row.goos, row.lookPath, run)
			if got != row.available {
				t.Fatalf("available = %v, want %v (reason %q)", got, row.available, reason)
			}
			if !strings.Contains(reason, row.reason) {
				t.Fatalf("reason %q does not contain %q", reason, row.reason)
			}
			if row.goos == "linux" && row.available {
				// The probe runs the REAL wrapper against a no-op, not a
				// bespoke command that could diverge from WrapSandbox.
				if want := WrapSandbox([]string{"true"}); !reflect.DeepEqual(seen, want) {
					t.Fatalf("probe ran %q, want the WrapSandbox form %q", seen, want)
				}
			}
			if row.goos != "linux" && seen != nil {
				t.Fatalf("non-linux probe must not exec anything, ran %q", seen)
			}
		})
	}
}

func TestWrapSandbox_Golden(t *testing.T) {
	argv := []string{"scripts/test", "verify", "--packages", "a,b", `it's "quoted"`, "$HOME"}
	got := WrapSandbox(argv)
	want := []string{"unshare", "-rn", "--", "sh", "-c", `ip link set lo up && exec "$0" "$@"`,
		"scripts/test", "verify", "--packages", "a,b", `it's "quoted"`, "$HOME"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WrapSandbox =\n%q\nwant\n%q", got, want)
	}
	// argv is appended verbatim — never interpolated into the script.
	if got[5] != sandboxScript || strings.Contains(got[5], "scripts/test") {
		t.Fatalf("script element %q must be the constant, with argv passed positionally", got[5])
	}
	if &got[0] == &argv[0] {
		t.Fatal("WrapSandbox must not alias the caller's slice")
	}
}

// TestWrapSandbox_LinuxNoNetwork is the with/without-wrap pair: the same
// loopback connect succeeds on the host and fails inside the sandbox.
func TestWrapSandbox_LinuxNoNetwork(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("sandbox is Linux-only: %s", SandboxUnavailableNonLinux)
	}
	if ok, reason := ProbeSandbox(context.Background()); !ok {
		t.Skipf("sandbox unavailable on this host: %s", reason)
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	// A listener on the host loopback; the connect target for both arms.
	listen := exec.Command(py, "-c", `
import socket,sys,time
s=socket.socket(); s.bind(("127.0.0.1",0)); s.listen(1)
sys.stdout.write(str(s.getsockname()[1])+"\n"); sys.stdout.flush()
time.sleep(20)`)
	out, err := listen.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := listen.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listen.Process.Kill(); _ = listen.Wait() })
	var port int
	if _, err := fmt.Fscan(out, &port); err != nil {
		t.Fatalf("read listener port: %v", err)
	}
	connect := []string{py, "-c", `import socket,sys; s=socket.create_connection(("127.0.0.1",int(sys.argv[1])),timeout=3); s.close()`, strconv.Itoa(port)}

	if b, err := exec.Command(connect[0], connect[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("without-wrap control: host loopback connect failed: %v\n%s", err, b)
	}
	wrapped := WrapSandbox(connect)
	if b, err := exec.Command(wrapped[0], wrapped[1:]...).CombinedOutput(); err == nil {
		t.Fatalf("with-wrap: connect to the host loopback SUCCEEDED inside the sandbox\n%s", b)
	}
}

func TestSandboxProbeExec_FoldsOutputIntoError(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	ctx := context.Background()
	if err := sandboxProbeExec(ctx, "sh", "-c", "exit 0"); err != nil {
		t.Fatalf("succeeding probe returned %v", err)
	}
	err := sandboxProbeExec(ctx, "sh", "-c", "echo permission denied by probe >&2; exit 3")
	if err == nil {
		t.Fatal("failing probe returned nil")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("error %v does not wrap the exit status", err)
	}
	if !strings.Contains(err.Error(), "permission denied by probe") {
		t.Fatalf("error %q does not carry the probe's output", err)
	}
}

func TestProbeSandbox_RealHostNeverPanics(t *testing.T) {
	available, reason := ProbeSandbox(context.Background())
	if runtime.GOOS != "linux" && (available || reason != SandboxUnavailableNonLinux) {
		t.Fatalf("non-linux host: available=%v reason=%q", available, reason)
	}
	if available && reason != "" {
		t.Fatalf("available with a reason %q", reason)
	}
}
