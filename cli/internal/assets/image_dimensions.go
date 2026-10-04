package assets

import (
	"bytes"
	"image"
)

// 【本地改动 2026-09-12】图片字节尺寸探测,不假设 Go 认识输入格式。
//
// 目的:上传图片时要先做尺寸/像素上限校验(防压缩炸弹),再交给 ffmpeg 编码。
// 旧实现直接用 image.DecodeConfig,而 Go 对 AVIF/HEIC 没有解码器,一律
// "image: unknown format" 硬失败——AVIF 正是 2026-09-02 前本仓库附件的存储
// 格式,用户把聊天里保存的图再传回去就传不上去。这是本次策略重写要修的根因。
//
// 思路:先试 image 包已注册的解码器(jpeg/png/gif,以及本包经
// HugoSmits86/nativewebp 注册的 webp),失败再按 ISO-BMFF 手写读 AVIF/HEIC
// 的 ispe box。读不出尺寸返回 ok=false,调用方必须把它当成硬错误——尺寸
// 上限是资源上限,不能因为"格式新"就绕过。
//
// 为什么不手写 WebP 解析:本包的 nativewebp 已经把 WEBP 注册进 image 包
// (image.DecodeConfig 对静态与动画 WebP 都返回正确画布尺寸,2026-09-12 用
// ffmpeg 生成的样本实测过),再加一份 RIFF 解析只是重复实现;真正没有解码器
// 的只有 ISO-BMFF 家族。
//
// 注意:image.DecodeConfig 只读文件头不解码像素,成本可忽略。
func imageDimensions(data []byte) (width, height int, ok bool) {
	if len(data) == 0 {
		return 0, 0, false
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err == nil {
		return config.Width, config.Height, true
	}
	return AVIFImageDimensions(data)
}
