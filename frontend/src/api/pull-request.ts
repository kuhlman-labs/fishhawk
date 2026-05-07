/*
 * Wire shape for the implement-stage `pull_request` artifact.
 * Mirrors PullRequestArtifactBody in docs/api/v0.openapi.yaml — keep
 * the two in sync on every backend change to the upload handler at
 * backend/internal/server/pullrequest.go.
 *
 * v0 has no schema_version on this artifact (the field shape isn't
 * frozen yet); the SPA gates on a structural check instead. When a
 * versioned `pull_request_v1` lands, add a sibling type and select
 * by Artifact.schema_version like plan does.
 */
export interface PullRequestArtifactBody {
  pr_number: number;
  pr_url: string;
  branch: string;
  head_sha: string;
  base_sha: string;
  title: string;
  /** Optional PR body (markdown). The runner ships an empty string when the agent provided no body. */
  body?: string;
  files_changed_count: number;
}

/**
 * Narrow check that an artifact's `content` is shaped like a PR
 * artifact body. Asserts only the required-non-null fields the UI
 * actively renders so the type guard stays robust against optional
 * additions on the wire.
 *
 * Defends against drift the same way isStandardV1Plan does: the
 * backend has already validated the wire shape on ingest, so a false
 * here means a bug we want to surface, not silently render half a
 * page.
 */
export function isPullRequestArtifact(content: unknown): content is PullRequestArtifactBody {
  if (typeof content !== 'object' || content === null) return false;
  const c = content as Record<string, unknown>;
  return (
    typeof c.pr_number === 'number' &&
    typeof c.pr_url === 'string' &&
    typeof c.branch === 'string' &&
    typeof c.head_sha === 'string' &&
    typeof c.base_sha === 'string' &&
    typeof c.title === 'string' &&
    typeof c.files_changed_count === 'number'
  );
}
