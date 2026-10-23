package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestLogger собирает логгер с выводом в буфер для проверки содержимого записей.
func newTestLogger(buf *bytes.Buffer) *Logger {
	return New(Config{
		Service: "svc",
		Version: "1.0.0",
		Env:     "prod",
		Level:   slog.LevelDebug, // asked for debug, but prod floor should raise it
		Output:  buf,
	})
}

// TestSugarAndBaseFields проверяет, что Infof форматирует msg и что в каждой
// записи есть базовые поля service/version/env.
func TestSugarAndBaseFields(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf)
	l.Infof("hello %s", "world")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, buf.String())
	}
	if rec["msg"] != "hello world" {
		t.Errorf("msg = %v", rec["msg"])
	}
	for k, want := range map[string]string{"service": "svc", "version": "1.0.0", "env": "prod"} {
		if rec[k] != want {
			t.Errorf("%s = %v, want %v", k, rec[k], want)
		}
	}
}

// TestProdDebugFloor проверяет, что в prod уровень debug гасится.
func TestProdDebugFloor(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf)
	l.Debugf("should be suppressed in prod")
	if buf.Len() != 0 {
		t.Errorf("debug emitted in prod: %s", buf.String())
	}
}

// TestRequestIDPropagation проверяет, что request_id из контекста попадает в лог.
func TestRequestIDPropagation(t *testing.T) {
	var buf bytes.Buffer
	New(Config{Service: "svc", Env: "prod", Output: &buf}) // sets default

	ctx := WithRequestID(context.Background(), "req-123")
	if got := RequestID(ctx); got != "req-123" {
		t.Fatalf("RequestID = %q", got)
	}
	C(ctx).Infof("hi")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec[RequestIDField] != "req-123" {
		t.Errorf("%s = %v", RequestIDField, rec[RequestIDField])
	}
}

// TestMiddlewareUsesIncomingHeader проверяет, что middleware берёт id из
// входящего заголовка и возвращает его в ответе.
func TestMiddlewareUsesIncomingHeader(t *testing.T) {
	var seen string
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderRequestID, "abc-789")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if seen != "abc-789" {
		t.Errorf("ctx request id = %q", seen)
	}
	if rw.Header().Get(HeaderRequestID) != "abc-789" {
		t.Errorf("response header = %q", rw.Header().Get(HeaderRequestID))
	}
}

// TestMiddlewareGeneratesWhenAbsent проверяет, что при отсутствии id middleware
// генерит новый (32 hex-символа).
func TestMiddlewareGeneratesWhenAbsent(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if id := rw.Header().Get(HeaderRequestID); len(id) != 32 {
		t.Errorf("generated id = %q (len %d), want 32 hex chars", id, len(id))
	}
}

// TestInjectRequestID проверяет, что id из ctx кладётся в исходящий запрос.
func TestInjectRequestID(t *testing.T) {
	ctx := WithRequestID(context.Background(), "out-1")
	req := httptest.NewRequest(http.MethodGet, "http://downstream/", nil)
	InjectRequestID(ctx, req)
	if got := req.Header.Get(HeaderRequestID); got != "out-1" {
		t.Errorf("outgoing header = %q", got)
	}
}

// TestTimeIsUTCRFC3339 проверяет формат метки времени: UTC RFC3339 с миллисекундами.
func TestTimeIsUTCRFC3339(t *testing.T) {
	var buf bytes.Buffer
	l := newTestLogger(&buf)
	l.Infof("x")
	var rec map[string]any
	_ = json.Unmarshal(buf.Bytes(), &rec)
	ts, _ := rec["time"].(string)
	if !strings.HasSuffix(ts, "Z") || !strings.Contains(ts, ".") {
		t.Errorf("time not UTC RFC3339 with ms: %q", ts)
	}
}
