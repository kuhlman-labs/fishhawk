/*
 * standard_v1 plan artifact. Mirrors docs/spec/plan-standard-v1.schema.json.
 * Frozen at Day 21 (~2026-05-20) per MVP_SPEC §8 — every future schema
 * version (standard_v2, ...) lands as a sibling type and is selected
 * by Artifact.schema_version, never by mutating this one in place.
 */
export type PlanVersion = 'standard_v1';

export interface ScopeFile {
  path: string;
  operation: 'create' | 'modify' | 'delete';
}

export interface ApproachStep {
  step: number;
  description: string;
}

export interface StandardV1Plan {
  plan_version: PlanVersion;
  ticket_reference: {
    type: 'github_issue';
    url: string;
    id: string;
  };
  generated_by: {
    agent: string;
    model: string;
    version?: string;
    timestamp: string;
  };
  summary: string;
  scope: {
    files: ScopeFile[];
    estimated_lines_changed?: number;
  };
  approach: ApproachStep[];
  verification: {
    test_strategy: string;
    rollback_plan: string;
  };
  risks_and_assumptions?: string[];
}

/**
 * A narrow check sufficient to gate on before treating an artifact's
 * content as a standard_v1 plan. The backend has already validated the
 * full schema on ingest, so this exists only to defend against drift
 * (e.g., a future runner emitting standard_v2 that the SPA hasn't
 * learned to render yet).
 */
export function isStandardV1Plan(content: unknown): content is StandardV1Plan {
  if (typeof content !== 'object' || content === null) return false;
  const c = content as Record<string, unknown>;
  return c.plan_version === 'standard_v1';
}
