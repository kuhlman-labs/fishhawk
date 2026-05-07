import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ApiClientError, api } from '@/api/client';
import { TranscriptSection } from './transcript-section';

function bundleResponse(lines: string[]): Response {
  const body = lines.join('\n') + '\n';
  return new Response(body, {
    status: 200,
    headers: { 'Content-Type': 'application/x-ndjson' },
  });
}

describe('<TranscriptSection>', () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.restoreAllMocks());

  it('shows a loading state, then a manifest header + each event', async () => {
    vi.spyOn(api, 'getStageTraceStream').mockResolvedValue(
      bundleResponse([
        '{"seq":1,"ts":"2026-05-07T12:00:00Z","kind":"manifest","data":{"agent":"claude-code","model":"claude-opus-4-7"}}',
        '{"seq":2,"ts":"2026-05-07T12:01:00Z","kind":"system.init","data":{}}',
        '{"seq":3,"ts":"2026-05-07T12:02:00Z","kind":"assistant","data":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}]}}}',
        '{"seq":4,"ts":"2026-05-07T12:03:00Z","kind":"trailer","data":{"event_count":2,"content_hash":"abc123"}}',
      ]),
    );

    render(<TranscriptSection stageId="aaaaaaaa-1111-1111-1111-111111111111" />);
    expect(screen.getByText(/loading transcript/i)).toBeInTheDocument();

    await waitFor(() => {
      // Manifest summary shows agent + model.
      expect(screen.getByText(/claude-code/)).toBeInTheDocument();
      expect(screen.getByText(/claude-opus-4-7/)).toBeInTheDocument();
    });
    // Event count excludes manifest + trailer.
    expect(screen.getByText(/2 events/)).toBeInTheDocument();
    expect(screen.getByText('Session started')).toBeInTheDocument();
    expect(screen.getByText('Hello')).toBeInTheDocument();
  });

  it('shows the empty-state message on a 404 from the backend', async () => {
    vi.spyOn(api, 'getStageTraceStream').mockRejectedValue(
      new ApiClientError(404, { error: 'trace_not_found' }, 'trace_not_found'),
    );
    render(<TranscriptSection stageId="aaaaaaaa-1111-1111-1111-111111111111" />);
    await waitFor(() => {
      expect(screen.getByText(/trace bundle not yet uploaded/i)).toBeInTheDocument();
    });
  });

  it('shows the error block when the bundle stream errors', async () => {
    vi.spyOn(api, 'getStageTraceStream').mockRejectedValue(new Error('storage offline'));
    render(<TranscriptSection stageId="aaaaaaaa-1111-1111-1111-111111111111" />);
    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/storage offline/i);
    });
  });

  it('surfaces the agent_failed flag from the manifest', async () => {
    vi.spyOn(api, 'getStageTraceStream').mockResolvedValue(
      bundleResponse([
        '{"seq":1,"ts":"t","kind":"manifest","data":{"agent":"claude-code","agent_failed":true,"agent_failure_reason":"oom"}}',
        '{"seq":2,"ts":"t","kind":"trailer","data":{"event_count":0,"content_hash":"abc"}}',
      ]),
    );
    render(<TranscriptSection stageId="aaaaaaaa-1111-1111-1111-111111111111" />);
    await waitFor(() => {
      expect(screen.getByText(/Agent failed: oom/i)).toBeInTheDocument();
    });
  });

  it('renders tool_use blocks with the input collapsed by default; click to expand', async () => {
    vi.spyOn(api, 'getStageTraceStream').mockResolvedValue(
      bundleResponse([
        '{"seq":1,"ts":"t","kind":"manifest","data":{"agent":"claude-code"}}',
        '{"seq":2,"ts":"t","kind":"assistant","data":{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"git status"}}]}}}',
      ]),
    );
    render(<TranscriptSection stageId="aaaaaaaa-1111-1111-1111-111111111111" />);

    await waitFor(() => {
      expect(screen.getByText(/Tool · Bash/)).toBeInTheDocument();
    });
    // The summary shows the command preview.
    expect(screen.getByText(/git status/)).toBeInTheDocument();
    // Input JSON is collapsed (the raw `{"command":"git status"}` is not in the DOM).
    expect(screen.queryByText('"command": "git status"')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /Tool · Bash/ }));
    expect(screen.getByText(/"command": "git status"/)).toBeInTheDocument();
  });

  it('emits stable turn-<n> anchor ids so reviewers can deep-link to a specific turn', async () => {
    vi.spyOn(api, 'getStageTraceStream').mockResolvedValue(
      bundleResponse([
        '{"seq":1,"ts":"t","kind":"manifest","data":{"agent":"claude-code"}}',
        '{"seq":2,"ts":"t","kind":"assistant","data":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first"}]}}}',
        '{"seq":3,"ts":"t","kind":"assistant","data":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second"}]}}}',
      ]),
    );
    const { container } = render(
      <TranscriptSection stageId="aaaaaaaa-1111-1111-1111-111111111111" />,
    );
    await waitFor(() => {
      expect(screen.getByText('first')).toBeInTheDocument();
    });
    expect(container.querySelector('#turn-2')).not.toBeNull();
    expect(container.querySelector('#turn-3')).not.toBeNull();
  });

  it('shows the bundle sha256 (truncated) at the bottom for cross-reference with the audit log', async () => {
    vi.spyOn(api, 'getStageTraceStream').mockResolvedValue(
      bundleResponse([
        '{"seq":1,"ts":"t","kind":"manifest","data":{"agent":"claude-code"}}',
        '{"seq":2,"ts":"t","kind":"trailer","data":{"event_count":0,"content_hash":"abcdef0123456789ffffffffffffffffffffffffffffffffffffffffffffffff"}}',
      ]),
    );
    render(<TranscriptSection stageId="aaaaaaaa-1111-1111-1111-111111111111" />);
    await waitFor(() => {
      expect(screen.getByText(/bundle sha256: abcdef012345/i)).toBeInTheDocument();
    });
  });
});
