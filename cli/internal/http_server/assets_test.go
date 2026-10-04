package http_server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/charmbracelet/log"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/types/known/timestamppb"
	"hmans.de/chatto/internal/config"
	"hmans.de/chatto/internal/connectapi"
	"hmans.de/chatto/internal/core"
	"hmans.de/chatto/internal/core/linkpreview"
	"hmans.de/chatto/internal/email"
	"hmans.de/chatto/internal/evtstream"
	apiv1 "hmans.de/chatto/internal/pb/chatto/api/v1"
	"hmans.de/chatto/internal/pb/chatto/api/v1/apiv1connect"
	evtv1 "hmans.de/chatto/internal/pb/chatto/core/evt/v1"
	"hmans.de/chatto/internal/testutil"
	"hmans.de/chatto/internal/testutil/fakes3"
	"hmans.de/chatto/pkg/signedurl"
)

// ============================================================================
// Asset Test Helpers
// ============================================================================

// assetTestEnv holds all test dependencies for asset tests
type assetTestEnv struct {
	server   *httptest.Server
	client   *http.Client
	core     *core.ChattoCore
	ctx      context.Context
	js       jetstream.JetStream
	previews *linkpreview.Cache
}

// legacyTransformURL builds the pre-2026-09-12 derivative URL for a stored
// attachment: /assets/files/{assetID}/image/960x400/contain, carrying an access
// ticket signed for those transform parameters.
//
// 【本地改动 2026-09-12】fork 的 URL 生成层已不再产出这种链接，但已经发出去的旧链接
// （旧客户端缓存、CDN、被粘贴到别处的 URL）仍然会打到这里，必须仍然可用。
//
// 【本地改动 2026-09-13】ticket 的签名绑定变换参数（signedurl.MatchesTransform），
// 原图 URL 上的 ticket 签名里没有 width/height/fit，直接复用会被
// resolveStableAssetViewerID 判为「ticket does not match derivative」而 403。
// 故此处重新签一张匹配 960x400/contain 的 ticket，模拟旧客户端当时拿到的形态。
func legacyTransformURL(t *testing.T, attachmentID, userID, originalURL string) string {
	t.Helper()
	ticket, err := signedurl.SignedAssetAccessTicket("test-signing-secret-32-bytes-!!", signedurl.AssetAccessTicket{
		AssetID:   attachmentID,
		UserID:    userID,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Width:     960,
		Height:    400,
		Fit:       "contain",
	})
	if err != nil {
		t.Fatalf("Failed to sign legacy transform ticket: %v", err)
	}
	base, query, _ := strings.Cut(originalURL, "?")
	_ = query
	slash := strings.LastIndex(base, "/")
	if slash < 0 {
		return originalURL
	}
	return base[:slash] + "/image/960x400/contain" + base[slash:] + "?access=" + ticket
}

// setupAssetTestServer creates a test server for asset testing with caching enabled.
func setupAssetTestServer(t *testing.T) *assetTestEnv {
	return setupAssetTestServerWithConfig(t, false)
}

// setupAssetTestServerWithS3 mirrors setupAssetTestServer but routes
// attachments through an in-memory fake S3 server. Use this to test the
// S3 presigned-redirect code path in the asset handlers (the path that
// previously contained an authorization bypass on empty room ID).
func setupAssetTestServerWithS3(t *testing.T) *assetTestEnv {
	return setupAssetTestServerWithConfig(t, true)
}

func setupAssetTestServerWithS3AndVideo(t *testing.T) *assetTestEnv {
	return setupAssetTestServerWithOptions(t, true, true)
}

func setupAssetTestServerWithConfig(t *testing.T, useS3 bool) *assetTestEnv {
	return setupAssetTestServerWithOptions(t, useS3, false)
}

func setupAssetTestServerWithOptions(t *testing.T, useS3 bool, videoEnabled bool) *assetTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	_, nc := testutil.StartSharedNATS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	assetsCfg := config.AssetsConfig{
		SigningSecret: "test-signing-secret-32-bytes-!!",
		MaxUploadSize: 10 * 1024 * 1024, // 10MB
		Cache: config.AssetsCacheConfig{
			Enabled: true,
			TTL:     config.Duration(7 * 24 * time.Hour), // 7 days
		},
	}
	if useS3 {
		s3Server := fakes3.NewServer(t)

		useSSL := false
		pathStyle := true
		assetsCfg.StorageBackend = config.StorageBackendS3
		assetsCfg.S3 = config.S3Config{
			Endpoint:        s3Server.EndpointHost(),
			Bucket:          "test-bucket",
			AccessKeyID:     "test-key",
			SecretAccessKey: "test-secret",
			UseSSL:          &useSSL,
			PathStyle:       &pathStyle,
		}
	}
	coreConfig := config.CoreConfig{
		Assets: assetsCfg,
	}
	chattoCore, err := core.NewChattoCore(ctx, nc, coreConfig)
	if err != nil {
		t.Fatalf("Failed to create ChattoCore: %v", err)
	}
	startCoreServices(t, chattoCore)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("Create JetStream test client: %v", err)
	}
	runtimeState, err := js.KeyValue(ctx, "RUNTIME_STATE")
	if err != nil {
		t.Fatalf("Open RUNTIME_STATE test cache: %v", err)
	}

	// Create router with session middleware
	router := gin.New()
	router.Use(gin.Recovery())

	sessionStore := cookie.NewStore([]byte("test-secret-key-32-bytes-long!!"))
	sessionStore.Options(sessions.Options{
		MaxAge:   60 * 60 * 24 * 90,
		HttpOnly: true,
		Secure:   false,
		Path:     "/",
	})
	router.Use(sessions.Sessions("chatto_session", sessionStore))

	// Create HTTPServer
	s := &HTTPServer{
		config: config.ChattoConfig{
			Auth: config.AuthConfig{},
			Webserver: config.WebserverConfig{
				URL:                 "http://localhost:4000",
				CookieSigningSecret: "test-secret-key-32-bytes-long!!",
			},
			Core: coreConfig,
			Video: config.VideoConfig{
				Enabled: videoEnabled,
			},
		},
		nc:     nc,
		router: router,
		core:   chattoCore,
		mailer: email.NewMockSender(true),
		logger: log.WithPrefix("test"),
	}

	s.setupAuthRoutes()
	s.setupConnectAPI()
	s.setupAssetRoutes()

	ts := httptest.NewServer(router)
	t.Cleanup(func() { ts.Close() })

	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &assetTestEnv{
		server:   ts,
		client:   client,
		core:     chattoCore,
		ctx:      ctx,
		js:       js,
		previews: linkpreview.NewCache(runtimeState),
	}
}

// url returns the test server URL for an asset path or an API-issued asset
// URL. API responses carry absolute URLs on the configured public origin, which
// differs from the httptest listener address.
func (env *assetTestEnv) url(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() {
		return env.server.URL + raw
	}
	return env.server.URL + parsed.RequestURI()
}

// login authenticates a user
func (env *assetTestEnv) login(t *testing.T, login, password string) {
	t.Helper()

	loginBody := fmt.Sprintf(`{"login":"%s","password":"%s"}`, login, password)
	req, err := http.NewRequest(http.MethodPost, env.url("/auth/browser/login"), bytes.NewReader([]byte(loginBody)))
	if err != nil {
		t.Fatalf("Create login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(connectapi.BrowserAuthenticationModeHeader, connectapi.BrowserAuthenticationModeCookie)
	req.Header.Set("Origin", env.server.URL)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatalf("Failed to login: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Login failed with status %d", resp.StatusCode)
	}
}

// createAssetTestPNG creates a simple PNG image for testing
func createAssetTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	// Fill with a test color
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{R: 100, G: 150, B: 200, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("Failed to encode PNG: %v", err)
	}
	return buf.Bytes()
}

func markPublicServerAssetForTest(t *testing.T, env *assetTestEnv, assetID string) {
	t.Helper()
	store := env.core.ServerStore()
	info, err := store.GetInfo(env.ctx, assetID)
	if err != nil {
		t.Fatalf("GetInfo public marker fixture: %v", err)
	}
	headers := maps.Clone(info.Headers)
	if headers == nil {
		headers = make(map[string][]string)
	}
	headers.Set(core.ServerAssetVisibilityHeader, core.ServerAssetVisibilityPublic)
	headers.Set(core.ServerAssetVisibilityNUIDHeader, info.NUID)
	headers.Set(core.ServerAssetVisibilityDigestHeader, info.Digest)
	if err := store.UpdateMeta(env.ctx, assetID, jetstream.ObjectMeta{
		Name:        assetID,
		Description: info.Description,
		Headers:     headers,
		Metadata:    maps.Clone(info.Metadata),
	}); err != nil {
		t.Fatalf("UpdateMeta public marker fixture: %v", err)
	}
}

func appendRoomTimelineAssetTestEvent(t *testing.T, env *assetTestEnv, roomID string, event *evtv1.Event) {
	t.Helper()
	event.Id = core.NewEventID()
	event.ActorId = core.SystemActorID
	event.CreatedAt = timestamppb.Now()
	if _, err := env.core.EventPublisher.AppendEventually(
		env.ctx, evtstream.RoomAggregate(roomID).SubjectFor(event), event,
	); err != nil {
		t.Fatalf("append room timeline fixture: %v", err)
	}
	if err := env.core.WaitForProjectionsCurrent(env.ctx); err != nil {
		t.Fatalf("wait for room timeline fixture projections: %v", err)
	}
}

func appendAssetProjectionTestEvent(t *testing.T, env *assetTestEnv, assetID string, event *evtv1.Event) {
	t.Helper()
	event.Id = core.NewEventID()
	event.ActorId = core.SystemActorID
	event.CreatedAt = timestamppb.Now()
	if _, err := env.core.EventPublisher.AppendEventually(
		env.ctx, evtstream.AssetAggregate(assetID).SubjectFor(event), event,
	); err != nil {
		t.Fatalf("append asset fixture: %v", err)
	}
	if err := env.core.WaitForProjectionsCurrent(env.ctx); err != nil {
		t.Fatalf("wait for asset fixture projections: %v", err)
	}
}

func (env *assetTestEnv) postAssetMessageWithAttachment(t *testing.T, roomID, body string, fileData []byte, fileName string) (string, *apiv1.MessageAttachment) {
	t.Helper()
	return env.postAssetMessageWithAttachmentContentType(t, roomID, body, fileData, fileName, "image/png")
}

func (env *assetTestEnv) postAssetMessageWithAttachmentContentType(t *testing.T, roomID, body string, fileData []byte, fileName, contentType string) (string, *apiv1.MessageAttachment) {
	t.Helper()

	assetUploadClient := apiv1connect.NewAssetUploadServiceClient(env.client, env.server.URL+connectAPIPrefix)
	sum := sha256.Sum256(fileData)
	created, err := assetUploadClient.CreateUpload(env.ctx, connect.NewRequest(&apiv1.CreateUploadRequest{
		RoomId:      roomID,
		Filename:    fileName,
		ContentType: contentType,
		Size:        int64(len(fileData)),
		Sha256:      hex.EncodeToString(sum[:]),
	}))
	if err != nil {
		t.Fatalf("Failed to create asset upload: %v", err)
	}
	chunkSum := sha256.Sum256(fileData)
	if _, err := assetUploadClient.UploadChunk(env.ctx, connect.NewRequest(&apiv1.UploadChunkRequest{
		UploadId:    created.Msg.GetUpload().GetUploadId(),
		Content:     fileData,
		ChunkSha256: hex.EncodeToString(chunkSum[:]),
	})); err != nil {
		t.Fatalf("Failed to upload asset chunk: %v", err)
	}
	completed, err := assetUploadClient.CompleteUpload(env.ctx, connect.NewRequest(&apiv1.CompleteUploadRequest{
		UploadId: created.Msg.GetUpload().GetUploadId(),
	}))
	if err != nil {
		t.Fatalf("Failed to complete asset upload: %v", err)
	}
	assetID := completed.Msg.GetAsset().GetId()
	if assetID == "" {
		t.Fatal("Completed asset upload returned empty asset id")
	}

	client := apiv1connect.NewMessageServiceClient(env.client, env.server.URL+connectAPIPrefix)
	req := connect.NewRequest(&apiv1.CreateMessageRequest{
		RoomId:             roomID,
		Body:               body,
		AttachmentAssetIds: []string{assetID},
	})
	resp, err := client.CreateMessage(env.ctx, req)
	if err != nil {
		t.Fatalf("Failed to post message with attachment: %v", err)
	}
	message := resp.Msg.GetMessage()
	if message == nil {
		t.Fatal("Expected posted message")
	}
	if len(message.GetAttachments()) == 0 {
		t.Fatal("Expected at least one attachment")
	}
	return message.GetId(), message.GetAttachments()[0]
}

func (env *assetTestEnv) deleteAssetMessage(t *testing.T, roomID, eventID string) {
	t.Helper()

	client := apiv1connect.NewMessageServiceClient(env.client, env.server.URL+connectAPIPrefix)
	req := connect.NewRequest(&apiv1.DeleteMessageRequest{
		RoomId:  roomID,
		EventId: eventID,
	})
	if _, err := client.DeleteMessage(env.ctx, req); err != nil {
		t.Fatalf("Failed to delete message: %v", err)
	}
}

// ============================================================================
// Asset Caching Tests
// ============================================================================

// TestAsset_TransformedImage_CacheHitMiss verifies the cache behaviour of
// attachment delivery under the fork's no-derivative policy.
//
// 【本地改动 2026-09-12/13】跟随 0b1610267（no derivatives）与 5ee7a0fb9
// （override attachment transform URLs to the original URL）：附件不再产生请求期
// 衍生图，GetThumbnailAssetUrl 与 GetAssetUrl 是同一个原图 URL。原图直出不经 resize
// 缓存，所以第二次请求拿不到 X-Cache: HIT——上游此测试断言 HIT，断言方向与 fork 语义相反。
//
// 本测试改为守护 fork 的实际回归面：
//
//	① 缩略图 URL 不带 /image/ 变换段，且与原图 URL 相同；
//	② 旧版本已经发出去的 /image/ 衍生图 URL 仍然可用，返回存储的原图字节并标 X-Cache: BYPASS。
func TestAsset_TransformedImage_CacheHitMiss(t *testing.T) {
	env := setupAssetTestServer(t)

	// Create user and space with room
	user, err := env.core.CreateUser(env.ctx, "system", "cacheuser", "Cache User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	if err != nil {
		t.Fatalf("Failed to create space: %v", err)
	}

	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "testroom", "Test Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}

	// Join room
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}

	// Login
	env.login(t, "cacheuser", "password123")

	// Upload an attachment via postMessage mutation
	imageData := createAssetTestPNG(t, 800, 600)
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "Test message with image", imageData, "test-image.png")
	thumbnailURL := attachment.GetThumbnailAssetUrl().GetUrl()
	if thumbnailURL == "" {
		t.Fatal("Expected thumbnail asset URL")
	}

	// 【本地改动 2026-09-13】fork 取消衍生图：缩略图 URL 就是原图 URL，且不带变换段。
	assetURL := attachment.GetAssetUrl().GetUrl()
	if thumbnailURL != assetURL {
		t.Fatalf("thumbnail URL = %q, want the original asset URL %q", thumbnailURL, assetURL)
	}
	if strings.Contains(thumbnailURL, "/image/") {
		t.Fatalf("thumbnail URL = %q, fork issues no attachment transform URL", thumbnailURL)
	}

	// 原图直出必须 200，且回的就是存储的原始字节。
	originalResp, err := env.client.Get(env.url(thumbnailURL))
	if err != nil {
		t.Fatalf("Failed to get original image: %v", err)
	}
	original, err := io.ReadAll(originalResp.Body)
	originalResp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to read original image: %v", err)
	}
	if !bytes.Equal(original, imageData) {
		t.Fatal("original URL must serve the stored original bytes")
	}

	// 旧版本发出去的 /image/ 衍生图 URL 仍须可用：返回原图字节并标 X-Cache: BYPASS。
	legacyURL := legacyTransformURL(t, attachment.GetId(), user.Id, assetURL)
	legacyResp, err := env.client.Get(env.url(legacyURL))
	if err != nil {
		t.Fatalf("Failed to get legacy transform URL: %v", err)
	}
	legacy, err := io.ReadAll(legacyResp.Body)
	legacyResp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to read legacy transform URL: %v", err)
	}
	if legacyResp.StatusCode != http.StatusOK {
		t.Fatalf("legacy transform URL status = %d, want %d", legacyResp.StatusCode, http.StatusOK)
	}
	if got := legacyResp.Header.Get("X-Cache"); got != "BYPASS" {
		t.Fatalf("legacy transform URL: X-Cache = %q, want BYPASS", got)
	}
	if !bytes.Equal(legacy, imageData) {
		t.Fatal("legacy transform URL must serve the stored original bytes")
	}
}

// TestAsset_TransformedAttachmentUsesCompressedProfileAndVersionedCache guards
// attachment delivery under the fork's no-derivative policy.
//
// 【本地改动 2026-09-12/13】跟随 0b1610267（no derivatives）与 5ee7a0fb9（override
// attachment transform URLs to the original URL）。上游此测试断言三件在 fork 里
// 不成立的事：缩略图 URL 带 /960x400/contain 变换段、响应字节等于
// AttachmentDerivativeJPEGQuality 压缩后的变换结果、resize 缓存 key 带
// attachment-stable-v2 版本前缀。fork 不在请求期做变换，所以三者都不该发生。
//
// 改为守护 fork 的实际回归面：
//
//	① 缩略图 URL 与原图 URL 相同且不带 /image/ 段；
//	② 原图 URL 返回存储的原始字节（未压缩、未缩放）；
//	③ 旧 /image/ 衍生图 URL 仍可用，返回原图字节并标 X-Cache: BYPASS；
//	④ 旧缓存 key 里预埋的条目不会被原图路径命中（无派生则无 resize 缓存写入）。
func TestAsset_TransformedAttachmentUsesCompressedProfileAndVersionedCache(t *testing.T) {
	env := setupAssetTestServer(t)

	user, err := env.core.CreateUser(env.ctx, "system", "compressedimageuser", "Compressed Image User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "compressed-images", "Compressed Images")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}
	env.login(t, "compressedimageuser", "password123")

	imageData := createAssetTestPNG(t, 1200, 800)
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "compressed image", imageData, "compressed.png")
	thumbnailURL := attachment.GetThumbnailAssetUrl().GetUrl()

	// ① fork 取消衍生图：缩略图 URL 就是原图 URL，不带变换段。
	assetURL := attachment.GetAssetUrl().GetUrl()
	if thumbnailURL != assetURL {
		t.Fatalf("thumbnail URL = %q, want the original asset URL %q", thumbnailURL, assetURL)
	}
	if strings.Contains(thumbnailURL, "/image/") {
		t.Fatalf("thumbnail URL = %q, fork issues no attachment transform URL", thumbnailURL)
	}

	// 预埋一条旧命名空间的 resize 缓存，验证原图路径不会命中它。
	oldCacheKey := core.ImageCacheKey("attachment-stable", attachment.GetId(), 960, 400, "contain")
	if err := env.core.StoreCachedResize(env.ctx, oldCacheKey, []byte("old-quality-cache-entry")); err != nil {
		t.Fatalf("Failed to seed old attachment cache namespace: %v", err)
	}

	// ② 原图 URL 返回存储的原始字节——未缩放、未按派生 profile 压缩。
	resp, err := env.client.Get(env.url(thumbnailURL))
	if err != nil {
		t.Fatalf("Failed to get original attachment: %v", err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to read original attachment: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("original attachment status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if !bytes.Equal(got, imageData) {
		t.Fatal("original URL must serve the stored original bytes, not a derivative")
	}

	// ③ 旧 /image/ 衍生图 URL 仍须可用：返回原图字节并标 X-Cache: BYPASS。
	legacyURL := legacyTransformURL(t, attachment.GetId(), user.Id, assetURL)
	legacyResp, err := env.client.Get(env.url(legacyURL))
	if err != nil {
		t.Fatalf("Failed to get legacy transform URL: %v", err)
	}
	legacy, err := io.ReadAll(legacyResp.Body)
	legacyResp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to read legacy transform URL: %v", err)
	}
	if legacyResp.StatusCode != http.StatusOK {
		t.Fatalf("legacy transform URL status = %d, want %d", legacyResp.StatusCode, http.StatusOK)
	}
	if got := legacyResp.Header.Get("X-Cache"); got != "BYPASS" {
		t.Fatalf("legacy transform URL: X-Cache = %q, want BYPASS", got)
	}
	if !bytes.Equal(legacy, imageData) {
		t.Fatal("legacy transform URL must serve the stored original bytes")
	}

	// ④ 附件不再产生衍生图，resize 缓存必须保持为空（预埋条目不被消费也不被写出）。
	cacheKey := core.ImageCacheKey(AttachmentStableCachePrefix, attachment.GetId(), 960, 400, "contain")
	if cached, err := env.core.GetCachedResize(env.ctx, cacheKey); err != nil {
		t.Fatalf("Failed to read attachment resize cache: %v", err)
	} else if len(cached) != 0 {
		t.Fatalf("attachment resize cache = %d bytes, want empty: fork issues no derivatives", len(cached))
	}
}

func TestAsset_DeleteAttachment_CleansUpCache(t *testing.T) {
	env := setupAssetTestServer(t)

	// Create user and space with room
	user, err := env.core.CreateUser(env.ctx, "system", "cleanupuser", "Cleanup User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	if err != nil {
		t.Fatalf("Failed to create space: %v", err)
	}

	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "testroom", "Test Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}

	// Join room
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}

	// Login
	env.login(t, "cleanupuser", "password123")

	// Upload an attachment
	imageData := createAssetTestPNG(t, 800, 600)
	eventID, attachment := env.postAssetMessageWithAttachment(t, room.Id, "Test message for cleanup", imageData, "cleanup-test.png")
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	thumbnailURL := attachment.GetThumbnailAssetUrl().GetUrl()
	if attachmentURL == "" || thumbnailURL == "" {
		t.Fatal("Expected original and thumbnail asset URLs")
	}

	// Request transformed image to populate cache
	transformResp, err := env.client.Get(env.url(thumbnailURL))
	if err != nil {
		t.Fatalf("Failed to get transformed image: %v", err)
	}
	transformResp.Body.Close()
	if transformResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", transformResp.StatusCode)
	}

	// Wait for async cache store
	time.Sleep(100 * time.Millisecond)

	// 【本地改动 2026-09-12/13】fork 取消附件衍生图：缩略图 URL 就是原图 URL，
	// 不经 resize 缓存，所以第二次请求不再是 X-Cache: HIT。改为直接校验原图
	// 字节可重复读取，然后沿用本测试真正的回归面——删除后两个 URL 都必须 404。
	replayResp, err := env.client.Get(env.url(thumbnailURL))
	if err != nil {
		t.Fatalf("Failed to re-get the original image: %v", err)
	}
	replay, err := io.ReadAll(replayResp.Body)
	replayResp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to read the original image: %v", err)
	}
	if !bytes.Equal(replay, imageData) {
		t.Fatal("thumbnail URL must serve the stored original bytes")
	}

	// Delete the message (which should delete the attachment and its cache)
	env.deleteAssetMessage(t, room.Id, eventID)

	// Original attachment URL should now return 404
	originalResp, err := env.client.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to get original attachment: %v", err)
	}
	originalResp.Body.Close()
	if originalResp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected 404 for deleted attachment, got %d", originalResp.StatusCode)
	}

	// Transformed URL should also return 404 (not cache hit from stale cache)
	transformResp3, err := env.client.Get(env.url(thumbnailURL))
	if err != nil {
		t.Fatalf("Failed to get transformed image: %v", err)
	}
	transformResp3.Body.Close()
	if transformResp3.StatusCode != http.StatusNotFound {
		t.Errorf("Expected 404 for deleted attachment transform, got %d", transformResp3.StatusCode)
	}
}

func TestAsset_OriginalAttachment_ServesCorrectly(t *testing.T) {
	env := setupAssetTestServer(t)

	// Create user and space with room
	user, err := env.core.CreateUser(env.ctx, "system", "serveuser", "Serve User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	if err != nil {
		t.Fatalf("Failed to create space: %v", err)
	}

	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "testroom", "Test Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}

	// Join room
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}

	// Login
	env.login(t, "serveuser", "password123")

	// Upload an attachment
	imageData := createAssetTestPNG(t, 400, 300)
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "Test message", imageData, "serve-test.png")
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	if attachmentURL == "" {
		t.Fatal("Expected original asset URL")
	}

	// Get original attachment
	originalResp, err := env.client.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to get original attachment: %v", err)
	}
	defer originalResp.Body.Close()

	if originalResp.StatusCode != http.StatusOK {
		t.Errorf("Expected 200 OK, got %d", originalResp.StatusCode)
	}
	if got := originalResp.Header.Get("Accept-Ranges"); got != "none" {
		t.Errorf("Expected Accept-Ranges: none, got %q", got)
	}

	// Should have correct content type
	contentType := originalResp.Header.Get("Content-Type")
	if contentType != "image/png" {
		t.Errorf("Expected Content-Type: image/png, got: %s", contentType)
	}

	// Body should be readable
	body, err := io.ReadAll(originalResp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}
	if len(body) == 0 {
		t.Error("Expected non-empty response body")
	}

	// Chatto-backed attachments intentionally ignore Range and return the full
	// object. Deployments that need seekable media should use S3 redirects.
	rangeRequest, err := http.NewRequest(http.MethodGet, env.url(attachmentURL), nil)
	if err != nil {
		t.Fatalf("Failed to create range request: %v", err)
	}
	rangeRequest.Header.Set("Range", "bytes=1-2")
	rangeResp, err := env.client.Do(rangeRequest)
	if err != nil {
		t.Fatalf("Failed to get ranged original attachment: %v", err)
	}
	defer rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected full 200 response for Range request, got %d", rangeResp.StatusCode)
	}
	if got := rangeResp.Header.Get("Accept-Ranges"); got != "none" {
		t.Fatalf("Expected Accept-Ranges: none, got %q", got)
	}
	rangeBody, err := io.ReadAll(rangeResp.Body)
	if err != nil {
		t.Fatalf("Failed to read ranged response body: %v", err)
	}
	if !bytes.Equal(rangeBody, imageData) {
		t.Fatal("Range request did not return the complete attachment")
	}
}

func TestAsset_ActiveAttachment_UsesSandboxHeaders(t *testing.T) {
	env := setupAssetTestServer(t)

	user, err := env.core.CreateUser(env.ctx, "system", "sandboxuser", "Sandbox User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "sandboxroom", "Sandbox Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}
	env.login(t, "sandboxuser", "password123")

	_, attachment := env.postAssetMessageWithAttachmentContentType(
		t,
		room.Id,
		"html attachment",
		[]byte("<!doctype html><script>window.__ran = true</script>"),
		"demo.html",
		"text/html; charset=utf-8",
	)
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	if attachmentURL == "" {
		t.Fatal("Expected stable attachment URL")
	}

	stableResp, err := env.client.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to fetch stable attachment URL: %v", err)
	}
	stableResp.Body.Close()
	if stableResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected stable attachment status 200, got %d", stableResp.StatusCode)
	}
	assertSandboxedOriginalAttachment(t, stableResp)
}

func TestAsset_OriginalDownload(t *testing.T) {
	for _, backend := range []string{"embedded", "s3"} {
		t.Run(backend, func(t *testing.T) {
			var env *assetTestEnv
			if backend == "s3" {
				env = setupAssetTestServerWithS3(t)
			} else {
				env = setupAssetTestServer(t)
			}
			user, err := env.core.CreateUser(env.ctx, "system", "downloaduser", "Download User", "password123")
			if err != nil {
				t.Fatal(err)
			}
			room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "downloadroom", "Download Room")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
				t.Fatal(err)
			}
			env.login(t, "downloaduser", "password123")
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			for _, filename := range []string{"report.html", "Bericht ü.html", `report; "final".html`} {
				t.Run(filename, func(t *testing.T) {
					body := []byte("<!doctype html><h1>Shared document</h1><script>window.__ran=true</script>")
					_, attachment := env.postAssetMessageWithAttachmentContentType(t, room.Id, "download", body, filename, "text/html; charset=utf-8")
					assetURL, err := url.Parse(env.url(attachment.GetAssetUrl().GetUrl()))
					if err != nil {
						t.Fatal(err)
					}
					for _, mode := range []string{"", "0", "1"} {
						query := assetURL.Query()
						query.Set("download", mode)
						assetURL.RawQuery = query.Encode()
						resp, err := client.Get(assetURL.String())
						if err != nil {
							t.Fatal(err)
						}
						got, err := io.ReadAll(resp.Body)
						resp.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) {
							t.Fatalf("mode %q: status %d or body mismatch", mode, resp.StatusCode)
						}
						assertSandboxedOriginalAttachment(t, resp)
						if resp.Header.Get("Cache-Control") != protectedAssetCacheControl {
							t.Fatal("download lost private cache policy")
						}
						if mode == "1" {
							disposition, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
							if err != nil || disposition != "attachment" || params["filename"] != filename {
								t.Fatalf("invalid download disposition: %q (%v)", resp.Header.Get("Content-Disposition"), err)
							}
						} else if resp.Header.Get("Content-Disposition") != "" {
							t.Fatal("inline response forced a download")
						}
					}
					for _, ticket := range []string{"", "invalid"} {
						query := assetURL.Query()
						query.Set("access", ticket)
						assetURL.RawQuery = query.Encode()
						resp, err := client.Get(assetURL.String())
						if err != nil {
							t.Fatal(err)
						}
						resp.Body.Close()
						if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
							t.Fatalf("unauthorized download status %d", resp.StatusCode)
						}
						if resp.Header.Get("Content-Disposition") != "" {
							t.Fatal("unauthorized response exposes a filename")
						}
					}
				})
			}
			// Passive S3 media normally redirects; explicit download must stream.
			if backend == "s3" {
				body := []byte("passive audio bytes")
				_, attachment := env.postAssetMessageWithAttachmentContentType(t, room.Id, "audio download", body, "recording.mp3", "audio/mpeg")
				resp, err := client.Get(env.url(attachment.GetAssetUrl().GetUrl() + "&download=1"))
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != http.StatusOK || !bytes.Equal(got, body) || resp.Header.Get("Location") != "" {
					t.Fatal("explicit S3 download did not stream original bytes")
				}
			}
		})
	}
}

func TestAttachmentDownloadFilename(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"../../report.html", "report.html"},
		{`C:\folder\report.html`, "report.html"},
		{"report\r\n.html", "report.html"},
		{"", "attachment"}, {"..", "attachment"}, {"/", "attachment"},
	} {
		if got := attachmentDownloadFilename(tc.input); got != tc.want {
			t.Errorf("filename = %q, want %q", got, tc.want)
		}
	}
}

func TestAsset_ActiveAttachmentOnS3_StreamsWithSandboxInsteadOfRedirect(t *testing.T) {
	env := setupAssetTestServerWithS3(t)

	user, err := env.core.CreateUser(env.ctx, "system", "s3sandboxuser", "S3 Sandbox User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "s3sandboxroom", "S3 Sandbox Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}
	env.login(t, "s3sandboxuser", "password123")

	_, attachment := env.postAssetMessageWithAttachmentContentType(
		t,
		room.Id,
		"s3 html attachment",
		[]byte("<!doctype html><script>window.__ran = true</script>"),
		"s3-demo.html",
		"text/html",
	)
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	if attachmentURL == "" {
		t.Fatal("Expected stable attachment URL")
	}

	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	stableResp, err := noRedirectClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to fetch S3 stable attachment URL: %v", err)
	}
	stableResp.Body.Close()
	if stableResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected S3 stable attachment to stream with 200, got %d", stableResp.StatusCode)
	}
	assertSandboxedOriginalAttachment(t, stableResp)
}

func TestAsset_StableS3ImageStreamsThroughChattoByDefault(t *testing.T) {
	env := setupAssetTestServerWithS3(t)

	user, err := env.core.CreateUser(env.ctx, "system", "s3imageuser", "S3 Image User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "s3imageroom", "S3 Image Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}
	env.login(t, "s3imageuser", "password123")

	imageData := createAssetTestPNG(t, 64, 48)
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "s3 image", imageData, "s3-image.png")
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	if attachmentURL == "" {
		t.Fatal("Expected stable attachment URL")
	}

	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirectClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to fetch S3 image attachment URL: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected S3 image to stream through Chatto with 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "" {
		t.Fatalf("Expected no redirect Location for ordinary S3 image, got %q", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != protectedAssetCacheControl {
		t.Fatalf("Cache-Control = %q, want %q", got, protectedAssetCacheControl)
	}
}

func TestAsset_StableS3VideoRedirectsUnlessProxyForcesStream(t *testing.T) {
	env := setupAssetTestServerWithS3AndVideo(t)
	env.core.VideoUploadsEnabled = true

	user, err := env.core.CreateUser(env.ctx, "system", "s3videouser", "S3 Video User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "s3videoroom", "S3 Video Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}
	env.login(t, "s3videouser", "password123")

	_, attachment := env.postAssetMessageWithAttachmentContentType(
		t,
		room.Id,
		"s3 video",
		[]byte("fake-video-bytes"),
		"s3-video.mp4",
		"video/mp4",
	)
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	if attachmentURL == "" {
		t.Fatal("Expected stable attachment URL")
	}

	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	redirectResp, err := noRedirectClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to fetch S3 video attachment URL: %v", err)
	}
	redirectResp.Body.Close()
	if redirectResp.StatusCode != http.StatusFound {
		t.Fatalf("Expected S3 video to redirect with 302, got %d", redirectResp.StatusCode)
	}
	if got := redirectResp.Header.Get("Cache-Control"); got != protectedAssetCacheControl {
		t.Fatalf("Redirect Cache-Control = %q, want %q", got, protectedAssetCacheControl)
	}
	if got := redirectResp.Header.Get("Location"); got == "" || !strings.Contains(got, "X-Amz-Expires=300") {
		t.Fatalf("Expected short-lived presigned S3 Location, got %q", got)
	}

}

func TestAsset_StableNilStorageS3VideoRedirectsViaProbe(t *testing.T) {
	env := setupAssetTestServerWithS3AndVideo(t)
	env.core.VideoUploadsEnabled = true

	user, err := env.core.CreateUser(env.ctx, "system", "s3legacyvideouser", "S3 Legacy Video User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "s3legacyvideoroom", "S3 Legacy Video Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}
	env.login(t, "s3legacyvideouser", "password123")

	videoBytes := []byte("fake legacy video bytes")
	_, attachment := env.postAssetMessageWithAttachmentContentType(
		t,
		room.Id,
		"s3 legacy video",
		videoBytes,
		"s3-legacy-video.mp4",
		"video/mp4",
	)
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	if attachmentURL == "" {
		t.Fatal("Expected stable attachment URL")
	}

	appendAssetProjectionTestEvent(t, env, attachment.GetId(), &evtv1.Event{
		Id: "E-storage-less-" + attachment.GetId(),
		Event: &evtv1.Event_AssetCreated{
			AssetCreated: &evtv1.AssetCreatedEvent{
				OriginalBinaryAvailable: true,
				RoomId:                  room.Id,
				Asset: &evtv1.AssetRecord{
					Id:          attachment.GetId(),
					Filename:    "s3-legacy-video.mp4",
					ContentType: "video/mp4",
					Size:        int64(len(videoBytes)),
				},
			},
		},
	})

	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	redirectResp, err := noRedirectClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to fetch storage-less S3 video attachment URL: %v", err)
	}
	redirectResp.Body.Close()
	if redirectResp.StatusCode != http.StatusFound {
		t.Fatalf("Expected storage-less S3 video to redirect with 302, got %d", redirectResp.StatusCode)
	}
	if got := redirectResp.Header.Get("Location"); got == "" || !strings.Contains(got, "X-Amz-Expires=300") {
		t.Fatalf("Expected probed short-lived presigned S3 Location, got %q", got)
	}
}

func TestOriginalAttachmentNeedsSandbox(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{name: "HTML", contentType: "text/html", want: true},
		{name: "HTML with parameters", contentType: "text/html; charset=utf-8", want: true},
		{name: "XHTML", contentType: "application/xhtml+xml", want: true},
		{name: "SVG", contentType: "image/svg+xml", want: true},
		{name: "XML", contentType: "application/xml", want: true},
		{name: "XML suffix", contentType: "application/atom+xml", want: true},
		{name: "PNG", contentType: "image/png", want: false},
		{name: "PDF", contentType: "application/pdf", want: false},
		{name: "unknown", contentType: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := originalAttachmentNeedsSandbox(tt.contentType); got != tt.want {
				t.Fatalf("originalAttachmentNeedsSandbox(%q) = %v, want %v", tt.contentType, got, tt.want)
			}
		})
	}
}

func assertSandboxedOriginalAttachment(t *testing.T, resp *http.Response) {
	t.Helper()
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != originalAttachmentSandboxCSP {
		t.Fatalf("Content-Security-Policy = %q, want %q", got, originalAttachmentSandboxCSP)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", got)
	}
}

func TestAsset_OriginalAttachment_HasCacheHeaders(t *testing.T) {
	env := setupAssetTestServer(t)

	// Create user and space with room
	user, err := env.core.CreateUser(env.ctx, "system", "cacheheaderuser", "Cache Header User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	if err != nil {
		t.Fatalf("Failed to create space: %v", err)
	}

	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "testroom", "Test Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}

	// Join room
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}

	// Login
	env.login(t, "cacheheaderuser", "password123")

	// Upload an attachment
	imageData := createAssetTestPNG(t, 400, 300)
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "Test message", imageData, "cache-header-test.png")
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	if attachmentURL == "" {
		t.Fatal("Expected original asset URL")
	}

	// Get original attachment
	originalResp, err := env.client.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to get original attachment: %v", err)
	}
	defer originalResp.Body.Close()

	if originalResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", originalResp.StatusCode)
	}

	// Verify caching headers
	cacheControl := originalResp.Header.Get("Cache-Control")
	if cacheControl != protectedAssetCacheControl {
		t.Errorf("Expected Cache-Control: %s, got: %s", protectedAssetCacheControl, cacheControl)
	}

	etag := originalResp.Header.Get("ETag")
	if etag == "" {
		t.Error("Expected ETag header to be set")
	}

	vary := originalResp.Header.Get("Vary")
	if vary != "Accept-Encoding, Authorization, Cookie" {
		t.Errorf("Expected Vary: Accept-Encoding, Authorization, Cookie, got: %s", vary)
	}
}

// A credential-authenticated asset read uses the caller's privileged-mode
// state. An owner outside privileged mode reads only what RBAC allows.
func TestAsset_StableURLBearerAppliesOwnerPrivilegedModeGate(t *testing.T) {
	env := setupAssetTestServer(t)

	author, err := env.core.CreateUser(env.ctx, "system", "gatedassetauthor", "Gated Asset Author", "password123")
	if err != nil {
		t.Fatalf("Failed to create author: %v", err)
	}
	owner, err := env.core.CreateUser(env.ctx, "system", "gatedassetowner", "Gated Asset Owner", "password123")
	if err != nil {
		t.Fatalf("Failed to create owner: %v", err)
	}
	if err := env.core.AssignOwnerRole(env.ctx, owner.Id); err != nil {
		t.Fatalf("Failed to assign owner role: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, author.Id, "channel", "", "gated-assets", "")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	for _, userID := range []string{author.Id, owner.Id} {
		if _, err := env.core.JoinRoom(env.ctx, userID, "channel", userID, room.Id); err != nil {
			t.Fatalf("Failed to join room: %v", err)
		}
	}
	env.login(t, "gatedassetauthor", "password123")
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "gated asset", createAssetTestPNG(t, 40, 30), "gated.png")
	stable, err := url.Parse(attachment.GetAssetUrl().GetUrl())
	if err != nil {
		t.Fatalf("Failed to parse stable URL: %v", err)
	}
	stable.RawQuery = ""
	if err := env.core.DenyRoomPermission(env.ctx, core.SystemActorID, room.Id, core.RoleEveryone, core.PermMessageRead); err != nil {
		t.Fatalf("Failed to deny message.read: %v", err)
	}

	token, err := env.core.CreateAuthToken(env.ctx, owner.Id)
	if err != nil {
		t.Fatalf("Failed to create auth token: %v", err)
	}
	fetch := func() int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, env.url(stable.String()), nil)
		if err != nil {
			t.Fatalf("Failed to build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("Failed to get stable URL: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := fetch(); status != http.StatusForbidden {
		t.Fatalf("owner without privileged mode status = %d, want %d", status, http.StatusForbidden)
	}
	if _, err := env.core.SetBearerPrivilegedMode(env.ctx, token, true); err != nil {
		t.Fatalf("Failed to activate privileged mode: %v", err)
	}
	if status := fetch(); status != http.StatusOK {
		t.Fatalf("owner with privileged mode status = %d, want %d", status, http.StatusOK)
	}
}

func TestAsset_StableURLAcceptsAccessTicketAndBearerAuth(t *testing.T) {
	env := setupAssetTestServer(t)

	user, err := env.core.CreateUser(env.ctx, "system", "bearerassetuser", "Bearer Asset User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "bearer-assets", "Bearer Assets")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}

	env.login(t, "bearerassetuser", "password123")
	imageData := createAssetTestPNG(t, 120, 90)
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "bearer asset", imageData, "bearer.png")
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	thumbnailURL := attachment.GetThumbnailAssetUrl().GetUrl()
	if attachmentURL == "" || thumbnailURL == "" {
		t.Fatal("Expected original and thumbnail asset URLs")
	}

	unauthClient := &http.Client{}

	withoutAccess, err := url.Parse(attachmentURL)
	if err != nil {
		t.Fatalf("Failed to parse stable URL: %v", err)
	}
	withoutAccess.RawQuery = ""

	unauthResp, err := unauthClient.Get(env.url(withoutAccess.String()))
	if err != nil {
		t.Fatalf("Failed to get stable URL without credentials: %v", err)
	}
	unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Expected stable URL without credentials to return 401, got %d", unauthResp.StatusCode)
	}

	ticketResp, err := unauthClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to get stable URL with access ticket: %v", err)
	}
	ticketResp.Body.Close()
	if ticketResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected stable URL with access ticket to return 200, got %d", ticketResp.StatusCode)
	}

	token, err := env.core.CreateAuthToken(env.ctx, user.Id)
	if err != nil {
		t.Fatalf("Failed to create auth token: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, env.url(withoutAccess.String()), nil)
	if err != nil {
		t.Fatalf("Failed to build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	bearerResp, err := unauthClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to get stable URL with bearer: %v", err)
	}
	bearerResp.Body.Close()
	if bearerResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected bearer stable URL request to return 200, got %d", bearerResp.StatusCode)
	}

	thumbResp, err := unauthClient.Get(env.url(thumbnailURL))
	if err != nil {
		t.Fatalf("Failed to get stable thumbnail URL with access ticket: %v", err)
	}
	thumbResp.Body.Close()
	if thumbResp.StatusCode != http.StatusOK {
		t.Fatalf("Expected stable thumbnail request with access ticket to return 200, got %d", thumbResp.StatusCode)
	}

	// 【本地改动 2026-09-13】跟随 5ee7a0fb9（override attachment transform URLs to
	// the original URL）：fork 不再签发带变换段的缩略图 URL，缩略图就是原图 URL。
	// 上游此处断言「缩略图 URL 含 960x400 变换段」并用篡改尺寸验证签名失效；
	// fork 的 URL 无变换段可篡改，故改为守护：缩略图 URL 与原图 URL 相同且无变换段，
	// 且旧变换段 URL（legacyTransformURL）仍可用并返回原图字节。
	if thumbnailURL != attachmentURL {
		t.Fatalf("thumbnail URL = %q, want the original asset URL %q", thumbnailURL, attachmentURL)
	}
	if strings.Contains(thumbnailURL, "/image/") {
		t.Fatalf("Expected fork to issue no transform URL, got %q", thumbnailURL)
	}
	legacyThumbResp, err := unauthClient.Get(env.url(legacyTransformURL(t, attachment.GetId(), user.Id, attachmentURL)))
	if err != nil {
		t.Fatalf("Failed to get legacy transform thumbnail URL: %v", err)
	}
	legacyThumb, err := io.ReadAll(legacyThumbResp.Body)
	legacyThumbResp.Body.Close()
	if err != nil {
		t.Fatalf("Failed to read legacy transform thumbnail URL: %v", err)
	}
	if legacyThumbResp.StatusCode != http.StatusOK {
		t.Fatalf("legacy transform thumbnail status = %d, want %d", legacyThumbResp.StatusCode, http.StatusOK)
	}
	if !bytes.Equal(legacyThumb, imageData) {
		t.Fatal("legacy transform thumbnail URL must serve the stored original bytes")
	}

	thumbnailWithoutAccess, err := url.Parse(thumbnailURL)
	if err != nil {
		t.Fatalf("Failed to parse stable thumbnail URL: %v", err)
	}
	thumbnailWithoutAccess.RawQuery = ""
	req, err = http.NewRequest(http.MethodGet, env.url(thumbnailWithoutAccess.String()), nil)
	if err != nil {
		t.Fatalf("Failed to build unsigned thumbnail request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	unsignedThumbResp, err := unauthClient.Do(req)
	if err != nil {
		t.Fatalf("Failed to get unsigned stable thumbnail URL with bearer: %v", err)
	}
	unsignedThumbResp.Body.Close()
	if unsignedThumbResp.StatusCode != http.StatusForbidden {
		t.Fatalf("Expected unsigned stable thumbnail request with bearer to return 403, got %d", unsignedThumbResp.StatusCode)
	}
}

func TestAsset_ServerAsset_HasCacheHeaders(t *testing.T) {
	env := setupAssetTestServer(t)

	// Create a user with an avatar (server asset)
	user, err := env.core.CreateUser(env.ctx, "system", "serverassetuser", "Instance Asset User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	// Upload and durably select an avatar through the production public producer.
	avatarData := createAssetTestPNG(t, 200, 200)
	avatar, err := env.core.UploadUserAvatar(env.ctx, user.Id, bytes.NewReader(avatarData))
	if err != nil {
		t.Fatalf("Failed to upload avatar: %v", err)
	}
	if err := env.core.SetUserAvatar(env.ctx, user.Id, avatar); err != nil {
		t.Fatalf("Failed to set avatar: %v", err)
	}
	avatarPath := core.ServerAssetDeliveryKey(avatar)
	if !strings.HasPrefix(avatarPath, core.PublicServerAssetObjectPrefix) {
		t.Fatalf("new NATS avatar key = %q, want public namespace", avatarPath)
	}
	if _, err := env.core.ServerStore().GetInfo(env.ctx, avatarPath); err != nil {
		t.Fatalf("new NATS avatar object %q: %v", avatarPath, err)
	}
	if _, err := env.core.ServerStore().GetInfo(env.ctx, avatar.GetId()); err == nil {
		t.Fatalf("new NATS avatar unexpectedly retained flat object %q", avatar.GetId())
	}

	// Get the server asset (avatars are public, no auth needed)
	resp, err := env.client.Get(env.url("/assets/server/" + avatarPath))
	if err != nil {
		t.Fatalf("Failed to get server asset: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", resp.StatusCode)
	}
	aliasResp, err := env.client.Get(env.url("/assets/server/" + avatar.GetId()))
	if err != nil {
		t.Fatalf("Failed to get new server asset through logical-ID alias: %v", err)
	}
	aliasResp.Body.Close()
	if aliasResp.StatusCode != http.StatusOK {
		t.Fatalf("logical-ID alias status = %d, want 200", aliasResp.StatusCode)
	}

	// Verify caching headers
	cacheControl := resp.Header.Get("Cache-Control")
	if cacheControl != "public, max-age=31536000, immutable" {
		t.Errorf("Expected Cache-Control: public, max-age=31536000, immutable, got: %s", cacheControl)
	}

	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Error("Expected ETag header to be set")
	}
	// ETag should contain the path
	expectedETag := fmt.Sprintf("\"%s\"", avatarPath)
	if etag != expectedETag {
		t.Errorf("Expected ETag: %s, got: %s", expectedETag, etag)
	}

	vary := resp.Header.Get("Vary")
	if vary != "Accept-Encoding" {
		t.Errorf("Expected Vary: Accept-Encoding, got: %s", vary)
	}
}

func TestAsset_ServerAssetTransformKeepsDefaultQuality(t *testing.T) {
	env := setupAssetTestServer(t)

	imageData := createAssetTestPNG(t, 400, 300)
	branding, err := env.core.UploadServerBanner(env.ctx, bytes.NewReader(imageData))
	if err != nil {
		t.Fatalf("Failed to upload server branding: %v", err)
	}
	if err := env.core.SetServerBanner(env.ctx, "system", branding); err != nil {
		t.Fatalf("Failed to set server branding: %v", err)
	}
	assetPath := core.ServerAssetDeliveryKey(branding)
	if !strings.HasPrefix(assetPath, core.PublicServerAssetObjectPrefix) {
		t.Fatalf("new NATS branding key = %q, want public namespace", assetPath)
	}

	// 【本地改动 2026-09-13】跟随 77f471f1c（compress server assets at upload and
	// stop request-time transforms）：banner/logo/头像/链接预览在上传时已缩放到上限并
	// 压成有损 WebP，请求期不再编码第二份更小的字节。GetTransformedServerAssetURL 把
	// /t/{sig} override 成原档 URL，所以此处拿到的就是原档链接，返回的应是存储的原字节。
	transformURL := env.core.GetTransformedServerAssetURL(assetPath, 200, 200, "contain")
	if strings.Contains(transformURL, "/t/") {
		t.Fatalf("GetTransformedServerAssetURL = %q, fork issues no server asset transform URL", transformURL)
	}
	if !strings.HasPrefix(transformURL, "/assets/server/"+assetPath) {
		t.Fatalf("GetTransformedServerAssetURL = %q, want the original server asset URL", transformURL)
	}
	resp, err := env.client.Get(env.url(transformURL))
	if err != nil {
		t.Fatalf("Failed to get server asset: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read server asset: %v", err)
	}
	if !bytes.Equal(got, imageData) {
		t.Fatal("server asset URL must serve the stored bytes unchanged")
	}
}

func TestAsset_LegacyFlatPublicAssetsRemainAvailable(t *testing.T) {
	env := setupAssetTestServer(t)
	imageData := createAssetTestPNG(t, 120, 80)
	store := env.core.ServerStore()

	legacyRecord := func(assetID, filename string) *evtv1.AssetRecord {
		if _, err := store.Put(env.ctx, jetstream.ObjectMeta{
			Name:    assetID,
			Headers: map[string][]string{"Content-Type": {"image/png"}},
		}, bytes.NewReader(imageData)); err != nil {
			t.Fatalf("store legacy public object %q: %v", assetID, err)
		}
		return &evtv1.AssetRecord{
			Id:          assetID,
			Filename:    filename,
			ContentType: "image/png",
			Size:        int64(len(imageData)),
			Storage:     &evtv1.AssetRecord_Nats{Nats: &evtv1.NATSAsset{Key: assetID}},
		}
	}
	assertOK := func(path string) {
		t.Helper()
		resp, err := (&http.Client{}).Get(env.url(path))
		if err != nil {
			t.Fatalf("GET %q: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %q status = %d, want 200", path, resp.StatusCode)
		}
	}

	user, err := env.core.CreateUser(env.ctx, "system", "legacyassetuser", "Legacy Asset User", "password123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	avatar := legacyRecord(core.NewAssetID(), "avatar.webp")
	if err := env.core.SetUserAvatar(env.ctx, user.GetId(), avatar); err != nil {
		t.Fatalf("SetUserAvatar legacy fixture: %v", err)
	}
	avatarURL, err := env.core.GetUserAvatarURL(env.ctx, user.GetId(), nil, nil, "")
	if err != nil {
		t.Fatalf("GetUserAvatarURL: %v", err)
	}
	if strings.Contains(avatarURL, core.PublicServerAssetObjectPrefix) {
		t.Fatalf("legacy avatar URL unexpectedly changed: %q", avatarURL)
	}
	assertOK(avatarURL)

	logo := legacyRecord(core.NewAssetID(), "logo.webp")
	if err := env.core.SetServerLogo(env.ctx, "system", logo); err != nil {
		t.Fatalf("SetServerLogo legacy fixture: %v", err)
	}
	logoURL, err := env.core.GetServerLogoURL(env.ctx, nil, nil, "")
	if err != nil {
		t.Fatalf("GetServerLogoURL: %v", err)
	}
	if strings.Contains(logoURL, core.PublicServerAssetObjectPrefix) {
		t.Fatalf("legacy logo URL unexpectedly changed: %q", logoURL)
	}
	assertOK(logoURL)

	previewID := core.NewAssetID()
	legacyRecord(previewID, "link-preview.webp")
	appendRoomTimelineAssetTestEvent(t, env, "Rlegacynamespace", &evtv1.Event{
		Event: &evtv1.Event_MessageBody{MessageBody: &evtv1.MessageBodyEvent{
			RoomId:  "Rlegacynamespace",
			EventId: "Elegacynamespacemessage",
			Body: &evtv1.MessageBody{LinkPreview: &evtv1.LinkPreview{
				ImageAssetId: &previewID,
			}},
		}},
	})
	assertOK(env.core.GetTransformedServerAssetURL(previewID, 64, 64, "cover"))
}

func TestAsset_CacheOnlyLegacyLinkPreviewRemainsAvailable(t *testing.T) {
	env := setupAssetTestServer(t)
	assetID := core.NewAssetID()
	previewURL := "https://legacy-cache-only.example/article"
	imageData := createAssetTestPNG(t, 120, 80)
	store := env.core.ServerStore()
	if _, err := store.Put(env.ctx, jetstream.ObjectMeta{
		Name:    assetID,
		Headers: map[string][]string{"Content-Type": {"image/png"}},
	}, bytes.NewReader(imageData)); err != nil {
		t.Fatalf("store cache-only legacy preview: %v", err)
	}
	if err := env.previews.Set(env.ctx, previewURL, &evtv1.LinkPreview{
		Url:          previewURL,
		ImageAssetId: &assetID,
		ImageAsset: &evtv1.AssetRecord{
			Id:          assetID,
			ContentType: "image/png",
			Storage:     &evtv1.AssetRecord_Nats{Nats: &evtv1.NATSAsset{Key: assetID}},
		},
	}); err != nil {
		t.Fatalf("cache legacy preview metadata: %v", err)
	}

	assertStatus := func(path string, want int) {
		t.Helper()
		resp, err := http.Get(env.url(path))
		if err != nil {
			t.Fatalf("GET %q: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("GET %q status = %d, want %d", path, resp.StatusCode, want)
		}
	}

	assetPath := "/assets/server/" + assetID
	assertStatus(assetPath, http.StatusNotFound)
	user, err := env.core.CreateUser(env.ctx, core.SystemActorID, "legacypreviewuser", "Legacy Preview User", "password123")
	if err != nil {
		t.Fatalf("CreateUser legacy preview: %v", err)
	}
	env.login(t, user.GetLogin(), "password123")
	messages := apiv1connect.NewMessageServiceClient(env.client, env.server.URL+connectAPIPrefix)
	previewResp, err := messages.FetchLinkPreview(env.ctx, connect.NewRequest(&apiv1.FetchLinkPreviewRequest{Url: previewURL}))
	if err != nil {
		t.Fatalf("FetchLinkPreview cached legacy preview: %v", err)
	}
	preview := previewResp.Msg.GetPreview()
	if preview.GetImageAssetId() != assetID {
		t.Fatalf("cached preview asset ID = %q, want %q", preview.GetImageAssetId(), assetID)
	}
	assertStatus(assetPath, http.StatusOK)
	assertStatus(preview.GetImageUrl(), http.StatusOK)

	info, err := store.GetInfo(env.ctx, assetID)
	if err != nil {
		t.Fatalf("GetInfo promoted legacy preview: %v", err)
	}
	if got := info.Headers.Get(core.ServerAssetVisibilityHeader); got != core.ServerAssetVisibilityPublic {
		t.Fatalf("legacy preview visibility = %q, want %q", got, core.ServerAssetVisibilityPublic)
	}
}

func TestAsset_PublicServerRouteRejectsPrivateAndUnknownNATSObjects(t *testing.T) {
	env := setupAssetTestServer(t)

	user, err := env.core.CreateUser(env.ctx, "system", "publicrouteuser", "Public Route User", "password123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "public-route-private", "Private Assets")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}
	env.login(t, "publicrouteuser", "password123")

	imageData := createAssetTestPNG(t, 320, 240)
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "private image", imageData, "private.png")
	assetID := attachment.GetId()
	if assetID == "" {
		t.Fatal("attachment asset id is empty")
	}

	assertStatus := func(path string, want int) *http.Response {
		t.Helper()
		resp, err := (&http.Client{}).Get(env.url(path))
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("GET %s status = %d, want %d", path, resp.StatusCode, want)
		}
		return resp
	}

	// The public original and transform routes must not expose a declared room
	// attachment, while its ticketed protected route remains usable.
	assertStatus("/assets/server/"+assetID, http.StatusNotFound)
	privateTransform := env.core.GetTransformedServerAssetURL(assetID, 64, 64, "cover")
	assertStatus(privateTransform, http.StatusNotFound)
	assertStatus("/assets/server/"+assetID+"/t/not-a-valid-signature", http.StatusNotFound)
	assertStatus(attachment.GetAssetUrl().GetUrl(), http.StatusOK)
	assertStatus(attachment.GetThumbnailAssetUrl().GetUrl(), http.StatusOK)

	// A pre-populated public-transform cache entry cannot bypass classification.
	cacheKey := core.ImageCacheKey(core.ServerAssetSignResource, assetID, 64, 64, "cover")
	if err := env.core.StoreCachedResize(env.ctx, cacheKey, createAssetTestPNG(t, 64, 64)); err != nil {
		t.Fatalf("seed private server-transform cache: %v", err)
	}
	resp := assertStatus(privateTransform, http.StatusNotFound)
	if got := resp.Header.Get("X-Cache"); got != "" {
		t.Fatalf("private transform exposed cache state %q", got)
	}

	// Model a supported legacy attachment whose object lacks modern Room-Id
	// metadata. Its durable AssetProjection declaration must still deny it.
	store := env.core.ServerStore()
	info, err := store.GetInfo(env.ctx, assetID)
	if err != nil {
		t.Fatalf("GetInfo legacy fixture: %v", err)
	}
	headers := maps.Clone(info.Headers)
	headers.Del("Room-Id")
	if err := store.UpdateMeta(env.ctx, assetID, jetstream.ObjectMeta{Name: assetID, Headers: headers}); err != nil {
		t.Fatalf("UpdateMeta legacy fixture: %v", err)
	}
	assertStatus("/assets/server/"+assetID, http.StatusNotFound)
	appendAssetProjectionTestEvent(t, env, assetID, &evtv1.Event{
		Event: &evtv1.Event_AssetDeleted{AssetDeleted: &evtv1.AssetDeletedEvent{
			AssetId: assetID,
		}},
	})
	assertStatus("/assets/server/"+assetID, http.StatusNotFound)

	// Exercise the real resumable-upload producer and discover its temporary
	// object key without completing (and therefore deleting) the upload.
	uploadClient := apiv1connect.NewAssetUploadServiceClient(env.client, env.server.URL+connectAPIPrefix)
	chunkData := []byte("private resumable chunk")
	chunkSum := sha256.Sum256(chunkData)
	createdUpload, err := uploadClient.CreateUpload(env.ctx, connect.NewRequest(&apiv1.CreateUploadRequest{
		RoomId:      room.Id,
		Filename:    "pending.txt",
		ContentType: "text/plain",
		Size:        int64(len(chunkData)),
		Sha256:      hex.EncodeToString(chunkSum[:]),
	}))
	if err != nil {
		t.Fatalf("create resumable upload fixture: %v", err)
	}
	if _, err := uploadClient.UploadChunk(env.ctx, connect.NewRequest(&apiv1.UploadChunkRequest{
		UploadId:    createdUpload.Msg.GetUpload().GetUploadId(),
		Content:     chunkData,
		ChunkSha256: hex.EncodeToString(chunkSum[:]),
	})); err != nil {
		t.Fatalf("upload resumable chunk fixture: %v", err)
	}
	objects, err := store.List(env.ctx)
	if err != nil {
		t.Fatalf("list upload chunk fixture: %v", err)
	}
	chunkKey := ""
	for _, object := range objects {
		if object != nil && strings.HasPrefix(object.Name, "asset-upload."+createdUpload.Msg.GetUpload().GetUploadId()+".") {
			chunkKey = object.Name
			break
		}
	}
	if chunkKey == "" {
		t.Fatal("resumable upload chunk object was not found")
	}
	assertStatus("/assets/server/"+chunkKey, http.StatusNotFound)
	assertStatus(env.core.GetTransformedServerAssetURL(chunkKey, 32, 32, "cover"), http.StatusNotFound)

	// Temporary, reserved, private-metadata, and unknown canonical objects all
	// look absent through both original and transformed public paths.
	fixtures := []struct {
		key     string
		headers map[string][]string
	}{
		{key: "internal/projection-snapshots/v1/objects/0123456789abcdef0123456789abcdef"},
		{key: "attachments/" + assetID},
		{key: "spaces/server/attachments/" + assetID},
		{key: "Aroommetadata00", headers: map[string][]string{"Room-Id": {room.Id}}},
		{key: core.PublicServerAssetObjectKey("Aprivatepublic0"), headers: map[string][]string{"Room-Id": {room.Id}}},
		{key: "Aunknownobject0"},
	}
	for _, fixture := range fixtures {
		if _, err := store.Put(env.ctx, jetstream.ObjectMeta{Name: fixture.key, Headers: fixture.headers}, bytes.NewReader(imageData)); err != nil {
			t.Fatalf("store fixture %q: %v", fixture.key, err)
		}
		assertStatus("/assets/server/"+fixture.key, http.StatusNotFound)
		assertStatus(env.core.GetTransformedServerAssetURL(fixture.key, 32, 32, "cover"), http.StatusNotFound)
	}
}

func TestAsset_PublicLinkPreviewMarkerServesWithoutAuthentication(t *testing.T) {
	env := setupAssetTestServer(t)
	assetID := core.NewAssetID()
	imageData := createAssetTestPNG(t, 120, 80)
	namespacedID := core.NewAssetID()
	namespacedKey := core.PublicServerAssetObjectKey(namespacedID)
	if _, err := env.core.ServerStore().Put(env.ctx, jetstream.ObjectMeta{
		Name:    namespacedKey,
		Headers: map[string][]string{"Content-Type": {"image/png"}},
	}, bytes.NewReader(imageData)); err != nil {
		t.Fatalf("store namespaced public image: %v", err)
	}
	for _, path := range []string{namespacedKey, namespacedID} {
		resp, err := (&http.Client{}).Get(env.url("/assets/server/" + path))
		if err != nil {
			t.Fatalf("GET namespaced public image through %q: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("namespaced public image through %q status = %d, want 200", path, resp.StatusCode)
		}
	}

	if _, err := env.core.ServerStore().Put(env.ctx, jetstream.ObjectMeta{
		Name:    assetID,
		Headers: map[string][]string{"Content-Type": {"image/png"}},
	}, bytes.NewReader(imageData)); err != nil {
		t.Fatalf("store marked link-preview image: %v", err)
	}
	markPublicServerAssetForTest(t, env, assetID)

	resp, err := (&http.Client{}).Get(env.url("/assets/server/" + assetID))
	if err != nil {
		t.Fatalf("GET public link-preview image: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public link-preview status = %d, want 200", resp.StatusCode)
	}

	// Historical preview objects did not carry the marker. Durable message-body
	// references rebuild the positive public declaration during projection replay.
	legacyID := core.NewAssetID()
	if _, err := env.core.ServerStore().Put(env.ctx, jetstream.ObjectMeta{
		Name:    legacyID,
		Headers: map[string][]string{"Content-Type": {"image/png"}},
	}, bytes.NewReader(imageData)); err != nil {
		t.Fatalf("store historical link-preview image: %v", err)
	}
	appendRoomTimelineAssetTestEvent(t, env, "Rpreviewhistory", &evtv1.Event{
		Event: &evtv1.Event_MessageBody{MessageBody: &evtv1.MessageBodyEvent{
			RoomId:  "Rpreviewhistory",
			EventId: "Epreviewmessage",
			Body: &evtv1.MessageBody{LinkPreview: &evtv1.LinkPreview{
				ImageAssetId: &legacyID,
				ImageAsset:   &evtv1.AssetRecord{Id: legacyID},
			}},
		}},
	})
	legacyResp, err := (&http.Client{}).Get(env.url("/assets/server/" + legacyID))
	if err != nil {
		t.Fatalf("GET historical link-preview image: %v", err)
	}
	legacyResp.Body.Close()
	if legacyResp.StatusCode != http.StatusOK {
		t.Fatalf("historical link-preview status = %d, want 200", legacyResp.StatusCode)
	}
}

func TestAsset_LegacyAttachmentRouteIsGone(t *testing.T) {
	env := setupAssetTestServer(t)

	resp, err := env.client.Get(env.url("/assets/attachments/not-a-locator"))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("Expected removed legacy attachment route to return 404, got %d", resp.StatusCode)
	}
}

func TestAsset_StableURLIsCapability(t *testing.T) {
	env := setupAssetTestServer(t)

	user, err := env.core.CreateUser(env.ctx, "system", "authuser", "Auth User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}

	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "testroom", "Test Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}

	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}

	env.login(t, "authuser", "password123")

	imageData := createAssetTestPNG(t, 400, 300)
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "Test message", imageData, "auth-test.png")
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	thumbnailURL := attachment.GetThumbnailAssetUrl().GetUrl()
	if attachmentURL == "" || thumbnailURL == "" {
		t.Fatal("Expected original and thumbnail stable asset URLs")
	}

	// A no-cookie / no-header client holding the access-ticket URL should be
	// able to fetch the binary.
	unauthClient := &http.Client{}

	originalResp, err := unauthClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	originalResp.Body.Close()
	if originalResp.StatusCode != http.StatusOK {
		t.Errorf("Stable URL should authorize itself; got status %d", originalResp.StatusCode)
	}

	transformResp, err := unauthClient.Get(env.url(thumbnailURL))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	transformResp.Body.Close()
	if transformResp.StatusCode != http.StatusOK {
		t.Errorf("Stable transform URL should authorize itself; got status %d", transformResp.StatusCode)
	}

	// A tampered access ticket must fail.
	tampered := strings.TrimSuffix(attachmentURL, "X") + "z"
	tamperedResp, err := unauthClient.Get(env.url(tampered))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	tamperedResp.Body.Close()
	if tamperedResp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected 403 for tampered access ticket, got %d", tamperedResp.StatusCode)
	}
}

func TestAsset_StableURLOnS3IsCapability(t *testing.T) {
	env := setupAssetTestServerWithS3(t)

	user, err := env.core.CreateUser(env.ctx, "system", "s3authuser", "S3 Auth User", "password123")
	if err != nil {
		t.Fatalf("Failed to create user: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, user.Id, "channel", "", "s3testroom", "S3 Test Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, user.Id, "channel", user.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}

	env.login(t, "s3authuser", "password123")

	imageData := createAssetTestPNG(t, 400, 300)
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "Test S3 message", imageData, "s3-auth-test.png")
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	thumbnailURL := attachment.GetThumbnailAssetUrl().GetUrl()
	if attachmentURL == "" || thumbnailURL == "" {
		t.Fatal("Expected original and thumbnail stable asset URLs")
	}

	// Anonymous client — the access-ticket URL alone should be enough to fetch.
	unauthClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	originalResp, err := unauthClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	originalResp.Body.Close()
	if originalResp.StatusCode != http.StatusOK {
		t.Errorf("S3 image stable URL: expected 200 with access ticket, got %d", originalResp.StatusCode)
	}

	transformResp, err := unauthClient.Get(env.url(thumbnailURL))
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	transformResp.Body.Close()
	if transformResp.StatusCode != http.StatusOK {
		t.Errorf("S3 transform URL: expected 200 with access ticket, got %d", transformResp.StatusCode)
	}

	// The public server route probes only the separate instance/ namespace and
	// must never fall through to S3 attachments/ objects.
	publicOriginal, err := unauthClient.Get(env.url("/assets/server/" + attachment.GetId()))
	if err != nil {
		t.Fatalf("S3 public original probe: %v", err)
	}
	publicOriginal.Body.Close()
	if publicOriginal.StatusCode != http.StatusNotFound {
		t.Fatalf("S3 public original probe status = %d, want 404", publicOriginal.StatusCode)
	}
	publicTransformURL := env.core.GetTransformedServerAssetURL(attachment.GetId(), 64, 64, "cover")
	publicTransform, err := unauthClient.Get(env.url(publicTransformURL))
	if err != nil {
		t.Fatalf("S3 public transform probe: %v", err)
	}
	publicTransform.Body.Close()
	if publicTransform.StatusCode != http.StatusNotFound {
		t.Fatalf("S3 public transform probe status = %d, want 404", publicTransform.StatusCode)
	}
}

func TestAsset_HLSGenerationIsAuthorizedAndBackendIndependent(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(*testing.T) *assetTestEnv
	}{
		{name: "NATS", setup: setupAssetTestServer},
		{name: "S3", setup: setupAssetTestServerWithS3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := tt.setup(t)
			login := "hls-" + strings.ToLower(tt.name)
			viewer, err := env.core.CreateUser(env.ctx, core.SystemActorID, login, "HLS Viewer", "password123")
			if err != nil {
				t.Fatalf("CreateUser: %v", err)
			}
			room, err := env.core.CreateRoom(env.ctx, viewer.Id, core.KindChannel, "", login+"-room", "HLS Room")
			if err != nil {
				t.Fatalf("CreateRoom: %v", err)
			}
			if _, err := env.core.JoinRoom(env.ctx, viewer.Id, core.KindChannel, viewer.Id, room.Id); err != nil {
				t.Fatalf("JoinRoom: %v", err)
			}

			original, err := env.core.UploadAttachment(env.ctx, viewer.Id, room.Id, "clip.mp4", "video/mp4", bytes.NewReader([]byte("source")))
			if err != nil {
				t.Fatalf("UploadAttachment: %v", err)
			}
			segment, err := env.core.UploadDerivativeAttachment(env.ctx, original.GetId(), evtv1.AssetDerivativeRole_ASSET_DERIVATIVE_ROLE_HLS_MEDIA_SEGMENT, room.Id, "segment-00000.ts", "video/mp2t", bytes.NewReader([]byte("segment-bytes")))
			if err != nil {
				t.Fatalf("UploadDerivativeAttachment(segment): %v", err)
			}
			if err := env.core.RecordAssetProcessingStarted(env.ctx, core.SystemActorID, room.Id, "E-hls", original.GetId()); err != nil {
				t.Fatalf("RecordAssetProcessingStarted: %v", err)
			}
			hls := &evtv1.AssetProcessedHLS{
				Renditions: []*evtv1.AssetHLSRendition{{
					Width: 640, Height: 360, Bandwidth: 500000,
					Segments: []*evtv1.AssetHLSSegment{{AssetId: segment.GetId(), DurationMs: 6000}},
				}},
			}
			if err := env.core.RecordAssetProcessedWithHLS(env.ctx, core.SystemActorID, room.Id, "E-hls", original.GetId(), 6000, 640, 360, nil, nil, hls); err != nil {
				t.Fatalf("RecordAssetProcessedWithHLS: %v", err)
			}

			masterURL := env.core.GetStableHLSMasterPlaylistAssetURL(original.GetId(), viewer.Id).URL
			plainClient := &http.Client{}
			masterResp, err := plainClient.Get(env.url(masterURL))
			if err != nil {
				t.Fatalf("GET master: %v", err)
			}
			masterBody, _ := io.ReadAll(masterResp.Body)
			masterResp.Body.Close()
			if masterResp.StatusCode != http.StatusOK {
				t.Fatalf("master status = %d, body = %s", masterResp.StatusCode, masterBody)
			}
			mediaURL := firstHLSURI(t, masterBody)
			if !strings.Contains(mediaURL, "/renditions/0/playlist.m3u8?access=") {
				t.Fatalf("rewritten media URL = %q", mediaURL)
			}

			mediaResp, err := plainClient.Get(env.url(mediaURL))
			if err != nil {
				t.Fatalf("GET media: %v", err)
			}
			mediaBody, _ := io.ReadAll(mediaResp.Body)
			mediaResp.Body.Close()
			if mediaResp.StatusCode != http.StatusOK {
				t.Fatalf("media status = %d, body = %s", mediaResp.StatusCode, mediaBody)
			}
			segmentURL := firstHLSURI(t, mediaBody)
			segmentResp, err := plainClient.Get(env.url(segmentURL))
			if err != nil {
				t.Fatalf("GET segment: %v", err)
			}
			segmentBody, _ := io.ReadAll(segmentResp.Body)
			segmentResp.Body.Close()
			if segmentResp.StatusCode != http.StatusOK || string(segmentBody) != "segment-bytes" {
				t.Fatalf("segment status/body = %d/%q", segmentResp.StatusCode, segmentBody)
			}

			if err := env.core.LeaveRoom(env.ctx, viewer.Id, core.KindChannel, viewer.Id, room.Id); err != nil {
				t.Fatalf("LeaveRoom: %v", err)
			}
			revoked, err := plainClient.Get(env.url(masterURL))
			if err != nil {
				t.Fatalf("GET revoked master: %v", err)
			}
			revoked.Body.Close()
			if revoked.StatusCode != http.StatusForbidden {
				t.Fatalf("revoked master status = %d, want 403", revoked.StatusCode)
			}
		})
	}
}

func TestHLSDerivativeRequiresExpectedParentAndRole(t *testing.T) {
	env := setupAssetTestServer(t)
	viewer, err := env.core.CreateUser(env.ctx, core.SystemActorID, "hls-derivative", "HLS Viewer", "password123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, viewer.Id, core.KindChannel, "", "hls-derivative-room", "HLS Room")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	original, err := env.core.UploadAttachment(env.ctx, viewer.Id, room.Id, "clip.mp4", "video/mp4", bytes.NewReader([]byte("source")))
	if err != nil {
		t.Fatalf("UploadAttachment(original): %v", err)
	}
	otherOriginal, err := env.core.UploadAttachment(env.ctx, viewer.Id, room.Id, "other.mp4", "video/mp4", bytes.NewReader([]byte("other source")))
	if err != nil {
		t.Fatalf("UploadAttachment(other): %v", err)
	}
	wrongParent, err := env.core.UploadDerivativeAttachment(env.ctx, otherOriginal.GetId(), evtv1.AssetDerivativeRole_ASSET_DERIVATIVE_ROLE_HLS_MEDIA_SEGMENT, room.Id, "other-segment.ts", "video/mp2t", bytes.NewReader([]byte("segment")))
	if err != nil {
		t.Fatalf("UploadDerivativeAttachment(wrong parent): %v", err)
	}
	wrongRole, err := env.core.UploadDerivativeAttachment(env.ctx, original.GetId(), evtv1.AssetDerivativeRole_ASSET_DERIVATIVE_ROLE_VIDEO_VARIANT, room.Id, "variant.mp4", "video/mp4", bytes.NewReader([]byte("variant")))
	if err != nil {
		t.Fatalf("UploadDerivativeAttachment(wrong role): %v", err)
	}

	server := &HTTPServer{core: env.core}
	for _, tt := range []struct {
		name    string
		assetID string
	}{
		{name: "wrong parent", assetID: wrongParent.GetId()},
		{name: "wrong role", assetID: wrongRole.GetId()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			if _, ok := server.hlsDerivative(c, original.GetId(), tt.assetID, evtv1.AssetDerivativeRole_ASSET_DERIVATIVE_ROLE_HLS_MEDIA_SEGMENT); ok {
				t.Fatal("hlsDerivative accepted an unrelated derivative")
			}
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", recorder.Code)
			}
		})
	}
}

func firstHLSURI(t *testing.T, playlist []byte) string {
	t.Helper()
	for line := range strings.SplitSeq(string(playlist), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	t.Fatal("playlist contained no URI")
	return ""
}

func TestRenderHLSPlaylistsFromManifest(t *testing.T) {
	hls := &evtv1.AssetProcessedHLS{Renditions: []*evtv1.AssetHLSRendition{{
		Width: 640, Height: 360, Bandwidth: 500_000,
		Segments: []*evtv1.AssetHLSSegment{{AssetId: "A-one", DurationMs: 6000}, {AssetId: "A-two", DurationMs: 1500}},
	}}}
	master, err := renderHLSMasterPlaylist(hls, func(index int) string { return fmt.Sprintf("/rendition/%d", index) })
	if err != nil {
		t.Fatalf("render master: %v", err)
	}
	if got := string(master); !strings.Contains(got, "BANDWIDTH=500000,RESOLUTION=640x360\n/rendition/0") {
		t.Fatalf("master playlist = %q", got)
	}
	media, err := renderHLSMediaPlaylist(hls.GetRenditions()[0], func(index int) string { return fmt.Sprintf("/segment/%d", index) })
	if err != nil {
		t.Fatalf("render media: %v", err)
	}
	if got := string(media); !strings.Contains(got, "#EXT-X-TARGETDURATION:6") || !strings.Contains(got, "#EXTINF:1.500,\n/segment/1") || !strings.HasSuffix(got, "#EXT-X-ENDLIST\n") {
		t.Fatalf("media playlist = %q", got)
	}
}

// TestAsset_RevokedMembership_RevokesStableURL covers the "kick / leave"
// path under the per-user access-ticket model.
func TestAsset_RevokedMembership_RevokesStableURL(t *testing.T) {
	env := setupAssetTestServerWithS3(t)

	owner, err := env.core.CreateUser(env.ctx, "system", "asset-owner", "Owner", "password123")
	if err != nil {
		t.Fatalf("Failed to create owner: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, owner.Id, "channel", "", "private-room", "Private Room")
	if err != nil {
		t.Fatalf("Failed to create room: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, owner.Id, "channel", owner.Id, room.Id); err != nil {
		t.Fatalf("Failed to join room: %v", err)
	}

	env.login(t, "asset-owner", "password123")
	imageData := createAssetTestPNG(t, 400, 300)
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "private", imageData, "private.png")
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	thumbnailURL := attachment.GetThumbnailAssetUrl().GetUrl()

	// Sanity check: owner can fetch their own URL without a cookie because the
	// access ticket is the capability.
	plainClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	r, err := plainClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("pre-leave GET: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("expected stable URL to work pre-leave, got %d", r.StatusCode)
	}
	thumb, err := plainClient.Get(env.url(thumbnailURL))
	if err != nil {
		t.Fatalf("pre-leave thumbnail GET: %v", err)
	}
	thumb.Body.Close()
	if thumb.StatusCode != http.StatusOK {
		t.Fatalf("expected stable thumbnail to work pre-leave, got %d", thumb.StatusCode)
	}

	// Owner leaves the room, so their stable access-ticket URL should stop working.
	if err := env.core.LeaveRoom(env.ctx, owner.Id, "channel", owner.Id, room.Id); err != nil {
		t.Fatalf("LeaveRoom: %v", err)
	}

	r2, err := plainClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("post-leave GET: %v", err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 after ticket user left the room, got %d", r2.StatusCode)
	}
	thumb2, err := plainClient.Get(env.url(thumbnailURL))
	if err != nil {
		t.Fatalf("post-leave thumbnail GET: %v", err)
	}
	thumb2.Body.Close()
	if thumb2.StatusCode != http.StatusForbidden {
		t.Errorf("expected cached thumbnail ticket to fail after leave, got %d", thumb2.StatusCode)
	}
}

func TestAsset_RevokedMessageReadRevokesStableURL(t *testing.T) {
	env := setupAssetTestServerWithS3(t)

	viewer, err := env.core.CreateUser(env.ctx, core.SystemActorID, "asset-read-viewer", "Asset Read Viewer", "password123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, viewer.Id, core.KindChannel, "", "asset-read-room", "")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	if _, err := env.core.JoinRoom(env.ctx, viewer.Id, core.KindChannel, viewer.Id, room.Id); err != nil {
		t.Fatalf("JoinRoom: %v", err)
	}
	env.login(t, "asset-read-viewer", "password123")
	_, attachment := env.postAssetMessageWithAttachment(t, room.Id, "private", createAssetTestPNG(t, 64, 64), "private.png")
	attachmentURL := attachment.GetAssetUrl().GetUrl()
	plainClient := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}

	before, err := plainClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("GET before denial: %v", err)
	}
	before.Body.Close()
	if before.StatusCode != http.StatusOK {
		t.Fatalf("status before denial = %d, want 200", before.StatusCode)
	}
	if err := env.core.DenyRoomPermission(env.ctx, core.SystemActorID, room.Id, core.RoleEveryone, core.PermMessageRead); err != nil {
		t.Fatalf("DenyRoomPermission: %v", err)
	}
	if err := env.core.DenyRoomPermission(env.ctx, core.SystemActorID, room.Id, core.RoleEveryone, core.PermMessageReadInteractions); err != nil {
		t.Fatalf("DenyRoomPermission message.read-interactions: %v", err)
	}
	after, err := plainClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("GET after denial: %v", err)
	}
	after.Body.Close()
	if after.StatusCode != http.StatusForbidden {
		t.Fatalf("status after denial = %d, want 403", after.StatusCode)
	}
}

func TestAsset_InteractionReaderCanFetchStableURL(t *testing.T) {
	env := setupAssetTestServerWithS3(t)

	author, err := env.core.CreateUser(env.ctx, core.SystemActorID, "asset-interaction-author", "Asset Interaction Author", "password123")
	if err != nil {
		t.Fatalf("CreateUser author: %v", err)
	}
	reader, err := env.core.CreateUser(env.ctx, core.SystemActorID, "asset-interaction-reader", "Asset Interaction Reader", "password123")
	if err != nil {
		t.Fatalf("CreateUser reader: %v", err)
	}
	room, err := env.core.CreateRoom(env.ctx, author.GetId(), core.KindChannel, "", "asset-interaction-room", "")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	for _, userID := range []string{author.GetId(), reader.GetId()} {
		if _, err := env.core.JoinRoom(env.ctx, userID, core.KindChannel, userID, room.GetId()); err != nil {
			t.Fatalf("JoinRoom %s: %v", userID, err)
		}
	}
	if err := env.core.DenyRoomPermission(env.ctx, core.SystemActorID, room.GetId(), core.RoleEveryone, core.PermMessageRead); err != nil {
		t.Fatalf("DenyRoomPermission message.read: %v", err)
	}
	if err := env.core.GrantUserRoomPermission(env.ctx, core.SystemActorID, room.GetId(), reader.GetId(), core.PermMessageReadInteractions); err != nil {
		t.Fatalf("GrantUserRoomPermission message.read-interactions: %v", err)
	}

	env.login(t, "asset-interaction-author", "password123")
	messageID, _ := env.postAssetMessageWithAttachment(
		t, room.GetId(), "@asset-interaction-reader related attachment", createAssetTestPNG(t, 64, 64), "related.png",
	)

	env.login(t, "asset-interaction-reader", "password123")
	messages := apiv1connect.NewMessageServiceClient(env.client, env.server.URL+connectAPIPrefix)
	message, err := messages.GetMessage(env.ctx, connect.NewRequest(&apiv1.GetMessageRequest{
		RoomId: room.GetId(), EventId: messageID,
	}))
	if err != nil {
		t.Fatalf("GetMessage as interaction reader: %v", err)
	}
	attachments := message.Msg.GetMessage().GetAttachments()
	if len(attachments) != 1 {
		t.Fatalf("interaction-scoped message attachments = %d, want 1", len(attachments))
	}
	attachmentURL := attachments[0].GetAssetUrl().GetUrl()
	if attachmentURL == "" {
		t.Fatal("interaction-scoped attachment URL is empty")
	}
	plainClient := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := plainClient.Get(env.url(attachmentURL))
	if err != nil {
		t.Fatalf("GET interaction-scoped attachment: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("interaction-scoped attachment status = %d, want 200", response.StatusCode)
	}
}

func TestAsset_NeighborhoodImage(t *testing.T) {
	env := setupAssetTestServer(t)
	store, err := env.js.ObjectStore(env.ctx, "NEIGHBORHOOD_IMAGES")
	if err != nil {
		t.Fatalf("open Neighborhood image store: %v", err)
	}
	name := strings.Repeat("b", 64)
	imageData := []byte("RIFF\x00\x00\x00\x00WEBP")
	if _, err := store.Put(env.ctx, jetstream.ObjectMeta{Name: name}, bytes.NewReader(imageData)); err != nil {
		t.Fatalf("store Neighborhood image: %v", err)
	}

	resp, err := http.Get(env.url(core.NeighborhoodImagePath(name)))
	if err != nil {
		t.Fatalf("GET Neighborhood image: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read Neighborhood image: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(body, imageData) {
		t.Fatalf("body = %q, want %q", body, imageData)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/webp" {
		t.Fatalf("Content-Type = %q, want image/webp", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}

	for _, path := range []string{
		core.NeighborhoodImagePath(strings.Repeat("c", 64)),
		core.NeighborhoodImagePath(strings.ToUpper(name)),
		core.NeighborhoodImagePath("not-a-hash"),
	} {
		resp, err := http.Get(env.url(path))
		if err != nil {
			t.Fatalf("GET %q: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %q status = %d, want 404", path, resp.StatusCode)
		}
	}
}
