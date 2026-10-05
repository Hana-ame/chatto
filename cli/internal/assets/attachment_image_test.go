package assets

// 【本地改动 2026-09-12】本文件整体为 fork 独有,upstream 没有同名文件。
// 目的:为 attachment_image.go 的 PrepareAttachmentImage(上传图片的唯一入口)
// 提供覆盖:ISO-BMFF 输入不再硬失败、编码失败原图回退、开关关闭、超限硬错误。
// 思路:回退用例显式传 /nonexistent/ffmpeg 强制失败,不依赖本地是否装了
// ffmpeg;需要真实 AVIF/WebP 样本的用例经 testFFmpegPath(t) 取路径生成,
// 没有就 t.Skip。
// 边界:只测 assets 包的纯处理函数,不测上传管线;管线覆盖在
// cli/internal/core/attachments_test.go 与 asset_uploads_test.go。

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// testAVIFBytes 用本机 ffmpeg 生成一份真实 AVIF。libsvtav1 与 libaom 都要求
// 宽高 ≥ 64,测试尺寸别选小了,否则编码器直接报参数错误。
func testAVIFBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	cfg := DefaultConfig()
	cfg.FFmpegPath = testFFmpegPath(t)
	out, err := EncodeAVIF(context.Background(), createTestImage(width, height), cfg)
	if err != nil {
		t.Fatalf("generating an AVIF sample failed: %v", err)
	}
	return out
}

// testWebPBytes 用本机 ffmpeg 生成一份真实 WebP,尺寸交给调用方自己读。
func testWebPBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	cfg := DefaultConfig()
	cfg.FFmpegPath = testFFmpegPath(t)
	out, err := EncodeWebP(context.Background(), createTestImage(width, height), cfg)
	if err != nil {
		t.Fatalf("generating a WebP sample failed: %v", err)
	}
	return out
}

// 【发现背景 2026-09-12 · 根因】本包经 HugoSmits86/nativewebp 向 image 包
// 注册了 WebP 解码器,但 AVIF/HEIC 在 Go 里仍然没有解码器;旧实现直接
// image.DecodeConfig,于是 AVIF 输入报 "image: unknown format",而 AVIF 正是
// 2026-09-02 前本仓库附件的存储格式——用户把聊天里保存的图再传回去就传不
// 上去。修复:尺寸改走 imageDimensions,注册解码器失败时手写读 ISO-BMFF 的
// ispe box。本用例用真实 AVIF 字节守护这条回归。
func TestValidateDecodedImageSizeAcceptsAVIF(t *testing.T) {
	if err := validateDecodedImageSize(testAVIFBytes(t, 128, 64)); err != nil {
		t.Fatalf("a real AVIF must be accepted: %v", err)
	}
}

// 【发现背景 2026-09-12】同上的存储格式回归:存储字节必须是可再上传的。
// 即使 ffmpeg 完全不可用,AVIF 上传也必须成功并按原图存储,不能因为编不动
// 就整个上传失败(best-effort 策略)。
func TestPrepareAttachmentImageAcceptsAVIFWithoutFFmpeg(t *testing.T) {
	avif := testAVIFBytes(t, 128, 64)
	cfg := DefaultConfig()
	cfg.FFmpegPath = "/nonexistent/ffmpeg"

	got, err := PrepareAttachmentImage(context.Background(), bytes.NewReader(avif), cfg)
	if err == nil {
		t.Fatal("a broken ffmpeg must surface as an error so the caller can log it")
	}
	if got == nil {
		t.Fatal("the caller still needs the fallback result to store the original")
	}
	if !bytes.Equal(got.Content, avif) {
		t.Fatal("fallback must store the original bytes")
	}
	if got.ContentType != "image/avif" {
		t.Fatalf("ContentType = %q, want image/avif", got.ContentType)
	}
	if got.Width != 128 || got.Height != 64 {
		t.Fatalf("dimensions = %dx%d, want 128x64", got.Width, got.Height)
	}
}

// WebP 在 Go 侧本来就能解(nativewebp 注册了解码器),这里守护的是回退路径的
// 语义:编不动时原样存储、类型与尺寸仍从字节里读出来。
func TestPrepareAttachmentImageAcceptsWebPWithoutFFmpeg(t *testing.T) {
	webp := testWebPBytes(t, 96, 64)
	cfg := DefaultConfig()
	cfg.FFmpegPath = "/nonexistent/ffmpeg"

	got, err := PrepareAttachmentImage(context.Background(), bytes.NewReader(webp), cfg)
	if err == nil {
		t.Fatal("a broken ffmpeg must surface as an error")
	}
	if got == nil || !bytes.Equal(got.Content, webp) {
		t.Fatal("fallback must store the original bytes")
	}
	if got.ContentType != "image/webp" {
		t.Fatalf("ContentType = %q, want image/webp", got.ContentType)
	}
	wantW, wantH, ok := imageDimensions(webp)
	if !ok {
		t.Fatal("a real WebP must have readable dimensions")
	}
	if got.Width != wantW || got.Height != wantH {
		t.Fatalf("dimensions = %dx%d, want %dx%d", got.Width, got.Height, wantW, wantH)
	}
}

func TestPrepareAttachmentImageFallsBackToOriginalOnEncodeFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FFmpegPath = "/nonexistent/ffmpeg"

	png := createTestImage(64, 64)
	got, err := PrepareAttachmentImage(context.Background(), bytes.NewReader(png), cfg)
	if err == nil {
		t.Fatal("a broken ffmpeg must surface as an error so the caller can log it")
	}
	if got == nil {
		t.Fatal("the caller still needs the fallback result to store the original")
	}
	if !bytes.Equal(got.Content, png) {
		t.Fatal("fallback must store the original bytes")
	}
	if got.ContentType != "image/png" {
		t.Fatalf("ContentType = %q, want image/png", got.ContentType)
	}
	if got.Width != 64 || got.Height != 64 {
		t.Fatalf("dimensions = %dx%d, want 64x64", got.Width, got.Height)
	}
}

func TestPrepareAttachmentImageDisabledStoresOriginal(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AVIFEnabled = false

	png := createTestImage(64, 64)
	got, err := PrepareAttachmentImage(context.Background(), bytes.NewReader(png), cfg)
	if err == nil {
		t.Fatal("AVIFEnabled=false must return ErrAVIFUnavailable")
	}
	if !errors.Is(err, ErrAVIFUnavailable) {
		t.Fatalf("err = %v, want ErrAVIFUnavailable", err)
	}
	if got == nil || !bytes.Equal(got.Content, png) || got.ContentType != "image/png" {
		t.Fatalf("got %v with ContentType %q, want the original PNG", got == nil, got.ContentType)
	}
}

func TestPrepareAttachmentImageProducesAVIF(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FFmpegPath = testFFmpegPath(t)

	png := createTestImage(96, 64)
	got, err := PrepareAttachmentImage(context.Background(), bytes.NewReader(png), cfg)
	if err != nil {
		t.Fatalf("PrepareAttachmentImage failed: %v", err)
	}
	if got.ContentType != "image/avif" || !isAVIFBytes(got.Content) {
		t.Fatalf("ContentType = %q, bytes are not AVIF", got.ContentType)
	}
	if got.Width != 96 || got.Height != 64 {
		t.Fatalf("dimensions = %dx%d, want the original 96x64", got.Width, got.Height)
	}
}

// 【发现背景 2026-09-12】「取消衍生图」的前提是存储字节等于显示字节:上传只
// 产出一份原尺寸文件,请求期不再缩放。守护宽高不被缩放。
func TestPrepareAttachmentImageNeverResizes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FFmpegPath = testFFmpegPath(t)

	for _, tc := range []struct{ w, h int }{{256, 96}, {64, 256}} {
		png := createTestImage(tc.w, tc.h)
		got, err := PrepareAttachmentImage(context.Background(), bytes.NewReader(png), cfg)
		if err != nil {
			t.Fatalf("PrepareAttachmentImage failed for %dx%d: %v", tc.w, tc.h, err)
		}
		if got.Width != tc.w || got.Height != tc.h {
			t.Fatalf("dimensions = %dx%d, want %dx%d (no resize)", got.Width, got.Height, tc.w, tc.h)
		}
	}
}

func TestPrepareAttachmentImageRejectsOversizedInput(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxUploadSize = 16

	tooBig := bytes.Repeat([]byte{1}, 40)
	if _, err := PrepareAttachmentImage(context.Background(), bytes.NewReader(tooBig), cfg); err == nil {
		t.Fatal("input beyond MaxUploadSize must fail")
	}
}

func TestPrepareAttachmentImageRejectsUnrecognizedFormat(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FFmpegPath = "/nonexistent/ffmpeg"

	if _, err := PrepareAttachmentImage(context.Background(), bytes.NewReader([]byte("nope")), cfg); err == nil {
		t.Fatal("unrecognized format must fail")
	}
}
