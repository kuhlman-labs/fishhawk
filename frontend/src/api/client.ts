import { getCookie } from '@/lib/cookie';
import type {
  CaptainResponse,
  CaptainVerbResponse,
  DelegationConfirmationResponse,
  DelegationConfirmRequest,
  DelegationLowerRequest,
  DelegationSource,
  DelegationVerbResponse,
  HandoverBrief,
  HandoverBriefSectionKind,
  RepoDelegation,
} from './captain';
import type {
  AcceptanceArbitrationRequest,
  AcceptanceArbitrationResult,
  ApiError,
  ApprovalRequest,
  Artifact,
  AttentionList,
  AuditEntry,
  AuditExport,
  Campaign,
  CampaignState,
  CampaignStatus,
  ConcernDeferRequest,
  ConcernWaiveRequest,
  DeferredConcernResult,
  GateView,
  GateViewStageKind,
  HealthStatus,
  PaginatedList,
  RepoEconomicsResponse,
  RepoHealth,
  RepoPostureResponse,
  RepoThroughput,
  Run,
  ScopeAmendment,
  ScopeAmendmentDecisionRequest,
  Stage,
  WaivedConcern,
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

/*
 * Repo dashboard path builder (E40.3 / #1714): owner and name are encoded
 * SEPARATELY so a `/` inside either can never be read as a path separator,
 * and `weeks` rides only when the caller set it (the server default is 12).
 */
function repoDashPath(
  owner: string,
  name: string,
  rollup: 'throughput' | 'health' | 'economics' | 'posture',
  weeks?: number,
): string {
  const base = `/v0/repos/${encodeURIComponent(owner)}/${encodeURIComponent(name)}/${rollup}`;
  return weeks !== undefined ? `${base}?weeks=${encodeURIComponent(String(weeks))}` : base;
}

/*
 * Delegation path builder (E76.6 / #3769): the same separately-encoded
 * owner/name discipline as repoDashPath, plus an optional sub-path and a
 * query string whose unset params are omitted rather than sent empty.
 */
function repoDelegationPath(
  owner: string,
  name: string,
  sub: '' | '/confirmation' | '/confirm' | '/lower',
  query?: Record<string, string | undefined>,
): string {
  const base = `/v0/repos/${encodeURIComponent(owner)}/${encodeURIComponent(name)}/delegation${sub}`;
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(query ?? {})) {
    if (v !== undefined && v !== '') q.set(k, v);
  }
  const qs = q.toString();
  return qs ? `${base}?${qs}` : base;
}

/*
 * One captain verb POST. The body carries `repo` and — for `offer` ONLY —
 * `successor`: the route answers 400 `validation_failed` to a `successor` on
 * any other verb. `delegated` is never sent; every captain verb refuses the
 * delegated path, and the SPA acts only as the signed-in human.
 */
function captainVerb(
  verb: 'offer' | 'withdraw' | 'accept' | 'relinquish' | 'claim',
  body: { repo: string; successor?: string },
): Promise<CaptainVerbResponse> {
  return request(`/v0/captain/${verb}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
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

/*
 * Shared non-2xx → ApiClientError conversion for the header-reading
 * export fetches, which cannot go through `request` (it discards the
 * Response). Mirrors `request`'s envelope parsing so a 403 still carries
 * the parsed `details.required_scope` the Record tab's re-auth panel names.
 */
async function exportError(res: Response): Promise<ApiClientError> {
  let body: ApiError | null = null;
  try {
    body = (await res.json()) as ApiError;
  } catch {
    // Non-JSON error body (e.g. plain text from a proxy). Fine.
  }
  const msg = body?.message ?? body?.error ?? `request failed: ${res.status}`;
  return new ApiClientError(res.status, body, msg);
}

export const api = {
  listRuns(params?: {
    limit?: number;
    cursor?: string;
    repo?: string;
    /** Equality filter on run.pull_request_url (#216). */
    pullRequestURL?: string;
    /** Equality filter on run.trigger_ref, e.g. "issue:42" (#216). */
    triggerRef?: string;
  }): Promise<PaginatedList<Run>> {
    const q = new URLSearchParams();
    if (params?.limit) q.set('limit', String(params.limit));
    if (params?.cursor) q.set('cursor', params.cursor);
    if (params?.repo) q.set('repo', params.repo);
    if (params?.pullRequestURL) q.set('pull_request_url', params.pullRequestURL);
    if (params?.triggerRef) q.set('trigger_ref', params.triggerRef);
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
    params?: {
      limit?: number;
      cursor?: string;
      category?: string;
      stageId?: string;
      /**
       * Return only entries with sequence strictly greater than this value
       * (applied before pagination). With `limit: 1` and `N - 1` it
       * resolves the single entry at sequence N independently of list
       * pagination — the run-narrative evidence-link target (#1715).
       * `0` is a valid anchor (sequences start at 1), so presence is
       * tested with `!== undefined`, not truthiness.
       */
      sinceSequence?: number;
    },
  ): Promise<PaginatedList<AuditEntry>> {
    const q = new URLSearchParams();
    if (params?.limit) q.set('limit', String(params.limit));
    if (params?.cursor) q.set('cursor', params.cursor);
    if (params?.category) q.set('category', params.category);
    if (params?.stageId) q.set('stage_id', params.stageId);
    if (params?.sinceSequence !== undefined) {
      q.set('since_sequence', String(params.sinceSequence));
    }
    const qs = q.toString();
    return request(`/v0/runs/${encodeURIComponent(runId)}/audit${qs ? `?${qs}` : ''}`);
  },

  /**
   * The gate-scoped decision view (#1960): open concerns with full note
   * prose + cross-round history, the settled ledger, and suppressed
   * relitigations. The run-narrative verdicts section (#1715) joins
   * reviewer verdicts against it. A `503 gate_view_unconfigured` surfaces
   * as an ApiClientError like every other non-2xx.
   */
  getRunGateView(runId: string, params?: { stageKind?: GateViewStageKind }): Promise<GateView> {
    const q = new URLSearchParams();
    if (params?.stageKind) q.set('stage_kind', params.stageKind);
    const qs = q.toString();
    return request(`/v0/runs/${encodeURIComponent(runId)}/gate-view${qs ? `?${qs}` : ''}`);
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
   * Latest state per declared blocking check on a stage (#228).
   * Returns the gate's declared list plus the most-recent observed
   * state per check name. Declared-but-not-observed entries are
   * absent from `items` — the SPA fills them with `not_tracked`.
   *
   * `fishhawk_audit_complete` (#229) is a self-derived row: the
   * backend computes its state from artifact + audit-log presence
   * and ships a `missing[]` list so the SPA can render the failure
   * reason inline.
   */
  listStageChecks(stageId: string): Promise<{
    declared: string[];
    /**
     * Surfaces that contributed to `declared` — one or both of
     * `branch_protection` and `ruleset:<id>`. Used by the
     * RequiredChecksPanel attribution sub-label (#256). Optional
     * on the wire for forward-compat with pre-#256 backends.
     */
    sources?: string[];
    items: Array<{
      name: string;
      state: 'pass' | 'fail' | 'pending' | 'not_tracked';
      status?: string;
      conclusion?: string | null;
      head_sha?: string;
      github_check_run_id?: number | null;
      ts?: string;
      missing?: Array<{
        kind:
          | 'plan_missing'
          | 'trace_missing'
          | 'pr_missing'
          | 'chain_invalid'
          | 'chain_unrecoverable'
          // #282: PR HEAD on GitHub isn't one of Fishhawk's
          // recorded head_shas — a commit landed outside the
          // Fishhawk runner.
          | 'foreign_commit'
          // #282 (pending-flavored): couldn't read the PR HEAD
          // from GitHub. Compute demotes the overall state to
          // pending so a flapping signal doesn't trip branch
          // protection.
          | 'head_fetch_failed';
        detail: string;
      }>;
    }>;
  }> {
    return request(`/v0/stages/${encodeURIComponent(stageId)}/checks`);
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
   * One page of the compliance export (Export v1, ADR-054 / #1604), for
   * the repo Record tab's chain-verification badge.
   *
   * Bypasses `request` because partiality and continuation ride RESPONSE
   * HEADERS (X-Fishhawk-Export-Complete / X-Fishhawk-Export-Next-Cursor)
   * and never the body — the verifier strict-decodes the three-field body,
   * so no marker can be added to it. `complete` is the header compared to
   * the EXACT string 'true': any other value, INCLUDING a missing header,
   * is read as NOT complete, so we fail toward declaring partiality rather
   * than silently claiming a whole-corpus verification.
   */
  async getAuditExportPage(params: {
    repo: string;
    limit?: number;
  }): Promise<{ data: AuditExport; complete: boolean; nextCursor: string | null }> {
    const q = new URLSearchParams();
    q.set('repo', params.repo);
    if (params.limit !== undefined) q.set('limit', String(params.limit));
    const res = await fetch(`/v0/audit/export?${q.toString()}`, {
      credentials: 'include',
      headers: { Accept: 'application/json' },
    });
    if (!res.ok) throw await exportError(res);
    return {
      data: (await res.json()) as AuditExport,
      complete: res.headers.get('X-Fishhawk-Export-Complete') === 'true',
      nextCursor: res.headers.get('X-Fishhawk-Export-Next-Cursor'),
    };
  },

  /**
   * The same export page as a RAW Response, so the caller can read both
   * the body bytes (for a Blob download) and Content-Disposition.
   */
  async getAuditExportDownload(params: { repo: string }): Promise<Response> {
    const q = new URLSearchParams();
    q.set('repo', params.repo);
    const res = await fetch(`/v0/audit/export?${q.toString()}`, {
      credentials: 'include',
      headers: { Accept: 'application/json' },
    });
    if (!res.ok) throw await exportError(res);
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

  /**
   * Campaign list (ADR-047 / #1437). Offset-cursor paginated, optional
   * repo + state filters — mirrors listRuns. Empty params are dropped.
   */
  listCampaigns(params?: {
    limit?: number;
    cursor?: string;
    repo?: string;
    state?: CampaignState;
  }): Promise<PaginatedList<Campaign>> {
    const q = new URLSearchParams();
    if (params?.limit) q.set('limit', String(params.limit));
    if (params?.cursor) q.set('cursor', params.cursor);
    if (params?.repo) q.set('repo', params.repo);
    if (params?.state) q.set('state', params.state);
    const qs = q.toString();
    return request(`/v0/campaigns${qs ? `?${qs}` : ''}`);
  },

  getCampaign(campaignId: string): Promise<Campaign> {
    return request(`/v0/campaigns/${encodeURIComponent(campaignId)}`);
  },

  /**
   * The campaign rollup surface: campaign + items (DAG edges via
   * depends_on, run links via run_id) + readiness rollup + the distilled
   * next_action. One fetch drives the whole detail page.
   */
  getCampaignStatus(campaignId: string): Promise<CampaignStatus> {
    return request(`/v0/campaigns/${encodeURIComponent(campaignId)}/status`);
  },

  /**
   * The cross-run attention queue (E40.1 / #1713): every decision parked
   * on a human, ranked. Read-only. `limit` caps the ranked list (a cut
   * sets `truncated`); omitted, the server default applies.
   */
  listAttention(params?: { limit?: number }): Promise<AttentionList> {
    const q = new URLSearchParams();
    if (params?.limit) q.set('limit', String(params.limit));
    const qs = q.toString();
    return request(`/v0/attention${qs ? `?${qs}` : ''}`);
  },

  /** Merged changes per week + median cycle time (+ optional wait-on-human). */
  getRepoThroughput(
    owner: string,
    name: string,
    params?: { weeks?: number },
  ): Promise<RepoThroughput> {
    return request(repoDashPath(owner, name, 'throughput', params?.weeks));
  },

  /** Plan first-shot approval, fixup and acceptance rates + failure-category mix. */
  getRepoHealth(owner: string, name: string, params?: { weeks?: number }): Promise<RepoHealth> {
    return request(repoDashPath(owner, name, 'health', params?.weeks));
  },

  /** Cost per merged change, weekly cache efficiency, ADR-030 budget burn. `{}` when no cost rows. */
  getRepoEconomics(
    owner: string,
    name: string,
    params?: { weeks?: number },
  ): Promise<RepoEconomicsResponse> {
    return request(repoDashPath(owner, name, 'economics', params?.weeks));
  },

  /** Workflow posture from the newest run's cached spec. `{}` when none is cached. */
  getRepoPosture(owner: string, name: string): Promise<RepoPostureResponse> {
    return request(repoDashPath(owner, name, 'posture'));
  },

  /** GET /healthz — carries the embedded schema-hash map the posture panel reads. */
  getHealth(): Promise<HealthStatus> {
    return request('/healthz');
  },

  /*
   * Human-decision write surfaces for the attention queue (E40.2 / #1717).
   * Each is a thin POST in the same style as submitApproval: CSRF
   * auto-attach and ApiClientError envelope parsing come from `request`.
   * No endpoint is added server-side; docs/api/v0.openapi.yaml is untouched.
   */

  decideScopeAmendment(
    runId: string,
    amendmentId: string,
    body: ScopeAmendmentDecisionRequest,
  ): Promise<ScopeAmendment> {
    return request(
      `/v0/runs/${encodeURIComponent(runId)}/scope-amendments/${encodeURIComponent(amendmentId)}/decision`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      },
    );
  },

  waiveConcern(concernId: string, body: ConcernWaiveRequest): Promise<WaivedConcern> {
    return request(`/v0/concerns/${encodeURIComponent(concernId)}/waive`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  },

  deferConcern(concernId: string, body: ConcernDeferRequest): Promise<DeferredConcernResult> {
    return request(`/v0/concerns/${encodeURIComponent(concernId)}/defer`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  },

  arbitrateAcceptance(
    runId: string,
    body: AcceptanceArbitrationRequest,
  ): Promise<AcceptanceArbitrationResult> {
    return request(`/v0/runs/${encodeURIComponent(runId)}/acceptance-arbitration`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  },

  /*
   * Change of command on the bridge (E76.6 / #3769). Ten thin wrappers over
   * EXISTING routes (backend/internal/server/captain.go and
   * delegation_confirm.go); no endpoint is added server-side. The UI adds no
   * authority: the server's refusal is the only identity gate.
   */

  /** GET /v0/captain — the derived captain record for `owner/name`. */
  getCaptain(repo: string): Promise<CaptainResponse> {
    const q = new URLSearchParams();
    q.set('repo', repo);
    return request(`/v0/captain?${q.toString()}`);
  },

  /** POST /v0/captain/offer — the only captain verb that sends `successor`. */
  captainOffer(body: { repo: string; successor: string }): Promise<CaptainVerbResponse> {
    return captainVerb('offer', { repo: body.repo, successor: body.successor });
  },

  captainWithdraw(repo: string): Promise<CaptainVerbResponse> {
    return captainVerb('withdraw', { repo });
  },

  captainAccept(repo: string): Promise<CaptainVerbResponse> {
    return captainVerb('accept', { repo });
  },

  captainRelinquish(repo: string): Promise<CaptainVerbResponse> {
    return captainVerb('relinquish', { repo });
  },

  captainClaim(repo: string): Promise<CaptainVerbResponse> {
    return captainVerb('claim', { repo });
  },

  /**
   * GET /v0/handover-brief. `fromSequence` / `toSequence` are tested with
   * `!== undefined` so an explicit 0 ("derive it") still rides the query.
   */
  getHandoverBrief(params: {
    repo: string;
    section?: HandoverBriefSectionKind;
    fromSequence?: number;
    toSequence?: number;
  }): Promise<HandoverBrief> {
    const q = new URLSearchParams();
    q.set('repo', params.repo);
    if (params.section) q.set('section', params.section);
    if (params.fromSequence !== undefined) q.set('from_sequence', String(params.fromSequence));
    if (params.toSequence !== undefined) q.set('to_sequence', String(params.toSequence));
    return request(`/v0/handover-brief?${q.toString()}`);
  },

  /** GET /v0/repos/{owner}/{name}/delegation — the resolved per-workflow view. */
  getRepoDelegation(
    owner: string,
    name: string,
    params?: { source?: DelegationSource; ref?: string; workflow?: string },
  ): Promise<RepoDelegation> {
    return request(
      repoDelegationPath(owner, name, '', {
        source: params?.source,
        ref: params?.ref,
        workflow: params?.workflow,
      }),
    );
  },

  /** GET /v0/repos/{owner}/{name}/delegation/confirmation — every workflow's verdict. */
  getRepoDelegationConfirmation(
    owner: string,
    name: string,
    params?: { source?: DelegationSource; ref?: string },
  ): Promise<DelegationConfirmationResponse> {
    return request(
      repoDelegationPath(owner, name, '/confirmation', {
        source: params?.source,
        ref: params?.ref,
      }),
    );
  },

  /** POST .../delegation/confirm — binds the workflow's OWN content_hash. */
  confirmRepoDelegation(
    owner: string,
    name: string,
    body: DelegationConfirmRequest,
  ): Promise<DelegationVerbResponse> {
    return request(repoDelegationPath(owner, name, '/confirm'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  },

  /** POST .../delegation/lower — files ONE autonomy:low work item; changes nothing in force. */
  lowerRepoDelegation(
    owner: string,
    name: string,
    body: DelegationLowerRequest,
  ): Promise<DelegationVerbResponse> {
    return request(repoDelegationPath(owner, name, '/lower'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  },
};
