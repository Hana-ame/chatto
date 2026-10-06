package connectapi

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"hmans.de/chatto/internal/core"
	apiv1 "hmans.de/chatto/internal/pb/chatto/api/v1"
	evtv1 "hmans.de/chatto/internal/pb/chatto/core/evt/v1"
)

type assetService struct {
	api *API
}

const (
	defaultAttachmentListLimit = 50
	maxAttachmentListLimit     = 100
)

type attachmentThumbnailRequest struct {
	width  int
	height int
	fit    string
}

func (s *roomService) ListRoomAttachments(ctx context.Context, req *connect.Request[apiv1.ListRoomAttachmentsRequest]) (*connect.Response[apiv1.ListRoomAttachmentsResponse], error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	limit, offset := apiPagination(req.Msg.GetPage(), defaultAttachmentListLimit, maxAttachmentListLimit)
	result, err := s.api.core.ListRoomAttachments(ctx, core.ListRoomAttachmentsInput{
		ActorID: caller.UserID,
		RoomID:  req.Msg.RoomId,
		Limit:   limit,
		Offset:  offset,
	})
	if err != nil {
		return nil, connectError(err)
	}

	thumbnail := assetThumbnailOptions(req.Msg.Thumbnail)
	attachments := make([]*apiv1.RoomAttachmentListItem, 0, len(result.Items))
	for _, item := range result.Items {
		if item == nil {
			continue
		}
		attachments = append(attachments, &apiv1.RoomAttachmentListItem{
			Attachment:        apiAsset(ctx, s.api, item.Attachment, caller.UserID, thumbnail),
			MessageEventId:    item.MessageEventID,
			ThreadRootEventId: item.ThreadRootEventID,
			CreatedAt:         item.CreatedAt,
		})
		// 【本地改动 2026-10-05】本块是上游的实现，第 2 组重放时被连同
		// apiAsset 的签名改动一起误删，导致 ListRoomAttachments 丢失
		// Description 字段（TestRoomMessageAndAssetServicesListAttachments…
		// 的 "room attachment description = \"\"" 断言暴露）。与公开 URL
		// 分歧无关，此处按上游原样恢复。
		if item.Description != "" {
			description := item.Description
			attachments[len(attachments)-1].Description = &description
		}
	}

	return connect.NewResponse(&apiv1.ListRoomAttachmentsResponse{
		Attachments: attachments,
		Page:        apiPageInfo(result.TotalCount, result.HasMore),
	}), nil
}

func (s *assetService) GetAsset(ctx context.Context, req *connect.Request[apiv1.GetAssetRequest]) (*connect.Response[apiv1.GetAssetResponse], error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	asset, err := s.api.core.GetRoomAsset(ctx, core.RoomAssetInput{
		ActorID: caller.UserID,
		RoomID:  req.Msg.RoomId,
		AssetID: req.Msg.AssetId,
	})
	if err != nil {
		return nil, connectError(err)
	}
	return connect.NewResponse(&apiv1.GetAssetResponse{
		Asset: apiAsset(ctx, s.api, asset, caller.UserID, assetThumbnailOptions(req.Msg.Thumbnail)),
	}), nil
}

func (s *assetService) BatchGetAssets(ctx context.Context, req *connect.Request[apiv1.BatchGetAssetsRequest]) (*connect.Response[apiv1.BatchGetAssetsResponse], error) {
	caller, err := requireCaller(ctx)
	if err != nil {
		return nil, err
	}
	assets, err := s.api.core.BatchGetRoomAssets(ctx, core.BatchRoomAssetsInput{
		ActorID:  caller.UserID,
		RoomID:   req.Msg.RoomId,
		AssetIDs: req.Msg.GetAssetIds(),
	})
	if err != nil {
		return nil, connectError(err)
	}
	thumbnail := assetThumbnailOptions(req.Msg.Thumbnail)
	out := make([]*apiv1.Asset, 0, len(assets))
	for _, asset := range assets {
		out = append(out, apiAsset(ctx, s.api, asset, caller.UserID, thumbnail))
	}
	return connect.NewResponse(&apiv1.BatchGetAssetsResponse{Assets: out}), nil
}

func apiAsset(ctx context.Context, api *API, attachment *evtv1.Attachment, viewerID string, thumbnail attachmentThumbnailRequest) *apiv1.Asset {
	if attachment == nil {
		return nil
	}
	// 【本地改动 2026-08-18】入口选择公开版 URL 生成（无 ticket、带 {fn.ext}，
	// 长期可缓存）；HLS 仍走 ticket 版（GetStableHLSMasterPlaylistAssetURL）。
	return &apiv1.Asset{
		Id:                attachment.Id,
		Filename:          attachment.Filename,
		ContentType:       attachment.ContentType,
		Size:              attachment.Size,
		Width:             attachment.Width,
		Height:            attachment.Height,
		AssetUrl:          assetURLView(api, ctx, api.core.GetPublicStableAttachmentAssetURL(attachment)),
		ThumbnailAssetUrl: assetURLView(api, ctx, api.core.GetPublicStableTransformedAttachmentAssetURL(attachment, thumbnail.width, thumbnail.height, thumbnail.fit)),
		VideoProcessing:   apiVideoProcessing(ctx, api, viewerID, attachment),
	}
}

func apiVideoProcessing(ctx context.Context, api *API, viewerID string, attachment *evtv1.Attachment) *apiv1.MessageVideoProcessing {
	if attachment == nil || (!strings.HasPrefix(attachment.GetContentType(), "video/") && attachment.GetContentType() != "image/gif") {
		return nil
	}

	state := api.core.GetAssetState(attachment.GetId())
	manifest := state.VideoManifest
	if manifest == nil {
		return nil
	}

	if succeeded := manifest.Succeeded; succeeded != nil {
		video := succeeded.GetVideo()
		if video == nil {
			return nil
		}
		// 【本地改动 2026-10-05 临时探针】把 variants/hls 的真实数量编进缩略图 URL 的
		// query，浏览器和 Playwright 都能看到，从而绕过「服务端日志不进 job log」
		// 的限制。定位完删掉。
		result := &apiv1.MessageVideoProcessing{
			Status:          apiv1.MessageVideoProcessingStatus_MESSAGE_VIDEO_PROCESSING_STATUS_COMPLETED,
			DurationMs:      video.GetDurationMs(),
			Width:           video.GetWidth(),
			Height:          video.GetHeight(),
			SourceAvailable: assetSourceAvailable(api, attachment.GetId(), true),
		}
		if thumbnailID := video.GetThumbnailAssetId(); thumbnailID != "" {
			// 【本地改动 2026-08-18】缩略图只有 ID，先取声明的附件对象再生成
			// 公开 URL（需要 Filename/ContentType 拼 {fn.ext}）。
			if created := api.core.GetAssetState(thumbnailID).Creation; created != nil {
				if thumb := core.AttachmentFromAsset(created.GetAsset()); thumb != nil {
					result.ThumbnailAssetUrl = assetURLView(api, ctx, api.core.GetPublicStableAttachmentAssetURL(thumb))
				}
			}
		}
		for _, variant := range video.GetVariants() {
			if variant == nil {
				continue
			}
			var width, height int32
			var size int64
			// 【本地改动 2026-08-29】整个 variant 循环是本 fork 新增（merge-base
			// 与 upstream 均无此段），最初按当时的 corev1.Attachment 写；合并
			// upstream #2162 后 core/v1 pb 包被删除，Attachment 迁到 evt/v1，
			// core.AttachmentFromAsset 的返回值也随之变成 *evtv1.Attachment。
			//
			// 【本地改动 2026-10-05 修正 URL 取法】原先用
			//   GetPublicStableAttachmentAssetURL(variantAttachment)
			// 而 variantAttachment 来自 GetAssetState(...).Creation。查不到 Creation
			// 时它是 nil → 返回空 StableAssetURL → 前端 AttachmentPreview.svelte 的
			// `.filter((v) => v.url)` 把所有 variant 丢掉 → VideoPlayer.svelte:102
			// 拿不到 variant 也拿不到 hlsUrl，于是回落到 fallbackUrl（原图）。
			// 原图走 /assets/files/{id}/{fn.ext}，该路由是 Accept-Ranges: none 的
			// 顺序流，浏览器无法 seek，element.duration 恒为 0。
			//
			// 症状（CI run 37326134758 / 37319871507，test-e2e-media）：
			//   video-player.test.ts:16  "expect(received).toBeGreaterThan(expected)
			//                                Expected: > 0  Received: 0"（duration）
			// Playwright trace 网络日志佐证：`assets/hls/**` 请求数为 **0**，
			// 只请求了一次 `/assets/files/<id>/test-video.mp4`，响应头 Accept-Ranges: none。
			//
			// 上游的做法（attachments.go 上游版第 169 行）是直接用
			//   GetStableAttachmentAssetURL(variant.GetAssetId(), viewerID)
			// 即用 manifest 里已有的 assetID 构造 URL，不需要回查 Creation。这里照做：
			// 用 Public 版保持第 2 组「无 ticket、任何人可取」的一致性，但**URL 的 id
			// 来源**必须是 variant.GetAssetId() 这个确定值，而不是可能为 nil 的回查结果。
			// Filename 用 %q.mp4：manifest 里的 assetID 形如 "<origin-id>_<quality>.mp4"，
			// stableAttachmentPath 的尾段只影响可读性与缓存键，服务端按 id 解析。
			variantAssetID := variant.GetAssetId()
			variantAttachment := &evtv1.Attachment{
				Id:          variantAssetID,
				Filename:    fmt.Sprintf("%s.mp4", variantAssetID),
				ContentType: "video/mp4",
			}
			if created := api.core.GetAssetState(variantAssetID).Creation; created != nil {
				if asset := created.GetAsset(); asset != nil {
					width = asset.GetWidth()
					height = asset.GetHeight()
					size = asset.GetSize()
				}
			}
			result.Variants = append(result.Variants, &apiv1.MessageVideoVariant{
				Quality:  variant.GetQuality(),
				Width:    width,
				Height:   height,
				Size:     size,
				AssetUrl: assetURLView(api, ctx, api.core.GetPublicStableAttachmentAssetURL(variantAttachment)),
			})
		}
		if hls := video.GetHls(); hls != nil && len(hls.GetRenditions()) > 0 {
			result.Hls = &apiv1.MessageVideoHLS{MasterPlaylistUrl: assetURLView(api, ctx, api.core.GetStableHLSMasterPlaylistAssetURL(attachment.GetId(), viewerID))}
		}
		// 【本地改动 2026-10-05 临时探针】counts 编进 Quality。
		// **必须同时给一个非空 AssetUrl**：客户端 MessageAttachments.svelte:136
		// `if (!variantAssetUrl) return []` 会把没有 URL 的 variant 整个丢掉，
		// 上一版只填 Quality，结果探针自己被过滤掉，props 仍显示 []，等于没探到。
		// 这里复用原图 URL，只是为了让它活过 flatMap，URL 本身不参与判断。
		if len(result.Variants) == 0 {
			result.Variants = append(result.Variants, &apiv1.MessageVideoVariant{
				Quality: fmt.Sprintf("PROBE v=%d h=%d",
					len(video.GetVariants()), len(video.GetHls().GetRenditions())),
				AssetUrl: assetURLView(api, ctx, api.core.GetPublicStableAttachmentAssetURL(attachment)),
			})
		}
		return result
	}

	if failed := manifest.Failed; failed != nil {
		reasonCode := assetProcessingFailureReasonCode(failed.GetFailureCode())
		return &apiv1.MessageVideoProcessing{
			Status:          apiv1.MessageVideoProcessingStatus_MESSAGE_VIDEO_PROCESSING_STATUS_FAILED,
			SourceAvailable: reasonCode != "original_missing" && assetSourceAvailable(api, attachment.GetId(), true),
			ReasonCode:      reasonCode,
		}
	}

	if manifest.Started != nil {
		return &apiv1.MessageVideoProcessing{
			Status:          apiv1.MessageVideoProcessingStatus_MESSAGE_VIDEO_PROCESSING_STATUS_PROCESSING,
			SourceAvailable: assetSourceAvailable(api, attachment.GetId(), true),
		}
	}

	return nil
}

func assetSourceAvailable(api *API, assetID string, fallback bool) bool {
	created := api.core.GetAssetState(assetID).Creation
	if created == nil {
		return fallback
	}
	return created.GetOriginalBinaryAvailable()
}

func assetProcessingFailureReasonCode(code evtv1.AssetProcessingFailureCode) string {
	switch code {
	case evtv1.AssetProcessingFailureCode_ASSET_PROCESSING_FAILURE_CODE_SOURCE_MISSING:
		return "original_missing"
	case evtv1.AssetProcessingFailureCode_ASSET_PROCESSING_FAILURE_CODE_PROCESSING_FAILED:
		return "processing_failed"
	default:
		return "processing_failed"
	}
}

func assetThumbnailOptions(options *apiv1.ImageTransformOptions) attachmentThumbnailRequest {
	width, height := 120, 120
	fit := "cover"
	if options != nil {
		if options.GetWidth() > 0 {
			width = int(options.GetWidth())
		}
		if options.GetHeight() > 0 {
			height = int(options.GetHeight())
		}
		switch options.GetFit() {
		case apiv1.ImageFitMode_IMAGE_FIT_MODE_CONTAIN:
			fit = "contain"
		case apiv1.ImageFitMode_IMAGE_FIT_MODE_COVER:
			fit = "cover"
		}
	}
	return attachmentThumbnailRequest{width: width, height: height, fit: fit}
}
