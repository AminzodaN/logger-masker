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
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
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

// replaceAttr нормализует метку времени к UTC RFC3339 с миллисекундами, чтобы
// время во всех сервисах было в едином формате и часовом поясе.
func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey {
		if t := a.Value.Time(); !t.IsZero() {
			a.Value = slog.StringValue(t.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
		}
	}
	return a
}

// --- printf-сахар ------------------------------------------------------------

// logf — общая реализация всех *f-методов: проверяет уровень (чтобы не платить
// за fmt.Sprintf, когда уровень выключен) и пишет отформатированное сообщение.
func (l *Logger) logf(level slog.Level, format string, args ...any) {
	if !l.sl.Enabled(context.Background(), level) {
		return
	}
	l.sl.Log(context.Background(), level, fmt.Sprintf(format, args...))
}

// Debugf пишет форматированное сообщение на уровне debug.
func (l *Logger) Debugf(format string, args ...any) { l.logf(slog.LevelDebug, format, args...) }

// Infof пишет форматированное сообщение на уровне info.
func (l *Logger) Infof(format string, args ...any) { l.logf(slog.LevelInfo, format, args...) }

// Warnf пишет форматированное сообщение на уровне warn.
func (l *Logger) Warnf(format string, args ...any) { l.logf(slog.LevelWarn, format, args...) }

// Errorf пишет форматированное сообщение на уровне error.
func (l *Logger) Errorf(format string, args ...any) { l.logf(slog.LevelError, format, args...) }

// Printf — алиас Infof для drop-in замены Printf из стандартного пакета log.
func (l *Logger) Printf(format string, args ...any) { l.logf(slog.LevelInfo, format, args...) }

// Fatalf пишет сообщение на уровне error и завершает процесс с кодом 1.
// Использовать только на старте / в main.
func (l *Logger) Fatalf(format string, args ...any) {
	l.logf(slog.LevelError, format, args...)
	os.Exit(1)
}

// --- структурный проброс -----------------------------------------------------

// Debug пишет msg с парами ключ-значение на уровне debug.
func (l *Logger) Debug(msg string, args ...any) { l.sl.Debug(msg, args...) }

// Info пишет msg с парами ключ-значение на уровне info.
func (l *Logger) Info(msg string, args ...any) { l.sl.Info(msg, args...) }

// Warn пишет msg с парами ключ-значение на уровне warn.
func (l *Logger) Warn(msg string, args ...any) { l.sl.Warn(msg, args...) }

// Error пишет msg с парами ключ-значение на уровне error.
func (l *Logger) Error(msg string, args ...any) { l.sl.Error(msg, args...) }

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
