package assets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AVIF 重编码上传的附件图片。选择 AV1 是因为它是 ffmpeg(视频管线本来就依赖
// 的二进制)里能拿到的最优压缩编码器。
//
// 【思路】上传时把 room 附件图片转成 AVIF,能显著减小存储和带宽;找不到
// ffmpeg 或没有 AV1 编码器时保持 best-effort,直接存原图,绝不阻塞上传。
//
// 【边界】AVIF 只作用于 room 附件图片。头像、服务端 branding、链接预览
// 都是 WebP-only,不走这里(见 ProcessAvatarImageWithConfig 等的注释)。
//
// 【踩坑】libsvtav1 的输出不能走管道(见 EncodeAVIF 内注释),必须写
// 临时文件;ffmpeg 路径要在探测和编码两处都解析(见 EncodeAVIF 内注释)。

// ErrAVIFUnavailable 表示当前环境无法做 AVIF 编码:ffmpeg 未安装或没有 AV1
// 编码器。调用方应原样保存原始字节(与 avif_enabled=false 行为一致)。
var ErrAVIFUnavailable = errors.New("AVIF encoding unavailable")

const (
	// avifCRF 是 libaom/libsvtav1 的 constant-rate-factor(上传图片用)。
	// AV1 的 CRF 不能和 JPEG/WebP 质量值 1:1 对应;30 是"视觉接近无损"
	// 的默认值,体积远小于无损。
	avifCRF = 30
	// avifFastPreset 是上传时选的 libsvtav1 preset。preset 范围 0(最慢/
	// 最好)到 13(最快);10 在编码延迟和视觉质量之间取平衡。
	avifFastPreset = 10
	// avifEncodeTimeout 限制单次 ffmpeg 编码时长,防止异常的编码器无限
	// 挂起拖垮上传请求(见下方 libsvtav1 管道踩坑)。
	avifEncodeTimeout = 60 * time.Second
	avifProbeTimeout  = 5 * time.Second
)

type avifEncoder string

const (
	avifEncoderSVT avifEncoder = "libsvtav1"
	avifEncoderAOM avifEncoder = "libaom-av1"
)

var (
	avifEncoderMu sync.Mutex
	// avifEncoderByFFmpeg 按 ffmpeg 路径缓存选中的编码器,避免每张图片
	// 上传都重新探测一次编码器列表(-encoders 输出很贵)。
	avifEncoderByFFmpeg = map[string]avifEncoder{}
	avifUnavailable     = map[string]bool{}
)

// EncodeAVIF 用 ffmpeg 把图片字节重编码为 AVIF 静态图。
// cfg.FFmpegPath 原样使用;为空时从 PATH 解析。cfg.AVIFEnabled 为 false 时
// 直接返回 ErrAVIFUnavailable,让调用方存原图——和"没有 ffmpeg"走同一条
// 路径,调用方无需区分是配置关闭还是环境缺失。
//
// 【目的】room 附件图片上传时转 AVIF 省存储/带宽;探测顺序 libsvtav1
// (快)优先,回退 libaom-av1(慢),统一用 avifCRF。
// 返回 ErrAVIFUnavailable 表示 ffmpeg 或 AV1 编码器不可用;其余错误是
// 瞬时编码失败(应记录日志并保留原图)。
func EncodeAVIF(ctx context.Context, data []byte, cfg Config) ([]byte, error) {
	if !cfg.AVIFEnabled {
		return nil, ErrAVIFUnavailable
	}
	ffmpegPath := cfg.FFmpegPath
	// 【踩坑】这里和 selectAVIFEncoder 里都要解析空路径:探测函数内部会
	// 自己 LookPath,但下面真正执行编码的 exec.CommandContext 用的是本函数
	// 的局部变量。之前只解析了探测侧,导致 TestEncodeAVIFResolvesFFmpegFromPath
	// 报 "exec: no command"(2026-08-14 临时文件重构时发现)。
	if ffmpegPath == "" {
		if resolved, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = resolved
		}
	}
	encoder, err := selectAVIFEncoder(ctx, ffmpegPath)
	if err != nil {
		return nil, err
	}

	args := []string{"-v", "error", "-y", "-i", "pipe:0"}
	switch encoder {
	case avifEncoderSVT:
		args = append(args, "-c:v", "libsvtav1", "-crf", strconv.Itoa(avifCRF), "-preset", strconv.Itoa(avifFastPreset))
	case avifEncoderAOM:
		args = append(args, "-c:v", "libaom-av1", "-crf", strconv.Itoa(avifCRF), "-cpu-used", "8", "-row-mt", "1", "-still-picture", "1")
	default:
		return nil, fmt.Errorf("unhandled AVIF encoder %q", encoder)
	}

	// 【踩坑·核心】AVIF 输出必须写可 seek 的临时文件,不能 `-f avif pipe:1`。
	// libsvtav1(优先选中的编码器)在输出端不可 seek 时会**挂死**:不报错、
	// 不退出、不写任何数据,直到 context 超时(60s)才被杀。2026-08-14
	// 在 Ubuntu ffmpeg 6.1 上合并 upstream main 后跑测试时发现:测试
	// 30s 超时 panic,goroutine 栈显示 cmd.Run() 一直卡在管道读。
	// 复现:`ffmpeg ... -c:v libsvtav1 -f avif pipe:1` 直接卡死;
	// libaom-av1 会报 "muxer does not support non seekable output" 正常退出。
	// 所以这里先建临时文件让 ffmpeg 写,编码完再读回,用完删除。
	outFile, err := os.CreateTemp("", "chatto-avif-*.avif")
	if err != nil {
		return nil, fmt.Errorf("create AVIF temp file: %w", err)
	}
	defer os.Remove(outFile.Name())
	if err := outFile.Close(); err != nil {
		return nil, fmt.Errorf("close AVIF temp file: %w", err)
	}
	args = append(args, "-f", "avif", outFile.Name())

	encodeCtx, cancel := context.WithTimeout(ctx, avifEncodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(encodeCtx, ffmpegPath, args...)
	cmd.Stdin = bytes.NewReader(data)
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
	out, err := os.ReadFile(outFile.Name())
	if err != nil {
		return nil, fmt.Errorf("read AVIF temp file: %w", err)
	}
	if !isAVIFBytes(out) {
		return nil, fmt.Errorf("ffmpeg produced %d bytes that are not an AVIF file", len(out))
	}
	return out, nil
}

// selectAVIFEncoder 探测给定 ffmpeg 二进制最快的 AV1 编码器,按路径缓存
// 结果。ffmpeg 缺失或没有 AV1 编码器时返回 ErrAVIFUnavailable(同样缓存,
// 避免反复探测)。
func selectAVIFEncoder(ctx context.Context, ffmpegPath string) (avifEncoder, error) {
	if ffmpegPath == "" {
		if resolved, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = resolved
		}
	}
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
		avifEncoderMu.Lock()
		avifUnavailable[ffmpegPath] = true
		avifEncoderMu.Unlock()
		return "", ErrAVIFUnavailable
	}

	var selected avifEncoder
	switch {
	case bytes.Contains(output, []byte("libsvtav1")):
		selected = avifEncoderSVT
	case bytes.Contains(output, []byte("libaom-av1")):
		selected = avifEncoderAOM
	}

	if selected == "" {
		avifEncoderMu.Lock()
		avifUnavailable[ffmpegPath] = true
		avifEncoderMu.Unlock()
		return "", ErrAVIFUnavailable
	}

	avifEncoderMu.Lock()
	avifEncoderByFFmpeg[ffmpegPath] = selected
	avifEncoderMu.Unlock()
	return selected, nil
}

// TransformImageWithFFmpeg 和 TransformImageWithOptions 一样做图片变换,
// 但当输入是 AVIF 时改用 ffmpeg 做解码+缩放——Go 标准库的 image 解码器
// 不认识 AVIF。输出固定是有损 WebP(支持透明),质量走 JPEG-quality 选项,
// 与现有 JPEG 衍生图的大小级别一致。
//
// 【目的】上传阶段把附件转成 AVIF 后,渲染时(缩放/裁剪)必须能读回 AVIF;
// 这是唯一能解码已存 AVIF 的地方。
// ffmpegPath 原样使用;为空时从 PATH 解析。AVIF 输入遇到缺失 ffmpeg 是
// 硬错误——没有其他解码器。
// 注意:这里只**解码**已存的 AVIF 附件,永远不会对头像/branding/链接
// 预览做 AVIF 重编码(那些是 WebP-only)。
func TransformImageWithFFmpeg(data []byte, width, height int, fit FitMode, options TransformOptions, ffmpegPath string) (*TransformResult, error) {
	if !isAVIFBytes(data) {
		return TransformImageWithOptions(data, width, height, fit, options)
	}
	if options.JPEGQuality < 1 || options.JPEGQuality > 100 {
		return nil, fmt.Errorf("invalid JPEG quality: %d", options.JPEGQuality)
	}
	if ffmpegPath == "" {
		if resolved, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = resolved
		}
	}
	if ffmpegPath == "" {
		return nil, ErrAVIFUnavailable
	}

	var scale string
	switch fit {
	case FitContain:
		scale = fmt.Sprintf("scale=min(%d\\,iw):min(%d\\,ih):force_original_aspect_ratio=decrease", width, height)
	case FitCover:
		scale = fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d", width, height, width, height)
	case FitExact:
		scale = fmt.Sprintf("scale=%d:%d", width, height)
	default:
		return nil, fmt.Errorf("invalid fit mode: %s", fit)
	}

	transformCtx, cancel := context.WithTimeout(context.Background(), avifEncodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(transformCtx, ffmpegPath,
		"-v", "error", "-y", "-i", "pipe:0",
		"-vf", scale,
		"-c:v", "libwebp",
		"-q:v", strconv.Itoa(options.JPEGQuality),
		"-f", "webp",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if transformCtx.Err() != nil {
			return nil, transformCtx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 512 {
			detail = detail[:512] + "..."
		}
		return nil, fmt.Errorf("ffmpeg AVIF transform failed: %w: %s", err, detail)
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("ffmpeg produced no transformed image")
	}
	return &TransformResult{
		Reader:      bytes.NewReader(stdout.Bytes()),
		ContentType: "image/webp",
	}, nil
}

// isAVIFBytes 判断数据是否以 ISO-BMFF 文件头开始,且 major brand 是 AVIF
// ("ftyp" box 后跟 "avif" brand)。
func isAVIFBytes(data []byte) bool {
	return len(data) >= 12 &&
		data[4] == 'f' && data[5] == 't' && data[6] == 'y' && data[7] == 'p' &&
		data[8] == 'a' && data[9] == 'v' && data[10] == 'i' && data[11] == 'f'
}
