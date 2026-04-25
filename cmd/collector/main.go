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
	sigma := flag.Float64("sigma", 0.80, "Annualised volatility σ for Black-Scholes")
	metricsAddr := flag.String("metrics", ":9100", "Prometheus /metrics listen address")
	csvDir := flag.String("csv", "data", "Directory for CSV output files")
	pollInterval := flag.Duration("poll", 5*time.Second, "Order book polling interval")
	envFile := flag.String("env", ".env", "Path to .env file with API credentials")
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
	priceFeed := btcprice.NewFeed(cfg.BinanceWSURL, cfg.BinancePair)
	go priceFeed.Run(ctx)
	log.Println("[btcprice] waiting for first BTC price...")
	spot := priceFeed.WaitPrice(ctx)
	if spot == 0 {
		return // context cancelled
	}
	log.Printf("[btcprice] spot=%.2f", spot)

	// Discover markets and populate metadata in one step.
	log.Printf("[discover] searching BTC markets expiring within %s...", *discoverWindow)
	cfg.MarketTokenIDs = discoverAndLoadMeta(gammaClient, spot, *discoverWindow)
	if len(cfg.MarketTokenIDs) == 0 {
		log.Fatalf("[discover] no active BTC markets found within %s", *discoverWindow)
	}
	log.Printf("[discover] tracking %d token(s)", len(cfg.MarketTokenIDs))

	// CSV logger
	csvWriter, err := csvlog.NewWriter(cfg.CSVDir)
	if err != nil {
		log.Fatalf("csvlog: %v", err)
	}
	defer csvWriter.Close()

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
		log.Println("[trader] disabled — collect-only mode")
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

	// Periodically re-discover to pick up new windows as old ones expire.
	rediscoverTicker := time.NewTicker(*discoverWindow / 2)
	defer rediscoverTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[collector] shutting down")
			return

		case <-rediscoverTicker.C:
			currentSpot := priceFeed.Price()
			newIDs := discoverAndLoadMeta(gammaClient, currentSpot, *discoverWindow)
			if len(newIDs) > 0 {
				cfg.MarketTokenIDs = newIDs
				log.Printf("[discover] refreshed — %d token(s)", len(newIDs))
			}

		case <-ticker.C:
			currentSpot := priceFeed.Price()
			metrics.BTCSpotPrice.Set(currentSpot)

			for _, tokenID := range cfg.MarketTokenIDs {
				snap, err := pollToken(pmClient, csvWriter, tokenID, currentSpot, cfg.Volatility)
				if err != nil {
					log.Printf("[collector] token %.8s: %v", tokenID, err)
					metrics.PollErrors.WithLabelValues(tokenID).Inc()
					continue
				}
				if tradeClient != nil && strategy != nil {
					maybeExecute(strategy, tradeClient, *snap)
				}
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

// discoverAndLoadMeta discovers active BTC markets via Gamma API,
// populates marketMeta for each token, and returns the list of Up token IDs.
// spot is used as the ATM strike for each discovered market.
func discoverAndLoadMeta(gc *gamma.Client, spot float64, window time.Duration) []string {
	markets, err := gc.DiscoverBTC(window)
	if err != nil {
		log.Printf("[discover] gamma error: %v", err)
		return nil
	}

	var ids []string
	for _, m := range markets {
		// Track both Up and Down tokens.
		for _, entry := range []struct {
			tokenID string
			outcome string
		}{
			{m.UpTokenID, "Up"},
			{m.DownTokenID, "Down"},
		} {
			if entry.tokenID == "" {
				continue
			}
			marketMeta[entry.tokenID] = tokenMeta{
				MarketID: m.ConditionID,
				Outcome:  entry.outcome,
				Strike:   spot,
				Expiry:   m.EndDate,
			}
			ids = append(ids, entry.tokenID)
		}
		log.Printf("[meta] %.8s…  expiry=%s | %s",
			m.UpTokenID, m.EndDate.Format("15:04:05Z"), m.Question)
	}
	return ids
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
		metrics.TimeToExpirySec.WithLabelValues(tokenID, meta.MarketID).Set(tte)
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

	lbls := []string{tokenID, meta.MarketID, meta.Outcome}
	metrics.MarketBestBid.WithLabelValues(lbls...).Set(bid)
	metrics.MarketBestAsk.WithLabelValues(lbls...).Set(ask)
	metrics.MarketMidPrice.WithLabelValues(lbls...).Set(mid)
	metrics.MarketSpread.WithLabelValues(lbls...).Set(spread)
	metrics.FairPrice.WithLabelValues(append(lbls, strikeStr)...).Set(fair)
	metrics.Edge.WithLabelValues(append(lbls, strikeStr)...).Set(edge)

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

// strings is used in pollToken via strings.TrimSpace — keep import alive.
var _ = strings.TrimSpace
