import { describe, expect, it } from 'vitest';
import {
  eventLines,
  findManifest,
  findTrailer,
  parseBundle,
  type BundleLine,
} from './transcript-bundle';

function streamOf(text: string): ReadableStream<Uint8Array> {
  const bytes = new TextEncoder().encode(text);
  return new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(bytes);
      controller.close();
    },
  });
}

function streamOfChunks(chunks: string[]): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const c of chunks) controller.enqueue(enc.encode(c));
      controller.close();
    },
  });
}

describe('parseBundle', () => {
  it('parses a 3-line bundle (manifest + event + trailer)', async () => {
    const body = streamOf(
      [
        '{"seq":1,"ts":"2026-05-07T12:00:00Z","kind":"manifest","data":{"agent":"claude-code"}}',
        '{"seq":2,"ts":"2026-05-07T12:01:00Z","kind":"assistant","data":{"type":"assistant"}}',
        '{"seq":3,"ts":"2026-05-07T12:02:00Z","kind":"trailer","data":{"event_count":1,"content_hash":"abc"}}',
        '',
      ].join('\n'),
    );
    const lines = await parseBundle(body);
    expect(lines).toHaveLength(3);
    expect(lines[0].kind).toBe('manifest');
    expect(lines[2].kind).toBe('trailer');
  });

  it('tolerates CRLF line endings', async () => {
    const body = streamOf(
      [
        '{"seq":1,"ts":"t","kind":"manifest","data":{}}',
        '{"seq":2,"ts":"t","kind":"raw","data":{}}',
      ].join('\r\n') + '\r\n',
    );
    const lines = await parseBundle(body);
    expect(lines).toHaveLength(2);
    expect(lines[1].kind).toBe('raw');
  });

  it('reassembles JSON across multiple chunk boundaries', async () => {
    const body = streamOfChunks(['{"seq":1,"ts":"t",', '"kind":"assistant",', '"data":{"x":1}}\n']);
    const lines = await parseBundle(body);
    expect(lines).toHaveLength(1);
    expect(lines[0].kind).toBe('assistant');
  });

  it('emits an unparsed line for malformed JSON rather than dropping silently', async () => {
    const body = streamOf(
      ['{"seq":1,"ts":"t","kind":"manifest","data":{}}', 'not json'].join('\n'),
    );
    const lines = await parseBundle(body);
    expect(lines).toHaveLength(2);
    expect(lines[1].kind).toBe('unparsed');
    expect(lines[1].unparsed?.raw).toBe('not json');
  });

  it('treats records missing seq/kind as unparsed', async () => {
    const body = streamOf('{"hello":"world"}\n');
    const lines = await parseBundle(body);
    expect(lines[0].kind).toBe('unparsed');
  });

  it('drops blank lines (gzip can leave trailing empties)', async () => {
    const body = streamOf('{"seq":1,"ts":"t","kind":"manifest","data":{}}\n\n\n');
    const lines = await parseBundle(body);
    expect(lines).toHaveLength(1);
  });
});

describe('findManifest / findTrailer / eventLines', () => {
  const lines: BundleLine[] = [
    { seq: 1, ts: 't', kind: 'manifest', data: { agent: 'claude-code' } },
    { seq: 2, ts: 't', kind: 'assistant', data: {} },
    { seq: 3, ts: 't', kind: 'user', data: {} },
    { seq: 4, ts: 't', kind: 'trailer', data: { event_count: 2, content_hash: 'abc' } },
  ];

  it('finds the manifest', () => {
    expect(findManifest(lines)?.agent).toBe('claude-code');
  });

  it('finds the trailer (walking from the end)', () => {
    expect(findTrailer(lines)?.event_count).toBe(2);
  });

  it('eventLines excludes manifest and trailer', () => {
    const evts = eventLines(lines);
    expect(evts.map((e) => e.kind)).toEqual(['assistant', 'user']);
  });

  it('returns null when manifest absent', () => {
    expect(findManifest([{ seq: 1, ts: 't', kind: 'raw' }])).toBeNull();
  });
});
