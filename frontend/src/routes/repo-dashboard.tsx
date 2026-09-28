import { useParams } from 'react-router';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import { EconomicsPanel } from '@/repo/economics-panel';
import { HealthPanel } from '@/repo/health-panel';
import { RepoInFlight } from '@/repo/in-flight-panel';
import { PosturePanel } from '@/repo/posture-panel';
import { ThroughputPanel } from '@/repo/throughput-panel';

/*
 * Repo dashboard at /repos/:owner/:name (E40.3 / #1714). Five INDEPENDENT
 * fetches, one per panel (the in-flight panel's lives in RepoInFlight), each panel owning its own loading and error
 * surface — so one failing rollup renders a panel-scoped alert and never
 * blanks the page. The window is the server default (12 weeks).
 */
export function RepoDashboard() {
  const { owner = '', name = '' } = useParams<{ owner: string; name: string }>();
  const repo = `${owner}/${name}`;

  const throughput = useAsync(() => api.getRepoThroughput(owner, name), [owner, name]);
  const economics = useAsync(() => api.getRepoEconomics(owner, name), [owner, name]);
  const health = useAsync(() => api.getRepoHealth(owner, name), [owner, name]);
  const posture = useAsync(() => api.getRepoPosture(owner, name), [owner, name]);

  return (
    <section className="space-y-4">
      <header>
        <h1 className="font-mono text-xl font-semibold tracking-tight">{repo}</h1>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">
          What is in flight, how the loop is performing, and the workflow posture for this
          repository.
        </p>
      </header>
      <RepoInFlight repo={repo} />
      <div className="grid gap-4 lg:grid-cols-2">
        <ThroughputPanel state={throughput} />
        <EconomicsPanel state={economics} />
        <HealthPanel state={health} />
        <PosturePanel state={posture} />
      </div>
    </section>
  );
}
