// Пакет logger — стандартный фасад логирования для всех сервисов. Оборачивает
// стандартный log/slog и добавляет:
//
//   - printf-сахар (Infof/Warnf/Errorf/Debugf/Printf/Fatalf) для частого случая
//     «отформатировать сообщение»;
//   - структурный проброс (Info/Warn/Error/Debug), когда нужны пары ключ-значение;
//   - распространение request_id через context.Context и net/http, чтобы один
//     запрос прослеживался по всем сервисам системы (см. context.go).
//
// Чувствительные payload'ы маскируются тегами полей и логируются через соседний
// пакет masklog, напр.:
//
//	logger.C(ctx).Info("partner reply", "resp", masklog.V(resp))
//
// Сервисы обязаны ходить через logger.New, а не создавать slog напрямую, чтобы у
// каждой записи была единая форма (service/version/env/time).
package logger

import (
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"

	"logger-mask/masklog"
)

// EnvLocal включает человекочитаемый text-вывод вместо JSON.
const EnvLocal = "local"

// Config настраивает общий конструктор логгера.
type Config struct {
	Service   string     // напр. "payment-gateway"
	Version   string     // напр. "1.14.2"
	Env       string     // напр. "prod" | "stage" | "local"
	Level     slog.Level // минимальный уровень; debug гасится вне local/stage
	Output    io.Writer  // по умолчанию os.Stdout
	AddSource bool       // добавлять в каждую запись файл:строку
}

// Logger оборачивает *slog.Logger printf-сахаром. Нулевое значение непригодно;
// создавай через New.
type Logger struct {
	sl *slog.Logger
}

// New собирает единый логгер: JSON в stdout (text при local), UTC RFC3339 с
// миллисекундами и прикреплённые к каждой записи service/version/env. Также
// ставит результат процессным default (см. Default). Через него сервисы и
// должны создавать логгер, чтобы форма записей была одинаковой.
func New(cfg Config) *Logger {
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}

	level := cfg.Level
	// debug запрещён в prod: тихо поднимаем пол уровня.
	if cfg.Env != EnvLocal && cfg.Env != "stage" && level < slog.LevelInfo {
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level:       level,
		AddSource:   cfg.AddSource,
		ReplaceAttr: replaceAttr,
	}
	var h slog.Handler
	if cfg.Env == EnvLocal {
		h = slog.NewTextHandler(out, opts)
	} else {
		h = slog.NewJSONHandler(out, opts)
	}

	sl := slog.New(h).With(
		"service", cfg.Service,
		"version", cfg.Version,
		"env", cfg.Env,
	)
	l := &Logger{sl: sl}
	SetDefault(l)
	return l
}

// Wrap адаптирует готовый *slog.Logger к фасаду logger — нужна, когда slog уже
// сконфигурирован снаружи (тесты, встраивание).
func Wrap(sl *slog.Logger) *Logger { return &Logger{sl: sl} }

// Slog возвращает нижележащий *slog.Logger для мест, где нужен именно slog
// (например, библиотеки, принимающие *slog.Logger).
func (l *Logger) Slog() *slog.Logger { return l.sl }

// With возвращает дочерний логгер с постоянными атрибутами ключ-значение,
// которые будут добавляться ко всем последующим записям.
func (l *Logger) With(args ...any) *Logger {
	return &Logger{sl: l.sl.With(args...)}
}

// replaceAttr нормализует метку времени и служит страховкой на выходе: даже если
// значение не обёрнуто в masklog.V, оно маскируется перед записью. Срабатывает
// на любом уровне групп, так что чувствительные поля не утекут из вложенных
// структур, сырых тел, текста ошибок и msg. Работает поверх обработчика slog,
// уже прошедшего Value.Resolve (обёртки masklog.V к этому моменту раскрыты).
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	// Время -> UTC RFC3339 с миллисекундами (единый формат во всех сервисах).
	if len(groups) == 0 && a.Key == slog.TimeKey {
		if t := a.Value.Time(); !t.IsZero() {
			a.Value = slog.StringValue(t.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
		}
		return a
	}
	// Служебные level/source не трогаем.
	if len(groups) == 0 && (a.Key == slog.LevelKey || a.Key == slog.SourceKey) {
		return a
	}
	// Ключи из стоп-списка на любом уровне групп -> маска по правилу ключа
	// (drop-ключи => ***). Для составного значения под таким ключом — ***.
	if masklog.Sensitive(a.Key) {
		if a.Value.Kind() == slog.KindString {
			return slog.String(a.Key, masklog.MaskValue(a.Key, a.Value.String()))
		}
		return slog.String(a.Key, masklog.Redacted)
	}
	return maskAttrValue(a)
}

// maskAttrValue применяет страховочную маску к значению атрибута по его виду:
// строки (включая msg и текст ошибок) — текстовая маска; сырые []byte /
// json.RawMessage — через JSON-движок (с fallback на текст); голые структуры /
// map / срезы структур — через masklog.V. Значения, уже умеющие маскировать
// себя сами (slog.LogValuer и наши обёртки-Marshaler'ы), не трогаются.
func maskAttrValue(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		s := a.Value.String()
		if m := masklog.MaskString(s); m != s {
			return slog.String(a.Key, m)
		}
		return a // быстрый путь: ничего не поменялось, 0 аллокаций
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case slog.LogValuer:
			return a
		case error:
			return slog.String(a.Key, masklog.MaskString(v.Error()))
		case []byte:
			return slog.Any(a.Key, masklog.JSON(v))
		case json.RawMessage:
			return slog.Any(a.Key, masklog.JSON(v))
		case json.Marshaler:
			return a // наши jsonBody/slogJSONArray и пользовательские маршалеры
		case encoding.TextMarshaler:
			return a
		default:
			if masklog.ShouldWrap(v) {
				return slog.Any(a.Key, masklog.V(v))
			}
			return a
		}
	}
	return a
}

// --- ядро записи -------------------------------------------------------------

// log — общее ядро структурных методов. Захватывает PC места вызова через
// runtime.Callers и пишет запись через Handler().Handle, чтобы AddSource
// указывал на место вызова, а не на logger.go (см. «wrapping output methods» в
// документации log/slog). ctx пробрасывается в Handle — handler'ы, читающие
// trace_id из ctx, его видят.
func (l *Logger) log(ctx context.Context, level slog.Level, msg string, args ...any) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !l.sl.Enabled(ctx, level) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:]) // пропустить [Callers, log, публичный метод]
	r := slog.NewRecord(time.Now(), level, msg, pcs[0])
	r.Add(args...)
	_ = l.sl.Handler().Handle(ctx, r)
}

// logf — ядро printf-методов: проверяет уровень (чтобы не платить за Sprintf при
// выключенном уровне) и пишет отформатированное сообщение с PC места вызова.
func (l *Logger) logf(ctx context.Context, level slog.Level, format string, args ...any) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !l.sl.Enabled(ctx, level) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:]) // пропустить [Callers, logf, публичный метод]
	r := slog.NewRecord(time.Now(), level, fmt.Sprintf(format, args...), pcs[0])
	_ = l.sl.Handler().Handle(ctx, r)
}

// --- printf-сахар ------------------------------------------------------------

// Debugf пишет форматированное сообщение на уровне debug.
func (l *Logger) Debugf(format string, args ...any) {
	l.logf(context.Background(), slog.LevelDebug, format, args...)
}

// Infof пишет форматированное сообщение на уровне info.
func (l *Logger) Infof(format string, args ...any) {
	l.logf(context.Background(), slog.LevelInfo, format, args...)
}

// Warnf пишет форматированное сообщение на уровне warn.
func (l *Logger) Warnf(format string, args ...any) {
	l.logf(context.Background(), slog.LevelWarn, format, args...)
}

// Errorf пишет форматированное сообщение на уровне error.
func (l *Logger) Errorf(format string, args ...any) {
	l.logf(context.Background(), slog.LevelError, format, args...)
}

// Printf — алиас Infof для drop-in замены Printf из стандартного пакета log.
func (l *Logger) Printf(format string, args ...any) {
	l.logf(context.Background(), slog.LevelInfo, format, args...)
}

// Fatalf пишет сообщение на уровне error и завершает процесс с кодом 1.
// Использовать только на старте / в main.
func (l *Logger) Fatalf(format string, args ...any) {
	l.logf(context.Background(), slog.LevelError, format, args...)
	os.Exit(1)
}

// printf-сахар с явным контекстом (для handler'ов, читающих trace_id из ctx).

// DebugfContext — Debugf с контекстом.
func (l *Logger) DebugfContext(ctx context.Context, format string, args ...any) {
	l.logf(ctx, slog.LevelDebug, format, args...)
}

// InfofContext — Infof с контекстом.
func (l *Logger) InfofContext(ctx context.Context, format string, args ...any) {
	l.logf(ctx, slog.LevelInfo, format, args...)
}

// WarnfContext — Warnf с контекстом.
func (l *Logger) WarnfContext(ctx context.Context, format string, args ...any) {
	l.logf(ctx, slog.LevelWarn, format, args...)
}

// ErrorfContext — Errorf с контекстом.
func (l *Logger) ErrorfContext(ctx context.Context, format string, args ...any) {
	l.logf(ctx, slog.LevelError, format, args...)
}

// --- структурный проброс -----------------------------------------------------

// Debug пишет msg с парами ключ-значение на уровне debug.
func (l *Logger) Debug(msg string, args ...any) { l.log(context.Background(), slog.LevelDebug, msg, args...) }

// Info пишет msg с парами ключ-значение на уровне info.
func (l *Logger) Info(msg string, args ...any) { l.log(context.Background(), slog.LevelInfo, msg, args...) }

// Warn пишет msg с парами ключ-значение на уровне warn.
func (l *Logger) Warn(msg string, args ...any) { l.log(context.Background(), slog.LevelWarn, msg, args...) }

// Error пишет msg с парами ключ-значение на уровне error.
func (l *Logger) Error(msg string, args ...any) { l.log(context.Background(), slog.LevelError, msg, args...) }

// структурный проброс с явным контекстом.

// DebugContext — Debug с контекстом.
func (l *Logger) DebugContext(ctx context.Context, msg string, args ...any) {
	l.log(ctx, slog.LevelDebug, msg, args...)
}

// InfoContext — Info с контекстом.
func (l *Logger) InfoContext(ctx context.Context, msg string, args ...any) {
	l.log(ctx, slog.LevelInfo, msg, args...)
}

// WarnContext — Warn с контекстом.
func (l *Logger) WarnContext(ctx context.Context, msg string, args ...any) {
	l.log(ctx, slog.LevelWarn, msg, args...)
}

// ErrorContext — Error с контекстом.
func (l *Logger) ErrorContext(ctx context.Context, msg string, args ...any) {
	l.log(ctx, slog.LevelError, msg, args...)
}

// --- процессный default -------------------------------------------------------

var (
	defaultMu sync.RWMutex
	defaultL  = Wrap(slog.Default())
)

// SetDefault ставит l процессным логгером по умолчанию (его отдают L и C).
func SetDefault(l *Logger) {
	defaultMu.Lock()
	defaultL = l
	defaultMu.Unlock()
}

// Default возвращает процессный логгер по умолчанию.
func Default() *Logger {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultL
}

// L — краткая форма Default, чтобы логировать из любого места без проброса *Logger.
func L() *Logger { return Default() }
