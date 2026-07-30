<script module lang="ts">
  import { defineMeta } from '@storybook/addon-svelte-csf';
  import ServerLoadingScreen from './ServerLoadingScreen.svelte';

  const { Story } = defineMeta({
    title: 'Chat/Server loading screen',
    component: ServerLoadingScreen,
    parameters: {
      layout: 'fullscreen'
    }
  });
</script>

<Story name="Cold server" args={{ serverName: 'Starlight Commons' }} asChild>
  <div class="fixed inset-0 flex min-w-0 bg-background">
    <ServerLoadingScreen serverName="Starlight Commons" />
  </div>
</Story>
