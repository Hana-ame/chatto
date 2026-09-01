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

// AVIFAvailable 报告当前环境下 room 附件图片上传是否真的会产出 AVIF。
//
// 【目的】能否编 AVIF 由三件事共同决定:cfg.AVIFEnabled 配置开关、ffmpeg
// 二进制是否存在、该 ffmpeg 是否带 AV1 编码器(libsvtav1 或 libaom-av1)。
// 生产探测(selectAVIFEncoder)与集成测试的 content-type 断言必须共用这同一个
// 口径,不能各自判断。
//
// 【踩坑】2026-08-30 ci.yml 的 test-cli job 首次跑全量 Go 测试时暴露:该 job
// 装了 ffmpeg(actions/setup 的 install-ffmpeg),但测试环境的 core.AVIFEnabled
// 是零值 false——NewChattoCore 不设这个字段,生产由 cmd/run.go 从
// AssetProcessing.AVIFEnabledOrDefault() 写入(默认 true)。于是 EncodeAVIF 在
// 函数开头就返回 ErrAVIFUnavailable,原图 image/png 落盘;而当时只判
// exec.LookPath("ffmpeg") 的断言期待 image/avif,必然失败。另一头 build-linux
// 不装 ffmpeg,探测为假,断言期待 image/png 平凡通过——所以这个开关从未被
// 集成测试真正覆盖过。
//
// 【边界】只报告 room 附件上传路径的 AVIF 可用性。头像、服务端 branding、
// 链接预览是 WebP-only,不走 EncodeAVIF(见本文件顶部注释)。
// 探测有副作用:结果按 ffmpeg 路径缓存在 selectAVIFEncoder 里,首次调用会跑一次
// `ffmpeg -encoders`(5s 超时),后续调用直接命中缓存。
func AVIFAvailable(ctx context.Context, cfg Config) bool {
	if !cfg.AVIFEnabled {
		return false
	}
	_, err := selectAVIFEncoder(ctx, cfg.FFmpegPath)
	return err == nil
}

// TransformImageWithFFmpeg 产出图片衍生图(缩略图/展示图)。
//
// 【本地改动 2026-08-16】衍生图统一编码为有损 WebP:ffmpeg 存在时,所有
// 非动画图片(JPEG/PNG/静态 GIF/AVIF 输入)都经 ffmpeg 解码+缩放,用
// libwebp 有损编码(-q:v = JPEGQuality),输出固定 image/webp。
// 目的:之前只有 AVIF 输入走 ffmpeg,不透明图走 Go JPEG、透明图走无损
// WebP(见 TransformImageWithOptions),衍生图格式 webp/jpeg 混用,用户
// 要求全部统一成有损 WebP(nativewebp v1.3.0 只支持无损 VP8L,CI 又是
// CGO_ENABLED=0,有损 WebP 只能靠 ffmpeg)。
//
// ffmpeg 缺失或编码失败时回退 TransformImageWithOptions 的旧行为
// (不透明→JPEG、透明→无损 WebP),保证无 ffmpeg 的部署行为完全不变。
// AVIF 输入在 ffmpeg 不可用/失败时仍是硬错误——Go 标准库没有 AVIF
// 解码器,没有别的路径可走。
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
		if isAVIFBytes(data) {
			return nil, ErrAVIFUnavailable
		}
		return TransformImageWithOptions(data, width, height, fit, options)
	}
	result, err := encodeWebPWithFFmpeg(data, width, height, fit, options, ffmpegPath)
	if err == nil {
		return result, nil
	}
	if isAVIFBytes(data) {
		return nil, err
	}
	// 【本地改动 2026-08-16】非 AVIF 输入 ffmpeg 编码失败时静默回退 Go
	// 路径,与上传路径 EncodeAVIF 的 best-effort 语义一致:ffmpeg 只是
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
// 读 ISO-BMFF。文件输入则正常。JPEG/PNG/GIF 是流式格式,pipe:0 也能工作,
// 但统一走临时文件更简单且差异极小(转换本身是重操作)。
func encodeWebPWithFFmpeg(data []byte, width, height int, fit FitMode, options TransformOptions, ffmpegPath string) (*TransformResult, error) {
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

	transformCtx, cancel := context.WithTimeout(context.Background(), avifEncodeTimeout)
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
// ("ftyp" box 后跟 "avif" brand)。
func isAVIFBytes(data []byte) bool {
	return len(data) >= 12 &&
		data[4] == 'f' && data[5] == 't' && data[6] == 'y' && data[7] == 'p' &&
		data[8] == 'a' && data[9] == 'v' && data[10] == 'i' && data[11] == 'f'
}
