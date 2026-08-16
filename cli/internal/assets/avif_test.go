package assets

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

func TestEncodeAVIFProducesAVIF(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	out, err := EncodeAVIF(context.Background(), createTestImage(400, 300), Config{FFmpegPath: ffmpegPath, AVIFEnabled: true})
	if err != nil {
		t.Fatalf("EncodeAVIF: %v", err)
	}
	if !isAVIFBytes(out) {
		t.Fatalf("output is not an AVIF file (len=%d)", len(out))
	}
}

func TestEncodeAVIFKeepsTransparency(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	out, err := EncodeAVIF(context.Background(), createTransparentTestImage(64, 64), Config{FFmpegPath: ffmpegPath, AVIFEnabled: true})
	if err != nil {
		t.Fatalf("EncodeAVIF: %v", err)
	}
	if !isAVIFBytes(out) {
		t.Fatalf("output is not an AVIF file (len=%d)", len(out))
	}
}

// TestEncodeAVIFResolvesFFmpegFromPath 守护"空路径从 PATH 解析"。
// 【发现背景 2026-08-14】临时文件重构(修 libsvtav1 管道挂死)时,探测
// 和编码两处各解析一次路径,只改了探测侧,导致编码侧拿到空路径报
// "exec: no command"。
// 【修复方式】EncodeAVIF 开头对空路径也做 exec.LookPath(见 avif.go 注释)。
func TestEncodeAVIFResolvesFFmpegFromPath(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}

	out, err := EncodeAVIF(context.Background(), createTestImage(100, 100), Config{AVIFEnabled: true})
	if err != nil {
		t.Fatalf("EncodeAVIF with empty path: %v", err)
	}
	if !isAVIFBytes(out) {
		t.Fatalf("output is not an AVIF file (len=%d)", len(out))
	}
}

func TestEncodeAVIFUnavailableWithoutFFmpeg(t *testing.T) {
	_, err := EncodeAVIF(context.Background(), createTestImage(10, 10), Config{FFmpegPath: "/nonexistent/ffmpeg", AVIFEnabled: true})
	if !errors.Is(err, ErrAVIFUnavailable) {
		t.Fatalf("error = %v, want ErrAVIFUnavailable", err)
	}
}

// TestEncodeAVIFDisabled 守护 avif_enabled 配置开关:AVIFEnabled=false 时
// 编码器必须报"不可用"(即存原图),且完全不探测/不调用 ffmpeg。
// 【发现背景 2026-08-14】引入显式关闭开关时新增;此前服务器只要装了带
// AV1 编码器的 ffmpeg,AVIF 就自动生效,想关都关不掉。
// 【修复方式】EncodeAVIF 开头检查 cfg.AVIFEnabled,false 直接返回
// ErrAVIFUnavailable——与"没有 ffmpeg"同一条路径,调用方无需区分。
func TestEncodeAVIFDisabled(t *testing.T) {
	_, err := EncodeAVIF(context.Background(), createTestImage(10, 10), Config{FFmpegPath: "/definitely/not/ffmpeg", AVIFEnabled: false})
	if !errors.Is(err, ErrAVIFUnavailable) {
		t.Fatalf("error = %v, want ErrAVIFUnavailable", err)
	}
}

func TestEncodeAVIFRejectsInvalidInput(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	_, err := EncodeAVIF(context.Background(), []byte("not an image at all"), Config{FFmpegPath: ffmpegPath, AVIFEnabled: true})
	if err == nil {
		t.Fatal("EncodeAVIF accepted invalid image data")
	}
	if errors.Is(err, ErrAVIFUnavailable) {
		t.Fatalf("invalid input reported as unavailable: %v", err)
	}
}

func TestTransformImageWithFFmpegScalesAVIF(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	avif, err := EncodeAVIF(context.Background(), createTestImage(400, 300), Config{FFmpegPath: ffmpegPath, AVIFEnabled: true})
	if err != nil {
		t.Fatalf("EncodeAVIF fixture: %v", err)
	}

	for _, fit := range []FitMode{FitContain, FitCover, FitExact} {
		result, err := TransformImageWithFFmpeg(avif, 200, 100, fit, TransformOptions{JPEGQuality: DefaultTransformJPEGQuality}, ffmpegPath)
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
// 非 AVIF 输入回退 Go 路径"。
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
// 覆盖不透明/透明/静态 GIF 三种输入,确保都输出 RIFF WEBP。
func TestTransformImageWithFFmpegLossyWebPForNonAVIF(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)
	cases := []struct {
		name string
		data []byte
	}{
		{"opaque PNG", createTestImage(400, 300)},
		{"transparent PNG", createTransparentTestImage(64, 64)},
		{"static GIF", createAnimatedGIF(64, 64, 1)},
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

// TestTransformImageWithFFmpegAVIFWithoutFFmpegIsHardError 守护"AVIF 输入在
// ffmpeg 不可用/失败时是硬错误"。
// 【发现背景 2026-08-16】重写时明确:Go 标准库没有 AVIF 解码器,AVIF 输入
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
