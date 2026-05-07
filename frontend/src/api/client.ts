import { getCookie } from '@/lib/cookie';
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
 * CSRF cookie + header names (mirrors backend/internal/server/csrf.go).
 * Exported so tests can assert against the constants directly.
 */
export const CSRF_COOKIE_NAME = '__Host-csrf';
export const CSRF_HEADER_NAME = 'X-CSRF-Token';

const STATE_CHANGING_METHODS = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

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
  // Auto-attach the CSRF token on state-changing methods. The
  // backend's csrf middleware (E4.6) requires X-CSRF-Token to match
  // __Host-csrf for cookie-authed requests; bearer-token callers
  // (CLI, server-to-server) bypass server-side, so missing the
  // cookie just means we don't add the header. Caller-provided
  // headers win — letting an explicit override reach the server is
  // useful in tests.
  const method = (init?.method ?? 'GET').toUpperCase();
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (STATE_CHANGING_METHODS.has(method)) {
    const csrf = getCookie(CSRF_COOKIE_NAME);
    if (csrf) {
      headers[CSRF_HEADER_NAME] = csrf;
    }
  }

  const res = await fetch(path, {
    credentials: 'include',
    ...init,
    headers: { ...headers, ...(init?.headers as Record<string, string> | undefined) },
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

  retryStage(stageId: string): Promise<Stage> {
    return request(`/v0/stages/${encodeURIComponent(stageId)}/retry`, {
      method: 'POST',
    });
  },

  listRunAudit(
    runId: string,
    params?: { limit?: number; cursor?: string; category?: string; stageId?: string },
  ): Promise<PaginatedList<AuditEntry>> {
    const q = new URLSearchParams();
    if (params?.limit) q.set('limit', String(params.limit));
    if (params?.cursor) q.set('cursor', params.cursor);
    if (params?.category) q.set('category', params.category);
    if (params?.stageId) q.set('stage_id', params.stageId);
    const qs = q.toString();
    return request(`/v0/runs/${encodeURIComponent(runId)}/audit${qs ? `?${qs}` : ''}`);
  },

  /**
   * SPA-readable prompt render (#215). Same body as the runner's
   * signature-authed `getStagePrompt` endpoint, no
   * `X-Fishhawk-Signature` required. Used by the implement-stage
   * session view to show the constructed prompt the agent received.
   */
  getStagePromptRender(stageId: string): Promise<{
    stage_id: string;
    stage_type: string;
    prompt: string;
    prompt_hash: string;
  }> {
    return request(`/v0/stages/${encodeURIComponent(stageId)}/prompt-render`);
  },

  /**
   * Stream the most-recent redacted trace bundle for a stage (#218).
   * The endpoint serves gzipped JSON Lines bytes; modern browsers
   * auto-decompress when `Content-Encoding: gzip` is present, so the
   * caller receives plain JSONL text. Returns the raw Response so
   * the caller can fold over `response.body` (a streaming
   * ReadableStream) line-by-line — fits the transcript surface,
   * which renders progressively rather than buffering the whole
   * bundle.
   *
   * Bypasses the JSON-decoding `request` helper since the body is
   * not JSON; CSRF auto-attach is unnecessary here (GET).
   */
  async getStageTraceStream(stageId: string): Promise<Response> {
    const res = await fetch(`/v0/stages/${encodeURIComponent(stageId)}/trace`, {
      credentials: 'include',
      headers: { Accept: 'application/x-ndjson' },
    });
    if (!res.ok) {
      let body: ApiError | null = null;
      try {
        body = (await res.json()) as ApiError;
      } catch {
        // Non-JSON error body. Fine.
      }
      const msg = body?.message ?? body?.error ?? `request failed: ${res.status}`;
      throw new ApiClientError(res.status, body, msg);
    }
    return res;
  },

  /**
   * Cross-chain audit search (#211). Returns per-run rows AND
   * global-chain rows in one time-descending feed; pagination and
   * filter envelope mirror listRunAudit so the same usePaginated
   * hook drives either page.
   */
  listGlobalAudit(params?: {
    limit?: number;
    cursor?: string;
    category?: string;
    runId?: string;
  }): Promise<PaginatedList<AuditEntry>> {
    const q = new URLSearchParams();
    if (params?.limit) q.set('limit', String(params.limit));
    if (params?.cursor) q.set('cursor', params.cursor);
    if (params?.category) q.set('category', params.category);
    if (params?.runId) q.set('run_id', params.runId);
    const qs = q.toString();
    return request(`/v0/audit${qs ? `?${qs}` : ''}`);
  },
};
