package webhook

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// GitLab-specific errors callers may switch on. They mirror the
// GitHub error vocabulary (ErrSignatureMissing / ErrSignatureInvalid /
// ErrSecretNotConfigured) so the server receiver can translate them
// to the same status codes (401 / 503) with a parallel check order.
var (
	// ErrGitLabTokenMissing is returned when X-Gitlab-Token is absent
	// or empty. GitLab omits the header entirely when no secret token
	// is configured on the hook; a configured receiver treats that as
	// unauthenticated.
	ErrGitLabTokenMissing = errors.New("webhook: X-Gitlab-Token missing")
	// ErrGitLabTokenInvalid is returned when X-Gitlab-Token is present
	// but does not match the configured secret.
	ErrGitLabTokenInvalid = errors.New("webhook: X-Gitlab-Token invalid")
	// ErrGitLabEventMissing is returned when X-Gitlab-Event is absent.
	ErrGitLabEventMissing = errors.New("webhook: X-Gitlab-Event missing")
	// ErrGitLabEventUUIDMissing is returned when X-Gitlab-Event-UUID —
	// the per-delivery id we namespace into the shared DeliveryStore —
	// is absent. Fail-closed: a delivery with no id can't be deduped,
	// so we reject it rather than risk a silent dedup bypass.
	ErrGitLabEventUUIDMissing = errors.New("webhook: X-Gitlab-Event-UUID missing")
)

// gitLabDeliveryPrefix namespaces the X-Gitlab-Event-UUID delivery id
// inside the shared DeliveryStore the GitHub path also uses. Without
// the prefix a GitLab delivery UUID and a GitHub delivery UUID could
// (astronomically unlikely, but by-construction possible) collide and
// one forge's event would be silently deduped as the other's.
const gitLabDeliveryPrefix = "gitlab:"

// gitLabScopeRefPrefix is the "gitlab:<project_id>" credential-scope
// ref prefix. It MUST stay in lockstep with forge/gitlab's
// scopeRefPrefix (E45.5) so a receiver-minted scope is parseable by
// the GitLab forge's projectIDFromScope. Hard-coded here rather than
// imported to keep this package free of a forge/gitlab dependency
// (the forge adapter already depends on nothing in webhook, and we
// don't want to introduce the reverse edge).
const gitLabScopeRefPrefix = "gitlab:"

// VerifyGitLabToken checks that header carries the configured secret.
// GitLab sends the webhook's secret token VERBATIM in the
// X-Gitlab-Token header — there is NO HMAC over the body (per
// https://docs.gitlab.com/user/project/integrations/webhooks/#validate-payloads-by-using-a-secret-token).
// The comparison is constant-time so a wrong token can't be leaked
// byte-by-byte via timing.
//
// Returns ErrSecretNotConfigured when secret is empty (the receiver
// translates that to 503), ErrGitLabTokenMissing for an empty header,
// and ErrGitLabTokenInvalid for a present-but-wrong token; the
// receiver maps both token errors to 401.
func VerifyGitLabToken(secret []byte, header string) error {
	if len(secret) == 0 {
		return ErrSecretNotConfigured
	}
	if header == "" {
		return ErrGitLabTokenMissing
	}
	if subtle.ConstantTimeCompare([]byte(header), secret) != 1 {
		return ErrGitLabTokenInvalid
	}
	return nil
}

// gitLabMinimal is the permissive subset of fields ParseGitLabEvent
// extracts across the five accepted object_kinds. JSON unmarshal is
// tolerant of absent fields, so one struct covers merge_request /
// note / issue / pipeline / build — a field missing for a given kind
// simply decodes to its zero value.
type gitLabMinimal struct {
	ObjectKind string `json:"object_kind"`
	Project    struct {
		ID              int    `json:"id"`
		PathWithNamespc string `json:"path_with_namespace"`
	} `json:"project"`
	User struct {
		Username string `json:"username"`
	} `json:"user"`
	ObjectAttributes struct {
		Action string `json:"action"`
		Status string `json:"status"`
	} `json:"object_attributes"`
	// BuildStatus is the Job (object_kind "build") event's status
	// field — Job events carry their status at the top level, not
	// under object_attributes.
	BuildStatus string `json:"build_status"`
}

// ParseGitLabEvent decodes a GitLab webhook delivery into the shared
// Event envelope. eventType is the X-Gitlab-Event header value (e.g.
// "Merge Request Hook"); eventUUID is the X-Gitlab-Event-UUID
// per-delivery id. Both headers are required — a missing one is a
// fail-closed error the receiver maps to 400 (never a silent accept).
//
// Type is normalized to the payload's object_kind (merge_request /
// note / issue / pipeline / build) rather than the header, because
// the matcher branches on object_kind and the body is the more stable
// contract. Action comes from object_attributes.action for MR/issue,
// object_attributes.status for pipeline, and build_status for build
// (note carries no action). DeliveryID is namespaced
// "gitlab:<uuid>"; CredentialRef is "gitlab:<project_id>"; Forge is
// "gitlab". Like the GitHub parser, absent JSON fields yield zero
// values — only the missing headers error.
func ParseGitLabEvent(eventType, eventUUID string, body []byte) (Event, error) {
	if eventType == "" {
		return Event{}, ErrGitLabEventMissing
	}
	if eventUUID == "" {
		return Event{}, ErrGitLabEventUUIDMissing
	}
	ev := Event{
		DeliveryID: gitLabDeliveryPrefix + eventUUID,
		Forge:      ForgeGitLab,
		RawBody:    body,
	}
	if len(body) > 0 {
		var m gitLabMinimal
		if err := json.Unmarshal(body, &m); err != nil {
			return ev, fmt.Errorf("webhook: parse gitlab body: %w", err)
		}
		ev.Type = m.ObjectKind
		ev.Repo = m.Project.PathWithNamespc
		ev.Sender = m.User.Username
		ev.Action = gitLabAction(m)
		if m.Project.ID != 0 {
			ev.CredentialRef = gitLabScopeRefPrefix + strconv.Itoa(m.Project.ID)
		}
	}
	return ev, nil
}

// gitLabAction selects the forge-neutral Action for a GitLab event
// from the kind-specific status/action field: object_attributes.action
// for merge_request/issue, object_attributes.status for pipeline,
// build_status for the Job (build) event, and empty for note (which
// has no lifecycle action).
func gitLabAction(m gitLabMinimal) string {
	switch m.ObjectKind {
	case "pipeline":
		return m.ObjectAttributes.Status
	case "build":
		return m.BuildStatus
	default:
		return m.ObjectAttributes.Action
	}
}
