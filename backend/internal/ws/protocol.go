package ws

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/pkg/errcode"
)

type envelope struct {
	Type      string          `json:"type"`
	ReqID     string          `json:"req_id"`
	Timestamp int64           `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

func frameReference(prefix []byte) envelope {
	var env envelope
	decoder := json.NewDecoder(bytes.NewReader(prefix))
	if tok, err := decoder.Token(); err != nil || tok != json.Delim('{') {
		return env
	}
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			break
		}
		switch name {
		case "type":
			err = decoder.Decode(&env.Type)
		case "req_id":
			err = decoder.Decode(&env.ReqID)
		default:
			var ignored json.RawMessage
			err = decoder.Decode(&ignored)
		}
		if err != nil || (env.Type != "" && env.ReqID != "") {
			break
		}
	}
	return env
}

func decodeEnvelope(raw []byte) (envelope, *errcode.Error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return frameReference(raw), errcode.ErrWSInvalidEnvelope
	}
	env := envelope{Data: fields["data"]}
	reqErr := json.Unmarshal(fields["req_id"], &env.ReqID)
	typeErr := json.Unmarshal(fields["type"], &env.Type)
	timeErr := json.Unmarshal(fields["timestamp"], &env.Timestamp)
	if reqErr != nil || typeErr != nil || timeErr != nil || !utf8.Valid(raw) || strings.TrimSpace(env.Type) == "" ||
		strings.TrimSpace(env.ReqID) == "" || strings.IndexFunc(env.ReqID, unicode.IsControl) >= 0 ||
		env.Timestamp < 1_000_000_000_000 || env.Timestamp > 9_999_999_999_999 || len(env.Data) == 0 || env.Data[0] != '{' {
		return env, errcode.ErrWSInvalidEnvelope
	}
	return env, nil
}

type frame struct {
	Type        string `json:"type"`
	ReqID       string `json:"req_id"`
	Timestamp   int64  `json:"timestamp"`
	Data        any    `json:"data"`
	controlPong *string
}

func outgoing(kind, reqID string, data any) frame {
	if reqID == "" {
		reqID = uuid.NewString()
	}
	return frame{Type: kind, ReqID: reqID, Timestamp: time.Now().UnixMilli(), Data: data}
}

type frameError struct {
	Code         int    `json:"code"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable"`
	RelatedType  string `json:"related_type"`
	RelatedReqID string `json:"related_req_id"`
}

func errorFrame(env envelope, failure *errcode.Error) frame {
	return outgoing("error", env.ReqID, frameError{failure.Code, failure.Message, failure.Code == errcode.ErrWSInternalError.Code, env.Type, env.ReqID})
}

func noticeFrame(user, session uint64, event string) frame {
	id := uuid.NewString()
	return outgoing("system.notice", id, map[string]any{
		"event": event, "notice_id": id, "actor_id": nil, "conversation_id": nil,
		"target_user_id": strconv.FormatUint(user, 10), "payload": map[string]string{"session_id": strconv.FormatUint(session, 10)},
	})
}
