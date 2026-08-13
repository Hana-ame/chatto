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

// AVIF re-encodes uploaded attachment images. Chatto prefers AV1 because it
// offers the best compression of the encoders available through the ffmpeg
// binary that the video pipeline already depends on.
//
// Availability is environment-dependent (ffmpeg plus an AV1 encoder must be
// installed), so callers treat encoding as best-effort and store the original
// bytes when it is unavailable.

// ErrAVIFUnavailable reports that AVIF encoding is not possible: ffmpeg is
// not installed or has no AV1 encoder. Callers should store the original
// bytes unchanged.
var ErrAVIFUnavailable = errors.New("AVIF encoding unavailable")

const (
	// avifCRF is the libaom/libsvtav1 constant-rate-factor used for uploaded
	// images. AV1 CRF values do not map 1:1 to JPEG/WebP quality; 30 is a
	// visually lossless-ish default that stays well under lossless size.
	avifCRF = 30
	// avifFastPreset is the libsvtav1 preset selected for uploads. Presets
	// range 0 (slowest/best) to 13 (fastest); 10 keeps encode latency low
	// while staying visually acceptable at avifCRF.
	avifFastPreset = 10
	// avifEncodeTimeout bounds a single ffmpeg encode so a misbehaving
	// encoder cannot stall an upload request indefinitely.
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
	// avifEncoderByFFmpeg caches the selected encoder per ffmpeg path so
	// uploads do not re-probe the encoder list on every image.
	avifEncoderByFFmpeg = map[string]avifEncoder{}
	avifUnavailable     = map[string]bool{}
)

// EncodeAVIF re-encodes image bytes to an AVIF still image using ffmpeg.
// cfg.FFmpegPath is used verbatim; when empty, ffmpeg is resolved from PATH.
// When cfg.AVIFEnabled is false the function reports ErrAVIFUnavailable so
// callers store original bytes unchanged, exactly like a missing ffmpeg.
//
// The fastest available encoder is preferred (libsvtav1, falling back to
// libaom-av1), both at avifCRF. Returns ErrAVIFUnavailable when ffmpeg or an
// AV1 encoder cannot be found; other errors are transient encode failures.
func EncodeAVIF(ctx context.Context, data []byte, cfg Config) ([]byte, error) {
	if !cfg.AVIFEnabled {
		return nil, ErrAVIFUnavailable
	}
	ffmpegPath := cfg.FFmpegPath
	// Resolve an empty path here as well as in selectAVIFEncoder: the probe
	// resolves internally but the encode below needs the concrete binary.
	// Broken on 2026-08-14 when TestEncodeAVIFResolvesFFmpegFromPath failed
	// with "exec: no command" after the temp-file rework.
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

	// Write the AVIF to a seekable temp file instead of pipe:1. libsvtav1
	// (the preferred encoder) hangs when its AVIF muxer gets a non-seekable
	// output: it neither errors nor exits nor writes data. Discovered on
	// Ubuntu ffmpeg 6.1 (2026-08-14) while merging upstream main; a plain
	// `-f avif pipe:1` run with libsvtav1 stuck until the context timeout.
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

// selectAVIFEncoder resolves the fastest AV1 encoder available for the given
// ffmpeg binary, caching the result per path. A missing ffmpeg or encoder
// results in ErrAVIFUnavailable (also cached).
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

// TransformImageWithFFmpeg transforms image bytes like TransformImageWithOptions,
// but when the input is AVIF it uses ffmpeg for both decoding and scaling,
// because the standard library image decoders do not handle AVIF. The output
// is always lossy WebP (alpha-capable) at the JPEG-quality option, matching
// the size class of the existing JPEG derivative path.
//
// ffmpegPath is used verbatim; when empty, ffmpeg is resolved from PATH. A
// missing ffmpeg is a hard error for AVIF input — there is no other decoder.
// This decodes previously stored AVIF attachment images at render time; it
// never re-encodes avatars, branding, or link previews (those are WebP-only).
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

// isAVIFBytes reports whether data starts with an ISO-BMFF file whose major
// brand is AVIF ("ftyp" box followed by the "avif" brand).
func isAVIFBytes(data []byte) bool {
	return len(data) >= 12 &&
		data[4] == 'f' && data[5] == 't' && data[6] == 'y' && data[7] == 'p' &&
		data[8] == 'a' && data[9] == 'v' && data[10] == 'i' && data[11] == 'f'
}
