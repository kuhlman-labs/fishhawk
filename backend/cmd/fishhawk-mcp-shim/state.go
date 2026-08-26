package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// stateSchema versions the on-disk snapshot. A reader that does not recognise
// the value must not guess at the field set.
const stateSchema = "fishhawk-mcp-shim/state/v1"

// stateDirEnv lets a test (or an operator with a non-default layout) pin the
// state directory. See the state-dir coupling note in README.md: scripts/dev's
// advisory forwards this variable only when it is set in ITS OWN environment, so
// a harness-side registration that sets it out of scripts/dev's view silently
// disables the advisory.
const stateDirEnv = "FISHHAWK_MCP_SHIM_STATE_DIR"

// Swap outcomes recorded in last_swap_outcome. Each names one reachable branch
// of the swap decision, so the operator reads WHY a swap did not happen rather
// than inferring it from silence (#2831).
const (
	outcomeSwapped                  = "swapped"
	outcomeCrashRespawn             = "crash_respawn"
	outcomeDeferredNoInitialize     = "deferred_no_initialize_recorded"
	outcomeDeferredHandshakeNotSeen = "deferred_handshake_not_observed"
	outcomeDeferredInFlight         = "deferred_in_flight"
)

const (
	// handshakeEvidenceThreshold is how many result-bearing child responses count
	// as proof that the initialize lifecycle completed even though the shim never
	// MATCHED the response.
	handshakeEvidenceThreshold = 1
	// deferralLogInterval bounds how often a persistent deferral repeats itself on
	// stderr: log on any change of outcome, then at most once per interval.
	deferralLogInterval = 5 * time.Minute
)

// swapState is the per-shim swap-state snapshot, written atomically to
// <state-dir>/<shim-pid>.json on every watcher tick and every swap-state
// transition. It is deliberately FLAT (no nested objects) so a shell reader can
// grep a field without a JSON parser, and it carries no request payloads, no
// environment and no credentials — only pids, paths, content hashes and
// JSON-RPC ids.
type swapState struct {
	Schema     string `json:"schema"`
	ShimPid    int    `json:"shim_pid"`
	ShimGitSHA string `json:"shim_git_sha"`

	ChildPid        int    `json:"child_pid"`
	ChildPath       string `json:"child_path"`
	ChildLaunchHash string `json:"child_launch_hash"`

	PendingSwapHash string `json:"pending_swap_hash"`
	PendingSince    string `json:"pending_since"`

	HandshakeDone     bool `json:"handshake_done"`
	HandshakePresumed bool `json:"handshake_presumed"`
	ServedResults     int  `json:"served_results"`

	InFlight            int    `json:"in_flight"`
	OldestInFlightID    string `json:"oldest_in_flight_id"`
	OldestInFlightSince string `json:"oldest_in_flight_since"`
	QuiesceExpired      bool   `json:"quiesce_expired"`

	LastSwapAt      string `json:"last_swap_at"`
	LastSwapOutcome string `json:"last_swap_outcome"`
	UpdatedAt       string `json:"updated_at"`
}

// defaultStateDir is <temp>/fishhawk-mcp-shim. os.TempDir honours $TMPDIR, which
// is the same resolution the zsh side uses (${TMPDIR:-/tmp}), so Go and shell
// agree on the default with no configuration.
func defaultStateDir() string {
	return filepath.Join(os.TempDir(), "fishhawk-mcp-shim")
}

// resolveStateDir applies the FISHHAWK_MCP_SHIM_STATE_DIR override over the
// default.
func resolveStateDir() string {
	if v := os.Getenv(stateDirEnv); v != "" {
		return v
	}
	return defaultStateDir()
}

// stateFilePath is the snapshot path for one shim pid.
func stateFilePath(dir string, pid int) string {
	return filepath.Join(dir, fmt.Sprintf("%d.json", pid))
}

// writeState writes st to <dir>/<shim_pid>.json atomically: a temp file in the
// SAME directory (so the rename cannot cross a filesystem boundary) is written,
// closed, then renamed over the target. os.Rename replaces an existing file
// atomically on POSIX, so a concurrent reader observes either the old snapshot
// or the new one — never a partial write. Any failure removes the temp so no
// litter accumulates.
func writeState(dir string, st swapState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, stateFilePath(dir, st.ShimPid)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// removeState deletes one shim's snapshot. Best-effort: an already-absent file
// is not an error.
func removeState(dir string, pid int) {
	_ = os.Remove(stateFilePath(dir, pid))
}

// processAlive reports whether pid is a live process this user can signal.
// Signal 0 performs error checking without delivering a signal. It cannot
// distinguish a REUSED pid — the same residual scripts/test's lease pruning
// accepts — so at worst a reused pid surfaces one stale advisory line; nothing
// destructive keys off this.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// stateSkip records one file the reader declined to use, and why. A skip is
// never fatal: a diagnostic that refuses to report because one file is
// unreadable is worse than one that reports the rest.
type stateSkip struct {
	Path    string
	Reason  string
	DeadPid bool
}

func (s stateSkip) String() string { return s.Path + ": " + s.Reason }

// loadStates reads every *.json snapshot under dir and returns those belonging
// to LIVE shim pids, plus a skip record per file it declined. An absent dir
// yields no states, no skips and no error.
func loadStates(dir string) ([]swapState, []stateSkip) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var states []swapState
	var skips []stateSkip
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			skips = append(skips, stateSkip{Path: p, Reason: "unreadable: " + err.Error()})
			continue
		}
		var st swapState
		if err := json.Unmarshal(b, &st); err != nil {
			skips = append(skips, stateSkip{Path: p, Reason: "unparseable: " + err.Error()})
			continue
		}
		if !processAlive(st.ShimPid) {
			skips = append(skips, stateSkip{
				Path:    p,
				Reason:  fmt.Sprintf("shim pid %d is not running (leftover state file)", st.ShimPid),
				DeadPid: true,
			})
			continue
		}
		states = append(states, st)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ShimPid < states[j].ShimPid })
	return states, skips
}

// stateVerdict is the classification of one snapshot against the binary
// currently on disk.
type stateVerdict string

const (
	// verdictCurrent — the running child was launched from the bytes on disk.
	verdictCurrent stateVerdict = "current"
	// verdictPending — the on-disk binary differs but the swap is legitimately
	// still in progress (or its age is unknown). This is the FAIL-SAFE verdict:
	// a diagnostic that cries wolf on a just-rebuilt binary would be turned off.
	verdictPending stateVerdict = "pending"
	// verdictStale — the on-disk binary differs AND the swap has been pending at
	// least the grace period. The session is running code that is no longer on
	// disk and the shim is not fixing it (#2831).
	verdictStale stateVerdict = "stale"
)

// classifyState decides one snapshot's verdict. onDiskHash is the recomputed
// hash of that snapshot's own child_path, or nil when it could not be hashed —
// which classifies pending, never stale.
func classifyState(st swapState, onDiskHash []byte, now time.Time, grace time.Duration) stateVerdict {
	if onDiskHash == nil {
		return verdictPending
	}
	if hex.EncodeToString(onDiskHash) == st.ChildLaunchHash {
		return verdictCurrent
	}
	if st.PendingSince == "" {
		return verdictPending
	}
	since, err := time.Parse(time.RFC3339, st.PendingSince)
	if err != nil {
		return verdictPending
	}
	if now.Sub(since) >= grace {
		return verdictStale
	}
	return verdictPending
}

// renderStatus writes the human-readable status report to w and returns an exit
// code — always exitOK, because a diagnostic that fails its caller's `set -e`
// would be worse than useless.
//
// staleOnly is the MACHINE-CONSUMABLE mode scripts/dev keys its advisory off:
// it prints ONLY stale entries and is SILENT ON BOTH STREAMS when nothing is
// stale. Skip reasons and the leftover-file prune belong to the human-readable
// --status mode alone — dead-pid state files are the steady state (a SIGKILLed
// shim never removes its own file), so routing their reasons into the machine
// mode would make every `scripts/dev up` on a used dev box print noise while
// nothing is actually stale.
func renderStatus(w, errw io.Writer, dir string, staleOnly bool, grace time.Duration, now func() time.Time) int {
	states, skips := loadStates(dir)
	at := now()
	shown := 0
	for _, st := range states {
		h, err := sha256File(st.ChildPath)
		if err != nil {
			h = nil
		}
		verdict := classifyState(st, h, at, grace)
		if staleOnly && verdict != verdictStale {
			continue
		}
		shown++
		writeStateBlock(w, st, verdict, h, at)
	}
	if staleOnly {
		return exitOK
	}
	if shown == 0 {
		_, _ = fmt.Fprintf(w, "no live fishhawk-mcp-shim state files under %s\n", dir)
	}
	for _, s := range skips {
		_, _ = fmt.Fprintf(errw, "fishhawk-mcp-shim: skipped %s\n", s)
		if s.DeadPid {
			// Prune after its own crashed predecessors: loadStates already proved
			// the pid is gone, and leaving the file behind is what makes leftover
			// state the steady state.
			if err := os.Remove(s.Path); err == nil {
				_, _ = fmt.Fprintf(errw, "fishhawk-mcp-shim: removed leftover state file %s\n", s.Path)
			}
		}
	}
	return exitOK
}

// writeStateBlock renders one snapshot as an operator-readable block. Every
// field the issue's AC2 names is present: the verdict, both pids, the child
// path, both hashes, how long the swap has been pending, the handshake status,
// served-result evidence, in-flight pressure, and the last swap attempt with
// its outcome.
func writeStateBlock(w io.Writer, st swapState, verdict stateVerdict, onDiskHash []byte, now time.Time) {
	handshake := "NEVER OBSERVED"
	switch {
	case st.HandshakePresumed:
		handshake = "presumed (initialize never matched; child served results)"
	case st.HandshakeDone:
		handshake = "done"
	}
	_, _ = fmt.Fprintf(w, "%s: shim pid %d (git_sha=%s)\n", strings.ToUpper(string(verdict)), st.ShimPid, st.ShimGitSHA)
	_, _ = fmt.Fprintf(w, "  child pid %d  path %s\n", st.ChildPid, st.ChildPath)
	_, _ = fmt.Fprintf(w, "  baseline hash %s  on-disk hash %s\n", shortHash(st.ChildLaunchHash), shortHash(hex.EncodeToString(onDiskHash)))
	_, _ = fmt.Fprintf(w, "  pending swap hash %s  pending for %s\n", shortHash(st.PendingSwapHash), pendingAge(st.PendingSince, now))
	_, _ = fmt.Fprintf(w, "  handshake %s  served results %d\n", handshake, st.ServedResults)
	_, _ = fmt.Fprintf(w, "  in-flight %d  oldest in-flight id %s age %s\n",
		st.InFlight, orNone(st.OldestInFlightID), pendingAge(st.OldestInFlightSince, now))
	_, _ = fmt.Fprintf(w, "  last swap at %s  outcome %s  (snapshot %s)\n",
		orNone(st.LastSwapAt), orNone(st.LastSwapOutcome), orNone(st.UpdatedAt))
	if verdict == verdictStale {
		_, _ = fmt.Fprintf(w, "  the live session is on a stale fishhawk-mcp binary; run /mcp to reconnect (#2831)\n")
	}
}

// shortHash abbreviates a hex hash for display. An empty hash renders as
// "(none)" rather than an empty column.
func shortHash(hexHash string) string {
	if hexHash == "" {
		return "(none)"
	}
	if len(hexHash) > 12 {
		return hexHash[:12]
	}
	return hexHash
}

// pendingAge renders how long ago an RFC3339 stamp was, or "(none)" when the
// stamp is absent and "(unparseable)" when it cannot be read.
func pendingAge(stamp string, now time.Time) string {
	if stamp == "" {
		return "(none)"
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return "(unparseable)"
	}
	return now.Sub(t).Truncate(time.Second).String()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
