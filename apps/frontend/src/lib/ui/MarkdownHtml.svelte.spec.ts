import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { userEvent } from 'vitest/browser';
import { render } from 'vitest-browser-svelte';
import '../../app.css';
import { renderMarkdown } from '$lib/markdown';

const toastMocks = vi.hoisted(() => ({ success: vi.fn(), error: vi.fn() }));

vi.mock('$lib/ui/toast', () => ({ toast: toastMocks }));

import MarkdownHtml from './MarkdownHtml.svelte';

let originalClipboard: PropertyDescriptor | undefined;
const writeText = vi.fn<(_: string) => Promise<void>>();

beforeEach(() => {
  originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
  Object.defineProperty(navigator, 'clipboard', {
    configurable: true,
    value: { writeText }
  });
  writeText.mockReset().mockResolvedValue(undefined);
  toastMocks.success.mockReset();
  toastMocks.error.mockReset();
});

afterEach(() => {
  if (originalClipboard) {
    Object.defineProperty(navigator, 'clipboard', originalClipboard);
  } else {
    Reflect.deleteProperty(navigator, 'clipboard');
  }
});

describe('MarkdownHtml code copy', () => {
  it('copies the original code from each block by mouse and keyboard', async () => {
    const html = await renderMarkdown('```text\n\tfirst\n```\n\n    second\n\n`inline`');
    const { container } = render(MarkdownHtml, { html });
    const buttons = container.querySelectorAll<HTMLButtonElement>('button[data-markdown-copy]');

    expect(buttons).toHaveLength(2);
    expect(buttons[0].previousElementSibling?.textContent).toBe('text');
    expect(buttons[1].previousElementSibling).toBeNull();
    expect(buttons[0].getAttribute('aria-label')).toBe('Copy to clipboard');
    expect(container.querySelector('p code')?.textContent).toBe('inline');
    await userEvent.click(buttons[0]);
    expect(writeText).toHaveBeenCalledWith('\tfirst\n');

    buttons[1].focus();
    await userEvent.keyboard('{Enter}');
    expect(writeText).toHaveBeenCalledWith('second\n');
    expect(toastMocks.success).toHaveBeenCalledTimes(2);
  });

  it('shows a failure toast when clipboard access fails', async () => {
    writeText.mockRejectedValueOnce(new Error('Clipboard denied'));
    const html = await renderMarkdown('```\nsecret\n```');
    const { container } = render(MarkdownHtml, { html });

    await userEvent.click(container.querySelector('button[data-markdown-copy]')!);
    expect(toastMocks.error).toHaveBeenCalledOnce();
    expect(toastMocks.success).not.toHaveBeenCalled();
  });
});

describe('MarkdownHtml inline image lightbox', () => {
  it('opens the zoomable attachment modal when an inline markdown image is clicked and navigates galleries', async () => {
    // 【本地改动 2026-10-19】发现背景：验证点击 ![]() 内联图片时不再跳新窗口，而是弹出與附件
    // 一致的 AttachmentModal + ZoomableImage 大图灯箱，且多图时支持左右切换。
    const html = await renderMarkdown(
      '![first](https://images.example.com/first.png) ![second](https://images.example.com/second.jpg)'
    );
    const view = render(MarkdownHtml, { html });
    const gallery = view.container.querySelector('[data-testid="message-image-gallery"]');
    expect(gallery).not.toBeNull();

    const links = view.container.querySelectorAll<HTMLAnchorElement>('a.markdown-image-link');
    expect(links).toHaveLength(2);

    await userEvent.click(links[0]!);
    const dialog = view.getByRole('dialog', { name: 'first.png' });
    await expect.element(dialog).toBeVisible();
    await expect.element(view.getByText('1 / 2')).toBeVisible();

    const nextButton = view.getByRole('button', { name: 'Next image' });
    await userEvent.click(nextButton);
    await expect.element(view.getByRole('dialog', { name: 'second.jpg' })).toBeVisible();
    await expect.element(view.getByText('2 / 2')).toBeVisible();
  });
});
