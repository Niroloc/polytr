package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	PolymarketCLOBURL string
	MarketTokenIDs    []string

	// Black-Scholes params
	Volatility float64
	RiskFree   float64

	// BTC price source
	BinanceWSURL string
	BinancePair  string

	PollInterval time.Duration
	MetricsAddr  string
	CSVDir       string

	Trading TradingConfig
}

type TradingConfig struct {
	// Ethereum / Polygon
	Address    string // 0x...
	PrivateKey string // hex, with or without 0x
	ChainID    int64

	// Polymarket L2 API credentials
	APIKey        string
	APISecret     string
	APIPassphrase string

	// Strategy knobs
	EdgeThreshold  float64       // minimum edge to trigger a trade
	MaxSizeUSDC    float64       // max USDC per order
	MinSizeUSDC    float64       // min USDC per order (Polymarket floor ~1)
	PriceOffset    float64       // how far from mid to place limit (e.g. 0.01)
	OrderTTL       time.Duration // order lifetime before auto-cancel

	Enabled bool // true when all required keys are present
}

func Default() Config {
	return Config{
		PolymarketCLOBURL: "https://clob.polymarket.com",
		MarketTokenIDs:    []string{},
		Volatility:        0.80,
		RiskFree:          0.0,
		BinanceWSURL:      "wss://stream.binance.com:9443/ws",
		BinancePair:       "btcusdt",
		PollInterval:      5 * time.Second,
		MetricsAddr:       ":9100",
		CSVDir:            "data",
		Trading: TradingConfig{
			ChainID:       137,
			EdgeThreshold: 0.04,
			MaxSizeUSDC:   10.0,
			MinSizeUSDC:   1.0,
			PriceOffset:   0.01,
			OrderTTL:      60 * time.Second,
		},
	}
}

// LoadTrading reads trading credentials and parameters from environment variables.
// Call after godotenv.Load() so .env values are already in os.Environ.
func (c *Config) LoadTrading() error {
	t := &c.Trading

	t.Address = os.Getenv("POLY_ADDRESS")
	t.PrivateKey = os.Getenv("POLY_PRIVATE_KEY")
	t.APIKey = os.Getenv("POLY_API_KEY")
	t.APISecret = os.Getenv("POLY_API_SECRET")
	t.APIPassphrase = os.Getenv("POLY_API_PASSPHRASE")

	if v := os.Getenv("POLY_CHAIN_ID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("POLY_CHAIN_ID: %w", err)
		}
		t.ChainID = id
	}

	if v := os.Getenv("TRADE_EDGE_THRESHOLD"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("TRADE_EDGE_THRESHOLD: %w", err)
		}
		t.EdgeThreshold = f
	}
	if v := os.Getenv("TRADE_MAX_SIZE_USDC"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("TRADE_MAX_SIZE_USDC: %w", err)
		}
		t.MaxSizeUSDC = f
	}
	if v := os.Getenv("TRADE_MIN_SIZE_USDC"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("TRADE_MIN_SIZE_USDC: %w", err)
		}
		t.MinSizeUSDC = f
	}
	if v := os.Getenv("TRADE_PRICE_OFFSET"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("TRADE_PRICE_OFFSET: %w", err)
		}
		t.PriceOffset = f
	}
	if v := os.Getenv("TRADE_ORDER_TTL_SECONDS"); v != "" {
		sec, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("TRADE_ORDER_TTL_SECONDS: %w", err)
		}
		t.OrderTTL = time.Duration(sec) * time.Second
	}

	t.Enabled = t.Address != "" && t.PrivateKey != "" &&
		t.APIKey != "" && t.APISecret != "" && t.APIPassphrase != ""

	return nil
}
