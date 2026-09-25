package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergegate"
	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	"github.com/kuhlman-labs/fishhawk/pricing"
)

// requiredRunScopes is the run-driving subset of operatorDefaultScopes
// (the canonical operator token scope set, backend/cmd/fishhawkd/token.go)
// that a caller's token must carry to drive a repo's first run end to end:
// read the run + its audit chain, and write the run/approval/stage
// transitions the plan → implement → review loop performs. It deliberately
// EXCLUDES write:campaigns, write:deploy, and read:audit-export — those gate
// the campaign primitive, the deploy stage, and the bulk compliance-export
// surfaces (E9.5/#1608), none of which is exercised on a repo's first
// feature_change run, so requiring them here would over-report a scope
// gap for exactly the onboarding caller this endpoint serves. Keep this in
// lockstep with operatorDefaultScopes if the run-drive contract changes.
var requiredRunScopes = []string{
	"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages",
}

// onboardingReadinessResponse aggregates the server-side-only checks
// `fishhawk doctor` (E29.5) needs before a repo's first run — five on GitHub,
// six on GitLab (E45.43 / #3348, E45.66 / #3580, E45.68 / #3582): App
// installation (on GitLab: a gitlab installation registered for exactly this
// project path AND the project resolvable with the deployment credential),
// the committed workflow spec's parse/validate state, per reviewer
// availability on this deployment, the caller token's scope adequacy, a
// forge-shaped merge-gate read — on GitHub (#3161) whether the
// `fishhawk_audit_complete` check Fishhawk publishes is actually REQUIRED by
// the repo's branch protection; on GitLab (#3580) whether the project's real
// default branch is protected and the project requires a successful head
// pipeline to merge — and, on GitLab only, the `gitlab_registration` rung
// (#3582): whether an installations row is registered for exactly this path,
// the check POST /v0/runs refuses 422 gitlab_project_not_registered on —
// plus, on BOTH families, the deployment-scoped `trace_store` rung (E45.75 /
// #3600), which is outside the repo-scoped count and never cascades. The
// repo-scoped checks cascade — a not-installed GitHub repo (on GitLab: an unresolvable
// project) yields an unavailable spec, empty reviewers and an `unknown` merge
// gate — each with an explanatory note.
//
// Forge names the family that answered ("github" or "gitlab"), resolved per
// request by onboardingForgeFamily.
//
// MergeGate is a POINTER and is OMITTED ENTIRELY (nil) on the gitlab family:
// mergegate.Reconcile reads branch protection + rulesets through a
// *githubclient.Client — GitHub-only surfaces the GitLab adapter stubs
// (ListRulesetRequiredChecks returns nil, nil) — so no authoritative
// merge-gate read exists on GitLab, and rendering one as `not_required` would
// be exactly the over-claim mergeGateReadiness's invariant forbids. Both
// client mirrors already model absence as a pointer (#3161), so the omitted
// key is read as "no claim about the merge gate", never as a verdict.
//
// GitLabMergeGate is the GitLab-shaped sibling (E45.66 / #3580), a SEPARATE
// key rather than a reuse of `merge_gate` because the two rungs answer
// DIFFERENT questions and must never be conflated: GitLab expresses no
// per-context required status check, so it cannot answer "is
// fishhawk_audit_complete required", only "is the default branch protected
// and must the head pipeline succeed". It is nil on the github family and
// set on every gitlab-family report, `unknown` with a naming reason on every
// degrade.
//
// GitLabRegistration (E45.68 / #3582) is a POINTER for the same reason as
// GitLabMergeGate: nil on the github family (no claim — no installations
// registry applies to a GitHub repo, and the registry is never consulted
// there), set on EVERY gitlab-family report including the forge-unconfigured
// one, `unknown` with a naming reason whenever the registry cannot answer.
//
// WorkItemProvider (E45.94 / #3646) is HYBRID-scoped — the one rung that is
// neither purely deployment- nor purely repo-scoped. The RESOLVED work-item
// provider id is a REPO fact (the repo's work-management conventions, read
// through the same conventionsLoader seam POST /v0/campaigns and
// POST /v0/work-items resolve through), while the REGISTERED provider set is
// a DEPLOYMENT fact (workmgmt.Registered(), populated at startup by
// registerWorkmgmtProviders, gated per configured client). Their INTERSECTION
// is the verdict. It is nevertheless set BEFORE the forge-family switch, so no
// repo-scoped cascade (not-installed app, unresolvable GitLab project,
// unavailable spec) can suppress it. Like TraceStore it is a pointer only so
// the client mirrors can model an older fishhawkd's absent key as "no claim"
// (never a zero-valued out-of-enum empty status); this handler never leaves it
// nil.
//
// ReviewGrounding (E45.90 / #3625) is DEPLOYMENT-scoped for the same reason
// as TraceStore, and answers a different question: is review grounding ON for
// this deployment, and what BOUNDS a grounded reviewer's reads per adapter? It
// is set on EVERY report of both families and sits OUTSIDE every repo-scoped
// cascade. Like TraceStore it is a pointer only so the client mirrors can
// model an older fishhawkd's absent key as "no claim" (never a zero-valued
// enabled:false verdict); this handler never leaves it nil. The rung does NOT
// change the default: grounding ships DORMANT and `enabled:false` is the
// supported posture, so the CLI renders it ok-with-a-hint, never a warn.
//
// TraceStore (E45.75 / #3600) is DEPLOYMENT-scoped: it answers "will this
// run's trace upload 503?", a fact about fishhawkd's wiring, not about the
// repo. It is set on EVERY report of both families and sits OUTSIDE every
// repo-scoped cascade — a not-installed repo, an unresolvable gitlab project
// and an unavailable spec all still carry it. It is a pointer only so the
// client mirrors can model an older fishhawkd's absent key as "no claim"
// (never a zero-valued verdict); this handler never leaves it nil.
type onboardingReadinessResponse struct {
	Repo               string                       `json:"repo"`
	Forge              string                       `json:"forge"`
	App                appInstallReadiness          `json:"app"`
	Spec               specReadiness                `json:"spec"`
	Reviewers          []reviewerReadiness          `json:"reviewers"`
	Scopes             scopeReadiness               `json:"scopes"`
	MergeGate          *mergeGateReadiness          `json:"merge_gate,omitempty"`
	GitLabMergeGate    *gitLabMergeGateReadiness    `json:"gitlab_merge_gate,omitempty"`
	GitLabRegistration *gitLabRegistrationReadiness `json:"gitlab_registration,omitempty"`
	TraceStore         *traceStoreReadiness         `json:"trace_store,omitempty"`
	ReviewGrounding    *reviewGroundingReadiness    `json:"review_grounding,omitempty"`
	WorkItemProvider   *workItemProviderReadiness   `json:"work_item_provider,omitempty"`
}

// traceStoreReadiness reports whether this deployment has a trace store wired
// and what kind (E45.75 / #3600). Kind is exactly one of the four
// traceStoreKind* values:
//
//   - "none"   — no store: POST /v0/runs/{id}/trace responds 503, so every
//     run fails at trace upload AFTER the agent has run and been billed.
//     Configured is false and Remediation names the fix.
//   - "memory" — the --dev-fixtures in-memory store: configured, but
//     EPHEMERAL (bundles are lost on restart). Note says so.
//   - "s3"     — the durable S3 / RustFS store.
//   - "other"  — some other non-nil tracestore.Storage implementation:
//     configured, with no claim about durability (never mislabelled "s3").
type traceStoreReadiness struct {
	Configured  bool   `json:"configured"`
	Kind        string `json:"kind"`
	Note        string `json:"note,omitempty"`
	Remediation string `json:"remediation,omitempty"`
}

// The closed trace_store kind vocabulary (four values; docs/api/v0.openapi.yaml
// enumerates the same set).
const (
	traceStoreKindS3     = "s3"
	traceStoreKindMemory = "memory"
	traceStoreKindNone   = "none"
	traceStoreKindOther  = "other"
)

const (
	traceStoreNoneNote        = "no trace store is configured on this deployment: POST /v0/runs/{id}/trace responds 503, so every run fails at trace upload AFTER the agent has run and been billed"
	traceStoreNoneRemediation = "set FISHHAWKD_S3_BUCKET (plus the S3 endpoint/region/credentials — see the trace-storage block in .env.example) and create the bucket with `make s3-init`, then restart fishhawkd"
	traceStoreMemoryNote      = "the in-memory trace store is wired (--dev-fixtures or --dev-trace-store): uploads succeed, but it is EPHEMERAL — every bundle is lost when fishhawkd restarts, so it is not durable for a real dogfood loop"
	traceStoreOtherNote       = "a non-S3, non-memory trace store implementation is wired; no claim is made about its durability"
)

// traceStoreReadinessFor resolves the trace_store rung from the configured
// store by concrete type, without widening the tracestore.Storage interface.
// Pure, so onboarding_test tables every branch without booting a server. A
// typed-nil concrete store is treated as unconfigured: its methods would
// dereference nil, so claiming it configured would be an over-claim.
func traceStoreReadinessFor(ts tracestore.Storage) traceStoreReadiness {
	none := traceStoreReadiness{
		Configured:  false,
		Kind:        traceStoreKindNone,
		Note:        traceStoreNoneNote,
		Remediation: traceStoreNoneRemediation,
	}
	switch v := ts.(type) {
	case nil:
		return none
	case *tracestore.S3Storage:
		if v == nil {
			return none
		}
		return traceStoreReadiness{Configured: true, Kind: traceStoreKindS3}
	case *tracestore.MemStorage:
		if v == nil {
			return none
		}
		return traceStoreReadiness{Configured: true, Kind: traceStoreKindMemory, Note: traceStoreMemoryNote}
	default:
		return traceStoreReadiness{Configured: true, Kind: traceStoreKindOther, Note: traceStoreOtherNote}
	}
}

// reviewGroundingReadiness reports whether this deployment grounds its plan-
// and implement-review agents against an exported read-only source tree
// (E45.90 / #3625), and what bounds a grounded reviewer's reads PER ADAPTER.
//
// Enabled false is the SUPPORTED DEFAULT, not a defect: grounding ships
// dormant behind FISHHAWKD_REVIEW_GROUNDING (#2522). The rung exists so an
// operator reading `fishhawk doctor` can DISCOVER the switch and the posture
// it buys, which is why Adapters renders in BOTH postures — the per-adapter
// asymmetry is precisely what an operator deciding whether to opt IN needs.
type reviewGroundingReadiness struct {
	Enabled     bool                          `json:"enabled"`
	Adapters    []reviewGroundingAdapterBound `json:"adapters,omitempty"`
	Note        string                        `json:"note,omitempty"`
	Remediation string                        `json:"remediation,omitempty"`
}

// reviewGroundingAdapterBound is one reviewer adapter's read bound. Bound is a
// CLOSED two-value vocabulary and the two are NOT equivalent — the asymmetry
// backend/internal/reviewsandbox/README.md § "Reviewer read bounds (#2522) —
// TWO mechanisms, NOT the same strength" forbids collapsing into one word:
//
//   - "confined"  — codex: the synthesized `confined` permission profile
//     (reviewsandbox.CodexConfinedHome), an OS-level deny-by-default
//     ALLOWLIST; an out-of-tree read returns EPERM.
//   - "blocklist" — claude: reviewsandbox.ClaudeDenyRules, a BOUNDED
//     `--disallowed-tools` deny-rule list over a fixed set of credential
//     roots, enforced at the TOOL layer. Defence-in-depth, and never
//     described here as the OS-enforced bound codex gets.
type reviewGroundingAdapterBound struct {
	Adapter string `json:"adapter"`
	Bound   string `json:"bound"`
	Note    string `json:"note,omitempty"`
}

// The closed review_grounding bound vocabulary (docs/api/v0.openapi.yaml
// enumerates the same two values).
const (
	reviewGroundingBoundConfined  = "confined"
	reviewGroundingBoundBlocklist = "blocklist"
)

const (
	reviewGroundingAdapterCodex  = "codex"
	reviewGroundingAdapterClaude = "claude"

	// The codex note is the ONLY place either note claims confinement, and it
	// is the adapter that actually has it.
	reviewGroundingCodexNote = "codex reviews run under a synthesized `confined` permission profile: an OS-level deny-by-default allowlist over the exported tree, so a read outside it returns EPERM rather than a model refusal"
	// The claude note deliberately makes NO confinement claim — the honest
	// label reviewsandbox/README.md pins. A resolver test asserts the word is
	// absent from this string.
	reviewGroundingClaudeNote = "claude reviews are bounded by a `--disallowed-tools` blocklist over a fixed set of credential roots, enforced at the TOOL layer: defence-in-depth, and NOT the OS-enforced bound codex gets"

	reviewGroundingOffNote        = "review grounding is OFF on this deployment (the supported default): the plan- and implement-review agents are DIFF-ONLY, so a reviewer downgrades a diff-invisible question to UNTRACED / UNESTABLISHED and calibrates the severity DOWN rather than reading the repository to settle it"
	reviewGroundingOffRemediation = "set FISHHAWKD_REVIEW_GROUNDING=true (or --review-grounding) to ground reviews against an exported read-only tree at the reviewed commit. It is an OPT-IN posture for a single-tenant host you control: a grounded reviewer processing untrusted diff content gets read access bounded per adapter, and the two bounds are NOT equivalent — codex gets OS-enforced confinement, claude gets a tool-layer blocklist that is defence-in-depth only"
	reviewGroundingOnNote         = "review grounding is ON: plan- and implement-review agents read an exported read-only tree at the reviewed commit. The per-adapter read bounds below are NOT equivalent — see adapters[].bound"
	reviewGroundingOnRemediation  = "set FISHHAWKD_REVIEW_GROUNDING=false (the default) to revert both adapters to the diff-only posture. While it is on, keep this deployment single-tenant: claude's bound is a tool-layer blocklist, not confinement, so a reviewer's verdict text remains an egress path for anything it can still read"
)

// reviewGroundingReadinessFor resolves the review_grounding rung from the
// server's grounding kill switch. Pure, for the same reason
// traceStoreReadinessFor is: onboarding_test tables every branch without
// booting a server.
//
// The adapter table is STATIC — it restates the posture #2522 shipped
// (reviewsandbox/confine.go CodexConfinedHome / ClaudeDenyRules), it is not a
// live probe of the adapter argv, and the backend never spawns a reviewer to
// measure confinement at readiness time.
func reviewGroundingReadinessFor(disabled bool) reviewGroundingReadiness {
	adapters := []reviewGroundingAdapterBound{
		{Adapter: reviewGroundingAdapterCodex, Bound: reviewGroundingBoundConfined, Note: reviewGroundingCodexNote},
		{Adapter: reviewGroundingAdapterClaude, Bound: reviewGroundingBoundBlocklist, Note: reviewGroundingClaudeNote},
	}
	if disabled {
		return reviewGroundingReadiness{
			Enabled:     false,
			Adapters:    adapters,
			Note:        reviewGroundingOffNote,
			Remediation: reviewGroundingOffRemediation,
		}
	}
	return reviewGroundingReadiness{
		Enabled:     true,
		Adapters:    adapters,
		Note:        reviewGroundingOnNote,
		Remediation: reviewGroundingOnRemediation,
	}
}

// workItemProviderReadiness reports whether the work-item provider this repo's
// conventions RESOLVE to is actually REGISTERED on this deployment (E45.94 /
// #3646). Without it, a deployment on which every campaign, every
// fishhawk_file_issue and the whole grooming loop is impossible reports
// all-green, and the operator learns otherwise only from a 501
// provider_unimplemented AFTER the first call.
//
// Status is a CLOSED three-value vocabulary:
//
//   - "registered"   — the resolved provider is in the registered set: a
//     filing call will dispatch.
//   - "unregistered" — the resolved provider is NOT registered. A FAILURE,
//     not data: fishhawk_start_campaign, fishhawk_file_issue
//     and the grooming loop all respond 501
//     provider_unimplemented. Note states the consequence and
//     MissingHint names the per-provider remedy.
//   - "unknown"      — the repo's conventions could not be resolved, so the
//     question could not be SETTLED. Read it FAIL-CLOSED: it
//     is NOT evidence the provider is unregistered, and it is
//     never rendered as a pass.
//
// Registered is ALWAYS emitted (possibly as an empty array) because the
// deployment registry answers even when the repo's conventions do not — it is
// the half of the verdict that is always knowable.
//
// CampaignSources (#3658) is ALSO always emitted as an array, never a JSON
// null: the campaign sources ("epic_ref", "items", in that fixed order) the
// resolved provider can serve, computed by campaignSourcesSupported from
// compile-time capability assertions. It is populated ONLY on the registered
// branch — an unregistered or unknown rung makes NO capability claim and
// reports []. A registered provider reporting [] (a File-only provider) will
// have fishhawk_start_campaign refuse 501 in either mode.
type workItemProviderReadiness struct {
	Status          string   `json:"status"`
	Provider        string   `json:"provider,omitempty"`
	Registered      []string `json:"registered"`
	CampaignSources []string `json:"campaign_sources"`
	Reason          string   `json:"reason,omitempty"`
	Note            string   `json:"note,omitempty"`
	MissingHint     string   `json:"missing_hint,omitempty"`
}

// The closed work_item_provider status vocabulary (three values;
// docs/api/v0.openapi.yaml enumerates the same set).
const (
	workItemProviderStatusRegistered   = "registered"
	workItemProviderStatusUnregistered = "unregistered"
	workItemProviderStatusUnknown      = "unknown"
)

const (
	// workItemProviderUnknownReason is a PRODUCT-OWNED closed-set string, not
	// the resolution error's text: a conventions-load failure can carry forge
	// transport detail (a URL, a status line, a credential-shaped token), and
	// this rung follows the merge_gate / gitlab_registration `reason`
	// discipline of naming the CLASS of failure rather than echoing it.
	workItemProviderUnknownReason = "conventions_unresolved"
	workItemProviderUnknownNote   = "the repo's work-management conventions could not be resolved, so the work-item provider this repo would file through is UNKNOWN. This is not evidence that no provider is registered: the deployment registry (registered[] below) answered, the repo did not."
	workItemProviderUnknownHint   = "check that .fishhawk/work-management.yaml on the repo's default branch parses (or that the deployment's FISHHAWKD_WORKMGMT_CONVENTIONS fallback file does), and check the fishhawkd log for the conventions-load failure; until the conventions resolve, this rung makes NO claim about whether filing would succeed"

	workItemProviderUnregisteredNote = "the work-item provider this repo's conventions resolve to is NOT registered on this deployment: fishhawk_start_campaign, fishhawk_file_issue and the backlog-grooming loop all respond 501 provider_unimplemented"

	// workItemProviderNoneRegisteredHint is the distinguished EMPTY-registry
	// sub-case: naming one provider's env vars would be misleading when the
	// deployment has wired NONE of them.
	workItemProviderNoneRegisteredHint = "NO work-item provider is registered on this deployment at all: configure at least one of the GitHub App (FISHHAWKD_GITHUB_APP_ID + FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE), GitLab (FISHHAWKD_GITLAB_BASE_URL + FISHHAWKD_GITLAB_TOKEN) or Jira (FISHHAWKD_JIRA_BASE_URL + FISHHAWKD_JIRA_EMAIL + FISHHAWKD_JIRA_API_TOKEN) credentials and RESTART fishhawkd — registerWorkmgmtProviders registers a provider only when its client is configured at startup"

	// workItemProviderRegisterContract is appended to every per-provider hint:
	// the registration is a STARTUP fact, so a credential set after boot
	// changes nothing until fishhawkd restarts.
	workItemProviderRegisterContract = ", then RESTART fishhawkd — registerWorkmgmtProviders registers a provider only when its client is configured at startup, so the remedy is deployment configuration plus a restart"
)

// workItemProviderMissingHint names the concrete startup gate for the
// resolved provider id, verified against backend/cmd/fishhawkd/serve.go's flag
// table and workmgmt_wiring.go's per-client gating.
//
// An id OUTSIDE the three shipped providers gets a GENERIC hint naming the id,
// the registered set and the conventions key — never a fabricated env var,
// because inventing a plausible FISHHAWKD_<ID>_TOKEN would send an operator
// looking for a knob that does not exist.
func workItemProviderMissingHint(provider string, registered []string) string {
	if len(registered) == 0 {
		return workItemProviderNoneRegisteredHint
	}
	switch provider {
	case "github_projects":
		return "set FISHHAWKD_GITHUB_APP_ID and FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE" + workItemProviderRegisterContract
	case "gitlab":
		return "set FISHHAWKD_GITLAB_BASE_URL and FISHHAWKD_GITLAB_TOKEN (both are required; a partial configuration leaves the provider disabled)" + workItemProviderRegisterContract
	case "jira":
		return "set FISHHAWKD_JIRA_BASE_URL, FISHHAWKD_JIRA_EMAIL and FISHHAWKD_JIRA_API_TOKEN (all three are required; a partial configuration leaves the provider disabled)" + workItemProviderRegisterContract
	default:
		return "the repo's conventions name work-item provider " + provider +
			", which this build has no startup configuration for; registered providers on this deployment are: " +
			strings.Join(registered, ", ") +
			". Either correct the `provider:` key in .fishhawk/work-management.yaml to one of those, or deploy a build that registers " + provider
	}
}

// workItemProviderReadinessFor resolves the work_item_provider rung from the
// repo's resolved conventions provider, the deployment's registered set, and
// the conventions-resolution error, plus the campaign sources the caller
// computed from the resolved provider instance (campaignSourcesSupported; nil
// when no instance resolved). The provider is resolved by the CALLER, not here.
// Pure, for the same reason
// traceStoreReadinessFor and reviewGroundingReadinessFor are: onboarding_test
// tables every branch without booting a server.
//
// The error branch is FAIL-CLOSED and comes FIRST: with no resolved provider
// id, falling through to the membership comparison would compare "" against
// the registry and render `unregistered` — a positive finding about a repo
// whose conventions were never read.
func workItemProviderReadinessFor(provider string, registered, campaignSources []string, resolveErr error) workItemProviderReadiness {
	// Unregistered and unknown verdicts make NO capability claim, so they
	// always report an empty array whatever the caller passed.
	noSources := []string{}
	if registered == nil {
		// Always emit an ARRAY, never a JSON null: the field is documented as
		// always present, and a null would decode into a client mirror
		// indistinguishably from "the key was absent".
		registered = []string{}
	}
	if resolveErr != nil {
		return workItemProviderReadiness{
			Status:          workItemProviderStatusUnknown,
			Registered:      registered,
			CampaignSources: noSources,
			Reason:          workItemProviderUnknownReason,
			Note:            workItemProviderUnknownNote,
			MissingHint:     workItemProviderUnknownHint,
		}
	}
	for _, id := range registered {
		if id == provider {
			if campaignSources == nil {
				campaignSources = []string{}
			}
			return workItemProviderReadiness{
				Status:          workItemProviderStatusRegistered,
				Provider:        provider,
				Registered:      registered,
				CampaignSources: campaignSources,
			}
		}
	}
	return workItemProviderReadiness{
		Status:          workItemProviderStatusUnregistered,
		Provider:        provider,
		Registered:      registered,
		CampaignSources: noSources,
		Note:            workItemProviderUnregisteredNote,
		MissingHint:     workItemProviderMissingHint(provider, registered),
	}
}

// appInstallReadiness reports whether the Fishhawk-specific authorization a
// run needs exists on the target repo. On the github family Installed means
// the GitHub App is installed; Reason carries the human-readable explanation
// when it is not (or when the client could not resolve the installation).
//
// On the gitlab family (E45.68 / #3582) Installed means "a gitlab installation
// row is registered for EXACTLY this project path (`fishhawkd installation
// register` — the authorization POST /v0/runs checks) AND the project resolves
// with the deployment credential"; Note says so and points at the
// gitlab_registration rung. The weaker fact — resolvable with the credential,
// which is what Installed meant before #3582 — moves to its own explicit
// Resolvable pointer: &true when ResolveRepoScope succeeded, &false on
// forge.ErrNotInstalled or any resolve fault, nil when the gitlab forge is
// unconfigured (never read). It is always nil on github, and InstallationID is
// never set on gitlab. The spec and gitlab_merge_gate cascades key on
// Resolvable, not on the stricter Installed: a resolvable-but-unregistered
// project still gets its spec fetched and its protection read.
type appInstallReadiness struct {
	Installed      bool   `json:"installed"`
	InstallationID int64  `json:"installation_id,omitempty"`
	Resolvable     *bool  `json:"resolvable,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Note           string `json:"note,omitempty"`
}

// specReadiness reports the committed workflow spec's fetch + parse + validate
// state. Source is "fetched" when the spec was read from the repo, else
// "unavailable" (with Note explaining why). Valid is only meaningful when
// Source == "fetched"; Error carries the parse or validation failure.
type specReadiness struct {
	Source string `json:"source"`
	Valid  bool   `json:"valid"`
	Error  string `json:"error,omitempty"`
	Note   string `json:"note,omitempty"`
}

// reviewerReadiness reports one spec-declared reviewer's availability on this
// deployment.
//
// Available means the provider is wired on this deployment AND the reviewer's
// resolved model was not AUTHORITATIVELY rejected. It does NOT assert the model
// is served when ModelStatus is "unverifiable" (no live snapshot to check
// against) — read ModelStatus for that.
//
// ModelStatus is the model-id verdict (#3578): "verified" (present in a fresh
// snapshot), "rejected" (authoritatively absent), or "unverifiable" (no
// authoritative snapshot — nil oracle, stale, or none for the provider). It is
// computed from the RESOLVED model — the explicit spec value, or the deployment
// default when the spec omits the model — so an omitted default is judged
// exactly as an explicit one (condition 2). Empty only when no reviewer backend
// is wired for the provider, or the wired set does not expose the resolved
// default id.
//
// ModelHint is the human sentence for a non-verified status: the did-you-mean
// rejection text for "rejected", or the "passed to the vendor verbatim" warning
// for "unverifiable"; it also carries the unpriced $0 note when Priced is false.
//
// Priced reports whether the pricing table knows the model's family, so an
// unpriced id is flagged rather than silently booking usage at $0. It is a
// POINTER with three states: nil (not computed — no reviewer backend wired for
// the provider, or the resolved default id is unavailable), &true (priced),
// &false (unpriced).
//
// MissingHint carries the adapter's missing-env-var hint when the provider
// cannot be resolved at all; it stays separate from ModelHint.
type reviewerReadiness struct {
	Provider        string `json:"provider"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	Available       bool   `json:"available"`
	ModelStatus     string `json:"model_status,omitempty"`
	ModelHint       string `json:"model_hint,omitempty"`
	Priced          *bool  `json:"priced,omitempty"`
	MissingHint     string `json:"missing_hint,omitempty"`
}

// scopeReadiness reports whether the caller token holds the run-driving scope
// subset. Missing lists the required scopes the caller lacks (empty when
// adequate). Cookie-session callers bypass scope enforcement and are adequate
// by construction, recorded in Note.
type scopeReadiness struct {
	Adequate bool     `json:"adequate"`
	Required []string `json:"required"`
	Missing  []string `json:"missing"`
	Note     string   `json:"note,omitempty"`
}

// mergeGateReadiness reports whether the `fishhawk_audit_complete` Check Run
// Fishhawk PUBLISHES is actually REQUIRED by the repo's protection
// configuration on its default branch (#3161), reconciled by
// backend/internal/mergegate.
//
// The invariant this field exists to carry: Status is `not_required` ONLY on a
// fully authoritative read of BOTH protection surfaces. Every degrade — no
// GitHub client, App not installed, default-branch lookup failure, a 403 from
// a missing `administration: read`, a rulesets endpoint that 404s, a ref_name
// token the v0 matcher cannot evaluate, a transport error or a probe timeout —
// resolves to `unknown` with a naming Reason. A surface that was never read
// must never render as "this repo requires nothing".
//
// It is REPORTING only: it gates no run, no merge and no exit code, and it is
// a point-in-time read — a ruleset edited after the probe is not reflected.
// Long-form contract: backend/internal/server/README.md.
type mergeGateReadiness struct {
	// Status is "required", "not_required" or "unknown".
	Status string `json:"status"`
	// Check is the context name that was probed.
	Check string `json:"check"`
	// Branch is the repo's REAL default branch, the branch the probe
	// evaluated. Empty when the probe never got far enough to resolve it.
	Branch string `json:"branch,omitempty"`
	// Sources are the protection surfaces observed requiring Check, each
	// with its OWN bypass posture. Never aggregated across sources: each
	// source enforces independently, so a merger must bypass every one of
	// them.
	Sources []mergeGateSource `json:"sources,omitempty"`
	// Bypassable is the conjunction over Sources — true only when EVERY
	// requiring source is individually bypassable. False when Sources is
	// empty.
	Bypassable bool `json:"bypassable"`
	// Authoritative is true only when both protection surfaces answered
	// definitively for the probed branch.
	Authoritative bool `json:"authoritative"`
	// Reason is a machine code naming why the evaluation did not settle (or
	// settled only partially). Non-empty whenever Status is "unknown".
	Reason string `json:"reason,omitempty"`
	// Detail is the human sentence for Reason.
	Detail string `json:"detail,omitempty"`
	// Remediation is the operator's next step, when there is one.
	Remediation string `json:"remediation,omitempty"`
	// RequiredContexts is the union of every context the evaluated sources
	// require — what IS required, when the probed check is not.
	RequiredContexts []string `json:"required_contexts,omitempty"`
}

// mergeGateSource is one protection surface requiring the probed check, with
// its own bypass detail.
//
// BypassEntries counts a ruleset's `bypass_actors` ENTRIES — each a role, team,
// app or integration that may cover many people or none. It is never a
// headcount, and the classic source's admin exemption is NOT coerced into a
// count of 1: that is carried by EnforceAdmins as its own named condition.
type mergeGateSource struct {
	// Identity is "branch_protection" or "ruleset:<id>".
	Identity string `json:"identity"`
	// Classic is true for the classic branch-protection source.
	Classic bool `json:"classic,omitempty"`
	// BypassEntries is the number of entries in THIS ruleset's bypass_actors
	// array. Always 0 for the classic source.
	BypassEntries int `json:"bypass_entries"`
	// EnforceAdmins mirrors classic protection's enforce_admins.enabled;
	// meaningful only when Classic is true. False means repository admins are
	// exempt from this source.
	EnforceAdmins bool `json:"enforce_admins,omitempty"`
	// Bypassable is whether THIS source alone can be bypassed.
	Bypassable bool `json:"bypassable"`
}

// Reason codes for the degrades that never reach mergegate.Reconcile — the
// preconditions the server itself cannot satisfy. The forge-level codes
// (rulesets_unqueryable, non_authoritative, administration_read_missing,
// transport_error) come from the mergegate package and are surfaced verbatim.
const (
	// mergeGateReasonNoGitHubClient — this deployment has no GitHub client
	// wired, so no protection surface can be read at all.
	mergeGateReasonNoGitHubClient = "github_client_unconfigured"
	// mergeGateReasonAppNotInstalled — both protection reads need an
	// installation token, which an uninstalled App cannot mint.
	mergeGateReasonAppNotInstalled = "app_not_installed"
	// mergeGateReasonDefaultBranch — the repo's REAL default branch could not
	// be resolved. The probe never guesses "main": a `~DEFAULT_BRANCH`
	// ruleset evaluated against the wrong branch would silently mis-answer.
	mergeGateReasonDefaultBranch = "default_branch_unresolved"
	// mergeGateReasonProbeFailed — Reconcile rejected the call itself (a
	// caller mistake such as an empty branch). Forge failures never land
	// here; they resolve to a StatusUnknown reconciliation instead.
	mergeGateReasonProbeFailed = "probe_failed"
)

// mergeGateRepository resolves the target repo's metadata (its REAL default
// branch). It is a package var so a test can drive probeMergeGate's
// nil/empty-default-branch guard: githubclient.GetRepository never returns
// (nil, nil) — it errors when `default_branch` is absent — so that guard is
// unreachable through the concrete client, yet removing it would leave a nil
// dereference one interface change away.
var mergeGateRepository = func(ctx context.Context, gh *githubclient.Client, scope forge.CredentialScope,
	repo githubclient.RepoRef) (*githubclient.Repository, error) {
	return gh.GetRepository(ctx, scope, repo)
}

// mergeGateReconcile is the reconciliation seam, a package var for the same
// reason: mergegate.Reconcile reserves its error return for CALLER mistakes
// (nil api, empty branch, empty check), all three of which probeMergeGate has
// already excluded by the time it calls — so the error branch is unreachable
// in production but must still fail closed rather than render a verdict.
var mergeGateReconcile = mergegate.Reconcile

// mergeGateProbeTimeout bounds the whole merge-gate probe — the default-branch
// resolve plus both protection reads. Same 10s order as
// requiredChecksCaptureTimeout (required_checks_capture.go): a readiness report
// must not hang on a slow forge. Declared as a var so a test can shrink it.
var mergeGateProbeTimeout = 10 * time.Second

// probeMergeGate runs readiness check (5). It reads the repo's REAL default
// branch and hands it to mergegate.Reconcile, which answers whether the
// published check gates merges on it.
//
// Every failure path returns StatusUnknown with a naming Reason — the
// fail-closed contract. `not_required` is reachable ONLY through a
// Reconciliation that said so on an authoritative read.
func (s *Server) probeMergeGate(ctx context.Context, repo string, repoRef githubclient.RepoRef,
	installed bool, installationID int64) mergeGateReadiness {
	out := mergeGateReadiness{
		Status: string(mergegate.StatusUnknown),
		Check:  auditcheckpublisher.CheckName,
	}
	switch {
	case s.cfg.GitHub == nil:
		out.Reason = mergeGateReasonNoGitHubClient
		out.Detail = "github client not configured on this deployment; the repository's branch protection could not be read"
		return out
	case !installed:
		out.Reason = mergeGateReasonAppNotInstalled
		out.Detail = "GitHub App is not installed on the target repository; reading branch protection needs an installation token"
		out.Remediation = "Install the Fishhawk GitHub App on " + repo + ", then re-run the check."
		return out
	}

	scope := forge.FromGitHubInstallationID(installationID)
	pctx, cancel := context.WithTimeout(ctx, mergeGateProbeTimeout)
	defer cancel()

	// The REAL default branch, never a guessed "main": the ruleset matcher
	// evaluates `~DEFAULT_BRANCH` against it, so a repo defaulting to `trunk`
	// resolves its rulesets only when the true name is supplied (#2506).
	meta, err := mergeGateRepository(pctx, s.cfg.GitHub, scope, repoRef)
	switch {
	case err != nil:
		out.Reason = mergeGateReasonDefaultBranch
		out.Detail = "could not resolve the repository's default branch: " + err.Error()
		out.Remediation = "Re-run the check once the forge is reachable."
		s.cfg.Logger.Warn("onboarding readiness: merge gate default-branch resolve failed",
			"repo", repo, "error", err.Error())
		return out
	case meta == nil || meta.DefaultBranch == "":
		out.Reason = mergeGateReasonDefaultBranch
		out.Detail = "the repository response carried no default branch, so the protection surfaces could not be evaluated"
		out.Remediation = "Re-run the check once the forge is reachable."
		return out
	}

	branch := meta.DefaultBranch
	out.Branch = branch

	rec, err := mergeGateReconcile(pctx, s.cfg.GitHub, scope, repoRef, branch, branch, auditcheckpublisher.CheckName)
	if err != nil {
		// Reserved for caller mistakes; forge failures come back as a
		// StatusUnknown reconciliation with a nil error.
		out.Reason = mergeGateReasonProbeFailed
		out.Detail = "the merge-gate probe could not run: " + err.Error()
		s.cfg.Logger.Warn("onboarding readiness: merge gate probe failed",
			"repo", repo, "error", err.Error())
		return out
	}

	out.Status = string(rec.Status)
	out.Check = rec.Check
	out.Bypassable = rec.Bypassable
	out.Authoritative = rec.Authoritative
	out.Reason = rec.Reason
	out.Detail = rec.Detail
	out.Remediation = rec.Remediation
	out.RequiredContexts = rec.RequiredContexts
	for _, src := range rec.Sources {
		out.Sources = append(out.Sources, mergeGateSource{
			Identity:      src.Identity,
			Classic:       src.Classic,
			BypassEntries: src.BypassEntries,
			EnforceAdmins: src.EnforceAdmins,
			Bypassable:    src.Bypassable,
		})
	}
	return out
}

// gitLabMergeGateReadiness is the GitLab-shaped merge-gate rung (E45.66 /
// #3580), the sibling of mergeGateReadiness on the gitlab family. It answers
// the question GitLab CAN answer: is the project's REAL default branch
// protected (by which protected-branch rules, with what push/merge access and
// force-push posture) and does the project require a successful head pipeline
// to merge (`only_allow_merge_if_pipeline_succeeds`, plus the informational
// `allow_merge_on_skipped_pipeline` and
// `only_allow_merge_if_all_discussions_are_resolved`). It is read through
// forge.MergeProtectionReader, which only the gitlab adapter implements.
//
// The invariant, mirroring mergeGateReadiness: Status is `not_pipeline_gated`
// ONLY when BOTH reads (the project settings and the protected-branch list)
// answered authoritatively. Every degrade — no gitlab forge, project not
// visible, an adapter without the capability, an unresolved default branch, a
// 401/403, a transport error or a probe timeout — resolves to `unknown` with a
// naming Reason, and every signal that was never read is ABSENT (the pointer
// bools stay nil), never rendered as false.
//
// What it does NOT claim: GitLab has no per-context required status check, so
// `pipeline_gated` is not a statement that the `fishhawk_audit_complete`
// commit status individually gates the merge — Note says so on every report.
// REPORTING only, point-in-time. Long-form contract:
// backend/internal/server/README.md.
type gitLabMergeGateReadiness struct {
	// Status is "pipeline_gated", "not_pipeline_gated" or "unknown".
	Status string `json:"status"`
	// Branch is the project's REAL default branch, the branch the probe
	// evaluated. Empty when the probe never got far enough to resolve it.
	Branch string `json:"branch,omitempty"`
	// Protected is whether at least one protected-branch rule (exact or
	// wildcard) covers Branch. Absent when the rule list was not read.
	Protected *bool `json:"protected,omitempty"`
	// MatchedRules names EVERY rule covering Branch — the exact-name rule
	// first, then wildcard rules in API order — because GitLab applies the
	// MOST PERMISSIVE of all matching rules, so a single rule is never the
	// effective protection. Empty when unprotected or unread.
	MatchedRules []string `json:"matched_rules,omitempty"`
	// AllowForcePush is the OR across every matched rule. Absent when unread.
	AllowForcePush *bool `json:"allow_force_push,omitempty"`
	// PushAccessLevels / MergeAccessLevels are the UNION across every matched
	// rule, deduplicated by level and sorted ascending — the lowest level is
	// the most permissive and comes first. Empty when unprotected or unread.
	PushAccessLevels  []gitLabAccessLevel `json:"push_access_levels,omitempty"`
	MergeAccessLevels []gitLabAccessLevel `json:"merge_access_levels,omitempty"`
	// PipelineMustSucceed mirrors the project's
	// only_allow_merge_if_pipeline_succeeds. Absent when the project settings
	// were not read.
	PipelineMustSucceed *bool `json:"pipeline_must_succeed,omitempty"`
	// AllowSkippedPipeline mirrors allow_merge_on_skipped_pipeline
	// (informational). Absent when unread.
	AllowSkippedPipeline *bool `json:"allow_skipped_pipeline,omitempty"`
	// DiscussionsMustBeResolved mirrors
	// only_allow_merge_if_all_discussions_are_resolved (informational).
	// Absent when unread.
	DiscussionsMustBeResolved *bool `json:"discussions_must_be_resolved,omitempty"`
	// Authoritative is true only when both reads answered definitively.
	Authoritative bool `json:"authoritative"`
	// Reason is a machine code naming why the evaluation did not settle.
	// Non-empty whenever Status is "unknown".
	Reason string `json:"reason,omitempty"`
	// Detail is the human sentence for Reason, or for a not_pipeline_gated
	// verdict the signal(s) that are off.
	Detail string `json:"detail,omitempty"`
	// Remediation is the operator's next step, when there is one.
	Remediation string `json:"remediation,omitempty"`
	// Note is the constant gitLabMergeGateNote, present on every report.
	Note string `json:"note"`
}

// gitLabAccessLevel is one role permitted to push to or merge into the
// protected branch: GitLab's numeric access level plus its description.
type gitLabAccessLevel struct {
	Level       int    `json:"level"`
	Description string `json:"description"`
}

// gitLabMergeGateNote is the constant sentence every gitlab_merge_gate
// report carries: what the rung answers, that access levels are the effective
// union across matching rules, and what GitLab cannot express.
const gitLabMergeGateNote = "gitlab: this rung reports whether the project's real default branch is protected and whether the project requires a successful head pipeline to merge; push/merge access levels are the effective UNION across every matching protected-branch rule per GitLab's most-permissive semantics. GitLab expresses no per-context required status check, so pipeline_gated is NOT a claim that the fishhawk_audit_complete commit status individually gates the merge."

// Reason codes for the gitlab_merge_gate rung's `unknown` degrades.
const (
	// gitLabMergeGateReasonForgeUnconfigured — no gitlab forge is wired on
	// this deployment, so nothing could be read.
	gitLabMergeGateReasonForgeUnconfigured = "gitlab_forge_unconfigured"
	// gitLabMergeGateReasonProjectNotVisible — the project did not resolve
	// with the deployment credential (rung (1) already says why).
	gitLabMergeGateReasonProjectNotVisible = "project_not_visible"
	// gitLabMergeGateReasonUnsupported — the resolved forge adapter does not
	// implement forge.MergeProtectionReader.
	gitLabMergeGateReasonUnsupported = "merge_protection_unsupported"
	// gitLabMergeGateReasonDefaultBranch — the project's REAL default branch
	// could not be resolved (an empty repository, or a project the read could
	// not find). The probe never guesses "main".
	gitLabMergeGateReasonDefaultBranch = "default_branch_unresolved"
	// gitLabMergeGateReasonForbidden — a 401/403 on the project or the
	// protected-branch read; the latter needs at least the Maintainer role.
	gitLabMergeGateReasonForbidden = "forbidden"
	// gitLabMergeGateReasonTransport — any other read failure, including the
	// probe timeout.
	gitLabMergeGateReasonTransport = "transport_error"
)

// probeGitLabMergeGate runs readiness check (5) on the gitlab family (E45.66 /
// #3580). f and scope are the forge + resolved scope probeGitLab produced;
// resolvable is its rung-(1) RESOLVABILITY verdict (not the stricter
// registration-conjoined Installed, #3582 — the protection read needs only
// the credential), so an unresolved project never reaches a forge call here.
//
// Every failure path returns `unknown` with a naming Reason and leaves every
// signal pointer nil — the fail-closed contract. `not_pipeline_gated` is
// reachable ONLY through an authoritative ReadMergeProtection.
func (s *Server) probeGitLabMergeGate(ctx context.Context, repo string, f forge.Forge,
	scope forge.CredentialScope, ref forge.RepoRef, resolvable bool) gitLabMergeGateReadiness {
	out := gitLabMergeGateReadiness{
		Status: string(mergegate.StatusUnknown),
		Note:   gitLabMergeGateNote,
	}
	switch {
	case f == nil:
		out.Reason = gitLabMergeGateReasonForgeUnconfigured
		out.Detail = "gitlab forge not configured on this deployment; the project's protected-branch and merge settings could not be read"
		out.Remediation = "Set FISHHAWKD_GITLAB_TOKEN and FISHHAWKD_GITLAB_BASE_URL, then re-run the check."
		return out
	case !resolvable:
		out.Reason = gitLabMergeGateReasonProjectNotVisible
		out.Detail = "project is not visible to the deployment GitLab credential; its protected-branch and merge settings could not be read"
		out.Remediation = "Confirm the deployment GitLab credential (FISHHAWKD_GITLAB_TOKEN) can read " + repo + ", then re-run the check."
		return out
	}
	reader, ok := f.(forge.MergeProtectionReader)
	if !ok {
		out.Reason = gitLabMergeGateReasonUnsupported
		out.Detail = "the resolved gitlab forge adapter does not expose protected-branch reads"
		return out
	}

	pctx, cancel := context.WithTimeout(ctx, mergeGateProbeTimeout)
	defer cancel()

	// An empty branch asks the adapter for the project's REAL default
	// branch (never a guessed "main"); the project read precedes the rule
	// list inside the adapter, so a 404 there is a real failure, not an
	// "unprotected" verdict.
	mp, err := reader.ReadMergeProtection(pctx, scope, ref, "")
	if err != nil {
		switch {
		case errors.Is(err, forge.ErrNotFound):
			out.Reason = gitLabMergeGateReasonDefaultBranch
			out.Detail = "could not resolve the project's default branch: " + err.Error()
			out.Remediation = "Confirm the project has a default branch with at least one commit, then re-run the check."
		case errors.Is(err, forge.ErrForbidden):
			out.Reason = gitLabMergeGateReasonForbidden
			out.Detail = "the deployment GitLab credential may not read the project's protected-branch settings (the protected_branches API needs at least the Maintainer role): " + err.Error()
			out.Remediation = "Grant the deployment credential the Maintainer role on the project (or a token with read access to its protected-branch settings), then re-run the check."
		default:
			out.Reason = gitLabMergeGateReasonTransport
			out.Detail = "reading the project's protection failed: " + err.Error()
			out.Remediation = "Re-run the check once the forge is reachable."
		}
		s.cfg.Logger.Warn("onboarding readiness: gitlab merge gate read failed",
			"repo", repo, "reason", out.Reason, "error", err.Error())
		return out
	}

	out.Authoritative = true
	out.Branch = mp.Branch
	out.Protected = boolPtr(mp.Protected)
	out.MatchedRules = mp.MatchedRules
	out.AllowForcePush = boolPtr(mp.AllowForcePush)
	out.PushAccessLevels = gitLabAccessLevels(mp.PushAccessLevels)
	out.MergeAccessLevels = gitLabAccessLevels(mp.MergeAccessLevels)
	out.PipelineMustSucceed = boolPtr(mp.PipelineMustSucceed)
	out.AllowSkippedPipeline = boolPtr(mp.AllowSkippedPipeline)
	out.DiscussionsMustBeResolved = boolPtr(mp.DiscussionsMustBeResolved)

	if mp.Protected && mp.PipelineMustSucceed {
		out.Status = gitLabMergeGateStatusPipelineGated
		return out
	}
	out.Status = gitLabMergeGateStatusNotPipelineGated
	var off []string
	if !mp.Protected {
		off = append(off, "default branch "+mp.Branch+" is not covered by any protected-branch rule")
	}
	if !mp.PipelineMustSucceed {
		off = append(off, "the project does not require a successful pipeline to merge (only_allow_merge_if_pipeline_succeeds is off)")
	}
	out.Detail = strings.Join(off, "; ")
	out.Remediation = "In the GitLab project: Settings → Repository → Protected branches (protect " + mp.Branch + "), and Settings → Merge requests → Merge checks → enable \"Pipelines must succeed\"; then re-run the check."
	return out
}

// Status values for the gitlab_merge_gate rung; `unknown` is shared with the
// GitHub rung via mergegate.StatusUnknown.
const (
	gitLabMergeGateStatusPipelineGated    = "pipeline_gated"
	gitLabMergeGateStatusNotPipelineGated = "not_pipeline_gated"
)

// gitLabAccessLevels maps the forge-neutral access levels onto the wire shape,
// keeping the adapter's ascending order.
func gitLabAccessLevels(in []forge.AccessLevel) []gitLabAccessLevel {
	if len(in) == 0 {
		return nil
	}
	out := make([]gitLabAccessLevel, 0, len(in))
	for _, l := range in {
		out = append(out, gitLabAccessLevel{Level: l.Level, Description: l.Description})
	}
	return out
}

// Reason strings for the gitlab-family `app` rung (E45.43 / #3348, redefined
// by E45.68 / #3582). The not-visible reason is a credential-visibility fact
// and no longer names the register command — registration is the
// gitlab_registration rung's business; the two are distinct preconditions.
const (
	onboardingGitLabForgeUnconfigured = "gitlab forge not configured on this deployment (set FISHHAWKD_GITLAB_TOKEN and FISHHAWKD_GITLAB_BASE_URL)"
	onboardingGitLabProjectNotVisible = "project is not visible to the deployment GitLab credential (FISHHAWKD_GITLAB_TOKEN); confirm the token can read the project path"
	onboardingGitLabInstalledNote     = "gitlab: installed means a gitlab installation is registered for exactly this project path (fishhawkd installation register — the authorization POST /v0/runs checks) AND the project resolves with the deployment credential (resolvable); no App installation applies on GitLab — see gitlab_registration"
	// onboardingGitLabNotRegisteredReason is the app.reason when the project
	// resolved but no installation row is registered for its exact path.
	onboardingGitLabNotRegisteredReason = "no gitlab installation is registered for this exact project path; see gitlab_registration.remediation"
	// onboardingGitLabRegistryUnknownReasonPrefix leads the app.reason when
	// the registry could not answer; the rung's reason code follows in
	// parentheses.
	onboardingGitLabRegistryUnknownReasonPrefix = "the gitlab installation registry could not answer ("
	onboardingGitLabRegistryUnknownReasonSuffix = "); see gitlab_registration"
)

// gitLabRegistrationReadiness is the gitlab-only registration rung (E45.68 /
// #3582): whether an `installations` row is registered for EXACTLY this
// project path — the pre-flight for the check POST /v0/runs performs through
// the SAME cfg.GitLabInstallations.ResolveGitLabProject seam
// (resolveCreateGitLabInstallation, runs.go), which refuses
// 422 gitlab_project_not_registered when the row is absent or ambiguous. A
// `registered` verdict here is therefore exactly "POST /v0/runs will pass the
// registry check for this path"; TestOnboardingReadiness_GitLab_RegistrationAgreesWithRunCreate
// pins that agreement.
//
// Fail-closed like the merge-gate rungs: Status is `unknown` ONLY when the
// registry could not answer — no registry wired (registry_unwired) or the
// lookup faulted (registry_lookup_failed) — with Reason naming which. A
// found=false answer is the POSITIVE `not_registered` finding.
//
// RefMatches surfaces the registration-drift hazard: the registered
// InstallationRef (`gitlab:<project_id>`, what a run acts on) is compared with
// ResolvedRef (the id the path resolved to with the deployment credential,
// same format — forge/gitlab ResolveRepoScope). It is set ONLY when both are
// known, so a mismatch is surfaced and an unresolved project is never
// rendered as a false match. Run-create ACCEPTS a mismatched row (it never
// resolves the path), so Installed stays true on a mismatch; Detail and
// Remediation say what would happen and how to re-register.
//
// What it does NOT check (Note says so on every report): the account_key
// binding the webhook receiver additionally enforces. REPORTING only.
type gitLabRegistrationReadiness struct {
	// Status is "registered", "not_registered" or "unknown".
	Status string `json:"status"`
	// ProjectPath is the registered row's project path. Absent unless
	// registered.
	ProjectPath string `json:"project_path,omitempty"`
	// InstallationRef is the registered row's `gitlab:<project_id>` ref, the
	// project a run would act on. Absent unless registered.
	InstallationRef string `json:"installation_ref,omitempty"`
	// ResolvedRef is the `gitlab:<project_id>` ref the path resolved to with
	// the deployment credential. Absent when the project did not resolve.
	ResolvedRef string `json:"resolved_ref,omitempty"`
	// RefMatches is InstallationRef == ResolvedRef, set ONLY when both are
	// known.
	RefMatches *bool `json:"ref_matches,omitempty"`
	// Reason is a machine code naming why the registry could not answer.
	// Non-empty whenever Status is "unknown".
	Reason string `json:"reason,omitempty"`
	// Detail is the human sentence for the status / reason.
	Detail string `json:"detail,omitempty"`
	// Remediation is the operator's next step — a copy-pasteable
	// `fishhawkd installation register` command carrying the REAL project id
	// when the path resolved.
	Remediation string `json:"remediation,omitempty"`
	// Note is the constant gitLabRegistrationNote, present on every report.
	Note string `json:"note"`
}

// Status values for the gitlab_registration rung; `unknown` is shared with
// the merge-gate rungs via mergegate.StatusUnknown.
const (
	gitLabRegistrationStatusRegistered    = "registered"
	gitLabRegistrationStatusNotRegistered = "not_registered"
)

// Reason codes for the gitlab_registration rung's `unknown` degrades.
const (
	// gitLabRegistrationReasonRegistryUnwired — this deployment has no
	// installation registry (no database), so it cannot answer.
	gitLabRegistrationReasonRegistryUnwired = "registry_unwired"
	// gitLabRegistrationReasonLookupFailed — the registry lookup faulted.
	gitLabRegistrationReasonLookupFailed = "registry_lookup_failed"
)

// gitLabRegistrationNote is the constant sentence every gitlab_registration
// report carries: what the rung checks, that it is the run-create check, and
// what it does not check.
const gitLabRegistrationNote = "gitlab: this rung reports whether an installations row is registered for EXACTLY this project_path — the check POST /v0/runs performs (an absent or ambiguous registration is refused 422 gitlab_project_not_registered). It does NOT check the account_key binding the webhook receiver additionally enforces. REPORTING only."

// onboardingForgeFor resolves the forge adapter for a NON-GitHub family
// through the same ladder issueOpsFor walks: cfg.ForgeResolver defaulting to
// the process registry (forge.Get), a resolver error or a nil forge — INCLUDING
// a typed nil inside a non-nil interface, which isNilForge unwraps — yielding
// nil so the caller degrades to a naming reason instead of dereferencing it.
func (s *Server) onboardingForgeFor(forgeID string) forge.Forge {
	resolver := s.cfg.ForgeResolver
	if resolver == nil {
		resolver = forge.Get
	}
	f, err := resolver(forgeID)
	if err != nil || isNilForge(f) {
		return nil
	}
	return f
}

// onboardingForgeFamily decides which forge family answers a readiness
// request. An explicit (already validated) `forge` query value wins; otherwise
// cfg.RepoProviders.ResolveProvider — the accounts-table discriminator the
// conventions loader and the repo-visibility gate already key on — decides.
// Not found, a provider outside {github, gitlab}, or no resolver wired at all
// falls back to github, the byte-identical legacy behaviour; a resolver STORE
// fault is returned so the handler answers 503 rather than guessing a family.
func (s *Server) onboardingForgeFamily(ctx context.Context, explicit, repo string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if s.cfg.RepoProviders == nil {
		return observationForgeGitHub, nil
	}
	provider, found, err := s.cfg.RepoProviders.ResolveProvider(ctx, repo)
	if err != nil {
		return "", err
	}
	if found && (provider == observationForgeGitHub || provider == observationForgeGitLab) {
		return provider, nil
	}
	return observationForgeGitHub, nil
}

// onboardingNestedPath reports whether repo carries more than the single
// owner/name separator — a nested GitLab project path, which the github
// family refuses.
func onboardingNestedPath(repo string) bool {
	return strings.Count(repo, "/") > 1
}

// writeOnboardingNestedOnGitHub is the 400 both nested-on-github orderings
// share (see the handler comment): the path shape is a GitLab project's, so
// the remedy is to name the family.
func (s *Server) writeOnboardingNestedOnGitHub(w http.ResponseWriter, r *http.Request, repo string) {
	s.writeError(w, r, http.StatusBadRequest, "validation_failed",
		"github repositories are owner/name; a nested path is a GitLab project — pass forge=gitlab",
		map[string]any{"field": "repo", "got": repo})
}

// classifySpecBytes is the forge-independent half of readiness check (2): the
// spec bytes were fetched, so Source is "fetched" and the three-arm parse →
// validate → valid switch decides Valid/Error. The parsed spec is returned
// only when it validated cleanly, so the reviewer probe runs only then.
func classifySpecBytes(content []byte) (specReadiness, *spec.Spec) {
	out := specReadiness{Source: "fetched"}
	p, perr := spec.ParseBytes(content)
	if perr != nil {
		out.Error = perr.Error()
		return out, nil
	}
	if verr := spec.Validate(p); verr != nil {
		out.Error = verr.Error()
		return out, nil
	}
	out.Valid = true
	return out, p
}

// probeGitHub runs readiness checks (1) and (2) on the github family: the
// GitHub App installation resolve and the workflow spec fetch, exactly as the
// run-create path performs them. installed + installationID feed the merge
// gate probe, which only this family runs.
func (s *Server) probeGitHub(ctx context.Context, repo string, repoRef githubclient.RepoRef,
) (app appInstallReadiness, sp specReadiness, parsed *spec.Spec, installed bool, installationID int64) {
	// (1) GitHub App installation. Reuse the runs.go run-create switch:
	// nil → installed, ErrNotInstalled → not-installed with reason, any
	// other error → not-installed with the error as reason + a WARN. Never
	// 500 the whole endpoint on a transient installation-resolve error.
	if s.cfg.GitHub == nil {
		app.Reason = "github client not configured on this deployment"
	} else {
		id, err := s.cfg.GitHub.GetRepoInstallation(ctx, repoRef)
		switch {
		case err == nil:
			app.Installed = true
			app.InstallationID = id
			installationID = id
		case errors.Is(err, githubclient.ErrNotInstalled):
			app.Reason = "GitHub App is not installed on the target repository"
		default:
			app.Reason = err.Error()
			s.cfg.Logger.Warn("onboarding readiness: resolve repo installation failed",
				"repo", repo, "error", err.Error())
		}
	}

	// (2) Workflow spec fetch + parse + validate. Only meaningful once the
	// App is installed (the fetch needs an installation token). Empty ref
	// resolves the repo's default branch, matching run-create (runs.go).
	if !app.Installed {
		sp.Source = "unavailable"
		sp.Note = "GitHub App is not installed on the target repository; cannot fetch the workflow spec"
		return app, sp, nil, false, 0
	}
	fc, err := s.cfg.GitHub.GetWorkflowSpec(ctx, forge.FromGitHubInstallationID(installationID), repoRef, "")
	switch {
	case err == nil:
		sp, parsed = classifySpecBytes(fc.Content)
	case errors.Is(err, githubclient.ErrNotFound):
		sp.Source = "unavailable"
		sp.Note = "no workflow spec found on the repository's default branch"
	default:
		sp.Source = "unavailable"
		sp.Note = err.Error()
		s.cfg.Logger.Warn("onboarding readiness: fetch workflow spec failed",
			"repo", repo, "error", err.Error())
	}
	return app, sp, parsed, true, installationID
}

// probeGitLab runs the resolvability half of readiness check (1) and check
// (2) on the gitlab family (E45.43 / #3348). There is no App to install, so
// the forge read here is "is the project resolvable with the deployment
// credential" (Forge.ResolveRepoScope: a 404 surfaces as
// forge.ErrNotInstalled), recorded on app.Resolvable — NEVER on app.Installed,
// which the handler derives from this AND the gitlab_registration rung
// (E45.68 / #3582) — and (2) reads the spec through forge.FileFetcher on the
// resolved scope — which addresses the project by its FULL namespaced path,
// so nested groups work — before handing the bytes to the shared classifier.
// The spec cascade keys on Resolvable, so a resolvable-but-unregistered
// project still gets its spec fetched and parsed. Every degrade (forge
// unconfigured or typed-nil, project not visible, resolve or fetch fault, an
// adapter without file reads) lands as a naming reason on a 200, never a 5xx:
// this is a readiness REPORT.
//
// It also returns the resolved forge and scope so the handler's gitlab arm can
// hand them to probeGitLabMergeGate (E45.66 / #3580) and
// probeGitLabRegistration: f is nil when the forge is unconfigured (and
// app.Resolvable stays nil — never read), and scope is the zero value unless
// the project resolved.
func (s *Server) probeGitLab(ctx context.Context, repo string, ref forge.RepoRef,
) (app appInstallReadiness, sp specReadiness, parsed *spec.Spec, f forge.Forge, scope forge.CredentialScope) {
	f = s.onboardingForgeFor(observationForgeGitLab)
	if f == nil {
		app.Reason = onboardingGitLabForgeUnconfigured
		sp.Source = "unavailable"
		sp.Note = onboardingGitLabForgeUnconfigured
		return app, sp, nil, nil, scope
	}
	scope, err := f.ResolveRepoScope(ctx, ref)
	switch {
	case err == nil:
		app.Resolvable = boolPtr(true)
	case errors.Is(err, forge.ErrNotInstalled):
		app.Resolvable = boolPtr(false)
		app.Reason = onboardingGitLabProjectNotVisible
	default:
		app.Resolvable = boolPtr(false)
		app.Reason = err.Error()
		s.cfg.Logger.Warn("onboarding readiness: resolve gitlab project failed",
			"repo", repo, "error", err.Error())
	}
	if app.Resolvable == nil || !*app.Resolvable {
		sp.Source = "unavailable"
		sp.Note = "project is not resolvable with the deployment GitLab credential; cannot fetch the workflow spec"
		return app, sp, nil, f, scope
	}
	fetcher, ok := f.(forge.FileFetcher)
	if !ok {
		sp.Source = "unavailable"
		sp.Note = "gitlab forge does not expose file reads"
		return app, sp, nil, f, scope
	}
	fc, err := fetcher.FetchFile(ctx, scope, ref, githubclient.WorkflowSpecPath, "")
	switch {
	case err == nil:
		sp, parsed = classifySpecBytes(fc.Content)
	case errors.Is(err, forge.ErrNotFound):
		sp.Source = "unavailable"
		sp.Note = "no workflow spec found on the project default branch"
	default:
		sp.Source = "unavailable"
		sp.Note = err.Error()
		s.cfg.Logger.Warn("onboarding readiness: fetch gitlab workflow spec failed",
			"repo", repo, "error", err.Error())
	}
	return app, sp, parsed, f, scope
}

// probeGitLabRegistration runs readiness check (6) on the gitlab family
// (E45.68 / #3582): is an installations row registered for EXACTLY this
// project path? It reads the SAME cfg.GitLabInstallations.ResolveGitLabProject
// seam resolveCreateGitLabInstallation (runs.go) refuses
// 422 gitlab_project_not_registered on, with the same trimmed path and the
// same exact-match / ambiguous-is-not-found posture, so `registered` is
// exactly "POST /v0/runs will pass the registry check". resolvedRef is the
// `gitlab:<project_id>` ref the path resolved to with the deployment
// credential (empty when it did not resolve): it fills the REAL project id
// into the remediation and drives the ref_matches comparison.
//
// Every degrade returns `unknown` with a naming Reason — the fail-closed
// contract; a registry that ANSWERED found=false is the positive
// `not_registered`. Note is set on every return.
func (s *Server) probeGitLabRegistration(ctx context.Context, repo, resolvedRef string) gitLabRegistrationReadiness {
	out := gitLabRegistrationReadiness{
		Status: string(mergegate.StatusUnknown),
		Note:   gitLabRegistrationNote,
	}
	if s.cfg.GitLabInstallations == nil {
		out.Reason = gitLabRegistrationReasonRegistryUnwired
		out.Detail = "this deployment has no installation registry (no database), so it cannot tell whether the project is registered"
		out.Remediation = "Run fishhawkd with FISHHAWKD_DATABASE_URL set, then re-run the check."
		return out
	}
	inst, found, err := s.cfg.GitLabInstallations.ResolveGitLabProject(ctx, repo)
	if err != nil {
		out.Reason = gitLabRegistrationReasonLookupFailed
		out.Detail = "the gitlab installation registry lookup failed: " + err.Error()
		out.Remediation = "Re-run the check once the database is reachable."
		s.cfg.Logger.Warn("onboarding readiness: gitlab registration lookup failed",
			"repo", repo, "error", err.Error())
		return out
	}
	// The resolved ref is a fact about the path, not the row: present on a
	// not_registered finding too (it is what the remediation fills in).
	out.ResolvedRef = resolvedRef
	if !found {
		out.Status = gitLabRegistrationStatusNotRegistered
		out.Detail = "no gitlab installation is registered for project path " + repo + " (exact match; an ambiguous double registration also lands here)"
		out.Remediation = gitLabRegisterCommand(repo, resolvedRef)
		return out
	}
	out.Status = gitLabRegistrationStatusRegistered
	out.ProjectPath = inst.ProjectPath
	out.InstallationRef = inst.InstallationRef
	if resolvedRef == "" || out.InstallationRef == "" {
		// A row with no ref is unknown territory for the comparison: never
		// render a match or a mismatch against an empty ref.
		return out
	}
	out.RefMatches = boolPtr(out.InstallationRef == resolvedRef)
	if !*out.RefMatches {
		out.Detail = "the registered installation_ref " + out.InstallationRef + " names a different project than " + repo + " resolves to (" + resolvedRef + "); a run would act on the registered project"
		out.Remediation = "Re-register with the resolved ref: " + gitLabRegisterCommand(repo, resolvedRef)
	}
	return out
}

// gitLabRegisterCommand renders the copy-pasteable `fishhawkd installation
// register` command run-create's 422 names, substituting the REAL resolved
// `gitlab:<project_id>` ref when the path resolved and the
// `gitlab:<project_id>` placeholder otherwise.
func gitLabRegisterCommand(repo, resolvedRef string) string {
	namespace, _, _ := strings.Cut(repo, "/")
	ref := resolvedRef
	if ref == "" {
		ref = "gitlab:<project_id>"
	}
	return "fishhawkd installation register --provider gitlab --account-key " + namespace + " --installation-ref " + ref + " --project-path " + repo
}

// probeReviewers runs readiness check (3): per-reviewer availability plus the
// model-validity honesty fields (#3578), only when the spec parsed + validated
// cleanly (a nil parsed yields the empty, non-null list). It reuses the
// ReviewerSet.For probe unavailableSpecReviewers performs, then makes the
// residual honest:
//
//   - The For() probe now REJECTS a reviewer whose resolved model — the spec
//     value OR the deployment default when the spec omitted one — is
//     authoritatively absent from a fresh snapshot. A rejection arrives as a
//     modeloracle.RejectedError wrapping the verdict, so this rung recovers the
//     RESOLVED model, marks ModelStatus=rejected, forces Available=false, and
//     surfaces the did-you-mean plus the unpriced note — for a bad DEFAULT
//     exactly as for a bad explicit model.
//   - When the reviewer resolves and the spec named an EXPLICIT model, the model
//     is verified directly against the oracle so ModelStatus reflects
//     verified/unverifiable and Priced flags an unknown pricing family.
//   - When the reviewer resolves and the spec OMITTED the model, the resolved
//     deployment default (recovered via reviewerModelDefaulter) is verified the
//     same way, so an unverifiable or unpriced default is flagged rather than
//     rendering a bare "ok" (condition 2 / #3578). Only when the wired set does
//     not expose the default do ModelStatus/Priced stay unset ("not computed").
//
// The success-path verification is a SECOND read of the oracle after For()'s:
// if the snapshot flips to authoritative absence between them, annotateReviewerModel
// forces Available=false with the did-you-mean, so a TOCTOU rejection never
// renders "ok".
func (s *Server) probeReviewers(ctx context.Context, parsed *spec.Spec) []reviewerReadiness {
	out := []reviewerReadiness{}
	if parsed == nil {
		return out
	}
	for _, rv := range collectSpecReviewers(parsed) {
		rr := reviewerReadiness{
			Provider:        rv.Provider,
			Model:           rv.Model,
			ReasoningEffort: rv.ReasoningEffort,
		}
		if s.cfg.PlanReviewers == nil {
			rr.MissingHint = "no reviewer backend is wired on this deployment; set FISHHAWKD_ANTHROPIC_API_KEY, FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER, or FISHHAWKD_ENABLE_CODEX_REVIEWER"
			out = append(out, rr)
			continue
		}
		_, err := s.cfg.PlanReviewers.For(rv.Provider, rv.Model, rv.ReasoningEffort)
		if err == nil {
			rr.Available = true
			// Compute the honesty fields against the id that will ACTUALLY run:
			// the EXPLICIT spec model, or — when the spec omitted it — the
			// resolved deployment default (condition 2). Only a set that does not
			// expose its default leaves the id unknown, keeping the "not computed"
			// state. annotateReviewerModel re-verifies, so a snapshot that flipped
			// to authoritative absence since For() lands as unavailable+rejected.
			model := rv.Model
			if model == "" {
				if d, ok := s.resolvedReviewerDefault(rv.Provider); ok {
					model = d
				}
			}
			if model != "" {
				s.annotateReviewerModel(ctx, &rr, rv.Provider, model)
			}
			out = append(out, rr)
			continue
		}
		rr.MissingHint = err.Error()
		// A model rejection (the resolved model — explicit or the deployment
		// default — is authoritatively absent) is distinct from an unconfigured
		// provider: surface it as rejected with the did-you-mean and force
		// Available=false. The wrapped verdict carries the RESOLVED model, so
		// this works for an omitted default too (condition 2).
		var rejected modeloracle.RejectedError
		if errors.As(err, &rejected) {
			rr.ModelStatus = string(modeloracle.ModelRejected)
			rr.ModelHint = rejected.Verdict.RejectMessage()
			priced := modelPriced(rejected.Verdict.Model)
			rr.Priced = &priced
			if !priced {
				rr.ModelHint = appendUnpricedNote(rr.ModelHint)
			}
		}
		out = append(out, rr)
	}
	return out
}

// annotateReviewerModel sets the model-validity honesty fields for a reviewer
// whose model resolved through For() (the explicit spec value, or the resolved
// deployment default when the spec omitted it), from the deployment's snapshot
// oracle and the pricing table (#3578).
//
// This is a SECOND oracle read after For()'s own verification. Normally it
// reports verified/unverifiable, but the snapshot can flip to authoritative
// absence between the two reads (a background refresh): a ModelRejected verdict
// here is that TOCTOU case, so it forces Available=false and surfaces the
// did-you-mean, mirroring the For()-error path so a second-read rejection never
// renders "ok". A verified id is still flagged when the pricing table does not
// know its family.
func (s *Server) annotateReviewerModel(ctx context.Context, rr *reviewerReadiness, provider, model string) {
	v := modeloracle.Verify(ctx, s.cfg.ModelOracle, provider, model)
	rr.ModelStatus = string(v.Status)
	switch v.Status {
	case modeloracle.ModelRejected:
		// Snapshot flipped to authoritative absence since For() accepted it.
		rr.Available = false
		rr.ModelHint = v.RejectMessage()
	case modeloracle.ModelUnverifiable:
		rr.ModelHint = fmt.Sprintf("model %q could not be verified against a live model snapshot for provider %q; it is passed to the vendor verbatim and a typo fails at review time", model, provider)
	}
	priced := modelPriced(model)
	rr.Priced = &priced
	if !priced {
		rr.ModelHint = appendUnpricedNote(rr.ModelHint)
	}
}

// reviewerModelDefaulter is OPTIONALLY implemented by a ReviewerSet to report
// the deployment default model For() resolves for a provider when the spec omits
// the reviewer model. The readiness rung uses it to compute the model-id honesty
// fields against the id that will ACTUALLY run (condition 2 / #3578): without it
// an unverifiable or unpriced deployment default would render a bare "ok". A set
// that does not implement it — or whose default is unset — keeps the honest "not
// computed" state.
type reviewerModelDefaulter interface {
	ResolvedReviewerModel(provider string) (string, bool)
}

// resolvedReviewerDefault returns the deployment default model for provider (the
// id For() resolves for an omitted spec model) when the wired ReviewerSet
// exposes it and the default is non-empty; absent otherwise.
func (s *Server) resolvedReviewerDefault(provider string) (string, bool) {
	d, ok := s.cfg.PlanReviewers.(reviewerModelDefaulter)
	if !ok {
		return "", false
	}
	model, ok := d.ResolvedReviewerModel(provider)
	if !ok || model == "" {
		return "", false
	}
	return model, true
}

// modelPriced reports whether the shared pricing table knows the model's family,
// so an unpriced id can be flagged rather than silently recorded at $0.
func modelPriced(model string) bool {
	_, ok := pricing.Cost(model, 1, 1)
	return ok
}

// appendUnpricedNote appends the $0-estimate warning to an existing model hint,
// joining with "; " when the hint already carries text.
func appendUnpricedNote(hint string) string {
	const note = "unpriced: usage under this model is recorded at $0 (estimated)"
	if hint == "" {
		return note
	}
	return hint + "; " + note
}

// probeScopes runs readiness check (4): caller-token scope adequacy against
// the run-driving subset. Cookie-session callers (TokenID == "") authenticate
// via OAuth, carry no explicit scope list, and bypass scope enforcement
// (requireWriteScope), so they are adequate by construction.
func probeScopes(ident Identity) scopeReadiness {
	out := scopeReadiness{Required: requiredRunScopes, Missing: []string{}}
	if ident.TokenID == "" {
		out.Adequate = true
		out.Note = "cookie-session caller: scope enforcement is bypassed for OAuth sessions"
		return out
	}
	for _, want := range requiredRunScopes {
		if !hasScope(ident, want) {
			out.Missing = append(out.Missing, want)
		}
	}
	out.Adequate = len(out.Missing) == 0
	return out
}

// handleGetOnboardingReadiness implements
// GET /v0/onboarding/readiness?repo=<path>[&forge=github|gitlab] (E29.4,
// forge-family-aware since E45.43 / #3348). It aggregates the server-side-only
// readiness probes a first run needs, reusing the exact classification the
// run-create path performs.
//
// Two-part gate:
//
//   - It does NOT gate on a write scope — scope adequacy is itself a reported
//     field, and a write-scope gate would lock out precisely the callers who
//     need to discover their gap.
//   - It DOES gate on repo read-visibility: a NON-ADMIN cookie-session caller
//     who lacks forge `read` on the queried repo gets 403 repo_forbidden BEFORE
//     any installation resolve or spec fetch (ADR-057 Amendment A2 / #2071,
//     issue #1512). The gate reuses enforceRepoVisibility, so the endpoint
//     inherits the whole #2071 point-read contract: the three unfiltered
//     postures (bearer/MCP token identities, workspace admins — INCLUDING admin
//     cookie sessions, which bypass via the RoleAdmin branch of repoFilterFor
//     and never see the 403 — and deployments with no repo-ACL mirror wired),
//     403 on a deny, 503 on a mirror-store / provider-resolution / role-
//     resolution fault, and the cross-forge / prefixless-subject fail-closed
//     denies.
//
// The ordering invariant is load-bearing: 401 anonymous → 400 malformed
// forge/repo → visibility → family resolution. Anonymous is rejected before
// any filter resolve (an unauthenticated caller must not learn a repo
// exists); the `forge` value is validated to the two known families and the
// repo string to a well-formed <namespace>/<project> path (nested GitLab
// groups allowed, every component non-empty) before the filter is handed it;
// and only then does the visibility gate run — so a denied caller reaches
// ZERO forge calls, ZERO spec fetches, and receives no spec.Error text at all.
//
// The github family's stricter two-segment rule lands in one of two places,
// depending on WHEN the family is known:
//
//   - forge=github EXPLICIT: the nested-path 400 fires BEFORE
//     enforceRepoVisibility, so an authenticated caller gets the promised
//     validation_failed ahead of any visibility denial, with zero visibility
//     and zero forge calls.
//   - forge OMITTED: the family is only known after RepoProviders resolution,
//     which necessarily follows visibility (the resolver is a store read the
//     denied caller must not trigger), so a registry-resolved github family
//     rejects the nested path AFTER the visibility gate — still with zero
//     forge calls.
func (s *Server) handleGetOnboardingReadiness(w http.ResponseWriter, r *http.Request) {
	ident := IdentityFrom(r.Context())
	if ident.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token or session is required", nil)
		return
	}

	forgeParam := strings.TrimSpace(r.URL.Query().Get("forge"))
	switch forgeParam {
	case "", observationForgeGitHub, observationForgeGitLab:
	default:
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"forge must be github or gitlab",
			map[string]any{"field": "forge", "got": forgeParam})
		return
	}

	// Trimmed ONCE, here, before any consumer sees it. ProjectPathWellFormed
	// trims INTERNALLY (registry_test.go pins "  acme/widgets  " as
	// well-formed), so without this the handler's own copy of `repo` would
	// pass validation yet reach enforceRepoVisibility, the RepoRef split,
	// both probes and the response echo verbatim — a padded query drawing a
	// spurious 403/not-visible/not-installed answer (#3488). Every consumer
	// below (the 400 `got` detail, the nested-path checks, the visibility
	// gate, onboardingForgeFamily, the strings.Cut split, probeGitHub/
	// probeGitLab/probeMergeGate, and resp.Repo) reads this single trimmed
	// value — no second trim anywhere.
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	// The shared shape rule (account.ProjectPathWellFormed): a non-empty
	// namespace, then one or more non-empty components. This admits a nested
	// GitLab path; the github family's two-segment rule is applied below.
	if !account.ProjectPathWellFormed(repo) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo must be <owner>/<name> (GitHub) or <namespace>/<project> with every component non-empty (GitLab; nested groups allowed)",
			map[string]any{"field": "repo", "got": repo})
		return
	}
	// Explicit github family: the two-segment rule fires BEFORE visibility.
	if forgeParam == observationForgeGitHub && onboardingNestedPath(repo) {
		s.writeOnboardingNestedOnGitHub(w, r, repo)
		return
	}

	// Repo read-visibility gate (#1512, ADR-057 Amendment A2 / #2071). Runs
	// AFTER the 401/400 checks above and BEFORE any forge call below, so a
	// denied caller learns nothing about the repo's installation or spec state.
	if !s.enforceRepoVisibility(w, r, repo) {
		return
	}

	family, err := s.onboardingForgeFamily(r.Context(), forgeParam, repo)
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "service_unavailable",
			"could not resolve the repository forge; retry shortly", nil)
		return
	}
	// Registry-resolved github family: the two-segment rule fires AFTER
	// visibility (the family was not known before it).
	if family == observationForgeGitHub && onboardingNestedPath(repo) {
		s.writeOnboardingNestedOnGitHub(w, r, repo)
		return
	}
	// Owner is the first segment, Name the remainder: RepoRef.String()
	// re-joins the full path, which is what the GitLab adapter addresses the
	// project by (gitlab.go ResolveRepoScope / FetchFile).
	owner, name, _ := strings.Cut(repo, "/")
	repoRef := forge.RepoRef{Owner: owner, Name: name}

	// (7) trace_store (E45.75 / #3600), (8) review_grounding (E45.90 / #3625)
	// and (9) work_item_provider (E45.94 / #3646) are set HERE, before the
	// forge switch and its repo-scoped cascades, so every report of both
	// families carries them whatever the app/spec/merge-gate probes conclude.
	ts := traceStoreReadinessFor(s.cfg.TraceStore)
	// (8) review_grounding (E45.90 / #3625) is set alongside it and for the
	// same reason: it is a fact about fishhawkd's wiring, not about the repo.
	rg := reviewGroundingReadinessFor(s.cfg.ReviewGroundingDisabled)
	// (9) work_item_provider (E45.94 / #3646) is set in the SAME pre-switch
	// block, but for a subtly different reason: it is HYBRID-scoped. The
	// resolved provider id is a REPO fact (the repo's work-management
	// conventions) while the registered set is a DEPLOYMENT fact, so the rung
	// is neither a pure deployment rung nor a member of the app/spec cascade —
	// yet it must survive every repo-scoped cascade, which is what placing it
	// here buys. The conventionsLoader read is cheap: RepoConventionsLoader is
	// TTL-cached per (provider, repo) and falls back to workmgmt.Default()
	// whenever the repo commits no conventions file.
	conv, convErr := conventionsLoader(r.Context(), repo)
	// campaign_sources (#3658) is computed from the resolved provider INSTANCE
	// (compile-time capability assertions), so it tracks the build: the gitlab
	// provider reports ["epic_ref","items"] since it implements both sources.
	// No instance (conventions unresolved, provider unregistered) → nil, which
	// the pure resolver renders as [] — no capability claim.
	var campaignSources []string
	if convErr == nil {
		if p, err := workmgmt.Get(conv.Provider); err == nil {
			campaignSources = campaignSourcesSupported(p)
		}
	}
	wp := workItemProviderReadinessFor(conv.Provider, workmgmt.Registered(), campaignSources, convErr)
	resp := onboardingReadinessResponse{
		Repo:             repo,
		Forge:            family,
		TraceStore:       &ts,
		ReviewGrounding:  &rg,
		WorkItemProvider: &wp,
	}
	var parsedSpec *spec.Spec
	switch family {
	case observationForgeGitLab:
		// (1)+(2) on GitLab; the GitHub-shaped (5) merge_gate is OMITTED —
		// see the response type's doc comment for why nil is the only honest
		// rendering — and the GitLab-shaped (5) gitlab_merge_gate takes its
		// place (E45.66 / #3580) on the forge + scope probeGitLab resolved.
		var glForge forge.Forge
		var glScope forge.CredentialScope
		resp.App, resp.Spec, parsedSpec, glForge, glScope = s.probeGitLab(r.Context(), repo, repoRef)
		resolvable := resp.App.Resolvable != nil && *resp.App.Resolvable
		// (6) gitlab_registration (E45.68 / #3582): consulted on EVERY
		// gitlab-family report — including the forge-unconfigured one, where
		// the registry can still answer — and NEVER on github. The resolved
		// ref is empty unless the project resolved, so the remediation
		// carries the real project id only when it is known.
		var resolvedRef string
		if resolvable {
			resolvedRef = glScope.Ref()
		}
		reg := s.probeGitLabRegistration(r.Context(), repo, resolvedRef)
		resp.GitLabRegistration = &reg
		// (1) on gitlab: installed = registered for exactly this path AND
		// resolvable. Forge/resolve reasons were set first and win; only a
		// resolvable project with no (or an unknowable) registration draws
		// the registration reason.
		resp.App.Installed = reg.Status == gitLabRegistrationStatusRegistered && resolvable
		if !resp.App.Installed && resp.App.Reason == "" {
			switch reg.Status {
			case gitLabRegistrationStatusNotRegistered:
				resp.App.Reason = onboardingGitLabNotRegisteredReason
			default:
				resp.App.Reason = onboardingGitLabRegistryUnknownReasonPrefix + reg.Reason + onboardingGitLabRegistryUnknownReasonSuffix
			}
		}
		resp.App.Note = onboardingGitLabInstalledNote
		mg := s.probeGitLabMergeGate(r.Context(), repo, glForge, glScope, repoRef, resolvable)
		resp.GitLabMergeGate = &mg
	default:
		var installed bool
		var installationID int64
		resp.App, resp.Spec, parsedSpec, installed, installationID = s.probeGitHub(r.Context(), repo, repoRef)
		// (5) Merge gate: is the check Fishhawk publishes actually required by
		// the repo's protection on its REAL default branch (#3161)? Needs an
		// installation token, so it runs only once the App is installed; every
		// other posture degrades to `unknown` with a naming reason rather than
		// to `not_required`.
		mg := s.probeMergeGate(r.Context(), repo, repoRef, installed, installationID)
		resp.MergeGate = &mg
	}

	// (3) Per-reviewer availability, (4) caller-token scope adequacy.
	resp.Reviewers = s.probeReviewers(r.Context(), parsedSpec)
	resp.Scopes = probeScopes(ident)

	s.writeJSON(w, r, http.StatusOK, resp)
}

// collectSpecReviewers enumerates the distinct (provider, model,
// reasoning_effort) reviewer tuples declared across every stage's
// reviewers.agents list in the spec, de-duped by that composite key — the
// same tuple identity unavailableSpecReviewers (runs.go) probes. Results are
// sorted by the composite key so the readiness response is deterministic
// regardless of Go's map-iteration order over sp.Workflows.
func collectSpecReviewers(sp *spec.Spec) []spec.AgentReviewer {
	seen := make(map[string]struct{})
	var out []spec.AgentReviewer
	for _, wf := range sp.Workflows {
		for _, st := range wf.Stages {
			if st.Reviewers == nil {
				continue
			}
			for _, a := range st.Reviewers.Agents {
				key := a.Provider + "\x00" + a.Model + "\x00" + a.ReasoningEffort
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, a)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].ReasoningEffort < out[j].ReasoningEffort
	})
	return out
}
