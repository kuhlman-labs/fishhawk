# `backend/internal/mergegate`

Reconciles the status check Fishhawk **publishes** against the branch protection the forge actually **enforces** (E64.44 / #3161).

Fishhawk posts a `fishhawk_audit_complete` Check Run on every run's pull request. Whether that check *gates the merge* is a property of the repository's protection configuration, not of Fishhawk. Before this package, nothing read the forge to find out — and four operator-facing surfaces asserted the check was "required" with no evidence behind the word. On this repository the claim was false for the whole dogfood period.

`Reconcile` answers exactly one question — **does `<check>` gate merges on `<branch>`?** — and it answers fail-closed.

## Status contract

| `Status` | Meaning | Reachable when |
|---|---|---|
| `required` | At least one protection source was **observed** requiring the check. | A positive finding on either surface. Does not depend on the sweep being exhaustive. |
| `not_required` | Neither surface requires it, and **both answered definitively**. | Only on a fully authoritative evaluation. This is the fail-closed invariant. |
| `unknown` | The question could not be settled. `Reason` names why. | Every degrade below. |

There is no fourth state. Anything that is neither a positive finding nor a fully authoritative absence is `unknown`.

This mirrors the #2497 / #2506 vacuous-green discipline `backend/internal/server/required_checks_capture.go` documents: a surface that was never read must never be reported as a positive "this repo requires nothing".

## Fail-closed degrade table

Each row is a named branch with its own behavioral test in `mergegate_test.go`.

| Condition | `Status` | `Reason` | Test |
|---|---|---|---|
| Rulesets endpoint answered 404 (some self-hosted GHES versions do not expose it) | `unknown`* | `rulesets_unqueryable` | `TestReconcile_RulesetsNotFound_IsUnknown` |
| An active branch ruleset carried a `ref_name` include token the v0 matcher cannot evaluate (fnmatch glob, unknown `~TOKEN`) | `unknown`* | `non_authoritative` | `TestReconcile_UnevaluatableRefNameToken_IsUnknown`, `TestReconcile_NonAuthoritative_IsUnknownNotNotRequired` |
| 403 from either surface — the App installation lacks `administration: read` (ADR-017 / #252) | `unknown` | `administration_read_missing` | `TestReconcile_ForbiddenAdministrationRead_IsUnknown` |
| Any other failure reading either surface | `unknown` | `transport_error` | `TestReconcile_TransportError_IsUnknown` |
| Both surfaces answered, nothing requires the check | `not_required` | *(empty)* | `TestReconcile_AuthoritativeAndAbsent_IsNotRequired` |

\* These two degrade the *evaluation*, not the *finding*: if a source that WAS read requires the check, `Status` stays `required` and `Reason` is retained as a caveat (`TestReconcile_RequiredButNonAuthoritative_KeepsPositiveFinding`). They resolve to `unknown` only when nothing was found — which is precisely the case where an unread surface could be hiding a requirement.

`unknown` also **clears** `Sources` and `Bypassable` (`TestReconcile_UnknownCarriesNoBypassClaim`): a report that could not settle the question must not carry a bypass verdict derived from the half of the read that landed.

`Reconcile` returns a non-nil `error` only for caller mistakes — a nil `ProtectionAPI`, an empty branch, an empty check. Every *forge* failure resolves to an `unknown` `Reconciliation` with a nil error, so a caller that forgets to inspect `Status` still cannot render a false "not required".

## Bypass semantics

A required check that anyone can bypass is not the gate it looks like, so the report never says "required" without saying what can get past it. Three rules, all load-bearing:

**1. Bypass is a conjunction, never a sum.** Classic branch protection and each ruleset enforce *independently*. A merger must bypass **every** source that requires the check, not any one of them. `Reconciliation.Bypassable` is therefore the AND over the requiring sources: one source with no bypass path still enforces the check regardless of what the others allow. Summing bypass counts across sources — the model this package deliberately does not use — inverts the logic and reports a gate as bypassable while another source is still holding it. Pinned by `TestReconcile_BypassRequiresEveryRequiringSource` (two requiring sources, one bypassable → `Bypassable=false`) with `TestReconcile_AllRequiringSourcesBypassable` as its flip side, so the AND is proven to be a real conjunction rather than a constant `false`.

**2. A bypass entry is not a person.** A ruleset `bypass_actors` element is a role, team, app or integration; it may cover many people or none. The field is `BypassEntries`, and the rendered wording is *"N bypass entries (roles, teams or apps), each of which may cover multiple people"*. Never "bypassable by N actors".

**3. Classic exemption is its own named condition.** Classic protection has no bypass list. `enforce_admins: false` exempts an unknown number of repository admins, and it is rendered as *"repository admins are exempt"* — never coerced into a count of 1. `Source.BypassEntries` stays `0` for the classic source, which `TestReconcile_RequiredViaClassicProtection_IsRequired` and `TestReconcile_RequiredButBypassable_ReportsPerSourceBypass` both assert.

Per source:

| Source | `Identity` | Bypassable when |
|---|---|---|
| Classic branch protection | `branch_protection` | `EnforceAdmins == false` |
| A repository ruleset | `ruleset:<id>` | `BypassEntries > 0` |

## Residuals

Stated, not implied:

- **The read is point-in-time.** A ruleset edited after the probe is not reflected. This package is **reporting only** — it does not gate a run, a merge, or an exit code. GitHub remains the merge-time authority.
- **`Bypassable` is complete only when `Authoritative` is true.** When the sweep was not exhaustive, a source that went unevaluated may still enforce, so `Bypassable == true` means "bypassable through every source that could be evaluated". That is the over-warning direction, which is the safe one for a report whose job is to flag a weak gate — but it is not a proof.
- **The v0 ref_name matcher is inherited, not extended.** `githubclient.rulesetMatchesBranch` understands `~ALL`, `~DEFAULT_BRANCH` and exact branch names. Anything else yields `authoritative=false` here rather than a guess.
- **Organization-level rulesets are only partly covered.** The client lists repo rulesets with `includes_parents=true`, which surfaces inherited org rulesets, but org rulesets scoped by repository-property conditions are not evaluated by the v0 matcher and land as non-authoritative → `unknown`.
- **Non-GitHub forges classify as `unknown`.** `forge.BranchProtection.EnforceAdmins` and `forge.RulesetRequiredCheck.BypassEntries` are additive and zero-valued for producers that do not populate them — the GitLab adapter compiles and behaves unchanged. A consumer must not read a zero-valued `EnforceAdmins` as a positive "admins are exempt"; only the GitHub client decodes it.

## Seam

`ProtectionAPI` names the same two methods as `webhook.ProtectionAPI` and is satisfied by `*githubclient.Client` directly. It is declared here rather than imported so a test can substitute an in-process fake with no httptest server and no `webhook` dependency — every test in this package seeds its state **by construction**, so a counterfactual deletion of a control reddens the behavioral assertion rather than a fixture.

## Callers

None yet in this slice — the package is deliberately dead code until the `merge_gate` onboarding-readiness rung wires it up (#3161 slice 2), which keeps this slice independently landable and green.
