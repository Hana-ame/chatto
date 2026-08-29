package http_server

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"hmans.de/chatto/internal/assets"
	"hmans.de/chatto/internal/authctx"
	"hmans.de/chatto/internal/core"
	evtv1 "hmans.de/chatto/internal/pb/chatto/core/evt/v1"
	"hmans.de/chatto/pkg/signedurl"
)

const protectedAssetCacheControl = "private, no-store"

// stripServerAssetFilenameTail removes a single trailing {fn.ext} segment from
// a /assets/server/ request path. The segment must be a short dot-bearing name
// in the URL-safe filename alphabet; anything else (or a path without '/') is
// not a decorated URL and is returned unchanged.
//
// 【本地改动 2026-08-23】配合 core 侧带 {fn.ext} 的公开 server 资产 URL。
// 只在「整路径分类失败」后才尝试剥尾段，且剥掉后的 key 仍走完整公开分类，
// 因此该容错不会让私有对象或保留命名空间变得可达。
func stripServerAssetFilenameTail(path string) (string, bool) {
	idx := strings.LastIndexByte(path, '/')
	if idx <= 0 {
		return path, false
	}
	tail := path[idx+1:]
	if tail == "" || len(tail) > 128 || !strings.Contains(tail, ".") {
		return path, false
	}
	for _, r := range tail {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_'
		if !ok {
			return path, false
		}
	}
	return path[:idx], true
}

// publicAssetCacheControl is used for everything addressable by an immutable
// URL: public server assets and (【本地改动 2026-08-18】) attachment binaries
// and derivatives, whose URLs are keyed by assetID and never change.
const publicAssetCacheControl = "public, max-age=31536000, immutable"

func (s *HTTPServer) setupAssetRoutes() {
	// Server assets use *path which catches everything including /t/signedPath for transforms
	// The serveServerAsset handler detects and routes transform requests appropriately
	// These handlers probe both NATS and S3 backends automatically
	s.router.GET("/assets/server/*path", s.serveServerAsset)
	// 【本地改动 2026-08-18】附件 URL 增加 {fn.ext} 尾段的新路由：带文件名的
	// 走公开入口（仅 assetID 访问 + public immutable 缓存）；旧的无文件名
	// 路由保留原 ticket/成员校验语义，历史消息与已发出的 URL 继续有效。
	s.router.GET("/assets/files/:assetID", s.serveStableAttachment)
	s.router.GET("/assets/files/:assetID/:filename", s.servePublicStableAttachment)
	s.router.GET("/assets/files/:assetID/image/:dimensions/:fit", s.serveStableTransformedAttachment)
	s.router.GET("/assets/files/:assetID/image/:dimensions/:fit/:filename", s.servePublicStableTransformedAttachment)
	// 【本地改动 2026-08-23】补注册 HEAD：gin 不会把 HEAD 映射到 GET 路由，
	// 未注册时源站对所有 /assets/* 的 HEAD 一律 404——CF 边缘未命中转发
	// HEAD 时拿到 404+no-store，缓存状态被标成 BYPASS（2026-08-23 线上定位）。
	headRoutes := []struct {
		path    string
		handler gin.HandlerFunc
	}{
		{"/assets/files/:assetID", s.serveStableAttachment},
		{"/assets/files/:assetID/:filename", s.servePublicStableAttachment},
		{"/assets/files/:assetID/image/:dimensions/:fit", s.serveStableTransformedAttachment},
		{"/assets/files/:assetID/image/:dimensions/:fit/:filename", s.servePublicStableTransformedAttachment},
	}
	for _, route := range headRoutes {
		s.router.HEAD(route.path, route.handler)
	}
	s.router.GET("/assets/hls/:assetID/master.m3u8", s.serveHLSMasterPlaylist)
	s.router.GET("/assets/hls/:assetID/renditions/:rendition/playlist.m3u8", s.serveHLSMediaPlaylist)
	s.router.GET("/assets/hls/:assetID/renditions/:rendition/segments/:segment", s.serveHLSSegment)
}

// transformRequest holds the parameters for a transformed asset request.
// This allows sharing the transformation logic between different asset types.
type transformRequest struct {
	// ResourceID1 and ResourceID2 are used for signing verification.
	// For attachments: ("attachment", attachmentID)
	// For server assets: ("server", key)
	ResourceID1 string
	ResourceID2 string
	SignedPath  string
	// CachePrefix distinguishes cache keys between asset types (e.g., "attachment", "server")
	CachePrefix string
	// AssetID is used for ETag generation and logging
	AssetID string
	// JPEGQuality overrides the default quality for opaque static derivatives.
	JPEGQuality int
	// FetchAsset returns the asset data and content type.
	// The reader will be closed if it implements io.Closer.
	FetchAsset func(ctx context.Context) (io.Reader, string, error)
	// Authorize checks if access is allowed. Return true if authorized.
	// If nil, asset is considered public and no authorization is needed.
	Authorize func(c *gin.Context) bool
}

type assetDeliveryMode int

const (
	deliveryChattoStream assetDeliveryMode = iota
	deliveryS3Redirect
)

const largeAttachmentRedirectThreshold = 32 << 20

func protectedAssetDeliveryMode(attachment *evtv1.Attachment) assetDeliveryMode {
	if attachment == nil {
		return deliveryChattoStream
	}
	if !attachmentCanUsePresignedRedirect(attachment.GetContentType()) {
		return deliveryChattoStream
	}
	if storage := attachment.GetStorage(); storage != nil {
		if _, ok := storage.GetAsset().(*evtv1.DeprecatedAsset_S3); !ok {
			return deliveryChattoStream
		}
	}
	contentType := strings.ToLower(attachment.GetContentType())
	if strings.HasPrefix(contentType, "video/") || strings.HasPrefix(contentType, "audio/") {
		return deliveryS3Redirect
	}
	if attachment.GetSize() >= largeAttachmentRedirectThreshold {
		return deliveryS3Redirect
	}
	return deliveryChattoStream
}

func (s *HTTPServer) serveServerAsset(c *gin.Context) {
	path := c.Param("path")

	// Trim leading slash
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}

	// /assets/server/* is intentionally unauthenticated and may only serve
	// explicitly public, server-scoped assets. Classify the base key before
	// transform signature parsing, derivative-cache access, object reads, or
	// image transformation so shared-store private objects always look absent.
	key := path
	signedPath := ""
	transformRequest := false
	if idx := strings.LastIndex(path, "/t/"); idx != -1 {
		transformRequest = true
		key = path[:idx]
		signedPath = path[idx+3:]
		// 【本地改动 2026-08-23】transform URL 可带 {fn.ext} 尾段
		// （/t/{params}.{sig}/{fn.ext}）。签名只覆盖第一段，剥掉装饰性尾段
		// 再验证；签名段本身不含 '/'。
		if cut := strings.IndexByte(signedPath, '/'); cut != -1 {
			signedPath = signedPath[:cut]
		}
	}
	location, public := s.core.ResolvePublicServerAsset(c.Request.Context(), key)
	if !public {
		// 【本地改动 2026-08-23】原始 URL 也可带 {fn.ext} 尾段
		// （/{key}/{fn.ext}）。合法 key 永远不含 '.'（canonical ID 是固定
		// 字母表、public/ 前缀下也是纯 ID），所以「整路径分类失败 → 剥掉
		// 最后一个点段重试」不会歧义。剥掉后仍走完整公开分类，私有对象与
		// 保留命名空间照旧 fail closed，不产生新的可达面。
		if base, ok := stripServerAssetFilenameTail(key); ok {
			if loc2, pub2 := s.core.ResolvePublicServerAsset(c.Request.Context(), base); pub2 {
				key, location, public = base, loc2, true
			}
		}
	}
	if key == "" || !public {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	if transformRequest && signedPath == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}

	// Check if this is a transform request: path ends with /t/{signedPath}
	// Pattern: {key}/t/{signedPath}
	if transformRequest {
		s.serveTransformedServerAsset(c, key, signedPath, location)
		return
	}

	s.logger.Debug("Serving server asset", "asset_id", key)

	// Probe both NATS and S3 backends
	reader, info, err := s.core.GetPublicServerAsset(c.Request.Context(), location)
	if err != nil {
		s.logger.Error("Failed to get server asset", "error", err, "asset_id", key)
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	// Close the reader if it implements io.Closer
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}

	// Get content type, fall back to extension-based detection
	contentType := info.ContentType
	if contentType == "" {
		contentType = getContentType(key)
	}

	// Immutable asset - cache forever
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("ETag", fmt.Sprintf("\"%s\"", key))
	c.Header("Vary", "Accept-Encoding")

	c.DataFromReader(
		http.StatusOK,
		info.Size,
		contentType,
		reader,
		nil,
	)
}

// serveStableAttachment serves the canonical authenticated asset URL:
//
//	/assets/files/{assetID}
//
// The URL identifies the binary, while the access ticket (or, for API clients,
// the request's cookie/bearer token) authorizes access.
func (s *HTTPServer) serveStableAttachment(c *gin.Context) {
	ctx := c.Request.Context()
	assetID := c.Param("assetID")

	attachment, ok := s.resolveStableAttachment(c, ctx, assetID, nil)
	if !ok {
		return
	}

	if protectedAssetDeliveryMode(attachment) == deliveryS3Redirect {
		if presignedURL, err := s.core.TryPresignedAttachmentURL(ctx, attachment, core.S3AssetRedirectTTL); err == nil {
			c.Header("Cache-Control", protectedAssetCacheControl)
			c.Redirect(http.StatusFound, presignedURL)
			return
		}
	}

	reader, info, err := s.core.GetAttachmentReader(ctx, attachment)
	if err != nil {
		s.logger.Error("Failed to get stable attachment", "error", err, "attachment_id", assetID)
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}

	contentType := info.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	setOriginalAttachmentSecurityHeaders(c, contentType)

	c.Header("Cache-Control", protectedAssetCacheControl)
	c.Header("ETag", fmt.Sprintf("\"%s\"", assetID))
	// 【本地改动 2026-08-23】Vary 不再携带 Authorization/Cookie：响应字节只由
	// assetID 决定，凭据只是访问门控而非表示选择器；这些 ticket URL 即将被
	// 带 {fn.ext} 的公开 URL 全面取代（仅剩历史客户端兜底），对它们做按凭据
	// 缓存分片毫无收益，反而干扰共享缓存键。
	c.Header("Vary", "Accept-Encoding")
	// Chatto-backed streams are sequential. Seekable media delivery requires an
	// S3 redirect, whose object server handles byte ranges directly.
	c.Header("Accept-Ranges", "none")
	c.DataFromReader(http.StatusOK, info.Size, contentType, reader, nil)
}

// servePublicStableAttachment serves the canonical public asset URL:
//
//	/assets/files/{assetID}/{fn.ext}
//
// 【本地改动 2026-08-18】带 {fn.ext} 尾段的新 URL 走公开入口：访问仅凭
// assetID（无 ticket、无成员校验），响应 public immutable 可长期缓存。
// 旧的无文件名 URL 继续走 serveStableAttachment（ticket 语义）。
func (s *HTTPServer) servePublicStableAttachment(c *gin.Context) {
	ctx := c.Request.Context()
	assetID := c.Param("assetID")

	attachment, ok := s.resolvePublicAttachment(c, assetID)
	if !ok {
		return
	}

	if protectedAssetDeliveryMode(attachment) == deliveryS3Redirect {
		if presignedURL, err := s.core.TryPresignedAttachmentURL(ctx, attachment, core.S3AssetRedirectTTL); err == nil {
			// 302 重定向本身不缓存；presigned URL 短期有效。
			c.Header("Cache-Control", protectedAssetCacheControl)
			c.Redirect(http.StatusFound, presignedURL)
			return
		}
	}

	reader, info, err := s.core.GetAttachmentReader(ctx, attachment)
	if err != nil {
		s.logger.Error("Failed to get stable attachment", "error", err, "attachment_id", assetID)
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}

	contentType := info.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	setOriginalAttachmentSecurityHeaders(c, contentType)

	c.Header("Cache-Control", publicAssetCacheControl)
	c.Header("ETag", fmt.Sprintf("\"%s\"", assetID))
	c.Header("Vary", "Accept-Encoding")
	// Chatto-backed streams are sequential. Seekable media delivery requires an
	// S3 redirect, whose object server handles byte ranges directly.
	c.Header("Accept-Ranges", "none")
	c.DataFromReader(http.StatusOK, info.Size, contentType, reader, nil)
}

const originalAttachmentSandboxCSP = "sandbox"

func setOriginalAttachmentSecurityHeaders(c *gin.Context, contentType string) {
	c.Header("X-Content-Type-Options", "nosniff")
	if originalAttachmentNeedsSandbox(contentType) {
		c.Header("Content-Security-Policy", originalAttachmentSandboxCSP)
	}
}

func attachmentCanUsePresignedRedirect(contentType string) bool {
	return !originalAttachmentNeedsSandbox(contentType)
}

func originalAttachmentNeedsSandbox(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.Split(contentType, ";")[0])
	}
	mediaType = strings.ToLower(mediaType)

	switch mediaType {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "application/xml", "text/xml":
		return true
	default:
		return strings.HasSuffix(mediaType, "+xml")
	}
}

// serveStableTransformedAttachment serves an authenticated image derivative:
//
//	/assets/files/{assetID}/image/{width}x{height}/{fit}
//
// Transform dimensions remain visible and stable in the URL. Authorization
// comes from the asset-scoped access ticket or request credentials.
func (s *HTTPServer) serveStableTransformedAttachment(c *gin.Context) {
	ctx := c.Request.Context()
	assetID := c.Param("assetID")
	params, err := parseStableTransformParams(c.Param("dimensions"), c.Param("fit"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	attachment, ok := s.resolveStableAttachment(c, ctx, assetID, params)
	if !ok {
		return
	}

	s.serveTransformedAssetWithParams(c, transformRequest{
		CachePrefix: AttachmentStableCachePrefix,
		AssetID:     assetID,
		JPEGQuality: AttachmentDerivativeJPEGQuality,
		FetchAsset: func(ctx context.Context) (io.Reader, string, error) {
			reader, info, err := s.core.GetAttachmentReader(ctx, attachment)
			if err != nil {
				return nil, "", err
			}
			return reader, info.ContentType, nil
		},
		Authorize: func(c *gin.Context) bool { return true },
	}, params)
}

// servePublicStableTransformedAttachment serves a public image derivative:
//
//	/assets/files/{assetID}/image/{width}x{height}/{fit}/{fn.ext}
//
// 【本地改动 2026-08-18】带 {fn.ext} 尾段的新 URL 走公开入口：仅凭 assetID
// 访问，Authorize 为 nil → 响应 public immutable 可长期缓存。
func (s *HTTPServer) servePublicStableTransformedAttachment(c *gin.Context) {
	assetID := c.Param("assetID")
	params, err := parseStableTransformParams(c.Param("dimensions"), c.Param("fit"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	attachment, ok := s.resolvePublicAttachment(c, assetID)
	if !ok {
		return
	}

	s.serveTransformedAssetWithParams(c, transformRequest{
		CachePrefix: AttachmentStableCachePrefix,
		AssetID:     assetID,
		JPEGQuality: AttachmentDerivativeJPEGQuality,
		FetchAsset: func(ctx context.Context) (io.Reader, string, error) {
			reader, info, err := s.core.GetAttachmentReader(ctx, attachment)
			if err != nil {
				return nil, "", err
			}
			return reader, info.ContentType, nil
		},
		Authorize: nil, // Attachment derivatives are publicly readable by assetID.
	}, params)
}

const (
	// AttachmentDerivativeJPEGQuality keeps displayed attachment images compact
	// without changing the encoding quality of public server assets.
	AttachmentDerivativeJPEGQuality = 75
	// AttachmentStableCachePrefix is versioned whenever attachment derivative
	// encoding changes so older cached bytes cannot be reused.
	AttachmentStableCachePrefix = core.AttachmentDerivativeCacheResource
)

func parseStableTransformParams(dimensions, fit string) (*signedurl.TransformParams, error) {
	widthText, heightText, ok := strings.Cut(dimensions, "x")
	if !ok {
		return nil, fmt.Errorf("invalid dimensions")
	}
	width, err := strconv.Atoi(widthText)
	if err != nil {
		return nil, fmt.Errorf("invalid width")
	}
	height, err := strconv.Atoi(heightText)
	if err != nil {
		return nil, fmt.Errorf("invalid height")
	}
	params := &signedurl.TransformParams{Width: width, Height: height, Fit: fit}
	if params.Width < 1 || params.Width > 2048 {
		return nil, fmt.Errorf("width out of range [1, 2048]: %d", params.Width)
	}
	if params.Height < 1 || params.Height > 2048 {
		return nil, fmt.Errorf("height out of range [1, 2048]: %d", params.Height)
	}
	if params.Fit != "contain" && params.Fit != "cover" && params.Fit != "exact" {
		return nil, fmt.Errorf("invalid fit mode: %s", params.Fit)
	}
	return params, nil
}

func (s *HTTPServer) resolveStableAttachment(c *gin.Context, ctx context.Context, assetID string, params *signedurl.TransformParams) (*evtv1.Attachment, bool) {
	if assetID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return nil, false
	}

	userID, ok := s.resolveStableAssetViewerID(c, assetID, params)
	if !ok {
		return nil, false
	}
	return s.resolveAttachmentForViewer(c, ctx, assetID, userID)
}

func (s *HTTPServer) resolveAttachmentForViewer(c *gin.Context, ctx context.Context, assetID, userID string) (*evtv1.Attachment, bool) {
	state := s.core.GetAssetState(assetID)
	declared := state.Creation
	if declared == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return nil, false
	}
	roomID := state.RoomID
	if roomID == "" {
		s.logger.Warn("Asset has no room scope", "attachment_id", assetID)
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return nil, false
	}

	kind, err := s.core.FindRoomKind(ctx, roomID)
	if err != nil {
		s.logger.Error("Failed to resolve room kind for stable attachment auth", "error", err, "room_id", roomID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify access"})
		return nil, false
	}
	isMember, err := s.core.RoomMembershipExists(ctx, kind, userID, roomID)
	if err != nil {
		s.logger.Error("Failed to check stable attachment room membership", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify access"})
		return nil, false
	}
	if !isMember {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied: not a member of the room"})
		return nil, false
	}
	canRead, err := s.core.CanReadRoomAsset(ctx, userID, kind, roomID, assetID)
	if err != nil {
		s.logger.Error("Failed to check stable attachment message-read permission", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify access"})
		return nil, false
	}
	if !canRead {
		c.JSON(http.StatusForbidden, gin.H{"error": "Access denied"})
		return nil, false
	}

	attachment := core.AttachmentFromAsset(declared.GetAsset())
	if attachment == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return nil, false
	}
	attachment.RoomId = roomID
	return attachment, true
}

// resolvePublicAttachment resolves an attachment by assetID without any
// viewer checks. 【本地改动 2026-08-18】带 {fn.ext} 的公开 URL 专用：访问
// 仅凭 assetID，不再校验成员身份/会话/ticket。assetID 即访问凭证。
// 2026-08-29 合并 upstream：返回值类型跟随 upstream #2162 从 corev1.Attachment
// 迁移到 evtv1.Attachment（core/v1 pb 包已拆分删除，AttachmentFromAsset 现返回 evtv1）。
func (s *HTTPServer) resolvePublicAttachment(c *gin.Context, assetID string) (*evtv1.Attachment, bool) {
	if assetID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return nil, false
	}
	state := s.core.GetAssetState(assetID)
	declared := state.Creation
	if declared == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return nil, false
	}
	attachment := core.AttachmentFromAsset(declared.GetAsset())
	if attachment == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Attachment not found"})
		return nil, false
	}
	return attachment, true
}

// 【本地改动 2026-08-29】返回值类型从 corev1.AssetProcessedHLS 迁移到 evtv1
// （原因同 resolvePublicAttachment）；函数体为上游实现，未改动。
func (s *HTTPServer) resolveHLS(c *gin.Context) (*evtv1.AssetProcessedHLS, string, bool) {
	assetID := c.Param("assetID")
	access := c.Query("access")
	if assetID == "" || access == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "HLS access ticket required"})
		return nil, "", false
	}
	ticket, err := signedurl.ParseSignedHLSAccessTicket(s.config.Core.Assets.SigningSecret, access)
	if err != nil {
		s.logger.Warn("Invalid HLS access ticket", "error", err, "asset_id", assetID)
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid HLS access ticket"})
		return nil, "", false
	}
	if ticket.Expired(time.Now().Unix()) {
		c.JSON(http.StatusForbidden, gin.H{"error": "HLS access ticket expired"})
		return nil, "", false
	}
	if ticket.AssetID != assetID {
		c.JSON(http.StatusForbidden, gin.H{"error": "HLS access ticket does not match asset"})
		return nil, "", false
	}
	if _, ok := s.resolveAttachmentForViewer(c, c.Request.Context(), assetID, ticket.UserID); !ok {
		return nil, "", false
	}
	manifest := s.core.GetAssetState(assetID).VideoManifest
	if manifest == nil || manifest.Succeeded == nil || manifest.Succeeded.GetVideo().GetHls() == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "HLS generation not found"})
		return nil, "", false
	}
	return manifest.Succeeded.GetVideo().GetHls(), access, true
}

func parseHLSIndex(c *gin.Context, name string, size int) (int, bool) {
	index, err := strconv.Atoi(c.Param(name))
	if err != nil || index < 0 || index >= size {
		c.JSON(http.StatusNotFound, gin.H{"error": "HLS resource not found"})
		return 0, false
	}
	return index, true
}

func hlsChildPath(assetID, access, suffix string) string {
	values := url.Values{}
	values.Set("access", access)
	return fmt.Sprintf("/assets/hls/%s/%s?%s", url.PathEscape(assetID), suffix, values.Encode())
}

func renderHLSMasterPlaylist(hls *evtv1.AssetProcessedHLS, childURL func(index int) string) ([]byte, error) {
	if hls == nil || len(hls.GetRenditions()) == 0 {
		return nil, fmt.Errorf("HLS manifest contains no renditions")
	}
	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	for i, rendition := range hls.GetRenditions() {
		if rendition == nil || rendition.GetBandwidth() < 1 || rendition.GetWidth() < 1 || rendition.GetHeight() < 1 || len(rendition.GetSegments()) == 0 {
			return nil, fmt.Errorf("HLS rendition %d is incomplete", i)
		}
		fmt.Fprintf(&playlist, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d\n%s\n", rendition.GetBandwidth(), rendition.GetWidth(), rendition.GetHeight(), childURL(i))
	}
	return []byte(playlist.String()), nil
}

func renderHLSMediaPlaylist(rendition *evtv1.AssetHLSRendition, segmentURL func(index int) string) ([]byte, error) {
	if rendition == nil || len(rendition.GetSegments()) == 0 {
		return nil, fmt.Errorf("HLS rendition contains no segments")
	}
	var targetDuration int64
	for i, segment := range rendition.GetSegments() {
		if segment == nil || segment.GetAssetId() == "" || segment.GetDurationMs() < 1 {
			return nil, fmt.Errorf("HLS segment %d is incomplete", i)
		}
		seconds := (segment.GetDurationMs() + 999) / 1000
		if seconds > targetDuration {
			targetDuration = seconds
		}
	}
	var playlist strings.Builder
	fmt.Fprintf(&playlist, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n", targetDuration)
	for i, segment := range rendition.GetSegments() {
		fmt.Fprintf(&playlist, "#EXTINF:%.3f,\n%s\n", float64(segment.GetDurationMs())/1000, segmentURL(i))
	}
	playlist.WriteString("#EXT-X-ENDLIST\n")
	return []byte(playlist.String()), nil
}

func (s *HTTPServer) hlsDerivative(c *gin.Context, originAssetID, assetID string, role evtv1.AssetDerivativeRole) (*evtv1.Attachment, bool) {
	declared := s.core.GetAssetState(assetID).Creation
	if declared == nil || declared.GetParentAssetId() != originAssetID || declared.GetDerivativeRole() != role {
		c.JSON(http.StatusNotFound, gin.H{"error": "HLS resource not found"})
		return nil, false
	}
	attachment := core.AttachmentFromAsset(declared.GetAsset())
	if attachment == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "HLS resource not found"})
		return nil, false
	}
	return attachment, true
}

func (s *HTTPServer) serveGeneratedHLSPlaylist(c *gin.Context, playlist []byte, err error) {
	if err != nil {
		s.logger.Error("Invalid durable HLS manifest", "error", err, "asset_id", c.Param("assetID"))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Invalid HLS manifest"})
		return
	}
	c.Header("Cache-Control", protectedAssetCacheControl)
	c.Header("Vary", "Origin")
	c.Data(http.StatusOK, "application/vnd.apple.mpegurl", playlist)
}

func (s *HTTPServer) serveHLSMasterPlaylist(c *gin.Context) {
	hls, access, ok := s.resolveHLS(c)
	if !ok {
		return
	}
	assetID := c.Param("assetID")
	playlist, err := renderHLSMasterPlaylist(hls, func(index int) string {
		return hlsChildPath(assetID, access, fmt.Sprintf("renditions/%d/playlist.m3u8", index))
	})
	s.serveGeneratedHLSPlaylist(c, playlist, err)
}

func (s *HTTPServer) serveHLSMediaPlaylist(c *gin.Context) {
	hls, access, ok := s.resolveHLS(c)
	if !ok {
		return
	}
	renditionIndex, ok := parseHLSIndex(c, "rendition", len(hls.GetRenditions()))
	if !ok {
		return
	}
	assetID := c.Param("assetID")
	rendition := hls.GetRenditions()[renditionIndex]
	playlist, err := renderHLSMediaPlaylist(rendition, func(index int) string {
		return hlsChildPath(assetID, access, fmt.Sprintf("renditions/%d/segments/%d.ts", renditionIndex, index))
	})
	s.serveGeneratedHLSPlaylist(c, playlist, err)
}

func (s *HTTPServer) serveHLSSegment(c *gin.Context) {
	hls, _, ok := s.resolveHLS(c)
	if !ok {
		return
	}
	renditionIndex, ok := parseHLSIndex(c, "rendition", len(hls.GetRenditions()))
	if !ok {
		return
	}
	rendition := hls.GetRenditions()[renditionIndex]
	segmentText := strings.TrimSuffix(c.Param("segment"), ".ts")
	segmentIndex, err := strconv.Atoi(segmentText)
	if err != nil || segmentIndex < 0 || segmentIndex >= len(rendition.GetSegments()) {
		c.JSON(http.StatusNotFound, gin.H{"error": "HLS resource not found"})
		return
	}
	attachment, ok := s.hlsDerivative(c, c.Param("assetID"), rendition.GetSegments()[segmentIndex].GetAssetId(), evtv1.AssetDerivativeRole_ASSET_DERIVATIVE_ROLE_HLS_MEDIA_SEGMENT)
	if !ok {
		return
	}
	reader, info, err := s.core.GetAttachmentReader(c.Request.Context(), attachment)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "HLS resource not found"})
		return
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	c.Header("Cache-Control", protectedAssetCacheControl)
	c.Header("Vary", "Origin")
	c.DataFromReader(http.StatusOK, info.Size, "video/mp2t", reader, nil)
}

func (s *HTTPServer) resolveStableAssetViewerID(c *gin.Context, assetID string, params *signedurl.TransformParams) (string, bool) {
	if access := c.Query("access"); access != "" {
		ticket, err := signedurl.ParseSignedAssetAccessTicket(s.config.Core.Assets.SigningSecret, access)
		if err != nil {
			s.logger.Warn("Invalid asset access ticket", "error", err, "asset_id", assetID)
			c.JSON(http.StatusForbidden, gin.H{"error": "Invalid asset access ticket"})
			return "", false
		}
		if ticket.Expired(time.Now().Unix()) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Asset access ticket expired"})
			return "", false
		}
		if ticket.AssetID != assetID {
			c.JSON(http.StatusForbidden, gin.H{"error": "Asset access ticket does not match asset"})
			return "", false
		}
		if !ticket.MatchesTransform(params) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Asset access ticket does not match derivative"})
			return "", false
		}
		return ticket.UserID, true
	}

	if params != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "Asset derivative URL requires a signed access ticket"})
		return "", false
	}

	reqWithUser := s.injectUserIntoContext(c)
	if authenticationValidationError(reqWithUser.Context()) != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Authentication service temporarily unavailable"})
		return "", false
	}
	user := authctx.ForContext(reqWithUser.Context())
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return "", false
	}
	return user.Id, true
}

// serveTransformedAsset handles the common logic for serving transformed images.
// It parses the signed path, checks cache, fetches the asset, transforms it, and serves the result.
func (s *HTTPServer) serveTransformedAsset(c *gin.Context, req transformRequest) {
	// Parse and verify the signed path
	params, err := signedurl.ParseSignedTransformPath(s.config.Core.Assets.SigningSecret, req.ResourceID1, req.ResourceID2, req.SignedPath)
	if err != nil {
		s.logger.Warn("Invalid transform path",
			"resource_id1", req.ResourceID1,
			"resource_id2", req.ResourceID2,
			"error", err)
		c.JSON(http.StatusForbidden, gin.H{"error": "Invalid or expired transform URL"})
		return
	}

	s.serveTransformedAssetWithParams(c, req, params)
}

func (s *HTTPServer) serveTransformedAssetWithParams(c *gin.Context, req transformRequest, params *signedurl.TransformParams) {
	ctx := c.Request.Context()

	// Build cache key with prefix to distinguish between asset types
	cacheKey := core.ImageCacheKey(req.CachePrefix, req.AssetID, params.Width, params.Height, params.Fit)

	// Try cache first
	if cached, err := s.core.GetCachedResize(ctx, cacheKey); err == nil && cached != nil {
		s.logger.Debug("Cache hit for transformed asset",
			"asset_id", req.AssetID,
			"cache_key", cacheKey)

		// Still need to check authorization if required
		if req.Authorize != nil && !req.Authorize(c) {
			return
		}

		c.Header("Cache-Control", transformedAssetCacheControl(req.Authorize == nil))
		c.Header("ETag", fmt.Sprintf("\"%s-%d-%d-%s\"", req.AssetID, params.Width, params.Height, params.Fit))
		c.Header("Vary", transformedAssetVary(req.Authorize == nil))
		c.Header("X-Cache", "HIT")
		c.Data(http.StatusOK, assets.DetectImageContentType(cached), cached)
		return
	}

	// Cache miss - fetch the asset first
	// (FetchAsset may cache metadata like room ID needed by Authorize)
	reader, contentType, err := req.FetchAsset(ctx)
	if err != nil {
		s.logger.Error("Failed to get asset", "error", err, "asset_id", req.AssetID)
		c.JSON(http.StatusNotFound, gin.H{"error": "Asset not found"})
		return
	}
	// Close the reader if it implements io.Closer
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}

	// Check authorization after fetching (Authorize can use metadata cached by FetchAsset)
	if req.Authorize != nil && !req.Authorize(c) {
		return
	}

	// Check if content type is an image
	if contentType == "" || !isImageContentType(contentType) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Asset is not an image"})
		return
	}

	// Read asset data into bytes for transformation
	data, err := io.ReadAll(reader)
	if err != nil {
		s.logger.Error("Failed to read asset", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read asset"})
		return
	}

	// Transform the image. 【本地改动 32e1f566】AVIF 输入需要 ffmpeg 解码
	// (Go 标准库不认识 AVIF),用的就是上传阶段 AVIF 重编码那同一个二进制。
	var result *assets.TransformResult
	if req.JPEGQuality > 0 {
		result, err = assets.TransformImageWithFFmpeg(data, params.Width, params.Height, assets.FitMode(params.Fit), assets.TransformOptions{
			JPEGQuality: req.JPEGQuality,
		}, s.core.AssetsConfig().FFmpegPath)
	} else {
		result, err = assets.TransformImageWithFFmpeg(data, params.Width, params.Height, assets.FitMode(params.Fit), assets.TransformOptions{
			JPEGQuality: assets.DefaultTransformJPEGQuality,
		}, s.core.AssetsConfig().FFmpegPath)
	}
	if err != nil {
		s.logger.Error("Failed to transform image", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to transform image"})
		return
	}

	// Read transformed bytes for caching and response
	transformedData, err := io.ReadAll(result.Reader)
	if err != nil {
		s.logger.Error("Failed to read transformed image", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read transformed image"})
		return
	}

	// Store in cache (fire-and-forget, skip animated GIFs which are large)
	if result.ContentType != "image/gif" && s.core.ImageCacheEnabled() {
		go func() {
			if err := s.core.StoreCachedResize(context.Background(), cacheKey, transformedData); err != nil {
				s.logger.Warn("Failed to cache transformed image", "error", err, "cache_key", cacheKey)
			}
		}()
	}

	// Set cache headers for long-term caching (immutable content)
	c.Header("Cache-Control", transformedAssetCacheControl(req.Authorize == nil))
	c.Header("ETag", fmt.Sprintf("\"%s-%d-%d-%s\"", req.AssetID, params.Width, params.Height, params.Fit))
	c.Header("Vary", transformedAssetVary(req.Authorize == nil))
	c.Header("X-Cache", "MISS")

	// Serve the transformed image with appropriate content type
	c.Data(http.StatusOK, result.ContentType, transformedData)
}

func transformedAssetCacheControl(public bool) string {
	if public {
		return "public, max-age=31536000, immutable"
	}
	return protectedAssetCacheControl
}

func transformedAssetVary(public bool) string {
	// 【本地改动 2026-08-23】两种分支统一为 Accept-Encoding：响应字节只由
	// assetID + transform 参数决定，凭据只是访问门控而非表示选择器；
	// per-user ticket 路由本身 no-store，按凭据分片缓存毫无收益。
	return "Accept-Encoding"
}

// serveTransformedServerAsset serves a dynamically transformed version of an server asset.
// URL format: /assets/server/{key}/t/{signedPath}
// Called by serveServerAsset when it detects a transform pattern in the path.
// Opens only the backend object bound by pre-cache public classification.
func (s *HTTPServer) serveTransformedServerAsset(c *gin.Context, key, signedPath string, location *core.PublicServerAssetLocation) {
	s.logger.Debug("Serving transformed server asset", "asset_id", key, "signed_path", signedPath)

	s.serveTransformedAsset(c, transformRequest{
		ResourceID1: core.ServerAssetSignResource,
		ResourceID2: key,
		SignedPath:  signedPath,
		CachePrefix: core.ServerAssetSignResource,
		AssetID:     key,
		FetchAsset: func(ctx context.Context) (io.Reader, string, error) {
			reader, info, err := s.core.GetPublicServerAsset(ctx, location)
			if err != nil {
				s.logger.Debug("Failed to fetch server asset",
					"asset_id", key,
					"error", err)
				return nil, "", err
			}
			contentType := info.ContentType
			if contentType == "" {
				contentType = getContentType(key)
				s.logger.Debug("Content type from header is empty, using extension-based fallback",
					"asset_id", key,
					"fallback_content_type", contentType)
			}
			s.logger.Debug("Fetched server asset",
				"asset_id", key,
				"content_type", contentType,
				"size", info.Size)
			return reader, contentType, nil
		},
		Authorize: nil, // Instance assets are public
	})
}

// isImageContentType checks if the content type is an image.
func isImageContentType(contentType string) bool {
	// 【本地改动 32e1f566】识别 image/avif:上传阶段可能把附件转成 AVIF,
	// 渲染/下载路径必须当作图片处理。
	return contentType == "image/jpeg" ||
		contentType == "image/png" ||
		contentType == "image/gif" ||
		contentType == "image/webp" ||
		contentType == "image/avif"
}

// getContentType returns the MIME type based on file extension.
func getContentType(path string) string {
	ext := filepath.Ext(path)
	switch ext {
	case ".webp":
		return "image/webp"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".avif":
		return "image/avif"
	default:
		return "application/octet-stream"
	}
}
