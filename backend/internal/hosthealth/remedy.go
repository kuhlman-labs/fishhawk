package hosthealth

import (
	"fmt"
	"slices"
	"strings"
)

// The remedy text is host- and codebase-agnostic by contract: it names no
// repository path, no language-specific cache, no operator location and no
// host name (pinned by TestNoHostOrRepoSpecifics). The only concrete names
// it carries are the contributors' own process names, and a table hint
// names only its own process.

// GenericAction is the first step for any memory-pressure episode.
const GenericAction = "Free memory: stop or restart the largest holder named here if its work is not needed now, and run fewer concurrent workloads until swap and pressure recover."

// GenericRemedy stands in for a per-OS hint when no contributor is a known
// process.
const GenericRemedy = "No known cause matches these holders: find out why the largest one grew (a leak, a runaway job, a file-watch or indexing storm) before restarting it, and leave it stopped if it is not needed."

// HoldGuidance is what the operator does with the workload while the
// condition holds.
const HoldGuidance = "Hold new stage dispatches until the condition clears; treat stage failures during this episode as infrastructure retries, not fix-ups."

// MaxContributors caps how many processes TopHolders returns and
// ComposeRemedy names: the names reach an audit row every tenant can read.
const MaxContributors = 5

// remedyEntry is one known process. A name matches when it equals key, has
// key as a prefix (prefix entries, e.g. kswapd0), or is the OS's truncated
// form of key.
type remedyEntry struct {
	key    string
	prefix bool
	hint   string
}

// remedyOS is one OS's table plus the length at which it truncates process
// names (macOS top prints 16 bytes, Linux comm is 15).
type remedyOS struct {
	truncation int
	entries    []remedyEntry
}

var remedyTable = map[string]remedyOS{
	"darwin": {truncation: 16, entries: []remedyEntry{
		{key: "fseventsd", hint: "fseventsd is the file-events daemon: it grows when very many files change on watched volumes. Restarting it releases the memory (the system relaunches it), and keeping high-churn directories such as build caches and scratch checkouts out of file-event and indexing coverage prevents a repeat."},
		{key: "mds_stores", hint: "mds_stores maintains the Spotlight index and keeps growing while indexed volumes churn. Exclude high-churn directories from Spotlight indexing and let the reindex settle."},
		{key: "mds", hint: "mds is the Spotlight metadata server and grows with indexing work. Exclude high-churn directories from Spotlight indexing so it settles."},
		{key: "com.apple.Virtualization.VirtualMachine", hint: "com.apple.Virtualization.VirtualMachine is the virtual machine hosting a container runtime, so its memory is the containers' memory. Stop idle containers, run fewer at once, or lower the runtime's memory allocation."},
	}},
	"linux": {truncation: 15, entries: []remedyEntry{
		{key: "dockerd", hint: "dockerd is the Docker daemon; its own growth tracks container churn and log volume. Prune stopped containers and unused images, and restart it during a quiet period."},
		{key: "containerd", hint: "containerd runs the containers themselves, so its growth tracks how many run at once. Stop idle containers and run fewer concurrently."},
		{key: "kswapd", prefix: true, hint: "kswapd is the kernel page-reclaim thread. Its activity is a symptom of memory pressure, not the cause: act on the largest user-space holder instead."},
	}},
}

// hintFor returns the per-OS hint for a process name and the table key it
// matched.
func hintFor(goos, name string) (key, hint string, ok bool) {
	table, ok := remedyTable[goos]
	if !ok || name == "" {
		return "", "", false
	}
	for _, e := range table.entries {
		switch {
		case name == e.key,
			e.prefix && strings.HasPrefix(name, e.key),
			len(name) == table.truncation && strings.HasPrefix(e.key, name):
			return e.key, e.hint, true
		}
	}
	return "", "", false
}

// TopHolders picks the contributors to name: every process whose footprint
// is at or over minBytes, else (none is) the fallbackN largest non-empty
// ones. Largest first, at most MaxContributors, names sanitized.
func TopHolders(procs []Process, minBytes uint64, fallbackN int) []Process {
	sorted := slices.Clone(procs)
	sortByFootprint(sorted)
	var out []Process
	for _, p := range sorted {
		if p.FootprintBytes() >= minBytes {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		for _, p := range sorted {
			if len(out) >= fallbackN {
				break
			}
			if p.FootprintBytes() > 0 {
				out = append(out, p)
			}
		}
	}
	return boundContributors(out)
}

// boundContributors returns a copy capped at MaxContributors with sanitized
// names.
func boundContributors(procs []Process) []Process {
	out := slices.Clone(procs[:min(len(procs), MaxContributors)])
	for i := range out {
		out[i].Name = SanitizeName(out[i].Name)
	}
	return out
}

// ComposeRemedy builds the remedy for a memory-pressure crossing: the
// contributors (at most MaxContributors, names sanitized), GenericAction,
// each matching per-OS hint once (GenericRemedy when none matches), then
// HoldGuidance.
func ComposeRemedy(goos string, contributors []Process) string {
	contributors = boundContributors(contributors)
	var parts []string
	if len(contributors) > 0 {
		descs := make([]string, len(contributors))
		for i, p := range contributors {
			descs[i] = describeHolder(p)
		}
		parts = append(parts, "Largest memory holders: "+strings.Join(descs, ", ")+".")
	}
	parts = append(parts, GenericAction)
	seen := map[string]bool{}
	var hints []string
	for _, p := range contributors {
		if key, hint, ok := hintFor(goos, p.Name); ok && !seen[key] {
			seen[key] = true
			hints = append(hints, hint)
		}
	}
	if len(hints) == 0 {
		hints = []string{GenericRemedy}
	}
	parts = append(parts, hints...)
	parts = append(parts, HoldGuidance)
	return strings.Join(parts, " ")
}

func describeHolder(p Process) string {
	if p.Name == "" {
		return fmt.Sprintf("pid %d (%s)", p.PID, formatBytes(p.FootprintBytes()))
	}
	return fmt.Sprintf("%s (pid %d, %s)", p.Name, p.PID, formatBytes(p.FootprintBytes()))
}

func formatBytes(b uint64) string {
	if b >= gib {
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	}
	return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
}
