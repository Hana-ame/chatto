package assets

import "testing"

// 【本地改动 2026-09-12】嗅探器用例:只认图片签名,无法识别必须返回空。
func TestDetectContentTypeIdentifiesImageSignatures(t *testing.T) {
	tests := []struct {
		name   string
		header []byte
		want   string
	}{
		{"avif major brand", []byte("\x00\x00\x00\x18ftypavif"), "image/avif"},
		// 【本地改动 2026-09-12】avis 是动画 AVIF(AV1 Sequence)的 brand:
		// 上传动画 GIF 转出的就是它,必须算图片,否则会被当视频路径处理。
		{"avis animated brand is AVIF", []byte("\x00\x00\x00\x0cftypavis"), "image/avif"},
		{"heic major brand", []byte("\x00\x00\x00\x18ftypheic"), "image/heic"},
		{"heix major brand", []byte("\x00\x00\x00\x18ftypheix"), "image/heic"},
		{"heif major brand is case insensitive", []byte("\x00\x00\x00\x18ftypHEIF"), "image/heic"},
		{"av01 brand is not a HEIF image", []byte("\x00\x00\x00\x18ftypav01"), ""},
		{"mp4 brand stays unclaimed", []byte("\x00\x00\x00\x18ftypmp42"), ""},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBP"), "image/webp"},
		{"png", []byte("\x89PNG\r\n\x1a\n"), "image/png"},
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0}, "image/jpeg"},
		{"gif89a", []byte("GIF89a"), "image/gif"},
		{"gif87a", []byte("GIF87a"), "image/gif"},
		{"unrecognized header", []byte("not a media file"), ""},
		{"short header", []byte("ftyp"), ""},
		{"empty header", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectContentType(tt.header); got != tt.want {
				t.Fatalf("DetectContentType() = %q, want %q", got, tt.want)
			}
		})
	}
}
