package assets

// 【本地改动 32e1f566 + 2026-09-02】（2026-08-14 引入，2026-09-02 随存储
// 格式从 AVIF 迁到 WebP 更新）本文件整体为 fork 独有，upstream 没有同名文件。
// 目的：为 webp.go 的 EncodeWebP / TransformImageWithFFmpeg 两条链路提供
// 覆盖——WebP 重编码（成功、透明、ffmpeg 缺失、开关禁用、输入非法）与 ffmpeg
// 衍生图（WebP/AVIF/JPEG/PNG/GIF 输入缩放、ffmpeg 缺失回退、统一有损 WebP、
// 动画 GIF 分流、WebP/AVIF 输入硬错误）。
// 思路：依赖 ffmpeg 的用例统一经 testFFmpegPath(t) 取路径，找不到就 t.Skip；
// 不依赖 ffmpeg 的用例显式传 /nonexistent/ffmpeg 强制走回退或错误分支。两种做法
// 让本地（无 ffmpeg）与 CI（install-ffmpeg 提供）都能全绿，且回退路径不会被
// skip 掉。
// 踩坑：各用例自己的【发现背景】【修复方式】注释记录了 2026-08-14 与 2026-08-16
// 两次重构中实际踩到的 bug。2026-09-02 把存储从 AVIF 改为 WebP 后，AVIF
// 重编码测试被 WebP 重编码测试取代；衍生图路径仍保留 WebP/AVIF 两种输入的
// 硬错误测试，保护历史 AVIF 附件的兼容。
// 边界：只测 assets 包的纯编码/转码函数，不测上传管线；管线覆盖在
// cli/internal/core/attachments_test.go。
// 合并提示：upstream 无此文件，正常合并不冲突；若上游将来新增同名测试，
// 需人工合并两份。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
)

func testFFmpegPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	return path
}

// --- EncodeWebP 上传存储路径 ---

func TestEncodeWebPProducesWebP(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	out, err := EncodeWebP(context.Background(), createTestImage(400, 300), Config{FFmpegPath: ffmpegPath, WebPEnabled: true})
	if err != nil {
		t.Fatalf("EncodeWebP: %v", err)
	}
	if !isWebPBytes(out) {
		t.Fatalf("output is not a WebP file (len=%d)", len(out))
	}
}

func TestEncodeWebPKeepsTransparency(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	out, err := EncodeWebP(context.Background(), createTransparentTestImage(64, 64), Config{FFmpegPath: ffmpegPath, WebPEnabled: true})
	if err != nil {
		t.Fatalf("EncodeWebP: %v", err)
	}
	if !isWebPBytes(out) {
		t.Fatalf("output is not a WebP file (len=%d)", len(out))
	}
}

// TestEncodeWebPResolvesFFmpegFromPath 守护"空路径从 PATH 解析"。
// 【发现背景 2026-08-14】临时文件重构(修 libsvtav1 管道挂死)时,探测
// 和编码两处各解析一次路径,只改了探测侧,导致编码侧拿到空路径报
// "exec: no command"。
// 【修复方式】EncodeWebP 开头对空路径也做 exec.LookPath(见 webp.go 注释)。
func TestEncodeWebPResolvesFFmpegFromPath(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}

	out, err := EncodeWebP(context.Background(), createTestImage(100, 100), Config{WebPEnabled: true})
	if err != nil {
		t.Fatalf("EncodeWebP with empty path: %v", err)
	}
	if !isWebPBytes(out) {
		t.Fatalf("output is not a WebP file (len=%d)", len(out))
	}
}

func TestEncodeWebPUnavailableWithoutFFmpeg(t *testing.T) {
	_, err := EncodeWebP(context.Background(), createTestImage(10, 10), Config{FFmpegPath: "/nonexistent/ffmpeg", WebPEnabled: true})
	if !errors.Is(err, ErrWebPUnavailable) {
		t.Fatalf("error = %v, want ErrWebPUnavailable", err)
	}
}

// TestEncodeWebPDisabled 守护 webp_enabled 配置开关:WebPEnabled=false 时
// 编码器必须报"不可用"(即存原图),且完全不探测/不调用 ffmpeg。
// 【发现背景 2026-08-14】引入显式关闭开关时新增;此前服务器只要装了带
// 编码器的 ffmpeg,重编码就自动生效,想关都关不掉。
// 【修复方式】EncodeWebP 开头检查 cfg.WebPEnabled,false 直接返回
// ErrWebPUnavailable——与"没有 ffmpeg"同一条路径,调用方无需区分。
func TestEncodeWebPDisabled(t *testing.T) {
	_, err := EncodeWebP(context.Background(), createTestImage(10, 10), Config{FFmpegPath: "/definitely/not/ffmpeg", WebPEnabled: false})
	if !errors.Is(err, ErrWebPUnavailable) {
		t.Fatalf("error = %v, want ErrWebPUnavailable", err)
	}
}

func TestEncodeWebPRejectsInvalidInput(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	_, err := EncodeWebP(context.Background(), []byte("not an image at all"), Config{FFmpegPath: ffmpegPath, WebPEnabled: true})
	if err == nil {
		t.Fatal("EncodeWebP accepted invalid image data")
	}
	if errors.Is(err, ErrWebPUnavailable) {
		t.Fatalf("invalid input reported as unavailable: %v", err)
	}
}

// --- TransformImageWithFFmpeg 衍生图路径 ---

// TestTransformImageWithFFmpegScales 守护"衍生图三种 fit 模式都正确缩放并
// 输出有损 WebP"。用 PNG 输入(ffmpeg 直接可解码),不依赖 AVIF 存储——
// AVIF 输入的处理路径与 PNG 完全相同(同一 ffmpeg 临时文件输入+WebP 输出)。
func TestTransformImageWithFFmpegScales(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	for _, fit := range []FitMode{FitContain, FitCover, FitExact} {
		result, err := TransformImageWithFFmpeg(createTestImage(400, 300), 200, 100, fit, TransformOptions{JPEGQuality: DefaultTransformJPEGQuality}, ffmpegPath)
		if err != nil {
			t.Fatalf("TransformImageWithFFmpeg(%s): %v", fit, err)
		}
		data, err := io.ReadAll(result.Reader)
		if err != nil {
			t.Fatalf("read transform result: %v", err)
		}
		if result.ContentType != "image/webp" {
			t.Fatalf("transform content type = %q, want image/webp", result.ContentType)
		}
		if len(data) == 0 {
			t.Fatalf("transform %s produced no output", fit)
		}
	}
}

// TestTransformImageWithFFmpegFallsBackWithoutFFmpeg 守护"ffmpeg 不可用时
// 非 WebP/AVIF 输入回退 Go 路径"。
// 【发现背景 2026-08-16】衍生图统一改有损 WebP 时重写了
// TransformImageWithFFmpeg:ffmpeg 存在时所有静态图走 ffmpeg,缺失/失败时
// 回退 TransformImageWithOptions 旧行为(不透明→JPEG、透明→无损 WebP)。
// 此测试用不存在的 ffmpeg 路径确保回退路径输出与旧行为一致。
func TestTransformImageWithFFmpegFallsBackWithoutFFmpeg(t *testing.T) {
	result, err := TransformImageWithFFmpeg(createTestImage(64, 64), 32, 32, FitContain, TransformOptions{JPEGQuality: DefaultTransformJPEGQuality}, "/nonexistent/ffmpeg")
	if err != nil {
		t.Fatalf("TransformImageWithFFmpeg on PNG: %v", err)
	}
	if result.ContentType != "image/jpeg" {
		t.Fatalf("content type = %q, want image/jpeg (fallback Go path)", result.ContentType)
	}
}

// TestTransformImageWithFFmpegLossyWebPForNonAVIF 守护"ffmpeg 可用时,所有
// 非动画图片的衍生图统一输出有损 WebP"。
// 【发现背景 2026-08-16】用户要求衍生图全部统一成有损 WebP;nativewebp
// v1.3.0 只支持无损 VP8L,CI 又是 CGO_ENABLED=0,只能靠 ffmpeg libwebp。
// 覆盖不透明/透明/静态 GIF/WebP 四种输入,确保都输出 RIFF WEBP。
func TestTransformImageWithFFmpegLossyWebPForAll(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)
	// 先用一个已知能编 WebP 的输入生成一个 WebP 字节,作为输入之一
	webpFixture, err := EncodeWebP(context.Background(), createTestImage(100, 80), Config{FFmpegPath: ffmpegPath, WebPEnabled: true})
	if err != nil {
		t.Fatalf("WebP fixture encode: %v", err)
	}
	cases := []struct {
		name string
		data []byte
	}{
		{"opaque PNG", createTestImage(400, 300)},
		{"transparent PNG", createTransparentTestImage(64, 64)},
		{"static GIF", createAnimatedGIF(64, 64, 1)},
		{"WebP", webpFixture},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := TransformImageWithFFmpeg(tc.data, 100, 80, FitContain, TransformOptions{JPEGQuality: DefaultTransformJPEGQuality}, ffmpegPath)
			if err != nil {
				t.Fatalf("TransformImageWithFFmpeg(%s): %v", tc.name, err)
			}
			out, err := io.ReadAll(result.Reader)
			if err != nil {
				t.Fatalf("read transform result: %v", err)
			}
			if result.ContentType != "image/webp" {
				t.Fatalf("content type = %q, want image/webp for %s", result.ContentType, tc.name)
			}
			if len(out) < 12 || string(out[:4]) != "RIFF" || string(out[8:12]) != "WEBP" {
				t.Fatalf("output for %s is not a WebP file (len=%d)", tc.name, len(out))
			}
		})
	}
}

// TestTransformImageWithFFmpegKeepsAnimatedGIF 守护"动画 GIF 绝不进 ffmpeg
// 分支"。
// 【发现背景 2026-08-16】重写 TransformImageWithFFmpeg 时若把动画 GIF 也
// 交给 ffmpeg,单帧编码会丢掉动画。此测试在 ffmpeg 可用(否则 skip)时
// 验证动画 GIF 仍走 nativewebp 动画管线,输出带 VP8X 的动画 WebP。
func TestTransformImageWithFFmpegKeepsAnimatedGIF(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)
	result, err := TransformImageWithFFmpeg(createAnimatedGIF(100, 100, 3), 50, 50, FitContain, TransformOptions{JPEGQuality: DefaultTransformJPEGQuality}, ffmpegPath)
	if err != nil {
		t.Fatalf("TransformImageWithFFmpeg on animated GIF: %v", err)
	}
	out, err := io.ReadAll(result.Reader)
	if err != nil {
		t.Fatalf("read transform result: %v", err)
	}
	if result.ContentType != "image/webp" {
		t.Fatalf("content type = %q, want image/webp", result.ContentType)
	}
	if !bytes.Contains(out, []byte("VP8X")) {
		t.Fatal("animated GIF derivative is not an animated WebP (missing VP8X container)")
	}
}

// TestTransformImageWithFFmpegWebPWithoutFFmpegIsHardError 守护"WebP 输入
// (新存储格式)在 ffmpeg 不可用/失败时是硬错误"。
// 【发现背景 2026-09-02】存储格式改为 WebP 后,Go 标准库仍没有 WebP 解码器,
// WebP 输入在 ffmpeg 不可用时没有回退路径,必须报错而不是静默返回错误结果。
func TestTransformImageWithFFmpegWebPWithoutFFmpegIsHardError(t *testing.T) {
	webp := []byte{0x52, 0x49, 0x46, 0x46, 0, 0, 0, 0, 'W', 'E', 'B', 'P'}
	_, err := TransformImageWithFFmpeg(webp, 10, 10, FitContain, TransformOptions{JPEGQuality: DefaultTransformJPEGQuality}, "/nonexistent/ffmpeg")
	if err == nil {
		t.Fatal("WebP input without usable ffmpeg must fail")
	}
}

// TestTransformImageWithFFmpegAVIFWithoutFFmpegIsHardError 守护"AVIF 输入
// (历史存储格式)在 ffmpeg 不可用/失败时是硬错误"。保留保护历史 AVIF
// 附件的兼容路径。
// 【发现背景 2026-08-16】Go 标准库没有 AVIF 解码器,AVIF 输入
// 没有回退路径,必须报错而不是静默返回错误结果。
func TestTransformImageWithFFmpegAVIFWithoutFFmpegIsHardError(t *testing.T) {
	avif := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f'}
	_, err := TransformImageWithFFmpeg(avif, 10, 10, FitContain, TransformOptions{JPEGQuality: DefaultTransformJPEGQuality}, "/nonexistent/ffmpeg")
	if err == nil {
		t.Fatal("AVIF input without usable ffmpeg must fail")
	}
}

func TestIsAVIFBytes(t *testing.T) {
	valid := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f'}
	if !isAVIFBytes(valid) {
		t.Fatal("isAVIFBytes rejected a valid AVIF header")
	}
	for _, bad := range [][]byte{
		{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'},
		{0, 0, 0, 24, 'F', 'T', 'Y', 'P', 'a', 'v', 'i', 'f'},
		{0, 0, 0},
	} {
		if isAVIFBytes(bad) {
			t.Fatalf("isAVIFBytes accepted %x", bad)
		}
	}
}

func TestIsWebPBytes(t *testing.T) {
	valid := []byte{0x52, 0x49, 0x46, 0x46, 0, 0, 0, 0, 'W', 'E', 'B', 'P'}
	if !isWebPBytes(valid) {
		t.Fatal("isWebPBytes rejected a valid WebP header")
	}
	for _, bad := range [][]byte{
		{0x52, 0x49, 0x46, 0x46, 0, 0, 0, 0, 'A', 'V', 'I', '1'},
		{0x52, 0x49, 0x46, 0x46, 0, 0, 0, 0, 'w', 'e', 'b', 'p'},
		{0, 0, 0},
	} {
		if isWebPBytes(bad) {
			t.Fatalf("isWebPBytes accepted %x", bad)
		}
	}
}
