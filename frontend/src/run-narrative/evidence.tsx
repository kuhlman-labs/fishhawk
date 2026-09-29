import { useEffect } from 'react';
import { Link } from 'react-router';
import {
  evidenceAnchorId,
  evidenceHref,
  type EvidenceRef,
  type TransitionTreatment,
} from './narrative';
import { useAuditEntry, type DegradeNote } from './use-run-narrative';

/*
 * Shared render primitives for the run-detail evidence narrative (#1715).
 *
 * <EvidenceLink> is the ONLY way a narrative claim links to its backing
 * evidence: an audit-backed claim goes to /runs/:runId?entry=<sequence>
 * (plus the in-page #entry-<sequence> anchor), which <EvidencePanel>
 * resolves independently of the #audit list's pagination; an
 * artifact-backed claim goes to the stage page that renders the artifact.
 * The link text is the truncated entry/content hash with the full value in
 * `title`, matching <AuditEntryRow>.
 */

function truncateHash(hash: string): string {
  return hash.length > 12 ? `${hash.slice(0, 12)}…` : hash;
}

export function EvidenceLink({ refTo }: { refTo: EvidenceRef }) {
  const hash = refTo.kind === 'audit' ? refTo.entryHash : refTo.contentHash;
  const label =
    refTo.kind === 'audit'
      ? `audit entry #${refTo.sequence} (${refTo.category})`
      : `${refTo.artifactKind} artifact`;
  return (
    <Link
      to={evidenceHref(refTo)}
      data-testid="evidence-link"
      data-evidence-kind={refTo.kind}
      title={hash ? `${label} · ${hash}` : label}
      aria-label={`Evidence: ${label}`}
      className="font-mono text-xs text-blue-700 hover:underline dark:text-blue-300"
    >
      {hash ? truncateHash(hash) : label}
    </Link>
  );
}

const treatmentLabel: Record<TransitionTreatment, string> = {
  gate: 'Gate',
  auto_advance: 'Auto-advance',
  other: 'Transition',
};

const treatmentStyle: Record<TransitionTreatment, string> = {
  // A gate is a decision point: bordered, labelled, higher contrast.
  gate: 'rounded border-2 border-amber-500 bg-amber-50 px-1.5 py-0.5 font-semibold text-amber-900 dark:border-amber-400 dark:bg-amber-950/40 dark:text-amber-200',
  // A mechanical auto-advance is muted and inline.
  auto_advance: 'px-1 text-neutral-400 italic dark:text-neutral-500',
  other: 'px-1 text-neutral-500',
};

/**
 * The gate-vs-auto-advance visual treatment. `data-treatment` carries the
 * classification so tests assert the treatment rather than a class string.
 */
export function TransitionMarker({ kind, label }: { kind: TransitionTreatment; label?: string }) {
  return (
    <span
      data-testid="transition-marker"
      data-treatment={kind}
      className={`inline-flex items-center font-mono text-xs ${treatmentStyle[kind]}`}
    >
      {label ?? treatmentLabel[kind]}
    </span>
  );
}

/** A section's degrade notes, each naming the read it is about. Renders nothing when empty. */
export function DegradeNotes({ notes }: { notes: readonly DegradeNote[] }) {
  if (notes.length === 0) return null;
  return (
    <ul data-testid="degrade-notes" className="space-y-1">
      {notes.map((n, i) => (
        <li
          key={`${n.read}-${i}`}
          role="note"
          data-degrade-kind={n.kind}
          data-read={n.read}
          className="rounded border border-dashed border-neutral-300 px-2 py-1 text-xs text-neutral-600 dark:border-neutral-700 dark:text-neutral-400"
        >
          {n.message}
        </li>
      ))}
    </ul>
  );
}

function payloadSummary(payload: unknown): string {
  let text: string;
  try {
    text = JSON.stringify(payload, null, 2) ?? String(payload);
  } catch {
    text = String(payload);
  }
  return text.length > 2000 ? `${text.slice(0, 2000)}…` : text;
}

/**
 * The target of an audit-backed EvidenceLink: resolves ?entry=<sequence> to
 * that one entry via `since_sequence = N-1, limit = 1` (useAuditEntry), so
 * an entry far past the #audit list's first page still resolves. A sequence
 * that returns nothing renders the named "entry not found" state.
 */
export function EvidencePanel({ runId, sequence }: { runId: string; sequence: number }) {
  const state = useAuditEntry(runId, sequence);
  const anchor = evidenceAnchorId(sequence);

  useEffect(() => {
    if (state.status !== 'ok') return;
    document.getElementById(anchor)?.scrollIntoView?.({ block: 'start' });
  }, [state.status, anchor]);

  let body: React.ReactNode;
  if (state.status === 'loading') {
    body = <p className="text-sm text-neutral-500">Loading audit entry #{sequence}…</p>;
  } else if (state.status === 'error') {
    body = (
      <p role="alert" className="text-sm text-rose-800 dark:text-rose-300">
        Couldn&apos;t load audit entry #{sequence}: {state.error.message}
      </p>
    );
  } else if (state.data === null) {
    body = (
      <p data-testid="evidence-entry-not-found" className="text-sm text-neutral-600">
        Audit entry #{sequence} not found for this run.
      </p>
    );
  } else {
    const e = state.data;
    body = (
      <dl className="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
        <dt className="text-neutral-500">Category</dt>
        <dd className="font-mono">{e.category}</dd>
        <dt className="text-neutral-500">Sequence</dt>
        <dd className="font-mono">#{e.sequence}</dd>
        <dt className="text-neutral-500">Entry hash</dt>
        <dd data-testid="evidence-entry-hash" className="font-mono text-xs break-all">
          {e.entry_hash}
        </dd>
        <dt className="text-neutral-500">Payload</dt>
        <dd>
          <pre className="max-h-64 overflow-auto rounded bg-neutral-50 p-2 font-mono text-xs dark:bg-neutral-900">
            {payloadSummary(e.payload)}
          </pre>
        </dd>
      </dl>
    );
  }

  return (
    <section
      id={anchor}
      data-testid="evidence-panel"
      aria-label={`Evidence: audit entry #${sequence}`}
      className="scroll-mt-8 space-y-2 rounded-md border border-blue-300 p-4 dark:border-blue-800"
    >
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-medium tracking-wide text-neutral-600 uppercase dark:text-neutral-400">
          Evidence
        </h2>
        <Link
          to={`/runs/${encodeURIComponent(runId)}#audit`}
          className="text-xs text-neutral-500 hover:underline"
        >
          Full audit log ↓
        </Link>
      </div>
      {body}
    </section>
  );
}
