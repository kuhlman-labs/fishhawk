package githubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// RepoPermission is a user's effective permission on one repository, as
// GET /repos/{o}/{r}/collaborators/{login}/permission reports it (#4620).
type RepoPermission struct {
	// RoleName is GitHub's role_name verbatim: admin, maintain, write,
	// triage, read, or a custom repository role's name. "" when absent.
	RoleName string
	// Permission is the legacy permission field verbatim: admin, write, read
	// or none. A maintain role reports write here; triage reports read.
	Permission string
}

// EffectiveRole ranks the two fields into one role (#4620): role_name when
// it is one of GitHub's five built-in roles (admin, maintain, write, triage,
// read), else the legacy permission (admin, write, read, none). A custom
// role therefore falls back to the legacy field, which is conservative for a
// write-equivalent custom role. "" when neither field carries a value.
func (p RepoPermission) EffectiveRole() string {
	switch p.RoleName {
	case "admin", "maintain", "write", "triage", "read":
		return p.RoleName
	}
	return p.Permission
}

// CollaboratorPermission reads login's effective permission on repo with the
// RUN's installation token (#4620, ADR-112 R1(c)).
//
//	GET /repos/{owner}/{repo}/collaborators/{login}/permission
//
// Both role_name and the legacy permission are returned verbatim;
// RepoPermission.EffectiveRole ranks them (role_name first, the legacy
// permission for a custom role). This is NOT the boot-singleton identity provider's
// permission read, whose 404→PermissionNone contract and the review gate's
// quorum path are untouched: here a 404 is ErrNotFound and the caller
// decides. A zero scope, a missing TokenProvider, a blank owner/name, or a
// blank login or one containing `/` is refused before any request; the
// login is path-escaped. ErrForbidden on auth issues; any other non-2xx is
// an untyped status error via classifyStatus.
func (c *Client) CollaboratorPermission(ctx context.Context, scope forge.CredentialScope, repo RepoRef, login string) (RepoPermission, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return RepoPermission{}, err
	}
	if c.Tokens == nil {
		return RepoPermission{}, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return RepoPermission{}, errors.New("githubclient: repo owner and name required")
	}
	if strings.TrimSpace(login) == "" {
		return RepoPermission{}, errors.New("githubclient: collaborator login is required")
	}
	if strings.Contains(login, "/") {
		return RepoPermission{}, fmt.Errorf("githubclient: collaborator login %q must not contain '/'", login)
	}

	endpoint := c.endpoint("/repos/" + url.PathEscape(repo.Owner) +
		"/" + url.PathEscape(repo.Name) +
		"/collaborators/" + url.PathEscape(login) + "/permission")
	req, err := c.buildRequest(ctx, http.MethodGet, endpoint, nil, installationID)
	if err != nil {
		return RepoPermission{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return RepoPermission{}, fmt.Errorf("githubclient: get collaborator permission: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := classifyStatus("get collaborator permission", resp); err != nil {
		return RepoPermission{}, err
	}
	var body struct {
		Permission string `json:"permission"`
		RoleName   string `json:"role_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return RepoPermission{}, fmt.Errorf("githubclient: decode collaborator permission: %w", err)
	}
	return RepoPermission{RoleName: body.RoleName, Permission: body.Permission}, nil
}
