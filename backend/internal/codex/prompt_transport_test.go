package codex

// Prompt transport over stdin (#5113). The adapter used to pass the review
// prompt as the trailing `codex exec ... <prompt>` positional argv string, and
// execve refuses an
// oversized argument list with E2BIG before the CLI runs (Linux: one string over
// MAX_ARG_STRLEN, 128 KiB; darwin: argv+env over kern.argmax, 1 MiB). These
// tests drive the PRODUCTION adapter (InferenceInTree, its env scrub, its
// confined CODEX_HOME, its procgroup hardening) against an argv-FORWARDING fake: unlike the other helper
// builders in this package, which discard the adapter's argv, ptForwardingBuilder
// hands the adapter's real argv to execve, so a prompt that regressed back into
// argv really hits the OS limit instead of being silently dropped.
//
// Evidence limit: the fake proves the OS transport only — that the child
// process receives the exact prompt bytes on stdin and none in argv. Whether the
// real `codex` CLI reads that stdin as its prompt is the vendor contract
// (`codex exec --help`: "If not provided as an argument (or if `-` is used),
// instructions are read from stdin."); the operational check is a real
// configured review.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

const (
	// ptHelperEnv gates TestPromptTransportHelper; ptModeEnv selects its mode;
	// ptOutEnv names the directory it writes its capture files into. All three
	// reach the child only through cfg.EnvPassthrough, i.e. through the
	// production reviewsandbox.Env scrub.
	ptHelperEnv = "FISHHAWK_PROMPT_TRANSPORT_HELPER"
	ptModeEnv   = "FISHHAWK_PROMPT_TRANSPORT_MODE"
	ptOutEnv    = "FISHHAWK_PROMPT_TRANSPORT_OUT"
	// ptSentinel is embedded in every line of the large prompt. No argv element
	// the child receives may contain it.
	ptSentinel = "FISHHAWK-5113-PROMPT-SENTINEL"
	// ptBigPromptBytes is the minimum large-prompt size: 4 MiB is past both the
	// Linux per-string limit (128 KiB) and the darwin total (1 MiB).
	ptBigPromptBytes = 4 << 20
)

// TestPromptTransportHelper is the argv-forwarding fake `codex`. It is re-exec'd
// as `<test binary> -test.run=^TestPromptTransportHelper$ -- <adapter argv>`;
// the `--` stops the test binary's flag parsing so the adapter's argv (`exec
// --json ... -`) reaches os.Args untouched.
//
//   - capture: read ALL of stdin, write it to stdin.bin, the adapter argv to
//     argv.json and the env byte total (a number, never values) to env_bytes,
//     then print an approve JSONL transcript.
//   - wedge: never read stdin; sleep far past any test deadline.
func TestPromptTransportHelper(t *testing.T) {
	if os.Getenv(ptHelperEnv) != "1" {
		return
	}
	defer os.Exit(0)

	argv := ptAdapterArgv(os.Args)
	switch os.Getenv(ptModeEnv) {
	case "capture":
		out := os.Getenv(ptOutEnv)
		stdin, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read stdin:", err)
			os.Exit(3)
		}
		argvJSON, _ := json.Marshal(argv)
		envBytes := 0
		for _, kv := range os.Environ() {
			envBytes += len(kv) + 1
		}
		for name, data := range map[string][]byte{
			"stdin.bin": stdin,
			"argv.json": argvJSON,
			"env_bytes": []byte(strconv.Itoa(envBytes)),
		} {
			if werr := os.WriteFile(filepath.Join(out, name), data, 0o600); werr != nil {
				fmt.Fprintln(os.Stderr, "write capture:", werr)
				os.Exit(3)
			}
		}
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{\"verdict\":\"approve\"}"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}`)
	case "wedge":
		time.Sleep(timescale.D(60 * time.Second))
	default:
		fmt.Fprintln(os.Stderr, "unknown "+ptModeEnv)
		os.Exit(2)
	}
}

// ptAdapterArgv returns the elements after the first `--` (the adapter argv).
func ptAdapterArgv(osArgs []string) []string {
	i := slices.Index(osArgs, "--")
	if i < 0 {
		return nil
	}
	return osArgs[i+1:]
}

// ptForwardingBuilder re-execs the test binary as the fake `codex`, forwarding
// the adapter's REAL argv to execve. It leaves cmd.Env nil so the adapter's
// production scrub builds the child environment. attempts, when non-nil, counts
// builder calls (= subprocess attempts).
func ptForwardingBuilder(attempts *int) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		if attempts != nil {
			*attempts++
		}
		return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestPromptTransportHelper$", "--"}, args...)...) //nolint:gosec // re-exec of the test binary itself
	}
}

// ptClient builds a production Client wired to the forwarding fake in the given
// mode. The helper control vars ride ONLY through cfg.EnvPassthrough, so the
// child env is exactly what reviewsandbox.Env emits in production plus those.
func ptClient(t *testing.T, mode, outDir string, attempts *int) *Client {
	t.Helper()
	t.Setenv(ptHelperEnv, "1")
	t.Setenv(ptModeEnv, mode)
	t.Setenv(ptOutEnv, outDir)
	cfg := testConfig()
	cfg.Timeout = timescale.D(60 * time.Second)
	cfg.EnvPassthrough = []string{ptHelperEnv, ptModeEnv, ptOutEnv}
	c := NewClient(cfg)
	c.Cmd = ptForwardingBuilder(attempts)
	c.HostPaths = testHostPaths(t)
	c.GOOS = "linux"
	return c
}

// ptBigPrompt builds a >= 4 MiB prompt mixing multi-byte UTF-8, CRLF, tabs and
// leading/trailing whitespace and newlines — every byte class a transport that
// trims, re-encodes or line-normalises would corrupt.
func ptBigPrompt(t *testing.T) string {
	t.Helper()
	const line = "Plan §3 — naïve café 日本語 🦅 review\tcolumn\r\nCRLF line\twith tabs " + ptSentinel + "\n"
	var b strings.Builder
	b.WriteString(" \t\r\n")
	for b.Len() < ptBigPromptBytes {
		b.WriteString(line)
	}
	b.WriteString("  \t\n\n")
	p := b.String()
	if len(p) < ptBigPromptBytes || !utf8.ValidString(p) || !strings.HasSuffix(p, "  \t\n\n") {
		t.Fatalf("test prompt construction: %d bytes, valid UTF-8 = %v", len(p), utf8.ValidString(p))
	}
	return p
}

// ptRequireArgvE2BIG is the non-skipped precondition: the SAME prompt sent as an
// argv string through the same forwarding fake must be refused by execve with
// E2BIG on this host. The env is EMPTY — the most lenient case for argv — so a
// refusal here is a refusal under any env. If the host accepts it, the stdin
// test would prove nothing about the limit, so the test FAILS rather than skips.
func ptRequireArgvE2BIG(t *testing.T, prompt string) {
	t.Helper()
	cmd := ptForwardingBuilder(nil)(context.Background(), DefaultBinary, "exec", "--json", "--skip-git-repo-check", prompt)
	cmd.Env = []string{}
	err := cmd.Start()
	if err == nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("precondition: a %d-byte prompt sent as one argv string was NOT refused by execve on this host; "+
			"the stdin transport test cannot prove the exec limit is avoided — raise ptBigPromptBytes", len(prompt))
	}
	if !errors.Is(err, syscall.E2BIG) {
		t.Fatalf("precondition: argv spawn of a %d-byte prompt failed with %v, want syscall.E2BIG", len(prompt), err)
	}
}

type ptCapture struct {
	stdin    []byte
	argv     []string
	envBytes int
}

// ptReadCapture reads what the capture child recorded. It reports sizes, never
// env values.
func ptReadCapture(t *testing.T, outDir string) ptCapture {
	t.Helper()
	var c ptCapture
	var err error
	if c.stdin, err = os.ReadFile(filepath.Join(outDir, "stdin.bin")); err != nil {
		t.Fatalf("capture child wrote no stdin record: %v", err)
	}
	argvJSON, err := os.ReadFile(filepath.Join(outDir, "argv.json"))
	if err != nil {
		t.Fatalf("capture child wrote no argv record: %v", err)
	}
	if err := json.Unmarshal(argvJSON, &c.argv); err != nil {
		t.Fatalf("decode argv record: %v", err)
	}
	envRaw, err := os.ReadFile(filepath.Join(outDir, "env_bytes"))
	if err != nil {
		t.Fatalf("capture child wrote no env byte record: %v", err)
	}
	if c.envBytes, err = strconv.Atoi(string(envRaw)); err != nil {
		t.Fatalf("decode env byte record: %v", err)
	}
	return c
}

// ptClearCapture removes a previous run's records so a stale file can never
// stand in for a run that recorded nothing.
func ptClearCapture(t *testing.T, outDir string) {
	t.Helper()
	for _, name := range []string{"stdin.bin", "argv.json", "env_bytes"} {
		if err := os.Remove(filepath.Join(outDir, name)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("clear capture: %v", err)
		}
	}
}

// ptStripOutputSchema removes the `--output-schema <path>` pair, whose temp-file
// name varies in width per invocation, without reading the (already removed)
// file.
func ptStripOutputSchema(t *testing.T, argv []string) []string {
	t.Helper()
	i := slices.Index(argv, "--output-schema")
	if i < 0 || i+1 >= len(argv) {
		t.Fatalf("child argv %q is missing the --output-schema <path> pair", argv)
	}
	return append(append([]string(nil), argv[:i]...), argv[i+2:]...)
}

// ptArgvBytes is the execve accounting for an argv: len(arg)+1 per string.
func ptArgvBytes(argv []string) int {
	n := 0
	for _, a := range argv {
		n += len(a) + 1
	}
	return n
}

// ptAssertStdinExact compares the received stdin to the prompt byte-for-byte,
// reporting lengths and the first differing offset rather than megabytes.
func ptAssertStdinExact(t *testing.T, got []byte, prompt string) {
	t.Helper()
	if bytes.Equal(got, []byte(prompt)) {
		return
	}
	diff := 0
	for diff < len(got) && diff < len(prompt) && got[diff] == prompt[diff] {
		diff++
	}
	t.Fatalf("child stdin = %d bytes, want the %d-byte prompt byte-exact (first difference at offset %d)", len(got), len(prompt), diff)
}

// TestPromptTransport_CodexStdinExactBytesAboveExecLimit is the #5113
// done-means: in BOTH postures, a >= 4 MiB prompt that execve refuses as an argv
// string (asserted first, never skipped) reaches the child byte-exact on stdin,
// no argv element carries it, the last argv element is exactly `-` (read the
// prompt from stdin), and the review succeeds in one attempt. Short and
// flag-shaped prompts arrive byte-exact too, and a flag-shaped prompt is never
// parsed as CLI flags because it is not in argv at all.
func TestPromptTransport_CodexStdinExactBytesAboveExecLimit(t *testing.T) {
	big := ptBigPrompt(t)
	ptRequireArgvE2BIG(t, big)

	cases := []struct {
		name   string
		prompt string
		// argvMustNotContain are substrings no child argv element may contain.
		argvMustNotContain []string
		// argvMustNotEqual are tokens no child argv element may equal.
		argvMustNotEqual []string
	}{
		{name: "above_exec_limit", prompt: big, argvMustNotContain: []string{ptSentinel}},
		{name: "short", prompt: "review", argvMustNotEqual: []string{"review"}},
		{
			name:             "flag_shaped",
			prompt:           "--help --version --dangerously-bypass-approvals-and-sandbox",
			argvMustNotEqual: []string{"--help", "--version", "--dangerously-bypass-approvals-and-sandbox"},
		},
	}
	for _, posture := range []string{"ungrounded", "grounded"} {
		t.Run(posture, func(t *testing.T) {
			treeDir := ""
			if posture == "grounded" {
				treeDir = t.TempDir()
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					outDir := t.TempDir()
					var attempts int
					c := ptClient(t, "capture", outDir, &attempts)

					text, _, _, err := c.InferenceInTree(context.Background(), tc.prompt, treeDir)
					if err != nil {
						t.Fatalf("InferenceInTree(%s, %d-byte prompt): %v", posture, len(tc.prompt), err)
					}
					if !strings.Contains(text, `"approve"`) {
						t.Errorf("verdict text = %q, want the capture child's approve", text)
					}
					if attempts != 1 {
						t.Errorf("attempts = %d, want 1", attempts)
					}
					got := ptReadCapture(t, outDir)
					ptAssertStdinExact(t, got.stdin, tc.prompt)
					for _, a := range got.argv {
						for _, s := range tc.argvMustNotContain {
							if strings.Contains(a, s) {
								t.Errorf("a child argv element (%d bytes) carries prompt content %q", len(a), s)
							}
						}
						if slices.Contains(tc.argvMustNotEqual, a) {
							t.Errorf("child argv %q carries prompt token %q", got.argv, a)
						}
					}
					if n := len(got.argv); n == 0 || got.argv[n-1] != "-" {
						t.Errorf("child argv %q: the last element must be exactly `-` (prompt on stdin)", got.argv)
					}
					for _, a := range got.argv {
						if strings.HasPrefix(a, "--dangerously") {
							t.Errorf("child argv %q contains a dangerous flag %q", got.argv, a)
						}
					}
					// The posture actually under test reached the child.
					if hasSandbox := slices.Contains(got.argv, "--sandbox"); hasSandbox != (posture == "grounded") {
						t.Errorf("posture %s: child argv carries --sandbox = %v: %q", posture, hasSandbox, got.argv)
					}
				})
			}
		})
	}
}

// TestPromptTransport_CodexArgvIndependentOfPromptSize pins that the spawn's
// argv and env byte totals, as received by the child, are IDENTICAL for a 6-byte
// and a >= 4 MiB prompt: the prompt contributes zero bytes to what execve
// accounts against the limit. The `--output-schema <path>` pair is stripped
// first because its os.CreateTemp name is a per-invocation random number of
// varying width. It runs in the ungrounded posture only: the grounded posture
// also carries a per-invocation random confined CODEX_HOME path in env, which
// would vary for reasons unrelated to the prompt. Only byte counts are compared
// and reported, never env values.
func TestPromptTransport_CodexArgvIndependentOfPromptSize(t *testing.T) {
	big := ptBigPrompt(t)
	outDir := t.TempDir()
	c := ptClient(t, "capture", outDir, nil)
	measure := func(prompt string) (argvBytes, envBytes int) {
		t.Helper()
		ptClearCapture(t, outDir)
		if _, _, _, err := c.InferenceInTree(context.Background(), prompt, ""); err != nil {
			t.Fatalf("InferenceInTree(ungrounded, %d-byte prompt): %v", len(prompt), err)
		}
		got := ptReadCapture(t, outDir)
		return ptArgvBytes(ptStripOutputSchema(t, got.argv)), got.envBytes
	}
	smallArgv, smallEnv := measure("review")
	bigArgv, bigEnv := measure(big)
	if smallArgv != bigArgv {
		t.Errorf("argv bytes: %d for a 6-byte prompt, %d for a %d-byte prompt; want identical", smallArgv, bigArgv, len(big))
	}
	if smallEnv != bigEnv {
		t.Errorf("env bytes: %d for a 6-byte prompt, %d for a %d-byte prompt; want identical", smallEnv, bigEnv, len(big))
	}
}

// TestPromptTransport_CodexWedgedChildNotReadingStdinTimesOut pins that stdin
// delivery cannot outlive the review deadline: a child that never reads its
// stdin leaves the >= 4 MiB copy blocked on a full pipe, and the deadline's
// whole-group SIGKILL must still end the invocation as a '(timeout)' in ONE
// attempt (MaxRetries=1 is available and unused) — well before the RAISED
// killGrace (WaitDelay), so the return comes from the kill breaking the pipe,
// not from WaitDelay force-closing a stuck copy.
func TestPromptTransport_CodexWedgedChildNotReadingStdinTimesOut(t *testing.T) {
	raised := timescale.D(60 * time.Second)
	defer setKillGrace(raised)()
	var attempts int
	c := ptClient(t, "wedge", t.TempDir(), &attempts)
	deadline := timescale.D(2 * time.Second)
	c.cfg.Timeout = deadline
	if c.cfg.MaxRetries != 1 {
		t.Fatalf("MaxRetries = %d, want the default 1 so a retry would be observable", c.cfg.MaxRetries)
	}

	start := time.Now()
	_, _, _, err := c.InferenceInTree(context.Background(), ptBigPrompt(t), "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a child that never reads stdin and never exits must fail at the deadline, got nil error")
	}
	if !strings.Contains(err.Error(), "(timeout)") {
		t.Errorf("err = %v, want a '(timeout)' classification", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a timeout is never retried)", attempts)
	}
	if elapsed < deadline {
		t.Errorf("returned after %s, before the %s deadline", elapsed, deadline)
	}
	if elapsed >= raised {
		t.Errorf("returned after %s, at or past the raised killGrace %s: the blocked stdin copy outlived the group kill", elapsed, raised)
	}
}

// TestPromptTransport_CodexSpawnFaultIsNotRetriedOrRelabeled pins the spawn
// fault class: a REAL execve E2BIG (forced by an oversized argv element the
// builder adds) surfaces as an 'invocation failed' error that wraps
// syscall.E2BIG, is never labelled timeout, OOM or quota, and is attempted
// exactly once although MaxRetries=1 is available.
func TestPromptTransport_CodexSpawnFaultIsNotRetriedOrRelabeled(t *testing.T) {
	var attempts int
	c := ptClient(t, "capture", t.TempDir(), nil)
	forward := ptForwardingBuilder(&attempts)
	c.Cmd = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := forward(ctx, name, args...)
		cmd.Args = append(cmd.Args, strings.Repeat("x", ptBigPromptBytes))
		return cmd
	}
	if c.cfg.MaxRetries != 1 {
		t.Fatalf("MaxRetries = %d, want the default 1 so a retry would be observable", c.cfg.MaxRetries)
	}

	_, _, _, err := c.InferenceInTree(context.Background(), "review", "")
	if err == nil {
		t.Fatal("a spawn refused by execve must fail the invocation, got nil error")
	}
	if !errors.Is(err, syscall.E2BIG) {
		t.Errorf("err = %v, want it to wrap syscall.E2BIG", err)
	}
	if !strings.Contains(err.Error(), "invocation failed") {
		t.Errorf("err = %v, want the 'invocation failed' classification", err)
	}
	lower := strings.ToLower(err.Error())
	for _, label := range []string{"timeout", "oom", "quota"} {
		if strings.Contains(lower, label) {
			t.Errorf("err = %v, must not be labelled %q", err, label)
		}
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (a spawn fault is not retried)", attempts)
	}
}
