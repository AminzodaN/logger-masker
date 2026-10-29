package masklog

import (
	"reflect"
	"strings"
	"sync"
)

// action — что делать с полем структуры в логах, выводится из его тега `log`.
type action uint8

const (
	actNone action = iota // выводить как есть
	actDrop               // никогда не выводить
	actMask               // необратимо замаскировать
	actJSON               // тело-JSON: разобрать и замаскировать по ключам/значениям
	actXML                // тело-XML/SOAP: то же по локальным именам
	actText               // произвольный текст: маскировать только по значениям
)

// fieldRule описывает, как логировать одно поле структуры.
type fieldRule struct {
	index    int
	name     string // имя в выводе (json-тег или snake_case)
	act      action
	maskType string // pan|phone|email|name; пусто => дефолт "***"
}

// structDesc — закэшированное описание логирования для типа структуры.
type structDesc struct {
	fields []fieldRule
}

var descCache sync.Map // reflect.Type -> *structDesc

// describe возвращает (и запоминает) правила логирования для типа структуры.
// Кэш по типу нужен, чтобы разбирать теги один раз на тип, а не на каждый лог —
// это главная оптимизация рефлексии на горячем пути.
func describe(t reflect.Type) *structDesc {
	if cached, ok := descCache.Load(t); ok {
		return cached.(*structDesc)
	}
	d := &structDesc{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous { // неэкспортируемое, не встроенное
			continue
		}
		fr := fieldRule{index: i, name: outputName(f)}
		switch logTag := f.Tag.Get("log"); logTag {
		case "":
			fr.act = actNone
		case "drop":
			fr.act = actDrop
		case "mask":
			fr.act = actMask
			fr.maskType = f.Tag.Get("mask")
		case "json":
			fr.act = actJSON
		case "xml":
			fr.act = actXML
		case "text":
			fr.act = actText
		default:
			// Неизвестный тег: fail-closed, маскируем дефолтным правилом.
			fr.act = actMask
		}
		d.fields = append(d.fields, fr)
	}
	descCache.Store(t, d)
	return d
}

// outputName выбирает имя поля в логе: имя из json-тега, если есть, иначе
// snake_case от Go-имени поля. Так ключи логов совпадают с JSON API.
func outputName(f reflect.StructField) string {
	if jt := f.Tag.Get("json"); jt != "" {
		if name := strings.Split(jt, ",")[0]; name != "" && name != "-" {
			return name
		}
	}
	return snakeCase(f.Name)
}

// snakeCase переводит Go-имя поля (CamelCase) в snake_case — используется как
// имя поля в логе, когда у поля нет json-тега, чтобы ключи были единообразны.
func snakeCase(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
