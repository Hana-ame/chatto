# FDR-008: File Attachments & Video Processing

**Status:** Active
**Last reviewed:** 2026-08-25

> **【本地改动 32e1f566 + 2026-09-02 + 2026-09-12】（room 附件图片存储与衍生图）**
> 本文件属 upstream 所有；下列是 fork 独有的文字改动，合并 upstream 时会被上游
> 版本覆盖，需要人工恢复（正文各处以 `【本地改动 ...】` HTML 注释就地标记）。
>
> **2026-09-12 起 fork 的附件策略（现行）**
>
> - **上传时编码一次，请求期零编码**：room 附件图片在上传时用 ffmpeg 重编码为
>   **原尺寸 AVIF**（CRF 30，优先 `libsvtav1`，回退 `libaom-av1`；静态输入出静态
>   AVIF，动画 GIF/WebP 出动画 AVIF）。ffmpeg 或 AV1 编码器不可用、或编码失败时
>   存原字节（best-effort，不阻塞上传）。
> - **取消衍生图（两层）**：
>   ① **URL 生成层（主）**：`core.Get*TransformedAttachmentAssetURL` 直接忽略
>   宽高/fit，回原图链接（`/assets/files/{id}/{fn.ext}`，ticket 版保留
>   `?access=`），客户端根本拿不到 `/image/` 链接。
>   ② **HTTP 层（兜底）**：已经发出去的旧 `/assets/files/{id}/image/{w}x{h}/{fit}`
>   链接（旧客户端缓存、CDN、被粘贴到别处的 URL）仍可用，直接返回存储的那一份
>   字节（响应头 `X-Cache: BYPASS`），不缩放、不重编码、不读写 ASSET_CACHE。
> - **停用视频转码**：`cmd/run.go` 从不置 `VideoUploadsEnabled`，PostMessage 不追加
>   `AssetProcessingStartedEvent`，durable worker 空转。视频原样存、原样发；动画
>   GIF 也不再生成 MP4，走图片管线出动画 AVIF。
> - **前端不再编码**：`prepareFiles` 原样透传文件字节（`heic2any` 已移除）。
>
> **改到的 upstream 正文**
>
> 1. `## Overview`：加了 fork 现状注释。
> 2. `## Behavior`：视频处理、图片渲染尺寸、衍生图这几条 bullet。
> 3. `### 10. Displayed images use bounded derivatives` 的 Decision / Why / Tradeoff
>    三行。
>
> **已知上游正文与 fork 不一致（未改，留给人决定）**
>
> - `### 6` 里 "Message posting atomically appends ... an `AssetProcessingStartedEvent`
>   for newly uploaded video/animated-GIF assets"：fork 从不追加。
> - `### 10` Decision 开头的 "Opaque static derivatives use JPEG quality 75"：fork 已无
>   衍生图。
> - `apps/docs-website/.../infrastructure/media-attachments.mdx` 第 16 行
>   "image URLs can request resized WebP variants, and thumbnails are cached"：fork 的
>   附件既不发 WebP 变体也不缓存。第 66 行的 "queues durable derivative work"：fork 的
>   视频管线整体停用。**刻意不改这个页面**：fork 从未改过 docs-website（公开文档归
>   upstream 所有，改了就每次 merge 冲突），分歧一律记在本文件的 fork 块里。
>
> **边界**：只影响 room 附件。头像由 Go 用 nativewebp 编成 **lossless WebP**
> （`assets.ProcessAvatarImageWithConfig`，与附件管线刻意不混用）；branding 与链接
> 预览仍走**请求期** ffmpeg transform（`assets.TransformImageWithFFmpeg`，输出**有损**
> WebP）并写 ASSET_CACHE 的 `server.*` 命名空间。注意 `TransformOptions.JPEGQuality`
> 这个名字是上游遗留：fork 的实现把该数值映射成 libwebp 的 `-q:v`，输出是 WebP 不是
> JPEG。
>
> **已知代价（已接受）**：动画 AVIF 可能比源 GIF 大（实测 GIF 278 KB → AVIF 333 KB）；
> HEIC 输入无法转 AVIF，原样存为 `image/heic`，只有支持 HEIC 的浏览器能显示；前端原有的
> HEIC→JPEG 兑底已随「终止前端编码」一起移除。【2026-09-12 实测定论】cloudcone 上
> `ffmpeg -version` 显示 `7.0.2-static (johnvansickle)`，`-decoders` 里只有
> `libdav1d`/`libaom-av1`/`av1` 三个 AV1 解码器，configure 只有
> `--enable-libaom`/`--enable-libdav1d`/`--enable-libwebp`，**没有 heif/heic 解码器**
> ——这条不是推测，是服务器上跑出来的。
>
> **历史**：2026-08-14 首次引入 AVIF 存储（`32e1f566`）；2026-09-02 改为 WebP 存储 +
> 请求期衍生图（`218426d6`）；2026-09-12 回到 AVIF 并彻底取消衍生图。

## Overview

Users can attach files to messages — images, videos, documents — via drag-and-drop, paste, or file picker. Images are dimensioned and resizable on the fly via signed URLs. When video processing is enabled, new videos are transcoded into adaptive HLS streams; animated GIFs and historical processed videos retain the MP4 path.

<!-- 【本地改动 2026-09-12】fork 现状:附件图片上传时重编码为原尺寸 AVIF,请求期不再
     缩放(transform 路由直接回存储字节);视频与动画 GIF 都不进转码管线,原样存、
     原样发。完整说明见本文件顶部的 fork 注记。 -->

## Behavior

- The composer accepts files via drag-and-drop, paste, and a file picker button when the viewer has `message.attach`.
- Draft attachments persist across room switches inside the same session.
- Message attachments are uploaded through `chatto.api.v1.AssetUploadService` before message creation. The browser sends bounded unary chunks with SHA-256 checksums, then calls `MessageService.CreateMessage` with completed attachment asset IDs.
- A completed asset can be attached only by its uploader and to one exact message. Reusing another member's asset ID, or reusing one's own already-attached asset ID, is rejected.
- While a message's attachments are being prepared and uploaded, the bundled composer keeps their previews visible, reports committed upload progress for each file, and disables editing and composer actions until the send finishes. A failed send keeps the submitted text and attachments available for correction or retry.
- Default upload size limits: 25 MB for general files, 100 MB for videos when video processing is enabled. **Fork:** `video.enabled` only controls whether videos can be uploaded and their size limit; the derivative pipeline is never enabled.
- Video uploads require server-side video processing to be enabled. When it is disabled, the composer rejects `video/*` files immediately and the message-post API rejects them before storage. **Fork:** not applicable — videos are stored exactly as uploaded and never transcoded.
- Images are inspected for dimensions at upload time and can be resized at render time via URL parameters (width, height, fit mode). Public attachment and avatar APIs expose transform parameters; public server branding images expose canonical URLs only. **Fork:** the server no longer issues attachment transform URLs at all — the URL builders override the width/height/fit request to the original file URL; the `/image/{w}x{h}/{fit}` routes still accept already-issued links for client compatibility and return the stored bytes untransformed (`X-Cache: BYPASS`); avatars and branding images are still resized at render time.
- The room timeline loads attachment images within 960×400 bounds, while the lightbox loads a separate derivative within 2048×2048 bounds. The uploaded image (re-encoded to AVIF when ffmpeg is available) remains available through Open original and file-download actions. **Fork:** the bounds are not applied at render time — timeline and lightbox load the same stored bytes, the original-size AVIF produced at upload. The stored image is the only copy, so Open original and the download action return those same bytes.

<!-- 【本地改动 32e1f566 + 2026-09-12】fork 把上游的 "The untouched upload remains
     available" 改成 "The uploaded image (re-encoded to AVIF when ffmpeg is available)
     remains available"：fork 的「原图」在 ffmpeg 可用时是 AVIF 重编码产物，原始上传
     字节不会保留。2026-09-12 起又补上「原尺寸、无衍生图」这半句：请求期不再缩放。 -->
- When enabled, videos and animated GIFs are processed by durable `asset-processing` runtime-unit workers. The processing marker commits atomically with the owning message, so a rejected message cannot create work and an accepted message remains queued while workers are offline. Workers may run inside `chatto run` or as separate `chatto asset-processing` processes. **Fork:** disabled — no processing marker is appended and the worker stays idle (see `cmd/run.go`).
- Processing status: durable STARTED / COMPLETED / FAILED outcomes are stored as asset aggregate events (`evt.asset.{assetId}.*`) and delivered through the normal live EVT subscription path after room-membership authorization and the applicable channel-room message-read check for the owning thread. DM membership authorizes DM delivery. There is no separate `video_processed` live event or new runtime KV state for video progress; failed videos keep the original message visible and show a processing-failed state, while the retained original remains available through the attachment's original/download action.
- Processed video dimensions are display dimensions used for layout, not necessarily raw encoded storage pixels. Non-square-pixel and rotated sources should render in their intended orientation and aspect ratio. The room timeline displays every posted video uncropped at its measured aspect ratio, including unusual near-square dimensions and converted animated GIF loops. The player canvas is bounded to the available timeline width and a maximum height; for ratios beyond 9:16 or 16:9, it uses letterboxing so playback controls remain usable without cropping the video.
- A thumbnail is generated from an early video frame using the same display dimensions, so non-square-pixel sources do not persist squished or pillarboxed poster images.
- For newly processed ordinary videos, the public attachment view exposes a signed HLS master-playlist URL. The durable processing manifest stores HLS rendition metadata and no MP4 variant. Existing processed videos are not backfilled; when HLS metadata is absent, the new client continues through their historical MP4 path.
- Opaque static attachment derivatives use JPEG quality 75. Derivatives that require transparency or animation use lossless WebP, and resized results can be held in the auto-expiring server cache. **Fork:** attachment derivatives do not exist; the auto-expiring resize cache is used for server assets only.
- Browser media uses direct signed asset URLs. Relative attachment URLs are resolved against the server that owns the message or room-file item, so remote-server images, audio, and video can load without cross-site cookies or bearer headers. Chatto-streamed NATS objects are full, non-seekable responses; S3-backed passive media redirects to object storage for byte-range delivery.
- Clients refresh expiring attachment URL fields through room-scoped `AssetService.GetAsset` / `BatchGetAssets`, or by refetching the relevant timeline or room attachment-list page. The timeline, previews, lightbox, downloads, and room-files surfaces refresh before expiry and retry after media load errors.
- Active document attachment types such as HTML, XHTML, SVG, and XML can still be uploaded and viewed inline, but original-file responses are delivered in a browser sandbox so uploaded scripts do not run as trusted Chatto application code.
- The room sidebar Files panel lists current accessible attachments from both root messages and thread replies, grouped by date as Today, Yesterday, This week, This month, then older calendar months. Date groups use the same separated, collapsible sidebar-section treatment as room navigation and remember their expanded state per room. Rows show a thumbnail or file-type icon, filename, and upload time; selecting a root-message attachment jumps the room timeline to that message, while selecting a thread-reply attachment opens the thread pane and highlights the reply.
- Each room's Files list starts empty and is loaded only when that panel is first opened. Once loaded, incoming message, edit, deletion, and processing updates keep the cached rows current without reloading the whole list; rooms whose Files panel has never opened make no attachment-list request.
- Deleting a message-owned attachment durably revokes access first, then removes its source/derivative bytes and transform-cache entries. Shared durable-consumer replicas retry failed physical deletion after process restart or replica handover.

## Design Decisions

### 1. Attachment uploads use chunked ConnectRPC sessions

**Decision:** Public message attachment uploads use `AssetUploadService`: `CreateUpload`, `UploadChunk`, `GetUpload`, `CompleteUpload`, and `CancelUpload`. Chunks are bounded unary ConnectRPC requests instead of browser client-streaming RPCs. Each upload declares the final file size and lowercase SHA-256 digest, each chunk carries its offset and chunk SHA-256, and `CompleteUpload` verifies the assembled digest before creating the durable asset. `CreateMessage` accepts only completed, live, room-matching asset IDs uploaded by the caller and atomically attaches each asset exclusively to the message.
**Why:** Attachments should remain inside the protobuf/ConnectRPC API surface instead of introducing a second REST upload endpoint. Unary chunks work with the current browser Connect stack and give resumable progress through the committed offset.
**Tradeoff:** Clients must hash the full file before completion and issue several RPCs for larger files. Temporary chunks and open sessions need cleanup if the browser disappears before completion. Retrying a send after the first attempt actually committed must hydrate the created message rather than attach the same asset to a second message.

### 2. Dual storage backends (NATS ObjectStore + S3)

**Decision:** Attachments can be stored in NATS ObjectStore (default, good for development and small deployments) or in an S3-compatible bucket (production-grade). Each asset records its storage backend and logical key at upload time; S3 deployments may add a configurable object-key prefix that is applied only at the S3 client boundary.
**Why:** Self-hosters running a single binary shouldn't have to spin up S3 just to send a screenshot. Larger operators need durable, replicated object storage. Supporting both lets us serve both ends of the spectrum. See ADR-021.
**Tradeoff:** Migration between backends or S3 prefixes is operator-managed. Stored asset keys remain prefix-free so moving objects between S3 base paths does not require rewriting Chatto metadata. Ordinary NATS-backed files still use complete non-seekable responses, but processed videos use bounded HLS segments and are seekable on either backend.

### 3. Video processing uses an EVT-backed durable worker queue

**Decision:** `AssetProcessingStartedEvent` is both the user-visible PENDING fact and the durable work item. It is appended atomically with the owning message and consumed by one shared JetStream pull consumer across all `asset-processing` runtime-unit replicas. A worker acknowledges only after projecting a terminal succeeded, failed, or deleted state. See ADR-066.
**Why:** Processing is an asynchronous obligation rather than a synchronous service request. Keeping the request in EVT avoids an outbox gap, while a durable consumer supplies distribution, redelivery, backpressure, and offline-worker tolerance without making message posting depend on worker availability.
**Tradeoff:** Delivery is at least once, so workers can repeat external ffmpeg/storage work after an interruption. Terminal-event OCC prevents manifest replacement, but interrupted or losing attempts can still leave unused derivative objects until durable failed-generation cleanup is added. Pre-queue histories receive a bounded startup backfill for messages with no processing marker.

### 4. Animated GIFs go through the video pipeline

**Decision:** When video processing is enabled, animated GIFs are detected at upload and routed to the video transcoder rather than served as raw images. When video processing is disabled, GIFs remain allowed as image uploads.
**Why:** Animated GIF files are typically much larger than equivalent MP4s, and they're inefficient to decode in browsers. Transcoding to MP4 produces smaller, smoother playback.
**Tradeoff:** A static thumbnail is shown until processing finishes, even for GIFs that would have rendered immediately as-is. Worth it for the playback experience and bandwidth savings.

### 5. New processed videos persist HLS segments only

**Decision:** Transcoding temporarily produces H.264 MP4 renditions whose target resolutions are derived from the source display resolution. A 1080p source might yield 720p and 480p; a 480p source skips the higher tiers. Video audio is encoded as AAC stereo so every independently loaded MPEG-TS segment carries a browser-compatible channel layout, including when the upload has quad or another unusual multichannel layout. Encoders force aligned six-second keyframes, then ffmpeg packages each temporary file into MPEG-TS segments without another encode. Only the thumbnail and segments are uploaded. The terminal manifest records rendition dimensions, peak bandwidth, and each segment's asset ID and exact duration. Chatto generates master and media playlists from that manifest on every authorised request. Animated GIF loops continue using one durable MP4 derivative directly.
**Why:** Bounded HLS segments provide duration, seeking, and adaptive rendition switching even when the asset backend can only stream complete objects. Generating small playlists on demand avoids storing duplicate MP4 media or request-specific playlist URLs.
**Tradeoff:** HLS packaging or segment upload failure is a failed processing outcome for a new ordinary video. The processor publishes that terminal outcome with an independent bounded context, then uses a separate bounded context to tombstone and delete partial derivatives. Cleanup interrupted before every tombstone is written can leave unused storage orphaned until durable failed-generation cleanup replaces this best-effort path. A success append with an ambiguous result is checked by exact event ID. If confirmation also fails, Chatto retains the output rather than risk deleting assets referenced by a committed manifest, which can also leave orphaned storage if the success did not commit. An uncommitted derivative creation has no canonical storage fact and likewise relies on bounded prompt compensation. A processing attempt that exceeds its fixed 30-minute worker budget becomes a terminal failure rather than repeating the same expensive work forever; worker shutdown remains retryable. Historical processed videos are not backfilled; their manifests omit HLS and the new client continues using their MP4 variants. Older clients cannot play new HLS-only manifests, which is an accepted pre-1.0 compatibility break. During a server rollout, every replica behind one endpoint must understand HLS before new HLS-only videos are created: an older replica cannot expose the HLS metadata or serve playlist and segment routes. An older replica can also ignore HLS child IDs while deleting a source; the durable asset-cleanup consumer on a newer replica re-reads the source manifest and tombstones any still-live HLS segments. Videos whose meaningful content occupies only part of the encoded canvas retain that empty space because the player does not guess which parts are safe to crop.

### 6. Attachments are declared content; derivative manifests are durable events

**Decision:** `AssetCreatedEvent` records each uploaded or generated binary as a first-class `Asset` on `evt.asset.{assetId}.asset_created`. `Asset` carries inline storage and flat media metadata such as dimensions, duration, and bitrate; room scope, uploader, and derivative context live on `AssetCreatedEvent`. Uploaded assets awaiting attachment also record SHA-256, expiry, and video-processing hints. Message posting atomically appends an `AssetAttachedEvent` for every attachment and an `AssetProcessingStartedEvent` for newly uploaded video/animated-GIF assets. Durable runtime-unit workers consume the processing facts. After transcoding succeeds, the original upload is retained as source content, and generated thumbnails and HLS segments—or the MP4 derivative for an animated GIF—are appended as derivative `AssetCreatedEvent`s whose owner points at the original asset. Durable failed/unavailable outcomes are recorded with `AssetProcessingFailedEvent.failure_code` and are mapped to stable client-facing failure reasons. Beta histories remain readable through the asset projection's legacy subscription lanes, with the first surviving message-body reference authored by the recorded uploader treated as owner.
**Why:** Attachments and video derivatives are content metadata, not runtime state. Making assets their own aggregates gives projections a single asset graph (`message -> original asset -> derivative assets`), keeps binary lifecycle facts out of the room aggregate, and lets future uploads exist outside messages without a parallel asset model. Keeping the original allows future re-encoding, and storing processing outcomes in EVT lets processed playback survive projection rebuilds and storage-boundary cleanup.
**Tradeoff:** Retaining originals costs more storage than the old replace-after-transcode behavior. Processing execution is at least once: the durable Started fact survives crashes, but ffmpeg and storage work may repeat before one terminal event wins. Every write-serving replica must be upgraded together for exclusive attachment to be a security boundary; an older replica neither enforces nor understands `AssetAttachedEvent` and can reopen attachment aliasing during a mixed-version rollout. Moving new writes from room aggregates to asset aggregates means older beta binaries must not be rolled back after new asset-subject writes have occurred; compatibility is maintained by this and later versions reading both subject shapes, not by rewriting history.

### 7. Attachment URLs are per-user signed capabilities

**Decision:** Public attachment APIs expose attachment media as stable asset paths plus per-user access tickets: `/assets/files/{assetId}?access={ticket}` for originals and `/assets/files/{assetId}/image/{width}x{height}/{fit}?access={ticket}` for image derivatives. HLS uses a domain-separated, source-video-scoped ticket on `/assets/hls/{assetId}/master.m3u8`; Chatto generates each playlist with authorised child routes and checks every requested segment against the durable derivative manifest. Attachment, thumbnail, video thumbnail, HLS master, and historical variant URLs expose the ticket expiry so the client can refresh before or after a lazy-load miss. Every fetch verifies that the signed user is still a room member and has broad `message.read`, or `message.read-interactions` with a relationship to the asset's owning thread. DM membership authorizes DM reads.
**Why:** Cross-origin `<img>` tags (used when the SPA loads attachments from a _remote_ registered server) can't carry session cookies (SameSite) or Authorization headers. A signed per-user access ticket lets browsers load remote attachments directly, while current membership and applicable permission checks revoke access after an access boundary changes.
**Tradeoff:** The access ticket is a bearer capability — anyone holding it can fetch until the expiry passes, the signed user loses room membership, or the signed user loses applicable channel-room message-read authority. Tickets use hourly issuance buckets and retain **23–24 hours** of validity, so repeated reads within a bucket return the same URL while normal rendering, lazy loading, deferred media startup, and lightbox use remain reliable across long-lived room views. Timeline, preview, and room-files clients use the exposed expiry to refresh shortly before it; the lightbox refreshes after 22 hours to preserve roughly an hour of margin at minimum ticket validity. Media load errors also trigger refreshes. Protected asset responses use `private, no-store`, so browser-visible protected bytes are not reused as authorization state. HLS segments stream through Chatto on both backends, while short-lived S3 redirects remain reserved for heavy passive originals such as video, audio, and large files. Rotating `[core.assets].signing_secret` invalidates all outstanding access tickets.

### 8. Active document attachments render in a browser sandbox

**Decision:** Original attachment responses for active document formats (HTML, XHTML, SVG, XML, and XML-derived media types) include a CSP sandbox and `nosniff`. S3-backed attachments of those types stream through Chatto instead of redirecting directly to a presigned object URL, so the same response policy applies.
**Why:** Some teams need to share these file types inline, but uploaded active content must not become trusted Chatto application code. A sandbox without same-origin privileges preserves the viewing use case while preventing the easiest same-origin stored-XSS path.
**Tradeoff:** Scripts, forms, top-level navigation, and same-origin APIs are restricted inside uploaded active documents. S3 deployments also lose the zero-copy redirect fast path for those active document types, while heavy passive originals can still use a short-lived S3 redirect after Chatto authorizes the request.

### 9. Room Files panel is a read projection, not durable attachment state

**Decision:** `Room.attachments` exposes a paginated list of current message attachments for a room. The read walks the visible room timeline projection, folds current message bodies, includes thread replies, preserves attachment order within each message, and sorts by newest message first. The bundled client owns one lazy file cache per room in its server-scoped state: opening Files hydrates it once, after which authoritative timeline message snapshots reconcile attachment rows already in the cache and newly posted attachments are inserted directly.
**Why:** Files should disappear from the sidebar when their message body is retracted or the attachment is removed. Deriving the server read from the existing room/message projections and updating the client cache from the same realtime message snapshots keeps both surfaces consistent without duplicate durable state or repeated full-list reads.
**Tradeoff:** There is no search or media filtering in this iteration. Hydrated room caches consume client memory for the server session, and attachment changes beyond a partially loaded page converge when that page is loaded.

### 10. Displayed images use bounded derivatives

<!-- 【本地改动 32e1f566 + 2026-09-12】fork 重写了下方 Decision / Why / Tradeoff
     三行。2026-08-14 首次补记上传时重编码为 AVIF；2026-09-02 ~ 2026-09-12 期间
     正常路径输出有损 WebP 衍生图；2026-09-12 起改为「上传即原尺寸 AVIF、请求期
     无衍生图」。合并 upstream 时这三行会被上游版本覆盖，需人工恢复。

     已知不一致（未改）：Decision 开头的 "Opaque static derivatives use JPEG quality
     75" 在 fork 里完全不成立，属 upstream 正文，留给人决定。 -->

**Decision:** Timeline images fit within 960×400 bounds and lightbox images fit within 2048×2048 bounds. Opaque static derivatives use JPEG quality 75, while transparency and animation continue to use lossless WebP. Newly uploaded image attachments are re-encoded to original-size AVIF through ffmpeg (the binary the video pipeline already requires) at CRF 30 using the fastest available encoder (`libsvtav1`, falling back to `libaom-av1`); static inputs produce static AVIF and animated inputs produce animated AVIF, both at source dimensions. When ffmpeg or an AV1 encoder is unavailable, or when encoding fails, uploads are stored unchanged. **Fork:** the render-time bounds above are not applied to attachments — the URL builders return the original file URL and discard the requested dimensions, and the `/image/{width}x{height}/{fit}` routes accept only already-issued links and return the stored bytes verbatim (`X-Cache: BYPASS`) without reading or writing the resize cache, so each attachment image is stored and served exactly once.
**Why:** Timeline frames are much smaller than typical camera and screenshot uploads, and even full-screen viewing rarely benefits from transferring the source resolution. Separate display sizes reduce bandwidth without sacrificing the original file-sharing behavior. AVIF compresses camera and screenshot uploads better than the original JPEG/PNG bytes while remaining universally supported in modern browsers. **Fork rationale:** encoding once at upload time makes the stored file the only copy, which removes a whole failure class from the request path — resizing an immutable container from a non-seekable stream fails (`partial file` while probing ISO-BMFF) and the bad result is then frozen in cache, which was the dominant cause of attachment 500s in this deployment. Original-size storage is the price.
**Tradeoff:** Opaque displayed images are lossy and capped in resolution. Transparent and animated images may see smaller savings because preserving their behavior requires lossless encoding. Upload-time re-encoding adds latency proportional to image size and depends on ffmpeg availability; deployments without ffmpeg fall back to storing original bytes, and mixed-version deployments may serve both formats. Stored content type follows the encoded format (`image/avif` or the original type). **Fork tradeoffs:** attachments stream full-resolution bytes in the timeline, so bandwidth grows as users upload large images; animated AVIF can be larger than the source GIF (measured 278 KB GIF → 333 KB AVIF); HEIC inputs cannot be encoded because the server ffmpeg has no `heif` decoder, so they are stored as `image/heic` and display only in HEIC-capable browsers, with the previous client-side HEIC→JPEG fallback removed along with client-side encoding in general.

### 11. Message-owned asset deletion is replayable

**Decision:** A message or attachment delete may tombstone an asset and touch its backing objects only when `AssetAttachedEvent` names that exact room and message. Historical duplicate references are removed from the aliasing message without deleting the canonical owner's asset. Request paths still attempt immediate NATS/S3 and transform-cache deletion, while shared `chatto-asset-cleanup-v1` durable-consumer replicas process canonical `AssetDeletedEvent` facts and retry each idempotent cleanup independently. The asset ID locates the same aggregate's durable `AssetCreatedEvent`, which supplies storage metadata even after the in-memory projection drops it. Beta room-scoped histories without a canonical asset creation aggregate are skipped rather than probing guessed object keys.
**Why:** A committed deletion must remain recoverable when immediate storage cleanup fails, the process exits, or another replica committed the event. Resolving the immutable creation fact preserves that guarantee without duplicating storage metadata in the deletion event or depending on a mutable projection.
**Tradeoff:** Each cleanup requires an aggregate-history lookup, and a fresh worker replays prior deletion facts idempotently. Beta room-scoped events cannot gain the same guarantee without a migration or unsafe backend-key inference, and server branding/avatar cleanup remains outside this message-owned worker.

### 12. Durable worker health is shared and owner-visible

**Decision:** Owner-only admin diagnostics derive asset cleanup and the other known durable-worker queue states directly from JetStream. The System tab reports inactive, healthy, working, unconfirmed, stalled, or unavailable state plus queue depth, ack-pending deliveries, unresolved redeliveries, and delivery progress. It does not expose asset IDs, filenames, storage keys, raw errors, or a reclaimed-byte estimate.
**Why:** Automatic retry is only operationally useful when self-hosters can tell whether the worker is available and whether durable work remains. Shared consumer state makes that answer consistent regardless of which replica serves the admin request.
**Tradeoff:** Broker state is point-in-time. Waiting pulls demonstrate availability, but an ack-pending delivery without a waiter may be actively handled or awaiting recovery after a crash, so diagnostics report that state as unconfirmed. The unresolved-redelivery count resets as messages are acknowledged and does not identify which current item retried; the consumer also does not expose the age of its oldest pending delivery. Counts describe queued and unacknowledged work, not historical deletion totals, and idempotent cleanup cannot reliably attribute reclaimed bytes.

## Permissions

Posting an attachment requires room membership, the relevant message-posting permission (`message.post` or `message.post-in-thread`), and `message.attach`. The `message.attach` permission is configurable at server, group, and room scope and only gates message attachments; server branding uploads, user avatars, link previews, and attachment deletion use their existing checks.

Reading attachment metadata or bytes requires room membership. Channel-room
reads also require broad `message.read`, or `message.read-interactions` with a
relationship to the asset's owning thread. This check applies again when a
client uses an existing signed or ticketed asset URL. DM membership authorizes
DM reads.

Fresh servers seed `message.attach` for `everyone` so new deployments keep uploads enabled by default. Existing servers are not automatically backfilled after upgrade; operators should grant `message.attach` manually or through their chosen RBAC maintenance flow if existing rooms should keep allowing uploads.

## Related

- **ADRs:** ADR-021 (dual asset storage), ADR-023 (HMAC-signed image transform URLs), ADR-032 (self-describing signed attachment URLs), ADR-036 (runtime state in `RUNTIME_STATE`), ADR-041 (runtime units for optional processes), ADR-045 (public API stability tiers), ADR-047 (direct ticketed asset URLs), ADR-066 (durable asset processing runtime unit), ADR-067 (Electron desktop packaging), ADR-069 (explicit durable consumer lifecycle), ADR-080 (explicit message-read permissions), ADR-082 (derived thread interactions)
- **FDRs:** FDR-002 (Replies & Threads), FDR-004 (Message Editing & Deletion), FDR-034 (Chatto Desktop), FDR-039 (Message Access & Interactions)
