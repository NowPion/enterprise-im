package errcode

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const (
	HeaderRequestID = "X-Request-ID"

	ContextKeyRequestID = "request_id"
	ContextKeyUserID    = "user_id"
	ContextKeyBizCode   = "biz_code"
)

// Envelope is the unified HTTP JSON response structure defined by docs/api.md.
type Envelope struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data"`
	RequestID string `json:"request_id"`
}

// GetRequestID retrieves the request_id stored in gin.Context.
func GetRequestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(ContextKeyRequestID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return c.Writer.Header().Get(HeaderRequestID)
}

// SetUserID stores the authenticated user identifier in gin.Context for access logging.
func SetUserID(c *gin.Context, userID string) {
	if c != nil {
		c.Set(ContextKeyUserID, userID)
	}
}

// GetUserID retrieves the user identifier stored in gin.Context, or empty string if unauthenticated.
func GetUserID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if v, ok := c.Get(ContextKeyUserID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// GetBizCode returns the business code recorded on the current request context, if any.
func GetBizCode(c *gin.Context) (int, bool) {
	if c == nil {
		return 0, false
	}
	v, ok := c.Get(ContextKeyBizCode)
	if !ok {
		return 0, false
	}
	code, ok := v.(int)
	return code, ok
}

// OK writes an HTTP 200 success envelope with data.
func OK(c *gin.Context, data any) {
	WriteJSON(c, http.StatusOK, OKCode.Code, OKCode.Message, data)
}

// Created writes an HTTP 201 success envelope for newly created resources.
func Created(c *gin.Context, data any) {
	WriteJSON(c, http.StatusCreated, OKCode.Code, OKCode.Message, data)
}

// Fail writes an error envelope using the Error definition's HTTP status and nil data.
func Fail(c *gin.Context, err *Error) {
	FailWithData(c, err, nil)
}

// FailWithData writes an error envelope with structured diagnostic data (e.g. /healthz 503).
func FailWithData(c *gin.Context, err *Error, data any) {
	if err == nil {
		err = ErrInternalConsistency
	}
	status := err.HTTPStatus
	if status < 100 || status > 599 {
		status = http.StatusInternalServerError
	}
	WriteJSON(c, status, err.Code, err.Message, data)
}

// FailWithStatus writes an error envelope with an explicit HTTP transport status.
func FailWithStatus(c *gin.Context, httpStatus int, err *Error, data any) {
	if err == nil {
		err = ErrInternalConsistency
	}
	WriteJSON(c, httpStatus, err.Code, err.Message, data)
}

// WriteJSON writes the unified response envelope and guarantees X-Request-ID alignment.
func WriteJSON(c *gin.Context, httpStatus int, code int, message string, data any) {
	reqID := GetRequestID(c)
	if reqID != "" {
		c.Header(HeaderRequestID, reqID)
	}
	c.Set(ContextKeyBizCode, code)
	c.JSON(httpStatus, Envelope{
		Code:      code,
		Message:   message,
		Data:      data,
		RequestID: reqID,
	})
}
