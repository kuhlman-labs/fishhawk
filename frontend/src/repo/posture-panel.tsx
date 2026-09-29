import { Link } from 'react-router';
import type { AsyncState } from '@/api/use-async';
import type {
  PostureGate,
  PostureReviewers,
  PostureStageBudget,
  PostureWorkflow,
  RepoPosture,
  RepoPostureResponse,
} from '@/api/types';
import { RepoPanelFrame } from './throughput-panel';

/*
 * Posture panel (E40.3 / #1714): the workflow spec cached on the repo's
 * newest run — stages, gates + approvers, reviewers, autonomy, budgets.
 *
 * The drift warning keys on what the SERVER measured, never on a hash
 * comparison: posture's schema_hash and /healthz's `schemas` map come from
 * the same binary, so comparing them could never fail (approval condition
 * 4). The warning fires when the declared major is not embedded
 * (`schema_supported: false`) or the cached spec does not validate against
 * the embedded schema (`spec_valid: false`, reason in `spec_error`). The
 * hash is displayed for reference only.
 */

function hasPosture(data: RepoPostureResponse): data is RepoPosture {
  return 'version' in data;
}

/** True when the drift warning must render. */
function postureDrifted(p: RepoPosture): boolean {
  return !p.schema_supported || !p.spec_valid;
}

function DriftWarning({ posture }: { posture: RepoPosture }) {
  const major = `workflow-v${posture.schema_major}`;
  return (
    <div
      role="alert"
      aria-label="Workflow spec drift"
      className="space-y-1 rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200"
    >
      <div className="font-medium">Workflow spec drift</div>
      {!posture.schema_supported && (
        <p>
          The spec declares version <code className="font-mono">{posture.version}</code> ({major}),
          which this fishhawkd embeds no schema for.
        </p>
      )}
      {!posture.spec_valid && (
        <p>
          The spec at version <code className="font-mono">{posture.version}</code> does not validate
          against the embedded {major} schema
          {posture.spec_error ? (
            <>
              : <span className="font-mono text-xs">{posture.spec_error}</span>
            </>
          ) : (
            '.'
          )}
        </p>
      )}
    </div>
  );
}

function GateCell({ gates }: { gates?: PostureGate[] }) {
  if (!gates || gates.length === 0) return <span className="text-neutral-500">—</span>;
  return (
    <ul className="space-y-0.5">
      {gates.map((g, i) => (
        <li key={i}>
          <span className="font-mono">{g.type}</span>
          {g.approvers_any_of && g.approvers_any_of.length > 0 && (
            <> · any of {g.approvers_any_of.join(', ')}</>
          )}
          {g.approvers_all_of && g.approvers_all_of.length > 0 && (
            <> · all of {g.approvers_all_of.join(', ')}</>
          )}
          {g.autonomy && <> · autonomy {g.autonomy}</>}
        </li>
      ))}
    </ul>
  );
}

function ReviewersCell({ reviewers }: { reviewers?: PostureReviewers }) {
  const agents = reviewers?.agents ?? [];
  const human = reviewers?.human ?? 0;
  if (agents.length === 0 && human === 0) return <span className="text-neutral-500">—</span>;
  return (
    <ul className="space-y-0.5">
      {agents.map((a, i) => (
        <li key={i} className="font-mono">
          {a.model ? `${a.provider}/${a.model}` : a.provider}
        </li>
      ))}
      {human > 0 && (
        <li>
          {human} human reviewer{human === 1 ? '' : 's'}
        </li>
      )}
    </ul>
  );
}

function BudgetCell({ budget }: { budget?: PostureStageBudget }) {
  const parts: string[] = [];
  if (budget?.max_tokens !== undefined) parts.push(`${budget.max_tokens} tokens`);
  if (budget?.max_runtime_seconds !== undefined) parts.push(`${budget.max_runtime_seconds}s`);
  if (budget?.limit_usd !== undefined) parts.push(`$${budget.limit_usd}`);
  if (parts.length === 0) return <span className="text-neutral-500">—</span>;
  return <>{parts.join(' · ')}</>;
}

function WorkflowPosture({ workflow }: { workflow: PostureWorkflow }) {
  return (
    <div className="space-y-2">
      <h3 className="text-sm font-semibold">
        <span className="font-mono">{workflow.id}</span>
        {workflow.autonomy && (
          <span className="ml-2 text-xs font-normal text-neutral-500">
            autonomy {workflow.autonomy}
          </span>
        )}
      </h3>
      <table aria-label={`Stages of ${workflow.id}`} className="w-full text-xs">
        <thead className="text-left tracking-wide text-neutral-500 uppercase">
          <tr>
            <th className="py-1 font-medium">Stage</th>
            <th className="py-1 font-medium">Executor</th>
            <th className="py-1 font-medium">Gates</th>
            <th className="py-1 font-medium">Reviewers</th>
            <th className="py-1 font-medium">Budget</th>
          </tr>
        </thead>
        <tbody className="align-top">
          {workflow.stages.map((s) => (
            <tr key={s.id}>
              <td className="py-1">
                <span className="font-mono">{s.id}</span>
                {s.type !== s.id && <span className="text-neutral-500"> ({s.type})</span>}
              </td>
              <td className="py-1 font-mono">
                {s.executor}
                {s.model ? ` · ${s.model}` : ''}
              </td>
              <td className="py-1">
                <GateCell gates={s.gates} />
              </td>
              <td className="py-1">
                <ReviewersCell reviewers={s.reviewers} />
              </td>
              <td className="py-1 tabular-nums">
                <BudgetCell budget={s.budget} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {workflow.budgets && workflow.budgets.length > 0 && (
        <ul aria-label={`Periodic budgets of ${workflow.id}`} className="text-xs">
          {workflow.budgets.map((b) => (
            <li key={b.period}>
              {b.period} budget ${b.limit_usd}
              {b.enforcement ? ` · ${b.enforcement}` : ''}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function PostureBody({ posture }: { posture: RepoPosture }) {
  return (
    <div className="space-y-3">
      {postureDrifted(posture) && <DriftWarning posture={posture} />}
      <p className="text-xs text-neutral-600 dark:text-neutral-400">
        Spec version <code className="font-mono">{posture.version}</code> (workflow-v
        {posture.schema_major})
        {posture.schema_hash && (
          <>
            {' '}
            · schema{' '}
            <code className="font-mono" title={posture.schema_hash}>
              {posture.schema_hash.slice(0, 12)}
            </code>
          </>
        )}{' '}
        · from run{' '}
        <Link
          to={`/runs/${posture.run_id}`}
          className="font-mono underline-offset-2 hover:underline"
        >
          {posture.run_id.slice(0, 8)}
        </Link>
      </p>
      {posture.workflows.map((w) => (
        <WorkflowPosture key={w.id} workflow={w} />
      ))}
    </div>
  );
}

/** Posture carries no scan window, so the frame's partial-data note never applies. */
type FramedPosture = { posture: RepoPostureResponse; truncated?: never };

export function PosturePanel({ state }: { state: AsyncState<RepoPostureResponse> }) {
  const framed: AsyncState<FramedPosture> =
    state.status === 'ok' ? { status: 'ok', data: { posture: state.data } } : state;
  return (
    <RepoPanelFrame title="Posture" state={framed}>
      {({ posture }) =>
        hasPosture(posture) ? (
          <PostureBody posture={posture} />
        ) : (
          <p className="text-sm text-neutral-500">
            No run for this repo carries a cached workflow spec yet.
          </p>
        )
      }
    </RepoPanelFrame>
  );
}
