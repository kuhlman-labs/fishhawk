package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/splitfiling"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// splitChildrenFiledCategory is the audit-log category for the ONE completion
// marker fileSplitProposalChildren emits after every phased child of an
// approved split_proposal is durably filed (#2057, E50.5). It carries the
// contract-phase classification, the filed children (ordinal/number/url), the
// contract-child number, the #2062 close-parent-watcher deferral reference, and
// — for a governed-exception — the in-memory-only cap-exception draft. It is the
// sole surface fishhawk_get_plan's loadSplitFiling reads (a sibling slice), so
// it is written ONCE, only on FULL completion (all N children durably filed),
// never per child; per-child durable progress rides work_item_filed markers so
// a partial run stays resumable (operator binding condition 1).
const splitChildrenFiledCategory = "split_children_filed"

// splitFilingRefusedCategory is the audit-log category for the ONE refusal
// marker fileSplitProposalChildren emits when a split_proposal has a PHASE whose
// own declared scope.files count exceeds the resolved implement cap (#2412). It
// is what stops the operator having to close auto-filed children: #2410 (a lead
// phase of 160 files against a cap of 45) would never have been emitted — the
// hook files ZERO children, records this marker naming every offending phase, and
// posts a parent refusal comment. Registered in audit.KnownCategories and
// surfaced through fishhawk_get_plan (loadSplitFiling's refused sub-object).
const splitFilingRefusedCategory = "split_filing_refused"

// SplitFilingRefusedCategory is the exported alias of splitFilingRefusedCategory
// — the audit category the on-approval hook writes its refusal marker under
// (#2412). The mcpserver getPlan read path queries the literal
// "split_filing_refused"; the cross-boundary round-trip test seeds under THIS
// const and reads through that literal, so a drift between the hook's write
// category and the resolver's read category fails the single end-to-end test.
const SplitFilingRefusedCategory = splitFilingRefusedCategory

// splitFilingRefusedPhase is one over-cap phase recorded on the refusal marker.
type splitFilingRefusedPhase struct {
	Index         int    `json:"index"`
	Title         string `json:"title"`
	DeclaredCount int    `json:"declared_count"`
}

// splitFilingRefusedPayload is the refusal marker's payload shape (#2412),
// decoded verbatim by fishhawk_get_plan's loadSplitFiling into its refused
// sub-object. Reason is the operator-facing summary; Cap the resolved implement
// cap the phases exceeded; Phases names each offending phase.
type splitFilingRefusedPayload struct {
	Reason string                    `json:"reason"`
	Cap    int                       `json:"cap"`
	Phases []splitFilingRefusedPhase `json:"phases"`
}

// SplitFilingRefusedPayload is an exported alias of the refusal-marker payload
// type. It exists so the sibling fishhawk-mcp getPlan round-trip test decodes the
// hook's ACTUAL payload type — compile-locking the approval-hook-write ↔ MCP-read
// audit-boundary contract — instead of a hand-copied wire mirror (mirroring the
// [SplitChildrenFiledPayload] pattern, #2412).
type SplitFilingRefusedPayload = splitFilingRefusedPayload

// SplitFilingRefusedPhase is an exported alias of the per-phase refusal-marker
// entry type (see [SplitFilingRefusedPayload]).
type SplitFilingRefusedPhase = splitFilingRefusedPhase

// splitFilingChild is one filed phased child recorded on the completion marker.
type splitFilingChild struct {
	PhaseIndex int    `json:"phase_index"`
	Title      string `json:"title"`
	Number     int    `json:"number"`
	URL        string `json:"url"`
	IsContract bool   `json:"is_contract"`
}

// splitCapExceptionDraft mirrors splitfiling.CapExceptionDraft onto the audit
// payload — the operator-authored, admin-merged draft that rides the audit only
// and is never written to .fishhawk/**.
type splitCapExceptionDraft struct {
	SpecDiff string `json:"spec_diff"`
	PRBody   string `json:"pr_body"`
}

// splitChildrenFiledPayload is the completion marker's payload shape, decoded
// verbatim by fishhawk_get_plan's loadSplitFiling.
type splitChildrenFiledPayload struct {
	ContractClassification string                  `json:"contract_classification"`
	Children               []splitFilingChild      `json:"children"`
	ContractChildNumber    int                     `json:"contract_child_number"`
	DeferralIssue          int                     `json:"deferral_issue"`
	CapException           *splitCapExceptionDraft `json:"cap_exception,omitempty"`
}

// SplitChildrenFiledPayload is an exported alias of the completion-marker payload
// type. It exists so the sibling fishhawk-mcp getPlan round-trip test decodes the
// hook's ACTUAL payload type — compile-locking the approval-hook-write ↔ MCP-read
// audit-boundary contract — instead of a hand-copied wire mirror that can
// silently drift from these struct tags (#2057 fix-up, resolving the
// E2E-verification concern).
type SplitChildrenFiledPayload = splitChildrenFiledPayload

// SplitFilingChild is an exported alias of the per-child completion-marker entry
// type (see [SplitChildrenFiledPayload]).
type SplitFilingChild = splitFilingChild

// SplitCapExceptionDraft is an exported alias of the cap-exception draft carried
// on the completion marker (see [SplitChildrenFiledPayload]).
type SplitCapExceptionDraft = splitCapExceptionDraft

// splitChildFiledMarker is the per-phase durable resume marker
// fileSplitProposalChildren records — via the existing work_item_filed category
// so it needs no new registry entry — immediately after each child is filed. On
// a re-approval after a partial failure, loadFiledSplitChildren reads these back
// to skip the already-filed ordinals (never a duplicate) and to resolve
// depends_on edges to sibling #N. The split_phase_index discriminator is what
// distinguishes a split-filing marker from any other work_item_filed entry on
// the run.
type splitChildFiledMarker struct {
	SplitPhaseIndex *int   `json:"split_phase_index"`
	CreatedNumber   int    `json:"created_number"`
	CreatedURL      string `json:"created_url"`
}

// filedChild is the in-memory resume record (number + url) for one filed phase.
type filedChild struct {
	number int
	url    string
}

// fileSplitProposalChildren is the best-effort on-approval hook (#2057, E50.5):
// when an operator approves a plan carrying a split_proposal at the plan gate,
// it files N conventions-complete phased child issues with depends_on edges and
// symbol-set scopes, classifies the contract phase delete-only vs
// governed-exception, drafts the in-memory cap exception for the latter, posts
// the parent acceptance-carrier comment, and emits the split_children_filed
// completion marker. It is invoked from finishApprovalAdvance with the same
// best-effort posture as recordDrivePlanApproved — every forge / work-item /
// audit error logs and returns, NEVER unwinding the approval the gate already
// recorded.
//
// Idempotency (operator binding condition 1): a prior split_children_filed
// completion marker means every child is already filed → no-op. A partial run
// (some children filed, then an error) writes NO completion marker, so a
// re-approval re-enters here, reads the per-phase work_item_filed resume markers
// — the durable per-ordinal filing record — skips the already-recorded ordinals,
// and fills only the un-recorded ones. The per-ordinal marker append is a HARD
// prerequisite, not a best-effort side effect: if it fails the run aborts (no
// completion marker, no parent comment) rather than proceeding on a durable
// state it can no longer resume from — mirroring refinement.ExecuteFiling's
// abort-on-record-failure discipline. The one irreducible residual — a child
// whose provider File succeeded but whose marker never persisted re-files once
// on a re-approval — is the same at-least-once seam ExecuteFiling documents
// (GitHub's issue-create API offers no idempotency key to close it).
func (s *Server) fileSplitProposalChildren(ctx context.Context, stage *run.Stage) {
	if s.cfg.AuditRepo == nil || s.cfg.RunRepo == nil {
		return
	}
	runID := stage.RunID

	approvedPlan, err := s.loadApprovedPlanForRun(ctx, runID)
	if err != nil {
		s.logSplitFilingWarn(ctx, runID, "load approved plan failed", err.Error())
		return
	}
	if approvedPlan == nil || approvedPlan.SplitProposal == nil || len(approvedPlan.SplitProposal.Phases) == 0 {
		return // no split_proposal → nothing to file
	}
	proposal := *approvedPlan.SplitProposal

	// Completion dedup: a prior split_children_filed marker means the full set
	// is already filed. A partial run left no marker, so it falls through and
	// resumes below.
	priorCompletion, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, splitChildrenFiledCategory)
	if err != nil {
		s.logSplitFilingWarn(ctx, runID, "list split_children_filed failed", err.Error())
		return
	}
	if len(priorCompletion) > 0 {
		return // already completed — idempotent no-op
	}

	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		s.logSplitFilingWarn(ctx, runID, "get run failed", err.Error())
		return
	}
	owner, name, ok := splitRepoFullName(runRow.Repo)
	if !ok {
		s.logSplitFilingWarn(ctx, runID, "malformed run repo", runRow.Repo)
		return
	}
	parentIssue := 0
	if runRow.TriggerRef != nil {
		if n, ok := parseIssueRef(*runRow.TriggerRef); ok {
			parentIssue = n
		}
	}
	if parentIssue == 0 {
		// No originating issue to reference / carry acceptance for; the whole
		// hook is issue-scoped, so skip rather than file orphan children.
		return
	}

	// Resolve the implement cap (0 = unresolved → Classify fails safe to
	// delete-only). fail-open on an unresolved spec.
	capFiles := 0
	if cons, _, ok := s.resolveImplementConstraints(ctx, runRow); ok {
		capFiles = cons.MaxFilesChanged
	}

	// #2412 REFUSAL: before building/classifying/filing ANY child, refuse the
	// whole filing when a phase's OWN declared scope.files count exceeds the
	// resolved implement cap — an over-cap lead phase (the #2410 case) would
	// itself fail category-B in its own implement stage, so filing it would just
	// make the operator close it. capFiles == 0 (unresolved cap) yields no
	// violations, so filing proceeds exactly as before.
	if violations := splitfiling.PhaseCapViolations(proposal, capFiles); len(violations) > 0 {
		s.refuseSplitFilingOverCapPhase(ctx, runRow, owner, name, parentIssue, capFiles, violations)
		return
	}

	// Reachability evidence for the contract classification (fail-open to nil).
	evidence := s.loadReachabilityEvidence(ctx, runID)

	specs := splitfiling.BuildChildSpecs(splitfiling.BuildInput{
		Proposal:         proposal,
		ParentIssue:      parentIssue,
		ParentAcceptance: acceptanceCriteriaStatements(approvedPlan),
	})
	classification := splitfiling.Classify(proposal, evidence, capFiles)
	capDraft := splitfiling.DraftCapException(proposal, evidence, capFiles)

	conv, err := conventionsLoader(ctx, runRow.Repo)
	if err != nil {
		s.logSplitFilingWarn(ctx, runID, "load work-management conventions failed", err.Error())
		return
	}
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
	}
	if runRow.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*runRow.InstallationID)
	}
	if target.Scope.IsZero() && s.cfg.GitHub != nil {
		if scope, rerr := s.resolveRepoScope(ctx, owner, name); rerr == nil {
			target.Scope = scope
		}
	}

	// Resume: the durable per-phase markers already filed on a prior pass.
	filed := s.loadFiledSplitChildren(ctx, runID)
	parentEpicRef := "#" + strconv.Itoa(parentIssue)

	// File each un-filed phase in wave (dependency) order, resolving each 0-based
	// depends_on edge to the already-filed sibling #N.
	for _, idx := range splitPhaseOrder(proposal) {
		if _, done := filed[idx]; done {
			continue // durably filed on a prior pass
		}
		spec := specs[idx]
		deps := make([]string, 0, len(spec.DependsOn))
		depsResolved := true
		for _, d := range spec.DependsOn {
			fc, ex := filed[d]
			if !ex {
				// A dependency has not filed yet (a prior-pass failure on an
				// earlier ordinal). Wave order normally guarantees deps first;
				// stop and leave the run resumable rather than file an edge to a
				// non-existent sibling.
				depsResolved = false
				break
			}
			deps = append(deps, "#"+strconv.Itoa(fc.number))
		}
		if !depsResolved {
			break
		}
		child := refinement.ChildDraft{
			Summary:            spec.Title,
			Proposal:           spec.Proposal,
			DoneMeans:          spec.DoneMeans,
			AcceptanceCriteria: spec.AcceptanceCriteria,
		}
		// Pass the parent issue as the explicit {epic} title var so the child
		// title renders deterministically ("[E<parent>.<n>] <phase>") WITHOUT a
		// forge round-trip: deriveEpicTitleVar short-circuits when TitleVars
		// already carries "epic", and the ordinal supplies "n". The split
		// parent is a regular issue, not an epic with a leading [E<n>] title, so
		// deriving {epic} from its title would fail and 422 the filing — an
		// explicit var keeps the best-effort hook self-contained. The ParentEpic
		// relation still renders the "Parent epic: #N" body marker + depends_on.
		req := refinement.FilingRequestForChild(child, idx+1, strconv.Itoa(parentIssue), parentEpicRef, deps)
		_, created, werr := s.applyAndFileWorkItem(ctx, req, conv, target, owner, name)
		if werr != nil {
			s.logSplitFilingWarn(ctx, runID, "file split child failed", werr.msg)
			break // no completion marker; run stays resumable
		}
		filed[idx] = filedChild{number: created.Number, url: created.URL}
		if err := s.writeSplitChildFiledMarker(ctx, runRow, spec, created); err != nil {
			// The durable per-ordinal record is a HARD prerequisite: a lost
			// marker aborts the run with NO completion marker (and no parent
			// comment) rather than filing more children on top of a durable
			// state we can no longer resume from — the same abort-on-record-
			// failure discipline refinement.ExecuteFiling uses. The just-filed
			// ordinal re-files once on a re-approval (its record never
			// persisted): the documented at-least-once residual, not a widening
			// of it.
			break
		}
	}

	// Completion only when EVERY phase is durably filed (operator binding
	// condition 1): a partial run writes no completion marker and posts no
	// parent comment, so a re-approval resumes.
	if len(filed) != len(specs) {
		return
	}

	children := make([]splitFilingChild, 0, len(specs))
	contractChildNumber := 0
	for _, spec := range specs {
		fc := filed[spec.PhaseIndex]
		children = append(children, splitFilingChild{
			PhaseIndex: spec.PhaseIndex,
			Title:      spec.Title,
			Number:     fc.number,
			URL:        fc.url,
			IsContract: spec.IsContract,
		})
		if spec.IsContract {
			contractChildNumber = fc.number
		}
	}

	// Persist the completion marker (the durable dedup record) BEFORE posting the
	// best-effort parent comment: once the marker is durable a re-approval no-ops
	// at the priorCompletion gate above and never re-enters, so the acceptance-
	// carrier comment is posted at most once for a completed filing. (If the
	// completion append itself fails, a re-approval re-enters and may re-post — a
	// narrow best-effort residual on that one interleaving, not a guarantee.)
	s.writeSplitChildrenFiledAudit(ctx, runRow, classification, children, contractChildNumber, capDraft)
	s.postSplitParentComment(ctx, runRow, owner, name, parentIssue, contractChildNumber)
}

// splitPhaseOrder returns the split proposal's phase indices in a
// dependency-respecting (wave) order via a Kahn topological sort over the
// 0-based depends_on edges. The plan's semanticCheck already validated the
// edges acyclic and in-range, so the sort always drains; on the defensive
// off-chance a cycle slips through, any not-yet-emitted indices are appended in
// natural order so no phase is dropped (filing then stops at the first
// unresolvable edge rather than looping).
func splitPhaseOrder(sp plan.SplitProposal) []int {
	n := len(sp.Phases)
	indeg := make([]int, n)
	for i, ph := range sp.Phases {
		indeg[i] = len(ph.DependsOn)
	}
	emitted := make([]bool, n)
	order := make([]int, 0, n)
	for len(order) < n {
		progressed := false
		for i := 0; i < n; i++ {
			if emitted[i] || indeg[i] != 0 {
				continue
			}
			emitted[i] = true
			order = append(order, i)
			progressed = true
			for j, ph := range sp.Phases {
				if emitted[j] {
					continue
				}
				for _, d := range ph.DependsOn {
					if d == i {
						indeg[j]--
					}
				}
			}
		}
		if !progressed {
			break
		}
	}
	// Defensive: append any phase a cycle left unemitted, in natural order.
	for i := 0; i < n; i++ {
		if !emitted[i] {
			order = append(order, i)
		}
	}
	return order
}

// acceptanceCriteriaStatements extracts the plan's acceptance-criteria
// statements — the text the contract child carries as the acceptance carrier.
// Nil/empty for an older plan without the typed contract.
func acceptanceCriteriaStatements(p *plan.Plan) []string {
	crits := p.Verification.AcceptanceCriteria
	if len(crits) == 0 {
		return nil
	}
	out := make([]string, 0, len(crits))
	for _, c := range crits {
		out = append(out, c.Statement)
	}
	return out
}

// loadReachabilityEvidence reads the newest plan_reachability_sweep audit entry
// for the run and maps its phases into []splitfiling.PhaseEvidence for the
// contract classification. Fail-open to nil on every branch (no repo, no entry,
// decode failure) — Classify then fails safe to delete-only.
func (s *Server) loadReachabilityEvidence(ctx context.Context, runID uuid.UUID) []splitfiling.PhaseEvidence {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, reachabilitySweepAuditKind)
	if err != nil || len(entries) == 0 {
		return nil
	}
	newest := entries[len(entries)-1]
	var payload PlanReachabilityPayload
	if uerr := json.Unmarshal(newest.Payload, &payload); uerr != nil {
		return nil
	}
	if len(payload.Phases) == 0 {
		return nil
	}
	ev := make([]splitfiling.PhaseEvidence, 0, len(payload.Phases))
	for _, ph := range payload.Phases {
		ev = append(ev, splitfiling.PhaseEvidence{
			Index:         ph.Index,
			Title:         ph.Title,
			DeclaredCount: ph.DeclaredCount,
			DerivedCount:  ph.DerivedCount,
		})
	}
	return ev
}

// loadFiledSplitChildren reads the per-phase work_item_filed resume markers for
// the run (those carrying a split_phase_index) into a map keyed by phase index.
// These markers ARE the durable per-ordinal filing record the hook consults on
// resume — appended immediately after each child files and treated as a hard
// prerequisite (writeSplitChildFiledMarker's failure aborts filing), so a
// re-approval skips every durably-recorded ordinal and re-files none of them.
// applyAndFileWorkItem itself provides NO durable idempotency (it calls
// provider.File unconditionally), so the marker is the sole resume record: a
// read error here fails open to an empty map and a resume then re-files (the
// same at-least-once residual refinement.ExecuteFiling documents), and the
// completion-marker dedup at the top of the hook prevents re-entry once the full
// set is recorded.
func (s *Server) loadFiledSplitChildren(ctx context.Context, runID uuid.UUID) map[int]filedChild {
	filed := map[int]filedChild{}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, categoryWorkItemFiled)
	if err != nil {
		return filed
	}
	for _, e := range entries {
		var m splitChildFiledMarker
		if json.Unmarshal(e.Payload, &m) != nil || m.SplitPhaseIndex == nil {
			continue
		}
		filed[*m.SplitPhaseIndex] = filedChild{number: m.CreatedNumber, url: m.CreatedURL}
	}
	return filed
}

// writeSplitChildFiledMarker records the durable per-phase resume marker
// immediately after a child is filed, so a partial-failure re-approval skips it.
// It returns the append error (also WARN-logged): because this marker is the
// hook's sole durable per-ordinal record, its append is a HARD prerequisite —
// the caller aborts the run (no completion marker) on failure rather than filing
// further children on a durable state it can no longer resume, mirroring
// refinement.ExecuteFiling's abort-on-RecordFiledItem-failure discipline.
func (s *Server) writeSplitChildFiledMarker(ctx context.Context, runRow *run.Run, spec splitfiling.ChildSpec, created *workmgmt.CreatedItem) error {
	idx := spec.PhaseIndex
	payload, _ := json.Marshal(map[string]any{
		"type":              "feature",
		"title":             spec.Title,
		"provider":          created.Provider,
		"created_url":       created.URL,
		"created_number":    created.Number,
		"split_phase_index": idx,
		"is_contract":       spec.IsContract,
	})
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		Timestamp: time.Now().UTC(),
		Category:  categoryWorkItemFiled,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.logSplitFilingWarn(ctx, runRow.ID, "append split-child resume marker failed; aborting split filing for resume", err.Error())
		return err
	}
	return nil
}

// writeSplitChildrenFiledAudit emits the ONE completion marker after every child
// is filed. Best-effort: an append failure logs but never unwinds the approval.
func (s *Server) writeSplitChildrenFiledAudit(ctx context.Context, runRow *run.Run, classification splitfiling.ContractClassification, children []splitFilingChild, contractChildNumber int, draft *splitfiling.CapExceptionDraft) {
	payload := splitChildrenFiledPayload{
		ContractClassification: string(classification),
		Children:               children,
		ContractChildNumber:    contractChildNumber,
		DeferralIssue:          splitfiling.DeferralIssue,
	}
	if draft != nil {
		payload.CapException = &splitCapExceptionDraft{SpecDiff: draft.SpecDiff, PRBody: draft.PRBody}
	}
	marshaled, _ := json.Marshal(payload)
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		Timestamp: time.Now().UTC(),
		Category:  splitChildrenFiledCategory,
		ActorKind: &systemKind,
		Payload:   marshaled,
	}); err != nil {
		s.logSplitFilingWarn(ctx, runRow.ID, "append split_children_filed audit failed", err.Error())
	}
}

// postSplitParentComment posts the best-effort parent acceptance-carrier comment
// naming the contract child and stating plainly that the parent is closed when
// that child lands — automated by follow-up #2062 (E50.6) once it ships. It does
// NOT claim the parent auto-closes now. It also mirrors the landability
// non-guarantee (#2412): every filed split's parent comment must plainly state
// independent landability is unverified, so the same caveat carried on each child
// body rides the parent comment too. Best-effort: a nil client, an absent
// installation, or a post error logs and returns.
func (s *Server) postSplitParentComment(ctx context.Context, runRow *run.Run, owner, name string, parentIssue, contractChildNumber int) {
	if s.cfg.GitHub == nil || runRow.InstallationID == nil {
		return
	}
	body := fmt.Sprintf(
		"Fishhawk filed the phased children of this issue's approved split proposal. "+
			"The contract-phase child #%d is the acceptance carrier: it carries this issue's acceptance criteria.\n\n"+
			"Close this parent (#%d) when contract child #%d lands. That parent-close is not automated by this change "+
			"(a `Closes #%d` line in a child issue body would be functionless — GitHub auto-closes only from a PR/commit "+
			"and only the enclosing issue); it will be automated by follow-up #%d (E50.6) once it ships.\n\n%s",
		contractChildNumber, parentIssue, contractChildNumber, parentIssue, splitfiling.DeferralIssue, splitfiling.LandabilityCaveat)
	repo := forge.RepoRef{Owner: owner, Name: name}
	if _, err := s.cfg.GitHub.CreateIssueComment(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, parentIssue, body); err != nil {
		s.logSplitFilingWarn(ctx, runRow.ID, "post parent acceptance-carrier comment failed", err.Error())
	}
}

// refuseSplitFilingOverCapPhase records the ONE split_filing_refused marker and
// posts the parent refusal comment when a split_proposal has an over-cap phase
// (#2412), filing ZERO children. It dedups symmetrically with the priorCompletion
// gate: a prior split_filing_refused entry for the run makes the whole branch a
// no-op, so a re-approval of the still-over-cap proposal neither duplicates the
// marker nor re-comments. The marker append is a HARD prerequisite exactly like
// writeSplitChildFiledMarker — on append failure it logs and returns WITHOUT
// posting the parent comment, so a refusal comment never claims a refusal the
// audit does not record.
func (s *Server) refuseSplitFilingOverCapPhase(ctx context.Context, runRow *run.Run, owner, name string, parentIssue, capFiles int, violations []splitfiling.PhaseCapViolation) {
	// Dedup: a prior refusal for this run means the branch already ran → no-op.
	prior, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, splitFilingRefusedCategory)
	if err != nil {
		s.logSplitFilingWarn(ctx, runRow.ID, "list split_filing_refused failed", err.Error())
		return
	}
	if len(prior) > 0 {
		return // already refused — idempotent no-op
	}

	phases := make([]splitFilingRefusedPhase, 0, len(violations))
	lines := make([]string, 0, len(violations))
	for _, v := range violations {
		phases = append(phases, splitFilingRefusedPhase{Index: v.Index, Title: v.Title, DeclaredCount: v.DeclaredCount})
		lines = append(lines, v.String())
	}
	reason := fmt.Sprintf(
		"refused to file split_proposal children: %d phase(s) declare more files than the implement-stage max_files_changed cap of %d",
		len(violations), capFiles,
	)

	// HARD prerequisite: the refusal marker must persist before the parent comment
	// so no comment claims a refusal the audit does not record.
	payload, _ := json.Marshal(splitFilingRefusedPayload{Reason: reason, Cap: capFiles, Phases: phases})
	systemKind := audit.ActorSystem
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		Timestamp: time.Now().UTC(),
		Category:  splitFilingRefusedCategory,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.logSplitFilingWarn(ctx, runRow.ID, "append split_filing_refused audit failed; not posting refusal comment", aerr.Error())
		return
	}
	s.postSplitRefusalComment(ctx, runRow, owner, name, parentIssue, capFiles, lines)
}

// postSplitRefusalComment posts the best-effort parent comment for a refused
// split filing (#2412): it names each offending phase (its declared count and
// the cap), states plainly that NO children were filed, and gives the remedy —
// re-slice the phases under the cap or declare the change irreducible and
// re-approve. It also mirrors the landability non-guarantee. Best-effort: a nil
// client, an absent installation, or a post error logs and returns.
func (s *Server) postSplitRefusalComment(ctx context.Context, runRow *run.Run, owner, name string, parentIssue, capFiles int, phaseLines []string) {
	if s.cfg.GitHub == nil || runRow.InstallationID == nil {
		return
	}
	body := fmt.Sprintf(
		"Fishhawk did **not** file the phased children of this issue's approved split proposal: %d phase(s) declare more files than the implement-stage max_files_changed cap of %d, so a filed child would itself fail the implement cap.\n\n- %s\n\nNo children were filed. Re-slice the offending phase(s) so each is at or under the cap, or — if the change is genuinely compile-atomic and cannot be phased — declare the plan `irreducible` with a rationale, then re-approve.\n\n%s",
		len(phaseLines), capFiles, strings.Join(phaseLines, "\n- "), splitfiling.LandabilityCaveat,
	)
	repo := forge.RepoRef{Owner: owner, Name: name}
	if _, err := s.cfg.GitHub.CreateIssueComment(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, parentIssue, body); err != nil {
		s.logSplitFilingWarn(ctx, runRow.ID, "post parent split-filing refusal comment failed", err.Error())
	}
}

// logSplitFilingWarn is the shared WARN logger for the best-effort hook, so no
// branch fails the approval silently.
func (s *Server) logSplitFilingWarn(ctx context.Context, runID uuid.UUID, msg, detail string) {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "split filing: "+msg,
		slog.String("run_id", runID.String()),
		slog.String("detail", detail),
	)
}
