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

// WebP 作为 room 附件图片的后端存储格式。之前本 fork 用 AVIF 存储、请求时
// 转 WebP 衍生图,AVIF 存储本身只是压缩手段,且衍生图路径需要处理 ISO-BMFF
// (AVIF/HEIF) 不可 seek 的管道问题(见下方【本地改动 2026-09-01】注释)。
//
// 【思路】存储格式直接定为 WebP,上传时就用 ffmpeg libwebp 有损编码为
// image/webp;请求衍生图时同样是 ffmpeg 解码+缩放+WebP 编码。存储与
// 衍生图用同一种格式,代码和依赖都简化:只依赖 libwebp 一个编码器,
// 不再依赖 AV1(libsvtav1/libaom-av1),也不再有 AVIF 存储→WebP 衍生图
// 的二次格式转换。
//
// 【兼容】本改动只影响新上传附件的存储格式。历史已存储的 AVIF 附件仍然
// 有效——传输层直接按 image/avif 返回原字节;衍生图路径的
// TransformImageWithFFmpeg 继续用 ffmpeg 解码 AVIF 输入并编码为 WebP
// 输出(isAVIFBytes 判断保留)。
//
// 【边界】WebP 存储只作用于 room 附件静态图。头像、服务端 branding、
// 链接预览始终是 WebP-only(见 ProcessAvatarImageWithConfig 等的注释),
// 不受此开关影响。动画 GIF 保留原字节(视频管线会转 MP4/HLS)。

// ErrWebPUnavailable 表示当前环境无法做 WebP 编码:ffmpeg 未安装或没有
// libwebp 编码器。调用方应原样保存原始字节(与 webp_enabled=false 行为一致)。
var ErrWebPUnavailable = errors.New("WebP encoding unavailable")

const (
	// webpStorageQuality 是存储用 WebP 的有损质量(VP8 的 -q:v,范围
	// 1-100)。85 在体积和视觉质量之间取平衡:存储是"主图",后续衍生图
	// 会再做一次缩放+重编码(用 JPEGQuality 默认 75),主图质量需要
	// 高于衍生图以减少二次压缩伪影。
	webpStorageQuality = 85
	// encodeTimeout 限制单次 ffmpeg 编码时长(上传存储 + 衍生图转换共用),
	// 防止异常编码器无限挂起拖垮请求。
	encodeTimeout = 60 * time.Second
	// probeTimeout 限制 `ffmpeg -encoders` 探测时长。
	probeTimeout = 5 * time.Second
)

var (
	webpEncoderMu sync.Mutex
	// webpEncoderCache 按 ffmpeg 路径缓存"是否带 libwebp 编码器",
	// 避免每次上传都重新探测(ffmpeg -encoders 输出很贵)。
	webpEncoderCache = map[string]bool{}
)

// EncodeWebP 用 ffmpeg 把图片字节重编码为 WebP 有损静态图。
//
// cfg.FFmpegPath 原样使用;为空时从 PATH 解析。cfg.WebPEnabled 为 false 时
// 直接返回 ErrWebPUnavailable,让调用方存原图——和"没有 ffmpeg"走同一条
// 路径,调用方无需区分是配置关闭还是环境缺失。
//
// 【目的】room 附件图片上传时转 WebP 省存储/带宽;只需要 libwebp 一个
// 编码器(不再需要 AV1),输出可直接 pipe:1(libwebp 是流式容器)。
// 返回 ErrWebPUnavailable 表示 ffmpeg 或 libwebp 不可用;其余错误是
// 瞬时编码失败(应记录日志并保留原图)。
//
// 输出走 pipe:1 而非临时文件:libwebp VP8 是流式输出格式,不需要可 seek
// 输出(和 AVIF 的 libsvtav1 mux 不同,后者必须写临时文件,见下方注释
// 历史)。所以这里省掉磁盘写读,编码延迟更低。
func EncodeWebP(ctx context.Context, data []byte, cfg Config) ([]byte, error) {
	if !cfg.WebPEnabled {
		return nil, ErrWebPUnavailable
	}
	ffmpegPath := cfg.FFmpegPath
	// 【踩坑】这里和 hasWebPEncoder 里都要解析空路径:探测函数内部会
	// 自己 LookPath,但真正执行编码的 exec.CommandContext 用的是本函数
	// 的局部变量。之前只解析了探测侧,导致编码侧拿到空路径报
	// "exec: no command"(2026-08-14 临时文件重构时发现)。
	if ffmpegPath == "" {
		if resolved, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = resolved
		}
	}
	if ffmpegPath == "" {
		return nil, ErrWebPUnavailable
	}
	if !hasWebPEncoder(ctx, ffmpegPath) {
		return nil, ErrWebPUnavailable
	}

	args := []string{"-v", "error", "-y", "-i", "pipe:0",
		"-c:v", "libwebp",
		"-q:v", strconv.Itoa(webpStorageQuality),
		"-f", "webp", "pipe:1",
	}

	encCtx, cancel := context.WithTimeout(ctx, encodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(encCtx, ffmpegPath, args...)
	cmd.Stdin = bytes.NewReader(data)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if encCtx.Err() != nil {
			return nil, encCtx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 512 {
			detail = detail[:512] + "..."
		}
		return nil, fmt.Errorf("ffmpeg webp encode failed: %w: %s", err, detail)
	}
	outBytes := out.Bytes()
	if !isWebPBytes(outBytes) {
		return nil, fmt.Errorf("ffmpeg produced %d bytes that are not a WebP file", len(outBytes))
	}
	return outBytes, nil
}

// hasWebPEncoder 探测给定 ffmpeg 二进制是否带 libwebp 编码器,按路径缓存
// 结果。ffmpeg 缺失或没有 libwebp 时返回 false(同样缓存)。
func hasWebPEncoder(ctx context.Context, ffmpegPath string) bool {
	if ffmpegPath == "" {
		return false
	}
	webpEncoderMu.Lock()
	if cached, ok := webpEncoderCache[ffmpegPath]; ok {
		webpEncoderMu.Unlock()
		return cached
	}
	webpEncoderMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, ffmpegPath, "-v", "error", "-hide_banner", "-encoders")
	output, err := cmd.Output()
	if err != nil {
		webpEncoderMu.Lock()
		webpEncoderCache[ffmpegPath] = false
		webpEncoderMu.Unlock()
		return false
	}
	has := bytes.Contains(output, []byte("libwebp"))
	webpEncoderMu.Lock()
	webpEncoderCache[ffmpegPath] = has
	webpEncoderMu.Unlock()
	return has
}

// WebPAvailable 报告当前环境下 room 附件图片上传是否真的会产出 WebP。
//
// 【目的】能否编 WebP 由两件事共同决定:cfg.WebPEnabled 配置开关、ffmpeg
// 二进制是否存在且带 libwebp 编码器。生产探测(hasWebPEncoder)与集成测试的
// content-type 断言必须共用这同一个口径,不能各自判断。
//
// 【边界】只报告 room 附件上传路径的 WebP 可用性。头像、服务端 branding、
// 链接预览是 WebP-only,不走 EncodeWebP(见本文件顶部注释)。
// 探测有副作用:结果按 ffmpeg 路径缓存在 hasWebPEncoder 里,首次调用会跑
// 一次 `ffmpeg -encoders`(probeTimeout 超时),后续调用直接命中缓存。
func WebPAvailable(ctx context.Context, cfg Config) bool {
	if !cfg.WebPEnabled {
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
	return hasWebPEncoder(ctx, ffmpegPath)
}

// TransformImageWithFFmpeg 产出图片衍生图(缩略图/展示图)。
//
// 【本地改动 2026-08-16】衍生图统一编码为有损 WebP:ffmpeg 存在时,所有
// 非动画图片(JPEG/PNG/静态 GIF/WebP 输入/AVIF 历史输入)都经 ffmpeg
// 解码+缩放,用 libwebp 有损编码(-q:v = JPEGQuality),输出固定 image/webp。
// 目的:之前只有非 JPEG 输入走 ffmpeg,不透明图走 Go JPEG、透明图走无损
// WebP(见 TransformImageWithOptions),衍生图格式 webp/jpeg 混用,用户
// 要求全部统一成有损 WebP(nativewebp v1.3.0 只支持无损 VP8L,CI 又是
// CGO_ENABLED=0,有损 WebP 只能靠 ffmpeg)。
//
// ffmpeg 缺失或编码失败时回退 TransformImageWithOptions 的旧行为
// (不透明→JPEG、透明→无损 WebP),保证无 ffmpeg 的部署行为完全不变。
// WebP 输入(新存储格式)和 AVIF 输入(历史存储格式)在 ffmpeg 不可用/
// 失败时仍是硬错误——Go 标准库没有 WebP/AVIF 解码器,没有别的路径可走。
//
// 注意:动画 GIF 永远走 nativewebp 动画管线(无损动画 WebP),不能交给
// ffmpeg——动画必须保留,且视频管线会单独把动画 GIF 转成 MP4。
// ffmpegPath 原样使用;为空时从 PATH 解析。
func TransformImageWithFFmpeg(data []byte, width, height int, fit FitMode, options TransformOptions, ffmpegPath string) (*TransformResult, error) {
	if options.JPEGQuality < 1 || options.JPEGQuality > 100 {
		return nil, fmt.Errorf("invalid JPEG quality: %d", options.JPEGQuality)
	}
	// 【本地改动 2026-08-16】动画 GIF 必须先于 ffmpeg 分支拦截:ffmpeg
	// 单帧编码会丢掉动画,而衍生图应保留动画(原行为)。
	if IsAnimatedGIF(data) {
		return TransformImageWithOptions(data, width, height, fit, options)
	}
	if ffmpegPath == "" {
		if resolved, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegPath = resolved
		}
	}
	if ffmpegPath == "" {
		// Go 标准库无法解码 WebP/AVIF,这两种格式在 ffmpeg 不可用时
		// 是硬错误;JPEG/PNG/GIF 可走 Go 回退。
		if isWebPBytes(data) || isAVIFBytes(data) {
			return nil, ErrWebPUnavailable
		}
		return TransformImageWithOptions(data, width, height, fit, options)
	}
	result, err := encodeWebPWithFFmpeg(data, width, height, fit, options, ffmpegPath)
	if err == nil {
		return result, nil
	}
	// WebP(新存储)和 AVIF(历史存储)输入 ffmpeg 编码失败时无 Go 回退,
	// 返回原始错误;JPEG/PNG/GIF 输入可静默回退 Go 路径。
	if isWebPBytes(data) || isAVIFBytes(data) {
		return nil, err
	}
	// 【本地改动 2026-08-16】非 WebP/AVIF 输入 ffmpeg 编码失败时静默回退
	// Go 路径,与上传路径 EncodeWebP 的 best-effort 语义一致:ffmpeg 只是
	// 优化手段,不该让衍生图生成失败。
	return TransformImageWithOptions(data, width, height, fit, options)
}

// encodeWebPWithFFmpeg 用 ffmpeg 解码+缩放输入图片并编码为有损 WebP。
// 输出有损 WebP(带透明时保留 alpha),质量档位由 options.JPEGQuality
// 映射到 libwebp 的 -q:v。只处理静态图;动画 GIF 由调用方拦截。
//
// 【本地改动 2026-09-01】输入必须走临时文件而非 pipe:0。AVIF(以及所有
// ISO-BMFF 格式:HEIF/HEIC)的文件结构需要 ffmpeg 反复 seek 才能解析
// (ftyp→meta→mdat 各 box 分散在文件中);pipe:0 不可 seek,ffmpeg 读到
// mdat box 头部就报 "partial file / EOF" 直接失败。
//
// 复现:cloudcone 线上 avif 附件(image/avif)衍生图转换全部 500,错误
// "stream 0, offset 0x121: partial file"。根因就是 ffmpeg 用 pipe:0
// 读 ISO-BMFF。文件输入则正常。JPEG/PNG/GIF/WebP 是流式格式,pipe:0
// 也能工作,但统一走临时文件更简单且差异极小(转换本身是重操作)。
//
// 【本地改动 2026-09-02】云存储后端改为 WebP 后,衍生图输入大多是 WebP
// (也有历史 AVIF),都经同一条 ffmpeg 临时文件输入路径处理,不再需要
// 区分存储格式——ffmpeg 解码器会按文件头自动识别。
func encodeWebPWithFFmpeg(data []byte, width, height int, fit FitMode, options TransformOptions, ffmpegPath string) (*TransformResult, error) {
	var scale string
	switch fit {
	case FitContain:
		scale = fmt.Sprintf("scale=min(%d\\,iw):min(%d\\,ih):force_original_aspect_ratio=decrease", width, height)
	case FitCover:
		// 【本地改动 2026-09-02】cover 模式对高度做二次裁剪,防止缩放后
		// 仍超出目标尺寸。
		scale = fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d", width, height, width, height)
	case FitExact:
		scale = fmt.Sprintf("scale=%d:%d", width, height)
	default:
		return nil, fmt.Errorf("invalid fit mode: %s", fit)
	}

	// 【本地改动 2026-09-01】写临时文件:ffmpeg 解析 ISO-BMFF(AVIF/HEIF)
	// 需要 seek,pipe:0 不可 seek 会直接失败。
	inFile, err := os.CreateTemp("", "chatto-transform-*.bin")
	if err != nil {
		return nil, fmt.Errorf("create transform temp file: %w", err)
	}
	defer os.Remove(inFile.Name())
	if _, err := inFile.Write(data); err != nil {
		_ = inFile.Close()
		return nil, fmt.Errorf("write transform temp file: %w", err)
	}
	if err := inFile.Close(); err != nil {
		return nil, fmt.Errorf("close transform temp file: %w", err)
	}

	transformCtx, cancel := context.WithTimeout(context.Background(), encodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(transformCtx, ffmpegPath,
		"-v", "error", "-y", "-i", inFile.Name(),
		"-vf", scale,
		"-c:v", "libwebp",
		"-q:v", strconv.Itoa(options.JPEGQuality),
		"-f", "webp",
		"pipe:1",
	)
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
		return nil, fmt.Errorf("ffmpeg webp transform failed: %w: %s", err, detail)
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
// ("ftyp" box 后跟 "avif" brand)。保留用于识别历史存储的 AVIF 附件。
func isAVIFBytes(data []byte) bool {
	return len(data) >= 12 &&
		data[4] == 'f' && data[5] == 't' && data[6] == 'y' && data[7] == 'p' &&
		data[8] == 'a' && data[9] == 'v' && data[10] == 'i' && data[11] == 'f'
}

// isWebPBytes 判断数据是否以 WebP 文件头开始(RIFF..WEBP magic)。
func isWebPBytes(data []byte) bool {
	return len(data) >= 12 &&
		data[0] == 'R' && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' &&
		data[8] == 'W' && data[9] == 'E' && data[10] == 'B' && data[11] == 'P'
}
