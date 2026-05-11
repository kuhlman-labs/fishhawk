import { FileClock } from 'lucide-react';
import { Link } from 'react-router';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import type { PullRequestArtifactBody } from '@/api/pull-request';
import type { Stage } from '@/api/types';
import { ApprovalPanel } from '@/plan/approval-panel';
import { Section } from '@/plan/sections';
import { PullRequestSummary } from '@/pull-request/pr-summary';
import { StageStateBadge } from '@/components/stage-state-badge';
import { RequiredChecksPanel, type RequiredCheck } from '@/components/required-checks-panel';

/*
 * Review-stage detail (#213). Composition:
 *
 *   - Header: stage state badge, "Review · pull request" eyebrow,
 *     h1, ApprovalPanel (gated stages only) or audit-log link.
 *   - PullRequestSummary: shared with the implement page, fed the
 *     implement stage's pull_request artifact.
 *   - RequiredChecksPanel (renamed from BlockingChecksPanel in #256):
 *     lists the run's required checks with their live observed state
 *     (#228). Both the declared list and the source attribution
 *     ("Required by branch protection" / "+ N rulesets") come from
 *     GET /v0/stages/{id}/checks, sourced from the run's branch-
 *     protection snapshot (#251). Informational only as of #253 /
 *     ADR-017 — the approve button no longer waits on check state;
 *     GitHub branch protection blocks the merge until the required
 *     checks (which include fishhawk_audit_complete, published as a
 *     Check Run per #231) report green. Declared-but-not-observed
 *     entries fill as `not_tracked` so the panel shows the full gate.
 *   - Approvers list: shown only for approval-typed gates.
 *
 * For check-only review gates (e.g. routine_change.workflows.yaml),
 * ApprovalPanel is suppressed — there's no human action; the gate
 * clears automatically when checks pass.
 */

interface Props {
  artifact: PullRequestArtifactBody;
  stage: Stage;
  runId: string;
  onStageUpdate: (next: Stage) => void;
  onStageRollback: (prev: Stage) => void;
}

export function ReviewDocument({ artifact, stage, runId, onStageUpdate, onStageRollback }: Props) {
  const gate = stage.gate;
  const isApprovalGate = gate?.type === 'approval';
  const showApprovalPanel = isApprovalGate && stage.state === 'awaiting_approval';

  // Live check states (#228). Declared list, sources, and observed
  // states all come from /v0/stages/{id}/checks: post-#254 the
  // declared names live on the run's branch-protection snapshot
  // (#251), not the spec. Sources name which surfaces contributed
  // (`branch_protection`, `ruleset:<id>`) so the panel can render
  // a "Required by branch protection" attribution sub-label (#256).
  // Falls back to an empty list when the endpoint is 503 (legacy
  // deployments without check ingestion) or the request is in
  // flight.
  const checksResult = useAsync(() => api.listStageChecks(stage.id), [stage.id]);
  const checks = mergeRequiredChecks(checksResult);
  const sources = checksResult.status === 'ok' ? (checksResult.data.sources ?? []) : [];

  return (
    <article className="max-w-3xl space-y-8 pb-20">
      <header className="flex items-start justify-between gap-4">
        <div>
          <p className="text-xs tracking-wide text-neutral-500 uppercase">Review · pull request</p>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">{artifact.title}</h1>
          <div className="mt-2">
            <StageStateBadge state={stage.state} />
          </div>
        </div>
        {showApprovalPanel ? (
          <ApprovalPanel
            stage={stage}
            runId={runId}
            onUpdate={onStageUpdate}
            onRollback={onStageRollback}
          />
        ) : (
          <Link
            to={`/runs/${runId}#audit`}
            className="inline-flex items-center gap-1 self-start text-xs text-neutral-500 hover:underline"
          >
            <FileClock className="size-3.5" aria-hidden />
            View audit log
          </Link>
        )}
      </header>

      <PullRequestSummary artifact={artifact} />

      <Section id="required-checks" title="Required checks">
        <RequiredChecksPanel checks={checks} sources={sources} />
      </Section>

      {isApprovalGate && gate?.approvers && (
        <Section id="approvers" title="Approvers">
          <ApproversBlock approvers={gate.approvers} />
        </Section>
      )}
    </article>
  );
}

interface ChecksResponse {
  declared: string[];
  // sources records which surfaces contributed to `declared` (one
  // or both of `branch_protection`, `ruleset:<id>`). Optional so
  // legacy backends that pre-date #256 don't break — the panel just
  // renders without an attribution sub-label in that case.
  sources?: string[];
  items: Array<{
    name: string;
    state: 'pass' | 'fail' | 'pending' | 'not_tracked';
  }>;
}

// mergeRequiredChecks pairs the response's declared list (sourced
// from the run's branch-protection snapshot post-#254) with the
// most-recent observed state from /v0/stages/{id}/checks. Declared-
// but-not-observed checks render as `not_tracked`. Loading and
// error states (including a 503 from a backend without check
// ingestion) return an empty list so the panel renders cleanly
// without a flicker.
function mergeRequiredChecks(
  result:
    | { status: 'loading' }
    | { status: 'error'; error: Error }
    | { status: 'ok'; data: ChecksResponse },
): RequiredCheck[] {
  if (result.status !== 'ok') return [];
  const observed = new Map<string, RequiredCheck['state']>();
  for (const item of result.data.items) {
    observed.set(item.name, item.state);
  }
  return result.data.declared.map((name) => ({
    name,
    state: observed.get(name) ?? 'not_tracked',
  }));
}

function ApproversBlock({
  approvers,
}: {
  approvers: NonNullable<NonNullable<Stage['gate']>['approvers']>;
}) {
  const anyOf = approvers.any_of ?? [];
  const allOf = approvers.all_of ?? [];
  const list = anyOf.length > 0 ? anyOf : allOf;
  const mode = anyOf.length > 0 ? 'any of' : 'all of';

  if (list.length === 0) {
    return <p className="text-sm text-neutral-500">No approvers declared on this gate.</p>;
  }

  return (
    <div className="space-y-2">
      <p className="text-xs tracking-wide text-neutral-500 uppercase">{mode}</p>
      <ul className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
        {list.map((role) => (
          <li
            key={role}
            className="border-b border-neutral-200 px-3 py-2 font-mono text-sm last:border-b-0 dark:border-neutral-800"
          >
            {role}
          </li>
        ))}
      </ul>
    </div>
  );
}
