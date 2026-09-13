package assets

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	_ "image/jpeg" // Register JPEG decoder
	_ "image/png"  // Register PNG decoder
	"io"

	"github.com/HugoSmits86/nativewebp"
	"github.com/disintegration/imageorient"
	xdraw "golang.org/x/image/draw"
)

// Default values for asset processing
const (
	// DefaultMaxUploadSize is the default maximum size for uploaded files (25 MB).
	DefaultMaxUploadSize int64 = 25 * 1024 * 1024
	// MaxAvatarDim is the maximum dimension for avatar images.
	MaxAvatarDim = 256
	// MaxLogoDim is the maximum dimension for space logo images.
	MaxLogoDim = 512
	// MaxBannerWidth is the maximum width for space banner images (4:3 aspect ratio).
	MaxBannerWidth = 768
	// MaxBannerHeight is the maximum height for space banner images (4:3 aspect ratio).
	MaxBannerHeight = 576
	// DefaultTransformJPEGQuality is the JPEG quality used by transformed images
	// unless the caller selects a surface-specific quality (1-100).
	// 80 provides a good balance between file size and visual quality.
	DefaultTransformJPEGQuality = 80
	// MaxDecodedImageDimension and MaxDecodedImagePixels bound allocations made
	// by image decoders before images are resized for display.
	MaxDecodedImageDimension = 16_384
	MaxDecodedImagePixels    = 40_000_000
	// Animated images retain full-canvas frame snapshots during conversion.
	MaxAnimatedImageFrames           = 256
	MaxAnimatedImageCumulativePixels = 100_000_000
)

// Config holds configuration for asset processing.
type Config struct {
	// MaxUploadSize is the maximum size for uploaded files in bytes.
	MaxUploadSize int64
	// FFmpegPath 是用于把上传的附件图片重编码为 AVIF 的 ffmpeg 二进制。
	// 【本地改动 32e1f566】为空时从 PATH 解析。
	FFmpegPath string
	// WebPEnabled 控制 EncodeWebP(2026-09-02 ~ 2026-09-12 期间的 room 附件
	// 存储格式)是否可用。
	// 【本地改动 32e1f566 + 218426d6 + 2026-09-02】2026-09-02 之前是
	// AVIFEnabled(AVIF 存储);存储格式改为 WebP 后字段重命名。关闭时保持
	// 原字节且完全不探测/不调用 ffmpeg。头像、branding、链接预览是
	// WebP-only,不受此开关影响。
	// 【2026-09-12】存储格式改回 AVIF 后,上传路径不再读这个字段(见
	// AVIFEnabled);保留只为兼容既有 webp_enabled 配置。
	WebPEnabled bool
	// AVIFEnabled 控制 room 附件图片上传时是否重编码为原尺寸 AVIF。
	// 【本地改动 2026-09-12】取代 WebP 存储;关闭时原样存上传字节。
	AVIFEnabled bool
}

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	return Config{
		MaxUploadSize: DefaultMaxUploadSize,
		// 【本地改动 218426d6 + 2026-09-02】WebP 默认 best-effort 开启
		// (2026-09-02 前是 AVIF);显式配置可关闭(见
		// AssetProcessingConfig.WebPEnabled)。
		WebPEnabled: true,
		// 【本地改动 2026-09-12】room 附件图片存储格式回到 AVIF,默认
		// best-effort 开启(没有带 AV1 编码器的 ffmpeg 时静默存原图)。
		AVIFEnabled: true,
	}
}

func readAndValidateImage(input io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read image: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("image exceeds maximum size of %d bytes", maxBytes)
	}
	if err := validateDecodedImageSize(data); err != nil {
		return nil, err
	}
	return data, nil
}

func validateDecodedImageSize(data []byte) error {
	// 【本地改动 2026-09-12】改用 imageDimensions 而不是 image.DecodeConfig:
	// 本包的 nativewebp 会向 image 包注册 WebP 解码器,但 AVIF/HEIC 在 Go 里
	// 仍然没有解码器,旧实现在这里一律报 "image: unknown format"——用户上传
	// 的 AVIF(2026-09-02 前本仓库附件的存储格式)或 iPhone 的 HEIC 就直接
	// 传不上去,这是本次策略重写要修的根因。imageDimensions 先试注册解码器,
	// 失败再手写读 ISO-BMFF 头。解析不出尺寸仍然是硬错误:尺寸/像素上限是
	// 资源上限(防压缩炸弹),不能因为格式新就跳过。
	width, height, ok := imageDimensions(data)
	if !ok {
		return fmt.Errorf("failed to decode image configuration: unrecognized image format")
	}
	if width > MaxDecodedImageDimension || height > MaxDecodedImageDimension {
		return fmt.Errorf("image dimensions %dx%d exceed the supported limit", width, height)
	}
	pixels := int64(width) * int64(height)
	if pixels > MaxDecodedImagePixels {
		return fmt.Errorf("image contains %d pixels, exceeding the supported limit of %d", pixels, MaxDecodedImagePixels)
	}
	if len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a") {
		if _, err := inspectGIFFrames(data); err != nil {
			return err
		}
	}
	return nil
}

// inspectGIFFrames walks GIF block framing without decompressing image data,
// allowing frame and per-frame dimension limits to be enforced before DecodeAll.
func inspectGIFFrames(data []byte) (int, error) {
	if len(data) < 13 {
		return 0, fmt.Errorf("invalid GIF header")
	}
	offset := 13
	packed := data[10]
	if packed&0x80 != 0 {
		offset += 3 * (1 << ((packed & 0x07) + 1))
	}
	if offset > len(data) {
		return 0, fmt.Errorf("invalid GIF color table")
	}

	frames := 0
	var cumulativePixels int64
	skipSubBlocks := func() error {
		for {
			if offset >= len(data) {
				return io.ErrUnexpectedEOF
			}
			size := int(data[offset])
			offset++
			if size == 0 {
				return nil
			}
			if size > len(data)-offset {
				return io.ErrUnexpectedEOF
			}
			offset += size
		}
	}

	for offset < len(data) {
		switch data[offset] {
		case 0x3b: // trailer
			return frames, nil
		case 0x21: // extension
			offset += 2 // introducer and label
			if offset > len(data) {
				return 0, io.ErrUnexpectedEOF
			}
			if err := skipSubBlocks(); err != nil {
				return 0, fmt.Errorf("invalid GIF extension: %w", err)
			}
		case 0x2c: // image descriptor
			if offset+10 > len(data) {
				return 0, io.ErrUnexpectedEOF
			}
			width := int(binary.LittleEndian.Uint16(data[offset+5 : offset+7]))
			height := int(binary.LittleEndian.Uint16(data[offset+7 : offset+9]))
			if width <= 0 || height <= 0 || width > MaxDecodedImageDimension || height > MaxDecodedImageDimension {
				return 0, fmt.Errorf("GIF frame dimensions %dx%d exceed the supported limit", width, height)
			}
			framePixels := int64(width) * int64(height)
			if framePixels > MaxDecodedImagePixels {
				return 0, fmt.Errorf("GIF frame contains %d pixels, exceeding the supported limit of %d", framePixels, MaxDecodedImagePixels)
			}
			frames++
			cumulativePixels += framePixels
			if frames > MaxAnimatedImageFrames || cumulativePixels > MaxAnimatedImageCumulativePixels {
				return 0, fmt.Errorf("animated image exceeds supported frame limits")
			}
			packed := data[offset+9]
			offset += 10
			if packed&0x80 != 0 {
				offset += 3 * (1 << ((packed & 0x07) + 1))
			}
			if offset >= len(data) {
				return 0, io.ErrUnexpectedEOF
			}
			offset++ // LZW minimum code size
			if err := skipSubBlocks(); err != nil {
				return 0, fmt.Errorf("invalid GIF image data: %w", err)
			}
		default:
			return 0, fmt.Errorf("invalid GIF block marker 0x%x", data[offset])
		}
	}
	return 0, io.ErrUnexpectedEOF
}

func decodeBoundedImage(input io.Reader, cfg Config) (image.Image, error) {
	data, err := readAndValidateImage(input, cfg.MaxUploadSize)
	if err != nil {
		return nil, err
	}
	img, _, err := imageorient.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}
	return img, nil
}

// FitMode defines how an image should be fitted within the target dimensions.
type FitMode string

const (
	// FitContain fits the image within bounds while preserving aspect ratio.
	// The entire image will be visible, with possible letterboxing.
	FitContain FitMode = "contain"

	// FitCover fills the entire bounds while preserving aspect ratio.
	// The image is center-cropped if necessary.
	FitCover FitMode = "cover"

	// FitExact stretches the image to exactly match the target dimensions.
	// This may distort the image if the aspect ratio differs.
	FitExact FitMode = "exact"
)

// TransformResult holds the result of an image transformation.
type TransformResult struct {
	// Reader provides the transformed image data
	Reader io.Reader
	// ContentType is the MIME type of the output ("image/webp" or "image/jpeg")
	ContentType string
}

// TransformOptions controls image derivative encoding.
type TransformOptions struct {
	// JPEGQuality is used for opaque static images. It must be between 1 and 100.
	JPEGQuality int
}

// DetectImageContentType returns the MIME type of image data based on magic bytes.
// Returns "image/jpeg" for JPEG, "image/webp" for WebP, "image/gif" for GIF,
// "image/png" for PNG, or "application/octet-stream" if unrecognized.
func DetectImageContentType(data []byte) string {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg"
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	case len(data) >= 3 && string(data[0:3]) == "GIF":
		return "image/gif"
	case len(data) >= 4 && data[0] == 0x89 && string(data[1:4]) == "PNG":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// IsAnimatedGIF checks if the given bytes represent an animated GIF (more than 1 frame).
func IsAnimatedGIF(data []byte) bool {
	frames, err := inspectGIFFrames(data)
	return err == nil && frames > 1
}

// ProcessAvatarImage reads an image from the input reader, resizes it to fit
// within the configured max dimensions while maintaining aspect ratio, and
// encodes it as WebP. Uses default config values.
func ProcessAvatarImage(input io.Reader) (io.Reader, error) {
	return ProcessAvatarImageWithConfig(input, DefaultConfig())
}

// ProcessAvatarImageWithConfig 从输入 reader 读图,缩放到 MaxAvatarDim 范围内
// 并保持宽高比,编码为 WebP。输入超过 cfg.MaxUploadSize 时报错。
// 【本地改动 32e1f566 + 2026-09-02 + 2026-09-13】此路径刻意与 room 附件的
// AVIF 管线分开(附件原尺寸存储,服务端资产缩放到上限后存储),但同属
// 「上传期编码一次」这条策略:头像在上传时就压缩好,请求期不再缩放重编码。
func ProcessAvatarImageWithConfig(input io.Reader, cfg Config) (io.Reader, error) {
	return processServerAssetImage(input, MaxAvatarDim, MaxAvatarDim, cfg)
}

// ProcessLogoImage reads an image from the input reader, resizes it to fit
// within MaxLogoDim x MaxLogoDim while maintaining aspect ratio, and encodes
// it as WebP. Returns an error if the input exceeds cfg.MaxUploadSize.
func ProcessLogoImage(input io.Reader) (io.Reader, error) {
	return ProcessLogoImageWithConfig(input, DefaultConfig())
}

// ProcessLogoImageWithConfig 从输入 reader 读图,缩放到 MaxLogoDim 范围内
// 并保持宽高比,编码为 WebP。输入超过 cfg.MaxUploadSize 时报错。
// 【本地改动 32e1f566 + 2026-09-02 + 2026-09-13】同头像:上传期压缩一次,
// 请求期不再缩放重编码。
func ProcessLogoImageWithConfig(input io.Reader, cfg Config) (io.Reader, error) {
	return processServerAssetImage(input, MaxLogoDim, MaxLogoDim, cfg)
}

// ProcessBannerImage reads an image from the input reader, resizes it to fit
// within MaxBannerWidth x MaxBannerHeight while maintaining aspect ratio, and
// encodes it as WebP. Returns an error if the input exceeds cfg.MaxUploadSize.
func ProcessBannerImage(input io.Reader) (io.Reader, error) {
	return ProcessBannerImageWithConfig(input, DefaultConfig())
}

// ProcessBannerImageWithConfig reads an image from the input reader, resizes it to fit
// within MaxBannerWidth x MaxBannerHeight while maintaining aspect ratio, and
// encodes it as WebP. Returns an error if the input exceeds cfg.MaxUploadSize.
// 【本地改动 2026-09-13】上传期压缩一次,请求期不再缩放重编码(见
// processServerAssetImage 的说明)。
func ProcessBannerImageWithConfig(input io.Reader, cfg Config) (io.Reader, error) {
	return processServerAssetImage(input, MaxBannerWidth, MaxBannerHeight, cfg)
}

// MaxLinkPreviewWidth is the maximum width for link preview OG images.
// Standard OG images are 1200x630; we preserve that resolution for sharp display on 2x screens.
const MaxLinkPreviewWidth = 1200

// MaxLinkPreviewHeight is the maximum height for link preview OG images.
const MaxLinkPreviewHeight = 630

// ProcessLinkPreviewImageWithConfig 从输入 reader 读图,缩放到
// MaxLinkPreviewWidth x MaxLinkPreviewHeight 范围内并保持宽高比,编码为
// WebP。输入超过 cfg.MaxUploadSize 时报错。
// 【本地改动说明 32e1f566 + 2026-09-02 + 2026-09-13】此路径刻意与 room 附件的
// AVIF 管线分开(附件原尺寸存储),但同属「上传期编码一次」这条策略:链接预览
// 图在抓取时就压缩好,请求期不再缩放重编码。
func ProcessLinkPreviewImageWithConfig(input io.Reader, cfg Config) (io.Reader, error) {
	return processServerAssetImage(input, MaxLinkPreviewWidth, MaxLinkPreviewHeight, cfg)
}

// isMultiFrameImage 报告图片字节是否为多帧动画。覆盖动画 GIF、动画 WebP
// (VP8X 容器 + ANMF 帧块)、动画 PNG(带 acTL 块)。
//
// 【本地改动 2026-09-13】服务端资产要压成单帧,而 ISO-BMFF 家族的动画
// 判断由 avif.go 的 IsAnimatedImageBytes 负责(avis brand)。这里不直接重用
// IsAnimatedImageBytes 是因为它是导出的、且还被附件路径的 EncodeAVIF 使用
// 来决定“保留动画”,附件侧语义相反(附件要留动画),加判断会污染它的契约。
func isMultiFrameImage(data []byte) bool {
	if IsAnimatedImageBytes(data) {
		return true
	}
	return isAnimatedPNGBytes(data)
}

// isAnimatedPNGBytes 报告 PNG 是否带 acTL 块(动画 PNG)。按 chunk 序列走完
// 前 8 字节签名后的每个 chunk,而不是 bytes.Contains——chunk 名可能偶然出现在
// IDAT 数据里。
func isAnimatedPNGBytes(data []byte) bool {
	if len(data) < 16 || string(data[0:8]) != "\x89PNG\r\n\x1a\n" {
		return false
	}
	for off := 8; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		if string(data[off+4:off+8]) == "acTL" {
			return true
		}
		off += 8 + size + 4 // chunk 长 + 类型 + CRC
	}
	return false
}

// processServerAssetImage 是头像、logo、banner、链接预览四条服务端资产上传路径
// 共用的实现:读出全部字节(限 cfg.MaxUploadSize)→ 上传期缩放到上限 →
// 编码为**有损 WebP**(VP8)。
//
// 【本地改动 2026-09-13】此前这四条路径都是 Go 解码 + resizeToFit +
// nativewebp.Encode,而 nativewebp v1.3.0 只写 VP8L 无损块(writer.go 里硬编码
// buf.Write([]byte("VP8L")),Options 只有 UseExtendedFormat/CompressionLevel、
// 没有质量档),所以头像/logo/banner 存的是接近无损的大字节。现在改走 ffmpeg
// libwebp 有损编码(-q:v = webpStorageQuality),和 room 附件「上传期编码一次」
// 的策略对齐:存一份、发一份,请求期不再缩放重编码。
//
// 【副作用,顺带修的坑】Go 侧解码器只有 jpeg/png/gif + nativewebp 注册的 webp,
// ISO-BMFF 家族(AVIF/HEIC)输入会直接 image: unknown format——附件管线此前踩过
// 这个坑。改走 ffmpeg 后这些输入在服务端资产侧也能正常上传。
//
// 【回退】ffmpeg 不可用或编码失败时退回旧的 Go + nativewebp 无损路径,保证没有
// ffmpeg 的部署行为不变(只是字节更大)。动画 GIF 永远走 Go 路径:image.Decode 只
// 取第一帧,避免把动画 logo 存成多 MB 的动画 WebP。
func processServerAssetImage(input io.Reader, maxWidth, maxHeight int, cfg Config) (io.Reader, error) {
	data, err := io.ReadAll(io.LimitReader(input, cfg.MaxUploadSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read image: %w", err)
	}
	if int64(len(data)) > cfg.MaxUploadSize {
		return nil, fmt.Errorf("image exceeds maximum upload size")
	}

	// 【本地改动 2026-09-13】服务端资产必须是**单帧**:头像/logo/banner/链接预览
	// 是身份标识不是媒体内容,取第一帧即可。多帧输入走 Go 路径(Go 解码器对动画
	// GIF/WebP/APNG 都只解第一帧),而不是让 ffmpeg 把动画输入编成动画 WebP
	// (VP8X + ANMF 多帧块,可能比源图还大)。
	//
	// 【踩坑】只拦动画 GIF 不够。2026-09-13 用本地 ffmpeg 与 cloudcone 7.0.2-static
	// 各跑了一遍:动画 GIF → ffmpeg `-c:v libwebp -f webp` 输出**动画 WebP**(ANMF
	// 块在);动画 PNG 同样输出动画 WebP;动画 WebP 则 ffmpeg 解码直接失败
	// ("Decode error rate 1 exceeds maximum 0.666667" / libwebp -22,两个环境一致),
	// 靠这个失败才回退到 Go 路径——这是歪打正着,ffmpeg 版本一变就漏,所以显式拦。
	// 只有 ISO-BMFF 家族(AVIF/HEIC)例外:Go 解码器不认(image: unknown format),
	// 只能走 ffmpeg,实测 ffmpeg 对它们输出单帧,无回归。
	if isMultiFrameImage(data) && !isHEIFFamilyBytes(data) {
		return processServerAssetImageGo(data, maxWidth, maxHeight, cfg)
	}

	if result, err := TransformImageWithFFmpeg(data, maxWidth, maxHeight, FitContain, TransformOptions{
		JPEGQuality: webpStorageQuality,
	}, cfg.FFmpegPath); err == nil {
		if encoded, rerr := io.ReadAll(result.Reader); rerr == nil && len(encoded) > 0 {
			return bytes.NewReader(encoded), nil
		}
	}

	return processServerAssetImageGo(data, maxWidth, maxHeight, cfg)
}

// processServerAssetImageGo 是 processServerAssetImage 的回退路径:Go 解码 +
// resizeToFit + nativewebp 无损 WebP(VP8L)。这是 2026-09-13 之前的原始行为,
// 保留它是为了让没有 ffmpeg 的部署不回归。
func processServerAssetImageGo(data []byte, maxWidth, maxHeight int, cfg Config) (io.Reader, error) {
	img, err := decodeBoundedImage(bytes.NewReader(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	resized := resizeToFit(img, maxWidth, maxHeight)

	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, resized, nil); err != nil {
		return nil, fmt.Errorf("failed to encode to webp: %w", err)
	}

	return bytes.NewReader(buf.Bytes()), nil
}

type AttachmentImageResult struct {
	// Original contains the original image bytes (unchanged)
	Original []byte
	// Width of the original image
	Width int
	// Height of the original image
	Height int
}

// ProcessAttachmentImage reads an image and extracts metadata (dimensions).
// The original image is returned as-is (not re-encoded).
// Thumbnails are generated on-the-fly via the transform system.
// Uses default config values.
func ProcessAttachmentImage(input io.Reader) (*AttachmentImageResult, error) {
	return ProcessAttachmentImageWithConfig(input, DefaultConfig())
}

// ProcessAttachmentImageWithConfig 读图并提取元数据(尺寸)。
// 原图原样返回(这里不重编码)。缩略图由 transform 系统按需生成。
// 输入超过 cfg.MaxUploadSize 或无法解码时报错。
// 【本地改动说明 32e1f566 + 2026-09-02 + 2026-09-12】历史上这是唯一允许
// WebP 重编码的上传路径(room 附件上传在开关开启且有 ffmpeg libwebp 编码器
// 时对结果调用 EncodeWebP)。**2026-09-12 起已停用**:room 附件上传改走
// attachment_image.go 的 PrepareAttachmentImage(统一产出原尺寸 AVIF,不
// 再生成衍生图)。本函数保留是因为它仍是被导出的公共 API,且上面的
// imageorient.Decode 只认 image 包已注册的解码器,AVIF/HEIC 输入会在
// 这里报错——不要在新路径上依赖它读尺寸,用 imageDimensions。
func ProcessAttachmentImageWithConfig(input io.Reader, cfg Config) (*AttachmentImageResult, error) {
	// Read all input into memory (limited to MaxUploadSize)
	originalBytes, err := readAndValidateImage(input, cfg.MaxUploadSize)
	if err != nil {
		return nil, err
	}

	// Decode the image to get dimensions (applies EXIF orientation for correct dimensions)
	img, _, err := imageorient.Decode(bytes.NewReader(originalBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	bounds := img.Bounds()

	return &AttachmentImageResult{
		Original: originalBytes,
		Width:    bounds.Dx(),
		Height:   bounds.Dy(),
	}, nil
}

// resizeToFit resizes an image to fit within the specified maximum width and height
// while maintaining aspect ratio. If the image is already smaller, it's returned as-is.
func resizeToFit(img image.Image, maxWidth, maxHeight int) image.Image {
	bounds := img.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()

	// If image is already within bounds, return as-is
	if srcWidth <= maxWidth && srcHeight <= maxHeight {
		return img
	}

	// Calculate the scaling factor to fit within bounds
	widthRatio := float64(maxWidth) / float64(srcWidth)
	heightRatio := float64(maxHeight) / float64(srcHeight)
	ratio := widthRatio
	if heightRatio < widthRatio {
		ratio = heightRatio
	}

	// Calculate new dimensions
	newWidth := int(float64(srcWidth) * ratio)
	newHeight := int(float64(srcHeight) * ratio)

	// Create destination image
	dst := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))

	// Use high-quality CatmullRom interpolation for resizing
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, xdraw.Over, nil)

	return dst
}

// hasTransparency checks if an image contains any non-opaque pixels.
func hasTransparency(img image.Image) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a < 0xffff {
				return true
			}
		}
	}
	return false
}

// TransformImage transforms an image according to the specified dimensions and fit mode.
// For animated GIFs, converts to animated WebP with proper frame compositing.
// For images with transparency, returns WebP to preserve alpha.
// For opaque images, returns JPEG for smaller file sizes.
func TransformImage(data []byte, width, height int, fit FitMode) (*TransformResult, error) {
	return TransformImageWithOptions(data, width, height, fit, TransformOptions{
		JPEGQuality: DefaultTransformJPEGQuality,
	})
}

// TransformImageWithOptions transforms an image with explicit encoding options.
func TransformImageWithOptions(data []byte, width, height int, fit FitMode, options TransformOptions) (*TransformResult, error) {
	if options.JPEGQuality < 1 || options.JPEGQuality > 100 {
		return nil, fmt.Errorf("invalid JPEG quality: %d", options.JPEGQuality)
	}
	if int64(len(data)) > DefaultMaxUploadSize {
		return nil, fmt.Errorf("image exceeds maximum size of %d bytes", DefaultMaxUploadSize)
	}
	if err := validateDecodedImageSize(data); err != nil {
		return nil, err
	}

	// Check if this is an animated GIF - handle specially to preserve animation
	if IsAnimatedGIF(data) {
		return transformAnimatedGIF(data, width, height, fit)
	}

	// Decode the input image (applies EXIF orientation)
	img, _, err := imageorient.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	var transformed image.Image

	switch fit {
	case FitContain:
		// Fit within bounds, preserve aspect ratio (same as resizeToFit)
		transformed = resizeToFit(img, width, height)

	case FitCover:
		// Fill bounds and center-crop excess
		transformed = resizeToCover(img, width, height)

	case FitExact:
		// Stretch to exact dimensions
		transformed = resizeToExact(img, width, height)

	default:
		return nil, fmt.Errorf("invalid fit mode: %s", fit)
	}

	// Encode to WebP if the image has transparency (JPEG doesn't support alpha),
	// otherwise use JPEG for smaller file sizes.
	var buf bytes.Buffer
	if hasTransparency(transformed) {
		if err := nativewebp.Encode(&buf, transformed, nil); err != nil {
			return nil, fmt.Errorf("failed to encode to webp: %w", err)
		}
		return &TransformResult{
			Reader:      bytes.NewReader(buf.Bytes()),
			ContentType: "image/webp",
		}, nil
	}

	if err := jpeg.Encode(&buf, transformed, &jpeg.Options{Quality: options.JPEGQuality}); err != nil {
		return nil, fmt.Errorf("failed to encode to jpeg: %w", err)
	}

	return &TransformResult{
		Reader:      bytes.NewReader(buf.Bytes()),
		ContentType: "image/jpeg",
	}, nil
}

// transformAnimatedGIF converts an animated GIF to animated WebP with proper
// frame compositing. GIF frames can be partial sub-rectangles with disposal
// methods that control how the canvas is updated between frames. This function
// composites frames correctly onto a running canvas before resizing, avoiding
// the palette destruction and compositing bugs inherent in frame-by-frame GIF
// resizing.
func transformAnimatedGIF(data []byte, width, height int, fit FitMode) (*TransformResult, error) {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to decode animated GIF: %w", err)
	}

	if len(g.Image) == 0 {
		return nil, fmt.Errorf("GIF has no frames")
	}
	if len(g.Image) > MaxAnimatedImageFrames {
		return nil, fmt.Errorf("animated image has %d frames, exceeding the supported limit of %d", len(g.Image), MaxAnimatedImageFrames)
	}

	// Use Config dimensions (the logical canvas size), not first frame bounds
	// which may be a sub-rectangle.
	canvasWidth := g.Config.Width
	canvasHeight := g.Config.Height
	if canvasWidth == 0 || canvasHeight == 0 {
		canvasWidth = g.Image[0].Bounds().Max.X
		canvasHeight = g.Image[0].Bounds().Max.Y
	}
	if int64(canvasWidth)*int64(canvasHeight)*int64(len(g.Image)) > MaxAnimatedImageCumulativePixels {
		return nil, fmt.Errorf("animated image exceeds the cumulative pixel limit of %d", MaxAnimatedImageCumulativePixels)
	}

	// Calculate target dimensions
	var newWidth, newHeight int
	switch fit {
	case FitContain:
		if canvasWidth <= width && canvasHeight <= height {
			newWidth, newHeight = canvasWidth, canvasHeight
		} else {
			widthRatio := float64(width) / float64(canvasWidth)
			heightRatio := float64(height) / float64(canvasHeight)
			ratio := widthRatio
			if heightRatio < widthRatio {
				ratio = heightRatio
			}
			newWidth = int(float64(canvasWidth) * ratio)
			newHeight = int(float64(canvasHeight) * ratio)
		}
	case FitCover, FitExact:
		newWidth, newHeight = width, height
	default:
		return nil, fmt.Errorf("invalid fit mode: %s", fit)
	}

	// Composite GIF frames onto a running canvas, handling disposal methods
	composited := compositeGIFFrames(g)

	// Resize each composited frame and build WebP animation
	needsResize := newWidth != canvasWidth || newHeight != canvasHeight
	webpFrames := make([]image.Image, len(composited))

	for i, frame := range composited {
		if needsResize {
			resized := image.NewNRGBA(image.Rect(0, 0, newWidth, newHeight))
			xdraw.CatmullRom.Scale(resized, resized.Bounds(), frame, frame.Bounds(), xdraw.Over, nil)
			webpFrames[i] = resized
		} else {
			webpFrames[i] = frame
		}
	}

	// Convert GIF timing and loop semantics to WebP
	durations := make([]uint, len(g.Image))
	disposals := make([]uint, len(g.Image))
	for i := range g.Image {
		delay := 0
		if i < len(g.Delay) {
			delay = g.Delay[i]
		}
		durations[i] = uint(delay) * 10 // centiseconds → milliseconds
		disposals[i] = 0                // keep — compositing already resolved
	}

	ani := &nativewebp.Animation{
		Images:          webpFrames,
		Durations:       durations,
		Disposals:       disposals,
		LoopCount:       convertGIFLoopCount(g.LoopCount),
		BackgroundColor: 0, // transparent
	}

	var buf bytes.Buffer
	if err := nativewebp.EncodeAll(&buf, ani, nil); err != nil {
		return nil, fmt.Errorf("failed to encode animated WebP: %w", err)
	}

	return &TransformResult{
		Reader:      bytes.NewReader(buf.Bytes()),
		ContentType: "image/webp",
	}, nil
}

// compositeGIFFrames composites GIF frames onto a running canvas, correctly
// handling disposal methods and sub-rectangle frames. Returns full-canvas
// NRGBA snapshots for each frame, ready for resizing and WebP encoding.
func compositeGIFFrames(g *gif.GIF) []*image.NRGBA {
	canvasWidth := g.Config.Width
	canvasHeight := g.Config.Height
	if canvasWidth == 0 || canvasHeight == 0 {
		canvasWidth = g.Image[0].Bounds().Max.X
		canvasHeight = g.Image[0].Bounds().Max.Y
	}

	canvas := image.NewNRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	var previous *image.NRGBA
	result := make([]*image.NRGBA, len(g.Image))

	for i, frame := range g.Image {
		disposal := byte(0)
		if g.Disposal != nil && i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}

		// For DisposalPrevious, snapshot canvas BEFORE drawing this frame
		if disposal == gif.DisposalPrevious {
			previous = image.NewNRGBA(canvas.Bounds())
			copy(previous.Pix, canvas.Pix)
		}

		// Draw frame onto canvas (Over respects GIF transparent index)
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)

		// Snapshot the composited canvas for this frame
		result[i] = image.NewNRGBA(canvas.Bounds())
		copy(result[i].Pix, canvas.Pix)

		// Apply disposal for the next frame
		switch disposal {
		case gif.DisposalBackground:
			// Clear the frame's rectangle to transparent
			draw.Draw(canvas, frame.Bounds(), image.Transparent, image.Point{}, draw.Src)
		case gif.DisposalPrevious:
			// Restore canvas from snapshot taken before this frame
			if previous != nil {
				copy(canvas.Pix, previous.Pix)
			}
			// DisposalNone (0x01) and unspecified (0x00): leave canvas as-is
		}
	}

	return result
}

// convertGIFLoopCount converts a GIF loop count to a WebP loop count.
// GIF: 0=forever, -1=once, N=play N+1 times.
// WebP: 0=infinite, N=play N times.
func convertGIFLoopCount(gifLoop int) uint16 {
	switch {
	case gifLoop == 0:
		return 0 // infinite
	case gifLoop < 0:
		return 1 // play once
	default:
		n := gifLoop + 1
		if n > 65535 {
			n = 65535
		}
		return uint16(n)
	}
}

// resizeToCover resizes and crops an image to fill the target dimensions while
// preserving aspect ratio. The image is center-cropped if necessary.
func resizeToCover(img image.Image, targetWidth, targetHeight int) image.Image {
	bounds := img.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()

	// Calculate the scaling factor to cover the entire target area
	widthRatio := float64(targetWidth) / float64(srcWidth)
	heightRatio := float64(targetHeight) / float64(srcHeight)
	ratio := widthRatio
	if heightRatio > widthRatio {
		ratio = heightRatio
	}

	// Calculate scaled dimensions (will be >= target dimensions)
	scaledWidth := int(float64(srcWidth) * ratio)
	scaledHeight := int(float64(srcHeight) * ratio)

	// Create intermediate scaled image
	scaled := image.NewRGBA(image.Rect(0, 0, scaledWidth, scaledHeight))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), img, bounds, xdraw.Over, nil)

	// Calculate crop offset to center the image
	cropX := (scaledWidth - targetWidth) / 2
	cropY := (scaledHeight - targetHeight) / 2

	// Create destination image with target dimensions
	dst := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))

	// Copy the center portion
	draw.Draw(
		dst,
		dst.Bounds(),
		scaled,
		image.Point{X: cropX, Y: cropY},
		draw.Src,
	)

	return dst
}

// resizeToExact stretches an image to exactly match the target dimensions,
// potentially distorting the aspect ratio.
func resizeToExact(img image.Image, targetWidth, targetHeight int) image.Image {
	bounds := img.Bounds()

	// Create destination image with exact target dimensions
	dst := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))

	// Scale to exact dimensions using CatmullRom interpolation
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, xdraw.Over, nil)

	return dst
}
