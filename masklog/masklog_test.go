package masklog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type sample struct {
	TxID   string `json:"tx_id"`
	Card   string `json:"card"   log:"mask" mask:"pan"`
	Phone  string `json:"phone"  log:"mask" mask:"phone"`
	Email  string `json:"email"  log:"mask" mask:"email"`
	Holder string `json:"holder" log:"mask" mask:"name"`
	Secret string `json:"secret" log:"drop"`
}

// TestFormatAppliesTags проверяет, что Format маскирует поля по тегам и не
// пропускает в вывод сырые значения.
func TestFormatAppliesTags(t *testing.T) {
	s := sample{
		TxID:   "P-1",
		Card:   "4111111111111111",
		Phone:  "+992901234567",
		Email:  "alisher@mail.tj",
		Holder: "Alisher Karimov",
		Secret: "top",
	}
	got := Format(s)

	for _, leak := range []string{"4111111111111111", "+992901234567", "alisher@mail.tj", "Karimov", "top"} {
		if strings.Contains(got, leak) {
			t.Errorf("Format leaked %q: %s", leak, got)
		}
	}
	for _, want := range []string{"411111******1111", "*********4567", "a***@mail.tj", "A****** K******"} {
		if !strings.Contains(got, want) {
			t.Errorf("Format missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "secret") {
		t.Errorf("dropped field present: %s", got)
	}
}

// TestLogValueMasksViaSlog проверяет slog-путь: V(x) даёт вложенный объект с
// замаскированными полями и без утечек.
func TestLogValueMasksViaSlog(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	l.Info("x", "resp", V(sample{
		Card:   "4111111111111111",
		Phone:  "+992901234567",
		Secret: "top",
	}))

	line := buf.String()
	for _, leak := range []string{"4111111111111111", "+992901234567", "top"} {
		if strings.Contains(line, leak) {
			t.Errorf("slog output leaked %q: %s", leak, line)
		}
	}
	// response must be a nested object with the masked card.
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	resp, ok := rec["resp"].(map[string]any)
	if !ok {
		t.Fatalf("resp is not a nested object: %T", rec["resp"])
	}
	if resp["card"] != "411111******1111" {
		t.Errorf("card not masked in group: %v", resp["card"])
	}
	if _, present := resp["secret"]; present {
		t.Errorf("dropped field present in group: %v", resp)
	}
}

// TestFormatNilAndCycleSafe проверяет, что Format не падает на nil и на
// самоссылающейся структуре (разрыв цикла).
func TestFormatNilAndCycleSafe(t *testing.T) {
	if got := Format(nil); got != "<nil>" {
		t.Errorf("Format(nil) = %q", got)
	}
	type node struct {
		Next *node
	}
	n := &node{}
	n.Next = n // self-reference
	if got := Format(n); !strings.Contains(got, "cycle") {
		t.Errorf("cycle not detected: %s", got)
	}
}

// TestNonTaggedStringishPreserved проверяет, что нетегированное поле-структура,
// чьё значение живёт в методах (time.Time), не схлопывается в пустую группу, а
// выводится своей текстовой формой — в обоих путях (slog и fmt).
func TestNonTaggedStringishPreserved(t *testing.T) {
	type receipt struct {
		TxID      string    `json:"tx_id"`
		CreatedAt time.Time `json:"created_at"`
		Amount    int       `json:"amount"`
	}
	r := receipt{
		TxID:      "P-1",
		CreatedAt: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC),
		Amount:    500,
	}
	const wantTime = "2026-10-27T10:00:00Z" // RFC3339 через TextMarshaler

	// slog-путь: created_at должен присутствовать как строка со временем.
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "receipt", V(r))
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	got, _ := rec["receipt"].(map[string]any)
	if got == nil {
		t.Fatalf("receipt missing: %s", buf.String())
	}
	if got["created_at"] != wantTime {
		t.Errorf("slog created_at = %v, want %q", got["created_at"], wantTime)
	}

	// fmt-путь: Format не должен печатать created_at:{} и обязан содержать время.
	s := Format(r)
	if strings.Contains(s, "created_at:{}") {
		t.Errorf("Format collapsed time to empty struct: %s", s)
	}
	if !strings.Contains(s, wantTime) {
		t.Errorf("Format missing time %q: %s", wantTime, s)
	}
}

// TestMaskHelpers проверяет каждую функцию маскирования на граничных примерах.
func TestMaskHelpers(t *testing.T) {
	cases := []struct{ typ, in, want string }{
		{MaskPAN, "4111111111111111", "411111******1111"},
		{MaskPAN, "123", "***"},
		{MaskPhone, "+992901234567", "*********4567"},
		{MaskEmail, "a@b.co", "a***@b.co"},
		{MaskEmail, "nodomain", "***"},
		{MaskName, "Alisher Karimov", "A****** K******"},
		{MaskLast4, "918123456", "*****3456"},
		{MaskLast4, "123", "***"},
		{MaskFirst4, "4111111111111111", "4111************"},
		{MaskFirst4, "123", "***"},
		{MaskBIN, "4111111111111111", "411111**********"},
		{MaskIP, "203.0.113.42", "203.0.113.***"},
		{MaskIP, "2001:db8::1a2b", "2001:db8::***"},
		{MaskIP, "notanip", "***"},
		{"unknown", "whatever", "***"},
	}
	for _, c := range cases {
		if got := mask(c.typ, c.in); got != c.want {
			t.Errorf("mask(%q, %q) = %q, want %q", c.typ, c.in, got, c.want)
		}
	}
}
