package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// previewCampaignRequest is the POST /v0/campaigns/preview body (#3647). It
// carries ONLY the SOURCE fields — repo plus one of epic_ref / items /
// grooming_source, and the optional provider selector — because a preview is a
// dry run of a campaign's ASSEMBLY, not of its persistence. pause_policy, operator_agent, working_dir and the
// Idempotency-Key are create-only knobs that decide nothing about the resolved
// DAG, so they are deliberately ABSENT from this struct and, because the
// decoder runs with DisallowUnknownFields, sending one is a 400
// validation_failed rather than a silently-ignored field.
type previewCampaignRequest struct {
	Repo string `json:"repo"`
	// EpicRef decomposes an epic's children, exactly as on create. Optional;
	// one of epic_ref / items / grooming_source is required.
	EpicRef string `json:"epic_ref,omitempty"`
	// Items is the subset filter (WITH epic_ref) or the authoritative no-epic
	// item set (WITHOUT epic_ref), exactly as on create.
	Items []string `json:"items,omitempty"`
	// GroomingSource previews an approved grooming run's ratified order,
	// exactly as on create. Mutually exclusive with epic_ref and items.
	GroomingSource *groomingSourceRequest `json:"grooming_source,omitempty"`
	// Provider is the OPTIONAL work-item provider selector (#3645), carried
	// here because it is SOURCE shape, not a persistence knob: it decides which
	// issue tracker the DAG is resolved against. A preview that could not be
	// pointed at the provider the create will be pointed at would report a graph
	// assembled from a different tracker than the one about to be campaigned
	// over. Validated by the SAME validateCampaignProviderSelector create uses,
	// so an unregistered id is the identical 400.
	Provider string `json:"provider,omitempty"`
}

// campaignPreviewItem is one resolved item in a preview report. It mirrors the
// persisted campaignItemResponse's legible fields, minus everything that only
// exists once a campaign row does (id, run_id, state, pause_reason).
//
// Wave is a *int rather than an int because it is the ONE field a preview
// cannot always answer: on an INVALID set the assembler never reached the
// topological sort, so there is no wave to report and the key is omitted. Every
// other field — issue_ref, depends_on, position — IS answerable on both paths
// and is always present, which is what makes an invalid report show the partial
// graph rather than only the failure (operator condition 2).
type campaignPreviewItem struct {
	IssueRef string `json:"issue_ref"`
	// DependsOn are the IN-SET depends_on edges for this item, rendered as
	// issue refs. This is how the preview carries the resolved edge set: an
	// edge From->To appears as To in From's depends_on. Always present (an
	// item with no dependency carries an empty array, never null), on BOTH the
	// valid and the invalid path.
	DependsOn []string `json:"depends_on"`
	Wave      *int     `json:"wave,omitempty"`
	Autonomy  string   `json:"autonomy,omitempty"`
	Position  int      `json:"position"`
}

// campaignPreviewDangling is one depends_on edge that blocks assembly, reported
// as DATA at 200 rather than as the 422 refusal create answers with (#3647).
// From/To use the same rendering authority the create refusal's detail lists do
// (workmgmt.DependsEdge.TargetRef), so an unresolvable cross-repo target renders
// `unparsable:<digest>:"token"` and never the identity-less `issue:0`.
type campaignPreviewDangling struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Reason is the drop cause, one of not_child / excluded_incomplete /
	// target_closed_incomplete / target_state_unreadable — the same four the
	// create refusal's dangling_* detail keys carry.
	Reason string `json:"reason"`
	// Remedy is one short operator-facing sentence per reason, so a report is
	// actionable without cross-referencing the create refusal's message.
	Remedy string `json:"remedy"`
}

// campaignPreviewResponse is the POST /v0/campaigns/preview report. It mirrors
// docs/api/v0.openapi.yaml's CampaignPreview schema.
//
// The whole point of the verb is that an unassemblable set is a 200 REPORT, not
// a refusal: valid:false with the blocking edges named is DATA an operator can
// iterate against, where create's 422 campaign_dangling_dependency is a wall.
// Refusals that are about the REQUEST rather than the graph (a malformed ref, a
// non-child subset ref, a provider without the capability, an uninstalled App)
// stay refusals, byte-identical to create's — that is what resolveCampaignSource
// being shared buys.
type campaignPreviewResponse struct {
	// Valid is true when campaign.Assemble succeeded — i.e. POST /v0/campaigns
	// with this same body would have created a campaign.
	Valid bool   `json:"valid"`
	Repo  string `json:"repo"`
	// EpicRef is the trimmed epic ref; omitted for the no-epic / grooming
	// variants, matching the empty-string sentinel a no-epic campaign persists.
	EpicRef   string `json:"epic_ref,omitempty"`
	ItemCount int    `json:"item_count"`
	// WaveCount and Waves are the topological dispatch order. Both are OMITTED
	// on an invalid set (assembly never reached the sort) — the one thing an
	// invalid report may leave out.
	WaveCount *int       `json:"wave_count,omitempty"`
	Waves     [][]string `json:"waves,omitempty"`
	// Items are the resolved items with their in-set edges. ALWAYS present on
	// both paths (operator condition 2): an operator inspecting an invalid set
	// needs the partial graph, not just the failure.
	Items []campaignPreviewItem `json:"items"`
	// SatisfiedDependencies are the depends_on edges elided because their
	// out-of-set target was already closed-and-completed (#2953), the same
	// block the create response carries.
	SatisfiedDependencies []satisfiedDependencyPayload `json:"satisfied_dependencies,omitempty"`
	// GroomingSource echoes the resolved grooming provenance for a
	// grooming-sourced preview. Omitted for every epic_ref / items preview.
	GroomingSource *campaignGroomingSourcePayload `json:"grooming_source,omitempty"`
	// Dangling names the blocking edges on an invalid set. Empty on a valid
	// one, and empty (with Message set) for a defensively-wrapped dangling
	// error that carries no typed form.
	Dangling []campaignPreviewDangling `json:"dangling,omitempty"`
	// ClosureCandidates are the DEDUPED, sorted out-of-set targets a caller
	// would add to items to close the set — derived from the not_child and
	// excluded_incomplete edges ONLY, the two whose remedy IS widening the
	// batch. A target_closed_incomplete or target_state_unreadable target is
	// deliberately EXCLUDED: adding it cannot satisfy the edge, so offering it
	// would send an operator after a widen that provably fails.
	//
	// It is ONE HOP, not a transitive closure: adding these can surface a new
	// generation of dangling edges, so closing a deep graph is an
	// iterate-until-valid loop — which is exactly what this endpoint makes
	// cheap. Do not read one preview as a completeness proof.
	ClosureCandidates []string `json:"closure_candidates,omitempty"`
	// Cycle carries the assembler's cycle message when the set is invalid
	// because its depends_on edges cycle. Omitted otherwise.
	Cycle string `json:"cycle,omitempty"`
	// Message carries the assembler's own message on a dangling-set report, so
	// a defensively-wrapped dangling error with no typed form still says WHY
	// rather than reporting an empty failure. Omitted on a valid report.
	Message string `json:"message,omitempty"`
}

// Per-reason remedies rendered into a dangling report entry. They are one short
// sentence each, matching the remedy the MCP start_campaign refusal renderer
// gives for the same cause — a preview and a refusal must not disagree about
// what to do.
const (
	previewRemedyNotChild           = "the target is outside the assembled set; add it to items (it is listed in closure_candidates), or drop the depends_on edge"
	previewRemedyExcludedIncomplete = "the target is a sibling excluded from items and not yet complete; include it in items (it is listed in closure_candidates), or omit items to sweep every child"
	previewRemedyClosedIncomplete   = "the target closed WITHOUT completing (not_planned/duplicate), so its work did not land and widening the batch cannot satisfy the edge; reopen or replace the dependency, or drop the edge"
	previewRemedyStateUnreadable    = "the target's issue state could not be read, so the dependency is UNPROVEN rather than unsatisfied; retry"
)

// handlePreviewCampaign implements POST /v0/campaigns/preview (#3647): a
// NON-MUTATING dry run of POST /v0/campaigns' assembly. It resolves the SAME
// item set through the SAME shared resolver, runs the SAME campaign.Assemble,
// and reports the wave-ordered DAG — or the edges that block it — as a 200
// report. It creates no campaign row, no item row and no audit entry on ANY
// path, which is what makes iterating toward a dependency-closed item set cheap
// instead of a sequence of 422s.
//
// It requires the SAME write:campaigns scope create does, deliberately. A
// preview costs the IDENTICAL per-item forge sweep and is the pre-flight of a
// write; widening it to read-only tokens would open a forge-read amplification
// surface to a strictly less privileged token for no operator benefit.
func (s *Server) handlePreviewCampaign(w http.ResponseWriter, r *http.Request) {
	// requestStart anchors the no-epic issue-set resolution deadline at THIS
	// handler's entry, for the reason handleCreateCampaign's opening comment
	// spells out: the client's timeout measures the whole request, so the
	// budget must cover everything ahead of the resolver too.
	requestStart := time.Now()
	if !s.requireWriteScope(w, r, "write:campaigns") {
		return
	}
	// The preview writes nothing, so it does not strictly need a campaign
	// repository — but an unwired deploy cannot CREATE the campaign a preview
	// is the pre-flight for, so reporting a previewable set there would be
	// misleading. It answers the same 503 its sibling does.
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}

	var req previewCampaignRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}

	// Convert to the create request the SHARED helpers take. Only the source
	// fields cross: the create-only knobs have no preview equivalent and were
	// already rejected as unknown fields by the decoder above.
	createReq := createCampaignRequest{
		Repo:           req.Repo,
		EpicRef:        req.EpicRef,
		Items:          req.Items,
		GroomingSource: req.GroomingSource,
		Provider:       req.Provider,
	}
	epicRef, owner, name, refusal := validateCampaignSourceShape(createReq)
	if refusal != nil {
		s.writeCampaignSourceRefusal(w, r, refusal)
		return
	}
	// The provider selector is validated by the SAME shared helper, in the same
	// position relative to the source shape, so an unregistered id refuses
	// identically here and at create without a forge round-trip.
	if provRefusal := validateCampaignProviderSelector(createReq); provRefusal != nil {
		s.writeCampaignSourceRefusal(w, r, provRefusal)
		return
	}
	source, refusal := s.resolveCampaignSource(r.Context(), requestStart, createReq, epicRef, owner, name)
	if refusal != nil {
		s.writeCampaignSourceRefusal(w, r, refusal)
		return
	}

	assembly, err := campaign.Assemble(epicRef, source.Result)
	if err != nil {
		switch {
		case errors.Is(err, campaign.ErrDanglingDependency):
			// THE POINT OF THE VERB: a dangling set is a 200 report, not the
			// 422 create refuses with.
			resp := partialPreviewReport(req.Repo, epicRef, source)
			resp.Message = err.Error()
			resp.Dangling, resp.ClosureCandidates = danglingPreview(err)
			s.writeJSON(w, r, http.StatusOK, resp)
			return
		case errors.Is(err, campaign.ErrCycle):
			resp := partialPreviewReport(req.Repo, epicRef, source)
			resp.Cycle = err.Error()
			s.writeJSON(w, r, http.StatusOK, resp)
			return
		default:
			// Neither sentinel (today: only Assemble's nil-result invariant).
			// That is not a property of the item set, so it stays the same 400
			// create answers with rather than becoming a valid:false report.
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				err.Error(), map[string]any{"epic_ref": req.EpicRef})
			return
		}
	}

	items := make([]campaignPreviewItem, 0, len(assembly.Items))
	for _, it := range assembly.Items {
		wave := it.Wave
		items = append(items, campaignPreviewItem{
			IssueRef:  it.IssueRef,
			DependsOn: nonNilRefs(it.DependsOn),
			Wave:      &wave,
			Autonomy:  it.Autonomy,
			Position:  it.Position,
		})
	}
	waveCount := len(assembly.Waves)
	s.writeJSON(w, r, http.StatusOK, campaignPreviewResponse{
		Valid:                 true,
		Repo:                  req.Repo,
		EpicRef:               epicRef,
		ItemCount:             len(assembly.Items),
		WaveCount:             &waveCount,
		Waves:                 assembly.Waves,
		Items:                 items,
		SatisfiedDependencies: satisfiedDependencyPayloads(assembly.SatisfiedDependencies),
		GroomingSource:        source.Provenance,
	})
}

// partialPreviewReport builds the valid:false skeleton for a set that failed to
// assemble: the items that DID resolve, each with its in-set depends_on edges,
// in resolution order. waves/wave_count are deliberately absent — assembly never
// reached the topological sort — but items and edges are not, because an
// operator iterating toward a closed set needs the partial graph (operator
// condition 2).
func partialPreviewReport(repo, epicRef string, source *campaignSourceResolution) campaignPreviewResponse {
	res := source.Result
	// Reverse-index the in-set edges: edge From->To means From depends on To.
	// Only edges whose BOTH endpoints are in the resolved child set are in-set;
	// a dropped edge is reported separately under dangling.
	inSet := make(map[int]bool, len(res.Children))
	for _, c := range res.Children {
		inSet[c.Number] = true
	}
	depsOf := make(map[int][]string, len(res.Children))
	for _, e := range res.Edges {
		if !inSet[e.From] || !inSet[e.To] {
			continue
		}
		depsOf[e.From] = append(depsOf[e.From], e.TargetRef())
	}
	items := make([]campaignPreviewItem, 0, len(res.Children))
	for i, c := range res.Children {
		items = append(items, campaignPreviewItem{
			IssueRef:  "issue:" + strconv.Itoa(c.Number),
			DependsOn: nonNilRefs(depsOf[c.Number]),
			Autonomy:  c.Autonomy,
			Position:  i,
		})
	}
	return campaignPreviewResponse{
		Valid:                 false,
		Repo:                  repo,
		EpicRef:               epicRef,
		ItemCount:             len(res.Children),
		Items:                 items,
		SatisfiedDependencies: satisfiedDependencyPayloads(res.SatisfiedEdges),
		GroomingSource:        source.Provenance,
	}
}

// danglingPreview renders a (possibly-wrapped) dangling assembly error into the
// report's dangling edge list and its closure_candidates set.
//
// A defensively-wrapped dangling error with no typed *campaign.
// DanglingDependencyError form yields NO edges and NO candidates — the caller
// still reports valid:false with the assembler's message, never valid:true.
func danglingPreview(err error) ([]campaignPreviewDangling, []string) {
	var de *campaign.DanglingDependencyError
	if !errors.As(err, &de) {
		return nil, nil
	}
	var edges []campaignPreviewDangling
	// closure holds ONLY the targets whose remedy is widening the batch.
	closure := map[string]bool{}
	appendEdges := func(in []workmgmt.DependsEdge, reason, remedy string, widenable bool) {
		for _, e := range in {
			target := e.TargetRef()
			edges = append(edges, campaignPreviewDangling{
				From:   "issue:" + strconv.Itoa(e.From),
				To:     target,
				Reason: reason,
				Remedy: remedy,
			})
			if widenable {
				closure[target] = true
			}
		}
	}
	appendEdges(de.NotChild, string(workmgmt.DropNotChild), previewRemedyNotChild, true)
	appendEdges(de.ExcludedIncomplete, string(workmgmt.DropExcludedIncomplete), previewRemedyExcludedIncomplete, true)
	// The two UNWIDENABLE causes: adding the target to items cannot satisfy the
	// edge, so they are reported but contribute NO closure candidate.
	appendEdges(de.ClosedIncomplete, string(workmgmt.DropTargetClosedIncomplete), previewRemedyClosedIncomplete, false)
	appendEdges(de.StateUnreadable, string(workmgmt.DropTargetStateUnreadable), previewRemedyStateUnreadable, false)

	candidates := make([]string, 0, len(closure))
	for ref := range closure {
		candidates = append(candidates, ref)
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return edges, nil
	}
	return edges, candidates
}

// nonNilRefs normalizes a ref slice so the wire shape is always a JSON array
// rather than null — the convention campaignRollupPayload's slices follow.
func nonNilRefs(refs []string) []string {
	if refs == nil {
		return []string{}
	}
	return refs
}
