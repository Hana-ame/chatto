# Interface Inventory

Key files: [`cli/internal/connectapi/api.go`](../../cli/internal/connectapi/api.go),
[`cli/internal/http_server/connect.go`](../../cli/internal/http_server/connect.go),
[`cli/internal/http_server/auth.go`](../../cli/internal/http_server/auth.go),
[`cli/internal/http_server/mcp.go`](../../cli/internal/http_server/mcp.go),
[`cli/internal/mcpserver/handler.go`](../../cli/internal/mcpserver/handler.go),
[`cli/internal/http_server/realtime.go`](../../cli/internal/http_server/realtime.go),
[`proto/chatto/`](../../proto/chatto/)

This inventory records mounted transport and service boundaries. The generated
[ConnectRPC API reference](../../apps/docs-website/src/content/docs/reference/connectrpc-api/index.mdx)
is authoritative for individual RPCs, request and response messages, and public
method documentation.

Related decisions: [ADR-044](../adr/ADR-044-connectrpc-service-conventions.md),
[ADR-045](../adr/ADR-045-public-api-stability-tiers.md),
[ADR-053](../adr/ADR-053-versioned-nats-service-namespaces.md),
[ADR-079](../adr/ADR-079-renewable-bearer-sessions.md),
[ADR-084](../adr/ADR-084-separate-internal-protobufs-by-storage-contract.md), and
[ADR-085](../adr/ADR-085-agent-integration-through-mcp.md).

## Transport boundaries

| Surface | Mount | Contract | Access boundary |
| ------- | ----- | -------- | --------------- |
| Public ConnectRPC | `/api/connect/chatto.{auth,discovery,api,admin}.v1.*` | Unary Connect, gRPC, and gRPC-Web services | Explicit per-service public or authenticated-user policy; method-level authorization remains inside operation models |
| Browser authentication | `GET /auth/browser/csrf`, `POST /auth/browser/login`, `POST /auth/browser/register/complete`, `POST /auth/browser/logout`, `POST /auth/browser/session/migrate`, `POST /auth/browser/session/renew`, `POST /auth/browser/revoke-bearer-session` | Bound CSRF-proof repair, cookie-only password/registration authentication, one-time 0.4 typed-cookie migration, logout, stable-handle session renewal, and removal of stored origin bearer authority | Every mutation requires JSON and an exact same-origin request. A browser-auth mode header, if present, must select cookies. Browser routes treat an absent header as cookie mode. Renewal and logout also require signed double-submit CSRF proof while a valid cookie authority exists. Migration uses the independent browser-route proof because it runs before a current cookie session exists. Logout can clear invalid session cookies with the same proof. The safe CSRF route requires a valid cookie session. These routes do not return bearer credentials. |
| Programmatic authentication | `POST /auth/login`, `POST /auth/register/complete`, `POST /auth/logout`, `POST /oauth/token` | First-party bearer issuance, stable bearer-session revocation, and OAuth code/refresh exchange | JSON is required for direct login and registration. These routes do not create, read, or clear ambient browser authentication cookies. OAuth token exchange also accepts the documented form encoding. |
| Realtime WebSocket | `GET /api/realtime` | Binary `chatto.realtime.v1.Realtime*` frames | Bearer access token in the hello frame or same-origin cookie; exact human credentials are revalidated before subscription and once per minute; bearer expiry and cookie renewal thresholds request reconnects, while OAuth-client blocks terminate matching established sessions |
| Bot incoming webhook | `POST /webhooks/incoming/{credential}` with optional `room_id` query parameter | Slack-compatible plain-text JSON subset with Chatto aliases and optional thread creation | Action-limited bot webhook credential; the handler posts through the normal message operation and does not accept the bot API key |
| Server OIDC client metadata | `GET /oauth/client-metadata.json` | CIMD public-client identity and exact callbacks for Chatto server login | Public; mounted only when an OIDC provider uses this deployment's metadata URL as its client ID |
| Frontend OAuth client metadata | `GET /oauth/frontend-client-metadata.json` | CIMD public-client identity and exact popup callback for connecting the bundled frontend to Chatto servers | Public; always mounted, but publishes metadata only when the request host matches `webserver.url` or an exact non-wildcard `webserver.allowed_origins` entry |
| Chatto client authorization | `GET /oauth/authorize`, `POST /oauth/token` | Authorization Code with S256 PKCE plus rotating refresh grant for a client application connecting to a Chatto server; browser clients use a CIMD URL `client_id`, Desktop uses its built-in identity, native clients can use registered local callbacks, and an optional `provider_id` hint can start one server-configured login provider | Public authorization start and CORS token/refresh exchange; the validated client identity and callback are bound through code exchange, local callbacks require consent for each authorization, refresh remains client-bound, and provider hints cannot supply an issuer or endpoint |
| OAuth authorization-server metadata | `GET /.well-known/oauth-authorization-server` on the public listener | RFC 8414 discovery for Chatto OAuth, including PKCE, CIMD, the authorization-response issuer, refresh, and enabled MCP scopes | Public metadata with wildcard read-only CORS |
| Network MCP | `/mcp` on the public HTTP listener; `GET /.well-known/oauth-protected-resource/mcp` publishes RFC 9728 metadata when `[mcp].enabled = true` | MCP `2026-07-28` stateless Streamable HTTP with `get_server_info`, `get_current_user`, `list_rooms`, `list_room_messages`, `post_message`, `join_room`, and `leave_room`; the canonical origin and exact non-wildcard server aliases each publish a separate MCP resource, while `webserver.url` remains the OAuth issuer | Resource-bound OAuth bearer for the exact requested origin with the current room and message read/write MCP scopes, or a current bot API key; every tool call also uses the normal operation authorization model and confirmed missing RBAC permissions are returned as tool errors |
| Protected attachments | `GET /assets/files/{assetId}` and image transform variants | Per-user URLs use hourly issuance buckets with 23–24 hours of remaining validity; Chatto streams full responses, while passive S3-backed video, audio, and large files can redirect to short-lived presigned URLs | Signed `access` ticket, authenticated cookie, or bearer token; every request rechecks room membership before resolving storage or exposing binary bytes |
| Protected HLS video | `GET /assets/hls/{assetId}/master.m3u8`, rendition playlists, and segments | Master and media playlists are generated from the durable manifest; segments are complete bounded responses from NATS or S3 | Domain-separated source-video `access` ticket; every request rechecks room membership and every segment ID/role against the durable HLS manifest |
| Operator ConnectRPC | `/api/connect/chatto.operator.v1.*` on the configured Unix socket | Root-equivalent local unary services | Unix-socket filesystem permissions; never mounted on the public listener |
| Trusted NATS services | `svc.chatto.>` and `svc.chatto_ext.>` | Versioned protobuf request/reply through NATS micro services | NATS account permissions; extension providers receive only their configured service and upstream Core subjects |
| Reflection | `/api/connect/grpc.reflection.v1*` and `v1alpha*` | Public service descriptors | Public; restricted resolver excludes internal `chatto.core.*` types |

<!-- 【本地改动 2026-09-12 + 2026-09-13】fork 的附件接口面比上面这行描述的小一层。

     1. **URL 生成层（主）**：`MediaModel.Get*TransformedAttachmentAssetURL` 直接
        忽略 width/height/fit，回原图链接（公开版 `/assets/files/{id}/{fn.ext}`，
        ticket 版 `/assets/files/{id}?access=…`）。access ticket 里也不再带
        transform 参数，客户端根本拿不到 `/image/{w}x{h}/{fit}` 链接。两个调用点
        是 connectapi 的 apiAsset.ThumbnailAssetUrl 与时间线装配。
     2. **HTTP 层（兜底）**：已经发出去的旧 `/assets/files/{id}/image/{w}x{h}/{fit}`
        链接（旧客户端缓存、CDN、被粘贴到别处的 URL）仍然可用，但直接返回存储的
        那一份字节，响应头 `X-Cache: BYPASS`，不缩放、不重编码、不读写
        ASSET_CACHE。实现见 cli/internal/http_server/assets.go 的
        transformRequest.BypassTransform 与 serveBypassedOriginalAsset。

     【2026-09-13】服务端资产（头像/logo/banner/链接预览）的接口面也缩到了同一
     层：

     1. **URL 生成层**：`MediaModel.GetTransformedServerAssetURLWithFilename`
        忽略 width/height/fit，override 成原档链接
        `/assets/server/{key}[/{fn.ext}]`，不再生成带 HMAC 签名的 `/t/{sig}` 路径。
        调用点是 core 的 serverAssetURL 与头像 URL、前端 /favicon 与
        /apple-touch-icon 重定向（两个尺寸现在指向同一条原档链接）、以及 PWA
        manifest 的 192x512 图标。
     2. **上传期压缩**：四条服务端资产路径（assets 的
        ProcessAvatarImageWithConfig / ProcessLogoImageWithConfig /
        ProcessBannerImageWithConfig / ProcessLinkPreviewImageWithConfig）共用
        processServerAssetImage：缩放到各自上限后用 ffmpeg libwebp 压成**有损
        WebP**（-q:v 85）。所以请求期没有第二份更小的字节可给，浏览器自己降采样。
     3. **HTTP 层**：旧的 `/assets/server/{key}/t/{sig}` 链接同样走 BYPASS，
        返回存储的原字节 + `X-Cache: BYPASS`。

     所以上面这行的 "image transform variants" 在 fork 里只是遗留兼容面，不是
     功能面；ASSET_CACHE 的两个命名空间（attachment-stable-v2.* 与 server.*）
     都永远不被写入。

     背景与取舍见 cli/internal/core/attachments.go 的 override 注释和
     docs/fdr/FDR-008 的 fork 说明块。合并 upstream 时这两层都要人工恢复。 -->

The public HTTP edge mounts every handler returned by `connectapi.API.Handlers`.
Authenticated services are wrapped with `connectrpc.com/authn` before protobuf
decoding and validation. `ExternalIdentityAuthService`,
`PushSubscriptionCleanupService`, `ServerDiscoveryService`, and reflection are
public; all other public-listener services require an authenticated user. The Operator API uses
`connectapi.API.OperatorHandlers` and is mounted only on the configured Unix
socket.

## Mounted public services

| Package | Public services | Auth policy |
| ------- | --------------- | ----------- |
| `chatto.auth.v1` | `ExternalIdentityAuthService`, `PushSubscriptionCleanupService` | Public capability-token flows |
| `chatto.discovery.v1` | `ServerDiscoveryService` | Public discovery |
| `chatto.api.v1` | `AssetService`, `AssetUploadService`, `BotService`, `MessageSearchService`, `MessageService`, `MyAccountService`, `NotificationPolicyService`, `NotificationService`, `PushNotificationService`, `RoleService`, `RoomDirectoryService`, `RoomService`, `ServerService`, `ThreadService`, `UserService`, `ViewerService`, `VoiceCallService` | Authenticated user |
| `chatto.admin.v1` | `AdminDiagnosticsService`, `AdminEventLogService`, `AdminInviteLinkService`, `AdminOAuthClientService`, `AdminPermissionService`, `AdminRoleService`, `AdminRoomLayoutService`, `AdminServerService`, `AdminUserService` | Authenticated user; methods enforce administrative permissions |

`AdminInviteLinkService` requires `user.invite`. Its resource includes the
full, deterministically reconstructed invite link so authorised operators can
copy it again; raw bearer tokens are not stored in `EVT`. Opening
`/invite/{token}` validates the compact capability, stores only the invitation
ID in the signed browser session, and immediately redirects to registration.

`AdminServerService` provides CRUD operations for Neighbor resources. These
methods require `server.manage-neighbors`. `ServerDiscoveryService.ListNeighbors`
returns canonical origins without a session or an ordering contract. The
server does not contact the advertised origins.

`UserService` provides user reads and the canonical target-aware avatar upload
and delete operations. Self-targeting is available to human and bot callers. A
cross-human target requires `user.manage-accounts`. A cross-bot target permits
the bot owner, `user.manage-accounts`, or `bot.manage`. A bot cannot target
another account. These operations validate stable request-time authorization
inputs, use OCC on the target user aggregate, and then return the ready user
projection.

`BotService` exposes bot lifecycle, administrator-initiated owner reassignment,
and create and revoke operations for as many as 20 named API keys and 20 named
incoming webhooks for each bot. Bot
permission reads and writes use `AdminPermissionService`'s canonical user
permission operations with the bot's user ID as the target. Human owners can
manage their own bots; `bot.manage` allows global management. A human with
`user.manage-accounts` can list and read all bots for avatar administration,
but this visibility does not grant bot credential, permission, ownership, or
lifecycle authority.

Owner reassignment validates stable request-time authorization inputs and uses
user-family OCC. This boundary serializes reassignment with deletion of the bot
or either human owner.

Matrix room metadata is limited to rooms visible to both the bot owner and the
managing caller; group metadata follows the room directory's complete group
layout so empty groups remain configurable. Each bot API key authenticates the
normal public and realtime surfaces, but cannot call bot-management or human
account-security operations. Reassignment requires `bot.manage`, preserves the
active keys and configured allowlist, and immediately changes the owner
permission ceiling.

API-key creation returns the raw key once. Safe metadata includes its stable
ID, manager-defined name, creation time, and best-effort last-use telemetry.
Revocation closes only established realtime connections that used the selected
key.
Incoming webhook creation returns the complete URL once. A manager replaces a
webhook when the manager creates a new credential, moves the caller, and
revokes the old credential. Each webhook can be revoked without a change to
other webhooks. Safe metadata includes the creation time and best-effort
last-use telemetry. The separate credentials cannot authenticate ConnectRPC or
realtime requests.

`NotificationPolicyService` provides explicit server, room-group, and room
policy scopes. Its batch read accepts at most 100 scopes, removes duplicates in
first-seen order, and omits missing or inaccessible resource scopes. The
existing `NotificationService` policy methods remain available for server and
room integrations.

`AdminDiagnosticsService.GetSystemInfo` is owner-only and includes
broker-derived status for Chatto's known durable worker queues. The additive
worker list is absent on older servers; clients must treat that as diagnostics
unavailable rather than as a healthy empty set.
JetStream account, stream/consumer, server-statistics, and projection telemetry
is independently optional. Message presence or the projection-availability flag
records whether collection succeeded, so one failure does not suppress unrelated
system diagnostics or turn unavailable metrics into healthy-looking zeroes.

## Mounted operator services

| Package | Service | Access policy |
| ------- | ------- | ------------- |
| `chatto.operator.v1` | `OperatorUserService` | Root-equivalent access over the private Unix socket |

## Trusted NATS services

The `chatto.search.v1` provider contract defines normalized query and readiness
messages under `svc.chatto_ext.search.v1.>`. `search.Client` validates both
sides of request/reply, maps NATS micro error headers, and treats missing
responders or the bounded provider-call deadline as provider unavailability.
Compatible providers share a queue group for replica load balancing. Ready
status and queries use `.status` and `.query`; startup progress uses
`.status.startup` only as a fallback when no ready status responder exists.
The bundled provider joins both ready queues only after replay is current.

This is a trusted server-side integration surface, not a public client API.
Query responses contain thin message and room IDs. The public
`MessageSearchService` prefilters provider queries to the caller's complete
current member-room set. It then uses
`MessageSearchReadModel` and the normal timeline hydrator to recheck room
membership, current body availability, and message/room identity before
returning canonical `Message` resources. Public cursors encrypt and authenticate
the provider cursor and bind it to the viewer and complete public request.

The bundled provider runs under `chatto run` when
`search_provider.enabled = true`; the same unit runs standalone through
`chatto search-provider`. `search.enabled` independently controls whether the
public service accepts queries. `GetStatus` preserves disabled, indexing,
ready, degraded, and unavailable states without affecting other APIs. Exact
provider replay counts stay on the trusted NATS contract and in operator logs;
the authenticated public status does not expose server-wide event-log scale.

`ServerDiscoveryService.GetServer` and `ListNeighbors` support side-effect-free
GET. They also receive wildcard public CORS and conditional-response caching.
Other bundled-client Connect traffic uses POST.

The discovery response includes the server software version as public
pre-authentication state, along with configured provider metadata and the
independently configured direct-registration and direct-login capabilities.
The direct-login capability uses scalar presence so a new client treats an
older server that omits it as enabled. The bundled client refreshes discovery
per server and owns an internal feature-to-minimum-server-version table for compatibility gates.
The 0.5 client requires the 0.5 server baseline before opening realtime
protocol 2, the only accepted behavioral version. The
`chatto.realtime.v1` suffix remains the protobuf namespace.

Public server discovery includes each OIDC provider's issuer for clients that
need to identify or present configured login options. Authling has no special
frontend trust path: a Chatto server uses it only when the operator configures
it as an ordinary OIDC provider.

`MessageSearchService.GetStatus` remains the authority for configured search
availability and transient provider readiness. Viewer permissions remain the
authority for authenticated feature access.

Public URL generation prefers the configured `webserver.url`. Without it, the
HTTP edge uses only the direct request TLS state and host; forwarded protocol
headers are not implicitly trusted. `webserver.trusted_proxies` affects client
IP attribution and realtime same-origin comparison, not public URL authority.

Chatto-streamed protected attachments are sequential full responses. They
advertise `Accept-Ranges: none` and ignore `Range`, returning `200` with the
complete object. NATS-backed video is therefore not seekable. Passive S3-backed
media redirects after authorization to a presigned object URL whose storage
backend provides byte-range delivery.

Processed videos can instead expose HLS. Six-second MPEG-TS segments make
seeking and adaptive rendition switching independent of byte-range support.
HLS child responses remain behind Chatto so membership loss revokes an already
issued playlist ticket on its next playlist or segment request.
