package middleware

import (
	"errors"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"strings"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/pkg/errcode"
)

// Recovery catches panics inside HTTP handlers, logs diagnostics internally,
// and returns a unified JSON error envelope without leaking internal details.
func Recovery(log *slog.Logger) gin.HandlerFunc {
	if log == nil {
		log = slog.Default()
	}
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				brokenPipe := isBrokenPipeError(rec)
				log.Error("http_panic_recovered",
					slog.String("request_id", errcode.GetRequestID(c)),
					slog.String("user_id", errcode.GetUserID(c)),
					slog.String("method", c.Request.Method),
					slog.String("path", c.Request.URL.Path),
					slog.Bool("broken_pipe", brokenPipe),
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)
				if brokenPipe {
					c.Abort()
					return
				}
				if !c.Writer.Written() {
					errcode.Fail(c, errcode.ErrInternalConsistency)
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}

func isBrokenPipeError(rec any) bool {
	err, ok := rec.(error)
	if !ok || err == nil {
		return false
	}
	var ne *net.OpError
	if errors.As(err, &ne) {
		var se *os.SyscallError
		if errors.As(ne, &se) {
			msg := strings.ToLower(se.Error())
			return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "connection reset by peer")
		}
	}
	return false
}
