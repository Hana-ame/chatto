import { describe, expect, it } from 'vitest';
import { isSelectedServerRouteReady } from './serverHydration';

const memberRoom = { id: 'room-1', viewerIsMember: true };
const nonmemberRoom = { id: 'room-2', viewerIsMember: false };

describe('isSelectedServerRouteReady', () => {
  it('holds a member room behind the loader until its timeline is materialized', () => {
    expect(
      isSelectedServerRouteReady({
        roomId: memberRoom.id,
        rooms: [memberRoom],
        hasTimeline: () => false
      })
    ).toBe(false);

    expect(
      isSelectedServerRouteReady({
        roomId: memberRoom.id,
        rooms: [memberRoom],
        hasTimeline: () => true
      })
    ).toBe(true);
  });

  it('holds a cold member-room permalink until its resolver navigates away', () => {
    expect(
      isSelectedServerRouteReady({
        roomId: memberRoom.id,
        rooms: [memberRoom],
        resolvingRoomMessageLink: true,
        hasTimeline: () => true
      })
    ).toBe(false);
  });

  it.each([
    ['non-room route', null, [memberRoom]],
    ['nonmember room', nonmemberRoom.id, [nonmemberRoom]],
    ['unknown room', 'missing-room', [memberRoom]]
  ])('treats a %s as terminal after base hydration', (_label, roomId, rooms) => {
    expect(
      isSelectedServerRouteReady({
        roomId,
        rooms,
        hasTimeline: () => false
      })
    ).toBe(true);
  });
});
