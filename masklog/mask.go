// Пакет masklog маскирует чувствительные поля структур в логах согласно их
// тегам `log:"..."` / `mask:"..."`. Пакет не привязан к конкретной лог-либе:
// точка входа masklog.V(x) реализует slog.LogValuer (slog выдаёт
// замаскированную группу) и fmt.Formatter/Stringer (%v / %+v тоже маскируются).
//
// Пакет самодостаточен и без сторонних зависимостей — его можно скопировать или
// импортировать в любой сервис без изменений.
package masklog

import "strings"

// Типы маскирования, понимаемые тегом `mask:"..."`.
const (
	MaskPAN    = "pan"
	MaskPhone  = "phone"
	MaskEmail  = "email"
	MaskName   = "name"
	MaskLast4  = "last4"  // показать последние 4 символа, остальное скрыть
	MaskFirst4 = "first4" // показать первые 4 символа, остальное скрыть
	MaskBIN    = "bin"    // BIN карты: показать первые 6 цифр, остальное скрыть
	MaskIP     = "ip"     // скрыть хостовую часть IP, оставить сеть
)

// mask возвращает необратимо замаскированную форму s по типу maskType. Все
// операции идут по рунам, поэтому кириллица (и любой многобайтовый текст)
// обрабатывается корректно. Неизвестный или пустой maskType даёт дефолт "***".
func mask(maskType, s string) string {
	switch maskType {
	case MaskPAN:
		return maskPAN(s)
	case MaskPhone:
		return maskPhone(s)
	case MaskEmail:
		return maskEmail(s)
	case MaskName:
		return maskName(s)
	case MaskLast4:
		return maskLast4(s)
	case MaskFirst4:
		return maskFirst4(s)
	case MaskBIN:
		return maskBIN(s)
	case MaskIP:
		return maskIP(s)
	default:
		return "***"
	}
}

// maskPAN оставляет первые 6 и последние 4 руны, напр. 4111111111111111 ->
// 411111******1111. Строки в 10 рун и короче маскируются целиком, чтобы
// короткое значение нельзя было восстановить.
func maskPAN(s string) string {
	r := []rune(s)
	if len(r) <= 10 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:6]) + strings.Repeat("*", len(r)-10) + string(r[len(r)-4:])
}

// keepLast оставляет видимыми последние n рун, остальное заменяет звёздочками.
// Значение в n рун и короче маскируется целиком, чтобы по короткой строке
// нельзя было восстановить исходник.
func keepLast(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return strings.Repeat("*", len(r))
	}
	return strings.Repeat("*", len(r)-n) + string(r[len(r)-n:])
}

// keepFirst оставляет видимыми первые n рун, остальное заменяет звёздочками.
// Значение в n рун и короче маскируется целиком (как и keepLast, fail-closed).
func keepFirst(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return strings.Repeat("*", len(r))
	}
	return string(r[:n]) + strings.Repeat("*", len(r)-n)
}

// maskPhone оставляет только последние 4 руны, напр. +992901234567 -> *********4567.
func maskPhone(s string) string { return keepLast(s, 4) }

// maskLast4 — универсальная частичная маска: показывает последние 4 символа, а
// остальное скрывает (напр. счёт 918123456 -> *****3456). Нужна, когда хвост
// значения можно оставить видимым для сверки (счёт, карта, телефон и т.п.).
func maskLast4(s string) string { return keepLast(s, 4) }

// maskFirst4 показывает первые 4 символа, остальное скрывает — зеркало maskLast4
// для случаев, когда для сверки полезен префикс значения.
func maskFirst4(s string) string { return keepFirst(s, 4) }

// maskBIN показывает первые 6 цифр карты (BIN/IIN — идентификатор эмитента), а
// остальное скрывает: напр. 4111111111111111 -> 411111**********. Позволяет
// аналитику по банку-эмитенту, не раскрывая полный номер карты.
func maskBIN(s string) string { return keepFirst(s, 6) }

// maskIP скрывает хостовую часть IP, оставляя сеть: IPv4 203.0.113.42 ->
// 203.0.113.***, IPv6 2001:db8::1a2b -> 2001:db8::***. Так адрес можно соотнести
// с подсетью/регионом, но не идентифицировать конкретного клиента. Значение без
// разделителя маскируется целиком (fail-closed).
func maskIP(s string) string {
	sep := byte('.')
	if strings.IndexByte(s, '.') < 0 {
		sep = ':'
	}
	i := strings.LastIndexByte(s, sep)
	if i <= 0 || i == len(s)-1 {
		return "***"
	}
	return s[:i+1] + "***"
}

// maskEmail оставляет первую руну локальной части и весь домен,
// напр. alisher@mail.tj -> a***@mail.tj. Значение без "@" уходит в дефолтную
// маску, чтобы его нельзя было принять за реальный адрес.
func maskEmail(s string) string {
	at := strings.IndexByte(s, '@')
	if at <= 0 {
		return "***"
	}
	local := []rune(s[:at])
	domain := s[at:] // включает '@'
	return string(local[0]) + "***" + domain
}

// maskName оставляет первую руну каждого слова, разделённого пробелами,
// напр. "Alisher Karimov" -> "A****** K******". Исходные пробелы сохраняются.
func maskName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inWord := false
	var wordLen int
	flush := func() {
		if wordLen > 1 {
			b.WriteString(strings.Repeat("*", wordLen-1))
		}
		wordLen = 0
	}
	for _, c := range s {
		if c == ' ' || c == '\t' || c == '\n' {
			if inWord {
				flush()
				inWord = false
			}
			b.WriteRune(c)
			continue
		}
		if !inWord {
			b.WriteRune(c) // первая руна слова остаётся видимой
			inWord = true
			wordLen = 1
		} else {
			wordLen++
		}
	}
	if inWord {
		flush()
	}
	return b.String()
}
