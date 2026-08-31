<script lang="ts">
  import { pushState, goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { serverRegistry } from '$lib/state/server/registry.svelte';
  import { serverConnectionManager } from '$lib/state/server/serverConnection.svelte';
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

  // Aggregate exact notification counts across all servers.
  const totalNotificationCount = $derived(
    serverRegistry.servers.reduce(
      (sum, instance) =>
        sum + (serverRegistry.tryGetStore(instance.id)?.notifications.unreadNotificationCount ?? 0),
      0
    )
  );
  const totalImportantNotificationCount = $derived(
    serverRegistry.servers.reduce(
      (sum, instance) =>
        sum +
        (serverRegistry.tryGetStore(instance.id)?.notifications.importantUnreadNotificationCount ?? 0),
      0
    )
  );

  // Show sign-out button when any server is registered
  const hasInstances = $derived(serverRegistry.servers.length > 0);
  const preferencesServerId = $derived.by(() => {
    const activeServerId = getActiveServer();
    if (activeServerId && serverRegistry.isAuthenticated(activeServerId)) return activeServerId;
    return serverRegistry.firstAuthenticatedServerId();
  });
  function handleSignOut() {
    pushState('', { modal: { type: 'logout' } });
  }

  function showAboutChatto() {
    pushState('', { modal: { type: 'aboutChatto' } });
  }

  // 【本地改动 2026-09-01】修复移动端「通知页/无服务器页点 hamburger 房间列表
  // 不出现」：hamburger 调 sidebarNav.toggle()，但房间列表侧栏（ServerSidebar
  // + RoomList）只由 Chrome 在 [serverId] 路由下挂载；通知页 /chat/notifications
  // 不在 [serverId] 下，toggle 后 DOM 里根本没有房间列表面板可滑出，用户只见
  // 服务器图标列，误以为坏了。
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
        void goto(resolve('/chat/[serverId]', { serverId: serverIdToSegment(serverId) }));
      }
      return;
    }
    sidebarNav.toggle();
  }
</script>

<header class="app-header flex items-center justify-between gap-2 p-2 text-muted md:text-sm">
  <!-- Leading: global navigation, notifications, and client-wide actions -->
  <div class="flex items-center gap-3">
    <!-- Hamburger - 44px tap target for mobile accessibility. 打开房间列表侧栏。 -->
    <button
      type="button"
      class="app-header-icon"
      onclick={handleHamburger}
      aria-label={m('ui.toggle_sidebar')}
      aria-expanded={sidebarNav.isOpen}
      title={m('ui.toggle_sidebar')}
    >
      <span class="iconify icon-[uil--bars] text-xl"></span>
    </button>

    {#if hasInstances}
      <!-- Notification bell - 44px tap target for mobile accessibility -->
      <a
        href={resolve('/chat/notifications')}
        aria-label={m('ui.notifications')}
        title={m('ui.notifications')}
        class="relative app-header-icon"
      >
        <span class="iconify icon-[uil--bell] text-lg"></span>
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
        <span class="iconify icon-[uil--apps] text-lg"></span>
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
        title={m('ui.realtime_paused')}
      ></span>
    {/if}
  </div>

  <!-- MOTD -->
  {#if motd}
    <MotdContent {motd} />
  {:else}
    <span class="flex-1"></span>
  {/if}

  <!-- Actions: Version + Logout -->
  <div class="flex items-center gap-3">
    {#if version}
      <button
        type="button"
        class="min-h-10 cursor-pointer rounded px-2 text-muted transition-colors hover:bg-surface-emphasized hover:text-text focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-action"
        onclick={showAboutChatto}
        title={m('ui.tooltip.about', { subject: 'Chatto' })}
        aria-label={m('ui.tooltip.about', { subject: 'Chatto' })}
      >
        v{version}
      </button>
    {/if}

    {#if hasInstances}
      <button
        type="button"
        class="iconify icon-[uil--signout] cursor-pointer hover:text-text"
        onclick={handleSignOut}
        title={m('ui.sign_out')}
      >
      </button>
    {/if}
  </div>
</header>

<style>
  /* Tauri window dragging - header is draggable, interactive elements are not */
  .app-header {
    -webkit-app-region: drag;
  }
  .app-header :global(a),
  .app-header :global(button) {
    -webkit-app-region: no-drag;
  }
</style>
