import { Link } from 'react-router';
import { api } from '@/api/client';
import { useAsync, type AsyncState } from '@/api/use-async';
import type { CampaignItem, CampaignStatus, Campaign, Run, Stage, StageState } from '@/api/types';
import { RepoPanelFrame, Stat } from './throughput-panel';

/*
 * In-flight panel (E40.3 / #1714). Composed entirely from EXISTING surfaces
 * — GET /v0/runs?repo=, GET /v0/runs/{id}/stages, GET /v0/campaigns?repo=
 * and GET /v0/campaigns/{id}/status — so it needs no backend of its own.
 *
 * RepoInFlight gathers the data (its own fetch, independent of the four
 * rollups); InFlightPanel renders an already-gathered state. A per-run stage
 * read or per-campaign status read that fails degrades THAT row only (the
 * row says what could not be read); only a failed list read fails the panel.
 */

/** Page size for the two list reads. A further page sets the matching *_truncated flag. */
const IN_FLIGHT_LIST_LIMIT = 100;

const ACTIVE_RUN_STATES = new Set<Run['state']>(['pending', 'running']);
const ACTIVE_CAMPAIGN_STATES = new Set<Campaign['state']>([
  'pending',
  'running',
  'paused',
  'awaiting_human',
]);
const TERMINAL_STAGE_STATES = new Set<StageState>([
  'succeeded',
  'failed',
  'cancelled',
  'superseded',
]);

export interface InFlightRun {
  run: Run;
  /** The first non-terminal stage by sequence (else the last); null when the run has no stages. */
  current_stage: Stage | null;
  /** Set when the stage read failed — the row renders it instead of a stage. */
  stage_error?: string;
}

export interface InFlightCampaign {
  campaign: Campaign;
  /** Absent when the status read failed; `status_error` then names why. */
  status?: CampaignStatus;
  status_error?: string;
}

export interface InFlightData {
  runs: InFlightRun[];
  campaigns: InFlightCampaign[];
  /** The run list had a further page, so an older active run may be missing. */
  runs_truncated: boolean;
  /** The campaign list had a further page, so an older active campaign may be missing. */
  campaigns_truncated: boolean;
  /**
   * Never set: RepoPanelFrame's rollup-window note does not describe a list
   * cut, so InFlightBody renders its own note from the two flags above.
   */
  truncated?: never;
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** The stage a run is currently on: the lowest-sequence non-terminal stage, else the last. */
function currentStage(stages: Stage[]): Stage | null {
  if (stages.length === 0) return null;
  const ordered = [...stages].sort((a, b) => a.sequence - b.sequence);
  return ordered.find((s) => !TERMINAL_STAGE_STATES.has(s.state)) ?? ordered[ordered.length - 1];
}

async function loadInFlight(repo: string): Promise<InFlightData> {
  const [runPage, campaignPage] = await Promise.all([
    api.listRuns({ repo, limit: IN_FLIGHT_LIST_LIMIT }),
    api.listCampaigns({ repo, limit: IN_FLIGHT_LIST_LIMIT }),
  ]);

  const runs = await Promise.all(
    runPage.items
      .filter((r) => ACTIVE_RUN_STATES.has(r.state))
      .map(async (run): Promise<InFlightRun> => {
        try {
          const { items } = await api.listRunStages(run.id);
          return { run, current_stage: currentStage(items) };
        } catch (err) {
          return { run, current_stage: null, stage_error: errorMessage(err) };
        }
      }),
  );

  const campaigns = await Promise.all(
    campaignPage.items
      .filter((c) => ACTIVE_CAMPAIGN_STATES.has(c.state))
      .map(async (campaign): Promise<InFlightCampaign> => {
        try {
          return { campaign, status: await api.getCampaignStatus(campaign.id) };
        } catch (err) {
          return { campaign, status_error: errorMessage(err) };
        }
      }),
  );

  return {
    runs,
    campaigns,
    runs_truncated: runPage.next_cursor !== null,
    campaigns_truncated: campaignPage.next_cursor !== null,
  };
}

/**
 * Wave index per item from the depends_on DAG: 0 for an item with no
 * in-campaign dependency, else 1 + the deepest dependency's wave. A
 * dependency naming no sibling item is ignored; a cycle is cut (the
 * revisited edge contributes wave 0) so a malformed DAG can't hang the page.
 */
function campaignWaves(items: CampaignItem[]): Map<string, number> {
  const byRef = new Map(items.map((i) => [i.issue_ref, i]));
  const waves = new Map<string, number>();
  const visiting = new Set<string>();
  const wave = (ref: string): number => {
    const known = waves.get(ref);
    if (known !== undefined) return known;
    if (visiting.has(ref)) return 0;
    visiting.add(ref);
    const item = byRef.get(ref);
    let w = 0;
    for (const dep of item?.depends_on ?? []) {
      if (byRef.has(dep)) w = Math.max(w, wave(dep) + 1);
    }
    visiting.delete(ref);
    waves.set(ref, w);
    return w;
  };
  for (const i of items) wave(i.issue_ref);
  return waves;
}

const DONE_ITEM_STATES = new Set<CampaignItem['state']>(['succeeded']);
const SETTLED_ITEM_STATES = new Set<CampaignItem['state']>(['succeeded', 'failed', 'cancelled']);

function CampaignProgress({ status }: { status: CampaignStatus }) {
  const { items, rollup } = status;
  const waves = campaignWaves(items);
  const totalWaves = items.length === 0 ? 0 : Math.max(...waves.values()) + 1;
  const openWaves = items
    .filter((i) => !SETTLED_ITEM_STATES.has(i.state))
    .map((i) => waves.get(i.issue_ref) ?? 0);
  const currentWave = openWaves.length === 0 ? totalWaves : Math.min(...openWaves) + 1;
  const done = items.filter((i) => DONE_ITEM_STATES.has(i.state)).length;
  const itemState = new Map(items.map((i) => [i.issue_ref, i.state]));
  const blocked = items.filter(
    (i) => i.state === 'blocked' || rollup.blocked.includes(i.issue_ref),
  );

  return (
    <div className="space-y-1 text-sm">
      <p>
        <span aria-label="wave progress">
          Wave {currentWave} of {totalWaves}
        </span>{' '}
        · {done} of {items.length} items done
      </p>
      <p className="text-xs text-neutral-600 dark:text-neutral-400">
        {rollup.eligible.length} eligible · {rollup.running.length} running ·{' '}
        {rollup.blocked.length} blocked · {rollup.paused.length} paused · {rollup.failed.length}{' '}
        failed · {rollup.done.length} done
      </p>
      {blocked.length > 0 && (
        <ul aria-label="Blocked items" className="space-y-0.5 text-xs">
          {blocked.map((i) => {
            const open = i.depends_on.filter((d) => itemState.get(d) !== 'succeeded');
            const blockers = open.length > 0 ? open : i.depends_on;
            return (
              <li key={i.id}>
                <span className="font-mono">{i.issue_ref}</span> blocked by{' '}
                {blockers.length > 0 ? (
                  <span className="font-mono">{blockers.join(', ')}</span>
                ) : (
                  <span>no recorded dependency</span>
                )}
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

function InFlightBody({ data }: { data: InFlightData }) {
  return (
    <div className="space-y-4">
      {(data.runs_truncated || data.campaigns_truncated) && (
        <p className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200">
          Partial data: only the newest {IN_FLIGHT_LIST_LIMIT}{' '}
          {data.runs_truncated && data.campaigns_truncated
            ? 'runs and campaigns were'
            : data.runs_truncated
              ? 'runs were'
              : 'campaigns were'}{' '}
          scanned, so an older in-flight item may be missing.
        </p>
      )}
      <dl className="grid grid-cols-2 gap-3">
        <Stat label="Active runs" value={data.runs.length} />
        <Stat label="Active campaigns" value={data.campaigns.length} />
      </dl>

      <div className="space-y-2">
        <h3 className="text-sm font-semibold">Runs</h3>
        {data.runs.length === 0 ? (
          <p className="text-sm text-neutral-500">No active runs.</p>
        ) : (
          <ul aria-label="Active runs" className="space-y-1 text-sm">
            {data.runs.map(({ run, current_stage, stage_error }) => (
              <li key={run.id} className="flex flex-wrap items-baseline gap-2">
                <Link
                  to={`/runs/${run.id}`}
                  className="font-mono underline-offset-2 hover:underline"
                >
                  {run.workflow_id}
                </Link>
                <span className="font-mono text-xs text-neutral-500">
                  {run.trigger_ref ?? run.id}
                </span>
                {stage_error !== undefined ? (
                  <span className="text-xs text-rose-700 dark:text-rose-300">
                    stage unavailable: {stage_error}
                  </span>
                ) : current_stage ? (
                  <span className="font-mono text-xs">
                    {current_stage.type} · {current_stage.state}
                  </span>
                ) : (
                  <span className="text-xs text-neutral-500">no stages yet</span>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>

      <div className="space-y-2">
        <h3 className="text-sm font-semibold">Campaigns</h3>
        {data.campaigns.length === 0 ? (
          <p className="text-sm text-neutral-500">No active campaigns.</p>
        ) : (
          <ul aria-label="Active campaigns" className="space-y-3">
            {data.campaigns.map(({ campaign, status, status_error }) => (
              <li key={campaign.id} className="space-y-1">
                <div className="flex flex-wrap items-baseline gap-2 text-sm">
                  <Link
                    to={`/campaigns/${campaign.id}`}
                    className="font-mono underline-offset-2 hover:underline"
                  >
                    {campaign.epic_ref}
                  </Link>
                  <span className="font-mono text-xs text-neutral-500">{campaign.state}</span>
                </div>
                {status ? (
                  <CampaignProgress status={status} />
                ) : (
                  <p className="text-xs text-rose-700 dark:text-rose-300">
                    status unavailable: {status_error}
                  </p>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

export function InFlightPanel({ state }: { state: AsyncState<InFlightData> }) {
  return (
    <RepoPanelFrame title="In flight" state={state}>
      {(data) => <InFlightBody data={data} />}
    </RepoPanelFrame>
  );
}

/** The in-flight panel with its own fetch, keyed on `owner/name`. */
export function RepoInFlight({ repo }: { repo: string }) {
  const state = useAsync(() => loadInFlight(repo), [repo]);
  return <InFlightPanel state={state} />;
}
