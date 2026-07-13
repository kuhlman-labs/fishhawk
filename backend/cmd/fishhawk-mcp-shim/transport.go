package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// childTransport is the ONLY seam the supervisor talks to. The supervisor holds
// no os/exec knowledge — it drives a child purely through this interface — so a
// later phase can substitute a streamable-HTTP upstream for the stdio child
// without touching the supervisor (ADR-060's #655 phase-0 constraint). The
// in-process fake in supervisor_test.go is the same seam, which is what lets the
// supervisor tests double as the seam-contract proof.
type childTransport interface {
	// Start launches the child and begins pumping frames on Frames(). It records
	// the launch-time content hash of the underlying binary (LaunchHash) so the
	// watcher can compare the on-disk file against the running child.
	Start(ctx context.Context) error
	// Send writes one newline-delimited JSON-RPC frame to the child, byte-verbatim.
	Send(frame []byte) error
	// Frames yields child->shim frames byte-verbatim, one per receive. It is NOT
	// closed on exit (Exited is the authoritative death signal); the sender
	// simply stops after EOF, so a select on a dead child's Frames blocks rather
	// than spinning on a closed channel.
	Frames() <-chan []byte
	// Exited fires exactly once, buffered, with the child's exit error (nil on a
	// clean exit) after cmd.Wait completes. The supervisor distinguishes a crash
	// from a shim-initiated Terminate by tracking which it requested.
	Exited() <-chan error
	// Terminate sends SIGTERM to the child, then SIGKILLs its whole process group
	// after grace. Idempotent.
	Terminate(grace time.Duration)
	// LaunchHash is the sha-256 of the binary this child was launched from.
	LaunchHash() []byte
}

// stdioChild is the os/exec implementation of childTransport: it spawns the
// child binary over pipes and passes frames byte-verbatim in both directions.
type stdioChild struct {
	path   string
	stderr io.Writer

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	frames chan []byte
	exited chan error
	hash   []byte

	mu      sync.Mutex
	stopped bool
}

// newStdioChild builds an unstarted stdio child for the binary at path. Child
// stderr is wired straight through to the shim's stderr so the child's own
// diagnostics stay visible.
func newStdioChild(path string, stderr io.Writer) *stdioChild {
	return &stdioChild{
		path:   path,
		stderr: stderr,
		frames: make(chan []byte, 256),
		exited: make(chan error, 1),
	}
}

func (c *stdioChild) Start(ctx context.Context) error {
	h, err := sha256File(c.path)
	if err != nil {
		return err
	}
	c.hash = h

	cmd := exec.CommandContext(ctx, c.path)
	cmd.Stderr = c.stderr
	setPgid(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd = cmd
	c.stdin = stdin
	go c.readLoop(stdout)
	return nil
}

// readLoop drains the child stdout pipe to EOF using bufio.Reader.ReadBytes —
// NOT bufio.Scanner, whose default 64KiB token cap would truncate large
// tool-result frames — preserving each line's exact bytes (including any
// trailing \r). It calls cmd.Wait only AFTER the pipe is fully drained (the
// Wait/pipe-read race), and reports exit off cmd.Wait rather than pipe EOF
// alone so a grandchild that inherited the stdout writer cannot mask the exit.
func (c *stdioChild) readLoop(stdout io.ReadCloser) {
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			c.frames <- cloneBytes(line)
		}
		if err != nil {
			break
		}
	}
	c.exited <- c.cmd.Wait()
}

func (c *stdioChild) Send(frame []byte) error {
	_, err := c.stdin.Write(frame)
	return err
}

func (c *stdioChild) Frames() <-chan []byte { return c.frames }
func (c *stdioChild) Exited() <-chan error  { return c.exited }
func (c *stdioChild) LaunchHash() []byte    { return c.hash }

// Terminate closes the child's stdin (the graceful MCP shutdown signal), sends
// SIGTERM, then escalates to a whole-process-group SIGKILL after grace so a
// SIGTERM-ignoring child — or a grandchild that escaped the direct kill — is
// still reaped. Idempotent: a second call is a no-op.
func (c *stdioChild) Terminate(grace time.Duration) {
	c.mu.Lock()
	if c.stopped || c.cmd == nil || c.cmd.Process == nil {
		c.mu.Unlock()
		return
	}
	c.stopped = true
	c.mu.Unlock()

	_ = c.stdin.Close()
	signalTerm(c.cmd)
	go func() {
		t := time.NewTimer(grace)
		defer t.Stop()
		<-t.C
		// kill(-pgid) reaps the whole group; a leader already gone yields ESRCH,
		// swallowed inside signalKillGroup.
		signalKillGroup(c.cmd)
	}()
}

// sha256File returns the sha-256 of the file at path. Used both for a child's
// launch hash and by the watcher's content poll, so a swap decision compares
// like against like.
func sha256File(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func cloneBytes(b []byte) []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
