import type { ReactNode } from 'react';
import { Link } from 'react-router';
import type { AsyncState } from '@/api/use-async';
import type {
  HandoverBrief,
  HandoverBriefCursor,
  HandoverBriefPart,
  HandoverBriefPartKind,
  HandoverBriefSection,
  HandoverBriefSectionKind,
} from '@/api/captain';
import { RepoPanelFrame } from '@/repo/throughput-panel';
import { apiErrorCode, captainRefusalMessage, shortHash } from './viewer';

/*
 * The handover brief render (E76.6 / #3769) over GET /v0/handover-brief
 * (E76.4 / #3767). Every row is CITED — sequence + truncated entry hash + run
 * link — and every failure is NAMED:
 *
 *  - a part's `unavailable` renders its `unavailable_reason`, NEVER an empty
 *    part that reads as "nothing happened" (an unavailable part's `items` is
 *    empty either way, so only this branch tells a failed read from a quiet
 *    window);
 *  - `truncated` / `omitted_count` render the count plus the `next` cursor's
 *    `call` verbatim — the un-elided retrieval surface;
 *  - `gaps`, `degradations` and `absent` render as named lists;
 *  - a 503 `handover_brief_unavailable` / 501 `handover_brief_unconfigured`
 *    renders the named reason, not the generic panel error.
 *
 * The brief does NOT read delegation confirmations (its `workflows` carry the
 * constant `confirmation: unavailable`); the delegation section of this tab is
 * the confirmation read.
 */

const SECTION_ORDER: HandoverBriefSectionKind[] = [
  'what_changed',
  'needs_decision',
  'in_flight',
  'delegation_in_force',
  'standing_orders',
];

const SECTION_TITLE: Record<HandoverBriefSectionKind, string> = {
  what_changed: 'What changed',
  needs_decision: 'Needs a decision',
  in_flight: 'In flight',
  delegation_in_force: 'Delegation in force',
  standing_orders: 'Standing orders',
};

const PART_TITLE: Record<HandoverBriefPartKind, string> = {
  merges: 'Merges',
  waivers_and_deferrals: 'Waivers and deferrals',
  unanswered_pages: 'Unanswered pages',
  open_decisions: 'Open decisions',
  campaigns: 'Campaigns',
  runs: 'Runs',
  workflows: 'Workflows',
};

// `truncated?: false` satisfies RepoPanelFrame's weak-type constraint without
// ever triggering its run-scan partial-data note.
type BriefView = ({ kind: 'brief'; brief: HandoverBrief } | { kind: 'named'; message: string }) & {
  truncated?: false;
};

const NAMED_ERRORS = new Set(['handover_brief_unavailable', 'handover_brief_unconfigured']);

function toView(state: AsyncState<HandoverBrief>): AsyncState<BriefView> {
  if (state.status === 'error' && NAMED_ERRORS.has(apiErrorCode(state.error) ?? '')) {
    const detail = state.error.message;
    return {
      status: 'ok',
      data: { kind: 'named', message: `${captainRefusalMessage(state.error)} ${detail}` },
    };
  }
  // `truncated` is deliberately NOT forwarded to the frame: RepoPanelFrame's
  // partial-data note speaks of a run scan, which is the wrong claim here.
  if (state.status === 'ok') return { status: 'ok', data: { kind: 'brief', brief: state.data } };
  return state;
}

export function HandoverBriefPanel({ state }: { state: AsyncState<HandoverBrief> }) {
  return (
    <RepoPanelFrame title="Handover brief" state={toView(state)}>
      {(view) =>
        view.kind === 'named' ? (
          <p role="note" className="text-sm text-neutral-700 dark:text-neutral-300">
            {view.message}
          </p>
        ) : (
          <BriefBody brief={view.brief} />
        )
      }
    </RepoPanelFrame>
  );
}

function sortSections(sections: HandoverBriefSection[]): HandoverBriefSection[] {
  const rank = (k: HandoverBriefSectionKind) => {
    const i = SECTION_ORDER.indexOf(k);
    return i === -1 ? SECTION_ORDER.length : i;
  };
  return [...sections].sort((a, b) => rank(a.kind) - rank(b.kind));
}

function BriefBody({ brief }: { brief: HandoverBrief }) {
  const w = brief.window;
  return (
    <div className="space-y-4 text-sm">
      <dl className="grid gap-1 sm:grid-cols-[max-content_1fr] sm:gap-x-4">
        <dt className="text-neutral-500">Window</dt>
        <dd className="font-mono text-xs">
          #{w.from_sequence} → #{w.to_sequence} (chain head #{w.chain_head}) · {w.basis}
        </dd>
        <dt className="text-neutral-500">Brief hash</dt>
        <dd className="font-mono text-xs" title={brief.brief_hash} data-testid="brief-hash">
          {shortHash(brief.brief_hash)}
        </dd>
        {brief.captain_subject && (
          <>
            <dt className="text-neutral-500">Captain</dt>
            <dd className="font-mono text-xs">{brief.captain_subject}</dd>
          </>
        )}
        {brief.successor && (
          <>
            <dt className="text-neutral-500">Successor</dt>
            <dd className="font-mono text-xs">{brief.successor}</dd>
          </>
        )}
      </dl>

      {brief.truncated && (
        <Elided label="The brief was bounded; parts were elided." cursor={brief.next} />
      )}

      {sortSections(brief.sections).map((s) => (
        <Section key={s.kind} section={s} />
      ))}

      {brief.absent.length > 0 && (
        <NamedList title="Not composed (no source)">
          {brief.absent.map((a) => (
            <li key={a.section}>
              <span className="font-mono">{a.section}</span> — {a.reason} ({a.anchor})
            </li>
          ))}
        </NamedList>
      )}

      {brief.gaps.length > 0 && (
        <NamedList title="Gaps">
          {brief.gaps.map((g, i) => (
            <li key={`${g.kind}-${g.sequence}-${i}`}>
              <span className="font-mono">{g.kind}</span> · #{g.sequence}
              {g.category ? ` · ${g.category}` : ''}
              {g.detail ? ` — ${g.detail}` : ''}
            </li>
          ))}
        </NamedList>
      )}
      {brief.gaps_truncated && (
        <Elided
          label={`${brief.gaps_omitted_count} more gap${brief.gaps_omitted_count === 1 ? '' : 's'} omitted.`}
          cursor={brief.gaps_next}
        />
      )}

      {brief.degradations.length > 0 && (
        <NamedList title="Degradations">
          {brief.degradations.map((d, i) => (
            <li key={`${d.kind}-${i}`}>
              <span className="font-mono">{d.kind}</span>
              {d.section ? ` · ${d.section}` : ''}
              {d.part ? `/${d.part}` : ''}
              {d.sequence !== undefined ? ` · #${d.sequence}` : ''}
              {d.detail ? ` — ${d.detail}` : ''}
            </li>
          ))}
        </NamedList>
      )}

      {brief.uncited_next && (
        <Elided label="Uncited entries are retrievable separately." cursor={brief.uncited_next} />
      )}
    </div>
  );
}

function Section({ section }: { section: HandoverBriefSection }) {
  const title = SECTION_TITLE[section.kind] ?? section.kind;
  return (
    <section aria-label={title} className="space-y-2">
      <h3 className="text-sm font-semibold">{title}</h3>
      {section.unavailable && (
        <p role="note" className="text-xs text-rose-700 dark:text-rose-300">
          Every part of this section is unavailable.
        </p>
      )}
      {section.standing_orders && (
        <dl className="grid gap-1 font-mono text-xs sm:grid-cols-[max-content_1fr] sm:gap-x-4">
          <dt className="text-neutral-500">source</dt>
          <dd>
            {section.standing_orders.source}
            {section.standing_orders.ref ? ` @ ${section.standing_orders.ref}` : ''}
          </dd>
          {section.standing_orders.spec_version && (
            <>
              <dt className="text-neutral-500">spec version</dt>
              <dd>{section.standing_orders.spec_version}</dd>
            </>
          )}
          <dt className="text-neutral-500">schema major</dt>
          <dd>{section.standing_orders.schema_major}</dd>
          <dt className="text-neutral-500">delegation hash</dt>
          <dd title={section.standing_orders.delegation_content_hash}>
            {shortHash(section.standing_orders.delegation_content_hash)}
          </dd>
        </dl>
      )}
      {section.parts.map((p) => (
        <Part key={p.kind} part={p} />
      ))}
    </section>
  );
}

function Part({ part }: { part: HandoverBriefPart }) {
  const title = PART_TITLE[part.kind] ?? part.kind;
  if (part.unavailable) {
    return (
      <div data-part={part.kind} className="space-y-1">
        <h4 className="text-xs font-medium tracking-wide text-neutral-500 uppercase">{title}</h4>
        <p role="note" className="text-xs text-rose-700 dark:text-rose-300">
          Unavailable: {part.unavailable_reason ?? 'no reason recorded'} — this read failed; an
          empty list here does not mean nothing happened.
        </p>
      </div>
    );
  }
  const items = part.items ?? [];
  const inFlight = part.in_flight ?? [];
  const workflows = part.workflows ?? [];
  const empty = items.length === 0 && inFlight.length === 0 && workflows.length === 0;
  return (
    <div data-part={part.kind} className="space-y-1">
      <h4 className="text-xs font-medium tracking-wide text-neutral-500 uppercase">{title}</h4>
      {empty && part.omitted_count === 0 && (
        <p className="text-xs text-neutral-500">Nothing in this window.</p>
      )}
      {items.length > 0 && (
        <ul className="space-y-1 text-xs">
          {items.map((it) => (
            <li key={`${it.source_sequence}-${it.category}`} className="flex flex-wrap gap-x-2">
              <span className="font-mono">{it.category}</span>
              <span className="font-mono text-neutral-500">
                #{it.source_sequence} · {shortHash(it.source_entry_hash)}
              </span>
              <Link to={`/runs/${it.run_id}`} className="font-mono underline">
                run {it.run_id.slice(0, 8)}
              </Link>
              <time dateTime={it.at} className="text-neutral-500">
                {it.at}
              </time>
              {it.headline && <span>{it.headline}</span>}
              {it.source_missing && (
                <span className="text-rose-700 dark:text-rose-300">source entry missing</span>
              )}
            </li>
          ))}
        </ul>
      )}
      {inFlight.length > 0 && (
        <ul className="space-y-1 text-xs">
          {inFlight.map((f) => (
            <li key={f.id} className="flex flex-wrap gap-x-2">
              <Link
                to={f.kind === 'campaign' ? `/campaigns/${f.id}` : `/runs/${f.id}`}
                className="font-mono underline"
              >
                {f.kind} {f.id.slice(0, 8)}
              </Link>
              <span>{f.state}</span>
              {f.workflow_id && <span className="font-mono">{f.workflow_id}</span>}
              {f.ref && <span className="font-mono">{f.ref}</span>}
              <time dateTime={f.created_at} className="text-neutral-500">
                {f.created_at}
              </time>
            </li>
          ))}
        </ul>
      )}
      {workflows.length > 0 && (
        <ul className="space-y-1 text-xs">
          {workflows.map((wf) => (
            <li key={wf.workflow_id} className="flex flex-wrap gap-x-2">
              <span className="font-mono">{wf.workflow_id}</span>
              <span>{wf.autonomy ? `autonomy ${wf.autonomy}` : 'no declared tier'}</span>
              {wf.must_page_human && wf.must_page_human.length > 0 && (
                <span>pages on {wf.must_page_human.join(', ')}</span>
              )}
              <span className="font-mono text-neutral-500" title={wf.content_hash}>
                {shortHash(wf.content_hash)}
              </span>
              <span className="text-neutral-500">
                confirmation not read by the brief — see the delegation section of this tab
              </span>
            </li>
          ))}
        </ul>
      )}
      {(part.truncated || part.omitted_count > 0) && (
        <Elided label={`${part.omitted_count} more omitted.`} cursor={part.next} />
      )}
    </div>
  );
}

function Elided({ label, cursor }: { label: string; cursor?: HandoverBriefCursor }) {
  return (
    <p className="text-xs text-amber-900 dark:text-amber-200">
      {label}
      {cursor ? (
        <>
          {' '}
          Retrieve with: <code className="font-mono break-all">{cursor.call}</code>
        </>
      ) : (
        ' No retrieval cursor was provided.'
      )}
    </p>
  );
}

function NamedList({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="space-y-1">
      <h3 className="text-sm font-semibold">{title}</h3>
      <ul className="list-disc space-y-0.5 pl-5 text-xs">{children}</ul>
    </div>
  );
}
