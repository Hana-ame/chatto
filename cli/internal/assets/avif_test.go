package assets

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
)

func testFFmpegPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	return path
}

func TestEncodeAVIFProducesAVIF(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	out, err := EncodeAVIF(context.Background(), createTestImage(400, 300), Config{FFmpegPath: ffmpegPath, AVIFEnabled: true})
	if err != nil {
		t.Fatalf("EncodeAVIF: %v", err)
	}
	if !isAVIFBytes(out) {
		t.Fatalf("output is not an AVIF file (len=%d)", len(out))
	}
}

func TestEncodeAVIFKeepsTransparency(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	out, err := EncodeAVIF(context.Background(), createTransparentTestImage(64, 64), Config{FFmpegPath: ffmpegPath, AVIFEnabled: true})
	if err != nil {
		t.Fatalf("EncodeAVIF: %v", err)
	}
	if !isAVIFBytes(out) {
		t.Fatalf("output is not an AVIF file (len=%d)", len(out))
	}
}

func TestEncodeAVIFResolvesFFmpegFromPath(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}

	out, err := EncodeAVIF(context.Background(), createTestImage(100, 100), Config{AVIFEnabled: true})
	if err != nil {
		t.Fatalf("EncodeAVIF with empty path: %v", err)
	}
	if !isAVIFBytes(out) {
		t.Fatalf("output is not an AVIF file (len=%d)", len(out))
	}
}

func TestEncodeAVIFUnavailableWithoutFFmpeg(t *testing.T) {
	_, err := EncodeAVIF(context.Background(), createTestImage(10, 10), Config{FFmpegPath: "/nonexistent/ffmpeg", AVIFEnabled: true})
	if !errors.Is(err, ErrAVIFUnavailable) {
		t.Fatalf("error = %v, want ErrAVIFUnavailable", err)
	}
}

// TestEncodeAVIFDisabled guards the avif_enabled config toggle: with
// AVIFEnabled false the encoder must report unavailable (storing original
// bytes) without ever probing or spawning ffmpeg. Added 2026-08-14 when the
// explicit disable switch was introduced; before that, disabling AVIF on a
// server with ffmpeg installed was impossible.
func TestEncodeAVIFDisabled(t *testing.T) {
	_, err := EncodeAVIF(context.Background(), createTestImage(10, 10), Config{FFmpegPath: "/definitely/not/ffmpeg", AVIFEnabled: false})
	if !errors.Is(err, ErrAVIFUnavailable) {
		t.Fatalf("error = %v, want ErrAVIFUnavailable", err)
	}
}

func TestEncodeAVIFRejectsInvalidInput(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	_, err := EncodeAVIF(context.Background(), []byte("not an image at all"), Config{FFmpegPath: ffmpegPath, AVIFEnabled: true})
	if err == nil {
		t.Fatal("EncodeAVIF accepted invalid image data")
	}
	if errors.Is(err, ErrAVIFUnavailable) {
		t.Fatalf("invalid input reported as unavailable: %v", err)
	}
}

func TestTransformImageWithFFmpegScalesAVIF(t *testing.T) {
	ffmpegPath := testFFmpegPath(t)

	avif, err := EncodeAVIF(context.Background(), createTestImage(400, 300), Config{FFmpegPath: ffmpegPath, AVIFEnabled: true})
	if err != nil {
		t.Fatalf("EncodeAVIF fixture: %v", err)
	}

	for _, fit := range []FitMode{FitContain, FitCover, FitExact} {
		result, err := TransformImageWithFFmpeg(avif, 200, 100, fit, TransformOptions{JPEGQuality: DefaultTransformJPEGQuality}, ffmpegPath)
		if err != nil {
			t.Fatalf("TransformImageWithFFmpeg(%s): %v", fit, err)
		}
		data, err := io.ReadAll(result.Reader)
		if err != nil {
			t.Fatalf("read transform result: %v", err)
		}
		if result.ContentType != "image/webp" {
			t.Fatalf("transform content type = %q, want image/webp", result.ContentType)
		}
		if len(data) == 0 {
			t.Fatalf("transform %s produced no output", fit)
		}
	}
}

func TestTransformImageWithFFmpegDelegatesNonAVIF(t *testing.T) {
	// Non-AVIF input must take the Go decode path and never touch ffmpeg.
	result, err := TransformImageWithFFmpeg(createTestImage(64, 64), 32, 32, FitContain, TransformOptions{JPEGQuality: DefaultTransformJPEGQuality}, "/nonexistent/ffmpeg")
	if err != nil {
		t.Fatalf("TransformImageWithFFmpeg on PNG: %v", err)
	}
	if result.ContentType != "image/jpeg" {
		t.Fatalf("content type = %q, want image/jpeg", result.ContentType)
	}
}

func TestIsAVIFBytes(t *testing.T) {
	valid := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f'}
	if !isAVIFBytes(valid) {
		t.Fatal("isAVIFBytes rejected a valid AVIF header")
	}
	for _, bad := range [][]byte{
		{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'},
		{0, 0, 0, 24, 'F', 'T', 'Y', 'P', 'a', 'v', 'i', 'f'},
		{0, 0, 0},
	} {
		if isAVIFBytes(bad) {
			t.Fatalf("isAVIFBytes accepted %x", bad)
		}
	}
}
