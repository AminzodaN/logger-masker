package masklog

import (
	"reflect"
	"strings"
)

// Redacted — публичная форма полностью скрытого значения ("***"). Пригождается
// страховке в logger.replaceAttr.
const Redacted = redacted

// Sensitive сообщает, помечен ли ключ как чувствительный в активных правилах.
// Используется страховкой на выходе (стоп-список ключей на любом уровне групп).
func Sensitive(key string) bool { return keyIsSensitive(key) }

// MaskValue применяет к строке value маску, назначенную ключу key активными
// правилами. Если ключ не чувствителен — возвращает value без изменений.
func MaskValue(key, value string) string {
	if mt, ok := activeRules().keyMask(key); ok {
		return mask(mt, value)
	}
	return value
}

// MaskString маскирует произвольную строку (msg, текст ошибки, свободный текст):
// номера карт (Луна), телефоны, email, пары ключ=значение по стоп-списку.
// Быстрый путь: строка без '@' и без цифровой последовательности ≥ 13
// возвращается без regex и без аллокаций.
func MaskString(s string) string {
	if !stringNeedsScan(s) {
		return s
	}
	return textMaskFallback(s)
}

// stringNeedsScan — порог быстрого пути для MaskString (страховка): '@' или
// серия из ≥13 цифр. Телефоны (<13 цифр) ловятся на структурном/body-уровне.
func stringNeedsScan(s string) bool {
	if strings.IndexByte(s, '@') >= 0 {
		return true
	}
	run := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			run++
			if run >= 13 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// ShouldWrap сообщает, нужно ли оборачивать голое значение через V перед
// записью: структура, указатель на структуру, map или срез/массив структур
// (их json.Marshal утёк бы целиком). Скаляры и строки оборачивать не нужно.
func ShouldWrap(v any) bool {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Struct, reflect.Map:
		return true
	case reflect.Slice, reflect.Array:
		return elemIsStruct(rv.Type())
	}
	return false
}
