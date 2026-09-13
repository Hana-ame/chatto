package http_server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"hmans.de/chatto/internal/config"
	"hmans.de/chatto/internal/core"
	evtv1 "hmans.de/chatto/internal/pb/chatto/core/evt/v1"
	"hmans.de/chatto/internal/testutil"
)

func TestExtractImmutableETag(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		expected string
	}{
		{
			name:     "entry with hash",
			path:     "/_app/immutable/entry/start.CxnbWTuF.js",
			expected: "CxnbWTuF",
		},
		{
			name:     "chunk with hash only",
			path:     "/_app/immutable/chunks/Dynhoydm.js",
			expected: "Dynhoydm",
		},
		{
			name:     "CSS with hash",
			path:     "/_app/immutable/assets/app.D2jh4_eq.css",
			expected: "D2jh4_eq",
		},
		{
			name:     "nested path with hash",
			path:     "/_app/immutable/nodes/0.BFpGYTTP.js",
			expected: "BFpGYTTP",
		},
		{
			name:     "entry app with hash",
			path:     "/_app/immutable/entry/app.BR6S17SI.js",
			expected: "BR6S17SI",
		},
		{
			name:     "woff2 font with hash",
			path:     "/_app/immutable/assets/ibm-plex-sans-latin-wght-normal.IvpUnPaZ.woff2",
			expected: "IvpUnPaZ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractImmutableETag(tt.path)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestImmutableAssetCaching(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Create a minimal test router that simulates our caching middleware
	router := gin.New()

	router.Use(setFrontendCacheHeaders)

	// Add a simple handler that returns content
	router.GET("/*path", func(c *gin.Context) {
		c.String(http.StatusOK, "file content")
	})

	t.Run("immutable asset returns correct headers", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/_app/immutable/entry/start.CxnbWTuF.js", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, cacheControlImmutable, w.Header().Get("Cache-Control"))
		assert.Equal(t, `"CxnbWTuF"`, w.Header().Get("ETag"))
	})

	t.Run("non-immutable asset returns no-cache", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index.html", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, cacheControlNoCache, w.Header().Get("Cache-Control"))
		assert.Empty(t, w.Header().Get("ETag"))
	})

	t.Run("conditional request with matching ETag returns 304", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/_app/immutable/entry/start.CxnbWTuF.js", nil)
		req.Header.Set("If-None-Match", `"CxnbWTuF"`)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotModified, w.Code)
		assert.Empty(t, w.Body.String()) // 304 should have no body
	})

	t.Run("conditional request with weak ETag returns 304", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/_app/immutable/entry/start.CxnbWTuF.js", nil)
		req.Header.Set("If-None-Match", `W/"CxnbWTuF"`)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotModified, w.Code)
	})

	t.Run("conditional request with non-matching ETag returns 200", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/_app/immutable/entry/start.CxnbWTuF.js", nil)
		req.Header.Set("If-None-Match", `"different-etag"`)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.NotEmpty(t, w.Body.String())
	})

	t.Run("_app non-immutable returns no-cache", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/_app/version.json", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, cacheControlNoCache, w.Header().Get("Cache-Control"))
		assert.Empty(t, w.Header().Get("ETag"))
	})

	t.Run("service worker returns revalidate cache policy", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/service-worker.js", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, cacheControlRevalidate, w.Header().Get("Cache-Control"))
		assert.Empty(t, w.Header().Get("ETag"))
	})
}

func TestImmutableFrontendAssetNeverCarriesAuthenticationCookies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	chattoCore := setupFrontendTestCoreWithLogo(t)
	ctx := testContext(t)
	user, err := chattoCore.CreateUser(ctx, core.SystemActorID, "immutable-cookie-user", "Immutable Cookie User", "password123")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	sessionID, _, err := chattoCore.CreateCookieSession(ctx, user.GetId(), "password_login")
	if err != nil {
		t.Fatalf("CreateCookieSession: %v", err)
	}

	router := gin.New()
	store := cookie.NewStore([]byte("test-secret-key-32-bytes-long!!"))
	router.Use(sessions.Sessions("chatto_session", store))
	server := &HTTPServer{
		config: config.ChattoConfig{
			Webserver: config.WebserverConfig{URL: "https://example.com"},
		},
		core:   chattoCore,
		router: router,
	}
	router.Use(server.csrfMiddleware())
	if err := server.setupFrontendRoutes(); err != nil {
		t.Fatalf("setupFrontendRoutes: %v", err)
	}

	assetRequest := httptest.NewRequest(http.MethodGet, "/_app/immutable/entry/start.CxnbWTuF.js", nil)
	assetRequest.AddCookie(&http.Cookie{Name: browserSessionCookieName, Value: sessionID})
	assetResponse := httptest.NewRecorder()
	router.ServeHTTP(assetResponse, assetRequest)

	if values := assetResponse.Header().Values("Set-Cookie"); len(values) != 0 {
		t.Fatalf("immutable asset Set-Cookie = %v, want none", values)
	}
	if got := assetResponse.Header().Get("Cache-Control"); got != cacheControlImmutable {
		t.Fatalf("immutable asset Cache-Control = %q, want %q", got, cacheControlImmutable)
	}
}

func TestServiceWorkerETag(t *testing.T) {
	gin.SetMode(gin.TestMode)

	content := []byte("self.addEventListener('fetch', () => {});")
	etag := serviceWorkerETag(content)

	router := gin.New()
	router.Use(setFrontendCacheHeaders)
	router.GET("/service-worker.js", func(c *gin.Context) {
		if setServiceWorkerETag(c, content) {
			return
		}
		c.Data(http.StatusOK, "application/javascript", content)
	})

	t.Run("returns etag", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/service-worker.js", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, cacheControlRevalidate, w.Header().Get("Cache-Control"))
		assert.Equal(t, etag, w.Header().Get("ETag"))
		assert.Equal(t, string(content), w.Body.String())
	})

	t.Run("matching if none match returns 304", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/service-worker.js", nil)
		req.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusNotModified, w.Code)
		assert.Equal(t, cacheControlRevalidate, w.Header().Get("Cache-Control"))
		assert.Equal(t, etag, w.Header().Get("ETag"))
		assert.Empty(t, w.Body.String())
	})
}

func TestDynamicPWAManifest(t *testing.T) {
	staticManifest := []byte(`{
  "name": "Chatto",
  "short_name": "Chatto",
  "icons": [
    { "src": "/icons/icon-192.png", "sizes": "192x192", "type": "image/png" },
    { "src": "/icons/icon-512.png", "sizes": "512x512", "type": "image/png" }
  ],
  "shortcuts": [
    {
      "name": "Open Chatto",
      "icons": [{ "src": "/icons/icon-192.png", "sizes": "192x192", "type": "image/png" }]
    }
  ]
}`)

	t.Run("uses server name without requiring a server logo", func(t *testing.T) {
		got, err := dynamicPWAManifest(staticManifest, "Engineering", nil)
		if err != nil {
			t.Fatalf("dynamicPWAManifest: %v", err)
		}

		var manifest map[string]any
		if err := json.Unmarshal(got, &manifest); err != nil {
			t.Fatalf("unmarshal manifest: %v", err)
		}
		assert.Equal(t, "Engineering", manifest["name"])
		assert.Equal(t, "Engineering", manifest["short_name"])
		assert.Len(t, manifest["icons"], 2)
	})

	t.Run("replaces install and shortcut icons with server logo URLs", func(t *testing.T) {
		got, err := dynamicPWAManifest(staticManifest, "Engineering", &pwaServerIconURLs{
			Icon192: "/assets/server/logo/t/192",
			Icon512: "/assets/server/logo/t/512",
		})
		if err != nil {
			t.Fatalf("dynamicPWAManifest: %v", err)
		}

		var manifest map[string]any
		if err := json.Unmarshal(got, &manifest); err != nil {
			t.Fatalf("unmarshal manifest: %v", err)
		}
		assert.Equal(t, "Engineering", manifest["name"])
		assert.Equal(t, "Engineering", manifest["short_name"])

		icons := manifest["icons"].([]any)
		assert.Len(t, icons, 4)
		assert.Equal(t, "/assets/server/logo/t/192", icons[0].(map[string]any)["src"])
		assert.Equal(t, "192x192", icons[0].(map[string]any)["sizes"])
		assert.Equal(t, "image/png", icons[0].(map[string]any)["type"])
		assert.Equal(t, "/assets/server/logo/t/512", icons[1].(map[string]any)["src"])
		assert.Equal(t, "image/png", icons[1].(map[string]any)["type"])
		assert.Equal(t, "maskable", icons[2].(map[string]any)["purpose"])
		assert.Equal(t, "image/png", icons[2].(map[string]any)["type"])
		assert.Equal(t, "maskable", icons[3].(map[string]any)["purpose"])
		assert.Equal(t, "image/png", icons[3].(map[string]any)["type"])

		shortcuts := manifest["shortcuts"].([]any)
		shortcutIcons := shortcuts[0].(map[string]any)["icons"].([]any)
		assert.Equal(t, "/assets/server/logo/t/192", shortcutIcons[0].(map[string]any)["src"])
		assert.Equal(t, "image/png", shortcutIcons[0].(map[string]any)["type"])
	})
}

func TestSameOriginServerAssetURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "keeps relative server asset URL",
			url:  "/assets/server/logo/t/signed",
			want: "/assets/server/logo/t/signed",
		},
		{
			name: "removes external asset origin",
			url:  "https://assets.example.com/assets/server/logo/t/signed?variant=pwa",
			want: "/assets/server/logo/t/signed?variant=pwa",
		},
		{
			name: "rejects unrelated asset URL",
			url:  "https://assets.example.com/assets/files/private",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sameOriginServerAssetURL(tt.url))
		})
	}
}

func TestClientAcceptsEncoding(t *testing.T) {
	tests := []struct {
		name           string
		acceptEncoding string
		encoding       string
		expected       bool
	}{
		{
			name:           "accepts brotli in list",
			acceptEncoding: "gzip, deflate, br",
			expected:       true,
			encoding:       "br",
		},
		{
			name:           "accepts gzip in list",
			acceptEncoding: "gzip, deflate, br",
			expected:       true,
			encoding:       "gzip",
		},
		{
			name:           "single encoding",
			acceptEncoding: "br",
			expected:       true,
			encoding:       "br",
		},
		{
			name:           "not in list",
			acceptEncoding: "gzip, deflate",
			expected:       false,
			encoding:       "br",
		},
		{
			name:           "empty header",
			acceptEncoding: "",
			expected:       false,
			encoding:       "br",
		},
		{
			name:           "with quality values",
			acceptEncoding: "gzip;q=1.0, br;q=0.8, *;q=0.1",
			expected:       true,
			encoding:       "br",
		},
		{
			name:           "no spaces",
			acceptEncoding: "gzip,deflate,br",
			expected:       true,
			encoding:       "br",
		},
		{
			name:           "zero quality rejects encoding",
			acceptEncoding: "gzip, br;q=0",
			expected:       false,
			encoding:       "br",
		},
		{
			name:           "wildcard accepts encoding",
			acceptEncoding: "*;q=0.5",
			expected:       true,
			encoding:       "br",
		},
		{
			name:           "specific rejection overrides wildcard",
			acceptEncoding: "*;q=1, br;q=0",
			expected:       false,
			encoding:       "br",
		},
		{
			name:           "encoding names are case insensitive",
			acceptEncoding: "GZip",
			expected:       true,
			encoding:       "gzip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := clientAcceptsEncoding(tt.acceptEncoding, tt.encoding)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func gzipFrontendTestData(t *testing.T, content []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(content); err != nil {
		t.Fatalf("write gzip test data: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip test data: %v", err)
	}
	return compressed.Bytes()
}

func TestReadFrontendIdentityFile(t *testing.T) {
	original := []byte("console.log('embedded');")
	compressed := gzipFrontendTestData(t, original)

	t.Run("prefers raw file when present", func(t *testing.T) {
		clientFS := fstest.MapFS{
			"app.js":    &fstest.MapFile{Data: original},
			"app.js.gz": &fstest.MapFile{Data: []byte("not gzip")},
		}
		content, err := readFrontendIdentityFile(clientFS, "app.js")
		assert.NoError(t, err)
		assert.Equal(t, original, content)
	})

	t.Run("inflates gzip when raw file was omitted", func(t *testing.T) {
		clientFS := fstest.MapFS{
			"app.js.gz": &fstest.MapFile{Data: compressed},
		}
		content, err := readFrontendIdentityFile(clientFS, "app.js")
		assert.NoError(t, err)
		assert.Equal(t, original, content)
		assert.True(t, frontendFileExists(clientFS, "app.js"))
	})

	t.Run("rejects corrupt gzip fallback", func(t *testing.T) {
		clientFS := fstest.MapFS{
			"app.js.gz": &fstest.MapFile{Data: []byte("not gzip")},
		}
		_, err := readFrontendIdentityFile(clientFS, "app.js")
		assert.Error(t, err)
	})

	t.Run("reports missing file", func(t *testing.T) {
		clientFS := fstest.MapFS{}
		_, err := readFrontendIdentityFile(clientFS, "app.js")
		assert.Error(t, err)
		assert.False(t, frontendFileExists(clientFS, "app.js"))
	})
}

func TestServeFrontendFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	original := []byte("console.log('embedded');")
	brotli := []byte("brotli representation")
	compressed := gzipFrontendTestData(t, original)
	clientFS := fstest.MapFS{
		"app.js.br": &fstest.MapFile{Data: brotli},
		"app.js.gz": &fstest.MapFile{Data: compressed},
	}

	tests := []struct {
		name           string
		acceptEncoding string
		wantBody       []byte
		wantEncoding   string
	}{
		{
			name:           "serves Brotli when accepted",
			acceptEncoding: "gzip, br",
			wantBody:       brotli,
			wantEncoding:   "br",
		},
		{
			name:           "serves gzip when accepted",
			acceptEncoding: "gzip",
			wantBody:       compressed,
			wantEncoding:   "gzip",
		},
		{
			name:     "inflates gzip for identity client",
			wantBody: original,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/app.js", func(c *gin.Context) {
				if err := serveFrontendFile(c, clientFS, "app.js"); err != nil {
					c.Status(http.StatusInternalServerError)
				}
			})
			req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
			if tt.acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)

			assert.Equal(t, http.StatusOK, response.Code)
			assert.Equal(t, tt.wantBody, response.Body.Bytes())
			assert.Equal(t, tt.wantEncoding, response.Header().Get("Content-Encoding"))
			assert.Equal(t, "Accept-Encoding", response.Header().Get("Vary"))
			assert.Contains(t, response.Header().Get("Content-Type"), "javascript")
		})
	}
}

func TestServeSPAFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Create a minimal HTTPServer for testing
	newTestServer := func() *HTTPServer {
		return &HTTPServer{
			config: config.ChattoConfig{Webserver: config.WebserverConfig{URL: "https://example.com"}},
		}
	}

	t.Run("returns 200 with content when 200.html exists", func(t *testing.T) {
		mockFS := fstest.MapFS{
			"200.html": &fstest.MapFile{
				Data: []byte("<!DOCTYPE html><html><head><!-- OG_META_PLACEHOLDER --></head><body>SPA</body></html>"),
			},
		}

		server := newTestServer()
		router := gin.New()
		router.GET("/test", func(c *gin.Context) {
			server.serveSPAFallback(c, mockFS)
		})

		req := httptest.NewRequest("GET", "/test", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), "SPA")
		assert.Equal(t, "text/html; charset=utf-8", w.Header().Get("Content-Type"))
	})

	t.Run("injects OpenGraph tags", func(t *testing.T) {
		mockFS := fstest.MapFS{
			"200.html": &fstest.MapFile{
				Data: []byte("<!DOCTYPE html><html><head><!-- OG_META_PLACEHOLDER --></head><body>SPA</body></html>"),
			},
		}

		server := newTestServer()
		router := gin.New()
		router.GET("/test", func(c *gin.Context) {
			server.serveSPAFallback(c, mockFS)
		})

		req := httptest.NewRequest("GET", "/test", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		// Should contain OpenGraph tags
		assert.Contains(t, w.Body.String(), `og:title`)
		assert.Contains(t, w.Body.String(), `og:description`)
		assert.Contains(t, w.Body.String(), `twitter:card`)
		// Placeholder should be replaced
		assert.NotContains(t, w.Body.String(), "OG_META_PLACEHOLDER")
	})

	t.Run("returns 500 when 200.html is missing", func(t *testing.T) {
		// Empty filesystem - no 200.html
		mockFS := fstest.MapFS{}

		server := newTestServer()
		router := gin.New()
		router.GET("/test", func(c *gin.Context) {
			server.serveSPAFallback(c, mockFS)
		})

		req := httptest.NewRequest("GET", "/test", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, "Failed to load application", w.Body.String())
	})
}

func TestBrowserIconRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newServer := func(t *testing.T, chattoCore *core.ChattoCore) *HTTPServer {
		t.Helper()
		server := &HTTPServer{
			config: config.ChattoConfig{Webserver: config.WebserverConfig{URL: "https://example.com"}},
			core:   chattoCore,
			router: gin.New(),
		}
		if err := server.setupFrontendRoutes(); err != nil {
			t.Fatalf("setupFrontendRoutes: %v", err)
		}
		return server
	}

	t.Run("redirects to the original server logo", func(t *testing.T) {
		chattoCore := setupFrontendTestCoreWithLogo(t)
		chattoCore.AssetBaseURL = "https://assets.example.com"
		server := newServer(t, chattoCore)

		// 【本地改动 2026-09-13】fork 取消服务端资产衍生图:logo/头像/banner/链接
		// 预览在上传时就缩放到上限并压缩(assets.processServerAssetImage),请求期
		// 不再缩放,所以 favicon(32) 与 apple-touch-icon(180) 请求的尺寸被丢弃,
		// 两个路由都 307 到**同一条**原档 URL。上游原本是两个不同尺寸的
		// transform,这里改为断言它们相等——浏览器自己降采样 512x512 的 logo。
		//
		// 发现背景:上游断言 Location 以 /assets/server/logo-asset/t/ 开头并用
		// ParseSignedTransformPath 解出尺寸;URL 生成层 override 成原档链接后
		// 签名段不存在,原断言必然红。
		// 回归提示:若 fork 将来恢复请求期缩放,必须把本断言改回「两个不同尺寸
		// 的签名 transform」。
		locations := make(map[string]string)
		for _, iconPath := range []string{"/favicon", "/apple-touch-icon"} {
			req := httptest.NewRequest(http.MethodGet, iconPath, nil)
			w := httptest.NewRecorder()
			server.router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
			assert.Equal(t, cacheControlNoCache, w.Header().Get("Cache-Control"))
			location := w.Header().Get("Location")
			assert.True(t, strings.HasPrefix(location, "/assets/server/logo-asset"), "location = %q", location)
			assert.NotContains(t, location, "assets.example.com")
			assert.NotContains(t, location, "/t/", "fork issues no server asset transform URL: %q", location)
			locations[iconPath] = location
		}
		assert.Equal(t, locations["/apple-touch-icon"], locations["/favicon"],
			"fork discards the requested icon size, so both routes point at the original logo")
	})

	t.Run("redirects to embedded icons when no server logo exists", func(t *testing.T) {
		server := newServer(t, nil)
		tests := map[string]string{
			"/favicon":          "/icons/favicon.png",
			"/apple-touch-icon": "/icons/apple-touch-icon.png",
		}
		for iconPath, fallbackPath := range tests {
			req := httptest.NewRequest(http.MethodGet, iconPath, nil)
			w := httptest.NewRecorder()
			server.router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusTemporaryRedirect, w.Code)
			assert.Equal(t, fallbackPath, w.Header().Get("Location"))
		}
	})
}

func TestServePWAWebManifestUsesServerLogoWhenAvailable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockFS := fstest.MapFS{
		"manifest.webmanifest": &fstest.MapFile{
			Data: []byte(`{
  "name": "Chatto",
  "icons": [{ "src": "/icons/icon-192.png", "sizes": "192x192", "type": "image/png" }],
  "shortcuts": [
    { "name": "Open Chatto", "icons": [{ "src": "/icons/icon-192.png", "sizes": "192x192", "type": "image/png" }] }
  ]
}`),
		},
	}
	chattoCore := setupFrontendTestCoreWithLogo(t)
	setTestServerName(t, context.Background(), chattoCore, "Engineering")
	chattoCore.AssetBaseURL = "https://assets.example.com"
	server := &HTTPServer{
		config: config.ChattoConfig{Webserver: config.WebserverConfig{URL: "https://example.com"}},
		core:   chattoCore,
		router: gin.New(),
	}
	server.router.GET("/manifest.webmanifest", func(c *gin.Context) {
		server.servePWAWebManifest(c, mockFS)
	})

	req := httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	w := httptest.NewRecorder()
	server.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/manifest+json", w.Header().Get("Content-Type"))

	var manifest map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	assert.Equal(t, "Engineering", manifest["name"])
	assert.Equal(t, "Engineering", manifest["short_name"])
	icons := manifest["icons"].([]any)
	// 【本地改动 2026-09-13】fork 取消服务端资产衍生图:manifest 的 192x192 与
	// 512x512 图标 URL 都指向同一条原档 logo 链接(尺寸被丢弃,不含 /t/),
	// 浏览器自己降采样。上游原本是两个不同尺寸的签名 transform。
	assert.True(t, strings.HasPrefix(icons[0].(map[string]any)["src"].(string), "/assets/server/logo-asset"))
	assert.NotContains(t, icons[0].(map[string]any)["src"], "/t/", "fork issues no server asset transform URL")
	assert.NotContains(t, icons[0].(map[string]any)["src"], "assets.example.com")
	assert.Equal(t, "192x192", icons[0].(map[string]any)["sizes"])
	assert.Equal(t, "image/png", icons[0].(map[string]any)["type"])
	assert.Equal(t, "maskable", icons[2].(map[string]any)["purpose"])
	assert.Equal(t, "image/png", icons[2].(map[string]any)["type"])
}

func TestFrontendFallbackDoesNotServeReservedBackendPrefixes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := &HTTPServer{
		config: config.ChattoConfig{Webserver: config.WebserverConfig{URL: "https://example.com"}},
		router: gin.New(),
	}
	sessionStore := cookie.NewStore([]byte("test-secret-key-32-bytes-long!!"))
	server.router.Use(sessions.Sessions("chatto_session", sessionStore))
	if err := server.setupFrontendRoutes(); err != nil {
		t.Fatalf("setupFrontendRoutes: %v", err)
	}

	tests := []string{
		"/api/unknown",
		"/auth/unknown",
		"/assets/unknown",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			server.router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusNotFound, w.Code)
			assert.NotContains(t, w.Body.String(), "<!DOCTYPE html>")
		})
	}
}

func setupFrontendTestCoreWithLogo(t *testing.T) *core.ChattoCore {
	t.Helper()

	_, nc := testutil.StartSharedNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	chattoCore, err := core.NewChattoCore(ctx, nc, config.CoreConfig{
		SecretKey: "test-core-secret",
		Assets: config.AssetsConfig{
			SigningSecret: "test-signing-secret",
		},
	})
	if err != nil {
		t.Fatalf("NewChattoCore: %v", err)
	}
	startCoreServices(t, chattoCore)

	logo := &evtv1.AssetRecord{
		Id:          "logo-asset",
		Filename:    "logo.webp",
		ContentType: "image/webp",
		Storage:     &evtv1.AssetRecord_Nats{Nats: &evtv1.NATSAsset{Key: "logo-asset"}},
	}
	if err := chattoCore.SetServerLogo(ctx, core.SystemActorID, logo); err != nil {
		t.Fatalf("SetServerLogo: %v", err)
	}
	return chattoCore
}

func TestFrontendFallbackAllowsRoutesWithReservedPrefixNames(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := &HTTPServer{
		config: config.ChattoConfig{Webserver: config.WebserverConfig{URL: "https://example.com"}},
		router: gin.New(),
	}
	sessionStore := cookie.NewStore([]byte("test-secret-key-32-bytes-long!!"))
	server.router.Use(sessions.Sessions("chatto_session", sessionStore))
	if err := server.setupFrontendRoutes(); err != nil {
		t.Fatalf("setupFrontendRoutes: %v", err)
	}

	tests := []string{
		"/apiary",
		"/author",
		"/assets-gallery",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			server.router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, strings.ToLower(w.Body.String()), "<!doctype html>")
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Create a minimal test router that simulates our security headers middleware
	router := gin.New()

	// Add the security headers middleware (same as in setupFrontendRoutes)
	router.Use(func(c *gin.Context) {
		setFrontendSecurityHeaders(c)
		c.Next()
	})

	router.GET("/*path", func(c *gin.Context) {
		c.String(http.StatusOK, "content")
	})

	t.Run("security headers are set", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/index.html", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
		assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
		assert.Equal(t, "strict-origin-when-cross-origin", w.Header().Get("Referrer-Policy"))
		assert.Equal(t, "frame-ancestors 'none'", w.Header().Get("Content-Security-Policy"))
		assert.Empty(t, w.Header().Get("Content-Security-Policy-Report-Only"))
	})
}
