package server

// This file is the ADR-143 D2 registry of human-only actions: the closed list
// of actions the D1 credential-kind test (`session` only) governs, plus the
// recorded exclusions with their reasons. The guards read it
// (humanOnlyActionEnforced) and two tests read it:
// TestHumanOnlyActions_EnforcedSetPinned pins the set of enforced entries, and
// TestHumanOnlyActions_CompletenessOverAgentRefusals checks that every
// function refusing a caller on isAgentSubject / operatorrole.IsTokenSubject /
// captainActorIsAgent, or on a delegated flag, is named here as a D2 site or
// an exclusion site (or in the test's own table of non-refusing classifiers).
//
// Mark rules (ADR-143 D2): every D2 action is listed from the first change and
// only its enforced mark varies. A mark is SET only by the action's enabling
// change and CLEARED only by an amendment of ADR-143. A later verb that clears
// a server-check concern MUST be added to serverCheckClearAction's verbs in
// the change that adds it, so it is enforced from that change. The list grows
// only by amendment.

// humanOnlyActionServerCheckClear is the id of the one D2 entry: clearing a
// server-check concern by any verb.
const humanOnlyActionServerCheckClear = "server_check_clear"

// humanOnlyAction is one ADR-143 D2 entry.
type humanOnlyAction struct {
	// ID is the stable registry id.
	ID string
	// Basis cites the records that reserve the action to a human.
	Basis string
	// Verbs are the clearing verbs the entry covers (the clearVerb* values
	// passed to refuseNonHumanServerCheckClear).
	Verbs []string
	// Sites are the production functions that enforce the entry.
	Sites []string
	// Enforced is the D1 mark: true once the enabling change has landed.
	Enforced bool
}

// humanOnlyExclusion is one ADR-143 D2 recorded exclusion: an action that
// keeps its present guard, with the clause, the reason and the production
// functions whose refusal it covers.
type humanOnlyExclusion struct {
	Clause string
	Reason string
	Sites  []string
}

// serverCheckClearAction is the single D2 entry (ADR-143 D2; ADR-084 rule 5
// and Amendment A1). Its enabling change is #5228.
var serverCheckClearAction = humanOnlyAction{
	ID:    humanOnlyActionServerCheckClear,
	Basis: "ADR-143 D2; ADR-084 rule 5 and Amendment A1",
	Verbs: []string{clearVerbWaive, clearVerbBulkWaive, clearVerbDefer, clearVerbResolve},
	Sites: []string{
		"server.refuseNonHumanServerCheckClear",
		"server.handleResolveConcerns",
	},
	Enforced: true,
}

// humanOnlyActions is the D2 list.
var humanOnlyActions = []humanOnlyAction{serverCheckClearAction}

// humanOnlyExclusions are ADR-143 D2's recorded exclusions (i)–(vii). A site
// is the package-qualified function name ("pkg.Func"; methods drop the
// receiver). Exclusions (i) and (v) name actions whose guard is not a subject
// or delegated refusal, so they carry no sites.
var humanOnlyExclusions = []humanOnlyExclusion{
	{
		Clause: "(i)",
		Reason: "ADR ratification: ADR-113 D2(a) admits only an oauth api token and refuses a cookie session, the reverse of D1; D2(b) admits an SSH signature whatever credential submits it (ADR-139 keeps both)",
	},
	{
		Clause: "(ii)",
		Reason: "stage approval gates, including the human-executor review gate: ADR-123 sets the credential rule for a gate that declares approvals.members",
		Sites:  []string{"server.refuseAgentOnHumanReviewGate"},
	},
	{
		Clause: "(iii)",
		Reason: "the escape-closure attestation: ADR-117 A1.3 decides its credential kinds",
		Sites:  []string{"server.escapeClose"},
	},
	{
		Clause: "(iv)",
		Reason: "actions whose guard refuses only run-bound and operator-agent tokens (grooming, upkeep and comms dispositions; campaign hotfix; review rerun): whether D1 applies to them is a separate decision",
		Sites: []string{
			"server.requireOperatorCapture",
			"server.handleCampaignHotfix",
			"server.handleRerunReview",
		},
	},
	{
		Clause: "(v)",
		Reason: "actions whose own decided record states a credential kind or a channel (ADR-126 D5 security dispositions, ADR-139 human acts)",
	},
	{
		Clause: "(vi)",
		Reason: "labelling a concern's judgement, and resolving with operator evidence a concern that is not a server-check concern: ADR-117 D1 sets no credential rule and Amendment A1 reaches only server-check concerns",
		Sites:  []string{"server.handleLabelConcern"},
	},
	{
		Clause: "(vii)",
		Reason: "the captain seat verbs, delegation confirm and lower, and first-officer seat attach and retire: ADR-083 decides captain identity and must be amended (after its record is backfilled) before D1 applies",
		Sites: []string{
			"server.captainActorIsAgent",
			"server.handleCaptainVerb",
			"server.handleDelegationConfirm",
			"server.handleDelegationLower",
			"server.authorizeSeatCaptainAct",
			"server.handleAttachFirstOfficerSeat",
			"server.handleRetireFirstOfficerSeat",
			"captain.guardActor",
			"delegationconfirm.GuardActor",
		},
	},
}

// humanOnlyActionEnforced reports whether the D2 entry id carries the D1
// enforced mark. An unknown id is not enforced.
func humanOnlyActionEnforced(id string) bool {
	for _, a := range humanOnlyActions {
		if a.ID == id {
			return a.Enforced
		}
	}
	return false
}
