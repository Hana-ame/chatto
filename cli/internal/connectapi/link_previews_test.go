package connectapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	evtv1 "hmans.de/chatto/internal/pb/chatto/core/evt/v1"
)

func TestAPILinkPreviewMapsProviderNeutralSocialPost(t *testing.T) {
	publishedAt := timestamppb.New(time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC))
	preview := apiLinkPreview(context.Background(), &API{}, &evtv1.LinkPreview{
		Url:         "https://bsky.app/profile/bsky.app/post/example",
		Title:       "Bluesky (@bsky.app)",
		Description: "A post rendered by Chatto.",
		EmbedType:   "bluesky",
		SocialPost: &evtv1.SocialPostPreview{
			Provider: "bluesky",
			Url:      "https://bsky.app/profile/bsky.app/post/example",
			Author: &evtv1.SocialPostAuthor{
				DisplayName: "Bluesky",
				Handle:      "bsky.app",
			},
			Text:        "A post rendered by Chatto.",
			PublishedAt: publishedAt,
			ExternalLink: &evtv1.SocialPostExternalLink{
				Url:         "https://example.com/story",
				Title:       "Story",
				Description: "Description",
			},
			ContentWarning: stringPtr("Spoilers"),
			QuotedPost: &evtv1.SocialPostPreview{
				Provider: "bluesky",
				Url:      "https://bsky.app/profile/quoted.example/post/quoted",
				Author:   &evtv1.SocialPostAuthor{Handle: "quoted.example"},
				Text:     "Quoted words.",
			},
		},
	})

	require.NotNil(t, preview.GetSocialPost())
	assert.Equal(t, "bluesky", preview.GetSocialPost().GetProvider())
	assert.Equal(t, "A post rendered by Chatto.", preview.GetSocialPost().GetText())
	assert.Equal(t, "Bluesky", preview.GetSocialPost().GetAuthor().GetDisplayName())
	assert.Equal(t, "bsky.app", preview.GetSocialPost().GetAuthor().GetHandle())
	assert.Equal(t, publishedAt, preview.GetSocialPost().GetPublishedAt())
	assert.Equal(t, "https://example.com/story", preview.GetSocialPost().GetExternalLink().GetUrl())
	assert.Equal(t, "Spoilers", preview.GetSocialPost().GetContentWarning())
	assert.Equal(t, "https://bsky.app/profile/bsky.app/post/example", preview.GetSocialPost().GetUrl())
	require.NotNil(t, preview.GetSocialPost().GetQuotedPost())
	assert.Equal(t, "Quoted words.", preview.GetSocialPost().GetQuotedPost().GetText())
	assert.Equal(t, "https://bsky.app/profile/quoted.example/post/quoted", preview.GetSocialPost().GetQuotedPost().GetUrl())
}

// 【本地改动 2026-10-05】恢复上游的 TestAPILinkPreviewImageURLsUseRequestOrigin。
// 上游合并本 fork 的 link_previews.go 时，把 absolutizeMediaURL 包装连同 ctx 参数
// 一起丢掉了：core 的 GetTransformedServerAssetURLWithFilename 只返回相对路径，
// 于是预览图/社交帖头像/引文图全部退化成相对路径。这个测试正是守护该行为的，
// 随签名改动一起被误删——恢复它正是为了不再靠「合并时小心点」来维持正确性。
// fork 的分歧只有尾段形态（{fn.ext}），origin 语义仍与上游一致，故断言原样保留。
func TestAPILinkPreviewImageURLsUseRequestOrigin(t *testing.T) {
	env := newConnectAPITestEnv(t)
	image := &evtv1.AssetRecord{Id: "Apreviewimage01", Storage: &evtv1.AssetRecord_Nats{Nats: &evtv1.NATSAsset{Key: "Apreviewimage01"}}}
	preview := &evtv1.LinkPreview{
		Url:        "https://example.com/story",
		ImageAsset: image,
		SocialPost: &evtv1.SocialPostPreview{
			Author: &evtv1.SocialPostAuthor{Handle: "author.example", AvatarAsset: image},
		},
	}
	ctx := WithRequestBaseURL(context.Background(), "https://alias.example")

	env.api.config.Webserver.URL = "https://chat.example"
	mapped := apiLinkPreview(ctx, env.api, preview)
	assert.True(t, strings.HasPrefix(mapped.GetImageUrl(), "https://alias.example/assets/server/"), "image URL = %q", mapped.GetImageUrl())
	avatarURL := mapped.GetSocialPost().GetAuthor().GetAvatarUrl()
	assert.True(t, strings.HasPrefix(avatarURL, "https://alias.example/assets/server/"), "author avatar URL = %q", avatarURL)

	// Without webserver.url, the direct request origin can have the wrong
	// scheme behind a TLS-terminating proxy, so the paths stay relative.
	env.api.config.Webserver.URL = ""
	mapped = apiLinkPreview(ctx, env.api, preview)
	assert.True(t, strings.HasPrefix(mapped.GetImageUrl(), "/assets/server/"), "image URL = %q", mapped.GetImageUrl())
	avatarURL = mapped.GetSocialPost().GetAuthor().GetAvatarUrl()
	assert.True(t, strings.HasPrefix(avatarURL, "/assets/server/"), "author avatar URL = %q", avatarURL)
}
