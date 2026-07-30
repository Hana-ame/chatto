<!--
@component

Full-canvas loading state shown while a cold server's realtime projection is
materialising. The surrounding server chrome stays mounted behind this screen,
then becomes visible as one complete composition.
-->
<script lang="ts">
  import ServerLogo from '$lib/components/ServerLogo.svelte';
  import * as m from '$lib/i18n/messages';

  let {
    serverName,
    iconUrl = null
  }: {
    serverName: string;
    iconUrl?: string | null;
  } = $props();
</script>

<section
  class="relative isolate flex min-h-0 min-w-0 flex-1 items-center justify-center overflow-hidden bg-background"
  role="status"
  aria-live="polite"
  aria-label={m['common.loading']()}
  data-testid="server-loading-screen"
>
  <div class="pointer-events-none absolute inset-0 overflow-hidden" aria-hidden="true">
    <div
      class="absolute top-1/2 left-1/2 size-72 -translate-x-1/2 -translate-y-1/2 animate-pulse rounded-full bg-server/10 blur-3xl motion-reduce:animate-none"
    ></div>
  </div>

  <div class="relative flex flex-col items-center gap-5 px-6 text-center">
    <div class="relative grid size-28 place-items-center" aria-hidden="true">
      <div
        class="absolute inset-3 animate-pulse rounded-full bg-server/10 motion-reduce:animate-none"
      ></div>
      <div
        class="absolute inset-0 animate-[spin_1.8s_linear_infinite] rounded-full border border-border motion-reduce:animate-none"
      >
        <span
          class="absolute top-0 left-1/2 size-2.5 -translate-x-1/2 -translate-y-1/2 rounded-full bg-server shadow-[0_0_18px_var(--color-server)]"
        ></span>
      </div>
      <div class="relative rounded-2xl bg-background p-2 shadow-lg ring-1 ring-border">
        <ServerLogo server={{ name: serverName, logoUrl: iconUrl }} />
      </div>
    </div>

    <div class="space-y-1.5">
      <p class="text-base font-medium text-text-top">{serverName}</p>
      <p class="text-sm text-muted">{m['common.loading']()}</p>
    </div>
  </div>
</section>
