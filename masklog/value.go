package masklog

import (
	"encoding"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// maxDepth ограничивает глубину рекурсии, чтобы глубокая или
// самоссылающаяся структура не переполнила стек прямо на пути записи лога.
const maxDepth = 10

// errSentinel подставляется (fail-closed) при любой пойманной панике во время
// маскирования — так сырые данные не утекут в лог из-за бага в обходе.
const errSentinel = "[masklog:error]"

// visited отслеживает уже пройденные указатели для разрыва циклов. Карта
// создаётся лениво: на плоской структуре (без указателей/срезов/map) не
// аллоцируется вовсе — это убирает одну аллокацию на каждый вызов лога.
type visited struct{ seen map[uintptr]struct{} }

// enter помечает ptr посещённым и возвращает true; false — если ptr уже
// встречался (цикл). При true нужен парный leave (обычно через defer).
func (v *visited) enter(ptr uintptr) bool {
	if _, ok := v.seen[ptr]; ok {
		return false
	}
	if v.seen == nil {
		v.seen = make(map[uintptr]struct{})
	}
	v.seen[ptr] = struct{}{}
	return true
}

// leave снимает отметку с ptr после выхода из его поддерева.
func (v *visited) leave(ptr uintptr) { delete(v.seen, ptr) }

// Value — безопасная обёртка над произвольным значением. Реализует:
//
//   - slog.LogValuer   — slog.Any("resp", masklog.V(r)) даёт замаскированную группу;
//   - fmt.Formatter    — Infof("%+v", masklog.V(r)) печатает замаскированную строку;
//   - fmt.Stringer     — то же для %v / обычной печати.
//
// Во всех случаях применяются одни и те же правила тегов `log`/`mask`.
type Value struct{ v any }

// V оборачивает v для безопасного логирования. Это единственная точка входа:
// её передают в slog или в format-строку вместо самого значения.
func V(v any) Value { return Value{v: v} }

// LogValue реализует slog.LogValuer: именно через него slog узнаёт, как
// безопасно вывести значение. Структура превращается в группу замаскированных
// атрибутов, любой другой тип — в своё (рекурсивно замаскированное) значение.
//
// Обход на slog-пути несёт depth и seen так же, как formatValue на fmt-пути:
// вложенные группы строятся здесь же, а не через ленивый slog.Any(name, V(...)) —
// иначе на циклической структуре slog раскрывал бы обёртку бесконечно и процесс
// падал бы с stack overflow (recover такое не ловит).
func (val Value) LogValue() (out slog.Value) {
	defer func() {
		if r := recover(); r != nil {
			out = slog.StringValue(errSentinel)
		}
	}()
	return slogValue(reflect.ValueOf(val.v), 0, &visited{})
}

// slogValue — рекурсивное ядро slog-пути: превращает любой reflect.Value в
// slog.Value, применяя теги к структурам и разрывая циклы. Это slog-близнец
// formatValue; seen хранит посещённые указатели, depth ограничивает глубину.
// Превышение глубины => "...", повтор указателя => "<cycle>" (fail-closed).
func slogValue(rv reflect.Value, depth int, vis *visited) slog.Value {
	if depth > maxDepth {
		return slog.StringValue("...")
	}
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if rv.IsNil() {
			return slog.AnyValue(nil)
		}
		if rv.Kind() == reflect.Pointer {
			ptr := rv.Pointer()
			if !vis.enter(ptr) {
				return slog.StringValue("<cycle>")
			}
			defer vis.leave(ptr)
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return slog.AnyValue(nil)
	}
	switch rv.Kind() {
	case reflect.Struct:
		// Тип без своих экспортируемых полей (time.Time, uuid, decimal, …)
		// иначе дал бы пустую группу — берём его текстовую форму.
		if len(describe(rv.Type()).fields) == 0 {
			if s, ok := stringish(rv); ok {
				return slog.StringValue(s)
			}
		}
		return slog.GroupValue(structAttrs(rv, depth, vis)...)
	case reflect.String:
		return slog.StringValue(rv.String())
	case reflect.Bool:
		return slog.BoolValue(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return slog.Int64Value(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return slog.Uint64Value(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return slog.Float64Value(rv.Float())
	case reflect.Slice, reflect.Array:
		// []byte — печатаем содержимым-строкой (как строковое поле), а не
		// массивом чисел; при наличии тега маскирование уже прошло выше.
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			return slog.StringValue(string(rv.Bytes()))
		}
		// Срез/массив структур выводим JSON-массивом замаскированных объектов,
		// чтобы по элементам можно было искать в ELK/Loki, а не одной строкой.
		if elemIsStruct(rv.Type()) {
			if rv.Kind() == reflect.Slice && !rv.IsNil() {
				ptr := rv.Pointer()
				if !vis.enter(ptr) {
					return slog.StringValue("[...]")
				}
				defer vis.leave(ptr)
			}
			vals := make([]slog.Value, 0, rv.Len())
			for i := 0; i < rv.Len(); i++ {
				vals = append(vals, slogValue(rv.Index(i), depth+1, vis))
			}
			return slog.AnyValue(slogJSONArray(vals))
		}
		if rv.CanInterface() {
			return slog.StringValue(Format(rv.Interface()))
		}
		return slog.StringValue("<unexported>")
	case reflect.Map:
		// map => группа: ключи из стоп-списка маскируются (***), значения-
		// структуры рекурсят со своими тегами. Так из map не утекает поддерево.
		return slog.GroupValue(mapAttrs(rv, depth, vis)...)
	default:
		if rv.CanInterface() {
			return slog.AnyValue(rv.Interface())
		}
		return slog.StringValue("<unexported>")
	}
}

// structAttrs строит набор замаскированных slog-атрибутов по одной структуре —
// это slog-ветка применения тегов (drop/mask/как есть). Несёт depth и seen,
// чтобы вложенные значения строились тем же cycle-safe обходом.
func structAttrs(rv reflect.Value, depth int, vis *visited) []slog.Attr {
	d := describe(rv.Type())
	attrs := make([]slog.Attr, 0, len(d.fields))
	for _, fr := range d.fields {
		fv := rv.Field(fr.index)
		switch fr.act {
		case actDrop:
			continue
		case actMask:
			attrs = append(attrs, slog.String(fr.name, maskField(fr, fv)))
		case actJSON:
			attrs = append(attrs, slog.Attr{Key: fr.name, Value: JSON(fieldBytes(fv)).LogValue()})
		case actXML:
			attrs = append(attrs, slog.Attr{Key: fr.name, Value: XML(fieldBytes(fv)).LogValue()})
		case actText:
			attrs = append(attrs, slog.String(fr.name, maskFreeText(leafString(fv))))
		default: // actNone
			attrs = append(attrs, slog.Attr{Key: fr.name, Value: slogValue(fv, depth+1, vis)})
		}
	}
	return attrs
}

// maskField маскирует тегированное `log:"mask"` поле. Составное значение
// (структура/срез/массив/map) нельзя осмысленно замаскировать частично —
// скрываем целиком "***" (иначе mask:"last4" над Passport дал бы "...567}").
func maskField(fr fieldRule, fv reflect.Value) string {
	rv := deref(fv)
	if rv.IsValid() {
		switch rv.Kind() {
		case reflect.Struct, reflect.Slice, reflect.Array, reflect.Map:
			// []byte — всё же скаляр-строка для масок; остальное скрываем.
			if !(rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8) {
				return redacted
			}
		}
	}
	return mask(fr.maskType, leafString(fv))
}

// elemIsStruct сообщает, является ли элемент среза/массива структурой (с учётом
// указателей) — такие срезы выводятся JSON-массивом замаскированных объектов.
func elemIsStruct(t reflect.Type) bool {
	et := t.Elem()
	for et.Kind() == reflect.Pointer {
		et = et.Elem()
	}
	return et.Kind() == reflect.Struct
}

// mapAttrs превращает map в группу slog-атрибутов: ключи из стоп-списка
// маскируются (***), значения-структуры рекурсят со своими тегами.
func mapAttrs(rv reflect.Value, depth int, vis *visited) []slog.Attr {
	if rv.IsNil() {
		return nil
	}
	ptr := rv.Pointer()
	if !vis.enter(ptr) {
		return []slog.Attr{slog.String("...", "<cycle>")}
	}
	defer vis.leave(ptr)

	keys := rv.MapKeys()
	sort.Slice(keys, func(i, j int) bool {
		return fmt.Sprint(iface(keys[i])) < fmt.Sprint(iface(keys[j]))
	})
	attrs := make([]slog.Attr, 0, len(keys))
	for _, k := range keys {
		name := fmt.Sprint(iface(k))
		if keyIsSensitive(name) {
			attrs = append(attrs, slog.String(name, redacted))
			continue
		}
		attrs = append(attrs, slog.Attr{Key: name, Value: slogValue(rv.MapIndex(k), depth+1, vis)})
	}
	return attrs
}

// slogJSONArray — срез slog.Value, сериализуемый в JSON-массив (и читаемую
// строку для TextHandler). Нужен, т.к. у slog нет нативного «значения-массива».
type slogJSONArray []slog.Value

// MarshalJSON пишет элементы массива так же, как их отрендерил бы JSONHandler.
func (a slogJSONArray) MarshalJSON() ([]byte, error) {
	buf := make([]byte, 0, 64)
	buf = append(buf, '[')
	for i, v := range a {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendSlogValueJSON(buf, v)
	}
	buf = append(buf, ']')
	return buf, nil
}

// MarshalText даёт читаемую форму (тот же JSON) для TextHandler.
func (a slogJSONArray) MarshalText() ([]byte, error) { return a.MarshalJSON() }

// appendSlogValueJSON дописывает slog.Value в dst как JSON (группы -> объекты,
// скаляры -> соответствующие литералы), повторяя форму JSONHandler.
func appendSlogValueJSON(dst []byte, v slog.Value) []byte {
	switch v.Kind() {
	case slog.KindGroup:
		dst = append(dst, '{')
		for i, a := range v.Group() {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, a.Key)
			dst = append(dst, ':')
			dst = appendSlogValueJSON(dst, a.Value)
		}
		dst = append(dst, '}')
	case slog.KindString:
		dst = appendJSONString(dst, v.String())
	case slog.KindInt64:
		dst = strconv.AppendInt(dst, v.Int64(), 10)
	case slog.KindUint64:
		dst = strconv.AppendUint(dst, v.Uint64(), 10)
	case slog.KindFloat64:
		dst = strconv.AppendFloat(dst, v.Float64(), 'g', -1, 64)
	case slog.KindBool:
		dst = strconv.AppendBool(dst, v.Bool())
	case slog.KindAny:
		a := v.Any()
		if a == nil {
			dst = append(dst, "null"...)
		} else if raw, err := json.Marshal(a); err == nil {
			dst = append(dst, raw...)
		} else {
			dst = appendJSONString(dst, errSentinel)
		}
	default:
		dst = appendJSONString(dst, v.String())
	}
	return dst
}

// fieldBytes извлекает сырые байты поля-тела: []byte / json.RawMessage как есть,
// строку — как байты; иначе откатывается к текстовой форме (fail-safe).
func fieldBytes(fv reflect.Value) []byte {
	rv := deref(fv)
	if !rv.IsValid() {
		return nil
	}
	switch rv.Kind() {
	case reflect.String:
		return []byte(rv.String())
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return rv.Bytes()
		}
	}
	return []byte(leafString(rv))
}

// Format возвращает безопасную строку вида %+v для v, применяя теги `log` ко
// всем встреченным структурам. Нужна для fmt-пути (%v/%+v) и для методов
// String()/GoString() у доменных типов. Рекурсивна, nil-safe, cycle-safe,
// ограничена по глубине; любая паника превращается в errSentinel.
func Format(v any) (out string) {
	defer func() {
		if r := recover(); r != nil {
			out = errSentinel
		}
	}()
	var b strings.Builder
	formatValue(&b, reflect.ValueOf(v), 0, &visited{})
	return b.String()
}

// String реализует fmt.Stringer — даёт замаскированный вывод при %v/%s.
func (val Value) String() string { return Format(val.v) }

// GoString реализует fmt.GoStringer — замаскированный вывод при %#v.
func (val Value) GoString() string { return Format(val.v) }

// Format реализует fmt.Formatter для любого глагола, всегда отдавая безопасную
// форму — чтобы никакой verb (%v/%+v/%#v/%s) не смог распечатать сырые данные.
func (val Value) Format(f fmt.State, _ rune) {
	io.WriteString(f, Format(val.v))
}

// formatValue — рекурсивное ядро Format: пишет безопасную строковую форму любого
// reflect.Value (структура, указатель, срез, map, скаляр) в b. Существует, чтобы
// fmt-путь применял те же правила тегов, что и slog-путь; seen хранит уже
// посещённые указатели (разрыв циклов), depth ограничивает глубину.
func formatValue(b *strings.Builder, rv reflect.Value, depth int, vis *visited) {
	if depth > maxDepth {
		b.WriteString("...")
		return
	}
	if !rv.IsValid() {
		b.WriteString("<nil>")
		return
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			b.WriteString("<nil>")
			return
		}
		if rv.Kind() == reflect.Pointer {
			ptr := rv.Pointer()
			if !vis.enter(ptr) {
				b.WriteString("...<cycle>")
				return
			}
			defer vis.leave(ptr)
		}
		formatValue(b, rv.Elem(), depth, vis)
	case reflect.Struct:
		// Зеркало slog-ветки: тип без экспортируемых полей (time.Time и т.п.)
		// печатаем его текстовой формой, а не как пустую структуру {}.
		if len(describe(rv.Type()).fields) == 0 {
			if s, ok := stringish(rv); ok {
				b.WriteString(s)
				return
			}
		}
		formatStruct(b, rv, depth, vis)
	case reflect.Slice, reflect.Array:
		// []byte — содержимым-строкой, зеркало slog-ветки (не массив чисел).
		if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
			b.WriteString(string(rv.Bytes()))
			return
		}
		if rv.Kind() == reflect.Slice && !rv.IsNil() {
			ptr := rv.Pointer()
			if !vis.enter(ptr) {
				b.WriteString("[...]")
				return
			}
			defer vis.leave(ptr)
		}
		b.WriteByte('[')
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				b.WriteByte(' ')
			}
			formatValue(b, rv.Index(i), depth+1, vis)
		}
		b.WriteByte(']')
	case reflect.Map:
		if rv.IsNil() {
			b.WriteString("map[]")
			return
		}
		ptr := rv.Pointer()
		if !vis.enter(ptr) {
			b.WriteString("map[...]")
			return
		}
		defer vis.leave(ptr)
		b.WriteString("map[")
		mapKeys := rv.MapKeys()
		sort.Slice(mapKeys, func(i, j int) bool {
			return fmt.Sprint(iface(mapKeys[i])) < fmt.Sprint(iface(mapKeys[j]))
		})
		for i, k := range mapKeys {
			if i > 0 {
				b.WriteByte(' ')
			}
			formatValue(b, k, depth+1, vis)
			b.WriteByte(':')
			// Значение под чувствительным ключом скрываем целиком (стоп-список).
			if keyIsSensitive(fmt.Sprint(iface(k))) {
				b.WriteString(redacted)
			} else {
				formatValue(b, rv.MapIndex(k), depth+1, vis)
			}
		}
		b.WriteByte(']')
	case reflect.String:
		b.WriteString(rv.String())
	default:
		fmt.Fprintf(b, "%v", iface(rv))
	}
}

// formatStruct пишет одну структуру как {name:val ...}, применяя теги полей:
// mask — маскирует, drop — пропускает, остальное рекурсит через formatValue.
// Это строковый близнец structAttrs (та же логика для slog); вынесены отдельно,
// потому что форматы вывода разные.
func formatStruct(b *strings.Builder, rv reflect.Value, depth int, vis *visited) {
	d := describe(rv.Type())
	b.WriteByte('{')
	first := true
	emit := func(name, val string) {
		if !first {
			b.WriteByte(' ')
		}
		first = false
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(val)
	}
	for _, fr := range d.fields {
		fv := rv.Field(fr.index)
		switch fr.act {
		case actDrop:
			continue
		case actMask:
			emit(fr.name, maskField(fr, fv))
		case actJSON:
			emit(fr.name, JSON(fieldBytes(fv)).String())
		case actXML:
			emit(fr.name, XML(fieldBytes(fv)).String())
		case actText:
			emit(fr.name, maskFreeText(leafString(fv)))
		default: // actNone
			if !first {
				b.WriteByte(' ')
			}
			first = false
			b.WriteString(fr.name)
			b.WriteByte(':')
			formatValue(b, fv, depth+1, vis)
		}
	}
	b.WriteByte('}')
}

// deref разыменовывает указатели и интерфейсы до базового значения; для
// nil-цепочки возвращает невалидный reflect.Value. Общий помощник для обеих
// веток, чтобы не дублировать разбор указателей.
func deref(rv reflect.Value) reflect.Value {
	for rv.IsValid() && (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface) {
		if rv.IsNil() {
			return reflect.Value{}
		}
		rv = rv.Elem()
	}
	return rv
}

// leafString сводит значение к простой строке, над которой работает функция
// маскирования: разыменовывает указатели/интерфейсы, для nil возвращает "",
// а нестроковые листья печатает через fmt.Sprint (чтобы числовое поле тоже
// можно было замаскировать).
func leafString(rv reflect.Value) string {
	rv = deref(rv)
	if !rv.IsValid() {
		return ""
	}
	if rv.Kind() == reflect.String {
		return rv.String()
	}
	// []byte — содержимое как строка (иначе fmt.Sprint дал бы массив чисел
	// "[52 49 …]": маска сработала бы по мусору, а байты утекли бы кодами).
	if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() == reflect.Uint8 {
		return string(rv.Bytes())
	}
	if rv.CanInterface() {
		return fmt.Sprint(rv.Interface())
	}
	return ""
}

// stringish возвращает текстовую форму для типов, чьё значение живёт в методах,
// а не в экспортируемых полях (time.Time, uuid.UUID, decimal.Decimal и т.п.).
// Без этого такой тип схлопнулся бы в пустую группу (slog выкинул бы ключ, fmt
// напечатал бы {}), т.к. describe не видит у него полей для вывода. Предпочитает
// encoding.TextMarshaler (у time.Time это RFC3339), затем fmt.Stringer; ok=false,
// если тип не реализует ни того, ни другого. Ошибку MarshalText глотаем и
// откатываемся к Stringer — fail-closed, сырьё в лог не попадает.
func stringish(rv reflect.Value) (string, bool) {
	if !rv.CanInterface() {
		return "", false
	}
	v := rv.Interface()
	if tm, ok := v.(encoding.TextMarshaler); ok {
		if b, err := tm.MarshalText(); err == nil {
			return string(b), true
		}
	}
	if s, ok := v.(fmt.Stringer); ok {
		return s.String(), true
	}
	return "", false
}

// iface безопасно достаёт interface{} из reflect.Value для печати, подставляя
// заглушку для неэкспортируемых полей, которые нельзя прочитать.
func iface(rv reflect.Value) any {
	if rv.CanInterface() {
		return rv.Interface()
	}
	return "<unexported>"
}
