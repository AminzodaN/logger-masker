package masklog

import (
	"strings"
	"sync"
)

// redacted — форма полностью скрытого значения (drop-ключи, составные значения
// под `log:"mask"`, значения без известной частичной маски).
const redacted = "***"

// DefaultMaxBodyBytes — потолок размера тела по умолчанию (256 КБ). Тело больше
// не разбирается структурно: применяется текстовая маска и обрезка.
const DefaultMaxBodyBytes = 256 * 1024

// Rules задаёт правила маскирования сырых тел (JSON/XML/текст) по именам ключей.
// Наборы конфигурируемы через SetRules; DefaultRules возвращает безопасный набор
// по умолчанию. Значение в картах — тип маски ("pan"/"phone"/"email"/"name"/
// "last4" или "" => полностью скрыть "***").
type Rules struct {
	// Keys — точные нормализованные имена ключей -> тип маски.
	Keys map[string]string
	// Suffixes — суффиксы нормализованных имён -> тип маски (ключ, кончающийся
	// суффиксом, маскируется). Проверяются после точных имён.
	Suffixes map[string]string
	// Allow — нормализованные имена-исключения, которые НЕ должны попадать под
	// суффиксное правило (напр. span, japan при включённом суффиксе pan).
	Allow map[string]struct{}
	// MaxBodyBytes — потолок размера тела; 0 => DefaultMaxBodyBytes.
	MaxBodyBytes int
	// Phone7 включает распознавание телефонов +7XXXXXXXXXX в значениях.
	Phone7 bool
}

// DefaultRules возвращает копию набора правил по умолчанию (можно свободно
// править и передавать в SetRules).
func DefaultRules() Rules {
	drop := []string{
		"cvv", "cvv2", "cvc", "cvc2", "pin", "pinblock", "otp", "track2",
		"password", "passwd", "secret", "token", "accesstoken", "refreshtoken",
		"apikey", "authorization", "cookie",
	}
	keys := map[string]string{}
	for _, k := range drop {
		keys[k] = "" // "" => "***"
	}
	for _, k := range []string{"pan", "cardnumber", "card", "primaryaccountnumber"} {
		keys[k] = MaskPAN
	}
	for _, k := range []string{"phone", "mobile", "msisdn"} {
		keys[k] = MaskPhone
	}
	for _, k := range []string{"email", "mail"} {
		keys[k] = MaskEmail
	}
	for _, k := range []string{"firstname", "lastname", "middlename", "fullname", "fio", "holder", "cardholder", "holdername"} {
		keys[k] = MaskName
	}
	for _, k := range []string{"passport", "passportnumber", "inn", "account", "accountnumber", "iban"} {
		keys[k] = MaskLast4
	}
	return Rules{
		Keys: keys,
		Suffixes: map[string]string{
			"cardnumber": MaskPAN,
			"phone":      MaskPhone,
			"email":      MaskEmail,
		},
		Allow:        map[string]struct{}{},
		MaxBodyBytes: DefaultMaxBodyBytes,
	}
}

var (
	rulesMu      sync.RWMutex
	currentRules = DefaultRules()
)

// SetRules заменяет активный набор правил маскирования тел. Потокобезопасно;
// обычно вызывается один раз на старте сервиса.
func SetRules(r Rules) {
	if r.MaxBodyBytes <= 0 {
		r.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if r.Keys == nil {
		r.Keys = map[string]string{}
	}
	if r.Suffixes == nil {
		r.Suffixes = map[string]string{}
	}
	if r.Allow == nil {
		r.Allow = map[string]struct{}{}
	}
	rulesMu.Lock()
	currentRules = r
	rulesMu.Unlock()
}

// activeRules возвращает текущий набор правил (копия указателя под RLock-снимком).
func activeRules() Rules {
	rulesMu.RLock()
	defer rulesMu.RUnlock()
	return currentRules
}

// normKey нормализует имя ключа: нижний регистр + удаление _, -, . — чтобы
// phone_number, phoneNumber и PHONE-NUMBER совпадали. Быстрый путь без
// аллокаций: если имя уже нормализовано, возвращается как есть (важно для
// 0-alloc в replaceAttr, где normKey зовётся на каждый атрибут).
func normKey(s string) string {
	need := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || c == '-' || c == '.' || (c >= 'A' && c <= 'Z') {
			need = true
			break
		}
	}
	if !need {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '_', '-', '.':
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r = r - 'A' + 'a'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// keyMask возвращает тип маски для ключа (и true, если ключ чувствительный):
// точное имя, затем суффикс (если имя не в allow-list). Используется движком тел.
func (r Rules) keyMask(key string) (string, bool) {
	nk := normKey(key)
	if mt, ok := r.Keys[nk]; ok {
		return mt, true
	}
	if _, blocked := r.Allow[nk]; !blocked {
		for suf, mt := range r.Suffixes {
			if len(nk) > len(suf) && strings.HasSuffix(nk, suf) {
				return mt, true
			}
		}
	}
	return "", false
}

// keyIsSensitive сообщает, помечен ли ключ как чувствительный в активных
// правилах. Используется стоп-списком в map-ветках и в replaceAttr.
func keyIsSensitive(key string) bool {
	_, ok := activeRules().keyMask(key)
	return ok
}
