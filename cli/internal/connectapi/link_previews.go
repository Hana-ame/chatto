package connectapi

import (
	"context"

	"connectrpc.com/connect"
	"hmans.de/chatto/internal/core"
	apiv1 "hmans.de/chatto/internal/pb/chatto/api/v1"
	evtv1 "hmans.de/chatto/internal/pb/chatto/core/evt/v1"
)

func (s *messageService) FetchLinkPreview(ctx context.Context, req *connect.Request[apiv1.FetchLinkPreviewRequest]) (*connect.Response[apiv1.FetchLinkPreviewResponse], error) {
	if _, err := requireCaller(ctx); err != nil {
		return nil, err
	}

	preview, err := s.api.core.GetLinkPreview(ctx, req.Msg.Url)
	if err != nil {
		return nil, connectError(err)
	}
	if preview == nil {
		return connect.NewResponse(&apiv1.FetchLinkPreviewResponse{}), nil
	}
	tokenURL := preview.GetUrl()
	if tokenURL == "" {
		tokenURL = req.Msg.Url
	}
	token, err := s.api.core.CreateLinkPreviewToken(ctx, tokenURL)
	if err != nil {
		return nil, connectError(err)
	}

	return connect.NewResponse(&apiv1.FetchLinkPreviewResponse{
		Preview:      apiLinkPreview(ctx, s.api, preview),
		PreviewToken: token,
	}), nil
}

func apiLinkPreview(ctx context.Context, api *API, preview *evtv1.LinkPreview) *apiv1.LinkPreview {
	if preview == nil {
		return nil
	}

	imageAssetID := preview.GetImageAssetId()
	imageAssetKey := imageAssetID
	if image := preview.GetImageAsset(); image != nil && image.GetId() != "" {
		imageAssetID = image.GetId()
		imageAssetKey = core.ServerAssetDeliveryKey(image)
	}

	imageURL := ""
	if imageAssetKey != "" {
		// 【本地改动 2026-08-23】URL 追加 {fn.ext} 尾段；image 记录缺失时
		// 推导不出扩展名，保持无尾段旧形态。
		imageURL = api.core.GetTransformedServerAssetURLWithFilename(
			imageAssetKey, core.ServerAssetURLFilename(preview.GetImageAsset(), "preview"), 600, 314, "contain")
		// 【本地改动 2026-10-05】恢复 absolutize。
		// GetTransformedServerAssetURLWithFilename 只返回相对路径（见 core 的
		// 注释「由调用方 absolutize 成绝对 URL」），fork 丢掉了这层包装，导致
		// 链接预览图/社交帖头像/引文图全部退化成相对路径。后果：把预览发给别人、
		// 或在 https 页面外消费 API 时链接不可用；混合内容风险与上游注释所述相同。
		// 与 room 附件的公开 URL 分歧不同——这里只恢复「补全 origin」，尾段仍是 fork 的形态。
		imageURL = api.absolutizeMediaURL(ctx, imageURL)
	}

	out := &apiv1.LinkPreview{
		Url: preview.GetUrl(),
	}
	if title := preview.GetTitle(); title != "" {
		out.Title = stringPtr(title)
	}
	if description := preview.GetDescription(); description != "" {
		out.Description = stringPtr(description)
	}
	if imageURL != "" {
		out.ImageUrl = stringPtr(imageURL)
	}
	if imageAssetID != "" {
		out.ImageAssetId = stringPtr(imageAssetID)
	}
	if siteName := preview.GetSiteName(); siteName != "" {
		out.SiteName = stringPtr(siteName)
	}
	if embedType := preview.GetEmbedType(); embedType != "" {
		out.EmbedType = stringPtr(embedType)
	}
	if embedID := preview.GetEmbedId(); embedID != "" {
		out.EmbedId = stringPtr(embedID)
	}
	if socialPost := preview.GetSocialPost(); socialPost != nil {
		out.SocialPost = apiSocialPostPreview(ctx, api, socialPost, 0)
	}
	return out
}

func apiSocialPostPreview(ctx context.Context, api *API, socialPost *evtv1.SocialPostPreview, quoteDepth int) *apiv1.SocialPostPreview {
	if socialPost == nil {
		return nil
	}
	mapped := &apiv1.SocialPostPreview{
		Provider:       socialPost.GetProvider(),
		Text:           socialPost.GetText(),
		PublishedAt:    socialPost.GetPublishedAt(),
		ContentWarning: optionalString(socialPost.GetContentWarning()),
		Url:            socialPost.GetUrl(),
	}
	if author := socialPost.GetAuthor(); author != nil {
		mapped.Author = &apiv1.SocialPostAuthor{
			DisplayName: author.GetDisplayName(),
			Handle:      author.GetHandle(),
		}
		mapped.Author.AvatarUrl, mapped.Author.AvatarAssetId = linkPreviewAsset(ctx, api, author.GetAvatarAsset(), 96, 96, "cover")
	}
	if external := socialPost.GetExternalLink(); external != nil {
		mapped.ExternalLink = &apiv1.SocialPostExternalLink{
			Url:         external.GetUrl(),
			Title:       optionalString(external.GetTitle()),
			Description: optionalString(external.GetDescription()),
		}
		mapped.ExternalLink.ImageUrl, mapped.ExternalLink.ImageAssetId = linkPreviewAsset(ctx, api, external.GetImageAsset(), 600, 314, "contain")
	}
	for _, image := range socialPost.GetImages() {
		imageURL, assetID := linkPreviewAsset(ctx, api, image.GetAsset(), 600, 600, "contain")
		if imageURL == nil || assetID == nil {
			continue
		}
		mapped.Images = append(mapped.Images, &apiv1.SocialPostImage{
			Url: *imageURL, AssetId: *assetID, Alt: optionalString(image.GetAlt()),
			Width: optionalUint32(image.GetWidth()), Height: optionalUint32(image.GetHeight()),
		})
	}
	if quoteDepth == 0 {
		mapped.QuotedPost = apiSocialPostPreview(ctx, api, socialPost.GetQuotedPost(), quoteDepth+1)
	}
	return mapped
}

func linkPreviewAsset(ctx context.Context, api *API, asset *evtv1.AssetRecord, width, height int, fit string) (*string, *string) {
	if asset == nil || asset.GetId() == "" {
		return nil, nil
	}
	assetID := asset.GetId()
	// 【本地改动 2026-08-23】URL 追加 {fn.ext} 尾段（公开 immutable 缓存）。
	url := api.core.GetTransformedServerAssetURLWithFilename(
		core.ServerAssetDeliveryKey(asset), core.ServerAssetURLFilename(asset, "preview"), width, height, fit)
	// 【本地改动 2026-10-05】同上：core 只给相对路径，这里补全 origin。
	url = api.absolutizeMediaURL(ctx, url)
	if url == "" {
		return nil, &assetID
	}
	return &url, &assetID
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalUint32(value uint32) *uint32 {
	if value == 0 {
		return nil
	}
	return &value
}
