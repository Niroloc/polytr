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
	metricsAddr := flag.String("metrics", ":9100", "Prometheus /metrics listen address")
	csvDir := flag.String("csv", "data", "Directory for CSV output files")
	pollInterval := flag.Duration("poll", 4*time.Second, "Order book polling interval")
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
	cfg.PollInterval = *pollInterval

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

	// Discover the current 5-minute BTC market pair.
	cfg.MarketTokenIDs = discoverAndLoadMeta(gammaClient, spot)
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

	// CLOB client for order book polling
	pmClient := polymarket.NewClient(cfg.PolymarketCLOBURL)

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

	// Paper trader runs in collect-only mode to simulate strategy execution.
	var paperTrader *trader.PaperTrader
	if !cfg.Trading.Enabled {
		paperTrader = trader.NewPaperTrader(trader.Params{
			EdgeThreshold:     cfg.Trading.EdgeThreshold,
			MaxSizeUSDC:       20,   // max USDC per individual buy tick
			MinSizeUSDC:       1,
			PriceOffset:       cfg.Trading.PriceOffset,
			OrderTTL:          cfg.Trading.OrderTTL,
			MaxWindowRiskUSDC: 100, // total net position limit per window
		}, windowLog)
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

	log.Printf("[collector] σ=%.2f poll=%s", cfg.Volatility, cfg.PollInterval)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	// Re-discover aligned to 5-minute window boundaries.
	rediscoverTimer := time.NewTimer(untilNextWindow())
	defer rediscoverTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[collector] shutting down")
			return

		case <-rediscoverTimer.C:
			currentSpot := priceFeed.Price()
			newIDs := discoverAndLoadMeta(gammaClient, currentSpot)
			if len(newIDs) > 0 {
				// Close paper positions on outgoing tokens, then reset the window budget.
				if paperTrader != nil {
					newSet := make(map[string]bool, len(newIDs))
					for _, id := range newIDs {
						newSet[id] = true
					}
					for _, oldID := range cfg.MarketTokenIDs {
						if !newSet[oldID] {
							m := marketMeta[oldID]
							paperTrader.OnExpiry(oldID, m.Outcome, computeSettlement(m.Outcome, currentSpot, m.Strike))
						}
					}
					paperTrader.OnNewWindow()
				}
				cfg.MarketTokenIDs = newIDs
			}
			rediscoverTimer.Reset(untilNextWindow())

		case <-ticker.C:
			currentSpot := priceFeed.Price()
			metrics.BTCSpotPrice.Set(currentSpot)

			// Use rolling Binance vol when available, fall back to static --sigma.
			effectiveSigma := cfg.Volatility
			if rollingVol != nil {
				if s := rollingVol.Sigma(); s > 0 {
					effectiveSigma = s
					metrics.SigmaCurrent.Set(s)
				}
			}

			now := time.Now()
			var active []string
			for _, tokenID := range cfg.MarketTokenIDs {
				meta := marketMeta[tokenID]
				if !meta.Expiry.IsZero() && now.After(meta.Expiry) {
					log.Printf("[collector] token %.8s expired, removing", tokenID)
					if paperTrader != nil {
						paperTrader.OnExpiry(tokenID, meta.Outcome, computeSettlement(meta.Outcome, currentSpot, meta.Strike))
					}
					delete(marketMeta, tokenID)
					continue
				}
				active = append(active, tokenID)
				snap, err := pollToken(pmClient, csvWriter, tokenID, currentSpot, effectiveSigma)
				if err != nil {
					log.Printf("[collector] token %.8s: %v", tokenID, err)
					metrics.PollErrors.WithLabelValues(marketMeta[tokenID].Outcome).Inc()
					continue
				}
				if tradeClient != nil && strategy != nil {
					maybeExecute(strategy, tradeClient, *snap)
				}
				if paperTrader != nil {
					paperTrader.OnTick(*snap)
				}
			}
			cfg.MarketTokenIDs = active
			// All tokens expired without rediscovery — reset window risk budget.
			if paperTrader != nil && len(active) == 0 {
				paperTrader.OnNewWindow()
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
// spot is recorded as the ATM strike (BTC price at window open).
func discoverAndLoadMeta(gc *gamma.Client, spot float64) []string {
	m, err := gc.Current5m()
	if err != nil {
		log.Printf("[discover] gamma error: %v", err)
		return nil
	}
	if m == nil {
		log.Println("[discover] current 5m market not found")
		return nil
	}

	for _, entry := range []struct {
		tokenID string
		outcome string
	}{
		{m.UpTokenID, "Up"},
		{m.DownTokenID, "Down"},
	} {
		// Delete the old MarketInfo series for this outcome before registering the new one.
		if old, ok := marketMeta[entry.tokenID]; !ok {
			// Different token (new window): delete any old info series for this outcome.
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
			Strike:   spot,
			Expiry:   m.EndDate,
		}
		metrics.MarketInfo.WithLabelValues(entry.outcome, entry.tokenID, m.ConditionID).Set(1)
	}

	log.Printf("[discover] %s  exp=%s  K=%.2f",
		m.Question, m.EndDate.Format("15:04:05Z"), spot)

	return []string{m.UpTokenID, m.DownTokenID}
}

// ── poll loop ─────────────────────────────────────────────────────────────────

func pollToken(
	client *polymarket.Client,
	csv *csvlog.Writer,
	tokenID string,
	spot, sigma float64,
) (*trader.Snapshot, error) {
	ob, err := client.GetOrderBook(tokenID)
	if err != nil {
		return nil, err
	}

	meta := marketMeta[tokenID]

	var tte float64
	if !meta.Expiry.IsZero() {
		tte = pricing.TimeToExpiry(meta.Expiry)
		metrics.TimeToExpirySec.WithLabelValues(meta.Outcome).Set(tte)
	}

	// ATM strike: use spot at market open; fall back to current spot.
	strike := meta.Strike
	if strike == 0 {
		strike = spot
	}
	strikeStr := strconv.FormatFloat(strike, 'f', 2, 64)

	var fair float64
	if spot > 0 && strike > 0 && tte > 0 {
		switch meta.Outcome {
		case "Up":
			fair = pricing.BinaryCallPrice(spot, strike, tte, sigma, 0)
		case "Down":
			fair = pricing.BinaryPutPrice(spot, strike, tte, sigma, 0)
		}
	}

	bid := ob.BestBid()
	ask := ob.BestAsk()
	mid := ob.MidPrice()
	spread := ob.Spread()
	edge := fair - mid

	out := meta.Outcome
	metrics.MarketBestBid.WithLabelValues(out).Set(bid)
	metrics.MarketBestAsk.WithLabelValues(out).Set(ask)
	metrics.MarketMidPrice.WithLabelValues(out).Set(mid)
	metrics.MarketSpread.WithLabelValues(out).Set(spread)
	metrics.FairPrice.WithLabelValues(out).Set(fair)
	metrics.Edge.WithLabelValues(out).Set(edge)
	metrics.Strike.WithLabelValues(out).Set(strike)

	log.Printf("[%.8s %s] spot=%.2f K=%.2f bid=%.4f ask=%.4f fair=%.4f edge=%+.4f tte=%.0fs",
		tokenID, meta.Outcome, spot, strike, bid, ask, fair, edge, tte)

	if err := csv.Write(csvlog.Snapshot{
		Ts:         time.Now(),
		MarketID:   meta.MarketID,
		TokenID:    tokenID,
		Outcome:    meta.Outcome,
		Strike:     strikeStr,
		BTCSpot:    spot,
		BestBid:    bid,
		BestAsk:    ask,
		MidPrice:   mid,
		Spread:     spread,
		FairPrice:  fair,
		Edge:       edge,
		TTESeconds: tte,
	}); err != nil {
		log.Printf("[csv] %v", err)
	}

	return &trader.Snapshot{
		TokenID:   tokenID,
		MarketID:  meta.MarketID,
		Outcome:   meta.Outcome,
		MidPrice:  mid,
		FairPrice: fair,
		Edge:      edge,
		Spread:    spread,
		Expiry:    meta.Expiry,
	}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func maybeExecute(s *trader.Strategy, tc *trader.Client, snap trader.Snapshot) {
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

// strings is used in pollToken via strings.TrimSpace — keep import alive.
var _ = strings.TrimSpace
