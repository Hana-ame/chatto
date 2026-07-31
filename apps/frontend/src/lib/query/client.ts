import { Code, ConnectError } from '@connectrpc/connect';
import { QueryClient, type QueryKey } from '@tanstack/svelte-query';
import { registerServerQueryCache } from './cacheRegistry';

const SERVER_QUERY_STALE_TIME_MS = 30_000;
const SERVER_QUERY_GC_TIME_MS = 5 * 60_000;

function retryServerQuery(failureCount: number, error: Error): boolean {
  if (
    error instanceof ConnectError &&
    [Code.InvalidArgument, Code.NotFound, Code.PermissionDenied, Code.Unauthenticated].includes(
      error.code
    )
  ) {
    return false;
  }
  return failureCount < 1;
}

/** Shared in-memory cache for snapshot-style server reads. */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: SERVER_QUERY_STALE_TIME_MS,
      gcTime: SERVER_QUERY_GC_TIME_MS,
      refetchOnWindowFocus: false,
      retry: retryServerQuery
    },
    mutations: {
      retry: false
    }
  }
});

export function serverQueryRoot(serverId: string): QueryKey {
  return ['server', serverId];
}

/** Remove cached private responses when a server session is disposed. */
export function removeServerQueries(serverId: string): void {
  queryClient.removeQueries({ queryKey: serverQueryRoot(serverId) });
}

registerServerQueryCache(removeServerQueries);
