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

	MarketBestBid = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_best_bid",
		Help: "Best bid price on Polymarket order book",
	}, []string{"token_id", "market_id", "outcome"})

	MarketBestAsk = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_best_ask",
		Help: "Best ask price on Polymarket order book",
	}, []string{"token_id", "market_id", "outcome"})

	MarketMidPrice = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_mid_price",
		Help: "Mid price (best_bid + best_ask) / 2",
	}, []string{"token_id", "market_id", "outcome"})

	MarketSpread = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_spread",
		Help: "Spread (best_ask - best_bid)",
	}, []string{"token_id", "market_id", "outcome"})

	FairPrice = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_fair_price",
		Help: "Black-Scholes fair price N(d2) for the binary option",
	}, []string{"token_id", "market_id", "outcome", "strike"})

	Edge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_edge",
		Help: "Edge = fair_price - mid_price; positive means we favour buying",
	}, []string{"token_id", "market_id", "outcome", "strike"})

	TimeToExpirySec = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_time_to_expiry_seconds",
		Help: "Seconds remaining until market expiry",
	}, []string{"token_id", "market_id"})

	PollErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "polymarket_poll_errors_total",
		Help: "Total number of errors polling Polymarket",
	}, []string{"token_id"})
)
