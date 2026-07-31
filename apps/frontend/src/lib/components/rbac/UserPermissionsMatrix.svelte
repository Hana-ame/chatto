<!--
@component

Per-user permission matrix loader. Owns the ConnectRPC query for the user's
matrix and the mutation dispatch for cell clicks; delegates rendering to
`SubjectPermissionsMatrix`.
-->
<script lang="ts">
  import { Hint } from '$lib/ui';
  import { useConnection } from '$lib/state/server/connection.svelte';
  import { createPermissionAPI } from '$lib/api-client/permissions';
  import { toast } from '$lib/ui/toast';
  import * as m from '$lib/i18n/messages';
  import {
    setUserPermission,
    type UserMutationScope,
    type UserPermissionState
  } from './userPermissionMutations';
  import SubjectPermissionsMatrix, {
    type MatrixData,
    type MatrixScope,
    type CellState
  } from './SubjectPermissionsMatrix.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { getActiveServer } from '$lib/state/activeServer.svelte';
  import { adminQueryKeys } from '$lib/query/admin';
  import { queryClient } from '$lib/query/client';

  type Matrix = MatrixData & { userId: string };

  let { userId }: { userId: string } = $props();

  const connection = useConnection();

  function permissionAPI() {
    return connection().getAPI(createPermissionAPI);
  }

  const matrixQuery = createQuery(
    () => ({
      queryKey: adminQueryKeys.userPermissions(getActiveServer(), connection(), userId),
      queryFn: ({ signal }) => permissionAPI().getUserPermissionMatrix(userId, { signal })
    }),
    () => queryClient
  );

  const data = $derived<Matrix | null>(matrixQuery.data ?? null);
  const loading = $derived(matrixQuery.isPending);
  const loadError = $derived(matrixQuery.error instanceof Error ? matrixQuery.error.message : null);
  let mutationError = $state<string | null>(null);
  let updatingKey = $state<string | null>(null);
  let mutationGeneration = 0;

  function mutationScopeFor(scope: MatrixScope): UserMutationScope {
    if (scope.kind === 'GROUP') {
      const groupId = scope.id.startsWith('group:') ? scope.id.slice('group:'.length) : '';
      return { tier: 'group', groupId };
    }
    if (scope.kind === 'ROOM') {
      const roomId = scope.id.startsWith('room:') ? scope.id.slice('room:'.length) : '';
      return { tier: 'room', roomId };
    }
    return { tier: 'server' };
  }

  async function handleCycle(scope: MatrixScope, permission: string, next: CellState) {
    if (!data) return;
    const generation = ++mutationGeneration;
    const serverId = getActiveServer();
    const activeConnection = connection();
    const activeUserId = data.userId;
    const queryKey = adminQueryKeys.userPermissions(serverId, activeConnection, activeUserId);
    const cellKey = `${scope.id}::${permission}`;
    updatingKey = cellKey;
    mutationError = null;

    const result = await setUserPermission(
      activeConnection.getAPI(createPermissionAPI),
      activeUserId,
      mutationScopeFor(scope),
      permission,
      next as UserPermissionState
    );
    if (result.error) {
      if (mutationGeneration === generation) {
        mutationError = result.error;
        updatingKey = null;
      }
      toast.error(result.error);
      return;
    }

    // Reload the matrix so both the override AND effective decisions stay
    // consistent — a server-scope grant flows into rooms via inheritance.
    await queryClient.invalidateQueries({ queryKey, exact: true });
    if (mutationGeneration === generation) updatingKey = null;
  }
</script>

{#if mutationError || loadError}
  <Hint tone="danger">{mutationError ?? loadError}</Hint>
{/if}

{#if loading}
  <div class="text-muted">{m['rbac.permissions.loading']()}</div>
{:else if !data}
  <Hint tone="info">{m['rbac.permissions.no_data']()}</Hint>
{:else}
  <SubjectPermissionsMatrix {data} {updatingKey} onCycle={handleCycle} subjectKind="user" />
{/if}
