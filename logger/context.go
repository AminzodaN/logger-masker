package logger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// reRequestID задаёт допустимую форму входящего идентификатора запроса:
// [A-Za-z0-9_-]{1,128}. Чужой заголовок не доверенный — не даём затащить в логи
// управляющие символы, переводы строк или гигантские значения.
var reRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// validRequestID проверяет, что id пригоден для использования как request_id.
func validRequestID(id string) bool { return reRequestID.MatchString(id) }

// RequestIDField — имя поля лога, под которым пишется идентификатор запроса.
const RequestIDField = "request_id"

// HeaderRequestID — HTTP-заголовок, несущий request_id между сервисами.
const HeaderRequestID = "X-Request-Id"

type idKey struct{}
type loggerKey struct{}

// WithRequestID возвращает дочерний контекст с id. Прокидывай его дальше по
// цепочке вызовов, чтобы каждый сервис и горутина ставили один и тот же id в
// свои записи.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, idKey{}, id)
}

// RequestID возвращает request_id из ctx или "" если его там нет.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(idKey{}).(string); ok {
		return id
	}
	return ""
}

// Into возвращает дочерний контекст с уже привязанным по request_id логгером,
// чтобы C(ctx) отдавал его без клонирования handler'а на каждый вызов. Привяжи
// один раз на запрос (это делает Middleware) — и цена лога падает до lookup'а.
func Into(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// loggerFrom достаёт привязанный логгер из ctx (положенный через Into) или nil.
func loggerFrom(ctx context.Context) *Logger {
	if ctx == nil {
		return nil
	}
	if l, ok := ctx.Value(loggerKey{}).(*Logger); ok {
		return l
	}
	return nil
}

// WithRequestID возвращает дочерний логгер, который ставит request_id в каждую
// запись.
func (l *Logger) WithRequestID(id string) *Logger {
	return l.With(RequestIDField, id)
}

// Ctx возвращает дочерний логгер, помеченный request_id из ctx (если он есть).
func (l *Logger) Ctx(ctx context.Context) *Logger {
	if id := RequestID(ctx); id != "" {
		return l.WithRequestID(id)
	}
	return l
}

// C возвращает логгер для ctx: либо привязанный один раз через Into (дешёвый
// lookup, обычный HTTP-путь), либо процессный default с проставленным из ctx
// request_id. Обычная точка вызова: logger.C(ctx).Infof(...).
func C(ctx context.Context) *Logger {
	if l := loggerFrom(ctx); l != nil {
		return l
	}
	return Default().Ctx(ctx)
}

// NewRequestID возвращает случайный 16-байтный hex request_id. Используется,
// когда во входящем запросе идентификатора ещё нет.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Middleware достаёт request_id из заголовка X-Request-Id или traceparent
// (генерит, если нет), кладёт его в контекст запроса и возвращает в ответе —
// так весь путь запроса делит один идентификатор. Логгер привязывается к
// запросу один раз (через Into), чтобы C(ctx) внутри был дешёвым.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestIDFromRequest(r)
		w.Header().Set(HeaderRequestID, id)
		ctx := WithRequestID(r.Context(), id)
		ctx = Into(ctx, Default().WithRequestID(id)) // привязка один раз на запрос
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// InjectRequestID копирует request_id из ctx в заголовки исходящего запроса,
// чтобы соседний сервис продолжил тот же путь запроса.
func InjectRequestID(ctx context.Context, r *http.Request) {
	if id := RequestID(ctx); id != "" {
		r.Header.Set(HeaderRequestID, id)
	}
}

// requestIDFromRequest выбирает идентификатор для входящего запроса: сперва
// X-Request-Id, затем trace-id из traceparent, иначе генерит новый.
func requestIDFromRequest(r *http.Request) string {
	if rid := r.Header.Get(HeaderRequestID); validRequestID(rid) {
		return rid
	}
	if tp := r.Header.Get("traceparent"); tp != "" {
		// traceparent = version-traceid-spanid-flags; берём часть trace-id.
		if parts := strings.SplitN(tp, "-", 3); len(parts) >= 2 && validRequestID(parts[1]) {
			return parts[1]
		}
	}
	return NewRequestID()
}
