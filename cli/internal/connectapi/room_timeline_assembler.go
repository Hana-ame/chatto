package connectapi

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/charmbracelet/log"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"hmans.de/chatto/internal/core"
	"hmans.de/chatto/internal/parallel"
	apiv1 "hmans.de/chatto/internal/pb/chatto/api/v1"
	evtv1 "hmans.de/chatto/internal/pb/chatto/core/evt/v1"
)

type roomTimelineAssembler struct {
	api       *API
	thumbnail attachmentThumbnailRequest
}

func newRoomTimelineAssembler(api *API) *roomTimelineAssembler {
	return newRoomTimelineAssemblerWithThumbnail(api, defaultTimelineAttachmentThumbnail())
}

func defaultTimelineAttachmentThumbnail() attachmentThumbnailRequest {
	return attachmentThumbnailRequest{
		width:  960,
		height: 400,
		fit:    "contain",
	}
}

func newRoomTimelineAssemblerWithThumbnail(api *API, thumbnail attachmentThumbnailRequest) *roomTimelineAssembler {
	return &roomTimelineAssembler{api: api, thumbnail: thumbnail}
}

// buildPage turns projected room timeline entries into the public Connect view.
// The projected event log intentionally stores facts, not UI rows: message
// bodies, reactions, thread metadata, and users live in sibling projections.
// Hydrating them here keeps the public API free of per-field resolver N+1s
// and gives future clients one renderable page per request.
func (a *roomTimelineAssembler) buildPage(ctx context.Context, viewerID string, kind core.RoomKind, events []*core.RoomEvent, hasOlder, hasNewer bool) (*apiv1.RoomTimelinePage, error) {
	apiEvents, h, err := a.hydrateEvents(ctx, viewerID, kind, events)
	if err != nil {
		return nil, err
	}

	users, err := h.users()
	if err != nil {
		return nil, err
	}

	return &apiv1.RoomTimelinePage{
		Events:   apiEvents,
		HasOlder: hasOlder,
		HasNewer: hasNewer,
		Includes: &apiv1.RoomTimelineIncludes{Users: users},
	}, nil
}

func (a *roomTimelineAssembler) hydrateEvents(ctx context.Context, viewerID string, kind core.RoomKind, events []*core.RoomEvent) ([]*apiv1.RoomTimelineEvent, *timelineHydrator, error) {
	ctx = core.WithDEKRequestCache(ctx)

	messageIDs := make([]string, 0, len(events))
	threadMetadata := make(map[string]*core.ThreadMetadata)
	for _, event := range events {
		posted := event.GetMessagePosted()
		if posted != nil {
			messageIDs = append(messageIDs, event.Id)
		}
		if posted == nil || posted.GetInThread() != "" {
			continue
		}
		metadata, err := a.api.core.GetThreadMetadata(ctx, kind, posted.GetRoomId(), event.Id)
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			return nil, nil, err
		}
		key := timelineThreadKey(posted.GetRoomId(), event.Id)
		threadMetadata[key] = metadata
	}

	reactionsByMessageID, err := a.api.core.GetReactionsBatch(ctx, messageIDs)
	if err != nil {
		return nil, nil, err
	}
	h := &timelineHydrator{
		api:                  a.api,
		ctx:                  ctx,
		viewerID:             viewerID,
		kind:                 kind,
		reactionsByMessageID: reactionsByMessageID,
		userIDs:              make(map[string]struct{}),
		thumbnail:            a.thumbnail,
		threadMetadata:       threadMetadata,
	}

	apiEvents, err := parallel.MapNonNil(ctx, maxConnectAPIHydrationConcurrency, events, func(ctx context.Context, _ int, event *core.RoomEvent) (*apiv1.RoomTimelineEvent, error) {
		return h.event(ctx, event)
	})
	if err != nil {
		return nil, nil, err
	}

	return apiEvents, h, nil
}

func (a *roomTimelineAssembler) buildThreadPage(ctx context.Context, viewerID, roomID, threadRootEventID string, kind core.RoomKind, root *core.RoomEvent, replies *core.RoomEventsResult, includeRoot bool) (*apiv1.RoomTimelinePage, error) {
	events := make([]*core.RoomEvent, 0, 1+len(replies.Events))
	if includeRoot {
		events = append(events, root)
	}
	events = append(events, replies.Events...)

	page, err := a.buildPage(ctx, viewerID, kind, events, replies.HasOlder, replies.HasNewer)
	if err != nil {
		return nil, err
	}
	page.StartCursor, err = a.api.formatRoomTimelineCursor(viewerID, roomID, threadRootEventID, replies.StartCursorSeq)
	if err != nil {
		return nil, err
	}
	page.EndCursor, err = a.api.formatRoomTimelineCursor(viewerID, roomID, threadRootEventID, replies.EndCursorSeq)
	if err != nil {
		return nil, err
	}
	return page, nil
}

type timelineHydrator struct {
	api                  *API
	ctx                  context.Context
	viewerID             string
	kind                 core.RoomKind
	reactionsByMessageID map[string][]core.ReactionSummary
	userMu               sync.Mutex
	userIDs              map[string]struct{}
	thumbnail            attachmentThumbnailRequest
	threadMetadata       map[string]*core.ThreadMetadata
	// bodyLoads shares one canonical body hydration per response. The response
	// owns this cache; projections never retain its plaintext.
	bodyMu    sync.Mutex
	bodyLoads map[string]func() (*core.DecryptedMessageBody, error)
}

func (h *timelineHydrator) messageBody(ctx context.Context, roomID, eventID string) (*core.DecryptedMessageBody, error) {
	id, err := h.api.core.ResolveMessageContentID(roomID, eventID)
	if errors.Is(err, core.ErrMessageNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.bodyMu.Lock()
	if h.bodyLoads == nil {
		h.bodyLoads = make(map[string]func() (*core.DecryptedMessageBody, error))
	}
	load := h.bodyLoads[id]
	if load == nil {
		load = sync.OnceValues(func() (*core.DecryptedMessageBody, error) { return h.api.core.GetFullMessageBody(ctx, id) })
		h.bodyLoads[id] = load
	}
	h.bodyMu.Unlock()
	return load()
}

func timelineThreadKey(roomID, threadRootEventID string) string {
	return roomID + "\x00" + threadRootEventID
}

func (h *timelineHydrator) event(ctx context.Context, event *core.RoomEvent) (*apiv1.RoomTimelineEvent, error) {
	if event == nil || event.Event == nil {
		return nil, nil
	}
	authorID := core.MessageAuthorID(event.Event)
	h.addUserID(authorID)

	apiEvent := &apiv1.RoomTimelineEvent{
		Id:        event.Id,
		CreatedAt: event.CreatedAt,
		ActorId:   authorID,
	}

	switch payload := event.Event.GetEvent().(type) {
	case *evtv1.Event_MessagePosted:
		message, err := h.messagePosted(ctx, event, payload.MessagePosted)
		if err != nil {
			return nil, err
		}
		apiEvent.Event = &apiv1.RoomTimelineEvent_MessagePosted{
			MessagePosted: &apiv1.RoomMessagePosted{Message: message},
		}
	case *evtv1.Event_RoomCreated:
		apiEvent.Event = &apiv1.RoomTimelineEvent_RoomCreated{RoomCreated: roomEvent(payload.RoomCreated.GetRoomId())}
	case *evtv1.Event_RoomUpdated:
		apiEvent.Event = &apiv1.RoomTimelineEvent_RoomUpdated{RoomUpdated: roomEvent(payload.RoomUpdated.GetRoomId())}
	case *evtv1.Event_RoomDeleted:
		apiEvent.Event = &apiv1.RoomTimelineEvent_RoomDeleted{RoomDeleted: roomEvent(payload.RoomDeleted.GetRoomId())}
	case *evtv1.Event_RoomArchived:
		apiEvent.Event = &apiv1.RoomTimelineEvent_RoomArchived{RoomArchived: roomEvent(payload.RoomArchived.GetRoomId())}
	case *evtv1.Event_RoomUnarchived:
		apiEvent.Event = &apiv1.RoomTimelineEvent_RoomUnarchived{RoomUnarchived: roomEvent(payload.RoomUnarchived.GetRoomId())}
	case *evtv1.Event_RoomThreadingModeChanged:
		change := payload.RoomThreadingModeChanged
		apiEvent.Event = &apiv1.RoomTimelineEvent_RoomThreadingModeChanged{
			RoomThreadingModeChanged: &apiv1.RoomTimelineThreadingModeChangedEvent{
				RoomId:        change.GetRoomId(),
				ThreadingMode: apiRoomThreadingModeChangeValue(change.GetThreadingMode()),
			},
		}
	case *evtv1.Event_UserJoinedRoom:
		apiEvent.Event = &apiv1.RoomTimelineEvent_UserJoinedRoom{UserJoinedRoom: roomEvent(payload.UserJoinedRoom.GetRoomId())}
	case *evtv1.Event_UserLeftRoom:
		apiEvent.Event = &apiv1.RoomTimelineEvent_UserLeftRoom{UserLeftRoom: roomEvent(payload.UserLeftRoom.GetRoomId())}
	case *evtv1.Event_VoiceCallStarted:
		apiEvent.Event = &apiv1.RoomTimelineEvent_CallStarted{CallStarted: callEvent(payload.VoiceCallStarted.GetRoomId(), payload.VoiceCallStarted.GetCallId())}
	case *evtv1.Event_VoiceCallEnded:
		apiEvent.Event = &apiv1.RoomTimelineEvent_CallEnded{CallEnded: callEvent(payload.VoiceCallEnded.GetRoomId(), payload.VoiceCallEnded.GetCallId())}
	default:
		return nil, fmt.Errorf("unsupported room timeline event %T", payload)
	}

	return apiEvent, nil
}

func (h *timelineHydrator) messagePosted(ctx context.Context, event *core.RoomEvent, payload *evtv1.MessagePostedEvent) (*apiv1.Message, error) {
	if !event.EchoMetadataHydrated {
		var err error
		payload, err = h.api.core.HydrateMessagePost(ctx, event.Event)
		if err != nil {
			return nil, err
		}
	}
	hydrationState, err := h.api.core.RoomTimelineReads().MessageHydrationState(event.Id)
	if err != nil {
		return nil, err
	}

	message := &apiv1.Message{
		Id:                        event.Id,
		RoomId:                    payload.GetRoomId(),
		CreatedAt:                 event.CreatedAt,
		ActorId:                   core.MessageAuthorID(event.Event),
		InReplyTo:                 payload.GetInReplyTo(),
		ThreadRootEventId:         payload.GetInThread(),
		EchoOfEventId:             payload.GetEchoOfEventId(),
		EchoFromThreadRootEventId: payload.GetEchoFromThreadRootEventId(),
		Reactions:                 h.reactions(event.Id),
		Pinned:                    hydrationState.Pinned,
	}
	if hydrationState.HasDeletedAt {
		message.DeletedAt = timestamppb.New(hydrationState.DeletedAt)
	}
	message.ChannelEchoEventId = hydrationState.ChannelEchoEventID
	if rootID, ok := h.api.core.MessageEventThreadRoot(payload.GetRoomId(), event.Event); ok {
		canReply, err := h.api.core.CanReplyInThread(ctx, h.viewerID, h.kind, payload.GetRoomId(), rootID)
		if err != nil {
			return nil, err
		}
		message.ViewerState = &apiv1.MessageViewerState{CanReplyInThread: &canReply}
	}

	var body *core.DecryptedMessageBody
	if !hydrationState.HasDeletedAt {
		body, err = h.messageBody(ctx, payload.GetRoomId(), event.Id)
	}
	if err != nil {
		if !errors.Is(err, core.ErrMessageBodyCorrupt) {
			return nil, err
		}
		// A single corrupt body envelope must not make the whole room history
		// unreadable. Keep the message envelope renderable and let clients show
		// the existing unavailable-message state.
		log.Warn("Failed to hydrate room timeline message body",
			"room_id", payload.GetRoomId(),
			"message_event_id", event.Id,
			"error", err)
		body = nil
	}
	if body != nil {
		message.Body = &body.Body
		message.Attachments = h.attachments(payload.GetRoomId(), body.MessageEventID, body.Attachments, body.AttachmentDescriptions)
		message.LinkPreview = h.linkPreview(body.LinkPreview)
		if body.UpdatedAt != nil {
			message.UpdatedAt = timestamppb.New(*body.UpdatedAt)
		}
	}

	if payload.GetInThread() == "" {
		key := timelineThreadKey(payload.GetRoomId(), event.Id)
		metadata, metadataKnown := h.threadMetadata[key]
		if !metadataKnown {
			var err error
			metadata, err = h.api.core.GetThreadMetadata(ctx, h.kind, payload.GetRoomId(), event.Id)
			if err != nil && !errors.Is(err, core.ErrNotFound) {
				return nil, err
			}
		}
		if metadata != nil && metadata.Exists {
			thread := &apiv1.ThreadSummary{
				ThreadRootEventId: event.Id,
			}
			thread.ReplyCount = int32(metadata.ReplyCount)
			if metadata.LastReplyAt != nil {
				thread.LastReplyAt = timestamppb.New(*metadata.LastReplyAt)
			}
			thread.ParticipantPreviewUserIds = firstN(metadata.ParticipantIDs, 5)
			thread.ParticipantCount = int32(metadata.ParticipantCount)
			h.addUserIDs(thread.ParticipantPreviewUserIds)
			following, err := h.api.core.IsFollowingThread(ctx, h.kind, h.viewerID, payload.GetRoomId(), event.Id)
			if err != nil {
				return nil, err
			}
			hasUnreadReplies := false
			if following && metadata.LastReplyAt != nil {
				lastOpened, err := h.api.core.GetThreadLastOpened(ctx, h.kind, h.viewerID, payload.GetRoomId(), event.Id)
				if err != nil {
					return nil, err
				}
				hasUnreadReplies = lastOpened.IsZero() || metadata.LastReplyAt.After(lastOpened)
			}
			thread.ViewerState = apiThreadViewerState(following, hasUnreadReplies)
			message.Thread = thread
		}
	}

	return message, nil
}

func (h *timelineHydrator) attachments(roomID, messageEventID string, attachments []*evtv1.Attachment, descriptions map[string]string) []*apiv1.MessageAttachment {
	result := make([]*apiv1.MessageAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment == nil {
			continue
		}
		// A response can share a canonical body across original and echo rows.
		attachment = proto.Clone(attachment).(*evtv1.Attachment)
		if attachment.RoomId == "" {
			attachment.RoomId = roomID
		}
		if attachment.MessageBodyId == "" {
			attachment.MessageBodyId = messageEventID
		}
		// 【本地改动 2026-08-18】入口选择公开版 URL 生成：无 ticket、带
		// {fn.ext} 尾段，浏览器/CDN 可长期缓存。
		assetURL := h.api.core.GetPublicStableAttachmentAssetURL(attachment)
		// 【本地改动 2026-09-12】fork 取消附件衍生图：时间线缩略图 URL 直接
		// override 成原图链接，不再有 /image/{w}x{h}/{fit} 段。
		//
		// 【本地改动 2026-10-05】维度用零值而非透传请求参数：fork 的公开 URL
		// override 忽略 width/height/fit，直接返回原图链接，传什么都得到同一
		// 个 URL。保留零值是为了让「这一步不产衍生图」在代码里自明。
		thumbnailURL := h.api.core.GetPublicStableTransformedAttachmentAssetURL(attachment, 0, 0, "")
		view := &apiv1.MessageAttachment{
			Id:                attachment.Id,
			Filename:          attachment.Filename,
			ContentType:       attachment.ContentType,
			Width:             attachment.Width,
			Height:            attachment.Height,
			AssetUrl:          assetURLView(h.api, h.ctx, assetURL),
			ThumbnailAssetUrl: assetURLView(h.api, h.ctx, thumbnailURL),
			VideoProcessing:   apiVideoProcessing(h.ctx, h.api, h.viewerID, attachment),
		}
		if description := descriptions[attachment.GetId()]; description != "" {
			view.Description = &description
		}
		result = append(result, view)
	}
	return result
}

func (h *timelineHydrator) linkPreview(preview *evtv1.LinkPreview) *apiv1.LinkPreview {
	// 【本地改动 2026-10-05】恢复传 h.ctx。apiLinkPreview 需要 ctx 才能
	// absolutizeMediaURL 补全 origin；早先把 ctx 从签名里去掉（连同
	// apiVideoProcessing）纯属误伤——那些签名本来就是上游的。
	return apiLinkPreview(h.ctx, h.api, preview)
}

func (h *timelineHydrator) reactions(messageEventID string) []*apiv1.MessageReaction {
	summaries := h.reactionsByMessageID[messageEventID]
	result := make([]*apiv1.MessageReaction, 0, len(summaries))
	for _, summary := range summaries {
		previewUserIDs := firstN(summary.UserIDs, 5)
		h.addUserIDs(previewUserIDs)
		result = append(result, &apiv1.MessageReaction{
			Emoji:          summary.Emoji,
			Count:          int32(len(summary.UserIDs)),
			HasReacted:     containsString(summary.UserIDs, h.viewerID),
			PreviewUserIds: previewUserIDs,
		})
	}
	return result
}

func (h *timelineHydrator) users() (map[string]*apiv1.User, error) {
	h.userMu.Lock()
	ids := make([]string, 0, len(h.userIDs))
	for id := range h.userIDs {
		ids = append(ids, id)
	}
	h.userMu.Unlock()

	coreUsers, err := h.api.core.GetUserReferences(h.ctx, ids)
	if err != nil {
		return nil, err
	}
	presences, err := h.api.core.GetUserPresences(h.ctx, ids)
	if err != nil {
		return nil, err
	}

	result := make(map[string]*apiv1.User, len(ids))
	avatarWidth, avatarHeight := 96, 96
	for i, id := range ids {
		user := coreUsers[i]
		if user == nil {
			// An absent profile can be a projection catch-up gap. Only the user
			// projection can identify an account-deletion tombstone.
			continue
		}
		summary, err := userSummaryWithPresence(h.ctx, h.api, user, &apiv1.ImageTransformOptions{
			Width:  int32(avatarWidth),
			Height: int32(avatarHeight),
			Fit:    apiv1.ImageFitMode_IMAGE_FIT_MODE_COVER,
		}, presences[id])
		if err != nil {
			return nil, err
		}
		result[id] = summary
	}
	return result, nil
}

func (h *timelineHydrator) addUserID(userID string) {
	if userID == "" {
		return
	}
	h.userMu.Lock()
	h.userIDs[userID] = struct{}{}
	h.userMu.Unlock()
}

func (h *timelineHydrator) addUserIDs(userIDs []string) {
	h.userMu.Lock()
	defer h.userMu.Unlock()
	for _, userID := range userIDs {
		if userID != "" {
			h.userIDs[userID] = struct{}{}
		}
	}
}

func roomEvent(roomID string) *apiv1.RoomTimelineRoomEvent {
	return &apiv1.RoomTimelineRoomEvent{RoomId: roomID}
}

func callEvent(roomID, callID string) *apiv1.RoomTimelineCallEvent {
	return &apiv1.RoomTimelineCallEvent{RoomId: roomID, CallId: callID}
}

// assetURLView maps a core asset URL to its API view.
//
// 【本地改动 2026-10-05 修正】恢复按请求 origin 绝对化（上游 #2693）。此前把它
// 改成包级函数并丢掉 ctx，理由是「core 侧已用 AssetBaseURL 生成完整 URL，无需按
// 请求补 origin」——那个前提现在不成立了：
//
//  1. GetPublicStableAttachmentAssetURL 已改回返回**相对**路径（见
//     core/attachments.go 的联邦说明）。core 只有本机 origin，而附件可能属于另一台
//     server；在 core 里烤 origin 会把跨源请求指到错误的 host。
//  2. HLS 入口 GetStableHLSMasterPlaylistAssetURL 一直是相对路径（含 ticket），
//     原来靠这里补 origin。现在不补了，hls.js 拿到的就是页面 host 下的相对
//     路径——浏览器其实能解析，但 fork 的 assetUrlForServer 只处理
//     /assets/files/ 前缀，/assets/hls/ 不在其中，视频在共享 viewer 里 readyState
//     停在 0。
//
// 绝对化必须用**请求** origin 而不是 webserver.url：a.absolutizeServerURL 优先取
// requestBaseURLFromContext，所以联邦客户端会拿到自己可达的 host。
func assetURLView(a *API, ctx context.Context, assetURL core.StableAssetURL) *apiv1.MessageAssetUrl {
	// 【本地改动 2026-08-18】公开 URL 无 ticket、永不过期：ExpiresAt 零值时
	// 不填充过期时间，前端据此跳过 URL 刷新（避免序列化成 1970 触发无限刷新）。
	if assetURL.ExpiresAt.IsZero() {
		return &apiv1.MessageAssetUrl{Url: a.absolutizeMediaURL(ctx, assetURL.URL)}
	}
	return &apiv1.MessageAssetUrl{
		Url:       a.absolutizeMediaURL(ctx, assetURL.URL),
		ExpiresAt: timestamppb.New(assetURL.ExpiresAt),
	}
}
