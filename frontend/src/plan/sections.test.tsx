import { describe, expect, it } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { Section } from './sections';

describe('<Section>', () => {
  it('renders no toggle button and shows body when not collapsible', () => {
    render(
      <Section id="ticket" title="Ticket">
        <p>body content</p>
      </Section>,
    );
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { name: /ticket/i })).toBeInTheDocument();
    expect(screen.getByText('body content')).toBeInTheDocument();
  });

  it('renders a toggle button with aria-expanded=true and body visible by default when collapsible', () => {
    render(
      <Section id="prompt" title="Prompt" collapsible>
        <p>visible body</p>
      </Section>,
    );
    const button = screen.getByRole('button', { name: /prompt/i });
    expect(button).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByText('visible body')).toBeInTheDocument();
  });

  it('toggles aria-expanded to "false" and hides the body when clicked', () => {
    render(
      <Section id="prompt" title="Prompt" collapsible>
        <p>visible body</p>
      </Section>,
    );
    const button = screen.getByRole('button', { name: /prompt/i });
    fireEvent.click(button);
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText('visible body')).not.toBeInTheDocument();
  });

  it('starts collapsed when defaultOpen=false and opens on click', () => {
    render(
      <Section id="prompt" title="Prompt" collapsible defaultOpen={false}>
        <p>hidden body</p>
      </Section>,
    );
    const button = screen.getByRole('button', { name: /prompt/i });
    expect(button).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByText('hidden body')).not.toBeInTheDocument();
    fireEvent.click(button);
    expect(button).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByText('hidden body')).toBeInTheDocument();
  });

  it('points aria-controls at the body element id', () => {
    render(
      <Section id="prompt" title="Prompt" collapsible>
        <p>body content</p>
      </Section>,
    );
    const button = screen.getByRole('button', { name: /prompt/i });
    const controls = button.getAttribute('aria-controls');
    expect(controls).toBe('prompt-body');
    expect(document.getElementById(controls!)).not.toBeNull();
    expect(document.getElementById(controls!)).toHaveTextContent('body content');
  });
});
