/*
 * Claude-Code stream-json shape adapter (#218).
 *
 * Each captured event's `data` is the verbatim JSON line the agent
 * runtime emitted. For Claude Code, that's a stream-json record:
 * `{type, subtype?, message?, ...}` where `message` carries the
 * actual content blocks (text, tool_use, tool_result).
 *
 * `describeEvent` projects one of those records onto a small
 * UI-ready shape — "what kind of turn is this, and what's the
 * shape of its body" — so the renderer is a simple switch over
 * known kinds and a default branch that surfaces the rest as raw.
 *
 * Acknowledged Claude-Code coupling: when a second agent runtime
 * lands (Cursor / Aider / etc.), this file is where the
 * polymorphism goes — either dispatch on the bundle's manifest
 * `agent` field, or bake the shape into a per-agent adapter
 * package. Out of scope for v0 per the parent issue.
 */

import type { BundleLine } from './transcript-bundle';

export type EventRole = 'system' | 'user' | 'assistant' | 'result' | 'stderr' | 'other';

export type ContentBlock =
  | { kind: 'text'; text: string }
  | { kind: 'tool_use'; id: string; name: string; input: unknown }
  | {
      kind: 'tool_result';
      tool_use_id: string;
      isError: boolean;
      content: string | unknown;
    }
  | { kind: 'unknown'; raw: unknown };

export interface DescribedEvent {
  /** Stable id for deep-linking to a specific turn (`#turn-<n>`). */
  id: string;
  seq: number;
  ts: string;
  role: EventRole;
  /** Short human label (`Assistant`, `Tool: Bash`, `result · success`, …). */
  label: string;
  /** Optional structured content blocks for assistant / user turns. */
  blocks?: ContentBlock[];
  /** Optional inline detail (token usage, error reason). */
  detail?: string;
  /** Verbatim payload — fed into the "show raw" disclosure. */
  raw: unknown;
}

interface ClaudeMessageContent {
  type?: string;
  text?: string;
  id?: string;
  name?: string;
  input?: unknown;
  tool_use_id?: string;
  is_error?: boolean;
  content?: string | unknown;
}

interface ClaudeMessage {
  role?: string;
  content?: ClaudeMessageContent[] | string;
  stop_reason?: string;
}

interface ClaudeUsage {
  input_tokens?: number;
  output_tokens?: number;
}

interface ClaudeRecord {
  type?: string;
  subtype?: string;
  message?: ClaudeMessage;
  result?: string;
  is_error?: boolean;
  num_turns?: number;
  total_cost_usd?: number;
  usage?: ClaudeUsage;
  text?: string;
}

export function describeEvent(line: BundleLine): DescribedEvent {
  const id = `turn-${line.seq}`;
  const base = { id, seq: line.seq, ts: line.ts, raw: line.data ?? line.unparsed };

  if (line.kind === 'unparsed') {
    return {
      ...base,
      role: 'other',
      label: 'Unparsed line',
      detail: truncate(line.unparsed?.raw ?? '', 80),
    };
  }

  if (line.kind === 'stderr') {
    return {
      ...base,
      role: 'stderr',
      label: 'stderr',
      detail: pickStderr(line.data),
    };
  }

  if (line.kind === 'raw') {
    const text = pickRawText(line.data);
    return {
      ...base,
      role: 'other',
      label: 'raw',
      detail: text ? truncate(text, 80) : undefined,
    };
  }

  // Stream-json kinds use `type[.subtype]` — see runner's
  // claudecode adapter. The bundle line's `data` is the verbatim
  // stream-json record.
  const data = (line.data ?? {}) as ClaudeRecord;
  const major = line.kind.split('.')[0] ?? line.kind;

  switch (major) {
    case 'system':
      return {
        ...base,
        role: 'system',
        label: line.kind === 'system.init' ? 'Session started' : line.kind,
      };

    case 'user':
      return {
        ...base,
        role: 'user',
        label: 'User',
        blocks: extractBlocks(data.message),
      };

    case 'assistant': {
      const usage = data.message ? null : data.usage;
      const detail = formatUsage(data.usage ?? usage);
      return {
        ...base,
        role: 'assistant',
        label: 'Assistant',
        blocks: extractBlocks(data.message),
        detail,
      };
    }

    case 'result': {
      const ok = !data.is_error && line.kind !== 'result.error';
      const cost =
        typeof data.total_cost_usd === 'number' ? `$${data.total_cost_usd.toFixed(4)}` : undefined;
      const turns =
        typeof data.num_turns === 'number'
          ? `${data.num_turns} turn${data.num_turns === 1 ? '' : 's'}`
          : undefined;
      const parts = [ok ? 'success' : 'error', turns, cost].filter(Boolean);
      return {
        ...base,
        role: 'result',
        label: 'Result',
        detail: parts.join(' · '),
      };
    }

    default:
      return {
        ...base,
        role: 'other',
        label: line.kind,
      };
  }
}

function extractBlocks(message: ClaudeMessage | undefined): ContentBlock[] | undefined {
  if (!message) return undefined;
  const content = message.content;
  if (typeof content === 'string') {
    return [{ kind: 'text', text: content }];
  }
  if (!Array.isArray(content)) return undefined;
  return content.map(blockFromContent);
}

function blockFromContent(c: ClaudeMessageContent): ContentBlock {
  switch (c.type) {
    case 'text':
      return { kind: 'text', text: c.text ?? '' };
    case 'tool_use':
      return {
        kind: 'tool_use',
        id: c.id ?? '',
        name: c.name ?? '(unnamed)',
        input: c.input ?? {},
      };
    case 'tool_result':
      return {
        kind: 'tool_result',
        tool_use_id: c.tool_use_id ?? '',
        isError: c.is_error ?? false,
        content: c.content ?? '',
      };
    default:
      return { kind: 'unknown', raw: c };
  }
}

function pickStderr(data: unknown): string | undefined {
  if (!data || typeof data !== 'object') return undefined;
  const text = (data as { text?: string }).text;
  return typeof text === 'string' ? truncate(text, 200) : undefined;
}

function pickRawText(data: unknown): string | undefined {
  if (!data || typeof data !== 'object') return undefined;
  const text = (data as { text?: string }).text;
  return typeof text === 'string' ? text : undefined;
}

function formatUsage(usage: ClaudeUsage | undefined | null): string | undefined {
  if (!usage) return undefined;
  const totals: string[] = [];
  if (typeof usage.input_tokens === 'number') totals.push(`${usage.input_tokens} in`);
  if (typeof usage.output_tokens === 'number') totals.push(`${usage.output_tokens} out`);
  return totals.length > 0 ? totals.join(' / ') + ' tokens' : undefined;
}

function truncate(s: string, max: number): string {
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

/** Sums input + output tokens across a parsed bundle's events. */
export function totalTokens(events: DescribedEvent[]): number | null {
  let total = 0;
  let saw = false;
  for (const e of events) {
    const usage = (e.raw as ClaudeRecord | undefined)?.usage;
    if (!usage) continue;
    total += (usage.input_tokens ?? 0) + (usage.output_tokens ?? 0);
    saw = true;
  }
  return saw ? total : null;
}
