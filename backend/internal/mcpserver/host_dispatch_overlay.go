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
)

// hostDispatchOverlayReadCap bounds the GetRun reads one campaign status call
// spends discovering a state dir. Each read is a backend round trip inside a
// status call the operator polls on every drive tick. A status found on any
// rung already speaks for EVERY item (one dispatcher per stack), so the cap
// only bounds the worst case where no dispatcher is found at all; the items it
// skips then read unverified, never absent.
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
// arm only by joining a set here (an unknown non-benign code reads refusing).
var (
	// hostDispatchPausedRefusals are the mid-tick Admit pause refusals (a pause
	// that landed after the tick's own pause check).
	hostDispatchPausedRefusals = map[string]bool{hdRefusePaused: true, hdRefusePauseCheck: true}
	// hostDispatchHoldRefusals are deliberate holds: the stage waits for the
	// campaign, and a hand dispatch would bypass the hold.
	hostDispatchHoldRefusals = map[string]bool{hdRefuseCampaignNotRunning: true, hostDispatchCampaignHeldCode: true}
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
// stale hit is reported.
type hostDispatchLadder struct {
	r     *runResolver
	now   time.Time
	seen  map[string]bool
	tried []string
	hit   *hostDispatchProbe
	stale *hostDispatchProbe
}

func (r *runResolver) newHostDispatchLadder(now time.Time) *hostDispatchLadder {
	return &hostDispatchLadder{r: r, now: now, seen: map[string]bool{}}
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
	fi, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			l.tried = append(l.tried, dir)
		} else {
			l.tried = append(l.tried, fmt.Sprintf("%s (skipped: %v)", dir, err))
		}
		return
	}
	if problem := hostDispatchStateDirProblem(fi, l.r.hostDispatchOwnerFunc()); problem != "" {
		l.tried = append(l.tried, fmt.Sprintf("%s (skipped: %s)", dir, hostDispatchSkipLabel(problem)))
		return
	}
	l.tried = append(l.tried, dir)
	st, err := ReadHostDispatcherStatus(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	p := &hostDispatchProbe{found: true, dir: dir, st: st}
	if err != nil {
		p.readErr = err.Error()
		l.hit = p
		return
	}
	if st.State(l.now) == hostDispatchStateStale {
		if l.stale == nil {
			l.stale = p
		}
		return
	}
	l.hit = p
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
// refusing decide; a waiting code is provisional), then the global daily cap,
// then the provisional states, else live.
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
		if !st.Stale(now) {
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
			return obs, hostDispatchVerdict{Kind: hostDispatchAwaitKindHuman, Reason: hostDispatchReasonRefused, NextStep: next}
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
// items from ONE status resolved per call. The call rungs (env, else cwd) are
// probed with no run read; only when they find nothing decisive and the env
// var is unset are the items' runs read for their working_dir rungs (at most
// hostDispatchOverlayReadCap reads, stopping at the first decisive hit), then
// the TMPDIR default. Global states therefore reach every item, including the
// ones the cap skipped; an item whose own rung could not be probed reads
// unverified only when no status was found anywhere.
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
	lad := r.newHostDispatchLadder(now)
	for _, dir := range r.hostDispatchCallRungs() {
		lad.probe(dir)
	}
	envSet := r.hostDispatchEnv(hostDispatchStateDirEnv) != ""
	unprobed := map[string]bool{}
	why := map[string]string{}
	if !lad.done() && !envSet {
		reads := 0
		seen := map[string]bool{}
		for _, i := range idx {
			if lad.done() {
				break
			}
			id := items[i].Awaiting.RunID
			if seen[id] {
				continue
			}
			seen[id] = true
			if reads >= hostDispatchOverlayReadCap {
				unprobed[id] = true
				why[id] = fmt.Sprintf("its run was not read: the overlay's read cap of %d reached", hostDispatchOverlayReadCap)
				continue
			}
			runUUID, perr := uuid.Parse(id)
			if perr != nil {
				unprobed[id] = true
				why[id] = "its run id is not a UUID"
				continue
			}
			reads++
			run, err := r.api.GetRun(ctx, runUUID)
			if err != nil {
				unprobed[id] = true
				why[id] = "its run could not be read: " + boundHostDispatchText(err.Error())
				continue
			}
			lad.probe(hostDispatchWorkingDirRung(run.WorkingDir))
		}
	}
	if !envSet {
		lad.probe(hostDispatchDefaultRung())
	}
	found := lad.result()
	for _, i := range idx {
		blk := items[i].Awaiting
		obs, v := observeHostDispatch(found, blk.RunID, blk.StageID, now)
		if obs.State == hostDispatchStateAbsent && unprobed[blk.RunID] {
			obs, v = hostDispatchUnverified(obs, why[blk.RunID], "")
		}
		applyHostDispatchObservation(blk, obs, v)
	}
}

// runHostDispatchFor is fishhawk_get_run_status's host_dispatch block. It is
// nil unless the overlay is enabled and either the dispatcher holds a
// run_refusals entry for the run, or the run is one the dispatcher would
// consider: local, non-terminal, with a spawnable dispatchable stage (pending
// or awaiting_host_dispatch), and eligible by the dispatcher's own predicate
// (its auto_dispatch mark, or a decomposition child of a marked parent). It
// probes env, else cwd, the run's own working_dir rung (no extra read) and
// TMPDIR.
func (r *runResolver) runHostDispatchFor(ctx context.Context, view *runDriveView, stages []Stage) *hostDispatchObservation {
	if !r.hostDispatchOverlayEnabled() || view == nil {
		return nil
	}
	run := &view.Run
	stageID := ""
	dispatchable := false
	if run.RunnerKind == driveRunnerKindLocal && !runStateIsTerminal(run.State) {
		if disp := driveDispatchableStage(stages, nil, view.DerivedStatus == derivedAcceptancePending); disp != nil && hostDispatchSpawnable(*disp) {
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
