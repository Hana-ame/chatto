<!--
@component

The **Server Gutter** — narrow inline-start column listing every server the user
is connected to, plus the add-server button pinned to the bottom. See the
"UI" section of `docs/GLOSSARY.md`.
-->
<script lang="ts">
  import { pushState } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { serverRegistry } from '$lib/client';
  import { m } from '$lib/i18n/messages';
  import { ScrollFader } from '$lib/ui';
  import { onMount } from 'svelte';
  import { SvelteURL } from 'svelte/reactivity';
  import ServerSidebarEntry from './ServerSidebarEntry.svelte';

  const directoryHref = resolve('/chat/servers');
  const directoryActive = $derived(
    page.route.id === '/chat/servers' || page.state.modal?.type === 'addServer'
  );

  /**
   * Open the Server Directory as a history-backed dialog over the current view.
   * Modified clicks keep the link behavior, and the full page stays in place
   * when it is already open.
   */
  function openAddServerDialog(event: MouseEvent) {
    if (
      event.defaultPrevented ||
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey ||
      directoryActive
    ) {
      return;
    }
    event.preventDefault();
    pushState('', { modal: { type: 'addServer' } });
  }

  // 【本地改动 2026-09-01，2026-10-03 恢复】共存游戏入口：Server Gutter 列中服务器
  // 图标下方渲染一份外部链接图标（点击新标签页打开）。数据从本仓库根 links.json 拉取
  // （经 GitHub raw → proxy.moonchan.xyz 代理，与消息图片代理同源，隐藏来源并保留
  // CORS）。图标为图片 URL，走 proxyUrl 改写后作为 <img> src；拉取失败/为空时静默
  // 不显示，不影响 Server Gutter 主体。
  // 用户改链接：编辑 repo 根 links.json（每项 { name, icon, url }），push 即生效；
  // 无需改前端代码、无需重新构建部署。
  // 踩坑：proxy.moonchan.xyz 只透传原 URL 的 Content-Type（raw 的 .json 返回
  // application/json），fetch().json() 只看 body 不校验 header，故可直接解析。
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
          <!-- eslint-disable-next-line svelte/no-navigation-without-resolve -- external absolute URL from links.json, not an app route -->
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
      onclick={openAddServerDialog}
      class={['server-gutter-item cursor-pointer', directoryActive && 'server-gutter-item-active']}
    >
      <span aria-hidden="true" class="iconify icon-[uil--plus]"></span>
    </a>
  </div>
</div>
