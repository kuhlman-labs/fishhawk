import type {
  ApiError,
  ApprovalRequest,
  Artifact,
  AuditEntry,
  PaginatedList,
  Run,
  Stage,
} from './types';

/*
 * Thin fetch wrapper. Same-origin (Vite proxies /v0 → fishhawkd in
 * dev; in prod the SPA is served by the same backend), so the
 * fishhawk_session cookie rides along with credentials: 'include'
 * and we don't pass anything else. ADR-005.
 *
 * On non-2xx, throws ApiClientError with the parsed error envelope so
 * callers can branch on .status (401 → redirect, 404 → not-found UI,
 * etc.) without re-parsing the body.
 */
export class ApiClientError extends Error {
  readonly status: number;
  readonly body: ApiError | null;

  constructor(status: number, body: ApiError | null, message: string) {
    super(message);
    this.name = 'ApiClientError';
    this.status = status;
    this.body = body;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    credentials: 'include',
    headers: { Accept: 'application/json' },
    ...init,
  });

  if (!res.ok) {
    let body: ApiError | null = null;
    try {
      body = (await res.json()) as ApiError;
    } catch {
      // Non-JSON error body (e.g., plain text from a proxy). Fine.
    }
    const msg = body?.message ?? body?.error ?? `request failed: ${res.status}`;
    throw new ApiClientError(res.status, body, msg);
  }

  if (res.status === 204) {
    return undefined as T;
  }
  return (await res.json()) as T;
}

export const api = {
  listRuns(params?: {
    limit?: number;
    cursor?: string;
    repo?: string;
  }): Promise<PaginatedList<Run>> {
    const q = new URLSearchParams();
    if (params?.limit) q.set('limit', String(params.limit));
    if (params?.cursor) q.set('cursor', params.cursor);
    if (params?.repo) q.set('repo', params.repo);
    const qs = q.toString();
    return request(`/v0/runs${qs ? `?${qs}` : ''}`);
  },

  getRun(runId: string): Promise<Run> {
    return request(`/v0/runs/${encodeURIComponent(runId)}`);
  },

  listRunStages(runId: string): Promise<{ items: Stage[] }> {
    return request(`/v0/runs/${encodeURIComponent(runId)}/stages`);
  },

  getStage(stageId: string): Promise<Stage> {
    return request(`/v0/stages/${encodeURIComponent(stageId)}`);
  },

  listStageArtifacts(stageId: string): Promise<{ items: Artifact[] }> {
    return request(`/v0/stages/${encodeURIComponent(stageId)}/artifacts`);
  },

  getArtifact<C = unknown>(artifactId: string): Promise<Artifact<C>> {
    return request(`/v0/artifacts/${encodeURIComponent(artifactId)}`);
  },

  submitApproval(stageId: string, body: ApprovalRequest): Promise<Stage> {
    return request(`/v0/stages/${encodeURIComponent(stageId)}/approvals`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  },

  listRunAudit(
    runId: string,
    params?: { limit?: number; cursor?: string; category?: string },
  ): Promise<PaginatedList<AuditEntry>> {
    const q = new URLSearchParams();
    if (params?.limit) q.set('limit', String(params.limit));
    if (params?.cursor) q.set('cursor', params.cursor);
    if (params?.category) q.set('category', params.category);
    const qs = q.toString();
    return request(`/v0/runs/${encodeURIComponent(runId)}/audit${qs ? `?${qs}` : ''}`);
  },
};
