import { Link, useParams, useSearchParams } from 'react-router';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import type { AuditEntry } from '@/api/types';
import { EvidencePanel } from '@/run-narrative/evidence';
import {
  ENTRY_QUERY_PARAM,
  decodePolicyDiff,
  evidenceRefFor,
  parseEntryParam,
  type EvidenceRef,
} from '@/run-narrative/narrative';
import { RunNarrativeView } from '@/run-narrative/run-narrative';
import {
  loadRunNarrative,
  type NarrativeClient,
  type RunNarrative,
} from '@/run-narrative/use-run-narrative';
import { FollowUpLink, RelatedRunsSection, RetryBadge } from '@/runs/related-runs';
import { RunAuditList } from './audit-list';

/*
 * Run detail (#1715): the existing header, then the gate-centric evidence
 * narrative (plan → advisory verdicts → diff summary → acceptance →
 * approvals → merge, then the gate timeline carrying the stage list),
 * RelatedRunsSection, and the unchanged raw #audit list. A `?entry=N`
 * query renders that audit entry in an evidence panel, resolved
 * independently of the #audit list's pagination.
 */

export function RunDetail() {
  const { runId } = useParams<{ runId: string }>();
  if (!runId) {
    return <div role="alert">Missing run id.</div>;
  }

  return <RunDetailLoaded runId={runId} />;
}

interface RunDetailData {
  narrative: RunNarrative;
  /** The policy_evaluated entry the diff summary's staged column was derived from. */
  stagedEvidence: EvidenceRef | null;
}

/**
 * Load the narrative through a client that also observes the
 * policy_evaluated category read, so the staged-scope column can link to
 * the exact entry it came from without a second read. The selection
 * mirrors the loader's: the newest entry whose diff decodes.
 */
async function loadRunDetail(runId: string): Promise<RunDetailData> {
  const policyEntries: AuditEntry[] = [];
  const client: NarrativeClient = {
    getRun: (id) => api.getRun(id),
    listRunStages: (id) => api.listRunStages(id),
    getRunGateView: (id, params) => api.getRunGateView(id, params),
    listStageArtifacts: (id) => api.listStageArtifacts(id),
    getArtifact: (id) => api.getArtifact(id),
    listRunAudit: async (id, params) => {
      const res = await api.listRunAudit(id, params);
      if (params?.category === 'policy_evaluated') policyEntries.push(...res.items);
      return res;
    },
  };
  const narrative = await loadRunNarrative(runId, client);
  const newest = [...policyEntries]
    .sort((a, b) => b.sequence - a.sequence)
    .find((e) => decodePolicyDiff(e.payload) !== null);
  return { narrative, stagedEvidence: newest ? evidenceRefFor(newest, runId) : null };
}

function RunDetailLoaded({ runId }: { runId: string }) {
  const data = useAsync(() => loadRunDetail(runId), [runId]);

  if (data.status === 'loading') {
    return <div className="text-sm text-neutral-500">Loading run…</div>;
  }
  if (data.status === 'error') {
    return <ErrorBox label="run" error={data.error} />;
  }

  return <RunDetailView data={data.data} />;
}

function RunDetailView({ data }: { data: RunDetailData }) {
  const { narrative, stagedEvidence } = data;
  const run = narrative.run;
  const [searchParams] = useSearchParams();
  const entrySequence = parseEntryParam(searchParams.get(ENTRY_QUERY_PARAM));
  return (
    <section className="space-y-6">
      <div>
        <Link to="/runs" className="text-xs text-neutral-500 hover:underline">
          ← Runs
        </Link>
      </div>

      <header className="space-y-2">
        <div className="flex items-center gap-2">
          <h1 className="font-mono text-lg font-semibold tracking-tight">{run.repo}</h1>
          {run.retry_attempt > 0 && (
            <RetryBadge
              attempt={run.retry_attempt}
              max={run.max_retries_snapshot}
              parentRunID={run.parent_run_id ?? null}
            />
          )}
        </div>
        {run.parent_run_id && <FollowUpLink parentRunID={run.parent_run_id} />}
        <dl className="grid grid-cols-[10rem_1fr] gap-y-1 text-sm">
          <dt className="text-neutral-500">Workflow</dt>
          <dd className="font-mono">{run.workflow_id}</dd>
          <dt className="text-neutral-500">State</dt>
          <dd className="font-mono">{run.state}</dd>
          <dt className="text-neutral-500">Trigger</dt>
          <dd className="font-mono text-xs">
            {run.trigger_source}
            {run.trigger_ref ? ` · ${run.trigger_ref}` : ''}
          </dd>
          {run.pull_request_url && (
            <>
              <dt className="text-neutral-500">Pull request</dt>
              <dd className="font-mono text-xs">
                <a
                  href={run.pull_request_url}
                  rel="noopener noreferrer"
                  target="_blank"
                  className="text-blue-700 hover:underline dark:text-blue-300"
                >
                  {run.pull_request_url}
                </a>
              </dd>
            </>
          )}
          <dt className="text-neutral-500">SHA</dt>
          <dd className="font-mono text-xs">{run.workflow_sha}</dd>
          <dt className="text-neutral-500">Run ID</dt>
          <dd className="font-mono text-xs">{run.id}</dd>
        </dl>
      </header>

      {entrySequence !== null && <EvidencePanel runId={run.id} sequence={entrySequence} />}

      <RunNarrativeView narrative={narrative} stagedEvidence={stagedEvidence} />

      <RelatedRunsSection run={run} />

      <div id="audit" className="scroll-mt-8 space-y-2">
        <h2 className="text-sm font-medium tracking-wide text-neutral-600 uppercase dark:text-neutral-400">
          Audit log
        </h2>
        <RunAuditList runId={run.id} />
      </div>
    </section>
  );
}

function ErrorBox({ label, error }: { label: string; error: Error }) {
  return (
    <div
      role="alert"
      className="rounded-md border border-rose-300 bg-rose-50 p-4 text-sm text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
    >
      <div className="font-medium">Couldn&apos;t load {label}.</div>
      <div className="mt-1 font-mono text-xs">{error.message}</div>
    </div>
  );
}
