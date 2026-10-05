// 【本地改动 2026-10-05】expiresAt 改为可选。
//
// proto3 的零值时间戳不会序列化到线上，客户端收到的是**缺失**字段。fork 的公开
// 附件 URL（/assets/files/{id}/{fn.ext}，无 ticket、永不过期）正是这种情况，类型
// 写死 `expiresAt: string` 会让调用方误以为它一定存在，也无法表达「永不过期」——
// 而 assetUrlNeedsRefresh 正是靠 expiresAt 缺失与否区分这两种语义。
//
// 本文件是这两个类型的唯一来源；src/attachments/attachmentUrls.ts 从这里导入，
// 避免两处重复定义分叉（改一处不改另一处会让 tsc 报 TS2345
// "incompatible between these types"，见 test-chattobot 的 @chatto/client:check）。
export type ExpiringAssetUrl = {
  url: string;
  expiresAt?: string;
};

export type RefreshedAttachmentUrls = {
  assetUrl: ExpiringAssetUrl | null;
  thumbnailAssetUrl: ExpiringAssetUrl | null;
  videoThumbnailAssetUrl: ExpiringAssetUrl | null;
  hlsMasterPlaylistUrl?: ExpiringAssetUrl | null;
  variantAssetUrls: Map<string, ExpiringAssetUrl | null>;
};
