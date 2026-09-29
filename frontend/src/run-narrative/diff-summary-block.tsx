import { Section } from '@/plan/sections';
import { DegradeNotes, EvidenceLink } from './evidence';
import type { EvidenceRef, ScopeDivergence } from './narrative';
import type { SectionState } from './use-run-narrative';

/*
 * Declared-vs-staged diff summary: the plan's declared scope.files beside
 * the REAL changed-file list from the newest policy_evaluated payload, with
 * every path present on one side but not the other marked. Each column
 * renders independently — a missing side degrades that column only.
 */

/** `unknown` — the other side is unavailable, so no divergence can be claimed. */
type Divergence = 'declared_only' | 'staged_only' | 'both' | 'unknown';

const divergenceLabel: Record<'declared_only' | 'staged_only', string> = {
  declared_only: 'declared, not staged',
  staged_only: 'staged, not declared',
};

function PathList({
  paths,
  divergent,
  kind,
  compared,
}: {
  paths: readonly string[];
  divergent: ReadonlySet<string>;
  kind: 'declared_only' | 'staged_only';
  /** False when the other side is unavailable. */
  compared: boolean;
}) {
  if (paths.length === 0) return <p className="text-xs text-neutral-500">No files.</p>;
  return (
    <ul className="space-y-0.5 font-mono text-xs">
      {paths.map((p) => {
        const d: Divergence = !compared ? 'unknown' : divergent.has(p) ? kind : 'both';
        return (
          <li key={p} data-testid="scope-path" data-path={p} data-divergence={d}>
            <span className={d === 'declared_only' || d === 'staged_only' ? 'font-semibold' : ''}>
              {p}
            </span>
            {(d === 'declared_only' || d === 'staged_only') && (
              <span className="ml-2 rounded bg-amber-100 px-1 text-amber-800 dark:bg-amber-900/40 dark:text-amber-300">
                {divergenceLabel[d]}
              </span>
            )}
          </li>
        );
      })}
    </ul>
  );
}

function Column({
  title,
  testId,
  evidence,
  children,
}: {
  title: string;
  testId: string;
  evidence: EvidenceRef | null;
  children: React.ReactNode;
}) {
  return (
    <div
      data-testid={testId}
      className="space-y-2 rounded-md border border-neutral-200 p-3 dark:border-neutral-800"
    >
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-xs font-medium text-neutral-600 dark:text-neutral-400">{title}</h3>
        {evidence && <EvidenceLink refTo={evidence} />}
      </div>
      {children}
    </div>
  );
}

export function DiffSummaryBlock({
  section,
  declaredEvidence,
  stagedEvidence,
}: {
  section: SectionState<ScopeDivergence>;
  /** The plan artifact the declared scope comes from. */
  declaredEvidence: EvidenceRef | null;
  /** The policy_evaluated entry the staged scope comes from. */
  stagedEvidence: EvidenceRef | null;
}) {
  const d = section.model;
  const declaredOnly = new Set(d?.declaredOnly ?? []);
  const stagedOnly = new Set(d?.stagedOnly ?? []);
  return (
    <Section id="narrative-diff" title="Diff summary">
      <div className="space-y-3">
        <DegradeNotes notes={section.notes} />
        <div className="grid gap-3 md:grid-cols-2">
          <Column title="Declared scope" testId="declared-scope" evidence={declaredEvidence}>
            {d?.declared ? (
              <PathList
                paths={d.declared}
                divergent={declaredOnly}
                kind="declared_only"
                compared={d.staged !== null}
              />
            ) : (
              <p className="text-xs text-neutral-500">Unavailable.</p>
            )}
          </Column>
          <Column title="Staged scope" testId="staged-scope" evidence={stagedEvidence}>
            {d?.staged ? (
              <PathList
                paths={d.staged}
                divergent={stagedOnly}
                kind="staged_only"
                compared={d.declared !== null}
              />
            ) : (
              <p className="text-xs text-neutral-500">Unavailable.</p>
            )}
          </Column>
        </div>
      </div>
    </Section>
  );
}
