package csvlog

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TradeRecord captures a single market trade observed on Polymarket's order book
// (fetched from data-api.polymarket.com/trades). One row per executed trade.
type TradeRecord struct {
	Ts          time.Time
	MarketID    string  // condition ID
	TokenID     string  // asset (YES/NO outcome token)
	Outcome     string  // "Up" or "Down"
	Side        string  // "BUY" or "SELL" (taker side)
	Price       float64 // executed price
	Size        float64 // executed size in shares
	TakerWallet string  // proxy wallet of the taker
	TxHash      string  // on-chain transaction hash (unique per trade)
}

var tradeHeader = []string{
	"timestamp", "market_id", "token_id", "outcome",
	"side", "price", "size", "taker_wallet", "tx_hash",
}

// TradeWriter appends market-trade rows to a per-day CSV file.
type TradeWriter struct {
	dir string
	mu  sync.Mutex
	w   *csv.Writer
	f   *os.File
	day string
}

func NewTradeWriter(dir string) (*TradeWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &TradeWriter{dir: dir}, nil
}

func (tw *TradeWriter) Write(r TradeRecord) error {
	tw.mu.Lock()
	defer tw.mu.Unlock()

	day := r.Ts.UTC().Format("2006-01-02")
	if err := tw.ensureFile(day); err != nil {
		return err
	}

	row := []string{
		r.Ts.UTC().Format(time.RFC3339Nano),
		r.MarketID,
		r.TokenID,
		r.Outcome,
		r.Side,
		f(r.Price),
		f(r.Size),
		r.TakerWallet,
		r.TxHash,
	}
	if err := tw.w.Write(row); err != nil {
		return err
	}
	tw.w.Flush()
	return tw.w.Error()
}

func (tw *TradeWriter) Close() {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.f != nil {
		tw.w.Flush()
		tw.f.Close()
	}
}

func (tw *TradeWriter) ensureFile(day string) error {
	if tw.day == day && tw.f != nil {
		return nil
	}
	if tw.f != nil {
		tw.w.Flush()
		tw.f.Close()
	}
	path := filepath.Join(tw.dir, fmt.Sprintf("trades_%s.csv", day))
	needsHeader := !fileExists(path)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	tw.f = file
	tw.w = csv.NewWriter(file)
	tw.day = day
	if needsHeader {
		return tw.w.Write(tradeHeader)
	}
	return nil
}
