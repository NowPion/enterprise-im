package service

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/snowflake"
	"enterprise-im/backend/internal/repository"
)

const (
	defaultFriendRequestListLimit = 20
	maxFriendRequestListLimit     = 100
	maxFriendRequestMessageRunes  = 255
	friendAcceptedSystemContent   = "已添加为好友"
	utcMillisLayout               = "2006-01-02T15:04:05.000Z"
)

// ServiceError pairs a frozen business errcode.Error with an explicit HTTP transport status.
type ServiceError struct {
	HTTPStatus int
	BizErr     *errcode.Error
}

// Error implements the error interface.
func (e *ServiceError) Error() string {
	if e == nil || e.BizErr == nil {
		return ""
	}
	return e.BizErr.Error()
}

func newServiceError(err *errcode.Error) *ServiceError {
	if err == nil {
		err = errcode.ErrInternalConsistency
	}
	return &ServiceError{
		HTTPStatus: err.HTTPStatus,
		BizErr:     err,
	}
}

func newForbiddenError(msg string) *ServiceError {
	return &ServiceError{
		HTTPStatus: http.StatusForbidden,
		BizErr:     errcode.ErrInvalidValue.WithMessage(msg),
	}
}

// FriendRequestDTO matches the frozen FriendRequest contract in docs/api.md.
type FriendRequestDTO struct {
	ID         uint64  `json:"id,string"`
	FromUserID uint64  `json:"from_user_id,string"`
	ToUserID   uint64  `json:"to_user_id,string"`
	Message    string  `json:"message"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	HandledAt  *string `json:"handled_at"`
}

// FriendRequestUserDTO matches the peer User object in docs/api.md.
type FriendRequestUserDTO struct {
	ID           uint64  `json:"id,string"`
	Username     string  `json:"username"`
	Nickname     string  `json:"nickname"`
	AvatarURL    string  `json:"avatar_url"`
	Signature    string  `json:"signature"`
	Online       bool    `json:"online"`
	LastOnlineAt *string `json:"last_online_at"`
}

// FriendRequestListItemDTO represents a FriendRequest item with its peer User.
type FriendRequestListItemDTO struct {
	ID         uint64                `json:"id,string"`
	FromUserID uint64                `json:"from_user_id,string"`
	ToUserID   uint64                `json:"to_user_id,string"`
	Message    string                `json:"message"`
	Status     string                `json:"status"`
	CreatedAt  string                `json:"created_at"`
	HandledAt  *string               `json:"handled_at"`
	Peer       *FriendRequestUserDTO `json:"peer"`
}

// FriendRequestPageDTO is the cursor-paginated list response payload.
type FriendRequestPageDTO struct {
	Items      []FriendRequestListItemDTO `json:"items"`
	NextCursor *string                    `json:"next_cursor"`
	HasMore    bool                       `json:"has_more"`
}

// AcceptFriendRequestResult matches POST /api/v1/friend-requests/{id}/accept data.
type AcceptFriendRequestResult struct {
	Request        FriendRequestDTO `json:"request"`
	FriendID       uint64           `json:"friend_id,string"`
	ConversationID uint64           `json:"conversation_id,string"`
	SystemMsgID    uint64           `json:"system_msg_id,string"`
}

// CreateFriendRequestInput holds validated parameters for creating a friend request.
type CreateFriendRequestInput struct {
	ToUserID uint64
	Message  string
}

// ListFriendRequestsInput holds query filters for listing friend requests.
type ListFriendRequestsInput struct {
	Direction string
	Status    string
	Cursor    string
	Limit     int
}

// FriendRequestService implements the core business rules for friend requests.
type FriendRequestService struct {
	repo           repository.FriendRequestRepository
	relationWriter repository.RelationWriter
	seqAllocator   repository.SeqAllocator
	presence       repository.PresenceChecker
	idGen          *snowflake.Generator
	log            *slog.Logger
}

// NewFriendRequestService constructs a FriendRequestService.
func NewFriendRequestService(
	repo repository.FriendRequestRepository,
	relationWriter repository.RelationWriter,
	seqAllocator repository.SeqAllocator,
	presence repository.PresenceChecker,
	idGen *snowflake.Generator,
	log *slog.Logger,
) *FriendRequestService {
	if relationWriter == nil {
		relationWriter = repository.NewGormRelationWriter()
	}
	if presence == nil {
		presence = repository.NewRedisPresenceChecker(nil)
	}
	if log == nil {
		log = slog.Default()
	}
	return &FriendRequestService{
		repo:           repo,
		relationWriter: relationWriter,
		seqAllocator:   seqAllocator,
		presence:       presence,
		idGen:          idGen,
		log:            log,
	}
}

// Create submits a new pending friend request after ascending user lock and duplicate/friendship checks.
func (s *FriendRequestService) Create(ctx context.Context, fromUserID uint64, input CreateFriendRequestInput) (*FriendRequestDTO, *ServiceError) {
	if s.repo == nil {
		return nil, newServiceError(errcode.ErrInternalConsistency)
	}
	if fromUserID == 0 || input.ToUserID == 0 {
		return nil, newServiceError(errcode.ErrInvalidParams)
	}
	if fromUserID == input.ToUserID {
		return nil, newServiceError(errcode.ErrFriendRequestSelf)
	}
	if !utf8.ValidString(input.Message) || utf8.RuneCountInString(input.Message) > maxFriendRequestMessageRunes {
		return nil, newServiceError(errcode.ErrInvalidValue.WithMessage("message must be at most 255 characters"))
	}

	var created model.FriendRequest
	err := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		users, lockErr := s.repo.LockUsersAscending(ctx, tx, fromUserID, input.ToUserID)
		if lockErr != nil {
			return s.mapDBError(ctx, "create_lock_users", lockErr)
		}

		fromUser, hasFrom := users[fromUserID]
		if !hasFrom {
			return newServiceError(errcode.ErrUserNotFound)
		}
		if fromUser.Status != model.UserStatusActive {
			return newServiceError(errcode.ErrAccountDisabled)
		}

		toUser, hasTo := users[input.ToUserID]
		if !hasTo {
			return newServiceError(errcode.ErrUserNotFound)
		}
		if toUser.Status != model.UserStatusActive {
			return newServiceError(errcode.ErrAccountDisabled)
		}

		alreadyFriends, friendErr := s.repo.HasActiveFriendship(ctx, tx, fromUserID, input.ToUserID)
		if friendErr != nil {
			return s.mapDBError(ctx, "create_check_friendship", friendErr)
		}
		if alreadyFriends {
			return newServiceError(errcode.ErrAlreadyFriends)
		}

		hasPending, pendingErr := s.repo.HasPendingFriendRequest(ctx, tx, fromUserID, input.ToUserID)
		if pendingErr != nil {
			return s.mapDBError(ctx, "create_check_pending", pendingErr)
		}
		if hasPending {
			return newServiceError(errcode.ErrFriendRequestPending)
		}

		now := time.Now().UTC().Truncate(time.Millisecond)
		created = model.FriendRequest{
			FromUserID: fromUserID,
			ToUserID:   input.ToUserID,
			Message:    input.Message,
			Status:     model.FriendRequestStatusPending,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if createErr := s.repo.CreateFriendRequest(ctx, tx, &created); createErr != nil {
			return s.mapDBError(ctx, "create_insert_request", createErr)
		}
		return nil
	})
	if err != nil {
		return nil, s.asServiceError(ctx, "friend_request_create", err)
	}

	dto := toFriendRequestDTO(created)
	return &dto, nil
}

// List returns cursor-paginated received or sent friend requests with batch-loaded peer users.
func (s *FriendRequestService) List(ctx context.Context, userID uint64, input ListFriendRequestsInput) (*FriendRequestPageDTO, *ServiceError) {
	if s.repo == nil {
		return nil, newServiceError(errcode.ErrInternalConsistency)
	}
	if userID == 0 {
		return nil, newServiceError(errcode.ErrAccessTokenInvalid)
	}

	direction := strings.TrimSpace(input.Direction)
	if direction == "" {
		return nil, newServiceError(errcode.ErrInvalidParams.WithMessage("direction is required"))
	}
	if direction != "received" && direction != "sent" {
		return nil, newServiceError(errcode.ErrInvalidValue.WithMessage("direction must be received or sent"))
	}

	status := strings.TrimSpace(input.Status)
	if status != "" && !isValidFriendRequestStatus(status) {
		return nil, newServiceError(errcode.ErrInvalidValue.WithMessage("invalid status filter"))
	}

	limit := input.Limit
	if limit == 0 {
		limit = defaultFriendRequestListLimit
	}
	if limit < 1 || limit > maxFriendRequestListLimit {
		return nil, newServiceError(errcode.ErrInvalidValue.WithMessage("limit must be between 1 and 100"))
	}

	var cursorID uint64
	if strings.TrimSpace(input.Cursor) != "" {
		decoded, decErr := decodeFriendRequestCursor(input.Cursor, userID, direction, status)
		if decErr != nil {
			return nil, newServiceError(errcode.ErrInvalidValue.WithMessage("invalid cursor"))
		}
		cursorID = decoded
	}

	rows, hasMore, err := s.repo.ListFriendRequests(ctx, repository.FriendRequestListFilter{
		UserID:    userID,
		Direction: direction,
		Status:    status,
		CursorID:  cursorID,
		Limit:     limit,
	})
	if err != nil {
		return nil, s.mapDBError(ctx, "list_friend_requests", err)
	}

	peerIDs := make([]uint64, 0, len(rows))
	for _, row := range rows {
		if direction == "received" {
			peerIDs = append(peerIDs, row.FromUserID)
		} else {
			peerIDs = append(peerIDs, row.ToUserID)
		}
	}

	peersMap, err := s.repo.BatchGetUsersByIDs(ctx, peerIDs)
	if err != nil {
		return nil, s.mapDBError(ctx, "list_batch_users", err)
	}

	var onlineMap map[uint64]bool
	if s.presence != nil && len(peerIDs) > 0 {
		var pErr error
		onlineMap, pErr = s.presence.BatchIsOnline(ctx, peerIDs)
		if pErr != nil {
			s.log.WarnContext(ctx, "friend_request_list_presence_unavailable", slog.Any("error", pErr))
			onlineMap = nil
		}
	}

	items := make([]FriendRequestListItemDTO, 0, len(rows))
	for _, row := range rows {
		peerID := row.FromUserID
		if direction == "sent" {
			peerID = row.ToUserID
		}
		var peerDTO *FriendRequestUserDTO
		if u, ok := peersMap[peerID]; ok {
			peerDTO = toFriendRequestUserDTO(u, onlineMap[peerID])
		}
		items = append(items, FriendRequestListItemDTO{
			ID:         row.ID,
			FromUserID: row.FromUserID,
			ToUserID:   row.ToUserID,
			Message:    row.Message,
			Status:     row.Status,
			CreatedAt:  FormatUTCMillis(row.CreatedAt),
			HandledAt:  FormatUTCMillisPtr(row.HandledAt),
			Peer:       peerDTO,
		})
	}

	var nextCursor *string
	if hasMore && len(rows) > 0 {
		encoded, encErr := encodeFriendRequestCursor(userID, direction, status, rows[len(rows)-1].ID)
		if encErr != nil {
			return nil, newServiceError(errcode.ErrInternalConsistency)
		}
		nextCursor = &encoded
	}

	return &FriendRequestPageDTO{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

type retryAcceptWithConvLockError struct {
	convID uint64
}

func (e *retryAcceptWithConvLockError) Error() string {
	return fmt.Sprintf("retry accept with pre-acquired conversation lock %d", e.convID)
}

// Accept accepts a pending friend request inside a single atomic DB transaction,
// creating/restoring bilateral friendships, unique direct_key conversation, members, and a Snowflake system message.
func (s *FriendRequestService) Accept(ctx context.Context, currentUserID, requestID uint64) (*AcceptFriendRequestResult, *ServiceError) {
	if s.repo == nil || s.relationWriter == nil || s.idGen == nil {
		return nil, newServiceError(errcode.ErrInternalConsistency)
	}
	if s.seqAllocator == nil {
		return nil, newServiceError(errcode.ErrRedisUnavailable)
	}
	if currentUserID == 0 || requestID == 0 {
		return nil, newServiceError(errcode.ErrInvalidValue)
	}

	snapshot, err := s.repo.GetFriendRequestByID(ctx, requestID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, newServiceError(errcode.ErrFriendRequestNotFound)
		}
		return nil, s.mapDBError(ctx, "accept_preread_request", err)
	}
	if snapshot.ToUserID != currentUserID {
		return nil, newForbiddenError("only recipient can accept friend request")
	}
	if snapshot.Status != model.FriendRequestStatusPending {
		return nil, newServiceError(errcode.ErrFriendRequestHandled)
	}

	directKey := repository.DirectKey(snapshot.FromUserID, snapshot.ToUserID)
	preConvID, err := s.repo.FindSingleConversationIDByDirectKey(ctx, directKey)
	if err != nil {
		return nil, s.mapDBError(ctx, "accept_lookup_conversation", err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		res, retryConvID, svcErr := s.acceptAttempt(ctx, currentUserID, requestID, snapshot.FromUserID, snapshot.ToUserID, directKey, preConvID)
		if retryConvID > 0 {
			preConvID = retryConvID
			continue
		}
		return res, svcErr
	}
	return nil, newServiceError(errcode.ErrInternalConsistency)
}

func (s *FriendRequestService) acceptAttempt(
	ctx context.Context,
	currentUserID uint64,
	requestID uint64,
	fromUserID uint64,
	toUserID uint64,
	directKey string,
	preConvID uint64,
) (*AcceptFriendRequestResult, uint64, *ServiceError) {
	var (
		lockedConvID uint64
		lockToken    string
		seqRes       *repository.SeqReservation
	)

	// Always acquire distributed conv:lock BEFORE MySQL row locks when conversation already exists,
	// matching the global lock hierarchy (Redis conv:lock -> MySQL rows) to prevent ABBA deadlocks.
	if preConvID > 0 {
		tok, lockErr := s.seqAllocator.AcquireConvLock(ctx, preConvID)
		if lockErr != nil {
			return nil, 0, s.mapSeqError(ctx, "accept_acquire_conv_lock", lockErr)
		}
		lockedConvID = preConvID
		lockToken = tok
	}

	releaseLock := func() {
		if lockedConvID > 0 && lockToken != "" {
			rbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if relErr := s.seqAllocator.ReleaseConvLock(rbCtx, lockedConvID, lockToken); relErr != nil {
				s.log.WarnContext(ctx, "accept_release_conv_lock_failed", slog.Uint64("conversation_id", lockedConvID), slog.Any("error", relErr))
			}
			lockedConvID = 0
			lockToken = ""
		}
	}
	defer releaseLock()

	var result AcceptFriendRequestResult
	txErr := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		users, lockErr := s.repo.LockUsersAscending(ctx, tx, fromUserID, toUserID)
		if lockErr != nil {
			return s.mapDBError(ctx, "accept_lock_users", lockErr)
		}

		toUser, hasTo := users[currentUserID]
		if !hasTo {
			return newServiceError(errcode.ErrUserNotFound)
		}
		if toUser.Status != model.UserStatusActive {
			return newServiceError(errcode.ErrAccountDisabled)
		}

		lockedReq, reqErr := s.repo.LockFriendRequestByID(ctx, tx, requestID)
		if reqErr != nil {
			if errors.Is(reqErr, gorm.ErrRecordNotFound) {
				return newServiceError(errcode.ErrFriendRequestNotFound)
			}
			return s.mapDBError(ctx, "accept_lock_request", reqErr)
		}
		if lockedReq.ToUserID != currentUserID {
			return newForbiddenError("only recipient can accept friend request")
		}
		if lockedReq.Status != model.FriendRequestStatusPending {
			return newServiceError(errcode.ErrFriendRequestHandled)
		}

		fromUser, hasFrom := users[lockedReq.FromUserID]
		if !hasFrom {
			return newServiceError(errcode.ErrUserNotFound)
		}
		if fromUser.Status != model.UserStatusActive {
			return newServiceError(errcode.ErrAccountDisabled)
		}

		alreadyFriends, friendErr := s.repo.HasActiveFriendship(ctx, tx, lockedReq.FromUserID, lockedReq.ToUserID)
		if friendErr != nil {
			return s.mapDBError(ctx, "accept_check_friendship", friendErr)
		}
		if alreadyFriends {
			return newServiceError(errcode.ErrAlreadyFriends)
		}

		now := time.Now().UTC().Truncate(time.Millisecond)
		updated, updErr := s.repo.UpdateFriendRequestStatus(ctx, tx, lockedReq.ID, model.FriendRequestStatusPending, model.FriendRequestStatusAccepted, now)
		if updErr != nil {
			return s.mapDBError(ctx, "accept_update_request", updErr)
		}
		if !updated {
			return newServiceError(errcode.ErrFriendRequestHandled)
		}

		if relErr := s.relationWriter.EnsureBilateralFriendship(ctx, tx, lockedReq.FromUserID, lockedReq.ToUserID); relErr != nil {
			return s.mapDBError(ctx, "accept_write_friendship", relErr)
		}

		conv, convErr := s.repo.LockSingleConversationByDirectKey(ctx, tx, directKey)
		if convErr != nil {
			return s.mapDBError(ctx, "accept_lock_conversation", convErr)
		}
		if conv != nil && preConvID == 0 {
			return &retryAcceptWithConvLockError{convID: conv.ID}
		}
		if conv == nil {
			conv, convErr = s.repo.CreateSingleConversation(ctx, tx, directKey)
			if convErr != nil {
				return s.mapDBError(ctx, "accept_create_conversation", convErr)
			}
			tok, acqErr := s.seqAllocator.AcquireConvLock(ctx, conv.ID)
			if acqErr != nil {
				return s.mapSeqError(ctx, "accept_acquire_new_conv_lock", acqErr)
			}
			lockedConvID = conv.ID
			lockToken = tok
		}

		memberIDs := []uint64{lockedReq.FromUserID, lockedReq.ToUserID}
		if memErr := s.repo.EnsureConversationMembersVisible(ctx, tx, conv.ID, memberIDs, conv.LastSeq, now); memErr != nil {
			return s.mapDBError(ctx, "accept_ensure_members", memErr)
		}

		allocated, seqErr := s.seqAllocator.AllocateNextSeq(ctx, tx, conv.ID, lockToken)
		if seqErr != nil {
			return s.mapSeqError(ctx, "accept_allocate_seq", seqErr)
		}
		seqRes = allocated

		msgID, genErr := s.idGen.NextID()
		if genErr != nil {
			s.log.ErrorContext(ctx, "accept_snowflake_failed", slog.Any("error", genErr))
			return newServiceError(errcode.ErrInternalConsistency)
		}

		extraJSON := model.NullableJSON(fmt.Sprintf(`{"event":"friend.accepted","request_id":"%d"}`, lockedReq.ID))
		sysMsg := &model.Message{
			ID:             msgID,
			ConversationID: conv.ID,
			Seq:            seqRes.Seq,
			SenderID:       0,
			ClientMsgID:    fmt.Sprintf("sys:friend_accept:%d", lockedReq.ID),
			MsgType:        model.MessageTypeSystem,
			Content:        friendAcceptedSystemContent,
			Extra:          extraJSON,
			Status:         model.MessageStatusNormal,
			CreatedAt:      now,
		}
		if msgErr := s.repo.CreateSystemMessage(ctx, tx, sysMsg); msgErr != nil {
			return s.mapDBError(ctx, "accept_create_system_message", msgErr)
		}
		if lastErr := s.repo.UpdateConversationLastMessage(ctx, tx, conv.ID, sysMsg); lastErr != nil {
			return s.mapDBError(ctx, "accept_update_conv_last_msg", lastErr)
		}
		if unreadErr := s.repo.IncrementMembersUnreadCount(ctx, tx, conv.ID, memberIDs, now); unreadErr != nil {
			return s.mapDBError(ctx, "accept_increment_unread", unreadErr)
		}

		receipts := []model.MessageReceipt{
			{
				MessageID:      sysMsg.ID,
				ConversationID: conv.ID,
				Seq:            sysMsg.Seq,
				UserID:         lockedReq.FromUserID,
				Status:         model.ReceiptStatusSent,
				CreatedAt:      now,
				UpdatedAt:      now,
			},
			{
				MessageID:      sysMsg.ID,
				ConversationID: conv.ID,
				Seq:            sysMsg.Seq,
				UserID:         lockedReq.ToUserID,
				Status:         model.ReceiptStatusSent,
				CreatedAt:      now,
				UpdatedAt:      now,
			},
		}
		if recErr := s.repo.CreateMessageReceipts(ctx, tx, receipts); recErr != nil {
			return s.mapDBError(ctx, "accept_create_receipts", recErr)
		}

		lockedReq.Status = model.FriendRequestStatusAccepted
		lockedReq.HandledAt = &now
		result = AcceptFriendRequestResult{
			Request:        toFriendRequestDTO(*lockedReq),
			FriendID:       lockedReq.FromUserID,
			ConversationID: conv.ID,
			SystemMsgID:    sysMsg.ID,
		}
		return nil
	})
	if txErr != nil {
		if seqRes != nil && seqRes.Rollback != nil {
			rbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if rbErr := seqRes.Rollback(rbCtx); rbErr != nil {
				s.log.WarnContext(ctx, "accept_seq_rollback_failed", slog.Uint64("conversation_id", lockedConvID), slog.Any("error", rbErr))
			}
			cancel()
		}
		var retryErr *retryAcceptWithConvLockError
		if errors.As(txErr, &retryErr) {
			return nil, retryErr.convID, nil
		}
		return nil, 0, s.asServiceError(ctx, "friend_request_accept", txErr)
	}

	return &result, 0, nil
}

// Reject rejects a pending friend request (recipient only).
func (s *FriendRequestService) Reject(ctx context.Context, currentUserID, requestID uint64) (*FriendRequestDTO, *ServiceError) {
	return s.handleSimpleTransition(ctx, currentUserID, requestID, true, model.FriendRequestStatusRejected)
}

// Cancel cancels a pending friend request (sender only).
func (s *FriendRequestService) Cancel(ctx context.Context, currentUserID, requestID uint64) (*FriendRequestDTO, *ServiceError) {
	return s.handleSimpleTransition(ctx, currentUserID, requestID, false, model.FriendRequestStatusCanceled)
}

func (s *FriendRequestService) handleSimpleTransition(
	ctx context.Context,
	currentUserID uint64,
	requestID uint64,
	requireRecipient bool,
	toStatus string,
) (*FriendRequestDTO, *ServiceError) {
	if s.repo == nil {
		return nil, newServiceError(errcode.ErrInternalConsistency)
	}
	if currentUserID == 0 || requestID == 0 {
		return nil, newServiceError(errcode.ErrInvalidValue)
	}

	var dto FriendRequestDTO
	err := s.repo.WithTx(ctx, func(tx *gorm.DB) error {
		req, lockErr := s.repo.LockFriendRequestByID(ctx, tx, requestID)
		if lockErr != nil {
			if errors.Is(lockErr, gorm.ErrRecordNotFound) {
				return newServiceError(errcode.ErrFriendRequestNotFound)
			}
			return s.mapDBError(ctx, "transition_lock_request", lockErr)
		}

		if requireRecipient {
			if req.ToUserID != currentUserID {
				return newForbiddenError("only recipient can reject friend request")
			}
		} else {
			if req.FromUserID != currentUserID {
				return newForbiddenError("only sender can cancel friend request")
			}
		}

		if req.Status != model.FriendRequestStatusPending {
			return newServiceError(errcode.ErrFriendRequestHandled)
		}

		now := time.Now().UTC().Truncate(time.Millisecond)
		updated, updErr := s.repo.UpdateFriendRequestStatus(ctx, tx, req.ID, model.FriendRequestStatusPending, toStatus, now)
		if updErr != nil {
			return s.mapDBError(ctx, "transition_update_request", updErr)
		}
		if !updated {
			return newServiceError(errcode.ErrFriendRequestHandled)
		}

		req.Status = toStatus
		req.HandledAt = &now
		dto = toFriendRequestDTO(*req)
		return nil
	})
	if err != nil {
		return nil, s.asServiceError(ctx, "friend_request_"+toStatus, err)
	}
	return &dto, nil
}

func (s *FriendRequestService) mapSeqError(ctx context.Context, op string, err error) *ServiceError {
	if errors.Is(err, repository.ErrConvLockTimeout) {
		s.log.WarnContext(ctx, op, slog.Any("error", err))
		return newServiceError(errcode.ErrInternalConsistency)
	}
	s.log.ErrorContext(ctx, op, slog.Any("error", err))
	return newServiceError(errcode.ErrRedisUnavailable)
}

func (s *FriendRequestService) mapDBError(ctx context.Context, op string, err error) *ServiceError {
	var svcErr *ServiceError
	if errors.As(err, &svcErr) {
		return svcErr
	}
	s.log.ErrorContext(ctx, op, slog.Any("error", err))
	if isUnavailableDBError(err) {
		return newServiceError(errcode.ErrMySQLUnavailable)
	}
	return newServiceError(errcode.ErrInternalConsistency)
}

func (s *FriendRequestService) asServiceError(ctx context.Context, op string, err error) *ServiceError {
	var svcErr *ServiceError
	if errors.As(err, &svcErr) {
		return svcErr
	}
	return s.mapDBError(ctx, op, err)
}

func isUnavailableDBError(err error) bool {
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func isValidFriendRequestStatus(status string) bool {
	switch status {
	case model.FriendRequestStatusPending,
		model.FriendRequestStatusAccepted,
		model.FriendRequestStatusRejected,
		model.FriendRequestStatusCanceled:
		return true
	default:
		return false
	}
}

type friendRequestCursorPayload struct {
	Version   int    `json:"v"`
	UserID    uint64 `json:"u"`
	Direction string `json:"d"`
	Status    string `json:"s"`
	LastID    uint64 `json:"id"`
}

func encodeFriendRequestCursor(userID uint64, direction, status string, lastID uint64) (string, error) {
	raw, err := json.Marshal(friendRequestCursorPayload{
		Version:   1,
		UserID:    userID,
		Direction: direction,
		Status:    status,
		LastID:    lastID,
	})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeFriendRequestCursor(cursor string, userID uint64, direction, status string) (uint64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, err
	}
	var payload friendRequestCursorPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, err
	}
	if payload.Version != 1 || payload.LastID == 0 {
		return 0, errors.New("invalid cursor payload")
	}
	if payload.UserID != userID || payload.Direction != direction || payload.Status != status {
		return 0, errors.New("cursor does not match query scope")
	}
	return payload.LastID, nil
}

func toFriendRequestDTO(fr model.FriendRequest) FriendRequestDTO {
	return FriendRequestDTO{
		ID:         fr.ID,
		FromUserID: fr.FromUserID,
		ToUserID:   fr.ToUserID,
		Message:    fr.Message,
		Status:     fr.Status,
		CreatedAt:  FormatUTCMillis(fr.CreatedAt),
		HandledAt:  FormatUTCMillisPtr(fr.HandledAt),
	}
}

func toFriendRequestUserDTO(u model.User, online bool) *FriendRequestUserDTO {
	return &FriendRequestUserDTO{
		ID:           u.ID,
		Username:     u.Username,
		Nickname:     u.Nickname,
		AvatarURL:    u.AvatarURL,
		Signature:    u.Signature,
		Online:       online,
		LastOnlineAt: FormatUTCMillisPtr(u.LastOnlineAt),
	}
}

// FormatUTCMillis formats a timestamp using UTC RFC 3339 with millisecond precision.
func FormatUTCMillis(t time.Time) string {
	return t.UTC().Format(utcMillisLayout)
}

// FormatUTCMillisPtr formats a nullable timestamp pointer using UTC RFC 3339 with millisecond precision.
func FormatUTCMillisPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := FormatUTCMillis(*t)
	return &s
}
