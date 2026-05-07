import { ArrowUpRight, GitBranch } from 'lucide-react';
import type { PullRequestArtifactBody } from '@/api/pull-request';
import { Section } from '@/plan/sections';

/*
 * Shared "Pull request" summary block used by both the implement-
 * stage detail page (#205, where this is the primary surface) and
 * the review-stage detail page (#213, where it's the input you're
 * approving). Keeps the two surfaces visually identical so reviewers
 * see the same shape regardless of which page they hit.
 */

function shortSha(sha: string): string {
  return sha.length > 7 ? sha.slice(0, 7) : sha;
}

export function PullRequestSummary({ artifact }: { artifact: PullRequestArtifactBody }) {
  return (
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
  );
}
