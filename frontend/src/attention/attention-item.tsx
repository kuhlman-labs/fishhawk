import { useState, type ReactNode } from 'react';
import { Link } from 'react-router';
import { Button } from '@/components/ui/button';
import type { AttentionContext, AttentionItem, AttentionItemKind } from '@/api/types';
import { cn } from '@/lib/cn';
import { DECISION_VERBS } from './decision-verbs';
import { DecisionPanel } from './decision-panel';

/*
 * One attention-queue card (E40.1 / #1713; decision write-surface E40.2 /
 * #1717): kind badge, repo + title, the one-screen decision context, relative
 * age, and a link to the item's detail page.
 *
 * The card is no longer read-only: a "Decide" disclosure expands an inline
 * per-kind decision panel (DecisionPanel) beside the retained detail link, for
 * every kind whose DECISION_VERBS entry is non-empty. A human-led campaign
 * item has no decision endpoint (its verb list is empty), so it keeps its
 * E40.1 shape with no Decide toggle. Re-execution ("drive-plane") affordances
 * are still excluded — see decision-verbs.ts.
 *
 * The link target is the server-emitted `detail_path`, never re-derived here,
 * so the six link rules live in one place (the backend).
 */

const ATTENTION_KIND_LABELS: Record<AttentionItemKind, string> = {
  plan_gate: 'Plan gate',
  scope_amendment: 'Scope amendment',
  acceptance_disposition: 'Acceptance disposition',
  split_verdict: 'Split verdict',
  paged_concern: 'Open concern',
  attend_human_led_campaign: 'Human-led campaign item',
};

const kindStyles: Record<AttentionItemKind, string> = {
  plan_gate: 'bg-blue-100 text-blue-800 dark:bg-blue-900/40 dark:text-blue-300',
  scope_amendment: 'bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-300',
  acceptance_disposition: 'bg-rose-100 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300',
  split_verdict: 'bg-orange-100 text-orange-800 dark:bg-orange-900/40 dark:text-orange-300',
  paged_concern: 'bg-neutral-200 text-neutral-700 dark:bg-neutral-800 dark:text-neutral-300',
  attend_human_led_campaign:
    'bg-violet-100 text-violet-800 dark:bg-violet-900/40 dark:text-violet-300',
};

/** Coarse relative age ("3h ago"); the absolute timestamp rides in `title`. */
function formatAge(iso: string, now: number = Date.now()): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return iso;
  const secs = Math.max(0, Math.floor((now - then) / 1000));
  if (secs < 60) return 'just now';
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 48) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div>
      <dt className="text-xs font-medium tracking-wide text-neutral-500 uppercase dark:text-neutral-400">
        {label}
      </dt>
      <dd className="mt-0.5 text-sm text-neutral-800 dark:text-neutral-200">{children}</dd>
    </div>
  );
}

function Prose({ text }: { text: string }) {
  return <p className="whitespace-pre-wrap">{text}</p>;
}

function PlanGateContext({ ctx }: { ctx: AttentionContext }) {
  return (
    <>
      {ctx.plan_summary && (
        <Field label="Plan summary">
          <Prose text={ctx.plan_summary} />
        </Field>
      )}
      {ctx.review_verdicts && ctx.review_verdicts.length > 0 && (
        <Field label="Plan reviews">
          <ul className="space-y-0.5">
            {ctx.review_verdicts.map((v, i) => (
              <li key={i} className="font-mono text-xs">
                {v.reviewer_model ? `${v.reviewer_model}: ` : ''}
                {v.verdict} · {v.concern_count} concern{v.concern_count === 1 ? '' : 's'}
              </li>
            ))}
          </ul>
        </Field>
      )}
    </>
  );
}

function ScopeAmendmentContext({ ctx }: { ctx: AttentionContext }) {
  return (
    <>
      {ctx.reason && (
        <Field label="Agent's reason">
          <Prose text={ctx.reason} />
        </Field>
      )}
      {ctx.requested_paths && ctx.requested_paths.length > 0 && (
        <Field label="Requested paths">
          <ul className="space-y-0.5">
            {ctx.requested_paths.map((p) => (
              <li key={`${p.operation}:${p.path}`} className="font-mono text-xs">
                {p.operation} {p.path}
              </li>
            ))}
          </ul>
        </Field>
      )}
    </>
  );
}

function AcceptanceContext({ ctx }: { ctx: AttentionContext }) {
  const parts: string[] = [];
  if (ctx.criteria_failed !== undefined) parts.push(`${ctx.criteria_failed} failed`);
  if (ctx.criteria_skipped !== undefined) parts.push(`${ctx.criteria_skipped} skipped`);
  return (
    <>
      {ctx.verdict && (
        <Field label="Acceptance verdict">
          <span className="font-mono">{ctx.verdict}</span>
        </Field>
      )}
      {parts.length > 0 && <Field label="Criteria">{parts.join(' · ')}</Field>}
      {ctx.failed_criteria && ctx.failed_criteria.length > 0 && (
        <Field label="Failed criteria">
          <ul className="space-y-0.5">
            {ctx.failed_criteria.map((c) => {
              const req = [c.method, c.path].filter(Boolean).join(' ');
              const req_with_status = c.status ? `${req} → ${c.status}` : req;
              return (
                <li key={c.id} className="font-mono text-xs">
                  {c.id}
                  {req_with_status && (
                    <span className="text-neutral-500 dark:text-neutral-400">
                      {' '}
                      ({req_with_status})
                    </span>
                  )}
                </li>
              );
            })}
          </ul>
        </Field>
      )}
    </>
  );
}

function ConcernContext({ ctx, split }: { ctx: AttentionContext; split: boolean }) {
  const meta = [ctx.severity, ctx.category, ctx.stage_kind && `${ctx.stage_kind} gate`]
    .filter(Boolean)
    .join(' · ');
  return (
    <>
      {meta && (
        <Field label="Concern">
          <span className="font-mono text-xs">{meta}</span>
          {ctx.reviewer_model && (
            <span className="ml-2 text-xs text-neutral-500 dark:text-neutral-400">
              raised by {ctx.reviewer_model}
            </span>
          )}
        </Field>
      )}
      {ctx.note && (
        <Field label="Reviewer note">
          <Prose text={ctx.note} />
        </Field>
      )}
      {ctx.new_evidence && (
        <Field label="New evidence">
          <Prose text={ctx.new_evidence} />
        </Field>
      )}
      {split && ctx.confirmation_note && (
        <Field label="Recorded as resolved by a re-review">
          <Prose text={ctx.confirmation_note} />
        </Field>
      )}
      {split && ctx.dispute_reasons && ctx.dispute_reasons.length > 0 && (
        <Field label="Dispute reasons">
          <ul className="list-disc space-y-0.5 pl-4">
            {ctx.dispute_reasons.map((r, i) => (
              <li key={i}>{r}</li>
            ))}
          </ul>
        </Field>
      )}
    </>
  );
}

function CampaignContext({ ctx }: { ctx: AttentionContext }) {
  return (
    <>
      {ctx.epic_ref && (
        <Field label="Epic">
          <span className="font-mono">{ctx.epic_ref}</span>
        </Field>
      )}
      {ctx.human_led_refs && ctx.human_led_refs.length > 0 && (
        <Field label="Human-led items">
          <span className="font-mono">{ctx.human_led_refs.join(', ')}</span>
        </Field>
      )}
      {ctx.detail && (
        <Field label="Next action">
          <Prose text={ctx.detail} />
        </Field>
      )}
    </>
  );
}

/*
 * Exhaustive over AttentionItemKind: a seventh kind added to the union
 * without a branch here fails typecheck at the `never` assignment instead
 * of rendering a blank card.
 */
function KindContext({ item }: { item: AttentionItem }) {
  const kind = item.kind;
  switch (kind) {
    case 'plan_gate':
      return <PlanGateContext ctx={item.context} />;
    case 'scope_amendment':
      return <ScopeAmendmentContext ctx={item.context} />;
    case 'acceptance_disposition':
      return <AcceptanceContext ctx={item.context} />;
    case 'split_verdict':
      return <ConcernContext ctx={item.context} split />;
    case 'paged_concern':
      return <ConcernContext ctx={item.context} split={false} />;
    case 'attend_human_led_campaign':
      return <CampaignContext ctx={item.context} />;
    default: {
      const unhandled: never = kind;
      return <Field label="Unknown item">{String(unhandled)}</Field>;
    }
  }
}

export function AttentionItemCard({
  item,
  onResolved,
}: {
  item: AttentionItem;
  /** Called when this item's decision was successfully submitted (E40.2 / #1717). */
  onResolved?: (item: AttentionItem) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  // Only kinds with a decision endpoint get the Decide disclosure; a
  // human-led campaign item (empty verb list) keeps its E40.1 read-only shape.
  // `?? []` guards a runtime-unknown kind (the labelled-fallback path), which
  // has no DECISION_VERBS entry — it stays read-only.
  const decidable = (DECISION_VERBS[item.kind] ?? []).length > 0;

  return (
    <article
      aria-label={`${ATTENTION_KIND_LABELS[item.kind] ?? item.kind}: ${item.title}`}
      className="space-y-3 rounded-md border border-neutral-200 p-4 dark:border-neutral-800"
    >
      <header className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <span
          className={cn(
            'inline-flex rounded-full px-2 py-0.5 font-mono text-xs',
            kindStyles[item.kind],
          )}
        >
          {ATTENTION_KIND_LABELS[item.kind] ?? item.kind}
        </span>
        <span className="font-mono text-xs text-neutral-600 dark:text-neutral-400">
          {item.repo}
        </span>
        <span className="font-medium text-neutral-900 dark:text-neutral-100">{item.title}</span>
        <time
          dateTime={item.since}
          title={new Date(item.since).toLocaleString()}
          className="ml-auto text-xs text-neutral-500 dark:text-neutral-400"
        >
          {formatAge(item.since)}
        </time>
      </header>
      <dl className="space-y-2">
        <KindContext item={item} />
      </dl>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        {decidable && (
          <Button variant="outline" size="sm" onClick={() => setExpanded((v) => !v)}>
            {expanded ? 'Hide' : 'Decide'}
          </Button>
        )}
        <Link
          to={item.detail_path}
          className="inline-block text-sm text-neutral-900 underline underline-offset-2 dark:text-neutral-100"
        >
          Open to decide →
        </Link>
      </div>
      {decidable && expanded && (
        <div className="rounded-md border border-neutral-200 bg-neutral-50 p-3 dark:border-neutral-800 dark:bg-neutral-900">
          <DecisionPanel
            item={item}
            onResolved={() => onResolved?.(item)}
            onCancel={() => setExpanded(false)}
          />
        </div>
      )}
    </article>
  );
}
