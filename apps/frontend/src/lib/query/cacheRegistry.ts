type ServerCacheRemover = (serverId: string) => void;

let removeServerCache: ServerCacheRemover | undefined;

/** Register the snapshot-query cache without loading it into every route bundle. */
export function registerServerQueryCache(remover: ServerCacheRemover): void {
  removeServerCache = remover;
}

/** Purge cached private reads when a server session is disposed. */
export function removeRegisteredServerQueries(serverId: string): void {
  removeServerCache?.(serverId);
}
