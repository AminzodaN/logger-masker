# logger · masklog

> Структурное логирование с маскированием чувствительных данных для Go-сервисов.

Два небольших самодостаточных пакета поверх стандартного `log/slog` — **без единой сторонней зависимости**:

- **`logger`** — единый логгер сервиса: JSON-вывод, уровни, printf-сахар и сквозной `request_id` через `context` и HTTP.
- **`masklog`** — маскирование чувствительных полей **по тегам структур** (`log:"mask" mask:"pan"`, `log:"drop"`), одинаково в `slog` и в `fmt`.

```go
logger.C(ctx).Info("partner reply", "resp", masklog.V(resp))
// card, phone, cvv и прочее замаскированы согласно тегам — в одну строку JSON
```

<p>
  <img alt="Go" src="https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white">
  <img alt="dependencies" src="https://img.shields.io/badge/dependencies-0-success">
  <img alt="stdlib" src="https://img.shields.io/badge/built%20on-log%2Fslog-blue">
</p>

---

## Содержание

- [Возможности](#возможности)
- [Установка](#установка)
- [Быстрый старт](#быстрый-старт)
- [Архитектура](#архитектура)
- [Пакет `logger`](#пакет-logger)
  - [Создание](#создание)
  - [Методы](#методы)
  - [`request_id` сквозь сервисы](#request_id-сквозь-сервисы)
- [Пакет `masklog`](#пакет-masklog)
  - [Теги](#теги)
  - [Как применять](#как-применять)
  - [Справочник масок](#справочник-масок)
  - [Нюанс про `%+v`](#нюанс-про-v)
- [Разработка](#разработка)
- [Переиспользование](#переиспользование)

---

## Возможности

- **Zero-dependency.** Только стандартная библиотека — пакеты можно скопировать в любой сервис как есть.
- **Маскирование по тегам.** `log:"mask" mask:"pan"`, `log:"drop"`, `log:"json|xml|text"` — правила живут рядом с полями структуры.
- **Единое поведение в `slog` и `fmt`.** `masklog.V(x)` реализует `slog.LogValuer` и `fmt.Formatter/Stringer` — результат одинаков в структурном логе и в `%+v`.
- **Fail-closed by design.** Любая паника внутри обхода превращается в `[masklog:error]`; неизвестный тег трактуется как `mask`; сырые данные не утекают.
- **Безопасный обход.** Рекурсивный walker с защитой от циклов, ограничением глубины и кэшем описаний типов.
- **Сквозной `request_id`.** Генерация, проброс через `context`, HTTP-middleware и инъекция в исходящие запросы.
- **Production-ready вывод.** JSON по умолчанию, UTC RFC3339 с миллисекундами, `service/version/env` в каждой записи; `local` → человекочитаемый text.

---

## Установка

Пакеты лежат в модуле `logger-mask`:

```go
import (
    "logger-mask/logger"
    "logger-mask/masklog"
)
```

Требуется **Go 1.25+**. Сторонних зависимостей нет — `go build` работает сразу.

---

## Быстрый старт

```go
package main

import (
	"context"
	"log/slog"

	"logger-mask/logger"
	"logger-mask/masklog"
)

// Тело входящего запроса: account — последние 4, device_id — первые 4,
// client_ip — скрыта хостовая часть.
type ServerRequest struct {
	PaymentID string `json:"payment_id"`
	Account   string `json:"account"   log:"mask" mask:"last4"`
	DeviceID  string `json:"device_id" log:"mask" mask:"first4"`
	ClientIP  string `json:"client_ip" log:"mask" mask:"ip"`
}

// Тело ответа партнёра: card — как PAN, bin — первые 6 цифр эмитента,
// phone/holder частично, cvv выбрасывается.
type PartnerResponse struct {
	TxID   string `json:"tx_id"`
	Status string `json:"status"`
	Card   string `json:"card"   log:"mask" mask:"pan"`
	BIN    string `json:"bin"    log:"mask" mask:"bin"`
	Phone  string `json:"phone"  log:"mask" mask:"phone"`
	Holder string `json:"holder" log:"mask" mask:"name"`
	CVV    string `json:"cvv"    log:"drop"`
}

func main() {
	logger.New(logger.Config{
		Service: "payment-gateway",
		Version: "1.14.2",
		Env:     "prod",
		Level:   slog.LevelInfo,
	})

	ctx := logger.WithRequestID(context.Background(), logger.NewRequestID())

	req := ServerRequest{
		PaymentID: "P-889120",
		Account:   "918123456",
		DeviceID:  "a1b2c3d4e5",
		ClientIP:  "203.0.113.42",
	}
	resp := PartnerResponse{
		TxID: "P-889120", Status: "DECLINED",
		Card: "4111111111111111", BIN: "5500005555555559",
		Phone: "+992901234567", Holder: "Alisher Karimov", CVV: "123",
	}

	// И тело запроса, и тело ответа маскируются по своим тегам.
	logger.C(ctx).Error("partner payment declined",
		"partner", "visa_acq",
		"error_code", "PAYMENT_DECLINED",
		"duration_ms", 412,
		"request", masklog.V(req),
		"response", masklog.V(resp),
	)
}
```

Вывод — одна строка JSON (развёрнута для читаемости):

```json
{
  "time": "2026-10-23T05:01:42.805Z",
  "level": "ERROR",
  "msg": "partner payment declined",
  "service": "payment-gateway",
  "version": "1.14.2",
  "env": "prod",
  "request_id": "0207418447ef96f3aca4c9148570083f",
  "partner": "visa_acq",
  "error_code": "PAYMENT_DECLINED",
  "duration_ms": 412,
  "request": {
    "payment_id": "P-889120",
    "account": "*****3456",
    "device_id": "a1b2******",
    "client_ip": "203.0.113.***"
  },
  "response": {
    "tx_id": "P-889120",
    "status": "DECLINED",
    "card": "411111******1111",
    "bin": "550000**********",
    "phone": "*********4567",
    "holder": "A****** K******"
  }
}
```

Чувствительные поля замаскированы по тегам, `cvv` выброшен, `request_id` проставлен,
а `service/version/env` и UTC-время добавлены ко всем записям автоматически.

---

## Архитектура

```
.
├── go.mod                  module logger-mask, Go 1.25, без сторонних зависимостей
├── main.go                 демо: PartnerResponse/ServerRequest + логирование
├── logger/                 логгер-фасад над log/slog
│   ├── logger.go             Config, New, printf-сахар, структурные методы, default
│   └── context.go            request_id: WithRequestID/C/Into/Middleware/Inject…
└── masklog/                маскирование структур по тегам
    ├── mask.go               функции маскирования pan/last4/first4/bin/ip/…
    ├── tags.go               разбор тегов log/mask + кэш описаний типов
    ├── value.go              V()=slog.LogValuer+fmt.Formatter, Format()
    └── body.go               маскирование сырых тел JSON/XML/текст
```

Пакеты **ортогональны**: `logger` пишет логи, `masklog` делает значение безопасным
перед записью. Друг о друге они не знают — их соединяет только место вызова:

```go
logger.C(ctx).Info("partner reply", "resp", masklog.V(resp))
```

---

## Пакет `logger`

### Создание

```go
log := logger.New(logger.Config{
	Service:   "payment-gateway", // попадает в каждую запись
	Version:   "1.14.2",
	Env:       "prod",            // "local" => человекочитаемый text вместо JSON
	Level:     slog.LevelInfo,
	Output:    os.Stdout,         // по умолчанию os.Stdout
	AddSource: false,             // true => добавит файл:строку
})
```

- `New` ставит логгер процессным **default** — дальше можно звать `logger.L()` / `logger.C(ctx)` откуда угодно, не прокидывая `*Logger`.
- В `prod` (всё, что не `local`/`stage`) уровень `debug` принудительно глушится, даже если в `Level` попросили ниже.
- Время всегда приводится к UTC RFC3339 с миллисекундами.
- `Env: "local"` переключает вывод на `slog` text-handler (удобно читать глазами).

### Методы

**printf-сахар** — форматируют `msg` и пишут на нужном уровне:

```go
log.Debugf("cache miss for %s", key)
log.Infof("processed in %d ms", ms)
log.Warnf("retry %d/%d", n, max)
log.Errorf("upstream %s failed: %v", name, err)
log.Printf("...")                   // алиас Infof, drop-in для stdlib log
log.Fatalf("cannot start: %v", err) // лог + os.Exit(1)
```

**Структурный путь** — ключ-значение, *предпочтителен* (дешевле и даёт вложенный JSON вместо строки):

```go
log.Info("partner reply", "rrn", rrn, "duration_ms", ms, "resp", masklog.V(resp))
log.Warn(...); log.Error(...); log.Debug(...)
```

**Вспомогательное:**

```go
log.With("partner", "visa_acq")   // дочерний логгер с постоянными полями
log.Slog()                        // *slog.Logger для мест, где нужен сам slog
logger.Wrap(existingSlog)         // адаптировать готовый *slog.Logger
ctx = logger.Into(ctx, log)       // привязать логгер к ctx (C(ctx) вернёт его без клона)
```

### `request_id` сквозь сервисы

Один идентификатор запроса проходит через все сервисы и попадает в каждую запись
лога — чтобы собрать путь запроса целиком.

```go
// 1. На входе HTTP-сервиса — middleware: читает X-Request-Id / traceparent,
//    генерит если нет, кладёт в context и возвращает в заголовке ответа.
mux := http.NewServeMux()
srv := logger.Middleware(mux)

// 2. В хендлере просто логируем через C(ctx) — request_id проставится сам.
func handler(w http.ResponseWriter, r *http.Request) {
	logger.C(r.Context()).Infof("handling %s", r.URL.Path)
}

// 3. Вызов соседнего сервиса — прокинуть тот же id в исходящий запрос.
req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
logger.InjectRequestID(ctx, req) // ставит X-Request-Id из ctx
```

Ручное управление без HTTP:

```go
ctx := logger.WithRequestID(ctx, logger.NewRequestID())
id  := logger.RequestID(ctx)      // достать id
logger.C(ctx).Info("...")         // логгер с полем request_id
```

> **Производительность.** `logger.Middleware` привязывает логгер к запросу **один раз**
> (`logger.Into`), поэтому `logger.C(ctx)` внутри запроса — дешёвый lookup (≈6 аллокаций/запись).
> Вне middleware `C(ctx)` каждый раз пере-привязывает `request_id` (≈13 аллокаций) — если в
> горячем цикле логов много, привяжи логгер сам один раз: `l := logger.C(ctx)` и используй `l`.

---

## Пакет `masklog`

### Теги

Ставятся на поля структуры:

| Тег | Эффект |
|---|---|
| `log:"mask" mask:"pan"`    | номер карты: `411111******1111` |
| `log:"mask" mask:"phone"`  | последние 4: `*********4567` |
| `log:"mask" mask:"email"`  | `a***@mail.tj` |
| `log:"mask" mask:"name"`   | первые буквы слов: `A****** K******` |
| `log:"mask" mask:"last4"`  | последние 4: `*****3456` (частичный просмотр для сверки) |
| `log:"mask" mask:"first4"` | первые 4: `4111************` (показать префикс) |
| `log:"mask" mask:"bin"`    | BIN карты, первые 6: `411111**********` (аналитика по эмитенту) |
| `log:"mask" mask:"ip"`     | хостовая часть IP скрыта: `203.0.113.***`, `2001:db8::***` |
| `log:"mask"` (без `mask:`) | дефолт `***` (скрыть полностью) |
| `log:"drop"`               | поле вообще не попадает в лог |
| *(нет тега)*               | пишется как есть |

Имя поля в логе берётся из `json:"..."`, иначе snake_case от имени поля.
Неизвестный `log:`-тег трактуется как `mask` (fail-closed).

### Как применять

`masklog.V(x)` — обёртка, которую понимают и `slog`, и `fmt`:

```go
// структурно (slog.LogValuer -> вложенный объект с замаскированными полями)
logger.C(ctx).Info("reply", "resp", masklog.V(resp))

// в формат-строке (fmt.Formatter/Stringer -> безопасная строка)
logger.C(ctx).Infof("reply %+v", masklog.V(resp))

// просто получить безопасную строку
s := masklog.Format(resp)
```

Тело запроса и тело ответа маскируются одинаково — это просто структуры с тегами:
`log:"drop"`-поля выпадают, поля без тега (или с `log:""`) проходят как есть.

**Свойства walker'а:** рекурсивный (вложенные структуры рекурсят со своими тегами),
cycle-safe, ограничен глубиной 10, кэширует описание типа, fail-closed — любая паника
внутри превращается в `[masklog:error]`, сырые данные не утекают.

### Справочник масок

| Маска | Вход | Выход | Назначение |
|---|---|---|---|
| `pan`    | `4111111111111111` | `411111******1111` | номер карты: BIN + последние 4 |
| `bin`    | `5500005555555559` | `550000**********` | только BIN (первые 6) |
| `phone`  | `+992901234567`    | `*********4567`    | последние 4 цифры |
| `email`  | `alisher@mail.tj`  | `a***@mail.tj`     | первая буква + домен |
| `name`   | `Alisher Karimov`  | `A****** K******`  | первые буквы слов |
| `last4`  | `918123456`        | `*****3456`        | последние 4 символа |
| `first4` | `a1b2c3d4e5`       | `a1b2******`       | первые 4 символа |
| `ip`     | `203.0.113.42`     | `203.0.113.***`    | скрыть хост, оставить подсеть |

> Короткие значения (≤ длины видимой части) маскируются **целиком** — чтобы по короткой
> строке нельзя было восстановить исходник.

### Нюанс про `%+v`

⚠️ `fmt` **не знает про теги** — он зовёт твой метод, только если тип реализует
`Stringer`/`Formatter`. Поэтому «голая» структура в `Infof("%+v", resp)` **течёт**:

```go
logger.C(ctx).Infof("%+v", resp)            // ТЕЧЁТ: {Card:4111111111111111 ...}
logger.C(ctx).Infof("%+v", masklog.V(resp)) // OK:    {card:411111******1111 ...}
```

Чтобы безопасен был даже «голый» `%+v`, добавь методы каждому чувствительному типу:

```go
func (r PartnerResponse) String() string   { return masklog.Format(r) }
func (r PartnerResponse) GoString() string { return masklog.Format(r) }
```

**Рекомендация:** чувствительные payload'ы всегда логируй структурно через `masklog.V(...)`
— это и безопасно, и дешевле, чем `%+v`.

---

## Разработка

```sh
go build ./...
go vet ./...
```

---

## Переиспользование

Сейчас пакеты лежат в этом же модуле (`import "logger-mask/logger"`, `"logger-mask/masklog"`).
Чтобы шарить между сервисами через `go get`, вынеси `logger` и `masklog` в отдельный
Go-модуль/репозиторий — внешних зависимостей у них нет, так что перенос тривиален.
