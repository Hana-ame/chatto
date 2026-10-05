<script lang="ts">
  import { pushState } from '$app/navigation';
  import { serverUi } from '$lib/state/server/serverUi';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { serverRegistry, serverConnectionManager } from '$lib/client';
  import { firstAuthenticatedServerId } from '$lib/serverCatalogue';
  import { getActiveServer } from '$lib/state/activeServer.svelte';
  import { serverIdToSegment } from '$lib/navigation';
  import { version } from '$app/environment';
  import { sidebarNav, quickSwitcher } from '$lib/state/globals.svelte';
  import { m } from '$lib/i18n/messages';
  import UnreadDot from '$lib/ui/UnreadDot.svelte';
  import MotdContent from '$lib/ui/MotdContent.svelte';
  import { SERVER_SETTINGS_ROOT_ROUTE } from '$lib/navigation/settingsRoutes';

  // MOTD follows the active server; the connection-lost icon below stays
  // bound to the origin store since it reflects the SPA host's own connection.
  const motd = $derived(serverRegistry.tryGetStore(getActiveServer())?.serverInfo.motd);
  const originStore = $derived(serverRegistry.tryGetStore(serverRegistry.originServer?.id ?? ''));

  /** A server's notification counts without viewed ones; zero without a store. */
  function attentionCounts(serverId: string) {
    const store = serverRegistry.tryGetStore(serverId);
    return store
      ? serverUi(store).attention.counts
      : { unreadNotificationCount: 0, importantUnreadNotificationCount: 0 };
  }

  // Aggregate exact notification counts across all servers.
  const totalNotificationCount = $derived(
    serverRegistry.servers.reduce(
      (sum, instance) => sum + attentionCounts(instance.id).unreadNotificationCount,
      0
    )
  );
  const totalImportantNotificationCount = $derived(
    serverRegistry.servers.reduce(
      (sum, instance) => sum + attentionCounts(instance.id).importantUnreadNotificationCount,
      0
    )
  );

  // Show sign-out button when any server is registered
  const hasInstances = $derived(serverRegistry.servers.length > 0);
  const preferencesServerId = $derived.by(() => {
    const activeServerId = getActiveServer();
    if (activeServerId && serverRegistry.isAuthenticated(activeServerId)) return activeServerId;
    return firstAuthenticatedServerId();
  });
  function handleSignOut() {
    pushState('', { modal: { type: 'logout' } });
  }

  function showAboutChatto() {
    pushState('', { modal: { type: 'aboutChatto' } });
  }

  // 【本地改动 2026-09-01，2026-10-03 恢复】修复移动端「通知页/无服务器页点
  // hamburger 房间列表不出现」：hamburger 调 sidebarNav.toggle()，但房间列表侧栏
  // （ServerSidebar + RoomList）只由 Chrome 在 [serverId] 路由下挂载；通知页
  // /chat/notifications 不在 [serverId] 下，toggle 后 DOM 里根本没有房间列表面板
  // 可滑出，用户只见服务器图标列，误以为坏了。
  // 思路：移动端 + 当前路由不含 [serverId]（即无可 toggle 的房间列表侧栏）时，
  // hamburger 先打开侧栏（sidebarNav.isOpen=true），再导航到默认已认证服务器的
  // 房间列表页；进入 [serverId] 页后 ServerSidebar 挂载且 isOpen 为真，房间列表
  // 直接滑出可见。有 [serverId] 的页面（房间、admin、设置）保持原 toggle 行为。
  // 边界：仅影响移动端（sidebarNav.isMobile）；桌面端 hamburger 行为不变；
  // 目标服务器复用 preferencesServerId（active 或 firstAuthenticated），与
  // 设置页入口一致。踩坑：仅 goto 不打开侧栏的话，[serverId] 页移动端默认
  // isOpen=false，导航后房间列表仍不可见，等于没修（2026-09-01 自查发现）。
  function handleHamburger() {
    if (sidebarNav.isMobile && !page.route.id?.includes('[serverId]')) {
      const serverId = preferencesServerId;
      if (serverId) {
        if (!sidebarNav.isOpen) sidebarNav.toggle();
        // 【本地改动 2026-10-05】改为动态 import $app/navigation 的 goto。
        //
        // 踩坑：模块顶层 `import { goto } from '$app/navigation'` 会让
        // AppHeader.svelte.spec.ts 在 vitest 浏览器模式整文件加载失败——
        //   SyntaxError: The requested module '.../@sveltejs/kit/src/runtime/app/
        //   navigation.js' does not provide an export named 'goto'
        // 该失败发生在模块解析期，会**连坐**同一批次里另外 6 个 spec
        // （ServerGutter / LinkPreviewCard / ServerSignedOut / ServerUnavailable 等
        // 本身并不 import goto），test-workspace 因此整 job 红。
        //
        // 证据：上游同一份 AppHeader.svelte.svelte.spec.ts 绿（11 tests），
        // 上游 AppHeader.svelte 只 import { pushState }。本 fork 独有的 goto 导入
        // 就是唯一差异。该失败在我 push 之前（基线 8bbb2491a / 42064270a）就存在，
        // 与第 2 组改动无关，是 2026-09-01 移动端 hamburger 改动带进来的。
        //
        // goto 只在用户点击 hamburger 时才需要，动态 import 不损失任何东西，
        // 且把依赖推迟到真正需要它的时候。
        void import('$app/navigation').then(({ goto }) =>
          goto(resolve('/chat/[serverId]', { serverId: serverIdToSegment(serverId) }))
        );
      }
      return;
    }
    sidebarNav.toggle();
  }
</script>

<!-- WebKit extends the solid background of a sticky header into its top system bar. -->
<header
  class="app-header sticky top-0 flex keyboard-hide-mobile h-[var(--app-header-height)] shrink-0 items-center justify-between gap-2 bg-surface p-2 text-muted desktop-presentation:text-sm"
>
  <!-- Leading: global navigation, notifications, and client-wide actions -->
  <div class="flex items-center gap-3">
    <!-- Sidebar toggle - 44px tap target for mobile accessibility -->
    <button
      type="button"
      class="app-header-icon"
      onclick={handleHamburger}
      aria-label={m('ui.toggle_sidebar')}
      aria-expanded={sidebarNav.isOpen}
      title={m('ui.toggle_sidebar')}
    >
      <span
        aria-hidden="true"
        class={[
          'iconify text-xl rtl:-scale-x-100',
          sidebarNav.isOpen ? 'icon-[lucide--panel-left-close]' : 'icon-[lucide--panel-left-open]'
        ]}
      ></span>
    </button>

    {#if hasInstances}
      <!-- Notification bell - 44px tap target for mobile accessibility -->
      <a
        href={resolve('/chat/notifications')}
        aria-label={m('ui.notifications')}
        title={m('ui.notifications')}
        class="app-header-icon relative"
      >
        <span aria-hidden="true" class="iconify icon-[uil--bell] text-lg"></span>
        {#if totalNotificationCount > 0}
          <UnreadDot
            color={totalImportantNotificationCount > 0 ? 'warning' : 'ambient'}
            class="absolute end-2 top-2"
            testid="notifications-unread-dot"
          />
        {/if}
      </a>
    {/if}

    <!-- Quick switcher trigger -->
    {#if hasInstances}
      <button
        type="button"
        class="app-header-icon"
        onclick={() => quickSwitcher.open()}
        aria-label={m('ui.open_quick_switcher')}
        title={m('ui.quick_switcher_shortcut')}
      >
        <span aria-hidden="true" class="iconify icon-[uil--apps] text-lg"></span>
      </button>
    {/if}

    <a
      href={preferencesServerId
        ? resolve(SERVER_SETTINGS_ROOT_ROUTE, {
            serverId: serverIdToSegment(preferencesServerId)
          })
        : resolve('/chat/preferences')}
      class="app-header-icon"
      aria-label={m('settings.app_preferences.title')}
      title={m('settings.app_preferences.title')}
    >
      <span class="iconify icon-[uil--setting] text-lg" aria-hidden="true"></span>
    </a>

    <!-- Connection lost indicator: only show when an authenticated server has lost connection.
         Skip the origin server if the user isn't authenticated (no WebSocket expected). -->
    {#if originStore?.currentUser.user && serverConnectionManager.originClient.showConnectionLostIcon}
      <span
        class={[
          'iconify icon-[uil--wifi-slash] text-lg',
          serverConnectionManager.originClient.showConnectionLostBanner
            ? 'text-warning'
            : 'animate-pulse'
        ]}
        role="img"
        aria-label={m('ui.realtime_paused')}
        title={m('ui.realtime_paused')}
      ></span>
    {/if}
  </div>

  <!-- MOTD -->
  {#if motd}
    <MotdContent {motd} onclick={() => pushState('', { modal: { type: 'motd', motd } })} />
  {:else}
    <span class="flex-1"></span>
  {/if}

  <!-- Actions: About + Logout -->
  <div class="flex shrink-0 items-center gap-3">
    {#if version}
      <!-- Wide viewports have room to show the client version next to the About action. -->
      <span class="hidden text-xs tabular-nums md:inline" data-testid="app-header-version"
        >v{version}</span
      >
      <button
        type="button"
        class="app-header-icon"
        onclick={showAboutChatto}
        title={m('ui.tooltip.about', { subject: 'Chatto' })}
        aria-label={m('ui.tooltip.about', { subject: 'Chatto' })}
      >
        <span class="iconify icon-[uil--info-circle] text-lg" aria-hidden="true"></span>
      </button>
    {/if}

    {#if hasInstances}
      <button
        type="button"
        class="app-header-icon"
        onclick={handleSignOut}
        title={m('ui.sign_out')}
        aria-label={m('ui.sign_out')}
      >
        <span class="iconify icon-[uil--signout] text-lg rtl:-scale-x-100" aria-hidden="true"
        ></span>
      </button>
    {/if}
  </div>
</header>

<style>
  /* Keep the surface full-width while content stays clear of native window buttons.
     Overlay coordinates use the viewport, not the padded shell width.
     The custom properties also let stories model the host-provided safe area. */
  .app-header {
    padding-left: calc(0.5rem + var(--app-header-titlebar-x, env(titlebar-area-x, 0px)));
    padding-right: calc(
      0.5rem + 100vw - var(--app-header-titlebar-x, env(titlebar-area-x, 0px)) -
        var(--app-header-titlebar-width, env(titlebar-area-width, 100vw))
    );
    /* Electron window dragging excludes the interactive elements below. */
    -webkit-app-region: drag;
  }
  .app-header :global(a),
  .app-header :global(button) {
    -webkit-app-region: no-drag;
  }
</style>
