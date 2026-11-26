package logger

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"enterprise-im/backend/internal/config"
)

func TestLoggerRedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	l, err := New(config.LogConfig{Level: "info", Format: "json"}, &buf)
	if err != nil {
		t.Fatalf("New logger failed: %v", err)
	}

	l.Info("auth_attempt",
		slog.String("request_id", "r-123"),
		slog.String("password", "secret-pass"),
		slog.String("access_token", "jwt.secret.token"),
		slog.String("authorization", "Bearer super-secret"),
	)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal log json: %v (raw=%s)", err, buf.String())
	}

	for _, key := range []string{"password", "access_token", "authorization"} {
		if entry[key] != RedactedValue {
			t.Fatalf("expected %s to be redacted, got %v", key, entry[key])
		}
	}
	if entry["request_id"] != "r-123" {
		t.Fatalf("expected request_id r-123, got %v", entry["request_id"])
	}
}

func TestRedactHeaderAndQuery(t *testing.T) {
	got := RedactHeaderValue("Authorization", "Bearer my-jwt-token")
	if got != "Bearer "+RedactedValue {
		t.Fatalf("unexpected redacted header: %q", got)
	}
	if strings.Contains(got, "my-jwt-token") {
		t.Fatalf("token leaked in redacted header: %q", got)
	}

	q := SanitizeQuery("access_token=abc123&cursor=42")
	if strings.Contains(q, "abc123") {
		t.Fatalf("token leaked in sanitized query: %q", q)
	}
	if !strings.Contains(q, "cursor=42") {
		t.Fatalf("expected non-sensitive param preserved, got %q", q)
	}
}
