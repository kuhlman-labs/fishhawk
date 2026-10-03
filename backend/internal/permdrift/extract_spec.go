package permdrift

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// The four .fishhawk/workflows.yaml extractors all read the document through
// spec.ParseBytes — never the raw YAML — so workflow-v2's `defaults` /
// `extends` reuse, the `constraints` object form and the
// `permissions.network` spelling are RESOLVED exactly as the product resolves
// them, and every value an extractor reads is the parsed value. Nothing is
// invented for an absent declaration: an undeclared tier, ceiling or posture
// is simply no entry (absence is what the entry's polarity says it is).

// parseSpec parses a workflow spec, mapping empty content (an absent file) to
// a nil spec and nil error.
func parseSpec(content []byte) (*spec.Spec, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}
	s, err := spec.ParseBytes(content)
	if err != nil {
		return nil, fmt.Errorf("workflow spec: %w", err)
	}
	return s, nil
}

// sortedWorkflowNames returns s's workflow names in sorted order.
func sortedWorkflowNames(s *spec.Spec) []string {
	names := make([]string, 0, len(s.Workflows))
	for n := range s.Workflows {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// stagePrefix is the key prefix of one stage.
func stagePrefix(wf string, st spec.Stage) string {
	return "workflows." + wf + ".stages." + st.ID
}

// ExtractSpecForbiddenPaths keys every stage constraint's forbidden_paths
// glob as `workflows.<wf>.stages.<stage>.forbidden_paths[<glob>]`, a presence
// Restriction keyed by the glob VALUE — so reordering the list is no change,
// and removing a glob is a widening.
func ExtractSpecForbiddenPaths(content []byte) (Grants, error) {
	out := Grants{}
	s, err := parseSpec(content)
	if err != nil || s == nil {
		return out, err
	}
	for _, wf := range sortedWorkflowNames(s) {
		for _, st := range s.Workflows[wf].Stages {
			for _, c := range st.Constraints {
				for _, glob := range c.ForbiddenPaths {
					out.Put(Entry{
						Key:      stagePrefix(wf, st) + ".forbidden_paths[" + glob + "]",
						Value:    Present,
						Rank:     PresenceRank,
						Polarity: Restriction,
					})
				}
			}
		}
	}
	return out, nil
}

// ExtractSpecEscalations keys each workflow escalation by its CANONICAL match
// (every predicate list sorted, then JSON-encoded — so reordering a match
// list is no change and two distinct matches never share a key):
//
//   - `workflows.<wf>.escalations[<match>]`: presence Restriction (removing
//     an escalation is a widening);
//   - `...[<match>].max_autonomy`: ranked Restriction on AutonomyLevels —
//     raising the ceiling is a widening, removing it is a widening (an absent
//     ceiling is above high);
//   - `...[<match>].approvals.count`: ranked Restriction whose rank is the
//     NEGATED count, so lowering the count is a widening;
//   - `...[<match>].approvals.member_of[<team>]`,
//     `...[<match>].approvals.min_permission[<perm>]` and
//     `...[<match>].reviewers[<persona>]`: presence Restrictions.
//
// Two escalations with the same canonical match fold into one key; their
// ceilings fold to the LOWEST (the strictest wins, as composition does) and
// their counts to the HIGHEST.
func ExtractSpecEscalations(content []byte) (Grants, error) {
	out := Grants{}
	s, err := parseSpec(content)
	if err != nil || s == nil {
		return out, err
	}
	for _, wf := range sortedWorkflowNames(s) {
		for _, esc := range s.Workflows[wf].Escalations {
			k := "workflows." + wf + ".escalations[" + canonicalMatch(esc.Match) + "]"
			out.Put(Entry{Key: k, Value: Present, Rank: PresenceRank, Polarity: Restriction})
			req := esc.Require
			if req.MaxAutonomy != "" {
				r, err := AutonomyLevels.mustRank(k+".max_autonomy", string(req.MaxAutonomy))
				if err != nil {
					return nil, fmt.Errorf("workflow spec: %w", err)
				}
				putStrictest(out, Entry{Key: k + ".max_autonomy", Value: string(req.MaxAutonomy), Rank: r, Polarity: Restriction})
			}
			if a := req.Approvals; a != nil {
				if a.Count != nil {
					putStrictest(out, Entry{Key: k + ".approvals.count", Value: strconv.Itoa(*a.Count), Rank: -*a.Count, Polarity: Restriction})
				}
				if a.MemberOf != "" {
					out.Put(Entry{Key: k + ".approvals.member_of[" + a.MemberOf + "]", Value: Present, Rank: PresenceRank, Polarity: Restriction})
				}
				if a.MinPermission != "" {
					out.Put(Entry{Key: k + ".approvals.min_permission[" + a.MinPermission + "]", Value: Present, Rank: PresenceRank, Polarity: Restriction})
				}
			}
			for _, p := range req.Reviewers {
				out.Put(Entry{Key: k + ".reviewers[" + p + "]", Value: Present, Rank: PresenceRank, Polarity: Restriction})
			}
		}
	}
	return out, nil
}

// putStrictest records a ranked Restriction, keeping the LOWER rank (the
// stricter limit) when the key is already present.
func putStrictest(out Grants, e Entry) {
	if prev, ok := out[e.Key]; ok && prev.Rank <= e.Rank {
		return
	}
	out.Put(e)
}

// canonicalMatch renders a predicate order-insensitively and injectively:
// every list sorted, then the whole JSON-encoded.
func canonicalMatch(p spec.Predicate) string {
	sorted := func(in []string) []string {
		out := append([]string(nil), in...)
		sort.Strings(out)
		return out
	}
	triggers := make([]string, 0, len(p.Triggers))
	for _, t := range p.Triggers {
		triggers = append(triggers, string(t))
	}
	b, _ := json.Marshal(struct {
		Paths       []string `json:"paths,omitempty"`
		Labels      []string `json:"labels,omitempty"`
		ChangeKinds []string `json:"change_kind,omitempty"`
		Triggers    []string `json:"trigger,omitempty"`
	}{sorted(p.Paths), sorted(p.Labels), sorted(p.ChangeKinds), sorted(triggers)})
	return string(b)
}

// ExtractSpecAutonomy keys a workflow's delegation from the RESOLVED autonomy
// block (spec.ResolveAutonomy over the parsed spec):
//
//   - `workflows.<wf>.autonomy`: the declared workflow tier, a ranked Grant
//     on AutonomyLevels — present only when the workflow declares one;
//   - `workflows.<wf>.actions.<class>`: a presence Grant for every action
//     class the workflow-level block resolves to `mode: auto` (an explicit
//     `actions` override that tightens a tier class is honoured, because the
//     resolved mode is read, not the tier);
//   - per APPROVAL gate, only where its EFFECTIVE mode for a class differs
//     from the workflow level's: `workflows.<wf>.stages.<stage>.gates[<i>].actions.<class>`
//     as a presence Grant when the gate is auto and the workflow is not, and
//     as a presence Restriction when the workflow is auto and the gate is not.
//     Recording only the difference keeps a workflow-level raise from
//     multiplying across every inheriting gate, while removing a gate block
//     that RESTRICTED a class still reads as a widening.
//
// A workflow declaring neither `autonomy` nor `actions` resolves to nothing
// delegated and yields no entries.
func ExtractSpecAutonomy(content []byte) (Grants, error) {
	out := Grants{}
	s, err := parseSpec(content)
	if err != nil || s == nil {
		return out, err
	}
	for _, wfName := range sortedWorkflowNames(s) {
		wf := s.Workflows[wfName]
		prefix := "workflows." + wfName
		if wf.Autonomy != "" {
			r, err := AutonomyLevels.mustRank(prefix+".autonomy", string(wf.Autonomy))
			if err != nil {
				return nil, fmt.Errorf("workflow spec: %w", err)
			}
			out.Put(Entry{Key: prefix + ".autonomy", Value: string(wf.Autonomy), Rank: r, Polarity: Grant})
		}
		wfAuto := autoClasses(spec.ResolveAutonomy(&wf, nil))
		for class := range wfAuto {
			out.Put(Entry{Key: prefix + ".actions." + class, Value: string(spec.ModeAuto), Rank: PresenceRank, Polarity: Grant})
		}
		for _, st := range wf.Stages {
			for gi := range st.Gates {
				g := &st.Gates[gi]
				if g.Type != spec.GateTypeApproval {
					continue
				}
				gateAuto := autoClasses(spec.ResolveAutonomy(&wf, g))
				gp := stagePrefix(wfName, st) + ".gates[" + strconv.Itoa(gi) + "].actions."
				for class := range gateAuto {
					if !wfAuto[class] {
						out.Put(Entry{Key: gp + class, Value: string(spec.ModeAuto), Rank: PresenceRank, Polarity: Grant})
					}
				}
				for class := range wfAuto {
					if !gateAuto[class] {
						out.Put(Entry{Key: gp + class, Value: "not auto (gate override)", Rank: PresenceRank, Polarity: Restriction})
					}
				}
			}
		}
	}
	return out, nil
}

// autoClasses returns the classes a resolved matrix delegates (`mode: auto`).
// A nil matrix (no block declared) delegates nothing.
func autoClasses(m *spec.ResolvedMatrix) map[string]bool {
	out := map[string]bool{}
	if m == nil {
		return out
	}
	for _, a := range m.Actions {
		if a.Mode == spec.ModeAuto {
			out[a.Action] = true
		}
	}
	return out
}

// ExtractSpecStagePermissions keys each stage's declared permissions from the
// parsed spec (Stage.Egress already folds the `permissions.network`
// spelling):
//
//   - `workflows.<wf>.stages.<stage>.egress[<host>]`: presence Grant;
//   - `...permissions.write[<glob>]`: presence Grant;
//   - `...permissions.shell`: ranked Grant on ShellLevels, present only when
//     declared.
func ExtractSpecStagePermissions(content []byte) (Grants, error) {
	out := Grants{}
	s, err := parseSpec(content)
	if err != nil || s == nil {
		return out, err
	}
	for _, wf := range sortedWorkflowNames(s) {
		for _, st := range s.Workflows[wf].Stages {
			p := stagePrefix(wf, st)
			if st.Egress != nil {
				for _, h := range st.Egress.TargetHosts {
					out.Put(Entry{Key: p + ".egress[" + h + "]", Value: Present, Rank: PresenceRank, Polarity: Grant})
				}
			}
			if st.Permissions == nil {
				continue
			}
			for _, w := range st.Permissions.Write {
				out.Put(Entry{Key: p + ".permissions.write[" + w + "]", Value: Present, Rank: PresenceRank, Polarity: Grant})
			}
			if sh := st.Permissions.Shell; sh != "" {
				r, err := ShellLevels.mustRank(p+".permissions.shell", string(sh))
				if err != nil {
					return nil, fmt.Errorf("workflow spec: %w", err)
				}
				out.Put(Entry{Key: p + ".permissions.shell", Value: string(sh), Rank: r, Polarity: Grant})
			}
		}
	}
	return out, nil
}
