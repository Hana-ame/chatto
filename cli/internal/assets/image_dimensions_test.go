package assets

// 【本地改动 2026-09-12】本文件整体为 fork 独有,upstream 没有同名文件。
// 目的:为 image_dimensions.go 与 validateDecodedImageSize 提供覆盖:Go 能解
// 的格式走注册解码器,读不出尺寸的输入必须报硬错误,尺寸/像素上限必须继续
// 生效。
// 思路:这里只用不依赖 ffmpeg 的输入(PNG 头、随机字节);真实 AVIF/HEIC
// 样本需要编码器,放在 avif_test.go 与 attachment_image_test.go 里生成。

import (
	"strings"
	"testing"
)

func createPNGBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	b := createTestImage(width, height)
	if len(b) == 0 {
		t.Fatal("createTestImage returned empty PNG")
	}
	return b
}

func TestImageDimensionsPNGViaGoDecoder(t *testing.T) {
	w, h, ok := imageDimensions(createPNGBytes(t, 33, 27))
	if !ok {
		t.Fatal("imageDimensions rejected a PNG")
	}
	if w != 33 || h != 27 {
		t.Fatalf("imageDimensions = %dx%d, want 33x27", w, h)
	}
}

func TestImageDimensionsEmpty(t *testing.T) {
	if _, _, ok := imageDimensions(nil); ok {
		t.Fatal("imageDimensions accepted an empty payload")
	}
}

// 【发现背景 2026-09-12】validateDecodedImageSize 旧实现直接
// image.DecodeConfig:AVIF/HEIC 输入报 "image: unknown format",而 AVIF 是
// 2026-09-02 前本仓库附件的存储格式,用户把聊天里保存的图再传回去就传不
// 上去。修复:尺寸改走 imageDimensions,注册解码器失败时手写读 ISO-BMFF。
// 本用例守护另一半不变量——换了解析方式之后,尺寸/像素上限仍然生效,不能
// 因为格式新就被绕过。
func TestValidateDecodedImageSizeStillBoundsDecodedSize(t *testing.T) {
	if err := validateDecodedImageSize(createPNGHeader(MaxDecodedImageDimension+1, 1)); err == nil {
		t.Fatal("decoded dimensions beyond MaxDecodedImageDimension must be rejected")
	}
	if err := validateDecodedImageSize(createPNGHeader(1, MaxDecodedImageDimension+1)); err == nil {
		t.Fatal("decoded height beyond MaxDecodedImageDimension must be rejected")
	}
}

func TestValidateDecodedImageSizeStillBoundsPixels(t *testing.T) {
	// 每边都远小于单边上限,总像素超过上限。
	w := 64
	h := int(MaxDecodedImagePixels/int64(w)) + 2
	if err := validateDecodedImageSize(createPNGHeader(uint32(w), uint32(h))); err == nil {
		t.Fatalf("%dx%d exceeds MaxDecodedImagePixels and must be rejected", w, h)
	}
}

func TestValidateDecodedImageSizeRejectsUnknownFormat(t *testing.T) {
	err := validateDecodedImageSize([]byte("definitely not an image at all"))
	if err == nil {
		t.Fatal("unknown format must still be rejected")
	}
	if !strings.Contains(err.Error(), "unrecognized image format") {
		t.Fatalf("error %q should name the unrecognized format", err)
	}
}
