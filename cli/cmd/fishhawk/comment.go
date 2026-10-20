package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/ghcomment"
	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// postOrEditStatusComment is a test seam for local-runner sticky comment
// posting (#428). Production wires to ghcomment.PostOrEditStatusComment;
// tests swap to capture calls without shelling to gh or reaching the backend.
// token is the bearer the caller's httpclient.Client already resolved (flag,
// env or credstore); the record-id POST requires write:runs (#4483).
var postOrEditStatusComment = func(backendURL, token, runID, repo string, issueNumber int) error {
	return ghcomment.PostOrEditStatusComment(backendURL, token, runID, repo, issueNumber)
}

// fetchRunForComment loads the run by id. Returns nil on any error
// (best-effort: comment failures don't propagate). Silent on error
// because the surrounding verb already reported success; a follow-up
// "couldn't load run for the comment" would be a distracting tail
// when the operator already saw the canonical result.
func fetchRunForComment(ctx context.Context, client *httpclient.Client, runID uuid.UUID) *httpclient.Run {
	if client == nil {
		return nil
	}
	r, err := client.GetRun(ctx, runID)
	if err != nil {
		return nil
	}
	return r
}
