import { render } from 'vitest-browser-svelte';
import { q, testSnippet } from '$lib/test-utils';
import MobileSidebarChrome from './MobileSidebarChrome.svelte';

function renderChrome() {
  return render(MobileSidebarChrome, {
    props: {
      children: testSnippet('<main data-testid="sidebar-child">child</main>')
    }
  });
}

describe('MobileSidebarChrome', () => {
  // 【本地改动 2026-09-01】发现背景：上游原 MobileSidebarChrome 是左侧服务器
  // 图标列（Server Gutter）+ 移动遮罩/滑动面板的容器。单服务器站点无切换
  // 需求、纯占宽度，用户明确要求去掉整条侧栏（方案 A），遂移除 ServerGutter
  // 渲染及 backdrop/panel/动画骨架，组件退化为对 children 的透传 wrapper。
  // 本 spec 保护这个"侧栏不再渲染、children 透传"的契约，防止上游合并时
  // 把 ServerGutter 面板无声带回来。
  beforeEach(() => {
    document.documentElement.dir = 'ltr';
  });

  it('transports children without rendering the server-gutter panel', () => {
    const { container } = renderChrome();

    // 侧栏面板 / 遮罩已移除——这是修复的核心断言
    expect(q(container, '[data-testid="mobile-sidebar-panel"]')).toBeNull();
    expect(q(container, '[data-testid="mobile-sidebar-backdrop"]')).toBeNull();
    expect(container.querySelector('[data-app-sidebar="true"]')).toBeNull();

    // 且 children 仍正常透传
    expect(q(container, '[data-testid="sidebar-child"]')?.textContent).toBe('child');
  });
});
