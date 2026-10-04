// Package delegationview projects a parsed workflow spec onto the
// per-workflow DELEGATION READ (E76.1 / #3747): for each workflow, what may
// the operator agent decide on its own here, and what does an escalation
// take back?
//
// It answers that question WITHOUT a run. The run-side delegation block
// (GET /v0/runs/{id}, buildDelegationPayload) evaluates conditions against a
// live run; this package is a pure projection of the DECLARATION, so a
// reviewer can read a repository's delegation posture from the spec at a ref
// before any work starts.
//
// THE PROJECTION IS DECLARATIVE, NOT AN EVALUATION. Two consequences that
// the field names carry deliberately:
//
//   - Matrix is the WORKFLOW-level resolved matrix — spec.ResolveAutonomy
//     with a nil gate, the same block delegation.Evaluate starts from before
//     a gate-level override wins wholesale. A workflow whose gates declare
//     their own autonomy blocks shows the workflow-level matrix here.
//   - CeilingMatrix is the matrix an escalation's `max_autonomy` WOULD
//     produce, not one in force. Whether an escalation fires depends on an
//     approved plan's scope.files, which does not exist before a run, so this
//     package never asks. It echoes each escalation's match criteria and the
//     ceiling's effect side by side and leaves the firing question to the
//     gate.
//
// DETERMINISM IS LOAD-BEARING because every entry carries a content hash a
// later re-confirmation (ADR-083) can bind to. Two properties hold it:
// Project walks the spec's workflow map in SORTED id order (a Go map range is
// deliberately randomized, so ranging it would make the view — and its hash —
// non-deterministic), and every view type is a plain struct with NO map
// field, because encoding/json marshals struct fields in DECLARATION order
// and slice elements in order but map keys in sorted-key order only for
// string keys (https://pkg.go.dev/encoding/json#Marshal). Plain structs plus
// ordered slices make the marshalled bytes a function of the content alone.
//
// Long-form contract: backend/internal/delegationview/README.md.
package delegationview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// View is the whole delegation read for one repository at one spec source.
//
// The five fields above ContentHash are VOLATILE — they describe WHERE the
// spec was read, not WHAT it delegates — and none of them is an input to any
// hash this package computes. That is what makes the hash identical for
// identical delegation whatever ref it was read at (see hashWorkflows).
type View struct {
	Repo        string `json:"repo"`
	Source      string `json:"source"`
	Ref         string `json:"ref,omitempty"`
	WorkflowSHA string `json:"workflow_sha,omitempty"`
	SpecVersion string `json:"spec_version,omitempty"`
	SchemaMajor int    `json:"schema_major"`
	// ContentHash is sha256 over the ordered workflow entries ONLY.
	ContentHash string               `json:"content_hash"`
	Workflows   []WorkflowDelegation `json:"workflows"`
}

// WorkflowDelegation is one workflow's delegation posture.
type WorkflowDelegation struct {
	ID string `json:"id"`
	// Autonomy is the declared tier shorthand, empty when the workflow
	// declares only `actions` — or declares no autonomy block at all, in
	// which case Matrix is EMPTY: the fail-closed reading, nothing delegated.
	Autonomy      string       `json:"autonomy,omitempty"`
	Matrix        []Action     `json:"matrix"`
	MustPageHuman []string     `json:"must_page_human,omitempty"`
	ModelPolicy   *ModelPolicy `json:"model_policy,omitempty"`
	Escalations   []Escalation `json:"escalations,omitempty"`
	// ContentHash is sha256 over this entry with ContentHash itself zeroed.
	ContentHash string `json:"content_hash"`
}

// Action is one resolved action class with its provenance — a wire mirror of
// spec.ResolvedAction, kept distinct so a field change in the resolver cannot
// silently reshape this API surface.
type Action struct {
	Action      string `json:"action"`
	Mode        string `json:"mode"`
	Condition   string `json:"condition,omitempty"`
	MinSeverity string `json:"min_severity,omitempty"`
	Source      string `json:"source"`
}

// ModelPolicy mirrors spec.ModelPolicy on the wire.
type ModelPolicy struct {
	Strategy string               `json:"strategy,omitempty"`
	Defaults *ModelPolicyDefaults `json:"defaults,omitempty"`
	Allowed  []string             `json:"allowed,omitempty"`
}

// ModelPolicyDefaults mirrors spec.ModelPolicyDefaults on the wire.
type ModelPolicyDefaults struct {
	Plan      string `json:"plan,omitempty"`
	Implement string `json:"implement,omitempty"`
	Review    string `json:"review,omitempty"`
}

// Escalation is one declared escalation: what it matches, what it raises, and
// the matrix its autonomy ceiling would produce.
type Escalation struct {
	Match       EscalationMatch     `json:"match"`
	MaxAutonomy string              `json:"max_autonomy,omitempty"`
	Approvals   *EscalatedApprovals `json:"approvals,omitempty"`
	// CeilingMatrix is the workflow matrix clamped by MaxAutonomy — what the
	// ceiling WOULD produce for a change this escalation matches. Empty when
	// the escalation declares no ceiling, or when the workflow resolves no
	// matrix for one to clamp.
	CeilingMatrix []Action `json:"ceiling_matrix,omitempty"`
}

// EscalationMatch echoes the escalation's shared path predicate.
type EscalationMatch struct {
	Paths      []string `json:"paths,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	ChangeKind []string `json:"change_kind,omitempty"`
	Trigger    []string `json:"trigger,omitempty"`
}

// EscalatedApprovals echoes the escalation's raised approval requirement.
// Count is a pointer so an undeclared count stays distinguishable from a
// declared one, exactly as spec.EscalatedApprovals keeps it.
type EscalatedApprovals struct {
	Count         *int   `json:"count,omitempty"`
	MemberOf      string `json:"member_of,omitempty"`
	MinPermission string `json:"min_permission,omitempty"`
}

// Project projects every workflow a parsed spec declares onto its delegation
// posture, in SORTED workflow-id order, with each entry's ContentHash
// stamped. A nil spec projects to an empty slice — never nil, so the JSON
// surface is a `[]` rather than a `null`.
func Project(parsed *spec.Spec) []WorkflowDelegation {
	out := []WorkflowDelegation{}
	if parsed == nil {
		return out
	}
	ids := make([]string, 0, len(parsed.Workflows))
	for id := range parsed.Workflows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, projectWorkflow(id, parsed.Workflows[id]))
	}
	return out
}

// projectWorkflow projects one workflow. A workflow declaring NO autonomy
// block resolves to nil and emits an empty matrix with an empty tier — the
// fail-closed reading (nothing delegated), never a nil dereference.
func projectWorkflow(id string, wf spec.Workflow) WorkflowDelegation {
	out := WorkflowDelegation{ID: id, Matrix: []Action{}}
	resolved := spec.ResolveAutonomy(&wf, nil)
	if resolved != nil {
		out.Autonomy = string(resolved.Tier)
		out.Matrix = actionsFrom(resolved.Actions)
		out.MustPageHuman = resolved.PageHumanOn
		out.ModelPolicy = modelPolicyFrom(resolved.ModelPolicy)
	}
	for i := range wf.Escalations {
		out.Escalations = append(out.Escalations, projectEscalation(wf.Escalations[i], resolved))
	}
	out.ContentHash = hashWorkflow(out)
	return out
}

// projectEscalation echoes one escalation and computes its ceiling matrix.
//
// The clamp is spec.ClampResolvedMatrix — the SAME function the run-side
// delegation seam applies, deliberately not a second implementation of the
// ceiling rule, so a change to how a ceiling composes cannot leave this read
// reporting a laxer matrix than the one that will actually be enforced.
func projectEscalation(e spec.Escalation, resolved *spec.ResolvedMatrix) Escalation {
	out := Escalation{
		Match: EscalationMatch{
			Paths:      e.Match.Paths,
			Labels:     e.Match.Labels,
			ChangeKind: e.Match.ChangeKinds,
			Trigger:    triggersFrom(e.Match.Triggers),
		},
		MaxAutonomy: string(e.Require.MaxAutonomy),
	}
	if a := e.Require.Approvals; a != nil {
		out.Approvals = &EscalatedApprovals{
			Count: a.Count, MemberOf: a.MemberOf, MinPermission: a.MinPermission,
		}
	}
	if e.Require.MaxAutonomy != "" && resolved != nil {
		out.CeilingMatrix = actionsFrom(spec.ClampResolvedMatrix(resolved, e.Require.MaxAutonomy).Actions)
	}
	return out
}

func actionsFrom(in []spec.ResolvedAction) []Action {
	out := make([]Action, 0, len(in))
	for _, a := range in {
		out = append(out, Action{
			Action:      a.Action,
			Mode:        string(a.Mode),
			Condition:   string(a.Condition),
			MinSeverity: a.MinSeverity,
			Source:      string(a.Source),
		})
	}
	return out
}

func triggersFrom(in []spec.TriggerForm) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, t := range in {
		out = append(out, string(t))
	}
	return out
}

func modelPolicyFrom(mp *spec.ModelPolicy) *ModelPolicy {
	if mp == nil {
		return nil
	}
	out := &ModelPolicy{Strategy: string(mp.Strategy), Allowed: mp.Allowed}
	if mp.Defaults != nil {
		out.Defaults = &ModelPolicyDefaults{
			Plan: mp.Defaults.Plan, Implement: mp.Defaults.Implement, Review: mp.Defaults.Review,
		}
	}
	return out
}

// hashWorkflow is one workflow entry's content hash: sha256 over the entry
// marshalled with ContentHash ZEROED, so the field the hash lands in is never
// an input to it. The entry type carries no volatile field (repo, ref,
// workflow_sha, source all live on View), so the hash is a function of the
// declared delegation alone.
func hashWorkflow(w WorkflowDelegation) string {
	w.ContentHash = ""
	return hashOf(w)
}

// HashWorkflows is the view-level content hash: sha256 over the ordered slice
// of already-hashed workflow entries. It is EXPORTED because the handler
// recomputes it after an optional single-workflow filter narrows the set —
// the per-workflow hashes are stamped before the filter (so a filtered read
// reports the same per-workflow hash an unfiltered one does) while the
// view-level hash reflects the retained set.
//
// Like hashWorkflow it never sees View's volatile fields: reading the SAME
// delegation at two different refs, or through the two different sources,
// yields the SAME hash.
func HashWorkflows(ws []WorkflowDelegation) string { return hashOf(ws) }

// HashMatrix is the content hash of one RESOLVED action matrix, computed
// through the SAME wire mirror (Action) and the same sha256 this read uses,
// so a delegation shadow stamp (E82.1 / #3778) hashing a run's clamped matrix
// yields the digest a matrix of identical content would carry here. The
// mirror is a plain struct and the slice is ordered, so the bytes — and the
// digest — are a function of the content alone.
//
// A nil or empty matrix hashes the empty Action slice (a stable, non-empty
// digest), so "no matrix governs the run" is its own comparable stratum
// rather than an empty string indistinguishable from a hashing failure.
func HashMatrix(actions []spec.ResolvedAction) string { return hashOf(actionsFrom(actions)) }

// hashOf marshals v and returns hex(sha256(bytes)). A marshalling failure is
// impossible for these types (no channel, func or NaN reachable from them),
// and a hash is not a place to surface one, so it degrades to the empty
// string rather than widening every caller's signature.
func hashOf(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
