import { useEffect, useState } from 'react';
import { ChevronDown, ChevronRight, TerminalSquare } from 'lucide-react';
import { api, ApiClientError } from '@/api/client';
import { Section } from '@/plan/sections';
import {
  eventLines,
  findManifest,
  findTrailer,
  parseBundle,
  type BundleLine,
  type ManifestData,
  type TrailerData,
} from './transcript-bundle';
import {
  describeEvent,
  totalTokens,
  type ContentBlock,
  type DescribedEvent,
} from './transcript-event';

/*
 * Transcript section for the implement-stage session view (#218).
 *
 * Fetches the redacted trace bundle, parses it into events, and
 * renders a chat-like log:
 *
 *   - Manifest summary (agent, model, agent_failed flag).
 *   - Per-event row with role-specific styling.
 *   - Tool-use / tool_result blocks default-collapsed; click to
 *     reveal inputs / outputs.
 *   - Token total at the bottom (when usage blocks present).
 *
 * Default rendering for assistant text is plain (preformatted)
 * rather than markdown — keeps the v0 dependency footprint flat.
 * Markdown can graduate when a reviewer asks for it; pick the
 * choice in a follow-up.
 */

interface Props {
  stageId: string;
}

type State =
  | { kind: 'idle' }
  | { kind: 'loading' }
  | {
      kind: 'ok';
      manifest: ManifestData | null;
      events: DescribedEvent[];
      trailer: TrailerData | null;
    }
  | { kind: 'empty' }
  | { kind: 'error'; message: string };

export function TranscriptSection({ stageId }: Props) {
  const [state, setState] = useState<State>({ kind: 'idle' });

  useEffect(() => {
    let cancelled = false;
    setState({ kind: 'loading' });

    void (async () => {
      try {
        const res = await api.getStageTraceStream(stageId);
        if (!res.body) {
          if (!cancelled) setState({ kind: 'error', message: 'response body missing' });
          return;
        }
        const lines: BundleLine[] = await parseBundle(res.body);
        if (cancelled) return;
        const events = eventLines(lines).map(describeEvent);
        setState({
          kind: 'ok',
          manifest: findManifest(lines),
          events,
          trailer: findTrailer(lines),
        });
      } catch (err) {
        if (cancelled) return;
        if (err instanceof ApiClientError && err.status === 404) {
          setState({ kind: 'empty' });
          return;
        }
        const msg = err instanceof Error ? err.message : String(err);
        setState({ kind: 'error', message: msg });
      }
    })();

    return () => {
      cancelled = true;
    };
  }, [stageId]);

  return (
    <Section id="transcript" title="Transcript" collapsible>
      <Body state={state} />
    </Section>
  );
}

function Body({ state }: { state: State }) {
  switch (state.kind) {
    case 'idle':
    case 'loading':
      return <p className="text-sm text-neutral-500">Loading transcript…</p>;

    case 'empty':
      return (
        <p className="rounded-md border border-dashed border-neutral-300 p-4 text-sm text-neutral-500 dark:border-neutral-700">
          Trace bundle not yet uploaded for this stage.
        </p>
      );

    case 'error':
      return (
        <div
          role="alert"
          className="rounded-md border border-rose-300 bg-rose-50 p-3 font-mono text-xs text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
        >
          Couldn&apos;t load transcript: {state.message}
        </div>
      );

    case 'ok': {
      const tokens = totalTokens(state.events);
      return (
        <div className="space-y-4">
          <ManifestRow
            manifest={state.manifest}
            eventCount={state.events.length}
            totalTokens={tokens}
          />
          <ol className="space-y-2">
            {state.events.map((event) => (
              <li key={event.id} id={event.id}>
                <EventRow event={event} />
              </li>
            ))}
          </ol>
          {state.trailer?.content_hash && (
            <p
              className="font-mono text-[10px] text-neutral-400"
              title={state.trailer.content_hash}
            >
              bundle sha256: {state.trailer.content_hash.slice(0, 12)}…
            </p>
          )}
        </div>
      );
    }
  }
}

function ManifestRow({
  manifest,
  eventCount,
  totalTokens,
}: {
  manifest: ManifestData | null;
  eventCount: number;
  totalTokens: number | null;
}) {
  const parts: string[] = [];
  if (manifest?.agent) parts.push(manifest.agent);
  if (manifest?.model) parts.push(manifest.model);
  parts.push(`${eventCount} event${eventCount === 1 ? '' : 's'}`);
  if (totalTokens != null) parts.push(`${totalTokens} tokens`);

  return (
    <div className="rounded-md border border-neutral-200 bg-neutral-50 p-3 text-xs dark:border-neutral-800 dark:bg-neutral-900">
      <div className="flex flex-wrap items-center gap-3 font-mono text-neutral-600 dark:text-neutral-400">
        <TerminalSquare className="size-4 shrink-0 text-neutral-500" aria-hidden />
        <span>{parts.join(' · ')}</span>
      </div>
      {manifest?.agent_failed && (
        <div className="mt-2 rounded border border-rose-300 bg-rose-50 p-2 font-mono text-xs text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200">
          Agent failed: {manifest.agent_failure_reason || '(no reason supplied)'}
        </div>
      )}
    </div>
  );
}

function EventRow({ event }: { event: DescribedEvent }) {
  const roleStyle: Record<DescribedEvent['role'], string> = {
    assistant: 'border-neutral-200 dark:border-neutral-800',
    user: 'border-neutral-200 bg-neutral-50 dark:border-neutral-800 dark:bg-neutral-900',
    system: 'border-neutral-200 bg-neutral-50 dark:border-neutral-800 dark:bg-neutral-900/40',
    result: 'border-neutral-200 dark:border-neutral-800',
    stderr: 'border-rose-300 bg-rose-50 dark:border-rose-900/60 dark:bg-rose-950/30',
    other: 'border-neutral-200 dark:border-neutral-800',
  };

  return (
    <div className={`rounded-md border p-3 ${roleStyle[event.role]}`}>
      <div className="flex items-baseline justify-between gap-3">
        <div className="flex items-baseline gap-2">
          <span className="font-mono text-xs tracking-wide text-neutral-500 uppercase">
            {event.label}
          </span>
          {event.detail && (
            <span className="font-mono text-xs text-neutral-500">{event.detail}</span>
          )}
        </div>
        {event.ts && (
          <span className="font-mono text-[10px] text-neutral-400">
            {new Date(event.ts).toLocaleTimeString()}
          </span>
        )}
      </div>
      {event.blocks && event.blocks.length > 0 && (
        <div className="mt-2 space-y-2">
          {event.blocks.map((block, i) => (
            <BlockRow key={i} block={block} />
          ))}
        </div>
      )}
      <RawDetails raw={event.raw} />
    </div>
  );
}

function BlockRow({ block }: { block: ContentBlock }) {
  switch (block.kind) {
    case 'text':
      return <p className="font-mono text-sm leading-relaxed whitespace-pre-wrap">{block.text}</p>;

    case 'tool_use': {
      const summary = `${block.name}${summarizeInput(block.input)}`;
      return (
        <Disclosure label={`Tool · ${summary}`}>
          <pre className="overflow-x-auto rounded bg-neutral-100 p-2 font-mono text-xs whitespace-pre-wrap dark:bg-neutral-900">
            {prettyJSON(block.input)}
          </pre>
        </Disclosure>
      );
    }

    case 'tool_result': {
      const tone = block.isError
        ? 'text-rose-700 dark:text-rose-300'
        : 'text-neutral-600 dark:text-neutral-400';
      return (
        <Disclosure
          label={<span className={tone}>{block.isError ? 'Tool error' : 'Tool result'}</span>}
        >
          <pre className="overflow-x-auto rounded bg-neutral-100 p-2 font-mono text-xs whitespace-pre-wrap dark:bg-neutral-900">
            {typeof block.content === 'string' ? block.content : prettyJSON(block.content)}
          </pre>
        </Disclosure>
      );
    }

    case 'unknown':
      return (
        <Disclosure label="Unknown block">
          <pre className="overflow-x-auto rounded bg-neutral-100 p-2 font-mono text-xs whitespace-pre-wrap dark:bg-neutral-900">
            {prettyJSON(block.raw)}
          </pre>
        </Disclosure>
      );
  }
}

function Disclosure({ label, children }: { label: React.ReactNode; children: React.ReactNode }) {
  const [open, setOpen] = useState(false);
  return (
    <div>
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="inline-flex items-center gap-1 font-mono text-xs text-neutral-600 hover:text-neutral-900 dark:text-neutral-400 dark:hover:text-neutral-100"
        aria-expanded={open}
      >
        {open ? (
          <ChevronDown className="size-3.5" aria-hidden />
        ) : (
          <ChevronRight className="size-3.5" aria-hidden />
        )}
        {label}
      </button>
      {open && <div className="mt-1">{children}</div>}
    </div>
  );
}

function RawDetails({ raw }: { raw: unknown }) {
  const [open, setOpen] = useState(false);
  if (raw == null) return null;
  return (
    <div className="mt-2">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="font-mono text-[10px] text-neutral-400 hover:text-neutral-600 dark:hover:text-neutral-200"
        aria-expanded={open}
      >
        {open ? 'Hide raw' : 'Show raw'}
      </button>
      {open && (
        <pre className="mt-1 overflow-x-auto rounded bg-neutral-100 p-2 font-mono text-[10px] whitespace-pre-wrap dark:bg-neutral-900">
          {prettyJSON(raw)}
        </pre>
      )}
    </div>
  );
}

function summarizeInput(input: unknown): string {
  if (!input || typeof input !== 'object') return '';
  const obj = input as Record<string, unknown>;
  const keysOfInterest = ['command', 'file_path', 'path', 'query', 'pattern'];
  for (const k of keysOfInterest) {
    const v = obj[k];
    if (typeof v === 'string' && v.length > 0) {
      return ` · ${v.length > 60 ? `${v.slice(0, 59)}…` : v}`;
    }
  }
  return '';
}

function prettyJSON(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
