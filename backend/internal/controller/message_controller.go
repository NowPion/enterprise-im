package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

// MessageController handles HTTP endpoints for message history, incremental sync, send, and recall.
type MessageController struct {
	msgSvc *service.MessageService
}

// NewMessageController constructs a MessageController.
func NewMessageController(msgSvc *service.MessageService) *MessageController {
	return &MessageController{msgSvc: msgSvc}
}

type sendMessageHTTPRequest struct {
	ClientMsgID *string         `json:"client_msg_id"`
	MsgType     *string         `json:"msg_type"`
	Content     *string         `json:"content"`
	Extra       json.RawMessage `json:"extra"`
}

// List handles GET /api/v1/conversations/:conversation_id/messages.
func (h *MessageController) List(c *gin.Context) {
	if h == nil || h.msgSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, authErr := extractChatUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	convID, paramErr := parsePositiveUint64Param(c.Param("conversation_id"), "conversation_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	var beforeSeq, afterSeq *uint64
	if rawBefore, hasBefore := c.GetQuery("before_seq"); hasBefore {
		parsed, seqErr := parseSeqCursorParam(rawBefore, "before_seq")
		if seqErr != nil {
			errcode.Fail(c, seqErr)
			return
		}
		beforeSeq = &parsed
	}
	if rawAfter, hasAfter := c.GetQuery("after_seq"); hasAfter {
		parsed, seqErr := parseSeqCursorParam(rawAfter, "after_seq")
		if seqErr != nil {
			errcode.Fail(c, seqErr)
			return
		}
		afterSeq = &parsed
	}
	if beforeSeq != nil && afterSeq != nil {
		errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("before_seq and after_seq cannot be used together"))
		return
	}

	limit := service.DefaultMessagePageLimit
	if rawLimit, hasLimit := c.GetQuery("limit"); hasLimit {
		parsedLimit, err := strconv.Atoi(strings.TrimSpace(rawLimit))
		if err != nil || parsedLimit < 1 || parsedLimit > service.MaxMessagePageLimit {
			errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("limit must be an integer between 1 and 100"))
			return
		}
		limit = parsedLimit
	}

	res, bizErr := h.msgSvc.ListMessages(c.Request.Context(), userID, convID, beforeSeq, afterSeq, limit)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// Sync handles GET /api/v1/conversations/:conversation_id/sync.
func (h *MessageController) Sync(c *gin.Context) {
	if h == nil || h.msgSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, authErr := extractChatUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	convID, paramErr := parsePositiveUint64Param(c.Param("conversation_id"), "conversation_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	var afterSeq uint64
	if rawAfter, hasAfter := c.GetQuery("after_seq"); hasAfter {
		parsed, seqErr := parseSeqCursorParam(rawAfter, "after_seq")
		if seqErr != nil {
			errcode.Fail(c, seqErr)
			return
		}
		afterSeq = parsed
	}

	limit := service.DefaultMessageSyncLimit
	if rawLimit, hasLimit := c.GetQuery("limit"); hasLimit {
		parsedLimit, err := strconv.Atoi(strings.TrimSpace(rawLimit))
		if err != nil || parsedLimit < 1 || parsedLimit > service.MaxMessagePageLimit {
			errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("limit must be an integer between 1 and 100"))
			return
		}
		limit = parsedLimit
	}

	res, bizErr := h.msgSvc.SyncMessages(c.Request.Context(), userID, convID, afterSeq, limit)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// Send handles POST /api/v1/conversations/:conversation_id/messages.
func (h *MessageController) Send(c *gin.Context) {
	if h == nil || h.msgSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, authErr := extractChatUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	convID, paramErr := parsePositiveUint64Param(c.Param("conversation_id"), "conversation_id")
	if paramErr != nil {
		errcode.Fail(c, paramErr)
		return
	}

	req, bindErr := parseSendMessageBody(c)
	if bindErr != nil {
		errcode.Fail(c, bindErr)
		return
	}

	content := ""
	if req.Content != nil {
		content = *req.Content
	}

	res, bizErr := h.msgSvc.SendMessage(c.Request.Context(), service.SendMessageInput{
		ConversationID: convID,
		SenderID:       userID,
		ClientMsgID:    *req.ClientMsgID,
		MsgType:        *req.MsgType,
		Content:        content,
		Extra:          req.Extra,
	}, nil)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}

	payload := gin.H{"message": res.Message}
	if res.IsNew {
		errcode.Created(c, payload)
		return
	}
	errcode.OK(c, payload)
}

// Recall handles POST /api/v1/conversations/:conversation_id/messages/:msg_id/recall.
func (h *MessageController) Recall(c *gin.Context) {
	if h == nil || h.msgSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, authErr := extractChatUserID(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}
	convID, convErr := parsePositiveUint64Param(c.Param("conversation_id"), "conversation_id")
	if convErr != nil {
		errcode.Fail(c, convErr)
		return
	}
	msgID, msgErr := parsePositiveUint64Param(c.Param("msg_id"), "msg_id")
	if msgErr != nil {
		errcode.Fail(c, msgErr)
		return
	}

	res, bizErr := h.msgSvc.RecallMessage(c.Request.Context(), userID, convID, msgID, nil)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

func parseSendMessageBody(c *gin.Context) (sendMessageHTTPRequest, *errcode.Error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return sendMessageHTTPRequest{}, errcode.ErrInvalidParams
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return sendMessageHTTPRequest{}, errcode.ErrPayloadTooLarge
		}
		return sendMessageHTTPRequest{}, errcode.ErrInvalidParams
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return sendMessageHTTPRequest{}, errcode.ErrInvalidParams.WithMessage("request body must not be empty")
	}

	var req sendMessageHTTPRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return sendMessageHTTPRequest{}, errcode.ErrInvalidParams.WithMessage("invalid JSON request body")
	}
	if req.ClientMsgID == nil || strings.TrimSpace(*req.ClientMsgID) == "" {
		return sendMessageHTTPRequest{}, errcode.ErrInvalidParams.WithMessage("client_msg_id is required")
	}
	if req.MsgType == nil || strings.TrimSpace(*req.MsgType) == "" {
		return sendMessageHTTPRequest{}, errcode.ErrInvalidParams.WithMessage("msg_type is required")
	}
	if *req.MsgType == model.MessageTypeText && req.Content == nil {
		return sendMessageHTTPRequest{}, errcode.ErrInvalidParams.WithMessage("content is required for text message")
	}
	return req, nil
}

func parseSeqCursorParam(raw, name string) (uint64, *errcode.Error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, errcode.ErrInvalidSeqCursor.WithMessage(name + " must be a non-negative integer")
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] < '0' || trimmed[i] > '9' {
			return 0, errcode.ErrInvalidSeqCursor.WithMessage("invalid " + name)
		}
	}
	val, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return 0, errcode.ErrInvalidSeqCursor.WithMessage("invalid " + name)
	}
	return val, nil
}
