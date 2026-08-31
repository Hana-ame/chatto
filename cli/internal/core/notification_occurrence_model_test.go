package core

import (
	"context"
	"errors"
	"fmt"
	"hmans.de/chatto/internal/pb/chatto/core/notification/v1"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	evtv1 "hmans.de/chatto/internal/pb/chatto/core/evt/v1"
)

func TestNotificationOccurrenceLifecycleUsesStreamFacts(t *testing.T) {
	chattoCore, _ := newTestCore(t)
	chattoCore.SetNotificationAlertHandler(func(context.Context, *notificationv1.NotificationOccurrence) error {
		return errors.New("hold alert pending for lifecycle assertions")
	})
	startCoreServices(t, chattoCore)
	ctx := testContext(t)
	model := chattoCore.NotificationOccurrences()
	now := time.Now().UTC().Truncate(time.Millisecond)
	input := CreateNotificationOccurrenceInput{
		RecipientID:   "U-notification-recipient",
		SourceEventID: "E-notification-source",
		SourceCreated: now,
		ActorID:       "U-notification-actor",
		Signal: testNotificationSignal(
			notificationTestSignalDirectMention,
			"R-notification-room",
			"E-notification-source",
		),
		Mode:           evtv1.NotificationDeliveryMode_NOTIFICATION_DELIVERY_MODE_PUSH_NOTIFICATION,
		AttentionLevel: notificationv1.NotificationAttentionLevel_NOTIFICATION_ATTENTION_LEVEL_IMPORTANT,
		SkipReadLookup: true,
	}

	created, wasCreated, err := model.Create(ctx, input)
	if err != nil || !wasCreated || created == nil {
		t.Fatalf("Create = (%+v, %v, %v), want new occurrence", created, wasCreated, err)
	}
	wantID := notificationOccurrenceID(input.RecipientID, input.SourceEventID, "direct_mention_received")
	if created.GetId() != wantID || created.GetNotificationStreamSequence() == 0 {
		t.Fatalf("created occurrence = %+v, want deterministic ID and stream sequence", created)
	}
	if created.GetAlertExpiresAt() == nil || !NotificationAlertPending(created) {
		t.Fatalf("created delivery state = %+v, want pending alert", created)
	}
	storedSignal, err := chattoCore.storage.notificationStream.GetMsg(ctx, created.GetNotificationStreamSequence())
	if err != nil {
		t.Fatalf("read stored signal: %v", err)
	}
	var signalEvent notificationv1.NotificationEvent
	if err := proto.Unmarshal(storedSignal.Data, &signalEvent); err != nil {
		t.Fatalf("decode stored signal: %v", err)
	}
	if got := signalEvent.GetSignalled(); signalEvent.GetNotificationId() != wantID || got.GetSourceEventId() != input.SourceEventID || got.GetSignal() == nil {
		t.Fatalf("stored immutable signal = %+v", got)
	}
	duplicate, wasCreated, err := model.Create(ctx, input)
	if err != nil || wasCreated || duplicate.GetId() != wantID {
		t.Fatalf("duplicate Create = (%+v, %v, %v)", duplicate, wasCreated, err)
	}
	read, err := model.MarkRead(ctx, input.RecipientID, wantID)
	if err != nil || !read.GetRead() || read.AlertDelivered == nil || read.GetAlertDelivered() {
		t.Fatalf("MarkRead = (%+v, %v)", read, err)
	}
	deleted, err := model.Delete(ctx, input.RecipientID, wantID)
	if err != nil || !deleted {
		t.Fatalf("Delete = (%v, %v)", deleted, err)
	}
	if _, err := model.Get(ctx, input.RecipientID, wantID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want not found", err)
	}
	if _, err := chattoCore.storage.notificationStream.GetMsg(ctx, created.GetNotificationStreamSequence()); !notificationSignalAlreadyAbsent(err) {
		t.Fatalf("rich signal after delete = %v, want securely deleted", err)
	}
	// Model a restart or another replica, which does not share this process's
	// successful-delete cache. Cleanup must converge when the signal is already
	// absent instead of retrying it for the tombstone's full grace period.
	model.cleanedMu.Lock()
	model.cleaned = make(map[uint64]time.Time)
	model.cleanedMu.Unlock()
	model.cleanupDismissedSignals(ctx, now)
	model.cleanedMu.Lock()
	_, cleanedAfterRestart := model.cleaned[created.GetNotificationStreamSequence()]
	model.cleanedMu.Unlock()
	if !cleanedAfterRestart {
		t.Fatal("already-absent signal was not recorded as successfully cleaned")
	}
	if recreated, wasCreated, err := model.Create(ctx, input); err != nil || wasCreated || recreated != nil {
		t.Fatalf("Create after tombstone = (%+v, %v, %v), want suppressed", recreated, wasCreated, err)
	}
	model.cleanupDismissedSignals(ctx, input.SourceCreated.Add(notificationTTL+notificationPhysicalCleanupGrace+time.Second))
	model.cleanedMu.Lock()
	cleanedCount := len(model.cleaned)
	model.cleanedMu.Unlock()
	if cleanedCount != 0 {
		t.Fatalf("expired secure-delete results = %d, want none", cleanedCount)
	}
}

// 【本地改动 28ba8cddd】2026-08-30 发现背景：cloudcone 生产日志从 2026-08-27 起每分钟
// 重复 "Notification signal physical deletion will retry … code=400
// err_code=10043 sequence N not found"。信号消息已按 TTL 过期被 NATS 移除后，
// cleanupDismissedSignals 的 SecureDeleteMsg 返回该错误，本应被
// notificationSignalAlreadyAbsent 判定为"已不存在"而跳过，但 nats.go 的
// deleteMsg 只把预制 ErrMsgDeleteUnsuccessful 用 %w 放上 error 链，APIError
// （含 err_code）用 %s 拼成文本，errors.As 拿不到 APIError()，导致 10043
// 分支永不命中，删除请求每分钟重试直到 tombstone 过期。修复：在 error 链之外
// 再从 "nats: API error: code=… err_code=N" 稳定文本提取 err_code 判定。
// 本测试用与 nats.go deleteMsg 完全一致的错误构造（%w + %s）复现该场景。
func TestNotificationSignalDeleteAlreadyAbsentRecognizesWrappedCode(t *testing.T) {
	// nats.go stream.deleteMsg 的失败构造：fmt.Errorf("%w: %s",
	// ErrMsgDeleteUnsuccessful, apiErr.Error())。
	wrapped := func(code jetstream.ErrorCode) error {
		apiErr := &jetstream.APIError{Code: 400, ErrorCode: code, Description: "sequence 11 not found"}
		return fmt.Errorf("%w: %s", jetstream.ErrMsgDeleteUnsuccessful, apiErr.Error())
	}
	if !notificationSignalAlreadyAbsent(wrapped(10043)) {
		t.Fatalf("SecureDeleteMsg-style wrapped err_code=10043 must count as already absent")
	}
	if !notificationSignalAlreadyAbsent(wrapped(10057)) {
		t.Fatalf("SecureDeleteMsg-style wrapped err_code=10057 must count as already absent")
	}
	// APIError 真正在 error 链上时（非 SecureDeleteMsg 路径）也应命中。
	if !notificationSignalAlreadyAbsent(fmt.Errorf("outer: %w", &jetstream.APIError{Code: 400, ErrorCode: 10043, Description: "sequence 11 not found"})) {
		t.Fatalf("directly wrapped err_code=10043 must count as already absent")
	}
	// 无关的 broker 错误码必须保持"可重试"，不能误判为已消失。
	if notificationSignalAlreadyAbsent(wrapped(10071)) {
		t.Fatalf("unrelated broker error must stay retryable")
	}
	// 没有 API error 文本的瞬时错误（网络/超时）必须保持"可重试"。
	if notificationSignalAlreadyAbsent(fmt.Errorf("nats: message deletion unsuccessful: nats: connection refused")) {
		t.Fatalf("transient failure without API error text must stay retryable")
	}
}

// 【本地改动 2026-08-31】TestNotificationSignalSequenceGoneFromStreamConfirmsLiveRange
// 保护 signalSequenceGoneFromStream 的 fail-closed 语义。
// 【发现背景】28ba8cddd 修复了「SecureDeleteMsg 报 10043/10057 应视为已不存在」后，
// 线上 warning 归零，但仅凭错误码判定仍有漏洞：嵌入式 NATS 在恢复重放/压缩窗口内，
// 对仍存在的消息也会临时报 not found（索引未加载完）。若此时标 cleaned，该通知信号
// 会被永久漏删（tombstone 不再重试）——「暂时 not found ≠ 永久不存在」。
// 【修复】标 cleaned 前先用 StreamInfo 的 FirstSeq..LastSeq 存活区间做二次确认：
// seq < FirstSeq 才视为已滚出保留范围（永久不存在）；seq 仍在区间内则不能标 cleaned，
// 退回重试。Info 查询失败（网络/超时）时返回 false（fail closed，宁可多等一轮也不漏删）。
// 【测试方法】用真实 embedded NATS：创建一条 occurrence（拿到其在区间内的 seq），
// 断言区间内→false；用 FirstSeq-1 构造必然在区间外的 seq→true；用已取消的 ctx
// 让 Info 失败→false。不依赖删除头消息的 FirstSeq 前移行为，避免 flaky。
func TestNotificationSignalSequenceGoneFromStreamConfirmsLiveRange(t *testing.T) {
	chattoCore, _ := newTestCore(t)
	startCoreServices(t, chattoCore)
	ctx := testContext(t)
	model := chattoCore.NotificationOccurrences()

	now := time.Now().UTC().Truncate(time.Millisecond)
	input := CreateNotificationOccurrenceInput{
		RecipientID:   "U-gone-seq",
		SourceEventID: "E-gone-seq",
		SourceCreated: now,
		ActorID:       "U-actor",
		Signal: testNotificationSignal(
			notificationTestSignalDirectMention,
			"R-gone-seq",
			"E-gone-seq",
		),
		Mode:           evtv1.NotificationDeliveryMode_NOTIFICATION_DELIVERY_MODE_PUSH_NOTIFICATION,
		AttentionLevel: notificationv1.NotificationAttentionLevel_NOTIFICATION_ATTENTION_LEVEL_IMPORTANT,
		SkipReadLookup: true,
	}
	created, _, err := model.Create(ctx, input)
	if err != nil || created == nil {
		t.Fatalf("Create = (%v, %v), want occurrence", created, err)
	}
	inRangeSeq := created.GetNotificationStreamSequence()

	// 区间内（真实刚写入的 seq）→ 不得视为已消失。
	if model.signalSequenceGoneFromStream(ctx, inRangeSeq) {
		t.Fatalf("live range seq %d must not count as gone", inRangeSeq)
	}

	// 构造必然在区间外的 seq（FirstSeq-1）→ 视为已滚出、已消失。
	info, infoErr := model.stream.Info(ctx)
	if infoErr != nil {
		t.Fatalf("StreamInfo: %v", infoErr)
	}
	if info.State.FirstSeq == 0 {
		t.Fatalf("unexpected FirstSeq 0")
	}
	outsideSeq := info.State.FirstSeq - 1
	if !model.signalSequenceGoneFromStream(ctx, outsideSeq) {
		t.Fatalf("seq %d (< FirstSeq %d) must count as gone", outsideSeq, info.State.FirstSeq)
	}

	// fail-closed：Info 查询失败（已取消 ctx）→ 必须返回 false（不标 cleaned）。
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if model.signalSequenceGoneFromStream(cancelled, outsideSeq) {
		t.Fatalf("StreamInfo failure must fail closed (return false)")
	}
}

func TestNotificationIdentitySeparatesSignalKinds(t *testing.T) {
	recipientID, sourceID := "U1", "E1"
	mention := notificationOccurrenceID(recipientID, sourceID, "direct_mention_received")
	reply := notificationOccurrenceID(recipientID, sourceID, "reply_received")
	if mention == reply {
		t.Fatalf("different signal kinds shared ID %q", mention)
	}
}

func TestNotificationCreateManyCommitsFanoutAsOneBatch(t *testing.T) {
	chattoCore, _ := newTestCore(t)
	startCoreServices(t, chattoCore)
	ctx := testContext(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	inputs := make([]CreateNotificationOccurrenceInput, 3)
	for i := range inputs {
		recipientID := fmt.Sprintf("U-batch-%d", i)
		inputs[i] = CreateNotificationOccurrenceInput{
			RecipientID: recipientID, SourceEventID: "E-batch-source", SourceCreated: now, ActorID: "U-actor",
			Signal: testNotificationSignal(notificationTestSignalAll, "R-batch", "E-batch-source"),
			Mode:   evtv1.NotificationDeliveryMode_NOTIFICATION_DELIVERY_MODE_IN_APP_NOTIFICATION, AttentionLevel: notificationv1.NotificationAttentionLevel_NOTIFICATION_ATTENTION_LEVEL_IMPORTANT,
			SkipReadLookup: true,
		}
	}
	if err := chattoCore.NotificationOccurrences().CreateMany(ctx, inputs); err != nil {
		t.Fatalf("CreateMany: %v", err)
	}
	var firstSequence uint64
	for i, input := range inputs {
		id := notificationOccurrenceID(input.RecipientID, input.SourceEventID, "all_mention_received")
		occurrence, err := chattoCore.NotificationOccurrences().Get(ctx, input.RecipientID, id)
		if err != nil {
			t.Fatalf("Get recipient %d: %v", i, err)
		}
		if i == 0 {
			firstSequence = occurrence.GetNotificationStreamSequence()
		}
		if got := occurrence.GetNotificationStreamSequence(); got != firstSequence+uint64(i) {
			t.Fatalf("recipient %d stream sequence = %d, want %d", i, got, firstSequence+uint64(i))
		}
	}
}

func TestNotificationCreateRetryReconcilesExistingOccurrenceWithReadBoundary(t *testing.T) {
	chattoCore, _ := setupTestCore(t)
	ctx := testContext(t)
	poster, err := chattoCore.CreateUser(ctx, SystemActorID, "retry-read-poster", "Retry Read Poster", "password")
	if err != nil {
		t.Fatalf("CreateUser poster: %v", err)
	}
	reader, err := chattoCore.CreateUser(ctx, SystemActorID, "retry-read-reader", "Retry Read Reader", "password")
	if err != nil {
		t.Fatalf("CreateUser reader: %v", err)
	}
	room, err := chattoCore.CreateRoom(ctx, poster.Id, KindChannel, "", "retry-read-room", "")
	if err != nil {
		t.Fatalf("CreateRoom: %v", err)
	}
	for _, userID := range []string{poster.Id, reader.Id} {
		if _, err := chattoCore.JoinRoom(ctx, userID, KindChannel, userID, room.Id); err != nil {
			t.Fatalf("JoinRoom %s: %v", userID, err)
		}
	}
	posted, err := chattoCore.PostMessage(ctx, KindChannel, room.Id, poster.Id, "covered source", nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	entry, ok := chattoCore.roomModel.timelineEntry(posted.GetId())
	if !ok {
		t.Fatal("posted message missing from timeline")
	}
	input := CreateNotificationOccurrenceInput{
		RecipientID: reader.Id, SourceEventID: posted.GetId(), SourceCreated: posted.GetCreatedAt().AsTime(), ActorID: poster.Id,
		Signal: testNotificationSignal(notificationTestSignalDirectMention, room.Id, posted.GetId()),
		Mode:   evtv1.NotificationDeliveryMode_NOTIFICATION_DELIVERY_MODE_IN_APP_NOTIFICATION, AttentionLevel: notificationv1.NotificationAttentionLevel_NOTIFICATION_ATTENTION_LEVEL_IMPORTANT,
		SourceStreamSequence: entry.StreamSeq, SkipReadLookup: true,
	}
	occurrence, created, err := chattoCore.NotificationOccurrences().Create(ctx, input)
	if err != nil || !created || occurrence.GetRead() {
		t.Fatalf("initial Create = (%+v, %v, %v), want new unread occurrence", occurrence, created, err)
	}

	// Model the crash window where read-boundary repair already consumed this
	// scope before the committed signal reached the projection. The materializer
	// retry must reconcile the existing occurrence without another watcher wake.
	scope := notificationReadBoundaryScope{userID: reader.Id, roomID: room.Id}
	key := notificationReadBoundaryKey(reader.Id, room.Id, "")
	index := chattoCore.notificationBoundaries
	index.mu.Lock()
	index.read[key] = notificationReadBoundaryEntry{
		boundary: notificationReadBoundary{targetSequence: entry.StreamSeq, observedSequence: entry.StreamSeq},
		revision: 1,
	}
	delete(index.readDirty, scope)
	index.mu.Unlock()

	input.SkipReadLookup = false
	retried, created, err := chattoCore.NotificationOccurrences().Create(ctx, input)
	if err != nil || created || !retried.GetRead() {
		t.Fatalf("retry Create = (%+v, %v, %v), want existing read occurrence", retried, created, err)
	}
	stored, err := chattoCore.NotificationOccurrences().Get(ctx, reader.Id, occurrence.GetId())
	if err != nil || !stored.GetRead() {
		t.Fatalf("stored retry occurrence = (%+v, %v), want read", stored, err)
	}
	visible, err := chattoCore.NotificationOccurrences().VisibleOccurrences(ctx, reader.Id, []*notificationv1.NotificationOccurrence{stored})
	if err != nil || len(visible) != 1 {
		t.Fatalf("visible occurrence before message.read denial = (%d, %v), want (1, nil)", len(visible), err)
	}
	if err := chattoCore.DenyRoomPermission(ctx, SystemActorID, room.Id, RoleEveryone, PermMessageRead); err != nil {
		t.Fatalf("DenyRoomPermission: %v", err)
	}
	visible, err = chattoCore.NotificationOccurrences().VisibleOccurrences(ctx, reader.Id, []*notificationv1.NotificationOccurrence{stored})
	if err != nil || len(visible) != 0 {
		t.Fatalf("visible occurrence after message.read denial = (%d, %v), want (0, nil)", len(visible), err)
	}
}

func TestConcurrentNotificationRemovalCountsOneCommit(t *testing.T) {
	chattoCore, _ := newTestCore(t)
	startCoreServices(t, chattoCore)
	ctx := testContext(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	occurrence, created, err := chattoCore.NotificationOccurrences().Create(ctx, CreateNotificationOccurrenceInput{
		RecipientID: "U-delete-race", SourceEventID: "E-delete-race", SourceCreated: now, ActorID: "U-actor",
		Signal: testNotificationSignal(notificationTestSignalDirectMention, "R-delete-race", "E-delete-race"),
		Mode:   evtv1.NotificationDeliveryMode_NOTIFICATION_DELIVERY_MODE_IN_APP_NOTIFICATION, AttentionLevel: notificationv1.NotificationAttentionLevel_NOTIFICATION_ATTENTION_LEVEL_IMPORTANT,
		SkipReadLookup: true,
	})
	if err != nil || !created {
		t.Fatalf("Create = (%+v, %v, %v), want new occurrence", occurrence, created, err)
	}

	start := make(chan struct{})
	results := make(chan int, 2)
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			count, deleteErr := chattoCore.NotificationOccurrences().deleteOccurrences(ctx, []*notificationv1.NotificationOccurrence{occurrence})
			results <- count
			errs <- deleteErr
		}()
	}
	ready.Wait()
	close(start)
	deleted := <-results + <-results
	for range 2 {
		if deleteErr := <-errs; deleteErr != nil {
			t.Fatalf("concurrent delete: %v", deleteErr)
		}
	}
	if deleted != 1 {
		t.Fatalf("combined concurrent delete count = %d, want 1", deleted)
	}
}

func TestNotificationProjectionExpiresOccurrencesAndTombstones(t *testing.T) {
	p := NewNotificationProjection()
	now := time.Now().UTC().Truncate(time.Millisecond)
	p.now = func() time.Time { return now }
	occurrence := &notificationv1.NotificationOccurrence{
		Id:              "N1",
		RecipientId:     "U1",
		SourceEventId:   "E1",
		SourceCreatedAt: timestamp(now.Add(-time.Hour)),
		Signal:          testNotificationSignal(notificationTestSignalReply, "R1", "E1"),
		ExpiresAt:       timestamp(now.Add(time.Minute)),
	}
	if err := p.Apply(notificationSignalledEvent("NE1", occurrence, now.Add(time.Minute)), 7); err != nil {
		t.Fatalf("Apply signal: %v", err)
	}
	if got, ok := p.occurrence("U1", "N1", now); !ok || got.GetNotificationStreamSequence() != 7 {
		t.Fatalf("projected occurrence = (%+v, %v)", got, ok)
	}
	scope := notificationReadBoundaryScope{userID: "U1", roomID: "R1"}
	if got := p.scopeOccurrences(scope, now); len(got) != 1 || got[0].GetId() != "N1" {
		t.Fatalf("scope occurrences = %+v, want N1", got)
	}
	if err := p.Apply(&notificationv1.NotificationEvent{
		Id: "NE2", RecipientId: "U1", NotificationId: "N1", OccurredAt: timestamp(now), ExpiresAt: timestamp(now.Add(time.Minute)),
		Event: &notificationv1.NotificationEvent_Removed{Removed: &notificationv1.NotificationRemoved{SignalStreamSequence: 7}},
	}, 8); err != nil {
		t.Fatalf("Apply dismissal: %v", err)
	}
	if _, ok := p.occurrence("U1", "N1", now); ok {
		t.Fatal("dismissed occurrence remained visible")
	}
	ref := notificationOccurrenceRef{recipientID: "U1", notificationID: "N1"}
	states := p.occurrenceStates([]notificationOccurrenceRef{
		ref,
		{recipientID: "U2", notificationID: "N1"},
		{recipientID: "U1", notificationID: "missing"},
	}, now)
	if state := states[ref]; state.occurrence != nil || !state.tombstoned {
		t.Fatalf("dismissed occurrence state = %+v, want tombstone only", state)
	}
	if state := states[notificationOccurrenceRef{recipientID: "U2", notificationID: "N1"}]; state.occurrence != nil || state.tombstoned {
		t.Fatalf("cross-recipient occurrence state = %+v, want empty", state)
	}
	if got := p.scopeOccurrences(scope, now); len(got) != 0 {
		t.Fatalf("dismissed scope occurrences = %+v, want none", got)
	}
	if got := p.pendingPhysicalDeletes(now)["N1"].signalSequence; got != 7 {
		t.Fatalf("pending physical delete sequence = %d, want 7", got)
	}
	now = now.Add(2 * time.Minute)
	if p.occurrenceStates([]notificationOccurrenceRef{ref}, now)[ref].tombstoned {
		t.Fatal("application-expired tombstone still suppressed semantic state")
	}
	if got := p.pendingPhysicalDeletes(now)["N1"].signalSequence; got != 7 {
		t.Fatalf("cleanup-grace tombstone sequence = %d, want 7", got)
	}
	now = now.Add(notificationPhysicalCleanupGrace)
	if got := p.pendingPhysicalDeletes(now); len(got) != 0 {
		t.Fatalf("physically expired tombstones = %+v, want none", got)
	}
}

func TestNotificationProjectionColdReplayRetainsExpiredDismissalCleanupCoordinate(t *testing.T) {
	p := NewNotificationProjection()
	now := time.Now().UTC().Truncate(time.Millisecond)
	p.now = func() time.Time { return now }
	expiresAt := now.Add(-time.Minute)
	occurrence := &notificationv1.NotificationOccurrence{
		Id: "N-expired", RecipientId: "U1", SourceEventId: "E1", SourceCreatedAt: timestamp(now.Add(-notificationTTL)),
		Signal: testNotificationSignal(notificationTestSignalReply, "R1", "E1"), ExpiresAt: timestamp(expiresAt),
	}
	if err := p.Apply(notificationSignalledEvent("signal-expired", occurrence, expiresAt), 7); err != nil {
		t.Fatalf("Apply expired signal: %v", err)
	}
	if _, visible := p.occurrence("U1", occurrence.GetId(), now); visible {
		t.Fatal("application-expired signal became visible during cold replay")
	}
	if err := p.Apply(&notificationv1.NotificationEvent{
		Id: "remove-expired", RecipientId: "U1", NotificationId: occurrence.GetId(), OccurredAt: timestamp(now), ExpiresAt: timestamp(expiresAt),
		Event: &notificationv1.NotificationEvent_Removed{Removed: &notificationv1.NotificationRemoved{SignalStreamSequence: 7}},
	}, 8); err != nil {
		t.Fatalf("Apply expired removal: %v", err)
	}
	if got := p.pendingPhysicalDeletes(now)[occurrence.GetId()].signalSequence; got != 7 {
		t.Fatalf("cold-replayed secure-delete sequence = %d, want 7", got)
	}
}

func TestNotificationProjectionKeepsFirstAlertResolution(t *testing.T) {
	p := NewNotificationProjection()
	now := time.Now().UTC().Truncate(time.Millisecond)
	p.now = func() time.Time { return now }
	occurrence := &notificationv1.NotificationOccurrence{
		Id: "N1", RecipientId: "U1", SourceEventId: "E1", SourceCreatedAt: timestamp(now),
		Signal:    testNotificationSignal(notificationTestSignalDirectMention, "R1", "E1"),
		ExpiresAt: timestamp(now.Add(time.Hour)), AlertExpiresAt: timestamp(now.Add(time.Minute)),
	}
	if err := p.Apply(notificationSignalledEvent("signal", occurrence, now.Add(time.Hour)), 1); err != nil {
		t.Fatal(err)
	}
	resolved := func(id string, delivered bool) *notificationv1.NotificationEvent {
		return &notificationv1.NotificationEvent{
			Id: id, RecipientId: "U1", NotificationId: "N1", OccurredAt: timestamp(now), ExpiresAt: timestamp(now.Add(time.Hour)),
			Event: &notificationv1.NotificationEvent_AlertResolved{AlertResolved: &notificationv1.NotificationAlertResolved{Delivered: delivered}},
		}
	}
	if err := p.Apply(resolved("first", true), 2); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(resolved("late-contradiction", false), 3); err != nil {
		t.Fatal(err)
	}
	got, ok := p.occurrence("U1", "N1", now)
	if !ok || got.AlertDelivered == nil || !got.GetAlertDelivered() {
		t.Fatalf("terminal alert state = %+v, want first Delivered outcome", got)
	}
}

func notificationSignalledEvent(id string, occurrence *notificationv1.NotificationOccurrence, expires time.Time) *notificationv1.NotificationEvent {
	return &notificationv1.NotificationEvent{
		Id: id, RecipientId: occurrence.GetRecipientId(), NotificationId: occurrence.GetId(), OccurredAt: occurrence.GetSourceCreatedAt(), ExpiresAt: timestamp(expires),
		Event: &notificationv1.NotificationEvent_Signalled{Signalled: &notificationv1.NotificationSignalled{
			SourceEventId:        occurrence.GetSourceEventId(),
			SourceCreatedAt:      occurrence.GetSourceCreatedAt(),
			ActorId:              occurrence.GetActorId(),
			Signal:               occurrence.GetSignal(),
			InitiallyRead:        occurrence.GetRead(),
			SourceStreamSequence: occurrence.GetSourceStreamSequence(),
			AttentionLevel:       occurrence.GetAttentionLevel(),
			AlertExpiresAt:       occurrence.GetAlertExpiresAt(),
		}},
	}
}

func TestUnsupportedNotificationSignalDetection(t *testing.T) {
	if !NotificationOccurrenceHasUnsupportedSignal(&notificationv1.NotificationOccurrence{Signal: testUnsupportedNotificationSignal()}) {
		t.Fatal("future signal was not detected")
	}
	if NotificationOccurrenceHasUnsupportedSignal(&notificationv1.NotificationOccurrence{Signal: &notificationv1.NotificationSignal{}}) {
		t.Fatal("empty signal was treated as an unknown future signal")
	}
}

// 【本地改动 2026-08-31】fakeNotificationStream 只覆盖被测试路径调用的 Info
// 方法，其余方法由内嵌的 jetstream.Stream 接口承接（未在测试路径上触达，
// 触达即 panic，测试不需要它们）。
// 注意：签名必须与接口方法完全一致（Info(ctx, opts ...StreamInfoOpt)），
// 否则接口分派会落到内嵌的 nil 接口字段上 panic。
// 用途：T3 模拟 StreamInfo 查询失败，验证 signalSequenceGoneFromStream 的
// fail-closed 行为（查询失败 → 不标 cleaned → 退回重试）。
type fakeNotificationStream struct {
	jetstream.Stream
	info *jetstream.StreamInfo
	err  error
}

func (f *fakeNotificationStream) Info(ctx context.Context, _ ...jetstream.StreamInfoOpt) (*jetstream.StreamInfo, error) {
	return f.info, f.err
}

// T1/T2：纯函数 notificationSignalGoneFromStreamRange 的存活区间语义。
// 发现背景：2026-08-29 notification boundary 竞态事故（seq 19391 被 idle-tail
// 误删导致 CPU 100%）后，加固方向是「标 cleaned 前先确认该 seq 确实不在
// stream 存活范围（FirstSeq..LastSeq）之外再视为不存在」（fail closed，宁可
// 多等一轮也不漏删——防嵌入式 NATS 恢复窗口/压缩窗口内的暂时 not found 被
// 误判永久消失而永久漏删）。2026-08-31 把该判定抽成纯函数以便直接单测区间
// 语义（原实现在 notificationSignalGoneFromStream 方法内，与 stream 耦合）。
func TestNotificationSignalGoneFromStreamRange(t *testing.T) {
	tests := []struct {
		name     string
		firstSeq uint64
		lastSeq  uint64
		sequence uint64
		want     bool
	}{
		// T1：目标 seq 已滚出存活区间（< FirstSeq），retention 已移除 → 可视为不存在。
		{name: "T1 rolled out before FirstSeq", firstSeq: 100, lastSeq: 500, sequence: 99, want: true},
		// T2：seq 仍落在存活区间 [FirstSeq, LastSeq] 内 → 只能算暂时查不到，绝不能标 cleaned。
		{name: "T2 exactly at FirstSeq", firstSeq: 100, lastSeq: 500, sequence: 100, want: false},
		{name: "T2 mid range", firstSeq: 100, lastSeq: 500, sequence: 250, want: false},
		{name: "T2 exactly at LastSeq", firstSeq: 100, lastSeq: 500, sequence: 500, want: false},
		// 边界补充：seq 超出 LastSeq（本 incarnation 从未出现该序列）→ 无法确认，
		// fail-closed 返回 false，避免把别的 incarnation 的 tombstone 误判为已消失。
		{name: "T2 beyond LastSeq is unconfirmed", firstSeq: 100, lastSeq: 500, sequence: 600, want: false},
		// 边界补充：空流（新建/全删，FirstSeq=1 LastSeq=0，无存活消息）时对 seq=1
		// 不确认已消失——保守起见仍退回重试，等 stream 给出可验证的区间。
		{name: "T2 empty stream is unconfirmed", firstSeq: 1, lastSeq: 0, sequence: 1, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &jetstream.StreamInfo{State: jetstream.StreamState{FirstSeq: tt.firstSeq, LastSeq: tt.lastSeq}}
			if got := notificationSignalGoneFromStreamRange(info, tt.sequence); got != tt.want {
				t.Fatalf("notificationSignalGoneFromStreamRange(FirstSeq=%d, LastSeq=%d, seq=%d) = %v, want %v",
					tt.firstSeq, tt.lastSeq, tt.sequence, got, tt.want)
			}
		})
	}
}

// T3：fail-closed —— StreamInfo 查询失败（网络/超时/服务端不可达）时，
// 方法必须返回 false，即「无法证明已消失」，由调用方 cleanupDismissedSignals
// 不标 cleaned、退回下一轮重试。
// 发现背景：同 T1/T2（2026-08-29 boundary 竞态事故后的加固）。若此处返回 true，
// 等于把「查不到」直接当成「已不存在」：通知信号 tombstone 一旦标 cleaned 就
// 不再重试，漏删即永久残留。
func TestNotificationSignalGoneFromStreamFailClosed(t *testing.T) {
	fake := &fakeNotificationStream{err: errors.New("injected stream info failure")}
	model := &NotificationOccurrenceModel{
		stream:  fake,
		logger:  log.NewWithOptions(os.Stderr, log.Options{}),
		cleaned: make(map[uint64]time.Time),
	}
	if got := model.signalSequenceGoneFromStream(context.Background(), 42); got {
		t.Fatal("signalSequenceGoneFromStream returned true on StreamInfo failure; must fail closed")
	}
}
