package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
)

var (
	validRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	fallbackReqCounter    atomic.Uint64
)

// RequestID ensures every HTTP request receives a safe server-aligned request_id
// stored in Gin context and echoed in the X-Request-ID response header.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		reqID := c.GetHeader(errcode.HeaderRequestID)
		if !validRequestIDPattern.MatchString(reqID) {
			reqID = generateRequestID()
		}
		c.Set(errcode.ContextKeyRequestID, reqID)
		c.Header(errcode.HeaderRequestID, reqID)
		c.Next()
	}
}

// AccessLogger emits structured slog access logs containing request_id, user_id,
// latency_ms, business result code, and redacted auth metadata.
func AccessLogger(log *slog.Logger) gin.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		latencyMs := float64(time.Since(start).Microseconds()) / 1000.0
		status := c.Writer.Status()
		bizCode, hasBizCode := errcode.GetBizCode(c)
		if !hasBizCode {
			if status >= http.StatusInternalServerError {
				bizCode = errcode.ErrInternalConsistency.Code
			} else if status >= http.StatusBadRequest {
				bizCode = errcode.ErrInvalidValue.Code
			}
		}

		attrs := []slog.Attr{
			slog.String("request_id", errcode.GetRequestID(c)),
			slog.String("user_id", errcode.GetUserID(c)),
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.String("query", logger.SanitizeQuery(c.Request.URL.RawQuery)),
			slog.Int("status", status),
			slog.Int("code", bizCode),
			slog.Float64("latency_ms", latencyMs),
			slog.String("client_ip", c.ClientIP()),
			slog.String("user_agent", c.Request.UserAgent()),
		}
		if authHeader := c.GetHeader("Authorization"); authHeader != "" {
			attrs = append(attrs, slog.String("authorization", logger.RedactHeaderValue("Authorization", authHeader)))
		}
		if cookieHeader := c.GetHeader("Cookie"); cookieHeader != "" {
			attrs = append(attrs, slog.String("cookie", logger.RedactHeaderValue("Cookie", cookieHeader)))
		}

		lvl := slog.LevelInfo
		switch {
		case status >= http.StatusInternalServerError:
			lvl = slog.LevelError
		case status >= http.StatusBadRequest:
			lvl = slog.LevelWarn
		}
		log.LogAttrs(c.Request.Context(), lvl, "http_access", attrs...)
	}
}

func generateRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "r-" + hex.EncodeToString(b[:])
	}
	seq := fallbackReqCounter.Add(1)
	return fmt.Sprintf("r-%d-%d", time.Now().UnixMilli(), seq)
}
