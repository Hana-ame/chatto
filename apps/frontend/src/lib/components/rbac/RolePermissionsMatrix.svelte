<!--
@component

Per-role permission matrix loader. Owns the ConnectRPC query for the
role's matrix and the mutation dispatch for cell clicks; delegates
rendering to `SubjectPermissionsMatrix` (shared with the user variant).

  Mutations go through the admin permission API via `setRolePermission`.
-->
<script lang="ts">
  import { Hint } from '$lib/ui';
  import { useConnection } from '$lib/state/server/connection.svelte';
  import { createPermissionAPI } from '$lib/api-client/permissions';
  import { toast } from '$lib/ui/toast';
  import * as m from '$lib/i18n/messages';
  import {
    setRolePermission,
    type MutationScope as RoleMutationScope,
    type PermissionState
  } from './permissionMutations';
  import SubjectPermissionsMatrix, {
    type MatrixData,
    type MatrixScope,
    type CellState
  } from './SubjectPermissionsMatrix.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { getActiveServer } from '$lib/state/activeServer.svelte';
  import { adminQueryKeys } from '$lib/query/admin';
  import { queryClient } from '$lib/query/client';

  type Matrix = MatrixData & { roleName: string };

  let { roleName }: { roleName: string } = $props();

  const connection = useConnection();

  function permissionAPI() {
    return connection().getAPI(createPermissionAPI);
  }

  const matrixQuery = createQuery(
    () => ({
      queryKey: adminQueryKeys.rolePermissions(getActiveServer(), connection(), roleName),
      queryFn: ({ signal }) => permissionAPI().getRolePermissionMatrix(roleName, { signal })
    }),
    () => queryClient
  );

  const data = $derived<Matrix | null>(matrixQuery.data ?? null);
  const loading = $derived(matrixQuery.isPending);
  const loadError = $derived(matrixQuery.error instanceof Error ? matrixQuery.error.message : null);
  let mutationError = $state<string | null>(null);
  let updatingKey = $state<string | null>(null);
  let mutationGeneration = 0;
  const isOwnerRole = $derived(roleName === 'owner');

  function mutationScopeFor(scope: MatrixScope, name: string): RoleMutationScope {
    if (scope.kind === 'GROUP') {
      const groupId = scope.id.startsWith('group:') ? scope.id.slice('group:'.length) : '';
      return { tier: 'group', roleName: name, groupId };
    }
    if (scope.kind === 'ROOM') {
      const roomId = scope.id.startsWith('room:') ? scope.id.slice('room:'.length) : '';
      return { tier: 'room', roleName: name, roomId };
    }
    return { tier: 'server', roleName: name };
  }

  async function handleCycle(scope: MatrixScope, permission: string, next: CellState) {
    if (!data) return;
    const generation = ++mutationGeneration;
    const serverId = getActiveServer();
    const activeConnection = connection();
    const activeRoleName = data.roleName;
    const queryKey = adminQueryKeys.rolePermissions(serverId, activeConnection, activeRoleName);
    const cellKey = `${scope.id}::${permission}`;
    updatingKey = cellKey;
    mutationError = null;

    const result = await setRolePermission(
      activeConnection.getAPI(createPermissionAPI),
      mutationScopeFor(scope, activeRoleName),
      permission,
      next as PermissionState
    );
    if (result.error) {
      if (mutationGeneration === generation) {
        mutationError = result.error;
        updatingKey = null;
      }
      toast.error(result.error);
      return;
    }

    await queryClient.invalidateQueries({ queryKey, exact: true });
    void queryClient.invalidateQueries({
      queryKey: adminQueryKeys.permissionTiers(serverId, activeConnection)
    });
    if (mutationGeneration === generation) updatingKey = null;
  }
</script>

{#if mutationError || loadError}
  <Hint tone="danger">{mutationError ?? loadError}</Hint>
{/if}

{#if loading}
  <div class="text-muted">{m['rbac.permissions.loading']()}</div>
{:else if !data}
  <Hint tone="info">{m['admin.permissions.role_not_found']()}</Hint>
{:else}
  <SubjectPermissionsMatrix
    {data}
    {updatingKey}
    onCycle={handleCycle}
    subjectKind="role"
    forceAllow={isOwnerRole}
    readOnly={isOwnerRole}
  />
{/if}
