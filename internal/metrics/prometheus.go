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

	// Effective annualised volatility used for Black-Scholes pricing.
	SigmaCurrent = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "polymarket_sigma",
		Help: "Effective annualised volatility σ used for Black-Scholes pricing",
	})

	// ── paper trading ─────────────────────────────────────────────────────────
	// All paper metrics carry a "strategy" label so multiple algorithms can be
	// compared on the same Grafana panels.

	PaperTradeEntryPrice = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_paper_entry_price",
		Help: "Entry price of the most recent paper trade",
	}, []string{"strategy", "outcome", "side"})

	PaperTradeSize = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_paper_trade_size_usdc",
		Help: "Notional size in USDC of the most recent paper trade",
	}, []string{"strategy", "outcome", "side"})

	PaperUnrealizedPnL = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_paper_unrealized_pnl",
		Help: "Mark-to-market unrealized P&L for the open paper position",
	}, []string{"strategy", "outcome"})

	PaperWindowPnL = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_paper_window_pnl",
		Help: "Realized P&L of the most recently closed 5-minute window per outcome",
	}, []string{"strategy", "outcome"})

	PaperTotalPnL = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_paper_total_pnl",
		Help: "Cumulative paper trading P&L since start",
	}, []string{"strategy"})

	PaperTradesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "polymarket_paper_trades_total",
		Help: "Number of paper trades executed",
	}, []string{"strategy", "outcome", "side"})

	PaperPositionNetUSDC = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_paper_position_net_usdc",
		Help: "Net paper position in USDC: +N = long Up by $N, −N = long Down by $N",
	}, []string{"strategy"})

	PaperWindowRiskUsed = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_paper_window_risk_used_usdc",
		Help: "USDC committed to paper trades in the current 5-minute window",
	}, []string{"strategy"})

	PaperWindowRiskLimit = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_paper_window_risk_limit_usdc",
		Help: "Maximum USDC risk allowed per 5-minute window",
	}, []string{"strategy"})

	PaperDailyPnL = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "polymarket_paper_daily_pnl",
		Help: "Paper trading P&L accumulated since midnight UTC today",
	}, []string{"strategy"})
)
