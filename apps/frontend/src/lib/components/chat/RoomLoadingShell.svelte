<!--
@component

Stable room-shaped shell shown while the initial server projection is cold.
Once server chrome is usable, the real room replaces this shell and reuses
TimelineLoadingSkeleton while only the selected timeline is materialising.
-->
<script lang="ts">
  import * as m from '$lib/i18n/messages';
  import PaneHeader from '$lib/ui/PaneHeader.svelte';
  import TimelineLoadingSkeleton from './TimelineLoadingSkeleton.svelte';
</script>

<section
  class="flex min-h-0 min-w-0 flex-1 flex-col"
  aria-busy="true"
  aria-label={m['common.loading']()}
  data-testid="room-loading-shell"
>
  <span class="sr-only" role="status">{m['common.loading']()}</span>

  <PaneHeader title="" loading skeletonButtons={2} />

  <div class="flex min-h-0 min-w-0 flex-1 items-end overflow-hidden">
    <TimelineLoadingSkeleton />
  </div>

  <div class="flex shrink-0 flex-col gap-2 p-2" aria-hidden="true">
    <div class="flex min-h-20 flex-col justify-between rounded-lg bg-surface px-3 py-2.5">
      <div class="skeleton h-3.5 w-40 max-w-[55%] rounded"></div>
      <div class="flex items-end justify-between gap-4">
        <div class="flex gap-2">
          <div class="skeleton h-5 w-5 rounded"></div>
          <div class="skeleton h-5 w-5 rounded"></div>
        </div>
        <div class="skeleton h-7 w-7 rounded-md"></div>
      </div>
    </div>
  </div>
</section>
