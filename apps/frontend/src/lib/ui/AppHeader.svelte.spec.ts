import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from 'vitest-browser-svelte';
import AppHeader from './AppHeader.svelte';

const { mocks } = vi.hoisted(() => ({
  mocks: {
    servers: [] as Array<{ id: string }>,
    activeServer: '',
    activeStore: undefined as undefined,
    authenticated: {} as Record<string, boolean>,
    getStore: vi.fn(),
    pushState: vi.fn(),
    goto: vi.fn(),
    toggleSidebar: vi.fn(),
    openQuickSwitcher: vi.fn(),
    routeId: '/chat/notifications',
    isMobile: false
  }
}));

vi.mock('$app/navigation', () => ({ pushState: mocks.pushState, goto: mocks.goto }));
vi.mock('$app/state', () => ({
  page: {
    get route() {
      return { id: mocks.routeId };
    }
  }
}));
vi.mock('$app/paths', () => ({
  resolve: (path: string, params?: Record<string, string>) =>
    params?.serverId ? path.replace('[serverId]', params.serverId) : path
}));
vi.mock('$app/environment', () => ({ version: '0.5.0-test' }));
vi.mock('$lib/state/activeServer.svelte', () => ({
  getActiveServer: () => mocks.activeServer
}));
vi.mock('$lib/state/server/registry.svelte', () => ({
  serverRegistry: {
    get servers() {
      return mocks.servers;
    },
    get originServer() {
      return undefined;
    },
    isAuthenticated: (id: string) => mocks.authenticated[id] === true,
    firstAuthenticatedServerId: () =>
      mocks.servers.find((server) => mocks.authenticated[server.id])?.id,
    isOriginServer: () => false,
    getServer: (id: string) =>
      mocks.servers.find((server) => server.id === id)
        ? { id, url: `https://${id}.example.com` }
        : undefined,
    getStore: mocks.getStore,
    tryGetStore: (id: string) => (id === mocks.activeServer ? mocks.activeStore : undefined)
  }
}));
vi.mock('$lib/state/server/serverConnection.svelte', () => ({
  serverConnectionManager: {
    originClient: {
      showConnectionLostIcon: false,
      showConnectionLostBanner: false
    }
  }
}));
vi.mock('$lib/state/globals.svelte', () => ({
  sidebarNav: {
    isOpen: false,
    isMobile: mocks.isMobile,
    toggle: mocks.toggleSidebar
  },
  quickSwitcher: {
    open: mocks.openQuickSwitcher
  }
}));
describe('AppHeader', () => {
  beforeEach(() => {
    mocks.servers = [];
    mocks.activeServer = '';
    mocks.activeStore = undefined;
    mocks.authenticated = {};
    mocks.getStore.mockReset();
    mocks.pushState.mockReset();
    mocks.goto.mockReset();
    mocks.routeId = '/chat/notifications';
    mocks.isMobile = false;
  });

  it('hides notifications when no servers are registered', () => {
    const { container } = render(AppHeader);

    expect(container.querySelector('a[href="/chat/notifications"]')).toBeNull();
    expect(container.querySelector('a[href="/chat/preferences"]')).not.toBeNull();
  });

  it('shows notifications when a server is registered', () => {
    mocks.servers = [{ id: 'remote' }];
    mocks.getStore.mockReturnValue({ notifications: { count: 0 } });

    const { container } = render(AppHeader);

    expect(container.querySelector('a[href="/chat/notifications"]')).not.toBeNull();
    expect(container.querySelector('a[href="/chat/preferences"]')).not.toBeNull();
  });

  it('treats a server without a store as having no unread notifications', () => {
    mocks.servers = [{ id: 'remote' }];

    const { container } = render(AppHeader);

    expect(container.querySelector('a[href="/chat/notifications"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="notifications-unread-dot"]')).toBeNull();
  });

  it('opens the canonical Settings entry point for the active authenticated server', () => {
    mocks.servers = [{ id: 'remote' }];
    mocks.activeServer = 'remote';
    mocks.authenticated = { remote: true };
    mocks.getStore.mockReturnValue({ notifications: { count: 0 } });

    const { container } = render(AppHeader);

    expect(
      container.querySelector('a[href="/chat/remote.example.com/settings"]')
    ).not.toBeNull();
    expect(container.querySelector('a[href="/chat/preferences"]')).toBeNull();
  });

  it('opens the About Chatto dialog from the frontend version', () => {
    const { container } = render(AppHeader);

    (container.querySelector('button[aria-label="About Chatto"]') as HTMLButtonElement).click();

    expect(mocks.pushState).toHaveBeenCalledWith('', { modal: { type: 'aboutChatto' } });
  });

  // 【本地改动 2026-09-01】发现背景：移动端从通知页点 hamburger 房间列表不出现。
  // 根因：hamburger 只调 sidebarNav.toggle()，但房间列表侧栏只在 [serverId] 路由
  // 下挂载；通知页不在 [serverId] 下，toggle 后无面板可滑出。修复：移动端 + 路由
  // 不含 [serverId] 时，hamburger 改为导航到默认已认证服务器的房间列表页。
  it('navigates to the room list page from a non-server route on mobile', () => {
    mocks.servers = [{ id: 'remote' }];
    mocks.activeServer = 'remote';
    mocks.authenticated = { remote: true };
    mocks.getStore.mockReturnValue({ notifications: { count: 0 } });
    mocks.isMobile = true;
    mocks.routeId = '/chat/notifications';

    const { container } = render(AppHeader);
    (container.querySelector('button[title="Toggle sidebar"]') as HTMLButtonElement).click();

    expect(mocks.toggleSidebar).not.toHaveBeenCalled();
    expect(mocks.goto).toHaveBeenCalledWith('/chat/remote.example.com/');
  });

  it('toggles the sidebar from a [serverId] route on mobile', () => {
    mocks.isMobile = true;
    mocks.routeId = '/chat/[serverId]/{roomId}';

    const { container } = render(AppHeader);
    (container.querySelector('button[title="Toggle sidebar"]') as HTMLButtonElement).click();

    expect(mocks.toggleSidebar).toHaveBeenCalledOnce();
    expect(mocks.goto).not.toHaveBeenCalled();
  });
});
