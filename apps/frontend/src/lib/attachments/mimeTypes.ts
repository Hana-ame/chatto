/**
 * 【本地改动 2026-09-12】上传前解析文件的真实 MIME 类型。
 *
 * 背景:浏览器/操作系统声明的 MIME 类型不可信——它来自扩展名注册表而不
 * 是文件字节,扩展名与真实编码不一致时声明就是错的,部分系统还会直接报
 * `application/octet-stream`。这个声明不是显示用的:它决定服务端走图片
 * 管线还是视频管线,于是被误报成视频的图片既不重编码,前端还会按视频渲染。
 * 发现于 2026-09-12 .avif 附件被认成 mp4 的 bug。
 *
 * 规则:Chatto 接受的已知格式以扩展名为准,只在两侧顶层媒体类别冲突时
 * 覆盖浏览器声明(video→image、video→audio);类别一致时保留声明,空声明
 * 和 octet-stream 用扩展名补齐。
 *
 * MIME type resolution for files uploaded from the browser.
 *
 * The browser's declared type is not reliable: it comes from the operating
 * system's extension registry, not from the bytes, so it is wrong whenever
 * the extension disagrees with the real codec, and some systems report
 * `application/octet-stream` outright. The value is not cosmetic: it tells
 * the server which processing pipeline to run, so a misdeclared AVIF image
 * would enter the video pipeline and never render as a picture.
 *
 * The filename extension is authoritative for the formats Chatto accepts.
 */

/** MIME type for each file extension Chatto treats as a known format. */
const EXTENSION_MIME_TYPES: Record<string, string> = {
  '.avif': 'image/avif',
  '.gif': 'image/gif',
  '.heic': 'image/heic',
  '.heif': 'image/heif',
  '.jpeg': 'image/jpeg',
  '.jpg': 'image/jpeg',
  '.png': 'image/png',
  '.webp': 'image/webp',
  '.flac': 'audio/flac',
  '.m4a': 'audio/mp4',
  '.mp3': 'audio/mpeg',
  '.ogg': 'audio/ogg',
  '.wav': 'audio/wav',
  '.mp4': 'video/mp4',
  '.mov': 'video/quicktime',
  '.webm': 'video/webm',
  '.txt': 'text/plain',
  '.pdf': 'application/pdf',
  '.zip': 'application/zip'
};

/** MIME type declared by the filename extension, or null when it is not known. */
export function mimeTypeForExtension(filename: string): string | null {
  const dot = filename.lastIndexOf('.');
  if (dot <= 0 || dot === filename.length - 1) return null;
  return EXTENSION_MIME_TYPES[filename.slice(dot).toLowerCase()] ?? null;
}

/** Top-level media category of a MIME type (`image`, `video`, `application`). */
function mediaCategory(mimeType: string): string {
  return mimeType.split('/')[0];
}

/**
 * MIME type to upload for `file`: the browser's value when it is reliable,
 * otherwise the value derived from the filename extension. A file name that
 * names a known format overrides a browser claim for another media category.
 */
export function resolveFileMimeType(file: File): string {
  const declared = file.type.trim().toLowerCase();
  const fromExtension = mimeTypeForExtension(file.name);

  if (!fromExtension) return declared || 'application/octet-stream';
  if (!declared || declared === 'application/octet-stream') return fromExtension;
  if (mediaCategory(declared) !== mediaCategory(fromExtension)) return fromExtension;
  return declared;
}

/**
 * `file` carrying `resolveFileMimeType(file)`, or `file` itself when the
 * browser already reported the type we would choose. `File.type` is read-only,
 * so a mismatch needs a wrapper: every downstream check and the upload request
 * then read the corrected type.
 */
export function withResolvedMimeType(file: File): File {
  const mimeType = resolveFileMimeType(file);
  if (file.type.toLowerCase() === mimeType) return file;
  return new File([file], file.name, { type: mimeType });
}
