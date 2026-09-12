package assets

// 【本地改动 2026-09-12】按文件头嗅探上传内容的真实类型。
//
// 背景:上传方声明的 Content-Type 不可信——浏览器按操作系统的扩展名注册表填
// File.type,扩展名与真实编码不一致时声明就是错的。服务端此前完全信任声明,
// 而声明决定走图片管线还是视频管线:被误报成视频的图片既不重编码也不生成
// 衍生图,前端还会按视频渲染。
//
// 本函数只看文件头,只认图片格式(即历史上被误报成视频的那类类型)。返回
// "" 表示"文件头过短或无法识别",调用方必须把它当作无结论、保留原声明;
// 绝不能当成正向判断。反向永不发生:没有任何图片签名会把视频重分类为图片。
func DetectContentType(header []byte) string {
	switch {
	case isAVIFBytes(header):
		return "image/avif"
	// 【本地改动 2026-09-12】major brand 只认 avif/avis。avis 是动画 AVIF
	// (AV1 Sequence)的 brand:上传动画 GIF 转出的 AVIF 就是 avis,若这里不认,
	// 它会在服务端被当成未知容器、按视频路径处理。av01 是 ISO-BMFF 的通用
	// AV1 容器,不是 HEIF 图片家族,不能算图片。
	case isAVISImageBytes(header):
		return "image/avif"
	case isHEICImageBytes(header):
		return "image/heic"
	case isWebPBytes(header):
		return "image/webp"
	case len(header) >= 8 && string(header[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(header) >= 3 && header[0] == 0xff && header[1] == 0xd8 && header[2] == 0xff:
		return "image/jpeg"
	case len(header) >= 6 && (string(header[:6]) == "GIF87a" || string(header[:6]) == "GIF89a"):
		return "image/gif"
	default:
		return ""
	}
}

// heifMajorBrand 返回 ISO-BMFF ftyp box 的 major brand(小写化),头不足时
// 返回空串。
func heifMajorBrand(header []byte) string {
	if len(header) < 12 || string(header[4:8]) != "ftyp" {
		return ""
	}
	brand := string(header[8:12])
	for i := 0; i < len(brand); i++ {
		if brand[i] >= 'A' && brand[i] <= 'Z' {
			brand = brand[:i] + string(brand[i]+'a'-'A') + brand[i+1:]
		}
	}
	return brand
}

// isAVISImageBytes 报告是否以动画 AVIF 的 major brand 开头。
func isAVISImageBytes(header []byte) bool {
	return heifMajorBrand(header) == "avis"
}

// isHEICImageBytes 报告是否以 HEIC/HEIF 图片的 major brand 开头。这类字节
// 是图片,但服务端 ffmpeg 没有 heif 解码器(见 avif.go 顶部注释),无法编码
// 成 AVIF,只能原样存储;识别出来至少能让它走图片管线、并留下正确的
// Content-Type,而不是被当成视频。
func isHEICImageBytes(header []byte) bool {
	switch heifMajorBrand(header) {
	case "heif", "heix", "heis", "heic", "hevc", "mif1":
		return true
	default:
		return false
	}
}
