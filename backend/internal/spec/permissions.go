package spec

import (
	"fmt"
	"strings"
)

// Declaration-only per-stage permissions (E53.5 / #2228).
//
// This file owns the DECLARATION side of the `permissions:` block: the
// normalization of `permissions.network` into the existing Stage.Egress field
// (the acceptance-criterion-2 seam), the shared write-glob predicate, and the
// semantic validation of `write` and `shell`.
//
// The block is DECLARATION-ONLY: it is validated, audited and surfaced but NOT
// enforced until E51 #2133 — do not rely on it as containment. Enforcement is a
// sibling epic; this slice ships the grammar plus its proof.
//
// WHY permissions.network NORMALIZES INTO Egress. `network` is a SPELLING of
// `egress`, not a parallel key: copying it into Stage.Egress after the typed
// decode means every existing egress consumer (resolveAcceptanceStageSpec ->
// resolveAcceptanceEgressTargetHosts -> the acceptance prompt's
// egress_target_hosts -> egressproxy.BuildAllowlist) keeps reading the SAME
// field, so an acceptance stage can NEVER be routed onto an unenforced path by
// choosing the new spelling. Declaring BOTH egress and permissions.network on
// one stage is a validation ERROR rather than a silent precedence rule, because
// a precedence rule is the quiet way to ship exactly the regression #2228 warns
// about — the enforced allow-list silently differing from what is written.

// The two E53.5 error messages that could otherwise leave a reader believing a
// permission is ENFORCED both carry the non-enforcement disclaimer (operator
// condition 4): both are about WHERE a permission may be declared and invite the
// inference that declaring it does something. A pure syntax error — a malformed
// write glob or an unknown shell posture — does NOT carry it, because a syntax
// complaint implies no containment. The disclaimer clause is inlined into each
// (rather than composed from a shared const) so the constant stays a single
// string literal, which the byte-parity test in both spec_test.go files reads
// verbatim; the two Go modules cannot share a package.

// MsgFmtPermissionsEgressConflict is the rejection for declaring BOTH `egress`
// and `permissions.network` on one stage (E53.5 / #2228). Arguments: workflow
// name, stage id. Byte-identical in cli/internal/spec/validate.go.
const MsgFmtPermissionsEgressConflict = "workflow %q stage %q: declares both `egress` and `permissions.network`; they are two spellings of one allowance, so declare exactly one — there is deliberately no precedence rule. permissions are validated, audited and surfaced but are NOT enforced until E51 (#2133); do not rely on this as containment"

// MsgFmtPermissionsExecutorBinding is the rejection for an `egress` or
// `permissions` declaration on a non-agent (human or delegate) executor at
// major >= 2 (E53.5 / #2228). Arguments: workflow name, stage id, executor
// branch. Backend-only — the CLI does not mirror the executor binding, matching
// how it leaves the deploy executor binding to the backend.
const MsgFmtPermissionsExecutorBinding = "workflow %q stage %q: `egress`/`permissions` is valid only on a stage with an agent executor, not a %s executor; a stage that runs no agent has no such declaration to make. Move the declaration to an agent stage, or drop it. permissions are validated, audited and surfaced but are NOT enforced until E51 (#2133); do not rely on this as containment"

// MsgFmtPermissionsShellUnknown is the rejection for an unknown `shell` posture.
// It is a pure syntax complaint, so it does NOT carry the non-enforcement
// disclaimer (operator condition 4). Arguments: pointer, the bad value, the
// valid set. Backend-only — the schema's enum already catches this for the CLI.
const MsgFmtPermissionsShellUnknown = "%s: unknown shell posture %q; valid postures are %s"

// normalizeStagePermissions folds `permissions.network` into Stage.Egress — the
// acceptance-criterion-2 seam. When both `egress` and `permissions.network` are
// declared on one stage it returns the conflict error rather than choosing a
// precedence. Called from the post-typed-decode pass in parse.go, BEFORE
// Validate, so validation and every downstream reader observe the resolved
// Stage.Egress. Version-agnostic in code — the schema gates `permissions` to
// v2, since no v0/v1 schema accepts the key.
func normalizeStagePermissions(s *Spec) error {
	for wfName, wf := range s.Workflows {
		for i := range wf.Stages {
			st := &wf.Stages[i]
			if st.Permissions == nil || st.Permissions.Network == nil {
				continue
			}
			if st.Egress != nil {
				return &ValidationError{
					Path:    fmt.Sprintf("/workflows/%s/stages/%d/permissions/network", wfName, i),
					Message: fmt.Sprintf(MsgFmtPermissionsEgressConflict, wfName, st.ID),
				}
			}
			st.Egress = st.Permissions.Network
		}
		// wf is a copy of the map value, but Stages is a slice whose backing
		// array is shared, so the &st mutation above lands on the stored spec.
		// The map re-assign is defensive and matches the surrounding style.
		s.Workflows[wfName] = wf
	}
	return nil
}

// WritePredicate returns the shared Predicate the `write` globs validate and
// match through, so the write dialect and match semantics cannot drift from
// applies_to / escalations. It is the LITERAL shared matcher: Predicate{Paths}.
func (p StagePermissions) WritePredicate() Predicate {
	return Predicate{Paths: p.Write}
}

// validateStagePermissions validates a stage's `permissions` block at ptr
// (`/workflows/<name>/stages/<i>/permissions`). It validates `write` THROUGH
// the shared predicate (so a malformed glob is caught by doublestar at
// declaration time), remapping the reported pointer onto `<ptr>/write/<i>`, and
// rejects an unknown `shell` posture naming the valid set. The network<->egress
// conflict is surfaced separately (normalizeStagePermissions), so it is not
// re-checked here.
func validateStagePermissions(ptr string, p *StagePermissions) error {
	if p == nil {
		return nil
	}
	if len(p.Write) > 0 {
		if err := p.WritePredicate().Validate(ptr); err != nil {
			// Predicate.Validate reports at `<ptr>/paths/<i>`; remap that onto
			// this block's `<ptr>/write/<i>` so the pointer names the field the
			// author wrote.
			return &ValidationError{
				Path:    ptr + "/write",
				Message: strings.Replace(err.Error(), ptr+"/paths/", ptr+"/write/", 1),
			}
		}
	}
	if p.Shell != "" && !validShellPosture(p.Shell) {
		return &ValidationError{
			Path:    ptr + "/shell",
			Message: fmt.Sprintf(MsgFmtPermissionsShellUnknown, ptr+"/shell", p.Shell, shellPosturesList()),
		}
	}
	return nil
}

// validShellPosture reports whether s is one of the closed postures.
func validShellPosture(s ShellPosture) bool {
	for _, v := range ValidShellPostures() {
		if v == s {
			return true
		}
	}
	return false
}

// shellPosturesList renders the valid shell postures for an error message.
func shellPosturesList() string {
	postures := ValidShellPostures()
	parts := make([]string, len(postures))
	for i, p := range postures {
		parts[i] = string(p)
	}
	return strings.Join(parts, ", ")
}
