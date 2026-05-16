package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"trading-polymarket/config"
	"trading-polymarket/internal/auth"
	"trading-polymarket/internal/btcprice"
	"trading-polymarket/internal/csvlog"
	"trading-polymarket/internal/gamma"
	"trading-polymarket/internal/metrics"
	"trading-polymarket/internal/polymarket"
	"trading-polymarket/internal/pricing"
	"trading-polymarket/internal/trader"
)

func main() {
	list := flag.Bool("list", false, "Print active BTC markets and exit")
	discoverWindow := flag.Duration("discover-window", 1*time.Hour, "Track markets expiring within this window")
	sigma := flag.Float64("sigma", 0.20, "Annualised volatility σ for Black-Scholes (fallback when --sigma-window=0 or insufficient data)")
	sigmaWindow := flag.Duration("sigma-window", 5*time.Minute, "Rolling window for σ estimation from Binance prices (0 = use static --sigma)")
	paperOrderSize := flag.Float64("paper-order-size", 20, "Max USDC per individual buy tick in paper trading")
	paperMaxRisk := flag.Float64("paper-max-risk", 100, "Max net position in USDC per 5-minute window in paper trading")
	metricsAddr := flag.String("metrics", ":9100", "Prometheus /metrics listen address")
	csvDir := flag.String("csv", "data", "Directory for CSV output files")
	envFile := flag.String("env", ".env", "Path to .env file with API credentials")
	btcSource := flag.String("btc-source", "polymarket", "BTC price source: polymarket (Chainlink on-chain) or binance (WebSocket)")
	polygonRPC := flag.String("polygon-rpc", btcprice.DefaultPolygonRPC, "Polygon JSON-RPC URL for Chainlink price feed")
	flag.Parse()

	if err := godotenv.Load(*envFile); err != nil && !os.IsNotExist(err) {
		log.Printf("[env] warning: %v", err)
	}

	cfg := config.Default()
	cfg.Volatility = *sigma
	cfg.MetricsAddr = *metricsAddr
	cfg.CSVDir = *csvDir

	gammaClient := gamma.NewClient()

	// -list: print markets and exit
	if *list {
		printMarkets(gammaClient, *discoverWindow)
		return
	}

	if err := cfg.LoadTrading(); err != nil {
		log.Fatalf("[config] %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Start BTC price feed and wait for first tick before using spot as strike.
	var priceFeed btcprice.Source
	switch *btcSource {
	case "polymarket":
		priceFeed = btcprice.NewChainlinkFeed(*polygonRPC)
	case "binance":
		priceFeed = btcprice.NewFeed(cfg.BinanceWSURL, cfg.BinancePair)
	default:
		log.Fatalf("[btcprice] unknown source %q — choose polymarket or binance", *btcSource)
	}
	go priceFeed.Run(ctx)
	log.Println("[btcprice] waiting for first BTC price...")
	spot := priceFeed.WaitPrice(ctx)
	if spot == 0 {
		return // context cancelled
	}
	log.Printf("[btcprice] spot=%.2f", spot)

	// Rolling volatility feed (always uses Binance regardless of --btc-source).
	var rollingVol *btcprice.RollingVol
	if *sigmaWindow > 0 {
		var volFeed btcprice.Source
		if *btcSource == "binance" {
			volFeed = priceFeed // reuse the existing Binance feed
		} else {
			volFeed = btcprice.NewFeed(cfg.BinanceWSURL, cfg.BinancePair)
			go volFeed.Run(ctx)
		}
		rollingVol = btcprice.NewRollingVol(volFeed, *sigmaWindow)
		go rollingVol.Run(ctx)
	}
	metrics.SigmaCurrent.Set(*sigma)

	// strikeFn resolves the ATM strike for a given window-open time.
	// When the price feed is Chainlink we look up the historical reading closest
	// to the window-open timestamp; otherwise we fall back to current spot.
	type historicalPricer interface{ PriceAt(time.Time) float64 }
	strikeFn := func(t time.Time) float64 {
		if hp, ok := priceFeed.(historicalPricer); ok {
			if p := hp.PriceAt(t); p > 0 {
				return p
			}
			log.Printf("[btcprice] no chainlink reading near %s, using current spot", t.Format("15:04:05Z"))
		}
		return priceFeed.Price()
	}

	// Discover the current 5-minute BTC market pair.
	cfg.MarketTokenIDs = discoverAndLoadMeta(gammaClient, strikeFn)
	if len(cfg.MarketTokenIDs) == 0 {
		log.Fatal("[discover] no active BTC 5m market found")
	}

	// CSV loggers
	csvWriter, err := csvlog.NewWriter(cfg.CSVDir)
	if err != nil {
		log.Fatalf("csvlog: %v", err)
	}
	defer csvWriter.Close()

	windowLog, err := csvlog.NewWindowWriter(cfg.CSVDir)
	if err != nil {
		log.Fatalf("windowlog: %v", err)
	}
	defer windowLog.Close()

	tradeLog, err := csvlog.NewTradeWriter(cfg.CSVDir)
	if err != nil {
		log.Fatalf("tradelog: %v", err)
	}
	defer tradeLog.Close()

	// Polymarket market WebSocket: drives order-book and trade updates in real time.
	wsClient := polymarket.NewWSClient()
	go wsClient.Run(ctx)
	wsClient.Subscribe(cfg.MarketTokenIDs)

	// Trading infrastructure (only when credentials are present)
	var tradeClient *trader.Client
	var strategy *trader.Strategy
	if cfg.Trading.Enabled {
		signer, err := trader.NewSigner(cfg.Trading.PrivateKey, cfg.Trading.ChainID)
		if err != nil {
			log.Fatalf("[trader] signer: %v", err)
		}
		creds := &auth.Credentials{
			Address:    cfg.Trading.Address,
			APIKey:     cfg.Trading.APIKey,
			APISecret:  cfg.Trading.APISecret,
			Passphrase: cfg.Trading.APIPassphrase,
		}
		tradeClient = trader.NewClient(cfg.PolymarketCLOBURL, creds, signer, cfg.Trading.OrderTTL)
		strategy = trader.NewStrategy(trader.Params{
			Name:          "edge",
			EdgeThreshold: cfg.Trading.EdgeThreshold,
			MaxSizeUSDC:   cfg.Trading.MaxSizeUSDC,
			MinSizeUSDC:   cfg.Trading.MinSizeUSDC,
			PriceOffset:   cfg.Trading.PriceOffset,
			OrderTTL:      cfg.Trading.OrderTTL,
		})
		log.Printf("[trader] enabled — address=%s edge≥%.3f maxSize=%.1f USDC",
			cfg.Trading.Address, cfg.Trading.EdgeThreshold, cfg.Trading.MaxSizeUSDC)
	} else {
		log.Println("[trader] disabled — collect-only mode (paper trading active)")
	}

	// Paper traders run strategies in parallel in collect-only mode.
	// Add new StrategySignaler implementations here to compare algorithms.
	var paperTraders []*trader.PaperTrader
	if !cfg.Trading.Enabled {
		ep := trader.PaperExecutorParams{
			MaxWindowRiskUSDC: *paperMaxRisk,
			MinSizeUSDC:       1,
			LossCutEdge:       1.5 * cfg.Trading.EdgeThreshold,
		}
		epHold := trader.PaperExecutorParams{
			MaxWindowRiskUSDC: *paperMaxRisk,
			MinSizeUSDC:       1,
			HoldToExpiry:      true,
			NoNet:             true,
		}
		sigParams := trader.Params{
			EdgeThreshold: cfg.Trading.EdgeThreshold,
			MaxSizeUSDC:   *paperOrderSize,
			MinSizeUSDC:   1,
			PriceOffset:   cfg.Trading.PriceOffset,
			OrderTTL:      cfg.Trading.OrderTTL,
		}
		paperTraders = []*trader.PaperTrader{
			trader.NewPaperTrader(trader.NewStrategy(func() trader.Params {
				p := sigParams; p.Name = "edge"; return p
			}()), ep, windowLog),
			trader.NewPaperTrader(trader.NewNearExpiryStrategy(func() trader.Params {
				p := sigParams; p.Name = "near_expiry"; return p
			}()), ep, windowLog),
			trader.NewPaperTrader(trader.NewStraddleStrategy(func() trader.Params {
				p := sigParams; p.Name = "straddle"; return p
			}()), epHold, windowLog),
		}
	}

	// Prometheus HTTP server
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux}
	go func() {
		log.Printf("[metrics] listening on %s/metrics", cfg.MetricsAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[metrics] %v", err)
		}
	}()
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	log.Printf("[collector] σ=%.2f event-driven (WS)", cfg.Volatility)

	// Re-discover aligned to 5-minute window boundaries.
	rediscoverTimer := time.NewTimer(untilNextWindow())
	defer rediscoverTimer.Stop()

	// Housekeeping: TTE metric refresh, expiry sweep, BTC spot/σ metrics.
	// Runs independently of WS traffic so metrics stay live on dormant markets.
	housekeeping := time.NewTicker(1 * time.Second)
	defer housekeeping.Stop()

	events := wsClient.Events()

	for {
		select {
		case <-ctx.Done():
			log.Println("[collector] shutting down")
			return

		case <-rediscoverTimer.C:
			currentSpot := priceFeed.Price()
			newIDs := discoverAndLoadMeta(gammaClient, strikeFn)
			if len(newIDs) > 0 {
				if len(paperTraders) > 0 {
					newSet := make(map[string]bool, len(newIDs))
					for _, id := range newIDs {
						newSet[id] = true
					}
					for _, oldID := range cfg.MarketTokenIDs {
						if !newSet[oldID] {
							m := marketMeta[oldID]
							settlement := computeSettlement(m.Outcome, currentSpot, m.Strike)
							for _, pt := range paperTraders {
								pt.OnExpiry(oldID, m.Outcome, settlement)
							}
						}
					}
					for _, pt := range paperTraders {
						pt.OnNewWindow()
					}
				}
				cfg.MarketTokenIDs = newIDs
				wsClient.Subscribe(newIDs)
			}
			rediscoverTimer.Reset(untilNextWindow())

		case <-housekeeping.C:
			currentSpot := priceFeed.Price()
			metrics.BTCSpotPrice.Set(currentSpot)
			if rollingVol != nil {
				if s := rollingVol.Sigma(); s > 0 {
					metrics.SigmaCurrent.Set(s)
				}
			}
			now := time.Now()
			var active []string
			for _, tid := range cfg.MarketTokenIDs {
				meta := marketMeta[tid]
				if !meta.Expiry.IsZero() {
					tte := pricing.TimeToExpiry(meta.Expiry)
					metrics.TimeToExpirySec.WithLabelValues(meta.Outcome).Set(tte)
					if now.After(meta.Expiry) {
						log.Printf("[collector] token %.8s expired, removing", tid)
						settlement := computeSettlement(meta.Outcome, currentSpot, meta.Strike)
						for _, pt := range paperTraders {
							pt.OnExpiry(tid, meta.Outcome, settlement)
						}
						delete(marketMeta, tid)
						continue
					}
				}
				active = append(active, tid)
			}
			if len(active) != len(cfg.MarketTokenIDs) {
				cfg.MarketTokenIDs = active
				if len(active) == 0 {
					for _, pt := range paperTraders {
						pt.OnNewWindow()
					}
				}
			}

		case ev := <-events:
			if ev.Type == "trade" {
				handleTradeEvent(ev, tradeLog)
				continue
			}
			// book / price_change: top-of-book changed for ev.TokenID.
			currentSpot := priceFeed.Price()
			effectiveSigma := cfg.Volatility
			if rollingVol != nil {
				if s := rollingVol.Sigma(); s > 0 {
					effectiveSigma = s
				}
			}
			snap := buildSnapshot(wsClient, ev.TokenID, currentSpot, effectiveSigma)
			if snap == nil {
				continue
			}
			writeAndEmitSnapshot(csvWriter, snap, currentSpot)
			if tradeClient != nil && strategy != nil {
				maybeExecute(strategy, tradeClient, *snap)
			}
			// Build batch with current snaps for all tracked tokens so cross-token
			// strategies (e.g. straddle) always see both legs.
			batch := make([]trader.Snapshot, 0, len(cfg.MarketTokenIDs))
			for _, tid := range cfg.MarketTokenIDs {
				if tid == ev.TokenID {
					batch = append(batch, *snap)
					continue
				}
				if s := buildSnapshot(wsClient, tid, currentSpot, effectiveSigma); s != nil {
					batch = append(batch, *s)
				}
			}
			for _, pt := range paperTraders {
				pt.OnTickBatch(batch)
			}
		}
	}
}

// ── metadata ──────────────────────────────────────────────────────────────────

var marketMeta = map[string]tokenMeta{}

type tokenMeta struct {
	MarketID string
	Outcome  string
	Strike   float64 // BTC spot at market open; 0 = ATM (use current spot)
	Expiry   time.Time
}

// discoverAndLoadMeta fetches the current 5-minute BTC market pair,
// populates marketMeta, and returns [upTokenID, downTokenID].
// strikeFn is called with the window-open time to get the ATM strike.
func discoverAndLoadMeta(gc *gamma.Client, strikeFn func(time.Time) float64) []string {
	m, err := gc.Current5m()
	if err != nil {
		log.Printf("[discover] gamma error: %v", err)
		return nil
	}
	if m == nil {
		log.Println("[discover] current 5m market not found")
		return nil
	}

	// Strike = Polymarket's opening price (Chainlink Data Streams) fetched from the
	// event page RSC payload — this is the exact value used for settlement.
	// Falls back to on-chain Chainlink historical block call, then current spot.
	strike := gc.FetchOpenPrice(m.Slug)
	if strike > 0 {
		log.Printf("[discover] strike=%.2f (source: polymarket RSC)", strike)
	} else {
		strike = strikeFn(slugOpenTime(m.Slug))
		log.Printf("[discover] strike=%.2f (source: chainlink fallback)", strike)
	}

	for _, entry := range []struct {
		tokenID string
		outcome string
	}{
		{m.UpTokenID, "Up"},
		{m.DownTokenID, "Down"},
	} {
		if old, ok := marketMeta[entry.tokenID]; !ok {
			for oldTokenID, oldMeta := range marketMeta {
				if oldMeta.Outcome == entry.outcome {
					metrics.MarketInfo.DeleteLabelValues(entry.outcome, oldTokenID, oldMeta.MarketID)
				}
			}
		} else {
			metrics.MarketInfo.DeleteLabelValues(entry.outcome, entry.tokenID, old.MarketID)
		}

		marketMeta[entry.tokenID] = tokenMeta{
			MarketID: m.ConditionID,
			Outcome:  entry.outcome,
			Strike:   strike,
			Expiry:   m.EndDate,
		}
		metrics.MarketInfo.WithLabelValues(entry.outcome, entry.tokenID, m.ConditionID).Set(1)
	}

	log.Printf("[discover] %s  exp=%s  K=%.2f", m.Question, m.EndDate.Format("15:04:05Z"), strike)

	return []string{m.UpTokenID, m.DownTokenID}
}

// slugOpenTime extracts the window-open Unix timestamp from a slug like
// "btc-updown-5m-1234567890" and returns the corresponding time.
func slugOpenTime(slug string) time.Time {
	parts := strings.Split(slug, "-")
	if len(parts) == 0 {
		return time.Time{}
	}
	ts, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// ── event handlers ────────────────────────────────────────────────────────────

// buildSnapshot reads the current order-book state for tokenID from the WS
// client and computes a trader.Snapshot. Returns nil if no book is available
// yet or tokenID is unknown. No side effects.
func buildSnapshot(ws *polymarket.WSClient, tokenID string, spot, sigma float64) *trader.Snapshot {
	ob := ws.BookSnapshot(tokenID)
	if ob == nil {
		return nil
	}
	meta, ok := marketMeta[tokenID]
	if !ok {
		return nil
	}
	var tte float64
	if !meta.Expiry.IsZero() {
		tte = pricing.TimeToExpiry(meta.Expiry)
	}
	strike := meta.Strike
	if strike == 0 {
		strike = spot
	}
	var fair float64
	if spot > 0 && strike > 0 && tte > 0 {
		switch meta.Outcome {
		case "Up":
			fair = pricing.BinaryCallPrice(spot, strike, tte, sigma, 0)
		case "Down":
			fair = pricing.BinaryPutPrice(spot, strike, tte, sigma, 0)
		}
	}
	mid := ob.MidPrice()
	return &trader.Snapshot{
		TokenID:   tokenID,
		MarketID:  meta.MarketID,
		Outcome:   meta.Outcome,
		MidPrice:  mid,
		BestBid:   ob.BestBid(),
		BestAsk:   ob.BestAsk(),
		FairPrice: fair,
		Edge:      fair - mid,
		Spread:    ob.Spread(),
		Expiry:    meta.Expiry,
	}
}

// writeAndEmitSnapshot updates per-token Prometheus metrics and writes a row
// to the snapshot CSV. Called on every WS top-of-book change.
func writeAndEmitSnapshot(csvWriter *csvlog.Writer, snap *trader.Snapshot, spot float64) {
	meta := marketMeta[snap.TokenID]
	strike := meta.Strike
	if strike == 0 {
		strike = spot
	}
	tte := 0.0
	if !meta.Expiry.IsZero() {
		tte = pricing.TimeToExpiry(meta.Expiry)
	}
	out := meta.Outcome
	metrics.MarketBestBid.WithLabelValues(out).Set(snap.BestBid)
	metrics.MarketBestAsk.WithLabelValues(out).Set(snap.BestAsk)
	metrics.MarketMidPrice.WithLabelValues(out).Set(snap.MidPrice)
	metrics.MarketSpread.WithLabelValues(out).Set(snap.Spread)
	metrics.FairPrice.WithLabelValues(out).Set(snap.FairPrice)
	metrics.Edge.WithLabelValues(out).Set(snap.Edge)
	metrics.Strike.WithLabelValues(out).Set(strike)

	if err := csvWriter.Write(csvlog.Snapshot{
		Ts:         time.Now(),
		MarketID:   snap.MarketID,
		TokenID:    snap.TokenID,
		Outcome:    snap.Outcome,
		Strike:     strconv.FormatFloat(strike, 'f', 2, 64),
		BTCSpot:    spot,
		BestBid:    snap.BestBid,
		BestAsk:    snap.BestAsk,
		MidPrice:   snap.MidPrice,
		Spread:     snap.Spread,
		FairPrice:  snap.FairPrice,
		Edge:       snap.Edge,
		TTESeconds: tte,
	}); err != nil {
		log.Printf("[csv] %v", err)
	}
}

// handleTradeEvent writes one WS-observed trade to the CSV and updates metrics.
// Takerwallet and txHash are not available via WebSocket — those columns stay
// empty (the REST data API has them but isn't queried in the WS path).
func handleTradeEvent(ev polymarket.MarketEvent, tw *csvlog.TradeWriter) {
	if ev.Trade == nil {
		return
	}
	meta, ok := marketMeta[ev.TokenID]
	if !ok {
		return
	}
	rec := csvlog.TradeRecord{
		Ts:       ev.Ts.UTC(),
		MarketID: meta.MarketID,
		TokenID:  ev.TokenID,
		Outcome:  meta.Outcome,
		Side:     ev.Trade.Side,
		Price:    ev.Trade.Price,
		Size:     ev.Trade.Size,
	}
	if err := tw.Write(rec); err != nil {
		log.Printf("[trades] write: %v", err)
	}
	metrics.MarketTradesTotal.WithLabelValues(meta.Outcome, ev.Trade.Side).Inc()
	metrics.MarketTradeVolumeUSDC.WithLabelValues(meta.Outcome, ev.Trade.Side).Add(ev.Trade.Price * ev.Trade.Size)
	metrics.MarketTradeLastPrice.WithLabelValues(meta.Outcome, ev.Trade.Side).Set(ev.Trade.Price)
	metrics.MarketTradeLastSize.WithLabelValues(meta.Outcome, ev.Trade.Side).Set(ev.Trade.Size)
}

func maybeExecute(s *trader.Strategy, tc *trader.Client, snap trader.Snapshot) {
	// Guard: one live order per token. Evaluate no longer applies this check.
	if s.OpenOrderID(snap.TokenID) != "" {
		return
	}
	sig := s.Evaluate(snap)
	if sig == nil {
		return
	}
	resp, err := tc.PlaceOrder(*sig)
	if err != nil {
		log.Printf("[trader] place order failed for %.8s: %v", snap.TokenID, err)
		return
	}
	s.RecordOpen(snap.TokenID, resp.OrderID)
}

func printMarkets(gc *gamma.Client, window time.Duration) {
	markets, err := gc.DiscoverBTC(window)
	if err != nil {
		log.Fatalf("[list] %v", err)
	}
	if len(markets) == 0 {
		fmt.Printf("No active BTC markets expiring within %s\n", window)
		return
	}
	fmt.Printf("Active BTC markets (expiring within %s):\n\n", window)
	for _, m := range markets {
		fmt.Printf("%-60s  exp=%s\n", m.Question, m.EndDate.Format("15:04 UTC"))
		fmt.Printf("  Up:   %s\n", m.UpTokenID)
		fmt.Printf("  Down: %s\n\n", m.DownTokenID)
	}
}

// untilNextWindow returns the duration until the next 5-minute boundary + 2s buffer.
// This ensures rediscover fires right after the new window opens on Polymarket.
func untilNextWindow() time.Duration {
	const period = 300
	now := time.Now().Unix()
	next := (now/period+1)*period + 2 // +2s buffer for Polymarket to publish the new market
	return time.Duration(next-now) * time.Second
}

// computeSettlement returns the binary settlement value (1.0 = ITM, 0.0 = OTM).
// Up wins when spot >= strike; Down wins otherwise.
func computeSettlement(outcome string, spot, strike float64) float64 {
	upWon := spot >= strike
	if (outcome == "Up" && upWon) || (outcome == "Down" && !upWon) {
		return 1.0
	}
	return 0.0
}

