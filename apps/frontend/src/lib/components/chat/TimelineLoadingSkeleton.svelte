<!--
@component

Bottom-aligned placeholder rows for a room timeline that is materialising from
the realtime projection. The shapes are deliberately deterministic so loading
does not visually reshuffle across renders.
-->
<script lang="ts"></script>

{#snippet message(
  nameWidth: string,
  firstLineWidth: string,
  secondLineWidth: string | null,
  compact = false
)}
  <div class={['flex gap-3', compact ? 'mt-1' : 'mt-5']}>
    {#if compact}
      <div class="w-9 shrink-0"></div>
    {:else}
      <div class="skeleton h-9 w-9 shrink-0 rounded-full"></div>
    {/if}
    <div class="flex min-w-0 flex-1 flex-col gap-2 pt-0.5">
      {#if !compact}
        <div class={['skeleton h-3 rounded', nameWidth]}></div>
      {/if}
      <div class={['skeleton h-3.5 max-w-full rounded', firstLineWidth]}></div>
      {#if secondLineWidth}
        <div class={['skeleton h-3.5 max-w-full rounded', secondLineWidth]}></div>
      {/if}
    </div>
  </div>
{/snippet}

<div
  class="w-full px-4 pt-10 pb-5 sm:px-6"
  data-testid="timeline-loading-skeleton"
  aria-hidden="true"
>
  <div class="skeleton mx-auto mb-7 h-3 w-28 rounded"></div>

  {@render message('w-24', 'w-[min(34rem,86%)]', 'w-[min(22rem,58%)]')}
  {@render message('', 'w-[min(28rem,72%)]', null, true)}
  {@render message('w-32', 'w-[min(38rem,92%)]', 'w-[min(30rem,74%)]')}
  {@render message('w-20', 'w-[min(24rem,64%)]', null)}
  {@render message('', 'w-[min(32rem,80%)]', null, true)}
</div>
