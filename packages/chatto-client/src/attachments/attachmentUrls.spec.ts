// @vitest-environment node
import { describe, expect, it, vi } from 'vitest';
import {
  ASSET_URL_REFRESH_LEAD_MS,
  DEFAULT_ATTACHMENT_THUMBNAIL_REFRESH,
  assetUrlExpiresAtMs,
  assetUrlNeedsRefresh,
  assetUrlRefreshAt,
  createAssetUrlRetainer,
  earliestAssetUrlRefreshAt,
  mergeRefreshedAttachmentUrls,
  refreshAttachmentUrlsForAssets,
  withAssetUrlRetryParam,
  type RefreshedAttachmentUrls
} from './attachmentUrls.js';

const at = (iso: string) => new Date(iso).getTime();
const url = (path: string, expiresAt = '2026-09-29T12:00:00Z') => ({
  url: `https://cdn.example${path}`,
  expiresAt
});

describe('asset URL expiry', () => {
  it('reads the expiry and refreshes the lead time before it', () => {
    const asset = url('/a?sig=1');
    expect(assetUrlExpiresAtMs(asset)).toBe(at('2026-09-29T12:00:00Z'));
    expect(assetUrlRefreshAt(asset)).toBe(at('2026-09-29T12:00:00Z') - ASSET_URL_REFRESH_LEAD_MS);
    expect(assetUrlRefreshAt(asset, 0)).toBe(at('2026-09-29T12:00:00Z'));
    expect(assetUrlExpiresAtMs(null)).toBeNull();
    expect(assetUrlRefreshAt(undefined)).toBeNull();
  });

  // 【本地改动 2026-10-05】fork 的公开附件 URL 永不过期：服务端不填 expiresAt，
  // proto3 零值不序列化 → 客户端拿到的是缺失字段。缺失必须读作「永不过期」。
  //
  // 真实症状（CI 42064270a / cf377d23c / a1dad3b62）：缺失曾被当成 Date.now()，
  // 于是每个 URL 恒定 needsRefresh；AttachmentViewerModal.svelte:110 在刷新后又
  // 复检该条件，永远抛 'Attachment URL unavailable'，预览字节一个都发不出去
  // —— Playwright trace 里 /assets/files/ 请求数为 0。附件预览、HTML 附件、
  // 视频播放器三组 e2e 全部 15–30s 超时。
  it('treats a missing expiry as never expiring', () => {
    const missing = { url: 'https://cdn.example/assets/files/A1/x.pdf' } as {
      url: string;
      expiresAt?: string;
    };
    expect(assetUrlExpiresAtMs(missing)).toBeNull();
    expect(assetUrlRefreshAt(missing)).toBeNull();
    // 关键断言：不能恒为 true，否则刷新循环永不退出。
    expect(assetUrlNeedsRefresh(missing)).toBe(false);
    expect(assetUrlNeedsRefresh({ url: missing.url, expiresAt: '' })).toBe(false);
  });

  it('treats an unreadable expiry as expired now', () => {
    const now = Date.now();
    expect(assetUrlExpiresAtMs({ url: 'x', expiresAt: 'not a date' })).toBeGreaterThanOrEqual(now);
    expect(assetUrlNeedsRefresh({ url: 'x', expiresAt: 'not a date' })).toBe(true);
  });

  it('needs a refresh from the refresh time on, and never without a URL', () => {
    const asset = url('/a');
    const refreshAt = assetUrlRefreshAt(asset)!;
    expect(assetUrlNeedsRefresh(asset, refreshAt - 1)).toBe(false);
    expect(assetUrlNeedsRefresh(asset, refreshAt)).toBe(true);
    expect(assetUrlNeedsRefresh(null, refreshAt)).toBe(false);
  });

  it('finds the earliest refresh and skips missing URLs', () => {
    expect(earliestAssetUrlRefreshAt([])).toBeNull();
    expect(earliestAssetUrlRefreshAt([null, undefined])).toBeNull();
    expect(
      earliestAssetUrlRefreshAt(
        [url('/late', '2026-09-29T13:00:00Z'), null, url('/early', '2026-09-29T12:30:00Z')],
        0
      )
    ).toBe(at('2026-09-29T12:30:00Z'));
  });
});

describe('createAssetUrlRetainer', () => {
  it('keeps a usable URL across a new signature of the same asset', () => {
    const now = at('2026-09-29T11:00:00Z');
    const retain = createAssetUrlRetainer(() => now);
    const first = url('/asset.png?sig=1#frag');
    expect(retain('k', first)).toBe(first);
    expect(retain('k', url('/asset.png?sig=2'))).toBe(first);
  });

  it('accepts a forced, expired, or different URL, and forgets a removed one', () => {
    let now = at('2026-09-29T11:00:00Z');
    const retain = createAssetUrlRetainer(() => now);
    const first = url('/asset.png?sig=1');
    retain('k', first);

    const forced = url('/asset.png?sig=2');
    expect(retain('k', forced, true)).toBe(forced);

    const other = url('/other.png?sig=1');
    expect(retain('k', other)).toBe(other);

    now = at('2026-09-29T12:00:00Z');
    const renewed = url('/other.png?sig=2', '2026-09-29T14:00:00Z');
    expect(retain('k', renewed)).toBe(renewed);

    expect(retain('k', null)).toBeNull();
    const again = url('/other.png?sig=3', '2026-09-29T14:00:00Z');
    expect(retain('k', again)).toBe(again);
  });
});

describe('attachment URL maps', () => {
  const entry = (path: string): RefreshedAttachmentUrls => ({
    assetUrl: url(path),
    thumbnailAssetUrl: null,
    videoThumbnailAssetUrl: null,
    variantAssetUrls: new Map()
  });

  it('merges fresh URLs over current ones and keeps the map without news', () => {
    const current = new Map([
      ['a', entry('/a1')],
      ['b', entry('/b1')]
    ]);
    expect(mergeRefreshedAttachmentUrls(current, new Map())).toBe(current);
    const merged = mergeRefreshedAttachmentUrls(current, new Map([['a', entry('/a2')]]));
    expect(merged.get('a')?.assetUrl?.url).toBe('https://cdn.example/a2');
    expect(merged.get('b')?.assetUrl?.url).toBe('https://cdn.example/b1');
    expect(current.get('a')?.assetUrl?.url).toBe('https://cdn.example/a1');
  });

  it('adds a retry parameter before the fragment, but not to local URLs', () => {
    expect(withAssetUrlRetryParam('https://cdn.example/a', 1)).toBe(
      'https://cdn.example/a?retry=1'
    );
    expect(withAssetUrlRetryParam('https://cdn.example/a?sig=x#t=5', 'two words')).toBe(
      'https://cdn.example/a?sig=x&retry=two%20words#t=5'
    );
    expect(withAssetUrlRetryParam('data:image/png;base64,AA', 1)).toBe('data:image/png;base64,AA');
    expect(withAssetUrlRetryParam('blob:https://app/1', 1)).toBe('blob:https://app/1');
  });

  it('refreshes URLs with the default thumbnail size and returns none on failure', async () => {
    const fresh = new Map([['a', entry('/a')]]);
    const api = { refreshAssetUrls: vi.fn(async () => fresh) };
    await expect(refreshAttachmentUrlsForAssets(api, 'R1', ['a'])).resolves.toBe(fresh);
    expect(api.refreshAssetUrls).toHaveBeenCalledWith(
      'R1',
      ['a'],
      DEFAULT_ATTACHMENT_THUMBNAIL_REFRESH
    );

    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    api.refreshAssetUrls.mockRejectedValueOnce(new Error('offline'));
    await expect(refreshAttachmentUrlsForAssets(api, 'R1', ['a'])).resolves.toEqual(new Map());
    expect(warn).toHaveBeenCalled();
    warn.mockRestore();
  });
});
