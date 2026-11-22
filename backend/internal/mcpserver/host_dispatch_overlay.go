package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// host_dispatch_overlay.go is the stdio status overlay (#5402): the interim,
// MCP-local way the operator's own fishhawk-mcp process tells the truth about
// the host dispatcher until the server heartbeat (#5489) supersedes it (the
// Captain's 2026-11-20 ruling c). The backend's campaign awaiting block says
// "the host dispatcher spawns this stage on its next tick" for every
// host_dispatcher item; it cannot know whether a dispatcher is running, paused,
// capped or refusing, because that state lives only in the dispatcher's
// host-local status.json. This file reads that file, read-only, and rewrites the
// block (fishhawk_get_campaign_status) or adds a host_dispatch block
// (fishhawk_get_run_status).
//
// It runs only when hostDispatchOverlayEnabled: the operator's stdio process
// (Config.LocalHostRungs) with a non-run-bound bearer. Over fishhawkd's /mcp
// route the host's files are the daemon's, and an in-runner fhm_ agent never
// reads the host, so the backend's block passes through unchanged there.

// Overlay states (hostDispatchObservation.State).
const (
	hostDispatchStateLive       = "live"
	hostDispatchStateWaiting    = "waiting"
	hostDispatchStateHeld       = "held"
	hostDispatchStateRefusing   = "refusing"
	hostDispatchStateCapped     = "capped"
	hostDispatchStatePaused     = "paused"
	hostDispatchStateStale      = "stale"
	hostDispatchStateAbsent     = "absent"
	hostDispatchStateUnverified = "unverified"
)

// Awaiting kinds and reasons the overlay writes. host_dispatcher /
// host_dispatch is the backend's own pair, kept for live and waiting.
const (
	hostDispatchAwaitKind        = "host_dispatcher"
	hostDispatchAwaitKindHuman   = "human"
	hostDispatchReasonDispatch   = "host_dispatch"
	hostDispatchReasonPaused     = "host_dispatch_paused"
	hostDispatchReasonCapped     = "host_dispatch_capped"
	hostDispatchReasonRefused    = "host_dispatch_refused"
	hostDispatchReasonAbsent     = "host_dispatcher_absent"
	hostDispatchReasonUnverified = "host_dispatch_unverified"
	hostDispatchStatusCmd        = "fishhawk-mcp host-dispatch --status"
	hostDispatchCampaignHeldCode = "campaign_dispatch_held"
	// hostDispatchAwaitKindSystem byte-matches the server's awaitingKindSystem
	// (backend/internal/server/campaign_item_awaiting.go).
	hostDispatchAwaitKindSystem = "system"
	// hostDispatchReasonInFlight byte-matches the server's
	// awaitingReasonInFlight (campaign_item_awaiting.go): system work is in
	// flight and the item waits on it, not on an operator.
	hostDispatchReasonInFlight = "in_flight"
)

// hostDispatchReviewRoundOrphanAfter is how long an implement review round
// hold on an acceptance stage reads as routine (#5298). Past it the next_step
// says the round may be orphaned and names the read and the re-run verbs.
const hostDispatchReviewRoundOrphanAfter = 30 * time.Minute

// hostDispatchLineageLockStuckAfter is how long a lineage_lock_held refusal
// (#5485) reads as routine waiting: a runner of the run's lineage, typically
// the previous stage tearing down its preview, still holds the lineage lock.
// Past it the holder may be wedged and the read escalates to the operator.
const hostDispatchLineageLockStuckAfter = 15 * time.Minute

// hostDispatchOverlayReadCap bounds the GetRun reads one campaign status call
// spends on working_dir rungs. Each read is a backend round trip inside a
// status call the operator polls on every drive tick. A decisive env or cwd
// rung needs no read; otherwise each distinct run is read until the cap. An
// item the cap skipped keeps the call rungs and TMPDIR (which speak for every
// item) and, when those find nothing, reads unverified, never absent.
const hostDispatchOverlayReadCap = 16

// hostDispatchPathsTriedMax bounds paths_tried: past it the list keeps the
// first entries and a final "(+N more)" entry.
const hostDispatchPathsTriedMax = 6

// hostDispatchCheckoutStateDir is the state dir scripts/dev pins inside a
// checkout (HD_STATE_DIR, scripts/dev:67), relative to the checkout root.
var hostDispatchCheckoutStateDir = filepath.Join(".fishhawk", "cache", "host-dispatch")

// hostDispatchDefaultStateDirName is `fishhawk-mcp host-dispatch`'s default
// --state-dir under os.TempDir() (cmd/fishhawk-mcp defaultHostDispatchStateDir).
const hostDispatchDefaultStateDirName = "fishhawk-host-dispatch"

// The per-run refusal code sets. Membership is tested on the RAW code read
// from the file; output carries the bounded copy. A new code reaches the right
// arm only by joining a set here or by a dedicated arm in observeHostDispatch
// (lineage_lock_held, #5485); an unknown non-benign code reads refusing.
var (
	// hostDispatchPausedRefusals are the mid-tick Admit pause refusals (a pause
	// that landed after the tick's own pause check).
	hostDispatchPausedRefusals = map[string]bool{hdRefusePaused: true, hdRefusePauseCheck: true}
	// hostDispatchHoldRefusals are deliberate holds: the stage waits for the
	// campaign or for the operator (run_dispatch_held, E110.50 / #5486), and a
	// hand dispatch would bypass the hold (the run hold's marker refuses it
	// too). dispatch_hold_unavailable is NOT a hold: it stays on the default
	// refusing arm, since nothing says the run is held.
	hostDispatchHoldRefusals = map[string]bool{hdRefuseCampaignNotRunning: true, hostDispatchCampaignHeldCode: true, hdRefuseRunDispatchHeld: true}
	// hostDispatchWaitingRefusals clear on their own on a later tick.
	// cap_reached:per_tick is global-only (never a per-run entry) and so is
	// not listed.
	hostDispatchWaitingRefusals = map[string]bool{
		hdRefuseCapInFlight:           true,
		hdRefuseSlotQueued:            true,
		hdRefuseMemoryPressure:        true,
		hdRefuseMarkerNotTransitioned: true,
		"dependency_not_satisfied":    true,
		"wave_not_integrated":         true,
	}
	// hostDispatchReviewRoundRefusals are the implement review round holds on
	// an acceptance stage (#5298): system work (the review round) is in flight
	// or expected, so the block reads system/in_flight, not human.
	hostDispatchReviewRoundRefusals = map[string]bool{
		hdRefuseImplementReviewInFlight:   true,
		hdRefuseImplementReviewNotStarted: true,
	}
)

// hostDispatchObservation is the overlay's read of the dispatcher for one item
// or run. Tool output only: plain exported fields, no custom MarshalJSON, so
// the SDK's reflected output schema matches what is emitted. Every string that
// came from status.json is bounded by boundHostDispatchText.
type hostDispatchObservation struct {
	State              string   `json:"state" jsonschema:"live | waiting | held | refusing | capped | paused | stale | absent | unverified"`
	StatusPath         string   `json:"status_path,omitempty" jsonschema:"the status.json the state was read from"`
	PathsTried         []string `json:"paths_tried,omitempty" jsonschema:"the state dirs probed, in order; a skipped (untrusted) dir carries its reason; at most 6 entries"`
	LastTickAt         string   `json:"last_tick_at,omitempty" jsonschema:"RFC 3339"`
	LastCompleteTickAt string   `json:"last_complete_tick_at,omitempty" jsonschema:"RFC 3339; absent from a pre-#5402 dispatcher"`
	TickFailureCode    string   `json:"tick_failure_code,omitempty" jsonschema:"the failing early return of the dispatcher's latest tick"`
	RefusalCode        string   `json:"refusal_code,omitempty" jsonschema:"this run's latest non-benign refusal code"`
	RefusalDetail      string   `json:"refusal_detail,omitempty"`
	RefusalStageID     string   `json:"refusal_stage_id,omitempty"`
	RefusalFirstAt     string   `json:"refusal_first_at,omitempty" jsonschema:"RFC 3339"`
	RefusalLastAt      string   `json:"refusal_last_at,omitempty" jsonschema:"RFC 3339"`
	DailyCap           int      `json:"daily_cap,omitempty"`
	SpawnsToday        int      `json:"spawns_today,omitempty"`
	CapResetsAt        string   `json:"cap_resets_at,omitempty" jsonschema:"RFC 3339: when the daily cap resets (00:00:00Z)"`
	Detail             string   `json:"detail,omitempty" jsonschema:"why the state was chosen"`
}

// hostDispatchVerdict is what an observation does to an awaiting block: an
// empty Kind leaves the block unchanged (live).
type hostDispatchVerdict struct {
	Kind     string
	Reason   string
	NextStep string
}

// hostDispatchProbe is the result of walking the state-dir ladder.
type hostDispatchProbe struct {
	found bool
	dir   string
	st    HostDispatcherStatus
	// readErr is non-empty when the winning rung's status.json exists but
	// could not be read (corrupt, a symlink): the state is unverified.
	readErr string
	tried   []string
}

// hostDispatchOverlayEnabled gates every overlay read: the operator's own
// stdio process only, never a run-bound in-runner bearer (the doctor rungs'
// posture).
func (r *runResolver) hostDispatchOverlayEnabled() bool {
	return r.localHostRungs && !r.runBoundToken
}

func (r *runResolver) hostDispatchEnv(key string) string {
	if r.getenv == nil {
		return os.Getenv(key)
	}
	return r.getenv(key)
}

func (r *runResolver) hostDispatchClock() time.Time {
	if r.hostDispatchNow != nil {
		return r.hostDispatchNow()
	}
	return time.Now()
}

// hostDispatchOwnerFunc is the S4 owner seam (nil: fileOwnerUID).
func (r *runResolver) hostDispatchOwnerFunc() func(os.FileInfo) (int, bool) {
	if r.hostDispatchOwner != nil {
		return r.hostDispatchOwner
	}
	return fileOwnerUID
}

// hostDispatchCallRungs is the call-level ladder head, probed with no run read:
// FISHHAWK_HOST_DISPATCH_STATE_DIR alone when it is set (exclusive), else the
// MCP process cwd's <cwd>/.fishhawk/cache/host-dispatch (the operator's
// checkout, where scripts/dev pins the dispatcher). A getwd error skips the
// cwd rung.
func (r *runResolver) hostDispatchCallRungs() []string {
	if dir := r.hostDispatchEnv(hostDispatchStateDirEnv); dir != "" {
		return []string{dir}
	}
	getwd := r.getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	cwd, err := getwd()
	if err != nil || cwd == "" {
		return nil
	}
	return []string{filepath.Join(cwd, hostDispatchCheckoutStateDir)}
}

// hostDispatchWorkingDirRung is a run's <working_dir>/.fishhawk/cache/
// host-dispatch, or "" for an empty or relative working_dir.
func hostDispatchWorkingDirRung(workingDir string) string {
	if workingDir == "" || !filepath.IsAbs(workingDir) {
		return ""
	}
	return filepath.Join(workingDir, hostDispatchCheckoutStateDir)
}

// hostDispatchDefaultRung is the ladder's last rung, `fishhawk-mcp
// host-dispatch`'s own default.
func hostDispatchDefaultRung() string {
	return filepath.Join(os.TempDir(), hostDispatchDefaultStateDirName)
}

// hostDispatchLadder walks state-dir rungs in order. The first trusted rung
// with a non-stale status (or an unreadable one) ends the walk; a STALE hit is
// remembered and the walk continues, so a dead dispatcher's leftover file
// cannot hide a live one further down. When every hit is stale, the first
// stale hit is reported. Forks share one per-call rung cache, so each dir is
// read at most once per call and every ladder sees the same read of it.
type hostDispatchLadder struct {
	r     *runResolver
	now   time.Time
	cache map[string]hostDispatchRung
	seen  map[string]bool
	tried []string
	hit   *hostDispatchProbe
	stale *hostDispatchProbe
}

// hostDispatchRung is one dir's read: its paths_tried entry and its status
// (nil when absent or untrusted).
type hostDispatchRung struct {
	tried string
	hit   *hostDispatchProbe
	stale bool
}

func (r *runResolver) newHostDispatchLadder(now time.Time) *hostDispatchLadder {
	return &hostDispatchLadder{r: r, now: now, cache: map[string]hostDispatchRung{}, seen: map[string]bool{}}
}

// fork copies the walk so far; the copy continues independently over the
// shared rung cache.
func (l *hostDispatchLadder) fork() *hostDispatchLadder {
	seen := make(map[string]bool, len(l.seen))
	for k := range l.seen {
		seen[k] = true
	}
	return &hostDispatchLadder{r: l.r, now: l.now, cache: l.cache, seen: seen,
		tried: append([]string(nil), l.tried...), hit: l.hit, stale: l.stale}
}

// done reports whether a decisive (non-stale) rung was found.
func (l *hostDispatchLadder) done() bool { return l.hit != nil }

// probe checks one rung, read-only: it never creates, chmods or writes. A
// rung's directory is trusted only if it passes the dispatcher's own state-dir
// check (hostDispatchStateDirProblem); an untrusted rung is skipped and named
// with its reason.
func (l *hostDispatchLadder) probe(dir string) {
	if l.done() || dir == "" {
		return
	}
	dir = filepath.Clean(dir)
	if l.seen[dir] {
		return
	}
	l.seen[dir] = true
	rg, ok := l.cache[dir]
	if !ok {
		rg = l.read(dir)
		l.cache[dir] = rg
	}
	l.tried = append(l.tried, rg.tried)
	switch {
	case rg.hit == nil:
	case rg.stale:
		if l.stale == nil {
			l.stale = rg.hit
		}
	default:
		l.hit = rg.hit
	}
}

// read is one rung's filesystem read (probe's uncached half).
func (l *hostDispatchLadder) read(dir string) hostDispatchRung {
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hostDispatchRung{tried: dir}
		}
		return hostDispatchRung{tried: fmt.Sprintf("%s (skipped: %v)", dir, err)}
	}
	if problem := hostDispatchStateDirProblem(fi, l.r.hostDispatchOwnerFunc()); problem != "" {
		return hostDispatchRung{tried: fmt.Sprintf("%s (skipped: %s)", dir, hostDispatchSkipLabel(problem))}
	}
	st, err := ReadHostDispatcherStatus(dir)
	if errors.Is(err, os.ErrNotExist) {
		return hostDispatchRung{tried: dir}
	}
	p := &hostDispatchProbe{found: true, dir: dir, st: st}
	if err != nil {
		p.readErr = err.Error()
		return hostDispatchRung{tried: dir, hit: p}
	}
	return hostDispatchRung{tried: dir, hit: p, stale: st.State(l.now) == hostDispatchStateStale}
}

// result is the decisive hit, else the first stale hit, else not found.
func (l *hostDispatchLadder) result() hostDispatchProbe {
	var p hostDispatchProbe
	switch {
	case l.hit != nil:
		p = *l.hit
	case l.stale != nil:
		p = *l.stale
	}
	p.tried = append([]string(nil), l.tried...)
	return p
}

func hostDispatchSkipLabel(problem string) string {
	switch problem {
	case hdStateDirSymlink:
		return "symlink"
	case hdStateDirNotDir:
		return "not a directory"
	case hdStateDirNotOwned:
		return "not owned by this user"
	default:
		return problem
	}
}

// hostDispatchBoundedPaths caps paths_tried at hostDispatchPathsTriedMax
// entries (a final "(+N more)") and bounds each entry.
func hostDispatchBoundedPaths(tried []string) []string {
	if len(tried) == 0 {
		return nil
	}
	keep := tried
	more := 0
	if len(tried) > hostDispatchPathsTriedMax {
		keep = tried[:hostDispatchPathsTriedMax-1]
		more = len(tried) - len(keep)
	}
	out := make([]string, 0, len(keep)+1)
	for _, p := range keep {
		out = append(out, boundHostDispatchText(p))
	}
	if more > 0 {
		out = append(out, fmt.Sprintf("(+%d more)", more))
	}
	return out
}

func hostDispatchRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// hostDispatchDailyCapped reports whether the global daily cap is reached
// TODAY (UTC): a SpawnsDay from an earlier day has already rolled over.
func hostDispatchDailyCapped(st HostDispatcherStatus, now time.Time) bool {
	return st.DailyCap > 0 && st.SpawnsToday >= st.DailyCap && st.SpawnsDay == now.UTC().Format(time.DateOnly)
}

// hostDispatchCapResetsAt is the UTC midnight after SpawnsDay (else after now).
func hostDispatchCapResetsAt(st HostDispatcherStatus, now time.Time) string {
	day, err := time.Parse(time.DateOnly, st.SpawnsDay)
	if err != nil {
		day = now.UTC().Truncate(24 * time.Hour)
	}
	return day.Add(24 * time.Hour).UTC().Format(time.RFC3339)
}

// observeHostDispatch turns one probe into the observation and verdict for
// run runID awaiting stage stageID. It is the single read-side choke point:
// every string copied from the file into the observation or a next_step is
// bounded here. Precedence: absent, unverified (unreadable file), then the
// dispatcher's health (stale, paused, failing), then the run's own entry
// (a stage mismatch is provisionally unverified; paused, capped, held and
// refusing decide; a waiting or review-round code is provisional), then the
// global daily cap, then the provisional states, else live.
func observeHostDispatch(p hostDispatchProbe, runID, stageID string, now time.Time) (hostDispatchObservation, hostDispatchVerdict) {
	obs := hostDispatchObservation{PathsTried: hostDispatchBoundedPaths(p.tried)}
	if !p.found {
		obs.State = hostDispatchStateAbsent
		return obs, hostDispatchVerdict{
			Kind:   hostDispatchAwaitKindHuman,
			Reason: hostDispatchReasonAbsent,
			NextStep: fmt.Sprintf("No host dispatcher status file found at %s. If a dispatcher runs with another --state-dir, set %s in this MCP server's environment; if none runs, start one (scripts/dev up) or dispatch the stage with fishhawk_dispatch_stage.",
				strings.Join(obs.PathsTried, ", "), hostDispatchStateDirEnv),
		}
	}
	dir := boundHostDispatchText(p.dir)
	obs.StatusPath = boundHostDispatchText(filepath.Join(p.dir, hostDispatchStatusFile))
	if p.readErr != "" {
		return hostDispatchUnverified(obs, "its status file at "+obs.StatusPath+" could not be read: "+boundHostDispatchText(p.readErr), dir)
	}
	st := p.st
	obs.LastTickAt = hostDispatchRFC3339(st.LastTickAt)
	obs.LastCompleteTickAt = hostDispatchRFC3339(st.LastCompleteTickAt)

	health := st.State(now)
	switch health {
	case "stale":
		obs.State = hostDispatchStateStale
		what := fmt.Sprintf("was last written at %s (more than %d ticks ago)", obs.LastTickAt, hostDispatchStatusStaleTicks)
		obs.Detail = "last tick at " + obs.LastTickAt
		switch {
		case st.FutureDated(now):
			// A future timestamp never ages out; no live writer stamps one.
			stamps := fmt.Sprintf("last_tick_at %s, last_complete_tick_at %s", obs.LastTickAt, obs.LastCompleteTickAt)
			what = fmt.Sprintf("is future-dated (%s; more than %s ahead of this host's clock at %s)", stamps, hostDispatchStatusFutureSkew, hostDispatchRFC3339(now))
			obs.Detail = "future-dated: " + stamps
		case !st.Stale(now):
			what = "shows no completed candidate pass since " + obs.LastCompleteTickAt
			obs.Detail = "no completed candidate pass since " + obs.LastCompleteTickAt
		}
		return obs, hostDispatchVerdict{
			Kind:   hostDispatchAwaitKindHuman,
			Reason: hostDispatchReasonAbsent,
			NextStep: fmt.Sprintf("The host dispatcher status file at %s %s, so the dispatcher that owns it is not dispatching. Check it with `%s --state-dir %s`, restart it (scripts/dev up), or dispatch the stage with fishhawk_dispatch_stage. If a dispatcher runs with another --state-dir, set %s in this MCP server's environment.",
				obs.StatusPath, what, hostDispatchStatusCmd, dir, hostDispatchStateDirEnv),
		}
	case "paused":
		pauseCheck := st.LastRefusalCode == hdRefusePauseCheck && !st.LastRefusalAt.Before(st.LastTickAt)
		return hostDispatchPausedVerdict(obs, dir, pauseCheck)
	case "failing":
		obs.State = hostDispatchStateRefusing
		obs.TickFailureCode = boundHostDispatchText(st.LastTickFailure.Code)
		obs.Detail = "the dispatcher's latest tick failed"
		return obs, hostDispatchVerdict{
			Kind:   hostDispatchAwaitKindHuman,
			Reason: hostDispatchReasonRefused,
			NextStep: fmt.Sprintf("The host dispatcher's last tick failed with %s at %s; it spawns nothing until a tick completes. Read logs/host-dispatch.log and fix the cause, or dispatch the stage with fishhawk_dispatch_stage.",
				obs.TickFailureCode, hostDispatchRFC3339(st.LastTickFailure.At)),
		}
	}

	provisional := ""
	var provisionalVerdict hostDispatchVerdict
	var provisionalObs hostDispatchObservation
	if ref, ok := st.RunRefusals[runID]; ok {
		obs.RefusalCode = boundHostDispatchText(ref.Code)
		obs.RefusalDetail = boundHostDispatchText(ref.Detail)
		obs.RefusalStageID = boundHostDispatchText(ref.StageID)
		obs.RefusalFirstAt = hostDispatchRFC3339(ref.FirstAt)
		obs.RefusalLastAt = hostDispatchRFC3339(ref.LastAt)
		detail := ""
		if obs.RefusalDetail != "" {
			detail = " (" + obs.RefusalDetail + ")"
		}
		switch {
		case ref.StageID != "" && stageID != "" && ref.StageID != stageID:
			// S5b: the entry may predate the stage's advance; it is neither
			// proof of a refusal of THIS stage nor proof that it is live.
			provisional = hostDispatchStateUnverified
			provisionalObs, provisionalVerdict = hostDispatchUnverified(obs,
				fmt.Sprintf("its last refusal for this run names stage %s, not this item's stage %s", obs.RefusalStageID, boundHostDispatchText(stageID)), dir)
		case hostDispatchPausedRefusals[ref.Code]:
			pauseCheck := ref.Code == hdRefusePauseCheck && !ref.LastAt.Before(st.LastTickAt)
			return hostDispatchPausedVerdict(obs, dir, pauseCheck)
		case ref.Code == hdRefuseCapDaily:
			return hostDispatchCappedVerdict(obs, st, now)
		case hostDispatchHoldRefusals[ref.Code]:
			obs.State = hostDispatchStateHeld
			next := fmt.Sprintf("The linked campaign is not running%s; the dispatcher holds this stage until it is resumed (fishhawk_resume_campaign). Do not dispatch past the hold.", detail)
			if ref.Code == hostDispatchCampaignHeldCode {
				next = fmt.Sprintf("A campaign hotfix hold is active%s; the stage dispatches after the hotfix lands. Do not dispatch past the hold.", detail)
			}
			if ref.Code == hdRefuseRunDispatchHeld {
				// The detail names the held run and the remedy; the hold
				// reason never reaches it (run.dispatch_hold.reason is data).
				next = fmt.Sprintf("An operator dispatch hold parks this run%s. Clear it with fishhawk_hold_dispatch action clear on the held run (held_run_id) when its dispatches may resume; the dispatcher spawns the stage on the next tick. Do not dispatch past the hold.", detail)
			}
			return obs, hostDispatchVerdict{Kind: hostDispatchAwaitKindHuman, Reason: hostDispatchReasonRefused, NextStep: next}
		case hostDispatchReviewRoundRefusals[ref.Code]:
			// #5298: the dispatcher holds acceptance for the implement review
			// round. It retries on its own, so the item waits on system work;
			// past hostDispatchReviewRoundOrphanAfter the round may be orphaned.
			provisional = hostDispatchStateWaiting
			next := hostDispatchReviewRoundNextStep(ref.Code)
			if now.Sub(ref.FirstAt) > hostDispatchReviewRoundOrphanAfter {
				next = fmt.Sprintf("The implement review round has held this acceptance stage (%s) since %s and may be orphaned: a failed reviewer retry or a server restart can leave a round pending, and a decomposed parent's consolidated round may never have started. Read the implement review_status and next_actions with fishhawk_get_run_status; fishhawk_rerun_review re-runs a zero-verdict advisory round or starts a decomposed parent's first consolidated round.",
					obs.RefusalCode, obs.RefusalFirstAt)
			}
			provisionalVerdict = hostDispatchVerdict{Kind: hostDispatchAwaitKindSystem, Reason: hostDispatchReasonInFlight, NextStep: next}
		case ref.Code == hdRefuseLineageLockHeld:
			// #5485: a live runner of this lineage still holds the lineage
			// lock. The dispatcher re-evaluates on its own, so the read waits;
			// past hostDispatchLineageLockStuckAfter the holder may be wedged.
			// lineage_lock_probe_failed stays on the default refusing arm: an
			// operator must fix the runner binary.
			if now.Sub(ref.FirstAt) <= hostDispatchLineageLockStuckAfter {
				provisional = hostDispatchStateWaiting
				provisionalVerdict = hostDispatchVerdict{
					Kind:     hostDispatchAwaitKind,
					Reason:   hostDispatchReasonDispatch,
					NextStep: "A runner for this run's lineage (typically the previous stage tearing down its preview) still holds the lineage lock (lineage_lock_held). The host dispatcher spawns the stage on the first tick that evaluates the run after the lock frees; no operator action is needed.",
				}
				break
			}
			obs.State = hostDispatchStateRefusing
			return obs, hostDispatchVerdict{
				Kind:   hostDispatchAwaitKindHuman,
				Reason: hostDispatchReasonRefused,
				NextStep: fmt.Sprintf("The run's lineage lock has held this stage (%s) since %s, longer than %s, so its holder may be wedged%s. Once the holder exits, the dispatcher spawns the stage on the first tick that evaluates the run after the lock frees; if it is an orphaned runner, cancel its run or kill the pid by hand.",
					obs.RefusalCode, obs.RefusalFirstAt, hostDispatchLineageLockStuckAfter, detail),
			}
		case hostDispatchWaitingRefusals[ref.Code]:
			provisional = hostDispatchStateWaiting
		default:
			obs.State = hostDispatchStateRefusing
			return obs, hostDispatchVerdict{
				Kind:   hostDispatchAwaitKindHuman,
				Reason: hostDispatchReasonRefused,
				NextStep: fmt.Sprintf("The host dispatcher refuses this stage: %s%s, since %s. Fix the cause it names; when the refusal is not a deliberate hold, fishhawk_dispatch_stage dispatches the stage by hand.",
					obs.RefusalCode, detail, obs.RefusalFirstAt),
			}
		}
	}
	if hostDispatchDailyCapped(st, now) {
		return hostDispatchCappedVerdict(obs, st, now)
	}
	switch provisional {
	case hostDispatchStateWaiting:
		obs.State = hostDispatchStateWaiting
		if provisionalVerdict.Kind != "" {
			return obs, provisionalVerdict
		}
		return obs, hostDispatchVerdict{
			Kind:     hostDispatchAwaitKind,
			Reason:   hostDispatchReasonDispatch,
			NextStep: fmt.Sprintf("The host dispatcher holds this stage (%s) and retries it on a later tick; no operator action is needed.", obs.RefusalCode),
		}
	case hostDispatchStateUnverified:
		return provisionalObs, provisionalVerdict
	}
	obs.State = hostDispatchStateLive
	return obs, hostDispatchVerdict{}
}

// hostDispatchReviewRoundNextStep is the routine next_step for an implement
// review round hold on an acceptance stage (#5298), read from the dispatcher's
// refusal or, before its first tick, from the round itself.
func hostDispatchReviewRoundNextStep(code string) string {
	if code == hdRefuseImplementReviewNotStarted {
		return "Acceptance waits for the implement review round (implement_review_not_started): the workflow declares implement agent reviewers but no round is recorded for the implement stage's current head yet. The host dispatcher holds the stage and dispatches it on the first tick after the round starts and settles; no operator action is needed."
	}
	return "Acceptance waits for the implement review round, which is in flight (implement_review_in_flight). The host dispatcher holds the stage and dispatches it on the first tick after the round settles; no operator action is needed."
}

// hostDispatchUnverified is the could-not-check verdict; dir, when known,
// pre-fills --state-dir.
func hostDispatchUnverified(obs hostDispatchObservation, why, dir string) (hostDispatchObservation, hostDispatchVerdict) {
	obs.State = hostDispatchStateUnverified
	obs.Detail = why
	check := fmt.Sprintf("`%s` (add --state-dir <dir> for a non-default dir)", hostDispatchStatusCmd)
	if dir != "" {
		check = fmt.Sprintf("`%s --state-dir %s`", hostDispatchStatusCmd, dir)
	}
	return obs, hostDispatchVerdict{
		Kind:     hostDispatchAwaitKind,
		Reason:   hostDispatchReasonUnverified,
		NextStep: fmt.Sprintf("The host dispatcher could not be checked for this stage (%s). Check it with %s.", why, check),
	}
}

func hostDispatchPausedVerdict(obs hostDispatchObservation, dir string, pauseCheckFailed bool) (hostDispatchObservation, hostDispatchVerdict) {
	obs.State = hostDispatchStatePaused
	if pauseCheckFailed {
		obs.Detail = "the pause check failed"
		return obs, hostDispatchVerdict{
			Kind:   hostDispatchAwaitKindHuman,
			Reason: hostDispatchReasonPaused,
			NextStep: fmt.Sprintf("The host dispatcher's pause check failed (treated as paused), so it spawns nothing. Fix %s so the dispatcher can check it (absent, or a pause file it can stat), or dispatch the stage with fishhawk_dispatch_stage.",
				filepath.Join(dir, hostDispatchPauseFile)),
		}
	}
	obs.Detail = "paused"
	return obs, hostDispatchVerdict{
		Kind:   hostDispatchAwaitKindHuman,
		Reason: hostDispatchReasonPaused,
		NextStep: fmt.Sprintf("The host dispatcher is paused, so it spawns nothing. Resume it with `fishhawk-mcp host-dispatch --resume --state-dir %s`, or dispatch the stage with fishhawk_dispatch_stage.",
			dir),
	}
}

func hostDispatchCappedVerdict(obs hostDispatchObservation, st HostDispatcherStatus, now time.Time) (hostDispatchObservation, hostDispatchVerdict) {
	obs.State = hostDispatchStateCapped
	obs.DailyCap = st.DailyCap
	obs.SpawnsToday = st.SpawnsToday
	obs.CapResetsAt = hostDispatchCapResetsAt(st, now)
	return obs, hostDispatchVerdict{
		Kind:   hostDispatchAwaitKindHuman,
		Reason: hostDispatchReasonCapped,
		NextStep: fmt.Sprintf("The host dispatcher reached its daily spawn cap (--daily-cap %d; %d spawned today) and spawns nothing more until %s. Dispatch the stage now with fishhawk_dispatch_stage, or restart the dispatcher with a higher --daily-cap (0 is no cap).",
			st.DailyCap, st.SpawnsToday, obs.CapResetsAt),
	}
}

// applyHostDispatchObservation writes one observation into an awaiting block.
// live leaves kind, reason and next_step as the backend wrote them (now
// positively verified).
func applyHostDispatchObservation(blk *campaignItemAwaiting, obs hostDispatchObservation, v hostDispatchVerdict) {
	if v.Kind != "" {
		blk.Kind, blk.Reason, blk.NextStep = v.Kind, v.Reason, v.NextStep
	}
	o := obs
	blk.HostDispatch = &o
}

// overlayCampaignHostDispatch rewrites every host_dispatcher awaiting block in
// items. The call rungs (env, else cwd) are probed once with no run read and,
// with TMPDIR, speak for every item. When they find nothing decisive and the
// env var is unset, each item's ladder continues with its OWN run's
// working_dir rung and then TMPDIR: a working_dir rung speaks only for items
// whose run has that working_dir, so a status planted in one run's checkout
// never decides another run's item. At most hostDispatchOverlayReadCap runs
// are read; an item whose own rung could not be probed reads unverified when
// its ladder found no status.
//
// In both branches an acceptance item the dispatcher has not refused for the
// implement review round gets the pre-tick round check
// (applyAcceptanceRoundHold, #5298). Its reads share the one read budget with
// the working_dir GetRun reads, so the cap bounds both.
func (r *runResolver) overlayCampaignHostDispatch(ctx context.Context, items []CampaignItem) {
	if !r.hostDispatchOverlayEnabled() {
		return
	}
	var idx []int
	for i := range items {
		if a := items[i].Awaiting; a != nil && a.Kind == hostDispatchAwaitKind {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return
	}
	now := r.hostDispatchClock()
	call := r.newHostDispatchLadder(now)
	for _, dir := range r.hostDispatchCallRungs() {
		call.probe(dir)
	}
	rounds := map[string]hostDispatchRoundRead{}
	reads := 0
	if call.done() || r.hostDispatchEnv(hostDispatchStateDirEnv) != "" {
		found := call.result()
		for _, i := range idx {
			blk := items[i].Awaiting
			obs, v := observeHostDispatch(found, blk.RunID, blk.StageID, now)
			r.applyAcceptanceRoundHold(ctx, blk, rounds, &obs, &v, &reads)
			applyHostDispatchObservation(blk, obs, v)
		}
		return
	}
	rungOf := map[string]string{}
	why := map[string]string{}
	for _, i := range idx {
		id := items[i].Awaiting.RunID
		if _, ok := rungOf[id]; ok {
			continue
		}
		if _, ok := why[id]; ok {
			continue
		}
		if reads >= hostDispatchOverlayReadCap {
			why[id] = fmt.Sprintf("its run was not read: the overlay's read cap of %d reached", hostDispatchOverlayReadCap)
			continue
		}
		runUUID, perr := uuid.Parse(id)
		if perr != nil {
			why[id] = "its run id is not a UUID"
			continue
		}
		reads++
		run, err := r.api.GetRun(ctx, runUUID)
		if err != nil {
			why[id] = "its run could not be read: " + boundHostDispatchText(err.Error())
			continue
		}
		rungOf[id] = hostDispatchWorkingDirRung(run.WorkingDir)
	}
	tmp := hostDispatchDefaultRung()
	byRung := map[string]hostDispatchProbe{}
	for _, i := range idx {
		blk := items[i].Awaiting
		rung := rungOf[blk.RunID] // "" (no own rung): the call rungs and TMPDIR only
		found, ok := byRung[rung]
		if !ok {
			lad := call.fork()
			lad.probe(rung)
			lad.probe(tmp)
			found = lad.result()
			byRung[rung] = found
		}
		obs, v := observeHostDispatch(found, blk.RunID, blk.StageID, now)
		if reason, unprobed := why[blk.RunID]; unprobed && obs.State == hostDispatchStateAbsent {
			obs, v = hostDispatchUnverified(obs, reason, "")
		}
		r.applyAcceptanceRoundHold(ctx, blk, rounds, &obs, &v, &reads)
		applyHostDispatchObservation(blk, obs, v)
	}
}

// hostDispatchRoundRead is one run's pre-tick implement review round check:
// the hold code ("" when settled), or why the round could not be read (the
// item then reads unverified).
type hostDispatchRoundRead struct {
	hold string
	why  string
}

// applyAcceptanceRoundHold is the overlay's pre-tick round check (#5298 V2):
// it closes the window before the dispatcher's first tick, when no
// run_refusals entry exists yet and the block would read live. For an
// acceptance item whose observation is live, or waiting on a code that is not
// a round hold, it reads the run's stages and the shared round predicate
// (acceptanceRoundHold) and, on a hold, rewrites the verdict to
// system/in_flight. Human verdicts (held, paused, capped, refusing, absent,
// stale, unverified) stand, and a round-code entry is not re-checked.
//
// Each run is checked once per call (rounds memoises it). A check is ONE unit
// against the read budget shared with the working_dir GetRun reads and capped
// by hostDispatchOverlayReadCap; past the cap, or on a read error, the item
// reads unverified, never live.
func (r *runResolver) applyAcceptanceRoundHold(ctx context.Context, blk *campaignItemAwaiting, rounds map[string]hostDispatchRoundRead, obs *hostDispatchObservation, v *hostDispatchVerdict, budget *int) {
	if blk.StageType == "acceptance" && (obs.State == hostDispatchStateLive || (obs.State == hostDispatchStateWaiting && !hostDispatchReviewRoundRefusals[obs.RefusalCode])) {
		rd, ok := rounds[blk.RunID]
		if !ok {
			rd = r.readAcceptanceRound(ctx, blk.RunID, budget)
			rounds[blk.RunID] = rd
		}
		switch {
		case rd.why != "":
			dir := ""
			if obs.StatusPath != "" {
				dir = filepath.Dir(obs.StatusPath)
			}
			*obs, *v = hostDispatchUnverified(*obs, rd.why, dir)
		case rd.hold != "":
			obs.Detail = "the implement review round holds acceptance: " + rd.hold
			*v = hostDispatchVerdict{Kind: hostDispatchAwaitKindSystem, Reason: hostDispatchReasonInFlight, NextStep: hostDispatchReviewRoundNextStep(rd.hold)}
		}
	}
}

// readAcceptanceRound performs one run's round check against the shared read
// budget: the stage read (for the implement stage's review_agents_configured)
// and the implement review status, through the same predicate the dispatcher
// applies.
func (r *runResolver) readAcceptanceRound(ctx context.Context, runID string, budget *int) hostDispatchRoundRead {
	if *budget >= hostDispatchOverlayReadCap {
		return hostDispatchRoundRead{why: fmt.Sprintf("its implement review round was not read: the overlay's read cap of %d reached", hostDispatchOverlayReadCap)}
	}
	runUUID, err := uuid.Parse(runID)
	if err != nil {
		return hostDispatchRoundRead{why: "its implement review round was not read: its run id is not a UUID"}
	}
	*budget++
	stages, err := r.api.ListRunStages(ctx, runUUID)
	if err != nil {
		return hostDispatchRoundRead{why: "its implement review round could not be read: " + boundHostDispatchText(err.Error())}
	}
	hold, err := r.acceptanceRoundHold(ctx, runUUID, stages)
	if err != nil {
		return hostDispatchRoundRead{why: "its implement review round could not be read: " + boundHostDispatchText(err.Error())}
	}
	return hostDispatchRoundRead{hold: hold}
}

// runHostDispatchFor is fishhawk_get_run_status's host_dispatch block. It is
// nil unless the overlay is enabled and either the dispatcher holds a
// run_refusals entry for the run, or the run is one the dispatcher would
// consider: local, non-terminal, with a spawnable dispatchable stage (pending
// or awaiting_host_dispatch), and eligible by the dispatcher's own predicate
// (its auto_dispatch mark, or a decomposition child of a marked parent). It
// probes env, else cwd, the run's own working_dir rung (no extra read) and
// TMPDIR.
//
// roundHold is implementReviewRoundHold over the status call's own implement
// review status and stages (#5298): an acceptance stage it holds is not one
// the dispatcher would spawn, so with no refusal entry the block is omitted
// and next_actions names the hold.
func (r *runResolver) runHostDispatchFor(ctx context.Context, view *runDriveView, stages []Stage, roundHold string) *hostDispatchObservation {
	if !r.hostDispatchOverlayEnabled() || view == nil {
		return nil
	}
	run := &view.Run
	stageID := ""
	dispatchable := false
	if run.RunnerKind == driveRunnerKindLocal && !runStateIsTerminal(run.State) {
		if disp := driveDispatchableStage(stages, nil, view.DerivedStatus == derivedAcceptancePending); disp != nil && hostDispatchSpawnable(*disp) && (disp.Type != "acceptance" || roundHold == "") {
			stageID, dispatchable = disp.ID, true
		}
	}
	now := r.hostDispatchClock()
	lad := r.newHostDispatchLadder(now)
	for _, dir := range r.hostDispatchCallRungs() {
		lad.probe(dir)
	}
	if r.hostDispatchEnv(hostDispatchStateDirEnv) == "" {
		lad.probe(hostDispatchWorkingDirRung(run.WorkingDir))
		lad.probe(hostDispatchDefaultRung())
	}
	found := lad.result()
	_, hasEntry := found.st.RunRefusals[run.ID]
	if !hasEntry {
		if !dispatchable {
			return nil
		}
		ok, _, err := (&hostDispatcher{r: r}).eligible(ctx, run)
		if err != nil || !ok {
			return nil
		}
	}
	obs, _ := observeHostDispatch(found, run.ID, stageID, now)
	return &obs
}
