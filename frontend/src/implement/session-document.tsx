import { useState } from 'react';
import { ArrowUpRight, ChevronDown, ChevronRight, FileClock } from 'lucide-react';
import { Link } from 'react-router';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import type { AuditEntry, Stage } from '@/api/types';
import type { PullRequestArtifactBody } from '@/api/pull-request';
import { ApprovalPanel } from '@/plan/approval-panel';
import { Section } from '@/plan/sections';
import { StageStateBadge } from '@/components/stage-state-badge';
import { renderStageEvent } from './stage-event';
import { TranscriptSection } from './transcript-section';

/*
 * Implement-stage session view (#215). Replaces the redundant
 * pull_request card from #205 with the agent's record of work:
 *
 *   - Constructed prompt the runner sent to the agent (foldable).
 *   - Stage-scoped audit feed: stage_dispatched, signing_key_issued,
 *     installation_token_issued, trace_uploaded, policy_evaluated,
 *     pull_request_opened, stage_succeeded / stage_failed.
 *   - Small PR-link row at the end (the full PR card lives on the
 *     review page; here we just point at it).
 *
 * The PR is an *output* of the implement stage, not its identity —
 * Brand Foundations §6 framing of the audit log as a first-class
 * surface, applied to one stage's slice of it.
 */

interface Props {
  stage: Stage;
  runId: string;
  pullRequest: PullRequestArtifactBody | null;
  onStageUpdate: (next: Stage) => void;
  onStageRollback: (prev: Stage) => void;
}

export function ImplementSessionDocument({
  stage,
  runId,
  pullRequest,
  onStageUpdate,
  onStageRollback,
}: Props) {
  const showApprovalPanel = stage.state === 'awaiting_approval';

  return (
    <article className="max-w-3xl space-y-8 pb-20">
      <header className="flex items-start justify-between gap-4">
        <div>
          <p className="text-xs tracking-wide text-neutral-500 uppercase">Implement · session</p>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">Implement stage</h1>
          <div className="mt-2 flex items-center gap-3">
            <StageStateBadge state={stage.state} />
            <span className="font-mono text-xs text-neutral-500" title={stage.id}>
              {stage.id.slice(0, 8)}…
            </span>
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

      <PromptSection stage={stage} />
      <ActivitySection stage={stage} runId={runId} />
      <TranscriptSection stageId={stage.id} />
      {pullRequest && <PullRequestRow pullRequest={pullRequest} />}
    </article>
  );
}

/* -- Prompt section -------------------------------------------------- */

// PromptSection only fetches once the agent could plausibly have
// received the prompt — the runner pulls it lazily, so rendering
// "no prompt yet" before dispatch would be misleading.
function PromptSection({ stage }: { stage: Stage }) {
  const stageStarted = stage.state !== 'pending' && stage.state !== 'cancelled';

  if (!stageStarted) {
    return (
      <Section id="prompt" title="Prompt">
        <p className="text-sm text-neutral-500">
          Prompt is constructed when the runner fetches it. Stage hasn&apos;t dispatched yet.
        </p>
      </Section>
    );
  }

  return <PromptBody stageId={stage.id} />;
}

function PromptBody({ stageId }: { stageId: string }) {
  const result = useAsync(() => api.getStagePromptRender(stageId), [stageId]);
  const [open, setOpen] = useState(true);

  if (result.status === 'loading') {
    return (
      <Section id="prompt" title="Prompt">
        <p className="text-sm text-neutral-500">Loading prompt…</p>
      </Section>
    );
  }
  if (result.status === 'error') {
    return (
      <Section id="prompt" title="Prompt">
        <div
          role="alert"
          className="rounded-md border border-rose-300 bg-rose-50 p-3 font-mono text-xs text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
        >
          Couldn&apos;t load prompt: {result.error.message}
        </div>
      </Section>
    );
  }

  return (
    <Section id="prompt" title="Prompt">
      <div className="space-y-2">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="inline-flex items-center gap-1 text-xs text-neutral-500 hover:text-neutral-900 dark:hover:text-neutral-100"
          aria-expanded={open}
        >
          {open ? (
            <ChevronDown className="size-3.5" aria-hidden />
          ) : (
            <ChevronRight className="size-3.5" aria-hidden />
          )}
          {open ? 'Hide' : 'Show'} prompt · sha256:{result.data.prompt_hash.slice(0, 12)}…
        </button>
        {open && (
          <pre className="overflow-x-auto rounded-md border border-neutral-200 bg-neutral-50 p-4 font-mono text-xs leading-relaxed whitespace-pre-wrap dark:border-neutral-800 dark:bg-neutral-900">
            {result.data.prompt}
          </pre>
        )}
      </div>
    </Section>
  );
}

/* -- Activity section ------------------------------------------------ */

function ActivitySection({ stage, runId }: { stage: Stage; runId: string }) {
  const result = useAsync(
    () => api.listRunAudit(runId, { stageId: stage.id, limit: 200 }),
    [runId, stage.id],
  );

  if (result.status === 'loading') {
    return (
      <Section id="activity" title="Activity">
        <p className="text-sm text-neutral-500">Loading activity…</p>
      </Section>
    );
  }
  if (result.status === 'error') {
    return (
      <Section id="activity" title="Activity">
        <div
          role="alert"
          className="rounded-md border border-rose-300 bg-rose-50 p-3 font-mono text-xs text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
        >
          Couldn&apos;t load activity: {result.error.message}
        </div>
      </Section>
    );
  }

  const entries = result.data.items;
  if (entries.length === 0) {
    return (
      <Section id="activity" title="Activity">
        <p className="rounded-md border border-dashed border-neutral-300 p-4 text-sm text-neutral-500 dark:border-neutral-700">
          No events recorded for this stage yet.
        </p>
      </Section>
    );
  }

  return (
    <Section id="activity" title="Activity">
      <ol className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
        {entries.map((entry) => (
          <ActivityRow key={entry.id} entry={entry} />
        ))}
      </ol>
    </Section>
  );
}

function ActivityRow({ entry }: { entry: AuditEntry }) {
  const desc = renderStageEvent(entry);
  return (
    <li className="flex items-baseline gap-3 border-b border-neutral-200 px-3 py-2 last:border-b-0 dark:border-neutral-800">
      <span className="w-44 shrink-0 font-mono text-xs text-neutral-500">
        {new Date(entry.ts).toLocaleString()}
      </span>
      <div className="min-w-0 flex-1">
        <p className="truncate font-mono text-sm">{desc.label}</p>
        {desc.detail && (
          <p className="truncate font-mono text-xs text-neutral-500">{desc.detail}</p>
        )}
      </div>
      <span className="font-mono text-[10px] text-neutral-400" title={entry.entry_hash}>
        {entry.entry_hash.slice(0, 8)}…
      </span>
    </li>
  );
}

/* -- Pull request row ----------------------------------------------- */

function PullRequestRow({ pullRequest }: { pullRequest: PullRequestArtifactBody }) {
  return (
    <Section id="pull-request" title="Output">
      <a
        href={pullRequest.pr_url}
        rel="noopener noreferrer"
        target="_blank"
        className="inline-flex items-center gap-1 text-sm font-medium text-blue-700 hover:underline dark:text-blue-300"
      >
        View PR #{pullRequest.pr_number} on GitHub
        <ArrowUpRight className="size-3.5" aria-hidden />
      </a>
      <p className="mt-1 text-xs text-neutral-500">
        {pullRequest.files_changed_count} file
        {pullRequest.files_changed_count === 1 ? '' : 's'} changed.
      </p>
    </Section>
  );
}
