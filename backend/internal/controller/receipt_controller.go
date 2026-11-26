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

	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

// ReceiptController handles HTTP endpoints for message receipt queries and delivered/read watermarks.
type ReceiptController struct {
	receiptSvc *service.ReceiptService
}

// NewReceiptController constructs a ReceiptController.
func NewReceiptController(receiptSvc *service.ReceiptService) *ReceiptController {
	return &ReceiptController{receiptSvc: receiptSvc}
}

// GetMessageReceipts handles GET /api/v1/conversations/:conversation_id/messages/:msg_id/receipts.
func (h *ReceiptController) GetMessageReceipts(c *gin.Context) {
	if h == nil || h.receiptSvc == nil {
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

	statusFilter := strings.TrimSpace(c.Query("status"))
	switch statusFilter {
	case "", repository.ReceiptFilterRead, repository.ReceiptFilterUnread, repository.ReceiptFilterDelivered, repository.ReceiptFilterPending:
	default:
		errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("status must be read, unread, delivered, or pending"))
		return
	}

	limit := service.DefaultReceiptPageLimit
	if rawLimit, hasLimit := c.GetQuery("limit"); hasLimit {
		parsedLimit, err := strconv.Atoi(strings.TrimSpace(rawLimit))
		if err != nil || parsedLimit < 1 || parsedLimit > service.MaxReceiptPageLimit {
			errcode.Fail(c, errcode.ErrInvalidValue.WithMessage("limit must be an integer between 1 and 100"))
			return
		}
		limit = parsedLimit
	}

	cursor := strings.TrimSpace(c.Query("cursor"))
	res, bizErr := h.receiptSvc.ListMessageReceipts(c.Request.Context(), userID, convID, msgID, statusFilter, cursor, limit)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// MarkRead handles POST /api/v1/conversations/:conversation_id/read.
func (h *ReceiptController) MarkRead(c *gin.Context) {
	if h == nil || h.receiptSvc == nil {
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

	seq, bodyErr := parseWatermarkSeqBody(c, "last_read_seq")
	if bodyErr != nil {
		errcode.Fail(c, bodyErr)
		return
	}

	res, bizErr := h.receiptSvc.MarkRead(c.Request.Context(), userID, convID, seq)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// MarkDelivered handles POST /api/v1/conversations/:conversation_id/delivered.
func (h *ReceiptController) MarkDelivered(c *gin.Context) {
	if h == nil || h.receiptSvc == nil {
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

	seq, bodyErr := parseWatermarkSeqBody(c, "last_delivered_seq")
	if bodyErr != nil {
		errcode.Fail(c, bodyErr)
		return
	}

	res, bizErr := h.receiptSvc.MarkDelivered(c.Request.Context(), userID, convID, seq)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

func parseWatermarkSeqBody(c *gin.Context, fieldName string) (uint64, *errcode.Error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return 0, errcode.ErrInvalidParams
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return 0, errcode.ErrPayloadTooLarge
		}
		return 0, errcode.ErrInvalidParams
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0, errcode.ErrInvalidParams.WithMessage("request body must not be empty")
	}
	if trimmed[0] != '{' {
		return 0, errcode.ErrInvalidParams.WithMessage("request body must be a JSON object")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return 0, errcode.ErrInvalidParams.WithMessage("invalid JSON request body")
	}
	rawVal, exists := fields[fieldName]
	if !exists || bytes.Equal(bytes.TrimSpace(rawVal), []byte("null")) {
		return 0, errcode.ErrInvalidParams.WithMessage(fieldName + " is required")
	}
	var seqStr string
	if err := json.Unmarshal(rawVal, &seqStr); err != nil {
		return 0, errcode.ErrInvalidParams.WithMessage(fieldName + " must be a decimal string")
	}
	return parseSeqCursorParam(seqStr, fieldName)
}
