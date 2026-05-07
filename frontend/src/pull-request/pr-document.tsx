import { ArrowUpRight, FileClock, GitBranch } from 'lucide-react';
import { Link } from 'react-router';
import type { PullRequestArtifactBody } from '@/api/pull-request';
import type { Stage } from '@/api/types';
import { Section } from '@/plan/sections';
import { ApprovalPanel } from '@/plan/approval-panel';
import { StageStateBadge } from '@/components/stage-state-badge';

/*
 * Implement-stage detail (#205): renders the `pull_request` artifact
 * the runner shipped at the end of the stage. PR link block is the
 * primary affordance — most reviewers click through to GitHub. The
 * page exists so audit-conscious workflows have a Fishhawk-side
 * record even when GitHub is unreachable, and so the page-level
 * audit log scrolls into view from the same URL the run-detail row
 * deep-links to.
 *
 * `feature_change.workflows.yaml` doesn't gate the implement stage
 * today (#207 made gateless transitions go straight to succeeded),
 * so ApprovalPanel only renders when state == awaiting_approval —
 * forward-compatible with a future workflow that adds an implement
 * gate, but doesn't surface an "Approved" status for stages that
 * never actually went through approval.
 */

interface Props {
  artifact: PullRequestArtifactBody;
  stage: Stage;
  runId: string;
  onStageUpdate: (next: Stage) => void;
  onStageRollback: (prev: Stage) => void;
}

function shortSha(sha: string): string {
  return sha.length > 7 ? sha.slice(0, 7) : sha;
}

export function PullRequestDocument({
  artifact,
  stage,
  runId,
  onStageUpdate,
  onStageRollback,
}: Props) {
  const showApprovalPanel = stage.state === 'awaiting_approval';

  return (
    <article className="max-w-3xl space-y-8 pb-20">
      <header className="flex items-start justify-between gap-4">
        <div>
          <p className="text-xs tracking-wide text-neutral-500 uppercase">
            Implement · pull request
          </p>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">{artifact.title}</h1>
          <div className="mt-2 flex items-center gap-3">
            <StageStateBadge state={stage.state} />
            <a
              href={artifact.pr_url}
              rel="noopener noreferrer"
              target="_blank"
              className="inline-flex items-center gap-1 font-mono text-xs text-neutral-600 hover:underline dark:text-neutral-400"
            >
              #{artifact.pr_number}
              <ArrowUpRight className="size-3.5" aria-hidden />
            </a>
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

      <Section id="pull-request" title="Pull request">
        <div className="space-y-3 rounded-md border border-neutral-200 p-4 dark:border-neutral-800">
          <div className="flex items-center gap-2">
            <GitBranch className="size-4 text-neutral-500" aria-hidden />
            <span className="font-mono text-sm">{artifact.branch}</span>
          </div>
          <dl className="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
            <dt className="text-neutral-500">Head</dt>
            <dd className="font-mono text-xs" title={artifact.head_sha}>
              {shortSha(artifact.head_sha)}
            </dd>
            <dt className="text-neutral-500">Base</dt>
            <dd className="font-mono text-xs" title={artifact.base_sha}>
              {shortSha(artifact.base_sha)}
            </dd>
            <dt className="text-neutral-500">Files changed</dt>
            <dd className="font-mono text-sm">{artifact.files_changed_count}</dd>
          </dl>
          <a
            href={artifact.pr_url}
            rel="noopener noreferrer"
            target="_blank"
            className="inline-flex items-center gap-1 text-sm font-medium text-blue-700 hover:underline dark:text-blue-300"
          >
            View on GitHub
            <ArrowUpRight className="size-3.5" aria-hidden />
          </a>
        </div>
      </Section>

      {artifact.body && artifact.body.trim() !== '' && (
        <Section id="body" title="Description">
          <div className="rounded-md border border-neutral-200 bg-neutral-50 p-4 font-mono text-sm leading-relaxed whitespace-pre-wrap dark:border-neutral-800 dark:bg-neutral-900">
            {artifact.body}
          </div>
        </Section>
      )}
    </article>
  );
}
