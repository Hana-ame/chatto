# Realtime Protocol v2 Migration

2026-08-04 · chatto server `0.5.0-dev` / chatto-bot

> **【本地改动 7e17ebe7】（2026-08-04 记录）** 本文件整体为 fork 独有，upstream 没有同名文件。
>
> - **目的**：记录 `chatto-bot` 因 realtime 协议 v2 不兼容而全部命令无响应的排查过程与 bot 侧迁移步骤。
> - **思路**：v2 由 upstream commit `a8868531`（#1588，resumable server projection stream）引入，是 breaking change；客户端必须升握手版本并把持久事件的投递解析整体改写为 projection stream 形态。
> - **踩坑**：握手版本不符时服务端回 `unsupported_protocol` 且 `fatal=True`，连接直接断开、没有降级路径——现象是「bot 进程活着但完全不响应」，很容易被误判成网络或鉴权问题。排查入口是服务端日志里那一行 `Realtime error: code=unsupported_protocol`。
> - **边界**：只覆盖 chatto-bot 的 Python 侧适配。服务端协议定义在 `proto/chatto/realtime/v1/`，不受本文件影响；后续 upstream 对 v2 的增量改动需另行比对。
> - **合并提示**：upstream 无同名文件，正常合并不冲突；本文件不会随 upstream 自动同步，upstream 协议再变时需人工更新。


## 背景与根因

`chatto-bot` 的 `!create` 等命令无响应。排查后发现实时流根本没有建立：

- 服务端 `cli/internal/http_server/realtime.go:28` 要求 `realtimeProtocolVersion = 2`，
  握手时版本不符即回 `unsupported_protocol`（fatal）：
  ```
  Realtime error: code=unsupported_protocol message=unsupported realtime protocol version fatal=True
  ```
- bot 的 `src/chatto_bot/realtime.py` 仍用 `PROTOCOL_VERSION = 1`。

服务端在 commit `a8868531 feat(realtime)!: add resumable server projection stream (#1588)`
引入了不兼容的 v2。这是一个 breaking change：客户端握手版本必须升到 2，且事件投递形态整体改变。

## v2 协议变更（协议视角）

### 帧结构

`RealtimeServerFrame` 新增：

| 帧 | 类型 | 说明 |
|---|---|---|
| `projection_event` | `RealtimeProjectionEvent` | 持久事实（消息、房间变更等）的唯一投递路径 |
| `caught_up` | `RealtimeCaughtUp{cursor}` | 订阅后 durable replay 到达 live 边界的标记 |

`RealtimeClientFrame` 新增 `hydrate_room`（`RealtimeHydrateRoom{room_id}`）。

`RealtimeClientHello` 用 `protocol_version = 2`（`resume_cursor` 字段被保留废弃）。
`RealtimeSubscribeEvents` 保留 `retained_room_ids` 字段。

### `RealtimeEventEnvelope` 瘦身

`RealtimeEventEnvelope.event` oneof 中所有**持久事件字段全部 reserved**，
只保留 5 种瞬时、不可回放的事件：

- `user_typing`(30)
- `presence_changed`
- `mention_notification`
- `new_direct_message_notification`
- `session_terminated`

被移走的旧字段（v1 时代在 envelope 里）：`message_posted`、`message_edited`、
`message_retracted`、`reaction_added`、`reaction_removed`、`room_created`、
`room_updated`、`room_deleted`、`room_archived`、`room_unarchived`、
`user_joined_room`、`user_left_room`、`user_profile_updated`、`notification_*`、
`thread_*`、`asset_*`、`call_*`、`server_*`、`room_universal_changed`、
`room_marked_as_read` 等。

### Projection 操作

`RealtimeProjectionEvent.operations` 是有序的幂等变更序列，目前 18 种操作
（`RealtimeProjectionOperation`），bot 只关心其中 3 种：

| 操作 | 语义 | bot 处理 |
|---|---|---|
| `room_timeline_event_upsert` | 在 retained 房间时间线新增/替换一条 `RoomTimelineEvent` | **dispatch** |
| `room_upsert` | 新增/替换一个房间及其 viewer 状态 | 更新 room-kind 缓存（DM vs channel） |
| `room_remove` | 移除房间 | 清 room-kind 缓存 |

其余（`reset`、`server_upsert`、`viewer_upsert`、`user_upsert`、`user_remove`、
`room_groups_replace`、`room_timeline_replace`、`server_state_upsert`、
`room_timeline_event_remove`、`notifications_replace`、
`room_viewer_state_replace`、`active_calls_replace`、`presences_replace`、
`thread_viewer_states_replace`、`room_activity`）由 catch-up 或无关。

### `RoomTimelineEvent` 形态

v2 的时间线事件（与 `GetRoomEvents` 返回的一致）：

```proto
message RoomTimelineEvent {
  string id = 1;
  google.protobuf.Timestamp created_at = 2;
  string actor_id = 3;
  oneof event {
    RoomMessagePosted message_posted = 10;   // 内联完整 Message
    RoomTimelineRoomEvent room_created = 20;
    RoomTimelineRoomEvent room_updated = 21;
    RoomTimelineRoomEvent room_deleted = 22;
    RoomTimelineRoomEvent room_archived = 23;
    RoomTimelineRoomEvent room_unarchived = 24;
    RoomTimelineRoomEvent user_joined_room = 30;
    RoomTimelineRoomEvent user_left_room = 31;
  }
}
```

关键点：

- 只有 8 个 oneof case。**v2 实时流不再投递 `message_edited`、`message_retracted`、
  `reaction_added` 等**——这些 v1 envelope 信号被彻底移除。
- `message_posted` 携带**完整内联 `Message`**（含 body、attachments、reactions、
  thread、deleted_at 等）和 `RoomTimelineIncludes.users`（actor 已在 includes 里），
  **不需要像 v1 那样再 fetch/hydrate**。
- 消息被删除（撤回/crypto-shredding）通过 `Message.deleted_at`（field 21）标记。

### Retained 房间（hydrate）

v2 只给 **retained** 房间投递 timeline 变更（上限 64 个）：
- 订阅时在 `RealtimeSubscribeEvents.retained_room_ids` 声明，或
- 订阅后用 `hydrate_room` 帧请求。

retained 流程：订阅 → `subscribed` →（若 reset）投影快照 → 事件回放 →
reconciliation → `caught_up` → live 投递。调用 `hydrate_room` 后服务器回
`room_timeline_replace`，随后该房间的变更以 `room_timeline_event_upsert` 实时到达。

## bot 侧适配（`chatto-bot`）

改动文件：`src/chatto_bot/realtime.py`、`bot.py`、`types.py`、`plugins/admin.py`、
`tests/*`，以及 `src/chatto_bot/_pb/**`（buf v1.72.0 全量重生成）。

### realtime.py

- `PROTOCOL_VERSION = 2`。
- `run()` / `_run_connection()` 新增 `on_timeline` 回调参数
  （`Callable[[RealtimeProjectionEvent], Awaitable[None]] | None`）。
- 主循环新增分支：
  - `projection_event` → 调 `on_timeline`（`Unauthenticated`/`RealtimeStopped` 透传，
    其余异常记录日志吞掉）；
  - `caught_up` → debug 日志。
- 新增 `hydrate(room_id)` 方法（入队到 `_hydrate_queue`），主循环每轮先 flush 队列
  发 `hydrate_room` 帧。
- hello / subscribed 用 `isinstance` 校验 payload 类型。

### bot.py

- 新增 `_on_projection(projection)`：遍历 `operations`，
  - `room_timeline_event_upsert` → 若 `_will_dispatch(tev.event.field)` 则走
    `_dispatch_timeline_event(tev, includes)`（消息 body 已内联，无额外 fetch）；
  - `room_upsert` → `_room_kinds[room.id] = room.kind == RoomKind.DM`（注意
    `room.kind` 是 int 枚举，不能 `getattr(..., "name", "")`，恒为空字符串）；
  - `room_remove` → 清缓存。
- `_run_realtime` 传入 `on_timeline=self._on_projection`。
- `_catch_up` 末尾对允许的已加入房间逐一 `self.realtime.hydrate(room.id)`
  （复用 config.rooms / dms 过滤逻辑）。

### plugins/admin.py

`_rooms_join` / `_rooms_create` 成功后立即 `self.bot.realtime.hydrate(room_id)`，
使新房间的 timeline 实时到达。

### types.py

- `parse_envelope` 与 `_EVENT_BUILDERS` 兼容 `RoomTimelineEvent` 载荷（鸭子类型：
  有 `.id/.actor_id/.created_at/.event` oneof）。
- `_message_posted_from_proto` 兜底分支改用 `getattr(signal, "room_id", "")`
  （`RoomMessagePosted` 没有 `room_id` 字段，只有内联 `message`）。
- 已不存在的 v1 signal（`message_edited`/`message_retracted`/`reaction_added` 等）
  的 builder 保留为死代码；测试相应删除或改为 v2 timeline 形态。

## 验证

线上日志（bot.log）：

```
Realtime server hello: version=0.5.0-dev+6a77ca864758 heartbeat=15s
capabilities=['chatto.realtime.events.live.v1', 'chatto.realtime.heartbeat.v1',
'chatto.realtime.ping.v1', 'chatto.realtime.events.resume.v1',
'chatto.realtime.projection.v1']
Realtime stream subscribed
Catch-up replayed 34 missed events
Realtime hydrating room <id>  × 11（全部已加入房间）
```

- 不再有 `unsupported_protocol`。
- 订阅、catch-up、房间 hydrate 均正常。
- `!create` 实测通过。

测试：`tests/` 193 个全部通过。提交：`524edb6 Upgrade realtime to protocol v2`。

## 踩坑记录

1. **服务端源码在本地 monorepo** `/mnt/d/WorkPlace/chatto`，VPS 上只有编译好的二进制，
   排查时不要去找 VPS 上的源码。
2. **buf 版本**：v1.47.2 解析不了 `buf.gen.python.yaml` 的 `types` 字段，
   需手动下载 v1.72.0（`/tmp/opencode/buf`）后 `PATH=... bash scripts/gen-proto.sh`。
3. **`room.kind` 是 int 枚举**：protobuf-python 的 enum 字段读出来是 int，
   `getattr(room.kind, "name", "")` 恒返回 `""`，必须 `room.kind == RoomKind.DM`。
4. **websockets 误用代理**：shell 里 `SOCKS_PROXY=172.29.80.1:10808`（无 scheme）会被
   `websockets.connect` 当代理解析并抛 `InvalidProxy`。重启 bot 需
   `env -u SOCKS_PROXY -u socks_proxy setsid nohup .venv/bin/python run_ai_bot.py ...`。
5. **后台启动超时陷阱**：`bash` 工具里 `cd A && nohup ... &` 的后台作业若让命令超时，
   进程组可能被信号带走（日志见 "Shutting down..."），用 `setsid` 脱离会话。
