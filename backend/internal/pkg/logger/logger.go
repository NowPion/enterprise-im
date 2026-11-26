package logger

import (
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"enterprise-im/backend/internal/config"
)

const RedactedValue = "[REDACTED]"

var sensitiveKeys = map[string]struct{}{
	"password":         {},
	"password_hash":    {},
	"secret":           {},
	"token":            {},
	"access_token":     {},
	"refresh_token":    {},
	"authorization":    {},
	"cookie":           {},
	"set-cookie":       {},
	"x-auth-token":     {},
	"x-api-key":        {},
	"dsn":              {},
	"mysql_password":   {},
	"redis_password":   {},
}

var sensitiveQueryParams = map[string]struct{}{
	"access_token":  {},
	"refresh_token": {},
	"token":         {},
	"password":      {},
	"secret":        {},
}

// New builds a configured *slog.Logger writing to out (os.Stdout when nil).
// Sensitive attribute keys are automatically redacted.
func New(cfg config.LogConfig, out io.Writer) (*slog.Logger, error) {
	if out == nil {
		out = os.Stdout
	}
	lvl, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if _, ok := sensitiveKeys[strings.ToLower(a.Key)]; ok {
				return slog.String(a.Key, RedactedValue)
			}
			return a
		},
	}

	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "", "json":
		handler = slog.NewJSONHandler(out, opts)
	case "text":
		handler = slog.NewTextHandler(out, opts)
	default:
		return nil, fmt.Errorf("unsupported log format %q", cfg.Format)
	}

	return slog.New(handler), nil
}

// InitGlobal builds a logger from cfg and registers it as slog's default logger.
func InitGlobal(cfg config.LogConfig) (*slog.Logger, error) {
	l, err := New(cfg, os.Stdout)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(l)
	return l, nil
}

// RedactHeaderValue masks sensitive authentication/session header values.
func RedactHeaderValue(headerName, val string) string {
	if val == "" {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(headerName)) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-auth-token", "x-api-key":
		parts := strings.SplitN(strings.TrimSpace(val), " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return "Bearer " + RedactedValue
		}
		return RedactedValue
	default:
		return val
	}
}

// SanitizeQuery redacts known credential query parameters before logging.
func SanitizeQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return RedactedValue
	}
	for k := range values {
		if _, sensitive := sensitiveQueryParams[strings.ToLower(k)]; sensitive {
			values.Set(k, RedactedValue)
		}
	}
	return values.Encode()
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unsupported log level %q", raw)
	}
}
