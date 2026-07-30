type RoomRouteAccess = {
  id: string;
  viewerIsMember: boolean;
};

/**
 * Whether a cold server route has enough projection state for its first reveal.
 *
 * Member rooms require a materialized timeline. Non-room, nonmember, and
 * unknown/invalid routes are terminal once the caller's base projection is
 * ready, so they must not leave the full-server loader up indefinitely.
 */
export function isSelectedServerRouteReady({
  roomId,
  rooms,
  resolvingRoomMessageLink = false,
  hasTimeline
}: {
  roomId: string | null;
  rooms: readonly RoomRouteAccess[];
  resolvingRoomMessageLink?: boolean;
  hasTimeline: (roomId: string) => boolean;
}): boolean {
  if (!roomId) return true;
  const room = rooms.find((candidate) => candidate.id === roomId);
  if (!room?.viewerIsMember) return true;
  if (resolvingRoomMessageLink) return false;
  return hasTimeline(roomId);
}
