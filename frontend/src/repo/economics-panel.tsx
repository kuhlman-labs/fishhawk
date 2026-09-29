import type { AsyncState } from '@/api/use-async';
import type { RepoEconomics, RepoEconomicsResponse } from '@/api/types';
import { Rate } from './health-panel';
import { RepoPanelFrame, Stat, WeekLabel } from './throughput-panel';

/*
 * Economics rollup panel (E40.3 / #1714): cost per merged change, ADR-030
 * periodic-budget burn with its tier, and the weekly cache-efficiency trend.
 * The server answers `{}` (or `{"truncated":true}`) when the window holds no
 * cost_recorded rows — presence, not a status code — so the body is
 * discriminated on `weeks`.
 */

function formatUSD(usd: number): string {
  return `$${usd.toFixed(2)}`;
}

function hasCost(data: RepoEconomicsResponse): data is RepoEconomics {
  return 'weeks' in data;
}

function EconomicsBody({ data }: { data: RepoEconomics }) {
  return (
    <div className="space-y-3">
      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        <Stat
          label="Cost per merged change"
          value={data.merged_changes > 0 ? formatUSD(data.cost_per_merged_change_usd) : '—'}
          hint={`${data.merged_changes} merged change${data.merged_changes === 1 ? '' : 's'}`}
        />
        <Stat
          label="Total cost"
          value={formatUSD(data.total_cost_usd)}
          hint={`${data.cost_entries} cost entr${data.cost_entries === 1 ? 'y' : 'ies'}`}
        />
      </dl>
      {data.budgets && data.budgets.length > 0 && (
        <table aria-label="Budget burn" className="w-full text-sm">
          <thead className="text-left text-xs tracking-wide text-neutral-500 uppercase">
            <tr>
              <th className="py-1 font-medium">Workflow</th>
              <th className="py-1 font-medium">Period</th>
              <th className="py-1 font-medium">Spent / limit</th>
              <th className="py-1 font-medium">Burn</th>
              <th className="py-1 font-medium">Tier</th>
            </tr>
          </thead>
          <tbody>
            {data.budgets.map((b) => (
              <tr key={`${b.workflow_id}:${b.period}`}>
                <td className="py-1 font-mono text-xs">{b.workflow_id}</td>
                <td className="py-1">{b.period}</td>
                <td className="py-1 tabular-nums">
                  {formatUSD(b.spent_usd)} / {formatUSD(b.limit_usd)}
                </td>
                <td className="py-1 tabular-nums">
                  <Rate value={b.fraction} />
                </td>
                <td className="py-1">
                  {b.tier} <span className="text-xs text-neutral-500">({b.enforcement})</span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <table aria-label="Weekly cache efficiency" className="w-full text-sm">
        <thead className="text-left text-xs tracking-wide text-neutral-500 uppercase">
          <tr>
            <th className="py-1 font-medium">Week</th>
            <th className="py-1 font-medium">Cost</th>
            <th className="py-1 font-medium">Cache read</th>
            <th className="py-1 font-medium">Reuse</th>
            <th className="py-1 font-medium">Net savings</th>
          </tr>
        </thead>
        <tbody>
          {data.weeks.map((w) => (
            <tr key={w.week_start}>
              <td className="py-1">
                <WeekLabel iso={w.week_start} />
              </td>
              <td className="py-1 tabular-nums">{formatUSD(w.cost_usd)}</td>
              <td className="py-1 tabular-nums">
                <Rate value={w.cache_read_ratio} />
              </td>
              <td className="py-1 tabular-nums">{w.reuse_factor.toFixed(1)}×</td>
              <td className="py-1 tabular-nums">{formatUSD(w.net_savings_usd)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function EconomicsPanel({ state }: { state: AsyncState<RepoEconomicsResponse> }) {
  return (
    <RepoPanelFrame title="Economics" state={state}>
      {(data) =>
        hasCost(data) ? (
          <EconomicsBody data={data} />
        ) : (
          <p className="text-sm text-neutral-500">No cost recorded in this window.</p>
        )
      }
    </RepoPanelFrame>
  );
}
