import { describe, expect, it } from 'vitest';
import { page } from 'vitest/browser';
import { render } from 'vitest-browser-svelte';
import { testSnippet } from '$lib/test-utils';
import ServerHydrationGate from './ServerHydrationGate.svelte';

describe('ServerHydrationGate', () => {
  it('keeps server UI mounted and inert until it can reveal the complete composition', async () => {
    const rendered = render(ServerHydrationGate, {
      props: {
        ready: false,
        serverName: 'Starlight Commons',
        children: testSnippet('<div data-testid="hydrated-server-content">Server UI</div>')
      }
    });

    await expect.element(page.getByTestId('server-loading-screen')).toBeVisible();
    await expect.element(page.getByTestId('hydrated-server-content')).toBeInTheDocument();
    await expect.element(page.getByTestId('server-ui')).toHaveAttribute('aria-hidden', 'true');
    await expect.element(page.getByTestId('server-ui')).toHaveAttribute('inert');

    await rendered.rerender({
      ready: true,
      serverName: 'Starlight Commons',
      children: testSnippet('<div data-testid="hydrated-server-content">Server UI</div>')
    });

    await expect.element(page.getByTestId('server-loading-screen')).not.toBeInTheDocument();
    await expect.element(page.getByTestId('server-ui')).toHaveAttribute('aria-hidden', 'false');
    await expect.element(page.getByTestId('server-ui')).not.toHaveAttribute('inert');
    await expect.element(page.getByTestId('hydrated-server-content')).toBeVisible();

    await rendered.rerender({
      ready: false,
      serverName: 'Starlight Commons',
      children: testSnippet('<div data-testid="hydrated-server-content">Next room</div>')
    });

    await expect.element(page.getByTestId('server-loading-screen')).not.toBeInTheDocument();
    await expect.element(page.getByText('Next room', { exact: true })).toBeVisible();
  });
});
