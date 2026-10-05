import { ImageFitMode } from '@chatto/api-types/api/v1/common_pb';

import type { AttachmentAPI } from '../api/attachments.js';

export type ExpiringAssetUrl = {
  url: string;
  /**
   * 【本地改动 2026-10-05】声明为可选，因为 proto3 的零值时间戳不会序列化到
   * 线上。fork 的公开附件 URL（/assets/files/{id}/{fn.ext}）本就不填过期时间，
   * 运行时拿到的就是缺失字段；类型若写死 `expiresAt: string`，调用方会误以为
   * 它一定存在，assetUrlNeedsRefresh 也就没法表达「永不过期」。
   */
  expiresAt?: string;
};

export type RefreshedAttachmentUrls = {
  assetUrl: ExpiringAssetUrl | null;
  thumbnailAssetUrl: ExpiringAssetUrl | null;
  videoThumbnailAssetUrl: ExpiringAssetUrl | null;
  hlsMasterPlaylistUrl?: ExpiringAssetUrl | null;
  variantAssetUrls: Map<string, ExpiringAssetUrl | null>;
};

export type AttachmentThumbnailRefreshOptions = {
  width: number;
  height: number;
  fit: ImageFitMode;
};

export const DEFAULT_ATTACHMENT_THUMBNAIL_REFRESH: AttachmentThumbnailRefreshOptions = {
  width: 960,
  height: 400,
  fit: ImageFitMode.CONTAIN
};

export const LIGHTBOX_ATTACHMENT_IMAGE_REFRESH: AttachmentThumbnailRefreshOptions = {
  width: 2048,
  height: 2048,
  fit: ImageFitMode.CONTAIN
};

export const ASSET_URL_REFRESH_LEAD_MS = 2 * 60_000;

export function assetUrlExpiresAtMs(assetUrl: ExpiringAssetUrl | null | undefined): number | null {
  if (!assetUrl) return null;
  // 【本地改动 2026-10-05】fork 的公开附件 URL 永不过期，服务端不填 expiresAt
  // （proto3 里零值不会序列化到线上，expiresAt 缺失）。缺失 = 永不过期，必须返回
  // null 让 assetUrlRefreshAt 也返回 null → assetUrlNeedsRefresh 恒为 false。
  //
  // 早先这里把 NaN 当成 Date.now()（“视为已过期”）。那是为「服务端发了垃圾值」的
  // 情形兜底，但在 fork 的公开 URL 上成了死循环：每个 URL 都恒定需要刷新，
  // AttachmentViewerModal 的 refresh()（apps/frontend/src/routes/chat/modals/
  // AttachmentViewerModal.svelte:110）在拿到刷新结果后又用 assetUrlNeedsRefresh
  // 复检，于是永远抛 'Attachment URL unavailable'，预览字节一个都发不出去 ——
  // trace 里 /assets/files/ 请求数为 0。症状：e2e/attachment-viewer.test.ts、
  // e2e/html-attachments.test.ts、e2e/video-player.test.ts 全部超时。
  if (assetUrl.expiresAt == null || assetUrl.expiresAt === '') return null;
  const expiresAt = new Date(assetUrl.expiresAt).getTime();
  return Number.isNaN(expiresAt) ? Date.now() : expiresAt;
}

export function assetUrlRefreshAt(
  assetUrl: ExpiringAssetUrl | null | undefined,
  leadMs = ASSET_URL_REFRESH_LEAD_MS
): number | null {
  const expiresAt = assetUrlExpiresAtMs(assetUrl);
  return expiresAt === null ? null : expiresAt - leadMs;
}

export function assetUrlNeedsRefresh(
  assetUrl: ExpiringAssetUrl | null | undefined,
  now = Date.now(),
  leadMs = ASSET_URL_REFRESH_LEAD_MS
): boolean {
  const refreshAt = assetUrlRefreshAt(assetUrl, leadMs);
  return refreshAt !== null && refreshAt <= now;
}

/**
 * Retain usable signed URLs across DTO refreshes when they still identify the
 * same asset. This prevents media elements from reloading on signature-only
 * changes while still accepting explicit refreshes, expiry, and new assets.
 */
export function createAssetUrlRetainer(now: () => number = Date.now) {
  const retained = new Map<string, ExpiringAssetUrl>();

  return (
    key: string,
    next: ExpiringAssetUrl | null,
    forceNext = false
  ): ExpiringAssetUrl | null => {
    if (!next) {
      retained.delete(key);
      return null;
    }

    const current = retained.get(key);
    if (
      !forceNext &&
      current &&
      !assetUrlNeedsRefresh(current, now()) &&
      assetResource(current.url) === assetResource(next.url)
    ) {
      return current;
    }

    retained.set(key, next);
    return next;
  };
}

function assetResource(url: string): string {
  const suffixStart = url.search(/[?#]/);
  return suffixStart === -1 ? url : url.slice(0, suffixStart);
}

export function earliestAssetUrlRefreshAt(
  assetUrls: Iterable<ExpiringAssetUrl | null | undefined>,
  leadMs = ASSET_URL_REFRESH_LEAD_MS
): number | null {
  let nextRefreshAt: number | null = null;
  for (const assetUrl of assetUrls) {
    const refreshAt = assetUrlRefreshAt(assetUrl, leadMs);
    if (refreshAt === null) continue;
    nextRefreshAt = nextRefreshAt === null ? refreshAt : Math.min(nextRefreshAt, refreshAt);
  }
  return nextRefreshAt;
}

export function mergeRefreshedAttachmentUrls(
  current: Map<string, RefreshedAttachmentUrls>,
  fresh: Map<string, RefreshedAttachmentUrls>
): Map<string, RefreshedAttachmentUrls> {
  if (fresh.size === 0) return current;
  return new Map([...current, ...fresh]);
}

export function withAssetUrlRetryParam(url: string, retry: string | number): string {
  if (url.startsWith('data:') || url.startsWith('blob:')) return url;

  const hashStart = url.indexOf('#');
  const base = hashStart === -1 ? url : url.slice(0, hashStart);
  const hash = hashStart === -1 ? '' : url.slice(hashStart);
  const separator = base.includes('?') ? '&' : '?';
  return `${base}${separator}retry=${encodeURIComponent(String(retry))}${hash}`;
}

export async function refreshAttachmentUrlsForAssets(
  api: Pick<AttachmentAPI, 'refreshAssetUrls'>,
  roomId: string,
  assetIds: readonly string[],
  thumbnailOptions = DEFAULT_ATTACHMENT_THUMBNAIL_REFRESH
): Promise<Map<string, RefreshedAttachmentUrls>> {
  try {
    return await api.refreshAssetUrls(roomId, [...assetIds], thumbnailOptions);
  } catch (error) {
    console.warn('Failed to refresh attachment URLs', error);
    return new Map();
  }
}
