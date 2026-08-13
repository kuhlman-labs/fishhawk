package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

/*
 * Campaign acceptance-slot visibility (E48.71 / #2503).
 *
 * Acceptance validates against ONE fixed, shared preview slot (the
 * FISHHAWK_PREVIEW_ADDR host, default localhost:8090). A campaign driving N
 * items therefore SERIALIZES on that single slot, and today the only signal of
 * the contention is the per-dispatch needs_target refusal — which says nothing
 * about the sibling item currently holding the slot. This block makes the
 * contention VISIBLE on fishhawk_get_campaign_status: it names the resolved
 * target host, the slot's live state from a single short /healthz probe, the
 * item whose acceptance stage is live (and therefore occupying the slot), and
 * the items whose acceptance stage is parked (and therefore serialized behind
 * it).
 *
 * It is deliberately scoped to OBSERVABILITY: no host mutation (ADR-038 keeps
 * that off the MCP surface) and no per-run preview allocation. Three controls
 * bound its cost and blast radius: it issues NO probe when no item is at or
 * near the acceptance gate, it caps the per-item stage reads (reporting the
 * truncation rather than capping silently), and every per-item read failure
 * degrades that item rather than failing the campaign-status snapshot (the
 * children_status.go best-effort contract).
 *
 * Trust posture (operator conditions on #2503): a probe that cannot POSITIVELY
 * decide the slot is free reports `unverifiable`, never `free`. A `free` verdict
 * is emitted ONLY on a connection REFUSED from a reachable host (a positive
 * "nothing is listening" signal); any other dial failure — timeout, black-hole,
 * no route, or probing from the wrong host — is `unverifiable` with the reason
 * surfaced. And a probe that says free while stage state names a live holder is
 * a self-contradiction resolved to `unverifiable`, never trusting either half.
 */

// acceptancePreviewAddrEnv is the operator override for the acceptance preview
// slot host. Its default byte-matches scripts/dev's `_preview_addr`
// (localhost:8090) so the probe and the dev tooling agree on the address.
const acceptancePreviewAddrEnv = "FISHHAWK_PREVIEW_ADDR"

// acceptanceSlotDefaultHost is the default preview slot host — byte-matching
// scripts/dev's `_preview_addr` fallback.
const acceptanceSlotDefaultHost = "localhost:8090"

// acceptanceSlotMaxItemReads caps the per-item ListRunStages fan-out so a hot
// drive-loop poll cannot issue an unbounded number of stage reads. When more
// candidate items than this exist, the block reports truncated=true rather than
// capping silently.
const acceptanceSlotMaxItemReads = 12

// acceptanceSlotProbeTimeout bounds the single /healthz probe. A status poll
// must not inherit the dispatch gate's multi-attempt boot budget, so a
// black-holed (packet-dropping) target adds at most this to the read. Package
// var so tests shrink it to run without wall-clock waits.
var acceptanceSlotProbeTimeout = 2 * time.Second

// AcceptanceSlotClaim names one campaign item's relationship to the shared
// acceptance preview slot: the item whose live acceptance stage holds it, or a
// parked item serialized behind it.
type AcceptanceSlotClaim struct {
	IssueRef   string `json:"issue_ref" jsonschema:"the campaign item's issue ref"`
	RunID      string `json:"run_id" jsonschema:"the item's run UUID"`
	StageID    string `json:"stage_id" jsonschema:"the run's acceptance stage UUID"`
	StageState string `json:"stage_state" jsonschema:"the acceptance stage's lifecycle state (dispatched/running for a holder, pending/awaiting_host_dispatch for a waiter)"`
}

// AcceptanceSlot is the additive, best-effort acceptance-slot visibility block
// surfaced on fishhawk_get_campaign_status (E48.71 / #2503). It is present only
// when at least one campaign item is at or near the acceptance gate.
type AcceptanceSlot struct {
	Shared           bool                  `json:"shared" jsonschema:"always true — the structural fact that acceptance validates against ONE fixed shared preview slot, so campaign items serialize on it"`
	TargetHost       string                `json:"target_host" jsonschema:"the resolved preview slot host the probe targeted (FISHHAWK_PREVIEW_ADDR or the localhost:8090 default)"`
	TargetHostSource string                `json:"target_host_source" jsonschema:"how target_host was resolved: 'env' (FISHHAWK_PREVIEW_ADDR set) or 'default' (localhost:8090)"`
	State            string                `json:"state" jsonschema:"the slot's live state from a single /healthz probe: 'free' (a connection refused from a reachable host — nothing is bound), 'held' (a build is serving the slot), or 'unverifiable' (the probe could not positively decide, or it disagreed with stage state)"`
	ServingGitSHA    string                `json:"serving_git_sha,omitempty" jsonschema:"the git_sha the slot is currently serving; present only when state is held"`
	Detail           string                `json:"detail" jsonschema:"the probe's URL-bearing explanation of how state was determined"`
	HeldBy           *AcceptanceSlotClaim  `json:"held_by,omitempty" jsonschema:"the campaign item whose acceptance stage is live (dispatched/running) and therefore occupying the slot; nil when no live acceptance stage was found (a settled or never-dispatched sibling may still bind the slot)"`
	Waiting          []AcceptanceSlotClaim `json:"waiting,omitempty" jsonschema:"the campaign items whose acceptance stage is parked (pending/awaiting_host_dispatch) and therefore serialized behind the holder"`
	ReadFailures     []string              `json:"read_failures,omitempty" jsonschema:"issue refs of candidate items whose stage read failed, so the block is partial — those items could not be classified (best-effort degrade). May appear alongside a classified participant, OR in an incomplete-inspection block (state unverifiable, held_by nil, waiting empty) when every candidate read failed and none could be classified"`
	ItemsInspected   int                   `json:"items_inspected" jsonschema:"how many candidate item runs the block inspected (bounded by inspection_limit)"`
	InspectionLimit  int                   `json:"inspection_limit" jsonschema:"the per-item stage-read cap (acceptanceSlotMaxItemReads)"`
	Truncated        bool                  `json:"truncated" jsonschema:"true when more candidate items existed than inspection_limit, so the block reflects only the first inspection_limit of them"`
	Note             string                `json:"note" jsonschema:"a one-line operator explanation of the slot state and the remediation (scripts/dev preview <head>), noting the slot is shared"`
}

// resolveAcceptanceSlotHost resolves the preview slot host from the env seam:
// FISHHAWK_PREVIEW_ADDR when non-empty after TrimSpace (source "env"), else the
// literal localhost:8090 (source "default"), byte-matching scripts/dev's
// `_preview_addr` default so the two agree.
//
// Known limit (like acceptance_target.go's gate): the probe is issued FROM the
// process serving the tool — the operator host on the stdio transport, the
// fishhawkd host on the /mcp route (#2390). On the HTTP transport a default
// localhost:8090 probes fishhawkd's own loopback, not the operator's preview;
// the FISHHAWK_PREVIEW_ADDR override is the escape hatch, and the detail string
// always names the URL actually probed so a misdirected probe is
// self-diagnosing.
func resolveAcceptanceSlotHost(getenv func(string) string) (host, source string) {
	if getenv != nil {
		if v := strings.TrimSpace(getenv(acceptancePreviewAddrEnv)); v != "" {
			return v, "env"
		}
	}
	return acceptanceSlotDefaultHost, "default"
}

// slotProbeOutcome is the four-way classification of the /healthz probe,
// ordered so the most-informative scheme attempt wins across
// acceptanceProbeSchemeOrder. serving > reachableNoIdentity > refused >
// unreachable: a served build identity is strongest; a reachable-but-unidentified
// answer next; a REFUSED connection (a positive "nothing is bound" signal) beats
// an UNREACHABLE dial failure (which cannot decide anything).
type slotProbeOutcome int

const (
	// slotProbeUnreachable: the dial failed in a way that is NOT a connection
	// refused (timeout, black-hole, no route, DNS, wrong host). The slot's state
	// cannot be positively decided → unverifiable (never free).
	slotProbeUnreachable slotProbeOutcome = iota
	// slotProbeRefused: a connection refused from a reachable host — the host
	// answered a RST, so it is up and nothing is listening on the slot port. The
	// one positive "free" signal.
	slotProbeRefused
	// slotProbeReachableNoIdentity: the target answered but exposes no comparable
	// build identity (non-200, non-JSON, or a missing/"unknown" git_sha) →
	// unverifiable.
	slotProbeReachableNoIdentity
	// slotProbeServing: a 200 with a non-empty, non-"unknown" git_sha — a build is
	// serving the slot.
	slotProbeServing
)

// probeAcceptanceSlotHealth issues a SINGLE-shot, no-retry /healthz read against
// host, reusing acceptanceProbeSchemeOrder for the scheme order and the shared
// acceptanceProbeHTTPClient (direct-dial Proxy:nil, redirects refused) so the
// campaign read cannot be routed through an ambient proxy or chased off-target.
// It differs from probeAcceptanceTargetIdentity deliberately: there is no
// expected head SHA to compare against on a campaign read, so it REPORTS the
// served git_sha rather than classifying it, and it wraps ctx in a short
// context.WithTimeout with NO unreachable-retry loop.
func probeAcceptanceSlotHealth(ctx context.Context, host string) (outcome slotProbeOutcome, gitSHA, detail string) {
	pctx, cancel := context.WithTimeout(ctx, acceptanceSlotProbeTimeout)
	defer cancel()

	best := slotProbeUnreachable
	bestDetail := fmt.Sprintf("no scheme reached host %q", host)
	bestSHA := ""
	for _, scheme := range acceptanceProbeSchemeOrder(host) {
		url := scheme + "://" + host + "/healthz"
		o, sha, d := acceptanceSlotProbeOnce(pctx, url)
		if o == slotProbeServing {
			return o, sha, d
		}
		if o > best {
			best, bestSHA, bestDetail = o, sha, d
		}
	}
	return best, bestSHA, bestDetail
}

// acceptanceSlotProbeOnce performs a single-scheme /healthz read and classifies
// it. The dial-error branch is the load-bearing discrimination for condition 1:
// a connection REFUSED (host reachable, nothing listening) is the positive free
// signal (slotProbeRefused); ANY other dial failure cannot decide and is
// slotProbeUnreachable → unverifiable.
func acceptanceSlotProbeOnce(ctx context.Context, url string) (slotProbeOutcome, string, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return slotProbeUnreachable, "", fmt.Sprintf("build probe request for %s: %v", url, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := acceptanceProbeHTTPClient.Do(req)
	if err != nil {
		// A connection refused means the host is REACHABLE and answered a RST —
		// nothing is bound to the slot port. That is the only dial outcome that
		// positively distinguishes a free slot. Every other dial failure
		// (timeout, black-hole, no route, wrong host) is indistinguishable from a
		// bound-but-isolated slot, so it must report unverifiable, never free
		// (operator condition 1 on #2503).
		if errors.Is(err, syscall.ECONNREFUSED) {
			return slotProbeRefused, "", fmt.Sprintf(
				"probe %s: connection refused — host reachable, nothing listening on the slot", url)
		}
		return slotProbeUnreachable, "", fmt.Sprintf(
			"probe %s: %v (dial failed, but not a connection refused — cannot decide whether the slot is free)", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return slotProbeReachableNoIdentity, "", fmt.Sprintf(
			"probe %s: status %d, build identity not verifiable", url, resp.StatusCode)
	}
	var body struct {
		GitSHA string `json:"git_sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return slotProbeReachableNoIdentity, "", fmt.Sprintf(
			"probe %s: non-JSON healthz body (%v), build identity not verifiable", url, err)
	}
	got := strings.TrimSpace(body.GitSHA)
	if got == "" || got == "unknown" {
		return slotProbeReachableNoIdentity, got, fmt.Sprintf(
			"probe %s: healthz exposes no git_sha build identifier", url)
	}
	return slotProbeServing, got, fmt.Sprintf("probe %s: target serving git_sha %q", url, got)
}

// acceptanceSlotItemTerminal reports whether a campaign item's state is terminal
// (no live run to inspect), so pass 1 issues no stage read for it.
func acceptanceSlotItemTerminal(state string) bool {
	switch state {
	case "done", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// acceptanceSlotIncompleteReason names WHY an inspection was incomplete —
// truncation at the candidate cap, failed candidate stage reads, or both — so a
// caller can say that an unverifiable verdict is a CLASSIFICATION gap rather
// than a probe result. Returns "" when the inspection was complete.
//
// Shared by the two sites that must refuse to answer positively on an
// incomplete inspection: the no-participant block, and the refused-probe arm
// that would otherwise report `free` while an un-inspected item could hold the
// slot (#2503).
func acceptanceSlotIncompleteReason(truncated bool, readFailures []string) string {
	var reasons []string
	if truncated {
		reasons = append(reasons, fmt.Sprintf(
			"inspection was truncated at the %d-item cap", acceptanceSlotMaxItemReads))
	}
	if len(readFailures) > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"%d candidate item stage read(s) failed (%s)",
			len(readFailures), strings.Join(readFailures, ", ")))
	}
	return strings.Join(reasons, "; ")
}

// acceptanceSlotFor builds the acceptance-slot visibility block from the
// campaign's items (E48.71 / #2503), or returns nil when no item is at or near
// the acceptance gate (the block is then omitted from the response entirely).
//
// Pass 1 (no network): select candidate items — those carrying a non-empty,
// parseable run id whose item state is not terminal — capping at
// acceptanceSlotMaxItemReads and recording truncation. Pass 2: for each
// candidate read its stages and find the acceptance stage; a dispatched/running
// stage HOLDS the slot (first in item order wins), a pending/awaiting stage
// WAITS behind it; a terminal stage or no acceptance stage is not a participant.
// A per-item stage-read error degrades that item (best-effort). When no holder
// and no waiter emerge, return nil WITHOUT probing.
func (r *runResolver) acceptanceSlotFor(ctx context.Context, items []CampaignItem) *AcceptanceSlot {
	type candidate struct {
		item  CampaignItem
		runID uuid.UUID
	}
	candidates := make([]candidate, 0, len(items))
	truncated := false
	for _, it := range items {
		if acceptanceSlotItemTerminal(it.State) {
			continue
		}
		ref := strings.TrimSpace(it.RunID)
		if ref == "" {
			continue
		}
		parsed, perr := uuid.Parse(ref)
		if perr != nil {
			continue
		}
		if len(candidates) >= acceptanceSlotMaxItemReads {
			// A genuinely-qualifying candidate beyond the cap exists: report the
			// truncation rather than capping silently, and stop reading.
			truncated = true
			break
		}
		candidates = append(candidates, candidate{item: it, runID: parsed})
	}

	var heldBy *AcceptanceSlotClaim
	var waiting []AcceptanceSlotClaim
	var readFailures []string
	for _, c := range candidates {
		stages, lerr := r.api.ListRunStages(ctx, c.runID)
		if lerr != nil {
			// Best-effort: a per-item read failure degrades that item and never
			// fails the campaign-status snapshot (the children_status.go contract).
			// Record the ref so the block is honestly PARTIAL rather than silently
			// dropping the item — an operator reading a slot with read_failures
			// knows a candidate could not be classified.
			readFailures = append(readFailures, c.item.IssueRef)
			continue
		}
		var acc *Stage
		for i := range stages {
			if stages[i].Type == "acceptance" {
				acc = &stages[i]
				break
			}
		}
		if acc == nil {
			continue
		}
		claim := AcceptanceSlotClaim{
			IssueRef:   c.item.IssueRef,
			RunID:      c.runID.String(),
			StageID:    acc.ID,
			StageState: acc.State,
		}
		switch acc.State {
		case "dispatched", "running":
			if heldBy == nil {
				held := claim
				heldBy = &held
			}
		case "pending", "awaiting_host_dispatch":
			waiting = append(waiting, claim)
		default:
			// A terminal acceptance stage (or any other state) is not a live
			// participant — it neither holds nor waits on the slot.
		}
	}

	// Cost short-circuit: a campaign nowhere near acceptance issues ZERO network
	// calls and the block is omitted entirely — BUT only when the inspection was
	// COMPLETE. Two ways it can be incomplete while no participant emerged, each of
	// which may hide a candidate at the acceptance gate: (a) it was TRUNCATED at the
	// cap before any participant was found, so one may exist among the items beyond
	// the cap; (b) one or more candidate stage reads FAILED, so a sole candidate —
	// or all candidates — at the acceptance gate could have been the very reads that
	// failed. Omitting the block in either case would present an incomplete
	// inspection as "no acceptance participation" and drop both the acceptance_slot
	// AND its truncation / read_failures signal (#2503 high concern). Surface an
	// incomplete-inspection block instead: state unverifiable, no probe. No probe is
	// issued because without a known participant a refused probe could not
	// distinguish free from a holder we never classified — it could only mislead
	// (conditions 1 & 2 on #2503).
	if heldBy == nil && len(waiting) == 0 {
		if !truncated && len(readFailures) == 0 {
			return nil
		}
		host, source := resolveAcceptanceSlotHost(r.getenv)
		reason := acceptanceSlotIncompleteReason(truncated, readFailures)
		return &AcceptanceSlot{
			Shared:           true,
			TargetHost:       host,
			TargetHostSource: source,
			State:            "unverifiable",
			Detail: fmt.Sprintf(
				"%s, and no acceptance participant was found among the items that WERE classified; a candidate item at the acceptance gate may exist among the un-inspected items, so the slot state cannot be positively decided. No slot probe was issued.",
				reason),
			ReadFailures:    readFailures,
			ItemsInspected:  len(candidates),
			InspectionLimit: acceptanceSlotMaxItemReads,
			Truncated:       truncated,
			Note: fmt.Sprintf(
				"acceptance validates against ONE shared preview slot at %s; %s, and none of the classified items was at the acceptance gate, so slot contention cannot be ruled out — re-check after the affected items settle, or inspect the individual item runs.",
				host, reason),
		}
	}

	host, source := resolveAcceptanceSlotHost(r.getenv)
	outcome, gitSHA, detail := probeAcceptanceSlotHealth(ctx, host)

	slot := &AcceptanceSlot{
		Shared:           true,
		TargetHost:       host,
		TargetHostSource: source,
		Detail:           detail,
		HeldBy:           heldBy,
		Waiting:          waiting,
		ReadFailures:     readFailures,
		ItemsInspected:   len(candidates),
		InspectionLimit:  acceptanceSlotMaxItemReads,
		Truncated:        truncated,
	}

	// Map the probe outcome onto the slot state, cross-checking against the
	// stage-derived holder so a probe/stage disagreement never reports free
	// (operator conditions 1 & 2 on #2503).
	switch outcome {
	case slotProbeServing:
		slot.State = "held"
		slot.ServingGitSHA = gitSHA
		if heldBy != nil {
			slot.Note = fmt.Sprintf(
				"acceptance validates against ONE shared preview slot at %s, currently serving git_sha %s and held by %s; every waiting item is serialized behind it and must wait for the slot to be re-pointed (scripts/dev preview <head>).",
				host, gitSHA, heldBy.IssueRef)
		} else {
			slot.Note = fmt.Sprintf(
				"acceptance validates against ONE shared preview slot at %s, currently serving git_sha %s but with no live acceptance stage holding it (a settled or not-yet-dispatched sibling still binds it); re-point it with scripts/dev preview <head> before the next acceptance dispatch.",
				host, gitSHA)
		}
	case slotProbeRefused:
		if heldBy != nil {
			// Self-contradiction: the probe says the slot is unbound (connection
			// refused) while stage state names a live holder. Trust neither half —
			// resolve to unverifiable and say which signals disagreed (condition 2).
			slot.State = "unverifiable"
			slot.Note = fmt.Sprintf(
				"acceptance validates against ONE shared preview slot at %s; signals DISAGREE — the probe got connection refused (the slot looks unbound) while item %s's acceptance stage is %s (a live holder). Reporting unverifiable rather than free: verify the preview host and whether the probe reached the right target (the probe runs from the process serving the tool).",
				host, heldBy.IssueRef, heldBy.StageState)
		} else if truncated || len(readFailures) > 0 {
			// A positive `free` requires a COMPLETE inspection. The probe says the
			// slot is unbound and no CLASSIFIED item holds it — but an item we never
			// classified (truncated away at the cap, or whose stage read failed) may
			// hold a live acceptance stage. This is the same probe-versus-stage
			// disagreement the heldBy arm above resolves to unverifiable; it differs
			// only in that the potential holder is one we could not read rather than
			// one we did, which is a reason for LESS confidence, not more. Reporting
			// free here would be exactly the confidently-wrong answer this block
			// exists to prevent (#2503, conditions 1 & 2).
			//
			// Reachable only when a waiter was classified: with no holder AND no
			// waiter the incomplete-inspection guard above has already returned.
			slot.State = "unverifiable"
			slot.Note = fmt.Sprintf(
				"acceptance validates against ONE shared preview slot at %s; the probe got connection refused (the slot looks unbound) and no classified item holds it, but %s — so an un-inspected item could hold the slot. Reporting unverifiable rather than free: re-check after the affected items settle, or inspect the individual item runs.",
				host, acceptanceSlotIncompleteReason(truncated, readFailures))
		} else {
			slot.State = "free"
			slot.Note = fmt.Sprintf(
				"acceptance validates against ONE shared preview slot at %s; the slot is unbound (connection refused from a reachable host). The next acceptance dispatch will still refuse with needs_target until scripts/dev preview <head> binds it to the merge candidate.",
				host)
		}
	default: // slotProbeUnreachable or slotProbeReachableNoIdentity
		slot.State = "unverifiable"
		slot.Note = fmt.Sprintf(
			"acceptance validates against ONE shared preview slot at %s; the probe could not positively decide the slot's state (%s). Something may answer the address without exposing a comparable build identity, or the target is unreachable from this host (the probe runs from the process serving the tool). Verify with scripts/dev preview.",
			host, detail)
	}
	return slot
}
