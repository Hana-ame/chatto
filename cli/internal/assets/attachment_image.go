package assets

import (
	"context"
	"io"
)

// 【本地改动 2026-09-12】不再 import imageorient:它内部用 image.Decode,
// 只认 image 包已注册的解码器,AVIF/HEIC 输入必败——那是旧路径把
// 「把聊天里保存的图再传回去」搞挂的根因。尺寸改由 imageDimensions
// (先试注册解码器,失败再手写读 ISO-BMFF 头)统一给出。

// PreparedAttachmentImage 是一次上传的图片处理结果。
type PreparedAttachmentImage struct {
	// Content 是要存储的字节:成功时是 AVIF,ffmpeg 不可用时是上传原图。
	Content []byte
	// ContentType 与 Content 对应。回退存原图且文件头认不出格式时为空,
	// 调用方此时应保留客户端声明的类型(可能仍是错的,但没有更好结论)。
	ContentType string
	// Width/Height 是存储字节的显示尺寸(已按 EXIF 方向矫正)。0 表示读不出,
	// 调用方按"未知尺寸"存储,前端自己按图片自适应。
	Width  int
	Height int
}

// PrepareAttachmentImage 处理一张上传的图片:读出全部字节(受
// cfg.MaxUploadSize 限制),尽量重编码为原尺寸 AVIF,并给出存储字节的宽高。
//
// 【本地改动 2026-09-12】取代旧的 ProcessAttachmentImageWithConfig +
// EncodeWebP 组合,是"上传只产出一份原尺寸 AVIF"这条策略的唯一实现,单请求
// 路径与分片上传路径都走它。
//
// 为什么要绕过 Go 解码器:本包经 nativewebp 注册了 WebP 解码器,但 AVIF
// 和 HEIC 在 Go 里仍然没有解码器(image: unknown format),会让旧的
// imageorient.Decode 直接失败——于是"把聊天里存下来的 AVIF 再传回去"这种
// 最常见操作根本传不上去。ffmpeg 能解全部这些格式,尺寸改从产出的 AVIF 的
// ispe box 读,回退时也从 imageDimensions 拿,不再依赖 Go 解码。
//
// 回退语义(best-effort):ffmpeg 缺失、没有 AV1 编码器、或 AVIFEnabled=false
// 时存原图字节,并把编码错误原样返回;调用方对 ErrAVIFUnavailable 静默处理,
// 其余错误记日志。只有"读不出字节"或"超过 MaxUploadSize"才是硬错误(返回
// nil, err)。
func PrepareAttachmentImage(ctx context.Context, input io.Reader, cfg Config) (*PreparedAttachmentImage, error) {
	data, err := readAndValidateImage(input, cfg.MaxUploadSize)
	if err != nil {
		return nil, err
	}
	// 尺寸兜底:AVIF 编码失败要存原图时,能读出的尺寸就用它。读不出不算
	// 错误,只是元数据里宽高为 0,前端按图片自适应。
	fallbackWidth, fallbackHeight := goImageDimensions(data)

	encoded, encErr := EncodeAVIF(ctx, data, cfg)
	if encErr != nil {
		return &PreparedAttachmentImage{
			Content:     data,
			ContentType: DetectContentType(data),
			Width:       fallbackWidth,
			Height:      fallbackHeight,
		}, encErr
	}
	width, height, ok := AVIFImageDimensions(encoded)
	if !ok {
		width, height = fallbackWidth, fallbackHeight
	}
	return &PreparedAttachmentImage{
		Content:     encoded,
		ContentType: "image/avif",
		Width:       width,
		Height:      height,
	}, nil
}

// goImageDimensions 读出上传图片的显示尺寸。jpeg/png/gif 走 Go 解码器
// (同时做 EXIF 方向矫正),WebP 与 ISO-BMFF(AVIF/HEIC)手写读头——Go 没有
// 这两类解码器。读不出返回 0,0;调用方把 0 当成「未知尺寸」而非错误。
func goImageDimensions(data []byte) (int, int) {
	w, h, ok := imageDimensions(data)
	if !ok {
		return 0, 0
	}
	return w, h
}
