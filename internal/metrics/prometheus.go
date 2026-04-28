package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	BTCSpotPrice = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "btc_spot_price",
		Help: "Current BTC/USDT spot price from Binance",
	})

	// Per-outcome time-series metrics — label is only "outcome" ("Up" or "Down")
	// so series update in-place across 5-minute windows without accumulating stale data.

	MarketBestBid = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_best_bid",
		Help: "Best bid price on Polymarket order book",
	}, []string{"outcome"})

	MarketBestAsk = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_best_ask",
		Help: "Best ask price on Polymarket order book",
	}, []string{"outcome"})

	MarketMidPrice = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_mid_price",
		Help: "Mid price (best_bid + best_ask) / 2",
	}, []string{"outcome"})

	MarketSpread = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_spread",
		Help: "Spread (best_ask - best_bid)",
	}, []string{"outcome"})

	FairPrice = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_fair_price",
		Help: "Black-Scholes fair price N(d2) for the binary option",
	}, []string{"outcome"})

	Edge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_edge",
		Help: "Edge = fair_price - mid_price; positive means we favour buying",
	}, []string{"outcome"})

	TimeToExpirySec = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_time_to_expiry_seconds",
		Help: "Seconds remaining until market expiry",
	}, []string{"outcome"})

	// Strike price at window open (updated once per 5-minute window).
	Strike = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_strike",
		Help: "BTC strike price (spot at window open) used for Black-Scholes",
	}, []string{"outcome"})

	// Info series: always 1; old combinations deleted on window switch so Grafana
	// can always identify the currently active token_id and market_id.
	MarketInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_market_info",
		Help: "Info series — token_id and market_id for the active window (value always 1)",
	}, []string{"outcome", "token_id", "market_id"})

	PollErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "polymarket_poll_errors_total",
		Help: "Total number of errors polling Polymarket",
	}, []string{"outcome"})
)
