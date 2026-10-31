package githubclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// mergeMethodsQuery reads the repository's documented merge-method settings
// (GraphQL Repository.mergeCommitAllowed / squashMergeAllowed /
// rebaseMergeAllowed).
const mergeMethodsQuery = `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    mergeCommitAllowed
    squashMergeAllowed
    rebaseMergeAllowed
  }
}`

// AllowedMergeMethods reads which merge methods the repository's settings allow
// (E93.2 / #4658) with ONE GraphQL query on the RUN's installation token. The
// server's ForgeMerger uses it only to ATTRIBUTE a typed merge refusal to a
// disallowed declared method — never to decide a merge — so every failure is
// an error, never a guessed value: a zero scope, missing TokenProvider or blank
// owner/name is refused before any request; a transport error, a non-2xx
// (classifyStatus), a GraphQL `errors` envelope (ErrValidation), a null
// repository (ErrNotFound) or ANY of the three fields absent or null fails
// closed.
func (c *Client) AllowedMergeMethods(ctx context.Context, scope forge.CredentialScope, repo RepoRef) (forge.AllowedMergeMethods, error) {
	installationID, err := installationIDForScope(scope)
	if err != nil {
		return forge.AllowedMergeMethods{}, err
	}
	if c.Tokens == nil {
		return forge.AllowedMergeMethods{}, errors.New("githubclient: client missing TokenProvider")
	}
	if repo.Owner == "" || repo.Name == "" {
		return forge.AllowedMergeMethods{}, errors.New("githubclient: repo owner and name required")
	}
	var data struct {
		Repository *struct {
			MergeCommitAllowed *bool `json:"mergeCommitAllowed"`
			SquashMergeAllowed *bool `json:"squashMergeAllowed"`
			RebaseMergeAllowed *bool `json:"rebaseMergeAllowed"`
		} `json:"repository"`
	}
	if err := c.doGraphQL(ctx, installationID, mergeMethodsQuery, map[string]any{
		"owner": repo.Owner,
		"name":  repo.Name,
	}, &data); err != nil {
		return forge.AllowedMergeMethods{}, fmt.Errorf("githubclient: merge settings: %w", err)
	}
	r := data.Repository
	if r == nil {
		return forge.AllowedMergeMethods{}, fmt.Errorf("%w: merge settings: repository %s/%s not visible", ErrNotFound, repo.Owner, repo.Name)
	}
	if r.MergeCommitAllowed == nil || r.SquashMergeAllowed == nil || r.RebaseMergeAllowed == nil {
		return forge.AllowedMergeMethods{}, fmt.Errorf("githubclient: merge settings: repository %s/%s: a merge-method setting is absent from the response", repo.Owner, repo.Name)
	}
	return forge.AllowedMergeMethods{
		Squash: *r.SquashMergeAllowed,
		Merge:  *r.MergeCommitAllowed,
		Rebase: *r.RebaseMergeAllowed,
	}, nil
}
