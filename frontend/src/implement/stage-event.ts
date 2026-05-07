import type { AuditEntry } from '@/api/types';

/*
 * Domain-aware rendering for stage-scoped audit entries (#215).
 *
 * `renderStageEvent` returns a small UI-ready object — a label and
 * an optional human-readable detail string — that the activity feed
 * uses to render each audit row. Categories the implement-stage
 * pipeline currently emits get bespoke rendering; everything else
 * falls through to a generic shape that surfaces the raw category
 * + a one-line JSON peek of the payload, so unknown / new
 * categories don't disappear silently.
 *
 * No JSX here — keeps the pure function easy to unit-test against
 * raw entries.
 */

export interface StageEventDescription {
  label: string;
  detail?: string;
}

interface PolicyEvaluationPayload {
  passed?: boolean;
  violations?: Array<{ rule?: string; message?: string }>;
  diff?: Array<{ path?: string }>;
}

interface PullRequestOpenedPayload {
  pr_number?: number;
  pr_url?: string;
  files_changed_count?: number;
}

interface InstallationTokenPayload {
  auth_method?: string;
}

interface FailureReasonPayload {
  failure_category?: string;
  reason?: string;
}

export function renderStageEvent(entry: AuditEntry): StageEventDescription {
  switch (entry.category) {
    case 'stage_dispatched':
      return { label: 'Stage dispatched' };

    case 'signing_key_issued':
      return { label: 'Signing key issued' };

    case 'installation_token_issued': {
      const p = entry.payload as InstallationTokenPayload | null;
      const method = p?.auth_method ? ` · ${p.auth_method}` : '';
      return { label: `Installation token issued${method}` };
    }

    case 'trace_uploaded':
      return { label: 'Trace bundle uploaded' };

    case 'policy_evaluated': {
      const p = entry.payload as PolicyEvaluationPayload | null;
      const fileCount = p?.diff?.length ?? 0;
      const fileSummary = fileCount === 1 ? '1 file' : `${fileCount} files`;
      if (p?.passed === false) {
        const n = p.violations?.length ?? 0;
        return {
          label: 'Policy evaluated',
          detail: `${fileSummary} · ${n} violation${n === 1 ? '' : 's'}`,
        };
      }
      return { label: 'Policy evaluated', detail: `${fileSummary} · pass` };
    }

    case 'pull_request_opened': {
      const p = entry.payload as PullRequestOpenedPayload | null;
      const prRef = p?.pr_number ? `#${p.pr_number}` : 'pull request';
      const files =
        typeof p?.files_changed_count === 'number'
          ? ` · ${p.files_changed_count} file${p.files_changed_count === 1 ? '' : 's'}`
          : '';
      return { label: `Pull request opened · ${prRef}${files}` };
    }

    case 'stage_succeeded':
      return { label: 'Stage succeeded' };

    case 'stage_failed': {
      const p = entry.payload as FailureReasonPayload | null;
      const cat = p?.failure_category ? ` · category ${p.failure_category}` : '';
      return { label: `Stage failed${cat}`, detail: p?.reason };
    }

    case 'stage_retried':
      return { label: 'Stage retried' };

    default:
      // Unknown category — surface the raw name + a JSON peek so
      // operators see new event types instead of silent drops. Keep
      // the peek short; the audit log page is the canonical place
      // to inspect full payloads.
      return {
        label: entry.category,
        detail: jsonPeek(entry.payload),
      };
  }
}

function jsonPeek(payload: unknown): string | undefined {
  if (payload == null || (typeof payload === 'object' && Object.keys(payload).length === 0)) {
    return undefined;
  }
  try {
    const s = JSON.stringify(payload);
    return s.length > 80 ? `${s.slice(0, 77)}…` : s;
  } catch {
    return undefined;
  }
}
