// Package splitfiling holds the pure logic behind on-approval child filing for
// an approved plan that carries a split_proposal (#2057, E50.5). It is
// deliberately dependency-light — it imports only backend/internal/plan for the
// SplitProposal shape and owns neutral input/output types (PhaseEvidence,
// ChildSpec, ContractClassification, CapExceptionDraft) so the server approval
// hook (backend/internal/server/split_filing.go, a sibling slice) can drive it
// without this package taking a server dependency.
//
// Three pure operations:
//
//   - BuildChildSpecs turns a plan.SplitProposal into an ordered []ChildSpec —
//     one per phase, each carrying a symbol-set scope statement (prose, never a
//     stale file list), 0-based depends_on edges, parent+design issue references,
//     and a contract flag on the terminal phase. The contract child additionally
//     carries the parent's acceptance criteria and, by construction, NO
//     "Closes #<parent>" line (a Closes line in a child ISSUE body is functionless
//     — GitHub auto-closes only from a PR/commit and only the enclosing issue; the
//     live parent-close mechanism is deferred to follow-up #2062).
//   - Classify (classify.go) decides the contract phase is delete-only by default
//     and governed-exception only when reachability evidence proves an atomic
//     rename overflows the resolved implement cap.
//   - DraftCapException (classify.go) renders the in-memory-only cap-exception
//     spec diff + PR body for the governed-exception case — never written to disk.
package splitfiling

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// designIssueRef and deferralIssueRef name the issues every child body and the
// cap-exception draft reference: #2008 is the E50 design issue the split-proposal
// work descends from, and #2062 (E50.6) is the deferred close-parent watcher that
// will close the parent once the contract child lands. They are exported so the
// server hook and its tests can assert on the exact numbers.
const (
	// DesignIssue is the E50 design issue every filed child body references.
	DesignIssue = 2008
	// DeferralIssue is the E50.6 close-parent-watcher follow-up (#2062) that the
	// contract child and the parent acceptance-carrier comment defer parent
	// closure to. This slice ships no close-parent watcher.
	DeferralIssue = 2062
)

// BuildInput bundles the neutral inputs BuildChildSpecs needs: the approved
// plan's split proposal, the parent (run) issue number every child references,
// and the parent's acceptance criteria the terminal contract child carries.
// DesignIssue defaults to the package DesignIssue constant when zero.
type BuildInput struct {
	Proposal         plan.SplitProposal
	ParentIssue      int
	DesignIssue      int
	ParentAcceptance []string
}

// ChildSpec is one conventions-neutral child issue the server hook files. It
// carries the fields the hook maps onto a refinement.ChildDraft (Title→Summary,
// Proposal, DoneMeans, AcceptanceCriteria, DependsOn) plus the classification
// metadata (PhaseIndex, IsContract) and issue references the hook renders into
// the body and depends_on edges.
type ChildSpec struct {
	// PhaseIndex is the child's 0-based phase index within the split proposal.
	PhaseIndex int
	// Title is the phase title, used as the child issue summary line.
	Title string
	// ScopeStatement is the symbol-set scope prose synthesized from the phase's
	// scope_hint (never a stale file list). It is also embedded in Proposal.
	ScopeStatement string
	// Proposal is the child issue body prose. It references the parent (run)
	// issue and the design issue and, for the contract child, states the deferral
	// to #2062 — but NEVER contains a "Closes #<parent>" line.
	Proposal string
	// DoneMeans is the child's done-means body section.
	DoneMeans string
	// DependsOn holds the 0-based phase indices this phase depends on, copied
	// verbatim from the split phase. The server hook resolves these to sibling
	// #N at filing time in wave order.
	DependsOn []int
	// ParentIssue is the parent (run) issue number every child references.
	ParentIssue int
	// DesignIssue is the design issue number every child body references (#2008).
	DesignIssue int
	// IsContract marks the terminal (last) phase — the contract child.
	IsContract bool
	// AcceptanceCriteria carries the parent's acceptance criteria, populated ONLY
	// on the contract child (the acceptance carrier). Non-contract children leave
	// it nil.
	AcceptanceCriteria []string
}

// BuildChildSpecs walks the split proposal's phases in order and returns one
// ChildSpec per phase (empty when the proposal has no phases). The terminal
// phase is flagged IsContract and carries the parent's acceptance criteria; no
// child carries a "Closes #<parent>" line.
func BuildChildSpecs(in BuildInput) []ChildSpec {
	design := in.DesignIssue
	if design == 0 {
		design = DesignIssue
	}
	phases := in.Proposal.Phases
	specs := make([]ChildSpec, 0, len(phases))
	last := len(phases) - 1
	for i, ph := range phases {
		isContract := i == last
		spec := ChildSpec{
			PhaseIndex:     i,
			Title:          ph.Title,
			ScopeStatement: scopeStatement(ph),
			DependsOn:      append([]int(nil), ph.DependsOn...),
			ParentIssue:    in.ParentIssue,
			DesignIssue:    design,
			IsContract:     isContract,
		}
		spec.Proposal = buildProposal(spec, isContract)
		spec.DoneMeans = buildDoneMeans(isContract)
		if isContract {
			spec.AcceptanceCriteria = append([]string(nil), in.ParentAcceptance...)
		}
		specs = append(specs, spec)
	}
	return specs
}

// scopeStatement synthesizes the child's symbol-set scope prose from the phase's
// scope_hint. When the hint is present it is used verbatim (the planner authored
// it as a symbol-set statement). When absent it falls back to prose naming the
// affected packages derived from the phase's declared files — never the raw file
// list itself, which goes stale as the migration proceeds.
func scopeStatement(ph plan.SplitPhase) string {
	if hint := strings.TrimSpace(ph.ScopeHint); hint != "" {
		return "Symbol-set scope: " + hint
	}
	if pkgs := uniquePackages(ph.Scope); len(pkgs) > 0 {
		return "Symbol-set scope: migrate the consumers within " + strings.Join(pkgs, ", ") + "."
	}
	return "Symbol-set scope: migrate the consumers declared for this phase."
}

// uniquePackages returns the sorted unique directory of each declared file in
// the phase scope — the package-level projection of the file list.
func uniquePackages(scope *plan.Scope) []string {
	if scope == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, f := range scope.Files {
		dir := path.Dir(strings.TrimSpace(f.Path))
		if dir == "" || dir == "." {
			continue
		}
		seen[dir] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for d := range seen {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// buildProposal renders the child body prose. It references the parent (run)
// issue and the design issue and embeds the symbol-set scope statement. The
// contract child states the parent-close deferral to #2062 WITHOUT a "Closes #"
// line; a non-contract child states plainly that it references but does not close
// the parent.
func buildProposal(c ChildSpec, isContract bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Phase %d of the approved split proposal for the over-cap change tracked on #%d", c.PhaseIndex+1, c.ParentIssue)
	if c.DesignIssue > 0 {
		fmt.Fprintf(&b, " (design: #%d)", c.DesignIssue)
	}
	b.WriteString(".\n\n")
	b.WriteString(c.ScopeStatement)
	b.WriteString("\n\n")
	if isContract {
		fmt.Fprintf(&b, "This is the terminal (contract) phase and the acceptance carrier for parent #%d: it carries the parent's acceptance criteria. It does NOT close the parent — GitHub auto-closes an issue only from a PR/commit and only the enclosing issue, so a close line here would be functionless. The parent is closed when this contract child lands, automated by follow-up #%d (E50.6) once it ships.", c.ParentIssue, DeferralIssue)
	} else {
		fmt.Fprintf(&b, "This is a transitional phase of the split for #%d; it references but does not close the parent.", c.ParentIssue)
	}
	return b.String()
}

// buildDoneMeans renders the child done-means section.
func buildDoneMeans(isContract bool) string {
	if isContract {
		return "The contract phase deletes the transitional names and the intermediate compiles; the parent's acceptance criteria (carried below) are satisfied."
	}
	return "The phase's symbol-set scope is migrated and the resulting tree compiles as an at-or-under-cap intermediate."
}
