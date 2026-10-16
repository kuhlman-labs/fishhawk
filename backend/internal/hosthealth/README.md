# backend/internal/hosthealth

Portable host system-health readers for macOS and Linux, plus the thresholds, hysteresis latch and generic remedy text built on them (E85.11 / #4330). Leaf package, stdlib only. It is the probe source ADR-100 D3 says #4330 and the adaptive concurrency controller share. The ops monitor's memory and workload-container probes (`backend/internal/opsmonitor`) consume it.

**Partial delivery.** #4330 stays open: this package and the ops-monitor trigger row are the deterministic half. The first-officer seat that consumes the `ops_monitor_alert` row (one advisory finding per crossing, under the shared spend cap and skip-notice rules) lands with #4148's standing-watch runtime. The default-off dispatch hold built on these readings is INTERIM; ADR-100's adaptive concurrency controller absorbs it.

## Reader

`Reader{GOOS, Exec, ReadFile, ReadDir}`. Every field is a seam; the zero `Reader` reads the running host (`runtime.GOOS`, `exec.CommandContext` + `WaitDelay 2s`, `os.ReadFile`, `os.ReadDir`).

- **Runtime OS dispatch.** `platform()` is the single OS gate: `darwin` and `linux` get their readers, any other GOOS returns `ErrNotSupported` (wrapped, naming the OS) from every method, `Containers` included, and runs nothing. No build tags: both parsers compile everywhere, so both fixture sets run on any host. Callers map `ErrNotSupported` to `not_applicable`, never `ok`.
- **Fail closed.** A required source that fails, is empty, or does not parse is an error, never a zero reading that would read healthy. Optional signals (the macOS pressure sysctl, Linux PSI files) read as `Available: false` when unreadable; a PSI file that reads but does not parse is an error.

| Method | macOS | Linux |
|---|---|---|
| `Memory` | `sysctl -n hw.memsize`; `vm_stat` (page-size header; available = free + inactive + speculative pages; compressed = pages occupied by compressor); `sysctl -n vm.swapusage`; `sysctl -n kern.memorystatus_vm_pressure_level` (optional) | `/proc/meminfo` `MemTotal`, `MemAvailable`, `SwapTotal`, `SwapFree` (all required); `/proc/pressure/memory` some/full avg10 (optional) |
| `Processes(n)` | `top -l 1 -o mem -n <n> -stats pid,command,mem,cmprs` | `ReadDir(/proc)` numeric entries → `/proc/<pid>/status` `Name`, `VmRSS`, `VmSwap` |
| `Load` | `sysctl -n vm.loadavg`; `ps -axo stat=` (R runnable, U/D uninterruptible) | `/proc/loadavg`; `/proc/stat` `procs_running`/`procs_blocked`; `/proc/pressure/io` + `/proc/pressure/cpu` some avg10 (optional) |
| `Containers` | `docker ps --filter name=fishhawk-gate- --format {{.Names}}`, then `docker stats --no-stream --format {{.Name}}\t{{.MemUsage}} <names>` | same |

### Parsing rules

- **macOS pressure level** maps `NOTE_MEMORYSTATUS_PRESSURE_*` (xnu `bsd/sys/event.h`): `1` normal, `2` warn, `4` critical; any other value is `unknown`, never `normal`. Linux PSI has no discrete level, so `Pressure.Level` is `unknown` there and consumers threshold `SomeAvg10`.
- **macOS `top` rows.** PID is the first field, CMPRS the last, MEM the second-last, COMMAND everything between (names with spaces survive; top truncates names to 16 bytes, e.g. `com.apple.Virtua`). Sizes take `B/K/M/G/T` binary suffixes and top's `+`/`-` delta markers. PID 0 (`kernel_task`) is valid. One malformed row fails the whole read: rows are sorted by memory, so skipping one could hide the largest holder.
- **Footprint.** `FootprintBytes()` is `VmRSS + VmSwap` on Linux. On macOS MEM is the physical footprint, which already includes compressed pages (top(1), footprint(1)), so the two are not added: `mem=41G cmprs=30G → 41G`. It is `max(MEM, CMPRS)` because a process whose compressed pages are accounted outside its physical footprint was observed (the virtualization VM process, CMPRS 21G over MEM 17G); `max` never reports such a process below its compressed size.
- **Linux process scan.** A pid that vanishes between the listing and the read, and a status with a malformed `VmRSS`/`VmSwap`, are skipped; a kernel thread (no `Vm*` lines) reads a zero footprint; zero readable statuses is an error. The scan checks `ctx` per entry.
- **`MemAvailable`** is required (absent before kernel 3.14): estimating it could read a pressured host as healthy. Sources: proc(5), <https://docs.kernel.org/filesystems/proc.html>, <https://docs.kernel.org/accounting/psi.html>.
- **Load attribution.** `WaitDominated()`: with PSI (both io and cpu files read), I/O some-avg10 > CPU some-avg10; without it, uninterruptible > runnable. Swap-in under memory thrash shows up as uninterruptible wait, so a thrashing host reads wait-dominated rather than CPU-bound.

### Containers

- **Exact prefix.** `GateContainerPrefix = "fishhawk-gate-"` matches gate containers (`fishhawk-gate-<hex>`) and gate service containers (`fishhawk-gate-svc-<hex>`) as `runner/internal/gateiso` names them (`container.go`, `service.go`). docker's `name=` filter is a SUBSTRING match (<https://docs.docker.com/reference/cli/docker/container/ls/#name>), so every listed name is re-checked against the exact prefix in Go before `docker stats` is asked about it. A rename in gateiso silently drops containers from the count; the prefix is this one constant.
- **Units.** `MemUsage` (`<usage> / <limit>`) is read in binary (`KiB/MiB/GiB/TiB`) or decimal (`kB/MB/GB/TB`) units.
- Zero gate containers is a valid empty reading (`docker stats` is not run).
- **`ErrRuntimeAbsent`**: the docker CLI is not on PATH (`exec.ErrNotFound`). Any other docker failure (daemon down, a container exiting between `ps` and `stats`) is a plain error. Callers map both to `unknown`.
- **Podman residual.** On a podman-only host there is no `docker` CLI, so `Containers` returns `ErrRuntimeAbsent`: the ops monitor's `workload_containers` reads `unknown` and the runner's `host.containers` inventory command is inconclusive.
- `docker ps`/`docker stats` contact no registry, so the locked-screen credential-helper hang (AGENTS.md, #4046) is not expected to apply.

## Thresholds

`DefaultThresholds(totalRAM, cores)`; `Validate()` reports every violation (`errors.Join`).

| Field | Default | Direction |
|---|---|---|
| `AvailableEnter` / `AvailableExit` | 5% / 10% of RAM | LOW-water: trips at or below enter, clears above exit, so exit > enter |
| `SwapEnter` / `SwapExit` | 90% / 70% of swap total (ADR-100 D4 trip / recovery) | HIGH-water: exit < enter |
| `CriticalSwap` / `CriticalAvailable` | 97% (ADR-100 D4) / 2% | must be strictly beyond `SwapEnter` / `AvailableEnter` |
| `PSIEnter` / `PSIExit` | 25 / 10 (memory some-avg10 %) | HIGH-water: exit < enter, within [0,100] |
| `HolderMinBytes` | max(1 GiB, 25% of RAM) | a single floor: attribution, not a trigger, so it has no exit band; must be > 0 |
| `ContainerMemEnter` / `ContainerMemExit` | 50% / 40% of RAM | HIGH-water: exit < enter |
| `ContainerCountMax` | max(4, cores/2) | ≥ 1 |

Every fraction is in (0,1). A critical latch enters at the `Critical*` values and exits at `SwapExit` / `AvailableExit`, so a hold set by a critical episode clears only once the host is healthy again.

## Latch

`Latch` is mutex-guarded; the zero value is unlatched. `Update(aboveEnter, belowExit) (latched, crossed)` latches on `aboveEnter`, unlatches only on `belowExit`, and reports `crossed` ONLY on the unlatched → latched edge. `UpdateHighWater(v, enter, exit)` feeds `v >= enter` / `v < exit`; `UpdateLowWater` feeds `v <= enter` / `v > exit`. With enter 0.90 / exit 0.70, `0.98, 0.85, 0.98, 0.85` crosses once, then `0.60, 0.95` crosses again. The hysteresis counterfactual pins this at the LATCH layer (`TestLatch_Hysteresis`), before any emit or quiet-window dedup can mask it.

## Remedy

The remedy text is host- and codebase-agnostic: no repository path, no language-specific cache, no operator location, no host name. The only concrete names it carries are the contributors' own process names; a table hint names only its own process and no path.

- **Per-OS table** (`remedyTable`): darwin `fseventsd` (file-events daemon), `mds_stores` / `mds` (Spotlight), `com.apple.Virtualization.VirtualMachine` (the VM hosting a container runtime); linux `dockerd`, `containerd`, `kswapd` (prefix: `kswapd0`…; a symptom of pressure, not its cause). A name matches its key exactly, by prefix for prefix entries, or as the OS's truncated form of the key (16 bytes on macOS `top`, 15 on Linux `comm`).
- `GenericAction` (always), `GenericRemedy` (when no contributor has a hint), `HoldGuidance` (always last: hold new stage dispatches until the condition clears; treat stage failures in the episode as infrastructure retries, not fix-ups).
- `TopHolders(procs, minBytes, fallbackN)`: every process at or over the floor, else the `fallbackN` largest non-empty ones; largest first; at most `MaxContributors` (5); names sanitized; input not mutated.
- `ComposeRemedy(goos, contributors)` = `Largest memory holders: name (pid N, X GiB), …` + `GenericAction` + each matching hint once (or `GenericRemedy`) + `HoldGuidance`. It applies the same cap and sanitization itself.
- **Host strings are data.** Process and container names reach an untenanted audit row every tenant can read. `SanitizeName` drops invalid UTF-8, control characters (newlines included) and Unicode format characters (bidi overrides), trims, and cuts to `MaxNameBytes` (64) on a rune boundary; the readers apply it at parse time, and `TopHolders`/`ComposeRemedy` apply it again. Pids are kept.

## Tests

Fixtures are inline captured output (`darwin_test.go`, `linux_test.go`, `containers_test.go`), driven through the seams; the fakes fail any argv or path they have no entry for. Pins: the OS gate with seams that would return healthy readings (`TestReader_UnsupportedOS`); the done-means macOS host (one process at 41G, swap 98%) and a Linux PSI + meminfo fixture; malformed/empty inputs per source; the prefix re-filter against an unrelated 30GiB container; `ErrRuntimeAbsent` through both a fake and the real exec with an empty PATH; threshold direction in both directions with inverted cases; the latch band; and `TestNoHostOrRepoSpecifics`, which asserts on the shipped strings. `TestReader_LiveHost` reads the real host on darwin/linux.
