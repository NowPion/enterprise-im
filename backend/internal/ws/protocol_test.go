package ws

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRouterRejectsUnsupportedAndUnknownTypes(t *testing.T) {
	var hub Hub
	client := newClient(context.Background(), nil)
	for kind, want := range map[string]int{"msg.send": 18004, "msg.delivered_ack": 18004, "msg.read_ack": 18004, "msg.recall": 18004, "presence.update": 18004, "msg.future": 18003, "ping": 18002} {
		hub.dispatch(client, envelope{Type: kind, ReqID: "ref", Data: []byte(`{}`)}, time.Time{})
		response := <-client.send
		failure := response.Data.(frameError)
		if response.Type != "error" || response.ReqID != "ref" || failure.RelatedReqID != "ref" || failure.RelatedType != kind || failure.Code != want || failure.Retryable {
			t.Fatalf("unexpected response to %s: %+v", kind, response)
		}
	}
	ref := frameReference([]byte(`{"req_id":"original","type":"msg.send","data":{"content":"`))
	if ref.ReqID != "original" || ref.Type != "msg.send" {
		t.Fatal("oversized message lost its available correlation fields")
	}
}

func TestEnvelopeBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"type":"ping","req_id":"req-1","timestamp":1795687200123,"data":{}}`, 0},
		{`{"type":"ping","req_id":"客户端:42","timestamp":1795687200123,"data":{}}`, 0},
		{`{"type":"ping","req_id":"req-1","timestamp":1795687200123,"DATA":{}}`, 18002},
		{`{"type":"msg.send","req_id":"req-1","timestamp":1795687200123}`, 18002},
		{`{"type":"other","req_id":"req-1","timestamp":1795687200123,"data":null}`, 18002},
		{`{"type":"ping","req_id":" ","timestamp":1795687200123,"data":{}}`, 18002},
		{`{"type":"ping","req_id":"req-1","timestamp":1795687200,"data":{}}`, 18002},
		{`{"type":"ping","req_id":"req-1","timestamp":"1795687200123","data":{}}`, 18002},
		{`{"nonce":"bare"}`, 18002}, {`[]`, 18002}, {`{} {}`, 18002},
	} {
		env, failure := decodeEnvelope([]byte(tc.body))
		if (failure != nil && failure.Code != tc.code) || (failure == nil && tc.code != 0) {
			t.Errorf("decode %s: %v", tc.body, failure)
		}
		if strings.Contains(tc.body, `"req-1"`) && env.ReqID != "req-1" {
			t.Errorf("lost correlation in %s", tc.body)
		}
	}
}
