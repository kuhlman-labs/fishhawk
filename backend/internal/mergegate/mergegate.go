// Package mergegate reconciles the status check Fishhawk PUBLISHES
// against the branch protection the forge actually enforces (#3161).
//
// Fishhawk publishes a `fishhawk_audit_complete` Check Run on every run's
// pull request. Whether that check GATES the merge is a property of the
// repository's protection configuration, not of Fishhawk — and until this
// package existed nothing read the forge to find out, so several operator
// surfaces asserted the check was "required" with no evidence behind the
// word.
//
// Reconcile answers one question — does <check> gate merges on <branch>? —
// and answers it FAIL-CLOSED: the only way to get `not_required` is a
// fully authoritative evaluation of BOTH protection surfaces that found
// nothing. Every degrade (a rulesets endpoint that 404s, a ref_name token
// the v0 matcher cannot evaluate, a 403 from a missing `administration:
// read` scope, any transport failure) yields `unknown` with a naming
// Reason. This mirrors the #2497 / #2506 vacuous-green discipline that
// server/required_checks_capture.go documents: an unqueried surface must
// never read as "this repo requires nothing".
//
// The report is REPORTING ONLY. It does not gate a run, a merge or an
// exit code, and it is a point-in-time read — GitHub remains the
// merge-time authority. See README.md for the long-form contract.
package mergegate

import (
	"context"
	"errors"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// ProtectionAPI is the narrow branch-protection + ruleset slice of the
// forge client this package needs. It names the same two methods as
// webhook.ProtectionAPI and is satisfied by *githubclient.Client
// directly; it is declared here rather than imported so a test can
// substitute an in-process fake with no httptest server and no webhook
// dependency.
type ProtectionAPI interface {
	GetBranchProtection(ctx context.Context, scope forge.CredentialScope,
		repo forge.RepoRef, branch string) (*forge.BranchProtection, error)
	ListRulesetRequiredChecksForDefault(ctx context.Context, scope forge.CredentialScope,
		repo forge.RepoRef, branch, defaultBranch string) ([]forge.RulesetRequiredCheck, bool, error)
}

// Status is the reconciled verdict for one check on one branch.
type Status string

// The three reconciled states. There is no fourth: anything that is not
// a positive finding and not a fully authoritative absence is Unknown.
const (
	// StatusRequired means at least one protection source was observed
	// requiring the check. A positive finding, so it does not depend on
	// the evaluation being exhaustive.
	StatusRequired Status = "required"
	// StatusNotRequired means BOTH surfaces answered definitively and
	// neither requires the check. Reachable ONLY on an authoritative
	// evaluation — this is the fail-closed invariant.
	StatusNotRequired Status = "not_required"
	// StatusUnknown means the evaluation could not settle the question.
	// Reason names why.
	StatusUnknown Status = "unknown"
)

// Reason codes for a non-settled or partially-settled evaluation. Empty
// when the evaluation was authoritative and found the check required or
// definitively absent.
const (
	// ReasonRulesetsUnqueryable — the rulesets endpoint answered 404.
	// Some self-hosted GHES versions do not expose it, so the surface
	// was never read and its silence is not evidence.
	ReasonRulesetsUnqueryable = "rulesets_unqueryable"
	// ReasonNonAuthoritative — an active branch ruleset carried a
	// ref_name include token the v0 matcher cannot evaluate (an fnmatch
	// glob, or an unknown ~TOKEN), so a requirement may be hidden
	// behind a condition that was not evaluated.
	ReasonNonAuthoritative = "non_authoritative"
	// ReasonScopeMissing — a 403 from either surface: the App
	// installation lacks `administration: read` (ADR-017 / #252).
	ReasonScopeMissing = "administration_read_missing"
	// ReasonTransportError — any other failure reading either surface.
	ReasonTransportError = "transport_error"
)

// Source identities. A ruleset source's Identity is "ruleset:<id>".
const (
	// SourceBranchProtection is the classic branch-protection source.
	SourceBranchProtection = "branch_protection"
	sourceRulesetPrefix    = "ruleset:"
)

// Source is one protection surface that requires the probed check.
//
// Each source enforces INDEPENDENTLY, so bypass is carried PER SOURCE
// and never summed across them (#3161): summing inverts the logic — a
// merger has to bypass EVERY requiring source, not any one of them.
type Source struct {
	// Identity is "branch_protection" or "ruleset:<id>".
	Identity string
	// Classic is true for the branch_protection source.
	Classic bool
	// BypassEntries is the number of entries in this ruleset's
	// `bypass_actors` array. Always 0 for the classic source, which has
	// no such list — classic exemption is carried by EnforceAdmins and
	// must NOT be coerced into a count of 1.
	//
	// An entry is a role, team, app or integration, NOT a person: one
	// entry may cover many people or none. Render it as "N bypass
	// entries (roles, teams or apps), each of which may cover multiple
	// people" — never as "bypassable by N actors".
	BypassEntries int
	// EnforceAdmins mirrors classic protection's enforce_admins.enabled
	// and is meaningful only when Classic is true. FALSE means
	// repository admins are exempt from this source, which is its own
	// named condition ("repository admins are exempt") and not a count.
	EnforceAdmins bool
	// Bypassable is whether THIS source alone can be bypassed: for a
	// ruleset, BypassEntries > 0; for classic, !EnforceAdmins.
	Bypassable bool
}

// Reconciliation is the answer to "does <Check> gate merges on this
// branch?".
type Reconciliation struct {
	// Status is the verdict. See the Status constants.
	Status Status
	// Check is the context name that was probed.
	Check string
	// RequiredContexts is the deduped union of every context the
	// evaluated sources require, in observation order — classic first,
	// then rulesets. Useful for showing the operator what IS required
	// when the probed check is not.
	RequiredContexts []string
	// Sources are the surfaces that require Check, each with its own
	// bypass detail. Empty unless Status is StatusRequired.
	Sources []Source
	// Bypassable is the conjunction over Sources: true only when EVERY
	// requiring source is individually bypassable. False when Sources
	// is empty.
	//
	// It is a complete answer only when Authoritative is true. When
	// Authoritative is false a source that was not evaluated may still
	// enforce, so a true here means "bypassable through every source
	// that could be evaluated" — the over-warning direction, which is
	// the safe one for a report whose job is to flag a weak gate.
	Bypassable bool
	// Authoritative is true only when BOTH surfaces answered
	// definitively for this branch.
	Authoritative bool
	// Reason is a machine code naming why the evaluation was not fully
	// authoritative, or empty. Non-empty whenever Status is
	// StatusUnknown; it may ALSO be non-empty alongside StatusRequired,
	// when a positive finding landed but the evaluation was still
	// incomplete.
	Reason string
	// Detail is a human sentence for Reason. Empty iff Reason is empty.
	Detail string
	// Remediation is the operator's next step. Empty when Status is
	// StatusRequired and the gate is not bypassable — nothing to do.
	Remediation string
}

// Reconcile reads both protection surfaces for `branch` and reports
// whether `wantContext` gates merges on it.
//
// `defaultBranch` is the repo's REAL default branch, used to evaluate
// `~DEFAULT_BRANCH` ruleset conditions — never guess "main".
//
// The returned error is reserved for caller mistakes (a nil api, an
// empty branch or check). Every FORGE failure resolves to a
// StatusUnknown Reconciliation with a nil error, so a caller that
// forgets to inspect Status still cannot render a false "not required".
func Reconcile(ctx context.Context, api ProtectionAPI, scope forge.CredentialScope,
	repo forge.RepoRef, branch, defaultBranch, wantContext string) (Reconciliation, error) {
	if api == nil {
		return Reconciliation{}, errors.New("mergegate: nil ProtectionAPI")
	}
	if branch == "" {
		return Reconciliation{}, errors.New("mergegate: branch is required")
	}
	if wantContext == "" {
		return Reconciliation{}, errors.New("mergegate: check context is required")
	}

	out := Reconciliation{Check: wantContext}

	seen := make(map[string]struct{})
	addContext := func(c string) {
		if _, dup := seen[c]; dup {
			return
		}
		seen[c] = struct{}{}
		out.RequiredContexts = append(out.RequiredContexts, c)
	}

	// --- classic branch protection ---------------------------------
	classic, classicErr := api.GetBranchProtection(ctx, scope, repo, branch)
	switch {
	case classicErr == nil && classic == nil:
		// A nil protection with a nil error carries no information —
		// treat it as "no classic protection", the same as ErrNotFound,
		// rather than dereferencing it.
	case classicErr == nil:
		requires := false
		for _, c := range classic.RequiredStatusCheckContexts {
			addContext(c)
			if c == wantContext {
				requires = true
			}
		}
		if requires {
			out.Sources = append(out.Sources, Source{
				Identity:      SourceBranchProtection,
				Classic:       true,
				EnforceAdmins: classic.EnforceAdmins,
				// enforce_admins:false exempts repository admins from
				// this source. It is a NAMED condition, not a count —
				// BypassEntries stays 0.
				Bypassable: !classic.EnforceAdmins,
			})
		}
	case errors.Is(classicErr, forge.ErrNotFound):
		// A positive "this branch has no classic protection". Still
		// authoritative; fall through to rulesets.
	case errors.Is(classicErr, forge.ErrForbidden):
		return unknown(out, ReasonScopeMissing,
			"reading branch protection was refused (403): the App installation is missing `administration: read`",
			"Re-install the Fishhawk GitHub App so the installation accepts the `administration: read` scope, then re-run the check."), nil
	default:
		return unknown(out, ReasonTransportError,
			fmt.Sprintf("reading classic branch protection failed: %v", classicErr),
			"Re-run the check once the forge is reachable."), nil
	}

	// --- rulesets ---------------------------------------------------
	rulesets, rulesetsAuth, rulesetsErr := api.ListRulesetRequiredChecksForDefault(ctx, scope, repo, branch, defaultBranch)
	authoritative := false
	reason, detail := "", ""
	switch {
	case rulesetsErr == nil:
		authoritative = rulesetsAuth
		if !authoritative {
			reason = ReasonNonAuthoritative
			detail = "an active branch ruleset carried a ref_name condition this version cannot evaluate, so a requirement may be hidden behind a condition that was not read"
		}
		for _, r := range rulesets {
			requires := false
			for _, c := range r.Contexts {
				addContext(c)
				if c == wantContext {
					requires = true
				}
			}
			if !requires {
				continue
			}
			out.Sources = append(out.Sources, Source{
				Identity:      fmt.Sprintf("%s%d", sourceRulesetPrefix, r.RulesetID),
				BypassEntries: r.BypassEntries,
				Bypassable:    r.BypassEntries > 0,
			})
		}
	case errors.Is(rulesetsErr, forge.ErrForbidden):
		return unknown(out, ReasonScopeMissing,
			"listing rulesets was refused (403): the App installation is missing `administration: read`",
			"Re-install the Fishhawk GitHub App so the installation accepts the `administration: read` scope, then re-run the check."), nil
	case errors.Is(rulesetsErr, forge.ErrNotFound):
		// The endpoint is absent (some self-hosted GHES versions).
		// The surface was never read, so its silence is not evidence:
		// carry on with whatever classic contributed, non-authoritative.
		reason = ReasonRulesetsUnqueryable
		detail = "the repository rulesets endpoint answered 404, so that surface could not be read"
	default:
		return unknown(out, ReasonTransportError,
			fmt.Sprintf("listing rulesets failed: %v", rulesetsErr),
			"Re-run the check once the forge is reachable."), nil
	}

	out.Authoritative = authoritative
	out.Reason, out.Detail = reason, detail

	// --- verdict ----------------------------------------------------
	if len(out.Sources) > 0 {
		// A positive finding stands on its own: the check IS required
		// by an observed source, whether or not the sweep was
		// exhaustive. A non-authoritative sweep leaves Reason set, so a
		// renderer can caveat the bypass posture.
		out.Status = StatusRequired
		out.Bypassable = allBypassable(out.Sources)
		if out.Bypassable {
			out.Remediation = bypassRemediation(out.Sources)
		}
		return out, nil
	}
	if !authoritative {
		// Nothing found, but a surface went unread. Fail closed.
		return unknown(out, reason, detail, notRequiredRemediation(wantContext, branch)), nil
	}
	out.Status = StatusNotRequired
	out.Remediation = notRequiredRemediation(wantContext, branch)
	return out, nil
}

// unknown stamps the fail-closed verdict onto a partially-built
// reconciliation. It clears Sources and Bypassable: a report that could
// not settle the question must not also carry a bypass claim derived
// from a partial read.
func unknown(out Reconciliation, reason, detail, remediation string) Reconciliation {
	out.Status = StatusUnknown
	out.Authoritative = false
	out.Sources = nil
	out.Bypassable = false
	out.Reason = reason
	out.Detail = detail
	out.Remediation = remediation
	return out
}

// allBypassable is the conjunction the bypass model turns on: the gate
// is bypassable only if EVERY requiring source is individually
// bypassable. One source with no bypass path still enforces the check
// regardless of what the others allow.
func allBypassable(sources []Source) bool {
	if len(sources) == 0 {
		return false
	}
	for _, s := range sources {
		if !s.Bypassable {
			return false
		}
	}
	return true
}

// bypassRemediation names each source's bypass condition in its own
// terms — entry counts for rulesets, the admin exemption for classic.
func bypassRemediation(sources []Source) string {
	out := "Every source requiring this check can be bypassed. Narrow them:"
	for _, s := range sources {
		switch {
		case s.Classic:
			out += fmt.Sprintf("\n  - %s: repository admins are exempt (enforce_admins is off); turn it on to apply the rules to admins too.",
				s.Identity)
		default:
			out += fmt.Sprintf("\n  - %s: %d bypass entries (roles, teams or apps), each of which may cover multiple people; review the ruleset's bypass list.",
				s.Identity, s.BypassEntries)
		}
	}
	return out
}

func notRequiredRemediation(check, branch string) string {
	return fmt.Sprintf(
		"Add %q to the required status checks for %q — either as a classic branch-protection check or in a ruleset's required_status_checks rule:\n"+
			"  gh api -X PUT /repos/{owner}/{repo}/rulesets/{id} -f 'rules[][type=required_status_checks]' \\\n"+
			"    -f 'rules[][parameters][required_status_checks][][context=%s]'\n"+
			"A bypass entry on that ruleset lets its holders merge past the check even once it is required.",
		check, branch, check)
}
