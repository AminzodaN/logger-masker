package logger

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"logger-mask/masklog"
)

type benchResp struct {
	TxID  string `json:"tx_id"`
	Card  string `json:"card"  log:"mask" mask:"pan"`
	Phone string `json:"phone" log:"mask" mask:"phone"`
}

// benchLogger собирает логгер с выводом в io.Discard, чтобы мерить стоимость
// логирования без учёта ввода-вывода.
func benchLogger() *Logger {
	return New(Config{Service: "svc", Env: "prod", Level: slog.LevelInfo, Output: io.Discard})
}

// BenchmarkInfof_CPerCall: C(ctx) пере-привязывает логгер на каждый вызов.
func BenchmarkInfof_CPerCall(b *testing.B) {
	l := benchLogger()
	ctx := WithRequestID(context.Background(), "req-123")
	_ = l
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		C(ctx).Infof("processed in %d ms", i)
	}
}

// BenchmarkInfof_CviaInto: HTTP-путь — Middleware привязал логгер один раз
// через Into, C(ctx) становится дешёвым lookup'ом.
func BenchmarkInfof_CviaInto(b *testing.B) {
	l := benchLogger()
	ctx := Into(WithRequestID(context.Background(), "req-123"), l.WithRequestID("req-123"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		C(ctx).Infof("processed in %d ms", i)
	}
}

// BenchmarkInfof_Prebound: логгер привязан один раз и переиспользуется (база).
func BenchmarkInfof_Prebound(b *testing.B) {
	l := benchLogger()
	bound := l.Ctx(WithRequestID(context.Background(), "req-123"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bound.Infof("processed in %d ms", i)
	}
}

// BenchmarkStructured_MaskV: структурный замаскированный payload через
// slog.LogValuer (рефлексия + кэш describe).
func BenchmarkStructured_MaskV(b *testing.B) {
	bound := benchLogger().Ctx(WithRequestID(context.Background(), "req-123"))
	resp := benchResp{TxID: "P-1", Card: "4111111111111111", Phone: "+992901234567"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bound.Info("partner reply", "resp", masklog.V(resp))
	}
}

// BenchmarkSugar_MaskFormat: тот же payload через %+v-путь (Stringer -> Format).
func BenchmarkSugar_MaskFormat(b *testing.B) {
	bound := benchLogger().Ctx(WithRequestID(context.Background(), "req-123"))
	resp := benchResp{TxID: "P-1", Card: "4111111111111111", Phone: "+992901234567"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bound.Infof("partner reply %s", masklog.V(resp))
	}
}
