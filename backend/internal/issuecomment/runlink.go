package issuecomment

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Every issue-comment surface (the living anchor, the sticky status comment,
// the sticky PR status comment, the advisory PR review, the page-class pings,
// and the CI-retry / budget-alert bodies) renders a link to the run's dashboard
// page derived from the configured base URL (FISHHAWKD_EXTERNAL_URL). The
// dogfood/dev posture leaves that base URL UNSET rather than pointing it at an
// operator-host-local address like http://localhost:5173 — because a rendered
// absolute link to a localhost host posts a dead, internal-topology-leaking URL
// on GitHub (#1787).
//
// The helpers below are the SINGLE decision point every surface reuses so an
// unset base URL degrades a run reference to a plain, link-less short-id (and a
// footer "view run" link is omitted entirely, along with its middot separator)
// rather than any localhost literal, while a configured base URL keeps the
// absolute link. "Unset" is the empty string; a configured value (even one that
// happens to be localhost) still renders the absolute link — the contract is
// "omit when unset", and the dogfood fix is to leave the URL unset.

// runShortLink renders the run's short-id as a markdown link to the run page
// when externalURL is configured, or as a bare backticked short-id (no link)
// when externalURL is empty. Used by every surface header and inline run
// reference (anchor / status / PR-status headers, CI-retry and budget-alert
// bodies).
func runShortLink(externalURL string, id uuid.UUID) string {
	short := shortID(id)
	base := strings.TrimRight(externalURL, "/")
	if base == "" {
		return "`" + short + "`"
	}
	return fmt.Sprintf("[`%s`](%s/runs/%s)", short, base, id.String())
}

// viewRunLink renders a footer / attribution "view run" link carrying the given
// label (the label includes any arrow glyph verbatim, e.g. "View run →") when
// externalURL is configured, or "" when it is empty. Returning "" is the
// omit-the-link branch every footer reuses: the caller drops the link from the
// middot-joined parts, so no dangling separator is left.
func viewRunLink(label, externalURL string, id uuid.UUID) string {
	base := strings.TrimRight(externalURL, "/")
	if base == "" {
		return ""
	}
	return fmt.Sprintf("[%s](%s/runs/%s)", label, base, id.String())
}

// runURLFor returns the bare run-page URL when externalURL is configured, or ""
// when it is empty. The empty string flows into commentContext.runURL so the
// CI-retry / budget-alert / ping renderers branch uniformly on emptiness
// instead of each rebuilding a relative /runs/<id> path that would render as a
// hostless dead link.
func runURLFor(externalURL string, id uuid.UUID) string {
	base := strings.TrimRight(externalURL, "/")
	if base == "" {
		return ""
	}
	return base + "/runs/" + id.String()
}
