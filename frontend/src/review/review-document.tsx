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
import { BlockingChecksPanel, type BlockingCheck } from '@/components/blocking-checks-panel';

/*
 * Review-stage detail (#213). Composition:
 *
 *   - Header: stage state badge, "Review · pull request" eyebrow,
 *     h1, ApprovalPanel (gated stages only) or audit-log link.
 *   - PullRequestSummary: shared with the implement page, fed the
 *     implement stage's pull_request artifact.
 *   - BlockingChecksPanel: lists the gate's declared blocking_checks
 *     with their live observed state (#228). The backend serves the
 *     declared list + most-recent observed state; declared-but-not-
 *     observed entries fill as `not_tracked` so the SPA always
 *     shows the full gate.
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

  // Live blocking-check states (#228). Falls back to all-not-tracked
  // when the endpoint is 503 (legacy deployments without check
  // ingestion) or the request is in flight.
  const checksResult = useAsync(() => api.listStageChecks(stage.id), [stage.id]);
  const checks = mergeBlockingChecks(gate?.blocking_checks ?? [], checksResult);

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

      <Section id="blocking-checks" title="Blocking checks">
        <BlockingChecksPanel checks={checks} />
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
  items: Array<{
    name: string;
    state: 'pass' | 'fail' | 'pending' | 'not_tracked';
  }>;
}

// mergeBlockingChecks pairs the gate's declared list with the most-
// recent observed state from /v0/stages/{id}/checks. Declared-but-
// not-observed checks render as `not_tracked`. Loading and error
// states (including a 503 from a backend without check ingestion)
// fall back to declaring everything as `not_tracked` so the panel
// always renders the full gate without a flicker.
function mergeBlockingChecks(
  declared: string[],
  result:
    | { status: 'loading' }
    | { status: 'error'; error: Error }
    | { status: 'ok'; data: ChecksResponse },
): BlockingCheck[] {
  const observed = new Map<string, BlockingCheck['state']>();
  if (result.status === 'ok') {
    for (const item of result.data.items) {
      observed.set(item.name, item.state);
    }
  }
  return declared.map((name) => ({
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
