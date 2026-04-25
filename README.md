# Polymarket BTC Binary Options Trader

Сбор данных, ценообразование и торговля бинарными опционами на Bitcoin на Polymarket.

- Цена опциона оценивается по формуле **Black-Scholes** для cash-or-nothing (N(d2))
- Котировки и ордербук — **Polymarket CLOB API**
- Цена BTC — **Binance WebSocket** в реальном времени
- Метрики — **Prometheus + Grafana**
- Логи — **CSV** с ротацией по дням

## Структура проекта

```
cmd/collector/          — точка входа
internal/
  polymarket/           — CLOB REST клиент
  btcprice/             — Binance WS фид цены BTC
  pricing/              — Black-Scholes для бинарных опционов
  auth/                 — L2 авторизация (HMAC-SHA256)
  trader/               — подпись EIP-712, размещение ордеров, стратегия
  metrics/              — Prometheus метрики
  csvlog/               — CSV логгер
config/                 — конфигурация и загрузка .env
grafana/                — автопровизионинг дашборда
```

## Требования

- Go 1.24+ (установлен в `~/go-dist/go/bin/go`)
- Docker + Docker Compose (для Grafana и Prometheus)
- `curl` + `jq` (для поиска маркетов)

Добавь Go в PATH, если ещё не добавлен:

```bash
echo 'export PATH=$HOME/go-dist/go/bin:$PATH' >> ~/.bashrc
source ~/.bashrc
```

## Быстрый старт

### 1. Запусти Grafana + Prometheus

```bash
docker compose up -d
```

- Grafana: http://localhost:3000 (дашборд провизионирован автоматически)
- Prometheus: http://localhost:9090

### 2. Найди token IDs

```bash
./scripts/find_markets.sh
```

Или вручную:

```bash
curl 'https://clob.polymarket.com/markets?tag=Bitcoin&limit=20' \
  | jq '.data[] | {question:.question, end_date_iso:.end_date_iso, tokens:.tokens}'
```

Каждый рынок имеет два токена: **YES** и **NO**. Передавай нужные token IDs через флаг `-tokens`.

### 3. Запусти коллектор (режим только сбора данных)

```bash
go run ./cmd/collector \
  -tokens "TOKEN_ID_1,TOKEN_ID_2" \
  -sigma 0.80
```

### 4. Запусти с торговлей

Скопируй `.env.schema` в `.env` и заполни:

```bash
cp .env.schema .env
```

```bash
go run ./cmd/collector \
  -tokens "TOKEN_ID_1,TOKEN_ID_2" \
  -sigma 0.80 \
  -env .env
```

При наличии всех ключей в `.env` торговля включается автоматически.

## Флаги

| Флаг | По умолчанию | Описание |
|---|---|---|
| `-tokens` | — | Обязательный. Comma-separated список Polymarket token IDs |
| `-sigma` | `0.80` | Годовая волатильность σ для Black-Scholes |
| `-poll` | `5s` | Интервал опроса ордербука |
| `-metrics` | `:9100` | Адрес Prometheus /metrics |
| `-csv` | `data` | Директория для CSV логов |
| `-env` | `.env` | Путь к файлу с API ключами |

## Переменные окружения (.env)

Смотри [.env.schema](.env.schema) — там описана каждая переменная.

Минимальный набор для торговли:

```
POLY_ADDRESS=0x...
POLY_PRIVATE_KEY=0x...
POLY_API_KEY=...
POLY_API_SECRET=...
POLY_API_PASSPHRASE=...
```

API ключи создаются на [polymarket.com](https://polymarket.com) → Settings → API Keys.

## Метрики Prometheus

| Метрика | Описание |
|---|---|
| `btc_spot_price` | Цена BTC/USDT с Binance |
| `polymarket_best_bid` | Лучший бид в ордербуке |
| `polymarket_best_ask` | Лучший аск в ордербуке |
| `polymarket_mid_price` | Середина спреда |
| `polymarket_spread` | ask − bid |
| `polymarket_fair_price` | Оценка Black-Scholes: N(d2) |
| `polymarket_edge` | fair − mid (> 0 = недооценён YES) |
| `polymarket_time_to_expiry_seconds` | Секунд до экспирации |
| `polymarket_poll_errors_total` | Счётчик ошибок опроса |

## Торговая стратегия

Простая логика на основе edge:

```
edge > threshold  →  BUY  YES @ mid − offset   (пассивный лимит)
edge < −threshold →  SELL YES @ mid + offset   (пассивный лимит)
```

Пропускает сигнал если:
- spread > 3 × threshold (неликвидный рынок)
- до экспирации < 30 секунд
- уже есть открытый ордер на этот токен

Параметры стратегии настраиваются через `.env`:

```
TRADE_EDGE_THRESHOLD=0.04    # минимальный edge для торговли
TRADE_MAX_SIZE_USDC=10.0     # максимальный размер ордера
TRADE_PRICE_OFFSET=0.01      # отступ от mid при выставлении лимита
TRADE_ORDER_TTL_SECONDS=60   # время жизни ордера
```

## Black-Scholes для бинарного опциона

Для cash-or-nothing call (платит $1 если BTC > K):

```
P = N(d2)
d2 = ( ln(S/K) − σ²/2 · T ) / ( σ · √T )
```

где S — цена BTC, K — страйк, T — время до экспирации в годах, σ — волатильность.
Для коротких опционов r ≈ 0.
