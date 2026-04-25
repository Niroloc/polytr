package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
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
	"trading-polymarket/internal/metrics"
	"trading-polymarket/internal/polymarket"
	"trading-polymarket/internal/pricing"
	"trading-polymarket/internal/trader"
)

func main() {
	list := flag.Bool("list", false, "Print active BTC markets and exit (no tracking)")
	discoverWindow := flag.Duration("discover-window", 1*time.Hour, "Auto-discover markets expiring within this window")
	sigma := flag.Float64("sigma", 0.80, "Annualised volatility σ for Black-Scholes (e.g. 0.80)")
	metricsAddr := flag.String("metrics", ":9100", "Prometheus /metrics listen address")
	csvDir := flag.String("csv", "data", "Directory for CSV output files")
	pollInterval := flag.Duration("poll", 5*time.Second, "Order book polling interval")
	envFile := flag.String("env", ".env", "Path to .env file with API credentials")
	flag.Parse()

	// Load .env (silently skip if file is missing — env vars may already be set)
	if err := godotenv.Load(*envFile); err != nil && !os.IsNotExist(err) {
		log.Printf("[env] warning: %v", err)
	}

	cfg := config.Default()
	cfg.Volatility = *sigma
	cfg.MetricsAddr = *metricsAddr
	cfg.CSVDir = *csvDir
	cfg.PollInterval = *pollInterval

	// Always need a client for discovery / listing
	pmClient := polymarket.NewClient(cfg.PolymarketCLOBURL)

	// -list: print markets and exit
	if *list {
		printMarkets(pmClient, *discoverWindow)
		return
	}

	log.Printf("[discover] searching BTC markets expiring within %s...", *discoverWindow)
	ids, err := discoverTokenIDs(pmClient, *discoverWindow)
	if err != nil {
		log.Fatalf("[discover] %v", err)
	}
	if len(ids) == 0 {
		log.Fatalf("[discover] no active BTC markets found within %s", *discoverWindow)
	}
	cfg.MarketTokenIDs = ids
	log.Printf("[discover] found %d token(s)", len(ids))

	if err := cfg.LoadTrading(); err != nil {
		log.Fatalf("[config] %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// BTC price feed
	priceFeed := btcprice.NewFeed(cfg.BinanceWSURL, cfg.BinancePair)
	go priceFeed.Run(ctx)

	// CSV logger
	csvWriter, err := csvlog.NewWriter(cfg.CSVDir)
	if err != nil {
		log.Fatalf("csvlog: %v", err)
	}
	defer csvWriter.Close()

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
		log.Println("[trader] disabled — no API keys found in .env (collect-only mode)")
	}

	// Prometheus HTTP server
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: cfg.MetricsAddr, Handler: mux}
	go func() {
		log.Printf("[metrics] listening on %s/metrics", cfg.MetricsAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[metrics] server error: %v", err)
		}
	}()
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	// Load market metadata at startup
	loadMarketMeta(pmClient, cfg.MarketTokenIDs)

	log.Printf("[collector] tracking %d token(s), σ=%.2f, poll=%s", len(cfg.MarketTokenIDs), cfg.Volatility, cfg.PollInterval)

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[collector] shutting down")
			return
		case <-ticker.C:
			spot := priceFeed.Price()
			metrics.BTCSpotPrice.Set(spot)

			for _, tokenID := range cfg.MarketTokenIDs {
				tokenID = strings.TrimSpace(tokenID)
				snap, err := pollToken(pmClient, csvWriter, tokenID, spot, cfg.Volatility)
				if err != nil {
					log.Printf("[collector] token %s: %v", tokenID, err)
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

// maybeExecute runs the strategy and places an order if signalled.
func maybeExecute(s *trader.Strategy, tc *trader.Client, snap trader.Snapshot) {
	sig := s.Evaluate(snap)
	if sig == nil {
		return
	}
	resp, err := tc.PlaceOrder(*sig)
	if err != nil {
		log.Printf("[trader] place order failed for %s: %v", snap.TokenID[:min(8, len(snap.TokenID))], err)
		return
	}
	s.RecordOpen(snap.TokenID, resp.OrderID)
}

// ── market metadata ──────────────────────────────────────────────────────────

var marketMeta = map[string]tokenMeta{}

type tokenMeta struct {
	MarketID string
	Outcome  string
	Strike   string
	Expiry   time.Time
}

var strikeRe = regexp.MustCompile(`\$[\d,]+(?:\.\d+)?`)

func parseStrike(question string) string {
	m := strikeRe.FindString(question)
	if m == "" {
		return ""
	}
	return strings.ReplaceAll(m[1:], ",", "")
}

func loadMarketMeta(client *polymarket.Client, tokenIDs []string) {
	for _, tokenID := range tokenIDs {
		tokenID = strings.TrimSpace(tokenID)
		market, outcome, err := client.GetMarketByTokenID(tokenID)
		if err != nil {
			log.Printf("[meta] token %s: %v", tokenID, err)
			continue
		}
		expiry, err := market.Expiry()
		if err != nil {
			log.Printf("[meta] token %s: parse expiry %q: %v", tokenID, market.EndDateISO, err)
		}
		strike := parseStrike(market.Question)
		marketMeta[tokenID] = tokenMeta{
			MarketID: market.ConditionID,
			Outcome:  outcome,
			Strike:   strike,
			Expiry:   expiry,
		}
		log.Printf("[meta] token=%.8s outcome=%s strike=%s expiry=%s | %s",
			tokenID, outcome, strike, expiry.Format(time.RFC3339), market.Question)
	}
}

// ── poll loop ────────────────────────────────────────────────────────────────

// pollToken fetches the order book, updates metrics/CSV, and returns a Snapshot for the strategy.
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

	strike, _ := strconv.ParseFloat(meta.Strike, 64)

	var fair float64
	if spot > 0 && strike > 0 && tte > 0 {
		if meta.Outcome == "Yes" {
			fair = pricing.BinaryCallPrice(spot, strike, tte, sigma, 0)
		} else {
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

	strikeLbls := append(lbls, meta.Strike)
	metrics.FairPrice.WithLabelValues(strikeLbls...).Set(fair)
	metrics.Edge.WithLabelValues(strikeLbls...).Set(edge)

	log.Printf("[%s] spot=%.2f bid=%.4f ask=%.4f mid=%.4f fair=%.4f edge=%+.4f tte=%.0fs",
		tokenID[:min(8, len(tokenID))], spot, bid, ask, mid, fair, edge, tte)

	if err := csv.Write(csvlog.Snapshot{
		Ts:         time.Now(),
		MarketID:   meta.MarketID,
		TokenID:    tokenID,
		Outcome:    meta.Outcome,
		Strike:     meta.Strike,
		BTCSpot:    spot,
		BestBid:    bid,
		BestAsk:    ask,
		MidPrice:   mid,
		Spread:     spread,
		FairPrice:  fair,
		Edge:       edge,
		TTESeconds: tte,
	}); err != nil {
		log.Printf("[csv] write error: %v", err)
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

// discoverTokenIDs fetches all active BTC markets expiring within window
// and returns the YES token ID for each (one entry per market).
func discoverTokenIDs(client *polymarket.Client, window time.Duration) ([]string, error) {
	markets, err := client.DiscoverBTCMarkets(window, 10)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range markets {
		for _, t := range m.Tokens {
			if t.Outcome == "Yes" {
				ids = append(ids, t.TokenID)
			}
		}
	}
	return ids, nil
}

// printMarkets prints active BTC markets in a human-readable format and exits.
func printMarkets(client *polymarket.Client, window time.Duration) {
	markets, err := client.DiscoverBTCMarkets(window, 10)
	if err != nil {
		log.Fatalf("[list] %v", err)
	}
	if len(markets) == 0 {
		fmt.Printf("No active BTC markets expiring within %s\n", window)
		return
	}
	fmt.Printf("Active BTC markets (expiring within %s):\n\n", window)
	for _, m := range markets {
		fmt.Printf("Market:  %s\n", m.Question)
		fmt.Printf("EndDate: %s\n", m.EndDateISO)
		for _, t := range m.Tokens {
			fmt.Printf("  token_id=%-66s outcome=%s\n", t.TokenID, t.Outcome)
		}
		fmt.Println()
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
