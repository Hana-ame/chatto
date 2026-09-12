// 【本地改动 2026-09-12】上传前的 MIME 解析:.avif 被系统报成 video/mp4
// 时必须纠正成 image/avif,类型已正确的文件保持同一引用。
import { describe, expect, it } from 'vitest';
import { mimeTypeForExtension, resolveFileMimeType, withResolvedMimeType } from './mimeTypes';

function makeFile(name: string, type: string): File {
  return new File([new Uint8Array(3)], name, { type });
}

describe('mimeTypeForExtension', () => {
  it('maps the extensions Chatto accepts to their MIME types', () => {
    expect(mimeTypeForExtension('photo.avif')).toBe('image/avif');
    expect(mimeTypeForExtension('photo.AVIF')).toBe('image/avif');
    expect(mimeTypeForExtension('photo.jpg')).toBe('image/jpeg');
    expect(mimeTypeForExtension('clip.mp4')).toBe('video/mp4');
    expect(mimeTypeForExtension('voice.m4a')).toBe('audio/mp4');
  });

  it('returns null for unknown or missing extensions', () => {
    expect(mimeTypeForExtension('noextension')).toBeNull();
    expect(mimeTypeForExtension('.hidden')).toBeNull();
    expect(mimeTypeForExtension('trailing.')).toBeNull();
    // .ts is TypeScript source, not MPEG transport stream.
    expect(mimeTypeForExtension('script.ts')).toBeNull();
  });
});

describe('resolveFileMimeType', () => {
  it('keeps a MIME type the browser reported correctly', () => {
    expect(resolveFileMimeType(makeFile('clip.mp4', 'video/mp4'))).toBe('video/mp4');
    expect(resolveFileMimeType(makeFile('shot.png', 'image/png'))).toBe('image/png');
  });

  it('corrects an AVIF image the operating system reports as video', () => {
    expect(resolveFileMimeType(makeFile('photo.avif', 'video/mp4'))).toBe('image/avif');
  });

  it('corrects an audio file the operating system reports as video', () => {
    expect(resolveFileMimeType(makeFile('voice.m4a', 'video/mp4'))).toBe('audio/mp4');
  });

  it('fills the MIME type from the extension when the browser knows nothing', () => {
    expect(resolveFileMimeType(makeFile('photo.avif', ''))).toBe('image/avif');
    expect(resolveFileMimeType(makeFile('photo.avif', 'application/octet-stream'))).toBe(
      'image/avif'
    );
  });

  it('keeps a browser value when the extension is not a known format', () => {
    expect(resolveFileMimeType(makeFile('bundle.js', 'text/javascript'))).toBe('text/javascript');
  });

  it('falls back to octet-stream when neither side knows', () => {
    expect(resolveFileMimeType(makeFile('bundle.js', ''))).toBe('application/octet-stream');
  });

  it('trusts the extension for the media category of a known format', () => {
    expect(resolveFileMimeType(makeFile('notes.txt', 'video/mp4'))).toBe('text/plain');
  });
});

describe('withResolvedMimeType', () => {
  it('returns the same file when its type is already correct', () => {
    const file = makeFile('shot.png', 'image/png');

    expect(withResolvedMimeType(file)).toBe(file);
  });

  it('wraps a file whose type must be corrected and keeps its name and size', () => {
    const file = makeFile('photo.avif', 'video/mp4');
    const prepared = withResolvedMimeType(file);

    expect(prepared).not.toBe(file);
    expect(prepared.name).toBe('photo.avif');
    expect(prepared.size).toBe(file.size);
    expect(prepared.type).toBe('image/avif');
  });
});
