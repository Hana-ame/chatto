<!--
@component

The **Server Gutter** — narrow inline-start column listing every server the user
is connected to, plus (below a divider) a set of external-link icons that open
user-defined game/website pages in a new tab, plus the add-server button pinned
to the bottom. See the "UI" section of `docs/GLOSSARY.md`.
-->
<script lang="ts">
  /* eslint-disable svelte/no-navigation-without-resolve -- external game/website URLs must bypass SvelteKit resolve */
  import { onMount } from 'svelte';
  import { SvelteURL } from 'svelte/url';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { serverRegistry } from '$lib/state/server/registry.svelte';
  import type { ServerPermissions } from '$lib/state/server/permissions';
  import { m } from '$lib/i18n/messages';
  import { ScrollFader } from '$lib/ui';
  import ServerSidebarEntry from './ServerSidebarEntry.svelte';

  // Check whether any authenticated server grants a permission.
  // Optimistically returns true while permissions are still loading.
  // Unauthenticated servers are skipped entirely.
  function anyServerHasPermission(key: keyof ServerPermissions): boolean {
    return serverRegistry.servers.some((s) => {
      const store = serverRegistry.tryGetStore(s.id);
      if (!store?.isAuthenticated) return false;

      const perms = store.permissions;
      return !perms.loaded || perms[key];
    });
  }

  void anyServerHasPermission;

  const directoryHref = resolve('/chat/servers');
  const directoryActive = $derived(page.route.id === '/chat/servers');

  // 【本地改动 2026-09-01】共存游戏入口：Server Gutter 列中服务器图标下方
  // 渲染一份外部链接图标（点击新标签页打开）。数据从本仓库根 links.json 拉
  // 取（经 GitHub raw → proxy.moonchan.xyz 代理，与消息图片代理同源，隐藏
  // 来源并保留 CORS）。图标为图片 URL，走 proxyUrl 改写后作为 <img> src；
  // 拉取失败/为空时静默不显示，不影响 Server Gutter 主体。
  // 用户改链接：编辑 repo 根 links.json（每项 { name, icon, url }），push 即
  // 生效；无需改前端代码、无需重新构建部署。
  // 踩坑：proxy.moonchan.xyz 只透传原 URL 的 Content-Type（raw 的 .json
  // 返回 application/json），fetch().json() 只看 body 不校验 header，故可
  // 直接解析；raw.githubusercontent.com 已开放 CORS，亦可不经代理直接 fetch。
  const LINKS_RAW_URL = 'https://raw.githubusercontent.com/Hana-ame/chatto/main/links.json';
  const IMAGE_PROXY_BASE = 'https://proxy.moonchan.xyz';

  type ExternalLink = { name: string; icon: string; url: string };

  function proxyUrl(src: string): string {
    let original: URL;
    try {
      original = new URL(src);
    } catch {
      return '#';
    }
    if (original.protocol !== 'http:' && original.protocol !== 'https:') return '#';
    if (original.hostname === new URL(IMAGE_PROXY_BASE).hostname) return src;
    const proxy = new SvelteURL(IMAGE_PROXY_BASE);
    proxy.pathname = original.pathname;
    proxy.search = original.search;
    proxy.searchParams.set('proxy_host', original.host);
    proxy.searchParams.set('proxy_scheme', original.protocol === 'https:' ? 'https' : 'http');
    return proxy.toString();
  }

  let externalLinks = $state<ExternalLink[]>([]);

  onMount(async () => {
    try {
      // 【本地改动 2026-09-01】JSON 拉取也走 proxy.moonchan.xyz（与图标同源
      // 代理，隐藏 GitHub raw 来源）；proxy 透传原 Content-Type（application/
      // json），fetch().json() 只看 body，可正常解析。
      const r = await fetch(proxyUrl(LINKS_RAW_URL));
      if (!r.ok) return;
      const data = (await r.json()) as ExternalLink[];
      if (Array.isArray(data)) externalLinks = data.filter(({ url }) => !!url);
    } catch {
      // 拉不到静默不显示——不影响 Server Gutter 主体
    }
  });
</script>

<div class="server-gutter flex min-h-0 flex-1 flex-col border-e border-border">
  <ScrollFader top bottom scrollClass="scrollbar-hide">
    <div class="flex flex-col gap-2 p-2 max-md:ps-3">
      {#each serverRegistry.servers as server (server.id)}
        {@const store = serverRegistry.tryGetStore(server.id)}
        {#if store}
          <!-- Authentication changes replace the per-server store. Remount the
               entry so its one-time private-data load follows the new state. -->
          {#key store}
            <ServerSidebarEntry serverId={server.id} />
          {/key}
        {/if}
      {/each}

      {#if externalLinks.length}
        <div class="h-px bg-border"></div>
        {#each externalLinks as link (link.url)}
          <a
            href={link.url}
            target="_blank"
            rel="noopener noreferrer"
            title={link.name}
            aria-label={link.name}
            class="server-gutter-item cursor-pointer"
          >
            <img
              src={proxyUrl(link.icon)}
              alt={link.name}
              class="h-11 w-11 rounded-xl object-cover shrink-0"
            />
          </a>
        {/each}
      {/if}
    </div>
  </ScrollFader>

  <!-- Add Server - pinned to the bottom -->
  <div class="flex shrink-0 flex-col items-center gap-2 p-2 max-md:ps-3">
    <a
      href={directoryHref}
      title={m('chat.server_gutter.add_server')}
      aria-label={m('chat.server_gutter.add_server')}
      aria-current={directoryActive ? 'page' : undefined}
      class={[
        'server-gutter-item cursor-pointer',
        directoryActive && 'server-gutter-item-active'
      ]}
    >
      <span class="iconify icon-[uil--plus]"></span>
    </a>
  </div>
</div>