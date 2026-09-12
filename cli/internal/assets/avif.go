package assets

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 【本地改动 2026-09-12】room 附件图片的统一存储格式改回 AVIF。
//
// 目的:上传时所有图片(静态与动画)都用 ffmpeg 重编码为**原尺寸** AVIF 存
// 一份,不再有衍生图,请求期也不再缩放重编码(见 http_server/assets.go 的
// bypass)。视频/音频等非图片原样存储。
//
// 时间线:2026-08-14 (32e1f566) 首次引入 AVIF 存储 → 2026-09-02 改为 WebP
// (只依赖 libwebp 一个编码器,衍生图统一有损 WebP)→ 2026-09-12 回到 AVIF
// 并取消衍生图。回到 AVIF 的理由:压缩率更高,一份原尺寸文件够用,WebP 时代
// 的"请求期 ffmpeg 缩放 + 缓存"链路是线上 500 的主要来源(pipe 不可 seek、
// ISO-BMFF 探测、缓存穿透)。
//
// 【坑 1 · 输入必须走临时文件】AVIF/WebP 动画/ISO-BMFF 输入需要可 seek,
// `pipe:0` 会让 ffmpeg 报 "partial file"(2026-09-01 线上 avif 衍生图全 500)。
// 【坑 2 · 输出必须走临时文件】libsvtav1 的 avif muxer 拿到不可 seek 的
// `pipe:1` 会既不报错也不退出,一直挂到超时(e73627822,Ubuntu ffmpeg 6.1)。
// 【坑 3 · 动画不能带 -still-picture 1】libaom 的 still-picture 模式只写
// 一帧,动画 GIF/WebP 会被压扁成静帧;2026-09-12 在 cloudcone 上用线上那张
// 240x240/16 帧 GIF 实测:去掉该参数后输出 nb_frames=16。
// 【坑 4 · Go 解不了 ISO-BMFF 图片】本包经 nativewebp 向 image 包注册了
// WebP 解码器,但 AVIF/HEIC 仍然没有解码器:旧路径对这些字节直接
// image: unknown format 报错,而 AVIF 正是 2026-09-02 前本仓库附件的存储
// 格式——用户把聊天里保存的图再传回去就失败。本文件用 ffmpeg 输出后的
// ispe 尺寸取代 Go 解码,顺带绕开这个坑(见 image_dimensions.go)。

// ErrAVIFUnavailable 表示当前环境无法编 AVIF:cfg.AVIFEnabled 关闭、ffmpeg
// 缺失、或 ffmpeg 没有 AV1 编码器。调用方应原样存原始字节(best-effort)。
var ErrAVIFUnavailable = errors.New("AVIF encoding unavailable")

const (
	// avifCRF 是上传附件 AVIF 的常量质量因子。AV1 的 CRF 与 JPEG/WebP 质量
	// 不是 1:1,30 大致对应"视觉接近无损"且体积远低于源图。
	avifCRF = 30
	// avifFastPreset 是 libsvtav1 的预设(0 最慢最好,13 最快),10 在上传
	// 请求路径上还能接受。
	avifFastPreset = 10
	// avifEncodeTimeout 限制单次 ffmpeg 编码,防止编码器挂死拖垮上传。
	avifEncodeTimeout = 60 * time.Second
	// avifProbeTimeout 限制 `ffmpeg -encoders` 探测时长。
	avifProbeTimeout = 5 * time.Second
)

// avifEncoder 是可用于产出 AVIF 的 AV1 编码器名。
type avifEncoder string

const (
	avifEncoderSVT avifEncoder = "libsvtav1"
	avifEncoderAOM avifEncoder = "libaom-av1"
)

var (
	avifEncoderMu sync.Mutex
	// avifEncoderByFFmpeg 按 ffmpeg 路径缓存选中的编码器,避免每次上传都
	// 跑一遍 `ffmpeg -encoders`。
	avifEncoderByFFmpeg = map[string]avifEncoder{}
	// avifUnavailable 缓存"这个 ffmpeg 编不了 AVIF"的否定结论。
	avifUnavailable = map[string]bool{}
)

// EncodeAVIF 用 ffmpeg 把图片字节重编码为 AVIF,尺寸保持原样(不缩放)。
// 动画输入(动画 GIF/动画 WebP/动画 AVIF)产出动画 AVIF;静帧输入产出静态
// AVIF。cfg.AVIFEnabled 为 false 时直接返回 ErrAVIFUnavailable,让调用方存
// 原图,和"环境里根本没有 ffmpeg"走同一条路径。
func EncodeAVIF(ctx context.Context, data []byte, cfg Config) ([]byte, error) {
	if !cfg.AVIFEnabled {
		return nil, ErrAVIFUnavailable
	}
	ffmpegPath := cfg.FFmpegPath
	// 【坑】探测函数内部会自己 LookPath,但真正执行编码的
	// exec.CommandContext 用的是这里的局部变量;为空时必须在两处都解析,
	// 否则报 "exec: no command"(2026-08-14 临时文件重构时踩过)。
	if ffmpegPath == "" {
		if resolved, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = resolved
		}
	}
	encoder, err := selectAVIFEncoder(ctx, ffmpegPath)
	if err != nil {
		return nil, err
	}

	args := []string{"-v", "error", "-y"}
	animated := IsAnimatedImageBytes(data)

	inFile, err := writeTempImageInput(data)
	if err != nil {
		return nil, err
	}
	defer os.Remove(inFile)

	outFile, err := os.CreateTemp("", "chatto-avif-*.avif")
	if err != nil {
		return nil, fmt.Errorf("create AVIF temp file: %w", err)
	}
	outPath := outFile.Name()
	if err := outFile.Close(); err != nil {
		_ = os.Remove(outPath)
		return nil, fmt.Errorf("close AVIF temp file: %w", err)
	}
	defer os.Remove(outPath)

	args = append(args, "-i", inFile, "-c:v", string(encoder), "-crf", strconv.Itoa(avifCRF))
	switch encoder {
	case avifEncoderSVT:
		args = append(args, "-preset", strconv.Itoa(avifFastPreset))
	case avifEncoderAOM:
		args = append(args, "-cpu-used", "8", "-row-mt", "1")
		// 【坑 3】-still-picture 1 只写单帧;动画输入必须不带它。
		if !animated {
			args = append(args, "-still-picture", "1")
		}
	}
	args = append(args, "-f", "avif", outPath)

	encodeCtx, cancel := context.WithTimeout(ctx, avifEncodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(encodeCtx, ffmpegPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if encodeCtx.Err() != nil {
			return nil, encodeCtx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 512 {
			detail = detail[:512] + "..."
		}
		return nil, fmt.Errorf("ffmpeg AVIF encode failed: %w: %s", err, detail)
	}
	out, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("read AVIF temp file: %w", err)
	}
	// 【发现背景 2026-09-12】输出校验不能用 isAVIFBytes:ffmpeg 对动画输入
	// 写出的是 brand=avis 的动画 AVIF(AVIF Sequence),静帧才是 avif。用
	// 严格品牌判断会把本仓库自己产出的动画 AVIF 当非 AVIF 拒掉,导致动画
	// GIF/WebP 上传永远回退原图。这里按 ISO-BMFF 家族校验。
	if !isHEIFFamilyBytes(out) {
		return nil, fmt.Errorf("ffmpeg produced %d bytes that are not an AVIF file", len(out))
	}
	return out, nil
}

// AVIFAvailable 报告当前环境下上传附件是否真的会产出 AVIF:配置开关 +
// ffmpeg 存在且带 AV1 编码器。生产探测与测试断言必须共用这一口径。
//
// 有副作用:结果按 ffmpeg 路径缓存,首次调用会跑一次 `ffmpeg -encoders`。
func AVIFAvailable(ctx context.Context, cfg Config) bool {
	if !cfg.AVIFEnabled {
		return false
	}
	ffmpegPath := cfg.FFmpegPath
	if ffmpegPath == "" {
		if resolved, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = resolved
		}
	}
	if ffmpegPath == "" {
		return false
	}
	_, err := selectAVIFEncoder(ctx, ffmpegPath)
	return err == nil
}

// selectAVIFEncoder 为给定 ffmpeg 二进制挑选可用的 AV1 编码器并按路径缓存。
// 优先 libsvtav1(快),回退 libaom-av1(cloudcone 上的静态 ffmpeg 7.0.2 只有
// 这一个)。ffmpeg 缺失或两个编码器都没有时返回 ErrAVIFUnavailable。
func selectAVIFEncoder(ctx context.Context, ffmpegPath string) (avifEncoder, error) {
	if ffmpegPath == "" {
		return "", ErrAVIFUnavailable
	}
	avifEncoderMu.Lock()
	if cached, ok := avifEncoderByFFmpeg[ffmpegPath]; ok {
		avifEncoderMu.Unlock()
		return cached, nil
	}
	if avifUnavailable[ffmpegPath] {
		avifEncoderMu.Unlock()
		return "", ErrAVIFUnavailable
	}
	avifEncoderMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, avifProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, ffmpegPath, "-v", "error", "-hide_banner", "-encoders")
	output, err := cmd.Output()
	if err != nil {
		markAVIFUnavailable(ffmpegPath)
		return "", ErrAVIFUnavailable
	}
	var selected avifEncoder
	switch {
	case bytes.Contains(output, []byte("libsvtav1")):
		selected = avifEncoderSVT
	case bytes.Contains(output, []byte("libaom-av1")):
		selected = avifEncoderAOM
	default:
		markAVIFUnavailable(ffmpegPath)
		return "", ErrAVIFUnavailable
	}
	avifEncoderMu.Lock()
	avifEncoderByFFmpeg[ffmpegPath] = selected
	avifEncoderMu.Unlock()
	return selected, nil
}

func markAVIFUnavailable(ffmpegPath string) {
	avifEncoderMu.Lock()
	avifUnavailable[ffmpegPath] = true
	avifEncoderMu.Unlock()
}

// writeTempImageInput 把输入字节落到临时文件,返回路径。ffmpeg 需要可 seek
// 的输入才能解析 AVIF/WebP 动画等容器,`pipe:0` 会报 "partial file"。
func writeTempImageInput(data []byte) (string, error) {
	inFile, err := os.CreateTemp("", "chatto-avif-in-*.bin")
	if err != nil {
		return "", fmt.Errorf("create AVIF input temp file: %w", err)
	}
	path := inFile.Name()
	if _, err := inFile.Write(data); err != nil {
		_ = inFile.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write AVIF input temp file: %w", err)
	}
	if err := inFile.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close AVIF input temp file: %w", err)
	}
	return path, nil
}

// IsAnimatedImageBytes 报告图片字节是否是多帧动画。覆盖动画 GIF、动画
// WebP(含 ANMF chunk)与动画 AVIF(major brand `avis`)。判定不了时返回
// false,按静帧处理最多少几帧,不会损坏文件。
func IsAnimatedImageBytes(data []byte) bool {
	switch {
	case len(data) >= 3 && data[0] == 'G' && data[1] == 'I' && data[2] == 'F':
		frames, err := inspectGIFFrames(data)
		return err == nil && frames > 1
	case isWebPBytes(data):
		return bytes.Contains(data, []byte("ANMF"))
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return string(data[8:12]) == "avis"
	default:
		return false
	}
}

// AVIFImageDimensions 从 AVIF(以及任何 ISO-BMFF/HEIF 家族)字节里读出显示
// 尺寸:扫描 meta → iprp → ipco → ispe 取第一个 ispe 的 width/height。
//
// 为什么自己解析:Go 标准库与 imageorient 都没有 AVIF 解码器(见本文件顶部
// 坑 4),而 ffmpeg 的输出尺寸必须记进附件元数据,否则前端不知道宽高。
// ffmpeg 默认 -autorotate,所以 ispe 里的尺寸是旋转矫正后的显示尺寸,正好
// 就是我们要存的。
func AVIFImageDimensions(data []byte) (width, height int, ok bool) {
	if !isAVIFBytes(data) && !isHEIFFamilyBytes(data) {
		return 0, 0, false
	}
	box := findBMFFBox(data, "meta")
	if box == nil {
		return 0, 0, false
	}
	// meta 是 fullbox:4 字节 version+flags 后才进入子 box。
	children := skipFullBoxHeader(box)
	for _, path := range []string{"iprp", "ipco"} {
		inner := findBMFFBox(children, path)
		if inner == nil {
			return 0, 0, false
		}
		children = inner
	}
	ispe := findBMFFBox(children, "ispe")
	if ispe == nil {
		return 0, 0, false
	}
	body := skipFullBoxHeader(ispe)
	if len(body) < 8 {
		return 0, 0, false
	}
	w := int(binary.BigEndian.Uint32(body[0:4]))
	h := int(binary.BigEndian.Uint32(body[4:8]))
	if w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// isHEIFFamilyBytes 报告字节是否以 ISO-BMFF ftyp 头开始(不区分 brand),
// HEIF/HEIC 与 AVIF 共享同一套 box 结构。
func isHEIFFamilyBytes(data []byte) bool {
	return len(data) >= 12 && string(data[4:8]) == "ftyp"
}

// findBMFFBox 在给定容器内容里找第一个匹配 fourcc 的 box,返回该 box 的
// **内容**(去掉 8 字节 box 头;largesize 也处理)。
func findBMFFBox(data []byte, boxType string) []byte {
	offset := 0
	for offset+8 <= len(data) {
		size := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		kind := string(data[offset+4 : offset+8])
		header := 8
		if size == 1 {
			if offset+16 > len(data) {
				return nil
			}
			large := binary.BigEndian.Uint64(data[offset+8 : offset+16])
			if large > uint64(len(data)) {
				return nil
			}
			size = int(large)
			header = 16
		} else if size == 0 {
			size = len(data) - offset
		}
		if size < header || offset+size > len(data) {
			return nil
		}
		if kind == boxType {
			return data[offset+header : offset+size]
		}
		offset += size
	}
	return nil
}

// skipFullBoxHeader 去掉 fullbox 的 version(1)+flags(3) 前缀。
func skipFullBoxHeader(content []byte) []byte {
	if len(content) < 4 {
		return nil
	}
	return content[4:]
}
