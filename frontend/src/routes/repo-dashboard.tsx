import { Link, useParams, useSearchParams } from 'react-router';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import { BridgeTab } from '@/bridge/bridge-tab';
import { EconomicsPanel } from '@/repo/economics-panel';
import { HealthPanel } from '@/repo/health-panel';
import { RepoInFlight } from '@/repo/in-flight-panel';
import { PosturePanel } from '@/repo/posture-panel';
import { RecordTab } from '@/repo/record-tab';
import { ThroughputPanel } from '@/repo/throughput-panel';

/*
 * Repo dashboard at /repos/:owner/:name (E40.3 / #1714). The Overview tab
 * makes five INDEPENDENT fetches, one per panel (the in-flight panel's
 * lives in RepoInFlight), each panel owning its own loading and error
 * surface — so one failing rollup renders a panel-scoped alert and never
 * blanks the page. The window is the server default (12 weeks).
 *
 * The Record tab (E40.6 / #1718) rides a `?tab=record` search param, so
 * the bare route and every existing deep link still land on Overview.
 * Each branch renders only its own panels, so the Record tab never fires
 * the four rollup fetches and vice versa.
 *
 * The Bridge tab (E76.6 / #3769) rides `?tab=bridge` the same way: it
 * fires only the captain, handover-brief and delegation reads, never the
 * Overview rollups or the Record tab's export, and vice versa.
 */
export function RepoDashboard() {
  const { owner = '', name = '' } = useParams<{ owner: string; name: string }>();
  const repo = `${owner}/${name}`;
  const [searchParams] = useSearchParams();
  const param = searchParams.get('tab');
  const tab = param === 'record' || param === 'bridge' ? param : 'overview';

  return (
    <section className="space-y-4">
      <header>
        <h1 className="font-mono text-xl font-semibold tracking-tight">{repo}</h1>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">
          What is in flight, how the loop is performing, and the workflow posture for this
          repository.
        </p>
      </header>
      <nav
        aria-label="Repository views"
        className="flex gap-2 border-b border-neutral-200 dark:border-neutral-800"
      >
        <TabLink to="?tab=" label="Overview" selected={tab === 'overview'} />
        <TabLink to="?tab=record" label="Record" selected={tab === 'record'} />
        <TabLink to="?tab=bridge" label="Bridge" selected={tab === 'bridge'} />
      </nav>
      {tab === 'record' ? (
        <RecordTab repo={repo} owner={owner} name={name} />
      ) : tab === 'bridge' ? (
        <BridgeTab repo={repo} owner={owner} name={name} />
      ) : (
        <OverviewTab repo={repo} owner={owner} name={name} />
      )}
    </section>
  );
}

function TabLink({ to, label, selected }: { to: string; label: string; selected: boolean }) {
  return (
    <Link
      to={to}
      aria-current={selected ? 'page' : undefined}
      className={
        selected
          ? '-mb-px border-b-2 border-neutral-900 px-3 py-2 text-sm font-medium dark:border-neutral-100'
          : '-mb-px border-b-2 border-transparent px-3 py-2 text-sm text-neutral-600 hover:text-neutral-900 dark:text-neutral-400 dark:hover:text-neutral-100'
      }
    >
      {label}
    </Link>
  );
}

function OverviewTab({ repo, owner, name }: { repo: string; owner: string; name: string }) {
  const throughput = useAsync(() => api.getRepoThroughput(owner, name), [owner, name]);
  const economics = useAsync(() => api.getRepoEconomics(owner, name), [owner, name]);
  const health = useAsync(() => api.getRepoHealth(owner, name), [owner, name]);
  const posture = useAsync(() => api.getRepoPosture(owner, name), [owner, name]);

  return (
    <>
      <RepoInFlight repo={repo} />
      <div className="grid gap-4 lg:grid-cols-2">
        <ThroughputPanel state={throughput} />
        <EconomicsPanel state={economics} />
        <HealthPanel state={health} />
        <PosturePanel state={posture} />
      </div>
    </>
  );
}
