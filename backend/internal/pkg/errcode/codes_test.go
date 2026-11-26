package errcode

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestFrozenErrorCodeTableComplete(t *testing.T) {
	expectedCodes := []int{
		0,
		10000, 10001, 10002, 10003,
		11001, 11002, 11003, 11004, 11005, 11006, 11007, 11008, 11009,
		12001, 12002, 12003, 12004, 12005, 12006, 12007, 12008,
		13001, 13002, 13003, 13004, 13005, 13006,
		14001, 14002, 14003, 14004, 14005, 14006, 14007, 14008,
		15001, 15002, 15003, 15004, 15005, 15006, 15007, 15008, 15009, 15010,
		16001, 16002, 16003, 16004, 16005, 16006,
		17001, 17002, 17003, 17004, 17005, 17006,
		18001, 18002, 18003, 18004, 18005, 18006, 18007,
		19001, 19002, 19003, 19004,
	}

	all := All()
	if len(all) != len(expectedCodes) {
		t.Fatalf("expected %d error codes, got %d", len(expectedCodes), len(all))
	}

	for _, code := range expectedCodes {
		entry, ok := Lookup(code)
		if !ok {
			t.Fatalf("missing frozen error code %d", code)
		}
		if entry.Message == "" {
			t.Fatalf("empty message for code %d", code)
		}
		if entry.HTTPStatus < 200 || entry.HTTPStatus >= 600 {
			t.Fatalf("invalid HTTPStatus %d for code %d", entry.HTTPStatus, code)
		}
	}

	if ErrWSSessionRevoked.WSCloseCode != 1008 {
		t.Fatalf("expected 18006 WSCloseCode 1008, got %d", ErrWSSessionRevoked.WSCloseCode)
	}
	if ErrWSInternalError.WSCloseCode != 1011 {
		t.Fatalf("expected 18007 WSCloseCode 1011, got %d", ErrWSInternalError.WSCloseCode)
	}
}

func TestEnvelopeHelpers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ok", func(c *gin.Context) {
		c.Set(ContextKeyRequestID, "r-test-ok")
		OK(c, map[string]string{"ping": "pong"})
	})
	r.GET("/fail", func(c *gin.Context) {
		c.Set(ContextKeyRequestID, "r-test-fail")
		FailWithData(c, ErrMySQLUnavailable, map[string]string{"mysql": "unavailable"})
	})

	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest(http.MethodGet, "/ok", nil)
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK || w1.Header().Get(HeaderRequestID) != "r-test-ok" {
		t.Fatalf("unexpected /ok response: status=%d header=%q body=%s", w1.Code, w1.Header().Get(HeaderRequestID), w1.Body.String())
	}

	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodGet, "/fail", nil)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusServiceUnavailable || w2.Header().Get(HeaderRequestID) != "r-test-fail" {
		t.Fatalf("unexpected /fail response: status=%d header=%q body=%s", w2.Code, w2.Header().Get(HeaderRequestID), w2.Body.String())
	}
}
