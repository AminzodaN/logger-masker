package main

import (
	"context"
	"log/slog"

	"logger-mask/logger"
	"logger-mask/masklog"
)

// PartnerResponse — типичный payload ответа партнёра. Card маскируется как PAN,
// BIN — только первые 6 цифр эмитента, phone/holder частично; CVV выбрасывается.
type PartnerResponse struct {
	TxID   string `json:"tx_id"`
	Status string `json:"status"`
	Card   string `json:"card"   log:"mask" mask:"pan"`
	BIN    string `json:"bin"    log:"mask" mask:"bin"`
	Phone  string `json:"phone"  log:"mask" mask:"phone"`
	Holder string `json:"holder" log:"mask" mask:"name"`
	CVV    string `json:"cvv"    log:"drop"`
}

// ServerRequest — тело входящего запроса: у account видно последние 4 символа,
// у device_id — первые 4, у client_ip скрыта хостовая часть (аноним по подсети).
type ServerRequest struct {
	PaymentID string `json:"payment_id"`
	Account   string `json:"account"   log:"mask" mask:"last4"`
	DeviceID  string `json:"device_id" log:"mask" mask:"first4"`
	ClientIP  string `json:"client_ip" log:"mask" mask:"ip"`
}

// String/GoString делают PartnerResponse безопасным даже при прямом логировании
// через %v / %+v (fmt вызовет эти методы вместо печати сырых полей).
func (r PartnerResponse) String() string   { return masklog.Format(r) }
func (r PartnerResponse) GoString() string { return masklog.Format(r) }

// String/GoString делают ServerRequest безопасным при прямом %v / %+v.
func (r ServerRequest) String() string   { return masklog.Format(r) }
func (r ServerRequest) GoString() string { return masklog.Format(r) }

// main — демонстрация: собирает логгер, кладёт request_id в контекст и логирует
// замаскированные тело запроса и ответа — структурно и через printf-сахар.
func main() {
	log := logger.New(logger.Config{
		Service: "payment-gateway",
		Version: "1.14.2",
		Env:     "prod",
		Level:   slog.LevelInfo,
	})

	// Обычно request_id кладёт logger.Middleware; здесь задаём вручную.
	ctx := logger.WithRequestID(context.Background(), logger.NewRequestID())

	req := ServerRequest{
		PaymentID: "123456",
		Account:   "918123456",
		DeviceID:  "a1b2c3d4e5",
		ClientIP:  "203.0.113.42",
	}

	resp := PartnerResponse{
		TxID:   "P-889120",
		Status: "DECLINED",
		Card:   "4111111111111111",
		BIN:    "5500005555555559",
		Phone:  "+992901234567",
		Holder: "Alisher Karimov",
		CVV:    "123",
	}

	// Структурный вызов: тело запроса и ответа — вложенные группы с маскированием
	// по тегам, request_id из ctx проставляется автоматически через logger.C.
	logger.C(ctx).Error("partner payment declined",
		"partner", "visa_acq",
		"rrn", "629415003781",
		"error_code", "PAYMENT_DECLINED",
		"duration_ms", 412,
		"request", masklog.V(req),
		"response", masklog.V(resp),
	)

	// printf-сахар; %+v безопасен, т.к. у типов есть String() на базе masklog.Format.
	logger.C(ctx).Infof("request %+v", req)
	logger.C(ctx).Infof("processed response %+v", resp)

	_ = log
}
