import { Section } from '@/plan/sections';
import { DegradeNotes, EvidenceLink } from './evidence';
import { safeExternalHref, type ApprovalRow } from './narrative';
import type { MergeSectionModel, SectionState } from './use-run-narrative';

/*
 * The two decision blocks. <ApprovalsBlock> renders who approved, how they
 * authenticated and through which channel (ADR-055 / #1709); every
 * enrichment field is optional on the wire, so each row renders ONLY when
 * its field is present — a legacy pre-#1709 approval shows identity (or
 * approver) alone, never an empty or placeholder row. <MergeBlock> renders
 * the terminal merge outcome, or an explicit not-merged state.
 */

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-neutral-500">{label}</dt>
      <dd className="font-mono text-xs">{children}</dd>
    </>
  );
}

function ApprovalItem({ row }: { row: ApprovalRow }) {
  const identity = row.identity
    ? [row.identity.provider, row.identity.subject].filter(Boolean).join(' · ')
    : '';
  return (
    <li
      data-testid="approval"
      data-decision={row.decision}
      className="rounded-md border-2 border-amber-500/60 p-3 dark:border-amber-400/50"
    >
      <dl className="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
        <Row label="Decision">{row.decision}</Row>
        {identity && <Row label="Identity">{identity}</Row>}
        {row.approver && <Row label="Approver">{row.approver}</Row>}
        {!identity && !row.approver && <Row label="Identity">not recorded</Row>}
        {row.authMethod && <Row label="Auth method">{row.authMethod}</Row>}
        {row.channel && <Row label="Channel">{row.channel}</Row>}
        {row.delegated && <Row label="Delegated">{row.delegated}</Row>}
        {row.onBehalfOf && <Row label="On behalf of">{row.onBehalfOf}</Row>}
        {row.comment && <Row label="Comment">{row.comment}</Row>}
        <Row label="Recorded">{row.ts}</Row>
        <Row label="Evidence">
          <EvidenceLink refTo={row.evidence} />
        </Row>
      </dl>
    </li>
  );
}

export function ApprovalsBlock({ section }: { section: SectionState<ApprovalRow[]> }) {
  const rows = section.model;
  return (
    <Section id="narrative-approvals" title="Approvals">
      <div className="space-y-3">
        <DegradeNotes notes={section.notes} />
        {rows &&
          (rows.length === 0 ? (
            <p className="text-sm text-neutral-500">No approvals recorded yet.</p>
          ) : (
            <ul className="space-y-2">
              {rows.map((r) => (
                <ApprovalItem key={r.sequence} row={r} />
              ))}
            </ul>
          ))}
      </div>
    </Section>
  );
}

export function MergeBlock({ section }: { section: SectionState<MergeSectionModel> }) {
  const model = section.model;
  const merge = model?.merge ?? null;
  const outcome = merge?.outcome ?? null;
  const verdict = merge?.verdict ?? null;
  const unavailable = model?.unavailable ?? [];
  let state: 'merged' | 'closed_without_merge' | 'verdict_only' | 'not_merged' | 'indeterminate';
  // A terminal outcome is positive evidence and stands on its own. With NO
  // terminal outcome and a failed merge read, the missing evidence could
  // itself be the merge event — so the state is indeterminate, never the
  // claim 'Not merged'.
  if (outcome) state = outcome.kind;
  else if (unavailable.length > 0) state = 'indeterminate';
  else if (verdict) state = 'verdict_only';
  else state = 'not_merged';
  const stateLabel = {
    merged: 'Merged',
    closed_without_merge: 'Closed without merge',
    verdict_only: 'Merge verdict recorded; merge not yet observed',
    not_merged: 'Not merged',
    indeterminate: 'Indeterminate: merge evidence could not be read',
  }[state];
  const prUrl = outcome?.prUrl ?? verdict?.prUrl;
  const prHref = safeExternalHref(prUrl);
  return (
    <Section id="narrative-merge" title="Merge">
      <div className="space-y-3">
        <DegradeNotes notes={section.notes} />
        {model && (
          <dl className="grid grid-cols-[8rem_1fr] gap-y-1 text-sm">
            <dt className="text-neutral-500">State</dt>
            <dd data-testid="merge-state" data-state={state} className="font-mono font-semibold">
              {stateLabel}
            </dd>
            {verdict && <Row label="Merge verdict">{verdict.verdict}</Row>}
            {outcome?.commitSha && <Row label="Commit">{outcome.commitSha}</Row>}
            {outcome?.actor && <Row label="By">{outcome.actor}</Row>}
            {prUrl && (
              <Row label="Pull request">
                {prHref ? (
                  <a
                    href={prHref}
                    rel="noopener noreferrer"
                    target="_blank"
                    className="text-blue-700 hover:underline dark:text-blue-300"
                  >
                    {prUrl}
                  </a>
                ) : (
                  // Not an http(s) url: render the recorded value as text.
                  <span data-testid="merge-pr-unlinked">{prUrl}</span>
                )}
              </Row>
            )}
            {state === 'indeterminate' && <Row label="Unreadable">{unavailable.join(', ')}</Row>}
            {(outcome || verdict) && (
              <Row label="Evidence">
                <span className="flex flex-wrap gap-2">
                  {outcome && <EvidenceLink refTo={outcome.evidence} />}
                  {verdict && <EvidenceLink refTo={verdict.evidence} />}
                </span>
              </Row>
            )}
          </dl>
        )}
      </div>
    </Section>
  );
}
