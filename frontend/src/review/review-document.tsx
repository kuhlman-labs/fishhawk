import { CheckCircle2, ExternalLink, GitMerge, MessageSquare, ShieldAlert } from 'lucide-react';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import type { PullRequestArtifactBody } from '@/api/pull-request';
import type { AuditEntry, Stage } from '@/api/types';
import { Section } from '@/plan/sections';
import { PullRequestSummary } from '@/pull-request/pr-summary';
import { StageStateBadge } from '@/components/stage-state-badge';
import { RequiredChecksPanel, type RequiredCheck } from '@/components/required-checks-panel';

/*
 * Review-stage detail (#213, ADR-018 / #314). Composition:
 *
 *   - Header: stage state badge, "Review · pull request" eyebrow,
 *     h1, and a "View on GitHub" affordance linking to the PR. The
 *     reviewer's next action lives on GitHub now — branch
 *     protection's required-reviewers is the approver gate, the
 *     PR merge is the success signal (ADR-018 / #311).
 *   - PullRequestSummary: shared with the implement page.
 *   - RequiredChecksPanel: live observed state of the run's
 *     required checks (#228, #251). Informational — branch
 *     protection enforces the merge gate.
 *   - Activity: chronological list of PR-side events ingested via
 *     the audit log (#312). Surfaces "@x approved", "@y requested
 *     changes", "@z merged" so reviewers see the same set of facts
 *     from the SPA as from the PR conversation.
 *   - Approvers list: informational for review stages (ADR-018);
 *     branch protection's required-reviewers is the actual gate.
 *
 * The pre-#314 ApprovalPanel is gone — review-stage approval moved
 * to GitHub. Plan-stage approvals still use the SPA's
 * ApprovalPanel via the plan document, untouched by this change.
 */

interface Props {
  artifact: PullRequestArtifactBody;
  stage: Stage;
  runId: string;
  // onStageUpdate / onStageRollback were part of the pre-#314
  // approval-flow callback contract. They're now unused — the
  // stage transitions on the merge webhook, not via SPA-driven
  // approval. The parent route still passes them; we accept and
  // ignore so the prop surface stays stable for callers.
  onStageUpdate?: (next: Stage) => void;
  onStageRollback?: (prev: Stage) => void;
}

export function ReviewDocument({ artifact, stage, runId }: Props) {
  const gate = stage.gate;
  const isApprovalGate = gate?.type === 'approval';

  const checksResult = useAsync(() => api.listStageChecks(stage.id), [stage.id]);
  const checks = mergeRequiredChecks(checksResult);
  const sources = checksResult.status === 'ok' ? (checksResult.data.sources ?? []) : [];

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
        <a
          href={artifact.pr_url}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1 self-start rounded-md border border-neutral-300 px-3 py-1.5 text-xs font-medium text-neutral-700 hover:bg-neutral-50 focus-visible:ring-1 focus-visible:ring-neutral-400 focus-visible:outline-none dark:border-neutral-700 dark:text-neutral-200 dark:hover:bg-neutral-900/50"
        >
          <ExternalLink className="size-3.5" aria-hidden />
          View on GitHub
        </a>
      </header>

      <PullRequestSummary artifact={artifact} />

      <Section id="required-checks" title="Required checks">
        <RequiredChecksPanel checks={checks} sources={sources} />
      </Section>

      <Section id="activity" title="Activity">
        <ReviewActivityPanel runId={runId} stageId={stage.id} />
      </Section>

      {isApprovalGate && gate?.approvers && (
        <Section id="approvers" title="Approvers (informational)">
          <ApproversBlock approvers={gate.approvers} />
        </Section>
      )}
    </article>
  );
}

interface ChecksResponse {
  declared: string[];
  sources?: string[];
  items: Array<{
    name: string;
    state: 'pass' | 'fail' | 'pending' | 'not_tracked';
  }>;
}

function mergeRequiredChecks(
  result:
    | { status: 'loading' }
    | { status: 'error'; error: Error }
    | { status: 'ok'; data: ChecksResponse },
): RequiredCheck[] {
  if (result.status !== 'ok') return [];
  const observed = new Map<string, RequiredCheck['state']>();
  for (const item of result.data.items) {
    observed.set(item.name, item.state);
  }
  return result.data.declared.map((name) => ({
    name,
    state: observed.get(name) ?? 'not_tracked',
  }));
}

/*
 * ReviewActivityPanel reads the run's audit log scoped to the
 * review stage and filters to the PR-side categories (#312):
 *
 *   - pr_approved_on_github       → "@x approved"
 *   - pr_review_submitted         → "@x requested changes" / "@x commented" / "@x dismissed"
 *   - pr_merged                   → "@x merged"
 *
 * Chronological (oldest first; matches how a reviewer scans the
 * PR conversation). Empty list → quiet "no activity yet" copy.
 * Loading / error states render their own placeholders so the
 * surrounding shell stays calm.
 */
function ReviewActivityPanel({ runId, stageId }: { runId: string; stageId: string }) {
  const result = useAsync(() => api.listRunAudit(runId, { stageId, limit: 100 }), [runId, stageId]);

  if (result.status === 'loading') {
    return <p className="text-sm text-neutral-500">Loading activity…</p>;
  }
  if (result.status === 'error') {
    return (
      <p className="text-sm text-rose-700 dark:text-rose-300">
        Couldn&apos;t load activity: {result.error.message}
      </p>
    );
  }

  const events = result.data.items.filter(isPRActivity);
  if (events.length === 0) {
    return <p className="text-sm text-neutral-500">No GitHub activity yet.</p>;
  }

  // Oldest first so a reader scans the timeline left-to-right /
  // top-to-bottom. listRunAudit returns descending by sequence —
  // reverse client-side.
  const ordered = [...events].reverse();

  return (
    <ul
      data-testid="review-activity"
      className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800"
    >
      {ordered.map((e) => (
        <li
          key={e.id}
          className="flex items-start gap-3 border-b border-neutral-200 px-3 py-2 text-sm last:border-b-0 dark:border-neutral-800"
        >
          <ActivityIcon category={e.category} payload={e.payload} />
          <div className="flex-1">
            <ActivityLine entry={e} />
            <p className="text-xs text-neutral-500">{formatTimestamp(e.ts)}</p>
          </div>
        </li>
      ))}
    </ul>
  );
}

const PR_ACTIVITY_CATEGORIES = new Set([
  'pr_approved_on_github',
  'pr_review_submitted',
  'pr_merged',
]);

function isPRActivity(entry: AuditEntry): boolean {
  return PR_ACTIVITY_CATEGORIES.has(entry.category);
}

function ActivityIcon({ category, payload }: { category: string; payload: unknown }) {
  if (category === 'pr_merged') {
    return <GitMerge className="mt-0.5 size-4 text-violet-600 dark:text-violet-400" aria-hidden />;
  }
  if (category === 'pr_approved_on_github') {
    return (
      <CheckCircle2 className="mt-0.5 size-4 text-emerald-600 dark:text-emerald-400" aria-hidden />
    );
  }
  if (category === 'pr_review_submitted') {
    const state = reviewStateOf(payload);
    if (state === 'changes_requested') {
      return (
        <ShieldAlert className="mt-0.5 size-4 text-amber-600 dark:text-amber-400" aria-hidden />
      );
    }
    return <MessageSquare className="mt-0.5 size-4 text-neutral-500" aria-hidden />;
  }
  return null;
}

function ActivityLine({ entry }: { entry: AuditEntry }) {
  const actor = entry.actor_subject ?? 'unknown';
  const at = `@${actor}`;
  switch (entry.category) {
    case 'pr_merged':
      return (
        <p>
          <span className="font-medium">{at}</span> merged the PR
        </p>
      );
    case 'pr_approved_on_github':
      return (
        <p>
          <span className="font-medium">{at}</span> approved
        </p>
      );
    case 'pr_review_submitted': {
      const state = reviewStateOf(entry.payload);
      return (
        <p>
          <span className="font-medium">{at}</span> {humanReviewVerb(state)}
        </p>
      );
    }
    default:
      return <p>{entry.category}</p>;
  }
}

function reviewStateOf(payload: unknown): string {
  if (payload && typeof payload === 'object' && 'state' in payload) {
    const v = (payload as { state?: unknown }).state;
    if (typeof v === 'string') return v;
  }
  return '';
}

function humanReviewVerb(state: string): string {
  switch (state) {
    case 'commented':
      return 'commented';
    case 'changes_requested':
      return 'requested changes';
    case 'dismissed':
      return 'dismissed a review';
    case 'approved':
      // Should hit the pr_approved_on_github category instead, but
      // be defensive — the backend's category split is based on the
      // event's state field; a future schema change could land
      // approved events on this generic category.
      return 'approved';
    default:
      return state ? `submitted a "${state}" review` : 'submitted a review';
  }
}

function formatTimestamp(ts: string): string {
  try {
    const d = new Date(ts);
    if (Number.isNaN(d.getTime())) return ts;
    return d.toLocaleString();
  } catch {
    return ts;
  }
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
      <p className="text-xs text-neutral-500">
        Branch protection&apos;s required-reviewers is the actual approver gate (ADR-018). This list
        is the workflow spec&apos;s declaration; Fishhawk records reviews against it but does not
        enforce.
      </p>
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
