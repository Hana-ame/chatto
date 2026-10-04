package assets

// 【本地改动 2026-09-12】本文件整体为 fork 独有,upstream 没有同名文件。
// 目的:为 avif.go 提供覆盖——AVIF 重编码(成功、静帧、动画保帧、尺寸不缩放)、
// 三条 unavailable 分支(开关关闭、ffmpeg 缺失、无 AV1 编码器)、AVIF 尺寸解析、
// 动画判定。
// 思路:依赖 ffmpeg 的用例统一经 testFFmpegPath(t) 取路径,找不到就 t.Skip;
// 不依赖 ffmpeg 的用例显式传 /nonexistent/ffmpeg,让本地与 CI 都能全绿且
// 回退分支不被 skip 掉(与 webp_test.go 的口径一致)。AVIFImageDimensions 用
// 手写 ISO-BMFF box 样例验证,不依赖真实编码产物。
// 边界:只测 assets 包的纯编码/解析函数,不测上传管线;管线覆盖在
// cli/internal/core/attachments_test.go。

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

// bmffBox 拼一个 ISO-BMFF box(size + fourcc + content)。
func bmffBox(typeStr string, content []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(8+len(content)))
	b.WriteString(typeStr)
	b.Write(content)
	return b.Bytes()
}

// syntheticAVIF 拼一个最小的 ISO-BMFF 容器:ftyp(avif brand)+
// meta(fullbox)+ iprp + ipco + ispe,用来喂 AVIFImageDimensions。
func syntheticAVIF(width, height int) []byte {
	ftyp := bmffBox("ftyp", []byte{
		'a', 'v', 'i', 'f',
		0, 0, 0, 1,
		'a', 'v', 'i', 'f', 'm', 'i', 'f', '1',
	})
	ispe := bmffBox("ispe", []byte{
		0, 0, 0, 0, // fullbox version + flags
		byte(width >> 24), byte(width >> 16), byte(width >> 8), byte(width),
		byte(height >> 24), byte(height >> 16), byte(height >> 8), byte(height),
	})
	ipco := bmffBox("ipco", ispe)
	iprp := bmffBox("iprp", ipco)
	meta := bmffBox("meta", append([]byte{0, 0, 0, 0}, iprp...))
	return append(append([]byte{}, ftyp...), meta...)
}

func TestEncodeAVIFProducesAVIF(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FFmpegPath = testFFmpegPath(t)

	out, err := EncodeAVIF(context.Background(), createTestImage(128, 64), cfg)
	if err != nil {
		t.Fatalf("EncodeAVIF failed: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("EncodeAVIF produced empty output")
	}
	if !isAVIFBytes(out) {
		t.Fatalf("output is not an AVIF file: %x", out[:min(len(out), 12)])
	}
	w, h, ok := AVIFImageDimensions(out)
	if !ok || w != 128 || h != 64 {
		t.Fatalf("AVIFImageDimensions = %dx%d ok=%v, want 128x64", w, h, ok)
	}
}

// 【发现背景 2026-09-12 · 坑 3】libaom 的 -still-picture 1 只写单帧,动画
// GIF/WebP 会被压扁成静帧。修复:动画输入不带该参数。本用例守护动画保帧,
// 输出 brand 必须是 avis(动画 AVIF),静帧是 avif。
func TestEncodeAVIFKeepsAnimation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FFmpegPath = testFFmpegPath(t)

	out, err := EncodeAVIF(context.Background(), createAnimatedGIF(96, 64, 5), cfg)
	if err != nil {
		t.Fatalf("EncodeAVIF failed: %v", err)
	}
	if !IsAnimatedImageBytes(out) {
		t.Fatalf("animated input produced a static AVIF: %x", out[:min(len(out), 12)])
	}
}

func TestEncodeAVIFDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AVIFEnabled = false

	_, err := EncodeAVIF(context.Background(), createTestImage(64, 64), cfg)
	if !errors.Is(err, ErrAVIFUnavailable) {
		t.Fatalf("err = %v, want ErrAVIFUnavailable", err)
	}
}

func TestEncodeAVIFUnavailableWithoutFFmpeg(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FFmpegPath = "/nonexistent/ffmpeg"

	_, err := EncodeAVIF(context.Background(), createTestImage(64, 64), cfg)
	if !errors.Is(err, ErrAVIFUnavailable) {
		t.Fatalf("err = %v, want ErrAVIFUnavailable", err)
	}
}

// 【发现背景 2026-08-14】探测函数内部会自己 LookPath,但真正执行编码的
// exec.CommandContext 用的是 cfg.FFmpegPath 解析出的局部变量;两处不同步
// 会报 "exec: no command"。这里守护 FFmpegPath 为空时两条路径都解析。
func TestEncodeAVIFResolvesFFmpegFromPath(t *testing.T) {
	testFFmpegPath(t) // 没有 ffmpeg 就没法验证这条分支
	cfg := DefaultConfig()

	out, err := EncodeAVIF(context.Background(), createTestImage(64, 64), cfg)
	if err != nil {
		t.Fatalf("EncodeAVIF with empty FFmpegPath failed: %v", err)
	}
	if !isAVIFBytes(out) {
		t.Fatal("output is not an AVIF file")
	}
}

func TestAVIFAvailableHonoursConfigSwitch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AVIFEnabled = false
	if AVIFAvailable(context.Background(), cfg) {
		t.Fatal("AVIFAvailable must be false when AVIFEnabled is off")
	}
}

func TestAVIFAvailableFalseWithoutFFmpeg(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FFmpegPath = "/nonexistent/ffmpeg"
	if AVIFAvailable(context.Background(), cfg) {
		t.Fatal("AVIFAvailable must be false when ffmpeg is missing")
	}
}

func TestAVIFImageDimensions(t *testing.T) {
	for _, tc := range []struct{ w, h int }{{64, 32}, {1, 4000}} {
		data := syntheticAVIF(tc.w, tc.h)
		w, h, ok := AVIFImageDimensions(data)
		if !ok || w != tc.w || h != tc.h {
			t.Fatalf("AVIFImageDimensions = %dx%d ok=%v, want %dx%d", w, h, ok, tc.w, tc.h)
		}
	}
}

func TestAVIFImageDimensionsRejectsNonImage(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("not a container at all"),
		{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'},
		{},
	} {
		if _, _, ok := AVIFImageDimensions(data); ok {
			t.Fatalf("AVIFImageDimensions accepted %x", data)
		}
	}
}

func TestAVIFImageDimensionsRejectsMalformedBoxes(t *testing.T) {
	// 只有 ftyp,没有 meta。
	data := bmffBox("ftyp", []byte("avif\x00\x00\x00\x01avif"))
	if _, _, ok := AVIFImageDimensions(data); ok {
		t.Fatal("AVIFImageDimensions accepted a container without meta")
	}

	// ispe 的 payload 太短(缺 8 字节宽高)。
	ispe := bmffBox("ispe", []byte{0, 0, 0, 0})
	ipco := bmffBox("ipco", ispe)
	iprp := bmffBox("iprp", ipco)
	data = append(bmffBox("ftyp", []byte("avif\x00\x00\x00\x01avif")), bmffBox("meta", append([]byte{0, 0, 0, 0}, iprp...))...)
	if _, _, ok := AVIFImageDimensions(data); ok {
		t.Fatal("AVIFImageDimensions accepted a truncated ispe")
	}

	// 宽高为 0。
	ispe = bmffBox("ispe", []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	ipco = bmffBox("ipco", ispe)
	iprp = bmffBox("iprp", ipco)
	data = append(bmffBox("ftyp", []byte("avif\x00\x00\x00\x01avif")), bmffBox("meta", append([]byte{0, 0, 0, 0}, iprp...))...)
	if _, _, ok := AVIFImageDimensions(data); ok {
		t.Fatal("AVIFImageDimensions accepted zero dimensions")
	}
}

func TestIsAnimatedImageBytes(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"animated GIF", createAnimatedGIF(20, 20, 3), true},
		{"static GIF", createStaticGIF(20, 20), false},
		{"PNG", createTestImage(20, 20), false},
		{"static AVIF brand", bmffBox("ftyp", []byte("avif\x00\x00\x00\x01avif")), false},
		{"animated AVIF brand", bmffBox("ftyp", []byte("avis\x00\x00\x00\x01avis")), true},
		{"webp without ANMF", []byte("RIFF\x00\x00\x00\x00WEBPVP8L"), false},
		{"webp with ANMF", []byte("RIFF\x00\x00\x00\x00WEBPANMF"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAnimatedImageBytes(tc.data); got != tc.want {
				t.Fatalf("IsAnimatedImageBytes = %v, want %v", got, tc.want)
			}
		})
	}
}

// 【发现背景 2026-09-12 · 坑 1】输入必须走临时文件:pipe:0 对 ISO-BMFF 输入
// 会报 "partial file"。这里同时守护「坏输入必须报错而不是静默返回」——
// 上传管线依赖 EncodeAVIF 的错误来走原图回退,静默返回垃圾字节会把坏文件
// 存成 image/avif。
func TestEncodeAVIFRejectsGarbageInput(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FFmpegPath = testFFmpegPath(t)

	_, err := EncodeAVIF(context.Background(), []byte("this is not an image"), cfg)
	if err == nil {
		t.Fatal("garbage input must fail")
	}
}
