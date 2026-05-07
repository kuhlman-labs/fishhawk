import { describe, expect, it } from 'vitest';
import { describeEvent, totalTokens } from './transcript-event';
import type { BundleLine } from './transcript-bundle';

function line(over: Partial<BundleLine> & { kind: string }): BundleLine {
  return { seq: 1, ts: '2026-05-07T12:00:00Z', ...over };
}

describe('describeEvent', () => {
  it('renders system.init with a friendly label', () => {
    const got = describeEvent(line({ kind: 'system.init', data: {} }));
    expect(got.role).toBe('system');
    expect(got.label).toBe('Session started');
  });

  it('renders an assistant turn with text + tool_use blocks', () => {
    const got = describeEvent(
      line({
        kind: 'assistant',
        data: {
          type: 'assistant',
          message: {
            role: 'assistant',
            content: [
              { type: 'text', text: 'Reading the file' },
              {
                type: 'tool_use',
                id: 'tu_1',
                name: 'Read',
                input: { file_path: 'backend/main.go' },
              },
            ],
          },
        },
      }),
    );
    expect(got.role).toBe('assistant');
    expect(got.label).toBe('Assistant');
    expect(got.blocks).toHaveLength(2);
    expect(got.blocks?.[0]).toMatchObject({ kind: 'text', text: 'Reading the file' });
    expect(got.blocks?.[1]).toMatchObject({ kind: 'tool_use', name: 'Read' });
  });

  it('flattens a string-shaped message content into a single text block', () => {
    const got = describeEvent(
      line({
        kind: 'assistant',
        data: {
          type: 'assistant',
          message: { role: 'assistant', content: 'plain answer' },
        },
      }),
    );
    expect(got.blocks).toEqual([{ kind: 'text', text: 'plain answer' }]);
  });

  it('renders a user tool_result block', () => {
    const got = describeEvent(
      line({
        kind: 'user',
        data: {
          type: 'user',
          message: {
            role: 'user',
            content: [
              {
                type: 'tool_result',
                tool_use_id: 'tu_1',
                is_error: false,
                content: 'package main\n',
              },
            ],
          },
        },
      }),
    );
    expect(got.role).toBe('user');
    expect(got.blocks?.[0]).toMatchObject({
      kind: 'tool_result',
      tool_use_id: 'tu_1',
      isError: false,
      content: 'package main\n',
    });
  });

  it('renders result.success with cost and turns when provided', () => {
    const got = describeEvent(
      line({
        kind: 'result.success',
        data: { type: 'result', subtype: 'success', num_turns: 4, total_cost_usd: 0.0234 },
      }),
    );
    expect(got.role).toBe('result');
    expect(got.detail).toContain('success');
    expect(got.detail).toContain('4 turns');
    expect(got.detail).toContain('$0.0234');
  });

  it('renders result.error', () => {
    const got = describeEvent(
      line({ kind: 'result.error', data: { type: 'result', subtype: 'error', is_error: true } }),
    );
    expect(got.detail).toContain('error');
  });

  it('renders stderr with a truncated detail', () => {
    const got = describeEvent(line({ kind: 'stderr', data: { text: 'oops' } }));
    expect(got.role).toBe('stderr');
    expect(got.detail).toBe('oops');
  });

  it('renders raw with the verbatim text as detail', () => {
    const got = describeEvent(line({ kind: 'raw', data: { text: 'hello world' } }));
    expect(got.role).toBe('other');
    expect(got.detail).toBe('hello world');
  });

  it('renders unparsed lines defensively rather than crashing', () => {
    const got = describeEvent({
      seq: 9,
      ts: '',
      kind: 'unparsed',
      unparsed: { raw: 'garbage bytes' },
    });
    expect(got.label).toBe('Unparsed line');
    expect(got.detail).toBe('garbage bytes');
  });

  it('falls through to the raw kind for unknown stream-json types', () => {
    const got = describeEvent(line({ kind: 'something_new', data: {} }));
    expect(got.label).toBe('something_new');
    expect(got.role).toBe('other');
  });

  it('formats token usage on assistant turns', () => {
    const got = describeEvent(
      line({
        kind: 'assistant',
        data: { type: 'assistant', usage: { input_tokens: 1500, output_tokens: 200 } },
      }),
    );
    expect(got.detail).toBe('1500 in / 200 out tokens');
  });

  it('emits stable turn-<n> ids for deep-linking', () => {
    expect(describeEvent(line({ seq: 42, kind: 'assistant' })).id).toBe('turn-42');
  });
});

describe('totalTokens', () => {
  it('sums input + output tokens across described events that carry usage', () => {
    const events = [
      describeEvent(
        line({
          seq: 1,
          kind: 'assistant',
          data: { type: 'assistant', usage: { input_tokens: 100, output_tokens: 20 } },
        }),
      ),
      describeEvent(
        line({
          seq: 2,
          kind: 'assistant',
          data: { type: 'assistant', usage: { input_tokens: 50, output_tokens: 5 } },
        }),
      ),
    ];
    expect(totalTokens(events)).toBe(175);
  });

  it('returns null when no event carries a usage block', () => {
    const events = [describeEvent(line({ seq: 1, kind: 'system.init', data: {} }))];
    expect(totalTokens(events)).toBeNull();
  });
});
