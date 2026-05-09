import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { AuditEntryRow } from './audit-entry-row';
import type { AuditEntry } from '@/api/types';

/*
 * Regression guard for #242: actor was getting a fixed 8rem column
 * while the locale-formatted timestamp got `1fr`, so v0 actor strings
 * (`user · github:<login>`) clipped at ~13 chars on wide viewports.
 * The fix swaps the two — actor flexes with a 12rem minimum, ts is
 * fixed at 12rem. Both the per-run and global (showRun) variants
 * share the shape, so we assert against both.
 */

function makeEntry(over: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: 'aaaaaaaa-1111-1111-1111-111111111111',
    sequence: 1,
    run_id: '11111111-2222-3333-4444-555555555555',
    stage_id: null,
    ts: '2026-05-04T20:00:00Z',
    category: 'trace_uploaded',
    actor_kind: 'user',
    actor_subject: 'github:kuhlman-labs-very-long-30char-handle',
    payload: {},
    prev_hash: null,
    entry_hash: 'abcdef1234567890abcdef1234567890',
    ...over,
  };
}

const LONG_ACTOR_SUBJECT = 'github:kuhlman-labs-very-long-30char-handle';
const LONG_ACTOR_TEXT = `user · ${LONG_ACTOR_SUBJECT}`;

describe('<AuditEntryRow>', () => {
  it('uses the per-run grid template when showRun is false', () => {
    render(
      <MemoryRouter>
        <ol>
          <AuditEntryRow entry={makeEntry()} />
        </ol>
      </MemoryRouter>,
    );
    const row = screen.getByRole('listitem');
    expect(row.className).toContain('grid-cols-[3rem_13rem_minmax(12rem,1fr)_12rem_8rem]');
  });

  it('uses the global grid template (with run column) when showRun is true', () => {
    render(
      <MemoryRouter>
        <ol>
          <AuditEntryRow entry={makeEntry()} showRun />
        </ol>
      </MemoryRouter>,
    );
    const row = screen.getByRole('listitem');
    expect(row.className).toContain('grid-cols-[3rem_13rem_minmax(12rem,1fr)_12rem_8rem_6rem]');
  });

  it('renders an actor string longer than 30 characters intact (no slicing)', () => {
    expect(LONG_ACTOR_SUBJECT.length).toBeGreaterThan(30);
    render(
      <MemoryRouter>
        <ol>
          <AuditEntryRow entry={makeEntry()} />
        </ol>
      </MemoryRouter>,
    );
    // The component splits kind / ' · subject' across two adjacent
    // text nodes, so match by accumulated textContent rather than a
    // single-node string lookup.
    const actor = screen
      .getAllByText((_, el) => el?.textContent === LONG_ACTOR_TEXT)
      .find((el) => el.tagName === 'SPAN');
    expect(actor).toBeDefined();
    expect(actor?.textContent).toBe(LONG_ACTOR_TEXT);
  });

  it('renders the same long actor intact in the showRun variant', () => {
    render(
      <MemoryRouter>
        <ol>
          <AuditEntryRow entry={makeEntry()} showRun />
        </ol>
      </MemoryRouter>,
    );
    const actor = screen
      .getAllByText((_, el) => el?.textContent === LONG_ACTOR_TEXT)
      .find((el) => el.tagName === 'SPAN');
    expect(actor?.textContent).toBe(LONG_ACTOR_TEXT);
  });
});
