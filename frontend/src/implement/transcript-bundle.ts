/*
 * Trace-bundle parsing utilities for the implement-stage transcript
 * view (#218).
 *
 * The backend serves gzipped JSONL bytes; modern browsers handle
 * gzip-decoding transparently when `Content-Encoding: gzip` is
 * present, so what reaches the SPA is plain UTF-8 newline-delimited
 * JSON. The runner's `bundle.SchemaV1` shape (per ADR-007) is:
 *
 *   - first line: `{seq, ts, kind: "manifest", data: ManifestData}`
 *   - middle:     `{seq, ts, kind, data}` — one per agent.Event
 *   - last:       `{seq, ts, kind: "trailer", data: TrailerData}`
 *
 * For Claude-Code stages, the middle lines' `kind` mirrors the
 * stream-json `type[.subtype]` of the captured line (e.g.
 * `system.init`, `assistant`, `user`, `result.success`, plus
 * `stderr`, `raw` for non-typed text).
 *
 * `parseBundle` reads a Response body, splits on newlines, and
 * yields one BundleLine at a time. It tolerates:
 *
 *   - LF or CRLF line endings.
 *   - Trailing blank lines (gzip can leave artifacts).
 *   - Malformed JSON lines — yielded as `kind: "unparsed"` so the
 *     transcript surfaces them rather than dropping silently.
 */

export interface BundleLine {
  seq: number;
  ts: string;
  kind: string;
  data?: unknown;
  /** True when the JSON parse failed; raw is the verbatim text. */
  unparsed?: { raw: string };
}

export interface ManifestData {
  bundle_schema?: string;
  run_id?: string;
  stage_id?: string;
  agent?: string;
  model?: string;
  generated_at?: string;
  agent_failed?: boolean;
  agent_failure_reason?: string;
}

export interface TrailerData {
  event_count?: number;
  content_hash?: string;
}

/**
 * parseBundle yields each line of the bundle as a BundleLine. It
 * fully drains the body before returning; for the v0 trace volume
 * (capped at 64 MiB gzipped per the runner) this is the simpler
 * shape. A future streaming variant can chunk-render as bytes
 * arrive — same parser, different consumer.
 */
export async function parseBundle(body: ReadableStream<Uint8Array>): Promise<BundleLine[]> {
  const reader = body.getReader();
  const decoder = new TextDecoder('utf-8');
  let buffer = '';
  const lines: BundleLine[] = [];

  const flushLine = (raw: string) => {
    const trimmed = raw.replace(/\r$/, '');
    if (trimmed.length === 0) return;
    let parsed: BundleLine | undefined;
    try {
      const obj = JSON.parse(trimmed) as BundleLine;
      // Defensive: the schema requires seq/ts/kind. If they're
      // missing, surface as unparsed rather than passing through
      // a half-formed record.
      if (typeof obj.seq === 'number' && typeof obj.kind === 'string') {
        parsed = obj;
      }
    } catch {
      // Fall through — emit unparsed.
    }
    if (parsed) {
      lines.push(parsed);
    } else {
      lines.push({
        seq: lines.length + 1,
        ts: '',
        kind: 'unparsed',
        unparsed: { raw: trimmed },
      });
    }
  };

  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let nl = buffer.indexOf('\n');
    while (nl >= 0) {
      flushLine(buffer.slice(0, nl));
      buffer = buffer.slice(nl + 1);
      nl = buffer.indexOf('\n');
    }
  }
  buffer += decoder.decode();
  if (buffer.length > 0) {
    flushLine(buffer);
  }
  return lines;
}

/**
 * Picks the manifest line out of a parsed bundle. Returns null when
 * absent — older bundles or broken streams won't have one; the
 * transcript surface degrades to "no manifest" rather than failing.
 */
export function findManifest(lines: BundleLine[]): ManifestData | null {
  for (const line of lines) {
    if (line.kind === 'manifest' && line.data && typeof line.data === 'object') {
      return line.data as ManifestData;
    }
  }
  return null;
}

/** Picks the trailer line. Same fallthrough behaviour as findManifest. */
export function findTrailer(lines: BundleLine[]): TrailerData | null {
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = lines[i];
    if (line.kind === 'trailer' && line.data && typeof line.data === 'object') {
      return line.data as TrailerData;
    }
  }
  return null;
}

/**
 * Returns the lines that aren't manifest / trailer / blank — the
 * actual agent events the transcript renders.
 */
export function eventLines(lines: BundleLine[]): BundleLine[] {
  return lines.filter((l) => l.kind !== 'manifest' && l.kind !== 'trailer');
}
