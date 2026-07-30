<script module lang="ts">
  import { defineMeta } from '@storybook/addon-svelte-csf';
  import RoomLoadingShell from './RoomLoadingShell.svelte';

  const { Story } = defineMeta({
    title: 'Chat/Room loading shell',
    component: RoomLoadingShell,
    parameters: {
      layout: 'fullscreen'
    }
  });
</script>

<Story name="Cold server" asChild>
  <div class="fixed inset-0 flex min-w-0 bg-background">
    <RoomLoadingShell />
  </div>
</Story>
