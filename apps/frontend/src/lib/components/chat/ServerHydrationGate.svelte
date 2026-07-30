<!--
@component

Keeps a server's real UI mounted and hydrating behind an inert loading screen,
then reveals the complete composition when its cold projection is usable.
-->
<script lang="ts">
  import { untrack, type Snippet } from 'svelte';
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

  // This component is keyed by server. Capture warm initial state and latch
  // after the first cold reveal so later room navigation never replays the
  // full-server loading screen.
  let revealed = $state(untrack(() => ready));
  const showServerUi = $derived(ready || revealed);
</script>

<div class="relative flex min-h-0 min-w-0 flex-1">
  <div
    class={[
      'flex min-h-0 min-w-0 flex-1 transition-opacity duration-300 ease-out motion-reduce:transition-none',
      showServerUi ? 'opacity-100' : 'pointer-events-none opacity-0 select-none'
    ]}
    inert={!showServerUi}
    aria-hidden={!showServerUi}
    data-testid="server-ui"
  >
    {@render children()}
  </div>

  {#if !showServerUi}
    <div
      class="absolute inset-0 z-[60] flex"
      out:fade={{ duration: prefersReducedMotion.current ? 0 : 220 }}
      onoutroend={() => (revealed = true)}
    >
      <ServerLoadingScreen {serverName} {iconUrl} />
    </div>
  {/if}
</div>
