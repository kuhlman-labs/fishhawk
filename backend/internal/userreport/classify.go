// Package userreport is the user-report source reader (E81.1 / #3771): it
// classifies the issues and issue comments a workmgmt.UserReportReader lists
// since a per-repository cursor as fishhawk_filed, bot, internal or external,
// hands the result to a caller's Recorder, and advances the forge-anchored
// cursor (user_report_cursors, migration 0096) only after the record
// succeeds. The one production caller is the server's comms scan gather
// (E81.5 / #4014), which runs Scan through a deferring cursor store so a
// prompt serve never advances the cursor.
// comms.go adds the comms draft-marker primitives (DraftMarker,
// ParseDraftMarkers, ContentHash); the marker is attacker-writable body text,
// trusted only on an item classified fishhawk_filed.
// Contract: README.md.
package userreport

import (
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Classification is the closed set of author classes a report item carries.
type Classification string

const (
	// ClassFishhawkFiled is an item Fishhawk itself filed: its body carries a
	// recognised Fishhawk provenance marker AND its author would not
	// otherwise classify external.
	ClassFishhawkFiled Classification = "fishhawk_filed"
	// ClassBot is a forge-evidenced bot author.
	ClassBot Classification = "bot"
	// ClassInternal is a write-capable-trust author (the provider's Internal
	// rule) or the repository's current captain on the same forge.
	ClassInternal Classification = "internal"
	// ClassExternal is everyone else, including any author whose association
	// could not be resolved.
	ClassExternal Classification = "external"
)

// Basis values Classify reports alongside the class. An association basis is
// "association:<forge association>"; a marker basis is "marker:<marker name>".
const (
	BasisBot                   = "bot"
	BasisSystemNote            = "system_note"
	BasisCaptain               = "captain"
	BasisAssociationUnresolved = "association_unresolved"
	BasisDefault               = "default"
)

// recognisedMarker is one Fishhawk provenance marker: the exact prefix a
// Fishhawk-written body carries, and the short name a basis reports.
type recognisedMarker struct {
	name   string
	prefix string
}

// recognisedMarkers is the CLOSED list of Fishhawk provenance markers. A body
// containing any prefix is Fishhawk-filed — subject to the author guard in
// Classify, because a marker is attacker-writable body text.
//
//   - intake: intakegroom.MarkerPrefix, imported (single source).
//   - upkeep: the prefix of upkeep.FindingMarker's output (unexported there;
//     pinned against FindingMarker by TestClassify_MarkerPrefixesMatchProducers).
//   - fingerprint: the product-report marker workmgmt/github/feedback.go
//     writes (unexported renderer; literal).
//   - sticky: the issuecomment sticky-comment marker (unexported renderer;
//     literal).
//   - comms: CommsMarkerPrefix, this package's DraftMarker output (E81.5 /
//     #3775; pinned against DraftMarker by
//     TestClassify_MarkerPrefixesMatchProducers).
var recognisedMarkers = []recognisedMarker{
	{name: "fishhawk-intake:v1", prefix: intakegroom.MarkerPrefix},
	{name: "fishhawk-upkeep:v1", prefix: "<!-- fishhawk-upkeep:v1 finding_id="},
	{name: "fishhawk-fingerprint", prefix: "<!-- fishhawk-fingerprint:"},
	{name: "fishhawk-sticky", prefix: "<!-- fishhawk-sticky "},
	{name: CommsMarkerName, prefix: CommsMarkerPrefix},
}

// markerIn returns the name of the first recognised marker body carries, or
// "" when it carries none.
func markerIn(body string) string {
	for _, m := range recognisedMarkers {
		if strings.Contains(body, m.prefix) {
			return m.name
		}
	}
	return ""
}

// ClassifyContext is what Classify needs beyond the item: the captain's bare
// login on the page's forge (CaptainLoginFor), empty when there is no usable
// captain — which disables the captain arm.
type ClassifyContext struct {
	CaptainLogin string
}

// Result is one item's classification. Basis is a short machine-readable
// reason. MarkerFromExternal is true when the body carries a recognised
// Fishhawk marker but the author classifies external — a possible forgery the
// marker did NOT earn fishhawk_filed for.
type Result struct {
	Class              Classification
	Basis              string
	MarkerFromExternal bool
}

// Classify applies the user-report classification rule:
//
//	fishhawk_filed > bot > internal > external
//
// fishhawk_filed requires a recognised marker AND an author who would
// otherwise classify bot or internal; an external author's marker stays
// external with MarkerFromExternal set. bot is the provider's forge-evidenced
// Bot flag. internal is the provider's Internal flag (never set for an
// unresolved association) or a login equal, case-insensitively, to the
// captain's login on the same forge. Everything else is external.
//
// A SYSTEM note (item.System) is always bot and NEVER fishhawk_filed: its
// body is forge-rendered around actor-controlled text (a title edit quotes
// the new title), so a marker in it proves nothing about who wrote it. A
// marker on a system note sets MarkerFromExternal so a consumer can flag it.
func Classify(it workmgmt.UserReportItem, cc ClassifyContext) Result {
	name := markerIn(it.Body)
	if it.System {
		return Result{Class: ClassBot, Basis: BasisSystemNote, MarkerFromExternal: name != ""}
	}
	base := authorClass(it.Author, cc)
	if name == "" {
		return base
	}
	if base.Class == ClassExternal {
		base.MarkerFromExternal = true
		return base
	}
	return Result{Class: ClassFishhawkFiled, Basis: "marker:" + name}
}

func authorClass(a workmgmt.ReportAuthor, cc ClassifyContext) Result {
	switch {
	case a.Bot:
		return Result{Class: ClassBot, Basis: BasisBot}
	case a.Internal:
		return Result{Class: ClassInternal, Basis: "association:" + a.Association}
	case cc.CaptainLogin != "" && a.Login != "" && strings.EqualFold(a.Login, cc.CaptainLogin):
		return Result{Class: ClassInternal, Basis: BasisCaptain}
	case !a.AssociationResolved:
		return Result{Class: ClassExternal, Basis: BasisAssociationUnresolved}
	case a.Association != "":
		return Result{Class: ClassExternal, Basis: "association:" + a.Association}
	default:
		return Result{Class: ClassExternal, Basis: BasisDefault}
	}
}

// CaptainLoginFor maps a captain subject to the bare login it names on forge
// ("github" | "gitlab"): the subject must be provider-qualified
// (captain.IdentityVerified) AND qualified for THAT forge. A static token
// subject, or a subject of another forge, returns "" — a github: login and a
// gitlab: login of the same spelling are different people.
func CaptainLoginFor(subject, forge string) string {
	if forge == "" || !captain.IdentityVerified(subject) {
		return ""
	}
	prefix := forge + ":"
	if !strings.HasPrefix(subject, prefix) {
		return ""
	}
	return strings.TrimPrefix(subject, prefix)
}
