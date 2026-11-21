package gateiso

import (
	"errors"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"testing"
)

// TestWrapLaunchMarker_Shape: the exact five-element wrapper prefix, the
// original argv verbatim at the tail, and a fresh slice (the input is never
// aliased).
func TestWrapLaunchMarker_Shape(t *testing.T) {
	argv := make([]string, 3, 8) // spare capacity: an append-alias would show
	copy(argv, []string{"sh", "-c", "make verify"})
	got := WrapLaunchMarker(argv, "abc")
	want := []string{"sh", "-c", launchMarkerScript, LaunchMarkerArg0, "abc", "sh", "-c", "make verify"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WrapLaunchMarker = %q, want %q", got, want)
	}
	got[5] = "mutated"
	if argv[0] != "sh" {
		t.Fatalf("WrapLaunchMarker aliased its input: %q", argv)
	}
	if launchMarkerScript != `printf 'fishhawk-gate-launched:%s\n' "$1" || exit 125; shift; exec "$@"` {
		t.Fatalf("launch marker script drifted: %q", launchMarkerScript)
	}
}

// TestWrapLaunchMarker_HostShellExecsArgvVerbatim runs the REAL wrapper under
// the host's sh: the marker is the first line of output, args with spaces and
// shell metacharacters reach the inner argv verbatim, and the inner exit code
// propagates unchanged.
func TestWrapLaunchMarker_HostShellExecsArgvVerbatim(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	nonce := NewLaunchNonce()
	for _, code := range []int{0, 1, 125, 127} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			inner := []string{"sh", "-c", `printf "%s|" "$@"; exit ` + strconv.Itoa(code), "x", "a b;c", "$HOME"}
			argv := WrapLaunchMarker(inner, nonce)
			out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
			got := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				got = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got != code {
				t.Errorf("exit = %d, want %d", got, code)
			}
			want := LaunchMarkerLine(nonce) + "\n" + "a b;c|$HOME|"
			if string(out) != want {
				t.Errorf("output = %q, want %q", out, want)
			}
			cleaned, launched := CutLaunchMarker(string(out), nonce)
			if !launched || cleaned != "a b;c|$HOME|" {
				t.Errorf("CutLaunchMarker = %q, %t", cleaned, launched)
			}
		})
	}
}

func TestCutLaunchMarker(t *testing.T) {
	const n = "0123456789abcdef0123456789abcdef"
	m := LaunchMarkerLine(n)
	for _, tc := range []struct {
		name, out, nonce, want string
		launched               bool
	}{
		{"absent", "docker: Error response from daemon\n", n, "docker: Error response from daemon\n", false},
		{"at_start", m + "\nok\n", n, "ok\n", true},
		{"after_pull_chatter", "Unable to find image 'x:1' locally\n1: Pulling from x\n" + m + "\nFAIL\n", n,
			"Unable to find image 'x:1' locally\n1: Pulling from x\nFAIL\n", true},
		{"crlf", "chatter\r\n" + m + "\r\nok\r\n", n, "chatter\r\nok\r\n", true},
		{"at_end_no_newline", "chatter\n" + m, n, "chatter\n", true},
		{"only_marker", m, n, "", true},
		{"substring_in_longer_line", "exec: sh -c ... " + m + " x\n", n, "exec: sh -c ... " + m + " x\n", false},
		{"prefix_of_longer_line", m + "ff\nok\n", n, m + "ff\nok\n", false},
		// A runtime echoing the argv can END a longer line with the marker
		// text: only a match starting the line counts.
		{"suffix_of_longer_line", "echo " + m + "\nok\n", n, "echo " + m + "\nok\n", false},
		{"different_nonce", LaunchMarkerLine("ffffffffffffffffffffffffffffffff") + "\nok\n", n,
			LaunchMarkerLine("ffffffffffffffffffffffffffffffff") + "\nok\n", false},
		{"empty_nonce", launchMarkerPrefix + "\nok\n", "", launchMarkerPrefix + "\nok\n", false},
		{"two_occurrences_first_removed", m + "\nmid\n" + m + "\n", n, "mid\n" + m + "\n", true},
		{"substring_then_whole_line", "x" + m + "\n" + m + "\nok", n, "x" + m + "\nok", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, launched := CutLaunchMarker(tc.out, tc.nonce)
			if got != tc.want || launched != tc.launched {
				t.Errorf("CutLaunchMarker(%q) = (%q, %t), want (%q, %t)", tc.out, got, launched, tc.want, tc.launched)
			}
		})
	}
}

func TestIsRuntimeLaunchExit(t *testing.T) {
	for code, want := range map[int]bool{125: true, 126: true, 127: true, -1: false, 0: false, 1: false, 73: false, 75: false, 124: false, 128: false, 137: false} {
		if got := IsRuntimeLaunchExit(code); got != want {
			t.Errorf("IsRuntimeLaunchExit(%d) = %t, want %t", code, got, want)
		}
	}
}

func TestNewLaunchNonce(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	a, b := NewLaunchNonce(), NewLaunchNonce()
	if !re.MatchString(a) || !re.MatchString(b) {
		t.Fatalf("nonces %q / %q are not 32 lowercase hex characters", a, b)
	}
	if a == b {
		t.Fatalf("two nonces are equal: %q", a)
	}
}
