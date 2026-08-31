<script lang="ts">
  import type { Snippet } from 'svelte';

  let { children }: { children?: Snippet } = $props();
</script>

<!--
  【本地改动 2026-09-01】移除左侧服务器图标列（Server Gutter / 移动侧栏面板）。
  目的：单服务器站点没有切换需求，ServerGutter 纯占宽度；用户明确要求去掉
  这条侧栏。服务器仍可通过顶部 quick-switcher（AppHeader 的 [apps] 图标）
  或 URL 切换，添加服务器走 /chat/servers 页面。
  思路：组件本身保留（test spec 依赖 data-testid 等），仅删 backdrop + panel +
  ServerGutter 渲染，退化为对 children 的透传 wrapper；骨架/动画代码一并移除
  以减少死代码。
  边界：只影响左侧服务器图标列；Chrome 里的房间列表侧栏（ServerSidebar）
  是另一组件，不受影响。
-->

<div class="flex min-h-0 flex-1 flex-row">
  {@render children?.()}
</div>
