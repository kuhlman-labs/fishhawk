package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_captain (E76.2 / #3765, ADR-083 #3751 rules 2-5) is the MCP half of
// the captain record: a thin wrapper over GET /v0/captain and the five POST
// /v0/captain/{offer,withdraw,accept,relinquish,claim} verbs. Every refusal is
// the backend's — this tool neither re-derives the record nor re-checks a
// verb, so an agent calling it is refused by the backend's single guard
// (captain_agent_identity_refused), never by a second copy here.
//
// The response is bounded per ADR-077 through the SHARED machinery
// (elisionLedger, attachAndMeasureOut, marshalledLen, capJSONString) — see
// boundCaptainOutput.

// captainActions is the closed action set; "read" is the default and the only
// action that writes nothing.
var captainActions = map[string]bool{
	"read": true, "offer": true, "withdraw": true, "accept": true, "relinquish": true, "claim": true,
}

// CaptainInput is the fishhawk_captain tool's input schema.
type CaptainInput struct {
	Repo      string `json:"repo,omitempty" jsonschema:"target repo as owner/name; falls back to GITHUB_REPOSITORY env when omitted"`
	Action    string `json:"action,omitempty" jsonschema:"one of read (default — writes nothing), offer, withdraw, accept, relinquish, claim"`
	Successor string `json:"successor,omitempty" jsonschema:"offer only: the subject you are offering the seat to (e.g. github:alice). Refused on every other action"`
}

// CaptainRecord mirrors the backend's current-captain projection.
// ClaimVerified is SEPARATE from IdentityVerified: null for an assigned
// (handed-over) seat, true/false only for a claimed one.
type CaptainRecord struct {
	Subject           string    `json:"subject"`
	IdentityVerified  bool      `json:"identity_verified" jsonschema:"true only for a provider-qualified subject (github:/gitlab:); a static token subject carries false"`
	ClaimVerified     *bool     `json:"claim_verified" jsonschema:"claimed seats only: true when a non-trivial repo approval predicate was checked and satisfied, false when the predicate was positively trivial; null for an assigned seat"`
	Basis             string    `json:"basis" jsonschema:"assigned (accepted a handover) or claimed (fallback claim at a vacant seat)"`
	AssignedSequence  int64     `json:"assigned_sequence"`
	AssignedEntryHash string    `json:"assigned_entry_hash"`
	AssignedAt        time.Time `json:"assigned_at"`
}

// CaptainOffer mirrors the backend's pending-offer projection.
// IdentityVerified is the SUCCESSOR's provider qualification.
type CaptainOffer struct {
	Successor        string    `json:"successor"`
	IdentityVerified bool      `json:"identity_verified"`
	OfferedBy        string    `json:"offered_by"`
	OfferEntryHash   string    `json:"offer_entry_hash"`
	OfferedSequence  int64     `json:"offered_sequence"`
	OfferedAt        time.Time `json:"offered_at"`
}

// CaptainHistoryEntry is one captain chain entry.
type CaptainHistoryEntry struct {
	Sequence  int64          `json:"sequence"`
	EntryHash string         `json:"entry_hash"`
	Category  string         `json:"category"`
	At        time.Time      `json:"at"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// CaptainState mirrors the GET /v0/captain body.
type CaptainState struct {
	Repo           string                `json:"repo"`
	Captain        *CaptainRecord        `json:"captain" jsonschema:"the current captain; null when the seat is vacant"`
	PendingOffer   *CaptainOffer         `json:"pending_offer" jsonschema:"the pending handover offer; null when none"`
	LastCaptain    *string               `json:"last_captain" jsonschema:"the most recent captain to leave the seat — what a claim records as previous_captain"`
	History        []CaptainHistoryEntry `json:"history" jsonschema:"the newest captain chain entries, ascending"`
	HistoryTotal   int                   `json:"history_total" jsonschema:"how many captain entries the chain holds for this repo"`
	SkippedEntries int                   `json:"skipped_entries"`
}

// CaptainVerbResult mirrors every POST verb's 200 body.
type CaptainVerbResult struct {
	Repo         string              `json:"repo"`
	Event        CaptainHistoryEntry `json:"event" jsonschema:"the chain entry this verb appended"`
	Captain      *CaptainRecord      `json:"captain"`
	PendingOffer *CaptainOffer       `json:"pending_offer"`
}

// CaptainOutput is the tool's result: exactly one of Record (action=read) or
// Result (a verb) is set.
type CaptainOutput struct {
	Record   *CaptainState      `json:"record,omitempty" jsonschema:"read action: the derived captain record"`
	Result   *CaptainVerbResult `json:"result,omitempty" jsonschema:"a verb: the recorded event and the record after it"`
	Elisions *Elisions          `json:"elisions,omitempty"`
}

// registerCaptain wires the fishhawk_captain tool.
func registerCaptain(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_captain",
		Description: strings.TrimSpace(`
Use this when you need to know who is captain of a repository, or when a
HUMAN captain is handing the seat over, stepping down, or claiming a vacant
seat (ADR-083).

ELIGIBILITY: action=read needs read:audit and writes nothing. Every other
action needs write:approvals and is REFUSED for agent and delegated identities
(captain_agent_identity_refused) — an agent can read the record but never hold,
hand over or claim the seat.

Actions (each appends exactly ONE audit chain entry, atomically):
  - read (default): current captain, pending offer, last captain, history.
  - offer (+successor): the sitting captain offers the seat.
  - withdraw: the captain who made the offer withdraws it.
  - accept: the named successor accepts and becomes captain.
  - relinquish: the sitting captain vacates the seat.
  - claim: a fallback claim of a VACANT seat, gated on the repository's
    approval predicate (strictest min_permission / member_of across its
    gates). A positively trivial predicate admits the claim with
    claim_verified=false; an undeterminable one (no run, no or unparseable
    spec, identity provider unavailable) is REFUSED.

identity_verified and claim_verified are separate: the first says the subject
is provider-qualified, the second says a claim was checked against a real
predicate.

Tool errors: repo missing; unknown action; successor on a non-offer action;
authentication_required (401); insufficient_scope (403);
captain_agent_identity_refused / captain_not_captain / captain_not_offerer /
captain_offer_successor_mismatch / captain_predicate_rejected (403);
captain_no_captain / captain_no_offer / captain_exists /
captain_self_handover (409); captain_successor_required /
validation_failed (400); captain_predicate_undeterminable (422);
captain_unconfigured (501).
`),
	}, resolver.captain)
}

// captain is the tool handler.
func (r *runResolver) captain(ctx context.Context, req *mcp.CallToolRequest, in CaptainInput) (*mcp.CallToolResult, CaptainOutput, error) {
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		repo = strings.TrimSpace(r.getenv("GITHUB_REPOSITORY"))
	}
	if repo == "" {
		return nil, CaptainOutput{}, fmt.Errorf("repo is required: pass repo or set GITHUB_REPOSITORY")
	}
	action := strings.TrimSpace(in.Action)
	if action == "" {
		action = "read"
	}
	if !captainActions[action] {
		return nil, CaptainOutput{}, fmt.Errorf("unknown action %q: one of read, offer, withdraw, accept, relinquish, claim", action)
	}
	successor := strings.TrimSpace(in.Successor)
	if successor != "" && action != "offer" {
		return nil, CaptainOutput{}, fmt.Errorf("successor applies only to action=offer; %s takes repo only", action)
	}
	budget := r.responseBudget(req)
	if action == "read" {
		st, err := r.api.GetCaptain(ctx, repo)
		if err != nil {
			return nil, CaptainOutput{}, fmt.Errorf("get captain: %w", err)
		}
		out, err := boundCaptainOutput(CaptainOutput{Record: st}, repo, budget)
		if err != nil {
			return nil, CaptainOutput{}, err
		}
		return nil, out, nil
	}
	res, err := r.api.PostCaptainVerb(ctx, action, repo, successor)
	if err != nil {
		return nil, CaptainOutput{}, fmt.Errorf("captain %s: %w", action, err)
	}
	out, err := boundCaptainOutput(CaptainOutput{Result: res}, repo, budget)
	if err != nil {
		return nil, CaptainOutput{}, err
	}
	return nil, out, nil
}

// boundCaptainOutput is the ADR-077 ladder for fishhawk_captain. Every tier
// MARKS its truncation.
//
//	B1    drop history from the OLDEST end (halving), keeping the newest
//	      entries — the ones nearest the current record.
//	FLOOR history and the verb event's payload dropped, every retained
//	      string capped: the current captain, pending offer and last captain
//	      are what the record is FOR, and are always kept.
func boundCaptainOutput(out CaptainOutput, repo string, budget responseBudget) (CaptainOutput, error) {
	set := func(o *CaptainOutput, e *Elisions) { o.Elisions = e }
	n, err := marshalledLen(out)
	if err != nil {
		return out, err
	}
	if n <= budget.bytes {
		return out, nil
	}
	pointer := pointerREST("/v0/captain?repo=" + repo)
	if out.Record != nil && len(out.Record.History) > 1 {
		led := &elisionLedger{budget: budget.bytes, source: budget.source, tier: "B1",
			note: "response reduced to fit the tool-result byte budget"}
		total := len(out.Record.History)
		for len(out.Record.History) > 1 {
			keep := len(out.Record.History) / 2
			out.Record.History = out.Record.History[len(out.Record.History)-keep:]
			led.entries = []elidedField{newStoredElision("record.history", fmt.Sprintf(
				"%d oldest history entries were dropped to fit the byte budget; the newest %d are kept and the current captain, pending offer and last captain are intact. The surface below returns the history",
				total-keep, keep), pointer.retrievalPointer, total-keep)}
			fits, ferr := attachAndMeasureOut(&out, led, budget.bytes, set)
			if ferr != nil {
				return out, ferr
			}
			if fits {
				return out, nil
			}
		}
	}
	return captainFloor(out, pointer, budget)
}

// captainFloor keeps the record's identity fields with every string capped
// and every unbounded part (history, event payload) dropped under one
// aggregate elision.
func captainFloor(out CaptainOutput, pointer unboundedPointer, budget responseBudget) (CaptainOutput, error) {
	floor := CaptainOutput{}
	if out.Record != nil {
		st := *out.Record
		st.Repo = capJSONString(st.Repo, floorFieldCap)
		st.Captain = capCaptainRecord(st.Captain)
		st.PendingOffer = capCaptainOffer(st.PendingOffer)
		if st.LastCaptain != nil {
			v := capJSONString(*st.LastCaptain, floorFieldCap)
			st.LastCaptain = &v
		}
		st.History = []CaptainHistoryEntry{}
		floor.Record = &st
	}
	if out.Result != nil {
		res := *out.Result
		res.Repo = capJSONString(res.Repo, floorFieldCap)
		res.Event.Payload = nil
		res.Event.Category = capJSONString(res.Event.Category, floorFieldCap)
		res.Captain = capCaptainRecord(res.Captain)
		res.PendingOffer = capCaptainOffer(res.PendingOffer)
		floor.Result = &res
	}
	led := &elisionLedger{budget: budget.bytes, source: budget.source, tier: floorTierName,
		note: "reduced to the constant-size floor: the current captain, pending offer and last captain with every string capped. The entry below is an AGGREGATE — the floor tier's explicit exception to per-field itemisation"}
	led.add(aggregateStoredElision("*", fmt.Sprintf(
		"the history list and the event payload were omitted and every retained string was capped to %d bytes; the surface below returns the full record", floorFieldCap),
		[]string{pointer.String()}))
	w, err := led.wire()
	if err != nil {
		return out, err
	}
	floor.Elisions = w
	return floor, nil
}

func capCaptainRecord(c *CaptainRecord) *CaptainRecord {
	if c == nil {
		return nil
	}
	v := *c
	v.Subject = capJSONString(v.Subject, floorFieldCap)
	v.Basis = capJSONString(v.Basis, floorFieldCap)
	v.AssignedEntryHash = capJSONString(v.AssignedEntryHash, floorFieldCap)
	return &v
}

func capCaptainOffer(o *CaptainOffer) *CaptainOffer {
	if o == nil {
		return nil
	}
	v := *o
	v.Successor = capJSONString(v.Successor, floorFieldCap)
	v.OfferedBy = capJSONString(v.OfferedBy, floorFieldCap)
	v.OfferEntryHash = capJSONString(v.OfferEntryHash, floorFieldCap)
	return &v
}
