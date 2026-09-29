import { useId, useState } from 'react';
import { api, ApiClientError } from '@/api/client';
import { useAsync } from '@/api/use-async';
import { Button } from '@/components/ui/button';
import { verifyExportChain, type ChainVerification } from '@/repo/chain-verification';
import { RepoPanelFrame } from '@/repo/throughput-panel';

/*
 * Repo dashboard Record tab (E40.6 / #1718), at
 * /repos/:owner/:name?tab=record. Three sections:
 *
 *  1. Chain verification — one page of GET /v0/audit/export folded
 *     through verifyExportChain. The badge reports chain STRUCTURE and
 *     SIGNING-KEY COVERAGE only; it never claims a signature was
 *     verified or an entry hash recomputed, and it names the offline
 *     `fishhawk-verify` CLI as the cryptographic-proof path. See the
 *     header of chain-verification.ts for why that boundary is where it
 *     is. No anchored-checkpoint field is rendered — ADR-056 / #1699
 *     has not landed and nothing in the tree exposes one.
 *  2. Export — a one-click Export v1 download of the same endpoint,
 *     with a DEDICATED 403 branch naming the required scope.
 *  3. Release evidence — a visibly disabled placeholder, wired when
 *     E33 (#1583) ships.
 */

/** Runs requested per verification page. Whole-run bounded server-side. */
const EXPORT_PAGE_RUNS = 25;

export function RecordTab({ repo, owner, name }: { repo: string; owner: string; name: string }) {
  return (
    <div className="space-y-4">
      <ChainVerificationPanel repo={repo} />
      <ExportPanel repo={repo} owner={owner} name={name} />
      <ReleaseEvidencePlaceholder />
    </div>
  );
}

interface VerificationView {
  verification: ChainVerification;
  /*
   * RepoPanelFrame's generic is constrained to `{ truncated?: boolean }`
   * and renders its own PartialDataNote off it. That note is worded for a
   * truncated RUN SCAN, which is not what a partial export page is, so
   * this view never sets it — declared `never` to satisfy the constraint
   * while making the deliberate non-use explicit. The export's partiality
   * note is rendered by VerificationBody instead.
   */
  truncated?: never;
}

function ChainVerificationPanel({ repo }: { repo: string }) {
  const state = useAsync<VerificationView>(async () => {
    const page = await api.getAuditExportPage({ repo, limit: EXPORT_PAGE_RUNS });
    return { verification: verifyExportChain(page.data, { complete: page.complete }) };
  }, [repo]);

  return (
    <RepoPanelFrame title="Chain verification" state={state}>
      {(data) => <VerificationBody verification={data.verification} />}
    </RepoPanelFrame>
  );
}

const STATUS_LABEL: Record<ChainVerification['status'], string> = {
  pass: 'Pass',
  fail: 'Fail',
  unverifiable: 'Unverifiable',
};

const STATUS_CLASS: Record<ChainVerification['status'], string> = {
  pass: 'border-emerald-300 bg-emerald-50 text-emerald-900 dark:border-emerald-900/60 dark:bg-emerald-950/40 dark:text-emerald-200',
  fail: 'border-rose-300 bg-rose-50 text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200',
  unverifiable:
    'border-amber-300 bg-amber-50 text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200',
};

/** Issues rendered inline before the list is truncated with a count. */
const MAX_LISTED_ISSUES = 5;

function VerificationBody({ verification }: { verification: ChainVerification }) {
  const { status, issues, runsVerified, entriesChecked, complete } = verification;
  const structural = issues.filter(
    (i) =>
      i.kind === 'chain_broken' ||
      i.kind === 'first_entry_has_prev_hash' ||
      i.kind === 'sequence_not_monotonic',
  );
  const coverage = issues.filter(
    (i) =>
      i.kind === 'missing_signing_key' ||
      i.kind === 'signing_key_expired' ||
      i.kind === 'signing_key_not_yet_valid',
  );
  const unverifiable = status === 'unverifiable';

  return (
    <div className="space-y-3">
      <div
        className={`inline-flex rounded-md border px-3 py-1 text-sm font-medium ${STATUS_CLASS[status]}`}
      >
        {STATUS_LABEL[status]}
      </div>

      {unverifiable ? (
        <p className="text-sm text-neutral-700 dark:text-neutral-300">
          The export body did not match the Export v1 shape, so nothing was verified. This is
          reported as unverifiable, never as a pass.
        </p>
      ) : (
        <dl className="space-y-1 text-sm">
          <CheckLine
            label="Hash-chain integrity"
            ok={structural.length === 0}
            okText="Linkage, genesis and sequence hold for every run on this page."
            failText={`${structural.length} structural issue${structural.length === 1 ? '' : 's'}.`}
          />
          <CheckLine
            label="Signing-key coverage"
            ok={coverage.length === 0}
            okText="Every run with entries has a signing key valid at the export time."
            failText={`${coverage.length} coverage issue${coverage.length === 1 ? '' : 's'}.`}
          />
        </dl>
      )}

      <p className="text-sm text-neutral-600 dark:text-neutral-400">
        {runsVerified} run{runsVerified === 1 ? '' : 's'}, {entriesChecked} entr
        {entriesChecked === 1 ? 'y' : 'ies'} checked.
      </p>

      {issues.length > 0 && (
        <ul className="space-y-1 text-xs">
          {issues.slice(0, MAX_LISTED_ISSUES).map((issue, i) => (
            <li key={i} className="font-mono text-rose-800 dark:text-rose-300">
              {issue.kind}
              {issue.runId !== '' && ` · run ${issue.runId}`}
              {issue.sequence !== 0 && ` · seq ${issue.sequence}`} — {issue.detail}
            </li>
          ))}
          {issues.length > MAX_LISTED_ISSUES && (
            <li className="text-neutral-600 dark:text-neutral-400">
              …and {issues.length - MAX_LISTED_ISSUES} more.
            </li>
          )}
        </ul>
      )}

      {!complete && (
        <p className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200">
          Partial data: only the returned page of the export was checked, not the whole record for
          this repository.
        </p>
      )}

      <p className="text-xs text-neutral-600 dark:text-neutral-400">
        This badge checks chain structure and signing-key coverage. It does not recompute entry
        hashes and does not verify trace-bundle signatures — the export carries content-hash
        pointers only. Run the offline <code className="font-mono">fishhawk-verify</code> CLI
        against a downloaded export for cryptographic proof.
      </p>
    </div>
  );
}

function CheckLine({
  label,
  ok,
  okText,
  failText,
}: {
  label: string;
  ok: boolean;
  okText: string;
  failText: string;
}) {
  return (
    <div className="flex flex-wrap gap-x-2">
      <dt className="font-medium">{label}</dt>
      <dd
        className={
          ok ? 'text-neutral-600 dark:text-neutral-400' : 'text-rose-800 dark:text-rose-300'
        }
      >
        {ok ? okText : failText}
      </dd>
    </div>
  );
}

/** The scope the export endpoint requires when the caller is a token. */
const EXPORT_SCOPE_FALLBACK = 'read:audit-export';

function requiredScopeOf(err: ApiClientError): string {
  const scope = err.body?.details?.required_scope;
  return typeof scope === 'string' && scope !== '' ? scope : EXPORT_SCOPE_FALLBACK;
}

function ExportPanel({ repo, owner, name }: { repo: string; owner: string; name: string }) {
  const headingId = useId();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error | null>(null);

  async function download() {
    setBusy(true);
    setError(null);
    let url: string | null = null;
    let anchor: HTMLAnchorElement | null = null;
    try {
      const res = await api.getAuditExportDownload({ repo });
      const blob = await res.blob();
      url = URL.createObjectURL(blob);
      anchor = document.createElement('a');
      anchor.href = url;
      anchor.download =
        filenameFromDisposition(res.headers.get('Content-Disposition')) ??
        `fishhawk-audit-export-${owner}-${name}.json`;
      document.body.appendChild(anchor);
      anchor.click();
    } catch (err: unknown) {
      setError(err instanceof Error ? err : new Error(String(err)));
    } finally {
      if (anchor !== null) anchor.remove();
      if (url !== null) URL.revokeObjectURL(url);
      setBusy(false);
    }
  }

  const reauth = error instanceof ApiClientError && error.status === 403 ? error : null;

  return (
    <section
      aria-labelledby={headingId}
      className="space-y-3 rounded-md border border-neutral-200 p-4 dark:border-neutral-800"
    >
      <h2 id={headingId} className="text-base font-semibold tracking-tight">
        Export
      </h2>
      <p className="text-sm text-neutral-600 dark:text-neutral-400">
        Download the Export v1 compliance bundle for this repository — the input the offline{' '}
        <code className="font-mono">fishhawk-verify</code> CLI consumes.
      </p>
      <Button type="button" onClick={() => void download()} disabled={busy}>
        {busy ? 'Preparing export…' : 'Download export (JSON)'}
      </Button>
      {reauth !== null ? (
        <div
          role="alert"
          className="rounded-md border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200"
        >
          <div className="font-medium">
            This token is missing the <code className="font-mono">{requiredScopeOf(reauth)}</code>{' '}
            scope.
          </div>
          <div className="mt-1">
            Re-authenticate with a token that carries it, then try the download again.
          </div>
        </div>
      ) : (
        error !== null && (
          <div
            role="alert"
            className="rounded-md border border-rose-300 bg-rose-50 p-3 text-sm text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
          >
            <div className="font-medium">Couldn&apos;t download the export.</div>
            <div className="mt-1 font-mono text-xs">{error.message}</div>
          </div>
        )
      )}
    </section>
  );
}

/** Reads `filename="…"` out of a Content-Disposition header, if present. */
function filenameFromDisposition(header: string | null): string | null {
  if (header === null) return null;
  const m = header.match(/filename="?([^";]+)"?/);
  return m ? m[1] : null;
}

function ReleaseEvidencePlaceholder() {
  const headingId = useId();
  return (
    <section
      aria-labelledby={headingId}
      className="space-y-3 rounded-md border border-dashed border-neutral-300 p-4 opacity-70 dark:border-neutral-700"
    >
      <h2 id={headingId} className="text-base font-semibold tracking-tight">
        Release evidence
      </h2>
      <p className="text-sm text-neutral-600 dark:text-neutral-400">
        Coming soon: per-release evidence bundles are wired when E33 (#1583) ships. Nothing is
        collected for this repository yet.
      </p>
      <Button type="button" variant="outline" disabled>
        View release evidence
      </Button>
    </section>
  );
}
