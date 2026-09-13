// 【本地改动 2026-09-13】服务端资产「上传期压缩、请求期零编码」路径的测试。
//
// 发现背景:processServerAssetImage 最初只拦动画 GIF(IsAnimatedGIF),其余多帧
// 输入都交给 ffmpeg。用本地 ffmpeg 与 cloudcone 7.0.2-static 各跑一遍后发现:
// 动画 GIF 与动画 PNG 经 ffmpeg `-c:v libwebp -f webp` 会输出**动画 WebP**
// (VP8X + ANMF 多帧块),头像/branding 会被存成多 MB 的动画图。动画 WebP 则
// ffmpeg 解码直接失败("Decode error rate 1 exceeds maximum 0.666667" /
// libwebp -22,两个环境一致),靠这个失败才回退到 Go 路径——歪打正着,ffmpeg
// 版本一变就漏。所以把判断显式化(isMultiFrameImage),本文件锁定「服务端资产
// 永远是单帧」。
//
// 回归提示:若 fork 将来允许服务端资产保留动画,这两个断言要改。
package assets

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"io"
	"testing"
)

// processServerAsset 跑一遍头像处理并返回输出字节。
func processServerAsset(t *testing.T, in []byte) []byte {
	t.Helper()
	out, err := ProcessAvatarImageWithConfig(bytes.NewReader(in), DefaultConfig())
	if err != nil {
		t.Fatalf("ProcessAvatarImageWithConfig: %v", err)
	}
	data, err := io.ReadAll(out)
	if err != nil {
		t.Fatalf("read processed avatar: %v", err)
	}
	return data
}

// assertSingleFrameWebP 断言输出是 WebP 且不含动画帧块(ANMF)。
func assertSingleFrameWebP(t *testing.T, data []byte) {
	t.Helper()
	if len(data) < 16 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Fatalf("processed server asset is not a WebP container: %q", data[:min(12, len(data))])
	}
	if bytes.Contains(data, []byte("ANMF")) {
		t.Fatalf("processed server asset keeps animation frames (%d bytes with ANMF), want a single frame", len(data))
	}
}

// TestProcessServerAssetImageFlattensAnimatedGIF 动画 GIF 头像必须压成单帧。
func TestProcessServerAssetImageFlattensAnimatedGIF(t *testing.T) {
	assertSingleFrameWebP(t, processServerAsset(t, createAnimatedGIF(100, 80, 5)))
}

// TestProcessServerAssetImageFlattensAnimatedPNG 动画 PNG(acTL)头像同样单帧。
func TestProcessServerAssetImageFlattensAnimatedPNG(t *testing.T) {
	assertSingleFrameWebP(t, processServerAsset(t, createAnimatedPNG(100, 80, 5)))
}

// TestIsMultiFrameImage 覆盖多帧判断的四种格式。
//
// 动画 WebP 只测路由判断、不做端到端:Go 无法编码动画 WebP,而 ffmpeg 的
// libwebp_anim 在小图上会把动画塌缩成静态图,能生成 ANMF 的最小样例也要
// ~10 KB,不值得塞进源码。端到端行为由上面的 GIF/APNG 用例代表。
func TestIsMultiFrameImage(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{"animated gif", createAnimatedGIF(40, 30, 3), true},
		{"static gif", createStaticGIF(40, 30), false},
		{"animated webp", syntheticAnimatedWebP(), true},
		{"static webp", syntheticStaticWebP(), false},
		{"animated png", createAnimatedPNG(40, 30, 3), true},
		{"static png", createTestImage(40, 30), false},
		{"animated avif", []byte("\x00\x00\x00\x1cftypavis\x00\x00\x00\x00"), true},
		{"static avif", []byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMultiFrameImage(tc.data); got != tc.want {
				t.Fatalf("isMultiFrameImage = %v, want %v", got, tc.want)
			}
		})
	}
}

// createAnimatedPNG 手工拼一个动画 PNG:签名 + IHDR + acTL + IDAT + IEND。
// Go 标准库的 png 包只能编静态 PNG,所以要自己走 chunk 序列。
func createAnimatedPNG(width, height, frames int) []byte {
	var out bytes.Buffer
	out.Write([]byte("\x89PNG\r\n\x1a\n"))

	writePNGChunk(&out, "IHDR", func(b *bytes.Buffer) {
		binary.Write(b, binary.BigEndian, uint32(width))
		binary.Write(b, binary.BigEndian, uint32(height))
		b.Write([]byte{8, 6, 0, 0, 0}) // 8-bit RGBA
	})

	writePNGChunk(&out, "acTL", func(b *bytes.Buffer) {
		b.Write([]byte{0, 0, 0, 0}) // version
		binary.Write(b, binary.BigEndian, uint32(frames))
		binary.Write(b, binary.BigEndian, uint32(0)) // num_disposal_frames
		binary.Write(b, binary.BigEndian, uint32(0)) // loop_count
	})

	var raw bytes.Buffer
	row := make([]byte, 1+width*4)
	for i := 0; i < height; i++ {
		row[0] = 0 // filter: none
		for x := 0; x < width; x++ {
			p := 1 + x*4
			row[p] = uint8(x)
			row[p+1] = uint8(i)
			row[p+2] = uint8(frames * 40)
			row[p+3] = 255
		}
		raw.Write(row)
	}
	var idat bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&idat, zlib.BestSpeed)
	zw.Write(raw.Bytes())
	zw.Close()
	writePNGChunk(&out, "IDAT", func(b *bytes.Buffer) { b.Write(idat.Bytes()) })

	writePNGChunk(&out, "IEND", func(*bytes.Buffer) {})
	return out.Bytes()
}

// writePNGChunk 写一个 PNG chunk:4 字节长度 + 4 字节类型 + 数据 + CRC32,
// 其中 CRC32(IEEE)覆盖「类型 + 数据」两段拼接。
func writePNGChunk(out *bytes.Buffer, name string, fill func(*bytes.Buffer)) {
	var data bytes.Buffer
	fill(&data)

	body := append([]byte(name), data.Bytes()...)
	var header [4]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(data.Len()))

	out.Write(header[:])
	out.Write(body)
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[0:4], crc32.ChecksumIEEE(body))
	out.Write(crc[:])
}

// syntheticAnimatedWebP 造一个带 ANMF 块的 WebP 头。帧数据是假的,只用于
// 触发路由判断,不会被真的解码。
func syntheticAnimatedWebP() []byte {
	var out bytes.Buffer
	out.Write([]byte("RIFF"))
	out.Write([]byte{0x00, 0x00, 0x00, 0x00})
	out.Write([]byte("WEBP"))
	out.Write([]byte("VP8X"))
	out.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0, 0, 0})
	out.Write([]byte("ANMF"))
	out.Write([]byte{0x18, 0x00, 0x00, 0x00})
	out.Write(make([]byte, 24))
	return out.Bytes()
}

// syntheticStaticWebP 造一个 VP8L 静态 WebP 头,同样只用于路由判断。
func syntheticStaticWebP() []byte {
	return append([]byte("RIFF\x00\x00\x00\x00WEBPVP8L"), make([]byte, 20)...)
}
