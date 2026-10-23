package masklog

import (
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
)

// maxDepth ограничивает глубину рекурсии, чтобы глубокая или
// самоссылающаяся структура не переполнила стек прямо на пути записи лога.
const maxDepth = 10

// errSentinel подставляется (fail-closed) при любой пойманной панике во время
// маскирования — так сырые данные не утекут в лог из-за бага в обходе.
const errSentinel = "[masklog:error]"

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
func (val Value) LogValue() (out slog.Value) {
	defer func() {
		if r := recover(); r != nil {
			out = slog.StringValue(errSentinel)
		}
	}()
	rv := deref(reflect.ValueOf(val.v))
	if !rv.IsValid() {
		return slog.AnyValue(nil)
	}
	if rv.Kind() == reflect.Struct {
		return slog.GroupValue(structAttrs(rv)...)
	}
	// Верхний уровень не структура: отдаём безопасную строковую форму.
	return slog.StringValue(Format(val.v))
}

// structAttrs строит набор замаскированных slog-атрибутов по одной структуре —
// это slog-ветка применения тегов (drop/mask/как есть). Вызывается из LogValue.
func structAttrs(rv reflect.Value) []slog.Attr {
	d := describe(rv.Type())
	attrs := make([]slog.Attr, 0, len(d.fields))
	for _, fr := range d.fields {
		fv := rv.Field(fr.index)
		switch fr.act {
		case actDrop:
			continue
		case actMask:
			attrs = append(attrs, slog.String(fr.name, mask(fr.maskType, leafString(fv))))
		default: // actNone
			attrs = append(attrs, naturalAttr(fr.name, fv))
		}
	}
	return attrs
}

// naturalAttr добавляет нетегированное поле типизированным slog-значением.
// Нужна, чтобы сохранить тип в JSON и при этом не потерять маскирование внутри:
// вложенные структуры рекурсят через V (их теги тоже применятся); коллекции
// рендерятся безопасной строкой; nil-указатель даёт nil-атрибут.
func naturalAttr(name string, fv reflect.Value) slog.Attr {
	rv := deref(fv)
	if !rv.IsValid() {
		return slog.Any(name, nil)
	}
	switch rv.Kind() {
	case reflect.Struct:
		if rv.CanInterface() {
			return slog.Any(name, V(rv.Interface()))
		}
	case reflect.String:
		return slog.String(name, rv.String())
	case reflect.Bool:
		return slog.Bool(name, rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return slog.Int64(name, rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return slog.Uint64(name, rv.Uint())
	case reflect.Float32, reflect.Float64:
		return slog.Float64(name, rv.Float())
	case reflect.Slice, reflect.Array, reflect.Map:
		if rv.CanInterface() {
			return slog.String(name, Format(rv.Interface()))
		}
	}
	if rv.CanInterface() {
		return slog.Any(name, rv.Interface())
	}
	return slog.String(name, "<unexported>")
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
	formatValue(&b, reflect.ValueOf(v), 0, map[uintptr]struct{}{})
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
func formatValue(b *strings.Builder, rv reflect.Value, depth int, seen map[uintptr]struct{}) {
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
			if _, ok := seen[ptr]; ok {
				b.WriteString("...<cycle>")
				return
			}
			seen[ptr] = struct{}{}
			defer delete(seen, ptr)
		}
		formatValue(b, rv.Elem(), depth, seen)
	case reflect.Struct:
		formatStruct(b, rv, depth, seen)
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && !rv.IsNil() {
			ptr := rv.Pointer()
			if _, ok := seen[ptr]; ok {
				b.WriteString("[...]")
				return
			}
			seen[ptr] = struct{}{}
			defer delete(seen, ptr)
		}
		b.WriteByte('[')
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				b.WriteByte(' ')
			}
			formatValue(b, rv.Index(i), depth+1, seen)
		}
		b.WriteByte(']')
	case reflect.Map:
		if rv.IsNil() {
			b.WriteString("map[]")
			return
		}
		ptr := rv.Pointer()
		if _, ok := seen[ptr]; ok {
			b.WriteString("map[...]")
			return
		}
		seen[ptr] = struct{}{}
		defer delete(seen, ptr)
		b.WriteString("map[")
		mapKeys := rv.MapKeys()
		sort.Slice(mapKeys, func(i, j int) bool {
			return fmt.Sprint(iface(mapKeys[i])) < fmt.Sprint(iface(mapKeys[j]))
		})
		for i, k := range mapKeys {
			if i > 0 {
				b.WriteByte(' ')
			}
			formatValue(b, k, depth+1, seen)
			b.WriteByte(':')
			formatValue(b, rv.MapIndex(k), depth+1, seen)
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
func formatStruct(b *strings.Builder, rv reflect.Value, depth int, seen map[uintptr]struct{}) {
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
			emit(fr.name, mask(fr.maskType, leafString(fv)))
		default: // actNone
			if !first {
				b.WriteByte(' ')
			}
			first = false
			b.WriteString(fr.name)
			b.WriteByte(':')
			formatValue(b, fv, depth+1, seen)
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
	if rv.CanInterface() {
		return fmt.Sprint(rv.Interface())
	}
	return ""
}

// iface безопасно достаёт interface{} из reflect.Value для печати, подставляя
// заглушку для неэкспортируемых полей, которые нельзя прочитать.
func iface(rv reflect.Value) any {
	if rv.CanInterface() {
		return rv.Interface()
	}
	return "<unexported>"
}
