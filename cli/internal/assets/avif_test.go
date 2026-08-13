package assets

import (
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

func TestTransformImageWithFFmpegDelegatesNonAVIF(t *testing.T) {
	// Non-AVIF input must take the Go decode path and never touch ffmpeg.
	result, err := TransformImageWithFFmpeg(createTestImage(64, 64), 32, 32, FitContain, TransformOptions{JPEGQuality: DefaultTransformJPEGQuality}, "/nonexistent/ffmpeg")
	if err != nil {
		t.Fatalf("TransformImageWithFFmpeg on PNG: %v", err)
	}
	if result.ContentType != "image/jpeg" {
		t.Fatalf("content type = %q, want image/jpeg", result.ContentType)
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
