package masklog

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// Этот файл реализует маскирование сырых тел (JSON/XML/текст). Движок JSON —
// потоковый на json.Decoder.Token() с UseNumber(): порядок полей сохраняется,
// числа не теряют точность, промежуточный map[string]any не строится. Правила
// (имена ключей, маски, allow-list) берутся из активного набора Rules.
//
// Принцип fail-closed: при любой ошибке разбора или паникe тело не выводится как
// есть — применяется текстовая маска (Луна + телефоны + email + ключ=значение по
// стоп-списку), а сама строка лога остаётся валидным JSON (тело кладётся строкой).

// --- обёртки для логирования тела без структуры -----------------------------

// JSON оборачивает JSON-тело (например, ответ партнёра) для безопасного
// логирования: в JSONHandler — вложенный замаскированный объект, в TextHandler
// (Env: local) и в fmt — читаемая строка. Маскирование ленивое (внутри
// MarshalJSON/LogValue), выполняется только если уровень лога включён.
type JSONValue struct{ raw []byte }

// JSON создаёт обёртку над JSON-телом b.
func JSON(b []byte) JSONValue { return JSONValue{raw: b} }

// LogValue реализует slog.LogValuer: отдаёт тело типом с MarshalJSON/MarshalText,
// чтобы JSONHandler вставил вложенный объект, а TextHandler — строку.
func (j JSONValue) LogValue() slog.Value { return slog.AnyValue(jsonBody(j.raw)) }

// String реализует fmt.Stringer — читаемая замаскированная форма тела.
func (j JSONValue) String() string { return string(jsonBodyText(j.raw)) }

// GoString реализует fmt.GoStringer.
func (j JSONValue) GoString() string { return j.String() }

// Format реализует fmt.Formatter: любой verb даёт безопасную форму.
func (j JSONValue) Format(f fmt.State, _ rune) { io.WriteString(f, j.String()) }

// jsonBody — тип тела с ленивым маскированием: MarshalJSON даёт вложенный объект
// (или JSON-строку при невалидном теле), MarshalText — читаемую строку.
type jsonBody []byte

// MarshalJSON маскирует тело и возвращает его как вложенный JSON либо (для
// невалидного/обрезанного/большого тела) как JSON-строку с текстовой маской.
func (j jsonBody) MarshalJSON() ([]byte, error) { return maskJSONBody(j), nil }

// MarshalText даёт читаемую замаскированную строку для TextHandler.
func (j jsonBody) MarshalText() ([]byte, error) { return jsonBodyText(j), nil }

// XMLValue оборачивает XML/SOAP-тело. Маскирование по локальному имени элемента
// и атрибута; результат всегда кладётся JSON-строкой (XML — не JSON).
type XMLValue struct{ raw []byte }

// XML создаёт обёртку над XML/SOAP-телом b.
func XML(b []byte) XMLValue { return XMLValue{raw: b} }

// LogValue реализует slog.LogValuer.
func (x XMLValue) LogValue() slog.Value { return slog.StringValue(xmlMaskString(x.raw)) }

// String реализует fmt.Stringer.
func (x XMLValue) String() string { return xmlMaskString(x.raw) }

// GoString реализует fmt.GoStringer.
func (x XMLValue) GoString() string { return x.String() }

// Format реализует fmt.Formatter.
func (x XMLValue) Format(f fmt.State, _ rune) { io.WriteString(f, x.String()) }

// TextValue оборачивает произвольный текст — маскирование только по значениям
// (карты/телефоны/email), без разбора структуры.
type TextValue struct{ s string }

// Text создаёт обёртку над произвольным текстом s.
func Text(s string) TextValue { return TextValue{s: s} }

// LogValue реализует slog.LogValuer.
func (t TextValue) LogValue() slog.Value { return slog.StringValue(maskFreeText(t.s)) }

// String реализует fmt.Stringer.
func (t TextValue) String() string { return maskFreeText(t.s) }

// GoString реализует fmt.GoStringer.
func (t TextValue) GoString() string { return t.String() }

// Format реализует fmt.Formatter.
func (t TextValue) Format(f fmt.State, _ rune) { io.WriteString(f, t.String()) }

// --- пул буферов -------------------------------------------------------------

var bufPool = sync.Pool{New: func() any { b := make([]byte, 0, 1024); return &b }}

func getBuf() *[]byte { return bufPool.Get().(*[]byte) }

func putBuf(p *[]byte) {
	if cap(*p) <= 1<<20 { // не держим аномально большие буферы
		*p = (*p)[:0]
		bufPool.Put(p)
	}
}

// --- потоковый JSON-движок ----------------------------------------------------

// maskJSONBody — вход для MarshalJSON: вложенный JSON при валидном теле, иначе
// JSON-строка с текстовой маской. Соблюдает MaxBodyBytes.
func maskJSONBody(b []byte) []byte {
	r := activeRules()
	if len(b) > r.MaxBodyBytes {
		return appendJSONString(nil, truncatedTextMask(b, r))
	}
	if out, valid := maskJSONCore(b, r); valid {
		return out
	}
	return appendJSONString(nil, textMaskFallback(string(b)))
}

// jsonBodyText — вход для текстового вывода (TextHandler/String): замаскированный
// JSON как строка либо текстовая маска.
func jsonBodyText(b []byte) []byte {
	r := activeRules()
	if len(b) > r.MaxBodyBytes {
		return []byte(truncatedTextMask(b, r))
	}
	if out, valid := maskJSONCore(b, r); valid {
		return out
	}
	return []byte(textMaskFallback(string(b)))
}

// maskJSONCore потоково разбирает и маскирует валидный JSON. Возвращает
// (маскированные байты, true) либо (nil, false), если тело невалидно/обрезано.
func maskJSONCore(b []byte, r Rules) ([]byte, bool) {
	p := getBuf()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	m := &jmasker{dec: dec, out: *p, rules: r}
	ok := func() (okk bool) {
		defer func() {
			if recover() != nil {
				okk = false
			}
		}()
		if err := m.value(false, ""); err != nil {
			return false
		}
		// После значения верхнего уровня должен быть только EOF.
		if _, err := dec.Token(); err != io.EOF {
			return false
		}
		return true
	}()
	if !ok {
		*p = m.out[:0]
		putBuf(p)
		return nil, false
	}
	res := append([]byte(nil), m.out...)
	*p = m.out[:0]
	putBuf(p)
	return res, true
}

// jmasker несёт состояние потокового обхода: декодер, выходной буфер, правила.
type jmasker struct {
	dec   *json.Decoder
	out   []byte
	rules Rules
}

// value пишет одно JSON-значение (объект/массив/скаляр). forced=true означает,
// что значение под чувствительным ключом — весь его скаляр(ы) маскируются
// маской forceMask. Массив и вложенный объект наследуют forced (ключ родителя).
func (m *jmasker) value(forced bool, forceMask string) error {
	tok, err := m.dec.Token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return m.object(forced, forceMask)
		case '[':
			return m.array(forced, forceMask)
		default:
			return fmt.Errorf("masklog: unexpected delim %q", t)
		}
	case string:
		m.scalarString(t, forced, forceMask)
	case json.Number:
		m.scalarNumber(t, forced, forceMask)
	case bool:
		if forced {
			m.out = appendJSONString(m.out, redacted)
		} else if t {
			m.out = append(m.out, "true"...)
		} else {
			m.out = append(m.out, "false"...)
		}
	case nil:
		m.out = append(m.out, "null"...)
	}
	return nil
}

func (m *jmasker) object(forced bool, forceMask string) error {
	m.out = append(m.out, '{')
	first := true
	for m.dec.More() {
		kt, err := m.dec.Token()
		if err != nil {
			return err
		}
		key, _ := kt.(string)
		if !first {
			m.out = append(m.out, ',')
		}
		first = false
		// Имя ключа тоже может оказаться номером карты/телефоном — маскируем его
		// в выводе (fail-closed), сохраняя исходный key для проверки правил.
		m.out = appendJSONString(m.out, maskFreeText(key))
		m.out = append(m.out, ':')
		cf, cm := forced, forceMask
		if !forced {
			if mt, ok := m.rules.keyMask(key); ok {
				cf, cm = true, mt
			}
		}
		if err := m.value(cf, cm); err != nil {
			return err
		}
	}
	if _, err := m.dec.Token(); err != nil { // consume '}'
		return err
	}
	m.out = append(m.out, '}')
	return nil
}

func (m *jmasker) array(forced bool, forceMask string) error {
	m.out = append(m.out, '[')
	first := true
	for m.dec.More() {
		if !first {
			m.out = append(m.out, ',')
		}
		first = false
		if err := m.value(forced, forceMask); err != nil { // массив наследует ключ родителя
			return err
		}
	}
	if _, err := m.dec.Token(); err != nil { // consume ']'
		return err
	}
	m.out = append(m.out, ']')
	return nil
}

func (m *jmasker) scalarString(s string, forced bool, forceMask string) {
	if forced {
		m.out = appendJSONString(m.out, mask(forceMask, s))
		return
	}
	if mt, ok := detectValueMask(s, m.rules); ok {
		m.out = appendJSONString(m.out, mask(mt, s))
		return
	}
	m.out = appendJSONString(m.out, maskFreeText(s))
}

func (m *jmasker) scalarNumber(n json.Number, forced bool, forceMask string) {
	s := n.String()
	if forced {
		m.out = appendJSONString(m.out, mask(forceMask, s))
		return
	}
	if hasLuhnPAN(s) {
		m.out = appendJSONString(m.out, maskPAN(s))
		return
	}
	m.out = append(m.out, s...) // число как есть — точность сохранена, JSON валиден
}

// --- детекторы значений -------------------------------------------------------

// detectValueMask распознаёт тип значения-строки целиком: email, номер карты
// (Луна), телефон. Возвращает тип маски и true при совпадении.
func detectValueMask(s string, r Rules) (string, bool) {
	if strings.IndexByte(s, '@') > 0 && reEmailFull.MatchString(s) {
		return MaskEmail, true
	}
	if isCardLike(s) {
		return MaskPAN, true
	}
	if isPhoneLike(s, r.Phone7) {
		return MaskPhone, true
	}
	return "", false
}

// isCardLike: всё значение целиком — номер карты (13–19 цифр, допускаются
// пробелы и дефисы, с валидной контрольной суммой Луна).
func isCardLike(s string) bool {
	d := stripSep(s)
	if len(d) < 13 || len(d) > 19 || !allDigits(d) {
		return false
	}
	return luhnValid(d)
}

// hasLuhnPAN сообщает, содержит ли строка цифровую последовательность, внутри
// которой есть окно 13–19 цифр, проходящее Луна. Нужна, чтобы номер карты не
// утёк, будучи вложенным в более длинную последовательность цифр (fail-closed).
func hasLuhnPAN(s string) bool {
	for i := 0; i < len(s); {
		if s[i] < '0' || s[i] > '9' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if runHasLuhnWindow(s[i:j]) {
			return true
		}
		i = j
	}
	return false
}

// runHasLuhnWindow проверяет все окна длиной 13–19 внутри строки цифр.
func runHasLuhnWindow(run string) bool {
	n := len(run)
	if n < 13 {
		return false
	}
	for l := 13; l <= 19 && l <= n; l++ {
		for k := 0; k+l <= n; k++ {
			if luhnValid(run[k : k+l]) {
				return true
			}
		}
	}
	return false
}

// isPhoneLike: телефон Таджикистана +992XXXXXXXXX (и опционально +7XXXXXXXXXX).
func isPhoneLike(s string, phone7 bool) bool {
	d := stripSepKeepPlus(s)
	if strings.HasPrefix(d, "+992") && len(d) == len("+992")+9 && allDigits(d[1:]) {
		return true
	}
	if phone7 && strings.HasPrefix(d, "+7") && len(d) == len("+7")+10 && allDigits(d[1:]) {
		return true
	}
	return false
}

// luhnValid проверяет контрольную сумму Луна для строки цифр.
func luhnValid(digits string) bool {
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		c := digits[i]
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func stripSep(s string) string {
	if !strings.ContainsAny(s, " -") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '-' {
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func stripSepKeepPlus(s string) string { return stripSep(strings.TrimSpace(s)) }

// --- маскирование свободного текста и текстовый fallback ----------------------

var (
	reEmail     = regexp.MustCompile(`[^\s@"'<>,;]+@[^\s@"'<>,;]+\.[^\s@"'<>,;.]+`)
	reEmailFull = regexp.MustCompile(`^[^\s@"'<>,;]+@[^\s@"'<>,;]+\.[^\s@"'<>,;.]+$`)
	rePhone992  = regexp.MustCompile(`\+992\d{9}`)
	rePhone7    = regexp.MustCompile(`\+7\d{10}`)
	reCard      = regexp.MustCompile(`\d(?:[ -]?\d){12,}`)

	reJSONKV = regexp.MustCompile(`"([A-Za-z0-9_.\-]+)"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	reEqKV   = regexp.MustCompile(`([A-Za-z0-9_.\-]+)=([^\s&;,]+)`)
	reXMLKV  = regexp.MustCompile(`<([A-Za-z0-9_.\-:]+)(?:\s[^>]*)?>([^<]*)</[^>]*>`)
)

// needsScan — быстрый фильтр: есть ли смысл запускать regex. true, если есть '@'
// или серия из ≥9 цифр (телефон/карта). Иначе строку можно вернуть как есть.
func needsScan(s string) bool {
	if strings.IndexByte(s, '@') >= 0 {
		return true
	}
	run := 0
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			run++
			if run >= 9 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// maskFreeText маскирует встроенные в произвольный текст email, телефоны и
// номера карт (с проверкой Луна). Быстрый путь без regex, если сканировать нечего.
func maskFreeText(s string) string {
	if !needsScan(s) {
		return s
	}
	s = reEmail.ReplaceAllStringFunc(s, maskEmail)
	s = rePhone992.ReplaceAllStringFunc(s, maskPhone)
	if activeRules().Phone7 {
		s = rePhone7.ReplaceAllStringFunc(s, maskPhone)
	}
	s = reCard.ReplaceAllStringFunc(s, func(m string) string {
		if d := stripSep(m); allDigits(d) && runHasLuhnWindow(d) {
			return maskPAN(m)
		}
		return m
	})
	return s
}

// textMaskFallback — маска для невалидного/обрезанного JSON/XML: сначала
// маскирование по значениям, затем пары ключ-значение по стоп-списку
// (JSON "k":"v", k=v и XML <k>v</k>).
func textMaskFallback(s string) string {
	s = maskFreeText(s)
	s = maskKVPairs(reJSONKV, s)
	s = maskKVPairs(reXMLKV, s)
	s = maskKVPairs(reEqKV, s)
	return s
}

// maskKVPairs заменяет значение (2-я группа) на маску, если ключ (1-я группа)
// чувствителен по активным правилам. Остальные пары не трогает.
func maskKVPairs(re *regexp.Regexp, s string) string {
	locs := re.FindAllStringSubmatchIndex(s, -1)
	if locs == nil {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range locs {
		key := s[m[2]:m[3]]
		vs, ve := m[4], m[5]
		mt, ok := activeRules().keyMask(key)
		if !ok {
			continue
		}
		b.WriteString(s[last:vs])
		b.WriteString(mask(mt, s[vs:ve]))
		last = ve
	}
	b.WriteString(s[last:])
	return b.String()
}

// truncatedTextMask применяет текстовую маску к телу и обрезает его до
// MaxBodyBytes с пометкой о числе отброшенных байт.
func truncatedTextMask(b []byte, r Rules) string {
	head := b
	trunc := 0
	if len(b) > r.MaxBodyBytes {
		head = b[:r.MaxBodyBytes]
		trunc = len(b) - r.MaxBodyBytes
	}
	s := textMaskFallback(string(head))
	if trunc > 0 {
		s += fmt.Sprintf("...[truncated %d bytes]", trunc)
	}
	return s
}

// --- XML-движок ---------------------------------------------------------------

// xmlMaskString маскирует XML/SOAP по локальному имени элемента и атрибута,
// пересобирая замаскированный XML. При ошибке разбора — текстовая маска.
func xmlMaskString(b []byte) string {
	r := activeRules()
	if len(b) > r.MaxBodyBytes {
		return truncatedTextMask(b, r)
	}
	type frame struct {
		forced bool
		mask   string
	}
	var out []byte
	var stack []frame
	dec := xml.NewDecoder(bytes.NewReader(b))
	ok := func() (okk bool) {
		defer func() {
			if recover() != nil {
				okk = false
			}
		}()
		for {
			tok, err := dec.RawToken()
			if err == io.EOF {
				return true
			}
			if err != nil {
				return false
			}
			switch t := tok.(type) {
			case xml.StartElement:
				cur := frame{}
				if len(stack) > 0 && stack[len(stack)-1].forced {
					cur = stack[len(stack)-1]
				} else if mt, sens := r.keyMask(t.Name.Local); sens {
					cur = frame{forced: true, mask: mt}
				}
				out = append(out, '<')
				out = append(out, xmlName(t.Name)...)
				for _, a := range t.Attr {
					out = append(out, ' ')
					out = append(out, xmlName(a.Name)...)
					out = append(out, '=', '"')
					var val string
					if cur.forced {
						val = mask(cur.mask, a.Value)
					} else if mt, sens := r.keyMask(a.Name.Local); sens {
						val = mask(mt, a.Value)
					} else {
						val = maskFreeText(a.Value)
					}
					out = appendXMLEscaped(out, val)
					out = append(out, '"')
				}
				out = append(out, '>')
				stack = append(stack, cur)
			case xml.EndElement:
				out = append(out, '<', '/')
				out = append(out, xmlName(t.Name)...)
				out = append(out, '>')
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			case xml.CharData:
				s := string(t)
				var masked string
				if len(stack) > 0 && stack[len(stack)-1].forced {
					if strings.TrimSpace(s) == "" {
						masked = s
					} else {
						masked = mask(stack[len(stack)-1].mask, s)
					}
				} else {
					masked = maskFreeText(s)
				}
				out = appendXMLEscaped(out, masked)
			case xml.Comment:
				// Комментарии могут нести данные — выбрасываем.
			case xml.ProcInst, xml.Directive:
				// Служебные узлы не несут payload — пропускаем.
			}
		}
	}()
	if !ok {
		return textMaskFallback(string(b))
	}
	return string(out)
}

func xmlName(n xml.Name) string {
	if n.Space != "" {
		return n.Space + ":" + n.Local
	}
	return n.Local
}

func appendXMLEscaped(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			dst = append(dst, "&lt;"...)
		case '>':
			dst = append(dst, "&gt;"...)
		case '&':
			dst = append(dst, "&amp;"...)
		case '"':
			dst = append(dst, "&quot;"...)
		default:
			dst = append(dst, s[i])
		}
	}
	return dst
}

// --- JSON-строки --------------------------------------------------------------

const hexDigits = "0123456789abcdef"

// appendJSONString дописывает s к dst как корректную JSON-строку (с кавычками),
// экранируя спецсимволы и заменяя невалидный UTF-8 на U+FFFD. Без лишних
// промежуточных аллокаций — работает прямо по dst.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		b := s[i]
		if b < 0x20 || b == '"' || b == '\\' {
			dst = append(dst, s[start:i]...)
			switch b {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}
		if b < utf8.RuneSelf {
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		if size == 1 { // невалидный UTF-8
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\xEF', '\xBF', '\xBD') // U+FFFD
			i++
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, s[start:]...)
	dst = append(dst, '"')
	return dst
}
