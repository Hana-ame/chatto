import { flushSync } from 'svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from 'vitest-browser-svelte';
import { q, testSnippet } from '$lib/test-utils';
import { sidebarNav } from '$lib/state/globals.svelte';
import MobileSidebarChrome from './MobileSidebarChrome.svelte';

function resetSidebar() {
  sidebarNav.setMobile(false);
  if (!sidebarNav.isOpen) sidebarNav.toggle();
  sidebarNav.setMobile(true);
}

function renderChrome() {
  return render(MobileSidebarChrome, {
    props: {
      children: testSnippet('<main data-testid="sidebar-child"></main>')
    }
  });
}

describe('MobileSidebarChrome', () => {
  // 【本地改动 2026-09-01】发现背景：上游原 MobileSidebarChrome 是左侧服务器
  // 图标列（Server Gutter）+ 移动遮罩/滑动面板容器。单服务器站点无切换需求、
  // 纯占宽度，用户曾要求去掉（试过以 CSS display:none 隐藏；发现主内容
  // 未自动对齐屏幕后回退）。现保留上游原始侧栏行为不变，spec 断言亦保持
  // 上游原版（panel/backdrop 元素在、transform/class 随 toggle 变化）。
  // 零冲突；本 spec 沿用原始断言（panel/backdrop 元素在、transform/class 随
  // toggle 变化），与"视觉上不可见"并存——这是被测试的行为。
  beforeEach(() => {
    vi.clearAllMocks();
    document.documentElement.dir = 'ltr';
    resetSidebar();
  });

  it('renders the gutter panel and children in the sidebar row', () => {
    const { container } = renderChrome();

    expect(q(container, '[data-testid="mobile-sidebar-panel"]')).not.toBeNull();
    expect(q(container, '[data-testid="sidebar-child"]')).not.toBeNull();
    expect(q(container, '[data-testid="mobile-sidebar-edge"]')).toBeNull();
  });

  it('marks mobile sidebar chrome as closed when the sidebar is closed', () => {
    const { container } = renderChrome();

    const panel = q(container, '[data-testid="mobile-sidebar-panel"]');
    const backdrop = q(
      container,
      '[data-testid="mobile-sidebar-backdrop"]'
    ) as HTMLButtonElement | null;
    expect(panel).not.toBeNull();
    expect(backdrop).not.toBeNull();
    if (!panel || !backdrop) return;

    expect(panel.classList.contains('sidebar-mobile-closed')).toBe(true);
    expect(panel.classList.contains('max-md:start-0')).toBe(true);
    expect(panel.style.transform).toBe('translateX(calc(-324px * var(--inline-direction)))');
    expect(backdrop.disabled).toBe(true);
    expect(backdrop.getAttribute('aria-hidden')).toBe('true');
    expect(backdrop.style.opacity).toBe('0');
  });

  it('opens and closes from the backdrop state without unmounting it', () => {
    const { container } = renderChrome();

    sidebarNav.toggle();
    flushSync();

    const panel = q(container, '[data-testid="mobile-sidebar-panel"]');
    const backdrop = q(
      container,
      '[data-testid="mobile-sidebar-backdrop"]'
    ) as HTMLButtonElement | null;
    expect(panel).not.toBeNull();
    expect(backdrop).not.toBeNull();
    if (!panel || !backdrop) return;

    expect(panel.classList.contains('sidebar-mobile-closed')).toBe(false);
    expect(panel.style.transform).toBe('translateX(calc(0px * var(--inline-direction)))');
    expect(backdrop.disabled).toBe(false);
    expect(backdrop.style.opacity).toBe('1');

    backdrop.click();
    flushSync();

    expect(q(container, '[data-testid="mobile-sidebar-backdrop"]')).toBe(backdrop);
    expect(panel.classList.contains('sidebar-mobile-closed')).toBe(true);
    expect(panel.style.transform).toBe('translateX(calc(-324px * var(--inline-direction)))');
    expect(backdrop.disabled).toBe(true);
    expect(backdrop.style.opacity).toBe('0');
  });
});