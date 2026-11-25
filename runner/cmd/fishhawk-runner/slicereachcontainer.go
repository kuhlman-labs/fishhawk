package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
)

// Slice gate: the IN-IMAGE reach load on the container gate path (#5524,
// ADR-090 A3.7 as amended 2026-11-23).
//
// On the container isolation path under a non-hosted profile the reach load
// (`go list -e -json` plus the imports-only parse) runs INSIDE the gate image
// as one extra container exec over the mounted clone, never on the host. The
// runner embeds slicereachhelper/main.go — a stdlib-only port of
// slicereach.go — passes it base64-encoded through the env, and the image's
// own go compiles and runs it. The exec goes through runGateInContainer
// directly (the container half of the gate exec, never
// runBoundedGateArgvDisposed's host switch, and no markSeamReached: it is not
// a gate verdict): the same mounts, --network=none, env pins and launch
// marker as the verify, NO gate service (withoutGateServices), and a seed it
// hands to the deciding verify on the same clone (withSeedHandoff), so the
// tree pays one host-side seed — ADR-063's unchanged verify seed — not two.
// The host only parses ONE nonce-framed result line and accepts it when the
// reason is in the fixed vocabulary and the graph names clean, relative,
// consistent directories. Every other outcome widens with a named reason.

//go:embed slicereachhelper/main.go
var sliceReachHelperSource []byte

const (
	// sliceReachResultMarker leads the helper's one result line.
	sliceReachResultMarker = "FISHHAWK_SLICE_REACH_RESULT"
	// sliceReachHelperEnv carries the base64 (StdEncoding, no newlines)
	// helper source into the container.
	sliceReachHelperEnv = "FISHHAWK_SLICE_REACH_HELPER_B64"
	// sliceReachNonceEnv carries the per-exec result nonce.
	sliceReachNonceEnv = "FISHHAWK_SLICE_REACH_NONCE"
	// sliceReachContainerScript is the fixed in-container program: decode the
	// helper into a fresh temp dir and run it with the image's go over /work
	// (gateiso.MountWork), outside any module. GOCACHE is a fresh dir under
	// that temp dir, never the gate's (shared, writable) cache volume, so a
	// build-cache entry an earlier gate exec planted cannot stand in for the
	// helper the host then trusts to choose the test scope. The cost is a
	// cold helper compile per reach exec (about 2s in the gate image).
	sliceReachContainerScript = `set -eu; d=$(mktemp -d); printf "%s" "$FISHHAWK_SLICE_REACH_HELPER_B64" | base64 -d > "$d/main.go"; cd "$d"; exec env GOWORK=off GOFLAGS= CGO_ENABLED=0 GOENV=off GOCACHE="$d/gocache" go run ./main.go ` + gateiso.MountWork
	// sliceReachResultCap bounds the result line's JSON document.
	sliceReachResultCap = 32 << 20

	sliceReachReasonContainerUnavailable = "reach_container_unavailable:"
	sliceReachReasonContainerExit        = "reach_container_exit:"
	sliceReachReasonContainerUnparseable = "reach_container_output_unparseable"
	sliceReachReasonLoadTimeout          = "reach_load_failed:load_timeout"
	sliceReachReasonNotContainerPath     = sliceReachReasonContainerUnavailable + "not_container_path"
)

// sliceReachContainerArgv is the fixed reach exec argv.
var sliceReachContainerArgv = []string{"sh", "-c", sliceReachContainerScript}

var (
	// sliceReachContainerTimeout bounds the reach container EXEC only — it is
	// runGateInContainer's exec timeout. The credential probe, image
	// resolution, passwd read, cache-volume prepare and the seed keep their
	// own existing bounds; none of them is charged to it, nor is it charged
	// to the gate's #4023 budget. A test lowers it.
	sliceReachContainerTimeout = 4 * time.Minute
	// sliceReachContainerExecFn is the exec seam; production is
	// runGateInContainer. A test stubs it.
	sliceReachContainerExecFn = runGateInContainer
)

// sliceReachLoadContainer is the container-path reach loader, with
// sliceReachLoad's signature. It returns the helper's graph, or a non-empty
// reason that widens the gate to the full form.
func sliceReachLoadContainer(ctx context.Context, wt string) (map[string][]string, map[string]bool, string) {
	// Defence in depth: the hook routes only a non-hosted container selection
	// here, and the exec below enters the container half of the gate exec
	// directly, so a host exec of the script is structurally impossible.
	sel := sliceGateSelectionFn(ctx)
	if sel.Path != gateiso.PathContainer || sel.Profile == gateiso.ProfileHosted {
		return nil, nil, sliceReachReasonNotContainerPath
	}
	lintDir, err := os.MkdirTemp("", "fishhawk-slicereach-lint-*")
	if err != nil {
		return nil, nil, sliceReachReasonContainerUnavailable + "lint_cache_dir"
	}
	defer func() { _ = os.RemoveAll(lintDir) }()
	sanitized := withIsolatedLintCache(sanitizedGateEnv(), lintDir)
	nonce := gateiso.NewLaunchNonce()
	extras := []string{
		sliceReachHelperEnv + "=" + base64.StdEncoding.EncodeToString(sliceReachHelperSource),
		sliceReachNonceEnv + "=" + nonce,
	}
	execCtx := withSeedHandoff(withLaunchMarker(ctx))
	out, code, disp := sliceReachContainerExecFn(withoutGateServices(execCtx), sel, sliceReachContainerArgv, wt, lintDir, sanitized, extras, sliceReachContainerTimeout)
	switch {
	case disp == gateTimedOut:
		return nil, nil, sliceReachReasonLoadTimeout
	case disp != gateExecuted:
		return nil, nil, sliceReachReasonContainerUnavailable + disp.String()
	case code != 0:
		logEvent(sliceReachLogSink, "slice_reach_container_failed", map[string]string{
			"exit_code": strconv.Itoa(code),
			"tail":      gateOutputTail(out),
		})
		return nil, nil, sliceReachReasonContainerExit + strconv.Itoa(code)
	}
	return sliceReachParseContainerOutput(out, nonce)
}

// sliceReachContainerResult is the helper's result document.
type sliceReachContainerResult struct {
	Reason    string              `json:"reason,omitempty"`
	Imports   map[string][]string `json:"imports,omitempty"`
	Emittable []string            `json:"emittable,omitempty"`
}

// sliceReachParseContainerOutput parses the exec's combined output: exactly
// ONE line carrying this exec's nonce, a JSON document within
// sliceReachResultCap with no unknown field and no trailing data, and either
// an allowed reason with no graph or a non-empty, clean, consistent graph.
// Anything else is reach_container_output_unparseable.
func sliceReachParseContainerOutput(out, nonce string) (map[string][]string, map[string]bool, string) {
	prefix := sliceReachResultMarker + " " + nonce + " "
	var doc string
	matches := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			doc = rest
			matches++
		}
	}
	if matches != 1 || len(doc) > sliceReachResultCap {
		return nil, nil, sliceReachReasonContainerUnparseable
	}
	var res sliceReachContainerResult
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return nil, nil, sliceReachReasonContainerUnparseable
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, sliceReachReasonContainerUnparseable
	}
	if res.Reason != "" {
		if !sliceReachContainerReasonOK(res.Reason) || len(res.Imports) != 0 || len(res.Emittable) != 0 {
			return nil, nil, sliceReachReasonContainerUnparseable
		}
		return nil, nil, sliceGateBoundReason(res.Reason)
	}
	if len(res.Imports) == 0 {
		return nil, nil, sliceReachReasonContainerUnparseable
	}
	for dir, edges := range res.Imports {
		if !sliceReachContainerDirOK(dir) {
			return nil, nil, sliceReachReasonContainerUnparseable
		}
		for _, e := range edges {
			if _, ok := res.Imports[e]; !ok || !sliceReachContainerDirOK(e) {
				return nil, nil, sliceReachReasonContainerUnparseable
			}
		}
	}
	emittable := make(map[string]bool, len(res.Emittable))
	for _, d := range res.Emittable {
		if _, ok := res.Imports[d]; !ok || !sliceReachContainerDirOK(d) {
			return nil, nil, sliceReachReasonContainerUnparseable
		}
		emittable[d] = true
	}
	return res.Imports, emittable, ""
}

// sliceReachContainerReasonOK admits only the helper's fixed refusal
// vocabulary, with no control character.
func sliceReachContainerReasonOK(r string) bool {
	if strings.IndexFunc(r, func(c rune) bool { return c < 0x20 || c == 0x7f }) >= 0 {
		return false
	}
	return r == sliceReachUnsupported ||
		strings.HasPrefix(r, sliceReachUnsupported+":") ||
		strings.HasPrefix(r, sliceReachFailed) ||
		strings.HasPrefix(r, "reach_dir_outside_clone:")
}

// sliceReachContainerDirOK admits `.` or a non-empty, relative,
// path.Clean-stable slash path with no `..` segment and no control or NUL
// character.
func sliceReachContainerDirOK(d string) bool {
	if d == "." {
		return true
	}
	if d == "" || strings.HasPrefix(d, "/") || path.Clean(d) != d {
		return false
	}
	if strings.IndexFunc(d, func(c rune) bool { return c < 0x20 || c == 0x7f }) >= 0 {
		return false
	}
	for _, seg := range strings.Split(d, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}
