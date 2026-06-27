/*
 * Wire shape for the deploy-stage `deployment` artifact (E23.9 / #1389).
 * Mirrors DeploymentArtifactBody in docs/api/v0.openapi.yaml — keep the
 * two in sync on every backend change to the upload handler at
 * POST /v0/runs/{run_id}/deployment.
 *
 * Fishhawk is delegating-only: the external pipeline runs the deploy
 * and this body reports its outcome (ADR-038 / #1384). v0 carries no
 * schema_version on this artifact (the field shape isn't frozen yet);
 * the SPA gates on a structural check instead, same as the
 * pull_request artifact. When a versioned `deployment_v1` lands, add a
 * sibling type and select by Artifact.schema_version like plan does.
 */

/** Terminal deploy disposition. `partial` / `rolled_back` are never blindly re-run. */
export type DeploymentOutcome = 'succeeded' | 'failed' | 'partial' | 'rolled_back';

/** Present when the body reports an explicit rollback sub-action against a prior deploy. */
export type DeploymentRollbackAction = 'initiated' | 'completed';

export interface DeploymentArtifactBody {
  /** The deploy target (gated pre-execution by the allowed_environments constraint). */
  environment: string;
  /** The git ref or sha that was deployed (ADR-038's "ref/sha"). */
  ref: string;
  /** The external pipeline run Fishhawk delegated to (delegating mode). */
  external_run_url: string;
  outcome: DeploymentOutcome;
  /** Opaque handle the external pipeline returns for reverting this deploy, when one exists. */
  rollback_handle?: string;
  rollback_action?: DeploymentRollbackAction;
}

/**
 * Narrow check that an artifact's `content` is shaped like a
 * deployment artifact body. Asserts only the required fields the v0
 * OpenAPI DeploymentArtifactBody marks required
 * (environment, ref, external_run_url, outcome) so the guard stays
 * robust against the optional rollback_* additions on the wire.
 *
 * Defends against drift the same way isPullRequestArtifact does: the
 * backend has already validated the wire shape on ingest, so a false
 * here means a bug we want to surface (the unrecognized-shape
 * warning), not silently render half a panel.
 */
export function isDeploymentArtifact(content: unknown): content is DeploymentArtifactBody {
  if (typeof content !== 'object' || content === null) return false;
  const c = content as Record<string, unknown>;
  return (
    typeof c.environment === 'string' &&
    typeof c.ref === 'string' &&
    typeof c.external_run_url === 'string' &&
    typeof c.outcome === 'string'
  );
}
