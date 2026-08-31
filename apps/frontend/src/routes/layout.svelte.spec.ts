import { tick } from 'svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from 'vitest-browser-svelte';
import { q, testSnippet } from '$lib/test-utils';
import type { PublicServerInfo } from '$lib/api-client/server';
import { sidebarNav } from '$lib/state/globals.svelte';

const { mocks } = vi.hoisted(() => ({
  mocks: {
    goto: vi.fn(),
    afterNavigate: vi.fn(),
    onNavigate: vi.fn(),
    appUi: {
      setActiveRoomScope: vi.fn(),
      setActiveServer: vi.fn()
    },
    originClient: {
      showConnectionLostIcon: false,
      showConnectionLostBanner: false,
      forceReconnect: vi.fn()
    },
    updateAppBadge: vi.fn(async () => {})
  }
}));

vi.mock('$app/navigation', () => ({
  afterNavigate: mocks.afterNavigate,
  goto: mocks.goto,
  onNavigate: mocks.onNavigate,
  pushState: vi.fn()
}));

vi.mock('$app/paths', () => ({
  resolve: (path: string) => path
}));

vi.mock('$app/state', () => ({
  page: {
    params: {},
    route: { id: '/' },
    state: {},
    url: new URL('https://chat.example.test/')
  },
  updated: {
    current: false
  }
}));

vi.mock('$lib/hooks/usePageTitle.svelte', () => ({
  usePageTitle: () => () => 'Chatto'
}));

vi.mock('$lib/hooks/usePinchZoomPrevention.svelte', () => ({
  usePinchZoomPrevention: vi.fn()
}));

vi.mock('$lib/hooks/useVisualViewport.svelte', () => ({
  useVisualViewport: vi.fn()
}));

vi.mock('$lib/notifications/pushNotifications', () => ({
  enablePushOnAllServers: vi.fn().mockResolvedValue({ permission: null, registrations: [] }),
  getPermission: vi.fn(() => null),
  getPushCapability: vi.fn(() => 'unsupported'),
  getPushRegistrationTargets: vi.fn(() => []),
  onNotificationClick: vi.fn(() => vi.fn()),
  refreshPushSubscriptions: vi.fn()
}));

vi.mock('$lib/notifications/notificationNavigationUi', () => ({
  prepareUiForNotificationPath: vi.fn(),
  prepareUiForNotificationTarget: vi.fn()
}));

vi.mock('$lib/notifications/appBadge', () => ({
  listenForAppBadgeRefresh: vi.fn(() => vi.fn()),
  updateAppBadge: mocks.updateAppBadge
}));

vi.mock('$lib/state/activeServer.svelte', () => ({
  getActiveServer: () => 'origin'
}));

vi.mock('$lib/state/appUi.svelte', () => ({
  getAppUiState: () => mocks.appUi,
  provideAppUiState: () => mocks.appUi
}));

vi.mock('$lib/state/server/useServerRegistry.svelte', () => ({
  useServerRegistry: vi.fn()
}));

vi.mock('$lib/state/server/ServerRuntimeCoordinator.svelte', async () => ({
  default: (await import('./chat/ChatRootTestStub.svelte')).default
}));

vi.mock('$lib/state/server/registry.svelte', () => ({
  generateServerId: vi.fn(() => 'server-id'),
  serverRegistry: {
    servers: [],
    originServer: { id: 'origin' },
    getStore: vi.fn(),
    tryGetStore: vi.fn(() => null),
    isAuthenticated: vi.fn(() => false),
    firstAuthenticatedServerId: vi.fn(() => undefined)
  }
}));

vi.mock('$lib/state/server/serverConnection.svelte', () => ({
  serverConnectionManager: {
    originClient: mocks.originClient,
    getClient: vi.fn(() => mocks.originClient)
  }
}));

import Layout from './+layout.svelte';

function installMobileMatchMedia() {
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    value: vi.fn(() => ({
      matches: true,
      media: '(max-width: 767px)',
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn()
    }))
  });
}

function resetSidebar() {
  sidebarNav.setMobile(false);
  if (!sidebarNav.isOpen) sidebarNav.toggle();
  sidebarNav.setMobile(true);
}

function renderLayout() {
  const serverInfo: PublicServerInfo = {
    name: 'Test Server',
    version: 'test',
    authorizeUrl: '/oauth/authorize',
    directRegistrationEnabled: true,
    directLoginEnabled: true,
    accountCreationPolicy: 'open',
    welcomeMessage: null,
    description: null,
    iconUrl: null,
    bannerUrl: null,
    authProviders: []
  };

  return render(Layout, {
    props: {
      data: {
        serverInfo,
        serverInfoLoaded: true,
        user: null
      },
      children: testSnippet('<main data-testid="layout-child"></main>')
    }
  });
}

function pointer(type: string, x: number, y = 120) {
  return new PointerEvent(type, {
    bubbles: true,
    cancelable: true,
    pointerId: 1,
    clientX: x,
    clientY: y
  });
}

describe('root layout sidebar gutter removed', () => {
  // 【本地改动 2026-09-01】发现背景：上游原 layout 在移动端渲染左侧服务器
  // 图标列（Server Gutter）面板 + 遮罩 + 左右滑动开关。单服务器站点无切换
  // 需求、纯占宽度，用户要求去掉整条侧栏（方案 A），遂删除 ServerGutter
  // 渲染与面板骨架（见 MobileSidebarChrome.svelte）。本 spec 保护这个"侧栏
  // 面板不再渲染"的契约，防止上游合并把 ServerGutter 面板无声带回来；同时
  // 验证 children 仍透传、左边缘交互不被（已不存在的）侧栏拦截。
  beforeEach(() => {
    vi.clearAllMocks();
    document.documentElement.dir = 'ltr';
    installMobileMatchMedia();
    resetSidebar();
  });

  it('renders children without the server-gutter panel', async () => {
    const { container } = renderLayout();
    await tick();

    expect(q(container, '[data-testid="layout-child"]')).not.toBeNull();
    // 侧栏面板 / 遮罩已移除——核心断言
    expect(q(container, '[data-testid="mobile-sidebar-panel"]')).toBeNull();
    expect(q(container, '[data-testid="mobile-sidebar-backdrop"]')).toBeNull();
    expect(container.querySelector('[data-app-sidebar="true"]')).toBeNull();
  });

  it('does not re-open the gutter panel when sidebarNav toggles', async () => {
    const { container } = renderLayout();
    await tick();

    sidebarNav.toggle();
    await tick();

    expect(q(container, '[data-testid="mobile-sidebar-panel"]')).toBeNull();
    expect(q(container, '[data-testid="mobile-sidebar-backdrop"]')).toBeNull();
    // children 不受 sidebarNav 切换影响
    expect(q(container, '[data-testid="layout-child"]')).not.toBeNull();
  });
});

describe('root layout notification synchronization', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    installMobileMatchMedia();
    resetSidebar();
  });

  it('mounts badge synchronization for a signed-out page', async () => {
    const { container } = renderLayout();

    await vi.waitFor(() => expect(mocks.updateAppBadge).toHaveBeenCalledWith({ kind: 'clear' }));
    expect(container.querySelector('[data-testid="chat-root-component-stub"]')).not.toBeNull();
  });
});
