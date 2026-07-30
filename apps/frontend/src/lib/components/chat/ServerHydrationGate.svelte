<!--
@component

Keeps a server's real UI mounted and hydrating behind an inert loading screen,
then reveals the complete composition when its cold projection is usable.
-->
<script lang="ts">
  import type { Snippet } from 'svelte';
  import { prefersReducedMotion } from 'svelte/motion';
  import { fade } from 'svelte/transition';
  import ServerLoadingScreen from './ServerLoadingScreen.svelte';

  let {
    ready,
    serverName,
    iconUrl = null,
    children
  }: {
    ready: boolean;
    serverName: string;
    iconUrl?: string | null;
    children: Snippet;
  } = $props();
</script>

<div class="relative flex min-h-0 min-w-0 flex-1">
  <div
    class={[
      'flex min-h-0 min-w-0 flex-1 transition-opacity duration-300 ease-out motion-reduce:transition-none',
      ready ? 'opacity-100' : 'pointer-events-none opacity-0 select-none'
    ]}
    inert={!ready}
    aria-hidden={!ready}
    data-testid="server-ui"
  >
    {@render children()}
  </div>

  {#if !ready}
    <div
      class="absolute inset-0 z-[60] flex"
      out:fade={{ duration: prefersReducedMotion.current ? 0 : 220 }}
    >
      <ServerLoadingScreen {serverName} {iconUrl} />
    </div>
  {/if}
</div>
